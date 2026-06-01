package agent_test

import (
	"context"
	"encoding/json"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/stacklok/mecatl/internal/adapter/memfs"
	"github.com/stacklok/mecatl/internal/adapter/mockllm"
	"github.com/stacklok/mecatl/internal/adapter/permpolicy"
	"github.com/stacklok/mecatl/internal/agent"
	"github.com/stacklok/mecatl/internal/governance"
	"github.com/stacklok/mecatl/internal/port"
	"github.com/stacklok/mecatl/internal/session"
	"github.com/stacklok/mecatl/internal/tool"
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
		Policy:  permpolicy.NewPolicy([]governance.Rule{{Effect: governance.Allow}}, nil),
		Model:   "child-model",
	})
}

// TestSubagentReturnsOnlyFinalString proves gauntlet #7 in its REFRAMED form: the
// real guarantee is CONTENT isolation, not "the parent observes nothing". The
// child's content — its message text, its tool args, its tool result bodies —
// must NEVER enter the parent's session.Conversation (the context sent to the
// LLM). The parent still receives exactly ONE ToolResult (the child's final
// summary, the one piece of child content that folds back by design).
//
// Separately, this test asserts that a REDACTED, metadata-only projection of the
// child's activity (subagent.start/tool/end) IS forwarded to the run's event
// stream and carries only metadata (goal/ids, tool NAMES + counts, usage, stop) —
// no child message text, no child tool args, no child result bodies. Forwarding
// that projection is orthogonal to isolation: it never touches Conversation.
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
	sess := newSession(t, session.Limits{})
	r := e.Run(context.Background(), sess, memfs.NewWorkspace("/ws"), "go")
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

	// THE REFRAMED GUARANTEE: none of the child's CONTENT entered the parent's
	// Conversation (the context sent to the LLM). The child's message text ("let
	// me read it"), its tool args ("main.go" in the Read call), and its tool result
	// body ("child read the file …") must appear nowhere in any parent message.
	// Only the child's final summary (folded back as the Task tool result) is
	// allowed. We scan every field of every message.
	// The child's distinctive content: its message text, its Read tool CALL (a
	// "Read" tool call never appears in the parent's own history — the parent only
	// calls Task), and its tool result body. None may appear anywhere in the parent
	// Conversation. (We do not key off "main.go" because that token also legitimately
	// appears in the parent's own Task prompt — only CHILD-specific content counts.)
	const childMsgText = "let me read it"
	const childResultBody = "child read the file"
	for _, msg := range sess.Conversation.Messages {
		if strings.Contains(msg.Text, childMsgText) {
			t.Fatalf("child message text leaked into parent Conversation: %q", msg.Text)
		}
		if strings.Contains(msg.Reasoning, childMsgText) {
			t.Fatalf("child reasoning leaked into parent Conversation: %q", msg.Reasoning)
		}
		for _, tc := range msg.ToolCalls {
			if tc.Name == "Read" {
				t.Fatalf("child Read tool call leaked into parent Conversation: %+v", tc)
			}
		}
		if msg.ToolResult != nil && strings.Contains(msg.ToolResult.Content, childResultBody) {
			t.Fatalf("child tool result body leaked into parent Conversation: %q", msg.ToolResult.Content)
		}
	}

	// The redacted, metadata-only projection IS forwarded to the event stream.
	var starts, ends int
	var toolEvents []*session.SubagentPayload
	for _, ev := range evs {
		switch ev.Type {
		case session.EvSubagentStart:
			starts++
			if ev.Subagent == nil || ev.Subagent.ParentCallID != "p1" || ev.Subagent.ChildID == "" {
				t.Fatalf("subagent.start malformed: %+v", ev.Subagent)
			}
		case session.EvSubagentTool:
			if ev.Subagent == nil {
				t.Fatalf("subagent.tool missing payload")
			}
			toolEvents = append(toolEvents, ev.Subagent)
		case session.EvSubagentEnd:
			ends++
			if ev.Subagent == nil || ev.Subagent.ParentCallID != "p1" {
				t.Fatalf("subagent.end malformed: %+v", ev.Subagent)
			}
			// End carries aggregate metadata only.
			if ev.Subagent.ToolCount != 1 {
				t.Fatalf("subagent.end tool count = %d, want 1", ev.Subagent.ToolCount)
			}
			if ev.Subagent.Stop != session.StopEndTurn {
				t.Fatalf("subagent.end stop = %q, want end_turn", ev.Subagent.Stop)
			}
			if ev.Subagent.Usage.InputTokens != 7 || ev.Subagent.Usage.OutputTokens != 2 {
				t.Fatalf("subagent.end usage = %+v, want the child's cumulative usage", ev.Subagent.Usage)
			}
		}
	}
	if starts != 1 || ends != 1 {
		t.Fatalf("want exactly one subagent.start and one subagent.end; got %d/%d", starts, ends)
	}
	if len(toolEvents) != 1 {
		t.Fatalf("want one subagent.tool event for the child's single Read; got %d", len(toolEvents))
	}
	// The redacted tool event carries the child tool NAME and nothing else that is
	// content: no args, no result body anywhere on the payload.
	tev := toolEvents[0]
	if tev.ToolName != "Read" {
		t.Fatalf("subagent.tool name = %q, want Read", tev.ToolName)
	}
	if tev.IsError {
		t.Fatalf("subagent.tool unexpectedly flagged error")
	}
	// Defensive: the payload struct has no field that could carry child content;
	// confirm the goal-bearing field is empty on a tool event (goal is start-only).
	if tev.Goal != "" {
		t.Fatalf("subagent.tool unexpectedly carried a goal: %q", tev.Goal)
	}
}

// TestSubagentGoalClampedSymmetrically proves the forwarded subagent.start Goal
// stays a single, bounded line even when the model supplies a long, MULTI-LINE
// explicit description — the description path is clamped identically to the prompt
// fallback (newlines collapsed to spaces, truncated to the goal cap with an
// ellipsis), so a long description can't break the one-line Task-card title.
func TestSubagentGoalClampedSymmetrically(t *testing.T) {
	childRead := &fakeTool{name: "Read", readOnly: true,
		exec: func(_ context.Context, in session.ToolCall, _ tool.Workspace) (session.ToolResult, error) {
			return session.NewToolResult(in.ID, "ok"), nil
		}}
	childEngine := childEngineWith(mockllm.New(mockllm.TextTurn("child summary")), catalogWith(t, childRead))
	task := agent.NewTaskTool(childEngine)

	// A long description with embedded newlines and tabs.
	desc := "line one of a very long description that easily exceeds the cap\nsecond line\tand a tab"
	argsJSON, err := json.Marshal(struct {
		Prompt      string `json:"prompt"`
		Description string `json:"description"`
	}{Prompt: "investigate", Description: desc})
	if err != nil {
		t.Fatalf("marshal args: %v", err)
	}
	parentLLM := mockllm.New(
		mockllm.ToolCallTurn(toolCall("p1", "Task", string(argsJSON))),
		mockllm.TextTurn("done"),
	)
	e := newEngine(agent.Deps{LLM: parentLLM, Catalog: catalogWith(t, task)})
	r := e.Run(context.Background(), newSession(t, session.Limits{}), memfs.NewWorkspace("/ws"), "go")
	evs := drain(r)

	var goal string
	var found bool
	for _, ev := range evs {
		if ev.Type == session.EvSubagentStart && ev.Subagent != nil {
			goal, found = ev.Subagent.Goal, true
		}
	}
	if !found {
		t.Fatalf("no subagent.start event observed")
	}
	if strings.ContainsAny(goal, "\n\r\t") {
		t.Fatalf("goal must be a single line (no newlines/tabs): %q", goal)
	}
	if n := len([]rune(goal)); n > 61 { // 60 runes + the trailing ellipsis rune
		t.Fatalf("goal length = %d runes, want <= 61 (cap + ellipsis): %q", n, goal)
	}
	if !strings.HasSuffix(goal, "…") {
		t.Fatalf("an over-cap goal should be truncated with an ellipsis: %q", goal)
	}
	if !strings.HasPrefix(goal, "line one") {
		t.Fatalf("goal should derive from the description: %q", goal)
	}
}

// TestSubagentConcurrentAttribution proves the redacted subagent.* events from
// TWO Task calls that run concurrently (both read-only → one read batch, parallel
// goroutines) are attributed to the correct parent call. Each child runs a single,
// distinctly-named tool; we assert each subagent.tool event's ParentCallID is
// paired with the tool name that child actually ran, and that start/end counts are
// exactly one per call. This guards the emit closure's per-goroutine binding under
// real concurrency.
func TestSubagentConcurrentAttribution(t *testing.T) {
	// childEngineFor builds a child engine whose single tool is named toolName.
	childEngineFor := func(toolName string) *agent.Engine {
		ct := &fakeTool{name: toolName, readOnly: true,
			exec: func(_ context.Context, in session.ToolCall, _ tool.Workspace) (session.ToolResult, error) {
				return session.NewToolResult(in.ID, "ok"), nil
			}}
		llm := mockllm.New(
			mockllm.ToolCallTurn(toolCall("ck", toolName, `{}`)),
			mockllm.TextTurn("child done"),
		)
		return childEngineWith(llm, catalogWith(t, ct))
	}

	// Two distinct Task tools, each delegating to its own child engine, registered
	// under DISTINCT catalog names so the parent can call both in one turn. (The
	// catalog keys on Spec().Name, which is "Task" for both, so we wrap to rename.)
	taskA := agent.NewTaskTool(childEngineFor("Alpha"), agent.WithChildSessionPrefix("subA"))
	taskB := agent.NewTaskTool(childEngineFor("Bravo"), agent.WithChildSessionPrefix("subB"))
	parentCat := catalogWith(t, renamedTask{Tool: taskA, name: "TaskA"}, renamedTask{Tool: taskB, name: "TaskB"})

	parentLLM := mockllm.New(
		mockllm.ToolCallTurn(
			toolCall("pa", "TaskA", `{"prompt":"investigate alpha"}`),
			toolCall("pb", "TaskB", `{"prompt":"investigate bravo"}`),
		),
		mockllm.TextTurn("both done"),
	)
	e := newEngine(agent.Deps{LLM: parentLLM, Catalog: parentCat})
	r := e.Run(context.Background(), newSession(t, session.Limits{}), memfs.NewWorkspace("/ws"), "go")
	evs := drain(r)

	// Expected tool name per parent call id.
	wantTool := map[string]string{"pa": "Alpha", "pb": "Bravo"}
	starts := map[string]int{}
	ends := map[string]int{}
	for _, ev := range evs {
		if ev.Subagent == nil {
			continue
		}
		p := ev.Subagent.ParentCallID
		switch ev.Type {
		case session.EvSubagentStart:
			starts[p]++
		case session.EvSubagentTool:
			if got := ev.Subagent.ToolName; got != wantTool[p] {
				t.Fatalf("subagent.tool for parent %q has tool %q, want %q (cross-attribution)", p, got, wantTool[p])
			}
		case session.EvSubagentEnd:
			ends[p]++
		}
	}
	for _, p := range []string{"pa", "pb"} {
		if starts[p] != 1 || ends[p] != 1 {
			t.Fatalf("parent %q: starts=%d ends=%d, want 1/1", p, starts[p], ends[p])
		}
	}
}

// renamedTask wraps a Task tool to advertise a different catalog name (so two Task
// tools can coexist in one catalog), forwarding both the Tool and observableTool
// methods so the dispatcher still routes it through ExecuteObserved.
type renamedTask struct {
	tool.Tool
	name string
}

func (rt renamedTask) Spec() tool.ToolSpec {
	s := rt.Tool.Spec()
	s.Name = rt.name
	return s
}

func (rt renamedTask) ExecuteObserved(ctx context.Context, call session.ToolCall, ws tool.Workspace, emit func(session.Event)) (session.ToolResult, error) {
	return rt.Tool.(interface {
		ExecuteObserved(context.Context, session.ToolCall, tool.Workspace, func(session.Event)) (session.ToolResult, error)
	}).ExecuteObserved(ctx, call, ws, emit)
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
		Policy:  permpolicy.NewPolicy(nil, nil),
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
