package ui

import (
	"fmt"
	"strings"

	tea "charm.land/bubbletea/v2"
	"charm.land/lipgloss/v2"
	"github.com/charmbracelet/x/ansi"

	"github.com/stacklok/mecatl/cmd/mecatui/theme"
)

// centerCard frames body in the askCard style and centers it over the
// conversation region; an unknown size (width/height <= 0) returns the bare card.
// It is the shared framing tail for every askCard-style overlay (permission
// modal, MCP, agents, help, zero-state) — the body builders stay separate, only
// this Place-based framing is shared. The palette deliberately does NOT use it
// (it is an inline MaxWidth dropdown, not a centered overlay).
func centerCard(th theme.Theme, body string, width, height int) string {
	card := th.Style("askCard").Render(body)
	if width <= 0 || height <= 0 {
		return card
	}
	return lipgloss.Place(width, height, lipgloss.Center, lipgloss.Center, card)
}

// View assembles the three-region layout (header / viewport / input / footer)
// into a tea.View. While a permission modal is open it overlays the modal,
// centred, over the conversation region. Bubble Tea v2 returns a tea.View struct
// (not a string); we set Content and request the alt screen.
func (m Model) View() tea.View {
	var v tea.View
	v.AltScreen = !m.deps.NoAltScreen
	v.WindowTitle = "mecatui"
	// Capture the mouse — but ONLY on the alt screen, and ONLY when mouse capture
	// is not disabled. Capturing the mouse buys wheel-scroll and the in-app
	// drag-select/copy layer (see selection.go) at the cost of the terminal's OWN
	// native click-drag selection (the terminal forwards drags to us instead).
	// Two opt-outs leave the mouse uncaptured so native selection works:
	//   - --inline / --no-alt-screen: native scrollback + selection, inline buffer.
	//   - --no-mouse (NoMouse): alt screen kept, but native selection over in-app
	//     wheel/drag — the escape hatch for terminals that strip OSC52. Keyboard
	//     scroll (pgup/pgdn/home/end) is unaffected either way.
	// (Bubble Tea v2 has no wheel-only mouse mode, so wheel-scroll and native
	// selection genuinely cannot coexist; this is the deliberate tradeoff.)
	if !m.deps.NoAltScreen && !m.deps.NoMouse {
		v.MouseMode = tea.MouseModeCellMotion
	}

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
	case m.team.view != teamNone:
		body = renderTeamOverlay(m.deps.Theme, m.team, m.conv.latestTeamBlock(), m.width, m.vp.Height())
	case m.agentsInv.view != agentsInvNone:
		body = renderAgentsInvOverlay(m.deps.Theme, m.agentsInv, m.caps, m.width, m.vp.Height())
	case m.skills.view != skillsNone:
		body = renderSkillsOverlay(m.deps.Theme, m.skills, m.caps, m.width, m.vp.Height())
	case m.soul.view != soulNone:
		body = renderSoulOverlay(m.deps.Theme, m.soul, m.caps, m.width, m.vp.Height())
	case m.userModel.view != userModelNone:
		body = renderUserModelOverlay(m.deps.Theme, m.userModel, m.caps, m.width, m.vp.Height())
	case m.models.view != modelsNone:
		body = renderModelsOverlay(m.deps.Theme, m.models, m.caps, m.width, m.vp.Height())
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
	// body MUST stay regions[1], directly below the header: convTopRow derives the
	// conversation's top screen row from the header's rendered height ALONE (the
	// regions are joined with "\n"), so inserting any region ABOVE the body here would
	// shift the body down and break the selection click→content mapping. The
	// convTopRow drift test (selection_test.go) guards this.
	regions := []string{header, body}
	if pal := renderPalette(m.deps.Theme, m.palette, m.caps, m.ta.Value(), m.width); pal != "" {
		regions = append(regions, pal)
	}
	// The @-mention file menu is the same kind of inline dropdown as the palette
	// (idle/running, above the input) and is mutually exclusive with it, so at most
	// one of the two renders.
	if men := renderMention(m.deps.Theme, m.mention, m.width); men != "" {
		regions = append(regions, men)
	}
	// Staged follow-ups (queued while a run streams) are summarised in a muted card
	// just above the input — between the body/palette and the input — so the user can
	// see what will run next. Shown in any phase whenever the queue is non-empty (it
	// only fills mid-run, but it survives an error/cancel pause, so it must render at
	// idle too until it drains or is cleared).
	if q := m.renderQueue(); q != "" {
		regions = append(regions, q)
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
	// Prefer the picker's active selection once set (it is what the NEXT session
	// will use); fall back to the launch-time --model display. sanitize: a model id
	// can be server/provider-derived.
	if name := m.activeModel.ModelID; name != "" {
		parts = append(parts, truncate(sanitizeTerminal(name), maxModelLen))
	} else if m.deps.Model != "" {
		parts = append(parts, truncate(m.deps.Model, maxModelLen))
	}
	if m.deps.Mode != "" {
		parts = append(parts, "mode "+m.deps.Mode)
	}
	if m.deps.Server != "" {
		parts = append(parts, m.deps.Server)
	}
	line := strings.Join(parts, "  ·  ")
	// Right-align ONE muted indicator on the header line when it fits beside the
	// identity segment; otherwise drop it (so the indicator never forces a wrap — the
	// identity line itself still wraps when it alone exceeds the width). The header is
	// the least-crowded bar — the footer is already busy with the context meter
	// and usage facets. The scroll-position indicator takes precedence over the
	// changed-files indicator while the user is scrolled up, so it is visible
	// exactly when it matters; at the bottom it is "" and the changed-files cue
	// shows (so the steady-state at-bottom frame is byte-identical to before).
	indicator := m.scrollIndicator()
	if indicator == "" {
		indicator = m.changedFilesIndicator()
	}
	if indicator != "" {
		line = m.fitHeader(line, indicator, m.widthOr(80))
	}
	return m.deps.Theme.Style("header").Width(m.widthOr(80)).Render(line)
}

// scrollIndicator returns the muted "↑ NN%" header cue shown ONLY when the user
// has scrolled up off the bottom (!m.stuck) — the discoverable signal that the
// view is no longer tailing live output and how far up it sits. It is "" while
// stuck (auto-following the bottom), so the at-bottom steady-state header — and
// thus the View goldens captured there — is unchanged.
func (m Model) scrollIndicator() string {
	if m.stuck {
		return ""
	}
	return fmt.Sprintf("↑ %d%%", int(m.vp.ScrollPercent()*100))
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
// and otherwise returns the identity line unchanged — so the indicator never forces
// a wrap; a too-narrow terminal simply sheds it. (The identity line itself still
// wraps when it alone exceeds the width — see headerHeight() in selection.go.)
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
		// While a non-empty selection is active at idle/default phase, the footer
		// shows a live "N chars · M lines" count (→ "copied · …" after a copy). This
		// arm is the ONLY phase the count can appear in — the running/approval/
		// connecting arms above own the footer-left in those phases — so the count is
		// never shown mid-run by construction (Req 5). With no selection the existing
		// statusMsg / "ready" is shown unchanged.
		switch {
		case m.sel.active && !m.sel.empty():
			left = m.selectionStatus()
		default:
			left = m.statusMsg
			if left == "" {
				left = "ready"
			}
		}
	}

	// The full decompressed chord list now lives in the "?" help overlay, so the
	// footer carries only the two entry points and quit. "/ commands" is ALWAYS
	// shown: the TUI ships built-in client-side commands (/clear, /help, and the
	// caps-gated /mcp,/agents), so "/" is a live entry point even when the server
	// has slash-command expansion disabled. While a run streams the line is extended
	// with the type-while-running affordance (enter queues a follow-up; esc clears
	// the staged input/queue or cancels the run).
	help := "? help · / commands · ctrl+c quit"
	if m.phase == phaseRunning {
		help = "enter queue · esc cancel/clear · " + help
	}
	// While the double-ctrl+c guard is armed, prepend a loud "again to quit" cue to
	// the help line. The footer is the one chrome line present in every phase (the
	// left status differs by phase), so it is the robust place for the hint.
	if m.quitArmed {
		help = m.deps.Theme.Style("ctxWarn").Render("ctrl+c again to quit") + " · " + help
	}

	width := m.widthOr(80)
	line := m.fitFooter(left, width)
	footer := m.deps.Theme.Style("footer").Width(width).Render(line)
	return footer + "\n" + m.deps.Theme.Style("muted").Render(help)
}

// selectionStatus is the footer-left segment shown while a non-empty selection
// is active (idle/default phase only — see renderFooter). It reports the live
// VISIBLE size of the selection as "N chars · M lines" (correct singular: "1
// char", "1 line"), counting the copy-ready runes (selectedText) and the spanned
// logical lines. After a copy the statusMsg carries "copied …", so the count is
// prefixed "copied · " — the selection persists past a copy (Req 7), so the
// confirmation rides alongside the still-live count rather than replacing it. The
// whole segment is muted so it reads as chrome, not an alert.
func (m Model) selectionStatus() string {
	muted := m.deps.Theme.Style("muted")
	txt := selectedText(m.vp.GetContent(), m.sel)
	chars := len([]rune(txt))
	startL, _, endL, _ := m.sel.normalize()
	lines := endL - startL + 1
	count := fmt.Sprintf("%s · %s", plural(chars, "char"), plural(lines, "line"))
	// statusMsg is lipgloss-rendered (ANSI-wrapped), so strip before matching the
	// "copied" sentinel copySelection sets.
	if strings.Contains(ansi.Strip(m.statusMsg), "copied") {
		return muted.Render("copied · " + count)
	}
	return muted.Render(count)
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
	// and hides once the team is done, whereas openTeam opens on the last-seen
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

// queuePreviewLimit is the number of staged follow-ups previewed in the queue
// card; the rest are summarised as a "+K more" line so a deep queue stays compact.
const queuePreviewLimit = 3

// queuePreviewWidth caps each preview line's display width so a long staged prompt
// can't blow out the card; truncate appends an ellipsis past the cap.
const queuePreviewWidth = 60

// renderQueue draws the "staged follow-ups" card shown just above the input
// whenever the queue is non-empty. It reuses existing theme slots only (muted /
// toolArgs for the normal card, ctxWarn for the paused header) — no new slots — so
// it inherits the palette/card visual language. Returns "" for an empty queue (View
// omits it then).
//
// Two states:
//   - DRAINING (m.queuePaused == ""): a run is streaming (or about to) and the queue
//     will fire FIFO at its clean completion — a muted "⏳ N queued".
//   - PAUSED (m.queuePaused != ""): the last run ended on a non-clean stop (error /
//     cancel / repeated failures / stream close) so the queue is HELD, not fired. A
//     ctxWarn "⏸ N queued · paused: <reason>" header plus the resume/clear keys, so
//     a held queue never reads as a silent hang (the whole point of this card).
func (m Model) renderQueue() string {
	n := len(m.queued)
	if n == 0 {
		return ""
	}
	th := m.deps.Theme
	muted := th.Style("muted")
	var b strings.Builder
	if m.queuePaused != "" {
		reason, _ := stopReasonLabel(m.queuePaused)
		b.WriteString(th.Style("ctxWarn").Render(fmt.Sprintf("⏸ %d queued · paused: %s", n, reason)))
	} else {
		b.WriteString(muted.Render(fmt.Sprintf("⏳ %d queued", n)))
	}
	shown := min(n, queuePreviewLimit)
	for i := 0; i < shown; i++ {
		b.WriteString("\n" + th.Style("toolArgs").Render("  "+truncate(oneLine(m.queued[i]), queuePreviewWidth)))
	}
	if rest := n - shown; rest > 0 {
		b.WriteString("\n" + muted.Render(fmt.Sprintf("  +%d more", rest)))
	}
	if m.queuePaused != "" {
		b.WriteString("\n" + muted.Render("  enter sends next · esc clears"))
	}
	return th.Style("askCard").Render(b.String())
}

// renderInput renders the textarea (now always focused — it stays editable while a
// run streams so a follow-up can be composed and enqueued).
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
