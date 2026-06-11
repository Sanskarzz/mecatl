package server_test

import (
	"context"
	"testing"
	"time"

	"github.com/stacklok/mecatl/engine/adapter/mockllm"
	"github.com/stacklok/mecatl/engine/session"
)

// TestServiceIsLiveTracksRunRegistry pins the IsLive contract the composition
// layer's child-session GC relies on: false for an unknown id, false for a
// created-but-runless session, true from StartRunContent (registration is
// synchronous) until FinishRun, false after.
func TestServiceIsLiveTracksRunRegistry(t *testing.T) {
	svc := newService(t, mockllm.New(mockllm.TextTurn("ok")), allowRules())
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()

	if svc.IsLive("never-seen") {
		t.Error("IsLive(unknown id) = true, want false")
	}
	sess, err := svc.CreateSession(ctx, "/ws", session.ModeDefault, session.Limits{})
	if err != nil {
		t.Fatalf("CreateSession: %v", err)
	}
	if svc.IsLive(sess.ID) {
		t.Error("IsLive(created, runless session) = true, want false")
	}

	run, err := svc.StartRunContent(ctx, sess.ID, "go", nil)
	if err != nil {
		t.Fatalf("StartRunContent: %v", err)
	}
	if !svc.IsLive(sess.ID) {
		t.Error("IsLive(mid-run session) = false, want true")
	}
	for range run.Events() {
		// drain to the terminal; the run stays registered until FinishRun
	}
	if !svc.IsLive(sess.ID) {
		t.Error("IsLive(drained, not yet FinishRun) = false, want true (the registry, not the run state, is the source)")
	}
	svc.FinishRun(sess.ID, run)
	if svc.IsLive(sess.ID) {
		t.Error("IsLive(after FinishRun) = true, want false")
	}
}

// TestServiceIsLiveDoesNotKnowEngineChildren is the HONESTY pin: IsLive reads
// the TOP-LEVEL run registry only, so an engine-spawned child id
// ("subagent-<callID>" etc.) answers FALSE even while its parent run — the run
// that is actually driving it — is live. The composition layer's child-session
// GC therefore cannot rely on the live-skip to protect engine children;
// their protection is age horizon + snapshot freshness (children persist at
// their terminal, and a resumed child re-persists at resume start) — see the
// invariant note on childGC.isLive in internal/app/childgc.go.
func TestServiceIsLiveDoesNotKnowEngineChildren(t *testing.T) {
	svc := newService(t, mockllm.New(mockllm.TextTurn("ok")), allowRules())
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()

	sess, err := svc.CreateSession(ctx, "/ws", session.ModeDefault, session.Limits{})
	if err != nil {
		t.Fatalf("CreateSession: %v", err)
	}
	run, err := svc.StartRunContent(ctx, sess.ID, "go", nil)
	if err != nil {
		t.Fatalf("StartRunContent: %v", err)
	}
	defer func() {
		for range run.Events() {
		}
		svc.FinishRun(sess.ID, run)
	}()

	if !svc.IsLive(sess.ID) {
		t.Fatal("precondition: the parent run must be live")
	}
	for _, child := range []session.SessionID{
		"subagent-" + sess.ID, "parallel-" + sess.ID + "-0", "team-" + sess.ID + "-lead",
	} {
		if svc.IsLive(child) {
			t.Errorf("IsLive(%q) = true; engine-child ids must NOT appear in the top-level run registry", child)
		}
	}
}
