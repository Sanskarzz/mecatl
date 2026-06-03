package telemetry

import (
	"context"
	"fmt"
	"log/slog"
	"sync"
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

// Latency-instrument names. Each is a base-2 exponential-histogram latency
// instrument (decision 2 in docs/design/perf-observability.md §5). They are
// exported as package constants because the MeterProvider installs an
// exponential-histogram metric.View keyed on each exact name; the view and the
// instrument name MUST agree, so views target instruments by these constants
// rather than duplicated string literals.
const (
	// toolDurationInstrument is the per-tool execution wall-clock histogram.
	toolDurationInstrument = "mecatl.tool.duration"
	// turnDurationInstrument is the per-turn model-call wall-clock histogram.
	turnDurationInstrument = "mecatl.turn.duration"
	// ttftInstrument is the time-to-first-token histogram.
	ttftInstrument = "mecatl.ttft"
	// interTokenInstrument is the per-turn MEAN inter-token gap histogram — the
	// average gap between consecutive content chunks within a turn.
	interTokenInstrument = "mecatl.inter_token" //nolint:gosec // G101 false positive: a metric instrument name, not a credential
	// interTokenMaxInstrument is the per-turn WORST inter-token gap histogram — the
	// single largest gap between consecutive content chunks within a turn. It is a
	// streaming-jitter tail signal: where mean tracks typical smoothness, max
	// captures the worst stall a user felt mid-turn.
	interTokenMaxInstrument = "mecatl.inter_token.max" //nolint:gosec // G101 false positive: a metric instrument name, not a credential
	// toolQueueInstrument is the tool queue-time histogram: the wait from a call
	// entering dispatch to its execution starting (the coordinated-omission fix).
	toolQueueInstrument = "mecatl.tool.queue"
)

// latencyInstruments is the single source of truth for which instruments are
// aggregated as base-2 exponential histograms. LatencyViews builds one view per
// entry, so adding a latency instrument here installs its exponential view
// everywhere the MeterProvider is assembled — no per-call-site duplication of the
// aggregation literal.
var latencyInstruments = []string{
	toolDurationInstrument,
	turnDurationInstrument,
	ttftInstrument,
	interTokenInstrument,
	interTokenMaxInstrument,
	toolQueueInstrument,
}

// exponentialLatencyAggregation is the single aggregation spec shared by every
// latency instrument. Defining it once keeps MaxSize/MaxScale from drifting
// across instruments (the drift the prior tool-duration commit's single-source
// lesson guards against).
func exponentialLatencyAggregation() sdkmetric.AggregationBase2ExponentialHistogram {
	return sdkmetric.AggregationBase2ExponentialHistogram{
		MaxSize:  160,
		MaxScale: 20,
	}
}

// LatencyViews returns the base-2 exponential-histogram views for EVERY latency
// instrument (tool/turn duration, TTFT, inter-token, tool-queue; decision 2 in
// docs/design/perf-observability.md §5). It is the single source of truth for the
// latency aggregation: any MeterProvider feeding NewMetrics MUST install these
// (sdkmetric.WithView(LatencyViews()...)), or the latency series degrade silently
// to the default explicit-bucket histogram.
//
// Aggregation choice is a view-on-the-provider/reader concern, NOT a
// per-instrument hint, which is why it belongs to whoever assembles the
// MeterProvider.
func LatencyViews() []sdkmetric.View {
	views := make([]sdkmetric.View, 0, len(latencyInstruments))
	for _, name := range latencyInstruments {
		views = append(views, sdkmetric.NewView(
			sdkmetric.Instrument{Name: name},
			sdkmetric.Stream{Aggregation: exponentialLatencyAggregation()},
		))
	}
	return views
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
	// turnDuration is the per-turn model-call wall-clock histogram (unit "s"),
	// recorded from EvTurnEnd.DurationMs.
	turnDuration metric.Float64Histogram
	// ttft is the time-to-first-token histogram (unit "s"), recorded from
	// EvTurnEnd.TTFTMs (skipped when the turn produced no content chunk).
	ttft metric.Float64Histogram
	// interToken is the per-turn mean inter-token-gap histogram (unit "s"),
	// recorded from EvTurnEnd.InterTokenMeanMs (skipped for <2-content-chunk turns).
	interToken metric.Float64Histogram
	// interTokenMax is the per-turn worst inter-token-gap histogram (unit "s"),
	// recorded from EvTurnEnd.InterTokenMaxMs (skipped for <2-content-chunk turns).
	// It is the streaming-jitter tail signal beside interToken's typical-gap mean.
	interTokenMax metric.Float64Histogram
	// toolQueue is the tool queue-time histogram (unit "s"): the wait from a call
	// entering dispatch to its execution starting (coordinated-omission fix).
	toolQueue metric.Float64Histogram
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
	if m.turnDuration, err = meter.Float64Histogram(
		turnDurationInstrument,
		metric.WithDescription("Per-turn model-call wall-clock duration in seconds."),
		metric.WithUnit("s"),
	); err != nil {
		return nil, fmt.Errorf("telemetry: turn duration histogram: %w", err)
	}
	if m.ttft, err = meter.Float64Histogram(
		ttftInstrument,
		metric.WithDescription("Time to first content token (text or reasoning) per turn, in seconds."),
		metric.WithUnit("s"),
	); err != nil {
		return nil, fmt.Errorf("telemetry: ttft histogram: %w", err)
	}
	if m.interToken, err = meter.Float64Histogram(
		interTokenInstrument,
		metric.WithDescription("Mean inter-token gap per turn, in seconds."),
		metric.WithUnit("s"),
	); err != nil {
		return nil, fmt.Errorf("telemetry: inter-token histogram: %w", err)
	}
	if m.interTokenMax, err = meter.Float64Histogram(
		interTokenMaxInstrument,
		metric.WithDescription("Worst (largest) inter-token gap per turn, in seconds."),
		metric.WithUnit("s"),
	); err != nil {
		return nil, fmt.Errorf("telemetry: inter-token max histogram: %w", err)
	}
	if m.toolQueue, err = meter.Float64Histogram(
		toolQueueInstrument,
		metric.WithDescription("Tool dispatch queue time in seconds (enqueue→execution start), by tool name."),
		metric.WithUnit("s"),
	); err != nil {
		return nil, fmt.Errorf("telemetry: tool queue histogram: %w", err)
	}

	return m, nil
}

// rssZeroLogOnce ensures the "RSS read returned 0 on a supported platform"
// diagnostic is logged at most once for the process lifetime, so a persistent
// /proc failure surfaces a single actionable line rather than one per scrape.
var rssZeroLogOnce sync.Once

// RegisterProcessGauges registers the process-level observable gauges on the
// given MeterProvider's meter. Currently it registers mecatl.process.rss (the
// resident set size in bytes), read lock-free via readRSS on each collection.
//
// The gauge is registered ONLY where RSS is actually readable (Linux): off
// Linux rssSupported() is false and the series is simply absent (decision 9 —
// the RSS gauge ships for general long-session memory visibility, no
// leak-specific alarm). It is a separate registration from NewMetrics because
// the domain instruments derive from the event/log stream, whereas this is an
// async observation of the OS process; keeping it apart lets a caller opt out.
//
// It returns an error if the instrument fails to construct.
func RegisterProcessGauges(mp metric.MeterProvider) error {
	if !rssSupported() {
		return nil
	}
	meter := mp.Meter(meterName)
	rss, err := meter.Int64ObservableGauge(
		"mecatl.process.rss",
		metric.WithDescription("Process resident set size in bytes (read from /proc on Linux)."),
		metric.WithUnit("By"),
		metric.WithInt64Callback(func(_ context.Context, o metric.Int64Observer) error {
			if v := readRSS(); v > 0 {
				o.Observe(int64(v)) //nolint:gosec // RSS bytes fits an int64 for any real process
			} else {
				// 0 on a platform that claims RSS support means a persistent /proc
				// read failure: the gauge series silently goes missing. Log it ONCE
				// at debug so the gap is diagnosable without flooding every scrape.
				rssZeroLogOnce.Do(func() {
					slog.Debug("process RSS read returned 0 on a supported platform; mecatl_process_rss series will be absent until /proc reads succeed")
				})
			}
			return nil
		}),
	)
	if err != nil {
		return fmt.Errorf("telemetry: process rss gauge: %w", err)
	}
	_ = rss // the instrument is driven by its callback; the handle is not used directly.
	return nil
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
	case session.EvTurnEnd:
		m.recordTurnEnd(ctx, ev.TurnEnd)
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

// recordTurnEnd records the per-turn latency histograms from a TurnEndPayload:
// turn duration always, plus TTFT and the inter-token gaps when they were
// actually measured. The inter-token signal is split into two instruments: the
// per-turn MEAN gap (interTokenInstrument, typical smoothness) and the per-turn
// MAX gap (interTokenMaxInstrument, the worst mid-turn stall — a streaming-jitter
// tail signal). A 0 on TTFTMs / InterTokenMeanMs / InterTokenMaxMs means "not
// measured" (no Clock, no content chunk, or fewer than two content chunks) —
// never a real observation — so each is skipped under its OWN independent >0
// guard to avoid recording bogus zeros. All instruments are unit "s", so ms is
// converted to seconds.
func (m *Metrics) recordTurnEnd(ctx context.Context, p *session.TurnEndPayload) {
	if p == nil {
		return
	}
	m.turnDuration.Record(ctx, msToSeconds(p.DurationMs))
	if p.TTFTMs > 0 {
		m.ttft.Record(ctx, msToSeconds(p.TTFTMs))
	}
	if p.InterTokenMeanMs > 0 {
		m.interToken.Record(ctx, msToSeconds(p.InterTokenMeanMs))
	}
	if p.InterTokenMaxMs > 0 {
		m.interTokenMax.Record(ctx, msToSeconds(p.InterTokenMaxMs))
	}
}

// msToSeconds converts a millisecond count to seconds for the "s"-unit latency
// instruments.
func msToSeconds(ms int64) float64 { return float64(ms) / 1000.0 }

// ToolCall records the per-tool call counter and the duration/queue-time latency
// histograms. It satisfies port.Logger. port.Logger carries no ctx, so the
// recordings use a background context — exemplar correlation is best-effort here.
// queued is the dispatch wait (enqueue→execution start); took is the execution
// wall time. Both are recorded with the same tool attribute.
func (m *Metrics) ToolCall(_ session.SessionID, call session.ToolCall, result session.ToolResult, queued, took time.Duration) {
	ctx := context.Background()
	errLabel := "false"
	if result.IsError {
		errLabel = "true"
	}
	m.toolCalls.Add(ctx, 1, metric.WithAttributes(
		attribute.String(attrTool, call.Name),
		attribute.String(attrError, errLabel),
	))
	toolAttr := metric.WithAttributes(attribute.String(attrTool, call.Name))
	m.toolDuration.Record(ctx, took.Seconds(), toolAttr)
	m.toolQueue.Record(ctx, queued.Seconds(), toolAttr)
}
