package welcome

import (
	"image/color"
	"strings"

	"charm.land/lipgloss/v2"

	"github.com/stacklok/mecatl/cmd/mecatui/theme"
)

// obsidianBG is the mascot's keyed background — the Aztec obsidian. The mascot's
// white field keys to this so the dog floats on the card; it matches the theme's
// dark panel so the half-block surround blends into the centered card.
const obsidianBG = "#0E1311"

// Info is the presentation-ready context the splash renders, assembled by the
// caller (ui.help.go) from the model + relayed capabilities. The welcome package
// stays decoupled from ui/client: it receives plain strings + bools, not a Model.
type Info struct {
	Cwd      string // workspace path (already display-trimmed by the caller if needed)
	Model    string // active model id ("" → omit the line)
	Provider string // provider name ("" → just the model)
	Version  string // mecatui build version ("" → omit)
	Tagline  string // one-line tagline under the identity ("" → omit)

	// Affordances are the pre-rendered (themed) affordance rows the caller builds
	// from its caps-tailored zeroStateRows(), so the welcome card's chord list stays
	// byte-equivalent to the legacy card. Each entry is one full line.
	Affordances []string

	// MemoryNote is the pre-rendered "memory is on" line (already themed), or "" when
	// memory is off — the single caps-conditional content the card carries.
	MemoryNote string

	// FullColor is true on a truecolor terminal: the wordmark then gets the
	// jade→gold gradient; otherwise it collapses to the single accent colour.
	FullColor bool

	// Kitty is true when the terminal supports the Kitty graphics protocol and the
	// caller has transmitted the mascot image: the splash then emits the
	// placeholder-cell grid (high-res image) instead of the half-block mascot, at
	// the SAME cols×rows footprint.
	Kitty bool
}

// Layout thresholds for the responsive width tier and the tiny-terminal clamp.
const (
	tierSmallCols  = 36 // short terminals
	tierMediumCols = 48 // the common case
	tierLargeCols  = 60 // tall terminals
	mascotMargin   = 4  // left indent of the mascot block (matches the spike)

	minSplashWidth  = 40 // below this, skip the mascot (and wordmark)
	minSplashHeight = 12

	shortHeight = 24 // <= → small tier
	tallHeight  = 44 // >= → large tier
)

// tierForHeight picks the mascot column count from the available height: a short
// viewport gets the compact 36-wide mascot, a tall one the detailed 60-wide, the
// common middle the 48-wide. (The approved visuals are faithful-{36,48,60}.png.)
func tierForHeight(height int) int {
	switch {
	case height <= shortHeight:
		return tierSmallCols
	case height >= tallHeight:
		return tierLargeCols
	default:
		return tierMediumCols
	}
}

// Splash assembles the UN-framed welcome body: mascot (top) · gradient wordmark ·
// info block (cwd / model·provider / version / tagline · affordance rows · memory
// note). The caller frames it (centerCard). It is responsive to height (the
// mascot tier) and degrades on a tiny terminal to a minimal hint that never
// panics. The literal "Welcome to mecatui" always appears (a test greps it).
//
// width/height are the conversation region dims. On a tiny region (width <
// minSplashWidth or height < minSplashHeight) the mascot and wordmark are skipped
// and only the title + a prompt hint + affordances are shown.
func Splash(th theme.Theme, in Info, width, height int) string {
	muted := th.Style("muted")
	title := th.Style("askTitle").Render("Welcome to mecatui")

	// Tiny-terminal clamp: a minimal, safe body. Still carries the greppable title,
	// a prompt hint, and the affordance rows so discoverability survives.
	if width > 0 && width < minSplashWidth || height > 0 && height < minSplashHeight {
		var b strings.Builder
		b.WriteString(title + "\n")
		b.WriteString(muted.Render("Type a request and press enter."))
		for _, row := range in.Affordances {
			b.WriteString("\n" + row)
		}
		return b.String()
	}

	bg := hexToRGB(obsidianBG)
	cols := tierForHeight(height)
	rows := cols / 2 // half-block: cols×cols sampling → cols/2 cell-rows (square)

	var b strings.Builder

	// 1. Mascot — kitty placeholder grid (high-res) or half-block fallback. Both
	// share the cols×rows cell footprint and the mascotMargin left indent.
	if in.Kitty {
		b.WriteString(PlaceholderGrid(cols, rows, mascotMargin))
	} else if mascot := HalfBlockMascot(cols, bg, mascotMargin); mascot != "" {
		b.WriteString(mascot)
	}
	b.WriteString("\n")

	// 2. Wordmark — gradient (truecolor) or single-accent fallback.
	b.WriteString(Wordmark(th, in.FullColor) + "\n\n")

	// 3. Title + prompt hint.
	b.WriteString(title + "\n")
	b.WriteString(th.Style("toolArgs").Render("  Type a request below and press enter.") + "\n\n")

	// 4. Identity block: cwd · model·provider · version · tagline.
	if in.Cwd != "" {
		b.WriteString(muted.Render("  "+in.Cwd) + "\n")
	}
	if line := modelLine(in); line != "" {
		b.WriteString(muted.Render("  "+line) + "\n")
	}
	if in.Version != "" {
		b.WriteString(muted.Render("  mecatui "+in.Version) + "\n")
	}
	if in.Tagline != "" {
		b.WriteString("\n" + th.Style("toolArgs").Render("  "+in.Tagline) + "\n")
	}

	// 5. Affordance rows (caps-tailored, built by the caller).
	if len(in.Affordances) > 0 {
		b.WriteString("\n")
		for _, row := range in.Affordances {
			b.WriteString(row + "\n")
		}
	}

	// 6. Memory note (caps-conditional).
	if in.MemoryNote != "" {
		b.WriteString("\n" + in.MemoryNote + "\n")
	}

	return strings.TrimRight(b.String(), "\n")
}

// modelLine composes the "model · provider" identity line from Info, omitting
// absent halves so a bare model or a bare provider renders cleanly.
func modelLine(in Info) string {
	switch {
	case in.Model != "" && in.Provider != "":
		return in.Model + " · " + in.Provider
	case in.Model != "":
		return in.Model
	case in.Provider != "":
		return in.Provider
	default:
		return ""
	}
}

// TierForHeight is the exported view of tierForHeight, so the ui caller can size
// the kitty transmit/placement footprint identically to the half-block tier
// (both must agree on cols×rows or the two mascot paths would lay out
// differently). Returns (cols, rows).
func TierForHeight(height int) (cols, rows int) {
	cols = tierForHeight(height)
	return cols, cols / 2
}

// ObsidianBG is the exported mascot keyed-background colour, for any caller that
// needs to match the card surround.
func ObsidianBG() color.Color { return lipgloss.Color(obsidianBG) }
