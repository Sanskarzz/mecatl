package agent_test

import (
	"context"
	"encoding/json"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/stacklok/ozzharness/internal/adapter/memfs"
	"github.com/stacklok/ozzharness/internal/adapter/mockllm"
	"github.com/stacklok/ozzharness/internal/adapter/permpolicy"
	"github.com/stacklok/ozzharness/internal/agent"
	"github.com/stacklok/ozzharness/internal/governance"
	"github.com/stacklok/ozzharness/internal/port"
	"github.com/stacklok/ozzharness/internal/session"
	"github.com/stacklok/ozzharness/internal/tool"
)

// recordingHook is a port.HookRunner test double that records the phases it was
// invoked with. It never blocks and never errors.
type recordingHook struct {
	mu     sync.Mutex
	phases []governance.HookPhase
}

func (h *recordingHook) Run(_ context.Context, ev governance.HookEvent) (governance.HookOutcome, error) {
	h.mu.Lock()
	h.phases = append(h.phases, ev.Phase)
	h.mu.Unlock()
	return governance.HookOutcome{}, nil
}

func (h *recordingHook) saw(p governance.HookPhase) bool {
	h.mu.Lock()
	defer h.mu.Unlock()
	for _, got := range h.phases {
		if got == p {
			return true
		}
	}
	return false
}

// childEngineWith builds a child Engine over a scoped catalog and an allow-all
// policy (the recommended non-interactive subagent wiring), driven by the given
// scripted child LLM.
func childEngineWith(llm port.LLMProvider, cat *tool.Catalog) *agent.Engine {
	return agent.NewEngine(agent.Deps{
		LLM:     llm,
		Catalog: cat,
		Policy:  permpolicy.NewPolicy([]governance.Rule{{Effect: governance.Allow}}),
		Model:   "child-model",
	})
}

// TestSubagentReturnsOnlyFinalString proves gauntlet #7: the parent receives
// exactly ONE ToolResult — the child's final summary — and NEVER observes the
// child's intermediate tool.call / tool.result / message.delta events.
func TestSubagentReturnsOnlyFinalString(t *testing.T) {
	// Child catalog: a read-only Read explorer tool (no Task, no mutating tools).
	childRead := &fakeTool{name: "Read", readOnly: true,
		exec: func(_ context.Context, in session.ToolCall, _ tool.Workspace) (session.ToolResult, error) {
			return session.NewToolResult(in.ID, "child read the file: package main"), nil
		}}
	childCat := catalogWith(t, childRead)

	// Child script: it reads a file, then summarizes.
	childLLM := mockllm.New(
		mockllm.ChunksTurn(
			mockllm.TextChunk("let me read it"),
			mockllm.ToolCallChunk(toolCall("k1", "Read", `{"path":"main.go"}`)),
			mockllm.UsageChunk(session.Usage{InputTokens: 7, OutputTokens: 2}),
			mockllm.DoneChunk(session.StopEndTurn),
		),
		mockllm.TextTurn("summary: main.go is package main"),
	)
	childEngine := childEngineWith(childLLM, childCat)

	// Parent catalog contains only the Task tool.
	task := agent.NewTaskTool(childEngine)
	parentCat := catalogWith(t, task)

	parentLLM := mockllm.New(
		mockllm.ToolCallTurn(toolCall("p1", "Task", `{"prompt":"investigate main.go"}`)),
		mockllm.TextTurn("parent received the summary"),
	)
	e := newEngine(agent.Deps{LLM: parentLLM, Catalog: parentCat})
	r := e.Run(context.Background(), newSession(t, session.Limits{}), memfs.NewWorkspace("/ws"), "go")
	evs := drain(r)

	// Collect every ToolResult the PARENT observed.
	var parentResults []*session.ToolResult
	for _, ev := range evs {
		if ev.Type == session.EvToolResult {
			parentResults = append(parentResults, ev.ToolResult)
		}
	}
	if len(parentResults) != 1 {
		t.Fatalf("parent saw %d tool results, want exactly 1: %v", len(parentResults), typesOf(evs))
	}
	got := parentResults[0]
	if got.IsError {
		t.Fatalf("parent tool result is an error: %q", got.Content)
	}
	if got.Content != "summary: main.go is package main" {
		t.Fatalf("parent tool result = %q, want the child's final summary", got.Content)
	}

	// The parent must NEVER see the child's intermediate signals. The child read
	// "main.go" and its tool result content was "child read the file ...".
	for _, ev := range evs {
		if ev.ToolCall != nil && ev.ToolCall.ID == "k1" {
			t.Fatalf("parent observed the child's intermediate tool.call (id k1)")
		}
		if ev.ToolResult != nil && strings.Contains(ev.ToolResult.Content, "child read the file") {
			t.Fatalf("parent observed the child's intermediate tool.result")
		}
		if ev.Type == session.EvMessageDelta && strings.Contains(ev.Text, "let me read it") {
			t.Fatalf("parent observed the child's intermediate message.delta")
		}
	}

	// Sanity: the parent's own final text is present and distinct from the child.
	res := lastResult(t, evs)
	if res.Text != "parent received the summary" {
		t.Fatalf("parent final text = %q", res.Text)
	}
}

// TestSubagentChildScopeExcludesTaskAndMutators asserts the child catalog the
// composition root wires excludes Task (no recursion) and mutating tools. The
// child here tries to call Task and Write; both must come back as unknown-tool
// errors inside the child, and the parent still gets a single clean summary.
func TestSubagentChildScopeExcludesTaskAndMutators(t *testing.T) {
	childRead := &fakeTool{name: "Read", readOnly: true,
		exec: func(_ context.Context, in session.ToolCall, _ tool.Workspace) (session.ToolResult, error) {
			return session.NewToolResult(in.ID, "ok"), nil
		}}
	// Scoped child catalog: read-only explorer only. No Task, no Write.
	childCat := catalogWith(t, childRead)

	if _, ok := childCat.Lookup("Task"); ok {
		t.Fatalf("child catalog must not contain Task (recursion risk)")
	}
	if _, ok := childCat.Lookup("Write"); ok {
		t.Fatalf("child catalog must not contain mutating tools")
	}

	// The child tries forbidden tools, then summarizes.
	childLLM := mockllm.New(
		mockllm.ToolCallTurn(
			toolCall("k1", "Task", `{"prompt":"recurse"}`),
			toolCall("k2", "Write", `{"path":"x"}`),
		),
		mockllm.TextTurn("done despite blocks"),
	)
	childEngine := childEngineWith(childLLM, childCat)

	task := agent.NewTaskTool(childEngine)
	parentLLM := mockllm.New(
		mockllm.ToolCallTurn(toolCall("p1", "Task", `{"prompt":"go"}`)),
		mockllm.TextTurn("ok"),
	)
	e := newEngine(agent.Deps{LLM: parentLLM, Catalog: catalogWith(t, task)})
	r := e.Run(context.Background(), newSession(t, session.Limits{}), memfs.NewWorkspace("/ws"), "go")
	evs := drain(r)

	var results []*session.ToolResult
	for _, ev := range evs {
		if ev.Type == session.EvToolResult {
			results = append(results, ev.ToolResult)
		}
	}
	if len(results) != 1 {
		t.Fatalf("parent saw %d tool results, want 1", len(results))
	}
	if results[0].Content != "done despite blocks" {
		t.Fatalf("parent result = %q", results[0].Content)
	}
}

// TestSubagentParentCancelPropagates asserts that cancelling the parent ctx
// cancels the child run: the child loop ends (cancelled) and Execute returns
// without hanging.
func TestSubagentParentCancelPropagates(t *testing.T) {
	// A child LLM whose stream blocks until ctx is cancelled.
	blocking := &blockingProvider{started: make(chan struct{})}
	childEngine := childEngineWith(blocking, catalogWith(t))

	task := agent.NewTaskTool(childEngine)

	// The parent calls Task; we instrument the child-start by reading the
	// blockingProvider's started channel from the parent goroutine.
	parentLLM := mockllm.New(
		mockllm.ToolCallTurn(toolCall("p1", "Task", `{"prompt":"go"}`)),
		mockllm.TextTurn("recovered"),
	)
	e := newEngine(agent.Deps{LLM: parentLLM, Catalog: catalogWith(t, task)})
	ctx, cancel := context.WithCancel(context.Background())
	r := e.Run(ctx, newSession(t, session.Limits{}), memfs.NewWorkspace("/ws"), "go")

	// Wait for the child's stream to start, then cancel the parent.
	<-blocking.started
	cancel()

	evs := drain(r)
	// The parent run itself ends cancelled (its ctx was cancelled). The key
	// assertion is that draining terminates at all — a child that ignored ctx
	// would hang Execute forever and this test would time out.
	res := lastResult(t, evs)
	if res.Stop != session.StopCancelled {
		t.Fatalf("parent stop = %q, want cancelled", res.Stop)
	}
}

// TestSubagentStopHookFires asserts the SubagentStop hook fires when the child
// finishes.
func TestSubagentStopHookFires(t *testing.T) {
	childRead := &fakeTool{name: "Read", readOnly: true,
		exec: func(_ context.Context, in session.ToolCall, _ tool.Workspace) (session.ToolResult, error) {
			return session.NewToolResult(in.ID, "ok"), nil
		}}
	childLLM := mockllm.New(mockllm.TextTurn("child summary"))
	childEngine := childEngineWith(childLLM, catalogWith(t, childRead))

	hook := &recordingHook{}
	task := agent.NewTaskTool(childEngine, agent.WithSubagentStopHook(hook))

	parentLLM := mockllm.New(
		mockllm.ToolCallTurn(toolCall("p1", "Task", `{"prompt":"go"}`)),
		mockllm.TextTurn("ok"),
	)
	e := newEngine(agent.Deps{LLM: parentLLM, Catalog: catalogWith(t, task)})
	r := e.Run(context.Background(), newSession(t, session.Limits{}), memfs.NewWorkspace("/ws"), "go")
	drain(r)

	if !hook.saw(governance.PhaseSubagentStop) {
		t.Fatalf("SubagentStop hook did not fire; saw %v", hook.phases)
	}
}

// TestSubagentAutoDeniesAsk proves the non-interactive invariant: even if the
// child's policy returns Ask, Execute auto-denies it so the child cannot block on
// a human, and the parent still gets a clean (single) result.
func TestSubagentAutoDeniesAsk(t *testing.T) {
	var executed atomic.Bool
	// A read-only child tool that, were it allowed, would set executed.
	childTool := &fakeTool{name: "Read", readOnly: true,
		exec: func(_ context.Context, in session.ToolCall, _ tool.Workspace) (session.ToolResult, error) {
			executed.Store(true)
			return session.NewToolResult(in.ID, "should not run"), nil
		}}
	// Policy with no rules → Ask by default.
	childEngine := agent.NewEngine(agent.Deps{
		LLM: mockllm.New(
			mockllm.ToolCallTurn(toolCall("k1", "Read", `{"path":"a"}`)),
			mockllm.TextTurn("child finished after denial"),
		),
		Catalog: catalogWith(t, childTool),
		Policy:  permpolicy.NewPolicy(nil),
		Model:   "child-model",
	})

	task := agent.NewTaskTool(childEngine)
	parentLLM := mockllm.New(
		mockllm.ToolCallTurn(toolCall("p1", "Task", `{"prompt":"go"}`)),
		mockllm.TextTurn("ok"),
	)
	e := newEngine(agent.Deps{LLM: parentLLM, Catalog: catalogWith(t, task)})

	done := make(chan []session.Event, 1)
	go func() {
		r := e.Run(context.Background(), newSession(t, session.Limits{}), memfs.NewWorkspace("/ws"), "go")
		done <- drain(r)
	}()

	var evs []session.Event
	select {
	case evs = <-done:
	case <-time.After(5 * time.Second):
		t.Fatalf("Task did not complete — child likely blocked on a permission ask")
	}

	if executed.Load() {
		t.Fatalf("the denied child tool was executed")
	}
	// Parent must NEVER see a permission.ask: the child's ask is resolved inside
	// the Task tool, never surfaced.
	for _, ev := range evs {
		if ev.Type == session.EvPermissionAsk {
			t.Fatalf("child permission.ask leaked to the parent stream")
		}
	}
	var results []*session.ToolResult
	for _, ev := range evs {
		if ev.Type == session.EvToolResult {
			results = append(results, ev.ToolResult)
		}
	}
	if len(results) != 1 || results[0].Content != "child finished after denial" {
		t.Fatalf("parent results = %+v, want single child summary", results)
	}
}

// TestSubagentRejectsEmptyPrompt asserts a missing/empty prompt yields an error
// ToolResult without spinning up a child.
func TestSubagentRejectsEmptyPrompt(t *testing.T) {
	childEngine := childEngineWith(mockllm.New(), tool.NewCatalog())
	task := agent.NewTaskTool(childEngine)

	res, err := task.Execute(context.Background(),
		session.NewToolCall("c1", "Task", json.RawMessage(`{"prompt":"   "}`)),
		memfs.NewWorkspace("/ws"))
	if err != nil {
		t.Fatalf("unexpected harness error: %v", err)
	}
	if !res.IsError || !strings.Contains(res.Content, "prompt") {
		t.Fatalf("expected an error result about the missing prompt, got %+v", res)
	}
}

// TestNewTaskToolNilEnginePanics asserts the composition-root contract: a nil
// child Engine is a programming error.
func TestNewTaskToolNilEnginePanics(t *testing.T) {
	defer func() {
		if recover() == nil {
			t.Fatalf("NewTaskTool(nil) did not panic")
		}
	}()
	_ = agent.NewTaskTool(nil)
}
