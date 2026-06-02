package ui

import (
	"context"
	"strings"
	"testing"

	tea "charm.land/bubbletea/v2"

	"github.com/stacklok/mecatl/cmd/mecatui/client"
)

// zeroStateModel builds a connected, sized, idle model with an EMPTY conversation
// and the given caps — the first-run state the welcome card renders in.
func zeroStateModel(t *testing.T, caps client.Capabilities) Model {
	t.Helper()
	recv := &fakeRecver{gate: make(chan struct{})}
	conv := &fakeConv{recv: recv, send: &fakeSender{}, caps: caps}
	m := New(Deps{
		Session:     conv,
		Conv:        conv,
		Theme:       aztec(),
		Server:      "127.0.0.1:8080",
		Workspace:   "/workspace",
		Mode:        "default",
		Model:       "mock-model",
		Ctx:         context.Background(),
		NoAltScreen: true,
	})
	m = applyAll(m,
		tea.WindowSizeMsg{Width: 100, Height: 30},
		client.SessionReadyMsg{SessionID: "sess-test-0001", Capabilities: caps},
	)
	return m
}

// TestZeroStateEmbeddedGolden locks the welcome card under embedded defaults: it
// offers "?", "/" (built-ins always exist), "ctrl+a" (teams on) and "ctrl+t",
// and notes that memory is on.
func TestZeroStateEmbeddedGolden(t *testing.T) {
	m := zeroStateModel(t, embeddedCaps())
	if !m.conv.isEmpty() {
		t.Fatal("conversation should be empty for the zero-state")
	}
	got := stripANSI([]byte(m.View().Content))
	compareGolden(t, "zerostate_embedded.golden", got)
}

// TestZeroStateAllOnGolden locks the welcome card under an all-on server: it adds
// the "/" line (commands on).
func TestZeroStateAllOnGolden(t *testing.T) {
	m := zeroStateModel(t, allOnCaps())
	got := stripANSI([]byte(m.View().Content))
	compareGolden(t, "zerostate_all_on.golden", got)
}

// TestZeroStateVanishesAfterPrompt asserts the welcome card disappears once the
// first block is appended (the conversation is no longer empty).
func TestZeroStateVanishesAfterPrompt(t *testing.T) {
	m := zeroStateModel(t, embeddedCaps())
	before := stripANSIstr(m.View().Content)
	if !strings.Contains(before, "Welcome to mecatui") {
		t.Fatalf("zero-state card missing before any prompt:\n%s", before)
	}
	m.conv.addUser("do something")
	m.refreshView()
	after := stripANSIstr(m.View().Content)
	if strings.Contains(after, "Welcome to mecatui") {
		t.Fatalf("zero-state card should vanish after the first block:\n%s", after)
	}
}

// TestZeroStateCapsTailoring asserts the affordance list tracks caps WITHOUT
// pinning layout: "/" ALWAYS appears (built-in slash commands always exist),
// "ctrl+a" only when teams are enabled, and notes only when their cap is on.
func TestZeroStateCapsTailoring(t *testing.T) {
	embedded := stripANSIstr(renderZeroState(aztec(), embeddedCaps(), 100, 24))
	allOn := stripANSIstr(renderZeroState(aztec(), allOnCaps(), 100, 24))

	// "/" is always advertised now — built-ins (/clear, /help) exist regardless of
	// server slash-command support.
	if !strings.Contains(embedded, "slash commands") {
		t.Errorf("embedded zero-state should advertise / slash commands (built-ins always exist):\n%s", embedded)
	}
	if !strings.Contains(allOn, "slash commands") {
		t.Errorf("all-on zero-state should advertise / slash commands:\n%s", allOn)
	}
	// teams on in both fixtures → ctrl+a present in both.
	if !strings.Contains(embedded, "agent team") {
		t.Errorf("embedded zero-state should advertise ctrl+a (teams on):\n%s", embedded)
	}
	// memory note gated on caps.Memory (on in both).
	if !strings.Contains(embedded, "memory is on") {
		t.Errorf("embedded zero-state should note memory is on:\n%s", embedded)
	}

	// A bare-bones server (everything off) still shows "/" (built-ins) + "?" +
	// "ctrl+t", but no teams/memory affordances.
	bare := stripANSIstr(renderZeroState(aztec(), client.Capabilities{}, 100, 24))
	if !strings.Contains(bare, "slash commands") {
		t.Errorf("bare zero-state should still advertise / (built-ins always exist):\n%s", bare)
	}
	if strings.Contains(bare, "agent team") || strings.Contains(bare, "memory is on") {
		t.Errorf("bare zero-state should not advertise teams/memory:\n%s", bare)
	}
}
