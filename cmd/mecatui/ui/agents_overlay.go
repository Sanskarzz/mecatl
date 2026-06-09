package ui

import (
	"fmt"
	"strings"

	"charm.land/bubbles/v2/key"
	tea "charm.land/bubbletea/v2"

	"github.com/stacklok/mecatl/cmd/mecatui/theme"
)

// agentsTab selects which body the unified ctrl+a "agents" overlay renders. The
// overlay is ONE surface with two tabs — Subagents (the flat Subagent-child fleet) and
// Teams (the in-process agent-team roster) — matching the field's "one consolidated
// agents window" convergence (Cursor's Agents Window, Claude Code's Agent View). The
// container's open/closed flag and the Teams-tab state still live on m.team
// (teamState) — the existing, tested team overlay becomes the Teams tab verbatim — so
// `m.team.view != teamNone` remains the single "overlay is open" predicate. The
// Subagents-tab state lives on m.subagents (subagentState).
type agentsTab int

const (
	tabSubagents agentsTab = iota // the flat Subagent-child fleet roster + per-child focus
	tabTeams                      // the agent-team roster + per-member focus (the former team overlay)
)

// subagentView is the active Subagents-tab sub-view (parallel to teamView): the flat
// fleet roster or one focused child's redacted chip trace. There is no none state —
// the tab is only reachable while the container (m.team.view) is open; the container's
// open flag owns "closed".
type subagentView int

const (
	subagentRoster subagentView = iota // the flat fleet roster (one row per child)
	subagentFocus                      // one selected child's redacted tool-chip trace
)

// subagentState holds the Subagents-tab overlay state on the Model. Like teamState it
// is value-embedded and holds only a sub-view, a selection cursor, and the focused
// child's ID (never a copy of the lanes — the panel reads the live fleet off the
// conversation each render). Focus is keyed by ChildID (not index) so a fleet that
// grows under the overlay can't shift focus onto the wrong child.
type subagentState struct {
	view   subagentView
	cursor int    // selected row in the fleet roster (index into the fleet order)
	child  string // the focused child's ChildID (subagentFocus)
}

// openAgents opens the unified ctrl+a agents overlay. It picks the CONTEXT-SENSITIVE
// default tab: Teams when a team is live (the team is the richer, watch-worthy
// surface), else Subagents when ≥1 subagent has run, else falls back to the team
// overlay's honest empty-state hint (so "teams not enabled" vs "nothing running yet"
// still reads). Both tabs are always reachable via `tab` once the overlay is open. It
// opens while idle OR mid-run (the deep view is most useful while agents stream) and
// stays inert under a permission modal / connecting / fatal phase.
func (m Model) openAgents() (tea.Model, tea.Cmd) {
	if m.phase != phaseIdle && m.phase != phaseRunning {
		return m, nil
	}
	teamLive := m.conv.liveTeamBlock() != nil
	haveTeam := m.conv.latestTeamBlock() != nil
	haveSub := m.conv.hasSubagents()
	if !haveTeam && !haveSub {
		// Nothing to show — surface a brief hint rather than opening an empty overlay,
		// distinguishing "teams not enabled on this server" from "nothing has run yet".
		if !m.caps.Teams {
			m.statusMsg = "agent teams are not enabled on this server"
		} else {
			m.statusMsg = "no team or subagent has run yet"
		}
		return m, nil
	}
	m.ta.Blur() // the overlay owns the keyboard while open
	// Context-sensitive default tab: Teams when one is LIVE; else Subagents when any
	// subagent has run; else Teams (a finished team is still reviewable). preferSubagentTab
	// is the single predicate, tested in isolation.
	m.agentsTab = m.preferredAgentsTab(teamLive, haveTeam, haveSub)
	m.team = teamState{view: teamRoster} // container open flag (+ Teams-tab state)
	m.subagents = subagentState{view: subagentRoster}
	return m, nil
}

// preferredAgentsTab is the context-sensitive default-tab predicate (its own function
// so it is testable in isolation): open on Subagents when subagents are running and no
// team is live; open on Teams when a team is live; otherwise prefer the tab that has
// content (Subagents if only subagents ran, else Teams).
func (Model) preferredAgentsTab(teamLive, haveTeam, haveSub bool) agentsTab {
	switch {
	case teamLive:
		return tabTeams
	case haveSub:
		return tabSubagents
	case haveTeam:
		return tabTeams
	default:
		return tabSubagents
	}
}

// closeAgents dismisses the overlay and returns focus to the prompt input.
func (m Model) closeAgents() (tea.Model, tea.Cmd) {
	m.team = teamState{}
	m.subagents = subagentState{}
	cmd := m.ta.Focus()
	return m, cmd
}

// switchAgentsTab flips the active tab (Subagents↔Teams) and resets the incoming
// tab's sub-view to its roster, so `tab` is always a clean tab switch (never lands
// mid-focus on the other tab). It does NOT close the overlay.
func (m Model) switchAgentsTab() Model {
	if m.agentsTab == tabSubagents {
		m.agentsTab = tabTeams
		m.team.view = teamRoster
	} else {
		m.agentsTab = tabSubagents
		m.subagents.view = subagentRoster
	}
	return m
}

// onAgentsKey is the unified overlay's key router, installed in onOverlayKey ahead of
// the legacy per-overlay handlers. It owns the container chrome: `tab` switches tabs,
// then it delegates to the active tab's handler (the Teams tab reuses onTeamKey
// verbatim; the Subagents tab uses onSubagentKey). Returns handled=false only when the
// overlay is closed, so the caller falls through to normal key handling.
func (m Model) onAgentsKey(msg tea.KeyPressMsg) (tea.Model, tea.Cmd, bool) {
	if m.team.view == teamNone {
		return m, nil, false
	}
	// `tab` switches tabs from EITHER tab's roster (not mid-focus — a focus pane's esc
	// steps back to its own roster first, matching the team overlay's esc semantics).
	if key.Matches(msg, m.keys.NextTab) && m.atAgentsRoster() {
		return m.switchAgentsTab(), nil, true
	}
	if m.agentsTab == tabSubagents {
		return m.onSubagentKey(msg)
	}
	return m.onTeamKey(msg)
}

// atAgentsRoster reports whether the active tab is showing its top-level roster (not a
// focus/sub-view), so `tab` only switches tabs from a roster — a focus pane's esc must
// step back to its own roster first (consistent across both tabs).
func (m Model) atAgentsRoster() bool {
	if m.agentsTab == tabSubagents {
		return m.subagents.view == subagentRoster
	}
	return m.team.view == teamRoster
}

// onSubagentKey routes keys while the Subagents tab is active. It mirrors onTeamKey:
// esc steps back from focus to the roster, then closes the overlay; the roster handler
// drives selection/focus. Returns handled=true (the overlay owns the keyboard).
func (m Model) onSubagentKey(msg tea.KeyPressMsg) (tea.Model, tea.Cmd, bool) {
	if m.subagents.view == subagentFocus {
		if key.Matches(msg, m.keys.Close) {
			m.subagents.view = subagentRoster
			m.subagents.child = ""
		}
		return m, nil, true
	}
	mm, cmd := m.onSubagentRosterKey(msg)
	return mm, cmd, true
}

// onSubagentRosterKey drives the fleet roster: up/down move the selection, pgup/pgdn
// page it, home/g·end/G jump to first/last, enter focuses the selected child by
// ChildID, esc closes the overlay. It mirrors onTeamRosterKey one-for-one (cursor is
// the single source of truth; the visible window is derived at render time).
func (m Model) onSubagentRosterKey(msg tea.KeyPressMsg) (tea.Model, tea.Cmd) {
	fleet := m.conv.subagentFleet
	n := len(fleet)
	page := teamRosterRows(m.vp.Height())
	switch {
	case key.Matches(msg, m.keys.Close):
		return m.closeAgents()
	case key.Matches(msg, m.keys.Up):
		m.subagents.cursor = clampCursor(m.subagents.cursor-1, n)
		return m, nil
	case key.Matches(msg, m.keys.Down):
		m.subagents.cursor = clampCursor(m.subagents.cursor+1, n)
		return m, nil
	case key.Matches(msg, m.keys.ScrollU):
		m.subagents.cursor = clampCursor(m.subagents.cursor-page, n)
		return m, nil
	case key.Matches(msg, m.keys.ScrollD):
		m.subagents.cursor = clampCursor(m.subagents.cursor+page, n)
		return m, nil
	case key.Matches(msg, m.keys.JumpTop):
		m.subagents.cursor = 0
		return m, nil
	case key.Matches(msg, m.keys.JumpEnd):
		m.subagents.cursor = clampCursor(n-1, n)
		return m, nil
	case key.Matches(msg, m.keys.Choose):
		if m.subagents.cursor < 0 || m.subagents.cursor >= n {
			return m, nil
		}
		m.subagents.child = fleet[m.subagents.cursor].childID
		m.subagents.view = subagentFocus
		return m, nil
	}
	return m, nil
}

// renderAgentsOverlay draws the active unified agents overlay centred over the
// conversation region. It prepends a one-line tab bar (Subagents | Teams, active tab
// highlighted) above the active tab's body, then frames the whole thing in the shared
// card. The team block may be nil (no team yet) — the Teams tab then shows an honest
// empty note rather than borrowing the Subagents body.
func renderAgentsOverlay(th theme.Theme, tab agentsTab, sub subagentState, team teamState, b *block, fleet []subagentLane, width, height int) string {
	bar := agentsTabBar(th, tab)
	// The body gets the height MINUS the tab bar + its blank line (agentsTabBarLines),
	// so the window math in the tab bodies still keeps the footer hint on-screen.
	bodyHeight := agentsBodyHeight(height)
	var body string
	if tab == tabSubagents {
		body = renderSubagentTab(th, sub, fleet, bodyHeight)
	} else {
		body = renderTeamsTab(th, team, b, bodyHeight)
	}
	return centerCard(th, bar+"\n\n"+body, width, height)
}

// agentsTabBarLines is how many vertical lines the unified overlay's tab strip costs
// (the tab bar itself + the blank line under it). The tab bodies are given the OUTER
// height minus this so their height-window math (teamRosterRows/teamFocusRows/…) keeps
// the footer hint on-screen under the tab bar.
const agentsTabBarLines = 2

// agentsBodyHeight is the height available to the active tab's body: the outer
// overlay height minus the tab bar. A non-positive (unknown) height passes through so
// the bodies' "size unknown → show all rows" path is preserved.
func agentsBodyHeight(height int) int {
	if height <= 0 {
		return height
	}
	return height - agentsTabBarLines
}

// agentsTabBar renders the "Subagents | Teams" tab strip: the active tab in the title
// style, the inactive in muted, joined by a muted separator. It is glyph-free so it
// reads identically with ANSI stripped; the active tab is distinguished by a leading
// "▸" marker (not colour alone) so a golden's stripANSI still shows which is active.
func agentsTabBar(th theme.Theme, tab agentsTab) string {
	active := th.Style("askTitle")
	muted := th.Style("muted")
	sub, teams := "Subagents", "Teams"
	if tab == tabSubagents {
		return active.Render("▸ "+sub) + muted.Render("   "+teams)
	}
	return muted.Render("  "+sub) + active.Render("   ▸ "+teams)
}

// renderTeamsTab renders the Teams tab body — the EXISTING team overlay roster /
// focus / tasks / findings sub-views verbatim, via the team.go renderers. A nil team
// block (no team has run) reads as an honest empty note so the tab is never blank.
func renderTeamsTab(th theme.Theme, st teamState, b *block, height int) string {
	if b == nil {
		muted := th.Style("muted")
		return muted.Render("no team has run this session") + "\n\n" +
			muted.Render("tab subagents · esc close")
	}
	switch st.view {
	case teamFocus:
		return renderTeamFocus(th, b, st.member, height)
	case teamTasks:
		return renderTeamTasks(th, b, height)
	case teamFindings:
		return renderTeamFindings(th, b, height)
	default:
		return renderTeamRoster(th, st, b, height)
	}
}

// renderSubagentTab renders the Subagents tab body: the flat fleet roster, or one
// focused child's redacted chip trace.
func renderSubagentTab(th theme.Theme, st subagentState, fleet []subagentLane, height int) string {
	if st.view == subagentFocus {
		return renderSubagentFocus(th, fleet, st.child, height)
	}
	return renderSubagentRoster(th, st, fleet, height)
}

// renderSubagentRoster renders the flat fleet roster WINDOWED to the available height,
// mirroring renderTeamRoster: a header (running/done counts), the slice of rows that
// fits with the selected row highlighted, "+K above/below" tails, and an always-visible
// footer hint. An empty fleet reads as a muted "(no subagents)". height<=0 shows all.
func renderSubagentRoster(th theme.Theme, st subagentState, fleet []subagentLane, height int) string {
	muted := th.Style("muted")
	var out strings.Builder

	running, done := fleetCounts(fleet)
	out.WriteString(th.Style("askTitle").Render(subagentRosterHeader(running, done)))
	out.WriteString("\n\n")

	if len(fleet) == 0 {
		out.WriteString(muted.Render("(no subagents)"))
		out.WriteString("\n\n" + muted.Render("tab teams · esc close"))
		return out.String()
	}

	cursor := clampCursor(st.cursor, len(fleet))
	start, end, above, below := teamWindow(cursor, len(fleet), teamRosterRows(height))
	if above > 0 {
		out.WriteString(muted.Render(fmt.Sprintf("  · +%d above", above)) + "\n")
	}
	for row := start; row < end; row++ {
		line := subagentRosterLine(&fleet[row])
		if row == cursor {
			out.WriteString(th.Style("askButtonActive").Render("› "+line) + "\n")
		} else {
			out.WriteString(muted.Render("  "+line) + "\n")
		}
	}
	if below > 0 {
		out.WriteString(muted.Render(fmt.Sprintf("  · +%d below", below)) + "\n")
	}

	out.WriteString("\n" + muted.Render("↑/↓ select · pgup/pgdn page · home/g·end/G first/last · enter focus · tab teams · esc close"))
	return out.String()
}

// subagentRosterHeader is the fleet roster title: "subagents · N running · M done".
func subagentRosterHeader(running, done int) string {
	return fmt.Sprintf("subagents · %d running · %d done", running, done)
}

// fleetCounts classifies the fleet slice into (running, done) — the standalone form of
// conversation.subagentFleetCounts, used by the render path which holds only the slice.
func fleetCounts(fleet []subagentLane) (running, done int) {
	for i := range fleet {
		if fleet[i].done {
			done++
		} else {
			running++
		}
	}
	return running, done
}

// subagentRosterLine is one fleet row: a state glyph (◐ running / ✓ done / ✗ error),
// the goal label, a short ChildID hash suffix (so two similar goals are unambiguous),
// the current/last child tool, the running tool count, and token usage. It is the
// Subagents analogue of teamRosterLine, holding only redacted metadata.
func subagentRosterLine(ln *subagentLane) string {
	goal := truncate(sanitizeTerminal(ln.goal), maxSubagentGoalLen)
	if goal == "" {
		goal = "subagent"
	}
	return fmt.Sprintf("%s %s #%s · %s · %s · ↑%s ↓%s",
		subagentLaneGlyph(ln),
		goal,
		shortChildID(ln.childID),
		subagentLaneState(ln),
		plural(ln.toolCount, "tool"),
		humanizeTokens(ln.usage.InputTokens),
		humanizeTokens(ln.usage.OutputTokens))
}

// maxSubagentGoalLen caps how many runes of a child's goal show on a fleet row so a
// long goal can't blow out the row width (the ChildID hash + columns follow it).
const maxSubagentGoalLen = 24

// subagentLaneGlyph is the per-child state glyph (glyph-not-colour): "✗" for a child
// that ended on an ERROR-family terminal (its stop reason maps to a hard error), "✓"
// for a clean/benign DONE, and "◐" for one still in flight. Errored-while-running
// (the last tool errored but the child has not ended) stays "◐" — a transient tool
// error is not a terminal disposition.
func subagentLaneGlyph(ln *subagentLane) string {
	if !ln.done {
		return "◐"
	}
	if subagentStopErrored(ln.stop) {
		return "✗"
	}
	return "✓"
}

// subagentLaneState derives a child's current state label for the fleet row: once
// done, the stop label (done / max-turns / budget / structured-output / error / …);
// while running, the latest child tool name (with a "…" heartbeat) or "working…" when
// none has run yet. The tool name is sanitized (server-derived).
func subagentLaneState(ln *subagentLane) string {
	if ln.done {
		return subagentStopLabel(ln.stop)
	}
	if ln.current != "" {
		return sanitizeTerminal(ln.current) + "…"
	}
	return "working…"
}

// shortChildID renders a stable short suffix of a ChildID for the fleet row, so two
// children with identical goal labels are still visually distinct (F2 §1.4 item 6).
// It takes the LAST up-to-childIDHashLen runes of the id (the id's tail carries the
// per-call discriminator, e.g. "explorer-<callID>"), sanitized.
func shortChildID(id string) string {
	id = sanitizeTerminal(id)
	r := []rune(id)
	if len(r) <= childIDHashLen {
		return id
	}
	return string(r[len(r)-childIDHashLen:])
}

// childIDHashLen is how many trailing runes of a ChildID the fleet row shows as its
// "#<hash>" disambiguator.
const childIDHashLen = 6

// renderSubagentFocus renders ONE child's detail: a header line (glyph + goal +
// current/last tool + count + usage), the context-isolation honesty note (Subagent never
// forwards child content — gauntlet #7), and the redacted tool-chip trace, height-
// bounded to the rows that fit. A focused ChildID with no matching lane (the child
// vanished — defensive) reads as a muted note. It mirrors renderTeamFocus minus the
// message lines Subagent never carries.
func renderSubagentFocus(th theme.Theme, fleet []subagentLane, child string, height int) string {
	muted := th.Style("muted")
	ln := findFleetLane(fleet, child)
	if ln == nil {
		return th.Style("askTitle").Render("subagent") + "\n\n" +
			muted.Render("subagent #"+shortChildID(child)+" is no longer in the fleet") + "\n\n" +
			muted.Render("esc back")
	}

	var out strings.Builder
	goal := truncate(sanitizeTerminal(ln.goal), maxTeamNameWidth*2)
	if goal == "" {
		goal = "subagent"
	}
	out.WriteString(th.Style("askTitle").Render("subagent · " + goal))
	out.WriteString("\n")
	out.WriteString(muted.Render(subagentRosterLine(ln)))
	out.WriteString("\n")
	out.WriteString(muted.Render("  args/results hidden (context-isolated)"))
	out.WriteString("\n\n")

	r := &renderer{th: th} // width-0 renderer: chips don't wrap, trace renders full
	trace := renderFleetChips(r, ln)
	if trace == "" {
		out.WriteString(muted.Render("(no activity yet)"))
	} else {
		out.WriteString(capRenderedLines(th, trace, teamFocusRows(height)))
	}

	out.WriteString("\n\n" + muted.Render("esc back"))
	return out.String()
}

// renderFleetChips renders a fleet lane's redacted tool-chip trace as a wrapped row of
// glyph+name chips, reusing the same chip vocabulary as the inline subagent card. It
// holds only tool names + ok/error glyphs (no args/results). Returns "" for an empty
// trace.
func renderFleetChips(r *renderer, ln *subagentLane) string {
	if len(ln.trace) == 0 {
		return ""
	}
	okStyle := r.th.Style("toolOk")
	errStyle := r.th.Style("toolErr")
	nameStyle := r.th.Style("toolName")
	chips := make([]string, 0, len(ln.trace))
	for _, c := range ln.trace {
		glyph := okStyle.Render("✓")
		if c.isError {
			glyph = errStyle.Render("✗")
		}
		chips = append(chips, glyph+" "+nameStyle.Render(sanitizeTerminal(c.name)))
	}
	return wrapChips(chips, r.chipContentWidth())
}

// findFleetLane returns the lane with the given ChildID off the fleet slice, or nil.
func findFleetLane(fleet []subagentLane, child string) *subagentLane {
	for i := range fleet {
		if fleet[i].childID == child {
			return &fleet[i]
		}
	}
	return nil
}

// subagentStopErrored reports whether a child's stop reason maps to a HARD error glyph
// (✗) rather than a benign done (✓). It mirrors subagentStopLabel's error case: error /
// cancelled / max-consecutive-failures / structured-output-retries-exhausted read as
// "✗". The budget / max-turns / max-tools / no-progress terminals are NON-error stops
// (the child still produced a usable partial), so they read as "✓" with the stop label
// naming the cap. An empty / unknown reason is benign.
func subagentStopErrored(stop string) bool {
	switch stop {
	case stopError, teamStopReasonCancelled, "max_consecutive_failures", "structured_output":
		return true
	default:
		return false
	}
}
