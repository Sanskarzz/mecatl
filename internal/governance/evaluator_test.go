package governance

import (
	"encoding/json"
	"testing"
)

func bashArgs(cmd string) json.RawMessage {
	b, _ := json.Marshal(map[string]string{"command": cmd})
	return b
}

func fileArgs(p string) json.RawMessage {
	b, _ := json.Marshal(map[string]string{"file_path": p})
	return b
}

// gauntlet #8: a deny in ANY scope beats an allow in ANY scope.
func TestDenyBeatsAllowAcrossScopes(t *testing.T) {
	rules := []Rule{
		// user scope (lowest precedence) allows Bash rm.
		{Scope: ScopeUser, Tool: "Bash", Pattern: "rm *", Effect: Allow},
		// managed scope (highest precedence) denies Bash rm — but per doc 08,
		// even a LOWER-precedence deny must beat a higher-precedence allow.
		{Scope: ScopeManaged, Tool: "Bash", Pattern: "rm *", Effect: Deny},
	}
	e := NewEvaluator(rules)
	got := e.Evaluate("Bash", bashArgs("rm x"), false)
	if got.Effect != Deny {
		t.Fatalf("expected Deny (deny beats allow across scopes), got %v (%s)", got.Effect, got.Reason)
	}
}

// Even when the deny is in the LOWEST scope and allow in the HIGHEST, deny wins.
func TestDenyInLowScopeBeatsAllowInHighScope(t *testing.T) {
	rules := []Rule{
		{Scope: ScopeManaged, Tool: "Bash", Pattern: "rm *", Effect: Allow},
		{Scope: ScopeUser, Tool: "Bash", Pattern: "rm *", Effect: Deny},
	}
	e := NewEvaluator(rules)
	if got := e.Evaluate("Bash", bashArgs("rm x"), false); got.Effect != Deny {
		t.Fatalf("expected Deny regardless of scope, got %v", got.Effect)
	}
}

// Among rules of the SAME effect, the higher-precedence scope wins (its reason).
func TestSameEffectHigherScopeWins(t *testing.T) {
	rules := []Rule{
		{Scope: ScopeUser, Tool: "Read", Effect: Allow},
		{Scope: ScopeManaged, Tool: "Read", Effect: Allow},
	}
	e := NewEvaluator(rules)
	got := e.Evaluate("Read", fileArgs("/x"), false)
	if got.Effect != Allow {
		t.Fatalf("expected Allow, got %v", got.Effect)
	}
}

// The ONLY loosening (issue #13): a higher-precedence Allow relaxes the BUILT-IN
// DEFAULT Ask floor (ScopeBuiltinDefault). Here a shared-project Allow loosens the
// built-in Bash/Write ask.
func TestConfigAllowLoosensBuiltinDefaultAsk(t *testing.T) {
	rules := []Rule{
		{Scope: ScopeSharedProject, Tool: "Write", Effect: Allow},
		{Scope: ScopeBuiltinDefault, Tool: "Write", Effect: Ask},
	}
	e := NewEvaluator(rules)
	if got := e.Evaluate("Write", fileArgs("/x"), false); got.Effect != Allow {
		t.Fatalf("expected Allow (config allow loosens the built-in-default ask), got %v", got.Effect)
	}
}

// A higher-scope Allow must NOT suppress a CONFIGURED Ask (issue #13 narrowing): a
// ScopeManaged Allow over a ScopeSharedProject Ask resolves to Ask — both are
// author intent, and a config ask still gates regardless of an out-ranking allow.
// This is the case the over-broad first cut got wrong; nothing else pins it.
func TestHigherScopeAllowDoesNotSuppressConfiguredAsk(t *testing.T) {
	rules := []Rule{
		{Scope: ScopeManaged, Tool: "Write", Effect: Allow},
		{Scope: ScopeSharedProject, Tool: "Write", Effect: Ask},
	}
	e := NewEvaluator(rules)
	if got := e.Evaluate("Write", fileArgs("/x"), false); got.Effect != Ask {
		t.Fatalf("expected Ask (a managed allow must not suppress a configured shared-project ask), got %v", got.Effect)
	}
}

// The inverse: a higher-precedence Ask still beats a lower-precedence Allow, so a
// managed/config ask gates a learned (lowest-scope) allow.
func TestHigherScopeAskBeatsLowerScopeAllow(t *testing.T) {
	rules := []Rule{
		{Scope: ScopeManaged, Tool: "Write", Effect: Ask},
		{Scope: ScopeUser, Tool: "Write", Effect: Allow},
	}
	e := NewEvaluator(rules)
	if got := e.Evaluate("Write", fileArgs("/x"), false); got.Effect != Ask {
		t.Fatalf("expected Ask (higher-scope ask beats lower-scope allow), got %v", got.Effect)
	}
}

// On an EXACT scope tie between Ask and Allow, Ask wins (fail safe toward asking).
func TestEqualScopeAskBeatsAllow(t *testing.T) {
	rules := []Rule{
		{Scope: ScopeUser, Tool: "Write", Effect: Allow},
		{Scope: ScopeUser, Tool: "Write", Effect: Ask},
	}
	e := NewEvaluator(rules)
	if got := e.Evaluate("Write", fileArgs("/x"), false); got.Effect != Ask {
		t.Fatalf("expected Ask on a scope tie, got %v", got.Effect)
	}
}

// No matching rule defaults to Ask (never silently allow).
func TestNoMatchDefaultsToAsk(t *testing.T) {
	e := NewEvaluator(nil)
	if got := e.Evaluate("Read", fileArgs("/x"), false); got.Effect != Ask {
		t.Fatalf("expected default Ask, got %v", got.Effect)
	}
}

// gauntlet / doc 08 #10: a deny on `rm` blocks the whole `git status && rm -rf /`
// compound even though `git status` would be allowed.
func TestCompoundBashDenyBlocksWhole(t *testing.T) {
	rules := []Rule{
		{Scope: ScopeUser, Tool: "Bash", Pattern: "git status", Effect: Allow},
		{Scope: ScopeUser, Tool: "Bash", Pattern: "rm *", Effect: Deny},
	}
	e := NewEvaluator(rules)
	got := e.Evaluate("Bash", bashArgs("git status && rm -rf /"), false)
	if got.Effect != Deny {
		t.Fatalf("expected Deny for compound containing rm, got %v (%s)", got.Effect, got.Reason)
	}
}

// A compound where every sub-command is allowed resolves to allow.
func TestCompoundBashAllAllowed(t *testing.T) {
	rules := []Rule{
		{Scope: ScopeUser, Tool: "Bash", Pattern: "git status", Effect: Allow},
		{Scope: ScopeUser, Tool: "Bash", Pattern: "ls", Effect: Allow},
	}
	e := NewEvaluator(rules)
	if got := e.Evaluate("Bash", bashArgs("git status && ls"), false); got.Effect != Allow {
		t.Fatalf("expected Allow, got %v (%s)", got.Effect, got.Reason)
	}
}

// Canonicalization is applied before matching: a wrapped rm matches an `rm` deny.
func TestCanonicalizedBashMatching(t *testing.T) {
	rules := []Rule{
		{Scope: ScopeManaged, Tool: "Bash", Pattern: "rm *", Effect: Deny},
	}
	e := NewEvaluator(rules)
	if got := e.Evaluate("Bash", bashArgs("timeout 5 rm x"), false); got.Effect != Deny {
		t.Fatalf("expected Deny after canonicalizing timeout wrapper, got %v", got.Effect)
	}
}

// gauntlet #3: plan mode denies Edit/Write and non-read-only Bash, allows
// Read/Grep/Glob and read-only Bash.
func TestPlanModeFilter(t *testing.T) {
	// Allow-everything rules so plan-mode is the only thing that can deny.
	rules := []Rule{
		{Effect: Allow}, // empty tool+pattern matches anything
	}
	e := NewEvaluator(rules)

	tests := []struct {
		name string
		tool string
		args json.RawMessage
		want Effect
	}{
		{"Edit denied", "Edit", fileArgs("/x"), Deny},
		{"Write denied", "Write", fileArgs("/x"), Deny},
		{"non-RO Bash denied", "Bash", bashArgs("rm -rf /"), Deny},
		{"Read allowed", "Read", fileArgs("/x"), Allow},
		{"Grep allowed", "Grep", fileArgs("/x"), Allow},
		{"Glob allowed", "Glob", fileArgs("/x"), Allow},
		{"read-only Bash allowed", "Bash", bashArgs("git status"), Allow},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got := e.Evaluate(tt.tool, tt.args, true)
			if got.Effect != tt.want {
				t.Fatalf("plan mode %s: got %v (%s), want %v", tt.tool, got.Effect, got.Reason, tt.want)
			}
		})
	}
}

// Plan-mode deny must beat even an explicit allow rule for a mutating tool.
func TestPlanModeBeatsAllowRule(t *testing.T) {
	rules := []Rule{
		{Scope: ScopeManaged, Tool: "Write", Effect: Allow},
	}
	e := NewEvaluator(rules)
	if got := e.Evaluate("Write", fileArgs("/x"), true); got.Effect != Deny {
		t.Fatalf("expected plan-mode Deny to override allow rule, got %v", got.Effect)
	}
}

// Finding 1: a Bash allow rule for an outer literal must not silently approve a
// destructive inner command hidden in command/process substitution or subshell
// grouping. Such a segment is floored at Ask (or worse) even when the outer
// command would be allowed.
func TestSubstitutionEscalatesPastAllow(t *testing.T) {
	rules := []Rule{
		// Allow echo broadly; the substitution must still escalate.
		{Scope: ScopeManaged, Tool: "Bash", Pattern: "echo *", Effect: Allow},
	}
	e := NewEvaluator(rules)
	cases := []struct {
		name string
		cmd  string
	}{
		{"command substitution", "echo ok $(rm -rf build)"},
		{"backtick", "echo `rm -rf build`"},
		{"subshell grouping", "ls;(rm -rf build)"},
		{"process substitution", "echo <(rm x)"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got := e.Evaluate("Bash", bashArgs(tc.cmd), false)
			if effectRank(got.Effect) < effectRank(Ask) {
				t.Fatalf("Evaluate(%q) = %v (%s); want >= Ask", tc.cmd, got.Effect, got.Reason)
			}
		})
	}
}

// An explicit deny on the inner destructive command is preserved (Deny beats the
// Ask floor) when the segment can be matched.
func TestSubstitutionPreservesDeny(t *testing.T) {
	rules := []Rule{
		{Scope: ScopeManaged, Tool: "Bash", Pattern: "echo *", Effect: Allow},
		{Scope: ScopeManaged, Tool: "Bash", Pattern: "*rm*", Effect: Deny},
	}
	e := NewEvaluator(rules)
	if got := e.Evaluate("Bash", bashArgs("echo $(rm -rf build)"), false); got.Effect != Deny {
		t.Fatalf("expected Deny for hidden rm, got %v (%s)", got.Effect, got.Reason)
	}
}

// Plan mode hard-denies non-read-only Bash; substitution/grouping/newline forms
// must be classified non-read-only so plan mode is not fooled.
func TestPlanModeDeniesSubstitutionAndNewline(t *testing.T) {
	e := NewEvaluator([]Rule{{Scope: ScopeManaged, Tool: "Bash", Effect: Allow}})
	cases := []string{
		"cat $(rm x)",
		"echo `rm x`",
		"ls;(rm -rf build)",
		"ls\nrm -rf build",
		"diff <(rm x) f",
	}
	for _, cmd := range cases {
		t.Run(cmd, func(t *testing.T) {
			if got := e.Evaluate("Bash", bashArgs(cmd), true); got.Effect != Deny {
				t.Fatalf("plan mode Evaluate(%q) = %v (%s); want Deny", cmd, got.Effect, got.Reason)
			}
		})
	}
}

// Direct ReadOnlyBash assertions called out in the finding.
func TestReadOnlyBashFindingCases(t *testing.T) {
	if ReadOnlyBash("cat $(rm x)") {
		t.Fatal(`ReadOnlyBash("cat $(rm x)") = true, want false`)
	}
	if ReadOnlyBash("ls\nrm x") {
		t.Fatal("ReadOnlyBash(\"ls\\nrm x\") = true, want false")
	}
}
