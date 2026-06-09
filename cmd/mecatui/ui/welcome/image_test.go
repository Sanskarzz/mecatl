package welcome

import (
	"image"
	"image/color"
	"strings"
	"testing"
)

// solid builds a w×h image filled with one colour, used to exercise the keying
// and averaging paths deterministically (no PNG decode).
func solid(w, h int, c color.Color) image.Image {
	img := image.NewRGBA(image.Rect(0, 0, w, h))
	for y := 0; y < h; y++ {
		for x := 0; x < w; x++ {
			img.Set(x, y, c)
		}
	}
	return img
}

func TestBuildGridDims(t *testing.T) {
	img := solid(40, 40, color.RGBA{R: 0x1F, G: 0xB3, B: 0x9A, A: 0xFF})
	g := buildGrid(img, 12, 12)
	if len(g) != 12 {
		t.Fatalf("grid rows = %d, want 12", len(g))
	}
	for i, row := range g {
		if len(row) != 12 {
			t.Fatalf("grid row %d cols = %d, want 12", i, len(row))
		}
	}
}

// TestSquareAspect locks the square relation the splash relies on: N cols passed
// with rows==N yields N/2 half-block cell-rows of N columns (toANSI packs two
// pixel-rows per cell).
func TestSquareAspect(t *testing.T) {
	const n = 16
	img := solid(64, 64, color.RGBA{R: 0x1F, G: 0xB3, B: 0x9A, A: 0xFF})
	g := buildGrid(img, n, n)
	ansi := toANSI(g, hexToRGB(obsidianBG), 0)
	lines := strings.Split(strings.TrimRight(ansi, "\n"), "\n")
	if len(lines) != n/2 {
		t.Fatalf("toANSI lines = %d, want %d (n/2)", len(lines), n/2)
	}
	// Each line is n half-block runs → n "▀" glyphs.
	if got := strings.Count(lines[0], "▀"); got != n {
		t.Fatalf("half-blocks per line = %d, want %d", got, n)
	}
}

// TestWhiteKeying asserts a near-white saturated-free pixel keys to background
// while a saturated jade pixel is kept.
func TestWhiteKeying(t *testing.T) {
	if !isWhiteish(rgb{0xFB, 0xFB, 0xFC}) {
		t.Error("near-white low-saturation pixel should key to background")
	}
	if isWhiteish(rgb{0x1F, 0xB3, 0x9A}) {
		t.Error("saturated jade must NOT be keyed to background")
	}

	// Through buildGrid: an all-white image → every cell is bg.
	white := solid(20, 20, color.RGBA{R: 0xFF, G: 0xFF, B: 0xFF, A: 0xFF})
	g := buildGrid(white, 4, 4)
	for _, row := range g {
		for _, c := range row {
			if !c.bg {
				t.Fatal("all-white image should produce all-bg cells")
			}
		}
	}

	// A saturated jade image → no bg cells, colour preserved (approximately).
	jade := solid(20, 20, color.RGBA{R: 0x1F, G: 0xB3, B: 0x9A, A: 0xFF})
	g = buildGrid(jade, 4, 4)
	for _, row := range g {
		for _, c := range row {
			if c.bg {
				t.Fatal("saturated jade image should produce no bg cells")
			}
			if c.c.g < 0xA0 {
				t.Fatalf("jade green channel collapsed: %v", c.c)
			}
		}
	}
}

// TestDownscaleAveraging checks the box-average: a 4×4 image split into a dark
// top half and a light bottom half, downscaled to 2×2, yields two dark cells over
// two light cells (the average of each box).
func TestDownscaleAveraging(t *testing.T) {
	img := image.NewRGBA(image.Rect(0, 0, 4, 4))
	dark := color.RGBA{R: 0x20, G: 0x40, B: 0x30, A: 0xFF}
	light := color.RGBA{R: 0x60, G: 0xA0, B: 0x80, A: 0xFF}
	for y := 0; y < 4; y++ {
		c := dark
		if y >= 2 {
			c = light
		}
		for x := 0; x < 4; x++ {
			img.Set(x, y, c)
		}
	}
	g := buildGrid(img, 2, 2)
	// Top row should be ~dark, bottom row ~light (neither keyed white).
	if g[0][0].bg || g[1][0].bg {
		t.Fatal("mid-tone cells should not be keyed to background")
	}
	if g[0][0].c.g >= g[1][0].c.g {
		t.Fatalf("top (dark) green %d should be < bottom (light) green %d",
			g[0][0].c.g, g[1][0].c.g)
	}
}

// TestToANSIShape locks the row count, per-line block count, and per-line reset.
func TestToANSIShape(t *testing.T) {
	img := solid(16, 16, color.RGBA{R: 0x1F, G: 0xB3, B: 0x9A, A: 0xFF})
	g := buildGrid(img, 8, 8)
	out := toANSI(g, hexToRGB(obsidianBG), 4)
	lines := strings.Split(strings.TrimRight(out, "\n"), "\n")
	if len(lines) != 4 { // 8 pixel-rows / 2
		t.Fatalf("lines = %d, want 4", len(lines))
	}
	for i, ln := range lines {
		if !strings.HasPrefix(ln, "    ") {
			t.Errorf("line %d missing 4-space margin: %q", i, ln)
		}
		if !strings.HasSuffix(ln, "\x1b[0m") {
			t.Errorf("line %d missing trailing reset", i)
		}
		if got := strings.Count(ln, "▀"); got != 8 {
			t.Errorf("line %d block count = %d, want 8", i, got)
		}
	}
}

// TestDecodeMascotCached asserts the embedded mascot decodes to the canonical
// 1254×1254 image and is cached (same pointer on a second call).
func TestDecodeMascotCached(t *testing.T) {
	img, err := DecodeMascot()
	if err != nil {
		t.Fatalf("DecodeMascot: %v", err)
	}
	if img == nil {
		t.Fatal("DecodeMascot returned nil image")
	}
	b := img.Bounds()
	if b.Dx() != 1254 || b.Dy() != 1254 {
		t.Fatalf("mascot bounds = %dx%d, want 1254x1254", b.Dx(), b.Dy())
	}
	img2, _ := DecodeMascot()
	if img != img2 {
		t.Error("DecodeMascot should return the cached image (same value)")
	}
}

// TestHalfBlockMascotMemoized renders twice at one tier and asserts the second
// call hits the cache (identical string, and the cache map holds the key).
func TestHalfBlockMascotMemoized(t *testing.T) {
	bg := hexToRGB(obsidianBG)
	a := HalfBlockMascot(36, bg, 4)
	if a == "" {
		t.Fatal("HalfBlockMascot returned empty")
	}
	b := HalfBlockMascot(36, bg, 4)
	if a != b {
		t.Error("memoized render differs between calls")
	}
	mascotRenderMu.Lock()
	_, ok := mascotRenderCache[mascotCacheKey{cols: 36, bg: bg}]
	mascotRenderMu.Unlock()
	if !ok {
		t.Error("render cache missing the 36-col key after a render")
	}
	// The 18 cell-rows (36/2) each carry the 4-space margin.
	lines := strings.Split(strings.TrimRight(a, "\n"), "\n")
	if len(lines) != 18 {
		t.Fatalf("36-col mascot lines = %d, want 18", len(lines))
	}
}
