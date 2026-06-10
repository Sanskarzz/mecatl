package server_test

import (
	"bytes"
	"context"
	"errors"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	mecatlv1 "github.com/stacklok/mecatl/contracts/gen/go/mecatl/v1"
	"github.com/stacklok/mecatl/internal/adapter/memfs"
	"github.com/stacklok/mecatl/internal/adapter/mockllm"
	"github.com/stacklok/mecatl/internal/adapter/permpolicy"
	"github.com/stacklok/mecatl/internal/adapter/server"
	"github.com/stacklok/mecatl/internal/adapter/store/memstore"
	"github.com/stacklok/mecatl/internal/agent"
	"github.com/stacklok/mecatl/internal/session"
	"github.com/stacklok/mecatl/internal/tool"
)

// newInteractiveSubagentService builds a Service whose engine is INTERACTIVE and
// carries a Subagent tool with a Bash-bearing child, so a child substitution ask
// surfaces on the Converse stream (the parked state the CancelChild e2e cancels
// out of). It returns the service and the recording Bash so the test can assert
// the command never executed.
func newInteractiveSubagentService(t *testing.T) (*server.Service, *scriptTool) {
	t.Helper()
	bash := &scriptTool{name: "Bash", readOnly: false, content: "ran"}
	childCat := tool.NewCatalog()
	childCat.MustRegister(bash)
	childLLM := mockllm.New(
		// A substitution with a non-read-only inner stand-in: NOT auto-approvable,
		// so it parks the child on a surfaced ask.
		mockllm.ToolCallTurn(call("k1", "Bash", `{"command":"cat $(zap)"}`)),
		mockllm.TextTurn("child: never reached"),
	)
	childEngine := agent.NewEngine(agent.Deps{
		LLM:     childLLM,
		Catalog: childCat,
		Policy:  permpolicy.NewPolicy(allowRules(), nil),
		Model:   "child-model",
	})
	task := agent.NewSubagentTool(childEngine)

	parentCat := tool.NewCatalog()
	parentCat.MustRegister(task)
	parentLLM := mockllm.New(
		mockllm.ToolCallTurn(call("p1", "Subagent", `{"prompt":"run it"}`)),
		mockllm.TextTurn("parent: done"),
	)
	engine := agent.NewEngine(agent.Deps{
		LLM:         parentLLM,
		Catalog:     parentCat,
		Policy:      permpolicy.NewPolicy(allowRules(), nil),
		Model:       "test-model",
		Interactive: true, // the child ask SURFACES instead of auto-denying
	})
	svc, err := server.NewService(server.Config{
		Engine:              engine,
		Store:               memstore.New(),
		Workspaces:          func(root string) tool.Workspace { return memfs.NewWorkspace(root) },
		Now:                 func() time.Time { return time.Unix(0, 0) },
		DefaultCapabilities: parentLLM.Capabilities(),
	})
	if err != nil {
		t.Fatalf("new service: %v", err)
	}
	return svc, bash
}

// TestGRPCConverseCancelChild is the wire e2e for the per-child cancel: a Subagent
// child parks on a surfaced permission.ask; the client answers with a CancelChild
// frame instead of an approval. The server must emit a permission.retract for the
// surfaced ask_id, fold the child back as a SUCCESS-with-note
// "[subagent cancelled by user]" tool result, never run the command, and complete
// the parent run cleanly.
func TestGRPCConverseCancelChild(t *testing.T) {
	svc, bash := newInteractiveSubagentService(t)
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
		t.Fatalf("send prompt: %v", err)
	}

	var (
		evs        []*mecatlv1.Event
		childID    string
		askID      string
		retractIDs []string
		sentCancel bool
	)
	for {
		resp, rerr := stream.Recv()
		if rerr != nil {
			break // EOF (or ctx timeout — the assertions below fail loudly then)
		}
		ev := resp.GetEvent()
		evs = append(evs, ev)
		switch ev.GetType() {
		case "subagent.start":
			childID = ev.GetSubagent().GetChildId()
		case "permission.ask":
			askID = ev.GetAsk().GetAskId()
			if !sentCancel {
				sentCancel = true
				// Answer the parked ask with a per-child CANCEL, not a verdict.
				if serr := stream.Send(&mecatlv1.ConverseRequest{
					Kind: &mecatlv1.ConverseRequest_CancelChild{CancelChild: &mecatlv1.CancelChild{ChildId: childID}},
				}); serr != nil {
					t.Fatalf("send CancelChild: %v", serr)
				}
			}
		case "permission.retract":
			retractIDs = append(retractIDs, ev.GetAsk().GetAskId())
		}
	}

	if !sentCancel || childID == "" || askID == "" {
		t.Fatalf("setup did not surface the child ask (childID=%q askID=%q); events: %v", childID, askID, typesOf(evs))
	}
	if len(retractIDs) != 1 || retractIDs[0] != askID {
		t.Fatalf("expected one permission.retract for ask %q, got %v", askID, retractIDs)
	}
	var taskResult *mecatlv1.ToolResult
	for _, ev := range evs {
		if ev.GetType() == "tool.result" {
			taskResult = ev.GetToolResult()
		}
	}
	if taskResult == nil || taskResult.GetIsError() {
		t.Fatalf("expected a non-error Subagent result, got %+v", taskResult)
	}
	if !strings.Contains(taskResult.GetContent(), "[subagent cancelled by user]") {
		t.Fatalf("result must carry the cancelled-by-user note, got %q", taskResult.GetContent())
	}
	if !strings.Contains(taskResult.GetContent(), "agentId: "+childID) {
		t.Fatalf("result must keep the resumable agentId trailer, got %q", taskResult.GetContent())
	}
	if bash.ran() {
		t.Fatalf("the cancelled child's command must never execute")
	}
	if res := lastResult(t, evs); res.GetStop() == "error" || res.GetStop() == "cancelled" {
		t.Fatalf("the PARENT run must complete cleanly, got stop %q", res.GetStop())
	}
}

// TestServiceCancelChildFallbacks pins the Approve-mirror error contract: an
// unknown session → ErrNotFound; a known session with no live run → ErrNoActiveRun.
func TestServiceCancelChildFallbacks(t *testing.T) {
	svc := newService(t, mockllm.New(mockllm.TextTurn("ok")), allowRules())
	if err := svc.CancelChild(context.Background(), "nope", "subagent-x"); !errors.Is(err, server.ErrNotFound) {
		t.Fatalf("unknown session: got %v, want ErrNotFound", err)
	}
	sess, err := svc.CreateSession(context.Background(), "/ws", session.ModeDefault, session.Limits{})
	if err != nil {
		t.Fatalf("CreateSession: %v", err)
	}
	if err := svc.CancelChild(context.Background(), sess.ID, "subagent-x"); !errors.Is(err, server.ErrNoActiveRun) {
		t.Fatalf("runless session: got %v, want ErrNoActiveRun", err)
	}
}

// TestHTTPCancelChild pins the REST mirror: bad body → 400, missing child_id → 400,
// unknown session → 404, known-but-runless session → 409 (the same surface shape as
// /approve; the live-run path is covered end-to-end by the gRPC test).
func TestHTTPCancelChild(t *testing.T) {
	svc := newService(t, mockllm.New(mockllm.TextTurn("ok")), allowRules())
	srv := httptest.NewServer(server.NewHTTPHandler(svc))
	defer srv.Close()
	id := createHTTPSession(t, srv)

	post := func(path, body string) int {
		t.Helper()
		resp, err := http.Post(srv.URL+path, "application/json", bytes.NewReader([]byte(body)))
		if err != nil {
			t.Fatalf("POST %s: %v", path, err)
		}
		defer resp.Body.Close()
		return resp.StatusCode
	}
	if got := post("/v1/sessions/"+id+"/cancel-child", "{not json"); got != http.StatusBadRequest {
		t.Fatalf("bad body: status = %d, want 400", got)
	}
	if got := post("/v1/sessions/"+id+"/cancel-child", `{}`); got != http.StatusBadRequest {
		t.Fatalf("missing child_id: status = %d, want 400", got)
	}
	if got := post("/v1/sessions/unknown/cancel-child", `{"child_id":"subagent-x"}`); got != http.StatusNotFound {
		t.Fatalf("unknown session: status = %d, want 404", got)
	}
	if got := post("/v1/sessions/"+id+"/cancel-child", `{"child_id":"subagent-x"}`); got != http.StatusConflict {
		t.Fatalf("runless session: status = %d, want 409", got)
	}
}
