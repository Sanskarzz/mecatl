package server_test

import (
	"context"
	"errors"
	"testing"
	"time"

	"github.com/stacklok/mecatl/internal/adapter/memfs"
	"github.com/stacklok/mecatl/internal/adapter/mockllm"
	"github.com/stacklok/mecatl/internal/adapter/permpolicy"
	"github.com/stacklok/mecatl/internal/adapter/server"
	"github.com/stacklok/mecatl/internal/adapter/store/jsonlstore"
	"github.com/stacklok/mecatl/internal/adapter/store/memstore"
	"github.com/stacklok/mecatl/internal/agent"
	"github.com/stacklok/mecatl/internal/governance"
	"github.com/stacklok/mecatl/internal/port"
	"github.com/stacklok/mecatl/internal/session"
	"github.com/stacklok/mecatl/internal/tool"
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
