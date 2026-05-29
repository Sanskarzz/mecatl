package skills

import (
	"sort"
	"strings"
)

// Jaccard2Gram returns the 2-gram (character-bigram) Jaccard similarity between
// a and b, in [0,1]: |A∩B| / |A∪B| over the SET of adjacent character pairs of
// each (case-folded, whitespace-collapsed). It is an offline, dependency-free,
// near-oracle novelty signal — no embeddings, no network — used to flag a
// candidate skill's description as a near-duplicate of an existing one.
//
// Identical strings score 1.0; strings with no shared bigram score 0.0. Two
// strings shorter than one bigram each (e.g. both empty) score 1.0 when equal,
// 0.0 otherwise.
func Jaccard2Gram(a, b string) float64 {
	ga := bigrams(a)
	gb := bigrams(b)
	if len(ga) == 0 && len(gb) == 0 {
		if normalizeForGram(a) == normalizeForGram(b) {
			return 1.0
		}
		return 0.0
	}
	if len(ga) == 0 || len(gb) == 0 {
		return 0.0
	}
	inter := 0
	for g := range ga {
		if _, ok := gb[g]; ok {
			inter++
		}
	}
	union := len(ga) + len(gb) - inter
	if union == 0 {
		return 0.0
	}
	return float64(inter) / float64(union)
}

// bigrams returns the set of adjacent rune pairs of s after normalization.
func bigrams(s string) map[string]struct{} {
	n := normalizeForGram(s)
	rs := []rune(n)
	out := make(map[string]struct{}, len(rs))
	for i := 0; i+1 < len(rs); i++ {
		out[string(rs[i:i+2])] = struct{}{}
	}
	return out
}

// normalizeForGram lower-cases s and collapses internal whitespace runs to a
// single space, trimming the ends, so trivial formatting differences do not skew
// the similarity score.
func normalizeForGram(s string) string {
	return strings.Join(strings.Fields(strings.ToLower(s)), " ")
}

// sortStrings sorts in place; a tiny local helper so callers need not import sort.
func sortStrings(s []string) { sort.Strings(s) }
