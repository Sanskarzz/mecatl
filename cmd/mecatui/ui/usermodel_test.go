package ui

import (
	"context"
	"errors"
	"strings"
	"testing"

	tea "charm.land/bubbletea/v2"

	"github.com/stacklok/mecatl/cmd/mecatui/client"
	"github.com/stacklok/mecatl/cmd/mecatui/theme"
)

// Tests for the /usermodel inspection panel: a read-only, idle-only overlay that
// fires GetUserModel and renders the current operator-facts index (key +
// description + aggregate metadata). The agent curates the user model; the panel
// only displays it.

// newUserModelModel builds an idle, sized Model wired to the given fakeUserModel
// and caps. Mirrors newSkillsModel/newAgentsInvModel.
func newUserModelModel(t *testing.T, um client.UserModelLister, caps client.Capabilities) Model {
	t.Helper()
	recv := &fakeRecver{script: nil, gate: make(chan struct{})}
	send := &fakeSender{}
	conv := &fakeConv{recv: recv, send: send, caps: caps}
	m := New(Deps{
		Session:     conv,
		Conv:        conv,
		UserModel:   um,
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
		client.SessionReadyMsg{SessionID: "sess-test-0001", Capabilities: caps},
	)
	return m
}

func sampleUserModel() *fakeUserModel {
	return &fakeUserModel{model: client.UserModel{
		Entries: []client.UserModelEntry{
			{Key: "name", Description: "the operator's name"},
			{Key: "stack", Description: "prefers Go + hexagonal architecture"},
		},
		SizeBytes: 64,
		SHA256:    "abc123def4567890",
	}}
}

// TestRunUserModelOpensPanel asserts runUserModel opens the panel, blurs the input,
// fires GetUserModel, and renders the entries once the result lands.
func TestRunUserModelOpensPanel(t *testing.T) {
	fum := sampleUserModel()
	m := newUserModelModel(t, fum, client.Capabilities{UserModel: true})

	mm, cmd := m.runUserModel()
	m = mm.(Model)
	if m.userModel.view != userModelPanel {
		t.Fatalf("view = %v, want userModelPanel", m.userModel.view)
	}
	if !m.userModel.loading {
		t.Error("panel should be loading until the RPC result lands")
	}
	if m.ta.Focused() {
		t.Error("opening the panel should blur the textarea")
	}
	if cmd == nil {
		t.Fatal("runUserModel should fire the GetUserModel RPC command")
	}

	m = feedCmd(t, m, cmd)
	if fum.calls != 1 {
		t.Errorf("GetUserModel calls = %d, want 1", fum.calls)
	}
	if !strings.Contains(m.View().Content, "operator's name") {
		t.Errorf("rendered panel missing entry description:\n%s", m.View().Content)
	}
}

// TestUserModelKeySwallowsNonEsc asserts a non-esc key while the panel is open is
// swallowed (handled=true).
func TestUserModelKeySwallowsNonEsc(t *testing.T) {
	m := newUserModelModel(t, sampleUserModel(), client.Capabilities{UserModel: true})
	mm, cmd := m.runUserModel()
	m = feedCmd(t, mm.(Model), cmd)

	_, _, handled := m.onUserModelKey(tea.KeyPressMsg{Code: 'j'})
	if !handled {
		t.Error("a non-esc key while the panel is open should be swallowed (handled=true)")
	}
}

// TestUserModelError asserts a GetUserModel error renders distinctly.
func TestUserModelError(t *testing.T) {
	fum := &fakeUserModel{err: errors.New("boom")}
	m := newUserModelModel(t, fum, client.Capabilities{UserModel: true})
	mm, cmd := m.runUserModel()
	m = feedCmd(t, mm.(Model), cmd)
	if m.userModel.err == nil {
		t.Fatal("a GetUserModel error should be recorded on the state")
	}
	if !strings.Contains(m.View().Content, "boom") {
		t.Errorf("error not surfaced in the panel:\n%s", m.View().Content)
	}
}

// --- goldens ---------------------------------------------------------------

// TestUserModelPanelGolden locks the populated inventory panel.
func TestUserModelPanelGolden(t *testing.T) {
	m := newUserModelModel(t, sampleUserModel(), client.Capabilities{UserModel: true})
	mm, cmd := m.runUserModel()
	m = feedCmd(t, mm.(Model), cmd)
	if m.userModel.view != userModelPanel {
		t.Fatalf("view = %v, want userModelPanel", m.userModel.view)
	}
	got := stripANSI([]byte(m.View().Content))
	compareGolden(t, "usermodel.golden", got)
}

// TestUserModelPanelEmptyDisabledGolden locks the "user model not enabled" empty
// state (caps.UserModel false).
func TestUserModelPanelEmptyDisabledGolden(t *testing.T) {
	m := newUserModelModel(t, &fakeUserModel{}, client.Capabilities{UserModel: false})
	mm, cmd := m.runUserModel()
	m = feedCmd(t, mm.(Model), cmd)
	got := stripANSI([]byte(m.View().Content))
	compareGolden(t, "usermodel_empty_disabled.golden", got)
}

// TestUserModelPanelEmptyEnabledGolden locks the "enabled but empty" empty state
// (caps.UserModel true, no entries).
func TestUserModelPanelEmptyEnabledGolden(t *testing.T) {
	m := newUserModelModel(t, &fakeUserModel{}, client.Capabilities{UserModel: true})
	mm, cmd := m.runUserModel()
	m = feedCmd(t, mm.(Model), cmd)
	got := stripANSI([]byte(m.View().Content))
	compareGolden(t, "usermodel_empty_enabled.golden", got)
}
