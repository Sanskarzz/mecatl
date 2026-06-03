package telemetry

import (
	"context"
	"fmt"
	"time"

	"go.opentelemetry.io/otel/attribute"
	"go.opentelemetry.io/otel/metric"
	sdkmetric "go.opentelemetry.io/otel/sdk/metric"

	"github.com/stacklok/mecatl/internal/port"
	"github.com/stacklok/mecatl/internal/session"
)

// meterName is the instrumentation scope name for the domain meter. The
// prometheus exporter does not fold the scope into series names by default, so
// each instrument carries the "mecatl." prefix below to keep the exposed series
// recognisable (e.g. mecatl_events_total).
const meterName = "github.com/stacklok/mecatl/internal/adapter/telemetry"

// toolDurationInstrument is the instrument name of the tool-duration histogram.
// It is exported as a package constant because Setup installs a base-2
// exponential-histogram metric.View keyed on this exact name (decision 2 in
// docs/design/perf-observability.md §5). The two MUST agree, so the view targets
// the instrument by this constant rather than a duplicated string literal.
const toolDurationInstrument = "mecatl.tool.duration"

// ToolDurationView is the single source of truth for the tool-duration
// aggregation. It reports the tool-duration histogram as a base-2 exponential
// histogram (accurate tails across a wide dynamic range; decision 2 in
// docs/design/perf-observability.md §5), keyed on toolDurationInstrument so the
// view and the instrument name can never drift apart.
//
// It is a view-on-the-provider/reader concern, NOT a per-instrument hint:
// aggregation choice belongs to whoever assembles the MeterProvider. Any
// MeterProvider feeding NewMetrics MUST install this view (sdkmetric.WithView),
// or tool.duration degrades silently to the default explicit-bucket histogram.
func ToolDurationView() sdkmetric.View {
	return sdkmetric.NewView(
		sdkmetric.Instrument{Name: toolDurationInstrument},
		sdkmetric.Stream{
			Aggregation: sdkmetric.AggregationBase2ExponentialHistogram{
				MaxSize:  160,
				MaxScale: 20,
			},
		},
	)
}

// Attribute keys. These are the only label dimensions any series carries; all
// values are bounded domain enums (event/stop/tool names) or low-cardinality
// flags — never a session id or free text.
const (
	attrType  = "type"  // event type
	attrStop  = "stop"  // run stop reason
	attrTool  = "tool"  // tool name
	attrError = "error" // tool error outcome ("true"/"false")
	attrKind  = "kind"  // token kind (input/output/cache_read/cache_write)
)

// Metrics is an OpenTelemetry-backed telemetry adapter. It implements both
// port.EventSink (deriving counters/gauges from the event stream) and
// port.Logger (deriving tool-call counters and a latency histogram).
//
// All series use bounded attribute sets: tool names and stop reasons are bounded
// domain values, and no series is ever labelled by session id or free text. The
// instruments are created from a metric.Meter obtained from the injected
// MeterProvider; the provider's prometheus exporter (wired in Setup) renders
// them on the /metrics registry.
type Metrics struct {
	events     metric.Int64Counter
	runs       metric.Int64Counter
	toolCalls  metric.Int64Counter
	tokens     metric.Int64Counter
	permAsks   metric.Int64Counter
	activeRuns metric.Int64UpDownCounter
	// cacheHit holds the prompt-cache hit ratio of the most recent run result.
	// It is a synchronous gauge: recordResult sets it to the latest run's ratio,
	// mirroring the single-value semantics of the old client_golang Gauge.
	cacheHit metric.Float64Gauge
	// toolDuration is the tool execution wall-clock histogram (unit "s"). Setup
	// installs a base-2 exponential-histogram view for it (toolDurationInstrument).
	toolDuration metric.Float64Histogram
}

// Compile-time interface checks.
var (
	_ port.EventSink = (*Metrics)(nil)
	_ port.Logger    = (*Metrics)(nil)
)

// NewMetrics constructs a Metrics adapter from an OTel MeterProvider. It
// implements both port.EventSink and port.Logger. The provider is expected to
// have a prometheus exporter reader and the tool-duration exponential-histogram
// view installed (see Setup); NewMetrics itself only creates the instruments.
//
// It returns an error if any instrument fails to construct — the OTel meter API
// is fallible, unlike client_golang's panic-on-misuse registration.
func NewMetrics(mp metric.MeterProvider) (*Metrics, error) {
	meter := mp.Meter(meterName)
	m := &Metrics{}

	var err error
	if m.events, err = meter.Int64Counter(
		"mecatl.events",
		metric.WithDescription("Total domain events observed, by event type."),
	); err != nil {
		return nil, fmt.Errorf("telemetry: events counter: %w", err)
	}
	if m.runs, err = meter.Int64Counter(
		"mecatl.runs",
		metric.WithDescription("Total runs finished, by stop reason."),
	); err != nil {
		return nil, fmt.Errorf("telemetry: runs counter: %w", err)
	}
	if m.toolCalls, err = meter.Int64Counter(
		"mecatl.tool.calls",
		metric.WithDescription("Total tool calls executed, by tool name and error outcome."),
	); err != nil {
		return nil, fmt.Errorf("telemetry: tool calls counter: %w", err)
	}
	if m.tokens, err = meter.Int64Counter(
		"mecatl.tokens",
		metric.WithDescription("Total tokens accounted, by kind (input/output/cache_read/cache_write)."),
		metric.WithUnit("{token}"),
	); err != nil {
		return nil, fmt.Errorf("telemetry: tokens counter: %w", err)
	}
	if m.permAsks, err = meter.Int64Counter(
		"mecatl.permission.asks",
		metric.WithDescription("Total permission.ask events observed."),
	); err != nil {
		return nil, fmt.Errorf("telemetry: permission asks counter: %w", err)
	}
	if m.activeRuns, err = meter.Int64UpDownCounter(
		"mecatl.active_runs",
		metric.WithDescription("Number of runs currently in flight."),
	); err != nil {
		return nil, fmt.Errorf("telemetry: active runs up/down counter: %w", err)
	}
	if m.cacheHit, err = meter.Float64Gauge(
		"mecatl.cache_hit_ratio",
		metric.WithDescription("Prompt-cache hit ratio of the most recent run result."),
	); err != nil {
		return nil, fmt.Errorf("telemetry: cache hit gauge: %w", err)
	}
	if m.toolDuration, err = meter.Float64Histogram(
		toolDurationInstrument,
		metric.WithDescription("Tool execution wall-clock duration in seconds, by tool name."),
		metric.WithUnit("s"),
	); err != nil {
		return nil, fmt.Errorf("telemetry: tool duration histogram: %w", err)
	}

	return m, nil
}

// Emit records OTel metrics derived from a single domain Event. The ctx is the
// run's context (threaded from port.EventSink.Emit) and is passed to every
// instrument operation so the SDK can attach exemplars from an active span.
func (m *Metrics) Emit(ctx context.Context, ev session.Event) {
	m.events.Add(ctx, 1, metric.WithAttributes(attribute.String(attrType, string(ev.Type))))

	switch ev.Type {
	case session.EvSessionInit:
		m.activeRuns.Add(ctx, 1)
	case session.EvPermissionAsk:
		m.permAsks.Add(ctx, 1)
	case session.EvResult:
		m.activeRuns.Add(ctx, -1)
		m.recordResult(ctx, ev.Result)
	case session.EvTurnStart,
		session.EvMessageDelta,
		session.EvToolCall,
		session.EvToolResult,
		session.EvToolProgress,
		session.EvHook,
		session.EvCompaction:
		// Counted by mecatl.events above; no further metric. EvToolProgress is a
		// transient advisory line — the events bump is all it warrants.
	}
}

// recordResult records run-terminal metrics: the stop reason, token totals, and
// the cache hit ratio.
func (m *Metrics) recordResult(ctx context.Context, r *session.ResultPayload) {
	if r == nil {
		m.runs.Add(ctx, 1, metric.WithAttributes(attribute.String(attrStop, string(session.StopNone))))
		return
	}
	m.runs.Add(ctx, 1, metric.WithAttributes(attribute.String(attrStop, string(r.Stop))))
	u := r.Usage
	m.tokens.Add(ctx, int64(u.InputTokens), metric.WithAttributes(attribute.String(attrKind, "input")))
	m.tokens.Add(ctx, int64(u.OutputTokens), metric.WithAttributes(attribute.String(attrKind, "output")))
	m.tokens.Add(ctx, int64(u.CacheReadTokens), metric.WithAttributes(attribute.String(attrKind, "cache_read")))
	m.tokens.Add(ctx, int64(u.CacheWriteTokens), metric.WithAttributes(attribute.String(attrKind, "cache_write")))
	m.cacheHit.Record(ctx, u.CacheHitRate())
}

// ToolCall records the per-tool call counter and latency histogram. It
// satisfies port.Logger. port.Logger carries no ctx, so the recordings use a
// background context — exemplar correlation is best-effort here.
func (m *Metrics) ToolCall(_ session.SessionID, call session.ToolCall, result session.ToolResult, took time.Duration) {
	ctx := context.Background()
	errLabel := "false"
	if result.IsError {
		errLabel = "true"
	}
	m.toolCalls.Add(ctx, 1, metric.WithAttributes(
		attribute.String(attrTool, call.Name),
		attribute.String(attrError, errLabel),
	))
	m.toolDuration.Record(ctx, took.Seconds(), metric.WithAttributes(
		attribute.String(attrTool, call.Name),
	))
}
