package agent

import (
	"context"
	"encoding/json"
	"time"

	"github.com/stacklok/mecatl/internal/governance"
	"github.com/stacklok/mecatl/internal/session"
)

// This file wires the run-lifecycle hook phases SessionStart, UserPromptSubmit,
// and Stop into the loop via the existing port.HookRunner seam (Deps.Hooks). The
// per-tool phases (PreToolUse/PostToolUse) live in dispatch.go; SubagentStop
// lives in subagent.go.
//
// A nil Deps.Hooks (the default when no hooks are configured) makes every fire
// site a clean no-op. The hookexec adapter likewise treats an empty phase map as
// "allow", so firing these phases with no configured command never blocks.

// fireSessionStart fires the SessionStart phase once at the start of a run. It is
// informational: a Block outcome is surfaced as a hook Event for observability
// but does NOT abort the run (SessionStart is advisory by design).
func (e *Engine) fireSessionStart(ctx context.Context, r *Run, sess *session.Session) {
	if e.deps.Hooks == nil || ctx.Err() != nil {
		return
	}
	ev := governance.HookEvent{
		Phase:     governance.PhaseSessionStart,
		SessionID: string(sess.ID),
	}
	outcome, err := e.deps.Hooks.Run(ctx, ev)
	if err == nil && outcome.Block && outcome.Message != "" {
		e.emit(r, session.Event{Type: session.EvHook, Text: outcome.Message})
	}
}

// fireUserPromptSubmit fires the UserPromptSubmit phase after the prompt has been
// recorded but before the first model call. This is a BLOCKING phase: a Block
// outcome (hookexec exit 2) rejects the prompt, and the caller terminates the run
// without ever calling the model. It returns blocked=true with the rejection
// reason in that case.
//
// Seam note: a hook MAY return a mutated payload to rewrite the prompt text
// (HookOutcome.Mutated). v1 does not apply the mutation to the already-recorded
// message; the mutation is detected and surfaced as a hook Event so the seam is
// observable, leaving in-place prompt rewriting to a future loop change.
func (e *Engine) fireUserPromptSubmit(ctx context.Context, r *Run, sess *session.Session, userText string) (blocked bool, reason string) {
	if e.deps.Hooks == nil {
		return false, ""
	}
	if ctx.Err() != nil {
		return false, ""
	}
	input, _ := json.Marshal(struct {
		Prompt string `json:"prompt"`
	}{Prompt: userText})
	ev := governance.HookEvent{
		Phase:     governance.PhaseUserPromptSubmit,
		Input:     input,
		SessionID: string(sess.ID),
	}
	outcome, err := e.deps.Hooks.Run(ctx, ev)
	if err != nil {
		// A hook execution fault rejects the prompt: the run cannot proceed past a
		// vetoing phase whose verdict is unknown.
		return true, "UserPromptSubmit hook error: " + err.Error()
	}
	if outcome.Block {
		msg := outcome.Message
		if msg == "" {
			msg = "prompt blocked by UserPromptSubmit hook"
		}
		e.emit(r, session.Event{Type: session.EvHook, Text: msg})
		return true, msg
	}
	if len(outcome.Mutated) > 0 {
		// Mutation seam: a future change applies this to the recorded prompt.
		e.emit(r, session.Event{Type: session.EvHook, Text: "UserPromptSubmit hook proposed a prompt mutation (not applied in v1)"})
	}
	return false, ""
}

// fireStop fires the Stop phase at the terminal end of a run (any terminal path:
// complete / stop-condition / error / cancel), after the result is determined. It
// is informational and best-effort: a Block outcome cannot veto an already-ended
// run (mirroring SubagentStop). It is invoked exactly once per run from the
// terminate/terminateComplete paths.
func (e *Engine) fireStop(ctx context.Context, r *Run, sess *session.Session, reason session.StopReason) {
	if e.deps.Hooks == nil {
		return
	}
	// Stop is a terminal notification: run it even if ctx is already cancelled,
	// using a detached, short-lived context so a cancelled run still notifies.
	hookCtx := ctx
	if ctx.Err() != nil {
		var cancel context.CancelFunc
		hookCtx, cancel = context.WithTimeout(context.WithoutCancel(ctx), 5*time.Second)
		defer cancel()
	}
	input, _ := json.Marshal(struct {
		Stop string `json:"stop_reason"`
	}{Stop: string(reason)})
	ev := governance.HookEvent{
		Phase:     governance.PhaseStop,
		Input:     input,
		SessionID: string(sess.ID),
	}
	outcome, err := e.deps.Hooks.Run(hookCtx, ev)
	if err == nil && outcome.Block && outcome.Message != "" {
		e.emit(r, session.Event{Type: session.EvHook, Text: outcome.Message})
	}
}
