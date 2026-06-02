package ui

import "charm.land/bubbles/v2/key"

// keyMap is the mecatui key binding set. Submit/newline are distinguished in
// Update via msg.String() ("enter" vs "shift+enter") because Bubble Tea v2
// reports them as distinct KeyPressMsg strings. The approval keys are only live
// while the permission modal is open; the cancel key only while a run is active.
type keyMap struct {
	Submit  key.Binding
	Newline key.Binding
	Cancel  key.Binding
	Quit    key.Binding
	Allow   key.Binding
	Deny    key.Binding
	ScrollU key.Binding
	ScrollD key.Binding

	// MCP overlay bindings. MCPPanel toggles the read-only inventory panel;
	// Resources / Prompts open the respective pickers. They are only live while
	// idle (no run streaming), like Submit. Inside an overlay, navigation reuses
	// the list keys below; esc closes the active overlay.
	MCPPanel  key.Binding
	Resources key.Binding
	Prompts   key.Binding
	Up        key.Binding
	Down      key.Binding
	Choose    key.Binding
	Close     key.Binding
	// Refresh re-issues the active overlay's primary fetch. It is a BARE 'r'
	// (not control-modified): it is only consulted while an overlay owns the
	// keyboard (onMCPKey intercepts before the idle ctrl+o/ctrl+r/ctrl+p open
	// keys), so it never collides with the global ctrl+r resources binding nor
	// with the textarea (blurred while an overlay is open). Used by the inventory
	// panel to re-probe LIVE MCP source status.
	Refresh key.Binding

	// Tasks flips the ctrl+a agents overlay from the roster to the shared team task
	// sub-view (and back). Like Refresh it is a BARE 't' consulted ONLY inside the
	// overlay (onAgentsRosterKey / the agentsTasks branch intercept before any idle
	// open key), so it never collides with the textarea (blurred while the overlay
	// is open) nor with any global control binding.
	Tasks key.Binding

	// Jump bindings for the windowed agent-team roster (tedious to traverse with
	// ↑/↓ at the 20–32-member scale the overlay exists for): home/g jump to the
	// first member, end/G to the last. Page up/down reuse ScrollU/ScrollD (pgup/
	// pgdown) inside the overlay to move the selection by a window's worth.
	JumpTop key.Binding
	JumpEnd key.Binding

	// Agents opens the agent-team hierarchy overlay: the FULL (uncapped) roster
	// of the most-recent Team tool card, with per-member focus. Like the MCP
	// bindings it is control-modified so it never collides with textarea input,
	// and is only live while idle.
	Agents key.Binding

	// ExpandTools is the general "show details" toggle: full vs line-capped
	// tool-result bodies + Edit/Write diffs, and collapsed vs expanded reasoning
	// summaries. Control-modified so it never collides with textarea input.
	ExpandTools key.Binding

	// Help opens the "?" keys-&-features overlay. Unlike the control-modified
	// open keys, "?" is a PRINTABLE rune, so onIdleKey opens help only when the
	// prompt input is EMPTY (otherwise "?" types into the textarea); inside the
	// overlay, "?" (or esc) closes it. An open MCP/agents overlay intercepts keys
	// before this binding is ever consulted, so "?" never opens help over another
	// overlay.
	Help key.Binding
}

// defaultKeys returns the standard bindings.
func defaultKeys() keyMap {
	return keyMap{
		Submit: key.NewBinding(
			key.WithKeys("enter"),
			key.WithHelp("enter", "send"),
		),
		Newline: key.NewBinding(
			key.WithKeys("shift+enter", "ctrl+j"),
			key.WithHelp("shift+enter", "newline"),
		),
		Cancel: key.NewBinding(
			key.WithKeys("esc"),
			key.WithHelp("esc", "cancel run"),
		),
		Quit: key.NewBinding(
			key.WithKeys("ctrl+c"),
			key.WithHelp("ctrl+c", "quit"),
		),
		Allow: key.NewBinding(
			key.WithKeys("a", "y", "enter"),
			key.WithHelp("a", "allow"),
		),
		Deny: key.NewBinding(
			key.WithKeys("d", "n", "esc"),
			key.WithHelp("d", "deny"),
		),
		ScrollU: key.NewBinding(
			key.WithKeys("pgup"),
			key.WithHelp("pgup", "scroll up"),
		),
		ScrollD: key.NewBinding(
			key.WithKeys("pgdown"),
			key.WithHelp("pgdn", "scroll down"),
		),
		// ctrl+o / ctrl+r / ctrl+p: control-modified so they never collide with
		// the textarea's printable input (a bare letter must still type into the
		// prompt). 'o' = inventOry overview, 'r' = Resources, 'p' = Prompts.
		MCPPanel: key.NewBinding(
			key.WithKeys("ctrl+o"),
			key.WithHelp("ctrl+o", "MCP inventory"),
		),
		Resources: key.NewBinding(
			key.WithKeys("ctrl+r"),
			key.WithHelp("ctrl+r", "MCP resources"),
		),
		Prompts: key.NewBinding(
			key.WithKeys("ctrl+p"),
			key.WithHelp("ctrl+p", "MCP prompts"),
		),
		// ctrl+a: the Agent-team hierarchy overlay (full roster + per-member focus).
		Agents: key.NewBinding(
			key.WithKeys("ctrl+a"),
			key.WithHelp("ctrl+a", "agent team"),
		),
		Up: key.NewBinding(
			key.WithKeys("up", "k"),
			key.WithHelp("↑/k", "up"),
		),
		Down: key.NewBinding(
			key.WithKeys("down", "j"),
			key.WithHelp("↓/j", "down"),
		),
		Choose: key.NewBinding(
			key.WithKeys("enter"),
			key.WithHelp("enter", "select"),
		),
		Close: key.NewBinding(
			key.WithKeys("esc"),
			key.WithHelp("esc", "close"),
		),
		Refresh: key.NewBinding(
			key.WithKeys("r"),
			key.WithHelp("r", "refresh"),
		),
		Tasks: key.NewBinding(
			key.WithKeys("t"),
			key.WithHelp("t", "tasks"),
		),
		JumpTop: key.NewBinding(
			key.WithKeys("home", "g"),
			key.WithHelp("home/g", "first"),
		),
		JumpEnd: key.NewBinding(
			key.WithKeys("end", "G"),
			key.WithHelp("end/G", "last"),
		),
		ExpandTools: key.NewBinding(
			key.WithKeys("ctrl+t"),
			key.WithHelp("ctrl+t", "expand/collapse details"),
		),
		Help: key.NewBinding(
			key.WithKeys("?"),
			key.WithHelp("?", "help"),
		),
	}
}
