package ui

import (
	"strings"

	"github.com/stacklok/mecatl/cmd/mecatui/client"
	"github.com/stacklok/mecatl/cmd/mecatui/theme"
)

// notEnabledTag is the muted annotation appended to a chord row whose feature is
// not enabled on the connected server (per the relayed client.Capabilities). It
// replaces the old undecodable "ctrl+o/r/p MCP" footer mnemonic with an honest,
// per-feature availability cue.
const notEnabledTag = "[not enabled]"

// helpRow is one line of the decompressed chord table: a key, the action it
// performs, and whether the feature it reaches is currently available. An
// unavailable row is greyed (muted style) and tagged notEnabledTag so the user
// learns the chord exists AND that it is off on this server — the whole point of
// the Phase-A capabilities channel.
type helpRow struct {
	key       string
	action    string
	available bool // false → greyed + tagged; true → normal style
	gated     bool // false → always-available row (no caps gate, never tagged)
}

// helpKeyWidth is the fixed column width the chord keys are padded to so the
// action column aligns. It comfortably fits the widest key ("shift+enter").
const helpKeyWidth = 14

// renderHelpOverlay draws the "?" keys-&-features overlay centred over the
// conversation region, reusing the askCard + lipgloss.Place treatment the MCP
// and agents overlays use. Every availability decision reads the relayed caps
// (not a ui-local guess), so the same overlay honestly reflects an embedded
// default (mcp/commands/skills off) and an external mecated with everything on.
func renderHelpOverlay(th theme.Theme, caps client.Capabilities, width, height int) string {
	return centerCard(th, helpBody(th, caps), width, height)
}

// helpBody builds the overlay's text: a title, grouped chord sections (each row
// caps-annotated), the skills clarification, and the close hint.
func helpBody(th theme.Theme, caps client.Capabilities) string {
	muted := th.Style("muted")
	var b strings.Builder

	b.WriteString(th.Style("askTitle").Render("mecatui — keys & features") + "\n\n")

	b.WriteString(muted.Render("Prompting") + "\n")
	writeHelpRows(&b, th, []helpRow{
		{key: "enter", action: "send the prompt"},
		{key: "shift+enter", action: "newline (also ctrl+j)"},
		{key: "/", action: "slash-command palette (built-ins always; workspace commands when enabled)"},
		{key: "@", action: "attach a file: image/audio inlines as media (when supported), else inlines text"},
		{key: "ctrl+v", action: "paste a clipboard image as an attachment (when supported), else paste text"},
		{key: "esc", action: "cancel the running turn"},
	})

	b.WriteString("\n" + muted.Render("While a run is streaming") + "\n")
	writeHelpRows(&b, th, []helpRow{
		{key: "enter", action: "queue a follow-up (sends when the turn ends)"},
		{key: "esc", action: "clear staged input / queue, else cancel run"},
	})

	b.WriteString("\n" + muted.Render("Inspect (while idle)") + "\n")
	writeHelpRows(&b, th, []helpRow{
		{key: "ctrl+o", action: "MCP inventory", available: caps.MCP, gated: true},
		{key: "ctrl+r", action: "MCP resources", available: caps.MCP, gated: true},
		{key: "ctrl+p", action: "MCP prompts", available: caps.MCP, gated: true},
		{key: "ctrl+a", action: "agent team", available: caps.Teams, gated: true},
		{key: "ctrl+t", action: "expand/collapse details"},
	})

	b.WriteString("\n" + muted.Render("General") + "\n")
	writeHelpRows(&b, th, []helpRow{
		{key: "pgup/pgdn", action: "scroll the conversation"},
		{key: "?", action: "this help (on an empty prompt)"},
		{key: "ctrl+c", action: "quit (press twice; first press clears the prompt or arms, again within 3s exits)"},
	})

	// The skills clarification. Skills always ACTIVATE automatically (the model
	// decides when, not the user), but when the server advertises Skills the
	// inventory IS browsable via /skills — so the copy is caps-aware: it points at
	// /skills when enabled, and keeps the "run automatically, not browsable" framing
	// when skills are off (nothing to browse).
	b.WriteString("\n")
	if caps.Skills {
		b.WriteString(muted.Render(
			"Skills activate automatically when the model needs them; type /skills\n"+
				"to browse the skills inventory.") + "\n")
	} else {
		b.WriteString(muted.Render(
			"Skills run automatically when the model needs them — not a browsable\n"+
				"list; watch the transcript for Skill tool calls.") + "\n")
	}
	// Agent definitions, when served, are browsable via /agents (the inventory the
	// Task tool routes delegations to). Distinct from caps.Teams / ctrl+a, which is
	// the live overlay of a team that has actually run.
	if caps.Agents {
		b.WriteString(muted.Render("Type /agents to browse the agent-definition inventory.") + "\n")
	}
	if caps.SlashCommands {
		b.WriteString(muted.Render("Type / to browse slash commands.") + "\n")
	}
	if caps.Memory {
		b.WriteString(muted.Render("Cross-session memory is on — context carries across runs.") + "\n")
	}
	switch {
	case caps.Image && caps.Audio:
		b.WriteString(muted.Render("Type @ to attach a file — images and audio go to the model as media.") + "\n")
	case caps.Image:
		b.WriteString(muted.Render("Type @ to attach a file — images go to the model as media.") + "\n")
	case caps.Audio:
		b.WriteString(muted.Render("Type @ to attach a file — audio goes to the model as media.") + "\n")
	default:
		b.WriteString(muted.Render("Type @ to attach a file — this model takes text only, so files inline as text.") + "\n")
	}

	b.WriteString("\n" + muted.Render("esc or ? to close"))
	return b.String()
}

// writeHelpRows renders a group of chord rows. An available (or ungated) row uses
// the normal toolArgs style; an unavailable gated row is greyed and tagged
// notEnabledTag so its disabledness reads at a glance.
func writeHelpRows(b *strings.Builder, th theme.Theme, rows []helpRow) {
	for _, r := range rows {
		key := r.key + strings.Repeat(" ", max(0, helpKeyWidth-len(r.key)))
		line := "  " + key + r.action
		if r.gated && !r.available {
			b.WriteString(th.Style("muted").Render(line+"  "+notEnabledTag) + "\n")
		} else {
			b.WriteString(th.Style("toolArgs").Render(line) + "\n")
		}
	}
}

// renderZeroState draws the first-run welcome card in the EMPTY viewport (when
// the conversation has no blocks yet). It is NOT an overlay — it claims no
// keyboard, so typing / "/" / "?" all flow over it, and it vanishes the instant
// the first block is appended. Content is tailored to caps: it only suggests
// entry points that are reachable on this server.
func renderZeroState(th theme.Theme, caps client.Capabilities, width, height int) string {
	muted := th.Style("muted")
	var b strings.Builder

	b.WriteString(th.Style("askTitle").Render("Welcome to mecatui") + "\n\n")
	b.WriteString(th.Style("toolArgs").Render("  Type a request below and press enter.") + "\n\n")

	writeHelpRows(&b, th, zeroStateRows(caps))

	if caps.Memory {
		b.WriteString("\n" + muted.Render(
			"  Cross-session memory is on — I'll remember context across runs.") + "\n")
	}

	return centerCard(th, b.String(), width, height)
}

// zeroStateRows is the caps-tailored affordance list on the welcome card: always
// "?", always "/" (built-in commands always exist), "ctrl+a" only when teams are
// enabled, and the always-available "ctrl+t". They are rendered as ungated rows
// (no [not enabled] tags on the welcome card — it advertises only what's on).
func zeroStateRows(caps client.Capabilities) []helpRow {
	rows := []helpRow{
		{key: "?", action: "keys & features"},
		{key: "/", action: "slash commands"},
	}
	if caps.Teams {
		rows = append(rows, helpRow{key: "ctrl+a", action: "agent team (when running)"})
	}
	rows = append(rows, helpRow{key: "ctrl+t", action: "details"})
	return rows
}
