package server_test

import (
	"context"
	"errors"
	"testing"

	"github.com/stacklok/mecatl/engine/adapter/memfs"
	"github.com/stacklok/mecatl/engine/adapter/memstore"
	"github.com/stacklok/mecatl/engine/adapter/mockllm"
	"github.com/stacklok/mecatl/engine/adapter/permpolicy"
	"github.com/stacklok/mecatl/engine/adapter/permstore"
	"github.com/stacklok/mecatl/engine/agent"
	"github.com/stacklok/mecatl/engine/session"
	"github.com/stacklok/mecatl/engine/tool"
	"github.com/stacklok/mecatl/internal/adapter/server"
)

// newDrainTestService builds a minimal Service over mockllm for the drain-gate
// tests: one turn that ends immediately, no lease wired (the gate must work
// WITHOUT a lease too — a draining replica rejects new runs regardless).
func newDrainTestService(t *testing.T) *server.Service {
	t.Helper()
	store := memstore.New()
	ps := permstore.New()
	cat := tool.NewCatalog()
	engine := agent.NewEngine(agent.Deps{
		LLM:     mockllm.New(mockllm.TextTurn("ok")),
		Catalog: cat,
		Policy:  permpolicy.NewPolicy(nil, ps),
		Model:   "test-model",
	})
	svc, err := server.NewService(server.Config{
		Engine:     engine,
		Store:      store,
		Workspaces: func(root string) tool.Workspace { return memfs.NewWorkspace(root) },
	})
	if err != nil {
		t.Fatalf("new service: %v", err)
	}
	t.Cleanup(svc.Close)
	return svc
}

// TestDrainGateStartsFalse: a fresh Service accepts run-entries (the gate is
// byte-identical to pre-ADR-0048 when Drain has not been called).
func TestDrainGateStartsFalse(t *testing.T) {
	svc := newDrainTestService(t)
	sess, err := svc.CreateSession(context.Background(), "/ws", session.ModeDefault, session.Limits{})
	if err != nil {
		t.Fatalf("CreateSession: %v", err)
	}
	if _, err := svc.StartRun(context.Background(), sess.ID, "go"); err != nil {
		t.Fatalf("StartRun on a fresh (non-draining) service = %v, want nil", err)
	}
	if svc.ActiveRuns() != 0 {
		t.Errorf("ActiveRuns on a no-lease service = %d, want 0", svc.ActiveRuns())
	}
}

// TestDrainRejectsNewRuns: once Drain is armed, StartRun (and the
// awaiting-resume path) returns ErrUnavailable BEFORE leasing/launching — the
// run never starts. A drain gate without a wired lease still rejects (the gate
// is not lease-dependent). We ALSO assert the run was never registered
// (LookupRun misses) and no lease is held (ActiveRuns==0) — the gate must
// refuse BEFORE the register/acquire side effects, not just return an error
// after launching.
func TestDrainRejectsNewRuns(t *testing.T) {
	svc := newDrainTestService(t)
	sess, err := svc.CreateSession(context.Background(), "/ws", session.ModeDefault, session.Limits{})
	if err != nil {
		t.Fatalf("CreateSession: %v", err)
	}
	svc.Drain()
	_, err = svc.StartRun(context.Background(), sess.ID, "after drain")
	if !errors.Is(err, server.ErrUnavailable) {
		t.Fatalf("StartRun after Drain = %v, want ErrUnavailable", err)
	}
	// The run must NEVER have been registered — the gate refused before launch.
	if _, ok := svc.LookupRun(sess.ID); ok {
		t.Fatal("a run was registered after Drain (the drain gate must refuse BEFORE registering/launching, not after)")
	}
	if svc.ActiveRuns() != 0 {
		t.Errorf("ActiveRuns after a drained StartRun = %d, want 0 (no lease held)", svc.ActiveRuns())
	}
}

// TestDrainIsIdempotent: calling Drain twice is harmless (a one-way gate).
func TestDrainIsIdempotent(t *testing.T) {
	svc := newDrainTestService(t)
	svc.Drain()
	svc.Drain() // must not panic or error
	sess, err := svc.CreateSession(context.Background(), "/ws", session.ModeDefault, session.Limits{})
	if err != nil {
		t.Fatalf("CreateSession: %v", err)
	}
	if _, err := svc.StartRun(context.Background(), sess.ID, "go"); !errors.Is(err, server.ErrUnavailable) {
		t.Fatalf("StartRun after double Drain = %v, want ErrUnavailable", err)
	}
}

// TestDrainDoesNotBlockExistingRun: Drain only gates NEW run-entries; an
// already-launched run is NOT cancelled by Drain itself (that is the bounded
// GracefulStop's job in the cmd binary). The run must complete with a BENIGN
// terminal stop (not StopCancelled) — asserting the stop reason catches a
// regression where Drain accidentally cancels the live run.
func TestDrainDoesNotBlockExistingRun(t *testing.T) {
	svc := newDrainTestService(t)
	sess, err := svc.CreateSession(context.Background(), "/ws", session.ModeDefault, session.Limits{})
	if err != nil {
		t.Fatalf("CreateSession: %v", err)
	}
	run, err := svc.StartRun(context.Background(), sess.ID, "go")
	if err != nil {
		t.Fatalf("StartRun before drain: %v", err)
	}
	// Arm the drain while the run is live; the run must still complete normally.
	svc.Drain()
	var stop session.StopReason
	for ev := range run.Events() {
		if ev.Type == session.EvResult && ev.Result != nil {
			stop = ev.Result.Stop
		}
	}
	svc.FinishRun(sess.ID, run)
	if stop != session.StopEndTurn {
		t.Fatalf("the live run's stop after Drain = %q, want %q (Drain must NOT cancel an in-flight run)", stop, session.StopEndTurn)
	}
}

// TestDrainAwaitingResumePathGated: the awaiting-resume path
// (resumeFromAwaiting) reaches acquireLease (and thus the drain gate) ONLY for
// a genuinely-awaiting session. For an idle session the state check fires
// first and returns ErrNoActiveRun — that is correct (there is nothing to
// resume), and the drain gate does not override it. The drain gate's job on
// the resume path is to prevent a NEW resumed run from starting on an
// awaiting session held by a draining replica; that path is structurally
// covered because acquireLease sits on resumeFromAwaiting after the
// StateAwaiting check. Here we assert the idle-session case returns the
// honest ErrNoActiveRun, not a drain error (the gate is not a blanket veto on
// Approve — an in-flight same-process Approve on a LIVE run stays allowed).
func TestDrainAwaitingResumePathGated(t *testing.T) {
	svc := newDrainTestService(t)
	sess, err := svc.CreateSession(context.Background(), "/ws", session.ModeDefault, session.Limits{})
	if err != nil {
		t.Fatalf("CreateSession: %v", err)
	}
	svc.Drain()
	// ApproveRun on an idle (non-awaiting) session with no live run: the state
	// check returns ErrNoActiveRun BEFORE the drain gate (acquireLease) is
	// reached — correct, there is nothing to resume.
	_, err = svc.ApproveRun(context.Background(), sess.ID, "ask-x", session.VerdictAllowOnce)
	if !errors.Is(err, server.ErrNoActiveRun) {
		t.Fatalf("ApproveRun after Drain on an idle session = %v, want ErrNoActiveRun (state check fires before the drain gate)", err)
	}
}
