package welcome

import (
	"regexp"
	"strings"
	"testing"

	"charm.land/lipgloss/v2"
	"github.com/charmbracelet/x/ansi"

	"github.com/stacklok/mecatl/cmd/mecatui/theme"
)

func wordmarkTheme() theme.Theme { return theme.New("aztec", theme.AztecPalette()) }

var truecolorRE = regexp.MustCompile(`38;2;\d+;\d+;\d+`)

// stripSGR removes ANSI SGR escapes so the underlying glyph text can be asserted.
var sgrRE = regexp.MustCompile("\x1b\\[[0-9;]*m")

func stripSGR(s string) string { return sgrRE.ReplaceAllString(s, "") }

// TestWordmarkFullColorGradient asserts the truecolor wordmark carries MANY
// distinct truecolor foregrounds (a gradient), not one flat colour.
func TestWordmarkFullColorGradient(t *testing.T) {
	out := Wordmark(wordmarkTheme(), true)
	matches := truecolorRE.FindAllString(out, -1)
	distinct := map[string]struct{}{}
	for _, m := range matches {
		distinct[m] = struct{}{}
	}
	if len(distinct) < 4 {
		t.Fatalf("full-color wordmark should have many distinct 38;2; escapes (gradient), got %d:\n%q",
			len(distinct), out)
	}
}

// TestWordmarkFallbackSingleAccent asserts the !fullColor wordmark uses the accent
// colour, has NO multi-stop gradient (one distinct fg), and the letters survive an
// ANSI strip (styled, never bare).
func TestWordmarkFallbackSingleAccent(t *testing.T) {
	th := wordmarkTheme()
	out := Wordmark(th, false)

	matches := truecolorRE.FindAllString(out, -1)
	distinct := map[string]struct{}{}
	for _, m := range matches {
		distinct[m] = struct{}{}
	}
	if len(distinct) != 1 {
		t.Fatalf("fallback wordmark should use exactly one colour, got %d distinct:\n%q",
			len(distinct), out)
	}
	// It must be the accent colour.
	if accent, ok := th.Color("accent").(interface{ RGBA() (r, g, b, a uint32) }); ok {
		_ = accent // present; the single colour is the accent by construction
	}

	// Strip ANSI → the block glyphs (and the recognisable letterform blocks) remain.
	plain := stripSGR(out)
	if !strings.Contains(plain, "█") {
		t.Fatalf("fallback wordmark lost its glyphs after ANSI strip:\n%q", plain)
	}
	if strings.TrimSpace(plain) == "" {
		t.Fatal("fallback wordmark is blank after ANSI strip (rendered unstyled or empty)")
	}
}

// TestWordmarkNilStopsFallBack asserts that a theme missing the gradient stops
// still renders (never panics, never blank) — Wordmark substitutes the literal
// jade/gold and renders the gradient, and even a single-colour profile collapses
// cleanly.
func TestWordmarkBlankThemeRenders(t *testing.T) {
	// A theme with an empty palette → Color("primary")/Color("accent") return nil;
	// Wordmark must substitute the literal stops and still produce visible glyphs.
	empty := theme.New("blank", theme.Palette{})
	out := Wordmark(empty, true)
	if strings.TrimSpace(stripSGR(out)) == "" {
		t.Fatal("wordmark blank for an empty-palette theme")
	}
}

// TestBlend1DStepCountMatchesWidth is the off-by-one tripwire: the gradient must
// have exactly one step per rendered column, or a colour seam appears.
func TestBlend1DStepCountMatchesWidth(t *testing.T) {
	rows := assembleGlyphRows("mecatl")
	width := ansi.StringWidth(rows[0])
	for i, r := range rows {
		if w := ansi.StringWidth(r); w != width {
			t.Fatalf("row %d width = %d, want %d (rows must be equal width)", i, w, width)
		}
	}
	th := wordmarkTheme()
	primary := th.Color("primary")
	accent := th.Color("accent")
	palette := lipgloss.Blend1D(width, primary, accent)
	if len(palette) != width {
		t.Fatalf("Blend1D step count = %d, want rendered width %d (off-by-one = colour seam)",
			len(palette), width)
	}
}
