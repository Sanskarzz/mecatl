package telemetry

import (
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/prometheus/client_golang/prometheus"
	"github.com/prometheus/client_golang/prometheus/testutil"

	"github.com/stacklok/ozzharness/internal/session"
)

func TestMetricsEventsTotal(t *testing.T) {
	reg := prometheus.NewRegistry()
	m := NewMetrics(reg)

	m.Emit(session.Event{Type: session.EvSessionInit})
	m.Emit(session.Event{Type: session.EvTurnStart, Turn: 0})
	m.Emit(session.Event{Type: session.EvMessageDelta})
	m.Emit(session.Event{Type: session.EvMessageDelta})

	if got := testutil.ToFloat64(m.events.WithLabelValues("session.init")); got != 1 {
		t.Errorf("events_total{session.init} = %v, want 1", got)
	}
	if got := testutil.ToFloat64(m.events.WithLabelValues("message.delta")); got != 2 {
		t.Errorf("events_total{message.delta} = %v, want 2", got)
	}
}

func TestMetricsRunsAndActiveRuns(t *testing.T) {
	reg := prometheus.NewRegistry()
	m := NewMetrics(reg)

	m.Emit(session.Event{Type: session.EvSessionInit})
	if got := testutil.ToFloat64(m.activeRuns); got != 1 {
		t.Fatalf("active_runs after init = %v, want 1", got)
	}
	m.Emit(session.Event{Type: session.EvResult, Result: &session.ResultPayload{Stop: session.StopEndTurn}})
	if got := testutil.ToFloat64(m.activeRuns); got != 0 {
		t.Fatalf("active_runs after result = %v, want 0", got)
	}
	if got := testutil.ToFloat64(m.runs.WithLabelValues("end_turn")); got != 1 {
		t.Errorf("runs_total{end_turn} = %v, want 1", got)
	}
}

func TestMetricsTokensAndCacheRatio(t *testing.T) {
	reg := prometheus.NewRegistry()
	m := NewMetrics(reg)

	m.Emit(session.Event{Type: session.EvResult, Result: &session.ResultPayload{
		Stop: session.StopEndTurn,
		Usage: session.Usage{
			InputTokens:      100,
			OutputTokens:     40,
			CacheReadTokens:  25,
			CacheWriteTokens: 10,
		},
	}})

	cases := map[string]float64{
		"input":       100,
		"output":      40,
		"cache_read":  25,
		"cache_write": 10,
	}
	for kind, want := range cases {
		if got := testutil.ToFloat64(m.tokens.WithLabelValues(kind)); got != want {
			t.Errorf("tokens_total{%s} = %v, want %v", kind, got, want)
		}
	}
	if got := testutil.ToFloat64(m.cacheHit); got != 0.25 {
		t.Errorf("cache_hit_ratio = %v, want 0.25", got)
	}
}

func TestMetricsPermissionAsks(t *testing.T) {
	reg := prometheus.NewRegistry()
	m := NewMetrics(reg)

	m.Emit(session.Event{Type: session.EvPermissionAsk})
	m.Emit(session.Event{Type: session.EvPermissionAsk})

	if got := testutil.ToFloat64(m.permAsks); got != 2 {
		t.Errorf("permission_asks_total = %v, want 2", got)
	}
}

func TestMetricsToolCall(t *testing.T) {
	reg := prometheus.NewRegistry()
	m := NewMetrics(reg)

	m.ToolCall("sess-1", session.NewToolCall("c1", "bash", nil),
		session.NewToolResult("c1", "ok"), 250*time.Millisecond)
	m.ToolCall("sess-1", session.NewToolCall("c2", "bash", nil),
		session.NewToolError("c2", "boom"), 10*time.Millisecond)

	if got := testutil.ToFloat64(m.toolCalls.WithLabelValues("bash", "false")); got != 1 {
		t.Errorf("tool_calls_total{bash,false} = %v, want 1", got)
	}
	if got := testutil.ToFloat64(m.toolCalls.WithLabelValues("bash", "true")); got != 1 {
		t.Errorf("tool_calls_total{bash,true} = %v, want 1", got)
	}
	if n := testutil.CollectAndCount(m.toolDuration, "ozz_tool_duration_seconds"); n != 1 {
		t.Errorf("tool_duration series count = %d, want 1 (one tool)", n)
	}
}

func TestMetricsHandlerServesMetrics(t *testing.T) {
	reg := prometheus.NewRegistry()
	m := NewMetrics(reg)
	m.Emit(session.Event{Type: session.EvSessionInit})

	srv := httptest.NewServer(MetricsHandler(reg))
	defer srv.Close()

	resp, err := http.Get(srv.URL)
	if err != nil {
		t.Fatalf("GET /metrics: %v", err)
	}
	defer func() { _ = resp.Body.Close() }()

	if resp.StatusCode != http.StatusOK {
		t.Fatalf("status = %d, want 200", resp.StatusCode)
	}
	body, err := io.ReadAll(resp.Body)
	if err != nil {
		t.Fatalf("read body: %v", err)
	}
	for _, name := range []string{"ozz_events_total", "ozz_active_runs"} {
		if !strings.Contains(string(body), name) {
			t.Errorf("metrics body missing %q", name)
		}
	}
}

func TestNewSinkFansOut(t *testing.T) {
	reg := prometheus.NewRegistry()
	m := NewMetrics(reg)
	c := &countingSink{}

	sink := NewSink(m, c)
	sink.Emit(session.Event{Type: session.EvSessionInit})

	if c.n != 1 {
		t.Errorf("fan-out sink count = %d, want 1", c.n)
	}
	if got := testutil.ToFloat64(m.events.WithLabelValues("session.init")); got != 1 {
		t.Errorf("metrics not driven by fan-out: events_total{session.init} = %v", got)
	}
}

type countingSink struct{ n int }

func (c *countingSink) Emit(session.Event) { c.n++ }
