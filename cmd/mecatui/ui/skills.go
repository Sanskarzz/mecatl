package ui

import (
	"strings"

	"charm.land/bubbles/v2/key"
	tea "charm.land/bubbletea/v2"
	"github.com/charmbracelet/x/ansi"

	"github.com/stacklok/mecatl/cmd/mecatui/client"
	"github.com/stacklok/mecatl/cmd/mecatui/theme"
)

// skillsView is the active skills overlay (none = closed). Like the MCP panel it
// is a read-only inventory layered over the conversation: it does not change the
// run phase, opens only while idle, and is dismissed with esc. Unlike MCP there
// is no resource/prompt picker — the inventory is the whole surface, since skill
// activation is a run-path concern the model drives, not a TUI action.
type skillsView int

const (
	skillsNone  skillsView = iota // overlay closed
	skillsPanel                   // read-only inventory (name + description)
)

// skillsState holds the skills overlay state on the Model. It is value-embedded
// so the Model stays a plain struct that Update copies. The skills slice is
// replaced wholesale on each RPC result (never mutated in place) so the
// value-copy semantics hold.
type skillsState struct {
	view    skillsView
	loading bool  // the ListSkills RPC is in flight
	err     error // the ListSkills error, rendered distinctly (nil on success)
	skills  []client.Skill
}

// openSkills opens the inventory panel and fires the ListSkills RPC. Only
// callable while idle and when a skills lister is wired; returns the model
// unchanged otherwise. The result arrives as a client.SkillsMsg handled in
// updateSkillsMsg.
func (m Model) openSkills() (tea.Model, tea.Cmd) {
	if m.phase != phaseIdle || m.deps.Skills == nil {
		return m, nil
	}
	m.ta.Blur() // overlay owns the keyboard while open
	m.skills = skillsState{view: skillsPanel, loading: true}
	return m, client.ListSkillsCmd(m.deps.Ctx, m.deps.Skills)
}

// closeSkills dismisses the overlay and returns focus to the prompt input.
func (m Model) closeSkills() (tea.Model, tea.Cmd) {
	m.skills = skillsState{}
	cmd := m.ta.Focus()
	return m, cmd
}

// onSkillsKey routes key presses while the skills overlay is open. esc closes
// it (the panel is read-only — there is nothing else to navigate). Returns
// handled=false when the overlay is closed so the caller falls through to normal
// idle key handling.
func (m Model) onSkillsKey(msg tea.KeyPressMsg) (tea.Model, tea.Cmd, bool) {
	if m.skills.view == skillsNone {
		return m, nil, false
	}
	if key.Matches(msg, m.keys.Close) {
		mm, cmd := m.closeSkills()
		return mm, cmd, true
	}
	return m, nil, true
}

// updateSkillsMsg reduces a client.SkillsMsg into the overlay state. It fires no
// follow-up command (the panel is a single-shot read), so it returns only the
// model + handled flag; handled=false for any other message so Update can fall
// through.
func (m Model) updateSkillsMsg(msg tea.Msg) (tea.Model, bool) {
	sm, ok := msg.(client.SkillsMsg)
	if !ok {
		return m, false
	}
	m.skills.loading = false
	if sm.Err != nil {
		m.skills.err = sm.Err
		return m, true
	}
	m.skills.err = nil
	m.skills.skills = sm.Skills
	return m, true
}

// renderSkillsOverlay draws the skills panel centred over the conversation region
// via centerCard (the same bordered-card treatment as the MCP/agents overlays).
// All server-derived strings are terminal-sanitized.
func renderSkillsOverlay(th theme.Theme, st skillsState, caps client.Capabilities, width, height int) string {
	if st.view != skillsPanel {
		return ""
	}
	return centerCard(th, renderSkillsPanel(th, st, caps, width), width, height)
}

// skillsTextWidth is the column budget for wrapping server-derived skill text
// (descriptions and the error line) to the card's inner width: the terminal width
// minus the askCard chrome (border + horizontal padding) and a centering margin,
// capped so lines stay readable on very wide terminals. A non-positive or very
// narrow terminal returns 0, which disables wrapping so an unknown size renders
// the bare, content-sized card like the other overlays do.
func skillsTextWidth(width int) int {
	const (
		chrome = 6   // askCard border(2) + horizontal padding(2*2)
		margin = 4   // breathing room so the centered card isn't flush to the edge
		maxW   = 100 // cap so prose stays readable on very wide terminals
		minW   = 20  // below this, don't wrap (degrade to the bare card)
	)
	w := width - chrome - margin
	if w > maxW {
		w = maxW
	}
	if w < minW {
		return 0
	}
	return w
}

// indentWrap word-wraps s to the text budget and indents every resulting line by
// two spaces (the inventory's description indent) so continuation lines align
// under the first. A budget <= 0 falls back to a single indented line (unknown
// size). ansi.Wrap breaks over-long tokens too, so a space-free string can't
// overflow the card.
func indentWrap(s string, budget int) string {
	const indent = "  "
	if budget <= 0 {
		return indent + s
	}
	wrapped := ansi.Wrap(s, budget-len(indent), "")
	lines := strings.Split(wrapped, "\n")
	for i, ln := range lines {
		lines[i] = indent + ln
	}
	return strings.Join(lines, "\n")
}

// skillsDisabledNote is the empty-inventory copy when skills are NOT enabled on
// the connected server (caps.Skills == false), with the remedy. Like the MCP
// panel, the relayed caps let the ui distinguish "not enabled" from "enabled but
// empty", which a ui-local guess never could for an external server.
const skillsDisabledNote = "Skills are not enabled on this server.\n" +
	"Run a mecated with a skills dir configured (or pass --server to one) to use them."

// skillsEmptyCopy returns the empty-state line: the "not enabled" note (with
// remedy) when caps.Skills is false, else the "enabled but empty" note.
func skillsEmptyCopy(caps client.Capabilities) string {
	if !caps.Skills {
		return skillsDisabledNote
	}
	return "No skills configured on this server."
}

// renderSkillsPanel renders the read-only inventory: one row per skill (name +
// description), name-sorted by the server. EVERY server-derived string is
// terminal-sanitized.
func renderSkillsPanel(th theme.Theme, st skillsState, caps client.Capabilities, width int) string {
	var b strings.Builder
	b.WriteString(th.Style("askTitle").Render("Skills inventory") + "\n\n")

	budget := skillsTextWidth(width)
	switch {
	case st.loading:
		b.WriteString(th.Style("muted").Render("loading…") + "\n")
	case st.err != nil:
		line := "list skills: " + sanitizeTerminal(st.err.Error())
		if budget > 0 {
			line = ansi.Wrap(line, budget, "")
		}
		b.WriteString(th.Style("errorText").Render(line) + "\n")
	case len(st.skills) == 0:
		b.WriteString(th.Style("muted").Render(skillsEmptyCopy(caps)) + "\n")
	default:
		for _, s := range st.skills {
			b.WriteString(th.Style("toolName").Render(sanitizeTerminal(s.Name)) + "\n")
			if s.Description != "" {
				b.WriteString(th.Style("toolArgs").Render(indentWrap(sanitizeTerminal(s.Description), budget)) + "\n")
			}
		}
	}

	b.WriteString("\n" + th.Style("muted").Render("skills activate automatically when relevant · esc close"))
	return b.String()
}
