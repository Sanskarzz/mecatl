package agent_test

import (
	"context"
	"encoding/json"
	"fmt"
	"iter"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/stacklok/mecatl/internal/adapter/hookexec"
	"github.com/stacklok/mecatl/internal/adapter/memfs"
	"github.com/stacklok/mecatl/internal/adapter/mockllm"
	"github.com/stacklok/mecatl/internal/adapter/permpolicy"
	"github.com/stacklok/mecatl/internal/agent"
	"github.com/stacklok/mecatl/internal/governance"
	"github.com/stacklok/mecatl/internal/port"
	"github.com/stacklok/mecatl/internal/session"
	"github.com/stacklok/mecatl/internal/tool"
)

// --- test doubles -----------------------------------------------------------

// fakeTool is an instrumented Tool whose read-only flag, execution body, and
// concurrency observation are controllable from a test.
type fakeTool struct {
	name     string
	readOnly bool
	exec     func(ctx context.Context, in session.ToolCall, ws tool.Workspace) (session.ToolResult, error)
}

func (f *fakeTool) Spec() tool.ToolSpec {
	return tool.ToolSpec{Name: f.name, Description: f.name + ": test tool", Schema: json.RawMessage(`{"type":"object"}`)}
}
func (f *fakeTool) ReadOnly() bool { return f.readOnly }
func (f *fakeTool) Execute(ctx context.Context, in session.ToolCall, ws tool.Workspace) (session.ToolResult, error) {
	return f.exec(ctx, in, ws)
}

// overlapTracker records the maximum number of concurrently-running tool
// executions observed, so tests can assert read-parallel vs mutate-serial.
type overlapTracker struct {
	mu      sync.Mutex
	running int
	maxObs  int
}

func (o *overlapTracker) enter() {
	o.mu.Lock()
	o.running++
	if o.running > o.maxObs {
		o.maxObs = o.running
	}
	o.mu.Unlock()
}
func (o *overlapTracker) leave() {
	o.mu.Lock()
	o.running--
	o.mu.Unlock()
}
func (o *overlapTracker) max() int {
	o.mu.Lock()
	defer o.mu.Unlock()
	return o.maxObs
}

// fakeClock advances by a fixed step on each Now() call so tool timing is
// deterministic.
type fakeClock struct {
	mu sync.Mutex
	t  time.Time
}

func (c *fakeClock) Now() time.Time {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.t = c.t.Add(time.Millisecond)
	return c.t
}

// recordingLogger captures tool-call observability for assertions.
type recordingLogger struct {
	mu      sync.Mutex
	calls   int
	results []session.ToolResult
}

func (l *recordingLogger) ToolCall(_ session.SessionID, _ session.ToolCall, result session.ToolResult, _, _ time.Duration) {
	l.mu.Lock()
	l.calls++
	l.results = append(l.results, result)
	l.mu.Unlock()
}

// lastResult returns the most recently logged tool result. Call it after the run
// completes (drain has returned), when no logging goroutine is still active.
func (l *recordingLogger) lastResult() (session.ToolResult, bool) {
	l.mu.Lock()
	defer l.mu.Unlock()
	if len(l.results) == 0 {
		return session.ToolResult{}, false
	}
	return l.results[len(l.results)-1], true
}

// --- helpers ----------------------------------------------------------------

func toolCall(id, name string, args string) session.ToolCall {
	return session.NewToolCall(session.ToolCallID(id), name, json.RawMessage(args))
}

func newSession(t *testing.T, limits session.Limits) *session.Session {
	t.Helper()
	return session.New("s1", session.ModeDefault, "/ws", limits, time.Unix(0, 0))
}

func catalogWith(t *testing.T, tools ...tool.Tool) *tool.Catalog {
	t.Helper()
	c := tool.NewCatalog()
	for _, tl := range tools {
		c.MustRegister(tl)
	}
	return c
}

// allowAll returns a policy that allows every tool call.
func allowAll() *permpolicy.Policy {
	return permpolicy.NewPolicy([]governance.Rule{{Effect: governance.Allow}}, nil)
}

// drain collects events until the channel closes, returning them in order.
func drain(r *agent.Run) []session.Event {
	var evs []session.Event
	for ev := range r.Events() {
		evs = append(evs, ev)
	}
	return evs
}

func typesOf(evs []session.Event) []session.EventType {
	out := make([]session.EventType, len(evs))
	for i, e := range evs {
		out[i] = e.Type
	}
	return out
}

func containsType(evs []session.Event, ty session.EventType) bool {
	for _, e := range evs {
		if e.Type == ty {
			return true
		}
	}
	return false
}

func lastResult(t *testing.T, evs []session.Event) *session.ResultPayload {
	t.Helper()
	for i := len(evs) - 1; i >= 0; i-- {
		if evs[i].Type == session.EvResult {
			return evs[i].Result
		}
	}
	t.Fatalf("no result event in %v", typesOf(evs))
	return nil
}

func newEngine(d agent.Deps) *agent.Engine {
	if d.Policy == nil {
		d.Policy = allowAll()
	}
	if d.Model == "" {
		d.Model = "test-model"
	}
	return agent.NewEngine(d)
}

// --- tests ------------------------------------------------------------------

// TestFullCycle exercises a full multi-turn run: text → tool call (Read) → text.
// It asserts the ordered event taxonomy and the final usage accounting (gauntlet
// #1: a complete loop runs end-to-end on mockllm).
func TestFullCycle(t *testing.T) {
	read := &fakeTool{name: "Read", readOnly: true,
		exec: func(_ context.Context, in session.ToolCall, _ tool.Workspace) (session.ToolResult, error) {
			return session.NewToolResult(in.ID, "file contents"), nil
		}}
	cat := catalogWith(t, read)

	llm := mockllm.New(
		mockllm.ChunksTurn(
			mockllm.TextChunk("let me look"),
			mockllm.ToolCallChunk(toolCall("c1", "Read", `{"path":"a.go"}`)),
			mockllm.UsageChunk(session.Usage{InputTokens: 10, OutputTokens: 2}),
			mockllm.DoneChunk(session.StopEndTurn),
		),
		mockllm.ChunksTurn(
			mockllm.TextChunk("all done"),
			mockllm.UsageChunk(session.Usage{InputTokens: 5, OutputTokens: 3}),
			mockllm.DoneChunk(session.StopEndTurn),
		),
	)

	clk := &fakeClock{t: time.Unix(0, 0)}
	logger := &recordingLogger{}
	e := newEngine(agent.Deps{LLM: llm, Catalog: cat, Clock: clk, Logger: logger})
	sess := newSession(t, session.Limits{})
	ws := memfs.NewWorkspace("/ws")

	r := e.Run(context.Background(), sess, ws, "look at a.go")
	evs := drain(r)

	if logger.calls != 1 {
		t.Fatalf("logger recorded %d tool calls, want 1", logger.calls)
	}

	for _, want := range []session.EventType{
		session.EvTurnStart, session.EvMessageDelta, session.EvToolCall,
		session.EvToolResult, session.EvResult,
	} {
		if !containsType(evs, want) {
			t.Fatalf("missing event %q in %v", want, typesOf(evs))
		}
	}
	res := lastResult(t, evs)
	if res.Stop != session.StopEndTurn {
		t.Fatalf("stop = %q, want end_turn", res.Stop)
	}
	if res.Text != "all done" {
		t.Fatalf("final text = %q", res.Text)
	}
	if got := res.Usage.InputTokens; got != 15 {
		t.Fatalf("cumulative input tokens = %d, want 15", got)
	}
	// Seq must be strictly increasing.
	for i := 1; i < len(evs); i++ {
		if evs[i].Seq <= evs[i-1].Seq {
			t.Fatalf("seq not increasing at %d: %d <= %d", i, evs[i].Seq, evs[i-1].Seq)
		}
	}
}

// TestReasoningAndTurnEnd asserts the Tier B additive events and the
// display/replay reasoning split: a ChunkReasoning (human-readable summary)
// produces a reasoning.delta DISPLAY event but is NOT what gets stored for
// replay, while a ChunkReasoningItem (the opaque encrypted_content blob) is what
// lands on Message.Reasoning to be replayed verbatim. Each successful turn also
// emits a turn.end carrying that turn's usage and a non-zero elapsed duration
// (from the injected fakeClock).
func TestReasoningAndTurnEnd(t *testing.T) {
	llm := mockllm.New(
		mockllm.ChunksTurn(
			mockllm.ReasoningChunk("let me think about it"),
			mockllm.ReasoningItemChunk("ENCRYPTED_REPLAY_BLOB"),
			mockllm.TextChunk("here is the answer"),
			mockllm.UsageChunk(session.Usage{InputTokens: 12, OutputTokens: 4}),
			mockllm.DoneChunk(session.StopEndTurn),
		),
	)
	clk := &fakeClock{t: time.Unix(0, 0)}
	e := newEngine(agent.Deps{LLM: llm, Catalog: catalogWith(t), Clock: clk})
	sess := newSession(t, session.Limits{})
	r := e.Run(context.Background(), sess, memfs.NewWorkspace("/ws"), "go")
	evs := drain(r)

	// reasoning.delta must be emitted, carrying the human-readable SUMMARY text
	// and no result/usage payload of its own.
	var reasoning *session.Event
	for i := range evs {
		if evs[i].Type == session.EvReasoningDelta {
			reasoning = &evs[i]
			break
		}
	}
	if reasoning == nil {
		t.Fatalf("no reasoning.delta event in %v", typesOf(evs))
	}
	if reasoning.Text != "let me think about it" {
		t.Fatalf("reasoning text = %q", reasoning.Text)
	}

	// The REPLAY blob — not the display summary — must be stored on the recorded
	// assistant message's Reasoning for verbatim replay to the provider.
	var asst *session.Message
	for i := range sess.Conversation.Messages {
		if sess.Conversation.Messages[i].Role == session.RoleAssistant {
			asst = &sess.Conversation.Messages[i]
			break
		}
	}
	if asst == nil {
		t.Fatalf("no assistant message recorded")
	}
	if asst.Reasoning != "ENCRYPTED_REPLAY_BLOB" {
		t.Fatalf("Message.Reasoning = %q, want the encrypted replay blob (not the display summary)", asst.Reasoning)
	}

	// The reasoning.delta must precede the message.delta of the same turn (it is
	// the chain-of-thought that precedes the answer).
	var ri, mi = -1, -1
	for i := range evs {
		switch evs[i].Type {
		case session.EvReasoningDelta:
			if ri < 0 {
				ri = i
			}
		case session.EvMessageDelta:
			if mi < 0 {
				mi = i
			}
		}
	}
	if ri < 0 || mi < 0 || ri > mi {
		t.Fatalf("reasoning.delta (%d) should precede message.delta (%d)", ri, mi)
	}

	// turn.end must carry this turn's usage and a positive elapsed duration.
	var turnEnd *session.Event
	for i := range evs {
		if evs[i].Type == session.EvTurnEnd {
			turnEnd = &evs[i]
			break
		}
	}
	if turnEnd == nil {
		t.Fatalf("no turn.end event in %v", typesOf(evs))
	}
	// The per-turn data lives in the typed TurnEnd payload, NOT in Event.Usage
	// (which is reserved for the cumulative-on-result semantics).
	if turnEnd.Usage != nil {
		t.Fatalf("turn.end must not set Event.Usage (reserved for result): %+v", turnEnd.Usage)
	}
	if turnEnd.TurnEnd == nil {
		t.Fatalf("turn.end missing TurnEnd payload")
	}
	if turnEnd.TurnEnd.Usage.InputTokens != 12 || turnEnd.TurnEnd.Usage.OutputTokens != 4 {
		t.Fatalf("turn.end usage = %+v, want {12,4}", turnEnd.TurnEnd.Usage)
	}
	if turnEnd.TurnEnd.DurationMs <= 0 {
		t.Fatalf("turn.end duration = %dms, want > 0 (fakeClock injected)", turnEnd.TurnEnd.DurationMs)
	}
	// turn.end precedes the terminal result.
	te, re := -1, -1
	for i := range evs {
		switch evs[i].Type {
		case session.EvTurnEnd:
			te = i
		case session.EvResult:
			re = i
		}
	}
	if te < 0 || re < 0 || te > re {
		t.Fatalf("turn.end (%d) should precede result (%d)", te, re)
	}
}

// TestTurnEndNoClock asserts turn.end is still emitted without a Clock, with a
// zero duration (the no-timing degradation).
func TestTurnEndNoClock(t *testing.T) {
	llm := mockllm.New(mockllm.TextTurn("hi"))
	e := newEngine(agent.Deps{LLM: llm, Catalog: catalogWith(t)}) // no Clock
	r := e.Run(context.Background(), newSession(t, session.Limits{}), memfs.NewWorkspace("/ws"), "go")
	evs := drain(r)

	if !containsType(evs, session.EvTurnEnd) {
		t.Fatalf("no turn.end event in %v", typesOf(evs))
	}
	for i := range evs {
		if evs[i].Type != session.EvTurnEnd {
			continue
		}
		if evs[i].TurnEnd == nil {
			t.Fatalf("turn.end missing TurnEnd payload")
		}
		if evs[i].TurnEnd.DurationMs != 0 {
			t.Fatalf("turn.end duration = %dms without a Clock, want 0", evs[i].TurnEnd.DurationMs)
		}
	}
}

// TestReadParallel asserts that two read-only calls in one turn run concurrently.
func TestReadParallel(t *testing.T) {
	var tracker overlapTracker
	bodies := make(chan struct{})
	gate := make(chan struct{})

	mk := func(name string) *fakeTool {
		return &fakeTool{name: name, readOnly: true,
			exec: func(_ context.Context, in session.ToolCall, _ tool.Workspace) (session.ToolResult, error) {
				tracker.enter()
				defer tracker.leave()
				bodies <- struct{}{} // signal "I'm running"
				<-gate               // block until both are confirmed running
				return session.NewToolResult(in.ID, name+" ok"), nil
			}}
	}
	cat := catalogWith(t, mk("Read"), mk("Grep"))

	llm := mockllm.New(
		mockllm.ToolCallTurn(
			toolCall("c1", "Read", `{"path":"a"}`),
			toolCall("c2", "Grep", `{"pattern":"x"}`),
		),
		mockllm.TextTurn("done"),
	)
	e := newEngine(agent.Deps{LLM: llm, Catalog: cat})
	r := e.Run(context.Background(), newSession(t, session.Limits{}), memfs.NewWorkspace("/ws"), "go")

	// Both bodies must enter before either is released → proves overlap.
	<-bodies
	<-bodies
	close(gate)
	drain(r)

	if tracker.max() < 2 {
		t.Fatalf("read-only tools did not overlap: max concurrency = %d", tracker.max())
	}
}

// TestMutateSerial asserts that two mutating calls never interleave.
func TestMutateSerial(t *testing.T) {
	var tracker overlapTracker
	mk := func(name string) *fakeTool {
		return &fakeTool{name: name, readOnly: false,
			exec: func(_ context.Context, in session.ToolCall, _ tool.Workspace) (session.ToolResult, error) {
				tracker.enter()
				time.Sleep(5 * time.Millisecond) // widen the window for interleaving
				tracker.leave()
				return session.NewToolResult(in.ID, name+" ok"), nil
			}}
	}
	cat := catalogWith(t, mk("Write"), mk("Edit"))
	llm := mockllm.New(
		mockllm.ToolCallTurn(
			toolCall("c1", "Write", `{"path":"a"}`),
			toolCall("c2", "Edit", `{"path":"b"}`),
		),
		mockllm.TextTurn("done"),
	)
	e := newEngine(agent.Deps{LLM: llm, Catalog: cat})
	r := e.Run(context.Background(), newSession(t, session.Limits{}), memfs.NewWorkspace("/ws"), "go")
	drain(r)

	if tracker.max() != 1 {
		t.Fatalf("mutating tools interleaved: max concurrency = %d", tracker.max())
	}
}

// TestPermissionApprove pauses on an ask, approves it, and confirms the tool ran
// and the loop continued (gauntlet #1: pause/resume).
func TestPermissionApprove(t *testing.T) {
	var executed atomic.Bool
	write := &fakeTool{name: "Write", readOnly: false,
		exec: func(_ context.Context, in session.ToolCall, _ tool.Workspace) (session.ToolResult, error) {
			executed.Store(true)
			return session.NewToolResult(in.ID, "wrote"), nil
		}}
	cat := catalogWith(t, write)
	// Default policy (no rule) → Ask.
	policy := permpolicy.NewPolicy(nil, nil)

	llm := mockllm.New(
		mockllm.ToolCallTurn(toolCall("c1", "Write", `{"path":"a"}`)),
		mockllm.TextTurn("done"),
	)
	e := newEngine(agent.Deps{LLM: llm, Catalog: cat, Policy: policy})
	r := e.Run(context.Background(), newSession(t, session.Limits{}), memfs.NewWorkspace("/ws"), "go")

	var ask *session.PendingAsk
	var collected []session.Event
	for ev := range r.Events() {
		collected = append(collected, ev)
		if ev.Type == session.EvPermissionAsk {
			ask = ev.Ask
			r.Approve(ask.AskID, session.VerdictAllowOnce)
		}
	}
	if ask == nil {
		t.Fatalf("no permission.ask emitted")
	}
	if !executed.Load() {
		t.Fatalf("approved tool was not executed")
	}
	if res := lastResult(t, collected); res.Stop != session.StopEndTurn {
		t.Fatalf("stop = %q, want end_turn", res.Stop)
	}
}

// TestPermissionDeny pauses on an ask, denies it, and confirms the model receives
// the deny reason as an error tool result and the tool did NOT run.
func TestPermissionDeny(t *testing.T) {
	var executed atomic.Bool
	write := &fakeTool{name: "Write", readOnly: false,
		exec: func(_ context.Context, in session.ToolCall, _ tool.Workspace) (session.ToolResult, error) {
			executed.Store(true)
			return session.NewToolResult(in.ID, "wrote"), nil
		}}
	cat := catalogWith(t, write)
	policy := permpolicy.NewPolicy(nil, nil) // Ask by default

	llm := mockllm.New(
		mockllm.ToolCallTurn(toolCall("c1", "Write", `{"path":"a"}`)),
		mockllm.TextTurn("understood"),
	)
	e := newEngine(agent.Deps{LLM: llm, Catalog: cat, Policy: policy})
	sess := newSession(t, session.Limits{})
	r := e.Run(context.Background(), sess, memfs.NewWorkspace("/ws"), "go")

	var denyResult *session.ToolResult
	// Track the order of the c1 events: the card (EvToolCall) must open BEFORE the
	// synthesized deny result, so the failed update lands on an already-open card.
	i, cardIdx, denyIdx := 0, -1, -1
	for ev := range r.Events() {
		switch ev.Type {
		case session.EvToolCall:
			if ev.ToolCall != nil && ev.ToolCall.ID == "c1" && cardIdx == -1 {
				cardIdx = i
			}
		case session.EvPermissionAsk:
			r.Approve(ev.Ask.AskID, session.VerdictDeny)
		case session.EvToolResult:
			denyResult = ev.ToolResult
			if ev.ToolResult != nil && ev.ToolResult.CallID == "c1" && denyIdx == -1 {
				denyIdx = i
			}
		}
		i++
	}
	if executed.Load() {
		t.Fatalf("denied tool was executed")
	}
	if denyResult == nil || !denyResult.IsError {
		t.Fatalf("expected an error tool result for the deny, got %+v", denyResult)
	}
	// Ordering: the card opens before the denial result (issue #6).
	if cardIdx == -1 {
		t.Fatalf("no EvToolCall opened for c1 (the card must open before the gate)")
	}
	if cardIdx >= denyIdx {
		t.Fatalf("event order = card@%d, deny@%d; want card < deny", cardIdx, denyIdx)
	}
	if !strings.Contains(denyResult.Content, "denied") {
		t.Fatalf("deny result does not carry a reason: %q", denyResult.Content)
	}
	// The deny reason must be in the conversation so the model can recover.
	found := false
	for _, m := range sess.Conversation.Messages {
		if m.Role == session.RoleTool && m.ToolResult != nil && m.ToolResult.IsError {
			found = true
		}
	}
	if !found {
		t.Fatalf("deny tool result not recorded in conversation")
	}
}

// TestCancelMidStream cancels the run while the model stream is in flight and
// asserts a terminal result=cancelled (gauntlet #1).
func TestCancelMidStream(t *testing.T) {
	// A turn whose stream blocks forever between chunks until ctx is cancelled.
	blocking := &blockingProvider{started: make(chan struct{})}

	e := newEngine(agent.Deps{LLM: blocking, Catalog: catalogWith(t)})
	ctx := context.Background()
	r := e.Run(ctx, newSession(t, session.Limits{}), memfs.NewWorkspace("/ws"), "go")

	// Wait until the provider is streaming, then cancel.
	<-blocking.started
	r.Cancel()

	evs := drain(r)
	res := lastResult(t, evs)
	if res.Stop != session.StopCancelled {
		t.Fatalf("stop = %q, want cancelled", res.Stop)
	}
}

// blockingProvider streams one text chunk, signals started, then blocks until the
// context is cancelled.
type blockingProvider struct {
	started chan struct{}
	once    sync.Once
}

func (*blockingProvider) Capabilities() port.ProviderCapabilities { return port.ProviderCapabilities{} }

func (b *blockingProvider) Stream(ctx context.Context, _ port.LLMRequest) (iter.Seq2[port.Chunk, error], error) {
	return func(yield func(port.Chunk, error) bool) {
		b.once.Do(func() { close(b.started) })
		if !yield(port.Chunk{Kind: port.ChunkText, Text: "thinking"}, nil) {
			return
		}
		<-ctx.Done()
		yield(port.Chunk{}, ctx.Err())
	}, nil
}

func TestStopMaxTurns(t *testing.T) {
	// The model always asks for a tool, so the loop would run forever without a
	// turn cap.
	read := &fakeTool{name: "Read", readOnly: true,
		exec: func(_ context.Context, in session.ToolCall, _ tool.Workspace) (session.ToolResult, error) {
			return session.NewToolResult(in.ID, "ok"), nil
		}}
	turns := make([]mockllm.Turn, 10)
	for i := range turns {
		turns[i] = mockllm.ToolCallTurn(toolCall(fmt.Sprintf("c%d", i), "Read", `{"path":"a"}`))
	}
	llm := mockllm.New(turns...)
	e := newEngine(agent.Deps{LLM: llm, Catalog: catalogWith(t, read)})
	sess := newSession(t, session.Limits{MaxTurns: 3})
	r := e.Run(context.Background(), sess, memfs.NewWorkspace("/ws"), "go")
	res := lastResult(t, drain(r))
	if res.Stop != session.StopMaxTurns {
		t.Fatalf("stop = %q, want max_turns", res.Stop)
	}
}

func TestStopMaxToolCalls(t *testing.T) {
	read := &fakeTool{name: "Read", readOnly: true,
		exec: func(_ context.Context, in session.ToolCall, _ tool.Workspace) (session.ToolResult, error) {
			return session.NewToolResult(in.ID, "ok"), nil
		}}
	turns := make([]mockllm.Turn, 10)
	for i := range turns {
		turns[i] = mockllm.ToolCallTurn(toolCall(fmt.Sprintf("c%d", i), "Read", `{"path":"a"}`))
	}
	llm := mockllm.New(turns...)
	e := newEngine(agent.Deps{LLM: llm, Catalog: catalogWith(t, read)})
	sess := newSession(t, session.Limits{MaxToolCalls: 2})
	r := e.Run(context.Background(), sess, memfs.NewWorkspace("/ws"), "go")
	res := lastResult(t, drain(r))
	if res.Stop != session.StopMaxToolCalls {
		t.Fatalf("stop = %q, want max_tool_calls", res.Stop)
	}
}

func TestStopMaxConsecutiveFailures(t *testing.T) {
	failing := &fakeTool{name: "Read", readOnly: true,
		exec: func(_ context.Context, in session.ToolCall, _ tool.Workspace) (session.ToolResult, error) {
			return session.NewToolError(in.ID, "boom"), nil
		}}
	turns := make([]mockllm.Turn, 10)
	for i := range turns {
		turns[i] = mockllm.ToolCallTurn(toolCall(fmt.Sprintf("c%d", i), "Read", `{"path":"a"}`))
	}
	llm := mockllm.New(turns...)
	e := newEngine(agent.Deps{LLM: llm, Catalog: catalogWith(t, failing)})
	sess := newSession(t, session.Limits{MaxConsecutiveFailures: 2})
	r := e.Run(context.Background(), sess, memfs.NewWorkspace("/ws"), "go")
	res := lastResult(t, drain(r))
	if res.Stop != session.StopMaxConsecutiveFailures {
		t.Fatalf("stop = %q, want max_consecutive_failures", res.Stop)
	}
}

// TestPreToolUseHookBlocks runs an exit-2 PreToolUse hook and confirms the tool
// is not executed and the model receives the block message (gauntlet #5).
func TestPreToolUseHookBlocks(t *testing.T) {
	var executed atomic.Bool
	write := &fakeTool{name: "Write", readOnly: false,
		exec: func(_ context.Context, in session.ToolCall, _ tool.Workspace) (session.ToolResult, error) {
			executed.Store(true)
			return session.NewToolResult(in.ID, "wrote"), nil
		}}
	hooks := hookexec.New(map[governance.HookPhase]string{
		governance.PhasePreToolUse: "echo blocked-by-policy >&2; exit 2",
	})
	llm := mockllm.New(
		mockllm.ToolCallTurn(toolCall("c1", "Write", `{"path":"a"}`)),
		mockllm.TextTurn("ok"),
	)
	e := newEngine(agent.Deps{LLM: llm, Catalog: catalogWith(t, write), Hooks: hooks})
	r := e.Run(context.Background(), newSession(t, session.Limits{}), memfs.NewWorkspace("/ws"), "go")

	evs := drain(r)

	var hookEv, blockRes bool
	var hookCallID session.ToolCallID
	// Indices of the three c1 events, to assert ordering: the card must open BEFORE
	// the veto (issue #6 — otherwise the failed update keys an unopened card).
	cardIdx, hookIdx, resultIdx := -1, -1, -1
	for i, ev := range evs {
		switch {
		case ev.Type == session.EvToolCall && ev.ToolCall != nil && ev.ToolCall.ID == "c1":
			if cardIdx == -1 {
				cardIdx = i
			}
		case ev.Type == session.EvHook && strings.Contains(ev.Text, "blocked-by-policy"):
			hookEv = true
			hookIdx = i
			if ev.Hook != nil {
				hookCallID = ev.Hook.CallID
			}
		case ev.Type == session.EvToolResult && ev.ToolResult.IsError &&
			strings.Contains(ev.ToolResult.Content, "blocked-by-policy"):
			blockRes = true
			resultIdx = i
		}
	}
	if executed.Load() {
		t.Fatalf("hook-blocked tool was executed")
	}
	if !hookEv {
		t.Fatalf("no hook event emitted")
	}
	// The blocked EvHook must carry the originating tool-call id so a client can
	// address the veto to the exact tool card (issue #6).
	if hookCallID != "c1" {
		t.Fatalf("blocked hook CallID = %q, want c1", hookCallID)
	}
	if !blockRes {
		t.Fatalf("block message not fed to the model as a tool result")
	}
	// Ordering: the card (EvToolCall) opens first, THEN the veto (EvHook), THEN the
	// synthesized error result — so both failure events land on an already-open card.
	if cardIdx == -1 {
		t.Fatalf("no EvToolCall opened for c1 (the card must open before the gate); events=%v", typesOf(evs))
	}
	if cardIdx >= hookIdx || hookIdx >= resultIdx {
		t.Fatalf("event order = card@%d, hook@%d, result@%d; want card < hook < result; events=%v",
			cardIdx, hookIdx, resultIdx, typesOf(evs))
	}
}

// argRecorder is a Tool that captures the Args of the last call it executed, so
// a test can assert which arguments actually reached the tool.
type argRecorder struct {
	name     string
	readOnly bool
	mu       sync.Mutex
	gotArgs  string
}

func (a *argRecorder) Spec() tool.ToolSpec {
	return tool.ToolSpec{Name: a.name, Description: a.name, Schema: json.RawMessage(`{"type":"object"}`)}
}
func (a *argRecorder) ReadOnly() bool { return a.readOnly }
func (a *argRecorder) Execute(_ context.Context, in session.ToolCall, _ tool.Workspace) (session.ToolResult, error) {
	a.mu.Lock()
	a.gotArgs = string(in.Args)
	a.mu.Unlock()
	return session.NewToolResult(in.ID, "ok"), nil
}
func (a *argRecorder) args() string {
	a.mu.Lock()
	defer a.mu.Unlock()
	return a.gotArgs
}

// TestPreToolUseHookMutatesArgs asserts a PreToolUse hook returning a non-empty
// Mutated payload rewrites the tool call's args before execution: the tool runs
// with the MUTATED args, and the CallID/Name are preserved.
func TestPreToolUseHookMutatesArgs(t *testing.T) {
	rec := &argRecorder{name: "Write", readOnly: false}
	hooks := &mutatingHooks{mutate: map[governance.HookPhase]json.RawMessage{
		governance.PhasePreToolUse: json.RawMessage(`{"path":"mutated.txt"}`),
	}}
	llm := mockllm.New(
		mockllm.ToolCallTurn(toolCall("c1", "Write", `{"path":"original.txt"}`)),
		mockllm.TextTurn("done"),
	)
	e := newEngine(agent.Deps{LLM: llm, Catalog: catalogWith(t, rec), Hooks: hooks})
	r := e.Run(context.Background(), newSession(t, session.Limits{}), memfs.NewWorkspace("/ws"), "go")

	var rewriteEv bool
	for ev := range r.Events() {
		if ev.Type == session.EvHook && strings.Contains(ev.Text, "rewrote tool arguments") {
			rewriteEv = true
		}
	}
	if got := rec.args(); got != `{"path":"mutated.txt"}` {
		t.Fatalf("tool executed with args %q, want the mutated args", got)
	}
	if !rewriteEv {
		t.Fatalf("expected a hook event noting the arg rewrite")
	}
}

// TestPreToolUseHookMutationMalformedIgnored asserts a malformed (non-JSON)
// Mutated payload is ignored: the tool runs with the ORIGINAL args and a notice
// event is emitted.
func TestPreToolUseHookMutationMalformedIgnored(t *testing.T) {
	rec := &argRecorder{name: "Write", readOnly: false}
	hooks := &mutatingHooks{mutate: map[governance.HookPhase]json.RawMessage{
		governance.PhasePreToolUse: json.RawMessage(`not-json`),
	}}
	llm := mockllm.New(
		mockllm.ToolCallTurn(toolCall("c1", "Write", `{"path":"original.txt"}`)),
		mockllm.TextTurn("done"),
	)
	e := newEngine(agent.Deps{LLM: llm, Catalog: catalogWith(t, rec), Hooks: hooks})
	r := e.Run(context.Background(), newSession(t, session.Limits{}), memfs.NewWorkspace("/ws"), "go")

	var ignoredEv bool
	for ev := range r.Events() {
		if ev.Type == session.EvHook && strings.Contains(ev.Text, "malformed argument mutation") {
			ignoredEv = true
		}
	}
	if got := rec.args(); got != `{"path":"original.txt"}` {
		t.Fatalf("tool executed with args %q, want the ORIGINAL args (malformed mutation ignored)", got)
	}
	if !ignoredEv {
		t.Fatalf("expected a hook event noting the malformed mutation was ignored")
	}
}

// TestPreToolUseHookNoMutationKeepsArgs asserts that with no Mutated payload the
// tool runs with its original args (regression for the allow path).
func TestPreToolUseHookNoMutationKeepsArgs(t *testing.T) {
	rec := &argRecorder{name: "Write", readOnly: false}
	hooks := newRecordingHooks(nil) // allows everything, no mutation
	llm := mockllm.New(
		mockllm.ToolCallTurn(toolCall("c1", "Write", `{"path":"original.txt"}`)),
		mockllm.TextTurn("done"),
	)
	e := newEngine(agent.Deps{LLM: llm, Catalog: catalogWith(t, rec), Hooks: hooks})
	r := e.Run(context.Background(), newSession(t, session.Limits{}), memfs.NewWorkspace("/ws"), "go")
	drain(r)

	if got := rec.args(); got != `{"path":"original.txt"}` {
		t.Fatalf("tool executed with args %q, want the original args", got)
	}
}

// postMutResult finds the tool.result event and the recorded RoleTool message
// for callID, returning their ToolResults so a test can assert both the client
// (event) view and the model (recorded) view agree.
func toolResultEvent(evs []session.Event) *session.ToolResult {
	for _, ev := range evs {
		if ev.Type == session.EvToolResult && ev.ToolResult != nil {
			return ev.ToolResult
		}
	}
	return nil
}

func recordedToolResult(sess *session.Session) *session.ToolResult {
	for _, m := range sess.Conversation.Messages {
		if m.Role == session.RoleTool && m.ToolResult != nil {
			return m.ToolResult
		}
	}
	return nil
}

// TestPostToolUseHookMutatesResult asserts a PostToolUse hook returning a Mutated
// {"content","is_error"} payload rewrites the result: BOTH the emitted tool.result
// event AND the result recorded for the model carry the rewritten content, and the
// is_error flag is honoured (here flipping a success into an error).
func TestPostToolUseHookMutatesResult(t *testing.T) {
	tl := &fakeTool{name: "Read", readOnly: true,
		exec: func(_ context.Context, in session.ToolCall, _ tool.Workspace) (session.ToolResult, error) {
			return session.NewToolResult(in.ID, "SECRET=abc123"), nil
		}}
	hooks := &mutatingHooks{mutate: map[governance.HookPhase]json.RawMessage{
		governance.PhasePostToolUse: json.RawMessage(`{"content":"[redacted]","is_error":true}`),
	}}
	llm := mockllm.New(
		mockllm.ToolCallTurn(toolCall("c1", "Read", `{"path":"a"}`)),
		mockllm.TextTurn("done"),
	)
	sess := newSession(t, session.Limits{})
	logger := &recordingLogger{}
	e := newEngine(agent.Deps{LLM: llm, Catalog: catalogWith(t, tl), Hooks: hooks, Logger: logger})
	r := e.Run(context.Background(), sess, memfs.NewWorkspace("/ws"), "go")
	evs := drain(r)

	evRes := toolResultEvent(evs)
	if evRes == nil || evRes.Content != "[redacted]" || !evRes.IsError {
		t.Fatalf("emitted tool.result = %+v, want content '[redacted]' is_error true", evRes)
	}
	recRes := recordedToolResult(sess)
	if recRes == nil || recRes.Content != "[redacted]" || !recRes.IsError {
		t.Fatalf("recorded result (model view) = %+v, want content '[redacted]' is_error true", recRes)
	}
	// The audit Logger must see the EFFECTIVE (redacted) result too: a redacting
	// PostToolUse hook must not leak the raw tool output ("SECRET=...") into the
	// audit log / telemetry. This is the whole point of logging after the hook.
	logged, ok := logger.lastResult()
	if !ok || logged.Content != "[redacted]" || !logged.IsError {
		t.Fatalf("logged result (audit view) = %+v ok=%v, want content '[redacted]' is_error true", logged, ok)
	}
	var rewriteEv bool
	for _, ev := range evs {
		if ev.Type == session.EvHook && strings.Contains(ev.Text, "rewrote the tool result") {
			rewriteEv = true
		}
	}
	if !rewriteEv {
		t.Fatalf("expected a hook event noting the result rewrite")
	}
}

// TestPostToolUseHookBlockLeavesResultUnchanged asserts a PostToolUse block still
// only annotates: the result the model sees is unchanged (block does not mutate).
func TestPostToolUseHookBlockLeavesResultUnchanged(t *testing.T) {
	tl := &fakeTool{name: "Read", readOnly: true,
		exec: func(_ context.Context, in session.ToolCall, _ tool.Workspace) (session.ToolResult, error) {
			return session.NewToolResult(in.ID, "original output"), nil
		}}
	hooks := newRecordingHooks(map[governance.HookPhase]string{
		governance.PhasePostToolUse: "post annotation",
	})
	llm := mockllm.New(
		mockllm.ToolCallTurn(toolCall("c1", "Read", `{"path":"a"}`)),
		mockllm.TextTurn("done"),
	)
	sess := newSession(t, session.Limits{})
	e := newEngine(agent.Deps{LLM: llm, Catalog: catalogWith(t, tl), Hooks: hooks})
	evs := drain(e.Run(context.Background(), sess, memfs.NewWorkspace("/ws"), "go"))

	recRes := recordedToolResult(sess)
	if recRes == nil || recRes.Content != "original output" || recRes.IsError {
		t.Fatalf("recorded result = %+v, want unchanged 'original output'", recRes)
	}
	var annotated bool
	for _, ev := range evs {
		if ev.Type == session.EvHook && strings.Contains(ev.Text, "post annotation") {
			annotated = true
		}
	}
	if !annotated {
		t.Fatalf("expected the PostToolUse block annotation hook event")
	}
}

// TestPostToolUseHookMutationMalformedIgnored asserts a malformed (non-JSON)
// Mutated payload is ignored: the original result stands + a notice is emitted.
func TestPostToolUseHookMutationMalformedIgnored(t *testing.T) {
	tl := &fakeTool{name: "Read", readOnly: true,
		exec: func(_ context.Context, in session.ToolCall, _ tool.Workspace) (session.ToolResult, error) {
			return session.NewToolResult(in.ID, "original output"), nil
		}}
	hooks := &mutatingHooks{mutate: map[governance.HookPhase]json.RawMessage{
		governance.PhasePostToolUse: json.RawMessage(`not-json`),
	}}
	llm := mockllm.New(
		mockllm.ToolCallTurn(toolCall("c1", "Read", `{"path":"a"}`)),
		mockllm.TextTurn("done"),
	)
	sess := newSession(t, session.Limits{})
	e := newEngine(agent.Deps{LLM: llm, Catalog: catalogWith(t, tl), Hooks: hooks})
	evs := drain(e.Run(context.Background(), sess, memfs.NewWorkspace("/ws"), "go"))

	recRes := recordedToolResult(sess)
	if recRes == nil || recRes.Content != "original output" {
		t.Fatalf("recorded result = %+v, want unchanged (malformed mutation ignored)", recRes)
	}
	var ignoredEv bool
	for _, ev := range evs {
		if ev.Type == session.EvHook && strings.Contains(ev.Text, "malformed result mutation") {
			ignoredEv = true
		}
	}
	if !ignoredEv {
		t.Fatalf("expected a hook event noting the malformed result mutation was ignored")
	}
}

// TestPostToolUseHookNoMutationKeepsResult asserts that with no Mutated payload
// the result is unchanged (allow-path regression).
func TestPostToolUseHookNoMutationKeepsResult(t *testing.T) {
	tl := &fakeTool{name: "Read", readOnly: true,
		exec: func(_ context.Context, in session.ToolCall, _ tool.Workspace) (session.ToolResult, error) {
			return session.NewToolResult(in.ID, "original output"), nil
		}}
	hooks := newRecordingHooks(nil) // allow, no mutation
	llm := mockllm.New(
		mockllm.ToolCallTurn(toolCall("c1", "Read", `{"path":"a"}`)),
		mockllm.TextTurn("done"),
	)
	sess := newSession(t, session.Limits{})
	e := newEngine(agent.Deps{LLM: llm, Catalog: catalogWith(t, tl), Hooks: hooks})
	drain(e.Run(context.Background(), sess, memfs.NewWorkspace("/ws"), "go"))

	recRes := recordedToolResult(sess)
	if recRes == nil || recRes.Content != "original output" || recRes.IsError {
		t.Fatalf("recorded result = %+v, want unchanged 'original output'", recRes)
	}
}

// TestUnknownToolError confirms an unknown tool yields an error result, not a
// crash, and the loop keeps going.
func TestUnknownToolError(t *testing.T) {
	llm := mockllm.New(
		mockllm.ToolCallTurn(toolCall("c1", "Nope", `{}`)),
		mockllm.TextTurn("recovered"),
	)
	e := newEngine(agent.Deps{LLM: llm, Catalog: catalogWith(t)})
	r := e.Run(context.Background(), newSession(t, session.Limits{}), memfs.NewWorkspace("/ws"), "go")
	evs := drain(r)
	res := lastResult(t, evs)
	if res.Stop != session.StopEndTurn {
		t.Fatalf("stop = %q", res.Stop)
	}
}

// TestSessionInitEmittedOncePerRunBeforeFirstTurn asserts the loop emits exactly
// one session.init event per run, and that it precedes every turn.start. This is
// the run-open signal telemetry adapters switch on; the loop must emit it (not
// rely on telemetry's defensive fallback).
func TestSessionInitEmittedOncePerRunBeforeFirstTurn(t *testing.T) {
	llm := mockllm.New(
		mockllm.ToolCallTurn(toolCall("c1", "Read", `{"path":"a.go"}`)),
		mockllm.TextTurn("done"),
	)
	read := &fakeTool{name: "Read", readOnly: true,
		exec: func(_ context.Context, in session.ToolCall, _ tool.Workspace) (session.ToolResult, error) {
			return session.NewToolResult(in.ID, "ok"), nil
		}}
	e := newEngine(agent.Deps{LLM: llm, Catalog: catalogWith(t, read)})
	r := e.Run(context.Background(), newSession(t, session.Limits{}), memfs.NewWorkspace("/ws"), "go")
	evs := drain(r)

	// Exactly one session.init.
	var initCount, firstInitIdx, firstTurnIdx = 0, -1, -1
	for i, ev := range evs {
		switch ev.Type {
		case session.EvSessionInit:
			if firstInitIdx < 0 {
				firstInitIdx = i
			}
			initCount++
		case session.EvTurnStart:
			if firstTurnIdx < 0 {
				firstTurnIdx = i
			}
		}
	}
	if initCount != 1 {
		t.Fatalf("session.init count = %d, want exactly 1 (events: %v)", initCount, typesOf(evs))
	}
	if firstTurnIdx < 0 {
		t.Fatalf("no turn.start emitted (events: %v)", typesOf(evs))
	}
	if firstInitIdx > firstTurnIdx {
		t.Fatalf("session.init (idx %d) must precede first turn.start (idx %d): %v",
			firstInitIdx, firstTurnIdx, typesOf(evs))
	}
	// And it must be the very first event of the run.
	if evs[0].Type != session.EvSessionInit {
		t.Fatalf("first event = %q, want session.init: %v", evs[0].Type, typesOf(evs))
	}
}
