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
	// sumGauge marks a GAUGE family whose per-role series are summable counts
	// (active_runs: main=1 + subagent=3 means 4 runs in flight), so the
	// unfiltered scalar is the SUM of the per-role values rather than the max.
	// Ratio-like gauges (cache_hit_ratio) stay on the max reduction — summing
	// ratios across roles would be meaningless.
	sumGauge bool
}

// curatedMetrics is the allowlist of metrics this server summarizes. Histograms
// are surfaced as count + quantile upper bounds; counters/gauges as scalar series.
// Keeping this curated bounds the output and keeps an unbounded /metrics scrape
// out of the model's context.
var curatedMetrics = []curatedMetric{
	{name: "tool_duration_seconds", family: "mecatl_tool_duration_seconds", desc: "Per-tool execution wall-clock latency (histogram, seconds)."},
	{name: "turn_duration_seconds", family: "mecatl_turn_duration_seconds", desc: "Per-turn model-call wall-clock latency (histogram, seconds)."},
	{name: "ttft_seconds", family: "mecatl_ttft_seconds", desc: "Time to first content token per turn (histogram, seconds)."},
	{name: "inter_token_seconds", family: "mecatl_inter_token_seconds", desc: "Mean inter-token gap per turn (histogram, seconds)."},
	{name: "inter_token_max_seconds", family: "mecatl_inter_token_max_seconds", desc: "Worst inter-token gap per turn (histogram, seconds)."},
	{name: "tool_queue_seconds", family: "mecatl_tool_queue_seconds", desc: "Tool dispatch queue time (histogram, seconds)."},
	{name: "events_total", family: "mecatl_events_total", desc: "Total domain events observed, by type (counter)."},
	{name: "runs_total", family: "mecatl_runs_total", desc: "Total runs finished, by stop reason (counter)."},
	{name: "turns_total", family: "mecatl_turns_total", desc: "Total turns completed (the per-turn denominator) (counter)."},
	{name: "turn_empty_total", family: "mecatl_turn_empty_total", desc: "Empty turns (no tool call, no text) — the empty SUBSET of turns_total; counts EvNoProgress emissions, up to MaxNoProgressNudges+1 per stuck sequence (counter)."},
	{name: "tool_calls_total", family: "mecatl_tool_calls_total", desc: "Total tool calls executed, by tool and error outcome (counter)."},
	// NOTE: the OTel prometheus exporter appends the counter _total suffix to the
	// GATHERED family name too (mecatl.tokens → mecatl_tokens_total); the short
	// name stays "tokens".
	{name: "tokens", family: "mecatl_tokens_total", desc: "Total tokens accounted, by kind (counter)."},
	{name: "permission_asks_total", family: "mecatl_permission_asks_total", desc: "Total permission.ask events (counter)."},
	{name: "active_runs", family: "mecatl_active_runs", desc: "Runs currently in flight (gauge).", sumGauge: true},
	{name: "cache_hit_ratio", family: "mecatl_cache_hit_ratio", desc: "Prompt-cache hit ratio of the most recent run (gauge)."},
	{name: "process_rss_bytes", family: "mecatl_process_rss_bytes", desc: "Process resident set size in bytes (gauge, Linux)."},
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

// roleFamilies is the CLOSED set of role label values the telemetry plane emits
// (issue #47). It is the validation allowlist for query_metric's and
// list_slow_turns' optional role filter — anything else is rejected with a
// recovery message, since no series can ever carry it. Every member must be a
// role family some engine actually produces (an unmatchable filter value is a
// model trap), which is why there is no "fork" entry.
var roleFamilies = []string{"main", "subagent", "member", "parallel", "usermodel", "child"}

// isRoleFamily reports whether s is one of the closed role family values.
func isRoleFamily(s string) bool {
	for _, r := range roleFamilies {
		if s == r {
			return true
		}
	}
	return false
}

// MetricSummaryEntry is one curated metric's reduced view in the metrics-summary
// resource: for a histogram, its count and quantile upper bounds; for a
// scalar (counter/gauge), its single value. Exactly one of Quantiles/Value is
// meaningful per Kind. ByRole, when present, is the bounded per-role breakdown
// (one row per role family observed on the family's series).
type MetricSummaryEntry struct {
	Name        string          `json:"name"`
	Description string          `json:"description"`
	Kind        string          `json:"kind"` // "histogram" | "scalar" | "absent"
	Count       uint64          `json:"count,omitempty"`
	Quantiles   []QuantileBound `json:"quantiles,omitempty"`
	Value       float64         `json:"value,omitempty"`
	ByRole      []RoleBreakdown `json:"by_role,omitempty"`
}

// RoleBreakdown is one role family's share of a curated metric: the observation
// count for a histogram, or the summed value for a counter. The role dimension
// is intrinsically bounded (the closed roleFamilies set), so the breakdown can
// never grow past a handful of rows.
type RoleBreakdown struct {
	Role  string  `json:"role"`
	Count uint64  `json:"count,omitempty"`
	Value float64 `json:"value,omitempty"`
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

// roleBreakdownMetrics names the curated SCALAR metrics that additionally carry
// the per-role breakdown in the summary (the histograms all carry it). tokens
// and tool_calls_total answer "which delegation family is burning the budget";
// active_runs answers "which family is in flight right now".
var roleBreakdownMetrics = map[string]bool{
	"tool_calls_total": true,
	"tokens":           true,
	"active_runs":      true,
	"turns_total":      true,
	"turn_empty_total": true,
}

// metricsSummary builds the curated metrics-summary payload: one entry per
// curated metric, histograms reduced to count + p50/p90/p99 upper bounds and
// scalars to their value, plus a bounded per-role breakdown for the histograms
// and the tool_calls_total/tokens counters. Absent families are reported with
// Kind "absent" so the consumer sees the full curated set rather than guessing
// which are missing.
func metricsSummary(d Deps) ([]MetricSummaryEntry, error) {
	byName, gerr := gatherByName(d)
	out := make([]MetricSummaryEntry, 0, len(curatedMetrics))
	for _, c := range curatedMetrics {
		entry := MetricSummaryEntry{Name: c.name, Description: c.desc, Kind: "absent"}
		if mf := byName[c.family]; mf != nil {
			fillEntry(&entry, c, mf, "", defaultMetricQuantiles)
			if entry.Kind == "histogram" || roleBreakdownMetrics[c.name] {
				entry.ByRole = roleBreakdown(mf)
			}
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
// count + quantile bounds (aggregated across all series, or the role-filtered
// subset when role is non-empty); everything else collapses to a scalar value
// under the same optional role filter. The curatedMetric carries the per-metric
// reduction knob (sumGauge) the scalar path needs.
func fillEntry(entry *MetricSummaryEntry, c curatedMetric, mf *dto.MetricFamily, role string, quantiles []float64) {
	if mf.GetType() == dto.MetricType_HISTOGRAM {
		count, bounds := histogramQuantiles(mf, role, quantiles...)
		entry.Kind = "histogram"
		entry.Count = count
		entry.Quantiles = bounds
		return
	}
	entry.Kind = "scalar"
	entry.Value = scalarValue(mf, role, c.sumGauge)
}

// roleBreakdown reduces a family to its bounded per-role rows: for a histogram,
// each role's observation count; for a scalar family, each role's summed value.
// Families with no role-labelled series yield nil (the breakdown is omitted).
// Boundedness is ENFORCED, not assumed: seriesRoles admits only the closed
// roleFamilies set, so a rogue label value on a gathered series can never grow
// the row count past the family cardinality.
func roleBreakdown(mf *dto.MetricFamily) []RoleBreakdown {
	roles := seriesRoles(mf)
	if len(roles) == 0 {
		return nil
	}
	out := make([]RoleBreakdown, 0, len(roles))
	for _, r := range roles {
		row := RoleBreakdown{Role: r}
		if mf.GetType() == dto.MetricType_HISTOGRAM {
			row.Count, _ = histogramQuantiles(mf, r)
		} else {
			// Within one role the sum-vs-max gauge knob is moot (the filter
			// leaves that role's series only), so the default reduction is fine.
			row.Value = scalarValue(mf, r, false)
		}
		out = append(out, row)
	}
	return out
}

// scalarValue extracts a single scalar from a counter/gauge family. Counters
// SUM across label series (e.g. mecatl_events_total has one series per type).
// Gauges reduce per the role-split series layout the telemetry adapter emits
// (one series per role family, since role is the only gauge label): the value
// of each ROLE is the max within that role's series (one series ⇒ its value),
// and across roles the family reduces by SUM when sumGauge is set (active_runs:
// main=1 + subagent=3 ⇒ 4 runs in flight — max would under-report 3) or by max
// otherwise (cache_hit_ratio, where a cross-role sum is meaningless). A
// non-empty role restricts the reduction to the series whose "role" label
// equals it. It never enumerates per-label series — that would reintroduce
// unbounded output.
func scalarValue(mf *dto.MetricFamily, role string, sumGauge bool) float64 {
	var sum float64
	// Per-role gauge maxima: series without a role label pool under "".
	gaugeByRole := map[string]float64{}
	for _, m := range mf.GetMetric() {
		if role != "" && metricRole(m) != role {
			continue
		}
		switch {
		case m.GetCounter() != nil:
			sum += m.GetCounter().GetValue()
		case m.GetGauge() != nil:
			r := metricRole(m)
			if v := m.GetGauge().GetValue(); v > gaugeByRole[r] {
				gaugeByRole[r] = v
			}
		case m.GetUntyped() != nil:
			sum += m.GetUntyped().GetValue()
		}
	}
	if mf.GetType() == dto.MetricType_GAUGE {
		var total, maxGauge float64
		for _, v := range gaugeByRole {
			total += v
			if v > maxGauge {
				maxGauge = v
			}
		}
		if sumGauge && role == "" {
			return total
		}
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
