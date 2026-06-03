package app

import (
	"context"
	"encoding/json"
	"testing"

	"github.com/stacklok/mecatl/internal/adapter/permpolicy"
	"github.com/stacklok/mecatl/internal/governance"
	"github.com/stacklok/mecatl/internal/session"
)

// TestMainRulesInjectsAllowAllWhenSet proves the AllowAllTools posture injects a
// single ScopeCLI allow-all rule into the main engine's static ruleset, and is a
// pure no-op (identical to defaultRules) when unset. See
// docs/design/ALLOW-ALL-POSTURE.md.
func TestMainRulesInjectsAllowAllWhenSet(t *testing.T) {
	withAllowAll := mainRules(Config{AllowAllTools: true})
	var found bool
	for _, r := range withAllowAll {
		if r.Scope == governance.ScopeCLI && r.Tool == "" && r.Pattern == "" && r.Effect == governance.Allow {
			found = true
			break
		}
	}
	if !found {
		t.Fatalf("AllowAllTools=true should inject a ScopeCLI empty-Tool/empty-Pattern Allow rule; rules=%+v", withAllowAll)
	}
	if len(withAllowAll) != len(defaultRules())+1 {
		t.Fatalf("AllowAllTools=true should add exactly one rule: got %d, want %d", len(withAllowAll), len(defaultRules())+1)
	}

	without := mainRules(Config{AllowAllTools: false})
	if len(without) != len(defaultRules()) {
		t.Fatalf("AllowAllTools=false should equal defaultRules(): got %d, want %d", len(without), len(defaultRules()))
	}
	for _, r := range without {
		if r.Scope == governance.ScopeCLI && r.Tool == "" && r.Pattern == "" && r.Effect == governance.Allow {
			t.Fatalf("AllowAllTools=false must NOT inject the allow-all rule; rules=%+v", without)
		}
	}
}

// TestMainRulesPolicyAutoAllowsAskFloor closes the loop on the structural check
// above: it drives mainRules through the REAL permpolicy evaluator and asserts the
// injected rule actually flips the built-in mutate-ask floor (Bash/Edit) from Ask
// to Allow when AllowAllTools is set, and leaves it at Ask when unset. This would
// catch a wrong scope (one that cannot loosen the ScopeBuiltinDefault floor) that
// the shape-only assertions miss. Benign commands only; nothing executes (the test
// stops at Evaluate).
func TestMainRulesPolicyAutoAllowsAskFloor(t *testing.T) {
	const sid = session.SessionID("s1")
	bashArgs, _ := json.Marshal(map[string]string{"command": "touch x"})
	bashCall := session.NewToolCall("c1", "Bash", bashArgs)
	editArgs, _ := json.Marshal(map[string]string{"file_path": "/x"})
	editCall := session.NewToolCall("c2", "Edit", editArgs)

	on := permpolicy.NewPolicy(mainRules(Config{AllowAllTools: true}), nil)
	if got := on.Evaluate(context.Background(), sid, session.ModeDefault, bashCall, nil); got.Effect != governance.Allow {
		t.Fatalf("AllowAllTools=true Bash: expected Allow, got %v", got.Effect)
	}
	if got := on.Evaluate(context.Background(), sid, session.ModeDefault, editCall, nil); got.Effect != governance.Allow {
		t.Fatalf("AllowAllTools=true Edit: expected Allow, got %v", got.Effect)
	}

	off := permpolicy.NewPolicy(mainRules(Config{AllowAllTools: false}), nil)
	if got := off.Evaluate(context.Background(), sid, session.ModeDefault, bashCall, nil); got.Effect != governance.Ask {
		t.Fatalf("AllowAllTools=false Bash: expected Ask, got %v", got.Effect)
	}
	if got := off.Evaluate(context.Background(), sid, session.ModeDefault, editCall, nil); got.Effect != governance.Ask {
		t.Fatalf("AllowAllTools=false Edit: expected Ask, got %v", got.Effect)
	}
}
