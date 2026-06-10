package ui

import (
	"context"
	"errors"
	"fmt"
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
	budget := cardTextWidth(width)
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

// TestSkillsKeySwallowsNonEsc asserts a non-esc, non-scroll key while the panel
// is open is swallowed (handled=true) so it never leaks into idle input — and
// that a scroll key is HANDLED (not a close, not a leak) now that the panel
// scrolls.
func TestSkillsKeySwallowsNonEsc(t *testing.T) {
	m := newSkillsModel(t, sampleSkills(), client.Capabilities{Skills: true})
	mm, cmd := m.runSkills()
	m = feedCmd(t, mm.(Model), cmd)

	mm2, _, handled := m.onSkillsKey(tea.KeyPressMsg{Code: 'j'})
	if !handled {
		t.Error("a non-esc key while the panel is open should be swallowed (handled=true)")
	}
	if mm2.(Model).skills.scroll != 0 {
		t.Error("a non-scroll key should not move the scroll offset")
	}

	// A scroll key is handled too (and keeps the panel open). The 2-skill sample
	// fits the window, so the offset stays clamped at 0.
	mm3, _, handled := m.onSkillsKey(tea.KeyPressMsg{Code: tea.KeyPgDown})
	if !handled {
		t.Error("pgdown while the panel is open should be handled")
	}
	m3 := mm3.(Model)
	if m3.skills.view != skillsPanel {
		t.Error("pgdown should not close the panel")
	}
	if m3.skills.scroll != 0 {
		t.Errorf("a fitting inventory should clamp scroll at 0, got %d", m3.skills.scroll)
	}
}

// scrollSkills returns n description-less skills ("skill-00".."skill-NN") — one
// rendered row each, UNIQUE so a render bug that ignored st.scroll (always
// showing the first window) would be caught (the TestSoulScroll fixture rationale).
func scrollSkills(n int) *fakeSkills {
	fs := &fakeSkills{}
	for i := 0; i < n; i++ {
		fs.skills = append(fs.skills, client.Skill{Name: fmt.Sprintf("skill-%02d", i)})
	}
	return fs
}

// TestSkillsScroll asserts the scroll keys move (and clamp) the inventory row
// window AND that the rendered window content + the "lines X–Y of N" indicator
// actually shift (mirrors TestSoulScroll). It also locks the scroll reset on a
// fresh inventory result and that esc still closes the scrolled panel.
func TestSkillsScroll(t *testing.T) {
	// 30 one-row skills exceed skillsBodyLines (14), each window distinguishable.
	m := newSkillsModel(t, scrollSkills(30), client.Capabilities{Skills: true})
	mm, cmd := m.runSkills()
	m = feedCmd(t, mm.(Model), cmd)

	if m.skills.scroll != 0 {
		t.Fatalf("initial scroll = %d, want 0", m.skills.scroll)
	}
	// At the top: window is rows 1–14 (skill-00..skill-13); the tail is NOT visible.
	top := stripANSIstr(m.View().Content)
	if !strings.Contains(top, "skill-00") || strings.Contains(top, "skill-29") {
		t.Errorf("top window should show skill-00 and NOT skill-29, got:\n%s", top)
	}
	if !strings.Contains(top, "lines 1–14 of 30") {
		t.Errorf("top indicator should read 'lines 1–14 of 30', got:\n%s", top)
	}

	// Page down once: skill-00 leaves the top, skill-14 enters.
	mm, _, _ = m.onSkillsKey(tea.KeyPressMsg{Code: tea.KeyPgDown})
	m = mm.(Model)
	if m.skills.scroll != 1 {
		t.Errorf("scroll after pgdown = %d, want 1", m.skills.scroll)
	}
	pd := stripANSIstr(m.View().Content)
	if strings.Contains(pd, "skill-00") {
		t.Errorf("after pgdown the window should no longer show skill-00, got:\n%s", pd)
	}
	if !strings.Contains(pd, "skill-14") {
		t.Errorf("after pgdown the window should reveal skill-14, got:\n%s", pd)
	}
	if !strings.Contains(pd, "lines 2–15 of 30") {
		t.Errorf("after pgdown the indicator should read 'lines 2–15 of 30', got:\n%s", pd)
	}

	// Jump to bottom; max scroll = 30 - 14 = 16: the tail becomes visible.
	mm, _, _ = m.onSkillsKey(tea.KeyPressMsg{Code: tea.KeyEnd})
	m = mm.(Model)
	if m.skills.scroll != 16 {
		t.Errorf("scroll after End = %d, want 16 (30-14)", m.skills.scroll)
	}
	bot := stripANSIstr(m.View().Content)
	if !strings.Contains(bot, "skill-29") || strings.Contains(bot, "skill-00") {
		t.Errorf("bottom window should show skill-29 and NOT skill-00, got:\n%s", bot)
	}
	if !strings.Contains(bot, "lines 17–30 of 30") {
		t.Errorf("bottom indicator should read 'lines 17–30 of 30', got:\n%s", bot)
	}

	// Pgdown past the end clamps.
	mm, _, _ = m.onSkillsKey(tea.KeyPressMsg{Code: tea.KeyPgDown})
	m = mm.(Model)
	if m.skills.scroll != 16 {
		t.Errorf("scroll clamps at 16, got %d", m.skills.scroll)
	}

	// Page up moves back (pins the ScrollU arm — removing it must fail here).
	mm, _, _ = m.onSkillsKey(tea.KeyPressMsg{Code: tea.KeyPgUp})
	m = mm.(Model)
	if m.skills.scroll != 15 {
		t.Errorf("scroll after pgup = %d, want 15", m.skills.scroll)
	}

	// Home returns to the top.
	mm, _, _ = m.onSkillsKey(tea.KeyPressMsg{Code: tea.KeyHome})
	m = mm.(Model)
	if m.skills.scroll != 0 {
		t.Errorf("scroll after Home = %d, want 0", m.skills.scroll)
	}

	// A fresh inventory result resets a stale offset (never opens mid-list).
	mm, _, _ = m.onSkillsKey(tea.KeyPressMsg{Code: tea.KeyEnd})
	m = mm.(Model)
	mFresh, _ := m.updateSkillsMsg(client.SkillsMsg{Skills: scrollSkills(30).skills})
	m = mFresh.(Model)
	if m.skills.scroll != 0 {
		t.Errorf("a fresh SkillsMsg should reset scroll to 0, got %d", m.skills.scroll)
	}

	// esc still closes the scrolled panel.
	mm, _, _ = m.onSkillsKey(tea.KeyPressMsg{Code: tea.KeyEsc})
	if mm.(Model).skills.view != skillsNone {
		t.Error("esc should still close the scrolled panel")
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

// TestSkillsPanelScrollGolden locks the scrolled, overflowing inventory panel:
// an inventory exceeding skillsBodyLines, paged down once, so the golden carries
// the windowed rows + the "lines X–Y of N" indicator + the scroll footer hint
// (the TestSoulPanelGolden pattern; the fixed window keeps it deterministic).
func TestSkillsPanelScrollGolden(t *testing.T) {
	m := newSkillsModel(t, scrollSkills(30), client.Capabilities{Skills: true})
	mm, cmd := m.runSkills()
	m = feedCmd(t, mm.(Model), cmd)
	if m.skills.view != skillsPanel {
		t.Fatalf("view = %v, want skillsPanel", m.skills.view)
	}
	mm, _, _ = m.onSkillsKey(tea.KeyPressMsg{Code: tea.KeyPgDown})
	m = mm.(Model)
	got := stripANSI([]byte(m.View().Content))
	compareGolden(t, "skills_scroll.golden", got)
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
