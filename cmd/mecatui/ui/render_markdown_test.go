package ui

import (
	"strings"
	"testing"

	"charm.land/lipgloss/v2"
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

// TestMarkdownReservesFinalColumn locks the streaming-scramble fix: no rendered
// markdown line may occupy the terminal's FINAL column. A glyph in the last
// column arms the terminal's pending-wrap (DECAWM) state, which desyncs the
// differential renderer during a reflowing stream — the stale-cell scramble
// where earlier-frame text bleeds mid-line ("Perfect!" → "…Perfect…").
// trimTrailingSpaces removes glamour's styled padding, but real wrapped CONTENT
// can fill a line to the full width; markdown() must wrap one column short so the
// last column always stays empty (mirroring the Width(r.width-2) inset that keeps
// tool cards from scrambling).
func TestMarkdownReservesFinalColumn(t *testing.T) {
	r := newTestRenderer() // width 100
	// Long single paragraph with no hard breaks, so glamour greedily fills lines
	// right up to the wrap boundary — the case where a wrapped line would otherwise
	// reach the full width and re-arm the pending-wrap trigger.
	src := strings.TrimSpace(strings.Repeat(
		"Perfect now I have a comprehensive understanding of the mecatl repository and "+
			"will save this knowledge before continuing with the next implementation step. ",
		6))
	out := r.markdown(src)
	lines := strings.Split(out, "\n")
	if len(lines) < 2 {
		t.Fatalf("expected the prose to wrap to multiple lines, got %d", len(lines))
	}
	for i, ln := range lines {
		if w := lipgloss.Width(ln); w >= r.width {
			t.Errorf("line %d reaches the final column (visible width %d >= %d): %q",
				i, w, r.width, stripANSIstr(ln))
		}
	}
}

// TestMarkdownAtNeverStale locks the per-block memoization (markdownAt): the
// cached render must always equal a fresh markdown() of the CURRENT src, never a
// stale earlier one. It walks a growing src (a streaming turn) through one index,
// asserting every step matches the uncached render — so the live block re-renders
// on change — and that a repeated identical src is byte-identical (a cache hit,
// not a re-render that could diverge). A second index with different content must
// not be contaminated by the first.
func TestMarkdownAtNeverStale(t *testing.T) {
	r := newTestRenderer()
	steps := []string{"Perfect", "Perfect! Now", "Perfect! Now I have a full picture."}
	for _, src := range steps {
		got := r.markdownAt(0, src)
		want := r.markdown(src)
		if got != want {
			t.Errorf("markdownAt(0, %q) returned stale render:\n got %q\nwant %q",
				src, stripANSIstr(got), stripANSIstr(want))
		}
	}
	// Repeated identical src is a cache hit — must be byte-identical.
	if a, b := r.markdownAt(0, steps[2]), r.markdownAt(0, steps[2]); a != b {
		t.Errorf("repeated markdownAt diverged: %q vs %q", stripANSIstr(a), stripANSIstr(b))
	}
	// A different index with different content is independent.
	other := "A separate block with its own text."
	if got, want := r.markdownAt(1, other), r.markdown(other); got != want {
		t.Errorf("index 1 contaminated by index 0:\n got %q\nwant %q",
			stripANSIstr(got), stripANSIstr(want))
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
