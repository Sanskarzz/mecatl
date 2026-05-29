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
	}
}
