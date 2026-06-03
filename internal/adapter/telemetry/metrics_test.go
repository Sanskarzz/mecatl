package telemetry

import (
	"context"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/prometheus/client_golang/prometheus"
	"go.opentelemetry.io/otel/attribute"
	otelprom "go.opentelemetry.io/otel/exporters/prometheus"
	"go.opentelemetry.io/otel/sdk/metric"
	"go.opentelemetry.io/otel/sdk/metric/metricdata"

	"github.com/stacklok/mecatl/internal/session"
)

// newTestMetrics builds a Metrics adapter backed by a ManualReader so tests can
// Collect() the recorded data points directly. It installs the same base-2
// exponential-histogram view Setup uses for the tool-duration instrument, so the
// tool-duration series collects as an ExponentialHistogram here too.
func newTestMetrics(t *testing.T) (*Metrics, *metric.ManualReader) {
	t.Helper()
	reader := metric.NewManualReader()
	mp := metric.NewMeterProvider(metric.WithReader(reader), metric.WithView(ToolDurationView()))
	m, err := NewMetrics(mp)
	if err != nil {
		t.Fatalf("NewMetrics: %v", err)
	}
	return m, reader
}

// collect gathers all metrics into a flat name→Aggregation map for assertions.
func collect(t *testing.T, reader *metric.ManualReader) map[string]metricdata.Aggregation {
	t.Helper()
	var rm metricdata.ResourceMetrics
	if err := reader.Collect(context.Background(), &rm); err != nil {
		t.Fatalf("Collect: %v", err)
	}
	out := make(map[string]metricdata.Aggregation)
	for _, sm := range rm.ScopeMetrics {
		for _, md := range sm.Metrics {
			out[md.Name] = md.Data
		}
	}
	return out
}

// sumPoint returns the int64 Sum value whose attribute key equals value, or
// fails if absent.
func sumPoint(t *testing.T, agg metricdata.Aggregation, key, value string) int64 {
	t.Helper()
	sum, ok := agg.(metricdata.Sum[int64])
	if !ok {
		t.Fatalf("aggregation is %T, want Sum[int64]", agg)
	}
	for _, dp := range sum.DataPoints {
		if v, present := dp.Attributes.Value(attribute.Key(key)); present && v.AsString() == value {
			return dp.Value
		}
	}
	t.Fatalf("no data point with %s=%q (points: %d)", key, value, len(sum.DataPoints))
	return 0
}

func TestMetricsEventsTotal(t *testing.T) {
	m, reader := newTestMetrics(t)

	m.Emit(context.Background(), session.Event{Type: session.EvSessionInit})
	m.Emit(context.Background(), session.Event{Type: session.EvTurnStart, Turn: 0})
	m.Emit(context.Background(), session.Event{Type: session.EvMessageDelta})
	m.Emit(context.Background(), session.Event{Type: session.EvMessageDelta})

	data := collect(t, reader)
	events := data["mecatl.events"]
	if got := sumPoint(t, events, attrType, "session.init"); got != 1 {
		t.Errorf("events{session.init} = %d, want 1", got)
	}
	if got := sumPoint(t, events, attrType, "message.delta"); got != 2 {
		t.Errorf("events{message.delta} = %d, want 2", got)
	}
}

func TestMetricsRunsAndActiveRuns(t *testing.T) {
	m, reader := newTestMetrics(t)

	m.Emit(context.Background(), session.Event{Type: session.EvSessionInit})
	if got := activeRunsValue(t, reader); got != 1 {
		t.Fatalf("active_runs after init = %d, want 1", got)
	}
	m.Emit(context.Background(), session.Event{Type: session.EvResult, Result: &session.ResultPayload{Stop: session.StopEndTurn}})
	if got := activeRunsValue(t, reader); got != 0 {
		t.Fatalf("active_runs after result = %d, want 0", got)
	}

	runs := collect(t, reader)["mecatl.runs"]
	if got := sumPoint(t, runs, attrStop, "end_turn"); got != 1 {
		t.Errorf("runs{end_turn} = %d, want 1", got)
	}
}

// TestMetricsResultNil drives the distinct r == nil branch of recordResult: a
// terminal EvResult with no payload must still count the run (stop=none) and must
// not touch tokens or the cache-hit gauge, and must not panic.
func TestMetricsResultNil(t *testing.T) {
	m, reader := newTestMetrics(t)

	m.Emit(context.Background(), session.Event{Type: session.EvResult, Result: nil})

	data := collect(t, reader)
	runs := data["mecatl.runs"]
	if got := sumPoint(t, runs, attrStop, string(session.StopNone)); got != 1 {
		t.Errorf("runs{stop=none} = %d, want 1", got)
	}
	// tokens and cache_hit_ratio must be untouched: with nothing recorded, the
	// instruments produce no data points at all.
	if _, present := data["mecatl.tokens"]; present {
		t.Errorf("tokens recorded on nil result; want none")
	}
	if _, present := data["mecatl.cache_hit_ratio"]; present {
		t.Errorf("cache_hit_ratio recorded on nil result; want none")
	}
}

// TestMetricsCacheHitLastValue asserts the cache-hit gauge is last-value: two
// results with different ratios leave the gauge reflecting only the second.
func TestMetricsCacheHitLastValue(t *testing.T) {
	m, reader := newTestMetrics(t)

	// First result: 25/(100) cache-read share.
	m.Emit(context.Background(), session.Event{Type: session.EvResult, Result: &session.ResultPayload{
		Stop:  session.StopEndTurn,
		Usage: session.Usage{InputTokens: 75, CacheReadTokens: 25},
	}})
	// Second result with a different ratio (50/100) — this is the value the gauge
	// must report.
	m.Emit(context.Background(), session.Event{Type: session.EvResult, Result: &session.ResultPayload{
		Stop:  session.StopEndTurn,
		Usage: session.Usage{InputTokens: 50, CacheReadTokens: 50},
	}})

	want := (session.Usage{InputTokens: 50, CacheReadTokens: 50}).CacheHitRate()
	gauge, ok := collect(t, reader)["mecatl.cache_hit_ratio"].(metricdata.Gauge[float64])
	if !ok {
		t.Fatalf("cache_hit_ratio is %T, want Gauge[float64]", collect(t, reader)["mecatl.cache_hit_ratio"])
	}
	if len(gauge.DataPoints) != 1 || gauge.DataPoints[0].Value != want {
		t.Errorf("cache_hit_ratio = %+v, want single point %v (last result only)", gauge.DataPoints, want)
	}
}

// activeRunsValue collects the (single) active_runs up/down counter value. It is
// a non-monotonic Sum, so this verifies the decrement-on-result semantics.
func activeRunsValue(t *testing.T, reader *metric.ManualReader) int64 {
	t.Helper()
	agg := collect(t, reader)["mecatl.active_runs"]
	sum, ok := agg.(metricdata.Sum[int64])
	if !ok {
		t.Fatalf("active_runs is %T, want Sum[int64]", agg)
	}
	if sum.IsMonotonic {
		t.Errorf("active_runs Sum is monotonic; want non-monotonic (UpDownCounter)")
	}
	if len(sum.DataPoints) != 1 {
		t.Fatalf("active_runs points = %d, want 1", len(sum.DataPoints))
	}
	return sum.DataPoints[0].Value
}

func TestMetricsTokensAndCacheRatio(t *testing.T) {
	m, reader := newTestMetrics(t)

	m.Emit(context.Background(), session.Event{Type: session.EvResult, Result: &session.ResultPayload{
		Stop: session.StopEndTurn,
		Usage: session.Usage{
			InputTokens:      100,
			OutputTokens:     40,
			CacheReadTokens:  25,
			CacheWriteTokens: 10,
		},
	}})

	data := collect(t, reader)
	tokens := data["mecatl.tokens"]
	cases := map[string]int64{"input": 100, "output": 40, "cache_read": 25, "cache_write": 10}
	for kind, want := range cases {
		if got := sumPoint(t, tokens, attrKind, kind); got != want {
			t.Errorf("tokens{%s} = %d, want %d", kind, got, want)
		}
	}

	gauge, ok := data["mecatl.cache_hit_ratio"].(metricdata.Gauge[float64])
	if !ok {
		t.Fatalf("cache_hit_ratio is %T, want Gauge[float64]", data["mecatl.cache_hit_ratio"])
	}
	if len(gauge.DataPoints) != 1 || gauge.DataPoints[0].Value != 0.25 {
		t.Errorf("cache_hit_ratio = %+v, want single point 0.25", gauge.DataPoints)
	}
}

func TestMetricsPermissionAsks(t *testing.T) {
	m, reader := newTestMetrics(t)

	m.Emit(context.Background(), session.Event{Type: session.EvPermissionAsk})
	m.Emit(context.Background(), session.Event{Type: session.EvPermissionAsk})

	agg := collect(t, reader)["mecatl.permission.asks"]
	sum, ok := agg.(metricdata.Sum[int64])
	if !ok {
		t.Fatalf("permission.asks is %T, want Sum[int64]", agg)
	}
	if len(sum.DataPoints) != 1 || sum.DataPoints[0].Value != 2 {
		t.Errorf("permission.asks = %+v, want single point 2", sum.DataPoints)
	}
}

func TestMetricsToolCallExponentialHistogram(t *testing.T) {
	m, reader := newTestMetrics(t)

	m.ToolCall("sess-1", session.NewToolCall("c1", "bash", nil),
		session.NewToolResult("c1", "ok"), 250*time.Millisecond)
	m.ToolCall("sess-1", session.NewToolCall("c2", "bash", nil),
		session.NewToolError("c2", "boom"), 10*time.Millisecond)

	data := collect(t, reader)
	calls := data["mecatl.tool.calls"]
	if got := sumPoint(t, calls, attrError, "false"); got != 1 {
		t.Errorf("tool.calls{error=false} = %d, want 1", got)
	}
	if got := sumPoint(t, calls, attrError, "true"); got != 1 {
		t.Errorf("tool.calls{error=true} = %d, want 1", got)
	}

	// The view must turn the tool-duration instrument into an exponential
	// histogram (decision 2), with both observations on the single bash series.
	exp, ok := data[toolDurationInstrument].(metricdata.ExponentialHistogram[float64])
	if !ok {
		t.Fatalf("tool.duration is %T, want ExponentialHistogram[float64]", data[toolDurationInstrument])
	}
	if len(exp.DataPoints) != 1 {
		t.Fatalf("tool.duration series = %d, want 1 (one tool)", len(exp.DataPoints))
	}
	if exp.DataPoints[0].Count != 2 {
		t.Errorf("tool.duration count = %d, want 2", exp.DataPoints[0].Count)
	}
	// Sum must be ~0.26s (250ms + 10ms recorded via took.Seconds()). This catches
	// a unit regression (e.g. recording nanoseconds or milliseconds) that Count
	// alone would not.
	if got := exp.DataPoints[0].Sum; got < 0.259 || got > 0.261 {
		t.Errorf("tool.duration sum = %v, want ≈0.26", got)
	}
}

// TestMetricsScrapeThroughPrometheusExporter wires the adapter through the OTel
// prometheus exporter (as Setup does) and asserts the expected series names
// appear in the /metrics text. These pinned names are mecatl's choice — there is
// no legacy byte-for-byte contract — so a future rename is caught here.
func TestMetricsScrapeThroughPrometheusExporter(t *testing.T) {
	reg := prometheus.NewRegistry()
	exp, err := otelprom.New(otelprom.WithRegisterer(reg))
	if err != nil {
		t.Fatalf("prometheus exporter: %v", err)
	}
	mp := metric.NewMeterProvider(metric.WithReader(exp), metric.WithView(ToolDurationView()))
	m, err := NewMetrics(mp)
	if err != nil {
		t.Fatalf("NewMetrics: %v", err)
	}

	m.Emit(context.Background(), session.Event{Type: session.EvSessionInit})
	m.Emit(context.Background(), session.Event{Type: session.EvPermissionAsk})
	m.Emit(context.Background(), session.Event{Type: session.EvResult, Result: &session.ResultPayload{
		Stop:  session.StopEndTurn,
		Usage: session.Usage{InputTokens: 10, CacheReadTokens: 5},
	}})
	m.ToolCall("s", session.NewToolCall("c", "bash", nil), session.NewToolResult("c", "ok"), 5*time.Millisecond)

	body := scrape(t, MetricsHandler(reg))
	// The prometheus exporter applies its own _total / unit suffixes; these are
	// the names it naturally produces for our instruments.
	for _, name := range []string{
		"mecatl_events_total",
		"mecatl_runs_total",
		"mecatl_tool_calls_total",
		"mecatl_tokens",
		"mecatl_permission_asks_total",
		"mecatl_active_runs",
		"mecatl_cache_hit_ratio",
		"mecatl_tool_duration_seconds",
	} {
		if !strings.Contains(body, name) {
			t.Errorf("/metrics missing series %q", name)
		}
	}
}

// TestMetricsThroughRealSetup drives the production wiring end to end: it calls
// the real Setup (metrics-on, no OTLP endpoint), builds NewMetrics from
// providers.Meter, records events, then scrapes MetricsHandler(providers.Registry).
// This proves (a) the Meter and the Registry returned by Setup are the SAME
// pipeline — a mecatl_* domain series only appears if NewMetrics's meter feeds the
// registry's exporter — and (b) Setup installs the exponential-histogram view, so
// the tool-duration series renders as a native/exponential histogram (a single
// le="+Inf" bucket) rather than the default explicit buckets. Deleting
// ToolDurationView from Setup would regress (b).
func TestMetricsThroughRealSetup(t *testing.T) {
	providers, err := Setup(context.Background(), OTLPConfig{}) // metrics only
	if err != nil {
		t.Fatalf("Setup: %v", err)
	}
	// Shutdown before any (future) leak assertion: the runtime collector and the
	// SDK reader own goroutines that Shutdown stops.
	defer func() { _ = providers.Shutdown(context.Background()) }()

	m, err := NewMetrics(providers.Meter)
	if err != nil {
		t.Fatalf("NewMetrics: %v", err)
	}

	m.Emit(context.Background(), session.Event{Type: session.EvSessionInit})
	m.Emit(context.Background(), session.Event{Type: session.EvResult, Result: &session.ResultPayload{
		Stop:  session.StopEndTurn,
		Usage: session.Usage{InputTokens: 10, CacheReadTokens: 5},
	}})
	m.ToolCall("s", session.NewToolCall("c", "bash", nil),
		session.NewToolResult("c", "ok"), 250*time.Millisecond)

	body := scrape(t, MetricsHandler(providers.Registry))

	// (a) A domain series proves Meter and Registry are the same pipeline.
	if !strings.Contains(body, "mecatl_events_total") {
		t.Errorf("/metrics from real Setup missing mecatl_events_total: Meter/Registry not the same pipeline")
	}
	if !strings.Contains(body, "mecatl_tool_duration_seconds_count") {
		t.Errorf("/metrics missing mecatl_tool_duration_seconds_count")
	}

	// (b) The exponential view collapses the histogram to a single le="+Inf"
	// bucket line. The default explicit-bucket histogram (no view) would instead
	// emit many finite-le bucket lines (le="0.005", le="0.01", …). Counting the
	// _bucket lines discriminates the two and breaks if the view is removed.
	var bucketLines int
	for _, line := range strings.Split(body, "\n") {
		if strings.HasPrefix(line, "mecatl_tool_duration_seconds_bucket{") {
			bucketLines++
			if !strings.Contains(line, `le="+Inf"`) {
				t.Errorf("tool-duration histogram has a finite-le bucket %q; exponential view not installed", line)
			}
		}
	}
	if bucketLines != 1 {
		t.Errorf("tool-duration _bucket lines = %d, want 1 (exponential view); explicit buckets imply the view was dropped", bucketLines)
	}
}

func scrape(t *testing.T, h http.Handler) string {
	t.Helper()
	srv := httptest.NewServer(h)
	defer srv.Close()
	resp, err := http.Get(srv.URL)
	if err != nil {
		t.Fatalf("GET /metrics: %v", err)
	}
	defer func() { _ = resp.Body.Close() }()
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("status = %d, want 200", resp.StatusCode)
	}
	b, err := io.ReadAll(resp.Body)
	if err != nil {
		t.Fatalf("read body: %v", err)
	}
	return string(b)
}

// TestSetupRuntimeCollectorSeries verifies Setup starts the contrib runtime
// collector against its meter provider — go_goroutine_count is the literal series
// the collector emits (from go.goroutine.count via the prometheus exporter).
func TestSetupRuntimeCollectorSeries(t *testing.T) {
	providers, err := Setup(context.Background(), OTLPConfig{}) // no endpoint: metrics only
	if err != nil {
		t.Fatalf("Setup: %v", err)
	}
	defer func() { _ = providers.Shutdown(context.Background()) }()

	if providers.Registry == nil || providers.Meter == nil {
		t.Fatal("Setup returned nil Meter/Registry with metrics always-on")
	}

	body := scrape(t, MetricsHandler(providers.Registry))
	if !strings.Contains(body, "go_goroutine_count") {
		t.Errorf("/metrics missing runtime collector series go_goroutine_count")
	}
}

func TestNewSinkFansOut(t *testing.T) {
	m, _ := newTestMetrics(t)
	c1 := &countingSink{}
	c2 := &countingSink{}

	// A ctx with a recognisable value so we can assert the fan-out forwards the
	// SAME ctx to EVERY wrapped sink, not a fresh background one.
	type ctxKey struct{}
	ctx := context.WithValue(context.Background(), ctxKey{}, "marker")

	sink := NewSink(m, c1, c2)
	sink.Emit(ctx, session.Event{Type: session.EvSessionInit})

	if c1.n != 1 || c2.n != 1 {
		t.Errorf("fan-out sink counts = %d,%d, want 1,1", c1.n, c2.n)
	}
	for i, c := range []*countingSink{c1, c2} {
		if c.lastCtx == nil || c.lastCtx.Value(ctxKey{}) != "marker" {
			t.Errorf("sink %d did not receive the forwarded ctx (got %v)", i, c.lastCtx)
		}
	}
}

type countingSink struct {
	n       int
	lastCtx context.Context //nolint:containedctx // test double records the forwarded ctx for assertion
}

func (c *countingSink) Emit(ctx context.Context, _ session.Event) {
	c.n++
	c.lastCtx = ctx
}
