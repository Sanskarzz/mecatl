package server_test

import (
	"context"
	"errors"
	"io"
	"net"
	"testing"
	"time"

	"google.golang.org/grpc"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/credentials/insecure"
	"google.golang.org/grpc/status"
	"google.golang.org/grpc/test/bufconn"

	mecatlv1 "github.com/stacklok/mecatl/contracts/gen/go/mecatl/v1"
	"github.com/stacklok/mecatl/internal/adapter/memfs"
	"github.com/stacklok/mecatl/internal/adapter/mockllm"
	"github.com/stacklok/mecatl/internal/adapter/permpolicy"
	"github.com/stacklok/mecatl/internal/adapter/server"
	"github.com/stacklok/mecatl/internal/adapter/store/memstore"
	"github.com/stacklok/mecatl/internal/agent"
	"github.com/stacklok/mecatl/internal/governance"
	"github.com/stacklok/mecatl/internal/tool"
)

// dialGRPC stands up an in-memory gRPC server backed by svc and returns a
// connected client plus a cleanup func.
func dialGRPC(t *testing.T, svc *server.Service) (mecatlv1.HarnessServiceClient, func()) {
	t.Helper()
	lis := bufconn.Listen(1 << 20)
	gs := grpc.NewServer()
	mecatlv1.RegisterHarnessServiceServer(gs, server.NewHarnessServer(svc))
	go func() { _ = gs.Serve(lis) }()

	conn, err := grpc.NewClient("passthrough:///bufnet",
		grpc.WithContextDialer(func(ctx context.Context, _ string) (net.Conn, error) {
			return lis.DialContext(ctx)
		}),
		grpc.WithTransportCredentials(insecure.NewCredentials()),
	)
	if err != nil {
		t.Fatalf("dial: %v", err)
	}
	return mecatlv1.NewHarnessServiceClient(conn), func() {
		_ = conn.Close()
		gs.Stop()
		_ = lis.Close()
	}
}

// newService builds a server.Service over a real *agent.Engine wired with
// mockllm + memfs + permpolicy + the given tools and policy rules.
func newService(t *testing.T, llm *mockllm.Provider, rules []governance.Rule, tools ...tool.Tool) *server.Service {
	t.Helper()
	cat := tool.NewCatalog()
	for _, tl := range tools {
		cat.MustRegister(tl)
	}
	engine := agent.NewEngine(agent.Deps{
		LLM:     llm,
		Catalog: cat,
		Policy:  permpolicy.NewPolicy(rules, nil),
		Model:   "test-model",
	})
	svc, err := server.NewService(server.Config{
		Engine:     engine,
		Store:      memstore.New(),
		Workspaces: func(root string) tool.Workspace { return memfs.NewWorkspace(root) },
		Now:        func() time.Time { return time.Unix(0, 0) },
		// The server reads DefaultCapabilities (composition-computed), not the engine.
		// In these tests there is no catalog/selector, so the intersection is the bare
		// adapter caps — mirror that by sourcing them from the wired provider.
		DefaultCapabilities: llm.Capabilities(),
	})
	if err != nil {
		t.Fatalf("new service: %v", err)
	}
	return svc
}

// TestGRPCConverseFullCycle drives CreateSession then a Converse stream that
// runs to a terminal result, asserting the event taxonomy crosses the wire.
func TestGRPCConverseFullCycle(t *testing.T) {
	read := &scriptTool{name: "Read", readOnly: true, content: "file body"}
	llm := mockllm.New(
		mockllm.ToolCallTurn(call("c1", "Read", `{"path":"a.go"}`)),
		mockllm.TextTurn("all done"),
	)
	svc := newService(t, llm, allowRules(), read)
	client, cleanup := dialGRPC(t, svc)
	defer cleanup()

	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()

	cs, err := client.CreateSession(ctx, &mecatlv1.CreateSessionRequest{Workspace: "/ws"})
	if err != nil {
		t.Fatalf("CreateSession: %v", err)
	}

	stream, err := client.Converse(ctx)
	if err != nil {
		t.Fatalf("Converse: %v", err)
	}
	if err := stream.Send(&mecatlv1.ConverseRequest{
		Kind: &mecatlv1.ConverseRequest_Prompt{Prompt: &mecatlv1.Prompt{SessionId: cs.GetSessionId(), Text: "look"}},
	}); err != nil {
		t.Fatalf("Send prompt: %v", err)
	}
	_ = stream.CloseSend()

	events := recvAll(t, stream)
	if !hasType(events, "tool.call") || !hasType(events, "tool.result") {
		t.Fatalf("missing tool events: %v", typesOf(events))
	}
	res := lastResult(t, events)
	if res.GetStop() != "end_turn" || res.GetText() != "all done" {
		t.Fatalf("result = %+v", res)
	}
}

// TestGRPCCloseSession asserts the session-end RPC: a created session closes ok,
// a second close is idempotent (still ok, since close != delete-snapshot), and a
// never-created id surfaces as codes.NotFound.
func TestGRPCCloseSession(t *testing.T) {
	svc := newService(t, mockllm.New(), allowRules())
	client, cleanup := dialGRPC(t, svc)
	defer cleanup()

	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()

	cs, err := client.CreateSession(ctx, &mecatlv1.CreateSessionRequest{Workspace: "/ws"})
	if err != nil {
		t.Fatalf("CreateSession: %v", err)
	}

	if _, err := client.CloseSession(ctx, &mecatlv1.CloseSessionRequest{SessionId: cs.GetSessionId()}); err != nil {
		t.Fatalf("CloseSession: %v", err)
	}
	// Idempotent: the snapshot still persists, so a second close succeeds.
	if _, err := client.CloseSession(ctx, &mecatlv1.CloseSessionRequest{SessionId: cs.GetSessionId()}); err != nil {
		t.Fatalf("second CloseSession: %v", err)
	}
	// Unknown id -> NotFound.
	_, err = client.CloseSession(ctx, &mecatlv1.CloseSessionRequest{SessionId: "never-created"})
	if status.Code(err) != codes.NotFound {
		t.Fatalf("CloseSession unknown id code = %v, want NotFound", status.Code(err))
	}
}

// TestGRPCConversePermissionApprove is the headline test: the model proposes a
// tool that requires approval; the loop pauses with a permission.ask; the
// client replies with ResumeApproval{allow:true} on the SAME stream; the loop
// resumes, the tool runs, and the run completes.
func TestGRPCConversePermissionApprove(t *testing.T) {
	write := &scriptTool{name: "Write", readOnly: false, content: "wrote"}
	llm := mockllm.New(
		mockllm.ToolCallTurn(call("c1", "Write", `{"path":"a"}`)),
		mockllm.TextTurn("done"),
	)
	// nil rules => default decision is Ask.
	svc := newService(t, llm, nil, write)
	client, cleanup := dialGRPC(t, svc)
	defer cleanup()

	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()

	cs, err := client.CreateSession(ctx, &mecatlv1.CreateSessionRequest{Workspace: "/ws"})
	if err != nil {
		t.Fatalf("CreateSession: %v", err)
	}
	stream, err := client.Converse(ctx)
	if err != nil {
		t.Fatalf("Converse: %v", err)
	}
	if err := stream.Send(&mecatlv1.ConverseRequest{
		Kind: &mecatlv1.ConverseRequest_Prompt{Prompt: &mecatlv1.Prompt{SessionId: cs.GetSessionId(), Text: "go"}},
	}); err != nil {
		t.Fatalf("Send prompt: %v", err)
	}

	var events []*mecatlv1.Event
	var sawAsk bool
	for {
		resp, err := stream.Recv()
		if errors.Is(err, io.EOF) {
			break
		}
		if err != nil {
			t.Fatalf("Recv: %v", err)
		}
		ev := resp.GetEvent()
		events = append(events, ev)
		if ev.GetType() == "permission.ask" {
			sawAsk = true
			if err := stream.Send(&mecatlv1.ConverseRequest{
				Kind: &mecatlv1.ConverseRequest_ResumeApproval{
					ResumeApproval: &mecatlv1.ResumeApproval{AskId: ev.GetAsk().GetAskId(), Allow: true},
				},
			}); err != nil {
				t.Fatalf("Send approve: %v", err)
			}
		}
		if ev.GetType() == "result" {
			break
		}
	}
	if !sawAsk {
		t.Fatalf("no permission.ask received: %v", typesOf(events))
	}
	if !write.ran() {
		t.Fatalf("approved tool did not run")
	}
	res := lastResult(t, events)
	if res.GetStop() != "end_turn" {
		t.Fatalf("stop = %q, want end_turn", res.GetStop())
	}
}

// TestGRPCConverseCancel sends a Cancel frame on a blocked run and asserts a
// terminal cancelled result.
func TestGRPCConverseCancel(t *testing.T) {
	llm := mockllm.New(mockllm.ChunksTurn(blockingChunks()...))
	svc := newService(t, llm, allowRules())
	client, cleanup := dialGRPC(t, svc)
	defer cleanup()

	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()

	cs, err := client.CreateSession(ctx, &mecatlv1.CreateSessionRequest{Workspace: "/ws"})
	if err != nil {
		t.Fatalf("CreateSession: %v", err)
	}
	stream, err := client.Converse(ctx)
	if err != nil {
		t.Fatalf("Converse: %v", err)
	}
	if err := stream.Send(&mecatlv1.ConverseRequest{
		Kind: &mecatlv1.ConverseRequest_Prompt{Prompt: &mecatlv1.Prompt{SessionId: cs.GetSessionId(), Text: "go"}},
	}); err != nil {
		t.Fatalf("Send prompt: %v", err)
	}

	// Wait for the first streamed delta, then cancel.
	var events []*mecatlv1.Event
	for {
		resp, err := stream.Recv()
		if err != nil {
			t.Fatalf("Recv before cancel: %v", err)
		}
		events = append(events, resp.GetEvent())
		if resp.GetEvent().GetType() == "message.delta" {
			break
		}
	}
	if err := stream.Send(&mecatlv1.ConverseRequest{
		Kind: &mecatlv1.ConverseRequest_Cancel{Cancel: &mecatlv1.Cancel{}},
	}); err != nil {
		t.Fatalf("Send cancel: %v", err)
	}

	for {
		resp, err := stream.Recv()
		if errors.Is(err, io.EOF) {
			break
		}
		if err != nil {
			t.Fatalf("Recv after cancel: %v", err)
		}
		events = append(events, resp.GetEvent())
		if resp.GetEvent().GetType() == "result" {
			break
		}
	}
	res := lastResult(t, events)
	if res.GetStop() != "cancelled" {
		t.Fatalf("stop = %q, want cancelled", res.GetStop())
	}
}

// TestGRPCConverseFirstFrameMustBePrompt rejects a non-prompt first frame.
func TestGRPCConverseFirstFrameMustBePrompt(t *testing.T) {
	svc := newService(t, mockllm.New(), allowRules())
	client, cleanup := dialGRPC(t, svc)
	defer cleanup()

	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()

	stream, err := client.Converse(ctx)
	if err != nil {
		t.Fatalf("Converse: %v", err)
	}
	if err := stream.Send(&mecatlv1.ConverseRequest{
		Kind: &mecatlv1.ConverseRequest_Cancel{Cancel: &mecatlv1.Cancel{}},
	}); err != nil {
		t.Fatalf("Send: %v", err)
	}
	_ = stream.CloseSend()
	_, err = stream.Recv()
	if err == nil {
		t.Fatalf("expected error for non-prompt first frame")
	}
}

// allowRules returns a policy rule set that allows every tool call.
func allowRules() []governance.Rule {
	return []governance.Rule{{Effect: governance.Allow}}
}
