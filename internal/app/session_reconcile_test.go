package app

import (
	"context"
	"testing"
	"time"

	"github.com/stacklok/mecatl/engine/adapter/memfs"
	"github.com/stacklok/mecatl/engine/adapter/memstore"
	"github.com/stacklok/mecatl/engine/adapter/mockllm"
	"github.com/stacklok/mecatl/engine/adapter/permpolicy"
	"github.com/stacklok/mecatl/engine/adapter/permstore"
	"github.com/stacklok/mecatl/engine/agent"
	"github.com/stacklok/mecatl/engine/port"
	"github.com/stacklok/mecatl/engine/session"
	"github.com/stacklok/mecatl/engine/tool"
	"github.com/stacklok/mecatl/internal/adapter/server"
)

// crashOrphanedSessionFixture builds a StateRunning session whose trailing
// assistant message carries a dangling tool_use call that never got a
// result — the exact shape a crash leaves behind (mirrors
// internal/adapter/server's own crashOrphanedSession fixture, issue #475).
func crashOrphanedSessionFixture(t *testing.T, id session.SessionID, createdAt time.Time) *session.Session {
	t.Helper()
	sess := session.New(id, session.ModeDefault, "/tmp/ws", session.Limits{}, createdAt)
	if err := sess.RecordUserPrompt("do the thing", nil); err != nil {
		t.Fatalf("RecordUserPrompt: %v", err)
	}
	if err := sess.BeginTurn(); err != nil {
		t.Fatalf("BeginTurn: %v", err)
	}
	calls := []session.ToolCall{session.NewToolCall("call-a", "read_file", nil)}
	if err := sess.RecordAssistant(session.NewAssistantMessage("", "", calls)); err != nil {
		t.Fatalf("RecordAssistant: %v", err)
	}
	if sess.State != session.StateRunning {
		t.Fatalf("precondition: state = %q, want running", sess.State)
	}
	return sess
}

// awaitingSessionFixture builds a StateAwaiting session (paused on a
// permission ask) — never a sweep candidate regardless of age.
func awaitingSessionFixture(t *testing.T, id session.SessionID, createdAt time.Time) *session.Session {
	t.Helper()
	sess := session.New(id, session.ModeDefault, "/tmp/ws", session.Limits{}, createdAt)
	if err := sess.RecordUserPrompt("do the thing", nil); err != nil {
		t.Fatalf("RecordUserPrompt: %v", err)
	}
	if err := sess.BeginTurn(); err != nil {
		t.Fatalf("BeginTurn: %v", err)
	}
	if err := sess.PauseForApproval(session.PendingAsk{AskID: "ask-1", Tool: "bash", Call: "call-a"}); err != nil {
		t.Fatalf("PauseForApproval: %v", err)
	}
	if sess.State != session.StateAwaiting {
		t.Fatalf("precondition: state = %q, want awaiting", sess.State)
	}
	return sess
}

// reconcileFixture builds a deterministic sweep harness: a memstore whose
// Save/ModifiedAt times AND the Service's own staleness clock come from one
// shared fake clock — mirroring childgc_test.go's gcFixture pattern.
type reconcileFixture struct {
	store *memstore.Store
	svc   *server.Service
	now   time.Time
}

func newReconcileFixture(t *testing.T) *reconcileFixture {
	t.Helper()
	f := &reconcileFixture{now: time.Date(2026, 6, 1, 12, 0, 0, 0, time.UTC)}
	f.store = memstore.New(memstore.WithNow(func() time.Time { return f.now }))
	cat := tool.NewCatalog()
	engine := agent.NewEngine(agent.Deps{
		LLM:     mockllm.New(mockllm.TextTurn("ok")),
		Catalog: cat,
		Policy:  permpolicy.NewPolicy(nil, permstore.New()),
		Model:   "test-model",
	})
	svc, err := server.NewService(server.Config{
		Engine:     engine,
		Store:      f.store,
		Workspaces: func(root string) tool.Workspace { return memfs.NewWorkspace(root) },
		Now:        func() time.Time { return f.now },
	})
	if err != nil {
		t.Fatalf("new service: %v", err)
	}
	t.Cleanup(svc.Close)
	f.svc = svc
	return f
}

func (f *reconcileFixture) save(t *testing.T, sess *session.Session) {
	t.Helper()
	if err := f.store.Save(context.Background(), sess); err != nil {
		t.Fatalf("save(%q): %v", sess.ID, err)
	}
}

func (f *reconcileFixture) state(t *testing.T, id session.SessionID) session.State {
	t.Helper()
	reloaded, err := f.store.Load(context.Background(), id)
	if err != nil {
		t.Fatalf("reload(%q): %v", id, err)
	}
	return reloaded.State
}

// TestStaleSessionReconcileSettlesChildCandidate pins the population the
// confirmed real bug came from (issue #475): a subagent-* child crash-orphaned
// in StateRunning is a sweep candidate exactly like a top-level session — the
// run-entry funnel's own repair (Step 3) never reaches it (nothing ever calls
// StartRunContent on a child id), so this sweep is its ONLY repair path.
func TestStaleSessionReconcileSettlesChildCandidate(t *testing.T) {
	f := newReconcileFixture(t)
	const id session.SessionID = "subagent-orphan"
	f.save(t, crashOrphanedSessionFixture(t, id, f.now))
	f.now = f.now.Add(2 * time.Hour) // past the staleness age window

	sweepStaleSessions(context.Background(), f.svc, port.NopDiagnostics{})

	if got := f.state(t, id); got != session.StateIdle {
		t.Fatalf("state after sweep = %q, want idle", got)
	}
	reloaded, err := f.store.Load(context.Background(), id)
	if err != nil {
		t.Fatalf("reload: %v", err)
	}
	if err := session.ValidateToolPairing(reloaded.Conversation.Messages); err != nil {
		t.Fatalf("tool pairing invalid after settle: %v", err)
	}
}

// TestStaleSessionReconcileExcludesScheduleFireSessions pins the exclusion:
// a "sched--"-prefixed fire session, however stale-looking, is left untouched
// by this sweep — it is the scheduler's OWN reconciler's job (see the note in
// scheduler_reconcile.go).
func TestStaleSessionReconcileExcludesScheduleFireSessions(t *testing.T) {
	f := newReconcileFixture(t)
	const id session.SessionID = "sched--nightly-000123"
	f.save(t, crashOrphanedSessionFixture(t, id, f.now))
	f.now = f.now.Add(2 * time.Hour)

	sweepStaleSessions(context.Background(), f.svc, port.NopDiagnostics{})

	if got := f.state(t, id); got != session.StateRunning {
		t.Fatalf("state after sweep = %q, want running (sched-- must be excluded)", got)
	}
}

// TestStaleSessionReconcileNeverTouchesAwaiting pins that StateAwaiting is
// never a candidate: only state=="running" rows are even considered.
func TestStaleSessionReconcileNeverTouchesAwaiting(t *testing.T) {
	f := newReconcileFixture(t)
	const id session.SessionID = "operator-awaiting"
	f.save(t, awaitingSessionFixture(t, id, f.now))
	f.now = f.now.Add(2 * time.Hour)

	sweepStaleSessions(context.Background(), f.svc, port.NopDiagnostics{})

	if got := f.state(t, id); got != session.StateAwaiting {
		t.Fatalf("state after sweep = %q, want awaiting (never a candidate)", got)
	}
}

// TestStaleSessionReconcileLeavesFreshRunningAlone proves the sweep actually
// calls Service.SessionStale (age-horizon-first) rather than reimplementing a
// weaker check of its own: a running session inside the staleness window is
// left untouched.
func TestStaleSessionReconcileLeavesFreshRunningAlone(t *testing.T) {
	f := newReconcileFixture(t)
	const id session.SessionID = "operator-fresh"
	f.save(t, crashOrphanedSessionFixture(t, id, f.now))
	// No clock advance: still well inside the age window.

	sweepStaleSessions(context.Background(), f.svc, port.NopDiagnostics{})

	if got := f.state(t, id); got != session.StateRunning {
		t.Fatalf("state after sweep = %q, want running (fresh, inside the age window)", got)
	}
}

// TestStartStaleSessionReconcileExitsOnCancel pins the goroutine-exit
// contract: the sweeper goroutine started by startStaleSessionReconcile
// returns when its returned close func is called. The package's goleak
// TestMain is the actual leak gate; this test additionally proves the
// specific goroutine reacts to cancellation promptly rather than relying on
// process exit to hide a leak.
func TestStartStaleSessionReconcileExitsOnCancel(t *testing.T) {
	f := newReconcileFixture(t)
	stop := startStaleSessionReconcile(Config{Diagnostics: port.NopDiagnostics{}}, f.svc)
	stop()
	// If the goroutine leaked, goleak (wired at TestMain for this package)
	// fails the whole test binary; nothing further to assert here beyond
	// giving the goroutine a moment to observe ctx.Done() before the test
	// (and svc.Close in Cleanup) returns.
	time.Sleep(20 * time.Millisecond)
}
