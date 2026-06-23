package agent_test

import (
	"strings"
	"testing"

	"github.com/stacklok/mecatl/engine/adapter/mockllm"
	"github.com/stacklok/mecatl/engine/agent"
	"github.com/stacklok/mecatl/engine/session"
)

// TestSubagentSalvagesPartialSummaryOnNoProgress is the issue #152 MODEL-FACING e2e: a
// free-text child emits enough EMPTY turns to exhaust its no-progress nudge budget
// (default MaxNoProgressNudges=2 → 3 empty turns) and ends on StopNoProgress with an
// EMPTY body. The salvage drives ONE wrap-up turn (post-Reopen) in which the child
// finally summarizes. The parent's ToolResult must carry that summary AND still bear the
// honest "[subagent stopped: ended without a final summary]" note — never the empty
// "(subagent produced no summary)".
func TestSubagentSalvagesPartialSummaryOnNoProgress(t *testing.T) {
	// Three empty turns trip StopNoProgress (2 nudges then exhausted); the fourth scripted
	// turn is the bounded salvage wrap-up that finally produces text.
	childLLM := mockllm.New(
		mockllm.EmptyTurn(),
		mockllm.EmptyTurn(),
		mockllm.EmptyTurn(),
		mockllm.TextTurn("SALVAGED NO-PROGRESS FINDINGS"),
	)
	childEngine := childEngineWith(childLLM, catalogWith(t))
	task := agent.NewSubagentTool(childEngine)

	results, _ := subagentParentResults(t, task,
		mockllm.ToolCallTurn(toolCall("p1", "Subagent", `{"prompt":"investigate"}`)),
		mockllm.TextTurn("parent done"),
	)
	if len(results) != 1 {
		t.Fatalf("want 1 result, got %d", len(results))
	}
	if results[0].IsError {
		t.Fatalf("salvaged no-progress stop is a success-with-note, got error: %+v", results[0])
	}
	body := results[0].Content
	if !strings.Contains(body, "agentId:") {
		t.Fatalf("result must carry the agentId trailer, got: %q", body)
	}
	if strings.Contains(body, "(subagent produced no summary)") {
		t.Fatalf("salvage should have filled in a body, got the empty placeholder: %q", body)
	}
	if !strings.Contains(body, "ended without a final summary") {
		t.Fatalf("result must keep the honest no-progress note, got: %q", body)
	}
	if !strings.Contains(body, "SALVAGED NO-PROGRESS FINDINGS") {
		t.Fatalf("result must carry the salvaged summary, got: %q", body)
	}
	// Bounding: exactly (nudges+1) empty turns + 1 salvage wrap-up = 4 model calls, no more
	// (the MaxTurns=1 pin brakes a stalling salvage).
	if got := childLLM.Calls(); got != 4 {
		t.Fatalf("child made %d model calls, want 4 (3 empty turns + 1 bounded salvage turn)", got)
	}
}

// TestSubagentNoProgressPreservesEarlierWork is the issue #152 work-preservation case
// short of salvage: the child produces text on a tool-call turn (so the loop records it
// and continues), then stalls into StopNoProgress on empty turns. The loop's own
// lastText machinery carries the earlier text into the StopNoProgress result, so the
// parent gets the child's work plus the honest note — never the empty placeholder, and
// without needing a salvage drive at all.
func TestSubagentNoProgressPreservesEarlierWork(t *testing.T) {
	// Turn 1: text + tool call → records an assistant message bearing the early text and
	// continues (a tool call was produced; lastText captures it). Turns 2-4: empty → 2
	// nudges then StopNoProgress. No salvage turn is needed (finalText is non-blank).
	childLLM := mockllm.New(
		mockllm.ChunksTurn(
			mockllm.TextChunk("EARLY ASSISTANT FINDINGS"),
			mockllm.ToolCallChunk(toolCall("k", "Read", `{"path":"a"}`)),
			mockllm.UsageChunk(session.Usage{}),
			mockllm.DoneChunk(session.StopEndTurn),
		),
		mockllm.EmptyTurn(),
		mockllm.EmptyTurn(),
		mockllm.EmptyTurn(),
	)
	childEngine := childEngineWith(childLLM, catalogWith(t, salvageReadTool()))
	task := agent.NewSubagentTool(childEngine)

	results, _ := subagentParentResults(t, task,
		mockllm.ToolCallTurn(toolCall("p1", "Subagent", `{"prompt":"investigate"}`)),
		mockllm.TextTurn("parent done"),
	)
	if len(results) != 1 {
		t.Fatalf("want 1 result, got %d", len(results))
	}
	if results[0].IsError {
		t.Fatalf("no-progress stop is a success-with-note, got error: %+v", results[0])
	}
	body := results[0].Content
	if !strings.Contains(body, "agentId:") {
		t.Fatalf("result must carry the agentId trailer, got: %q", body)
	}
	if strings.Contains(body, "(subagent produced no summary)") {
		t.Fatalf("the earlier work should be preserved, got the placeholder: %q", body)
	}
	if !strings.Contains(body, "EARLY ASSISTANT FINDINGS") {
		t.Fatalf("result must carry the child's earlier work, got: %q", body)
	}
	if !strings.Contains(body, "ended without a final summary") {
		t.Fatalf("result must keep the honest no-progress note, got: %q", body)
	}
	// finalText was non-blank (lastText), so NO salvage drive runs: exactly the 4 working
	// turns, no extra model call.
	if got := childLLM.Calls(); got != 4 {
		t.Fatalf("child made %d model calls, want 4 (no salvage needed when work is preserved)", got)
	}
}

// TestSubagentNoProgressResultNeverEmpty is the worst case (issue #152 done-condition):
// the child emits NO assistant text anywhere AND the salvage drive is empty, so there is
// nothing to recover. The result must STILL be non-empty: it carries the agentId trailer
// and names the stop reason via the StopNoProgress note — never a truly-empty result.
func TestSubagentNoProgressResultNeverEmpty(t *testing.T) {
	// Five empty turns: 3 for the working run (StopNoProgress), then the bounded salvage
	// turn is also empty, and there is no prior assistant text to digest.
	childLLM := mockllm.New(
		mockllm.EmptyTurn(),
		mockllm.EmptyTurn(),
		mockllm.EmptyTurn(),
		mockllm.EmptyTurn(),
	)
	childEngine := childEngineWith(childLLM, catalogWith(t))
	task := agent.NewSubagentTool(childEngine)

	results, _ := subagentParentResults(t, task,
		mockllm.ToolCallTurn(toolCall("p1", "Subagent", `{"prompt":"investigate"}`)),
		mockllm.TextTurn("parent done"),
	)
	if len(results) != 1 {
		t.Fatalf("want 1 result, got %d", len(results))
	}
	body := results[0].Content
	if strings.TrimSpace(body) == "" {
		t.Fatalf("result must never be empty, got: %q", body)
	}
	if !strings.Contains(body, "agentId:") {
		t.Fatalf("result must carry the agentId trailer even with nothing to recover, got: %q", body)
	}
	// The placeholder floor is acceptable here — but the stop reason MUST still be named
	// so the parent knows WHY the child produced nothing.
	if !strings.Contains(body, "ended without a final summary") {
		t.Fatalf("result must name the no-progress stop reason, got: %q", body)
	}
}
