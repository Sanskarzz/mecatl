package ui

import (
	"context"
	"errors"
	"strings"
	"testing"

	tea "charm.land/bubbletea/v2"
	"github.com/charmbracelet/x/ansi"

	"github.com/stacklok/mecatl/cmd/mecatui/client"
	"github.com/stacklok/mecatl/cmd/mecatui/theme"
)

// newSkillsModel builds an idle, sized Model wired to the given fakeSkills and
// the given caps, ready to open the /skills panel. It mirrors newMCPModel.
func newSkillsModel(t *testing.T, sk client.SkillLister, caps client.Capabilities) Model {
	t.Helper()
	recv := &fakeRecver{script: nil, gate: make(chan struct{})}
	send := &fakeSender{}
	conv := &fakeConv{recv: recv, send: send, caps: caps}
	m := New(Deps{
		Session:     conv,
		Conv:        conv,
		Skills:      sk,
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

func sampleSkills() *fakeSkills {
	return &fakeSkills{
		skills: []client.Skill{
			{Name: "code-review", Description: "review a diff for bugs"},
			{Name: "deep-research", Description: "fan-out web research"},
		},
	}
}

// TestRunSkillsOpensPanel asserts runSkills opens the panel, blurs the input,
// and fires the ListSkills RPC whose result lands the inventory.
func TestRunSkillsOpensPanel(t *testing.T) {
	fs := sampleSkills()
	m := newSkillsModel(t, fs, client.Capabilities{Skills: true})

	mm, cmd := m.runSkills()
	m = mm.(Model)
	if m.skills.view != skillsPanel {
		t.Fatalf("view = %v, want skillsPanel", m.skills.view)
	}
	if !m.skills.loading {
		t.Error("panel should be loading until the RPC result lands")
	}
	if m.ta.Focused() {
		t.Error("opening the panel should blur the textarea")
	}
	if cmd == nil {
		t.Fatal("runSkills should fire the ListSkills RPC command")
	}

	// Feed the RPC result back.
	m = feedCmd(t, m, cmd)
	if fs.calls != 1 {
		t.Errorf("ListSkills calls = %d, want 1", fs.calls)
	}
	if m.skills.loading {
		t.Error("loading should clear once the result lands")
	}
	if len(m.skills.skills) != 2 {
		t.Fatalf("skills = %#v, want 2", m.skills.skills)
	}
	body := stripANSIstr(m.View().Content)
	if !strings.Contains(body, "code-review") || !strings.Contains(body, "fan-out web research") {
		t.Errorf("panel should render the skills, got:\n%s", body)
	}
}

// TestSkillsPanelWrapsLongDescriptions locks the overflow fix: a long skill
// description must wrap to the card's inner width instead of running off the
// right edge. It renders the panel body directly at a known width and asserts
// every line fits the wrap budget AND the description actually spilled onto >1
// indented line (so the assertion would fail if wrapping were removed).
func TestSkillsPanelWrapsLongDescriptions(t *testing.T) {
	th := theme.New("aztec", theme.AztecPalette())
	const width = 100
	budget := skillsTextWidth(width)
	if budget <= 0 {
		t.Fatalf("precondition: width %d should yield a positive wrap budget", width)
	}

	long := "This is a deliberately long skill description that should wrap across " +
		"several lines instead of overflowing the panel card and running off the " +
		"right edge of the terminal the way it did before the wrapping fix landed."
	st := skillsState{view: skillsPanel, skills: []client.Skill{{Name: "wrappy", Description: long}}}

	plain := stripANSIstr(renderSkillsPanel(th, st, client.Capabilities{Skills: true}, width))
	indented := 0
	for _, ln := range strings.Split(plain, "\n") {
		if w := ansi.StringWidth(ln); w > budget {
			t.Errorf("rendered line exceeds wrap budget %d (got %d): %q", budget, w, ln)
		}
		if strings.HasPrefix(ln, "  ") && strings.TrimSpace(ln) != "" {
			indented++
		}
	}
	if indented < 2 {
		t.Errorf("long description should wrap onto >=2 indented lines, got %d:\n%s", indented, plain)
	}
}

// TestSkillsPanelGatedWhileRunning asserts the panel is idle-only and a nil
// Skills dep disables it (mirrors TestMCPOverlayGating).
func TestSkillsPanelGatedWhileRunning(t *testing.T) {
	m := newSkillsModel(t, sampleSkills(), client.Capabilities{Skills: true})
	m.phase = phaseRunning
	mm, _ := m.openSkills()
	if mm.(Model).skills.view != skillsNone {
		t.Error("panel opened while running")
	}

	m2 := newSkillsModel(t, nil, client.Capabilities{Skills: true})
	m2.deps.Skills = nil
	mm2, cmd := m2.openSkills()
	if mm2.(Model).skills.view != skillsNone {
		t.Error("panel opened with nil Skills dep")
	}
	if cmd != nil {
		t.Error("nil Skills dep should fire no command")
	}
}

// TestSkillsEscClosesPanel asserts esc closes the panel and restores idle input.
func TestSkillsEscClosesPanel(t *testing.T) {
	m := newSkillsModel(t, sampleSkills(), client.Capabilities{Skills: true})
	mm, cmd := m.runSkills()
	m = feedCmd(t, mm.(Model), cmd)
	if m.skills.view != skillsPanel {
		t.Fatalf("precondition: panel should be open, view=%v", m.skills.view)
	}

	mm2, _ := m.Update(tea.KeyPressMsg{Code: tea.KeyEsc})
	m = mm2.(Model)
	if m.skills.view != skillsNone {
		t.Fatalf("esc did not close the panel: %v", m.skills.view)
	}
	if !m.ta.Focused() {
		t.Error("esc should restore focus to the textarea")
	}
}

// TestSkillsErrorRendered asserts a ListSkills failure surfaces in the panel
// rather than silently degrading.
func TestSkillsErrorRendered(t *testing.T) {
	fs := &fakeSkills{err: errors.New("boom")}
	m := newSkillsModel(t, fs, client.Capabilities{Skills: true})
	mm, cmd := m.runSkills()
	m = feedCmd(t, mm.(Model), cmd)

	if m.skills.err == nil {
		t.Fatal("a ListSkills error should be recorded on the panel state")
	}
	body := stripANSIstr(m.View().Content)
	if !strings.Contains(body, "list skills") || !strings.Contains(body, "boom") {
		t.Errorf("panel should render the error, got:\n%s", body)
	}
}

// TestSkillsPanelLoadingRender asserts the loading branch of renderSkillsPanel:
// open the panel but do NOT feed the result cmd, so st.loading stays true, and
// assert the rendered output carries the loading indicator (mirrors mcp_test.go's
// in-flight "refreshing…" footer assertion).
func TestSkillsPanelLoadingRender(t *testing.T) {
	m := newSkillsModel(t, sampleSkills(), client.Capabilities{Skills: true})
	mm, _ := m.runSkills()
	m = mm.(Model) // deliberately NOT feeding the RPC result — stay loading.
	if !m.skills.loading {
		t.Fatalf("precondition: panel should be loading, view=%v loading=%v", m.skills.view, m.skills.loading)
	}
	body := stripANSIstr(m.View().Content)
	if !strings.Contains(body, "loading…") {
		t.Errorf("loading panel should render the loading indicator, got:\n%s", body)
	}
}

// TestUpdateSkillsMsgFallThrough asserts updateSkillsMsg returns handled=false for
// a non-SkillsMsg, so Update falls through to normal handling (streaming etc.).
func TestUpdateSkillsMsgFallThrough(t *testing.T) {
	m := newSkillsModel(t, sampleSkills(), client.Capabilities{Skills: true})
	if _, handled := m.updateSkillsMsg(tea.KeyPressMsg{Code: tea.KeyEnter}); handled {
		t.Error("updateSkillsMsg should not handle a non-SkillsMsg")
	}
}

// TestSkillsPanelSanitizesNames locks the sanitizeTerminal wrapper against
// deletion: open the panel with a skill whose NAME embeds ANSI/OSC escapes, feed
// the inventory, render, and assert no raw ESC (0x1b) survives in the output
// (mirrors sanitize_test.go's 0x1b guard). Per repo memory the literal is an
// innocuous ANSI escape, never a destructive-looking command.
func TestSkillsPanelSanitizesNames(t *testing.T) {
	fs := &fakeSkills{
		skills: []client.Skill{
			{Name: "\x1b]0;pwned\x07evil", Description: "\x1b[31mred\x1b[0m"},
		},
	}
	m := newSkillsModel(t, fs, client.Capabilities{Skills: true})
	mm, cmd := m.runSkills()
	m = feedCmd(t, mm.(Model), cmd)

	// stripANSI removes the LEGITIMATE theme styling escapes; what remains must
	// carry NO raw ESC — if any survives, it came from the server-derived skill
	// name/description and sanitizeTerminal was not applied.
	out := stripANSIstr(m.View().Content)
	if strings.ContainsRune(out, 0x1b) {
		t.Errorf("raw ESC (0x1b) leaked into the rendered panel; sanitizeTerminal not applied:\n%q", out)
	}
	// The sanitized name still renders as inert text (ESC stripped, body kept).
	if !strings.Contains(out, "]0;pwnedevil") {
		t.Errorf("sanitized skill name not rendered as inert text, got:\n%q", out)
	}
}

// TestSkillsKeySwallowsNonEsc asserts a non-esc key while the panel is open is
// swallowed (handled=true) so it never leaks into idle input.
func TestSkillsKeySwallowsNonEsc(t *testing.T) {
	m := newSkillsModel(t, sampleSkills(), client.Capabilities{Skills: true})
	mm, cmd := m.runSkills()
	m = feedCmd(t, mm.(Model), cmd)

	_, _, handled := m.onSkillsKey(tea.KeyPressMsg{Code: 'j'})
	if !handled {
		t.Error("a non-esc key while the panel is open should be swallowed (handled=true)")
	}
}

// TestSkillsErrorClearedOnSuccess asserts a success result after an error clears
// the recorded error (the panel doesn't keep showing a stale failure).
func TestSkillsErrorClearedOnSuccess(t *testing.T) {
	m := newSkillsModel(t, sampleSkills(), client.Capabilities{Skills: true})

	// First drive an error result.
	mErr, _ := m.updateSkillsMsg(client.SkillsMsg{Err: errors.New("boom")})
	m = mErr.(Model)
	if m.skills.err == nil {
		t.Fatal("precondition: error should be recorded")
	}

	// Then a successful result must clear it.
	mOK, _ := m.updateSkillsMsg(client.SkillsMsg{Skills: []client.Skill{{Name: "x"}}})
	m = mOK.(Model)
	if m.skills.err != nil {
		t.Errorf("a successful result should clear the prior error, got %v", m.skills.err)
	}
}

// TestSkillsEmptyStateNotEnabled asserts the panel distinguishes "skills not
// enabled on this server" (caps.Skills false) from "enabled but none configured".
func TestSkillsEmptyStateNotEnabled(t *testing.T) {
	// caps.Skills false, empty inventory → "not enabled" copy.
	disabled := skillsEmptyCopy(client.Capabilities{Skills: false})
	if !strings.Contains(disabled, "not enabled") {
		t.Errorf("disabled empty copy = %q, want 'not enabled' framing", disabled)
	}
	// caps.Skills true, empty inventory → "none configured" copy.
	enabled := skillsEmptyCopy(client.Capabilities{Skills: true})
	if strings.Contains(enabled, "not enabled") {
		t.Errorf("enabled empty copy should not say 'not enabled', got %q", enabled)
	}
	if !strings.Contains(enabled, "No skills configured") {
		t.Errorf("enabled empty copy = %q, want 'No skills configured'", enabled)
	}
}
