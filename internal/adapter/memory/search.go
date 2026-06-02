package memory

import (
	"math"
	"sort"
	"strings"
	"unicode"

	"github.com/stacklok/mecatl/internal/tool"
)

// BM25 ranking constants and search paging bounds. These are local, fixed
// tuning knobs — there is no configuration surface for them (the feature is
// deliberately dependency-free and self-contained).
const (
	// bm25K1 is the BM25 term-frequency saturation parameter. The standard
	// default (1.5) gives diminishing returns as a term repeats within an entry.
	bm25K1 = 1.5
	// bm25B is the BM25 length-normalisation parameter. The standard default
	// (0.75) partially penalises longer entries so a long value cannot dominate
	// purely by accumulating term hits.
	bm25B = 0.75
	// defaultSearchK is the result count used when the caller passes k <= 0.
	defaultSearchK = 10
	// maxSearchK is the hard ceiling on returned results, so a runaway limit
	// cannot dump the whole store back to the model.
	maxSearchK = 50
)

// tokenize lowercases s and splits it into terms on any rune that is neither a
// letter nor a digit, so "pref/test-runner" → ["pref","test","runner"]. There is
// no stemming and no stop-word removal in v1; matching is exact term overlap.
func tokenize(s string) []string {
	return strings.FieldsFunc(strings.ToLower(s), func(r rune) bool {
		return !unicode.IsLetter(r) && !unicode.IsDigit(r)
	})
}

// scored pairs an entry index with its BM25 score, for sorting before we
// materialise the top-k result slice.
type scored struct {
	idx   int
	score float64
}

// bm25Rank ranks entries by BM25 relevance to query and returns the top k
// best-first. The scoring corpus per entry is its key + derived description
// (descriptionOrFirstLine, exactly as Index derives it) + value; the value
// contributes to scoring ONLY — it is zeroed on every returned entry, and the
// returned Description is the derived one (so results mirror Index's shape).
//
// Ordering is total and deterministic: score descending, then key ascending
// (keys are unique, so ties break to a single order). Entries with a zero score
// (no query-term overlap) are dropped. An empty/whitespace query, or a query
// that tokenises to nothing, yields an empty slice. k <= 0 uses defaultSearchK;
// k is clamped to maxSearchK.
func bm25Rank(entries []tool.MemoryEntry, query string, k int) []tool.MemoryEntry {
	queryTerms := uniqueTerms(tokenize(query))
	if len(entries) == 0 || len(queryTerms) == 0 {
		return nil
	}

	switch {
	case k <= 0:
		k = defaultSearchK
	case k > maxSearchK:
		k = maxSearchK
	}

	// Per-entry term-frequency maps and lengths, plus the corpus totals BM25
	// needs (document frequency per term, total length for the average).
	tfs := make([]map[string]int, len(entries))
	lengths := make([]int, len(entries))
	df := make(map[string]int, len(queryTerms))
	totalLen := 0
	for i, e := range entries {
		corpus := e.Key + " " + descriptionOrFirstLine(e.Description, e.Value) + " " + e.Value
		terms := tokenize(corpus)
		lengths[i] = len(terms)
		totalLen += len(terms)
		tf := make(map[string]int, len(terms))
		for _, t := range terms {
			tf[t]++
		}
		tfs[i] = tf
		// Count document frequency only for query terms (the only ones scored);
		// queryTerms is already deduplicated, so each counts once per entry.
		for _, qt := range queryTerms {
			if tf[qt] > 0 {
				df[qt]++
			}
		}
	}

	n := float64(len(entries))
	avgLen := 0.0
	if len(entries) > 0 {
		avgLen = float64(totalLen) / n
	}

	// Precompute the non-negative IDF for each unique query term.
	idf := make(map[string]float64, len(df))
	for _, qt := range queryTerms {
		dfi := float64(df[qt])
		idf[qt] = math.Log(1 + (n-dfi+0.5)/(dfi+0.5))
	}

	results := make([]scored, 0, len(entries))
	for i := range entries {
		var s float64
		for _, qt := range queryTerms {
			f := float64(tfs[i][qt])
			if f == 0 {
				continue
			}
			norm := 1 - bm25B + bm25B*(float64(lengths[i])/avgLen)
			s += idf[qt] * (f * (bm25K1 + 1)) / (f + bm25K1*norm)
		}
		if s > 0 {
			results = append(results, scored{idx: i, score: s})
		}
	}

	// Total order: score desc, then key asc (unique keys ⇒ deterministic).
	sort.Slice(results, func(a, b int) bool {
		if results[a].score != results[b].score {
			return results[a].score > results[b].score
		}
		return entries[results[a].idx].Key < entries[results[b].idx].Key
	})

	if len(results) > k {
		results = results[:k]
	}

	out := make([]tool.MemoryEntry, 0, len(results))
	for _, r := range results {
		e := entries[r.idx]
		out = append(out, tool.MemoryEntry{
			Key:         e.Key,
			Description: descriptionOrFirstLine(e.Description, e.Value),
			UpdatedAt:   e.UpdatedAt,
			// Value omitted by design (like Index); it was used for scoring only.
		})
	}
	return out
}

// uniqueTerms returns the distinct terms of in, preserving first-seen order. It
// is used so a query term repeated by the caller (e.g. "test test") is scored
// once, matching BM25's per-term contribution model.
func uniqueTerms(in []string) []string {
	seen := make(map[string]struct{}, len(in))
	out := make([]string, 0, len(in))
	for _, t := range in {
		if _, ok := seen[t]; ok {
			continue
		}
		seen[t] = struct{}{}
		out = append(out, t)
	}
	return out
}
