package agent_test

import (
	"context"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/stacklok/mecatl/internal/adapter/memfs"
	"github.com/stacklok/mecatl/internal/adapter/mockllm"
	"github.com/stacklok/mecatl/internal/agent"
	"github.com/stacklok/mecatl/internal/session"
	"github.com/stacklok/mecatl/internal/tool"
)

// TestTaskPerCallMaxTurnsTightens is the MODEL-FACING e2e: a Task call that supplies
// max_turns lower than the default makes its child STRICTER for that call. The child
// would loop far longer (20 scripted tool-call turns) but the per-call max_turns=3 caps
// it at exactly 3 model calls.
func TestTaskPerCallMaxTurnsTightens(t *testing.T) {
	boundedEngine, boundedLLM := readLoopChild(t, 20)
	task := agent.NewTaskTool(boundedEngine)

	taskParentResults(t, task,
		mockllm.ToolCallTurn(toolCall("p1", "Task", `{"prompt":"loop","max_turns":3}`)),
		mockllm.TextTurn("parent done"),
	)
	if got := boundedLLM.Calls(); got != 3 {
		t.Fatalf("child made %d model calls, want 3 (per-call max_turns must tighten the child session)", got)
	}
}

// TestTaskPerCallMaxToolCallsTightens proves the per-call max_tool_calls override caps
// the child's total tool invocations. With max_tool_calls=2 and a child that would call
// a tool every turn, the child stops after the tool-call limit trips.
func TestTaskPerCallMaxToolCallsTightens(t *testing.T) {
	boundedEngine, boundedLLM := readLoopChild(t, 20)
	task := agent.NewTaskTool(boundedEngine)

	taskParentResults(t, task,
		mockllm.ToolCallTurn(toolCall("p1", "Task", `{"prompt":"loop","max_tool_calls":2}`)),
		mockllm.TextTurn("parent done"),
	)
	// Each turn issues exactly one tool call; MaxToolCalls=2 stops the run at the next
	// turn boundary once 2 tool calls are recorded — turn 1 (1 call) + turn 2 (2 calls,
	// limit reached) → the boundary check trips before turn 3, so EXACTLY 2 model calls.
	if got := boundedLLM.Calls(); got != 2 {
		t.Fatalf("child made %d model calls, want exactly 2 (per-call max_tool_calls=2 must bound it)", got)
	}
}

// TestTaskPerCallTightenOnlyCannotLoosen proves the TIGHTEN-ONLY guarantee: a per-call
// max_turns HIGHER than the inherited (default) limit is ignored — the model cannot use
// a per-call arg to escape the operator's bound. The child still stops at the inherited
// default MaxTurns (12), not the requested 100.
func TestTaskPerCallTightenOnlyCannotLoosen(t *testing.T) {
	boundedEngine, boundedLLM := readLoopChild(t, 50)
	task := agent.NewTaskTool(boundedEngine)

	taskParentResults(t, task,
		mockllm.ToolCallTurn(toolCall("p1", "Task", `{"prompt":"loop","max_turns":100}`)),
		mockllm.TextTurn("parent done"),
	)
	if got := boundedLLM.Calls(); got != agent.DefaultChildLimits().MaxTurns {
		t.Fatalf("child made %d model calls, want the inherited default MaxTurns=%d (tighten-only: a higher per-call arg must NOT loosen)",
			got, agent.DefaultChildLimits().MaxTurns)
	}
}

// sleepThenLoopTool is a read-only child tool that sleeps a fixed duration (long
// enough to blow a short deadline) and keeps returning success, so a child that calls
// it repeatedly will overrun any short wall-clock deadline. It is the ADVERSARIAL,
// deadline-ignoring child: it never voluntarily stops.
type sleepThenLoopTool struct {
	sleep time.Duration
	calls atomic.Int64
}

func (*sleepThenLoopTool) Spec() tool.ToolSpec {
	return tool.ToolSpec{Name: "Slow", Description: "slow", Schema: []byte(`{"type":"object"}`)}
}
func (*sleepThenLoopTool) ReadOnly() bool { return true }
func (s *sleepThenLoopTool) Execute(ctx context.Context, in session.ToolCall, _ tool.Workspace) (session.ToolResult, error) {
	s.calls.Add(1)
	select {
	case <-time.After(s.sleep):
	case <-ctx.Done():
	}
	return session.NewToolResult(in.ID, "slow ok"), nil
}

// TestTaskPerCallTimeoutProducesTimeBudgetError is the MODEL-FACING e2e + ADVERSARIAL
// (uncooperative-mock) test: a Task call with a short timeout_ms drives a child that
// IGNORES the deadline and keeps emitting tool calls / sleeping. The ctx deadline must
// terminate it, and the RESULT must be a model-addressable time-budget tool error.
func TestTaskPerCallTimeoutProducesTimeBudgetError(t *testing.T) {
	slow := &sleepThenLoopTool{sleep: 50 * time.Millisecond}
	// A child whose every turn calls the slow tool — it never stops on its own.
	var script []mockllm.Turn
	for i := 0; i < 100; i++ {
		script = append(script, mockllm.ToolCallTurn(toolCall("k", "Slow", `{}`)))
	}
	childEngine := childEngineWith(mockllm.New(script...), catalogWith(t, slow))
	task := agent.NewTaskTool(childEngine)

	results, _ := taskParentResults(t, task,
		mockllm.ToolCallTurn(toolCall("p1", "Task", `{"prompt":"loop forever","timeout_ms":120}`)),
		mockllm.TextTurn("parent recovered"),
	)
	if len(results) != 1 {
		t.Fatalf("want 1 result, got %d", len(results))
	}
	if !results[0].IsError {
		t.Fatalf("timed-out subagent should be an error result, got %+v", results[0])
	}
	if !strings.Contains(results[0].Content, "time budget") {
		t.Fatalf("error should mention the time budget, got %q", results[0].Content)
	}
}

// signalThenBlockTool signals each Execute entry on `entered` then blocks until the
// ctx is cancelled, so a test can deterministically know the child is in-flight before
// it cancels the PARENT ctx.
type signalThenBlockTool struct {
	entered chan struct{}
}

func (*signalThenBlockTool) Spec() tool.ToolSpec {
	return tool.ToolSpec{Name: "Block", Description: "block", Schema: []byte(`{"type":"object"}`)}
}
func (*signalThenBlockTool) ReadOnly() bool { return true }
func (b *signalThenBlockTool) Execute(ctx context.Context, in session.ToolCall, _ tool.Workspace) (session.ToolResult, error) {
	b.entered <- struct{}{}
	<-ctx.Done()
	return session.NewToolResult(in.ID, "blocked ok"), nil
}

// TestTaskParentCancelWithTimeoutIsNotTimeBudget is the BRANCH-GAP guard (QA #1): when
// timeout_ms is ALSO set, a PARENT cancellation must take the cancellation path, NOT be
// mislabeled "exceeded its time budget". The terminal branch keys off the SEPARATE
// timeoutCtx (DeadlineExceeded) rather than the merged ctx; if it regressed to ctx.Err()
// a parent cancel would falsely read as a deadline. A generous timeout_ms (that must NOT
// fire) is set; the child blocks in-flight; the test cancels the PARENT ctx. The result
// must be an error result that does NOT mention the time budget.
func TestTaskParentCancelWithTimeoutIsNotTimeBudget(t *testing.T) {
	block := &signalThenBlockTool{entered: make(chan struct{}, 1)}
	childEngine := childEngineWith(mockllm.New(
		mockllm.ToolCallTurn(toolCall("k", "Block", `{}`)),
		mockllm.TextTurn("should not reach"),
	), catalogWith(t, block))
	task := agent.NewTaskTool(childEngine)

	// A GENEROUS deadline that must not fire within the test; the parent cancel wins.
	parentCtx, cancel := context.WithCancel(context.Background())
	defer cancel()

	type out struct {
		res session.ToolResult
		err error
	}
	done := make(chan out, 1)
	go func() {
		res, err := task.Execute(parentCtx,
			session.NewToolCall("c1", "Task", []byte(`{"prompt":"block","timeout_ms":600000}`)),
			memfs.NewWorkspace("/base"))
		done <- out{res, err}
	}()

	<-block.entered // the child is in-flight inside Block, holding on ctx.Done()
	cancel()        // cancel the PARENT — NOT the deadline

	got := <-done
	if got.err != nil {
		t.Fatalf("Task.Execute returned a transport error: %v", got.err)
	}
	// A parent cancel must NOT be mislabeled as a time-budget overrun. The exact
	// rendering of a cancellation is not under test here; the load-bearing assertion is
	// the NEGATIVE: the result must never claim the child "exceeded its time budget".
	if strings.Contains(got.res.Content, "time budget") {
		t.Fatalf("parent cancel was mislabeled as a time-budget overrun: %q", got.res.Content)
	}
}

// TestTaskOmittedPerCallArgsUnchanged is the regression guard: a Task call with NONE of
// the per-call knobs behaves exactly as before — the child runs to its scripted text
// answer under the inherited default limits, and the result is the child's summary.
func TestTaskOmittedPerCallArgsUnchanged(t *testing.T) {
	childEngine := childEngineWith(mockllm.New(mockllm.TextTurn("CHILD SUMMARY")), catalogWith(t))
	task := agent.NewTaskTool(childEngine)

	results, _ := taskParentResults(t, task,
		mockllm.ToolCallTurn(toolCall("p1", "Task", `{"prompt":"investigate"}`)),
		mockllm.TextTurn("parent done"),
	)
	if len(results) != 1 || results[0].IsError || results[0].Content != "CHILD SUMMARY" {
		t.Fatalf("omitted per-call args must be byte-identical to today; got %+v", results[0])
	}
}

// TestTaskPerCallTimeoutOmittedNoDeadline proves an omitted timeout_ms imposes NO
// deadline: a child with a brief sleep finishes normally (no time-budget error) even
// though the same sleep would blow a tight deadline if one were set.
func TestTaskPerCallTimeoutOmittedNoDeadline(t *testing.T) {
	slow := &sleepThenLoopTool{sleep: 5 * time.Millisecond}
	childEngine := childEngineWith(mockllm.New(
		mockllm.ToolCallTurn(toolCall("k", "Slow", `{}`)),
		mockllm.TextTurn("finished"),
	), catalogWith(t, slow))
	task := agent.NewTaskTool(childEngine)

	results, _ := taskParentResults(t, task,
		mockllm.ToolCallTurn(toolCall("p1", "Task", `{"prompt":"go"}`)),
		mockllm.TextTurn("parent done"),
	)
	if len(results) != 1 || results[0].IsError {
		t.Fatalf("omitted timeout must impose no deadline; got error result %+v", results[0])
	}
	if results[0].Content != "finished" {
		t.Fatalf("result = %q, want the child's summary (no deadline)", results[0].Content)
	}
}
