package welcome

import (
	"strings"
	"testing"

	"github.com/charmbracelet/x/ansi/kitty"

	"github.com/stacklok/mecatl/cmd/mecatui/theme"
)

func splashTheme() theme.Theme { return theme.New("aztec", theme.AztecPalette()) }

func splashInfo(kittyOn bool) Info {
	return Info{
		Cwd:         "/workspace",
		Model:       "mock-model",
		Provider:    "openai",
		Version:     "v9.9.9-test",
		Tagline:     "your local agentic coding harness",
		Affordances: []string{"  ?   keys & features", "  /   slash commands"},
		MemoryNote:  "  memory is on",
		FullColor:   true,
		Kitty:       kittyOn,
	}
}

// TestSplashKittyEmitsPlaceholderGrid proves the Kitty branch of Splash: with
// Info.Kitty=true the mascot is the U+10EEEE placeholder grid, NOT the half-block
// "▀" render — so the two paths are mutually exclusive and the kitty path is
// actually reachable through Splash (not only via the low-level PlaceholderGrid).
func TestSplashKittyEmitsPlaceholderGrid(t *testing.T) {
	out := Splash(splashTheme(), splashInfo(true), 100, 40)
	if !strings.Contains(out, string(kitty.Placeholder)) {
		t.Fatal("kitty Splash should emit the U+10EEEE placeholder grid")
	}
	// The half-block mascot path must be ABSENT. The half-block render interleaves an
	// SGR escape before every ▀, so a contiguous ▀-run only appears after stripping
	// SGR — check on the stripped text (the wordmark's ▀ glyphs are short, so a long
	// run is the mascot tell).
	if strings.Contains(stripSGR(out), "▀▀▀▀▀▀▀▀") {
		t.Fatal("kitty Splash must NOT also emit the half-block mascot rows")
	}
	// Sanity: the half-block path (Kitty=false) is the opposite — placeholders absent,
	// half-block run present (after SGR strip).
	half := Splash(splashTheme(), splashInfo(false), 100, 40)
	if strings.Contains(half, string(kitty.Placeholder)) {
		t.Fatal("half-block Splash must NOT emit placeholder cells")
	}
	if !strings.Contains(stripSGR(half), "▀▀▀▀▀▀▀▀") {
		t.Fatal("half-block Splash should emit the half-block mascot rows")
	}
}

// TestSplashKittyGridColumnsMatchTier asserts the placeholder grid's per-row cell
// count equals the tier's column count for the chosen height — the kitty grid and
// the half-block share the same cols×rows footprint, so layout is identical.
func TestSplashKittyGridColumnsMatchTier(t *testing.T) {
	const height = 20 // small tier
	wantCols, _ := TierForHeight(height)
	out := Splash(splashTheme(), splashInfo(true), 100, height)
	ph := string(kitty.Placeholder)
	// Find the first placeholder-bearing line and count its cells.
	var found bool
	for _, ln := range strings.Split(out, "\n") {
		if strings.Contains(ln, ph) {
			if got := strings.Count(ln, ph); got != wantCols {
				t.Fatalf("placeholder cells per row = %d, want %d (tier cols for height %d)",
					got, wantCols, height)
			}
			found = true
			break
		}
	}
	if !found {
		t.Fatal("no placeholder row found in the kitty Splash")
	}
}

// TestTierForHeightBoundaries tripwires the responsive thresholds: <=24 → 36,
// 25..43 → 48, >=44 → 60.
func TestTierForHeightBoundaries(t *testing.T) {
	cases := []struct {
		h, want int
	}{
		{24, tierSmallCols},
		{25, tierMediumCols},
		{43, tierMediumCols},
		{44, tierLargeCols},
		{0, tierSmallCols}, // 0 <= shortHeight(24) → small tier (the clamp guards real 0-height)
	}
	for _, tc := range cases {
		if got := tierForHeight(tc.h); got != tc.want {
			t.Errorf("tierForHeight(%d) = %d, want %d", tc.h, got, tc.want)
		}
	}
}

// TestModelLine covers the identity-line permutations: model+provider, model-only,
// provider-only, neither.
func TestModelLine(t *testing.T) {
	cases := []struct {
		name        string
		model, prov string
		want        string
	}{
		{"both", "gpt-5", "openai", "gpt-5 · openai"},
		{"model only", "gpt-5", "", "gpt-5"},
		{"provider only", "", "openai", "openai"},
		{"neither", "", "", ""},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if got := modelLine(Info{Model: tc.model, Provider: tc.prov}); got != tc.want {
				t.Errorf("modelLine(model=%q,prov=%q) = %q, want %q", tc.model, tc.prov, got, tc.want)
			}
		})
	}
}

// TestSplashVersionOmittedWhenEmpty asserts a "" Version omits the version line
// (no bare "mecatui " label), while a set Version includes it.
func TestSplashVersionOmittedWhenEmpty(t *testing.T) {
	with := Splash(splashTheme(), splashInfo(false), 100, 40)
	if !strings.Contains(stripSGR(with), "mecatui v9.9.9-test") {
		t.Error("a set Version should render the version line")
	}
	in := splashInfo(false)
	in.Version = ""
	without := stripSGR(Splash(splashTheme(), in, 100, 40))
	if strings.Contains(without, "  mecatui v") {
		t.Error("an empty Version must omit the version line")
	}
}
