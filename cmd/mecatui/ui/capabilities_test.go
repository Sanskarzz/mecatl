package ui

import (
	"context"
	"testing"

	tea "charm.land/bubbletea/v2"

	"github.com/stacklok/mecatl/cmd/mecatui/client"
	"github.com/stacklok/mecatl/cmd/mecatui/theme"
)

// TestCreateSessionCmdCarriesCaps asserts the createSessionCmd builds a
// SessionReadyMsg carrying the capabilities the SessionCreator returned (the
// relayed truth), and that the SessionReadyMsg handler stores them on m.caps.
// This pins the Phase-A plumbing: caps flow create → cmd → msg → model, stored
// (and, this phase, unrendered).
func TestCreateSessionCmdCarriesCaps(t *testing.T) {
	want := client.Capabilities{MCP: true, Memory: true, Teams: true}
	conv := &fakeConv{recv: &fakeRecver{}, send: &fakeSender{}, caps: want}
	m := New(Deps{
		Session: conv,
		Conv:    conv,
		Theme:   theme.New("aztec", theme.AztecPalette()),
		Ctx:     context.Background(),
	})

	cmd := m.createSessionCmd()
	if cmd == nil {
		t.Fatal("createSessionCmd returned nil")
	}
	msg := cmd()
	ready, ok := msg.(client.SessionReadyMsg)
	if !ok {
		t.Fatalf("createSessionCmd msg = %T, want client.SessionReadyMsg", msg)
	}
	if ready.SessionID != "sess-test-0001" {
		t.Fatalf("SessionID = %q, want sess-test-0001", ready.SessionID)
	}
	if ready.Capabilities != want {
		t.Fatalf("Capabilities = %+v, want %+v", ready.Capabilities, want)
	}

	got := applyAll(m, tea.WindowSizeMsg{Width: 100, Height: 30}, ready)
	if got.caps != want {
		t.Fatalf("model caps = %+v, want %+v", got.caps, want)
	}
}

// TestSessionReadyDefaultCapsAllFalse asserts an older server (no caps field)
// leaves the model at the all-false zero value rather than over-promising.
func TestSessionReadyDefaultCapsAllFalse(t *testing.T) {
	conv := &fakeConv{recv: &fakeRecver{}, send: &fakeSender{}} // caps left zero
	m := New(Deps{
		Session: conv,
		Conv:    conv,
		Theme:   theme.New("aztec", theme.AztecPalette()),
		Ctx:     context.Background(),
	})
	ready := m.createSessionCmd()().(client.SessionReadyMsg)
	if (ready.Capabilities != client.Capabilities{}) {
		t.Fatalf("default caps = %+v, want all-false zero value", ready.Capabilities)
	}
	got := applyAll(m, tea.WindowSizeMsg{Width: 100, Height: 30}, ready)
	if (got.caps != client.Capabilities{}) {
		t.Fatalf("model caps = %+v, want all-false zero value", got.caps)
	}
}
