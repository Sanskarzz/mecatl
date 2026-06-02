package ui

import (
	"strings"
	"testing"
)

// TestMarkdownNoTrailingPadding locks the trimTrailingSpaces mitigation: glamour
// right-pads every wrapped line out to the full wrap width, which pushes each
// conversation row to the terminal's final column and feeds the differential
// renderer's pending-wrap desync (the stale-cell scramble seen during streaming).
// markdown() must strip that padding so no rendered line carries trailing spaces.
func TestMarkdownNoTrailingPadding(t *testing.T) {
	r := newTestRenderer() // width 100
	// A short paragraph (would otherwise pad to ~100 cols) plus inline code and an
	// em-dash, mirroring the assistant prose in the bug report.
	src := "The edits I made to `AGENTS.md` are committed in `04cca3` — already up to date."
	out := r.markdown(src)
	if strings.TrimSpace(out) == "" {
		t.Fatal("markdown returned empty for non-empty source")
	}
	for i, ln := range strings.Split(out, "\n") {
		if ln != strings.TrimRight(ln, " ") {
			t.Errorf("line %d has trailing padding spaces: %q", i, stripANSIstr(ln))
		}
	}
}

// TestMarkdownPreservesContent guards that stripping trailing padding never eats
// the visible text or its order (the regression we were chasing was scrambled,
// not merely padded, text — this keeps the content path honest).
func TestMarkdownPreservesContent(t *testing.T) {
	r := newTestRenderer()
	src := "First line stays first.\n\nSecond paragraph stays second."
	plain := stripANSIstr(r.markdown(src))
	first := strings.Index(plain, "First line stays first.")
	second := strings.Index(plain, "Second paragraph stays second.")
	if first < 0 || second < 0 {
		t.Fatalf("content lost: %q", plain)
	}
	if first > second {
		t.Errorf("content reordered: first=%d second=%d in %q", first, second, plain)
	}
}
