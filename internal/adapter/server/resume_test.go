package server_test

import (
	"context"
	"encoding/json"
	"errors"
	"testing"
	"time"

	"github.com/stacklok/mecatl/engine/agent"
	"github.com/stacklok/mecatl/engine/governance"
	"github.com/stacklok/mecatl/engine/port"
	"github.com/stacklok/mecatl/engine/session"
	"github.com/stacklok/mecatl/engine/tool"
	"github.com/stacklok/mecatl/internal/adapter/memfs"
	"github.com/stacklok/mecatl/internal/adapter/mockllm"
	"github.com/stacklok/mecatl/internal/adapter/permpolicy"
	"github.com/stacklok/mecatl/internal/adapter/server"
	"github.com/stacklok/mecatl/internal/adapter/store/jsonlstore"
	"github.com/stacklok/mecatl/internal/adapter/store/memstore"
)

// newServiceWithStore builds a Service over the given (shared) store, so two
// Services can be pointed at the same durable store to simulate a restart.
func newServiceWithStore(t *testing.T, store port.SessionStore) *server.Service {
	t.Helper()
	engine := agent.NewEngine(agent.Deps{
		LLM:     mockllm.New(),
		Catalog: tool.NewCatalog(),
		Policy:  permpolicy.NewPolicy([]governance.Rule{{Effect: governance.Allow}}, nil),
		Model:   "test-model",
		Store:   store,
	})
	svc, err := server.NewService(server.Config{
		Engine:     engine,
		Store:      store,
		Workspaces: func(root string) tool.Workspace { return memfs.NewWorkspace(root) },
		Now:        func() time.Time { return time.Unix(0, 0) },
	})
	if err != nil {
		t.Fatalf("new service: %v", err)
	}
	return svc
}

// TestAutoResumeFromStore creates a session through one Service backed by a
// jsonlstore, then builds a NEW Service over the SAME store dir (simulating a
// process restart) and confirms GetSession resolves the session by loading it
// from the store.
func TestAutoResumeFromStore(t *testing.T) {
	dir := t.TempDir()
	store, err := jsonlstore.New(dir)
	if err != nil {
		t.Fatalf("jsonlstore: %v", err)
	}

	// Process 1: create and persist a session.
	svc1 := newServiceWithStore(t, store)
	sess, err := svc1.CreateSession(context.Background(), "/ws", session.ModeDefault, session.Limits{MaxTurns: 3})
	if err != nil {
		t.Fatalf("CreateSession: %v", err)
	}

	// Process 2: a brand-new Service + store over the same dir (restart).
	store2, err := jsonlstore.New(dir)
	if err != nil {
		t.Fatalf("jsonlstore reopen: %v", err)
	}
	svc2 := newServiceWithStore(t, store2)

	got, err := svc2.GetSession(context.Background(), sess.ID)
	if err != nil {
		t.Fatalf("GetSession after restart: %v", err)
	}
	if got.ID != sess.ID || got.Workspace != "/ws" || got.Mode != session.ModeDefault {
		t.Fatalf("loaded session mismatch: %+v", got)
	}
	if got.Limits.MaxTurns != 3 {
		t.Fatalf("limits not round-tripped: %+v", got.Limits)
	}
}

// TestApproveFallsBackToStore confirms Approve/Cancel for a session present only
// in the store (no in-flight run, e.g. after a restart) return ErrNoActiveRun,
// while an unknown id returns ErrNotFound.
func TestApproveFallsBackToStore(t *testing.T) {
	dir := t.TempDir()
	store, err := jsonlstore.New(dir)
	if err != nil {
		t.Fatalf("jsonlstore: %v", err)
	}

	svc1 := newServiceWithStore(t, store)
	sess, err := svc1.CreateSession(context.Background(), "/ws", session.ModeDefault, session.Limits{MaxTurns: 3})
	if err != nil {
		t.Fatalf("CreateSession: %v", err)
	}

	// Restart: new Service, no in-flight runs.
	store2, _ := jsonlstore.New(dir)
	svc2 := newServiceWithStore(t, store2)

	// Known session, no live run -> ErrNoActiveRun.
	if err := svc2.Approve(context.Background(), sess.ID, "ask-1", session.VerdictAllowOnce); !errors.Is(err, server.ErrNoActiveRun) {
		t.Fatalf("Approve known/runless = %v, want ErrNoActiveRun", err)
	}
	if err := svc2.Cancel(context.Background(), sess.ID); !errors.Is(err, server.ErrNoActiveRun) {
		t.Fatalf("Cancel known/runless = %v, want ErrNoActiveRun", err)
	}

	// Unknown session -> ErrNotFound.
	if err := svc2.Approve(context.Background(), "does-not-exist", "ask-1", session.VerdictAllowOnce); !errors.Is(err, server.ErrNotFound) {
		t.Fatalf("Approve unknown = %v, want ErrNotFound", err)
	}
}

// TestSetModePersistsAndValidates exercises the SetMode seam: it changes an idle
// session's mode and persists it, rejects an empty mode (ErrInvalidArgument) and
// an unknown session (ErrNotFound).
func TestSetModePersistsAndValidates(t *testing.T) {
	store := memstore.New()
	svc := newServiceWithStore(t, store)
	sess, err := svc.CreateSession(context.Background(), "/ws", session.ModeDefault, session.Limits{MaxTurns: 3})
	if err != nil {
		t.Fatalf("CreateSession: %v", err)
	}

	got, err := svc.SetMode(context.Background(), sess.ID, session.ModePlan)
	if err != nil {
		t.Fatalf("SetMode: %v", err)
	}
	if got.Mode != session.ModePlan {
		t.Fatalf("returned mode = %q, want plan", got.Mode)
	}
	// Persisted: a fresh load reflects the change.
	reloaded, err := svc.GetSession(context.Background(), sess.ID)
	if err != nil {
		t.Fatalf("GetSession: %v", err)
	}
	if reloaded.Mode != session.ModePlan {
		t.Fatalf("persisted mode = %q, want plan", reloaded.Mode)
	}

	// Empty mode -> ErrInvalidArgument.
	if _, err := svc.SetMode(context.Background(), sess.ID, ""); !errors.Is(err, server.ErrInvalidArgument) {
		t.Fatalf("SetMode empty = %v, want ErrInvalidArgument", err)
	}
	// Unknown session -> ErrNotFound.
	if _, err := svc.SetMode(context.Background(), "nope", session.ModePlan); !errors.Is(err, server.ErrNotFound) {
		t.Fatalf("SetMode unknown = %v, want ErrNotFound", err)
	}
}

// TestLoadSessionReopensCompleted confirms LoadSession reopens a completed
// session to idle (preserving history) so it can run again, and returns
// ErrNotFound for an unknown id.
func TestLoadSessionReopensCompleted(t *testing.T) {
	store := memstore.New()
	svc := newServiceWithStore(t, store)
	sess, err := svc.CreateSession(context.Background(), "/ws", session.ModeDefault, session.Limits{MaxTurns: 3})
	if err != nil {
		t.Fatalf("CreateSession: %v", err)
	}
	// Drive it to a completed terminal state and persist that snapshot.
	if err := sess.BeginTurn(); err != nil {
		t.Fatalf("BeginTurn: %v", err)
	}
	if err := sess.Complete(); err != nil {
		t.Fatalf("Complete: %v", err)
	}
	if err := store.Save(context.Background(), sess); err != nil {
		t.Fatalf("Save: %v", err)
	}

	loaded, err := svc.LoadSession(context.Background(), sess.ID)
	if err != nil {
		t.Fatalf("LoadSession: %v", err)
	}
	if loaded.State != session.StateIdle {
		t.Fatalf("loaded state = %q, want idle (reopened)", loaded.State)
	}

	if _, err := svc.LoadSession(context.Background(), "missing"); !errors.Is(err, server.ErrNotFound) {
		t.Fatalf("LoadSession unknown = %v, want ErrNotFound", err)
	}
}

// blockingTool signals it has started, then blocks until the run's context is
// cancelled — so a run can be driven to StateCancelled mid-dispatch (after the
// assistant tool-call is recorded, before its result).
type blockingTool struct{ started chan struct{} }

func (*blockingTool) Spec() tool.ToolSpec {
	return tool.ToolSpec{Name: "Read", Description: "Read: test tool", Schema: json.RawMessage(`{"type":"object"}`)}
}
func (*blockingTool) ReadOnly() bool { return true }
func (b *blockingTool) Execute(ctx context.Context, _ session.ToolCall, _ tool.Workspace) (session.ToolResult, error) {
	close(b.started)
	<-ctx.Done()
	return session.ToolResult{}, ctx.Err()
}

var _ tool.Tool = (*blockingTool)(nil)

// newServiceWithEngine builds a Service over a memstore whose engine uses the
// given LLM + catalog, so a run can be driven to a real cancelled state through
// the Service surface.
func newServiceWithEngine(t *testing.T, llm port.LLMProvider, cat *tool.Catalog) (*server.Service, port.SessionStore) {
	t.Helper()
	store := memstore.New()
	engine := agent.NewEngine(agent.Deps{
		LLM:     llm,
		Catalog: cat,
		Policy:  permpolicy.NewPolicy([]governance.Rule{{Effect: governance.Allow}}, nil),
		Model:   "test-model",
		Store:   store,
	})
	svc, err := server.NewService(server.Config{
		Engine:     engine,
		Store:      store,
		Workspaces: func(root string) tool.Workspace { return memfs.NewWorkspace(root) },
		Now:        func() time.Time { return time.Unix(0, 0) },
	})
	if err != nil {
		t.Fatalf("new service: %v", err)
	}
	return svc, store
}

// TestStartRunContentRecoversCancelledSession is THE regression test: a run is
// driven to StateCancelled (blocking tool + Cancel), then a SECOND StartRunContent
// on the same session must NOT return the "record user prompt … from cancelled"
// wedge error and must produce a terminal result.
func TestStartRunContentRecoversCancelledSession(t *testing.T) {
	bt := &blockingTool{started: make(chan struct{})}
	cat := tool.NewCatalog()
	cat.MustRegister(bt)
	// Turn-1: a tool call to the blocking tool. Turn-2: a clean end-of-turn.
	llm := mockllm.New(
		mockllm.ToolCallTurn(session.NewToolCall("c1", "Read", json.RawMessage(`{"path":"a.go"}`))),
		mockllm.ChunksTurn(mockllm.TextChunk("all done"), mockllm.DoneChunk(session.StopEndTurn)),
	)
	svc, _ := newServiceWithEngine(t, llm, cat)

	sess, err := svc.CreateSession(context.Background(), "/ws", session.ModeDefault, session.Limits{})
	if err != nil {
		t.Fatalf("CreateSession: %v", err)
	}

	run, err := svc.StartRunContent(context.Background(), sess.ID, "look at a.go", nil)
	if err != nil {
		t.Fatalf("first StartRunContent: %v", err)
	}
	<-bt.started
	run.Cancel()
	drainRun(t, run)

	reloaded, err := svc.GetSession(context.Background(), sess.ID)
	if err != nil {
		t.Fatalf("GetSession: %v", err)
	}
	if reloaded.State != session.StateCancelled {
		t.Fatalf("after cancel state = %q, want cancelled", reloaded.State)
	}

	// The regression: a second prompt must be accepted, not wedged.
	run2, err := svc.StartRunContent(context.Background(), sess.ID, "second prompt", nil)
	if err != nil {
		t.Fatalf("second StartRunContent returned error (wedge?): %v", err)
	}
	var sawResult bool
	for ev := range run2.Events() {
		if ev.Type == session.EvResult {
			sawResult = true
			if ev.Result.Stop != session.StopEndTurn {
				t.Fatalf("second run stop = %q, want end_turn", ev.Result.Stop)
			}
		}
	}
	if !sawResult {
		t.Fatalf("second run produced no terminal result")
	}
}

// TestLoadSessionRecoversCancelledViaInterrupt confirms a persisted cancelled
// session is recovered to StateIdle (via Interrupt) and re-persisted.
func TestLoadSessionRecoversCancelledViaInterrupt(t *testing.T) {
	store := memstore.New()
	svc := newServiceWithStore(t, store)
	sess, err := svc.CreateSession(context.Background(), "/ws", session.ModeDefault, session.Limits{})
	if err != nil {
		t.Fatalf("CreateSession: %v", err)
	}
	// Drive to a cancelled terminal state with an orphaned tool call, persist it.
	if err := sess.RecordUserPrompt("go", nil); err != nil {
		t.Fatalf("RecordUserPrompt: %v", err)
	}
	if err := sess.BeginTurn(); err != nil {
		t.Fatalf("BeginTurn: %v", err)
	}
	calls := []session.ToolCall{session.NewToolCall("c1", "Read", nil)}
	if err := sess.RecordAssistant(session.NewAssistantMessage("", "", calls)); err != nil {
		t.Fatalf("RecordAssistant: %v", err)
	}
	if err := sess.Cancel(); err != nil {
		t.Fatalf("Cancel: %v", err)
	}
	if err := store.Save(context.Background(), sess); err != nil {
		t.Fatalf("Save: %v", err)
	}

	loaded, err := svc.LoadSession(context.Background(), sess.ID)
	if err != nil {
		t.Fatalf("LoadSession: %v", err)
	}
	if loaded.State != session.StateIdle {
		t.Fatalf("loaded state = %q, want idle (interrupted)", loaded.State)
	}
	// History was repaired: the orphaned c1 now has a synthetic error result.
	var found bool
	for _, m := range loaded.Conversation.Messages {
		if m.Role == session.RoleTool && m.ToolResult != nil && m.ToolResult.CallID == "c1" && m.ToolResult.IsError {
			found = true
		}
	}
	if !found {
		t.Fatalf("interrupted history missing synthetic result for c1")
	}
	// Persisted: a fresh load reflects StateIdle.
	persisted, err := svc.GetSession(context.Background(), sess.ID)
	if err != nil {
		t.Fatalf("GetSession: %v", err)
	}
	if persisted.State != session.StateIdle {
		t.Fatalf("persisted state = %q, want idle", persisted.State)
	}
}

// TestLoadSessionDoesNotRecoverFailed confirms a persisted FAILED session stays
// non-resumable: LoadSession leaves it StateFailed (the next run surfaces the
// illegal transition).
func TestLoadSessionDoesNotRecoverFailed(t *testing.T) {
	store := memstore.New()
	svc := newServiceWithStore(t, store)
	sess, err := svc.CreateSession(context.Background(), "/ws", session.ModeDefault, session.Limits{})
	if err != nil {
		t.Fatalf("CreateSession: %v", err)
	}
	if err := sess.Fail(); err != nil {
		t.Fatalf("Fail: %v", err)
	}
	if err := store.Save(context.Background(), sess); err != nil {
		t.Fatalf("Save: %v", err)
	}

	loaded, err := svc.LoadSession(context.Background(), sess.ID)
	if err != nil {
		t.Fatalf("LoadSession: %v", err)
	}
	if loaded.State != session.StateFailed {
		t.Fatalf("loaded state = %q, want failed (not recovered)", loaded.State)
	}
}
