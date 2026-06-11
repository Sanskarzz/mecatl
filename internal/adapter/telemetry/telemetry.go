// Package telemetry is an outbound adapter that derives OpenTelemetry traces
// and Prometheus metrics from the harness's domain event stream.
//
// It implements both port.EventSink (so it observes every session.Event the
// loop emits) and port.ToolCallRecorder (so it observes per-tool execution
// timing). The adapter is self-contained: the leader wires it by teeing the
// telemetry sink into the Engine's EventSink and ToolCallRecorder, mounting MetricsHandler at /metrics,
// and passing a TracerProvider to NewTracing.
//
// # Context
//
// port.EventSink.Emit(ctx, ev) carries the run's context.Context, but
// session.Event has no session-id field. When the ctx carries a trace span
// (e.g. the run goroutine was started under an inbound request span), Tracing
// parents its run span to it, so concurrent runs correlate to their originating
// request. When the ctx carries no span, Tracing falls back to a single
// provider-level root per sink instance (see Tracing). Tool child spans are
// produced with accurate latency, and the run lifecycle span captures stop
// reason and token usage.
package telemetry

import (
	"context"

	"github.com/stacklok/mecatl/engine/port"
	"github.com/stacklok/mecatl/engine/session"
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

// Emit relays ctx and ev to every wrapped sink.
func (f *fanOut) Emit(ctx context.Context, ev session.Event) {
	for _, s := range f.sinks {
		s.Emit(ctx, ev)
	}
}
