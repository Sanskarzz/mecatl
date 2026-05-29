package agent_test

import (
	"context"
	"strings"
	"sync"
	"testing"

	"github.com/stacklok/ozzharness/internal/adapter/memfs"
	"github.com/stacklok/ozzharness/internal/adapter/mockllm"
	"github.com/stacklok/ozzharness/internal/agent"
	"github.com/stacklok/ozzharness/internal/governance"
	"github.com/stacklok/ozzharness/internal/prompt"
	"github.com/stacklok/ozzharness/internal/session"
	"github.com/stacklok/ozzharness/internal/tool"
)

// recordingHooks is a fake port.HookRunner that records every phase it is asked
// to run, in order, and blocks on a configured set of phases.
type recordingHooks struct {
	mu     sync.Mutex
	phases []governance.HookPhase
	block  map[governance.HookPhase]string // phase → block message
}

func newRecordingHooks(block map[governance.HookPhase]string) *recordingHooks {
	return &recordingHooks{block: block}
}

func (h *recordingHooks) Run(_ context.Context, ev governance.HookEvent) (governance.HookOutcome, error) {
	h.mu.Lock()
	h.phases = append(h.phases, ev.Phase)
	h.mu.Unlock()
	if msg, ok := h.block[ev.Phase]; ok {
		return governance.HookOutcome{Block: true, Message: msg}, nil
	}
	return governance.HookOutcome{}, nil
}

func (h *recordingHooks) recorded() []governance.HookPhase {
	h.mu.Lock()
	defer h.mu.Unlock()
	out := make([]governance.HookPhase, len(h.phases))
	copy(out, h.phases)
	return out
}

func (h *recordingHooks) count(p governance.HookPhase) int {
	n := 0
	for _, ph := range h.recorded() {
		if ph == p {
			n++
		}
	}
	return n
}

func runWithHooks(t *testing.T, llm *mockllm.Provider, hooks *recordingHooks) []session.Event {
	t.Helper()
	cat := catalogWith(t, &fakeTool{name: "Read", readOnly: true, exec: okExec})
	e := newEngine(agent.Deps{LLM: llm, Catalog: cat, Hooks: hooks})
	sess := newSession(t, session.Limits{})
	ws := memfs.NewWorkspace("/ws")
	return drain(e.Run(context.Background(), sess, ws, "hi"))
}

func okExec(_ context.Context, in session.ToolCall, _ tool.Workspace) (session.ToolResult, error) {
	return session.NewToolResult(in.ID, "ok"), nil
}

// TestSessionStartFiresOnceBeforeTurns asserts SessionStart fires exactly once,
// before the first UserPromptSubmit and before any model call.
func TestSessionStartFiresOnceBeforeTurns(t *testing.T) {
	llm := mockllm.New(mockllm.TextTurn("done"))
	hooks := newRecordingHooks(nil)
	runWithHooks(t, llm, hooks)

	if got := hooks.count(governance.PhaseSessionStart); got != 1 {
		t.Fatalf("SessionStart fired %d times, want 1", got)
	}
	rec := hooks.recorded()
	if len(rec) == 0 || rec[0] != governance.PhaseSessionStart {
		t.Fatalf("first phase = %v, want SessionStart; all=%v", rec, rec)
	}
	// SessionStart must precede UserPromptSubmit.
	idxStart, idxPrompt := -1, -1
	for i, p := range rec {
		if p == governance.PhaseSessionStart && idxStart < 0 {
			idxStart = i
		}
		if p == governance.PhaseUserPromptSubmit && idxPrompt < 0 {
			idxPrompt = i
		}
	}
	if idxStart > idxPrompt {
		t.Fatalf("SessionStart(%d) did not precede UserPromptSubmit(%d)", idxStart, idxPrompt)
	}
}

// TestUserPromptSubmitFiresBeforeModel asserts UserPromptSubmit fires before the
// first model call on a normal (non-blocking) run.
func TestUserPromptSubmitFiresBeforeModel(t *testing.T) {
	llm := mockllm.New(mockllm.TextTurn("done"))
	hooks := newRecordingHooks(nil)
	runWithHooks(t, llm, hooks)

	if got := hooks.count(governance.PhaseUserPromptSubmit); got != 1 {
		t.Fatalf("UserPromptSubmit fired %d times, want 1", got)
	}
	if llm.Calls() != 1 {
		t.Fatalf("model called %d times, want 1", llm.Calls())
	}
}

// TestUserPromptSubmitBlockAbortsBeforeLLM asserts a Block on UserPromptSubmit
// rejects the prompt and the model is NEVER called.
func TestUserPromptSubmitBlockAbortsBeforeLLM(t *testing.T) {
	llm := mockllm.New(mockllm.TextTurn("should never run"))
	hooks := newRecordingHooks(map[governance.HookPhase]string{
		governance.PhaseUserPromptSubmit: "prompt rejected: policy violation",
	})
	evs := runWithHooks(t, llm, hooks)

	if llm.Calls() != 0 {
		t.Fatalf("model was called %d times; want 0 (prompt should be rejected pre-call)", llm.Calls())
	}
	res := lastResult(t, evs)
	if res.Stop != session.StopError {
		t.Fatalf("stop = %q, want error (rejected prompt)", res.Stop)
	}
	if res.Error == "" {
		t.Fatalf("expected a rejection error message")
	}
	if !containsType(evs, session.EvHook) {
		t.Fatalf("expected a hook event explaining the rejection")
	}
	// Stop still fires on the rejection (terminal) path.
	if got := hooks.count(governance.PhaseStop); got != 1 {
		t.Fatalf("Stop fired %d times on rejection, want 1", got)
	}
}

// TestStopFiresOnceOnNormalCompletion asserts Stop fires exactly once at terminal
// end of a successful run.
func TestStopFiresOnceOnNormalCompletion(t *testing.T) {
	llm := mockllm.New(mockllm.TextTurn("done"))
	hooks := newRecordingHooks(nil)
	runWithHooks(t, llm, hooks)

	if got := hooks.count(governance.PhaseStop); got != 1 {
		t.Fatalf("Stop fired %d times, want 1", got)
	}
	rec := hooks.recorded()
	if rec[len(rec)-1] != governance.PhaseStop {
		t.Fatalf("last phase = %v, want Stop", rec[len(rec)-1])
	}
}

// TestStopFiresOnLimitTermination asserts Stop fires once when a stop-condition
// (MaxTurns) terminates the run.
func TestStopFiresOnLimitTermination(t *testing.T) {
	// One scripted turn that calls a tool, with MaxTurns=1 so the loop stops.
	llm := mockllm.New(mockllm.ToolCallTurn(toolCall("c1", "Read", `{"path":"a"}`)))
	hooks := newRecordingHooks(nil)
	cat := catalogWith(t, &fakeTool{name: "Read", readOnly: true, exec: okExec})
	e := newEngine(agent.Deps{LLM: llm, Catalog: cat, Hooks: hooks})
	sess := newSession(t, session.Limits{MaxTurns: 1})
	drain(e.Run(context.Background(), sess, memfs.NewWorkspace("/ws"), "hi"))

	if got := hooks.count(governance.PhaseStop); got != 1 {
		t.Fatalf("Stop fired %d times on limit termination, want 1", got)
	}
}

// TestNilHooksAreNoOp asserts a run with no HookRunner configured (the default)
// works unchanged and never panics.
func TestNilHooksAreNoOp(t *testing.T) {
	llm := mockllm.New(mockllm.TextTurn("done"))
	cat := catalogWith(t, &fakeTool{name: "Read", readOnly: true, exec: okExec})
	e := newEngine(agent.Deps{LLM: llm, Catalog: cat}) // Hooks left nil
	sess := newSession(t, session.Limits{})
	evs := drain(e.Run(context.Background(), sess, memfs.NewWorkspace("/ws"), "hi"))
	if res := lastResult(t, evs); res.Stop != session.StopEndTurn {
		t.Fatalf("stop = %q, want end_turn", res.Stop)
	}
}

// --- P3: InstructionAssembler seam ------------------------------------------

// fakeAssembler records that it was invoked and returns a fixed message.
type fakeAssembler struct {
	called int
	msg    string
}

func (a *fakeAssembler) Assemble(_ context.Context, _ tool.Workspace) ([]session.Message, error) {
	a.called++
	return []session.Message{session.NewUserMessage(a.msg)}, nil
}

// TestLoopUsesInjectedAssembler asserts the loop calls the injected
// InstructionAssembler instead of the default.
func TestLoopUsesInjectedAssembler(t *testing.T) {
	llm := mockllm.New(mockllm.TextTurn("done"))
	asm := &fakeAssembler{msg: "INJECTED INSTRUCTIONS"}
	cat := catalogWith(t, &fakeTool{name: "Read", readOnly: true, exec: okExec})
	e := newEngine(agent.Deps{LLM: llm, Catalog: cat, Instructions: asm})
	sess := newSession(t, session.Limits{})
	drain(e.Run(context.Background(), sess, memfs.NewWorkspace("/ws"), "hi"))

	if asm.called != 1 {
		t.Fatalf("assembler invoked %d times, want 1", asm.called)
	}
	// The injected instruction message must be in the conversation.
	found := false
	for _, m := range sess.Conversation.Messages {
		if m.Text == "INJECTED INSTRUCTIONS" {
			found = true
		}
	}
	if !found {
		t.Fatalf("injected instruction message not recorded in conversation")
	}
}

// TestDefaultAssemblerWhenNil asserts NewEngine defaults the assembler so a run
// with no Instructions field set still discovers root instructions.
func TestDefaultAssemblerWhenNil(t *testing.T) {
	llm := mockllm.New(mockllm.TextTurn("done"))
	cat := catalogWith(t, &fakeTool{name: "Read", readOnly: true, exec: okExec})
	e := newEngine(agent.Deps{LLM: llm, Catalog: cat}) // Instructions nil → RootAssembler
	sess := newSession(t, session.Limits{})
	ws := memfs.NewWorkspace("/ws")
	if err := ws.Write(context.Background(), "AGENTS.md", []byte("project rule")); err != nil {
		t.Fatalf("seed: %v", err)
	}
	drain(e.Run(context.Background(), sess, ws, "hi"))

	found := false
	for _, m := range sess.Conversation.Messages {
		if m.Role == session.RoleUser && strings.Contains(m.Text, "project rule") {
			found = true
		}
	}
	if !found {
		t.Fatalf("default RootAssembler did not discover AGENTS.md")
	}
}

// ensure prompt import is used (RootAssembler default wiring is exercised
// indirectly; reference it to document the default explicitly).
var _ prompt.InstructionAssembler = prompt.RootAssembler{}
