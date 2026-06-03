package mcpperf

import (
	"fmt"
	"sort"

	dto "github.com/prometheus/client_model/go"
)

// curatedMetric maps a short, model-friendly metric name to the prometheus
// metric-family name the OTel exporter produces, with a one-line description. The
// curated set is the read budget query_metric and perf://metrics/summary expose —
// the latency histograms plus the small set of useful counters/gauges — never the
// whole /metrics surface.
type curatedMetric struct {
	// name is the short name a client passes to query_metric.
	name string
	// family is the prometheus MetricFamily name produced by the exporter.
	family string
	// desc is a one-line human description.
	desc string
}

// curatedMetrics is the allowlist of metrics this server summarizes. Histograms
// are surfaced as count + quantile upper bounds; counters/gauges as scalar series.
// Keeping this curated bounds the output and keeps an unbounded /metrics scrape
// out of the model's context.
var curatedMetrics = []curatedMetric{
	{"tool_duration_seconds", "mecatl_tool_duration_seconds", "Per-tool execution wall-clock latency (histogram, seconds)."},
	{"turn_duration_seconds", "mecatl_turn_duration_seconds", "Per-turn model-call wall-clock latency (histogram, seconds)."},
	{"ttft_seconds", "mecatl_ttft_seconds", "Time to first content token per turn (histogram, seconds)."},
	{"inter_token_seconds", "mecatl_inter_token_seconds", "Mean inter-token gap per turn (histogram, seconds)."},
	{"inter_token_max_seconds", "mecatl_inter_token_max_seconds", "Worst inter-token gap per turn (histogram, seconds)."},
	{"tool_queue_seconds", "mecatl_tool_queue_seconds", "Tool dispatch queue time (histogram, seconds)."},
	{"events_total", "mecatl_events_total", "Total domain events observed, by type (counter)."},
	{"runs_total", "mecatl_runs_total", "Total runs finished, by stop reason (counter)."},
	{"tool_calls_total", "mecatl_tool_calls_total", "Total tool calls executed, by tool and error outcome (counter)."},
	{"tokens", "mecatl_tokens", "Total tokens accounted, by kind (counter)."},
	{"permission_asks_total", "mecatl_permission_asks_total", "Total permission.ask events (counter)."},
	{"active_runs", "mecatl_active_runs", "Runs currently in flight (gauge)."},
	{"cache_hit_ratio", "mecatl_cache_hit_ratio", "Prompt-cache hit ratio of the most recent run (gauge)."},
	{"process_rss_bytes", "mecatl_process_rss_bytes", "Process resident set size in bytes (gauge, Linux)."},
}

// curatedByName indexes curatedMetrics by short name.
var curatedByName = func() map[string]curatedMetric {
	m := make(map[string]curatedMetric, len(curatedMetrics))
	for _, c := range curatedMetrics {
		m[c.name] = c
	}
	return m
}()

// defaultMetricQuantiles is the histogram quantile set query_metric and the
// metrics summary report when the caller does not request a specific one.
var defaultMetricQuantiles = []float64{0.5, 0.9, 0.99}

// MetricSummaryEntry is one curated metric's reduced view in the metrics-summary
// resource: for a histogram, its count and quantile upper bounds; for a
// scalar (counter/gauge), its single value. Exactly one of Quantiles/Value is
// meaningful per Kind.
type MetricSummaryEntry struct {
	Name        string          `json:"name"`
	Description string          `json:"description"`
	Kind        string          `json:"kind"` // "histogram" | "scalar" | "absent"
	Count       uint64          `json:"count,omitempty"`
	Quantiles   []QuantileBound `json:"quantiles,omitempty"`
	Value       float64         `json:"value,omitempty"`
}

// gatherByName gathers the metric families and indexes them by family name. A
// Gather error is returned to the caller (the tool/resource turns it into a
// failure result); a partial slice on a soft error is still indexed.
func gatherByName(d Deps) (map[string]*dto.MetricFamily, error) {
	families, err := d.Gatherer.Gather()
	byName := make(map[string]*dto.MetricFamily, len(families))
	for _, mf := range families {
		if mf != nil {
			byName[mf.GetName()] = mf
		}
	}
	return byName, err
}

// metricsSummary builds the curated metrics-summary payload: one entry per
// curated metric, histograms reduced to count + p50/p90/p99 upper bounds and
// scalars to their value. Absent families are reported with Kind "absent" so the
// consumer sees the full curated set rather than guessing which are missing.
func metricsSummary(d Deps) ([]MetricSummaryEntry, error) {
	byName, gerr := gatherByName(d)
	out := make([]MetricSummaryEntry, 0, len(curatedMetrics))
	for _, c := range curatedMetrics {
		entry := MetricSummaryEntry{Name: c.name, Description: c.desc, Kind: "absent"}
		if mf := byName[c.family]; mf != nil {
			fillEntry(&entry, mf, defaultMetricQuantiles)
		}
		out = append(out, entry)
	}
	// gerr is surfaced only if it prevented all collection (byName empty); a
	// partial gather still yields useful entries.
	if gerr != nil && len(byName) == 0 {
		return out, fmt.Errorf("mcpperf: gather metrics: %w", gerr)
	}
	return out, nil
}

// fillEntry populates a summary entry from a metric family: a histogram becomes
// count + quantile bounds; everything else collapses to its first metric's
// scalar value (gauge or counter).
func fillEntry(entry *MetricSummaryEntry, mf *dto.MetricFamily, quantiles []float64) {
	if mf.GetType() == dto.MetricType_HISTOGRAM {
		count, bounds := histogramQuantiles(mf, quantiles...)
		entry.Kind = "histogram"
		entry.Count = count
		entry.Quantiles = bounds
		return
	}
	entry.Kind = "scalar"
	entry.Value = scalarValue(mf)
}

// scalarValue extracts a single scalar from a counter/gauge family: it SUMS
// counter values across label series (e.g. mecatl_events_total has one series per
// type) and takes the max gauge value, which is a reasonable single-number
// reduction for a model-facing summary. It never enumerates per-label series —
// that would reintroduce unbounded output.
func scalarValue(mf *dto.MetricFamily) float64 {
	var sum, maxGauge float64
	for _, m := range mf.GetMetric() {
		switch {
		case m.GetCounter() != nil:
			sum += m.GetCounter().GetValue()
		case m.GetGauge() != nil:
			if v := m.GetGauge().GetValue(); v > maxGauge {
				maxGauge = v
			}
		case m.GetUntyped() != nil:
			sum += m.GetUntyped().GetValue()
		}
	}
	if mf.GetType() == dto.MetricType_GAUGE {
		return maxGauge
	}
	return sum
}

// availableMetricNames returns the sorted short names of every curated metric, for
// the query_metric discovery path (called with no metric_name).
func availableMetricNames() []string {
	names := make([]string, 0, len(curatedMetrics))
	for _, c := range curatedMetrics {
		names = append(names, c.name)
	}
	sort.Strings(names)
	return names
}
