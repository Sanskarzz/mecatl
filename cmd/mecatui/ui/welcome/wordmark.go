package welcome

import (
	"image/color"
	"strings"

	"charm.land/lipgloss/v2"
	"github.com/charmbracelet/x/ansi"

	"github.com/stacklok/mecatl/cmd/mecatui/theme"
)

// wordmarkRows is the fixed height of the block-char wordmark — three text rows,
// the most a lowercase set needs while staying compact in a short terminal.
const wordmarkRows = 3

// glyphs are HAND-AUTHORED 3-row block-character letterforms for the six
// lowercase letters of "mecatl" plus a space. Each value is a [3]string whose
// rows are EQUAL width (the glyph's cell width), built from the heavy box-drawing
// blocks "█"/"▀"/"▄" and spaces. Laid side-by-side they spell the wordmark; the
// gradient is applied across the rendered columns afterwards. Only the seven
// runes actually present in "mecatl" + ' ' are defined — an unknown rune falls
// back to a blank cell of space width (see glyphFor).
var glyphs = map[rune][3]string{
	' ': {
		"  ",
		"  ",
		"  ",
	},
	'm': {
		"█▀▄▀▄",
		"█ █ █",
		"█ █ █",
	},
	'e': {
		"▄▀▀▄",
		"█▀▀▀",
		"▀▀▀▀",
	},
	'c': {
		"▄▀▀▄",
		"█   ",
		"▀▀▀▀",
	},
	'a': {
		"▄▀▀▄",
		"▄▀▀█",
		"▀▀▀▀",
	},
	't': {
		" █  ",
		"▀█▀▀",
		" ▀▀▀",
	},
	'l': {
		"█ ",
		"█ ",
		"▀▀",
	},
}

// glyphFor returns the 3-row letterform for r, or the space glyph for any rune
// not in "mecatl ". This keeps Wordmark total over an arbitrary input string,
// though in practice it is only ever called on the literal "mecatl".
func glyphFor(r rune) [3]string {
	if g, ok := glyphs[r]; ok {
		return g
	}
	return glyphs[' ']
}

// glyphSep is the single-column gap laid between adjacent glyphs so the
// letterforms breathe.
const glyphSep = " "

// jadeStop / goldStop are the wordmark gradient endpoints, mirroring the spike's
// Aztec swatches. They are the fallbacks used only when the theme does not supply
// the "primary"/"accent" slots (it always does for the built-in themes); the
// live theme colours take precedence in Wordmark.
const (
	jadeStop = "#1FB39A" // theme "primary"
	goldStop = "#E9B949" // theme "accent"
)

// Wordmark lays the "mecatl" block glyphs side-by-side into three rows and colours
// them. When fullColor is true AND both gradient stops resolve, it applies a
// per-grapheme-cluster jade→gold gradient computed with lipgloss.Blend1D over the
// rendered column count — so the colour sweeps smoothly left to right and the step
// count exactly matches the rendered width (an off-by-one would leave a visible
// seam). Otherwise it falls back to rendering EVERY glyph in the single accent
// colour — never unstyled, never the raw blend. The result is three newline-joined
// rows, no trailing newline.
func Wordmark(th theme.Theme, fullColor bool) string {
	rows := assembleGlyphRows("mecatl")

	primary := th.Color("primary")
	accent := th.Color("accent")
	if primary == nil {
		primary = lipgloss.Color(jadeStop)
	}
	if accent == nil {
		accent = lipgloss.Color(goldStop)
	}

	// Fallback: single accent colour for every glyph. Used when the terminal is
	// not truecolor (a per-column gradient would band into mush on a 256/16-colour
	// profile) or when a stop is missing.
	if !fullColor || primary == nil || accent == nil {
		style := lipgloss.NewStyle().Foreground(accent)
		out := make([]string, wordmarkRows)
		for i, row := range rows {
			out[i] = style.Render(row)
		}
		return strings.Join(out, "\n")
	}

	// The gradient is one column = one step. Every row of the assembled wordmark is
	// the SAME rendered width (assembleGlyphRows pads to equal width), so we measure
	// once and colour each row's clusters against the same palette — the colour of a
	// column is identical across the three rows, giving vertical gradient bands.
	width := ansi.StringWidth(rows[0])
	palette := lipgloss.Blend1D(width, primary, accent)

	out := make([]string, wordmarkRows)
	for i, row := range rows {
		out[i] = colourByColumn(row, palette)
	}
	return strings.Join(out, "\n")
}

// assembleGlyphRows lays the glyphs of s side-by-side (separated by glyphSep) into
// wordmarkRows rows and pads every row to the common width, so all three rows are
// the same rendered width (required for the column-aligned gradient).
func assembleGlyphRows(s string) [wordmarkRows]string {
	var rows [wordmarkRows]string
	// Accumulate each of the three rows as a plain string (a glyph row is short, so
	// the concatenation cost is negligible and keeps a single Builder per row out of
	// an array — gosec can't prove an array element is a Builder).
	first := true
	for _, r := range s {
		g := glyphFor(r)
		for i := 0; i < wordmarkRows; i++ {
			if !first {
				rows[i] += glyphSep
			}
			rows[i] += g[i]
		}
		first = false
	}
	// Equalise widths so the gradient maps cleanly column→colour.
	maxW := 0
	for i := 0; i < wordmarkRows; i++ {
		if w := ansi.StringWidth(rows[i]); w > maxW {
			maxW = w
		}
	}
	for i := 0; i < wordmarkRows; i++ {
		if pad := maxW - ansi.StringWidth(rows[i]); pad > 0 {
			rows[i] += strings.Repeat(" ", pad)
		}
	}
	return rows
}

// colourByColumn colours each grapheme cluster of row with the palette entry for
// its starting column, iterating clusters with ansi.FirstGraphemeCluster so a
// multi-cell or combining glyph maps to the correct column (the same grapheme
// engine ansi.StringWidth uses, so widths agree). A space cluster is emitted
// uncoloured (no escape) so the gradient only paints the visible block glyphs and
// the gaps stay terminal-default — keeping the wordmark's negative space clean.
func colourByColumn(row string, palette []color.Color) string {
	if len(palette) == 0 {
		return row
	}
	var out strings.Builder
	col := 0
	rest := row
	for len(rest) > 0 {
		cluster, w := ansi.FirstGraphemeCluster(rest, ansi.GraphemeWidth)
		rest = rest[len(cluster):]
		if strings.TrimSpace(cluster) == "" {
			out.WriteString(cluster)
			col += w
			continue
		}
		idx := col
		if idx >= len(palette) {
			idx = len(palette) - 1
		}
		out.WriteString(lipgloss.NewStyle().Foreground(palette[idx]).Render(cluster))
		col += w
	}
	return out.String()
}
