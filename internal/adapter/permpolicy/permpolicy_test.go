package permpolicy_test

import (
	"context"
	"encoding/json"
	"testing"

	"github.com/stacklok/mecatl/internal/adapter/permpolicy"
	"github.com/stacklok/mecatl/internal/adapter/permstore"
	"github.com/stacklok/mecatl/internal/governance"
	"github.com/stacklok/mecatl/internal/port"
	"github.com/stacklok/mecatl/internal/session"
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

const sid = session.SessionID("s1")

// gauntlet #8 end-to-end through the port: deny beats allow across scopes.
func TestPolicyDenyBeatsAllow(t *testing.T) {
	p := permpolicy.NewPolicy([]governance.Rule{
		{Scope: governance.ScopeManaged, Tool: "Bash", Pattern: "rm *", Effect: governance.Allow},
		{Scope: governance.ScopeUser, Tool: "Bash", Pattern: "rm *", Effect: governance.Deny},
	}, nil)
	got := p.Evaluate(context.Background(), sid, session.ModeDefault, bashCall("rm x"))
	if got.Effect != governance.Deny {
		t.Fatalf("expected Deny, got %v", got.Effect)
	}
}

// gauntlet #3 through the port: plan mode denies Write, allows Read.
func TestPolicyPlanMode(t *testing.T) {
	p := permpolicy.NewPolicy([]governance.Rule{{Effect: governance.Allow}}, nil)

	if got := p.Evaluate(context.Background(), sid, session.ModePlan, fileCall("Write", "/x")); got.Effect != governance.Deny {
		t.Fatalf("plan mode Write: expected Deny, got %v", got.Effect)
	}
	if got := p.Evaluate(context.Background(), sid, session.ModePlan, fileCall("Read", "/x")); got.Effect != governance.Allow {
		t.Fatalf("plan mode Read: expected Allow, got %v", got.Effect)
	}
	// Non-plan mode lets the mutating tool through (rule allows it).
	if got := p.Evaluate(context.Background(), sid, session.ModeDefault, fileCall("Write", "/x")); got.Effect != governance.Allow {
		t.Fatalf("default mode Write: expected Allow, got %v", got.Effect)
	}
}

// Learn then Evaluate: an allow-always learned for `git status` makes the next
// `git status` resolve Allow (it would otherwise be the default Ask).
func TestPolicyLearnThenAllow(t *testing.T) {
	p := permpolicy.NewPolicy(nil, permstore.New())
	if got := p.Evaluate(context.Background(), sid, session.ModeDefault, bashCall("git status")); got.Effect != governance.Ask {
		t.Fatalf("pre-learn: expected Ask, got %v", got.Effect)
	}
	p.Learn(sid, bashCall("git status"))
	if got := p.Evaluate(context.Background(), sid, session.ModeDefault, bashCall("git status")); got.Effect != governance.Allow {
		t.Fatalf("post-learn: expected Allow, got %v", got.Effect)
	}
}

// A learned allow NEVER overrides a static deny.
func TestPolicyLearnedAllowCannotOverrideDeny(t *testing.T) {
	p := permpolicy.NewPolicy([]governance.Rule{
		{Scope: governance.ScopeManaged, Tool: "Bash", Pattern: "git status", Effect: governance.Deny},
	}, permstore.New())
	p.Learn(sid, bashCall("git status"))
	if got := p.Evaluate(context.Background(), sid, session.ModeDefault, bashCall("git status")); got.Effect != governance.Deny {
		t.Fatalf("expected Deny to survive a learned allow, got %v", got.Effect)
	}
}

// A learned allow does NOT bypass plan-mode mutation denial.
func TestPolicyLearnedAllowDoesNotBypassPlanMode(t *testing.T) {
	p := permpolicy.NewPolicy(nil, permstore.New())
	p.Learn(sid, fileCall("Write", "/x"))
	if got := p.Evaluate(context.Background(), sid, session.ModePlan, fileCall("Write", "/x")); got.Effect != governance.Deny {
		t.Fatalf("plan mode must still deny Write despite learned allow, got %v", got.Effect)
	}
}

// Cross-session isolation: a rule learned in session A is invisible to session B.
func TestPolicyLearnedRuleSessionIsolation(t *testing.T) {
	p := permpolicy.NewPolicy(nil, permstore.New())
	p.Learn("A", bashCall("git status"))
	if got := p.Evaluate(context.Background(), "A", session.ModeDefault, bashCall("git status")); got.Effect != governance.Allow {
		t.Fatalf("session A should see its learned allow, got %v", got.Effect)
	}
	if got := p.Evaluate(context.Background(), "B", session.ModeDefault, bashCall("git status")); got.Effect != governance.Ask {
		t.Fatalf("session B must NOT see session A's learned rule, got %v", got.Effect)
	}
}

// A compound Bash command is refused by Learn (no-op): nothing is recorded, so a
// later single `git status` still asks (the compound's first segment was NOT
// silently learned as a tool+pattern allow).
func TestPolicyLearnRefusesCompound(t *testing.T) {
	p := permpolicy.NewPolicy(nil, permstore.New())
	p.Learn(sid, bashCall("git status; rm -rf /"))
	if got := p.Evaluate(context.Background(), sid, session.ModeDefault, bashCall("git status")); got.Effect != governance.Ask {
		t.Fatalf("a compound learn must not have recorded `git status`, got %v", got.Effect)
	}
}

// With a nil store, Learn is a no-op and Evaluate is the pure static policy.
func TestPolicyNilStoreLearnIsNoop(t *testing.T) {
	p := permpolicy.NewPolicy(nil, nil)
	p.Learn(sid, bashCall("git status")) // must not panic
	if got := p.Evaluate(context.Background(), sid, session.ModeDefault, bashCall("git status")); got.Effect != governance.Ask {
		t.Fatalf("nil-store policy should stay Ask, got %v", got.Effect)
	}
}
