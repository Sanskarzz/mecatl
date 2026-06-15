package agent_test

import (
	"strings"
	"testing"

	"github.com/stacklok/mecatl/engine/adapter/mockllm"
	"github.com/stacklok/mecatl/engine/adapter/permpolicy"
	"github.com/stacklok/mecatl/engine/agent"
	"github.com/stacklok/mecatl/engine/session"
)

// budgetTrippingChild builds a child engine with NO engine-level token budget (so the
// inherited default is unlimited) running a script whose every turn keeps calling a
// read-only loop tool and spends `perTurn` tokens, so ONLY a per-call MaxRunTokensOverride
// can stop it. A budget-stopped child renders the "[subagent stopped: reached its token
// budget]" note (StopBudget is a clean success-with-note terminal), which is the observable
// proof that the per-call budget rode RunOptions.MaxRunTokensOverride into the loop brake.
func budgetTrippingChild(t *testing.T, perTurn, turns int) *agent.Engine {
	t.Helper()
	var script []mockllm.Turn
	for i := 0; i < turns; i++ {
		script = append(script,
			mockllm.ChunksTurn(
				mockllm.ToolCallChunk(toolCall("k", "Loop", `{}`)),
				mockllm.UsageChunk(session.Usage{InputTokens: perTurn}),
				mockllm.DoneChunk(session.StopEndTurn),
			),
		)
	}
	// A trailing text turn so an UNBOUNDED child finishes cleanly with a real summary.
	script = append(script, mockllm.TextTurn("CHILD DONE"))
	return childEngineWith(mockllm.New(script...), catalogWith(t, loopTool()))
}

// TestSubagentMaxRunTokensAliasResolvesToOverride proves the PREFERRED max_run_tokens alias
// caps the child exactly as the deprecated max_tokens does: the per-call budget rides
// RunOptions.MaxRunTokensOverride into the loop brake and trips StopBudget, rendered as the
// token-budget note. Without the override the child would loop all scripted turns.
func TestSubagentMaxRunTokensAliasResolvesToOverride(t *testing.T) {
	// 6 budget-spending turns at 100 each; a 250 budget trips at the 3rd turn boundary.
	child := budgetTrippingChild(t, 100, 6)
	task := agent.NewSubagentTool(child)

	results, _ := subagentParentResults(t, task,
		mockllm.ToolCallTurn(toolCall("p1", "Subagent", `{"prompt":"loop","max_run_tokens":250}`)),
		mockllm.TextTurn("parent done"),
	)
	if len(results) != 1 {
		t.Fatalf("want 1 result, got %d", len(results))
	}
	if results[0].IsError {
		t.Fatalf("a budget-stopped child is a clean success-with-note, not an error: %q", results[0].Content)
	}
	if !strings.Contains(results[0].Content, "reached its token budget") {
		t.Fatalf("max_run_tokens did not trip the per-call budget; result = %q", results[0].Content)
	}
}

// TestSubagentMaxTokensDeprecatedAliasStillWorks is the backward-compat guard: the
// deprecated max_tokens alone still caps the child via the same budget brake.
func TestSubagentMaxTokensDeprecatedAliasStillWorks(t *testing.T) {
	child := budgetTrippingChild(t, 100, 6)
	task := agent.NewSubagentTool(child)

	results, _ := subagentParentResults(t, task,
		mockllm.ToolCallTurn(toolCall("p1", "Subagent", `{"prompt":"loop","max_tokens":250}`)),
		mockllm.TextTurn("parent done"),
	)
	if len(results) != 1 {
		t.Fatalf("want 1 result, got %d", len(results))
	}
	if results[0].IsError {
		t.Fatalf("a budget-stopped child is a clean success-with-note, not an error: %q", results[0].Content)
	}
	if !strings.Contains(results[0].Content, "reached its token budget") {
		t.Fatalf("the deprecated max_tokens alias did not trip the per-call budget; result = %q", results[0].Content)
	}
}

// TestSubagentMaxRunTokensConflictRejected is the ADVERSARIAL case: the model supplies BOTH
// aliases with DIFFERENT positive values. The call must be rejected with a model-visible
// error tool result, and the child must NEVER run (no token-budget note, no child summary).
func TestSubagentMaxRunTokensConflictRejected(t *testing.T) {
	child := budgetTrippingChild(t, 100, 6)
	task := agent.NewSubagentTool(child)

	results, _ := subagentParentResults(t, task,
		mockllm.ToolCallTurn(toolCall("p1", "Subagent", `{"prompt":"loop","max_tokens":100,"max_run_tokens":200}`)),
		mockllm.TextTurn("parent done"),
	)
	if len(results) != 1 {
		t.Fatalf("want 1 result, got %d", len(results))
	}
	if !results[0].IsError {
		t.Fatalf("conflicting budget aliases must be a model-visible error, got %+v", results[0])
	}
	if !strings.Contains(results[0].Content, "set only one of max_run_tokens or") {
		t.Fatalf("error should name the conflicting-alias rule, got %q", results[0].Content)
	}
	// The child must not have run at all — neither a summary nor a budget note.
	if strings.Contains(results[0].Content, "CHILD DONE") || strings.Contains(results[0].Content, "token budget") {
		t.Fatalf("the child ran despite the conflicting-alias rejection: %q", results[0].Content)
	}
}

// TestSubagentMaxRunTokensSameValueAccepted proves that supplying BOTH aliases with the SAME
// positive value is NOT a conflict (they name the same budget) — the call is accepted and
// the shared value caps the child.
func TestSubagentMaxRunTokensSameValueAccepted(t *testing.T) {
	child := budgetTrippingChild(t, 100, 6)
	task := agent.NewSubagentTool(child)

	results, _ := subagentParentResults(t, task,
		mockllm.ToolCallTurn(toolCall("p1", "Subagent", `{"prompt":"loop","max_tokens":250,"max_run_tokens":250}`)),
		mockllm.TextTurn("parent done"),
	)
	if len(results) != 1 {
		t.Fatalf("want 1 result, got %d", len(results))
	}
	if results[0].IsError {
		t.Fatalf("equal-value aliases must be accepted, not rejected: %q", results[0].Content)
	}
	if !strings.Contains(results[0].Content, "reached its token budget") {
		t.Fatalf("the shared (250) budget did not cap the child; result = %q", results[0].Content)
	}
}

// TestSubagentBudgetUnsetByDefault is the default-OFF guard (mirrors
// TestSubagentOmittedPerCallArgsUnchanged): with NEITHER alias set the child runs to its
// scripted text answer under an unlimited budget — even though its spend would trip a tiny
// per-call ceiling if one were supplied. A regression that defaulted the budget to a
// non-zero value would stop the child early with a token-budget note instead.
func TestSubagentBudgetUnsetByDefault(t *testing.T) {
	// The child spends 100/turn over 6 turns (600 total) before its summary; with no
	// budget set it must reach "CHILD DONE", not stop on a budget note.
	child := budgetTrippingChild(t, 100, 6)
	task := agent.NewSubagentTool(child)

	results, _ := subagentParentResults(t, task,
		mockllm.ToolCallTurn(toolCall("p1", "Subagent", `{"prompt":"loop"}`)),
		mockllm.TextTurn("parent done"),
	)
	if len(results) != 1 {
		t.Fatalf("want 1 result, got %d", len(results))
	}
	if results[0].IsError {
		t.Fatalf("default-budget child must finish cleanly, got error %+v", results[0])
	}
	if strings.Contains(results[0].Content, "token budget") {
		t.Fatalf("no budget was set, yet the child stopped on a token budget (default is NOT off): %q", results[0].Content)
	}
	if !strings.Contains(results[0].Content, "CHILD DONE") {
		t.Fatalf("default-budget child should run to its summary; got %q", results[0].Content)
	}
}

// TestSubagentMaxRunTokensNonPositiveTreatedAsUnset proves an explicit NON-POSITIVE budget
// (the model emitting max_run_tokens: 0, and the deprecated max_tokens: -1) is treated as
// UNSET — exactly like omitting the arg: the child runs to completion under the inherited
// unlimited budget, with no token-budget note. A regression that read a 0/negative value
// straight into MaxRunTokensOverride would stop the child immediately (a 0 ceiling is
// "already over budget").
func TestSubagentMaxRunTokensNonPositiveTreatedAsUnset(t *testing.T) {
	for _, tc := range []struct {
		name string
		args string
	}{
		{"zero max_run_tokens", `{"prompt":"loop","max_run_tokens":0}`},
		{"negative max_run_tokens", `{"prompt":"loop","max_run_tokens":-5}`},
		{"zero deprecated max_tokens", `{"prompt":"loop","max_tokens":0}`},
		{"zero both aliases", `{"prompt":"loop","max_run_tokens":0,"max_tokens":0}`},
	} {
		t.Run(tc.name, func(t *testing.T) {
			child := budgetTrippingChild(t, 100, 6)
			task := agent.NewSubagentTool(child)

			results, _ := subagentParentResults(t, task,
				mockllm.ToolCallTurn(toolCall("p1", "Subagent", tc.args)),
				mockllm.TextTurn("parent done"),
			)
			if len(results) != 1 {
				t.Fatalf("want 1 result, got %d", len(results))
			}
			if results[0].IsError {
				t.Fatalf("a non-positive budget must be treated as unset (no error), got %+v", results[0])
			}
			if strings.Contains(results[0].Content, "token budget") {
				t.Fatalf("a non-positive budget stopped the child on a token budget (should be unset/unlimited): %q", results[0].Content)
			}
			if !strings.Contains(results[0].Content, "CHILD DONE") {
				t.Fatalf("a non-positive budget should let the child run to its summary; got %q", results[0].Content)
			}
		})
	}
}

// TestSubagentMaxRunTokensTightenOnlyCannotLoosen is the token-budget analogue of
// TestSubagentPerCallTightenOnlyCannotLoosen: a per-call max_run_tokens HIGHER than the
// inherited engine budget cannot loosen it. The child engine carries a tight 250-token
// budget; a per-call max_run_tokens=10000 must NOT raise it, so the child still stops on
// the inherited budget rather than running to its summary.
func TestSubagentMaxRunTokensTightenOnlyCannotLoosen(t *testing.T) {
	var script []mockllm.Turn
	for i := 0; i < 6; i++ {
		script = append(script,
			mockllm.ChunksTurn(
				mockllm.ToolCallChunk(toolCall("k", "Loop", `{}`)),
				mockllm.UsageChunk(session.Usage{InputTokens: 100}),
				mockllm.DoneChunk(session.StopEndTurn),
			),
		)
	}
	script = append(script, mockllm.TextTurn("CHILD DONE"))
	// Engine budget 250 (tight); the per-call 10000 must not raise it.
	child := agent.NewEngine(agent.Deps{
		LLM:          mockllm.New(script...),
		Catalog:      catalogWith(t, loopTool()),
		Policy:       permpolicy.NewPolicy(permpolicy.AllowAllFloorRules(), nil),
		Model:        "child-model",
		MaxRunTokens: 250,
	})
	task := agent.NewSubagentTool(child)

	results, _ := subagentParentResults(t, task,
		mockllm.ToolCallTurn(toolCall("p1", "Subagent", `{"prompt":"loop","max_run_tokens":10000}`)),
		mockllm.TextTurn("parent done"),
	)
	if len(results) != 1 {
		t.Fatalf("want 1 result, got %d", len(results))
	}
	if !strings.Contains(results[0].Content, "reached its token budget") {
		t.Fatalf("tighten-only violated: a higher per-call budget loosened the inherited ceiling; result = %q", results[0].Content)
	}
	if strings.Contains(results[0].Content, "CHILD DONE") {
		t.Fatalf("the child ran to completion — the inherited 250 budget was loosened by the per-call 10000: %q", results[0].Content)
	}
}
