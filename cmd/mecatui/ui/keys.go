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

	// ExpandTools is the general "show details" toggle: full vs line-capped
	// tool-result bodies + Edit/Write diffs, and collapsed vs expanded reasoning
	// summaries. Control-modified so it never collides with textarea input.
	ExpandTools key.Binding
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
		ExpandTools: key.NewBinding(
			key.WithKeys("ctrl+t"),
			key.WithHelp("ctrl+t", "expand/collapse details"),
		),
	}
}
