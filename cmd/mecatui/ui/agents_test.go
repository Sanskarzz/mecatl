package ui

// Tests for the ctrl+a agent-team hierarchy OVERLAY: a full-screen, uncapped view
// of the most-recent Team tool card's roster, plus a per-member focus pane. It is
// additive over the inline Team card (which stays capped at maxTeamLanes with a
// "· +K more" roll-up) — the overlay is the overflow home that shows the WHOLE
// team. It reads the live lanes already accumulated in the conversation (no new
// events / RPCs) and is an idle-only affordance, like the MCP overlays.

import (
	"fmt"
	"strings"
	"testing"

	tea "charm.land/bubbletea/v2"

	"github.com/stacklok/mecatl/cmd/mecatui/client"
)

// seedTeam appends a Team tool block to the model's conversation and applies the
// given team.* projection via the conversation accumulators, so the model has a
// populated team for the overlay to open over. It mirrors teamCard's build seam
// but operates on the live Model conversation.
func seedTeam(m Model, build func(c *conversation)) Model {
	m.conv.addTool("t1", "Team", `{"goal":"ship the feature"}`)
	build(&m.conv)
	m.refreshView()
	return m
}

// bigRoster builds an N-member roster (a lead + N-1 numbered members) for the
// uncapped/windowed overlay tests — deliberately larger than maxTeamLanes so the
// inline cap and the overlay's windowed behaviour diverge observably. Members
// carry a role so the roster-row role rendering is exercised at scale.
func bigRoster(n int) []client.TeamMemberSpec {
	r := []client.TeamMemberSpec{{Name: "lead", Role: "coordinator", Lead: true, Mutating: true}}
	for i := 0; i < n-1; i++ {
		r = append(r, client.TeamMemberSpec{Name: "member-" + string(rune('a'+i)), Role: "worker"})
	}
	return r
}

// TestAgentsOpensRoster asserts ctrl+a over a populated team opens the roster.
func TestAgentsOpensRoster(t *testing.T) {
	m := newMCPModel(t, aztec(), nil)
	m = seedTeam(m, func(c *conversation) {
		c.setTeamStart("t1", "", roster())
		c.addTeamMember(member("scout", "tool.call", client.TeamMsg{ToolName: "Grep"}))
	})
	mm, _ := m.Update(ctrlKey('a'))
	m = mm.(Model)
	if m.agents.view != agentsRoster {
		t.Fatalf("view = %v, want agentsRoster", m.agents.view)
	}
	out := stripANSIstr(m.View().Content)
	if !strings.Contains(out, "agents · 2 members") {
		t.Errorf("roster header missing, got %q", out)
	}
	if !strings.Contains(out, "[lead]") || !strings.Contains(out, "scout") {
		t.Errorf("roster should list lead + scout, got %q", out)
	}
	if !strings.Contains(out, "enter focus") {
		t.Errorf("footer hint missing, got %q", out)
	}
}

// TestAgentsNoTeamIsNoOp asserts ctrl+a with no team is a no-op (overlay stays
// closed) and surfaces a brief hint.
func TestAgentsNoTeamIsNoOp(t *testing.T) {
	m := newMCPModel(t, aztec(), nil)
	mm, _ := m.Update(ctrlKey('a'))
	m = mm.(Model)
	if m.agents.view != agentsNone {
		t.Fatalf("overlay opened with no team: %v", m.agents.view)
	}
	if m.statusMsg != "no active team" {
		t.Errorf("status = %q, want 'no active team'", m.statusMsg)
	}
}

// TestAgentsGatedWhileRunning asserts the overlay is idle-only (cannot open
// mid-run), like the MCP overlays.
func TestAgentsGatedWhileRunning(t *testing.T) {
	m := newMCPModel(t, aztec(), nil)
	m = seedTeam(m, func(c *conversation) { c.setTeamStart("t1", "", roster()) })
	m.phase = phaseRunning
	mm, _ := m.openAgents()
	if mm.(Model).agents.view != agentsNone {
		t.Error("overlay opened while running")
	}
}

// TestAgentsSelectionAndFocus asserts ↑/↓ move the selection, enter focuses the
// selected member (rendering its trace), esc returns to the roster, and esc again
// closes the overlay.
func TestAgentsSelectionAndFocus(t *testing.T) {
	m := newMCPModel(t, aztec(), nil)
	m = seedTeam(m, func(c *conversation) {
		c.setTeamStart("t1", "", roster())
		c.addTeamMember(member("scout", "message.delta", client.TeamMsg{Text: "searching the codebase"}))
		c.addTeamMember(member("scout", "tool.call", client.TeamMsg{ToolName: "Grep", Detail: "pattern: handleErr"}))
		c.addTeamMember(member("scout", "tool.result", client.TeamMsg{ToolName: "Grep", Detail: "3 matches"}))
	})
	mm, _ := m.Update(ctrlKey('a'))
	m = mm.(Model)

	// Lead sorts first (cursor 0). Down → cursor 1 (scout).
	mm, _ = m.Update(tea.KeyPressMsg{Code: tea.KeyDown})
	m = mm.(Model)
	if m.agents.cursor != 1 {
		t.Fatalf("cursor = %d after down, want 1", m.agents.cursor)
	}

	// Enter → focus scout.
	mm, _ = m.Update(tea.KeyPressMsg{Code: tea.KeyEnter})
	m = mm.(Model)
	if m.agents.view != agentsFocus || m.agents.member != "scout" {
		t.Fatalf("focus state = %v/%q, want focus/scout", m.agents.view, m.agents.member)
	}
	out := stripANSIstr(m.View().Content)
	if !strings.Contains(out, "agent · scout") {
		t.Errorf("focus header missing, got %q", out)
	}
	if !strings.Contains(out, "searching the codebase") || !strings.Contains(out, "Grep") {
		t.Errorf("focus pane should show the member's trace, got %q", out)
	}
	if !strings.Contains(out, "3 matches") {
		t.Errorf("focus pane should show the bounded Detail preview, got %q", out)
	}

	// esc → back to roster.
	mm, _ = m.Update(tea.KeyPressMsg{Code: tea.KeyEsc})
	m = mm.(Model)
	if m.agents.view != agentsRoster || m.agents.member != "" {
		t.Fatalf("esc from focus = %v/%q, want roster/empty", m.agents.view, m.agents.member)
	}

	// esc → closed.
	mm, _ = m.Update(tea.KeyPressMsg{Code: tea.KeyEsc})
	m = mm.(Model)
	if m.agents.view != agentsNone {
		t.Fatalf("esc from roster did not close: %v", m.agents.view)
	}
}

// TestAgentsRosterUncapped asserts the overlay shows EVERY member (uncapped),
// even well beyond the inline maxTeamLanes cap — it is the overflow home the
// inline card defers to. A 9-member roster shows all 9 lanes (no "+K more").
func TestAgentsRosterUncapped(t *testing.T) {
	const n = 9
	if n <= maxTeamLanes {
		t.Fatalf("test premise broken: n=%d must exceed maxTeamLanes=%d", n, maxTeamLanes)
	}
	big := bigRoster(n)
	m := newMCPModel(t, aztec(), nil)
	m = seedTeam(m, func(c *conversation) { c.setTeamStart("t1", "", big) })
	mm, _ := m.Update(ctrlKey('a'))
	m = mm.(Model)
	out := stripANSIstr(m.View().Content)

	// Every member name is present.
	for _, mem := range big {
		if !strings.Contains(out, mem.Name) {
			t.Errorf("member %q missing from uncapped overlay, got %q", mem.Name, out)
		}
	}
	// Count lane glyph lines — all n must render (no inline-style roll-up).
	lanes := 0
	for _, ln := range strings.Split(out, "\n") {
		if strings.Contains(ln, "◆") || strings.Contains(ln, "○") {
			lanes++
		}
	}
	if lanes != n {
		t.Errorf("overlay rendered %d lanes, want all %d (uncapped)", lanes, n)
	}
	if strings.Contains(out, "more") {
		t.Errorf("overlay should not roll up overflow (it is uncapped), got %q", out)
	}
	if !strings.Contains(out, "agents · 9 members") {
		t.Errorf("header should report 9 members, got %q", out)
	}
}

// TestAgentsRosterLeadFirst asserts the roster anchors the lead at the top even
// when the server sent it last, reusing teamLaneOrder.
func TestAgentsRosterLeadFirst(t *testing.T) {
	leadLast := []client.TeamMemberSpec{
		{Name: "scout"},
		{Name: "builder", Mutating: true},
		{Name: "lead", Lead: true, Mutating: true},
	}
	m := newMCPModel(t, aztec(), nil)
	m = seedTeam(m, func(c *conversation) { c.setTeamStart("t1", "", leadLast) })
	mm, _ := m.Update(ctrlKey('a'))
	m = mm.(Model)
	out := stripANSIstr(m.View().Content)
	var laneLines []string
	for _, ln := range strings.Split(out, "\n") {
		if strings.Contains(ln, "◆") || strings.Contains(ln, "○") {
			laneLines = append(laneLines, ln)
		}
	}
	if len(laneLines) < 1 || !strings.Contains(laneLines[0], "lead") {
		t.Errorf("lead lane should render first in the overlay, got %q", laneLines)
	}
}

// TestAgentsShowsLatestTeam asserts that with two Team cards in the conversation,
// the overlay shows the MOST-RECENT one (latestTeamBlock scans from the end).
func TestAgentsShowsLatestTeam(t *testing.T) {
	m := newMCPModel(t, aztec(), nil)
	m.conv.addTool("ta", "Team", `{}`)
	m.conv.setTeamStart("ta", "", []client.TeamMemberSpec{{Name: "alpha", Lead: true}})
	m.conv.addTool("tb", "Team", `{}`)
	m.conv.setTeamStart("tb", "", []client.TeamMemberSpec{{Name: "bravo", Lead: true}})
	m.refreshView()

	mm, _ := m.Update(ctrlKey('a'))
	m = mm.(Model)
	out := stripANSIstr(m.View().Content)
	if !strings.Contains(out, "bravo") {
		t.Errorf("overlay should show the latest team (bravo), got %q", out)
	}
	if strings.Contains(out, "alpha") {
		t.Errorf("overlay should NOT show the earlier team (alpha), got %q", out)
	}
}

// TestAgentsResolvedSubhead asserts the roster sub-header shows the round count +
// stop reason once the team has ended.
func TestAgentsResolvedSubhead(t *testing.T) {
	m := newMCPModel(t, aztec(), nil)
	m = seedTeam(m, func(c *conversation) {
		c.setTeamStart("t1", "", roster())
		c.setTeamEnd("t1", "", 4, "end_turn", client.Usage{InputTokens: 5200, OutputTokens: 410})
	})
	mm, _ := m.Update(ctrlKey('a'))
	m = mm.(Model)
	out := stripANSIstr(m.View().Content)
	if !strings.Contains(out, "4 rounds") || !strings.Contains(out, "stop:done") {
		t.Errorf("resolved roster sub-header missing rounds/stop, got %q", out)
	}
	if !strings.Contains(out, "↑5.2K") {
		t.Errorf("resolved sub-header should show summed usage, got %q", out)
	}
}

// resize sends a WindowSizeMsg so a test can pick the overlay height (the window
// capacity derives from vp.Height() = h - 8). Returns the resized model.
func resize(m Model, w, h int) Model {
	mm, _ := m.Update(tea.WindowSizeMsg{Width: w, Height: h})
	return mm.(Model)
}

// countLanes counts roster lane lines (rows carrying a state glyph) in a stripped
// overlay frame.
func countLanes(out string) int {
	n := 0
	for _, ln := range strings.Split(out, "\n") {
		if strings.Contains(ln, "◆") || strings.Contains(ln, "○") {
			n++
		}
	}
	return n
}

// TestAgentsRosterWindowed asserts a roster larger than the available height is
// WINDOWED: only as many rows as fit render, the footer hint stays visible, and
// the hidden rows are surfaced via the "+K below"/"+K above" tails. This is the
// height-safety property the inline card has and the uncapped overlay was missing.
func TestAgentsRosterWindowed(t *testing.T) {
	const n = 20
	big := bigRoster(n)
	m := newMCPModel(t, aztec(), nil)
	m = resize(m, 100, 24) // vp height 16 → ~6 lane rows
	m = seedTeam(m, func(c *conversation) { c.setTeamStart("t1", "", big) })
	mm, _ := m.Update(ctrlKey('a'))
	m = mm.(Model)
	out := stripANSIstr(m.View().Content)

	rows := agentsRosterRows(m.vp.Height())
	if rows >= n {
		t.Fatalf("test premise broken: window %d must be smaller than roster %d", rows, n)
	}
	if got := countLanes(out); got != rows {
		t.Errorf("windowed roster rendered %d lanes, want exactly the window (%d)", got, rows)
	}
	// Cursor at the top → there is more below, nothing above.
	if !strings.Contains(out, "below") {
		t.Errorf("a windowed roster with the cursor at the top should show a '+K below' tail, got %q", out)
	}
	if strings.Contains(out, "above") {
		t.Errorf("cursor at the top should NOT show a '+K above' tail, got %q", out)
	}
	// The footer hint is ALWAYS visible (never clipped by the window/tails).
	if !strings.Contains(out, "enter focus") {
		t.Errorf("footer hint clipped by the window, got %q", out)
	}
}

// TestAgentsWindowFollowsCursor asserts the window scrolls to keep the selected
// row visible: jumping to the end shows the last member and an "+K above" tail,
// and the selected row (the last member) is in-window.
func TestAgentsWindowFollowsCursor(t *testing.T) {
	const n = 20
	big := bigRoster(n)
	m := newMCPModel(t, aztec(), nil)
	m = resize(m, 100, 24)
	m = seedTeam(m, func(c *conversation) { c.setTeamStart("t1", "", big) })
	mm, _ := m.Update(ctrlKey('a'))
	m = mm.(Model)

	// end/G jumps to the last member.
	mm, _ = m.Update(tea.KeyPressMsg{Code: tea.KeyEnd})
	m = mm.(Model)
	if m.agents.cursor != n-1 {
		t.Fatalf("end did not jump to last: cursor=%d want %d", m.agents.cursor, n-1)
	}
	out := stripANSIstr(m.View().Content)
	// The last member is the alphabetically-last numbered one ("member-s" for n=20:
	// lead + members a..s).
	last := "member-" + string(rune('a'+(n-2)))
	if !strings.Contains(out, last) {
		t.Errorf("last member %q not in window after jump-to-end, got %q", last, out)
	}
	if !strings.Contains(out, "above") {
		t.Errorf("jump-to-end should show a '+K above' tail, got %q", out)
	}
	if strings.Contains(out, "below") {
		t.Errorf("jump-to-end should NOT show a '+K below' tail, got %q", out)
	}
	// The selected (highlighted) row must be the last member — find the › row.
	for _, ln := range strings.Split(out, "\n") {
		if strings.Contains(ln, "›") && !strings.Contains(ln, last) {
			t.Errorf("selected row is not the last member: %q", ln)
		}
	}

	// home/g jumps back to the first.
	mm, _ = m.Update(tea.KeyPressMsg{Code: tea.KeyHome})
	m = mm.(Model)
	if m.agents.cursor != 0 {
		t.Fatalf("home did not jump to first: cursor=%d", m.agents.cursor)
	}
}

// TestAgentsPageKeys asserts pgup/pgdn move the selection by a window's worth
// (clamped to the ends).
func TestAgentsPageKeys(t *testing.T) {
	const n = 20
	big := bigRoster(n)
	m := newMCPModel(t, aztec(), nil)
	m = resize(m, 100, 24)
	m = seedTeam(m, func(c *conversation) { c.setTeamStart("t1", "", big) })
	mm, _ := m.Update(ctrlKey('a'))
	m = mm.(Model)

	page := agentsRosterRows(m.vp.Height())
	mm, _ = m.Update(tea.KeyPressMsg{Code: tea.KeyPgDown})
	m = mm.(Model)
	if m.agents.cursor != page {
		t.Errorf("pgdn moved cursor to %d, want one page (%d)", m.agents.cursor, page)
	}
	mm, _ = m.Update(tea.KeyPressMsg{Code: tea.KeyPgUp})
	m = mm.(Model)
	if m.agents.cursor != 0 {
		t.Errorf("pgup from one page in should return to 0, got %d", m.agents.cursor)
	}
}

// TestAgentsRosterShowsRole asserts the member role is surfaced on the roster row
// (the detail the calm inline card omits).
func TestAgentsRosterShowsRole(t *testing.T) {
	m := newMCPModel(t, aztec(), nil)
	m = seedTeam(m, func(c *conversation) {
		c.setTeamStart("t1", "", []client.TeamMemberSpec{
			{Name: "lead", Role: "coordinator", Lead: true, Mutating: true},
			{Name: "scout", Role: "researcher"},
		})
	})
	mm, _ := m.Update(ctrlKey('a'))
	m = mm.(Model)
	out := stripANSIstr(m.View().Content)
	if !strings.Contains(out, "coordinator") || !strings.Contains(out, "researcher") {
		t.Errorf("roster rows should show member roles, got %q", out)
	}
}

// TestInlineTeamRollupAdvertisesOverlay asserts the INLINE Team card's "+K more"
// roll-up (shown only when the roster exceeds maxTeamLanes) advertises ctrl+a so a
// capped inline card is the discovery point for the full overlay.
func TestInlineTeamRollupAdvertisesOverlay(t *testing.T) {
	var big []client.TeamMemberSpec
	big = append(big, client.TeamMemberSpec{Name: "lead", Lead: true})
	for i := 0; i < maxTeamLanes+2; i++ {
		big = append(big, client.TeamMemberSpec{Name: "m" + string(rune('a'+i))})
	}
	out := teamCard(t, false, func(c *conversation) { c.setTeamStart("t1", "", big) })
	if !strings.Contains(out, "more · ctrl+a") {
		t.Errorf("inline roll-up should advertise the ctrl+a overlay, got %q", out)
	}

	// A small team (no roll-up) must NOT carry the hint — it has nothing to overflow.
	small := teamCard(t, false, func(c *conversation) { c.setTeamStart("t1", "", roster()) })
	if strings.Contains(small, "ctrl+a") {
		t.Errorf("a non-overflowing inline card should not advertise ctrl+a, got %q", small)
	}
}

// ctxTurnEnd builds a turn.end team.member msg carrying the per-member context
// meter fields (used input tokens + the engine window) for member name on team t1.
func ctxTurnEnd(name string, used, window int64) client.TeamMsg {
	return member(name, "turn.end", client.TeamMsg{
		Usage:         client.Usage{InputTokens: used},
		ContextUsed:   used,
		ContextWindow: window,
	})
}

// TestAgentsRosterContextMeter asserts each roster lane shows the per-member
// context band (the footer's renderContextMeter vocabulary) once a turn.end has
// carried a known window: a low-pressure member reads "ctx … NN%" with no ⚠, a
// danger-band member appends the ⚠ marker (which survives ANSI stripping), and a
// member whose window is still unknown shows its ↑/↓ usage but NO ctx/% meter.
func TestAgentsRosterContextMeter(t *testing.T) {
	m := newMCPModel(t, aztec(), nil)
	m = seedTeam(m, func(c *conversation) {
		c.setTeamStart("t1", "", []client.TeamMemberSpec{
			{Name: "lead", Role: "coordinator", Lead: true, Mutating: true},
			{Name: "low", Role: "worker"},
			{Name: "danger", Role: "worker"},
			{Name: "nowin", Role: "worker"},
		})
		c.addTeamMember(ctxTurnEnd("low", 40000, 200000))     // 20% → ok, no ⚠
		c.addTeamMember(ctxTurnEnd("danger", 190000, 200000)) // 95% → danger ⚠
		// "nowin" gets a turn.end with a 0 window: usage lands, but no meter.
		c.addTeamMember(member("nowin", "turn.end", client.TeamMsg{
			Usage: client.Usage{InputTokens: 1200}}))
	})
	mm, _ := m.Update(ctrlKey('a'))
	m = mm.(Model)
	out := stripANSIstr(m.View().Content)

	rosterLine := func(name string) string {
		for _, ln := range strings.Split(out, "\n") {
			if strings.Contains(ln, name) {
				return ln
			}
		}
		return ""
	}

	low := rosterLine("low")
	if !strings.Contains(low, "ctx ") || !strings.Contains(low, "20%") {
		t.Errorf("low-pressure lane should show 'ctx … 20%%', got %q", low)
	}
	if strings.Contains(low, ctxDangerMark) {
		t.Errorf("low-pressure lane must NOT show the ⚠ marker, got %q", low)
	}
	if !strings.Contains(low, "40K/200K") {
		t.Errorf("low lane should show used/window, got %q", low)
	}

	danger := rosterLine("danger")
	if !strings.Contains(danger, "95%") || !strings.Contains(danger, ctxDangerMark) {
		t.Errorf("danger lane should show '95%% ⚠' (⚠ surviving ANSI strip), got %q", danger)
	}
	if !strings.Contains(danger, ctxGlyphDanger) {
		t.Errorf("danger lane should use the danger fill glyph, got %q", danger)
	}

	nowin := rosterLine("nowin")
	if strings.Contains(nowin, "ctx ") || strings.Contains(nowin, "%") {
		t.Errorf("unknown-window lane must NOT draw a ctx meter, got %q", nowin)
	}
	if !strings.Contains(nowin, "↑") || !strings.Contains(nowin, "↓") {
		t.Errorf("unknown-window lane should still show ↑/↓ usage, got %q", nowin)
	}
}

// TestAgentsFocusContextMeter asserts the per-member focus pane sub-header also
// carries the context band when the member's window is known.
func TestAgentsFocusContextMeter(t *testing.T) {
	m := newMCPModel(t, aztec(), nil)
	m = seedTeam(m, func(c *conversation) {
		c.setTeamStart("t1", "", []client.TeamMemberSpec{{Name: "scout", Role: "researcher", Lead: true}})
		c.addTeamMember(ctxTurnEnd("scout", 176000, 200000)) // 88% → warn band
	})
	mm, _ := m.Update(ctrlKey('a'))
	m = mm.(Model)
	mm, _ = m.Update(tea.KeyPressMsg{Code: tea.KeyEnter})
	m = mm.(Model)
	if m.agents.view != agentsFocus {
		t.Fatalf("view = %v, want agentsFocus", m.agents.view)
	}
	out := stripANSIstr(m.View().Content)
	if !strings.Contains(out, "ctx ") || !strings.Contains(out, "88%") {
		t.Errorf("focus sub-header should carry the context meter (88%%), got %q", out)
	}
	if !strings.Contains(out, "176K/200K") {
		t.Errorf("focus meter should show used/window, got %q", out)
	}
}

// --- task sub-view ---------------------------------------------------------

// tasksTeam builds a team whose shared task list exercises every state the task
// sub-view distinguishes: a completed task, an in-progress task with an assignee,
// a pending task blocked by the incomplete task, and a pending-unblocked task.
func tasksTeam(c *conversation) {
	c.setTeamStart("t1", "", roster())
	c.setTeamTasks("t1", []client.TeamTask{
		{ID: "task-1", Description: "investigate", State: "completed", Assignee: "scout"},
		{ID: "task-2", Description: "implement fix", State: "in_progress", Assignee: "lead"},
		{ID: "task-3", Description: "review", State: "pending", Deps: []string{"task-2"}},
		{ID: "task-4", Description: "lint", State: "pending"},
	})
}

// TestSetTeamTasksAttribution asserts setTeamTasks attributes the snapshot to the
// matching Team block (returns true) and is a no-op miss (returns false) for an
// unknown parent call id — the same attribution contract setTeamEnd has.
func TestSetTeamTasksAttribution(t *testing.T) {
	c := &conversation{}
	c.addTool("t1", "Team", `{}`)
	if !c.setTeamTasks("t1", []client.TeamTask{{ID: "task-1", State: "pending"}}) {
		t.Fatal("setTeamTasks should attribute to the Team card and return true")
	}
	if got := c.blocks[0].teamTasks; len(got) != 1 || got[0].id != "task-1" {
		t.Errorf("task snapshot not stored on the block: %+v", got)
	}
	if c.setTeamTasks("nope", []client.TeamTask{{ID: "x"}}) {
		t.Error("setTeamTasks should return false for an unknown parent call id")
	}
}

// TestAgentsTasksToggle asserts t flips the roster to the task sub-view, and t/esc
// both return to the roster; esc from the roster still closes the overlay.
func TestAgentsTasksToggle(t *testing.T) {
	m := newMCPModel(t, aztec(), nil)
	m = seedTeam(m, tasksTeam)
	mm, _ := m.Update(ctrlKey('a'))
	m = mm.(Model)
	if m.agents.view != agentsRoster {
		t.Fatalf("view = %v, want agentsRoster", m.agents.view)
	}

	// t → task sub-view.
	mm, _ = m.Update(tea.KeyPressMsg{Code: 't', Text: "t"})
	m = mm.(Model)
	if m.agents.view != agentsTasks {
		t.Fatalf("t did not open the task sub-view: %v", m.agents.view)
	}

	// t → back to roster.
	mm, _ = m.Update(tea.KeyPressMsg{Code: 't', Text: "t"})
	m = mm.(Model)
	if m.agents.view != agentsRoster {
		t.Fatalf("t did not toggle back to the roster: %v", m.agents.view)
	}

	// t → tasks, then esc → back to roster.
	mm, _ = m.Update(tea.KeyPressMsg{Code: 't', Text: "t"})
	m = mm.(Model)
	mm, _ = m.Update(tea.KeyPressMsg{Code: tea.KeyEsc})
	m = mm.(Model)
	if m.agents.view != agentsRoster {
		t.Fatalf("esc from tasks should return to the roster, got %v", m.agents.view)
	}

	// esc from the roster closes the overlay.
	mm, _ = m.Update(tea.KeyPressMsg{Code: tea.KeyEsc})
	m = mm.(Model)
	if m.agents.view != agentsNone {
		t.Fatalf("esc from roster did not close: %v", m.agents.view)
	}
}

// TestAgentsTasksEmpty asserts a team with no tasks reads as a muted "(no tasks)"
// with a zeroed summary and never panics.
func TestAgentsTasksEmpty(t *testing.T) {
	m := newMCPModel(t, aztec(), nil)
	m = seedTeam(m, func(c *conversation) { c.setTeamStart("t1", "", roster()) })
	mm, _ := m.Update(ctrlKey('a'))
	m = mm.(Model)
	mm, _ = m.Update(tea.KeyPressMsg{Code: 't', Text: "t"})
	m = mm.(Model)
	if m.agents.view != agentsTasks {
		t.Fatalf("view = %v, want agentsTasks", m.agents.view)
	}
	out := stripANSIstr(m.View().Content)
	if !strings.Contains(out, "(no tasks)") {
		t.Errorf("empty task list should show '(no tasks)', got %q", out)
	}
	if !strings.Contains(out, "0 done · 0 in-progress · 0 pending") {
		t.Errorf("empty summary should report zero counts, got %q", out)
	}
}

// TestAgentsTasksSummary asserts the summary line counts each state and flags the
// blocked subset of pending tasks.
func TestAgentsTasksSummary(t *testing.T) {
	m := newMCPModel(t, aztec(), nil)
	m = seedTeam(m, tasksTeam)
	mm, _ := m.Update(ctrlKey('a'))
	m = mm.(Model)
	mm, _ = m.Update(tea.KeyPressMsg{Code: 't', Text: "t"})
	m = mm.(Model)
	out := stripANSIstr(m.View().Content)
	// 1 completed, 1 in-progress, 2 pending of which 1 (task-3, dep on the
	// in-progress task-2) is blocked.
	if !strings.Contains(out, "1 done · 1 in-progress · 2 pending(1 blocked)") {
		t.Errorf("task summary mismatch, got %q", out)
	}
	// The blocked pending task uses the ⊘ glyph (survives ANSI strip).
	if !strings.Contains(out, "⊘") {
		t.Errorf("a blocked pending task should render the ⊘ glyph, got %q", out)
	}
}

// --- goldens ---------------------------------------------------------------

// agentsGoldenTeam builds a representative team for the overlay goldens: a lead +
// three members with mixed activity (a mutating builder, a read-only scout, a
// tester), so the roster exercises lead-first ordering, the ✎/· mutating cue, and
// the … heartbeat.
func agentsGoldenTeam(c *conversation) {
	c.setTeamStart("t1", "", []client.TeamMemberSpec{
		{Name: "lead", Role: "coordinator", Lead: true, Mutating: true},
		{Name: "scout", Role: "researcher", Mutating: false},
		{Name: "builder", Role: "implementer", Mutating: true},
		{Name: "tester", Role: "verifier", Mutating: true},
	})
	c.addTeamMember(member("scout", "message.delta", client.TeamMsg{Text: "searching for the failing path"}))
	c.addTeamMember(member("scout", "tool.call", client.TeamMsg{ToolName: "Grep", Detail: "pattern: handleErr"}))
	c.addTeamMember(member("scout", "tool.result", client.TeamMsg{ToolName: "Grep", Detail: "3 matches in dispatch.go"}))
	c.addTeamMember(member("scout", "turn.end", client.TeamMsg{Usage: client.Usage{InputTokens: 1200, OutputTokens: 80}}))
	c.addTeamMember(member("builder", "tool.call", client.TeamMsg{ToolName: "Edit"}))
	c.addTeamMember(member("builder", "turn.end", client.TeamMsg{Usage: client.Usage{InputTokens: 3400, OutputTokens: 220}}))
	c.addTeamMember(member("lead", "turn.end", client.TeamMsg{Usage: client.Usage{InputTokens: 900, OutputTokens: 40}}))
}

// TestAgentsRosterGolden locks the roster overlay.
func TestAgentsRosterGolden(t *testing.T) {
	m := newMCPModel(t, aztec(), nil)
	m = seedTeam(m, agentsGoldenTeam)
	mm, _ := m.Update(ctrlKey('a'))
	m = mm.(Model)
	if m.agents.view != agentsRoster {
		t.Fatalf("view = %v, want agentsRoster", m.agents.view)
	}
	got := stripANSI([]byte(m.View().Content))
	compareGolden(t, "agents_roster.golden", got)
}

// TestAgentsTasksView locks the task sub-view golden: a team with completed,
// in-progress (assignee), pending-blocked (deps), and pending-unblocked tasks; the
// summary line with counts + the blocked annotation; one glyphed row per task.
func TestAgentsTasksView(t *testing.T) {
	m := newMCPModel(t, aztec(), nil)
	m = seedTeam(m, tasksTeam)
	mm, _ := m.Update(ctrlKey('a'))
	m = mm.(Model)
	mm, _ = m.Update(tea.KeyPressMsg{Code: 't', Text: "t"})
	m = mm.(Model)
	if m.agents.view != agentsTasks {
		t.Fatalf("view = %v, want agentsTasks", m.agents.view)
	}
	got := stripANSI([]byte(m.View().Content))
	compareGolden(t, "agents_tasks.golden", got)
}

// TestAgentsRosterWindowedGolden locks a 20-member roster WINDOWED at a ~24-row
// terminal: a bounded window of rows, the "+K below" tail, and the footer hint
// all visible (no clipping). The cursor sits a few rows in so both tails show.
func TestAgentsRosterWindowedGolden(t *testing.T) {
	big := bigRoster(20)
	m := newMCPModel(t, aztec(), nil)
	m = resize(m, 100, 24)
	m = seedTeam(m, func(c *conversation) { c.setTeamStart("t1", "", big) })
	mm, _ := m.Update(ctrlKey('a'))
	m = mm.(Model)
	// Move the cursor down past the first window so both "+K above" and "+K below"
	// tails render at once (cursor centred in the windowed slice).
	for i := 0; i < 8; i++ {
		mm, _ = m.Update(tea.KeyPressMsg{Code: tea.KeyDown})
		m = mm.(Model)
	}
	got := stripANSI([]byte(m.View().Content))
	compareGolden(t, "agents_roster_windowed.golden", got)
}

// TestAgentsFocusGolden locks the per-member focus pane (the scout, which has a
// message line + a resolved tool chip with a Detail preview).
func TestAgentsFocusGolden(t *testing.T) {
	m := newMCPModel(t, aztec(), nil)
	m = seedTeam(m, agentsGoldenTeam)
	mm, _ := m.Update(ctrlKey('a'))
	m = mm.(Model)
	// Lead is row 0; scout is row 1.
	mm, _ = m.Update(tea.KeyPressMsg{Code: tea.KeyDown})
	m = mm.(Model)
	mm, _ = m.Update(tea.KeyPressMsg{Code: tea.KeyEnter})
	m = mm.(Model)
	if m.agents.view != agentsFocus || m.agents.member != "scout" {
		t.Fatalf("focus = %v/%q, want focus/scout", m.agents.view, m.agents.member)
	}
	got := stripANSI([]byte(m.View().Content))
	compareGolden(t, "agents_focus.golden", got)
}

// verboseFocusTeam builds a single-member team whose lane carries a long trace
// (many tool chips, each with a Detail preview so each is its own line) — enough
// to overflow a short terminal's focus pane and exercise the height bound.
func verboseFocusTeam(c *conversation) {
	c.setTeamStart("t1", "", []client.TeamMemberSpec{{Name: "scout", Role: "researcher", Lead: true}})
	c.addTeamMember(member("scout", "message.delta", client.TeamMsg{Text: "investigating the whole subsystem"}))
	for i := 0; i < maxTeamTrace; i++ {
		c.addTeamMember(member("scout", "tool.call", client.TeamMsg{
			ToolName: "Read", Detail: fmt.Sprintf("file-%02d.go", i),
		}))
		c.addTeamMember(member("scout", "tool.result", client.TeamMsg{
			ToolName: "Read", Detail: fmt.Sprintf("%d lines", 100+i),
		}))
	}
}

// TestAgentsFocusWindowed asserts a verbose member's focus pane is BOUNDED to the
// available height (the same height-safety the roster window has): the trace is
// capped to the rows that fit, a "… +N more lines" tail surfaces the overflow,
// and the header + "esc back" footer are ALWAYS visible (never clipped by
// lipgloss.Place). Mirrors TestAgentsRosterWindowed.
func TestAgentsFocusWindowed(t *testing.T) {
	m := newMCPModel(t, aztec(), nil)
	m = resize(m, 100, 24) // vp height 16 → ~6 trace rows
	m = seedTeam(m, verboseFocusTeam)
	mm, _ := m.Update(ctrlKey('a'))
	m = mm.(Model)
	// scout is the only (lead) member → row 0; enter focuses it.
	mm, _ = m.Update(tea.KeyPressMsg{Code: tea.KeyEnter})
	m = mm.(Model)
	if m.agents.view != agentsFocus {
		t.Fatalf("view = %v, want agentsFocus", m.agents.view)
	}

	rows := agentsFocusRows(m.vp.Height())
	// The unbounded trace would be > rows (message + maxTeamTrace chip lines).
	if rows >= maxTeamTrace {
		t.Fatalf("test premise broken: focus window %d must be smaller than the trace", rows)
	}
	out := stripANSIstr(m.View().Content)

	// Header + footer always visible (never pushed off-screen).
	if !strings.Contains(out, "agent · scout") {
		t.Errorf("focus header clipped by the height bound, got %q", out)
	}
	if !strings.Contains(out, "esc back") {
		t.Errorf("focus footer clipped by the height bound, got %q", out)
	}
	// The overflow is surfaced by the "… +N more lines" tail.
	if !strings.Contains(out, "more line") {
		t.Errorf("a bounded focus pane should show a '… +N more lines' tail, got %q", out)
	}
	// On a TALL terminal the same trace fits with no tail (the bound is min(cap, fit)).
	tall := resize(m, 100, 80)
	tallOut := stripANSIstr(tall.View().Content)
	if strings.Contains(tallOut, "more line") {
		t.Errorf("a tall terminal should not truncate the trace, got %q", tallOut)
	}
}

// TestAgentsFocusWindowedGolden locks the bounded focus pane at a ~24-row
// terminal: a capped trace, the "… +N more lines" tail, and the header + "esc
// back" footer all visible (no clipping).
func TestAgentsFocusWindowedGolden(t *testing.T) {
	m := newMCPModel(t, aztec(), nil)
	m = resize(m, 100, 24)
	m = seedTeam(m, verboseFocusTeam)
	mm, _ := m.Update(ctrlKey('a'))
	m = mm.(Model)
	mm, _ = m.Update(tea.KeyPressMsg{Code: tea.KeyEnter})
	m = mm.(Model)
	if m.agents.view != agentsFocus {
		t.Fatalf("view = %v, want agentsFocus", m.agents.view)
	}
	got := stripANSI([]byte(m.View().Content))
	compareGolden(t, "agents_focus_windowed.golden", got)
}
