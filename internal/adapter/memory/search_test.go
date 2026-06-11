package memory

import (
	"reflect"
	"testing"

	"github.com/stacklok/mecatl/engine/tool"
)

func TestTokenize(t *testing.T) {
	cases := []struct {
		in   string
		want []string
	}{
		{"pref/test-runner", []string{"pref", "test", "runner"}},
		{"Preferred Test Runner", []string{"preferred", "test", "runner"}},
		{"  multiple   spaces  ", []string{"multiple", "spaces"}},
		{"camelCase123 and_under", []string{"camelcase123", "and", "under"}},
		{"", nil},
		{"!!!---", nil},
	}
	for _, c := range cases {
		got := tokenize(c.in)
		if len(got) == 0 && len(c.want) == 0 {
			continue
		}
		if !reflect.DeepEqual(got, c.want) {
			t.Errorf("tokenize(%q) = %v, want %v", c.in, got, c.want)
		}
	}
}

func sampleEntries() []tool.MemoryEntry {
	return []tool.MemoryEntry{
		{Key: "pref/test-runner", Description: "preferred test runner", Value: "Run tests with gotestsum"},
		{Key: "project/deploy-gate", Description: "deploy gate", Value: "staging deploy needs manual approval"},
		{Key: "pref/editor", Description: "favourite editor", Value: "vim"},
		{Key: "project/tracker", Description: "issue tracker", Value: "issues live in Linear"},
	}
}

func keysOf(entries []tool.MemoryEntry) []string {
	out := make([]string, len(entries))
	for i, e := range entries {
		out[i] = e.Key
	}
	return out
}

func TestBM25RankRelevantFirst(t *testing.T) {
	got := bm25Rank(sampleEntries(), "preferred test runner", 10)
	if len(got) == 0 {
		t.Fatal("expected at least one ranked result")
	}
	if got[0].Key != "pref/test-runner" {
		t.Errorf("top result = %q, want pref/test-runner; full order %v", got[0].Key, keysOf(got))
	}
}

func TestBM25RankDeterministicTieBreak(t *testing.T) {
	entries := sampleEntries()
	a := bm25Rank(entries, "preferred test runner editor deploy", 10)
	b := bm25Rank(entries, "preferred test runner editor deploy", 10)
	if !reflect.DeepEqual(keysOf(a), keysOf(b)) {
		t.Errorf("ranking not deterministic across runs:\n%v\n%v", keysOf(a), keysOf(b))
	}
}

func TestBM25RankOmitsValueFillsDescription(t *testing.T) {
	got := bm25Rank(sampleEntries(), "preferred test runner", 10)
	for _, e := range got {
		if e.Value != "" {
			t.Errorf("ranked entry %q leaked a value: %q", e.Key, e.Value)
		}
		if e.Description == "" {
			t.Errorf("ranked entry %q has no description", e.Key)
		}
	}
}

func TestBM25RankDerivesDescriptionFromValue(t *testing.T) {
	// An entry with no explicit description must get one derived from its value's
	// first line, exactly like Index.
	entries := []tool.MemoryEntry{
		{Key: "k", Value: "first relevant line\nsecond line"},
	}
	got := bm25Rank(entries, "relevant", 10)
	if len(got) != 1 {
		t.Fatalf("got %d results, want 1", len(got))
	}
	if got[0].Description != "first relevant line" {
		t.Errorf("derived description = %q, want %q", got[0].Description, "first relevant line")
	}
}

// TestBM25RankScoresValueOnlyTerm proves the Value contributes to scoring, not
// just the key and description. The query term "approval" appears ONLY in the
// value of entry "k" (not in its key or derived description, and not anywhere in
// the unrelated entries). If `+ " " + e.Value` were dropped from the scoring
// corpus in search.go, "k" would score zero and be filtered out, leaving no
// results. We assert exactly one result, and that it is "k".
func TestBM25RankScoresValueOnlyTerm(t *testing.T) {
	entries := []tool.MemoryEntry{
		{Key: "k", Description: "deploy gate", Value: "staging needs manual approval"},
		{Key: "pref/editor", Description: "favourite editor", Value: "vim"},
		{Key: "project/tracker", Description: "issue tracker", Value: "issues live in Linear"},
	}
	got := bm25Rank(entries, "approval", 10)
	if len(got) != 1 {
		t.Fatalf("got %d results, want exactly 1 (the value-only match); keys %v", len(got), keysOf(got))
	}
	if got[0].Key != "k" {
		t.Errorf("match = %q, want k (the value-only term must be scored)", got[0].Key)
	}
}

// TestBM25RankUniversalTermNonNegative guards the deliberate non-negative IDF
// form (log(1 + (N-df+0.5)/(df+0.5))) against a regression to the classic
// log((N-df+0.5)/(df+0.5)) form, which goes NEGATIVE when a term appears in more
// than half the corpus (and is exactly zero when it appears in every entry). A
// term present in EVERY entry must still yield a positive score so the `s > 0`
// gate keeps every entry rather than silently dropping them all.
func TestBM25RankUniversalTermNonNegative(t *testing.T) {
	entries := []tool.MemoryEntry{
		{Key: "a/one", Description: "alpha", Value: "project alpha notes"},
		{Key: "b/two", Description: "beta", Value: "project beta notes"},
		{Key: "c/three", Description: "gamma", Value: "project gamma notes"},
		{Key: "d/four", Description: "delta", Value: "project delta notes"},
	}
	got := bm25Rank(entries, "project", 10)
	if len(got) == 0 {
		t.Fatal("a term present in every entry was dropped (IDF went non-positive)")
	}
	if len(got) != len(entries) {
		t.Errorf("got %d results, want all %d entries returned for a universal term; keys %v",
			len(got), len(entries), keysOf(got))
	}
}

func TestBM25RankZeroOverlapDropped(t *testing.T) {
	got := bm25Rank(sampleEntries(), "kubernetes helm operator", 10)
	if len(got) != 0 {
		t.Errorf("zero-overlap query should drop all entries, got %v", keysOf(got))
	}
}

func TestBM25RankEmptyQuery(t *testing.T) {
	if got := bm25Rank(sampleEntries(), "", 10); got != nil {
		t.Errorf("empty query should return nil, got %v", keysOf(got))
	}
	if got := bm25Rank(sampleEntries(), "   ", 10); got != nil {
		t.Errorf("whitespace query should return nil, got %v", keysOf(got))
	}
}

func TestBM25RankNoEntries(t *testing.T) {
	if got := bm25Rank(nil, "anything", 10); got != nil {
		t.Errorf("no entries should return nil, got %v", keysOf(got))
	}
}

func TestBM25RankLimitClamp(t *testing.T) {
	// k <= 0 falls back to the default; here only one entry overlaps so we check
	// the clamp does not error and respects an explicit small limit.
	entries := sampleEntries()
	got := bm25Rank(entries, "pref project test runner editor deploy gate tracker", 2)
	if len(got) > 2 {
		t.Errorf("limit 2 returned %d results", len(got))
	}
	// k <= 0 uses the default and should not panic.
	_ = bm25Rank(entries, "pref test", 0)
	_ = bm25Rank(entries, "pref test", -5)
	// k above the ceiling is clamped (no error/panic).
	_ = bm25Rank(entries, "pref test", maxSearchK+100)
}
