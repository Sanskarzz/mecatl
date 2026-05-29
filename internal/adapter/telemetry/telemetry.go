// Package telemetry is an outbound adapter that derives OpenTelemetry traces
// and Prometheus metrics from the harness's domain event stream.
//
// It implements both port.EventSink (so it observes every session.Event the
// loop emits) and port.Logger (so it observes per-tool execution timing). The
// adapter is self-contained: the leader wires it by teeing the telemetry sink
// into the Engine's EventSink and Logger, mounting MetricsHandler at /metrics,
// and passing a TracerProvider to NewTracing.
//
// # Context limitation
//
// port.EventSink.Emit(ev) carries no context.Context, and session.Event has no
// session-id field. Consequently the tracing implementation cannot link its
// spans to an inbound gRPC/HTTP request context, and it cannot key spans by run
// when multiple runs interleave on the same sink. Tracing therefore scopes
// spans to a single provider-level root per sink instance (see Tracing). Tool
// child spans are still produced with accurate latency, and the run lifecycle
// span captures stop reason and token usage. Per-run correlation across
// concurrent runs requires a future ctx-aware seam (e.g. an Emit(ctx, ev)
// variant or a per-run sink handed out by the server interceptor).
package telemetry

import (
	"github.com/stacklok/ozzharness/internal/port"
	"github.com/stacklok/ozzharness/internal/session"
)

// fanOut is a port.EventSink that relays each Event to every wrapped sink in
// order. It is the tee that lets metrics and tracing both observe the stream.
type fanOut struct {
	sinks []port.EventSink
}

// NewSink returns a port.EventSink that fans out every Event to each of the
// given sinks, in the order provided. It lets a single Engine EventSink drive
// both the Metrics and Tracing adapters.
func NewSink(sinks ...port.EventSink) port.EventSink {
	return &fanOut{sinks: sinks}
}

// Emit relays ev to every wrapped sink.
func (f *fanOut) Emit(ev session.Event) {
	for _, s := range f.sinks {
		s.Emit(ev)
	}
}
