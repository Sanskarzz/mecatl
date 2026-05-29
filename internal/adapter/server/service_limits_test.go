package server_test

import (
	"context"
	"testing"

	"github.com/stacklok/ozzharness/internal/adapter/memfs"
	"github.com/stacklok/ozzharness/internal/adapter/mockllm"
	"github.com/stacklok/ozzharness/internal/adapter/permpolicy"
	"github.com/stacklok/ozzharness/internal/adapter/server"
	"github.com/stacklok/ozzharness/internal/adapter/store/memstore"
	"github.com/stacklok/ozzharness/internal/agent"
	"github.com/stacklok/ozzharness/internal/governance"
	"github.com/stacklok/ozzharness/internal/session"
	"github.com/stacklok/ozzharness/internal/tool"
)

// newLimitsService builds a minimal Service with the given DefaultLimits.
func newLimitsService(t *testing.T, def session.Limits) *server.Service {
	t.Helper()
	engine := agent.NewEngine(agent.Deps{
		LLM:     mockllm.New(),
		Catalog: tool.NewCatalog(),
		Policy:  permpolicy.NewPolicy([]governance.Rule{{Effect: governance.Allow}}),
		Model:   "test-model",
	})
	svc, err := server.NewService(server.Config{
		Engine:        engine,
		Store:         memstore.New(),
		Workspaces:    func(root string) tool.Workspace { return memfs.NewWorkspace(root) },
		DefaultLimits: def,
	})
	if err != nil {
		t.Fatalf("new service: %v", err)
	}
	return svc
}

// Finding 3: a session created with empty (all-zero) limits must inherit the
// injected DefaultLimits so it is bounded rather than running unbounded.
func TestCreateSessionAppliesDefaultLimits(t *testing.T) {
	def := session.Limits{MaxTurns: 50, MaxToolCalls: 200, MaxConsecutiveFailures: 5}
	svc := newLimitsService(t, def)

	sess, err := svc.CreateSession(context.Background(), "/ws", "", session.Limits{})
	if err != nil {
		t.Fatalf("CreateSession: %v", err)
	}
	if sess.Limits != def {
		t.Fatalf("default limits not applied: got %+v, want %+v", sess.Limits, def)
	}
}

// Explicit limits supplied by the caller must NOT be overridden by the defaults.
func TestCreateSessionKeepsExplicitLimits(t *testing.T) {
	def := session.Limits{MaxTurns: 50, MaxToolCalls: 200, MaxConsecutiveFailures: 5}
	svc := newLimitsService(t, def)

	explicit := session.Limits{MaxTurns: 3}
	sess, err := svc.CreateSession(context.Background(), "/ws", "", explicit)
	if err != nil {
		t.Fatalf("CreateSession: %v", err)
	}
	if sess.Limits != explicit {
		t.Fatalf("explicit limits overridden: got %+v, want %+v", sess.Limits, explicit)
	}
}
