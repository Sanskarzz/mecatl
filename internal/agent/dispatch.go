package agent

import (
	"context"
	"encoding/json"
	"fmt"
	"sync"
	"time"

	"github.com/stacklok/ozzharness/internal/governance"
	"github.com/stacklok/ozzharness/internal/session"
	"github.com/stacklok/ozzharness/internal/tool"
)

// dispatch executes a turn's tool calls and returns their results in the
// original call order, plus a cancelled flag set when ctx was cancelled (mid
// permission-await or mid-execution) so the loop can terminate as cancelled.
//
// Ordering contract (gauntlet #4, read-parallel / mutate-serial):
//   - Calls are processed in their original order, batched into maximal runs of
//     consecutive read-only tools.
//   - A read-only batch runs CONCURRENTLY (one goroutine per call).
//   - A mutating tool runs ALONE, strictly serially, never overlapping anything.
//   - Permission "asks" are sequenced one at a time (we never ask for two at
//     once): a batch that contains an Ask is resolved call-by-call before the
//     read-only calls that follow it execute.
//
// Results are keyed by CallID and re-assembled in input order.
func (e *Engine) dispatch(ctx context.Context, r *Run, sess *session.Session, ws tool.Workspace, turnIdx int, calls []session.ToolCall) ([]session.ToolResult, bool) {
	results := make(map[session.ToolCallID]session.ToolResult, len(calls))

	i := 0
	for i < len(calls) {
		c := calls[i]
		t, known := e.deps.Catalog.Lookup(c.Name)

		// Mutating (or unknown) tools flush alone, serially.
		if !known || !t.ReadOnly() {
			res, cancelled := e.runOne(ctx, r, sess, ws, turnIdx, c, t, known)
			if cancelled {
				return nil, true
			}
			results[c.ID] = res
			i++
			continue
		}

		// Gather the maximal run of consecutive read-only calls.
		j := i
		var batch []session.ToolCall
		for j < len(calls) {
			nc := calls[j]
			nt, ok := e.deps.Catalog.Lookup(nc.Name)
			if !ok || !nt.ReadOnly() {
				break
			}
			batch = append(batch, nc)
			j++
		}

		batchRes, cancelled := e.runReadBatch(ctx, r, sess, ws, turnIdx, batch)
		if cancelled {
			return nil, true
		}
		for id, res := range batchRes {
			results[id] = res
		}
		i = j
	}

	// Re-assemble in original call order.
	ordered := make([]session.ToolResult, len(calls))
	for k, c := range calls {
		ordered[k] = results[c.ID]
	}
	return ordered, false
}

// runReadBatch runs a batch of read-only tool calls concurrently. Each call still
// passes through permission evaluation and the hook lifecycle. Permission "asks"
// are sequenced first (resolved one at a time, before any execution) so we never
// surface two asks simultaneously; the calls cleared to execute then run in
// parallel. It returns the results keyed by CallID and a cancelled flag.
func (e *Engine) runReadBatch(ctx context.Context, r *Run, sess *session.Session, ws tool.Workspace, turnIdx int, batch []session.ToolCall) (map[session.ToolCallID]session.ToolResult, bool) {
	out := make(map[session.ToolCallID]session.ToolResult, len(batch))

	// Phase 1: resolve permission (asks sequenced) and run hooks. Calls that are
	// denied or hook-blocked get a synthesized result now and are excluded from
	// the parallel execution phase.
	type pending struct {
		call session.ToolCall
		t    tool.Tool
	}
	var toRun []pending
	for _, c := range batch {
		t, _ := e.deps.Catalog.Lookup(c.Name)
		decision, cancelled := e.authorize(ctx, r, sess, turnIdx, c)
		if cancelled {
			return nil, true
		}
		if decision.Effect == governance.Deny {
			out[c.ID] = denyResult(c, decision.Reason)
			e.emit(r, session.Event{Type: session.EvToolResult, Turn: turnIdx, ToolResult: ptr(out[c.ID])})
			continue
		}
		blocked, msg, herr := e.preHook(ctx, r, sess, turnIdx, c)
		if herr != nil {
			return nil, true
		}
		if blocked {
			out[c.ID] = session.NewToolError(c.ID, msg)
			e.emit(r, session.Event{Type: session.EvToolResult, Turn: turnIdx, ToolResult: ptr(out[c.ID])})
			continue
		}
		toRun = append(toRun, pending{call: c, t: t})
	}

	// Phase 2: execute the cleared read-only calls concurrently.
	var mu sync.Mutex
	var wg sync.WaitGroup
	for _, p := range toRun {
		wg.Add(1)
		go func(p pending) {
			defer wg.Done()
			res := e.execute(ctx, r, sess, ws, turnIdx, p.call, p.t)
			mu.Lock()
			out[p.call.ID] = res
			mu.Unlock()
		}(p)
	}
	wg.Wait()

	if ctx.Err() != nil {
		return nil, true
	}
	return out, false
}

// runOne handles a single (mutating or unknown) tool call serially: authorize,
// pre-hook, execute, post-hook. It returns the result and a cancelled flag.
func (e *Engine) runOne(ctx context.Context, r *Run, sess *session.Session, ws tool.Workspace, turnIdx int, c session.ToolCall, t tool.Tool, known bool) (session.ToolResult, bool) {
	if !known {
		res := session.NewToolError(c.ID, fmt.Sprintf("unknown tool %q", c.Name))
		e.emit(r, session.Event{Type: session.EvToolResult, Turn: turnIdx, ToolResult: ptr(res)})
		return res, false
	}

	decision, cancelled := e.authorize(ctx, r, sess, turnIdx, c)
	if cancelled {
		return session.ToolResult{}, true
	}
	if decision.Effect == governance.Deny {
		res := denyResult(c, decision.Reason)
		e.emit(r, session.Event{Type: session.EvToolResult, Turn: turnIdx, ToolResult: ptr(res)})
		return res, false
	}

	blocked, msg, herr := e.preHook(ctx, r, sess, turnIdx, c)
	if herr != nil {
		return session.ToolResult{}, true
	}
	if blocked {
		res := session.NewToolError(c.ID, msg)
		e.emit(r, session.Event{Type: session.EvToolResult, Turn: turnIdx, ToolResult: ptr(res)})
		return res, false
	}

	return e.execute(ctx, r, sess, ws, turnIdx, c, t), false
}

// authorize evaluates the permission policy for a call and, on Ask, pauses the
// loop until the client approves or denies (or ctx cancels). It returns the
// effective decision (Allow or Deny — an approved Ask becomes Allow, a denied or
// cancelled Ask becomes Deny) and a cancelled flag set only when ctx was
// cancelled while awaiting.
func (e *Engine) authorize(ctx context.Context, r *Run, sess *session.Session, turnIdx int, c session.ToolCall) (governance.PermissionDecision, bool) {
	decision := e.deps.Policy.Evaluate(ctx, sess.Mode, c)
	if decision.Effect != governance.Ask {
		return decision, false
	}

	askID := newAskID(sess.ID, sess.Counters.ToolCalls, c.ID)
	ask := session.PendingAsk{
		AskID:  askID,
		Tool:   c.Name,
		Args:   c.Args,
		Reason: decision.Reason,
	}

	// Register the resolution channel BEFORE pausing/emitting so an Approve that
	// races in cannot be lost.
	ch := r.asks.register(askID)
	if err := sess.PauseForApproval(ask); err != nil {
		r.asks.discard(askID)
		return governance.PermissionDecision{Effect: governance.Deny, Reason: "internal: cannot pause"}, false
	}
	a := ask
	e.emit(r, session.Event{Type: session.EvPermissionAsk, Turn: turnIdx, Ask: &a})

	allow, ok := r.asks.await(ctx, askID, ch)

	// Resume the session regardless of verdict; the loop (below) owns acting on
	// the decision, so the aggregate only reconciles its own lifecycle.
	if _, err := sess.ResumeWith(); err != nil {
		// Already resumed/terminal (e.g. cancel path); fall through.
		_ = err
	}

	if !ok {
		// ctx cancelled while awaiting.
		return governance.PermissionDecision{Effect: governance.Deny, Reason: "cancelled"}, true
	}
	if allow {
		return governance.PermissionDecision{Effect: governance.Allow}, false
	}
	return governance.PermissionDecision{
		Effect: governance.Deny,
		Reason: fmt.Sprintf("denied by user: %s", decision.Reason),
	}, false
}

// preHook runs the PreToolUse hook. It returns blocked=true with the hook's
// message when the hook vetoes the call (exit 2), and a non-nil error only when
// ctx was cancelled (the hook ran under a cancelled context).
func (e *Engine) preHook(ctx context.Context, r *Run, sess *session.Session, turnIdx int, c session.ToolCall) (blocked bool, msg string, err error) {
	if e.deps.Hooks == nil {
		return false, "", nil
	}
	if ctx.Err() != nil {
		return false, "", ctx.Err()
	}
	ev := governance.HookEvent{
		Phase:     governance.PhasePreToolUse,
		Tool:      c.Name,
		Input:     c.Args,
		SessionID: string(sess.ID),
	}
	outcome, herr := e.deps.Hooks.Run(ctx, ev)
	if herr != nil {
		if ctx.Err() != nil {
			return false, "", ctx.Err()
		}
		// A hook execution error is surfaced to the model as a block annotation
		// rather than aborting the whole run.
		return true, fmt.Sprintf("PreToolUse hook error: %v", herr), nil
	}
	if outcome.Block {
		m := outcome.Message
		if m == "" {
			m = "blocked by PreToolUse hook"
		}
		e.emit(r, session.Event{Type: session.EvHook, Turn: turnIdx, Text: m})
		return true, m, nil
	}
	return false, "", nil
}

// execute runs the tool against the workspace, times it, logs it, and emits the
// tool.call then tool.result events. The PostToolUse hook fires best-effort
// afterwards (a block there annotates but the tool already ran).
func (e *Engine) execute(ctx context.Context, r *Run, sess *session.Session, ws tool.Workspace, turnIdx int, c session.ToolCall, t tool.Tool) session.ToolResult {
	call := c
	e.emit(r, session.Event{Type: session.EvToolCall, Turn: turnIdx, ToolCall: &call})

	res, dur := e.timeExecute(ctx, ws, c, t)

	if e.deps.Logger != nil {
		e.deps.Logger.ToolCall(sess.ID, c, res, dur)
	}
	e.emit(r, session.Event{Type: session.EvToolResult, Turn: turnIdx, ToolResult: ptr(res)})

	e.postHook(ctx, r, sess, turnIdx, c, res)
	return res
}

// timeExecute runs the tool, capturing its result and elapsed wall time via the
// injected Clock (zero duration when no Clock is configured). A harness-level
// execution error becomes an error ToolResult so the model can recover; the loop
// never aborts on a single tool failure.
func (e *Engine) timeExecute(ctx context.Context, ws tool.Workspace, c session.ToolCall, t tool.Tool) (session.ToolResult, time.Duration) {
	var start time.Time
	if e.deps.Clock != nil {
		start = e.deps.Clock.Now()
	}
	res, err := t.Execute(ctx, c, ws)
	if err != nil {
		res = session.NewToolError(c.ID, fmt.Sprintf("tool %q failed: %v", c.Name, err))
	}
	var dur time.Duration
	if e.deps.Clock != nil {
		dur = e.deps.Clock.Now().Sub(start)
	}
	return res, dur
}

// postHook runs the PostToolUse hook best-effort. A block here only annotates
// (the tool already executed); execution errors are ignored.
func (e *Engine) postHook(ctx context.Context, r *Run, sess *session.Session, turnIdx int, c session.ToolCall, res session.ToolResult) {
	if e.deps.Hooks == nil || ctx.Err() != nil {
		return
	}
	input, _ := json.Marshal(struct {
		Args    json.RawMessage `json:"args"`
		Content string          `json:"content"`
		IsError bool            `json:"is_error"`
	}{Args: c.Args, Content: res.Content, IsError: res.IsError})
	ev := governance.HookEvent{
		Phase:     governance.PhasePostToolUse,
		Tool:      c.Name,
		Input:     input,
		SessionID: string(sess.ID),
	}
	outcome, err := e.deps.Hooks.Run(ctx, ev)
	if err == nil && outcome.Block && outcome.Message != "" {
		e.emit(r, session.Event{Type: session.EvHook, Turn: turnIdx, Text: outcome.Message})
	}
}

// emit assigns the next Seq via Run.emit, then mirrors the sequenced event to the
// injected EventSink when one is configured. The Run channel is the primary
// surface; the sink is an optional secondary relay.
func (e *Engine) emit(r *Run, ev session.Event) {
	sequenced := r.emit(ev)
	if e.deps.Sink != nil {
		e.deps.Sink.Emit(sequenced)
	}
}

// denyResult synthesizes the error ToolResult that teaches the model why a call
// was refused.
func denyResult(c session.ToolCall, reason string) session.ToolResult {
	if reason == "" {
		reason = "denied by permission policy"
	}
	return session.NewToolError(c.ID, "permission denied: "+reason)
}

// ptr returns a pointer to a copy of v (Events carry pointers to value objects).
func ptr(v session.ToolResult) *session.ToolResult {
	c := v
	return &c
}

// newAskID derives a stable, unique ask id for a permission pause.
func newAskID(id session.SessionID, n int, callID session.ToolCallID) string {
	return fmt.Sprintf("%s:%d:%s", id, n, callID)
}
