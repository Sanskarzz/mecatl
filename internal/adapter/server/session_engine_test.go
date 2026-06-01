package server_test

import (
	"context"
	"errors"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/stacklok/mecatl/internal/adapter/mcp"
	"github.com/stacklok/mecatl/internal/adapter/memfs"
	"github.com/stacklok/mecatl/internal/adapter/mockllm"
	"github.com/stacklok/mecatl/internal/adapter/permpolicy"
	"github.com/stacklok/mecatl/internal/adapter/server"
	"github.com/stacklok/mecatl/internal/adapter/store/memstore"
	"github.com/stacklok/mecatl/internal/agent"
	"github.com/stacklok/mecatl/internal/session"
	"github.com/stacklok/mecatl/internal/tool"
)

// newMCPService builds a server.Service with a configurable SessionEngine factory,
// a shared engine that streams sharedReply, and the in-memory store/workspace.
func newMCPService(t *testing.T, sharedReply string, factory server.SessionEngineFactory) *server.Service {
	t.Helper()
	shared := agent.NewEngine(agent.Deps{
		LLM:     mockllm.New(mockllm.TextTurn(sharedReply)),
		Catalog: tool.NewCatalog(),
		Policy:  permpolicy.NewPolicy(nil),
		Model:   "test-model",
	})
	svc, err := server.NewService(server.Config{
		Engine:        shared,
		Store:         memstore.New(),
		Workspaces:    func(root string) tool.Workspace { return memfs.NewWorkspace(root) },
		DefaultLimits: session.Limits{MaxTurns: 10, MaxToolCalls: 20},
		Now:           func() time.Time { return time.Unix(0, 0) },
		SessionEngine: factory,
	})
	if err != nil {
		t.Fatalf("new service: %v", err)
	}
	return svc
}

// finalText drains a run and returns the terminal result text.
func finalText(t *testing.T, run *agent.Run) string {
	t.Helper()
	var text string
	for ev := range run.Events() {
		if ev.Type == session.EvResult && ev.Result != nil {
			text = ev.Result.Text
		}
	}
	return text
}

// TestCreateSessionWithMCPEmptySpecsSharedEngine asserts that with no MCP specs the
// session uses the SHARED engine and the per-session factory is NOT called.
func TestCreateSessionWithMCPEmptySpecsSharedEngine(t *testing.T) {
	var called atomic.Int32
	factory := func(_ context.Context, _ []mcp.ServerConfig) (*agent.Engine, func() error, error) {
		called.Add(1)
		return nil, func() error { return nil }, nil
	}
	svc := newMCPService(t, "shared reply", factory)

	sess, err := svc.CreateSessionWithMCP(context.Background(), "/ws", session.ModeDefault, session.Limits{}, nil)
	if err != nil {
		t.Fatalf("CreateSessionWithMCP: %v", err)
	}
	if called.Load() != 0 {
		t.Fatalf("factory called %d times for empty specs, want 0", called.Load())
	}

	run, err := svc.StartRun(context.Background(), sess.ID, "hi")
	if err != nil {
		t.Fatalf("StartRun: %v", err)
	}
	if got := finalText(t, run); !strings.Contains(got, "shared reply") {
		t.Fatalf("final text = %q, want the shared engine's reply", got)
	}
	svc.FinishRun(sess.ID, run)
}

// TestCreateSessionWithMCPNilFactory asserts that supplying specs with no
// SessionEngine factory configured is an invalid-argument error.
func TestCreateSessionWithMCPNilFactory(t *testing.T) {
	svc := newMCPService(t, "shared", nil) // no SessionEngine
	_, err := svc.CreateSessionWithMCP(context.Background(), "/ws", session.ModeDefault, session.Limits{},
		[]mcp.ServerConfig{{Name: "docs", URL: "https://example.test/mcp"}})
	if err == nil || !errors.Is(err, server.ErrInvalidArgument) {
		t.Fatalf("want ErrInvalidArgument for specs without a factory, got %v", err)
	}
}

// TestStartRunRoutesToPerSessionEngine asserts a session created with specs runs on
// the PER-SESSION engine (a distinguishable reply), not the shared one.
func TestStartRunRoutesToPerSessionEngine(t *testing.T) {
	var closed atomic.Int32
	perSession := agent.NewEngine(agent.Deps{
		LLM:     mockllm.New(mockllm.TextTurn("per-session reply")),
		Catalog: tool.NewCatalog(),
		Policy:  permpolicy.NewPolicy(nil),
		Model:   "test-model",
	})
	factory := func(_ context.Context, specs []mcp.ServerConfig) (*agent.Engine, func() error, error) {
		if len(specs) != 1 || specs[0].URL != "https://example.test/mcp" {
			t.Errorf("factory specs = %+v", specs)
		}
		return perSession, func() error { closed.Add(1); return nil }, nil
	}
	svc := newMCPService(t, "shared reply", factory)

	sess, err := svc.CreateSessionWithMCP(context.Background(), "/ws", session.ModeDefault, session.Limits{},
		[]mcp.ServerConfig{{Name: "docs", URL: "https://example.test/mcp"}})
	if err != nil {
		t.Fatalf("CreateSessionWithMCP: %v", err)
	}

	run, err := svc.StartRun(context.Background(), sess.ID, "hi")
	if err != nil {
		t.Fatalf("StartRun: %v", err)
	}
	if got := finalText(t, run); !strings.Contains(got, "per-session reply") {
		t.Fatalf("final text = %q, want the per-session engine's reply", got)
	}
	svc.FinishRun(sess.ID, run)

	// CloseSession tears the per-session engine's MCP manager down exactly once.
	svc.CloseSession(sess.ID)
	if closed.Load() != 1 {
		t.Fatalf("close called %d times, want 1", closed.Load())
	}
	// Idempotent: a second close is a no-op.
	svc.CloseSession(sess.ID)
	if closed.Load() != 1 {
		t.Fatalf("close called %d times after second CloseSession, want 1", closed.Load())
	}
}

// TestServiceCloseTearsDownSessionEngines asserts Service.Close closes every
// registered per-session engine.
func TestServiceCloseTearsDownSessionEngines(t *testing.T) {
	var closed atomic.Int32
	factory := func(_ context.Context, _ []mcp.ServerConfig) (*agent.Engine, func() error, error) {
		eng := agent.NewEngine(agent.Deps{
			LLM:     mockllm.New(mockllm.TextTurn("x")),
			Catalog: tool.NewCatalog(),
			Policy:  permpolicy.NewPolicy(nil),
			Model:   "test-model",
		})
		return eng, func() error { closed.Add(1); return nil }, nil
	}
	svc := newMCPService(t, "shared", factory)
	if _, err := svc.CreateSessionWithMCP(context.Background(), "/ws", session.ModeDefault, session.Limits{},
		[]mcp.ServerConfig{{Name: "a", URL: "https://a.test/mcp"}}); err != nil {
		t.Fatalf("create a: %v", err)
	}
	if _, err := svc.CreateSessionWithMCP(context.Background(), "/ws", session.ModeDefault, session.Limits{},
		[]mcp.ServerConfig{{Name: "b", URL: "https://b.test/mcp"}}); err != nil {
		t.Fatalf("create b: %v", err)
	}
	svc.Close()
	if closed.Load() != 2 {
		t.Fatalf("close called %d times, want 2", closed.Load())
	}
}
