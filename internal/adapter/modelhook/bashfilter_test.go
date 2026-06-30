package modelhook_test

import (
	"context"
	"encoding/json"
	"testing"

	"github.com/stacklok/mecatl/engine/governance"
	"github.com/stacklok/mecatl/internal/adapter/modelhook"
)

// countingChecker records how many times Check was called and returns a fixed verdict.
// It is the oracle for "did the read-only pre-filter skip the checker?" — calls==0
// proves the skip path fired (zero LLM calls), calls>=1 proves inspection.
type countingChecker struct {
	verdict modelhook.Verdict
	calls   int
}

func (c *countingChecker) Check(_ context.Context, _ modelhook.CheckRequest) (modelhook.Verdict, error) {
	c.calls++
	return c.verdict, nil
}

// bashSkipRule is the default-shaped Bash rule: pre/block with the read-only pre-filter on.
func bashSkipRule(t *testing.T) modelhook.CompiledRule {
	t.Helper()
	r, ok := modelhook.CompileRule(modelhook.RuleSpec{
		Match: "Bash", Phases: []string{"pre"}, Mode: string(modelhook.ModeBlock), SkipReadOnlyBash: true,
	})
	if !ok {
		t.Fatal("bash skip rule must compile")
	}
	return r
}

// bashArgs builds a Bash tool-call args JSON with a "command" field.
func bashArgs(cmd string) json.RawMessage {
	b, _ := json.Marshal(map[string]string{"command": cmd})
	return b
}

// runOne drives one Runner.Run for a Bash event in the given phase with the given raw
// args, returning the checker call count and the outcome.
func runOne(t *testing.T, rule modelhook.CompiledRule, phase governance.HookPhase, tool string, input json.RawMessage) (int, governance.HookOutcome) {
	t.Helper()
	chk := &countingChecker{verdict: modelhook.Verdict{Safe: boolp(true)}}
	r := modelhook.New(noopHooks{}, modelhook.Options{Rules: []modelhook.CompiledRule{rule}, Checker: chk})
	out, err := r.Run(context.Background(), governance.HookEvent{Phase: phase, Tool: tool, Input: input})
	if err != nil {
		t.Fatalf("Run error: %v", err)
	}
	return chk.calls, out
}

// noopHooks is an inert inner HookRunner (allows every phase, mutates nothing).
type noopHooks struct{}

func (noopHooks) Run(context.Context, governance.HookEvent) (governance.HookOutcome, error) {
	return governance.HookOutcome{}, nil
}

// TestBashReadOnlySkipsChecker: a CONFIDENTLY read-only Pre Bash command skips the
// checker entirely (calls==0) and returns the zero (pass-through) outcome.
func TestBashReadOnlySkipsChecker(t *testing.T) {
	rule := bashSkipRule(t)
	readOnly := []string{
		"ls",
		"cat f",
		"grep x f",
		"git status",
		"git log",
		"timeout 5 ls",
		"cat $(ls)", // a fully-read-only substitution: SubstitutionReadOnly clears it.
	}
	for _, cmd := range readOnly {
		t.Run(cmd, func(t *testing.T) {
			calls, out := runOne(t, rule, governance.PhasePreToolUse, "Bash", bashArgs(cmd))
			if calls != 0 {
				t.Fatalf("read-only %q must skip the checker; calls=%d", cmd, calls)
			}
			if out.Block || len(out.Mutated) != 0 || out.Message != "" {
				t.Fatalf("read-only skip must return the zero outcome; got %+v", out)
			}
		})
	}
}

// TestBashMutatingIsInspected: a mutating command is NOT skipped — the checker runs
// once.
func TestBashMutatingIsInspected(t *testing.T) {
	rule := bashSkipRule(t)
	mutating := []string{
		"mkdir build",
		"touch out",
		"git commit -m x",
		"cp a b",
		"echo hi > f",
	}
	for _, cmd := range mutating {
		t.Run(cmd, func(t *testing.T) {
			calls, _ := runOne(t, rule, governance.PhasePreToolUse, "Bash", bashArgs(cmd))
			if calls != 1 {
				t.Fatalf("mutating %q must be inspected; calls=%d", cmd, calls)
			}
		})
	}
}

// TestBashAmbiguousIsInspected: a substitution-as-verb / not-provably-read-only command
// is inspected (fail-safe direction — never skipped on ambiguity).
func TestBashAmbiguousIsInspected(t *testing.T) {
	rule := bashSkipRule(t)
	ambiguous := []string{
		"$(printf ls)",     // command substitution in command position: executes its output.
		"`printf ls`",      // backtick in command position: same.
		"some-unknown-bin", // unknown verb: not provably read-only.
	}
	for _, cmd := range ambiguous {
		t.Run(cmd, func(t *testing.T) {
			calls, _ := runOne(t, rule, governance.PhasePreToolUse, "Bash", bashArgs(cmd))
			if calls != 1 {
				t.Fatalf("ambiguous %q must be inspected (fail-safe); calls=%d", cmd, calls)
			}
		})
	}
}

// TestBashFilterDoesNotApplyToPost: the same rule, but on the POST phase, the filter
// does NOT apply (a post-phase Bash rule would inspect; but this rule is pre-only, so a
// post event does not match and the checker is never called). To prove the filter is
// pre-only we use a rule that covers BOTH phases and confirm a read-only command is
// inspected on post.
func TestBashFilterDoesNotApplyToPost(t *testing.T) {
	r, ok := modelhook.CompileRule(modelhook.RuleSpec{
		Match: "Bash", Phases: []string{"pre", "post"}, Mode: string(modelhook.ModeBlock), SkipReadOnlyBash: true,
	})
	if !ok {
		t.Fatal("rule must compile")
	}
	// Post phase: the loop packs {args, content, is_error} — but the filter only ever
	// fires on Pre, so a read-only command on Post is still inspected.
	calls, _ := runOne(t, r, governance.PhasePostToolUse, "Bash", json.RawMessage(`{"content":"ls"}`))
	if calls != 1 {
		t.Fatalf("the read-only filter must NOT apply on the post phase; calls=%d", calls)
	}
}

// TestBashFilterDoesNotApplyToNonBash: a non-Bash tool with a skipReadOnlyBash rule is
// inspected normally (the filter is tool=="Bash" only).
func TestBashFilterDoesNotApplyToNonBash(t *testing.T) {
	// A "*" rule with the flag set, matching WebFetch on pre. The flag is inert for a
	// non-Bash tool, so the checker runs.
	r, ok := modelhook.CompileRule(modelhook.RuleSpec{
		Match: "*", Phases: []string{"pre"}, Mode: string(modelhook.ModeBlock), SkipReadOnlyBash: true,
	})
	if !ok {
		t.Fatal("rule must compile")
	}
	calls, _ := runOne(t, r, governance.PhasePreToolUse, "WebFetch", json.RawMessage(`{"url":"x"}`))
	if calls != 1 {
		t.Fatalf("the read-only filter must NOT apply to a non-Bash tool; calls=%d", calls)
	}
}

// TestBashMalformedArgsAreInspected: an unparseable args object OR a missing command
// field falls through to inspection (fail-safe — never presumed read-only).
func TestBashMalformedArgsAreInspected(t *testing.T) {
	rule := bashSkipRule(t)
	cases := []struct {
		name  string
		input json.RawMessage
	}{
		{"not json", json.RawMessage(`{not json`)},
		{"no command field", json.RawMessage(`{"foo":"ls"}`)},
		{"empty command", json.RawMessage(`{"command":"   "}`)},
		{"command not a string", json.RawMessage(`{"command":123}`)},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			calls, _ := runOne(t, rule, governance.PhasePreToolUse, "Bash", c.input)
			if calls != 1 {
				t.Fatalf("malformed args (%s) must be inspected; calls=%d", c.name, calls)
			}
		})
	}
}

// TestBashCmdFallbackField: the "cmd" alias field is read when "command" is absent. A
// read-only "cmd" still skips.
func TestBashCmdFallbackField(t *testing.T) {
	rule := bashSkipRule(t)
	calls, _ := runOne(t, rule, governance.PhasePreToolUse, "Bash", json.RawMessage(`{"cmd":"ls"}`))
	if calls != 0 {
		t.Fatalf("a read-only command in the cmd field must skip; calls=%d", calls)
	}
}

// TestBashFilterWithoutFlagInspects: WITHOUT SkipReadOnlyBash a Bash pre rule inspects
// every command, read-only or not — proving the flag is the sole opt-in.
func TestBashFilterWithoutFlagInspects(t *testing.T) {
	r, ok := modelhook.CompileRule(modelhook.RuleSpec{
		Match: "Bash", Phases: []string{"pre"}, Mode: string(modelhook.ModeBlock),
	})
	if !ok {
		t.Fatal("rule must compile")
	}
	calls, _ := runOne(t, r, governance.PhasePreToolUse, "Bash", bashArgs("ls"))
	if calls != 1 {
		t.Fatalf("without the flag, a read-only Bash command must still be inspected; calls=%d", calls)
	}
}

// TestCompileRuleCarriesSkipReadOnlyBash: CompileRule threads the flag from RuleSpec
// into the CompiledRule (observable via the runtime skip behaviour, since the field is
// unexported). The behavioural check above (TestBashReadOnlySkipsChecker vs
// TestBashFilterWithoutFlagInspects) is the proof; this test pins the COMPILE step
// directly by toggling the flag and asserting the divergent outcome.
func TestCompileRuleCarriesSkipReadOnlyBash(t *testing.T) {
	with, ok1 := modelhook.CompileRule(modelhook.RuleSpec{Match: "Bash", Phases: []string{"pre"}, Mode: string(modelhook.ModeBlock), SkipReadOnlyBash: true})
	without, ok2 := modelhook.CompileRule(modelhook.RuleSpec{Match: "Bash", Phases: []string{"pre"}, Mode: string(modelhook.ModeBlock), SkipReadOnlyBash: false})
	if !ok1 || !ok2 {
		t.Fatal("both rules must compile")
	}
	if c, _ := runOne(t, with, governance.PhasePreToolUse, "Bash", bashArgs("ls")); c != 0 {
		t.Fatalf("SkipReadOnlyBash=true must skip a read-only command; calls=%d", c)
	}
	if c, _ := runOne(t, without, governance.PhasePreToolUse, "Bash", bashArgs("ls")); c != 1 {
		t.Fatalf("SkipReadOnlyBash=false must inspect a read-only command; calls=%d", c)
	}
}
