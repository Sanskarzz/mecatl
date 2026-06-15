package main

import (
	"testing"

	"github.com/stacklok/mecatl/perf/kpi"
)

// pointByName looks a point up in a suite, returning it and whether it was found.
func pointByName(points []benchPoint, name string) (benchPoint, bool) {
	for _, p := range points {
		if p.Name == name {
			return p, true
		}
	}
	return benchPoint{}, false
}

func TestConvert_CacheHitWhitelist(t *testing.T) {
	// Every scenario emits the three smaller-suite metrics; ONLY the whitelisted
	// scenarios (single_session_long, team_fanout) emit a bigger-suite cache-hit
	// point. compaction_cycle and the tui_* benches must be ABSENT from bigger
	// even though they carry a CacheHitRate field (their honest 0 is by design).
	rows := []kpi.ScenarioResult{
		{SchemaVersion: kpi.SchemaVersion, Name: "single_session_long", Sample: 0, AllocsPerOp: 36000, TokensInput: 100, TokensOutput: 50, CacheHitRate: 0.90},
		{SchemaVersion: kpi.SchemaVersion, Name: "team_fanout", Sample: 0, AllocsPerOp: 2600, TokensInput: 40, TokensOutput: 10, CacheHitRate: 0.75},
		{SchemaVersion: kpi.SchemaVersion, Name: "compaction_cycle", Sample: 0, AllocsPerOp: 4200, TokensInput: 30, TokensOutput: 5, CacheHitRate: 0},
		{SchemaVersion: kpi.SchemaVersion, Name: "tui_scrollback_view", Sample: 0, AllocsPerOp: 6200, CacheHitRate: 0},
	}

	smaller, bigger := convert(rows)

	// bigger: exactly the two whitelisted scenarios, nothing else.
	if _, ok := pointByName(bigger, "single_session_long/cache_hit_rate"); !ok {
		t.Error("single_session_long/cache_hit_rate missing from bigger suite")
	}
	if _, ok := pointByName(bigger, "team_fanout/cache_hit_rate"); !ok {
		t.Error("team_fanout/cache_hit_rate missing from bigger suite")
	}
	if _, ok := pointByName(bigger, "compaction_cycle/cache_hit_rate"); ok {
		t.Error("compaction_cycle/cache_hit_rate must NOT be in the bigger suite (by-design 0)")
	}
	if _, ok := pointByName(bigger, "tui_scrollback_view/cache_hit_rate"); ok {
		t.Error("tui_scrollback_view/cache_hit_rate must NOT be in the bigger suite (token-less)")
	}
	if len(bigger) != 2 {
		t.Errorf("bigger suite has %d points, want 2 (the whitelist)", len(bigger))
	}

	// A whitelisted scenario whose cache hit rate regressed to 0 STILL emits a
	// point (the allowlist, not a >0 heuristic, guarantees the gate can fail).
	rowsRegressed := []kpi.ScenarioResult{
		{SchemaVersion: kpi.SchemaVersion, Name: "single_session_long", Sample: 0, CacheHitRate: 0},
	}
	_, biggerReg := convert(rowsRegressed)
	p, ok := pointByName(biggerReg, "single_session_long/cache_hit_rate")
	if !ok {
		t.Fatal("a whitelisted scenario must emit a cache-hit point even at 0")
	}
	if p.Value != 0 {
		t.Errorf("regressed cache hit value = %v, want 0", p.Value)
	}

	// smaller: every scenario emits allocs/op, tokens_total, goroutine_delta.
	for _, name := range []string{"single_session_long", "team_fanout", "compaction_cycle", "tui_scrollback_view"} {
		for _, suffix := range []string{"/allocs_per_op", "/tokens_total", "/goroutine_delta"} {
			if _, ok := pointByName(smaller, name+suffix); !ok {
				t.Errorf("smaller suite missing %s%s", name, suffix)
			}
		}
	}
	// tokens_total is the sum of input+output.
	if tp, _ := pointByName(smaller, "single_session_long/tokens_total"); tp.Value != 150 {
		t.Errorf("single_session_long/tokens_total = %v, want 150 (100+50)", tp.Value)
	}
}

func TestConvert_MedianAcrossSamples(t *testing.T) {
	// Three samples of one scenario with differing allocs; the gated value is the
	// MEDIAN, so an outlier sample cannot move it.
	rows := []kpi.ScenarioResult{
		{SchemaVersion: kpi.SchemaVersion, Name: "team_fanout", Sample: 0, AllocsPerOp: 2600},
		{SchemaVersion: kpi.SchemaVersion, Name: "team_fanout", Sample: 1, AllocsPerOp: 2700},
		{SchemaVersion: kpi.SchemaVersion, Name: "team_fanout", Sample: 2, AllocsPerOp: 9999}, // outlier
	}
	smaller, _ := convert(rows)
	p, ok := pointByName(smaller, "team_fanout/allocs_per_op")
	if !ok {
		t.Fatal("team_fanout/allocs_per_op missing")
	}
	if p.Value != 2700 {
		t.Errorf("median allocs = %v, want 2700 (median of 2600,2700,9999)", p.Value)
	}

	// Even count → mean of the two middle values.
	rowsEven := []kpi.ScenarioResult{
		{SchemaVersion: kpi.SchemaVersion, Name: "team_fanout", Sample: 0, AllocsPerOp: 100},
		{SchemaVersion: kpi.SchemaVersion, Name: "team_fanout", Sample: 1, AllocsPerOp: 200},
		{SchemaVersion: kpi.SchemaVersion, Name: "team_fanout", Sample: 2, AllocsPerOp: 300},
		{SchemaVersion: kpi.SchemaVersion, Name: "team_fanout", Sample: 3, AllocsPerOp: 400},
	}
	smallerEven, _ := convert(rowsEven)
	pe, _ := pointByName(smallerEven, "team_fanout/allocs_per_op")
	if pe.Value != 250 {
		t.Errorf("even median = %v, want 250 (mean of 200,300)", pe.Value)
	}
}

func TestMedianFloat(t *testing.T) {
	cases := []struct {
		in   []float64
		want float64
	}{
		{nil, 0},
		{[]float64{5}, 5},
		{[]float64{3, 1, 2}, 2},
		{[]float64{4, 2}, 3},
		{[]float64{9999, 2600, 2700}, 2700},
	}
	for _, c := range cases {
		if got := medianFloat(c.in); got != c.want {
			t.Errorf("medianFloat(%v) = %v, want %v", c.in, got, c.want)
		}
	}
}

func TestReadResults_SchemaMismatch(t *testing.T) {
	// A row with the wrong schema version is a HARD error (drift between producer
	// and converter); readResults must reject the whole file rather than
	// mis-aggregate.
	dir := t.TempDir()
	path := dir + "/bad.json"
	if err := kpi.WriteJSON(path, []kpi.ScenarioResult{
		{SchemaVersion: kpi.SchemaVersion + 1, Name: "single_session_long"},
	}); err != nil {
		t.Fatal(err)
	}
	if _, err := readResults(path); err == nil {
		t.Fatal("readResults accepted a schema-version mismatch; want a hard error")
	}
}

func TestReadResults_OK(t *testing.T) {
	dir := t.TempDir()
	path := dir + "/good.json"
	want := []kpi.ScenarioResult{
		{SchemaVersion: kpi.SchemaVersion, Name: "single_session_long", AllocsPerOp: 100},
	}
	if err := kpi.WriteJSON(path, want); err != nil {
		t.Fatal(err)
	}
	got, err := readResults(path)
	if err != nil {
		t.Fatalf("readResults: %v", err)
	}
	if len(got) != 1 || got[0].Name != "single_session_long" {
		t.Errorf("readResults round-trip mismatch: %+v", got)
	}
}
