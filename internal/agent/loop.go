// Package agent is the use-case heart of ozzharness: the streaming agent loop
// that ties the ports together. It records the user prompt, calls the
// LLMProvider, streams assistant deltas, dispatches tool calls (read-parallel /
// mutate-serial) through the permission policy and hook lifecycle, pauses on
// permission asks, compacts the history at the context-window threshold, and
// emits a single ordered stream of session.Events terminating in a result.
//
// Import rule: this package imports ONLY internal/session, internal/port,
// internal/tool, internal/governance, internal/prompt, and the standard library.
// Adapters are injected as ports; the loop never names a concrete adapter or the
// api layer. (Tests may import adapters.)
package agent

import (
	"context"
	"errors"
	"fmt"
	"sync/atomic"

	"github.com/stacklok/ozzharness/internal/port"
	"github.com/stacklok/ozzharness/internal/prompt"
	"github.com/stacklok/ozzharness/internal/session"
	"github.com/stacklok/ozzharness/internal/tool"
)

// defaultCompactionRatio is the fraction of the context window at which the loop
// triggers compaction when Deps.CompactionRatio is unset.
const defaultCompactionRatio = 0.8

// charsPerToken is the crude bytes→tokens estimate the loop uses to decide when
// to compact. It is intentionally coarse: the threshold only needs to be in the
// right ballpark, and an offline heuristic must not call a tokenizer service.
const charsPerToken = 4

// Deps are the injected ports and configuration a single Engine is built from.
// Every field is a port (an interface) or plain config, so the agent package
// never depends on a concrete adapter. The composition root wires real or fake
// adapters in.
type Deps struct {
	// LLM is the model-call seam.
	LLM port.LLMProvider
	// Catalog is the tool registry; the loop reads Specs(mode) and Lookup(name).
	Catalog *tool.Catalog
	// Policy evaluates each tool call (deny → ask → allow).
	Policy port.PermissionPolicy
	// Hooks runs the PreToolUse / PostToolUse lifecycle hooks.
	Hooks port.HookRunner
	// Store persists session state (optional; nil disables persistence).
	Store port.SessionStore
	// Clock supplies wall time for tool-call timing (optional; nil → no timing).
	Clock port.Clock
	// Logger records tool-execution observability (optional; nil → no logging).
	Logger port.Logger
	// Sink, when non-nil, also receives every Event the loop emits, in addition
	// to the Run.Events() channel which is always the primary surface.
	Sink port.EventSink
	// Compactor compresses history at the threshold; nil → HeuristicCompactor.
	Compactor Compactor
	// Instructions assembles the project-instruction messages recorded once at
	// the start of a run; nil → prompt.RootAssembler (root-only AGENTS.md /
	// CLAUDE.md, the v1 default).
	Instructions prompt.InstructionAssembler
	// PromptConfig seeds the cache-stable system prompt (role/tone/safety). The
	// loop fills in Tools and the volatile Env per turn.
	PromptConfig prompt.Config

	// Model is the provider model identifier sent on every request.
	Model string
	// ContextWindowTokens is the model's context window; compaction triggers at
	// CompactionRatio of it. Zero disables compaction.
	ContextWindowTokens int
	// CompactionRatio overrides defaultCompactionRatio when in (0,1].
	CompactionRatio float64

	// ProgressiveTools, when true, enables progressive tool disclosure
	// (pattern 9): the per-turn request advertises lightweight specs for tools
	// implementing tool.Disclosable plus a built-in ToolSearch tool the model
	// uses to hydrate a full spec on demand. The DEFAULT (false) sends every
	// tool's full spec every turn, exactly as v1 does. The ToolSearch tool is
	// registered into the catalog by NewEngine only when this is enabled.
	ProgressiveTools bool
}

// Engine builds Runs from a fixed set of ports. It is safe for concurrent use:
// each Run owns its own goroutine and state, and the injected ports are expected
// to be concurrency-safe (the provided adapters are). One Engine typically backs
// the whole process; the API layer (WP10) calls Run per prompt.
type Engine struct {
	deps Deps
}

// NewEngine constructs an Engine from deps, applying defaults for the optional
// Compactor and CompactionRatio.
func NewEngine(deps Deps) *Engine {
	if deps.Compactor == nil {
		deps.Compactor = HeuristicCompactor{}
	}
	if deps.CompactionRatio <= 0 || deps.CompactionRatio > 1 {
		deps.CompactionRatio = defaultCompactionRatio
	}
	if deps.Instructions == nil {
		deps.Instructions = prompt.RootAssembler{}
	}
	// Progressive disclosure: register the ToolSearch hydration tool so the model
	// can fetch a full spec on demand. It is registered only when enabled and only
	// if a Catalog is present and does not already carry one (idempotent).
	if deps.ProgressiveTools && deps.Catalog != nil {
		if _, ok := deps.Catalog.Lookup(tool.ToolSearchName); !ok {
			_ = deps.Catalog.Register(tool.NewToolSearch(deps.Catalog))
		}
	}
	return &Engine{deps: deps}
}

// Run is the handle to one in-flight prompt. It exposes the Event stream plus the
// out-of-band controls the bidi API needs (Approve resolves a permission.ask;
// Cancel aborts the run). The Events channel is closed exactly once, when the run
// terminates.
type Run struct {
	events chan session.Event
	asks   *askRegistry
	cancel context.CancelFunc
	seq    atomic.Int64
}

// Events returns the channel of domain Events for this run. It is closed when the
// run ends (after the terminal result Event has been delivered).
func (r *Run) Events() <-chan session.Event { return r.events }

// Approve resolves the permission.ask identified by askID with the client's
// verdict (allow true permits the tool, false denies it). It is non-blocking and
// safe to call from another goroutine; an unknown or already-resolved askID is
// ignored.
func (r *Run) Approve(askID string, allow bool) { r.asks.resolve(askID, allow) }

// Cancel aborts the in-flight run by cancelling its context. The loop observes
// the cancellation (mid-stream, mid-tool, or while awaiting an approval) and
// terminates with a result carrying StopCancelled.
func (r *Run) Cancel() { r.cancel() }

// Run starts processing userText against sess in a background goroutine and
// returns immediately with a Run handle. The loop runs until it produces a
// terminal result Event, then closes the Events channel. ws is the session-scoped
// workspace tools execute against.
func (e *Engine) Run(ctx context.Context, sess *session.Session, ws tool.Workspace, userText string) *Run {
	ctx, cancel := context.WithCancel(ctx)
	r := &Run{
		events: make(chan session.Event, 64),
		asks:   newAskRegistry(),
		cancel: cancel,
	}
	go func() {
		defer close(r.events)
		defer cancel()
		e.drive(ctx, r, sess, ws, userText)
	}()
	return r
}

// drive runs the loop algorithm for one prompt. It always terminates the session
// (Complete/Stop/Cancel/Fail) and emits exactly one terminal result Event.
func (e *Engine) drive(ctx context.Context, r *Run, sess *session.Session, ws tool.Workspace, userText string) {
	// Step 0a: emit the run-open signal exactly once per run, before any other
	// event. Telemetry adapters (tracing/metrics) switch on session.init as the
	// signal to open a run span/counter; emitting it here makes that contract
	// honest rather than relying on their defensive fallback. It must precede the
	// SessionStart hook events and the first turn.start.
	e.emit(r, session.Event{Type: session.EvSessionInit})

	// Step 0b: fire SessionStart once before any work. Informational: a Block
	// outcome is logged but does NOT abort the run (the phase is advisory; only
	// PreToolUse and UserPromptSubmit are vetoing phases).
	if sess.Counters.Turns == 0 {
		e.fireSessionStart(ctx, r, sess)
	}

	// Step 1: record the user message and assemble project instructions once.
	if err := e.recordPrompt(ctx, sess, ws, userText); err != nil {
		e.terminate(ctx, r, sess, session.StopError, "", session.Usage{}, err)
		return
	}

	// Step 1b: fire UserPromptSubmit AFTER recording the prompt but BEFORE the
	// first model call. This is a BLOCKING phase: a Block outcome rejects the
	// prompt and terminates the run without ever calling the model.
	if blocked, reason := e.fireUserPromptSubmit(ctx, r, sess, userText); blocked {
		e.terminate(ctx, r, sess, session.StopError, reason, session.Usage{}, fmt.Errorf("agent: prompt rejected by UserPromptSubmit hook: %s", reason))
		return
	}

	var total session.Usage
	var lastText string

	for {
		// Step 2: stop conditions BEFORE the model call.
		if reason, stopped := sess.StopReason(); stopped {
			e.terminate(ctx, r, sess, reason, lastText, total, nil)
			return
		}
		if ctx.Err() != nil {
			e.terminate(ctx, r, sess, session.StopCancelled, lastText, total, nil)
			return
		}

		if err := sess.BeginTurn(); err != nil {
			e.terminate(ctx, r, sess, session.StopError, lastText, total, err)
			return
		}
		turnIdx := sess.Counters.Turns - 1
		e.emit(r, session.Event{Type: session.EvTurnStart, Turn: turnIdx})

		// Step 3: compaction seam (mutates history in place when it triggers).
		e.maybeCompact(ctx, r, sess, turnIdx)

		// Step 4: build the request and consume the model stream.
		asst, usage, streamStop, err := e.runTurn(ctx, r, sess, turnIdx)
		if err != nil {
			if errors.Is(err, context.Canceled) || ctx.Err() != nil {
				e.terminate(ctx, r, sess, session.StopCancelled, lastText, total, nil)
				return
			}
			e.terminate(ctx, r, sess, session.StopError, lastText, total, err)
			return
		}
		total = total.Add(usage)
		if asst.Text != "" {
			lastText = asst.Text
		}

		if err := sess.RecordAssistant(asst); err != nil {
			e.terminate(ctx, r, sess, session.StopError, lastText, total, err)
			return
		}

		// Step 5: no tool calls → the model is done.
		if len(asst.ToolCalls) == 0 {
			stop := session.StopEndTurn
			if streamStop != session.StopNone {
				stop = streamStop
			}
			e.terminateComplete(ctx, r, sess, stop, lastText, total)
			return
		}

		// Step 6: dispatch the tool calls, then loop back to step 2.
		results, cancelled := e.dispatch(ctx, r, sess, ws, turnIdx, asst.ToolCalls)
		if cancelled {
			e.terminate(ctx, r, sess, session.StopCancelled, lastText, total, nil)
			return
		}
		if err := sess.RecordToolResults(results); err != nil {
			e.terminate(ctx, r, sess, session.StopError, lastText, total, err)
			return
		}
		e.save(ctx, sess)
	}
}

// recordPrompt records the user prompt and, on the first turn, the assembled
// project instructions (via Deps.Instructions; default RootAssembler reads
// AGENTS.md / CLAUDE.md at the workspace root), routing both through the session
// root so all history mutation flows through the aggregate.
func (e *Engine) recordPrompt(ctx context.Context, sess *session.Session, ws tool.Workspace, userText string) error {
	var instr []session.Message
	if sess.Counters.Turns == 0 {
		discovered, err := e.deps.Instructions.Assemble(ctx, ws)
		if err != nil {
			return fmt.Errorf("agent: assemble instructions: %w", err)
		}
		instr = discovered
	}
	if err := sess.RecordUserPrompt(userText, instr); err != nil {
		return fmt.Errorf("agent: record user prompt: %w", err)
	}
	return nil
}

// runTurn builds the LLMRequest, calls Stream, and assembles the chunk sequence
// into a single assistant Message. It emits message.delta events for text. It
// honours ctx cancellation mid-stream by returning context.Canceled.
func (e *Engine) runTurn(ctx context.Context, r *Run, sess *session.Session, turnIdx int) (session.Message, session.Usage, session.StopReason, error) {
	req := e.buildRequest(sess)
	seq, err := e.deps.LLM.Stream(ctx, req)
	if err != nil {
		return session.Message{}, session.Usage{}, session.StopNone, fmt.Errorf("agent: start stream: %w", err)
	}

	var text, reasoning string
	var calls []session.ToolCall
	var usage session.Usage
	stop := session.StopNone

	for chunk, cerr := range seq {
		if cerr != nil {
			return session.Message{}, usage, stop, fmt.Errorf("agent: stream: %w", cerr)
		}
		if ctx.Err() != nil {
			return session.Message{}, usage, stop, context.Canceled
		}
		switch chunk.Kind {
		case port.ChunkText:
			text += chunk.Text
			e.emit(r, session.Event{Type: session.EvMessageDelta, Turn: turnIdx, Text: chunk.Text})
		case port.ChunkReasoning:
			reasoning += chunk.Text
		case port.ChunkToolCall:
			if chunk.ToolCall != nil {
				calls = append(calls, *chunk.ToolCall)
			}
		case port.ChunkUsage:
			if chunk.Usage != nil {
				usage = usage.Add(*chunk.Usage)
			}
		case port.ChunkDone:
			stop = chunk.Stop
		}
	}

	// A consumer that exited because ctx was cancelled mid-stream surfaces as a
	// cancellation, not a normal end-of-turn.
	if ctx.Err() != nil {
		return session.Message{}, usage, stop, context.Canceled
	}

	return session.NewAssistantMessage(text, reasoning, calls), usage, stop, nil
}

// buildRequest assembles the provider-neutral LLMRequest for the current turn:
// the layered system prompt (cache-stable prefix + volatile env suffix), the
// conversation history, and the mode-filtered tool specs.
func (e *Engine) buildRequest(sess *session.Session) port.LLMRequest {
	cfg := e.deps.PromptConfig
	// Progressive disclosure (pattern 9): when enabled, advertise lightweight
	// specs (full spec for non-disclosable tools, including the ToolSearch tool
	// registered by NewEngine) so the model hydrates schemas on demand. When OFF
	// (the default) send every tool's full spec exactly as v1 does.
	if e.deps.ProgressiveTools {
		cfg.Tools = e.deps.Catalog.AdvertisedSpecs(sess.Mode)
	} else {
		cfg.Tools = e.deps.Catalog.Specs(sess.Mode)
	}
	cfg.Env.Model = e.deps.Model
	cfg.Env.Mode = string(sess.Mode)
	if cfg.Env.Cwd == "" {
		cfg.Env.Cwd = sess.Workspace
	}
	return port.LLMRequest{
		System:   prompt.Build(cfg),
		Messages: sess.Conversation.Messages,
		Tools:    cfg.Tools,
		Model:    e.deps.Model,
	}
}

// maybeCompact runs the Compactor when the estimated history token count crosses
// the threshold. It replaces the conversation in place and emits a compaction
// Event. It reports whether compaction ran.
func (e *Engine) maybeCompact(ctx context.Context, r *Run, sess *session.Session, turnIdx int) bool {
	if e.deps.ContextWindowTokens <= 0 {
		return false
	}
	threshold := int(float64(e.deps.ContextWindowTokens) * e.deps.CompactionRatio)
	if estimateTokens(sess.Conversation) < threshold {
		return false
	}
	compacted, summary, err := e.deps.Compactor.Compact(ctx, sess.Conversation)
	if err != nil {
		// Compaction is best-effort: a failure must not abort the run. Keep the
		// existing history and continue.
		return false
	}
	if err := sess.ReplaceHistory(compacted); err != nil {
		// Replacement is legal only while running; if the seam rejects it, keep the
		// existing history rather than aborting the run.
		return false
	}
	e.emit(r, session.Event{Type: session.EvCompaction, Turn: turnIdx, Text: summary})
	return true
}

// estimateTokens is the loop's coarse, offline history-size estimate, summing the
// text/reasoning/tool bodies and dividing by charsPerToken. It never calls out.
func estimateTokens(conv *session.Conversation) int {
	chars := 0
	for _, m := range conv.Messages {
		chars += len(m.Text) + len(m.Reasoning)
		for _, c := range m.ToolCalls {
			chars += len(c.Name) + len(c.Args)
		}
		if m.ToolResult != nil {
			chars += len(m.ToolResult.Content)
		}
	}
	return chars / charsPerToken
}

// emit assigns the next monotonic Seq, publishes the event on the Run channel,
// and returns the sequenced event (so callers can mirror it to a secondary sink).
func (r *Run) emit(ev session.Event) session.Event {
	ev.Seq = r.seq.Add(1)
	r.events <- ev
	return ev
}

// terminate ends the run with a non-success terminal state. It moves the session
// to the matching terminal state (Cancel for cancelled, Fail for error, Stop for
// a tripped limit) and emits the terminal result Event.
func (e *Engine) terminate(ctx context.Context, r *Run, sess *session.Session, reason session.StopReason, text string, usage session.Usage, cause error) {
	switch reason {
	case session.StopCancelled:
		_ = sess.Cancel()
	case session.StopError:
		_ = sess.Fail()
	default:
		if !sess.State.IsTerminal() {
			_ = sess.Stop(reason)
		}
	}
	var errMsg string
	if cause != nil {
		errMsg = cause.Error()
	}
	e.fireStop(ctx, r, sess, reason)
	e.emitResult(r, sess, reason, text, usage, errMsg)
	e.save(ctx, sess)
}

// terminateComplete ends the run successfully (the model finished its turn),
// recording the explicit stop reason and emitting the result Event.
func (e *Engine) terminateComplete(ctx context.Context, r *Run, sess *session.Session, reason session.StopReason, text string, usage session.Usage) {
	if !sess.State.IsTerminal() {
		_ = sess.Stop(reason)
	}
	e.fireStop(ctx, r, sess, reason)
	e.emitResult(r, sess, reason, text, usage, "")
	e.save(ctx, sess)
}

// emitResult publishes the single terminal result Event. errMsg carries the
// failure detail on an error termination (empty for success/limit/cancel).
func (e *Engine) emitResult(r *Run, _ *session.Session, reason session.StopReason, text string, usage session.Usage, errMsg string) {
	u := usage
	e.emit(r, session.Event{
		Type: session.EvResult,
		Result: &session.ResultPayload{
			Stop:  reason,
			Text:  text,
			Usage: usage,
			Error: errMsg,
		},
		Usage: &u,
	})
}

// save best-effort persists the session if a Store is configured.
func (e *Engine) save(ctx context.Context, sess *session.Session) {
	if e.deps.Store == nil {
		return
	}
	_ = e.deps.Store.Save(ctx, sess)
}
