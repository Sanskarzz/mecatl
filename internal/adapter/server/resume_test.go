package server_test

import (
	"context"
	"errors"
	"testing"
	"time"

	"github.com/stacklok/ozzharness/internal/adapter/memfs"
	"github.com/stacklok/ozzharness/internal/adapter/mockllm"
	"github.com/stacklok/ozzharness/internal/adapter/permpolicy"
	"github.com/stacklok/ozzharness/internal/adapter/server"
	"github.com/stacklok/ozzharness/internal/adapter/store/jsonlstore"
	"github.com/stacklok/ozzharness/internal/agent"
	"github.com/stacklok/ozzharness/internal/governance"
	"github.com/stacklok/ozzharness/internal/port"
	"github.com/stacklok/ozzharness/internal/session"
	"github.com/stacklok/ozzharness/internal/tool"
)

// newServiceWithStore builds a Service over the given (shared) store, so two
// Services can be pointed at the same durable store to simulate a restart.
func newServiceWithStore(t *testing.T, store port.SessionStore) *server.Service {
	t.Helper()
	engine := agent.NewEngine(agent.Deps{
		LLM:     mockllm.New(),
		Catalog: tool.NewCatalog(),
		Policy:  permpolicy.NewPolicy([]governance.Rule{{Effect: governance.Allow}}),
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
	if err := svc2.Approve(context.Background(), sess.ID, "ask-1", true); !errors.Is(err, server.ErrNoActiveRun) {
		t.Fatalf("Approve known/runless = %v, want ErrNoActiveRun", err)
	}
	if err := svc2.Cancel(context.Background(), sess.ID); !errors.Is(err, server.ErrNoActiveRun) {
		t.Fatalf("Cancel known/runless = %v, want ErrNoActiveRun", err)
	}

	// Unknown session -> ErrNotFound.
	if err := svc2.Approve(context.Background(), "does-not-exist", "ask-1", true); !errors.Is(err, server.ErrNotFound) {
		t.Fatalf("Approve unknown = %v, want ErrNotFound", err)
	}
}
