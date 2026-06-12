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
	// Paste (ctrl+v) reads the OS clipboard: an image stages as an inline media
	// attachment ([Image #N]), text inserts into the prompt. Distinct from a
	// bracketed paste (tea.PasteMsg, handled by onPaste) which never reaches here.
	Paste key.Binding
	Quit  key.Binding
	Allow key.Binding
	// AllowAlways (w) resolves the permission modal as always-allow: permit this
	// call AND learn a session-scoped rule so the exact command is not re-asked.
	// Offered only for the main agent's asks (never a surfaced subagent ask).
	AllowAlways key.Binding
	Deny        key.Binding
	ScrollU     key.Binding
	ScrollD     key.Binding
	// ScrollTop / ScrollBottom jump the conversation viewport to its top / bottom
	// (End naturally re-sticks auto-follow). These are DEDICATED scrollback keys
	// bound to "home"/"end" ONLY — deliberately NOT g/G, which must stay typeable
	// in prose. They are distinct from the team-overlay JumpTop/JumpEnd below
	// (home/g, end/G), which are consulted ONLY inside the agent-team overlay to
	// move the roster selection; these move the conversation scroll position.
	ScrollTop    key.Binding
	ScrollBottom key.Binding

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

	// Tasks flips the ctrl+a team overlay from the roster to the shared team task
	// sub-view (and back). Like Refresh it is a BARE 't' consulted ONLY inside the
	// overlay (onTeamRosterKey / the teamTasks branch intercept before any idle
	// open key), so it never collides with the textarea (blurred while the overlay
	// is open) nor with any global control binding.
	Tasks key.Binding

	// Findings flips the ctrl+a team overlay from the roster to the shared team
	// findings-ledger sub-view (and back). Like Tasks it is a BARE 'f' consulted
	// ONLY inside the overlay (onTeamRosterKey / the teamFindings branch intercept
	// before any idle open key), so it never collides with the blurred textarea nor
	// any global control binding.
	Findings key.Binding

	// Jump bindings for the windowed agent-team roster (tedious to traverse with
	// ↑/↓ at the 20–32-member scale the overlay exists for): home/g jump to the
	// first member, end/G to the last. Page up/down reuse ScrollU/ScrollD (pgup/
	// pgdown) inside the overlay to move the selection by a window's worth.
	JumpTop key.Binding
	JumpEnd key.Binding

	// Agents (ctrl+a) opens the unified live agents overlay: ONE surface with three
	// tabs — Subagents (the flat Subagent-child fleet), Parallel (the fan-out groups),
	// and Teams (the agent-team roster with per-member focus). The default tab is
	// context-sensitive (a live team, else a live parallel run, else whichever
	// family has activity — see agents_overlay.go). Like the MCP bindings it is
	// control-modified so it never collides with textarea input. It is live both while
	// idle AND mid-run (Gap B) — the deep view is most useful while agents stream; it
	// stays inert under a permission modal. (Mapped from /team in the palette; /agents
	// is the def inventory, palette-only.)
	Agents key.Binding

	// NextTab (tab) switches the active tab inside the unified agents overlay
	// (Subagents↔Teams). Like Tasks/Findings/Refresh it is a BARE key consulted ONLY
	// inside the overlay (onAgentsKey intercepts before any idle open key), so it
	// never collides with the blurred textarea nor any global control binding.
	NextTab key.Binding

	// CancelChild (x) cancels the selected/focused subagent lane inside the ctrl+a
	// agents overlay (non-terminal lanes only). Like Tasks/Findings it is a BARE
	// key consulted ONLY inside the overlay, so it never collides with the blurred
	// textarea nor any global control binding. Confirm-less single keypress —
	// recoverable: the child is persisted and resumable.
	CancelChild key.Binding

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

	// SetGlobalDefault (ctrl+g) sets the /models picker's CURSOR row as the client
	// global default (the model new/unseen workspaces inherit). It is CONTROL-modified
	// deliberately: a bare 'g' is ScrollTop/JumpTop (key.Matches), and the picker's
	// filter input is focused, so a bare 'g' must stay typeable in a model name
	// ("gemini"/"gpt"). Consulted ONLY inside the /models picker (onModelsKey), so it
	// never collides with the conversation scrollback or the blurred textarea.
	SetGlobalDefault key.Binding
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
		Paste: key.NewBinding(
			key.WithKeys("ctrl+v"),
			key.WithHelp("ctrl+v", "paste image"),
		),
		Quit: key.NewBinding(
			key.WithKeys("ctrl+c"),
			key.WithHelp("ctrl+c", "quit"),
		),
		Allow: key.NewBinding(
			key.WithKeys("a", "y", "enter"),
			key.WithHelp("a", "allow"),
		),
		AllowAlways: key.NewBinding(
			key.WithKeys("w"),
			key.WithHelp("w", "always allow (session)"),
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
		// home/end ONLY (not g/G — those stay typeable in prose). Distinct from the
		// team-overlay JumpTop/JumpEnd (home/g, end/G), which only move the roster
		// selection while that overlay owns the keyboard.
		ScrollTop: key.NewBinding(
			key.WithKeys("home"),
			key.WithHelp("home", "scroll to top"),
		),
		ScrollBottom: key.NewBinding(
			key.WithKeys("end"),
			key.WithHelp("end", "scroll to bottom"),
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
		// ctrl+a: the unified agents overlay (Subagents + Teams tabs).
		Agents: key.NewBinding(
			key.WithKeys("ctrl+a"),
			key.WithHelp("ctrl+a", "agents (subagents / teams)"),
		),
		// tab: switch tabs inside the agents overlay. Consulted only while the overlay
		// owns the keyboard.
		NextTab: key.NewBinding(
			key.WithKeys("tab"),
			key.WithHelp("tab", "switch tab"),
		),
		// x: cancel the selected/focused subagent lane inside the agents overlay.
		CancelChild: key.NewBinding(
			key.WithKeys("x"),
			key.WithHelp("x", "cancel subagent"),
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
		Findings: key.NewBinding(
			key.WithKeys("f"),
			key.WithHelp("f", "findings"),
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
		// ctrl+g: set the picker cursor row as the global default. ctrl-modified so a
		// bare 'g' stays typeable in the picker's filter (it is also ScrollTop/JumpTop).
		SetGlobalDefault: key.NewBinding(
			key.WithKeys("ctrl+g"),
			key.WithHelp("ctrl+g", "set global default"),
		),
	}
}
