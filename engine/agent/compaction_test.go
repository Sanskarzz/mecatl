package agent_test

import (
	"context"
	"encoding/json"
	"errors"
	"reflect"
	"strings"
	"testing"
	"time"

	"github.com/stacklok/mecatl/engine/adapter/memfs"
	"github.com/stacklok/mecatl/engine/adapter/mockllm"
	"github.com/stacklok/mecatl/engine/agent"
	"github.com/stacklok/mecatl/engine/session"
	"github.com/stacklok/mecatl/engine/tool"
)

// TestHeuristicCompactorPreservesPaths checks the default compactor preserves the
// system prompt, the goal, and every touched file path, while truncating large
// tool-output bodies (gauntlet #12-lite).
func TestHeuristicCompactorPreservesPaths(t *testing.T) {
	conv := &session.Conversation{}
	conv.Append(session.NewSystemMessage("system rules"))
	conv.Append(session.NewUserMessage("fix the bug in handler.go"))
	conv.Append(session.NewAssistantMessage("", "", []session.ToolCall{
		session.NewToolCall("c1", "Read", json.RawMessage(`{"path":"handler.go"}`)),
	}))
	conv.Append(session.NewToolMessage(session.NewToolResult("c1", strings.Repeat("X", 5000))))
	conv.Append(session.NewAssistantMessage("", "", []session.ToolCall{
		session.NewToolCall("c2", "Edit", json.RawMessage(`{"file_path":"util.go"}`)),
	}))
	conv.Append(session.NewToolMessage(session.NewToolResult("c2", "edited")))
	for i := 0; i < 10; i++ {
		conv.Append(session.NewUserMessage("more chatter"))
	}

	compacted, summary, err := agent.HeuristicCompactor{}.Compact(context.Background(), conv)
	if err != nil {
		t.Fatalf("Compact: %v", err)
	}

	// Paths survive (in summary).
	for _, p := range []string{"handler.go", "util.go"} {
		if !strings.Contains(summary, p) {
			t.Fatalf("summary missing path %q: %s", p, summary)
		}
	}
	// System prompt and goal survive.
	var haveSystem, haveGoal bool
	for _, m := range compacted {
		if m.Role == session.RoleSystem && m.Text == "system rules" {
			haveSystem = true
		}
		if m.Role == session.RoleUser && strings.Contains(m.Text, "fix the bug") {
			haveGoal = true
		}
	}
	if !haveSystem {
		t.Fatalf("system prompt not preserved")
	}
	if !haveGoal {
		t.Fatalf("goal not preserved")
	}
	// Big bodies are dropped: no message should carry the 5000-char blob.
	for _, m := range compacted {
		if m.ToolResult != nil && len(m.ToolResult.Content) >= 5000 {
			t.Fatalf("large tool body survived compaction: %d chars", len(m.ToolResult.Content))
		}
	}
	// The result is shorter than the input.
	if len(compacted) >= len(conv.Messages) {
		t.Fatalf("compaction did not shrink history: %d -> %d", len(conv.Messages), len(compacted))
	}
}

// TestHeuristicCompactorNoOrphanedToolResultAtTailHead builds a conversation
// sized so the count-based cut (len-keep) lands ON a tool-result message whose
// matching assistant tool call sits ABOVE the cut (in the dropped head). Before
// the boundary-snap fix the tail STARTED on that orphaned result, which a
// provider rejects with HTTP 400. The compacted history must be tool-pairing
// valid and non-empty.
func TestHeuristicCompactorNoOrphanedToolResultAtTailHead(t *testing.T) {
	conv := &session.Conversation{}
	conv.Append(session.NewSystemMessage("system rules"))
	conv.Append(session.NewUserMessage("fix the bug"))
	// Build assistant-call / tool-result pairs. With KeepLastTurns chosen so the
	// cut bisects a pair, the tail's first message is a tool result.
	for i := 0; i < 5; i++ {
		id := session.ToolCallID(string(rune('a' + i)))
		conv.Append(session.NewAssistantMessage("", "", []session.ToolCall{
			session.NewToolCall(id, "Read", json.RawMessage(`{"path":"f.go"}`)),
		}))
		conv.Append(session.NewToolMessage(session.NewToolResult(id, "body")))
	}
	// msgs: [sys, goal, A(a),T(a), A(b),T(b), A(c),T(c), A(d),T(d), A(e),T(e)]
	// len = 12. keep = 3 => cut = 9 => msgs[9] = T(d) (a tool result whose call
	// A(d) at index 8 is above the cut). Without snapping, tail starts on T(d).
	compacted, _, err := agent.HeuristicCompactor{KeepLastTurns: 3}.Compact(context.Background(), conv)
	if err != nil {
		t.Fatalf("Compact: %v", err)
	}
	if err := session.ValidateToolPairing(compacted); err != nil {
		t.Fatalf("compacted history is not tool-pairing valid: %v", err)
	}
	if len(compacted) == 0 {
		t.Fatalf("compacted history is empty")
	}
}

// TestHeuristicCompactorTailAllToolResults exercises the case where snapping must
// advance the cut all the way to len(msgs) because every message after the
// initial cut is a tool result. The tail ends up empty; the output is still
// non-empty (system + goal + summary) and orphan-free.
func TestHeuristicCompactorTailAllToolResults(t *testing.T) {
	conv := &session.Conversation{}
	conv.Append(session.NewSystemMessage("system rules"))
	conv.Append(session.NewUserMessage("goal"))
	// One assistant message requesting many calls, then a run of tool results.
	calls := make([]session.ToolCall, 0, 4)
	for i := 0; i < 4; i++ {
		id := session.ToolCallID(string(rune('a' + i)))
		calls = append(calls, session.NewToolCall(id, "Read", json.RawMessage(`{}`)))
	}
	conv.Append(session.NewAssistantMessage("", "", calls))
	for i := 0; i < 4; i++ {
		id := session.ToolCallID(string(rune('a' + i)))
		conv.Append(session.NewToolMessage(session.NewToolResult(id, "body")))
	}
	// msgs len = 7: [sys, goal, A(abcd), T(a),T(b),T(c),T(d)]. keep=4 => cut=3 =>
	// msgs[3..] are all tool results; snapping advances cut to len (empty tail).
	compacted, _, err := agent.HeuristicCompactor{KeepLastTurns: 4}.Compact(context.Background(), conv)
	if err != nil {
		t.Fatalf("Compact: %v", err)
	}
	if err := session.ValidateToolPairing(compacted); err != nil {
		t.Fatalf("compacted history is not tool-pairing valid: %v", err)
	}
	if len(compacted) == 0 {
		t.Fatalf("compacted history is empty")
	}
}

// TestHeuristicCompactorMultipleConsecutiveLeadingOrphans checks the snap loop
// consumes MULTIPLE consecutive leading tool results (a multi-call assistant turn
// bisected by the cut leaves several orphans at the tail head).
func TestHeuristicCompactorMultipleConsecutiveLeadingOrphans(t *testing.T) {
	conv := &session.Conversation{}
	conv.Append(session.NewSystemMessage("system rules"))
	conv.Append(session.NewUserMessage("goal"))
	calls := make([]session.ToolCall, 0, 3)
	for i := 0; i < 3; i++ {
		id := session.ToolCallID(string(rune('a' + i)))
		calls = append(calls, session.NewToolCall(id, "Read", json.RawMessage(`{}`)))
	}
	conv.Append(session.NewAssistantMessage("", "", calls))              // index 2
	conv.Append(session.NewToolMessage(session.NewToolResult("a", "x"))) // 3
	conv.Append(session.NewToolMessage(session.NewToolResult("b", "x"))) // 4
	conv.Append(session.NewToolMessage(session.NewToolResult("c", "x"))) // 5
	conv.Append(session.NewAssistantMessage("done", "", nil))            // 6
	// len=7, keep=4 => cut=3 => tail would start at T(a) with 3 consecutive
	// leading orphans T(a),T(b),T(c). Snapping must consume all three.
	compacted, _, err := agent.HeuristicCompactor{KeepLastTurns: 4}.Compact(context.Background(), conv)
	if err != nil {
		t.Fatalf("Compact: %v", err)
	}
	if err := session.ValidateToolPairing(compacted); err != nil {
		t.Fatalf("compacted history is not tool-pairing valid: %v", err)
	}
	// The trailing standalone assistant message (index 6) must still survive.
	var sawDone bool
	for _, m := range compacted {
		if m.Role == session.RoleAssistant && m.Text == "done" {
			sawDone = true
		}
	}
	if !sawDone {
		t.Fatalf("trailing assistant message did not survive snapping")
	}
}

// TestCascadeCompactorNoOrphanedToolResult feeds the same orphan-prone shape to
// the CascadeCompactor (offline: BudgetTokens 0 runs every deterministic tier
// once) and asserts the compacted history is tool-pairing valid.
func TestCascadeCompactorNoOrphanedToolResult(t *testing.T) {
	conv := &session.Conversation{}
	conv.Append(session.NewSystemMessage("system rules"))
	conv.Append(session.NewUserMessage("fix the bug"))
	for i := 0; i < 5; i++ {
		id := session.ToolCallID(string(rune('a' + i)))
		conv.Append(session.NewAssistantMessage("", "", []session.ToolCall{
			session.NewToolCall(id, "Read", json.RawMessage(`{"path":"f.go"}`)),
		}))
		conv.Append(session.NewToolMessage(session.NewToolResult(id, strings.Repeat("X", 2000))))
	}
	compacted, _, err := agent.CascadeCompactor{KeepLastTurns: 3}.Compact(context.Background(), conv)
	if err != nil {
		t.Fatalf("Compact: %v", err)
	}
	if err := session.ValidateToolPairing(compacted); err != nil {
		t.Fatalf("cascade compacted history is not tool-pairing valid: %v", err)
	}
	if len(compacted) == 0 {
		t.Fatalf("cascade compacted history is empty")
	}
}

// TestCascadeCompactorTailAllToolResults is cascade parity with the heuristic
// tail-all-tool-results case: the cut snaps to len (empty tail), output stays
// non-empty and orphan-free.
func TestCascadeCompactorTailAllToolResults(t *testing.T) {
	conv := &session.Conversation{}
	conv.Append(session.NewSystemMessage("system rules"))
	conv.Append(session.NewUserMessage("goal"))
	calls := make([]session.ToolCall, 0, 4)
	for i := 0; i < 4; i++ {
		id := session.ToolCallID(string(rune('a' + i)))
		calls = append(calls, session.NewToolCall(id, "Read", json.RawMessage(`{}`)))
	}
	conv.Append(session.NewAssistantMessage("", "", calls))
	for i := 0; i < 4; i++ {
		id := session.ToolCallID(string(rune('a' + i)))
		conv.Append(session.NewToolMessage(session.NewToolResult(id, "body")))
	}
	compacted, _, err := agent.CascadeCompactor{KeepLastTurns: 4}.Compact(context.Background(), conv)
	if err != nil {
		t.Fatalf("Compact: %v", err)
	}
	if err := session.ValidateToolPairing(compacted); err != nil {
		t.Fatalf("cascade compacted history is not tool-pairing valid: %v", err)
	}
	if len(compacted) == 0 {
		t.Fatalf("cascade compacted history is empty")
	}
}

// TestCascadeCompactorMultipleConsecutiveLeadingOrphans is cascade parity with the
// heuristic multiple-leading-orphans case: the snap consumes a run of consecutive
// leading tool results and the trailing standalone assistant message survives.
func TestCascadeCompactorMultipleConsecutiveLeadingOrphans(t *testing.T) {
	conv := &session.Conversation{}
	conv.Append(session.NewSystemMessage("system rules"))
	conv.Append(session.NewUserMessage("goal"))
	calls := make([]session.ToolCall, 0, 3)
	for i := 0; i < 3; i++ {
		id := session.ToolCallID(string(rune('a' + i)))
		calls = append(calls, session.NewToolCall(id, "Read", json.RawMessage(`{}`)))
	}
	conv.Append(session.NewAssistantMessage("", "", calls))
	conv.Append(session.NewToolMessage(session.NewToolResult("a", "x")))
	conv.Append(session.NewToolMessage(session.NewToolResult("b", "x")))
	conv.Append(session.NewToolMessage(session.NewToolResult("c", "x")))
	conv.Append(session.NewAssistantMessage("done", "", nil))
	compacted, _, err := agent.CascadeCompactor{KeepLastTurns: 4}.Compact(context.Background(), conv)
	if err != nil {
		t.Fatalf("Compact: %v", err)
	}
	if err := session.ValidateToolPairing(compacted); err != nil {
		t.Fatalf("cascade compacted history is not tool-pairing valid: %v", err)
	}
	var sawDone bool
	for _, m := range compacted {
		if m.Role == session.RoleAssistant && m.Text == "done" {
			sawDone = true
		}
	}
	if !sawDone {
		t.Fatalf("trailing assistant message did not survive cascade snapping")
	}
}

// danglingTailConversation builds a history whose preserved tail ENDS on an
// assistant tool call with no following result (a dangling call). Both compactors
// preserve the tail verbatim, so the assembled slice inherits the dangling call —
// the post-assembly validator must trip and force abort-to-original. (The cut is
// well past the dangle so the head/middle split cannot accidentally drop it.)
func danglingTailConversation() *session.Conversation {
	conv := &session.Conversation{}
	conv.Append(session.NewSystemMessage("system rules"))
	conv.Append(session.NewUserMessage("goal"))
	// A run of clean pairs to give the head/middle something to summarise.
	for i := 0; i < 3; i++ {
		id := session.ToolCallID(string(rune('a' + i)))
		conv.Append(session.NewAssistantMessage("", "", []session.ToolCall{
			session.NewToolCall(id, "Read", json.RawMessage(`{"path":"f.go"}`)),
		}))
		conv.Append(session.NewToolMessage(session.NewToolResult(id, "body")))
	}
	// The final message dangles: an assistant tool call with NO following result.
	conv.Append(session.NewAssistantMessage("", "", []session.ToolCall{
		session.NewToolCall("dangle", "Read", json.RawMessage(`{"path":"g.go"}`)),
	}))
	return conv
}

// assertAbortedToOriginal checks the compactor returned the sentinel AND the
// original conv.Messages unchanged (the fallback contract).
func assertAbortedToOriginal(t *testing.T, conv *session.Conversation, out []session.Message, err error) {
	t.Helper()
	if !errors.Is(err, agent.ErrCompactionWouldOrphan) {
		t.Fatalf("err = %v, want ErrCompactionWouldOrphan", err)
	}
	if out == nil {
		t.Fatalf("aborted compaction returned a nil slice, want the original history")
	}
	if !reflect.DeepEqual(out, conv.Messages) {
		t.Fatalf("aborted compaction did not return the original history:\n got %+v\nwant %+v", out, conv.Messages)
	}
}

// TestHeuristicCompactorAbortsToOriginalOnOrphan checks the heuristic compactor
// aborts to the ORIGINAL history (returning ErrCompactionWouldOrphan) when its
// only possible output would be tool-pairing-invalid (a dangling tail call).
func TestHeuristicCompactorAbortsToOriginalOnOrphan(t *testing.T) {
	conv := danglingTailConversation()
	// KeepLastTurns 1 preserves only the dangling assistant call as the tail; the
	// summary head carries the matched pairs, so the assembled slice dangles.
	out, _, err := agent.HeuristicCompactor{KeepLastTurns: 1}.Compact(context.Background(), conv)
	assertAbortedToOriginal(t, conv, out, err)
}

// TestCascadeCompactorAbortsToOriginalOnOrphan is the cascade parity: a dangling
// tail call forces the cascade's finish self-validation to abort to original.
func TestCascadeCompactorAbortsToOriginalOnOrphan(t *testing.T) {
	conv := danglingTailConversation()
	out, _, err := agent.CascadeCompactor{KeepLastTurns: 1}.Compact(context.Background(), conv)
	assertAbortedToOriginal(t, conv, out, err)
}

// TestCompactionThroughLoopNeverOrphans is the end-to-end regression: it drives
// the REAL agent loop with the REAL HeuristicCompactor and a mockllm script that
// emits genuine tool calls, so the conversation accumulates assistant-call /
// tool-result pairs. A tiny ContextWindowTokens makes maybeCompact fire mid-run.
// The bug was: the count-based tail cut landed on a tool result whose call was
// dropped → orphaned replay → provider HTTP 400 → StateFailed (permanently
// bricked). With the fix the run completes, the final history is tool-pairing
// valid, and the session is NOT failed. This assertion FAILS if the bug regresses
// (ReplaceHistory now rejects an unpaired slice; absent the snap the compactor
// would have produced one and the assertion below would trip).
func TestCompactionThroughLoopNeverOrphans(t *testing.T) {
	mk := func(id string) session.ToolCall {
		return session.NewToolCall(session.ToolCallID(id), "Read", json.RawMessage(`{"path":"f.go"}`))
	}
	// Several tool-call turns build up paired history, then a final text turn ends
	// the run. maybeCompact runs at the start of each turn, so by the later turns
	// the accumulated pairs trip the threshold.
	llm := mockllm.New(
		mockllm.ToolCallTurn(mk("c1")),
		mockllm.ToolCallTurn(mk("c2")),
		mockllm.ToolCallTurn(mk("c3")),
		mockllm.ToolCallTurn(mk("c4")),
		mockllm.ToolCallTurn(mk("c5")),
		mockllm.TextTurn("done"),
	)

	// A capturing diag distinguishes a GENUINE compaction (the snap kept the
	// history paired, so ReplaceHistory accepted it) from abort-to-original (the
	// safety net firing the "continuing without compaction" WARN). Without this
	// assertion the test passes even with snapCutToTurnBoundary reverted, because
	// abort-to-original keeps the session healthy — proving only the net, not the
	// snap.
	//
	// KeepLastTurns is ODD (3) so the count cut (len-3) lands on a TOOL RESULT in
	// the alternating [prompt, A1,T1, A2,T2, ...] history the loop builds (tool
	// results sit at even indices ≥2; len-3 is even when len is odd, which it is
	// after each tool turn). Reverting the snap therefore makes the real compactor
	// emit an orphan → ReplaceHistory rejects → the WARN below fires → red.
	diag := newCapturingDiag()
	e := agent.NewEngine(agent.Deps{
		LLM:                 llm,
		Catalog:             catalogWith(t, readBodyTool()),
		Policy:              allowAll(),
		Model:               "m",
		Compactor:           agent.HeuristicCompactor{KeepLastTurns: 3},
		ContextWindowTokens: 200, // threshold 160 chars/4: trips after ~2 big pairs
		CompactionRatio:     0.8,
		Diagnostics:         diag,
	})

	sess := session.New("s-compact", session.ModeDefault, "/ws", session.Limits{}, time.Unix(0, 0))
	r := e.Run(context.Background(), sess, memfs.NewWorkspace("/ws"), "investigate the files")

	var sawCompaction bool
	for ev := range r.Events() {
		if ev.Type == session.EvCompaction {
			sawCompaction = true
		}
	}

	// The run must NOT have failed (a 400-from-orphan would land it in StateFailed).
	if sess.State == session.StateFailed {
		reason, _ := sess.StopReason()
		t.Fatalf("session reached StateFailed: %v", reason)
	}
	// The replayed history must be tool-pairing valid throughout.
	if err := session.ValidateToolPairing(sess.Conversation.Messages); err != nil {
		t.Fatalf("final conversation has unpaired tools: %v", err)
	}
	// Compaction GENUINELY succeeded — the snap kept the slice paired, so the loop
	// never fell back to the degraded "continuing without compaction" branch. This
	// is the assertion that distinguishes a real compaction from the safety net:
	// revert snapCutToTurnBoundary and the real compactor emits an unpaired slice,
	// the loop aborts to original, this WARN fires (and EvCompaction is skipped) —
	// turning BOTH checks below red. (Self-checked: reverting the snap fails here.)
	if rec, ok := findMsg(diag.snapshot(), "continuing without compaction"); ok {
		t.Fatalf("compaction aborted to original (degraded), want genuine compaction: %q", rec.msg)
	}
	if !sawCompaction {
		t.Fatalf("compaction never triggered; the test does not exercise the bug")
	}
}

// TestCompactionEmitsNonDestructiveArchive is the cloud-native Phase 3b
// compaction-archive sub-gate at the loop level: a genuine compaction must emit
// EvCompactionArchive AFTER EvCompaction carrying the PRE-compaction conversation
// (the span ReplaceHistory dropped) — captured BEFORE the mutation. The archive
// must contain a tool call that compaction dropped from the live history, proving
// the capture is the pre-mutation slice and not the rewritten tail.
//
// MUTATION-KILL: move `archived := sess.Conversation.Messages` to AFTER
// ReplaceHistory in maybeCompact (so it reads the mutated, compacted conversation)
// and the archive no longer contains the dropped early tool call — the assertion
// that a dropped call is recoverable from the archive (but absent from the live
// history) then fails.
func TestCompactionEmitsNonDestructiveArchive(t *testing.T) {
	mk := func(id string) session.ToolCall {
		return session.NewToolCall(session.ToolCallID(id), "Read", json.RawMessage(`{"path":"f.go"}`))
	}
	// Build genuine paired history over several tool turns, then a final text turn.
	llm := mockllm.New(
		mockllm.ToolCallTurn(mk("c1")),
		mockllm.ToolCallTurn(mk("c2")),
		mockllm.ToolCallTurn(mk("c3")),
		mockllm.TextTurn("done"),
	)
	// A compactor that records the EXACT slice it was handed (the pre-compaction
	// history) and returns a TINY, tool-pairing-valid output — so compaction fires
	// EXACTLY ONCE (the compacted history is far under threshold, so no later turn
	// re-trips it), making the pre-vs-post distinction deterministic.
	rec := &recordingInputCompactor{out: []session.Message{session.NewUserMessage("goal")}}
	e := agent.NewEngine(agent.Deps{
		LLM:                 llm,
		Catalog:             catalogWith(t, readBodyTool()),
		Policy:              allowAll(),
		Model:               "m",
		Compactor:           rec,
		ContextWindowTokens: 200,
		CompactionRatio:     0.8,
	})

	sess := session.New("s-archive", session.ModeDefault, "/ws", session.Limits{}, time.Unix(0, 0))
	r := e.Run(context.Background(), sess, memfs.NewWorkspace("/ws"), "investigate the files")

	compactions, archives := 0, 0
	lastWasCompaction := false
	var archived []session.Message
	for ev := range r.Events() {
		switch ev.Type {
		case session.EvCompaction:
			compactions++
			lastWasCompaction = true
			continue
		case session.EvCompactionArchive:
			archives++
			if !lastWasCompaction {
				t.Fatalf("EvCompactionArchive #%d did not immediately follow an EvCompaction notice", archives)
			}
			if ev.CompactionArchive == nil {
				t.Fatalf("EvCompactionArchive carries no payload")
			}
			archived = ev.CompactionArchive.Replaced
		}
		lastWasCompaction = false
	}

	if sess.State == session.StateFailed {
		reason, _ := sess.StopReason()
		t.Fatalf("session reached StateFailed: %v", reason)
	}
	if compactions != 1 {
		t.Fatalf("expected exactly one compaction, got %d (the test needs a single deterministic compaction)", compactions)
	}
	if archives != 1 {
		t.Fatalf("expected exactly one EvCompactionArchive, got %d", archives)
	}
	if len(archived) == 0 {
		t.Fatalf("archive is empty: the pre-compaction span was not captured")
	}

	// The archive MUST equal the slice the compactor was handed — i.e. the genuine
	// pre-compaction history captured BEFORE ReplaceHistory ran. A post-mutation
	// capture would instead equal the compacted output (rec.out, the tiny tail).
	if !reflect.DeepEqual(archived, rec.input) {
		t.Fatalf("archive is not the pre-compaction slice the compactor saw:\n archive=%v\n input=%v", archived, rec.input)
	}
	// And it must hold the early tool calls that compaction DROPPED from the live
	// history (c1/c2/c3 are gone from the compacted [goal] history) — the
	// non-destructive recovery the gate proves.
	archivedCalls := callIDsIn(archived)
	liveCalls := callIDsIn(sess.Conversation.Messages)
	recoveredDropped := false
	for id := range archivedCalls {
		if _, stillLive := liveCalls[id]; !stillLive {
			recoveredDropped = true
			break
		}
	}
	if !recoveredDropped {
		t.Fatalf("archive holds no tool call that compaction dropped from the live history; "+
			"archive ids=%v live ids=%v (capture must be the PRE-mutation slice)", keysOf(archivedCalls), keysOf(liveCalls))
	}
}

// recordingInputCompactor records the EXACT message slice it was handed (the
// pre-compaction history) and returns a fixed, tool-pairing-valid output. The
// recorded input is the oracle the archive must equal.
type recordingInputCompactor struct {
	input []session.Message
	out   []session.Message
}

func (c *recordingInputCompactor) Compact(_ context.Context, conv *session.Conversation) ([]session.Message, string, error) {
	// Snapshot the input slice (the conversation the loop captures for the archive).
	c.input = append([]session.Message(nil), conv.Messages...)
	return c.out, "compacted summary", nil
}

// callIDsIn collects the set of assistant tool-call ids across a message slice.
func callIDsIn(msgs []session.Message) map[session.ToolCallID]struct{} {
	out := make(map[session.ToolCallID]struct{})
	for _, m := range msgs {
		for _, c := range m.ToolCalls {
			out[c.ID] = struct{}{}
		}
	}
	return out
}

// keysOf projects a tool-call-id set to a slice for a failure message.
func keysOf(set map[session.ToolCallID]struct{}) []session.ToolCallID {
	out := make([]session.ToolCallID, 0, len(set))
	for id := range set {
		out = append(out, id)
	}
	return out
}

// fakeCompactor returns a fixed slice/error from Compact, for driving the loop's
// defensive paths deterministically.
type fakeCompactor struct {
	out []session.Message
	err error
}

func (f fakeCompactor) Compact(_ context.Context, _ *session.Conversation) ([]session.Message, string, error) {
	return f.out, "fake", f.err
}

// TestCompactionThroughLoopAbortsToOriginal drives the loop with a FAKE compactor
// (not the real HeuristicCompactor) in two variants, each exercising a distinct
// abort path, and asserts the run survives uncompacted: NO EvCompaction event,
// session NOT failed, and the conversation unchanged from the pre-compaction
// snapshot. This mirrors the live repro (compact-attempt → abort → survive
// instead of brick) and is the only end-to-end coverage of the loop's defensive
// branches.
func TestCompactionThroughLoopAbortsToOriginal(t *testing.T) {
	// An unpaired slice the loop must refuse: a tool result with no preceding call.
	unpaired := []session.Message{
		session.NewUserMessage("goal"),
		session.NewToolMessage(session.NewToolResult("ghost", "result")),
	}

	variants := []struct {
		name      string
		compactor agent.Compactor
	}{
		{
			// (a) NIL error + unpaired slice: exercises maybeCompact's defensive
			// ValidateToolPairing branch between Compact and ReplaceHistory.
			name:      "nil error unpaired slice",
			compactor: fakeCompactor{out: unpaired, err: nil},
		},
		{
			// (b) ErrCompactionWouldOrphan sentinel: exercises the sentinel branch
			// (reusing the Compact-error WARN).
			name:      "ErrCompactionWouldOrphan sentinel",
			compactor: fakeCompactor{out: nil, err: agent.ErrCompactionWouldOrphan},
		},
	}

	for _, v := range variants {
		t.Run(v.name, func(t *testing.T) {
			llm := mockllm.New(mockllm.TextTurn("done"))
			e := agent.NewEngine(agent.Deps{
				LLM:                 llm,
				Catalog:             catalogWith(t),
				Policy:              allowAll(),
				Model:               "m",
				Compactor:           v.compactor,
				ContextWindowTokens: 10, // tiny: the prompt trips the threshold
				CompactionRatio:     0.8,
			})

			sess := session.New("s-abort", session.ModeDefault, "/ws", session.Limits{}, time.Unix(0, 0))
			bigPrompt := strings.Repeat("word ", 200)
			r := e.Run(context.Background(), sess, memfs.NewWorkspace("/ws"), bigPrompt)

			var sawCompaction, sawArchive bool
			for ev := range r.Events() {
				if ev.Type == session.EvCompaction {
					sawCompaction = true
				}
				if ev.Type == session.EvCompactionArchive {
					sawArchive = true
				}
			}

			// The history the model saw must equal what the prompt produced — the
			// compactor's (bad) output must NEVER have been applied.
			if sawCompaction {
				t.Fatalf("EvCompaction emitted despite abort-to-original")
			}
			// The degrade-and-continue branch emits NO archive (no compaction happened,
			// so there is no replaced span to record).
			if sawArchive {
				t.Fatalf("EvCompactionArchive emitted despite abort-to-original")
			}
			if sess.State == session.StateFailed {
				reason, _ := sess.StopReason()
				t.Fatalf("session reached StateFailed instead of surviving: %v", reason)
			}
			if err := session.ValidateToolPairing(sess.Conversation.Messages); err != nil {
				t.Fatalf("conversation was corrupted by the rejected compaction: %v", err)
			}
			// The conversation must NOT contain the fake compactor's ghost result.
			for _, m := range sess.Conversation.Messages {
				if m.ToolResult != nil && m.ToolResult.CallID == "ghost" {
					t.Fatalf("rejected compaction output leaked into history")
				}
			}
		})
	}
}

// readBodyTool returns a read-only Read tool whose body is large enough to grow
// the history quickly, for the through-loop compaction test.
func readBodyTool() *fakeTool {
	return &fakeTool{
		name:     "Read",
		readOnly: true,
		exec: func(_ context.Context, in session.ToolCall, _ tool.Workspace) (session.ToolResult, error) {
			return session.NewToolResult(in.ID, strings.Repeat("data ", 50)), nil
		},
	}
}

// recordingCompactor records that it was invoked and returns a minimal history.
type recordingCompactor struct{ called int }

func (c *recordingCompactor) Compact(_ context.Context, _ *session.Conversation) ([]session.Message, string, error) {
	c.called++
	return []session.Message{session.NewUserMessage("compacted goal")}, "compacted summary", nil
}

// TestCompactionTriggersAtThreshold drives the loop with a tiny context window so
// the threshold trips, and asserts the Compactor runs and a compaction Event is
// emitted.
func TestCompactionTriggersAtThreshold(t *testing.T) {
	rc := &recordingCompactor{}
	llm := mockllm.New(mockllm.TextTurn("done"))
	e := agent.NewEngine(agent.Deps{
		LLM:                 llm,
		Catalog:             catalogWith(t),
		Policy:              allowAll(),
		Model:               "m",
		Compactor:           rc,
		ContextWindowTokens: 10, // tiny: a long prompt blows past 80% of it
		CompactionRatio:     0.8,
	})

	sess := session.New("s1", session.ModeDefault, "/ws", session.Limits{}, time.Unix(0, 0))
	bigPrompt := strings.Repeat("word ", 200) // ~250 tokens >> threshold of 8
	r := e.Run(context.Background(), sess, memfs.NewWorkspace("/ws"), bigPrompt)

	var sawCompaction bool
	for ev := range r.Events() {
		if ev.Type == session.EvCompaction {
			sawCompaction = true
		}
	}
	if rc.called == 0 {
		t.Fatalf("compactor was not invoked")
	}
	if !sawCompaction {
		t.Fatalf("no compaction event emitted")
	}
}

// countingTokenCounter records invocations and reports a fixed huge count so the
// loop's compaction trigger always trips through the injected counter.
type countingTokenCounter struct {
	calls int
	fixed int
}

func (*countingTokenCounter) Count(string) int { return 0 }
func (c *countingTokenCounter) CountMessages(_ []session.Message) int {
	c.calls++
	return c.fixed
}

// TestCompactionTriggerUsesInjectedCounter checks the loop drives its compaction
// threshold through the injected TokenCounter (not a hard-coded estimate): a
// counter reporting over-threshold trips compaction; one reporting under does not.
func TestCompactionTriggerUsesInjectedCounter(t *testing.T) {
	run := func(reported int) (compacted bool, counterCalls int) {
		rc := &recordingCompactor{}
		tc := &countingTokenCounter{fixed: reported}
		llm := mockllm.New(mockllm.TextTurn("done"))
		e := agent.NewEngine(agent.Deps{
			LLM:                 llm,
			Catalog:             catalogWith(t),
			Policy:              allowAll(),
			Model:               "m",
			Compactor:           rc,
			TokenCounter:        tc,
			ContextWindowTokens: 100,
			CompactionRatio:     0.8, // threshold = 80
		})
		sess := session.New("s", session.ModeDefault, "/ws", session.Limits{}, time.Unix(0, 0))
		r := e.Run(context.Background(), sess, memfs.NewWorkspace("/ws"), "hi")
		for range r.Events() {
		}
		return rc.called > 0, tc.calls
	}

	// Over threshold (90 >= 80): compaction trips, and the injected counter was used.
	over, overCalls := run(90)
	if !over {
		t.Fatalf("compaction did not trigger when counter reported over threshold")
	}
	if overCalls == 0 {
		t.Fatalf("injected counter was never consulted")
	}

	// Under threshold (10 < 80): compaction does not trip.
	under, _ := run(10)
	if under {
		t.Fatalf("compaction triggered when counter reported under threshold")
	}
}
