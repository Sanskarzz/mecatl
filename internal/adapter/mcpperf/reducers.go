package mcpperf

import (
	"bytes"
	"math"
	"path/filepath"
	"sort"

	"github.com/google/pprof/profile"
	dto "github.com/prometheus/client_model/go"
)

// parseProfile parses raw pprof bytes (the gzipped protobuf form written by
// runtime/pprof at debug=0) into a *profile.Profile for the reducers. It is a
// thin wrapper over profile.Parse kept beside the reducers that consume it.
func parseProfile(raw []byte) (*profile.Profile, error) {
	return profile.Parse(bytes.NewReader(raw))
}

// FuncStat is one ranked function row from a CPU/timing profile. It carries the
// REDACTED function identity (name + file basename only) plus its flat and
// cumulative values in the profile's own unit (e.g. nanoseconds for a CPU
// profile). No absolute path and no pprof label survives into this struct.
type FuncStat struct {
	// Function is the function's name (pprof Function.Name).
	Function string `json:"function"`
	// File is the BASENAME of the function's source file — the absolute path is
	// dropped deliberately, because it leaks $HOME and workspace layout.
	File string `json:"file"`
	// FlatValue is the value attributed to this function alone (self time/space).
	FlatValue int64 `json:"flat_value"`
	// CumValue is the value attributed to this function and everything it called.
	CumValue int64 `json:"cum_value"`
}

// AllocStat is one ranked allocation row from a heap/allocs profile. It mirrors
// FuncStat but names the value bytes, since the memTop reducer ranks by an
// allocation-space sample index.
type AllocStat struct {
	// Function is the allocating function's name.
	Function string `json:"function"`
	// File is the BASENAME of the allocating function's source file (path dropped).
	File string `json:"file"`
	// FlatBytes is the bytes attributed to this function alone.
	FlatBytes int64 `json:"flat_bytes"`
	// CumBytes is the bytes attributed to this function and its callees.
	CumBytes int64 `json:"cum_bytes"`
}

// funcAgg accumulates flat/cum values for one redacted function identity while
// folding a profile's samples.
type funcAgg struct {
	function string
	file     string
	flat     int64
	cum      int64
}

// baseName returns the basename of a source file path, or "" for an empty path.
// This is the REDACTION that keeps absolute paths (which embed $HOME and the
// workspace root) out of tool output: only the leaf file name is ever exposed.
func baseName(path string) string {
	if path == "" {
		return ""
	}
	return filepath.Base(path)
}

// pickSampleIndex resolves the value index to rank by. It prefers an exact
// SampleType.Type match for want; failing that, it falls back to the profile's
// DefaultSampleType, then to the last sample type (pprof convention: the last
// value is usually the "interesting" one — inuse_space, cpu). It returns -1 only
// for a profile with no sample types.
func pickSampleIndex(prof *profile.Profile, want string) int {
	if prof == nil || len(prof.SampleType) == 0 {
		return -1
	}
	for i, st := range prof.SampleType {
		if st.Type == want {
			return i
		}
	}
	if prof.DefaultSampleType != "" {
		for i, st := range prof.SampleType {
			if st.Type == prof.DefaultSampleType {
				return i
			}
		}
	}
	return len(prof.SampleType) - 1
}

// aggregateByFunction folds a profile's samples into per-function flat/cum
// values for the given value index, applying the REDACTION at the point of
// extraction:
//
//   - the function identity is name + basename(file); the absolute Filename is
//     never read into output;
//   - sample.Label and sample.NumLabel are NEVER read — pprof labels can carry
//     request-correlated data (and, on a flight-recorder-adjacent profile, even
//     argument or prompt fragments), so we drop every label unconditionally.
//
// Flat is attributed to the LEAF function of each sample's location stack; cum is
// attributed to every distinct function appearing anywhere in the stack (so a
// function is not double-counted in cum for a single sample even if it recurses).
func aggregateByFunction(prof *profile.Profile, idx int) []funcAgg {
	if prof == nil || idx < 0 {
		return nil
	}
	byKey := make(map[string]*funcAgg)
	get := func(fn *profile.Function) *funcAgg {
		// Key on name+basename so two functions with the same name in different
		// files stay distinct, while the absolute path is discarded.
		file := ""
		name := ""
		if fn != nil {
			name = fn.Name
			file = baseName(fn.Filename)
		}
		key := name + "\x00" + file
		a := byKey[key]
		if a == nil {
			a = &funcAgg{function: name, file: file}
			byKey[key] = a
		}
		return a
	}

	for _, s := range prof.Sample {
		if idx >= len(s.Value) {
			continue
		}
		v := s.Value[idx]
		if v == 0 || len(s.Location) == 0 {
			continue
		}
		// Flat: the leaf frame of the top-most location's first line.
		if leaf := leafFunction(s.Location[0]); leaf != nil {
			get(leaf).flat += v
		}
		// Cum: each distinct function in the whole stack, counted once per sample.
		seen := make(map[string]struct{})
		for _, loc := range s.Location {
			for _, ln := range loc.Line {
				fn := ln.Function
				file := ""
				name := ""
				if fn != nil {
					name = fn.Name
					file = baseName(fn.Filename)
				}
				key := name + "\x00" + file
				if _, dup := seen[key]; dup {
					continue
				}
				seen[key] = struct{}{}
				get(fn).cum += v
			}
		}
	}

	out := make([]funcAgg, 0, len(byKey))
	for _, a := range byKey {
		out = append(out, *a)
	}
	return out
}

// leafFunction returns the innermost function of a location (the first line), or
// nil if the location has no lines.
func leafFunction(loc *profile.Location) *profile.Function {
	if loc == nil || len(loc.Line) == 0 {
		return nil
	}
	return loc.Line[0].Function
}

// topFunctions ranks a profile's functions by flat value (descending, cum as
// tiebreaker) and returns the top n as redacted FuncStat rows. sampleType selects
// the value dimension to rank by (e.g. "cpu", "samples"); an empty sampleType
// uses the profile's default. n <= 0 yields nil.
func topFunctions(prof *profile.Profile, sampleType string, n int) []FuncStat {
	if n <= 0 {
		return nil
	}
	idx := pickSampleIndex(prof, sampleType)
	aggs := aggregateByFunction(prof, idx)
	sort.Slice(aggs, func(i, j int) bool {
		if aggs[i].flat != aggs[j].flat {
			return aggs[i].flat > aggs[j].flat
		}
		if aggs[i].cum != aggs[j].cum {
			return aggs[i].cum > aggs[j].cum
		}
		return aggs[i].function < aggs[j].function
	})
	if len(aggs) > n {
		aggs = aggs[:n]
	}
	out := make([]FuncStat, 0, len(aggs))
	for _, a := range aggs {
		out = append(out, FuncStat{
			Function:  a.function,
			File:      a.file,
			FlatValue: a.flat,
			CumValue:  a.cum,
		})
	}
	return out
}

// memTop ranks a heap/allocs profile's functions by an allocation-space value
// and returns the top n as redacted AllocStat rows, plus the total flat bytes
// across all functions for that dimension. It prefers an "inuse_space" sample
// type, then "alloc_space", then the profile default. The same redaction as
// topFunctions applies (basename only, labels dropped).
func memTop(prof *profile.Profile, n int) (total int64, top []AllocStat) {
	idx := pickSampleIndex(prof, "inuse_space")
	if prof != nil {
		// Prefer an alloc/inuse space dimension explicitly if present.
		for _, want := range []string{"inuse_space", "alloc_space", "inuse_objects", "alloc_objects"} {
			if i := exactSampleIndex(prof, want); i >= 0 {
				idx = i
				break
			}
		}
	}
	aggs := aggregateByFunction(prof, idx)
	for _, a := range aggs {
		total += a.flat
	}
	sort.Slice(aggs, func(i, j int) bool {
		if aggs[i].flat != aggs[j].flat {
			return aggs[i].flat > aggs[j].flat
		}
		if aggs[i].cum != aggs[j].cum {
			return aggs[i].cum > aggs[j].cum
		}
		return aggs[i].function < aggs[j].function
	})
	if n > 0 && len(aggs) > n {
		aggs = aggs[:n]
	}
	top = make([]AllocStat, 0, len(aggs))
	for _, a := range aggs {
		top = append(top, AllocStat{
			Function:  a.function,
			File:      a.file,
			FlatBytes: a.flat,
			CumBytes:  a.cum,
		})
	}
	return total, top
}

// exactSampleIndex returns the index of the sample type whose Type exactly
// matches want, or -1.
func exactSampleIndex(prof *profile.Profile, want string) int {
	if prof == nil {
		return -1
	}
	for i, st := range prof.SampleType {
		if st.Type == want {
			return i
		}
	}
	return -1
}

// QuantileBound is one (quantile, upper-bound) pair derived from a histogram. The
// field is named UpperBound, not "value", to be HONEST about precision: it is the
// upper bound of the histogram bucket the quantile rank falls in, not an
// interpolated exact quantile.
type QuantileBound struct {
	// Quantile is the requested rank in [0,1] (e.g. 0.99 for p99).
	Quantile float64 `json:"quantile"`
	// UpperBound is the upper bound of the bucket containing that rank, in the
	// histogram's own unit (seconds for the latency instruments).
	UpperBound float64 `json:"upper_bound"`
}

// histogramQuantiles reads a prometheus histogram MetricFamily's FIRST histogram
// metric and returns the total observation count plus, for each requested
// quantile, the upper bound of the bucket the quantile rank falls in. It handles
// both classic explicit-bucket histograms (the dto.Bucket list, cumulative) and
// native exponential histograms (Schema + PositiveSpan/PositiveDelta, as emitted
// by the OTel prometheus exporter for the latency instruments).
//
// Returns count 0 and nil bounds for a nil family, a non-histogram family, or a
// histogram with no observations — the caller turns that into a "no data yet"
// tool result rather than an error.
func histogramQuantiles(mf *dto.MetricFamily, quantiles ...float64) (count uint64, bounds []QuantileBound) {
	if mf == nil || mf.GetType() != dto.MetricType_HISTOGRAM {
		return 0, nil
	}
	metrics := mf.GetMetric()
	if len(metrics) == 0 {
		return 0, nil
	}
	h := metrics[0].GetHistogram()
	if h == nil {
		return 0, nil
	}
	count = h.GetSampleCount()
	if count == 0 {
		return 0, nil
	}

	// Build a cumulative (upperBound, cumulativeCount) ladder from whichever
	// representation the histogram uses.
	ladder := classicLadder(h)
	if len(ladder) == 0 {
		ladder = nativeLadder(h)
	}
	if len(ladder) == 0 {
		return count, nil
	}

	bounds = make([]QuantileBound, 0, len(quantiles))
	for _, q := range quantiles {
		bounds = append(bounds, QuantileBound{
			Quantile:   q,
			UpperBound: quantileUpperBound(ladder, count, q),
		})
	}
	return count, bounds
}

// ladderPoint is one (upperBound, cumulativeCount) step of a cumulative
// histogram, ordered by ascending upperBound.
type ladderPoint struct {
	upper float64
	cum   uint64
}

// classicLadder builds the cumulative ladder from a classic explicit-bucket
// histogram. The dto buckets are already cumulative and increasing. Returns nil
// if the histogram carries no classic buckets (i.e. it is native).
func classicLadder(h *dto.Histogram) []ladderPoint {
	bs := h.GetBucket()
	if len(bs) == 0 {
		return nil
	}
	out := make([]ladderPoint, 0, len(bs))
	for _, b := range bs {
		ub := b.GetUpperBound()
		if math.IsInf(ub, 1) {
			// Fold the +Inf bucket's count into the last finite step rather than
			// reporting +Inf as a bound.
			if len(out) > 0 {
				out[len(out)-1].cum = b.GetCumulativeCount()
			}
			continue
		}
		out = append(out, ladderPoint{upper: ub, cum: b.GetCumulativeCount()})
	}
	return out
}

// nativeLadder reconstructs an ascending cumulative ladder of POSITIVE buckets
// from a native exponential histogram (Schema + PositiveSpan/PositiveDelta). The
// upper bound of native bucket index k at schema s is base^k where
// base = 2^(2^-s). Negative buckets and the zero bucket are ignored: the latency
// instruments only observe non-negative durations, so the positive buckets carry
// the whole distribution.
func nativeLadder(h *dto.Histogram) []ladderPoint {
	spans := h.GetPositiveSpan()
	deltas := h.GetPositiveDelta()
	if len(spans) == 0 || len(deltas) == 0 {
		return nil
	}
	schema := h.GetSchema()
	base := math.Pow(2, math.Pow(2, float64(-schema)))

	type bucket struct {
		index int32
		count uint64
	}
	var buckets []bucket
	var cur int32
	var running int64
	di := 0
	for _, sp := range spans {
		cur += sp.GetOffset()
		for j := uint32(0); j < sp.GetLength(); j++ {
			if di >= len(deltas) {
				break
			}
			running += deltas[di]
			di++
			if running > 0 {
				buckets = append(buckets, bucket{index: cur, count: uint64(running)})
			}
			cur++
		}
	}
	if len(buckets) == 0 {
		return nil
	}
	// Native bucket index k covers (base^(k-1), base^k]; use base^k as the upper
	// bound. Buckets arrive in ascending index order, so a running sum yields the
	// cumulative ladder directly. Include the zero-bucket count as the floor.
	out := make([]ladderPoint, 0, len(buckets))
	cum := h.GetZeroCount()
	for _, b := range buckets {
		cum += b.count
		upper := math.Pow(base, float64(b.index))
		out = append(out, ladderPoint{upper: upper, cum: cum})
	}
	return out
}

// quantileUpperBound returns the upper bound of the first ladder step whose
// cumulative count reaches the quantile's target rank. It is the same
// representative-bucket-bound approach the runtime/metrics snapshot uses, kept
// honest by the QuantileBound.UpperBound field name.
func quantileUpperBound(ladder []ladderPoint, count uint64, q float64) float64 {
	if len(ladder) == 0 || count == 0 {
		return 0
	}
	if q < 0 {
		q = 0
	}
	if q > 1 {
		q = 1
	}
	target := uint64(math.Ceil(q * float64(count)))
	if target == 0 {
		target = 1
	}
	for _, p := range ladder {
		if p.cum >= target {
			return p.upper
		}
	}
	return ladder[len(ladder)-1].upper
}
