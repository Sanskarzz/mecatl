package mcpperf

import (
	"testing"

	"github.com/google/pprof/profile"
	dto "github.com/prometheus/client_model/go"
	"google.golang.org/protobuf/proto"
)

// buildProfile assembles a tiny *profile.Profile with one "cpu" sample dimension.
// Each spec is (functionName, absoluteFilePath, flatValue, label). The label is
// attached to the sample to prove labels are dropped by the reducers.
type funcSpec struct {
	name  string
	file  string
	value int64
	label string
}

func buildProfile(sampleType string, specs []funcSpec) *profile.Profile {
	p := &profile.Profile{
		SampleType: []*profile.ValueType{{Type: sampleType, Unit: "nanoseconds"}},
	}
	for i, s := range specs {
		fn := &profile.Function{
			ID:       uint64(i + 1),
			Name:     s.name,
			Filename: s.file,
		}
		loc := &profile.Location{
			ID:   uint64(i + 1),
			Line: []profile.Line{{Function: fn, Line: 10}},
		}
		p.Function = append(p.Function, fn)
		p.Location = append(p.Location, loc)
		sample := &profile.Sample{
			Location: []*profile.Location{loc},
			Value:    []int64{s.value},
		}
		if s.label != "" {
			sample.Label = map[string][]string{"prompt": {s.label}}
		}
		p.Sample = append(p.Sample, sample)
	}
	return p
}

func TestTopFunctionsRanksAndRedacts(t *testing.T) {
	prof := buildProfile("cpu", []funcSpec{
		{name: "hot", file: "/home/secret-user/workspace/mecatl/hot.go", value: 300, label: "leak this prompt token"},
		{name: "warm", file: "/home/secret-user/workspace/mecatl/warm.go", value: 200},
		{name: "cold", file: "/home/secret-user/workspace/mecatl/cold.go", value: 100},
	})

	top := topFunctions(prof, "cpu", 2)
	if len(top) != 2 {
		t.Fatalf("expected 2 rows, got %d: %+v", len(top), top)
	}
	if top[0].Function != "hot" || top[1].Function != "warm" {
		t.Errorf("ranking wrong: got %q, %q", top[0].Function, top[1].Function)
	}
	if top[0].FlatValue != 300 {
		t.Errorf("hot flat = %d, want 300", top[0].FlatValue)
	}

	// REDACTION: file must be the basename only, never the absolute path.
	if top[0].File != "hot.go" {
		t.Errorf("file not basenamed: got %q, want hot.go", top[0].File)
	}
	for _, row := range top {
		if containsAny(row.File, "/home", "secret-user", "workspace") {
			t.Errorf("absolute path leaked into File: %q", row.File)
		}
		if containsAny(row.Function, "prompt", "leak this") {
			t.Errorf("label leaked into Function: %q", row.Function)
		}
	}
}

func TestMemTopRanksAndTotals(t *testing.T) {
	prof := buildProfile("inuse_space", []funcSpec{
		{name: "bigAlloc", file: "/abs/path/big.go", value: 4096},
		{name: "smallAlloc", file: "/abs/path/small.go", value: 512},
	})

	total, top := memTop(prof, 10)
	if total != 4096+512 {
		t.Errorf("total = %d, want %d", total, 4096+512)
	}
	if len(top) != 2 || top[0].Function != "bigAlloc" {
		t.Fatalf("unexpected top: %+v", top)
	}
	if top[0].FlatBytes != 4096 {
		t.Errorf("bigAlloc flat = %d, want 4096", top[0].FlatBytes)
	}
	if top[0].File != "big.go" {
		t.Errorf("file not basenamed: %q", top[0].File)
	}
}

func TestHistogramQuantilesClassicBuckets(t *testing.T) {
	// Classic cumulative buckets: 10 observations spread so p50→0.05, p90→0.5, p99→1.0.
	mf := classicHistogram("test_seconds", []float64{0.05, 0.1, 0.5, 1.0}, []uint64{5, 6, 9, 10})

	count, bounds := histogramQuantiles(mf, "", 0.5, 0.9, 0.99)
	if count != 10 {
		t.Fatalf("count = %d, want 10", count)
	}
	want := map[float64]float64{0.5: 0.05, 0.9: 0.5, 0.99: 1.0}
	for _, b := range bounds {
		if got := want[b.Quantile]; got != b.UpperBound {
			t.Errorf("q%.2f upper bound = %v, want %v", b.Quantile, b.UpperBound, got)
		}
	}
}

func TestHistogramQuantilesNativeExponentialBuckets(t *testing.T) {
	// Native exponential histogram — the representation the OTel prometheus
	// exporter ACTUALLY emits for the latency instruments (classic buckets are
	// not what runs in production). This exercises nativeLadder, which had zero
	// coverage. Schema 0 → base = 2^(2^-0) = 2, so native bucket index k covers
	// (2^(k-1), 2^k] and we report 2^k as the upper bound.
	//
	// One zero-count observation (the cumulative floor) plus three positive
	// buckets. PositiveSpan{Offset:1, Length:3} ⇒ indices 1,2,3.
	// PositiveDelta is delta-encoded: [2, 1, -1] ⇒ running counts 2, 3, 2.
	schema := int32(0)
	htype := dto.MetricType_HISTOGRAM
	mf := &dto.MetricFamily{
		Name: proto.String("native_seconds"),
		Type: &htype,
		Metric: []*dto.Metric{{Histogram: &dto.Histogram{
			SampleCount:   proto.Uint64(8), // 1 zero + (2+3+2) positive = 8
			ZeroCount:     proto.Uint64(1),
			Schema:        proto.Int32(schema),
			PositiveSpan:  []*dto.BucketSpan{{Offset: proto.Int32(1), Length: proto.Uint32(3)}},
			PositiveDelta: []int64{2, 1, -1},
		}}},
	}

	// Cumulative ladder, floor = ZeroCount = 1:
	//   index 1: cum 1+2=3, upper 2^1 = 2
	//   index 2: cum 3+3=6, upper 2^2 = 4
	//   index 3: cum 6+2=8, upper 2^3 = 8
	count, bounds := histogramQuantiles(mf, "", 0.1, 0.5, 0.9, 0.99)
	if count != 8 {
		t.Fatalf("native count = %d, want 8", count)
	}
	// target = ceil(q*count):
	//   p10 → ceil(0.8)=1   → first cum≥1 is index1 → 2
	//   p50 → ceil(4)=4     → first cum≥4 is index2 → 4
	//   p90 → ceil(7.2)=8   → first cum≥8 is index3 → 8
	//   p99 → ceil(7.92)=8  → index3 → 8
	want := map[float64]float64{0.1: 2, 0.5: 4, 0.9: 8, 0.99: 8}
	got := map[float64]float64{}
	for _, b := range bounds {
		got[b.Quantile] = b.UpperBound
	}
	for q, w := range want {
		if g, ok := got[q]; !ok || g != w {
			t.Errorf("native q%.2f upper bound = %v (present=%v), want %v (base^k reconstruction wrong)", q, g, ok, w)
		}
	}
}

func TestHistogramQuantilesEmptyAndNonHistogram(t *testing.T) {
	if c, b := histogramQuantiles(nil, "", 0.5); c != 0 || b != nil {
		t.Errorf("nil family: got count=%d bounds=%v", c, b)
	}
	gaugeType := dto.MetricType_GAUGE
	mf := &dto.MetricFamily{Name: proto.String("g"), Type: &gaugeType}
	if c, _ := histogramQuantiles(mf, "", 0.5); c != 0 {
		t.Errorf("gauge family: count = %d, want 0", c)
	}
}

// classicHistogram builds a HISTOGRAM MetricFamily with one metric whose buckets
// have the given cumulative counts at the given upper bounds. The last
// cumulative count is taken as the sample count.
func classicHistogram(name string, uppers []float64, cums []uint64) *dto.MetricFamily {
	htype := dto.MetricType_HISTOGRAM
	buckets := make([]*dto.Bucket, 0, len(uppers))
	for i := range uppers {
		buckets = append(buckets, &dto.Bucket{
			UpperBound:      proto.Float64(uppers[i]),
			CumulativeCount: proto.Uint64(cums[i]),
		})
	}
	total := cums[len(cums)-1]
	return &dto.MetricFamily{
		Name: proto.String(name),
		Type: &htype,
		Metric: []*dto.Metric{
			{Histogram: &dto.Histogram{
				SampleCount: proto.Uint64(total),
				Bucket:      buckets,
			}},
		},
	}
}

// assertLadderSane checks the structural invariants every merged ladder must
// hold: cumulative counts and upper bounds both non-decreasing, the final
// cumulative count equal to wantTotal, and each requested quantile's upper
// bound inside the ladder's own bound range.
func assertLadderSane(t *testing.T, ladder []ladderPoint, wantTotal uint64, quantiles ...float64) {
	t.Helper()
	if len(ladder) == 0 {
		t.Fatal("merged ladder is empty")
	}
	for i := 1; i < len(ladder); i++ {
		if ladder[i].upper <= ladder[i-1].upper {
			t.Errorf("ladder bounds not strictly ascending at %d: %v then %v", i, ladder[i-1].upper, ladder[i].upper)
		}
		if ladder[i].cum < ladder[i-1].cum {
			t.Errorf("ladder cumulative counts not monotone at %d: %d then %d", i, ladder[i-1].cum, ladder[i].cum)
		}
	}
	if got := ladder[len(ladder)-1].cum; got != wantTotal {
		t.Errorf("merged total count = %d, want %d (the sum of the input ladders)", got, wantTotal)
	}
	lo, hi := ladder[0].upper, ladder[len(ladder)-1].upper
	for _, q := range quantiles {
		ub := quantileUpperBound(ladder, wantTotal, q)
		if ub < lo || ub > hi {
			t.Errorf("q%.2f upper bound %v outside the data range [%v, %v]", q, ub, lo, hi)
		}
	}
}

// TestMergeLaddersClassicDifferentBounds merges two CLASSIC ladders whose bucket
// bounds differ and interleave — the layout the role-split series can produce
// when two exporters/views bucket the same instrument differently. The merge
// must pool the de-cumulated buckets over the UNION of bounds, never assume
// identical ladders.
func TestMergeLaddersClassicDifferentBounds(t *testing.T) {
	l1 := []ladderPoint{{upper: 0.1, cum: 2}, {upper: 0.4, cum: 5}, {upper: 1.0, cum: 7}}
	l2 := []ladderPoint{{upper: 0.2, cum: 3}, {upper: 0.5, cum: 4}, {upper: 2.0, cum: 9}}

	merged := mergeLadders([][]ladderPoint{l1, l2})
	assertLadderSane(t, merged, 7+9, 0.5, 0.9, 0.99)

	// Exact expectation: per-bucket counts pooled over the union of bounds,
	// re-cumulated. l1 de-cumulates to {0.1:2, 0.4:3, 1.0:2}; l2 to
	// {0.2:3, 0.5:1, 2.0:5}.
	want := []ladderPoint{
		{upper: 0.1, cum: 2}, {upper: 0.2, cum: 5}, {upper: 0.4, cum: 8},
		{upper: 0.5, cum: 9}, {upper: 1.0, cum: 11}, {upper: 2.0, cum: 16},
	}
	if len(merged) != len(want) {
		t.Fatalf("merged ladder = %+v, want %+v", merged, want)
	}
	for i := range want {
		if merged[i] != want[i] {
			t.Errorf("merged[%d] = %+v, want %+v", i, merged[i], want[i])
		}
	}
	// p50: target ceil(8) = 8 → first cum ≥ 8 is 0.4.
	if got := quantileUpperBound(merged, 16, 0.5); got != 0.4 {
		t.Errorf("merged p50 upper bound = %v, want 0.4", got)
	}
}

// TestMergeLaddersNativeDifferentSchemas merges two NATIVE exponential ladders
// of DIFFERENT Schema (different bases ⇒ different, non-aligned bucket bounds) —
// the "upper-bound honest" path: every observation must stay attributed to a
// bound at or above its own bucket's, with the total count preserved and the
// quantiles inside the pooled data range.
func TestMergeLaddersNativeDifferentSchemas(t *testing.T) {
	// Schema 0 ⇒ base 2: spans offset 1 length 2, deltas [2,1] ⇒ buckets
	// idx1:2 (upper 2), idx2:3 (upper 4). No zero bucket. Count 5.
	h1 := &dto.Histogram{
		SampleCount:   proto.Uint64(5),
		Schema:        proto.Int32(0),
		PositiveSpan:  []*dto.BucketSpan{{Offset: proto.Int32(1), Length: proto.Uint32(2)}},
		PositiveDelta: []int64{2, 1},
	}
	// Schema 1 ⇒ base 2^(1/2): spans offset 2 length 3, deltas [1,1,1] ⇒ running
	// counts 1,2,3 at indices 2,3,4 (uppers √2²=2, √2³≈2.83, √2⁴=4), plus a
	// zero-bucket floor of 1. Count 7.
	h2 := &dto.Histogram{
		SampleCount:   proto.Uint64(7),
		ZeroCount:     proto.Uint64(1),
		Schema:        proto.Int32(1),
		PositiveSpan:  []*dto.BucketSpan{{Offset: proto.Int32(2), Length: proto.Uint32(3)}},
		PositiveDelta: []int64{1, 1, 1},
	}

	l1 := nativeLadder(h1)
	l2 := nativeLadder(h2)
	if got := l1[len(l1)-1].cum; got != 5 {
		t.Fatalf("ladder 1 total = %d, want 5", got)
	}
	if got := l2[len(l2)-1].cum; got != 7 {
		t.Fatalf("ladder 2 total = %d, want 7", got)
	}

	merged := mergeLadders([][]ladderPoint{l1, l2})
	assertLadderSane(t, merged, 5+7, 0.5, 0.9, 0.99)

	// Upper-bound honesty at the tail: p99 (target 12, the very last
	// observation) must land on the pooled maximum bound, 4 (= 2² = √2⁴, modulo
	// float rounding in the base^k reconstruction).
	p99 := quantileUpperBound(merged, 12, 0.99)
	if p99 < 3.999 || p99 > 4.001 {
		t.Errorf("merged p99 upper bound = %v, want ≈4 (the pooled max bound)", p99)
	}
}

func containsAny(s string, subs ...string) bool {
	for _, sub := range subs {
		if len(sub) > 0 && indexOf(s, sub) >= 0 {
			return true
		}
	}
	return false
}

func indexOf(s, sub string) int {
	for i := 0; i+len(sub) <= len(s); i++ {
		if s[i:i+len(sub)] == sub {
			return i
		}
	}
	return -1
}
