package telemetry

import (
	"context"
	"net/http"
	"time"

	"github.com/prometheus/client_golang/prometheus"
	"github.com/prometheus/client_golang/prometheus/promhttp"

	"github.com/stacklok/mecatl/internal/port"
	"github.com/stacklok/mecatl/internal/session"
)

// Metrics is a Prometheus-backed telemetry adapter. It implements both
// port.EventSink (deriving counters/gauges from the event stream) and
// port.Logger (deriving tool-call counters and a latency histogram).
//
// All series use bounded label sets: tool names and stop reasons are bounded
// domain values, and no series is ever labelled by session id or free text.
type Metrics struct {
	events       *prometheus.CounterVec
	runs         *prometheus.CounterVec
	toolCalls    *prometheus.CounterVec
	toolDuration *prometheus.HistogramVec
	tokens       *prometheus.CounterVec
	cacheHit     prometheus.Gauge
	permAsks     prometheus.Counter
	activeRuns   prometheus.Gauge
}

// Compile-time interface checks.
var (
	_ port.EventSink = (*Metrics)(nil)
	_ port.Logger    = (*Metrics)(nil)
)

// NewMetrics constructs a Metrics adapter and registers its collectors with
// reg. It implements both port.EventSink and port.Logger.
func NewMetrics(reg prometheus.Registerer) *Metrics {
	m := &Metrics{
		events: prometheus.NewCounterVec(prometheus.CounterOpts{
			Name: "mecatl_events_total",
			Help: "Total domain events observed, by event type.",
		}, []string{"type"}),
		runs: prometheus.NewCounterVec(prometheus.CounterOpts{
			Name: "mecatl_runs_total",
			Help: "Total runs finished, by stop reason.",
		}, []string{"stop"}),
		toolCalls: prometheus.NewCounterVec(prometheus.CounterOpts{
			Name: "mecatl_tool_calls_total",
			Help: "Total tool calls executed, by tool name and error outcome.",
		}, []string{"tool", "error"}),
		toolDuration: prometheus.NewHistogramVec(prometheus.HistogramOpts{
			Name:    "mecatl_tool_duration_seconds",
			Help:    "Tool execution wall-clock duration in seconds, by tool name.",
			Buckets: prometheus.DefBuckets,
		}, []string{"tool"}),
		tokens: prometheus.NewCounterVec(prometheus.CounterOpts{
			Name: "mecatl_tokens_total",
			Help: "Total tokens accounted, by kind (input/output/cache_read/cache_write).",
		}, []string{"kind"}),
		cacheHit: prometheus.NewGauge(prometheus.GaugeOpts{
			Name: "mecatl_cache_hit_ratio",
			Help: "Prompt-cache hit ratio of the most recent run result.",
		}),
		permAsks: prometheus.NewCounter(prometheus.CounterOpts{
			Name: "mecatl_permission_asks_total",
			Help: "Total permission.ask events observed.",
		}),
		activeRuns: prometheus.NewGauge(prometheus.GaugeOpts{
			Name: "mecatl_active_runs",
			Help: "Number of runs currently in flight.",
		}),
	}
	reg.MustRegister(
		m.events,
		m.runs,
		m.toolCalls,
		m.toolDuration,
		m.tokens,
		m.cacheHit,
		m.permAsks,
		m.activeRuns,
	)
	return m
}

// Emit records Prometheus metrics derived from a single domain Event. The ctx
// is currently unused: the client_golang recording API takes no context. It is
// part of the port.EventSink contract (so OTel-metrics implementers can read a
// span/baggage from it) and is threaded for when this adapter migrates to the
// OTel metrics SDK.
func (m *Metrics) Emit(_ context.Context, ev session.Event) {
	m.events.WithLabelValues(string(ev.Type)).Inc()

	switch ev.Type {
	case session.EvSessionInit:
		m.activeRuns.Inc()
	case session.EvPermissionAsk:
		m.permAsks.Inc()
	case session.EvResult:
		m.activeRuns.Dec()
		m.recordResult(ev.Result)
	case session.EvTurnStart,
		session.EvMessageDelta,
		session.EvToolCall,
		session.EvToolResult,
		session.EvToolProgress,
		session.EvHook,
		session.EvCompaction:
		// Counted by events_total above; no further metric. EvToolProgress is a
		// transient advisory line — the events_total bump is all it warrants.
	}
}

// recordResult records run-terminal metrics: the stop reason, token totals, and
// the cache hit ratio.
func (m *Metrics) recordResult(r *session.ResultPayload) {
	if r == nil {
		m.runs.WithLabelValues(string(session.StopNone)).Inc()
		return
	}
	m.runs.WithLabelValues(string(r.Stop)).Inc()
	u := r.Usage
	m.tokens.WithLabelValues("input").Add(float64(u.InputTokens))
	m.tokens.WithLabelValues("output").Add(float64(u.OutputTokens))
	m.tokens.WithLabelValues("cache_read").Add(float64(u.CacheReadTokens))
	m.tokens.WithLabelValues("cache_write").Add(float64(u.CacheWriteTokens))
	m.cacheHit.Set(u.CacheHitRate())
}

// ToolCall records the per-tool call counter and latency histogram. It
// satisfies port.Logger.
func (m *Metrics) ToolCall(_ session.SessionID, call session.ToolCall, result session.ToolResult, took time.Duration) {
	errLabel := "false"
	if result.IsError {
		errLabel = "true"
	}
	m.toolCalls.WithLabelValues(call.Name, errLabel).Inc()
	m.toolDuration.WithLabelValues(call.Name).Observe(took.Seconds())
}

// MetricsHandler returns an http.Handler that serves the given registry in the
// Prometheus text exposition format, suitable for mounting at /metrics.
func MetricsHandler(reg *prometheus.Registry) http.Handler {
	return promhttp.HandlerFor(reg, promhttp.HandlerOpts{})
}
