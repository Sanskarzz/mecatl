package agent_test

import (
	"context"
	"strings"
	"testing"

	"github.com/stacklok/mecatl/engine/adapter/mockllm"
	"github.com/stacklok/mecatl/engine/agent"
	"github.com/stacklok/mecatl/engine/session"
	"github.com/stacklok/mecatl/engine/tool"
)

// salvageReadTool is a read-only child tool whose Execute always succeeds — a child
// that keeps calling it never produces a final text answer, so it exhausts its
// turn/tool-call budget with an EMPTY summary (the issue #48 failure mode).
func salvageReadTool() tool.Tool {
	return &fakeTool{name: "Read", readOnly: true,
		exec: func(_ context.Context, in session.ToolCall, _ tool.Workspace) (session.ToolResult, error) {
			return session.NewToolResult(in.ID, "read ok"), nil
		}}
}

// TestSubagentSalvagesPartialSummaryOnMaxTurns is the issue #48 MODEL-FACING e2e: a
// free-text child burns its whole turn budget fetching (never emitting text) and trips
// StopMaxTurns. The salvage drives ONE wrap-up turn (post-Reopen) in which the child
// finally summarizes. The parent's ToolResult must carry that summary AND still bear the
// honest "[subagent stopped: reached its max-turns limit]" note — never the empty
// "(subagent produced no summary)".
func TestSubagentSalvagesPartialSummaryOnMaxTurns(t *testing.T) {
	// max_turns=2 → two tool-call turns trip the limit; cursor index 2 (the text turn)
	// is consumed by the bounded salvage drive after Reopen.
	childLLM := mockllm.New(
		mockllm.ToolCallTurn(toolCall("k", "Read", `{"path":"a"}`)),
		mockllm.ToolCallTurn(toolCall("k", "Read", `{"path":"b"}`)),
		mockllm.TextTurn("SALVAGED PARTIAL FINDINGS"),
	)
	childEngine := childEngineWith(childLLM, catalogWith(t, salvageReadTool()))
	task := agent.NewSubagentTool(childEngine)

	results, _ := subagentParentResults(t, task,
		mockllm.ToolCallTurn(toolCall("p1", "Subagent", `{"prompt":"investigate","max_turns":2}`)),
		mockllm.TextTurn("parent done"),
	)
	if len(results) != 1 {
		t.Fatalf("want 1 result, got %d", len(results))
	}
	if results[0].IsError {
		t.Fatalf("salvaged limit stop is a success-with-note, got error: %+v", results[0])
	}
	body := results[0].Content
	if strings.Contains(body, "(subagent produced no summary)") {
		t.Fatalf("salvage should have filled in a body, got the empty placeholder: %q", body)
	}
	if !strings.Contains(body, "SALVAGED PARTIAL FINDINGS") {
		t.Fatalf("result must carry the salvaged summary, got: %q", body)
	}
	if !strings.Contains(body, "reached its max-turns limit") {
		t.Fatalf("result must keep the honest max-turns note, got: %q", body)
	}
	// The salvage spends exactly ONE extra model call: 2 limit turns + 1 wrap-up.
	if got := childLLM.Calls(); got != 3 {
		t.Fatalf("child made %d model calls, want 3 (2 limit turns + 1 bounded salvage turn)", got)
	}
}

// TestSubagentSalvageDoesNotClobberExistingSummary is the guard: when the child DID
// produce a final summary before the limit tripped, the salvage must NOT run (no extra
// model call) and must NOT overwrite the real answer.
func TestSubagentSalvageDoesNotClobberExistingSummary(t *testing.T) {
	// The child answers with text on turn 2 (within max_turns=2) — a clean end, not a
	// limit stop with an empty body. No salvage turn should be consumed.
	childLLM := mockllm.New(
		mockllm.ToolCallTurn(toolCall("k", "Read", `{"path":"a"}`)),
		mockllm.TextTurn("REAL SUMMARY"),
		mockllm.TextTurn("SALVAGE SHOULD NOT RUN"),
	)
	childEngine := childEngineWith(childLLM, catalogWith(t, salvageReadTool()))
	task := agent.NewSubagentTool(childEngine)

	results, _ := subagentParentResults(t, task,
		mockllm.ToolCallTurn(toolCall("p1", "Subagent", `{"prompt":"investigate","max_turns":2}`)),
		mockllm.TextTurn("parent done"),
	)
	if len(results) != 1 || results[0].IsError {
		t.Fatalf("want 1 success result, got %+v", results)
	}
	if !strings.Contains(results[0].Content, "REAL SUMMARY") {
		t.Fatalf("must return the child's real summary, got: %q", results[0].Content)
	}
	if strings.Contains(results[0].Content, "SALVAGE SHOULD NOT RUN") {
		t.Fatalf("salvage must not run when the child already summarized, got: %q", results[0].Content)
	}
	// Exactly 2 model calls: one tool-call turn + the answering text turn. The third
	// scripted turn (the salvage sentinel) must remain unconsumed.
	if got := childLLM.Calls(); got != 2 {
		t.Fatalf("child made %d model calls, want 2 (no salvage drive)", got)
	}
}

// TestSubagentBudgetStopDoesNotSalvage proves StopBudget is deliberately EXCLUDED from
// salvage: spending another turn would violate the token ceiling. A child that exhausts
// its run-token budget with no summary must NOT get a wrap-up drive.
//
// Note: the operator-level engine budget (MaxRunTokens) is used here rather than a
// per-call max_tokens override, because per-call values below agent.MinSubagentRunTokens
// (25 000) are floored up to 25 000 — a 50-token per-call ceiling would be silently
// raised and the child would never hit it with only 200 scripted tokens. The operator
// budget bypasses the floor and stays authoritative at any value.
func TestSubagentBudgetStopDoesNotSalvage(t *testing.T) {
	// A child whose first turn calls a tool with usage that overshoots the OPERATOR budget
	// (50 tokens); the boundary check trips StopBudget before turn 2, with an empty body.
	// The second scripted turn (the salvage sentinel) must remain unconsumed.
	childLLM := mockllm.New(
		mockllm.ChunksTurn(
			mockllm.ToolCallChunk(toolCall("k", "Read", `{"path":"a"}`)),
			mockllm.UsageChunk(session.Usage{InputTokens: 100, OutputTokens: 100}),
			mockllm.DoneChunk(session.StopEndTurn),
		),
		mockllm.TextTurn("SALVAGE SHOULD NOT RUN ON BUDGET"),
	)
	// Tight operator budget of 50 tokens; the first turn spends 200 (100 in + 100 out),
	// tripping StopBudget before the salvage sentinel can run.
	childEngine := agent.NewEngine(agent.Deps{
		LLM:          childLLM,
		Catalog:      catalogWith(t, salvageReadTool()),
		Policy:       allowAll(),
		Model:        "child-model",
		MaxRunTokens: 50,
	})
	task := agent.NewSubagentTool(childEngine)

	results, _ := subagentParentResults(t, task,
		mockllm.ToolCallTurn(toolCall("p1", "Subagent", `{"prompt":"investigate"}`)),
		mockllm.TextTurn("parent done"),
	)
	if len(results) != 1 {
		t.Fatalf("want 1 result, got %d", len(results))
	}
	if strings.Contains(results[0].Content, "SALVAGE SHOULD NOT RUN ON BUDGET") {
		t.Fatalf("a token-budget stop must NOT trigger salvage, got: %q", results[0].Content)
	}
	// Only the FIRST turn ran; the salvage text turn stays unconsumed.
	if got := childLLM.Calls(); got != 1 {
		t.Fatalf("child made %d model calls, want 1 (budget stop, no salvage)", got)
	}
}
