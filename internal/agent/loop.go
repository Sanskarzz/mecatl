// Package agent is the use-case heart of mecatl: the streaming agent loop
// that ties the ports together. It records the user prompt, calls the
// LLMProvider, streams assistant deltas, dispatches tool calls (read-parallel /
// mutate-serial) through the permission policy and hook lifecycle, pauses on
// permission asks, compacts the history at the context-window threshold, and
// emits a single ordered stream of session.Events terminating in a result.
//
// Import rule: this package imports ONLY internal/session, internal/port,
// internal/tool, internal/governance, internal/prompt, internal/team (the domain
// coordination substrate for agent teams), and the standard library. Adapters are
// injected as ports; the loop never names a concrete adapter or the api layer.
// (Tests may import adapters.)
package agent

import (
	"context"
	"errors"
	"fmt"
	"sync/atomic"
	"time"

	"github.com/stacklok/mecatl/internal/port"
	"github.com/stacklok/mecatl/internal/prompt"
	"github.com/stacklok/mecatl/internal/session"
	"github.com/stacklok/mecatl/internal/tool"
)

// defaultCompactionRatio is the fraction of the context window at which the loop
// triggers compaction when Deps.CompactionRatio is unset.
const defaultCompactionRatio = 0.8

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
	// ToolCallRecorder records tool-execution observability — the tool-call audit
	// seam (optional; nil → no recording).
	ToolCallRecorder port.ToolCallRecorder
	// Sink, when non-nil, also receives every Event the loop emits, in addition
	// to the Run.Events() channel which is always the primary surface.
	Sink port.EventSink
	// Diagnostics is the general-purpose operational logging seam (optional; nil →
	// port.NopDiagnostics, applied in NewEngine, so the engine never nil-panics and
	// stays silent when no sink is injected). It is DISTINCT from ToolCallRecorder
	// (the per-tool audit seam) and Sink (the model's conversation stream).
	Diagnostics port.Diagnostics
	// Compactor compresses history at the threshold; nil → HeuristicCompactor.
	Compactor Compactor
	// TokenCounter estimates history size for the compaction trigger (and is
	// shared with the Compactor); nil → HeuristicTokenCounter.
	TokenCounter TokenCounter
	// Instructions assembles the project-instruction messages recorded once at
	// the start of a run; nil → prompt.RootAssembler (root-only AGENTS.md /
	// CLAUDE.md, the v1 default).
	Instructions prompt.InstructionAssembler
	// CommandExpander rewrites a raw user prompt into the text the model sees,
	// expanding slash-command invocations (e.g. "/review foo.go") against the
	// workspace before the prompt is recorded; nil → prompt.NoopExpander (no
	// expansion, the v1 default).
	CommandExpander prompt.CommandExpander
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

	// Role is the operator-facing label this engine logs under in diagnostics: the
	// empty string for the MAIN engine (correlated by session only), or a non-empty
	// role for a child/subagent engine (e.g. "task" for the Task subagent, a team
	// member's name, or a fork-branch label) so interleaved child diagnostics are
	// readable. It is read once per run when binding the run-scoped Diagnostics (see
	// drive): empty → only the "session" key; set → "session"+"agent" keys. It is
	// NOT plumbed into Sink/ToolCallRecorder (those stay off for children) and never
	// reaches the model.
	Role string

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
	if deps.TokenCounter == nil {
		deps.TokenCounter = HeuristicTokenCounter{}
	}
	if deps.Compactor == nil {
		deps.Compactor = HeuristicCompactor{}
	}
	if deps.CompactionRatio <= 0 || deps.CompactionRatio > 1 {
		deps.CompactionRatio = defaultCompactionRatio
	}
	if deps.Instructions == nil {
		deps.Instructions = prompt.RootAssembler{}
	}
	if deps.CommandExpander == nil {
		deps.CommandExpander = prompt.NoopExpander{}
	}
	if deps.Diagnostics == nil {
		deps.Diagnostics = port.NopDiagnostics{}
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

// Capabilities reports the multimodal input capabilities of the Engine's LLM
// provider, so a surface adapter can advertise them and gate unsupported prompt
// content. It is a pure pass-through to the injected provider.
func (e *Engine) Capabilities() port.ProviderCapabilities {
	return e.deps.LLM.Capabilities()
}

// ContextWindow reports the model's context window in tokens (Deps.ContextWindowTokens),
// or 0 when unknown/unset. The team supervisor reads it from each member's engine
// so a forwarded turn.end can carry the denominator for the per-member context
// meter in the ctrl+a agents overlay (the window lives in private deps).
func (e *Engine) ContextWindow() int { return e.deps.ContextWindowTokens }

// HasTool reports whether a tool with the given registered name is present in
// the Engine's catalog. It is the read-only seam a surface adapter uses to
// report capabilities (e.g. memory/skills/bash availability) from the BUILT
// catalog rather than a static config flag, so the report can never claim a
// feature the engine did not register. It is nil-safe: a nil catalog yields
// false. It exposes only presence, never the concrete tool, keeping the agent
// package free of any adapter dependency.
func (e *Engine) HasTool(name string) bool {
	if e.deps.Catalog == nil {
		return false
	}
	_, ok := e.deps.Catalog.Lookup(name)
	return ok
}

// catalogToolInfo is a (name, read-only) summary of one tool in an Engine's
// catalog. The supervisor uses it to verify a member's tool set without
// type-asserting concrete tool types (which would require importing an adapter,
// breaking the layering rule).
type catalogToolInfo struct {
	name     string
	readOnly bool
}

// catalogTools returns a (name, read-only) summary of every tool in the Engine's
// catalog, or nil if the Engine carries no catalog. It is the read-only seam the
// Supervisor uses to enforce the read-only-member / workspace-mutating-tool
// invariant (see Supervisor.AddMember). It deliberately exposes only the
// name+ReadOnly bits, never the concrete tools, keeping the agent package free of
// any adapter dependency.
func (e *Engine) catalogTools() []catalogToolInfo {
	if e.deps.Catalog == nil {
		return nil
	}
	tools := e.deps.Catalog.Tools()
	out := make([]catalogToolInfo, 0, len(tools))
	for _, t := range tools {
		out = append(out, catalogToolInfo{name: t.Spec().Name, readOnly: t.ReadOnly()})
	}
	return out
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
	// ctx is the run's context, captured at RunContent. Engine.emit forwards it
	// to the injected EventSink so telemetry adapters can read a trace span from
	// it and correlate spans/metrics to the originating request. Each run (including
	// child/subagent/fork runs, which enter through their own Run/RunContent call)
	// captures its OWN ctx, so a child's emits correlate to the child, not the
	// parent. It is set once before the run goroutine starts and only read after,
	// so it needs no synchronisation.
	ctx context.Context //nolint:containedctx // run-scoped carrier forwarded to the EventSink; never the request's own field
	// diag is the run-scoped operational-logging seam: deps.Diagnostics bound to
	// this run's session id (and, for a child engine, its agent role) via With, so
	// every line emitted through it carries the correlation keys. It is bound ONCE
	// per run in RunContent — NOT at engine construction, because the engine is
	// built before the session id exists and is often SHARED across sessions. It is
	// Nop-safe: deps.Diagnostics is never nil post-NewEngine, and With on
	// NopDiagnostics returns NopDiagnostics. It is set before the run goroutine
	// starts and only read after, so it needs no synchronisation.
	diag port.Diagnostics
}

// Events returns the channel of domain Events for this run. It is closed when the
// run ends (after the terminal result Event has been delivered).
func (r *Run) Events() <-chan session.Event { return r.events }

// Approve resolves the permission.ask identified by askID with the client's
// verdict: VerdictDeny refuses the call, VerdictAllowOnce permits this call only,
// and VerdictAllowAlways permits it AND asks the policy to learn a per-session
// allow rule for the matching tool+pattern. It is non-blocking and safe to call
// from another goroutine; an unknown or already-resolved askID is ignored.
func (r *Run) Approve(askID string, v session.ApprovalVerdict) { r.asks.resolve(askID, v) }

// Cancel aborts the in-flight run by cancelling its context. The loop observes
// the cancellation (mid-stream, mid-tool, or while awaiting an approval) and
// terminates with a result carrying StopCancelled.
func (r *Run) Cancel() { r.cancel() }

// Run starts processing userText against sess in a background goroutine and
// returns immediately with a Run handle. The loop runs until it produces a
// terminal result Event, then closes the Events channel. ws is the session-scoped
// workspace tools execute against. It is the text-only entry; for a multimodal
// prompt use RunContent.
func (e *Engine) Run(ctx context.Context, sess *session.Session, ws tool.Workspace, userText string) *Run {
	return e.RunContent(ctx, sess, ws, userText, nil)
}

// RunContent is the multimodal sibling of Run: it processes userText PLUS
// non-text media parts (image/audio) against sess. userText may be empty when
// parts carries the content. Command expansion and the UserPromptSubmit hook
// operate on the TEXT only; the media parts pass through untouched and are
// recorded verbatim on the user message. Run delegates here with nil parts.
func (e *Engine) RunContent(ctx context.Context, sess *session.Session, ws tool.Workspace, userText string, parts []session.Content) *Run {
	ctx, cancel := context.WithCancel(ctx)
	r := &Run{
		events: make(chan session.Event, 64),
		asks:   newAskRegistry(),
		cancel: cancel,
		ctx:    ctx,
		// Bind the run-scoped diagnostics ONCE here, where the live session is in
		// scope: correlate every emitted line to this session id, and (for a child
		// engine, Role != "") to its agent role too. The main engine has Role=="" so
		// only the "session" key is bound. With on NopDiagnostics returns Nop, so an
		// engine with no injected sink stays silent.
		diag: e.bindRunDiag(sess.ID),
	}
	go func() {
		defer close(r.events)
		defer cancel()
		e.drive(ctx, r, sess, ws, userText, parts)
	}()
	return r
}

// drive runs the loop algorithm for one prompt. It always terminates the session
// (Complete/Stop/Cancel/Fail) and emits exactly one terminal result Event. parts
// carries any non-text media riding alongside userText (nil for a text prompt).
func (e *Engine) drive(ctx context.Context, r *Run, sess *session.Session, ws tool.Workspace, userText string, parts []session.Content) {
	// Step 0a: emit the run-open signal exactly once per run, before any other
	// event. Telemetry adapters (tracing/metrics) switch on session.init as the
	// signal to open a run span/counter; emitting it here makes that contract
	// honest rather than relying on their defensive fallback. It must precede the
	// SessionStart hook events and the first turn.start.
	e.emit(r, session.Event{Type: session.EvSessionInit})

	// Step 0b: fire SessionStart once before any work, before the prompt is even
	// recorded. This is a BLOCKING run-level gate (symmetric with
	// UserPromptSubmit): a Block outcome (or a hook execution error) aborts the
	// run before any prompt processing or model call.
	if sess.Counters.Turns == 0 {
		if blocked, reason := e.fireSessionStart(ctx, r, sess); blocked {
			e.terminate(ctx, r, sess, session.StopError, reason, session.Usage{}, fmt.Errorf("agent: session rejected by SessionStart hook: %s", reason))
			return
		}
	}

	// Step 1: record the user message and assemble project instructions once.
	// recordPrompt expands the prompt, fires the BLOCKING UserPromptSubmit phase
	// (applying any mutation to the effective prompt), and only then records the
	// final text into the aggregate. A Block (or hook error) ends the run before
	// any model call; ok=false signals that without recording anything.
	ok, reason, err := e.recordPrompt(ctx, r, sess, ws, userText, parts)
	if err != nil {
		e.terminate(ctx, r, sess, session.StopError, "", session.Usage{}, err)
		return
	}
	if !ok {
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

		// Snapshot the turn start for the turn.end elapsed measurement. When no
		// Clock is injected the duration is reported as 0 (guarded like dispatch).
		var turnStart time.Time
		if e.deps.Clock != nil {
			turnStart = e.deps.Clock.Now()
		}

		// Step 3: compaction seam (mutates history in place when it triggers).
		e.maybeCompact(ctx, r, sess, turnIdx)

		// Step 4: build the request and consume the model stream.
		asst, usage, streamStop, timing, err := e.runTurn(ctx, r, sess, turnIdx)
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

		// Close the turn's model exchange with its own usage and elapsed time in a
		// typed TurnEndPayload. Emitted only on the success path (never on the
		// error/cancel returns above), before the assistant message is recorded.
		// usage here is THIS turn's accounting; Event.Usage is left unset so it
		// keeps its single cumulative-on-result meaning.
		var durMs int64
		if e.deps.Clock != nil {
			durMs = e.deps.Clock.Now().Sub(turnStart).Milliseconds()
		}
		e.emit(r, session.Event{Type: session.EvTurnEnd, Turn: turnIdx,
			TurnEnd: &session.TurnEndPayload{
				Usage:            usage,
				DurationMs:       durMs,
				TTFTMs:           timing.ttftMs,
				InterTokenMeanMs: timing.interTokenMeanMs,
				InterTokenMaxMs:  timing.interTokenMaxMs,
			}})

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

// recordPrompt produces the effective user prompt and records it (with, on the
// first turn, the assembled project instructions via Deps.Instructions; default
// RootAssembler reads AGENTS.md / CLAUDE.md at the workspace root) through the
// session root so all history mutation flows through the aggregate.
//
// Ordering is load-bearing:
//  1. Deps.CommandExpander expands a slash-command invocation into its template
//     body; the EXPANDED text is the candidate prompt. The default NoopExpander
//     leaves the text unchanged (v1 behaviour). Expansion is best-effort: a read
//     fault is treated as unchanged rather than aborting the run.
//  2. The BLOCKING UserPromptSubmit hook fires on that expanded text, BEFORE the
//     prompt is recorded. A Block (or hook error) returns ok=false with a reason
//     and records nothing — the caller ends the run before any model call. A
//     non-empty Mutated payload REPLACES the effective prompt text.
//  3. The final (possibly mutated) text is recorded via RecordUserPrompt, so it
//     is exactly what the model receives.
//
// ok=false means the prompt was rejected (reason is set); a non-nil error means a
// recording/assembly fault. Both paths leave the run to terminate.
// parts (non-text media) ride alongside the text untouched: command expansion
// and the UserPromptSubmit hook see and may rewrite only the TEXT; the media is
// neither expanded nor mutated by a hook and is recorded verbatim on the user
// message via RecordUserPromptWithParts.
func (e *Engine) recordPrompt(ctx context.Context, r *Run, sess *session.Session, ws tool.Workspace, userText string, parts []session.Content) (ok bool, reason string, err error) {
	if expanded, exp, eerr := e.deps.CommandExpander.Expand(ctx, ws, userText); eerr == nil && exp {
		userText = expanded
	}
	// UserPromptSubmit fires on the expanded text before recording so a mutation
	// can rewrite the effective prompt and a block can reject it pre-record.
	finalText, blocked, reason := e.fireUserPromptSubmit(ctx, r, sess, userText)
	if blocked {
		return false, reason, nil
	}
	var instr []session.Message
	if sess.Counters.Turns == 0 {
		discovered, aerr := e.deps.Instructions.Assemble(ctx, ws)
		if aerr != nil {
			return false, "", fmt.Errorf("agent: assemble instructions: %w", aerr)
		}
		instr = discovered
	}
	if rerr := sess.RecordUserPromptWithParts(finalText, parts, instr); rerr != nil {
		return false, "", fmt.Errorf("agent: record user prompt: %w", rerr)
	}
	return true, "", nil
}

// turnTiming carries the latency measurements runTurn derives from the model
// stream, in milliseconds. A field is 0 when it could not be measured (no Clock
// injected, no content chunk for TTFT, or fewer than two content chunks for the
// inter-token summary) — telemetry treats a 0 here as "not measured", never a
// real observation.
type turnTiming struct {
	ttftMs           int64
	interTokenMeanMs int64
	interTokenMaxMs  int64
}

// runTurn builds the LLMRequest, calls Stream, and assembles the chunk sequence
// into a single assistant Message. It emits message.delta events for text. It
// honours ctx cancellation mid-stream by returning context.Canceled.
//
// It also measures, via the injected Clock (never time.Now directly, so tests
// drive it with a fake clock): TTFT — the elapsed time from the start of the
// model stream to the FIRST content chunk (text or reasoning) — and the
// inter-token gaps between consecutive content chunks, summarised as mean and
// max. Tool-call/usage/done chunks are NOT content and never count toward TTFT or
// the inter-token gaps (those carry no user-perceived token). A turn with zero
// content chunks reports no TTFT; a turn with one content chunk reports a TTFT
// but no inter-token summary (there is no gap).
func (e *Engine) runTurn(ctx context.Context, r *Run, sess *session.Session, turnIdx int) (session.Message, session.Usage, session.StopReason, turnTiming, error) {
	req := e.buildRequest(sess)
	seq, err := e.deps.LLM.Stream(ctx, req)
	if err != nil {
		return session.Message{}, session.Usage{}, session.StopNone, turnTiming{}, fmt.Errorf("agent: start stream: %w", err)
	}

	// Latency measurement state. streamStart anchors TTFT; lastContent marks the
	// previous content chunk's arrival for the inter-token gaps. Both are taken
	// from the injected Clock; when no Clock is configured, hasClock is false and
	// every measurement degrades to 0 (matching the turn-duration guard).
	hasClock := e.deps.Clock != nil
	var streamStart, lastContent time.Time
	if hasClock {
		streamStart = e.deps.Clock.Now()
	}
	var (
		timing             turnTiming
		contentChunks      int
		gapSumMs, gapMaxMs int64
	)
	// noteContent records the arrival of one content chunk (text or reasoning) for
	// the TTFT / inter-token measurement. It must be called exactly once per
	// content chunk, before the chunk is otherwise handled.
	noteContent := func() {
		if !hasClock {
			return
		}
		now := e.deps.Clock.Now()
		contentChunks++
		if contentChunks == 1 {
			timing.ttftMs = now.Sub(streamStart).Milliseconds()
		} else {
			gap := now.Sub(lastContent).Milliseconds()
			gapSumMs += gap
			if gap > gapMaxMs {
				gapMaxMs = gap
			}
		}
		lastContent = now
	}

	// text is the visible assistant text. reasoningBlob is the opaque
	// encrypted_content replayed back to the provider on subsequent turns (stored
	// on Message.Reasoning); it is distinct from the human-readable reasoning
	// summary, which only drives display-only reasoning.delta events.
	var text, reasoningBlob string
	var calls []session.ToolCall
	var usage session.Usage
	stop := session.StopNone

	for chunk, cerr := range seq {
		if cerr != nil {
			return session.Message{}, usage, stop, turnTiming{}, fmt.Errorf("agent: stream: %w", cerr)
		}
		if ctx.Err() != nil {
			return session.Message{}, usage, stop, turnTiming{}, context.Canceled
		}
		switch chunk.Kind {
		case port.ChunkText:
			noteContent()
			text += chunk.Text
			e.emit(r, session.Event{Type: session.EvMessageDelta, Turn: turnIdx, Text: chunk.Text})
		case port.ChunkReasoning:
			// Display-only: surface the human-readable reasoning summary to
			// clients. This text is NOT what gets replayed to the provider (see
			// ChunkReasoningItem below); it must not be stored on Message.Reasoning.
			noteContent()
			e.emit(r, session.Event{Type: session.EvReasoningDelta, Turn: turnIdx, Text: chunk.Text})
		case port.ChunkReasoningItem:
			// The opaque encrypted_content replay blob. Stored on Message.Reasoning
			// and sent back verbatim next turn for stateless reasoning continuity.
			// The provider emits at most one per turn; concatenation is harmless if
			// it ever splits.
			reasoningBlob += chunk.Text
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
		return session.Message{}, usage, stop, turnTiming{}, context.Canceled
	}

	// Summarise the inter-token gaps: mean over the (contentChunks-1) gaps and the
	// largest single gap. With fewer than two content chunks there is no gap, so
	// both stay 0 (already the zero value) — never a bogus zero observation.
	if contentChunks >= 2 {
		timing.interTokenMeanMs = gapSumMs / int64(contentChunks-1)
		timing.interTokenMaxMs = gapMaxMs
	}

	return session.NewAssistantMessage(text, reasoningBlob, calls), usage, stop, timing, nil
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
	if e.deps.TokenCounter.CountMessages(sess.Conversation.Messages) < threshold {
		return false
	}
	compacted, summary, err := e.deps.Compactor.Compact(ctx, sess.Conversation)
	if err != nil {
		// Compaction is best-effort: a failure must not abort the run. Keep the
		// existing history and continue — but no longer SILENTLY: surface the
		// degraded mode on the operator channel so a run that keeps growing
		// uncompacted is diagnosable. Behaviour is unchanged (still continue).
		// ErrCompactionWouldOrphan (a compactor refusing to emit a tool-pairing-
		// invalid history) lands here too, reusing this WARN — no new diagnostics
		// line, preserving the "loop emits exactly TWO lines" invariant.
		r.diag.Log(ctx, port.LevelWarn, "compaction failed; continuing without compaction", "error", err)
		return false
	}
	// Last line of defense: never hand the session a history that would orphan a
	// tool result. A compactor SHOULD have caught this and returned the sentinel,
	// but validate again before ReplaceHistory and degrade-and-continue (same
	// branch, same WARN) rather than risk a provider HTTP 400 that bricks the run.
	if err := session.ValidateToolPairing(compacted); err != nil {
		r.diag.Log(ctx, port.LevelWarn, "compaction failed; continuing without compaction", "error", err)
		return false
	}
	if err := sess.ReplaceHistory(compacted); err != nil {
		// Replacement is legal only while running; if the seam rejects it, keep the
		// existing history rather than aborting the run — and emit a degraded-mode
		// warning rather than swallowing it. Behaviour is unchanged (still continue).
		r.diag.Log(ctx, port.LevelWarn, "compaction produced history the session rejected; continuing without compaction", "error", err)
		return false
	}
	e.emit(r, session.Event{Type: session.EvCompaction, Turn: turnIdx, Text: summary})
	return true
}

// bindRunDiag returns the run-scoped Diagnostics for a run against the given
// session id: deps.Diagnostics with the "session" key bound, plus the "agent" key
// when Deps.Role is set (a child/subagent engine). The main engine (Role=="") binds
// only "session". deps.Diagnostics is never nil post-NewEngine and With on
// NopDiagnostics returns NopDiagnostics, so the result is always non-nil and safe.
func (e *Engine) bindRunDiag(id session.SessionID) port.Diagnostics {
	if e.deps.Role != "" {
		return e.deps.Diagnostics.With("session", string(id), "agent", e.deps.Role)
	}
	return e.deps.Diagnostics.With("session", string(id))
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
