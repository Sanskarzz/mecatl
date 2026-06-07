package ui

import (
	"fmt"
	"strings"
	"testing"

	tea "charm.land/bubbletea/v2"
	"github.com/charmbracelet/x/ansi"

	"github.com/stacklok/mecatl/cmd/mecatui/client"
	"github.com/stacklok/mecatl/cmd/mecatui/theme"
)

// selModel builds an idle, ALT-SCREEN (selectable) model whose conversation
// overflows the viewport, with a known multi-line content set into the viewport so
// screen→content mapping, highlighting, and copy are all exercisable. Unlike
// scrollModel it sets NoAltScreen=false (selection requires the alt screen) and
// threads a fakeClipboard so the shell-write fallback is assertable.
func selModel(t *testing.T) (Model, *fakeClipboard) {
	t.Helper()
	cb := &fakeClipboard{}
	m, _, _ := newTestModel(t, theme.New("aztec", theme.AztecPalette()))
	m.deps.NoAltScreen = false
	m.deps.Clipboard = cb
	m = applyAll(m,
		tea.WindowSizeMsg{Width: 100, Height: 30},
		client.SessionReadyMsg{SessionID: "sess-sel-0001"},
	)
	m.conv.addUser("a request")
	m.conv.appendAssistant(strings.Repeat("line of streamed output\n", 120))
	m.phase = phaseIdle
	m.stuck = true
	m.refreshView()
	return m, cb
}

// pressMouse / motionMouse / releaseMouse drive the three mouse messages through
// Update at a given button + cell.
func pressMouse(m Model, btn tea.MouseButton, x, y int) (Model, tea.Cmd) {
	return pressKey(m, tea.MouseClickMsg{Button: btn, X: x, Y: y})
}
func motionMouse(m Model, x, y int) (Model, tea.Cmd) {
	return pressKey(m, tea.MouseMotionMsg{Button: tea.MouseLeft, X: x, Y: y})
}
func releaseMouse(m Model, x, y int) (Model, tea.Cmd) {
	return pressKey(m, tea.MouseReleaseMsg{Button: tea.MouseLeft, X: x, Y: y})
}

// collectLeaves runs a (possibly batched) command and returns its leaf messages.
func collectLeaves(cmd tea.Cmd) []tea.Msg {
	var out []tea.Msg
	var walk func(c tea.Cmd)
	walk = func(c tea.Cmd) {
		if c == nil {
			return
		}
		msg := c()
		if batch, ok := msg.(tea.BatchMsg); ok {
			for _, sub := range batch {
				walk(sub)
			}
			return
		}
		if msg != nil {
			out = append(out, msg)
		}
	}
	walk(cmd)
	return out
}

// osc52Payload returns the OSC52 (tea.SetClipboard) payload found among the leaves,
// or "" if none. tea.SetClipboard yields an unexported setClipboardMsg whose
// underlying type is a string, so its %v rendering IS the payload.
func osc52Payload(leaves []tea.Msg) (string, bool) {
	for _, msg := range leaves {
		// shellWriteResultMsg is the other leaf; skip it.
		if _, ok := msg.(shellWriteResultMsg); ok {
			continue
		}
		return fmt.Sprint(msg), true
	}
	return "", false
}

// TestScreenToContentMapsRowToLine: a click in the conversation region maps to the
// expected logical line (YOffset + row offset); clicks above the top row and at/
// below the viewport bottom miss (Req 9).
func TestScreenToContentMapsRowToLine(t *testing.T) {
	m, _ := selModel(t)
	top := convTopRow(m)
	if top != 2 {
		t.Fatalf("convTopRow = %d, want 2", top)
	}
	// A click at the first viewport row maps to YOffset()+0.
	line, _, ok := screenToContent(m, 0, top)
	if !ok {
		t.Fatal("click at the first viewport row should map")
	}
	if line != m.vp.YOffset() {
		t.Errorf("first row mapped to line %d, want YOffset %d", line, m.vp.YOffset())
	}
	// Above the top row: a miss (header region).
	if _, _, ok := screenToContent(m, 0, top-1); ok {
		t.Error("a click above the viewport top must not map (header region)")
	}
	// At/below the viewport bottom: a miss (input/footer region).
	if _, _, ok := screenToContent(m, 0, top+m.vp.Height()); ok {
		t.Error("a click at the viewport bottom edge must not map (input region)")
	}
}

// TestScreenToContentRespectsScrollOffset: after scrolling up, the SAME screen row
// maps to a HIGHER logical line (the YOffset shifted) — Req 2's logical anchoring.
func TestScreenToContentRespectsScrollOffset(t *testing.T) {
	m, _ := selModel(t)
	top := convTopRow(m)
	beforeLine, _, ok := screenToContent(m, 0, top+1)
	if !ok {
		t.Fatal("precondition: row should map")
	}
	beforeOff := m.vp.YOffset()

	m, _ = pressKey(m, tea.KeyPressMsg{Code: tea.KeyPgUp})
	if m.vp.YOffset() >= beforeOff {
		t.Fatalf("precondition: pgup should reduce YOffset (%d → %d)", beforeOff, m.vp.YOffset())
	}

	afterLine, _, ok := screenToContent(m, 0, top+1)
	if !ok {
		t.Fatal("row should still map after scroll")
	}
	if afterLine >= beforeLine {
		t.Errorf("after scrolling up the same screen row should map to a higher (smaller) logical line: %d → %d", beforeLine, afterLine)
	}
	if afterLine != m.vp.YOffset()+1 {
		t.Errorf("mapped line %d, want YOffset+1 = %d", afterLine, m.vp.YOffset()+1)
	}
}

// TestSelectedTextStripsANSI: a styled (SGR-coloured) line yields an ANSI-FREE
// payload with trailing padding trimmed. FAILS if any ANSI escape leaks.
func TestSelectedTextStripsANSI(t *testing.T) {
	th := theme.New("aztec", theme.AztecPalette())
	styled := th.Style("toolName").Render("HELLO") + " world   " // trailing pad
	content := styled + "\nplain second line"
	sel := selection{active: true, anchorL: 0, anchorC: 0, headL: 0, headC: graphemeCount("HELLO world   ")}
	got := selectedText(content, sel)
	if strings.Contains(got, "\x1b") {
		t.Errorf("payload leaked ANSI: %q", got)
	}
	if got != "HELLO world" {
		t.Errorf("payload = %q, want %q (ansi stripped, trailing pad trimmed)", got, "HELLO world")
	}
}

// TestByteRangesMultiLine: a multi-line selection produces ascending, non-
// overlapping byte ranges that, fed to the REAL viewport.SetHighlights, do not
// panic, and the GetContent slicing of each range equals the expected stripped
// substring.
func TestByteRangesMultiLine(t *testing.T) {
	m, _ := selModel(t)
	// Replace the viewport content with a known plain set for exact assertions.
	m.vp.SetContent("hello world\nsecond line\nthird row")
	content := m.vp.GetContent()
	// Select from col 6 line0 ("world") through col 6 line1 ("second").
	sel := selection{active: true, anchorL: 0, anchorC: 6, headL: 1, headC: 6}
	ranges := byteRanges(content, sel)
	if len(ranges) != 2 {
		t.Fatalf("ranges = %v, want 2 (one per spanned line)", ranges)
	}
	// Ascending + non-overlapping.
	if ranges[0][0] > ranges[0][1] || ranges[0][1] > ranges[1][0] || ranges[1][0] > ranges[1][1] {
		t.Errorf("ranges not ascending/non-overlapping: %v", ranges)
	}
	// The byte ranges index the STRIPPED content (matches the viewport contract).
	if got := content[ranges[0][0]:ranges[0][1]]; got != "world" {
		t.Errorf("range[0] slice = %q, want %q", got, "world")
	}
	if got := content[ranges[1][0]:ranges[1][1]]; got != "second" {
		t.Errorf("range[1] slice = %q, want %q", got, "second")
	}
	// Must not panic when handed to the real viewport.
	m.vp.SetHighlights(ranges)
}

// TestPressDragReleaseCopies: a press anchors, motion extends, release copies. The
// returned command carries the OSC52 payload AND the shell-write fallback fires;
// the status reads "copied". FAILS if the copy path breaks.
func TestPressDragReleaseCopies(t *testing.T) {
	m, cb := selModel(t)
	m.vp.SetContent("hello world\nsecond line\nthird row")
	m.vp.SetYOffset(0)
	top := convTopRow(m)

	m, _ = pressMouse(m, tea.MouseLeft, 0, top) // anchor at line0 col0
	if !m.sel.active {
		t.Fatal("press should activate a selection")
	}
	m, _ = motionMouse(m, 11, top) // extend to end of "hello world"
	m, cmd := releaseMouse(m, 11, top)

	leaves := collectLeaves(cmd)
	payload, ok := osc52Payload(leaves)
	if !ok {
		t.Fatal("release should return an OSC52 SetClipboard command")
	}
	if payload != "hello world" {
		t.Errorf("OSC52 payload = %q, want %q", payload, "hello world")
	}
	if len(cb.wrote) != 1 || string(cb.wrote[0]) != "hello world" {
		t.Errorf("shell-write fallback not invoked with the payload: %v", cb.wrote)
	}
	if !strings.Contains(stripANSIstr(m.statusMsg), "copied") {
		t.Errorf("status = %q, want a 'copied N chars' confirmation", stripANSIstr(m.statusMsg))
	}
}

// TestRightClickCopiesExistingSelection: with an active selection, a RIGHT click
// copies it through the same path (Req 4).
func TestRightClickCopiesExistingSelection(t *testing.T) {
	m, cb := selModel(t)
	m.vp.SetContent("hello world\nsecond line")
	m.vp.SetYOffset(0)
	top := convTopRow(m)

	m, _ = pressMouse(m, tea.MouseLeft, 0, top)
	m, _ = motionMouse(m, 5, top) // select "hello"
	// Right click (no release yet) copies the current selection.
	m, cmd := pressMouse(m, tea.MouseRight, 40, top)

	payload, ok := osc52Payload(collectLeaves(cmd))
	if !ok || payload != "hello" {
		t.Errorf("right-click OSC52 payload = %q ok=%v, want %q", payload, ok, "hello")
	}
	if len(cb.wrote) != 1 || string(cb.wrote[0]) != "hello" {
		t.Errorf("right-click shell-write not invoked: %v", cb.wrote)
	}
}

// TestEscClearsSelectionThenRestoresSemantics: esc with an active selection clears
// it and does NOT cancel a run; a SECOND esc (no selection) falls through to today's
// esc meaning (clear input / cancel) — Req 5.
func TestEscClearsSelectionThenRestoresSemantics(t *testing.T) {
	m, _ := selModel(t)
	m.phase = phaseRunning // so "cancel run" is the would-be esc meaning
	top := convTopRow(m)
	m.vp.SetContent("hello world\nsecond line")
	m.vp.SetYOffset(0)

	m, _ = pressMouse(m, tea.MouseLeft, 0, top)
	m, _ = motionMouse(m, 5, top)
	if !m.sel.active {
		t.Fatal("precondition: a selection should be active")
	}

	// First esc: clears the selection, consumes the key, does NOT cancel the run.
	m, cmd := pressKey(m, tea.KeyPressMsg{Code: tea.KeyEscape})
	if m.sel.active {
		t.Error("first esc should clear the active selection")
	}
	if cmd != nil {
		t.Error("esc that only clears a selection should issue no cancel command")
	}
	if m.phase != phaseRunning {
		t.Error("esc clearing a selection must not cancel the run")
	}

	// Second esc (no selection, empty input, no queue): today's esc cancels the run.
	m, cmd = pressKey(m, tea.KeyPressMsg{Code: tea.KeyEscape})
	if cmd == nil {
		t.Error("second esc (no selection) should fall through to today's cancel semantics")
	}
}

// TestSelectionBlockedUnderOverlay: a left press while an overlay owns the body
// starts NO selection (Req 8).
func TestSelectionBlockedUnderOverlay(t *testing.T) {
	m, _ := selModel(t)
	m.showHelp = true // help owns the body
	top := convTopRow(m)

	m, _ = pressMouse(m, tea.MouseLeft, 0, top)
	if m.sel.active {
		t.Error("a press while an overlay owns the body must not start a selection")
	}
}

// TestNoAltScreenDisablesSelection: in --inline/--no-alt-screen mode no selection is
// ever created (Req 7).
func TestNoAltScreenDisablesSelection(t *testing.T) {
	m, _ := selModel(t)
	m.deps.NoAltScreen = true
	top := convTopRow(m)

	m, _ = pressMouse(m, tea.MouseLeft, 0, top)
	if m.sel.active {
		t.Error("NoAltScreen must disable selection")
	}
	if selectable(m) {
		t.Error("selectable should be false under NoAltScreen")
	}
}

// TestNoMouseDisablesSelection: the --no-mouse escape hatch leaves the alt screen
// up but disables in-app selection so the terminal's native selection works — a
// left-press starts nothing and selectable() is false.
func TestNoMouseDisablesSelection(t *testing.T) {
	m, _ := selModel(t)
	m.deps.NoMouse = true
	top := convTopRow(m)

	m, _ = pressMouse(m, tea.MouseLeft, 0, top)
	if m.sel.active {
		t.Error("NoMouse must disable in-app selection")
	}
	if selectable(m) {
		t.Error("selectable should be false under NoMouse")
	}
}

// TestNoMouseLeavesMouseUncaptured: View must NOT request mouse capture under
// --no-mouse (so the terminal keeps the mouse for native selection), while the
// default alt-screen path DOES capture it (MouseModeCellMotion) for wheel + in-app
// drag-select. This is the load-bearing toggle behind the escape hatch.
func TestNoMouseLeavesMouseUncaptured(t *testing.T) {
	m, _ := selModel(t)

	if got := m.View().MouseMode; got != tea.MouseModeCellMotion {
		t.Errorf("default alt screen: MouseMode = %v, want MouseModeCellMotion (mouse captured)", got)
	}

	m.deps.NoMouse = true
	if got := m.View().MouseMode; got != tea.MouseModeNone {
		t.Errorf("--no-mouse: MouseMode = %v, want MouseModeNone (uncaptured for native selection)", got)
	}

	// --inline already leaves the mouse uncaptured regardless of NoMouse.
	m.deps.NoMouse = false
	m.deps.NoAltScreen = true
	if got := m.View().MouseMode; got != tea.MouseModeNone {
		t.Errorf("--inline: MouseMode = %v, want MouseModeNone", got)
	}
}

// TestSelectionAssumesSoftWrapDisabled guards the load-bearing invariant of the
// screen→content mapping: screenToContent maps a screen row to logical line
// YOffset()+(y-convTop) and a cell X straight to a grapheme column ONLY because the
// viewport does not soft-wrap (glamour hard-wraps the content to the width instead).
// If anyone enables viewport SoftWrap, that one-line offset silently mis-maps every
// click — this test fails loudly to point them at selection.go's mapping.
func TestSelectionAssumesSoftWrapDisabled(t *testing.T) {
	m, _ := selModel(t)
	if m.vp.SoftWrap {
		t.Fatal("viewport SoftWrap is enabled — the selection screen→content mapping in " +
			"selection.go assumes it is OFF (no wrap-aware walk); re-derive screenToContent/" +
			"graphemeColForCellX for wrapped lines before enabling it")
	}
}

// TestWheelKeepsSelection: a wheel scroll while a selection exists still scrolls
// (re-derives auto-follow) and does NOT clear the selection (Req 10).
func TestWheelKeepsSelection(t *testing.T) {
	m, _ := selModel(t)
	top := convTopRow(m)
	m, _ = pressMouse(m, tea.MouseLeft, 0, top)
	m, _ = motionMouse(m, 5, top)
	if !m.sel.active {
		t.Fatal("precondition: a selection should be active")
	}
	beforeStuck := m.stuck

	m, _ = pressKey(m, tea.MouseWheelMsg{Button: tea.MouseWheelUp})

	if !m.sel.active {
		t.Error("a wheel scroll must NOT clear an active selection")
	}
	if m.stuck == beforeStuck && beforeStuck {
		t.Error("a wheel-up should have unstuck the view (auto-follow re-derived)")
	}
}

// TestSelectionSurvivesStreamingDelta: a selection's logical anchors are unchanged
// by a streamed delta + frame flush, and the viewport highlights stay applied; the
// stuck/AtBottom state is unaffected (Req 2).
func TestSelectionSurvivesStreamingDelta(t *testing.T) {
	m, _ := selModel(t)
	top := convTopRow(m)
	// Scroll up so the selection is on a stable, non-tail line and a delta won't
	// re-pin the view.
	m, _ = pressKey(m, tea.KeyPressMsg{Code: tea.KeyPgUp})
	m, _ = pressMouse(m, tea.MouseLeft, 0, top)
	m, _ = motionMouse(m, 10, top)
	anchorL, anchorC := m.sel.anchorL, m.sel.anchorC
	headL, headC := m.sel.headL, m.sel.headC
	beforeStuck := m.stuck

	m.phase = phaseRunning
	m = applyAll(m,
		client.AssistantDeltaMsg{Turn: 1, Text: strings.Repeat("appended\n", 5)},
		renderTickMsg{},
	)

	if m.sel.anchorL != anchorL || m.sel.anchorC != anchorC || m.sel.headL != headL || m.sel.headC != headC {
		t.Errorf("streaming delta moved the selection anchors: (%d,%d)-(%d,%d) → (%d,%d)-(%d,%d)",
			anchorL, anchorC, headL, headC, m.sel.anchorL, m.sel.anchorC, m.sel.headL, m.sel.headC)
	}
	if !m.sel.active {
		t.Error("selection should still be active after a streaming delta")
	}
	if m.stuck != beforeStuck {
		t.Errorf("streaming delta changed stuck: %v → %v", beforeStuck, m.stuck)
	}
	// The highlight is re-applied each refreshView while active: a fresh
	// byteRanges against the current content is non-empty (the selected span still
	// exists in the grown content).
	if len(byteRanges(m.vp.GetContent(), m.sel)) == 0 {
		t.Error("selection highlight ranges should still be derivable after a delta")
	}
	// Stronger: the VIEWPORT itself must still carry the highlight after the
	// SetContent (which clears highlights) → re-apply loop ran in refreshView. The
	// bubbles viewport doesn't expose its highlight ranges, so assert the rendered
	// view actually contains the reverse-video SGR (the "selection" theme style),
	// AND that the recomputed byte offsets still slice the expected span out of the
	// current content (closing the clear-then-reapply loop for real).
	if !strings.Contains(m.vp.View(), "\x1b[7m") {
		t.Error("after a streaming delta the rendered viewport should still carry the reverse-video selection highlight")
	}
	// byteRanges offsets index the ANSI-STRIPPED content (the viewport contract), so
	// slicing the stripped form recovers the same first-line span selectedText
	// reports — the recompute-against-current-content path is intact.
	ranges := byteRanges(m.vp.GetContent(), m.sel)
	stripped := ansi.Strip(m.vp.GetContent())
	got := strings.TrimRight(stripped[ranges[0][0]:ranges[0][1]], " ")
	wantFirstLine := strings.SplitN(selectedText(m.vp.GetContent(), m.sel), "\n", 2)[0]
	if got == "" || got != wantFirstLine {
		t.Errorf("recomputed range slices %q, want the selectedText first line %q", got, wantFirstLine)
	}
}

// TestEmptyClickNoCopy: a plain click (press+release at the same cell, no drag)
// selects nothing and copies nothing.
func TestEmptyClickNoCopy(t *testing.T) {
	m, cb := selModel(t)
	top := convTopRow(m)

	m, _ = pressMouse(m, tea.MouseLeft, 3, top)
	m, cmd := releaseMouse(m, 3, top)

	if m.sel.active {
		t.Error("an empty click should leave no active selection")
	}
	if cmd != nil {
		t.Error("an empty click should copy nothing (no command)")
	}
	if len(cb.wrote) != 0 {
		t.Errorf("an empty click should not invoke the shell write: %v", cb.wrote)
	}
}

// TestOpeningOverlayClearsSelection: opening an overlay (help) mid-selection clears
// it (Req 8 — the mid-selection clear).
func TestOpeningOverlayClearsSelection(t *testing.T) {
	m, _ := selModel(t)
	top := convTopRow(m)
	m, _ = pressMouse(m, tea.MouseLeft, 0, top)
	m, _ = motionMouse(m, 5, top)
	if !m.sel.active {
		t.Fatal("precondition: a selection should be active")
	}

	// "?" on an empty prompt opens help.
	m, _ = pressKey(m, tea.KeyPressMsg{Code: '?'})
	if !m.showHelp {
		t.Fatal("precondition: ? should open help")
	}
	if m.sel.active {
		t.Error("opening an overlay mid-selection should clear the selection")
	}
}

// hlContent is a known multi-line viewport content with enough lines to overflow
// the viewport so edge-autoscroll has room to move. Each line is "row NN" so a
// scrolled line is identifiable by its number.
func hlContent(n int) string {
	var b strings.Builder
	for i := 0; i < n; i++ {
		if i > 0 {
			b.WriteByte('\n')
		}
		fmt.Fprintf(&b, "row %02d", i)
	}
	return b.String()
}

// TestDragNearTopEdgeScrollsUp: a drag whose motion lands at/above the top edge
// scrolls the viewport UP by a line and extends the selection head to the
// newly-revealed top content line. Returns a re-arming tick.
func TestDragNearTopEdgeScrollsUp(t *testing.T) {
	m, _ := selModel(t)
	m.vp.SetContent(hlContent(200))
	m.vp.SetYOffset(50) // mid-content: room to scroll both ways
	top := convTopRow(m)

	// Anchor somewhere in the middle of the visible region.
	m, _ = pressMouse(m, tea.MouseLeft, 0, top+3)
	beforeOff := m.vp.YOffset()

	// Motion AT the top edge → scroll up.
	m, cmd := motionMouse(m, 0, top)

	if m.vp.YOffset() != beforeOff-1 {
		t.Errorf("YOffset = %d, want one line up (%d)", m.vp.YOffset(), beforeOff-1)
	}
	if m.sel.autoScroll != scrollUp {
		t.Errorf("autoScroll = %v, want scrollUp", m.sel.autoScroll)
	}
	if m.sel.headL != m.vp.YOffset() {
		t.Errorf("head line = %d, want the new top content line %d", m.sel.headL, m.vp.YOffset())
	}
	if cmd == nil {
		t.Error("an armed edge-autoscroll should return a re-arming tick command")
	}
	// An upward scroll is an explicit user scroll → unstick auto-follow.
	if m.stuck {
		t.Error("edge-autoscroll up should unstick auto-follow")
	}
}

// TestDragNearBottomEdgeScrollsDown: a drag at/below the bottom edge scrolls DOWN a
// line and extends the head to the new bottom visible content line.
func TestDragNearBottomEdgeScrollsDown(t *testing.T) {
	m, _ := selModel(t)
	m.vp.SetContent(hlContent(200))
	m.vp.SetYOffset(50)
	top := convTopRow(m)
	bottom := top + m.vp.Height() - 1

	m, _ = pressMouse(m, tea.MouseLeft, 0, top+3)
	beforeOff := m.vp.YOffset()

	m, cmd := motionMouse(m, 0, bottom)

	if m.vp.YOffset() != beforeOff+1 {
		t.Errorf("YOffset = %d, want one line down (%d)", m.vp.YOffset(), beforeOff+1)
	}
	if m.sel.autoScroll != scrollDown {
		t.Errorf("autoScroll = %v, want scrollDown", m.sel.autoScroll)
	}
	wantHead := m.vp.YOffset() + m.vp.Height() - 1
	if m.sel.headL != wantHead {
		t.Errorf("head line = %d, want the new bottom visible content line %d", m.sel.headL, wantHead)
	}
	if cmd == nil {
		t.Error("an armed edge-autoscroll should return a re-arming tick command")
	}
}

// TestAutoScrollTickContinues: an armed autoScroll tick scrolls one more line and
// re-arms (the self-re-arming continuous scroll while held at the edge).
func TestAutoScrollTickContinues(t *testing.T) {
	m, _ := selModel(t)
	m.vp.SetContent(hlContent(200))
	m.vp.SetYOffset(50)
	top := convTopRow(m)

	m, _ = pressMouse(m, tea.MouseLeft, 0, top+3)
	m, _ = motionMouse(m, 0, top) // arm scrollUp
	armedOff := m.vp.YOffset()

	mm, cmd := m.Update(autoScrollMsg{})
	m = mm.(Model)

	if m.vp.YOffset() != armedOff-1 {
		t.Errorf("tick YOffset = %d, want one more line up (%d)", m.vp.YOffset(), armedOff-1)
	}
	if m.sel.autoScroll != scrollUp {
		t.Error("tick should keep autoScroll armed while still at the edge")
	}
	if cmd == nil {
		t.Error("the tick should re-arm itself while still scrolling")
	}
}

// TestAutoScrollStopsOnRelease: a release disarms autoScroll (a pending tick
// no-ops) and finalises the selection.
func TestAutoScrollStopsOnRelease(t *testing.T) {
	m, _ := selModel(t)
	m.vp.SetContent(hlContent(200))
	m.vp.SetYOffset(50)
	top := convTopRow(m)

	m, _ = pressMouse(m, tea.MouseLeft, 0, top+3)
	m, _ = motionMouse(m, 0, top) // arm scrollUp
	if m.sel.autoScroll != scrollUp {
		t.Fatal("precondition: autoScroll should be armed")
	}

	m, _ = releaseMouse(m, 0, top)

	// After release the selection finalised (active=false on a real selection it
	// copied) OR cleared; either way a pending tick must no-op. Drive the stale tick.
	mm, cmd := m.Update(autoScrollMsg{})
	m = mm.(Model)
	if m.sel.autoScroll != scrollNone {
		t.Errorf("release should disarm autoScroll, got %v", m.sel.autoScroll)
	}
	if cmd != nil {
		t.Error("a stale autoScroll tick after release must not re-arm")
	}
}

// TestAutoScrollStopsOnMotionBackInside: moving the pointer back inside the region
// disarms autoScroll so the tick no-ops.
func TestAutoScrollStopsOnMotionBackInside(t *testing.T) {
	m, _ := selModel(t)
	m.vp.SetContent(hlContent(200))
	m.vp.SetYOffset(50)
	top := convTopRow(m)

	m, _ = pressMouse(m, tea.MouseLeft, 0, top+3)
	m, _ = motionMouse(m, 0, top) // arm scrollUp
	if m.sel.autoScroll != scrollUp {
		t.Fatal("precondition: autoScroll should be armed")
	}

	// Motion back inside the region (not at an edge) disarms.
	m, _ = motionMouse(m, 2, top+5)
	if m.sel.autoScroll != scrollNone {
		t.Errorf("motion back inside should disarm autoScroll, got %v", m.sel.autoScroll)
	}

	mm, cmd := m.Update(autoScrollMsg{})
	m = mm.(Model)
	if cmd != nil {
		t.Error("a stale tick after motion-back-inside must not re-arm")
	}
}

// TestAutoScrollStopsAtContentTop: at the actual content top, an up-edge drag does
// NOT spin a tick (YOffset can't move further) — autoScroll stays disarmed.
func TestAutoScrollStopsAtContentTop(t *testing.T) {
	m, _ := selModel(t)
	m.vp.SetContent(hlContent(200))
	m.vp.GotoTop() // YOffset == 0, can't scroll up
	top := convTopRow(m)

	m, _ = pressMouse(m, tea.MouseLeft, 0, top+3)
	m, cmd := motionMouse(m, 0, top) // up-edge, but already at content top

	if m.vp.YOffset() != 0 {
		t.Errorf("YOffset = %d, want 0 (already at content top)", m.vp.YOffset())
	}
	if m.sel.autoScroll != scrollNone {
		t.Errorf("at content top autoScroll should not arm, got %v", m.sel.autoScroll)
	}
	if cmd != nil {
		t.Error("at content top no re-arming tick should be returned")
	}
}

// TestAutoScrollStopsAtContentBottom: at the actual content bottom, a down-edge
// drag does NOT spin a tick.
func TestAutoScrollStopsAtContentBottom(t *testing.T) {
	m, _ := selModel(t)
	m.vp.SetContent(hlContent(200))
	m.vp.GotoBottom() // can't scroll down further
	top := convTopRow(m)
	bottom := top + m.vp.Height() - 1

	m, _ = pressMouse(m, tea.MouseLeft, 0, top+1)
	m, cmd := motionMouse(m, 0, bottom)

	if m.sel.autoScroll != scrollNone {
		t.Errorf("at content bottom autoScroll should not arm, got %v", m.sel.autoScroll)
	}
	if cmd != nil {
		t.Error("at content bottom no re-arming tick should be returned")
	}
}

// TestAutoScrollClearedByEsc: esc during an armed edge-autoscroll clears the
// selection AND disarms the tick.
func TestAutoScrollClearedByEsc(t *testing.T) {
	m, _ := selModel(t)
	m.vp.SetContent(hlContent(200))
	m.vp.SetYOffset(50)
	top := convTopRow(m)

	m, _ = pressMouse(m, tea.MouseLeft, 0, top+3)
	m, _ = motionMouse(m, 0, top)
	if m.sel.autoScroll != scrollUp {
		t.Fatal("precondition: autoScroll armed")
	}

	m, _ = pressKey(m, tea.KeyPressMsg{Code: tea.KeyEscape})
	if m.sel.active || m.sel.autoScroll != scrollNone {
		t.Errorf("esc should clear the selection and disarm autoScroll: active=%v dir=%v", m.sel.active, m.sel.autoScroll)
	}
	mm, cmd := m.Update(autoScrollMsg{})
	m = mm.(Model)
	if cmd != nil {
		t.Error("a stale tick after esc-clear must not re-arm")
	}
}

// TestAutoScrollArmIsSingleFlight: the FIRST edge motion arms a tick loop (returns
// a tick cmd); a SECOND edge motion in the SAME direction must NOT spawn a second
// concurrent loop (returns nil) — terminal drag reporting emits a motion per cell,
// so re-arming each one would multiply the autoscroll speed.
func TestAutoScrollArmIsSingleFlight(t *testing.T) {
	m, _ := selModel(t)
	m.vp.SetContent(hlContent(200))
	m.vp.SetYOffset(50)
	top := convTopRow(m)

	m, _ = pressMouse(m, tea.MouseLeft, 0, top+3)

	// First edge motion: arms the loop and returns a tick.
	m, cmd1 := motionMouse(m, 0, top)
	if m.sel.autoScroll != scrollUp {
		t.Fatalf("first edge motion should arm scrollUp, got %v", m.sel.autoScroll)
	}
	if cmd1 == nil {
		t.Fatal("first edge motion should return a tick command (arms the loop)")
	}

	// Second edge motion, same direction: must NOT spawn a second loop.
	m, cmd2 := motionMouse(m, 0, top)
	if m.sel.autoScroll != scrollUp {
		t.Errorf("second edge motion should keep scrollUp, got %v", m.sel.autoScroll)
	}
	if cmd2 != nil {
		t.Error("a second same-direction edge motion must NOT spawn a second tick loop (single-flight)")
	}
}

// TestAutoScrollDirectionFlipNoSecondLoop: arming scrollUp then dragging to the
// opposite (bottom) edge flips the direction to scrollDown WITHOUT spawning a
// second tick loop — the single running loop reads the new direction itself.
func TestAutoScrollDirectionFlipNoSecondLoop(t *testing.T) {
	m, _ := selModel(t)
	m.vp.SetContent(hlContent(200))
	m.vp.SetYOffset(50)
	top := convTopRow(m)
	bottom := top + m.vp.Height() - 1

	m, _ = pressMouse(m, tea.MouseLeft, 0, top+3)
	m, cmd1 := motionMouse(m, 0, top) // arm scrollUp (spawns the loop)
	if m.sel.autoScroll != scrollUp || cmd1 == nil {
		t.Fatalf("precondition: scrollUp armed with a tick (dir=%v cmd=%v)", m.sel.autoScroll, cmd1)
	}

	// Flip to the bottom edge: direction changes, but no second loop is spawned.
	m, cmd2 := motionMouse(m, 0, bottom)
	if m.sel.autoScroll != scrollDown {
		t.Errorf("opposite-edge motion should flip autoScroll to scrollDown, got %v", m.sel.autoScroll)
	}
	if cmd2 != nil {
		t.Error("a direction flip must NOT spawn a second tick loop (the existing loop handles it)")
	}
}

// nonSelectable is one row of the table-driven overlay/mode gate test: a mutator
// that drives the model into a state where selectable() is false.
type nonSelectableCase struct {
	name  string
	enter func(m *Model)
}

// TestSelectableGateBlocksAndClears is the TABLE-driven cover of the selectable()
// gate (Req 8): for each non-selectable state, (a) a left-press starts NO selection,
// and (b) an already-active selection is cleared via the Update chokepoint. The
// permission-ask case (the most likely real mid-drag interruption) is included.
func TestSelectableGateBlocksAndClears(t *testing.T) {
	cases := []nonSelectableCase{
		{"mcpOverlay", func(m *Model) { m.mcp.view = mcpPanel }},
		{"modelsOverlay", func(m *Model) { m.models.view = modelsPanel }},
		{"help", func(m *Model) { m.showHelp = true }},
		{"awaitingApproval", func(m *Model) { m.phase = phaseAwaitingApproval }},
		{"fatal", func(m *Model) { m.phase = phaseFatal }},
	}
	for _, tc := range cases {
		t.Run(tc.name+"/press-blocked", func(t *testing.T) {
			m, _ := selModel(t)
			tc.enter(&m)
			if selectable(m) {
				t.Fatalf("precondition: %s should be non-selectable", tc.name)
			}
			top := convTopRow(m)
			m, _ = pressMouse(m, tea.MouseLeft, 0, top)
			if m.sel.active {
				t.Errorf("%s: a left-press must not start a selection while non-selectable", tc.name)
			}
		})
		t.Run(tc.name+"/active-cleared", func(t *testing.T) {
			m, _ := selModel(t)
			top := convTopRow(m)
			// Start a real selection while still selectable.
			m, _ = pressMouse(m, tea.MouseLeft, 0, top)
			m, _ = motionMouse(m, 5, top)
			if !m.sel.active {
				t.Fatal("precondition: selection should be active")
			}
			// Transition to the non-selectable state via a real message through Update
			// so the chokepoint runs. We drive the state mutator, then send a benign
			// message (a renderTick) so Update's post-reduce chokepoint sees the new
			// state. (In production the SAME message that opens the overlay carries the
			// transition; here we split it to keep the table mutator simple.)
			tc.enter(&m)
			mm, _ := m.Update(renderTickMsg{})
			m = mm.(Model)
			if m.sel.active {
				t.Errorf("%s: an active selection must be cleared once the body is non-selectable", tc.name)
			}
		})
	}
}

// TestKeyboardScrollKeepsSelection: pgup/pgdn/home/end must NOT clear an active
// selection (a separate code path from the wheel), and the highlight stays
// derivable after each (Req 2, keyboard path).
func TestKeyboardScrollKeepsSelection(t *testing.T) {
	keys := []struct {
		name string
		code tea.KeyPressMsg
	}{
		{"pgup", tea.KeyPressMsg{Code: tea.KeyPgUp}},
		{"pgdn", tea.KeyPressMsg{Code: tea.KeyPgDown}},
		{"home", tea.KeyPressMsg{Code: tea.KeyHome}},
		{"end", tea.KeyPressMsg{Code: tea.KeyEnd}},
	}
	for _, k := range keys {
		t.Run(k.name, func(t *testing.T) {
			m, _ := selModel(t)
			top := convTopRow(m)
			m, _ = pressMouse(m, tea.MouseLeft, 0, top)
			m, _ = motionMouse(m, 10, top)
			if !m.sel.active {
				t.Fatal("precondition: selection active")
			}

			m, _ = pressKey(m, k.code)

			if !m.sel.active {
				t.Errorf("%s scroll must not clear the selection", k.name)
			}
			if len(byteRanges(m.vp.GetContent(), m.sel)) == 0 {
				t.Errorf("%s scroll left the selection highlight underivable", k.name)
			}
		})
	}
}

// TestCtrlVPasteWithActiveSelection is a no-regression guard: a ctrl+v paste while a
// selection is active behaves exactly as without one — the text is inserted and the
// esc-guard (Cancel-only) does not misfire on the paste path.
func TestCtrlVPasteWithActiveSelection(t *testing.T) {
	cb := &fakeClipboard{mime: "text/plain", data: []byte("pasted text")}
	m, _ := newClipboardModel(t, client.Capabilities{Image: true}, cb)
	m.deps.NoAltScreen = false
	m.conv.addUser("x")
	m.conv.appendAssistant(strings.Repeat("line of text\n", 60))
	m.refreshView()
	top := convTopRow(m)

	// Establish an active selection.
	m, _ = pressMouse(m, tea.MouseLeft, 0, top)
	m, _ = motionMouse(m, 5, top)
	if !m.sel.active {
		t.Fatal("precondition: selection active")
	}

	m = pressCtrlV(t, m)

	if !strings.Contains(m.ta.Value(), "pasted text") {
		t.Errorf("ctrl+v should insert the pasted text regardless of an active selection, got %q", m.ta.Value())
	}
}
