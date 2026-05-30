package ui

import (
	"fmt"
	"strings"

	tea "charm.land/bubbletea/v2"
	"charm.land/lipgloss/v2"
)

// View assembles the three-region layout (header / viewport / input / footer)
// into a tea.View. While a permission modal is open it overlays the modal,
// centred, over the conversation region. Bubble Tea v2 returns a tea.View struct
// (not a string); we set Content and request the alt screen.
func (m Model) View() tea.View {
	var v tea.View
	v.AltScreen = !m.deps.NoAltScreen
	v.WindowTitle = "mecatui"

	if m.phase == phaseFatal {
		v.Content = m.renderFatal()
		return v
	}

	header := m.renderHeader()
	footer := m.renderFooter()

	var body string
	switch {
	case m.phase == phaseAwaitingApproval:
		body = renderPermissionModal(m.deps.Theme, m.ask, m.width, m.vp.Height())
	case m.mcp.view != mcpNone:
		body = renderMCPOverlay(m.deps.Theme, m.mcp, m.width, m.vp.Height())
	default:
		body = m.vp.View()
	}

	input := m.renderInput()

	v.Content = strings.Join([]string{header, body, input, footer}, "\n")
	return v
}

// renderHeader is the top bar: session id · model · mode · server.
func (m Model) renderHeader() string {
	sid := m.sessionID
	if sid == "" {
		sid = "connecting…"
	}
	parts := []string{
		"mecatui",
		"session " + short(sid),
	}
	if m.deps.Model != "" {
		parts = append(parts, truncate(m.deps.Model, maxModelLen))
	}
	if m.deps.Mode != "" {
		parts = append(parts, "mode "+m.deps.Mode)
	}
	if m.deps.Server != "" {
		parts = append(parts, m.deps.Server)
	}
	line := strings.Join(parts, "  ·  ")
	return m.deps.Theme.Style("header").Width(m.widthOr(80)).Render(line)
}

// renderFooter is the status bar: spinner + active tool + status + usage.
func (m Model) renderFooter() string {
	var left string
	switch m.phase {
	case phaseRunning:
		spin := m.sp.View()
		if m.activeTool != "" {
			left = fmt.Sprintf("%s Running %s…", spin, m.activeTool)
		} else {
			left = spin + " thinking…"
		}
	case phaseAwaitingApproval:
		left = m.deps.Theme.Style("askTitle").Render("⚠ awaiting approval")
	case phaseConnecting:
		left = m.sp.View() + " connecting…"
	default:
		left = m.statusMsg
		if left == "" {
			left = "ready"
		}
	}

	// The footer help uses the terse "ctrl+t details"; the richer expand/collapse
	// affordances live inline on each collapsible header (where discoverability
	// belongs), not in this always-on status line.
	help := "enter send · shift+enter newline · esc cancel · ctrl+o/r/p MCP · " +
		"ctrl+t details · ctrl+c quit"

	width := m.widthOr(80)
	line := m.fitFooter(left, width)
	footer := m.deps.Theme.Style("footer").Width(width).Render(line)
	return footer + "\n" + m.deps.Theme.Style("muted").Render(help)
}

// footerGapPad is the minimum blank gap kept between the left status and the
// right-aligned usage segment so they never touch.
const footerGapPad = 2

// fitFooter right-aligns the richest usage segment that fits beside the left
// status, shedding facets before the context signal — context % is the single
// most valuable signal, so it survives longest. Tiers, richest to poorest:
//
//	full:    "ctx ▒▒▒▒▒·· 70% · 140K/200K · ↑7.9K ↓345 ⊕1.2K cache 88%"
//	meter:   "ctx ▒▒▒▒▒·· 70% · 140K/200K"   (drop io/cache facets first)
//	compact: "ctx ▒▒▒▒▒·· 70%"               (drop used/total)
//	minimal: "ctx 70%"                        (drop the bar)
//	nothing: left status alone
//
// When the window is unknown every meter tier collapses to "ctx 7.9K", so the
// tiers naturally narrow to just that, then to nothing.
func (m Model) fitFooter(left string, width int) string {
	th := m.deps.Theme
	meter := renderContextMeter(th, m.contextTokens, m.deps.ContextWindow)
	candidates := []string{
		meter + " · " + renderUsageFacets(m.usage),
		meter,
		renderContextMeterCompact(th, m.contextTokens, m.deps.ContextWindow),
		renderContextMeterMinimal(th, m.contextTokens, m.deps.ContextWindow),
	}
	leftW := lipgloss.Width(left)
	for _, seg := range candidates {
		gap := width - leftW - lipgloss.Width(seg) - footerGapPad
		if gap >= 0 {
			return left + strings.Repeat(" ", gap) + seg
		}
	}
	return left
}

// renderInput renders the textarea, dimmed while a run is active.
func (m Model) renderInput() string {
	return m.ta.View()
}

// renderFatal renders a centred fatal-error panel.
func (m Model) renderFatal() string {
	msg := m.deps.Theme.Style("errorText").Render("connection failed") + "\n\n" +
		m.deps.Theme.Style("muted").Render(m.fatalErr) + "\n\n" +
		m.deps.Theme.Style("muted").Render("press ctrl+c to quit")
	card := m.deps.Theme.Style("askCard").Render(msg)
	if m.width > 0 && m.height > 0 {
		return lipgloss.Place(m.width, m.height, lipgloss.Center, lipgloss.Center, card)
	}
	return card
}

// short truncates a long id for the header.
func short(s string) string {
	if len(s) <= 12 {
		return s
	}
	return s[:12]
}

// maxModelLen caps the model name shown in the header so a long provider-scoped
// id (e.g. "anthropic/claude-opus-4-...") can't blow out the header width.
const maxModelLen = 24

// truncate clamps s to at most limit display runes, appending an ellipsis when
// it overflows (the "…" counts toward limit). Rune-safe so multibyte model ids
// aren't split mid-character. limit <= 1 yields the raw ellipsis.
func truncate(s string, limit int) string {
	r := []rune(s)
	if len(r) <= limit {
		return s
	}
	if limit <= 1 {
		return "…"
	}
	return string(r[:limit-1]) + "…"
}

// widthOr returns the terminal width or a fallback when unset (pre-first-resize).
func (m Model) widthOr(fallback int) int {
	if m.width > 0 {
		return m.width
	}
	return fallback
}
