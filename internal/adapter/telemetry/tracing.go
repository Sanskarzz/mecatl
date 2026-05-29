package telemetry

import (
	"context"
	"sync"

	"go.opentelemetry.io/otel/attribute"
	"go.opentelemetry.io/otel/codes"
	"go.opentelemetry.io/otel/trace"

	"github.com/stacklok/ozzharness/internal/port"
	"github.com/stacklok/ozzharness/internal/session"
)

// tracerName is the instrumentation scope name for spans this adapter creates.
const tracerName = "github.com/stacklok/ozzharness/internal/adapter/telemetry"

// Tracing is an OpenTelemetry-backed telemetry adapter. It implements
// port.EventSink and turns the event stream into spans:
//
//   - a run span opened on the first event of a run (session.init, or the first
//     event seen if init is missing) and ended on the terminal result event,
//     with the stop reason mapped to span status and token usage recorded as
//     attributes;
//   - a child span per tool, opened on tool.call and ended on the matching
//     tool.result, keyed by ToolCallID.
//
// # Context limitation
//
// Because EventSink.Emit carries no context.Context and session.Event has no
// session id, Tracing cannot link to an inbound request context and cannot
// distinguish concurrent runs on a single sink. It therefore keeps a single
// "current run" span per Tracing instance: a result event closes the open run
// span, and the next session.init opens a fresh one. Tool spans are correlated
// by ToolCallID, which is unique within a run. For per-run correlation across
// truly concurrent runs, hand each run its own Tracing/EventSink from a
// ctx-aware server seam. The span map is mutex-guarded so concurrent Emit calls
// are safe even under the single-root model.
type Tracing struct {
	tracer trace.Tracer

	mu       sync.Mutex
	runCtx   context.Context //nolint:containedctx // span carrier; no request ctx is available on Emit
	runSpan  trace.Span
	turnSpan trace.Span
	tools    map[session.ToolCallID]trace.Span
}

// Compile-time interface check.
var _ port.EventSink = (*Tracing)(nil)

// NewTracing constructs a Tracing adapter that creates spans from tp. It
// implements port.EventSink.
func NewTracing(tp trace.TracerProvider) *Tracing {
	return &Tracing{
		tracer: tp.Tracer(tracerName),
		tools:  make(map[session.ToolCallID]trace.Span),
	}
}

// Emit maintains run, turn, and tool spans from the event stream.
func (t *Tracing) Emit(ev session.Event) {
	t.mu.Lock()
	defer t.mu.Unlock()

	switch ev.Type {
	case session.EvSessionInit:
		t.startRun()
	case session.EvTurnStart:
		t.ensureRun()
		t.startTurn(ev.Turn)
	case session.EvToolCall:
		t.ensureRun()
		t.startTool(ev.ToolCall)
	case session.EvToolResult:
		t.endTool(ev.ToolResult)
	case session.EvResult:
		t.endRun(ev.Result)
	case session.EvMessageDelta, session.EvPermissionAsk, session.EvHook, session.EvCompaction:
		t.ensureRun()
	}
}

// ensureRun opens a run span lazily if the first event of a run was not a
// session.init (defensive: the run span must exist before any child span).
func (t *Tracing) ensureRun() {
	if t.runSpan == nil {
		t.startRun()
	}
}

// startRun opens the per-run root span.
func (t *Tracing) startRun() {
	if t.runSpan != nil {
		return
	}
	t.runCtx, t.runSpan = t.tracer.Start(context.Background(), "ozz.run")
}

// startTurn opens a per-turn child span, ending any previous turn span first.
func (t *Tracing) startTurn(turn int) {
	if t.turnSpan != nil {
		t.turnSpan.End()
	}
	_, t.turnSpan = t.tracer.Start(t.runCtx, "ozz.turn",
		trace.WithAttributes(attribute.Int("ozz.turn", turn)))
}

// parent returns the most specific open parent context for a child span.
func (t *Tracing) parent() context.Context {
	if t.turnSpan != nil {
		return trace.ContextWithSpan(t.runCtx, t.turnSpan)
	}
	return t.runCtx
}

// startTool opens a tool child span keyed by ToolCallID.
func (t *Tracing) startTool(call *session.ToolCall) {
	if call == nil {
		return
	}
	_, span := t.tracer.Start(t.parent(), "ozz.tool",
		trace.WithAttributes(
			attribute.String("ozz.tool.name", call.Name),
			attribute.String("ozz.tool.call_id", string(call.ID)),
		))
	t.tools[call.ID] = span
}

// endTool ends the tool span matching the result's CallID.
func (t *Tracing) endTool(res *session.ToolResult) {
	if res == nil {
		return
	}
	span, ok := t.tools[res.CallID]
	if !ok {
		return
	}
	span.SetAttributes(attribute.Bool("ozz.tool.error", res.IsError))
	if res.IsError {
		span.SetStatus(codes.Error, "tool returned error")
	}
	span.End()
	delete(t.tools, res.CallID)
}

// endRun ends the run span (and any open turn/tool spans), recording the stop
// reason as status and token usage as attributes.
func (t *Tracing) endRun(r *session.ResultPayload) {
	// Close any dangling tool spans first.
	for id, span := range t.tools {
		span.End()
		delete(t.tools, id)
	}
	if t.turnSpan != nil {
		t.turnSpan.End()
		t.turnSpan = nil
	}
	if t.runSpan == nil {
		return
	}
	if r != nil {
		t.runSpan.SetAttributes(
			attribute.String("ozz.run.stop", string(r.Stop)),
			attribute.Int("ozz.tokens.input", r.Usage.InputTokens),
			attribute.Int("ozz.tokens.output", r.Usage.OutputTokens),
			attribute.Int("ozz.tokens.cache_read", r.Usage.CacheReadTokens),
			attribute.Int("ozz.tokens.cache_write", r.Usage.CacheWriteTokens),
		)
		switch r.Stop {
		case session.StopError, session.StopMaxConsecutiveFailures:
			t.runSpan.SetStatus(codes.Error, string(r.Stop))
		case session.StopNone, session.StopEndTurn, session.StopMaxTurns,
			session.StopMaxToolCalls, session.StopCancelled:
			t.runSpan.SetStatus(codes.Ok, "")
		}
	}
	t.runSpan.End()
	t.runSpan = nil
	t.runCtx = nil
}
