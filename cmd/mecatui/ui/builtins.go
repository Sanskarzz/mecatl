package ui

import (
	tea "charm.land/bubbletea/v2"

	"github.com/stacklok/mecatl/cmd/mecatui/client"
)

// builtin is one client-side slash command: a name (without the leading "/"), a
// short description for the palette, and a run func that acts purely on the
// Bubble Tea Model. Built-ins live in the ui package (not client) because they
// are a Model concern — they mutate model state (clear the conversation, open an
// overlay) rather than talking to the server. They coexist with server-side
// workspace commands in the palette; see mergeCommands for precedence.
//
// Note: /compact is intentionally NOT a built-in. Compaction is a server
// operation with no client-reachable RPC today, so a "/compact" built-in would
// have nothing to call. It is a follow-up that needs a proto message + server
// RPC before the ui can offer it.
type builtin struct {
	name string
	desc string
	run  func(Model) (tea.Model, tea.Cmd)
}

// builtinCommands returns the caps-filtered built-in set for the connected
// server. /clear and /help are ALWAYS present — they act purely on the Model and
// need no server feature. /mcp is present only when the server advertises MCP
// AND a Commander-independent MCP collaborator is wired (mcpWired); /agents (the
// definition inventory) only when the server advertises Agents AND an agents
// collaborator is wired (agentsWired); /team (the live-team overlay) only when
// the server advertises Teams; /skills only when the server advertises Skills
// AND a skills collaborator is wired (skillsWired). The order is fixed
// (clear, help, mcp, agents, team, skills) and locked by a test so the palette
// ordering is stable.
func builtinCommands(caps client.Capabilities, mcpWired, agentsWired, skillsWired bool) []builtin {
	out := []builtin{
		{
			name: "clear",
			desc: "clear the conversation and scrollback",
			run:  Model.runClear,
		},
		{
			name: "help",
			desc: "show keys & features",
			run:  Model.runHelp,
		},
	}
	if caps.MCP && mcpWired {
		out = append(out, builtin{
			name: "mcp",
			desc: "browse MCP inventory",
			run:  Model.runMCP,
		})
	}
	if caps.Agents && agentsWired {
		out = append(out, builtin{
			name: "agents",
			desc: "browse agent definitions",
			run:  Model.runAgentsInv,
		})
	}
	if caps.Teams {
		out = append(out, builtin{
			name: "team",
			desc: "live agent-team overlay",
			run:  Model.runTeam,
		})
	}
	if caps.Skills && skillsWired {
		out = append(out, builtin{
			name: "skills",
			desc: "browse skills inventory",
			run:  Model.runSkills,
		})
	}
	return out
}

// builtinByName looks up a built-in by name within the caps-filtered set, for
// dispatch. ok is false when no built-in by that name is currently registered
// (either unknown, or gated off on this server).
func builtinByName(caps client.Capabilities, mcpWired, agentsWired, skillsWired bool, name string) (builtin, bool) {
	for _, b := range builtinCommands(caps, mcpWired, agentsWired, skillsWired) {
		if b.name == name {
			return b, true
		}
	}
	return builtin{}, false
}

// runClear resets the conversation and all derived session state so the
// zero-state welcome card reappears (conv.isEmpty() becomes true). It is
// idle-only: while a run streams it is a no-op with an explanatory status, so a
// /clear mid-turn can't tear out the live stream's backing state.
func (m Model) runClear() (tea.Model, tea.Cmd) {
	if m.phase != phaseIdle {
		m.statusMsg = m.deps.Theme.Style("warning").Render("cannot clear while running")
		return m, nil
	}
	// resetSession (model.go) owns the full set of session-derived fields, so a
	// future such field can never be silently forgotten by /clear.
	m = m.resetSession()
	m.statusMsg = m.deps.Theme.Style("success").Render("cleared")
	m.refreshView()
	return m, nil
}

// runHelp opens the "?" keys-&-features overlay — the same state the "?" key
// sets (see onIdleKey). It blurs the textarea so the overlay owns the keyboard.
func (m Model) runHelp() (tea.Model, tea.Cmd) {
	m.showHelp = true
	m.ta.Blur()
	return m, nil
}

// runMCP opens the MCP inventory panel — the same surface ctrl+o opens. Only
// registered when caps.MCP && the MCP collaborator is wired, so openMCP's own
// nil/idle guards are belt-and-braces here.
func (m Model) runMCP() (tea.Model, tea.Cmd) {
	return m.openMCP(mcpPanel)
}

// runAgentsInv opens the agent-definition inventory panel. Only registered when
// caps.Agents && the agents collaborator is wired, so openAgentsInv's own
// nil/idle guards are belt-and-braces here.
func (m Model) runAgentsInv() (tea.Model, tea.Cmd) {
	return m.openAgentsInv()
}

// runTeam opens the live agent-team overlay — the same surface ctrl+a opens.
// Only registered when caps.Teams.
func (m Model) runTeam() (tea.Model, tea.Cmd) {
	return m.openTeam()
}

// runSkills opens the skills inventory panel. Only registered when caps.Skills
// && the skills collaborator is wired, so openSkills's own nil/idle guards are
// belt-and-braces here.
func (m Model) runSkills() (tea.Model, tea.Cmd) {
	return m.openSkills()
}
