package agent_test

import (
	"context"
	"iter"
	"sync/atomic"
	"testing"

	"github.com/stacklok/mecatl/engine/adapter/memfs"
	"github.com/stacklok/mecatl/engine/adapter/mockllm"
	"github.com/stacklok/mecatl/engine/agent"
	"github.com/stacklok/mecatl/engine/port"
	"github.com/stacklok/mecatl/engine/session"
	"github.com/stacklok/mecatl/engine/tool"
)

// runawayProvider is an ADVERSARIAL, uncooperative port.LLMProvider: every turn it
// emits ONE tool call (so the loop keeps going) plus a fixed per-turn usage and a
// benign StopEndTurn — and it NEVER stops on its own. A cooperative scripted mock
// eventually runs out of turns; this one does not, so the ONLY thing that can
// terminate a run driven by it is a loop-level brake (the token budget / max-turns /
// a deadline). It is the regression guard that the budget is what stops a runaway.
type runawayProvider struct {
	perTurn session.Usage
	calls   atomic.Int64
}

func (*runawayProvider) Capabilities() port.ProviderCapabilities { return port.ProviderCapabilities{} }

func (p *runawayProvider) Stream(ctx context.Context, _ port.LLMRequest) (iter.Seq2[port.Chunk, error], error) {
	n := p.calls.Add(1)
	id := session.ToolCallID("c" + string(rune('A'+(n%26))))
	usage := p.perTurn
	return func(yield func(port.Chunk, error) bool) {
		if ctx.Err() != nil {
			return
		}
		tc := session.NewToolCall(id, "Loop", []byte(`{}`))
		if !yield(port.Chunk{Kind: port.ChunkToolCall, ToolCall: &tc}, nil) {
			return
		}
		if !yield(port.Chunk{Kind: port.ChunkUsage, Usage: &usage}, nil) {
			return
		}
		yield(port.Chunk{Kind: port.ChunkDone, Stop: session.StopEndTurn}, nil)
	}, nil
}

// loopTool is a trivial read-only tool the runawayProvider keeps calling so the loop
// never falls into the no-tool-call branch.
func loopTool() *fakeTool {
	return &fakeTool{name: "Loop", readOnly: true,
		exec: func(_ context.Context, in session.ToolCall, _ tool.Workspace) (session.ToolResult, error) {
			return session.NewToolResult(in.ID, "ok"), nil
		}}
}

// TestBudgetTerminatesRunawayCleanly is the MODEL-FACING e2e + ADVERSARIAL test: a
// provider that emits a tool call forever is stopped by the token budget alone. It
// asserts the run ends with StopBudget, COMPLETED (Reopen-recoverable, NOT failed), and
// that the per-turn usage accumulated past the ceiling.
func TestBudgetTerminatesRunawayCleanly(t *testing.T) {
	const perTurn = 100
	const budget = 350 // crossed after 4 turns (4*100=400 >= 350)

	llm := &runawayProvider{perTurn: session.Usage{InputTokens: 60, OutputTokens: 40}}
	e := newEngine(agent.Deps{LLM: llm, Catalog: catalogWith(t, loopTool()), MaxRunTokens: budget})
	sess := newSession(t, session.Limits{}) // no turn/tool limits: the budget is the only brake
	ws := memfs.NewWorkspace("/ws")

	evs := drain(e.Run(context.Background(), sess, ws, "run forever"))

	res := lastResult(t, evs)
	if res.Stop != session.StopBudget {
		t.Fatalf("terminal stop = %q, want %q (the token budget must stop the runaway)", res.Stop, session.StopBudget)
	}
	// COMPLETED + Reopen-recoverable: a budget stop is a clean terminal, never failed.
	if sess.State != session.StateCompleted {
		t.Fatalf("session state = %q, want completed (StopBudget is a clean terminal)", sess.State)
	}
	if err := sess.Reopen(); err != nil {
		t.Fatalf("Reopen after StopBudget: %v (a budget-stopped session must stay recoverable)", err)
	}
	// Cumulative usage on the result must have crossed the ceiling.
	if got := res.Usage.TotalTokens(); got < budget {
		t.Fatalf("cumulative usage = %d, want >= budget %d", got, budget)
	}
	_ = perTurn
}

// TestBudgetDisabledByZero asserts MaxRunTokens==0 disables the budget entirely: a
// finite cooperative script runs to its real end (StopEndTurn) with no early budget
// stop, byte-identical to the pre-budget behaviour.
func TestBudgetDisabledByZero(t *testing.T) {
	llm := mockllm.NewWith(nil,
		mockllm.ChunksTurn(
			mockllm.ToolCallChunk(toolCall("c1", "Loop", `{}`)),
			mockllm.UsageChunk(session.Usage{InputTokens: 10000, OutputTokens: 10000}),
			mockllm.DoneChunk(session.StopEndTurn),
		),
		mockllm.TextTurn("done"),
	)
	e := newEngine(agent.Deps{LLM: llm, Catalog: catalogWith(t, loopTool()), MaxRunTokens: 0})
	sess := newSession(t, session.Limits{})
	evs := drain(e.Run(context.Background(), sess, memfs.NewWorkspace("/ws"), "go"))

	res := lastResult(t, evs)
	if res.Stop != session.StopEndTurn || res.Text != "done" {
		t.Fatalf("result = {stop:%q text:%q}, want {end_turn done} (MaxRunTokens==0 disables the budget)", res.Stop, res.Text)
	}
}

// TestBudgetBoundaryCheckCompletesInFlightTurn is the ADVERSARIAL boundary case: a
// SINGLE turn whose usage massively OVERSHOOTS the ceiling must still complete that
// turn (the check is at the turn boundary, never a mid-stream abort — preserving
// no-replay-after-first-chunk). The overshoot turn runs to completion; the budget then
// stops the run BEFORE the next turn.
func TestBudgetBoundaryCheckCompletesInFlightTurn(t *testing.T) {
	const budget = 50
	llm := mockllm.NewWith(nil,
		// Turn 1: a tool call (loop continues) + a huge usage overshoot.
		mockllm.ChunksTurn(
			mockllm.ToolCallChunk(toolCall("c1", "Loop", `{}`)),
			mockllm.UsageChunk(session.Usage{InputTokens: 5000, OutputTokens: 5000}),
			mockllm.DoneChunk(session.StopEndTurn),
		),
		// Turn 2 must NEVER run: the budget trips at the boundary before it.
		mockllm.TextTurn("should-never-run"),
	)
	e := newEngine(agent.Deps{LLM: llm, Catalog: catalogWith(t, loopTool()), MaxRunTokens: budget})
	sess := newSession(t, session.Limits{})
	evs := drain(e.Run(context.Background(), sess, memfs.NewWorkspace("/ws"), "go"))

	// Exactly ONE model call: the in-flight turn completed, the budget stopped before turn 2.
	if got := llm.Calls(); got != 1 {
		t.Fatalf("model calls = %d, want 1 (overshoot turn completes; budget stops before the next turn)", got)
	}
	res := lastResult(t, evs)
	if res.Stop != session.StopBudget {
		t.Fatalf("terminal stop = %q, want %q", res.Stop, session.StopBudget)
	}
	if res.Text == "should-never-run" {
		t.Fatal("the second turn ran; the budget must trip at the boundary, not after the next turn")
	}
}

// TestSubagentChildInheritsBudgetAndReturnsCleanResult is the CHILD-PATH behavioral guard
// (not a field-copy assertion): a Subagent child whose engine carries MaxRunTokens and is
// driven by a never-stopping runawayProvider actually hits StopBudget and the Subagent tool
// returns a CLEAN tool result (not an error) — StopBudget is a non-error terminal, so it
// folds back as the child's summary, never a tool error like StopError would.
func TestSubagentChildInheritsBudgetAndReturnsCleanResult(t *testing.T) {
	const budget = 250
	childLLM := &runawayProvider{perTurn: session.Usage{InputTokens: 60, OutputTokens: 40}}
	childEngine := agent.NewEngine(agent.Deps{
		LLM:          childLLM,
		Catalog:      catalogWith(t, loopTool()),
		Policy:       allowAll(),
		Model:        "child-model",
		MaxRunTokens: budget,
	})
	task := agent.NewSubagentTool(childEngine)

	res, err := task.Execute(context.Background(),
		session.NewToolCall("p1", "Subagent", []byte(`{"prompt":"run forever"}`)),
		memfs.NewWorkspace("/ws"))
	if err != nil {
		t.Fatalf("Subagent.Execute returned a transport error: %v", err)
	}
	// A budget-stopped child is a CLEAN terminal → a normal tool result, NOT an error.
	if res.IsError {
		t.Fatalf("budget-stopped child must yield a clean tool result, got error: %q", res.Content)
	}
	// The child must have actually run multiple turns before the budget tripped (proving
	// the child engine inherited and enforced the ceiling, not stopped at turn 1).
	if got := childLLM.calls.Load(); got < 2 {
		t.Fatalf("child made %d model calls, want >= 2 (the inherited budget should bound a runaway child)", got)
	}
}

// countingProvider records how many Stream calls it received, then delegates to a
// scripted mockllm. It proves whether the loop reached a model call at all.
type countingProvider struct {
	calls atomic.Int64
	inner port.LLMProvider
}

func (p *countingProvider) Capabilities() port.ProviderCapabilities { return p.inner.Capabilities() }

func (p *countingProvider) Stream(ctx context.Context, req port.LLMRequest) (iter.Seq2[port.Chunk, error], error) {
	p.calls.Add(1)
	return p.inner.Stream(ctx, req)
}

// TestBudgetReadsCumulativeAggregate is the restart-budget guard (cloud-native
// Phase 1): a session loaded carrying cumulative Usage ALREADY past the MaxRunTokens
// ceiling (the snapshot-restore shape) must trip StopBudget at the FIRST turn
// boundary — before any model call — instead of re-granting a full fresh budget.
// The budget brake is evaluated against the AGGREGATE's cumulative Usage
// (sess.Usage), NOT a fresh-from-zero per-run total; there is no loop seed. Mutation:
// changing the budget check from `budgetExhausted(r, sess.Usage)` to
// `budgetExhausted(r, total)` makes the run proceed and the model gets called.
func TestBudgetReadsCumulativeAggregate(t *testing.T) {
	const budget = 350

	inner := mockllm.New(mockllm.TextTurn("should-never-run"))
	llm := &countingProvider{inner: inner}
	e := newEngine(agent.Deps{LLM: llm, Catalog: catalogWith(t, loopTool()), MaxRunTokens: budget})

	// A session reloaded mid-conversation with prior spend already over the ceiling.
	sess := newSession(t, session.Limits{})
	sess.Usage = session.Usage{InputTokens: 300, OutputTokens: 100} // 400 >= 350

	evs := drain(e.Run(context.Background(), sess, memfs.NewWorkspace("/ws"), "continue"))

	res := lastResult(t, evs)
	if res.Stop != session.StopBudget {
		t.Fatalf("terminal stop = %q, want %q (the cumulative budget must trip at the first boundary)", res.Stop, session.StopBudget)
	}
	if got := llm.calls.Load(); got != 0 {
		t.Fatalf("model was called %d time(s); want 0 (the budget tripped at the boundary before any turn)", got)
	}
	// The aggregate's cumulative usage still reflects the loaded prior spend (the
	// per-run EvResult.Usage is zero here — no turn ran this run — which is correct).
	if got := sess.Usage.TotalTokens(); got < budget {
		t.Fatalf("aggregate usage = %d, want >= prior spend %d", got, budget)
	}
	// Clean terminal, Reopen-recoverable.
	if sess.State != session.StateCompleted {
		t.Fatalf("session state = %q, want completed", sess.State)
	}
}
