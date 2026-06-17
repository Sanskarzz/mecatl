package mcpperf

import (
	"encoding/json"
	"strings"
	"testing"
	"time"

	dto "github.com/prometheus/client_model/go"
	"google.golang.org/protobuf/proto"
)

// histSeriesWithRole builds one labelled histogram series (classic cumulative
// buckets) carrying a role label, for the multi-series families the role-split
// telemetry now produces.
func histSeriesWithRole(role string, uppers []float64, cums []uint64) *dto.Metric {
	buckets := make([]*dto.Bucket, 0, len(uppers))
	for i := range uppers {
		buckets = append(buckets, &dto.Bucket{
			UpperBound:      proto.Float64(uppers[i]),
			CumulativeCount: proto.Uint64(cums[i]),
		})
	}
	return &dto.Metric{
		Label: []*dto.LabelPair{{Name: proto.String("role"), Value: proto.String(role)}},
		Histogram: &dto.Histogram{
			SampleCount: proto.Uint64(cums[len(cums)-1]),
			Bucket:      buckets,
		},
	}
}

// counterSeriesWithLabels builds one labelled counter series.
func counterSeriesWithLabels(value float64, labels map[string]string) *dto.Metric {
	m := &dto.Metric{Counter: &dto.Counter{Value: proto.Float64(value)}}
	for k, v := range labels {
		m.Label = append(m.Label, &dto.LabelPair{Name: proto.String(k), Value: proto.String(v)})
	}
	return m
}

// gaugeSeriesWithLabels builds one labelled gauge series.
func gaugeSeriesWithLabels(value float64, labels map[string]string) *dto.Metric {
	m := &dto.Metric{Gauge: &dto.Gauge{Value: proto.Float64(value)}}
	for k, v := range labels {
		m.Label = append(m.Label, &dto.LabelPair{Name: proto.String(k), Value: proto.String(v)})
	}
	return m
}

// roleSplitHistogram is a tool-duration family with TWO role series:
// role=main  — buckets {0.1:2, 0.5:3, 1.0:4}, count 4
// role=subagent — buckets {0.1:1, 0.5:5, 1.0:6}, count 6
// Aggregated: count 10, merged ladder {0.1:3, 0.5:8, 1.0:10}.
func roleSplitHistogram() *dto.MetricFamily {
	htype := dto.MetricType_HISTOGRAM
	return &dto.MetricFamily{
		Name: proto.String("mecatl_tool_duration_seconds"),
		Type: &htype,
		Metric: []*dto.Metric{
			histSeriesWithRole("main", []float64{0.1, 0.5, 1.0}, []uint64{2, 3, 4}),
			histSeriesWithRole("subagent", []float64{0.1, 0.5, 1.0}, []uint64{1, 5, 6}),
		},
	}
}

// roleGatherer serves role-split families: the histogram above plus
// role-labelled tool_calls_total and tokens counters.
type roleGatherer struct{}

func (roleGatherer) Gather() ([]*dto.MetricFamily, error) {
	ctype := dto.MetricType_COUNTER
	toolCalls := &dto.MetricFamily{
		Name: proto.String("mecatl_tool_calls_total"),
		Type: &ctype,
		Metric: []*dto.Metric{
			counterSeriesWithLabels(5, map[string]string{"role": "main", "tool": "Edit"}),
			counterSeriesWithLabels(9, map[string]string{"role": "subagent", "tool": "Read"}),
			counterSeriesWithLabels(2, map[string]string{"role": "member", "tool": "Read"}),
		},
	}
	tokens := &dto.MetricFamily{
		Name: proto.String("mecatl_tokens_total"),
		Type: &ctype,
		Metric: []*dto.Metric{
			counterSeriesWithLabels(70, map[string]string{"role": "main", "kind": "input"}),
			counterSeriesWithLabels(300, map[string]string{"role": "subagent", "kind": "input"}),
		},
	}
	gtype := dto.MetricType_GAUGE
	// One in-flight main run plus three concurrent subagent children: the
	// unfiltered total in flight is 4 (sum across role series, NOT the max 3).
	activeRuns := &dto.MetricFamily{
		Name: proto.String("mecatl_active_runs"),
		Type: &gtype,
		Metric: []*dto.Metric{
			gaugeSeriesWithLabels(1, map[string]string{"role": "main"}),
			gaugeSeriesWithLabels(3, map[string]string{"role": "subagent"}),
		},
	}
	// Per-turn counters, role-split. turns_total carries ONLY the role label
	// (the stop label was dropped — EvTurnEnd has no stop reason).
	turns := &dto.MetricFamily{
		Name: proto.String("mecatl_turns_total"),
		Type: &ctype,
		Metric: []*dto.Metric{
			counterSeriesWithLabels(12, map[string]string{"role": "main"}),
			counterSeriesWithLabels(8, map[string]string{"role": "subagent"}),
		},
	}
	turnEmpty := &dto.MetricFamily{
		Name: proto.String("mecatl_turn_empty_total"),
		Type: &ctype,
		Metric: []*dto.Metric{
			counterSeriesWithLabels(2, map[string]string{"role": "main"}),
			counterSeriesWithLabels(1, map[string]string{"role": "subagent"}),
		},
	}
	return []*dto.MetricFamily{roleSplitHistogram(), toolCalls, tokens, activeRuns, turns, turnEmpty}, nil
}

// TestHistogramQuantilesAggregatesAllRoleSeries guards the metrics[0] bug at
// the reducer level: with two role series, the count is the SUM across series
// and the quantile ladder is the MERGE — reading only the first series would
// report count 4 (main only) and main's distribution.
func TestHistogramQuantilesAggregatesAllRoleSeries(t *testing.T) {
	mf := roleSplitHistogram()

	count, bounds := histogramQuantiles(mf, "", 0.5, 0.9, 0.99)
	if count != 10 {
		t.Fatalf("aggregated count = %d, want 10 (sum across role series; 4 means only metrics[0] was read)", count)
	}
	// Merged cumulative ladder {0.1:3, 0.5:8, 1.0:10}:
	//   p50 → ceil(5) = 5  → first cum≥5 is 0.5
	//   p90 → ceil(9) = 9  → 1.0
	//   p99 → ceil(9.9)=10 → 1.0
	want := map[float64]float64{0.5: 0.5, 0.9: 1.0, 0.99: 1.0}
	got := map[float64]float64{}
	for _, b := range bounds {
		got[b.Quantile] = b.UpperBound
	}
	for q, w := range want {
		if g, ok := got[q]; !ok || g != w {
			t.Errorf("aggregated q%.2f upper bound = %v (present=%v), want %v", q, g, ok, w)
		}
	}

	// A role filter narrows to that series alone.
	subCount, subBounds := histogramQuantiles(mf, "subagent", 0.5)
	if subCount != 6 {
		t.Errorf("role-filtered count = %d, want 6", subCount)
	}
	// subagent ladder {0.1:1, 0.5:5, 1.0:6}: p50 → ceil(3)=3 → 0.5.
	if len(subBounds) != 1 || subBounds[0].UpperBound != 0.5 {
		t.Errorf("role-filtered p50 = %+v, want upper bound 0.5", subBounds)
	}
}

// TestQueryMetricHistogramAggregatesAllRoleSeries proves the aggregation holds
// THROUGH the tool surface: query_metric with no role reports the cross-role
// total, never silently one role's series.
func TestQueryMetricHistogramAggregatesAllRoleSeries(t *testing.T) {
	d := fullDeps()
	d.Gatherer = roleGatherer{}
	sess := dialTestServer(t, d)

	res := callTool(t, sess, "query_metric", QueryMetricInput{MetricName: "tool_duration_seconds"})
	if res.IsError {
		t.Fatalf("query_metric isError: %s", resultText(res))
	}
	var out QueryMetricOutput
	decodeStructured(t, res, &out)
	if out.Metric == nil || out.Metric.Count != 10 {
		t.Fatalf("aggregated metric = %+v, want count 10 across both role series (4 means the metrics[0] bug is back)", out.Metric)
	}
	// The unfiltered histogram read carries the bounded per-role breakdown.
	gotRoles := map[string]uint64{}
	for _, r := range out.Metric.ByRole {
		gotRoles[r.Role] = r.Count
	}
	if gotRoles["main"] != 4 || gotRoles["subagent"] != 6 {
		t.Errorf("by_role breakdown = %+v, want main:4 subagent:6", out.Metric.ByRole)
	}
}

// TestQueryMetricRoleFilter covers the optional role filter on both shapes: a
// histogram narrowed to one role family, a counter narrowed likewise, and the
// closed-set rejection of an unknown role.
func TestQueryMetricRoleFilter(t *testing.T) {
	d := fullDeps()
	d.Gatherer = roleGatherer{}
	sess := dialTestServer(t, d)

	// Histogram, role=subagent → that series only.
	hist := callTool(t, sess, "query_metric", QueryMetricInput{MetricName: "tool_duration_seconds", Role: "subagent"})
	if hist.IsError {
		t.Fatalf("histogram role filter isError: %s", resultText(hist))
	}
	var histOut QueryMetricOutput
	decodeStructured(t, hist, &histOut)
	if histOut.Metric == nil || histOut.Metric.Count != 6 {
		t.Errorf("tool_duration_seconds{role=subagent} = %+v, want count 6", histOut.Metric)
	}
	if len(histOut.Metric.ByRole) != 0 {
		t.Errorf("role-filtered read carries a by_role breakdown %+v; it should be omitted (the entry already IS one role)", histOut.Metric.ByRole)
	}

	// Counter, role=subagent → that role's sum only.
	cnt := callTool(t, sess, "query_metric", QueryMetricInput{MetricName: "tool_calls_total", Role: "subagent"})
	if cnt.IsError {
		t.Fatalf("counter role filter isError: %s", resultText(cnt))
	}
	var cntOut QueryMetricOutput
	decodeStructured(t, cnt, &cntOut)
	if cntOut.Metric == nil || cntOut.Metric.Value != 9 {
		t.Errorf("tool_calls_total{role=subagent} = %+v, want value 9", cntOut.Metric)
	}

	// Counter with NO role still sums every series (5+9+2) and carries the
	// breakdown (tool_calls_total is one of the curated breakdown counters).
	all := callTool(t, sess, "query_metric", QueryMetricInput{MetricName: "tool_calls_total"})
	var allOut QueryMetricOutput
	decodeStructured(t, all, &allOut)
	if allOut.Metric == nil || allOut.Metric.Value != 16 {
		t.Errorf("tool_calls_total aggregate = %+v, want value 16", allOut.Metric)
	}
	gotRoles := map[string]float64{}
	for _, r := range allOut.Metric.ByRole {
		gotRoles[r.Role] = r.Value
	}
	if gotRoles["main"] != 5 || gotRoles["subagent"] != 9 || gotRoles["member"] != 2 {
		t.Errorf("tool_calls_total by_role = %+v, want main:5 subagent:9 member:2", allOut.Metric.ByRole)
	}

	// An unknown role is rejected with a recovery message naming the closed set.
	bad := callTool(t, sess, "query_metric", QueryMetricInput{MetricName: "tool_duration_seconds", Role: "sess-deadbeef"})
	if !bad.IsError {
		t.Fatalf("unknown role should be isError")
	}
	if !strings.Contains(resultText(bad), "subagent") {
		t.Errorf("unknown-role recovery text should list the valid roles: %s", resultText(bad))
	}
}

// TestActiveRunsGaugeSumsAcrossRoles guards the multi-role gauge under-report:
// active_runs has one gauge series per role family (main=1, subagent=3), so the
// unfiltered scalar must be the SUM across roles (4) — the old max reduction
// reported 3, silently dropping the main run — while a role filter still reads
// that one role's value, and the unfiltered read carries the per-role rows.
func TestActiveRunsGaugeSumsAcrossRoles(t *testing.T) {
	d := fullDeps()
	d.Gatherer = roleGatherer{}
	sess := dialTestServer(t, d)

	all := callTool(t, sess, "query_metric", QueryMetricInput{MetricName: "active_runs"})
	if all.IsError {
		t.Fatalf("active_runs isError: %s", resultText(all))
	}
	var allOut QueryMetricOutput
	decodeStructured(t, all, &allOut)
	if allOut.Metric == nil || allOut.Metric.Value != 4 {
		t.Errorf("active_runs unfiltered = %+v, want value 4 (1 main + 3 subagent; 3 means the max reduction is back)", allOut.Metric)
	}
	gotRoles := map[string]float64{}
	for _, r := range allOut.Metric.ByRole {
		gotRoles[r.Role] = r.Value
	}
	if gotRoles["main"] != 1 || gotRoles["subagent"] != 3 {
		t.Errorf("active_runs by_role = %+v, want main:1 subagent:3", allOut.Metric.ByRole)
	}

	for role, want := range map[string]float64{"main": 1, "subagent": 3} {
		res := callTool(t, sess, "query_metric", QueryMetricInput{MetricName: "active_runs", Role: role})
		if res.IsError {
			t.Fatalf("active_runs{role=%s} isError: %s", role, resultText(res))
		}
		var out QueryMetricOutput
		decodeStructured(t, res, &out)
		if out.Metric == nil || out.Metric.Value != want {
			t.Errorf("active_runs{role=%s} = %+v, want %v", role, out.Metric, want)
		}
	}

	// cache_hit_ratio keeps the max reduction: summing ratios across roles would
	// be meaningless, so the sum applies ONLY to the sumGauge-marked family.
	gtype := dto.MetricType_GAUGE
	ratio := &dto.MetricFamily{
		Name: proto.String("mecatl_cache_hit_ratio"),
		Type: &gtype,
		Metric: []*dto.Metric{
			gaugeSeriesWithLabels(0.5, map[string]string{"role": "main"}),
			gaugeSeriesWithLabels(0.8, map[string]string{"role": "subagent"}),
		},
	}
	if got := scalarValue(ratio, "", false); got != 0.8 {
		t.Errorf("cache_hit_ratio unfiltered = %v, want 0.8 (max, never the cross-role sum)", got)
	}
}

// TestRoleBreakdownExcludesRogueLabels proves the per-role breakdown is BOUNDED
// by construction: a series carrying a role value outside the closed
// roleFamilies set (a foreign series on the shared registry) yields no by_role
// row, so the breakdown can never grow past the family cardinality.
func TestRoleBreakdownExcludesRogueLabels(t *testing.T) {
	ctype := dto.MetricType_COUNTER
	mf := &dto.MetricFamily{
		Name: proto.String("mecatl_tool_calls_total"),
		Type: &ctype,
		Metric: []*dto.Metric{
			counterSeriesWithLabels(5, map[string]string{"role": "main"}),
			counterSeriesWithLabels(9, map[string]string{"role": "subagent"}),
			counterSeriesWithLabels(1, map[string]string{"role": "sess-deadbeef0123"}),
			counterSeriesWithLabels(2, map[string]string{"role": "task:secret-def"}),
		},
	}
	roles := seriesRoles(mf)
	if len(roles) != 2 || roles[0] != "main" || roles[1] != "subagent" {
		t.Errorf("seriesRoles = %v, want [main subagent] (rogue labels must be excluded)", roles)
	}
	rows := roleBreakdown(mf)
	for _, r := range rows {
		if !isRoleFamily(r.Role) {
			t.Errorf("roleBreakdown emitted a non-family row %+v; the closed set must bound the output", r)
		}
	}
	if len(rows) != 2 {
		t.Errorf("roleBreakdown = %+v, want exactly the 2 closed-family rows", rows)
	}
}

// TestMetricsSummaryCarriesRoleBreakdown asserts the perf://metrics/summary
// payload gains the bounded per-role rows for the histograms and the
// tool_calls_total/tokens counters.
func TestMetricsSummaryCarriesRoleBreakdown(t *testing.T) {
	d := fullDeps()
	d.Gatherer = roleGatherer{}

	entries, err := metricsSummary(d)
	if err != nil {
		t.Fatalf("metricsSummary: %v", err)
	}
	byName := map[string]MetricSummaryEntry{}
	for _, e := range entries {
		byName[e.Name] = e
	}

	hist := byName["tool_duration_seconds"]
	if hist.Kind != "histogram" || hist.Count != 10 {
		t.Fatalf("tool_duration_seconds = %+v, want aggregated histogram count 10", hist)
	}
	roles := map[string]uint64{}
	for _, r := range hist.ByRole {
		roles[r.Role] = r.Count
	}
	if roles["main"] != 4 || roles["subagent"] != 6 {
		t.Errorf("tool_duration_seconds by_role = %+v, want main:4 subagent:6", hist.ByRole)
	}

	calls := byName["tool_calls_total"]
	if calls.Kind != "scalar" || calls.Value != 16 {
		t.Fatalf("tool_calls_total = %+v, want scalar 16", calls)
	}
	callRoles := map[string]float64{}
	for _, r := range calls.ByRole {
		callRoles[r.Role] = r.Value
	}
	if callRoles["main"] != 5 || callRoles["subagent"] != 9 || callRoles["member"] != 2 {
		t.Errorf("tool_calls_total by_role = %+v", calls.ByRole)
	}

	tokens := byName["tokens"]
	if tokens.Kind != "scalar" || tokens.Value != 370 {
		t.Fatalf("tokens = %+v, want scalar 370", tokens)
	}
	tokenRoles := map[string]float64{}
	for _, r := range tokens.ByRole {
		tokenRoles[r.Role] = r.Value
	}
	if tokenRoles["main"] != 70 || tokenRoles["subagent"] != 300 {
		t.Errorf("tokens by_role = %+v, want main:70 subagent:300", tokens.ByRole)
	}

	// active_runs is in the breakdown set too, with the cross-role SUM as its
	// scalar (1 main + 3 subagent in flight).
	active := byName["active_runs"]
	if active.Kind != "scalar" || active.Value != 4 {
		t.Fatalf("active_runs = %+v, want scalar 4 (sum across role series)", active)
	}
	activeRoles := map[string]float64{}
	for _, r := range active.ByRole {
		activeRoles[r.Role] = r.Value
	}
	if activeRoles["main"] != 1 || activeRoles["subagent"] != 3 {
		t.Errorf("active_runs by_role = %+v, want main:1 subagent:3", active.ByRole)
	}

	// turns_total / turn_empty_total are in the breakdown set too: the per-turn
	// denominator and the empty-turn numerator, each split per role family.
	turns := byName["turns_total"]
	if turns.Kind != "scalar" || turns.Value != 20 {
		t.Fatalf("turns_total = %+v, want scalar 20 (12 main + 8 subagent)", turns)
	}
	turnRoles := map[string]float64{}
	for _, r := range turns.ByRole {
		turnRoles[r.Role] = r.Value
	}
	if turnRoles["main"] != 12 || turnRoles["subagent"] != 8 {
		t.Errorf("turns_total by_role = %+v, want main:12 subagent:8", turns.ByRole)
	}

	empty := byName["turn_empty_total"]
	if empty.Kind != "scalar" || empty.Value != 3 {
		t.Fatalf("turn_empty_total = %+v, want scalar 3 (2 main + 1 subagent)", empty)
	}
	emptyRoles := map[string]float64{}
	for _, r := range empty.ByRole {
		emptyRoles[r.Role] = r.Value
	}
	if emptyRoles["main"] != 2 || emptyRoles["subagent"] != 1 {
		t.Errorf("turn_empty_total by_role = %+v, want main:2 subagent:1", empty.ByRole)
	}
}

// TestListSlowTurnsCarriesRole proves the bounded role family rides each slow
// turn over the wire and the optional role filter narrows the page.
func TestListSlowTurnsCarriesRole(t *testing.T) {
	d := fullDeps()
	d.SlowTurns = fakeSlowTurns{turns: []SlowTurn{
		{TurnIndex: 3, DurationMs: 1500, EndedAt: time.Unix(300, 0).UTC(), Role: "subagent"},
		{TurnIndex: 2, DurationMs: 1200, EndedAt: time.Unix(200, 0).UTC(), Role: "main"},
		{TurnIndex: 1, DurationMs: 900, EndedAt: time.Unix(100, 0).UTC(), Role: "member"},
	}}
	sess := dialTestServer(t, d)

	// Unfiltered: every turn carries its role.
	res := callTool(t, sess, "list_slow_turns", ListSlowTurnsInput{Limit: 10})
	if res.IsError {
		t.Fatalf("list_slow_turns isError: %s", resultText(res))
	}
	var out ListSlowTurnsOutput
	decodeStructured(t, res, &out)
	if len(out.Turns) != 3 {
		t.Fatalf("turns = %d, want 3", len(out.Turns))
	}
	if out.Turns[0].Role != "subagent" || out.Turns[1].Role != "main" || out.Turns[2].Role != "member" {
		t.Errorf("roles = %q,%q,%q, want subagent,main,member", out.Turns[0].Role, out.Turns[1].Role, out.Turns[2].Role)
	}
	// The role rides the wire as the snake_case "role" key.
	raw, err := json.Marshal(res.StructuredContent)
	if err != nil {
		t.Fatalf("marshal structuredContent: %v", err)
	}
	if !strings.Contains(string(raw), `"role":"subagent"`) {
		t.Errorf("raw slow-turn JSON missing role key: %s", raw)
	}

	// Filtered to one family.
	filtered := callTool(t, sess, "list_slow_turns", ListSlowTurnsInput{Limit: 10, Role: "main"})
	var fOut ListSlowTurnsOutput
	decodeStructured(t, filtered, &fOut)
	if fOut.TotalCount != 1 || len(fOut.Turns) != 1 || fOut.Turns[0].Role != "main" {
		t.Errorf("role-filtered page = %+v (total %d), want exactly the main turn", fOut.Turns, fOut.TotalCount)
	}

	// Unknown role rejected with the closed set named.
	bad := callTool(t, sess, "list_slow_turns", ListSlowTurnsInput{Role: "sess-1234"})
	if !bad.IsError {
		t.Fatalf("unknown role should be isError")
	}
	if !strings.Contains(resultText(bad), "subagent") {
		t.Errorf("unknown-role recovery text should list the valid roles: %s", resultText(bad))
	}
}
