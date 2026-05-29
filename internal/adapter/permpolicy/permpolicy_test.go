package permpolicy_test

import (
	"context"
	"encoding/json"
	"testing"

	"github.com/stacklok/ozzharness/internal/adapter/permpolicy"
	"github.com/stacklok/ozzharness/internal/governance"
	"github.com/stacklok/ozzharness/internal/port"
	"github.com/stacklok/ozzharness/internal/session"
)

// Compile-time assertion that Policy satisfies the frozen port interface.
var _ port.PermissionPolicy = (*permpolicy.Policy)(nil)

func bashCall(cmd string) session.ToolCall {
	args, _ := json.Marshal(map[string]string{"command": cmd})
	return session.NewToolCall("c1", "Bash", args)
}

func fileCall(tool, p string) session.ToolCall {
	args, _ := json.Marshal(map[string]string{"file_path": p})
	return session.NewToolCall("c1", tool, args)
}

// gauntlet #8 end-to-end through the port: deny beats allow across scopes.
func TestPolicyDenyBeatsAllow(t *testing.T) {
	p := permpolicy.NewPolicy([]governance.Rule{
		{Scope: governance.ScopeManaged, Tool: "Bash", Pattern: "rm *", Effect: governance.Allow},
		{Scope: governance.ScopeUser, Tool: "Bash", Pattern: "rm *", Effect: governance.Deny},
	})
	got := p.Evaluate(context.Background(), session.ModeDefault, bashCall("rm x"))
	if got.Effect != governance.Deny {
		t.Fatalf("expected Deny, got %v", got.Effect)
	}
}

// gauntlet #3 through the port: plan mode denies Write, allows Read.
func TestPolicyPlanMode(t *testing.T) {
	p := permpolicy.NewPolicy([]governance.Rule{{Effect: governance.Allow}})

	if got := p.Evaluate(context.Background(), session.ModePlan, fileCall("Write", "/x")); got.Effect != governance.Deny {
		t.Fatalf("plan mode Write: expected Deny, got %v", got.Effect)
	}
	if got := p.Evaluate(context.Background(), session.ModePlan, fileCall("Read", "/x")); got.Effect != governance.Allow {
		t.Fatalf("plan mode Read: expected Allow, got %v", got.Effect)
	}
	// Non-plan mode lets the mutating tool through (rule allows it).
	if got := p.Evaluate(context.Background(), session.ModeDefault, fileCall("Write", "/x")); got.Effect != governance.Allow {
		t.Fatalf("default mode Write: expected Allow, got %v", got.Effect)
	}
}
