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
	case m.showHelp:
		// Help wins among the overlays (they don't coexist in practice, but help is
		// the keyboard-owning one when set).
		body = renderHelpOverlay(m.deps.Theme, m.caps, m.width, m.vp.Height())
	case m.phase == phaseAwaitingApproval:
		body = m.rend.renderPermissionModal(m.ask, m.expandTools, m.width, m.vp.Height())
	case m.mcp.view != mcpNone:
		body = renderMCPOverlay(m.deps.Theme, m.mcp, m.caps, m.width, m.vp.Height())
	case m.agents.view != agentsNone:
		body = renderAgentsOverlay(m.deps.Theme, m.agents, m.conv.latestTeamBlock(), m.width, m.vp.Height())
	case m.phase == phaseIdle && m.conv.isEmpty():
		// First-run zero-state: a welcome card in the empty viewport. Not an overlay
		// (claims no keyboard); typing flows over it and it vanishes on the first block.
		body = renderZeroState(m.deps.Theme, m.caps, m.width, m.vp.Height())
	default:
		body = m.vp.View()
	}

	input := m.renderInput()

	// The slash-command palette is an inline dropdown shown just ABOVE the input
	// (not an overlay over the conversation): it appears only while idle and the
	// input is a command line, so it never competes with the permission modal or
	// the MCP/agents overlays.
	regions := []string{header, body}
	if pal := renderPalette(m.deps.Theme, m.palette, m.caps, m.ta.Value(), m.width); pal != "" {
		regions = append(regions, pal)
	}
	regions = append(regions, input, footer)

	v.Content = strings.Join(regions, "\n")
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
	if delta := m.changedFilesIndicator(); delta != "" {
		// Right-align the muted "Δ N files" indicator on the header line when it
		// fits beside the identity segment; otherwise drop it (the header never
		// wraps). The header is the least-crowded bar — the footer is already busy
		// with the context meter and usage facets.
		line = m.fitHeader(line, delta, m.widthOr(80))
	}
	return m.deps.Theme.Style("header").Width(m.widthOr(80)).Render(line)
}

// changedFilesIndicator returns the muted "✎ N files" header indicator
// summarising how many distinct workspace files file-mutating tools have touched
// this session, or "" when none have. The "✎" (pencil = "edited") is coherent
// with the hook-modified glyph and avoids "Δ" colliding with the Edit/Write
// "+N/-N" diff size signals. It is a compact count; the full path list is
// revealed under ctrl+t (see renderChangedFiles).
func (m Model) changedFilesIndicator() string {
	n := len(m.filesChanged)
	if n == 0 {
		return ""
	}
	return "✎ " + plural(n, "file")
}

// headerGapPad is the minimum blank gap kept between the header identity segment
// and the right-aligned changed-files indicator so they never touch. Mirrors the
// footer's footerGapPad but is owned by the header path (naming honesty).
const headerGapPad = 2

// fitHeader right-aligns the muted indicator beside the identity line when there
// is room (accounting for the header's 1-cell horizontal padding on each side),
// and otherwise returns the identity line unchanged — the header is a single
// non-wrapping row, so a too-narrow terminal simply sheds the indicator.
func (m Model) fitHeader(line, indicator string, width int) string {
	const headerPad = 2 // the "header" style pads 1 cell each side
	styled := m.deps.Theme.Style("muted").Render(indicator)
	gap := width - headerPad - lipgloss.Width(line) - lipgloss.Width(indicator) - headerGapPad
	if gap < 0 {
		return line
	}
	return line + strings.Repeat(" ", gap) + styled
}

// renderFooter is the status bar: spinner + active tool + status + usage.
func (m Model) renderFooter() string {
	var left string
	switch m.phase {
	case phaseRunning:
		spin := m.sp.View()
		switch {
		case m.activeTool != "" && m.toolProgress != "":
			// A long-running tool forwarded a transient progress line: show it in
			// place of the bare "Running X…" so the footer reflects live activity
			// instead of looking frozen. Cleared on the next tool.result/turn boundary.
			left = fmt.Sprintf("%s %s…", spin, m.toolProgress)
		case m.activeTool != "":
			left = fmt.Sprintf("%s Running %s…", spin, m.activeTool)
		default:
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

	// The full decompressed chord list now lives in the "?" help overlay, so the
	// footer carries only the two entry points and quit. "/ commands" is dropped
	// when slash commands are not enabled, so the footer never advertises a
	// disabled entry point (an honest touch the relayed caps now afford).
	help := "? help · / commands · ctrl+c quit"
	if !m.caps.SlashCommands {
		help = "? help · ctrl+c quit"
	}

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
// most valuable signal, so it survives longest. When a team is LIVE a team-summary
// segment is PREPENDED to the right side; it is LOWER priority than the context
// meter (it's an advertisement, context % is the headline safety signal), so it is
// the FIRST thing dropped as width tightens. Tiers, richest to poorest:
//
//	"⟳ team-x · 2/3 working · ctrl+a agents  ctx ▒▒▒▒▒·· 70% · 140K/200K · ↑7.9K ↓345 cache 88%"
//	"⟳ team-x · 2/3 working · ctrl+a agents  ctx ▒▒▒▒▒·· 70% · 140K/200K"
//	"⟳ 2/3 working · ctrl+a  ctx ▒▒▒▒▒·· 70%"   (team→medium, ctx→compact)
//	"⟳ 2/3  ctx 70%"                            (team→compact, ctx→minimal)
//	"ctx 70%"                                    (team DROPPED, ctx wins)
//	then the existing ctx-only fallbacks, then left status alone.
//
// When no team is live the team segment is empty and the candidate list collapses
// to EXACTLY the historical ctx-only list — keeping the no-team footer
// byte-identical (existing goldens unaffected).
//
// When the window is unknown every meter tier collapses to "ctx 7.9K", so the
// tiers naturally narrow to just that, then to nothing.
func (m Model) fitFooter(left string, width int) string {
	th := m.deps.Theme
	meter := renderContextMeter(th, m.contextTokens, m.deps.ContextWindow)
	meterCompact := renderContextMeterCompact(th, m.contextTokens, m.deps.ContextWindow)
	meterMinimal := renderContextMeterMinimal(th, m.contextTokens, m.deps.ContextWindow)

	// The team segment is non-empty ONLY for a LIVE team — liveTeamBlock returns the
	// latest team that is still running (not teamDone). This DELIBERATELY differs
	// from the ctrl+a overlay's gate: the footer is a live-activity advertisement
	// and hides once the team is done, whereas openAgents opens on the last-seen
	// team done-or-not (so the user can still review a finished roster). The two are
	// meant to disagree in the done state — do not unify them.
	var teamFull, teamMedium, teamCompact string
	if b := m.conv.liveTeamBlock(); b != nil {
		working, total := teamWorkingCounts(b.teamLanes)
		teamFull = teamFooterFull(th, b.teamID, working, total)
		teamMedium = th.Style("spinner").Render(teamFooterMedium(b.teamID, working, total))
		teamCompact = th.Style("spinner").Render(teamFooterCompact(working, total))
	}

	const sep = "  " // gap between the team segment and the ctx segment

	var candidates []string
	if teamFull != "" {
		// Richest-to-poorest cross-product. The team segment sheds before the ctx
		// meter: the last team-bearing tier (team-compact + ctx-minimal) is followed
		// by the team-LESS ctx-minimal so context wins when width is tight.
		candidates = append(candidates,
			teamFull+sep+meter+" · "+renderUsageFacets(m.usage),
			teamFull+sep+meter,
			teamMedium+sep+meterCompact,
			teamCompact+sep+meterMinimal,
		)
	}
	candidates = append(candidates,
		meter+" · "+renderUsageFacets(m.usage),
		meter,
		meterCompact,
		meterMinimal,
	)
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
