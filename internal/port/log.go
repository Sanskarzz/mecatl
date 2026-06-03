package port

import (
	"context"
	"time"

	"github.com/stacklok/mecatl/internal/session"
)

// EventSink receives domain Events from the loop and relays them to the API
// stream (gRPC server-stream / HTTP SSE).
type EventSink interface {
	// Emit publishes a single Event. The ctx is the run's context: telemetry
	// implementers may read a trace span from it (so concurrent runs correlate
	// their spans/metrics to the originating request) but MUST NOT retain it past
	// the call. Implementations must not block the loop indefinitely.
	//
	// The ctx is a trace/baggage carrier ONLY: implementers MUST NOT derive
	// cancellation or deadlines from it. Terminal-event emits (e.g. the final
	// EvResult after Run.Cancel) deliberately pass an already-cancelled ctx, and
	// correctness relies on sinks reading only the span context from it — a sink
	// that bailed on ctx.Err() would drop those terminal events.
	Emit(ctx context.Context, ev session.Event)
}

// Logger records structured observability for tool execution. It is an
// observability seam, distinct from the model-visible conversation.
type Logger interface {
	// ToolCall records that a tool was executed, with its result, the time it
	// spent waiting in the dispatch queue before execution started (queued), and
	// the wall time its execution then took (took).
	//
	// queued is the coordinated-omission measure: it is the gap between when the
	// call ENTERED dispatch and when its execution actually began. For a read-only
	// call cleared to run immediately it is near-zero; for a mutating call held by
	// the read-parallel/mutate-serial ordering (or behind a permission ask) it is
	// the real wait the model's call sat through. Both durations are 0 when no
	// Clock is injected.
	ToolCall(id session.SessionID, call session.ToolCall, result session.ToolResult, queued, took time.Duration)
}
