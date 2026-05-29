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

	"github.com/stacklok/ozzharness/internal/adapter/hookexec"
	"github.com/stacklok/ozzharness/internal/adapter/memfs"
	"github.com/stacklok/ozzharness/internal/adapter/mockllm"
	"github.com/stacklok/ozzharness/internal/adapter/permpolicy"
	"github.com/stacklok/ozzharness/internal/agent"
	"github.com/stacklok/ozzharness/internal/governance"
	"github.com/stacklok/ozzharness/internal/port"
	"github.com/stacklok/ozzharness/internal/session"
	"github.com/stacklok/ozzharness/internal/tool"
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
	mu    sync.Mutex
	calls int
}

func (l *recordingLogger) ToolCall(_ session.SessionID, _ session.ToolCall, _ session.ToolResult, _ time.Duration) {
	l.mu.Lock()
	l.calls++
	l.mu.Unlock()
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
	return permpolicy.NewPolicy([]governance.Rule{{Effect: governance.Allow}})
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
	policy := permpolicy.NewPolicy(nil)

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
			r.Approve(ask.AskID, true)
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
	policy := permpolicy.NewPolicy(nil) // Ask by default

	llm := mockllm.New(
		mockllm.ToolCallTurn(toolCall("c1", "Write", `{"path":"a"}`)),
		mockllm.TextTurn("understood"),
	)
	e := newEngine(agent.Deps{LLM: llm, Catalog: cat, Policy: policy})
	sess := newSession(t, session.Limits{})
	r := e.Run(context.Background(), sess, memfs.NewWorkspace("/ws"), "go")

	var denyResult *session.ToolResult
	for ev := range r.Events() {
		switch ev.Type {
		case session.EvPermissionAsk:
			r.Approve(ev.Ask.AskID, false)
		case session.EvToolResult:
			denyResult = ev.ToolResult
		}
	}
	if executed.Load() {
		t.Fatalf("denied tool was executed")
	}
	if denyResult == nil || !denyResult.IsError {
		t.Fatalf("expected an error tool result for the deny, got %+v", denyResult)
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

	var hookEv, blockRes bool
	for ev := range r.Events() {
		if ev.Type == session.EvHook && strings.Contains(ev.Text, "blocked-by-policy") {
			hookEv = true
		}
		if ev.Type == session.EvToolResult && ev.ToolResult.IsError &&
			strings.Contains(ev.ToolResult.Content, "blocked-by-policy") {
			blockRes = true
		}
	}
	if executed.Load() {
		t.Fatalf("hook-blocked tool was executed")
	}
	if !hookEv {
		t.Fatalf("no hook event emitted")
	}
	if !blockRes {
		t.Fatalf("block message not fed to the model as a tool result")
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
