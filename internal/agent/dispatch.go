package agent

import (
	"context"
	"encoding/json"
	"fmt"
	"sync"
	"time"

	"github.com/stacklok/mecatl/internal/governance"
	"github.com/stacklok/mecatl/internal/session"
	"github.com/stacklok/mecatl/internal/tool"
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
		c := c // local copy: openCard takes &c, and this loop variable is reused.
		t, _ := e.deps.Catalog.Lookup(c.Name)
		// Open the tool card BEFORE the permission/hook gate so any synthesized
		// failure (a deny result or a PreToolUse veto) lands on a card the client has
		// already seen — see openCard.
		e.openCard(r, turnIdx, c)
		decision, cancelled := e.authorize(ctx, r, sess, turnIdx, c)
		if cancelled {
			return nil, true
		}
		if decision.Effect == governance.Deny {
			out[c.ID] = denyResult(c, decision.Reason)
			e.emit(r, session.Event{Type: session.EvToolResult, Turn: turnIdx, ToolResult: ptr(out[c.ID])})
			continue
		}
		effective, blocked, msg, herr := e.preHook(ctx, r, sess, turnIdx, c)
		if herr != nil {
			return nil, true
		}
		if blocked {
			out[c.ID] = session.NewToolError(c.ID, msg)
			e.emit(r, session.Event{Type: session.EvToolResult, Turn: turnIdx, ToolResult: ptr(out[c.ID])})
			continue
		}
		// Execute the EFFECTIVE call (args possibly rewritten by the hook).
		toRun = append(toRun, pending{call: effective, t: t})
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

	// Open the tool card BEFORE the permission/hook gate so any synthesized failure
	// (a deny result or a PreToolUse veto) lands on a card the client has already
	// seen. Only known tools get a card; the unknown-tool branch above emits only
	// its error result, since there is no real tool to open a card for.
	e.openCard(r, turnIdx, c)

	decision, cancelled := e.authorize(ctx, r, sess, turnIdx, c)
	if cancelled {
		return session.ToolResult{}, true
	}
	if decision.Effect == governance.Deny {
		res := denyResult(c, decision.Reason)
		e.emit(r, session.Event{Type: session.EvToolResult, Turn: turnIdx, ToolResult: ptr(res)})
		return res, false
	}

	effective, blocked, msg, herr := e.preHook(ctx, r, sess, turnIdx, c)
	if herr != nil {
		return session.ToolResult{}, true
	}
	if blocked {
		res := session.NewToolError(c.ID, msg)
		e.emit(r, session.Event{Type: session.EvToolResult, Turn: turnIdx, ToolResult: ptr(res)})
		return res, false
	}

	// Execute the EFFECTIVE call (args possibly rewritten by the PreToolUse hook).
	return e.execute(ctx, r, sess, ws, turnIdx, effective, t), false
}

// authorize evaluates the permission policy for a call and, on Ask, pauses the
// loop until the client approves or denies (or ctx cancels). It returns the
// effective decision (Allow or Deny — an approved Ask becomes Allow, a denied or
// cancelled Ask becomes Deny) and a cancelled flag set only when ctx was
// cancelled while awaiting.
func (e *Engine) authorize(ctx context.Context, r *Run, sess *session.Session, turnIdx int, c session.ToolCall) (governance.PermissionDecision, bool) {
	decision := e.deps.Policy.Evaluate(ctx, sess.ID, sess.Mode, c)
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

	verdict, ok := r.asks.await(ctx, askID, ch)

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

	switch verdict {
	case session.VerdictAllowAlways:
		// Learn a per-session allow rule for this exact tool+pattern as a SIDE
		// EFFECT — it governs FUTURE calls only and never blocks or re-evaluates the
		// current one (which proceeds one-shot via the Allow below). Learn is itself
		// a no-op when the call is not safely learnable (compound/substituted Bash,
		// no targetable pattern). It can NEVER override a deny or bypass plan mode:
		// the rule is consulted by Evaluate at the lowest scope, behind the
		// deny-dominant fold and the plan-mode gate.
		e.deps.Policy.Learn(sess.ID, c)
		return governance.PermissionDecision{Effect: governance.Allow}, false
	case session.VerdictAllowOnce:
		// Permit THIS call only; nothing learned.
		return governance.PermissionDecision{Effect: governance.Allow}, false
	default:
		// VerdictDeny (incl. the zero value / fail-safe).
		return governance.PermissionDecision{
			Effect: governance.Deny,
			Reason: fmt.Sprintf("denied by user: %s", decision.Reason),
		}, false
	}
}

// preHook runs the PreToolUse hook. It returns blocked=true with the hook's
// message when the hook vetoes the call (exit 2), and a non-nil error only when
// ctx was cancelled (the hook ran under a cancelled context). When the hook is
// allowed, it returns the EFFECTIVE call to execute: identical to the input call
// unless the hook returned a non-empty HookOutcome.Mutated payload, in which case
// the call's Args are rewritten (see the mutation note below).
//
// The PreToolUse HookEvent.Input is the tool's raw arguments JSON (c.Args, no
// wrapper). HookOutcome.Mutated is interpreted SYMMETRICALLY: it is the rewritten
// arguments JSON, and it replaces c.Args while preserving the CallID and tool
// Name. A malformed (non-JSON) mutation is ignored — the original args stand —
// and a notice event is emitted.
//
// TRUST / ORDERING (security-relevant): the permission policy (authorize →
// Policy.Evaluate) has ALREADY run on the ORIGINAL, pre-mutation args by the time
// preHook is called. The mutated args are NOT re-permission-checked. This is
// deliberate and matches the trust model: a PreToolUse hook is operator-deployed
// and strictly more trusted than the model, so a hook is allowed to rewrite a
// call past the policy that gated the model's original request (mirroring Claude
// Code semantics). Callers MUST execute the returned call, not the input call.
func (e *Engine) preHook(ctx context.Context, r *Run, sess *session.Session, turnIdx int, c session.ToolCall) (effective session.ToolCall, blocked bool, msg string, err error) {
	if e.deps.Hooks == nil {
		return c, false, "", nil
	}
	if ctx.Err() != nil {
		return c, false, "", ctx.Err()
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
			return c, false, "", ctx.Err()
		}
		// A hook execution error is surfaced to the model as a block annotation
		// rather than aborting the whole run.
		return c, true, fmt.Sprintf("PreToolUse hook error: %v", herr), nil
	}
	if outcome.Block {
		m := outcome.Message
		if m == "" {
			m = "blocked by PreToolUse hook"
		}
		e.emit(r, session.Event{Type: session.EvHook, Turn: turnIdx, Text: m,
			Hook: &session.HookPayload{Phase: string(governance.PhasePreToolUse), Tool: c.Name, Decision: session.HookBlocked, CallID: c.ID}})
		return c, true, m, nil
	}
	if len(outcome.Mutated) > 0 {
		// Apply the mutation: the payload is the rewritten args JSON. Validate it as
		// JSON before adopting it; a malformed payload is ignored. The permission
		// decision is NOT re-evaluated on these args — see the trust note above.
		if json.Valid(outcome.Mutated) {
			e.emit(r, session.Event{Type: session.EvHook, Turn: turnIdx, Text: "PreToolUse hook rewrote tool arguments for " + c.Name,
				Hook: &session.HookPayload{Phase: string(governance.PhasePreToolUse), Tool: c.Name, Decision: session.HookModified, CallID: c.ID}})
			return session.NewToolCall(c.ID, c.Name, outcome.Mutated), false, "", nil
		}
		e.emit(r, session.Event{Type: session.EvHook, Turn: turnIdx, Text: "PreToolUse hook returned a malformed argument mutation (ignored)",
			Hook: &session.HookPayload{Phase: string(governance.PhasePreToolUse), Tool: c.Name, Decision: session.HookInfo, CallID: c.ID}})
	}
	return c, false, "", nil
}

// execute runs the tool against the workspace, times it, runs the PostToolUse
// hook, then logs and emits the effective result.
//
// The EvToolCall "open card" event is NOT emitted here — it is emitted by openCard
// BEFORE the permission/hook gate (in runReadBatch Phase 1 and runOne), so a
// synthesized failure on the gated paths (a deny result or a PreToolUse veto) lands
// on a card the client has already opened. execute is only ever reached AFTER the
// gate, so the card always exists by the time the result is emitted.
//
// Ordering note: PostToolUse runs BEFORE the result is logged, emitted, or
// returned, so a PostToolUse result mutation is reflected uniformly — the
// EFFECTIVE (possibly rewritten) result is what the audit Logger records, what the
// client sees on the event stream, AND what the loop records for the model
// (RecordToolResults records exactly what this returns). There is deliberately no
// divergence between the three views; in particular a redacting hook's redaction
// reaches the audit log too rather than leaking the raw output. PostToolUse remains
// otherwise best-effort: a hook execution error does not abort, and a block only
// annotates (the tool already ran; a block neither undoes nor suppresses the
// result).
func (e *Engine) execute(ctx context.Context, r *Run, sess *session.Session, ws tool.Workspace, turnIdx int, c session.ToolCall, t tool.Tool) session.ToolResult {
	res, dur := e.timeExecute(ctx, r, ws, turnIdx, c, t)

	// PostToolUse may rewrite the result. The effective (possibly rewritten) result
	// is what we log, emit, and return, so the audit log, the client event stream,
	// and the model's recorded history all agree — in particular, a redacting hook's
	// redaction reaches the audit log too rather than leaking the raw tool output.
	res = e.postHook(ctx, r, sess, turnIdx, c, res)

	if e.deps.Logger != nil {
		e.deps.Logger.ToolCall(sess.ID, c, res, dur)
	}

	e.emit(r, session.Event{Type: session.EvToolResult, Turn: turnIdx, ToolResult: ptr(res)})
	return res
}

// timeExecute runs the tool, capturing its result and elapsed wall time via the
// injected Clock (zero duration when no Clock is configured). A harness-level
// execution error becomes an error ToolResult so the model can recover; the loop
// never aborts on a single tool failure.
//
// Observability seam: a tool that implements observableTool (the Task subagent)
// is run via ExecuteObserved with an emit closure bound to THIS run, so it can
// forward a redacted, metadata-only projection of its internal activity (the
// subagent.* events) onto the same sequenced event stream the loop emits. The
// closure stamps the current Turn and routes through e.emit (Seq + sink mirror),
// matching every other dispatch emit. It is invoked from the (possibly
// concurrent, read-parallel) tool goroutine — consistent with the existing
// dispatch emits, which e.emit serialises. Tools that do not implement the seam
// take the ordinary Execute path unchanged.
func (e *Engine) timeExecute(ctx context.Context, r *Run, ws tool.Workspace, turnIdx int, c session.ToolCall, t tool.Tool) (session.ToolResult, time.Duration) {
	var start time.Time
	if e.deps.Clock != nil {
		start = e.deps.Clock.Now()
	}
	var (
		res session.ToolResult
		err error
	)
	if ot, ok := t.(observableTool); ok {
		emit := func(ev session.Event) {
			ev.Turn = turnIdx
			e.emit(r, ev)
		}
		res, err = ot.ExecuteObserved(ctx, c, ws, emit)
	} else {
		res, err = t.Execute(ctx, c, ws)
	}
	if err != nil {
		res = session.NewToolError(c.ID, fmt.Sprintf("tool %q failed: %v", c.Name, err))
	}
	var dur time.Duration
	if e.deps.Clock != nil {
		dur = e.deps.Clock.Now().Sub(start)
	}
	return res, dur
}

// resultPayload is the JSON shape of the PostToolUse hook's view of a tool
// result and the symmetric shape its Mutated payload is interpreted as.
type resultPayload struct {
	Content string `json:"content"`
	IsError bool   `json:"is_error"`
}

// postHook runs the PostToolUse hook best-effort and returns the EFFECTIVE
// result. It carries the result under review as the HookEvent.Input
// ({"content", "is_error"}, plus the call args for context). A block only
// annotates (the tool already executed; the result is neither undone nor
// suppressed); a hook execution error is ignored — neither aborts the run.
//
// Mutation: a non-empty HookOutcome.Mutated is interpreted SYMMETRICALLY with the
// result the hook saw — the same {"content", "is_error"} object — and, when valid
// JSON, REPLACES the result (a fresh session.ToolResult preserving the CallID,
// via NewToolError when is_error is true else NewToolResult). A malformed (non-
// JSON) payload is ignored (the original result stands) and a notice is emitted.
//
// TRUST: a PostToolUse hook is operator-deployed and trusted, so it may rewrite
// what the model sees the tool returned (e.g. redact secrets from output). Because
// execute emits the EFFECTIVE result, the client stream shows the rewritten result
// too — there is no hidden divergence between the client and model views.
func (e *Engine) postHook(ctx context.Context, r *Run, sess *session.Session, turnIdx int, c session.ToolCall, res session.ToolResult) session.ToolResult {
	if e.deps.Hooks == nil || ctx.Err() != nil {
		return res
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
	if err != nil {
		// Best-effort: a PostToolUse execution error never aborts and never alters
		// the result.
		return res
	}
	if outcome.Block && outcome.Message != "" {
		// PostToolUse can't veto an already-run tool, but a Block message is the
		// hook flagging the output — surface it as a blocked-severity notice so it
		// reads distinctly from a benign annotation.
		e.emit(r, session.Event{Type: session.EvHook, Turn: turnIdx, Text: outcome.Message,
			Hook: &session.HookPayload{Phase: string(governance.PhasePostToolUse), Tool: c.Name, Decision: session.HookBlocked, CallID: c.ID}})
	}
	if len(outcome.Mutated) > 0 {
		// Apply the mutation: decode the same {"content", "is_error"} shape and
		// rebuild the result via the value-object constructors, preserving CallID.
		if json.Valid(outcome.Mutated) {
			var p resultPayload
			if jerr := json.Unmarshal(outcome.Mutated, &p); jerr == nil {
				e.emit(r, session.Event{Type: session.EvHook, Turn: turnIdx, Text: "PostToolUse hook rewrote the tool result for " + c.Name,
					Hook: &session.HookPayload{Phase: string(governance.PhasePostToolUse), Tool: c.Name, Decision: session.HookModified, CallID: c.ID}})
				if p.IsError {
					return session.NewToolError(res.CallID, p.Content)
				}
				return session.NewToolResult(res.CallID, p.Content)
			}
		}
		e.emit(r, session.Event{Type: session.EvHook, Turn: turnIdx, Text: "PostToolUse hook returned a malformed result mutation (ignored)",
			Hook: &session.HookPayload{Phase: string(governance.PhasePostToolUse), Tool: c.Name, Decision: session.HookInfo, CallID: c.ID}})
	}
	return res
}

// openCard emits the EvToolCall "open card" event for a call. It is called BEFORE
// the permission/hook gate (not inside execute) so the card exists before any
// synthesized failure — a permission-deny result or a PreToolUse veto — is emitted
// against its id; otherwise a client (e.g. the ACP adapter) would receive a
// failed/error update for a tool_call it never opened and could silently drop it.
//
// The card carries the ORIGINAL call args as received. A PreToolUse hook that
// rewrites the args emits its own HookModified notice; the card is not re-opened
// with the rewritten args (accepted tradeoff: the original args are shown, the
// modified-notice flags the rewrite).
func (e *Engine) openCard(r *Run, turnIdx int, c session.ToolCall) {
	call := c
	e.emit(r, session.Event{Type: session.EvToolCall, Turn: turnIdx, ToolCall: &call})
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
