package agent_test

import (
	"context"
	"sync"
	"testing"
	"time"

	"github.com/stacklok/mecatl/engine/agent"
	"github.com/stacklok/mecatl/engine/session"
	"github.com/stacklok/mecatl/engine/tool"
	"github.com/stacklok/mecatl/internal/adapter/memfs"
	"github.com/stacklok/mecatl/internal/adapter/mockllm"
)

// scriptedClock returns a pre-scripted sequence of timestamps, one per Now()
// call, so a test can pin the exact instants the loop reads. After the script is
// exhausted it keeps returning the final value (so unrelated trailing reads do
// not panic). It is the controlled clock the TTFT / inter-token measurement tests
// drive instead of the 1ms-step fakeClock, which cannot express specific gaps.
type scriptedClock struct {
	mu    sync.Mutex
	times []time.Time
	i     int
}

func (c *scriptedClock) Now() time.Time {
	c.mu.Lock()
	defer c.mu.Unlock()
	if c.i >= len(c.times) {
		if len(c.times) == 0 {
			return time.Unix(0, 0)
		}
		return c.times[len(c.times)-1]
	}
	t := c.times[c.i]
	c.i++
	return t
}

// at builds a timestamp at ms milliseconds past the Unix epoch.
func at(ms int64) time.Time { return time.Unix(0, 0).Add(time.Duration(ms) * time.Millisecond) }

// turnEndOf returns the TurnEndPayload of the first turn.end event, or fails.
func turnEndOf(t *testing.T, evs []session.Event) *session.TurnEndPayload {
	t.Helper()
	for _, e := range evs {
		if e.Type == session.EvTurnEnd {
			if e.TurnEnd == nil {
				t.Fatalf("turn.end event carried a nil TurnEnd payload")
			}
			return e.TurnEnd
		}
	}
	t.Fatalf("no turn.end event in %v", typesOf(evs))
	return nil
}

// TestTurnEndLatencyMeasured drives a single streamed turn whose content chunks
// arrive at scripted instants and asserts TTFT, the mean inter-token gap, and the
// max gap are computed from the injected Clock.
//
// The clock is read in this order for a single-turn run:
//
//	[0] turnStart   (drive, before runTurn)
//	[1] streamStart  (runTurn, anchors TTFT)
//	[2] content #1 (text)      → TTFT = t2 - t1
//	[3] content #2 (text)      → gap1 = t3 - t2
//	[4] content #3 (text)      → gap2 = t4 - t3
//	[5] durMs       (drive, after runTurn)
func TestTurnEndLatencyMeasured(t *testing.T) {
	llm := mockllm.New(
		mockllm.ChunksTurn(
			mockllm.TextChunk("a"),
			mockllm.TextChunk("b"),
			mockllm.TextChunk("c"),
			mockllm.UsageChunk(session.Usage{}),
			mockllm.DoneChunk(session.StopEndTurn),
		),
	)
	// streamStart=10ms; first token at 40ms (TTFT=30); then 50ms (gap 10) and
	// 90ms (gap 40). Mean gap = (10+40)/2 = 25, max = 40. turnStart=5, durMs read
	// at 200ms (turn duration = 195).
	clk := &scriptedClock{times: []time.Time{
		at(5), at(10), at(40), at(50), at(90), at(200),
	}}
	e := newEngine(agent.Deps{LLM: llm, Catalog: catalogWith(t), Clock: clk})
	sess := newSession(t, session.Limits{})
	r := e.Run(context.Background(), sess, memfs.NewWorkspace("/ws"), "go")
	evs := drain(r)

	te := turnEndOf(t, evs)
	if te.TTFTMs != 30 {
		t.Errorf("TTFTMs = %d, want 30", te.TTFTMs)
	}
	if te.InterTokenMeanMs != 25 {
		t.Errorf("InterTokenMeanMs = %d, want 25", te.InterTokenMeanMs)
	}
	if te.InterTokenMaxMs != 40 {
		t.Errorf("InterTokenMaxMs = %d, want 40", te.InterTokenMaxMs)
	}
	if te.DurationMs != 195 {
		t.Errorf("DurationMs = %d, want 195", te.DurationMs)
	}
}

// TestTurnEndLatencyMaxIsFirstGap drives a turn whose LARGEST inter-token gap is
// the FIRST one (40ms), followed by a smaller gap (10ms). It asserts the max
// tracks the running maximum (40), not merely the most-recent gap — a regression
// where interTokenMaxMs latched the last gap would wrongly report 10. The mean is
// (40+10)/2 = 25, identical to TestTurnEndLatencyMeasured's, so only the max
// discriminates the two orderings.
//
// Clock reads: [0] turnStart=5, [1] streamStart=10, [2] content #1=20 (TTFT=10),
// [3] content #2=60 (gap1=40), [4] content #3=70 (gap2=10), [5] durMs=210.
func TestTurnEndLatencyMaxIsFirstGap(t *testing.T) {
	llm := mockllm.New(
		mockllm.ChunksTurn(
			mockllm.TextChunk("a"),
			mockllm.TextChunk("b"),
			mockllm.TextChunk("c"),
			mockllm.UsageChunk(session.Usage{}),
			mockllm.DoneChunk(session.StopEndTurn),
		),
	)
	clk := &scriptedClock{times: []time.Time{
		at(5), at(10), at(20), at(60), at(70), at(210),
	}}
	e := newEngine(agent.Deps{LLM: llm, Catalog: catalogWith(t), Clock: clk})
	sess := newSession(t, session.Limits{})
	r := e.Run(context.Background(), sess, memfs.NewWorkspace("/ws"), "go")
	evs := drain(r)

	te := turnEndOf(t, evs)
	if te.InterTokenMaxMs != 40 {
		t.Errorf("InterTokenMaxMs = %d, want 40 (the FIRST gap is the largest)", te.InterTokenMaxMs)
	}
	if te.InterTokenMeanMs != 25 { // (40 + 10) / 2
		t.Errorf("InterTokenMeanMs = %d, want 25", te.InterTokenMeanMs)
	}
}

// TestTurnEndLatencyReasoningCountsAsContent asserts a ChunkReasoning (the
// human-readable summary) counts as a content chunk for TTFT/inter-token, while a
// ChunkReasoningItem (the opaque replay blob), usage, and done chunks do NOT.
//
// Clock reads: [0] turnStart, [1] streamStart, [2] reasoning #1 → TTFT, [3] text
// #2 → gap, [4] durMs. The ReasoningItem/Usage/Done chunks consume no clock read.
func TestTurnEndLatencyReasoningCountsAsContent(t *testing.T) {
	llm := mockllm.New(
		mockllm.ChunksTurn(
			mockllm.ReasoningChunk("thinking"),
			mockllm.ReasoningItemChunk("BLOB"), // not content: no clock read, no gap
			mockllm.TextChunk("answer"),
			mockllm.UsageChunk(session.Usage{}),
			mockllm.DoneChunk(session.StopEndTurn),
		),
	)
	clk := &scriptedClock{times: []time.Time{
		at(0), at(20), at(35), at(60), at(100),
	}}
	e := newEngine(agent.Deps{LLM: llm, Catalog: catalogWith(t), Clock: clk})
	sess := newSession(t, session.Limits{})
	r := e.Run(context.Background(), sess, memfs.NewWorkspace("/ws"), "go")
	evs := drain(r)

	te := turnEndOf(t, evs)
	if te.TTFTMs != 15 { // 35 - 20
		t.Errorf("TTFTMs = %d, want 15", te.TTFTMs)
	}
	if te.InterTokenMeanMs != 25 { // single gap 60 - 35
		t.Errorf("InterTokenMeanMs = %d, want 25", te.InterTokenMeanMs)
	}
	if te.InterTokenMaxMs != 25 {
		t.Errorf("InterTokenMaxMs = %d, want 25", te.InterTokenMaxMs)
	}
}

// TestTurnEndLatencyOneContentChunk: a turn with exactly one content chunk has a
// TTFT but NO inter-token summary (no gap exists) — both gap fields must stay 0,
// never a bogus zero observation.
func TestTurnEndLatencyOneContentChunk(t *testing.T) {
	llm := mockllm.New(
		mockllm.ChunksTurn(
			mockllm.TextChunk("only"),
			mockllm.UsageChunk(session.Usage{}),
			mockllm.DoneChunk(session.StopEndTurn),
		),
	)
	clk := &scriptedClock{times: []time.Time{at(0), at(5), at(25), at(50)}}
	e := newEngine(agent.Deps{LLM: llm, Catalog: catalogWith(t), Clock: clk})
	sess := newSession(t, session.Limits{})
	r := e.Run(context.Background(), sess, memfs.NewWorkspace("/ws"), "go")
	evs := drain(r)

	te := turnEndOf(t, evs)
	if te.TTFTMs != 20 { // 25 - 5
		t.Errorf("TTFTMs = %d, want 20", te.TTFTMs)
	}
	if te.InterTokenMeanMs != 0 || te.InterTokenMaxMs != 0 {
		t.Errorf("inter-token = (%d,%d), want (0,0) for a single content chunk",
			te.InterTokenMeanMs, te.InterTokenMaxMs)
	}
}

// TestTurnEndLatencyNoContent: a turn whose only chunks are a tool call (plus
// usage/done) produces NO content chunk, so TTFT and both inter-token fields must
// be 0 (not measured) — never a bogus zero.
func TestTurnEndLatencyNoContent(t *testing.T) {
	llm := mockllm.New(
		mockllm.ChunksTurn(
			mockllm.ToolCallChunk(toolCall("c1", "noop", `{}`)),
			mockllm.UsageChunk(session.Usage{}),
			mockllm.DoneChunk(session.StopEndTurn),
		),
		// Second turn ends the run (the tool result feeds back; the model stops).
		mockllm.ChunksTurn(
			mockllm.TextChunk("done"),
			mockllm.UsageChunk(session.Usage{}),
			mockllm.DoneChunk(session.StopEndTurn),
		),
	)
	noop := &fakeTool{name: "noop", readOnly: true,
		exec: func(_ context.Context, in session.ToolCall, _ tool.Workspace) (session.ToolResult, error) {
			return session.NewToolResult(in.ID, "ok"), nil
		}}
	clk := &scriptedClock{} // value-less script: every read returns the epoch
	e := newEngine(agent.Deps{LLM: llm, Catalog: catalogWith(t, noop), Clock: clk})
	sess := newSession(t, session.Limits{})
	r := e.Run(context.Background(), sess, memfs.NewWorkspace("/ws"), "go")
	evs := drain(r)

	// The FIRST turn.end is the tool-call-only turn: no content → no TTFT/gaps.
	te := turnEndOf(t, evs)
	if te.TTFTMs != 0 {
		t.Errorf("TTFTMs = %d, want 0 (no content chunk)", te.TTFTMs)
	}
	if te.InterTokenMeanMs != 0 || te.InterTokenMaxMs != 0 {
		t.Errorf("inter-token = (%d,%d), want (0,0) (no content chunk)",
			te.InterTokenMeanMs, te.InterTokenMaxMs)
	}
}

// TestTurnEndLatencyNoClock: with no Clock injected every latency field is 0.
func TestTurnEndLatencyNoClock(t *testing.T) {
	llm := mockllm.New(
		mockllm.ChunksTurn(
			mockllm.TextChunk("a"),
			mockllm.TextChunk("b"),
			mockllm.UsageChunk(session.Usage{}),
			mockllm.DoneChunk(session.StopEndTurn),
		),
	)
	e := newEngine(agent.Deps{LLM: llm, Catalog: catalogWith(t)}) // no Clock
	sess := newSession(t, session.Limits{})
	r := e.Run(context.Background(), sess, memfs.NewWorkspace("/ws"), "go")
	evs := drain(r)

	te := turnEndOf(t, evs)
	if te.TTFTMs != 0 || te.InterTokenMeanMs != 0 || te.InterTokenMaxMs != 0 || te.DurationMs != 0 {
		t.Errorf("latency fields = (ttft=%d mean=%d max=%d dur=%d), want all 0 without a Clock",
			te.TTFTMs, te.InterTokenMeanMs, te.InterTokenMaxMs, te.DurationMs)
	}
}

// queueRecordingLogger captures the queued/took durations passed to ToolCall,
// keyed by tool name, so a test can assert mutate-serial queueing was measured.
type queueRecordingLogger struct {
	mu     sync.Mutex
	queued map[string]time.Duration
	took   map[string]time.Duration
	order  []string
}

func (l *queueRecordingLogger) ToolCall(_ session.SessionID, call session.ToolCall, _ session.ToolResult, queued, took time.Duration) {
	l.mu.Lock()
	defer l.mu.Unlock()
	if l.queued == nil {
		l.queued = map[string]time.Duration{}
		l.took = map[string]time.Duration{}
	}
	l.queued[call.Name] = queued
	l.took[call.Name] = took
	l.order = append(l.order, call.Name)
}

// TestDispatchQueueTimeMeasured asserts queue time is captured from enqueue to
// execution start and passed to Logger.ToolCall, and — crucially — that a second
// mutating tool serialized BEHIND the first records a strictly larger queue time
// than the first (the coordinated-omission measure). The 1ms-step fakeClock makes
// every Now() read advance time, so the later-executing mutate tool, which sits
// through the first tool's gate+execution clock reads, sees a larger enqueue→start
// gap.
func TestDispatchQueueTimeMeasured(t *testing.T) {
	// Two mutating tools in one turn → dispatched serially, one after the other.
	first := &fakeTool{name: "edit_a", readOnly: false,
		exec: func(_ context.Context, in session.ToolCall, _ tool.Workspace) (session.ToolResult, error) {
			return session.NewToolResult(in.ID, "a"), nil
		}}
	second := &fakeTool{name: "edit_b", readOnly: false,
		exec: func(_ context.Context, in session.ToolCall, _ tool.Workspace) (session.ToolResult, error) {
			return session.NewToolResult(in.ID, "b"), nil
		}}

	llm := mockllm.New(
		mockllm.ChunksTurn(
			mockllm.ToolCallChunk(toolCall("c1", "edit_a", `{}`)),
			mockllm.ToolCallChunk(toolCall("c2", "edit_b", `{}`)),
			mockllm.UsageChunk(session.Usage{}),
			mockllm.DoneChunk(session.StopEndTurn),
		),
		mockllm.ChunksTurn(
			mockllm.TextChunk("done"),
			mockllm.UsageChunk(session.Usage{}),
			mockllm.DoneChunk(session.StopEndTurn),
		),
	)
	clk := &fakeClock{t: time.Unix(0, 0)} // advances 1ms per Now()
	logger := &queueRecordingLogger{}
	e := newEngine(agent.Deps{
		LLM: llm, Catalog: catalogWith(t, first, second), Clock: clk, ToolCallRecorder: logger,
	})
	sess := newSession(t, session.Limits{})
	r := e.Run(context.Background(), sess, memfs.NewWorkspace("/ws"), "go")
	_ = drain(r)

	logger.mu.Lock()
	defer logger.mu.Unlock()
	qa, okA := logger.queued["edit_a"]
	qb, okB := logger.queued["edit_b"]
	if !okA || !okB {
		t.Fatalf("missing queued samples: edit_a=%v edit_b=%v (order=%v)", okA, okB, logger.order)
	}
	// Both calls enqueued at the SAME instant (one enqueue read for the whole
	// turn); edit_b executes only after edit_a's gate+execution advanced the clock,
	// so its enqueue→start wait is strictly larger.
	if qb <= qa {
		t.Errorf("edit_b queue time (%v) not > edit_a queue time (%v); mutate-serial queueing not measured", qb, qa)
	}
	// edit_a still sat through at least its own authorize/openCard clock reads
	// before execution started, so its queue time is nonzero too.
	if qa <= 0 {
		t.Errorf("edit_a queue time = %v, want > 0", qa)
	}
}
