package welcome

import (
	"fmt"
	"image"
	"strings"
	"sync"
)

// rgb is a packed 8-bit-per-channel colour, used for the downscaled mascot grid.
type rgb struct{ r, g, b uint8 }

// hexToRGB parses a "#rrggbb" string into an rgb. A malformed string yields the
// zero value (black) — callers pass only literal theme hexes, so this never
// fails in practice. (Kept as a general parser even though the splash currently
// only keys against the single obsidian background.)
//
//nolint:unparam // intentionally general; the one caller passes obsidianBG today.
func hexToRGB(h string) rgb {
	var r, g, b uint8
	_, _ = fmt.Sscanf(h, "#%02x%02x%02x", &r, &g, &b)
	return rgb{r, g, b}
}

// cellPx is one downscaled pixel of the mascot grid: a colour, or a flag that it
// is the background (keyed-out white / transparent), which toANSI paints as bg.
type cellPx struct {
	c  rgb
	bg bool
}

// isWhiteish reports whether a pixel is near-white and low-saturation — the
// mascot art sits on a white field, so these pixels are keyed to the terminal
// background. The thresholds (min channel > 226 AND channel spread < 18) are the
// spike's tuned values: aggressive enough to drop the white surround, tight
// enough to keep the saturated jade/gold/cream of the dog itself.
func isWhiteish(p rgb) bool {
	mn := min3(p.r, p.g, p.b)
	mx := max3(p.r, p.g, p.b)
	return mn > 226 && (mx-mn) < 18
}

// buildGrid box-downscales src to a pixel grid of width cols and height rows by
// alpha-weighted area-averaging each source box, keying near-white and
// near-transparent boxes to the background. rows is the PIXEL height (the cell
// height is rows/2, since each half-block cell stacks two pixels), so callers
// pass rows == cols to keep the dog square: N cols → N×N source sampling → N/2
// half-block cell-rows of N columns, and a terminal cell is ~1:2 (w:h), so N
// cols × N/2 cell-rows reads square.
func buildGrid(src image.Image, cols, rows int) [][]cellPx {
	b := src.Bounds()
	sw, sh := b.Dx(), b.Dy()

	grid := make([][]cellPx, rows)
	for y := 0; y < rows; y++ {
		grid[y] = make([]cellPx, cols)
		for x := 0; x < cols; x++ {
			x0 := b.Min.X + x*sw/cols
			x1 := b.Min.X + (x+1)*sw/cols
			y0 := b.Min.Y + y*sh/rows
			y1 := b.Min.Y + (y+1)*sh/rows
			if x1 <= x0 {
				x1 = x0 + 1
			}
			if y1 <= y0 {
				y1 = y0 + 1
			}
			var sr, sg, sb, sa, n float64
			for yy := y0; yy < y1; yy++ {
				for xx := x0; xx < x1; xx++ {
					r, g, bl, a := src.At(xx, yy).RGBA()
					af := float64(a) / 65535
					sr += float64(r) / 65535 * 255 * af
					sg += float64(g) / 65535 * 255 * af
					sb += float64(bl) / 65535 * 255 * af
					sa += af
					n++
				}
			}
			// A box that is mostly transparent reads as background.
			if n == 0 || sa/n < 0.35 {
				grid[y][x] = cellPx{bg: true}
				continue
			}
			px := rgb{
				uint8(clamp(sr / sa)),
				uint8(clamp(sg / sa)),
				uint8(clamp(sb / sa)),
			}
			// Key the near-white art field to the terminal background.
			if isWhiteish(px) {
				grid[y][x] = cellPx{bg: true}
				continue
			}
			grid[y][x] = cellPx{c: px}
		}
	}
	return grid
}

// toANSI renders a pixel grid as truecolor half-block (▀) rows: each output row
// packs two pixel-rows, the upper as the glyph foreground and the lower as its
// background, so one monospace cell shows two stacked pixels. A keyed-out (bg)
// pixel paints as bg, so the mascot floats on the terminal background. margin is
// the literal left indent (spaces) prepended to each row so the caller controls
// horizontal placement. Each row ends with a reset.
func toANSI(grid [][]cellPx, bg rgb, margin int) string {
	if len(grid) == 0 {
		return ""
	}
	var out strings.Builder
	pad := strings.Repeat(" ", margin)
	cols := len(grid[0])
	for cy := 0; cy < len(grid)/2; cy++ {
		out.WriteString(pad)
		for x := 0; x < cols; x++ {
			tc := grid[2*cy][x].c
			if grid[2*cy][x].bg {
				tc = bg
			}
			bc := grid[2*cy+1][x].c
			if grid[2*cy+1][x].bg {
				bc = bg
			}
			fmt.Fprintf(&out, "\x1b[38;2;%d;%d;%dm\x1b[48;2;%d;%d;%dm▀",
				tc.r, tc.g, tc.b, bc.r, bc.g, bc.b)
		}
		out.WriteString("\x1b[0m\n")
	}
	return out.String()
}

// mascotCacheKey keys the memoised half-block render: the column count fully
// determines the rendered string (the source image and bg are fixed), so the
// width tier is the only varying input.
type mascotCacheKey struct {
	cols int
	bg   rgb
}

var (
	mascotRenderMu    sync.Mutex
	mascotRenderCache = map[mascotCacheKey]string{}
)

// HalfBlockMascot returns the memoised half-block render of the mascot at the
// given column count over background bg, indented by margin spaces. cols → a
// cols×cols sampling → cols/2 cell-rows (square). It decodes the mascot lazily
// (DecodeMascot is itself cached) and returns "" if decoding fails, so the caller
// degrades to a mascot-less splash rather than panicking. The result is cached by
// (cols, bg) so an idle zero-state re-render is a map lookup, not a downscale.
//
// Note the cache key omits margin: margin is applied AFTER the cached body via a
// cheap per-line re-indent, so different margins share one downscale.
func HalfBlockMascot(cols int, bg rgb, margin int) string {
	if cols <= 0 {
		return ""
	}
	key := mascotCacheKey{cols: cols, bg: bg}
	mascotRenderMu.Lock()
	body, ok := mascotRenderCache[key]
	mascotRenderMu.Unlock()
	if !ok {
		img, err := DecodeMascot()
		if err != nil {
			return ""
		}
		grid := buildGrid(img, cols, cols)
		body = toANSI(grid, bg, 0)
		mascotRenderMu.Lock()
		mascotRenderCache[key] = body
		mascotRenderMu.Unlock()
	}
	if margin <= 0 {
		return body
	}
	return indentLines(body, margin)
}

// indentLines prepends margin spaces to each non-empty line of s. It is used to
// apply the welcome left margin to the cached (margin-0) mascot body, so the
// expensive downscale is shared across margins.
func indentLines(s string, margin int) string {
	if s == "" {
		return s
	}
	pad := strings.Repeat(" ", margin)
	lines := strings.Split(strings.TrimRight(s, "\n"), "\n")
	for i, ln := range lines {
		if ln != "" {
			lines[i] = pad + ln
		}
	}
	return strings.Join(lines, "\n") + "\n"
}

// clamp confines a float to [0,255] before an 8-bit cast.
func clamp(v float64) float64 {
	if v < 0 {
		return 0
	}
	if v > 255 {
		return 255
	}
	return v
}

func min3(a, b, c uint8) uint8 { return min(a, min(b, c)) }
func max3(a, b, c uint8) uint8 { return max(a, max(b, c)) }
