package ui

import (
	"context"
	"strings"
	"testing"

	tea "charm.land/bubbletea/v2"

	"github.com/stacklok/mecatl/cmd/mecatui/client"
	"github.com/stacklok/mecatl/cmd/mecatui/theme"
)

// fakeCommander is a scripted client.Commander for the palette tests. It returns
// a fixed command set (recording the workspace it was asked for) or an error,
// and counts calls so a test can assert the fetch fires at most once.
type fakeCommander struct {
	cmds  []client.Command
	err   error
	calls int
	gotWS string
}

func (f *fakeCommander) ListCommands(_ context.Context, workspace string) ([]client.Command, error) {
	f.calls++
	f.gotWS = workspace
	if f.err != nil {
		return nil, f.err
	}
	return f.cmds, nil
}

// newRawPaletteModel builds an idle, sized Model wired to the given Commander,
// WITHOUT pre-seeding the command set — so the lazy ListCommands fetch is still
// armed (used by the fetch test).
func newRawPaletteModel(t *testing.T, cmds client.Commander) Model {
	t.Helper()
	recv := &fakeRecver{script: nil, gate: make(chan struct{})}
	send := &fakeSender{}
	conv := &fakeConv{recv: recv, send: send}
	m := New(Deps{
		Session:     conv,
		Conv:        conv,
		Cmds:        cmds,
		Theme:       theme.New("aztec", theme.AztecPalette()),
		Server:      "127.0.0.1:8080",
		Workspace:   "/workspace",
		Mode:        "default",
		Model:       "mock-model",
		Ctx:         context.Background(),
		NoAltScreen: true,
	})
	m = applyAll(m,
		tea.WindowSizeMsg{Width: 100, Height: 30},
		client.SessionReadyMsg{SessionID: "sess-test-0001"},
	)
	return m
}

// newPaletteModel builds an idle, sized Model with the Commander's commands
// PRE-SEEDED (a CommandsMsg already applied, fetch latch flipped) so the
// per-keystroke tests need no async fetch drained — keeping them deterministic
// and fast (no cursor-blink sleep). cmds may be nil to model "no Commander".
func newPaletteModel(t *testing.T, cmds *fakeCommander) Model {
	t.Helper()
	var commander client.Commander
	if cmds != nil {
		commander = cmds
	}
	m := newRawPaletteModel(t, commander)
	if cmds != nil {
		// Pre-seed the discovered set and flip the fetch latch so syncPalette won't
		// re-issue the RPC on the first "/".
		m.palette.commands = cmds.cmds
		m.palette.fetched = true
	}
	return m
}

// typeRune feeds a single printable rune to the idle Model, DISCARDING the
// keystroke's command. The textarea's own cursor-blink command would otherwise
// make the offline test sleep on its 530ms tick; and the palette's lazy fetch is
// exercised explicitly (see TestPaletteFetchesOnce) — every other test pre-seeds
// the command set via newPaletteModel so typing needs no async fetch drained.
func typeRune(t *testing.T, m Model, r rune) Model {
	t.Helper()
	mm, _ := m.Update(tea.KeyPressMsg{Code: r, Text: string(r)})
	return mm.(Model)
}

func sampleCommands() *fakeCommander {
	return &fakeCommander{cmds: []client.Command{
		{Name: "fix", Description: "fix a failing test"},
		{Name: "review", Description: "review a pull request"},
		{Name: "refactor", Description: "refactor a function"},
	}}
}

// TestPaletteOpensOnSlash verifies typing "/" opens the palette with the
// (pre-seeded) commands and renders name + description.
func TestPaletteOpensOnSlash(t *testing.T) {
	m := newPaletteModel(t, sampleCommands())

	m = typeRune(t, m, '/')

	if !m.palette.open {
		t.Fatalf("palette did not open on '/'")
	}
	if len(m.palette.filtered) != 3 {
		t.Fatalf("filtered = %d, want 3", len(m.palette.filtered))
	}
	view := m.View().Content
	if !strings.Contains(view, "/fix") || !strings.Contains(view, "fix a failing test") {
		t.Fatalf("palette view missing command name/description:\n%s", view)
	}
}

// TestPaletteFetchesOnce verifies the palette fires ListCommands lazily — once,
// for the session workspace — on first entry into command mode, and reuses the
// cached set on subsequent keystrokes. It drives the real fetch command.
func TestPaletteFetchesOnce(t *testing.T) {
	fc := sampleCommands()
	m := newRawPaletteModel(t, fc)

	// First "/": Update returns the fetch (batched with the textarea blink). Run
	// the fetch result and feed only the CommandsMsg back, then re-derive.
	mm, cmd := m.Update(tea.KeyPressMsg{Code: '/', Text: "/"})
	m = mm.(Model)
	m = feedCommandsMsg(t, m, cmd)

	if fc.calls != 1 {
		t.Fatalf("ListCommands called %d times, want 1", fc.calls)
	}
	if fc.gotWS != "/workspace" {
		t.Fatalf("fetch workspace = %q, want /workspace", fc.gotWS)
	}
	if !m.palette.open || len(m.palette.filtered) != 3 {
		t.Fatalf("palette not populated from fetch: open=%v filtered=%d", m.palette.open, len(m.palette.filtered))
	}

	// A second keystroke must NOT re-fetch (the latch holds).
	m = typeRune(t, m, 'r')
	if fc.calls != 1 {
		t.Fatalf("ListCommands re-fetched: calls = %d, want 1", fc.calls)
	}
}

// feedCommandsMsg runs cmd (flattening a batch), reduces any CommandsMsg it
// yields, and discards the rest (notably the cursor-blink tick).
func feedCommandsMsg(t *testing.T, m Model, cmd tea.Cmd) Model {
	t.Helper()
	if cmd == nil {
		return m
	}
	msg := cmd()
	var leaves []tea.Cmd
	if batch, ok := msg.(tea.BatchMsg); ok {
		leaves = batch
	} else {
		leaves = []tea.Cmd{func() tea.Msg { return msg }}
	}
	for _, c := range leaves {
		if cm, ok := runCmd(c).(client.CommandsMsg); ok {
			mm, _ := m.Update(cm)
			m = mm.(Model)
		}
	}
	return m
}

// TestPalettePrefixFilters verifies typing more of the name narrows the list.
func TestPalettePrefixFilters(t *testing.T) {
	m := newPaletteModel(t, sampleCommands())

	m = typeRune(t, m, '/')
	m = typeRune(t, m, 'r') // "/r" → review, refactor
	if len(m.palette.filtered) != 2 {
		t.Fatalf("after '/r' filtered = %d, want 2 (review, refactor): %+v", len(m.palette.filtered), m.palette.filtered)
	}
	m = typeRune(t, m, 'e') // "/re" → review, refactor still
	m = typeRune(t, m, 'v') // "/rev" → review only
	if len(m.palette.filtered) != 1 || m.palette.filtered[0].Name != "review" {
		t.Fatalf("after '/rev' filtered = %+v, want [review]", m.palette.filtered)
	}
}

// TestPaletteNavigateAndComplete verifies ↓ moves the selection and enter
// completes the chosen command into the input as "/<name> ", closing the palette.
func TestPaletteNavigateAndComplete(t *testing.T) {
	m := newPaletteModel(t, sampleCommands())
	m = typeRune(t, m, '/') // open with fix, review, refactor (server order)

	// ↓ selects the second row.
	mm, _ := m.Update(tea.KeyPressMsg{Code: tea.KeyDown})
	m = mm.(Model)
	if m.palette.cursor != 1 {
		t.Fatalf("cursor = %d, want 1 after ↓", m.palette.cursor)
	}

	// enter completes the selected command.
	mm, _ = m.Update(tea.KeyPressMsg{Code: tea.KeyEnter})
	m = mm.(Model)
	if got := m.ta.Value(); got != "/review " {
		t.Fatalf("input = %q, want \"/review \" after complete", got)
	}
	if m.palette.open {
		t.Fatalf("palette stayed open after completion")
	}
}

// TestPaletteCompleteWithTab verifies tab also completes (the first row by
// default).
func TestPaletteCompleteWithTab(t *testing.T) {
	m := newPaletteModel(t, sampleCommands())
	m = typeRune(t, m, '/')

	mm, _ := m.Update(tea.KeyPressMsg{Code: tea.KeyTab})
	m = mm.(Model)
	if got := m.ta.Value(); got != "/fix " {
		t.Fatalf("input = %q, want \"/fix \" after tab complete", got)
	}
	if m.palette.open {
		t.Fatalf("palette stayed open after tab completion")
	}
}

// TestPaletteEscDismisses verifies esc closes the palette WITHOUT changing the
// input, and that it stays closed while still in command mode.
func TestPaletteEscDismisses(t *testing.T) {
	m := newPaletteModel(t, sampleCommands())
	m = typeRune(t, m, '/')
	m = typeRune(t, m, 'f') // input "/f"

	mm, _ := m.Update(tea.KeyPressMsg{Code: tea.KeyEsc})
	m = mm.(Model)
	if m.palette.open {
		t.Fatalf("palette open after esc")
	}
	if got := m.ta.Value(); got != "/f" {
		t.Fatalf("input = %q, want unchanged \"/f\" after esc", got)
	}
	// Typing another matching char does NOT reopen it (dismiss latched).
	m = typeRune(t, m, 'i') // "/fi"
	if m.palette.open {
		t.Fatalf("palette reopened while still in command mode after esc-dismiss")
	}
	// Clearing back out of command mode resets the latch; a fresh "/" reopens.
	m.ta.SetValue("")
	m, _ = m.syncPalette()
	m = typeRune(t, m, '/')
	if !m.palette.open {
		t.Fatalf("palette did not reopen on a fresh '/' after leaving command mode")
	}
}

// TestPaletteNoCommandsNoPalette verifies an empty command set yields no palette
// DROPDOWN (no rows). With the input on a command line ("/"), renderPalette now
// shows a single honest muted note instead of "" — caps-aware. When the input is
// NOT a command line it still returns "".
func TestPaletteNoCommandsNoPalette(t *testing.T) {
	m := newPaletteModel(t, &fakeCommander{cmds: nil})
	m = typeRune(t, m, '/')
	if m.palette.open {
		t.Fatalf("palette opened with no commands")
	}
	// Input is a command line ("/") with no rows: a caps-aware note renders.
	note := renderPalette(m.deps.Theme, m.palette, m.caps, m.ta.Value(), 100)
	if note == "" {
		t.Fatalf("renderPalette returned empty for a command line with no rows; want a caps-aware note")
	}
	// Not a command line: no palette and no note.
	if renderPalette(m.deps.Theme, m.palette, m.caps, "hello", 100) != "" {
		t.Fatalf("renderPalette non-empty for a non-command input")
	}
}

// TestPaletteNilCommanderDisabled verifies the palette never fetches or opens
// when no Commander is wired.
func TestPaletteNilCommanderDisabled(t *testing.T) {
	m := newPaletteModel(t, nil)
	m.deps.Cmds = nil
	m = typeRune(t, m, '/')
	if m.palette.open {
		t.Fatalf("palette opened with nil Commander")
	}
}

// TestPaletteClosesWhenLeavingCommandMode verifies a space after the name (args)
// or a non-command line closes the palette.
func TestPaletteClosesWhenLeavingCommandMode(t *testing.T) {
	m := newPaletteModel(t, sampleCommands())
	m = typeRune(t, m, '/')
	if !m.palette.open {
		t.Fatalf("palette did not open")
	}
	m = typeRune(t, m, 'f')
	m = typeRune(t, m, ' ') // "/f " → args mode, palette closes
	if m.palette.open {
		t.Fatalf("palette stayed open after a space (args mode)")
	}
}

// TestPaletteNotTriggeredMidLine verifies a "/" that is not at the line start
// (text precedes it) does not open the palette.
func TestPaletteNotTriggeredMidLine(t *testing.T) {
	m := newPaletteModel(t, sampleCommands())
	m = typeRune(t, m, 'h')
	m = typeRune(t, m, 'i')
	m = typeRune(t, m, '/') // "hi/" → not a command line
	if m.palette.open {
		t.Fatalf("palette opened for a mid-line '/'")
	}
}
