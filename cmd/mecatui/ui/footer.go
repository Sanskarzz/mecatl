package ui

// This file holds the PURE footer-segment builders for mecatui: token
// humanisation, the cache-hit-rate computation, the context-meter string, and
// the session usage facets. These are deliberately layout-free — the actual
// footer assembly, right-alignment, and narrow-width tiering live in view.go's
// renderFooter. Keeping the segment builders here makes them unit-testable in
// isolation (no Model, no terminal) and keeps renderFooter readable.

import (
	"fmt"
	"strings"

	"github.com/stacklok/mecatl/cmd/mecatui/client"
	"github.com/stacklok/mecatl/cmd/mecatui/theme"
)

// Context-meter pressure thresholds, as a fraction of the context window. The
// bands echo the corpus guidance that compaction triggers around 70–80% full:
// stay "ok" comfortably below it, "warn" approaching it, "danger" once over.
const (
	ctxWarnFraction   = 0.60
	ctxDangerFraction = 0.85
	ctxBarWidth       = 8 // glyph cells in the meter bar
)

// Per-band fill glyphs. Pressure is encoded in the GLYPH (not just colour) so it
// survives ANSI stripping and is legible to red/green-colourblind users: the
// filled cells grow heavier with pressure — medium shade when ok, dark shade
// when warning, full block in the danger band. The empty glyph is the light
// shade (distinct from every fill glyph, and from the footer's "·" separator).
const (
	ctxGlyphOk     = "▒"
	ctxGlyphWarn   = "▓"
	ctxGlyphDanger = "█"
	ctxGlyphEmpty  = "░" // unfilled cells
)

// ctxDangerMark is appended to the percentage in the danger band — a non-colour
// textual cue so "over budget" reads even with ANSI stripped.
const ctxDangerMark = " ⚠"

// humanizeTokens renders a token count compactly: < 1000 verbatim, thousands as
// "7.9K", millions as "1.2M". One decimal place, trailing ".0" trimmed
// (e.g. 2000 → "2K", 7903 → "7.9K", 1200000 → "1.2M"). Negatives are clamped to
// 0 (token counts are never negative on the wire).
func humanizeTokens(n int64) string {
	if n < 0 {
		n = 0
	}
	switch {
	case n < 1000:
		return fmt.Sprintf("%d", n)
	case n < 1_000_000:
		return trimDecimal(float64(n)/1000.0) + "K"
	default:
		return trimDecimal(float64(n)/1_000_000.0) + "M"
	}
}

// trimDecimal formats v to one decimal place, dropping a redundant ".0".
func trimDecimal(v float64) string {
	s := fmt.Sprintf("%.1f", v)
	return strings.TrimSuffix(s, ".0")
}

// cacheHitRate computes the session cache-hit rate exactly as
// internal/session/usage.go does: CacheReadTokens / InputTokens, guarded against
// divide-by-zero (→ 0). The result is a fraction in [0,1].
func cacheHitRate(u client.Usage) float64 {
	if u.InputTokens <= 0 {
		return 0
	}
	return float64(u.CacheReadTokens) / float64(u.InputTokens)
}

// pctString formats a fraction in [0,1] as an integer percentage (e.g. 0.881 →
// "88%"). Values are clamped to [0,100].
func pctString(frac float64) string {
	p := int(frac*100 + 0.5)
	if p < 0 {
		p = 0
	}
	if p > 100 {
		p = 100
	}
	return fmt.Sprintf("%d%%", p)
}

// ctxFraction returns the clamped used/window fill fraction in [0,1].
func ctxFraction(used, window int64) float64 {
	if window <= 0 {
		return 0
	}
	frac := float64(used) / float64(window)
	if frac < 0 {
		return 0
	}
	if frac > 1 {
		return 1
	}
	return frac
}

// ctxPressureSlot maps a fill fraction to the themed pressure slot name.
func ctxPressureSlot(frac float64) string {
	switch {
	case frac >= ctxDangerFraction:
		return "ctxDanger"
	case frac >= ctxWarnFraction:
		return "ctxWarn"
	default:
		return "ctxOk"
	}
}

// ctxGlyph returns the per-band fill glyph for a fraction (non-colour pressure).
func ctxGlyph(frac float64) string {
	switch {
	case frac >= ctxDangerFraction:
		return ctxGlyphDanger
	case frac >= ctxWarnFraction:
		return ctxGlyphWarn
	default:
		return ctxGlyphOk
	}
}

// ctxLabel is the bar-less percentage label, with the danger ⚠ cue appended in
// the danger band. Used as the lowest-fidelity context tier ("ctx 92% ⚠").
func ctxLabel(frac float64) string {
	label := pctString(frac)
	if frac >= ctxDangerFraction {
		label += ctxDangerMark
	}
	return label
}

// renderContextMeter renders the FULL-fidelity context segment: a per-band bar +
// percentage (+ ⚠ in danger) + used/total, e.g.
// "ctx ▓▓▓▓▓▓░░ 70% · 140K/200K". With an unknown window (window<=0) it degrades
// to just the current size ("ctx 7.9K"). The bar/percentage carry the
// ctxOk/ctxWarn/ctxDanger colour AND a per-band glyph so pressure is legible
// without colour.
func renderContextMeter(th theme.Theme, used, window int64) string {
	if used < 0 {
		used = 0
	}
	if window <= 0 {
		return "ctx " + humanizeTokens(used)
	}
	frac := ctxFraction(used, window)
	style := th.Style(ctxPressureSlot(frac))
	meter := style.Render(ctxBar(frac) + " " + ctxLabel(frac))
	return "ctx " + meter + " · " + humanizeTokens(used) + "/" + humanizeTokens(window)
}

// renderContextMeterCompact is the mid-fidelity tier: the coloured per-band bar +
// label, WITHOUT the used/total suffix, e.g. "ctx ▓▓▓▓▓▓░░ 70%". Unknown window
// degrades to the bare size, identical to the full tier.
func renderContextMeterCompact(th theme.Theme, used, window int64) string {
	if used < 0 {
		used = 0
	}
	if window <= 0 {
		return "ctx " + humanizeTokens(used)
	}
	frac := ctxFraction(used, window)
	style := th.Style(ctxPressureSlot(frac))
	return "ctx " + style.Render(ctxBar(frac)+" "+ctxLabel(frac))
}

// renderContextMeterMinimal is the lowest-fidelity tier: no bar — just the
// coloured percentage (+ ⚠ in danger), e.g. "ctx 70%". Unknown window degrades
// to the bare size.
func renderContextMeterMinimal(th theme.Theme, used, window int64) string {
	if used < 0 {
		used = 0
	}
	if window <= 0 {
		return "ctx " + humanizeTokens(used)
	}
	frac := ctxFraction(used, window)
	return "ctx " + th.Style(ctxPressureSlot(frac)).Render(ctxLabel(frac))
}

// ctxBar builds the meter bar: filled cells use the per-band glyph, the rest the
// empty glyph. Glyph choice (not just colour) carries the pressure band.
func ctxBar(frac float64) string {
	filled := int(frac*float64(ctxBarWidth) + 0.5)
	if filled > ctxBarWidth {
		filled = ctxBarWidth
	}
	return strings.Repeat(ctxGlyph(frac), filled) + strings.Repeat(ctxGlyphEmpty, ctxBarWidth-filled)
}

// trivialTurnTokens is the per-direction token count below which a turn's
// input/output is considered negligible. A turn whose input AND output are both
// at or under this AND has no measurable duration (sub-second or no clock) is a
// near-empty turn whose stat line is pure noise, so it is suppressed entirely —
// keeping a long multi-turn run scannable.
const trivialTurnTokens = 50

// turnStatLine formats the inline per-turn stat line shown after a turn's model
// exchange closes. It leads with cost — the turn's input/output tokens
// (humanised) — then the elapsed model-call time, e.g. "↑1.2K ↓340 · 4.1s". The
// duration segment is omitted when the server reported 0ms (no clock), giving
// just "↑1.2K ↓340". Users think in cost, not turn numbers, so no index is shown.
func turnStatLine(msg client.TurnEndMsg) string {
	var b strings.Builder
	fmt.Fprintf(&b, "↑%s ↓%s",
		humanizeTokens(msg.Usage.InputTokens), humanizeTokens(msg.Usage.OutputTokens))
	if msg.DurationMs > 0 {
		b.WriteString(" · " + formatDuration(msg.DurationMs))
	}
	return b.String()
}

// trivialTurn reports whether a turn's stats are too negligible to be worth a
// scrollback line: both token directions at or below trivialTurnTokens AND no
// sub-second-or-better duration signal. Such lines are suppressed.
func trivialTurn(msg client.TurnEndMsg) bool {
	return msg.Usage.InputTokens <= trivialTurnTokens &&
		msg.Usage.OutputTokens <= trivialTurnTokens &&
		msg.DurationMs < 1000
}

// formatDuration renders an elapsed millisecond count compactly: under one
// second verbatim in ms ("840ms"), otherwise seconds to one decimal ("4.1s",
// trailing ".0" trimmed → "2s"). Negative values clamp to 0.
func formatDuration(ms int64) string {
	if ms < 0 {
		ms = 0
	}
	if ms < 1000 {
		return fmt.Sprintf("%dms", ms)
	}
	return trimDecimal(float64(ms)/1000.0) + "s"
}

// renderUsageFacets renders the session-total facets surfaced from ALL usage
// fields: input/output token arrows, the previously-dropped cache-WRITE total
// (⊕), and the cache-hit rate. Format: "↑7.9K ↓345 ⊕1.2K cache 88%". The cache
// write is omitted when zero to keep the segment scannable.
func renderUsageFacets(u client.Usage) string {
	var b strings.Builder
	fmt.Fprintf(&b, "↑%s ↓%s", humanizeTokens(u.InputTokens), humanizeTokens(u.OutputTokens))
	if u.CacheWriteTokens > 0 {
		fmt.Fprintf(&b, " ⊕%s", humanizeTokens(u.CacheWriteTokens))
	}
	fmt.Fprintf(&b, " cache %s", pctString(cacheHitRate(u)))
	return b.String()
}
