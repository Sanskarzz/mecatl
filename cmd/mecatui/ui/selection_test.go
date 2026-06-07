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
//
// It executes every leaf cmd SYNCHRONOUSLY, so it must only be handed batches whose
// leaves all return promptly — the release / right-click copy paths
// (tea.SetClipboard + the shell-write fallback), which is what the pre-existing copy
// tests feed it. It must NOT be handed the double/triple-click copy-on-select batch:
// that one also carries the multi-click disarm tick (tea.Tick(clickWindow=400ms)),
// and running it would block the full window. The click-copy tests assert on model
// state instead (selectedText + statusMsg), so no tea.Tick is ever executed and
// there is no timing dependence anywhere — see TestDoubleClickSelectsWord et al.
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
		// shellWriteResultMsg is the other copy-path leaf; skip it. (The multi-click
		// disarm tick never reaches here: collectLeaves is only fed the tick-free
		// release / right-click copy batches — the click-copy tests assert on model
		// state instead, so this helper never walks a batch containing a tea.Tick.)
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

// TestByteRangesEmptyInteriorLineGetsNoCell pins the SHIPPED behaviour for an
// empty interior line in a multi-line selection. We VERIFIED a one-cell
// [off, off+1] range over the '\n' byte does NOT paint in bubbles viewport
// v2.1.0 (lipgloss.StyleRanges renders the bg SGR with no glyph between the
// escapes), so byteRanges deliberately skips the empty line: the surrounding
// lines stay highlighted, the empty line is a gap. This locks that contract
// (and that the ranges stay ascending/non-overlapping + don't panic).
func TestByteRangesEmptyInteriorLineGetsNoCell(t *testing.T) {
	m, _ := selModel(t)
	m.vp.SetContent("alpha\n\nbeta")
	content := m.vp.GetContent()
	// Select from line0col0 through line2col4 — spanning the empty middle line.
	sel := selection{active: true, anchorL: 0, anchorC: 0, headL: 2, headC: 4}
	ranges := byteRanges(content, sel)
	// Two ranges: "alpha" (line0) and "beta" (line2). The empty line1 contributes
	// none (the known limitation).
	if len(ranges) != 2 {
		t.Fatalf("ranges = %v, want 2 (empty interior line contributes none)", ranges)
	}
	// Ascending + non-overlapping.
	if ranges[0][0] > ranges[0][1] || ranges[0][1] > ranges[1][0] || ranges[1][0] > ranges[1][1] {
		t.Errorf("ranges not ascending/non-overlapping: %v", ranges)
	}
	if got := content[ranges[0][0]:ranges[0][1]]; got != "alpha" {
		t.Errorf("range[0] slice = %q, want %q", got, "alpha")
	}
	if got := content[ranges[1][0]:ranges[1][1]]; got != "beta" {
		t.Errorf("range[1] slice = %q, want %q", got, "beta")
	}
	// Must not panic when handed to the real viewport.
	m.vp.SetHighlights(ranges)
}

// TestByteRangesGlyphWidthNotPadded asserts an interior line's highlight stops at
// its last glyph, NOT the terminal width — the highlight is ragged (glyph-bounded),
// never a full-width bar (Req 6). The interior line's range slices to exactly the
// stripped line content.
func TestByteRangesGlyphWidthNotPadded(t *testing.T) {
	m, _ := selModel(t)
	m.vp.SetContent("first line\nshort\nlast line here")
	content := m.vp.GetContent()
	// Select all three lines; the middle line "short" is the interior line.
	sel := selection{active: true, anchorL: 0, anchorC: 0, headL: 2, headC: 14}
	ranges := byteRanges(content, sel)
	if len(ranges) != 3 {
		t.Fatalf("ranges = %v, want 3", ranges)
	}
	// The interior line's range must equal exactly the stripped "short" (5 cells),
	// not padded to the 100-cell terminal width.
	mid := content[ranges[1][0]:ranges[1][1]]
	if mid != "short" {
		t.Errorf("interior line slice = %q, want %q (glyph-bounded, not full-width)", mid, "short")
	}
	if w := ranges[1][1] - ranges[1][0]; w != len("short") {
		t.Errorf("interior line width = %d bytes, want %d (== grapheme count, no pad)", w, len("short"))
	}
}

// TestSelectedTextUnchangedByEmptyLineFix asserts the COPY path still preserves
// the empty middle line (the byteRanges/highlight change is additive and must not
// touch selectedText). The empty line survives as a "\n" in the joined payload.
func TestSelectedTextUnchangedByEmptyLineFix(t *testing.T) {
	m, _ := selModel(t)
	m.vp.SetContent("alpha\n\nbeta")
	sel := selection{active: true, anchorL: 0, anchorC: 0, headL: 2, headC: 4}
	got := selectedText(m.vp.GetContent(), sel)
	if got != "alpha\n\nbeta" {
		t.Errorf("selectedText = %q, want %q (empty middle line preserved)", got, "alpha\n\nbeta")
	}
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

// TestRightClickDoesNotAdvanceClickCount: a right-click is outside the multi-click
// sequence (Req 10) — it must NOT advance clickCount/clickGen, yet it still copies
// the existing selection.
func TestRightClickDoesNotAdvanceClickCount(t *testing.T) {
	m, _, y := convModel(t, "hello world here")

	// Build a REAL (non-empty) selection via press+drag so the right-click has
	// something to copy. The drag invalidates the multi-click sequence (clickCount→0)
	// while leaving an active span.
	m, _ = pressMouse(m, tea.MouseLeft, 0, y)
	m, _ = motionMouse(m, 5, y) // select "hello"
	if !m.sel.active || m.sel.empty() {
		t.Fatal("precondition: press+drag should leave a non-empty selection")
	}
	// Re-arm the count to a known value WITHOUT disturbing the span (a left press
	// would collapse it to a zero-width anchor). The right-click must leave both the
	// count and the generation exactly as it found them.
	m.clickCount = 1
	m.clickGen = 7
	beforeCount, beforeGen := m.clickCount, m.clickGen

	m, cmd := pressMouse(m, tea.MouseRight, 40, y)

	if m.clickCount != beforeCount {
		t.Errorf("right-click advanced clickCount %d → %d, want it unchanged", beforeCount, m.clickCount)
	}
	if m.clickGen != beforeGen {
		t.Errorf("right-click bumped clickGen %d → %d, want it unchanged", beforeGen, m.clickGen)
	}
	if payload, ok := osc52Payload(collectLeaves(cmd)); !ok || payload != "hello" {
		t.Errorf("right-click should copy the existing selection, got payload %q ok=%v", payload, ok)
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

// lineIndexContaining returns the index of the first content line containing sub,
// or -1. Lines are the viewport content split on "\n" (the same basis the
// selection's absolute line indices use).
func lineIndexContaining(content, sub string) int {
	for i, ln := range strings.Split(content, "\n") {
		if strings.Contains(ansi.Strip(ln), sub) {
			return i
		}
	}
	return -1
}

// TestSelectionClearedOnReflowAboveIt: selection-identity robustness. The anchor/
// head are ABSOLUTE line indices, so a re-render that changes the line count above
// (or within) the selection — here ctrl+t expanding a tool body — re-points them at
// different text. The selection must be DROPPED, not left highlighting/copying the
// wrong runes. (A pure append BELOW does not trigger this — see
// TestSelectionSurvivesStreamingDelta.)
func TestSelectionClearedOnReflowAboveIt(t *testing.T) {
	cb := &fakeClipboard{}
	m, _, _ := newTestModel(t, theme.New("aztec", theme.AztecPalette()))
	m.deps.NoAltScreen = false
	m.deps.Clipboard = cb
	m = applyAll(m,
		tea.WindowSizeMsg{Width: 100, Height: 40},
		client.SessionReadyMsg{SessionID: "sess-reflow-1"},
	)
	// A tool block whose body is long enough that ctrl+t (full vs line-capped) changes
	// its rendered height, followed by a UNIQUE assistant marker line BELOW it.
	m.conv.addUser("req")
	m.conv.addTool("t1", "Bash", `{"cmd":"seq 40"}`)
	m.conv.resolveTool("t1", strings.TrimRight(strings.Repeat("toolbodyline\n", 40), "\n"), false)
	const marker = "UNIQUEMARKERZZZ"
	m.conv.appendAssistant(marker + " trailing words here")
	m.phase = phaseIdle
	m.stuck = true
	m.refreshView()

	// Select within the marker line (in the capped render).
	markerLine := lineIndexContaining(m.vp.GetContent(), marker)
	if markerLine < 0 {
		t.Fatal("marker not found in capped content")
	}
	top := convTopRow(m)
	y := top + (markerLine - m.vp.YOffset())
	if y < top || y >= top+m.vp.Height() {
		t.Fatalf("marker line %d not on screen (YOffset=%d top=%d h=%d)", markerLine, m.vp.YOffset(), top, m.vp.Height())
	}
	m, _ = pressMouse(m, tea.MouseLeft, 0, y)
	m, _ = motionMouse(m, 40, y) // wide enough to cover the whole marker line
	if !m.sel.active {
		t.Fatal("expected an active selection on the marker line")
	}
	if !strings.Contains(m.sel.snapshot, marker) {
		t.Fatalf("selection snapshot %q should cover the marker", m.sel.snapshot)
	}

	// Toggle ctrl+t → the tool body expands, shifting the marker DOWN, so line index
	// markerLine now holds a tool-body line instead of the marker.
	m, _ = pressKey(m, tea.KeyPressMsg{Code: 't', Mod: tea.ModCtrl})
	afterIdx := lineIndexContaining(m.vp.GetContent(), marker)
	if afterIdx == markerLine {
		t.Fatalf("test setup did not shift the layout (marker stayed at line %d); ctrl+t must change the tool body height", markerLine)
	}
	if m.sel.active {
		t.Errorf("selection should be CLEARED after a reflow shifted the content under it (marker %d → %d)", markerLine, afterIdx)
	}
	if len(byteRanges(m.vp.GetContent(), m.sel)) != 0 {
		t.Error("no selection highlight ranges should remain after the reflow-clear")
	}
	if strings.Contains(m.vp.View(), selectionBgSGR(t, m)) {
		t.Error("no selection background highlight should remain after the reflow-clear")
	}
}

// selectionBgSGR returns the truecolor background SGR substring the resolved
// "selection" theme style emits (e.g. "48;2;42;77;69" for aztec's #2A4D45), so a
// test can assert the highlight block's presence/absence without hard-coding the
// hex. It probes the style by rendering a single glyph and extracting the
// background portion of the SGR — the part that survives even when no glyph is
// present.
func selectionBgSGR(t *testing.T, m Model) string {
	t.Helper()
	probe := m.deps.Theme.Style("selection").Render("X")
	idx := strings.Index(probe, "48;")
	if idx < 0 {
		t.Fatalf("selection style has no background SGR: %q", probe)
	}
	// Slice from "48;" up to (and excluding) the SGR terminator 'm'.
	end := strings.IndexByte(probe[idx:], 'm')
	if end < 0 {
		t.Fatalf("malformed selection SGR: %q", probe)
	}
	return probe[idx : idx+end]
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

// TestSelectionHighlightWrapsSelectedText is the STRONG visibility guard: it
// asserts the selection background SGR immediately WRAPS the selected glyphs in the
// rendered viewport — not an empty open+reset pair around unstyled text. A prior
// bug left SelectedHighlightStyle unset, so the viewport's focused-range pass
// re-rendered the span with an empty style and wiped the colour; the rendered
// output was "<open><reset>text<open><reset>" (SGR present but the text NOT inside
// the block). A presence-only check ("does the SGR byte appear?") passed anyway —
// this test fails on that bug because it requires the selected text to sit between
// the open SGR and the reset.
func TestSelectionHighlightWrapsSelectedText(t *testing.T) {
	m, _ := selModel(t)
	// Controlled single-line content: the selected span has no newline, so a correct
	// highlight must wrap it contiguously (a multi-line span would be split across
	// per-line ranges and couldn't be matched as one substring).
	m.vp.SetContent("alpha bravo charlie delta echo")
	m.vp.SetYOffset(0)
	// Select "bravo" — cols [6,11), no trailing space, so the wrapped span equals the
	// (trailing-trimmed) selectedText.
	m.sel = selection{active: true, anchorL: 0, anchorC: 6, headL: 0, headC: 11}
	m.sel.snapshot = selectedText(m.vp.GetContent(), m.sel)
	applySelectionHighlight(&m)

	want := selectedText(m.vp.GetContent(), m.sel)
	if want != "bravo" {
		t.Fatalf("setup: selected text = %q, want %q", want, "bravo")
	}
	// Full open SGR for the selection style (fg+bg), up to and including the 'm'.
	probe := m.deps.Theme.Style("selection").Render("X")
	mIdx := strings.IndexByte(probe, 'm')
	if mIdx < 0 {
		t.Fatalf("selection style has no SGR: %q", probe)
	}
	openSGR := probe[:mIdx+1]

	if !strings.Contains(m.vp.View(), openSGR+want) {
		t.Errorf("selection highlight does not wrap the selected text:\n want open SGR %q immediately followed by %q in the rendered view", openSGR, want)
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
	// view actually carries the selection BACKGROUND SGR (the "selection" theme
	// style is now a solid block, NOT reverse video), AND that the recomputed byte
	// offsets still slice the expected span out of the current content (closing the
	// clear-then-reapply loop for real).
	selSGR := selectionBgSGR(t, m)
	if !strings.Contains(m.vp.View(), selSGR) {
		t.Errorf("after a streaming delta the rendered viewport should still carry the selection background SGR %q", selSGR)
	}
	if strings.Contains(m.vp.View(), "\x1b[7m") {
		t.Error("the selection highlight must be a solid block, not reverse video")
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

// driveAutoScroll feeds one autoScrollMsg tick through Update and returns the new
// model plus the re-arm command (nil once the loop disarms). Pure-reducer, no clock.
func driveAutoScroll(t *testing.T, m Model) (Model, tea.Cmd) {
	t.Helper()
	mm, cmd := m.Update(autoScrollMsg{})
	return mm.(Model), cmd
}

// TestRampToLines pins the pure acceleration curve directly: gentle first contact
// (ramp 0 → 1 line), a Fibonacci-ish ramp, then saturation at the cap. This is the
// deterministic kernel the per-tick deltas are derived from.
func TestRampToLines(t *testing.T) {
	// onAutoScroll increments the ramp THEN scrolls, so ramp=1 is the first tick.
	want := []int{1, 1, 2, 3, 5, 8, 10, 10, 10, 10}
	for r, w := range want {
		if got := rampToLines(r); got != w {
			t.Errorf("rampToLines(%d) = %d, want %d", r, got, w)
		}
	}
	if got := rampToLines(100); got != maxAutoScrollLines {
		t.Errorf("rampToLines(100) = %d, want cap %d", got, maxAutoScrollLines)
	}
}

// TestAutoScrollFirstTickIsOneLine: the FIRST held tick after arming scrolls exactly
// one line (gentle first contact). rampToLines(1)==1.
func TestAutoScrollFirstTickIsOneLine(t *testing.T) {
	m, _ := selModel(t)
	m.vp.SetContent(hlContent(200))
	m.vp.SetYOffset(50)
	top := convTopRow(m)

	m, _ = pressMouse(m, tea.MouseLeft, 0, top+3)
	m, _ = motionMouse(m, 0, top) // arm scrollUp (already scrolled 1 via arm)
	armedOff := m.vp.YOffset()

	m, cmd := driveAutoScroll(t, m)
	if m.vp.YOffset() != armedOff-1 {
		t.Errorf("first tick YOffset = %d, want one line up (%d)", m.vp.YOffset(), armedOff-1)
	}
	if cmd == nil {
		t.Error("first tick should re-arm while still scrolling")
	}
}

// TestAutoScrollAccelerates: a sustained hold ramps the lines-per-tick along the
// curve. Observed deltas are 1,2,3,5,8,10 (ramp 1..6). Fails if the curve is wrong
// or absent (a flat 1-line-per-tick would diverge at tick 2).
func TestAutoScrollAccelerates(t *testing.T) {
	m, _ := selModel(t)
	m.vp.SetContent(hlContent(2000)) // tall enough not to hit the top before saturating
	m.vp.SetYOffset(1000)
	top := convTopRow(m)

	m, _ = pressMouse(m, tea.MouseLeft, 0, top+3)
	m, _ = motionMouse(m, 0, top) // arm scrollUp
	armedOff := m.vp.YOffset()

	// Cumulative offsets for deltas 1,2,3,5,8,10 scrolling UP.
	deltas := []int{1, 2, 3, 5, 8, 10}
	want := armedOff
	for i, d := range deltas {
		var cmd tea.Cmd
		m, cmd = driveAutoScroll(t, m)
		want -= d
		if m.vp.YOffset() != want {
			t.Fatalf("tick %d: YOffset = %d, want %d (cumulative delta %d)", i+1, m.vp.YOffset(), want, d)
		}
		if cmd == nil {
			t.Fatalf("tick %d should re-arm while still scrolling", i+1)
		}
	}
}

// TestAutoScrollSpeedCaps: past saturation every tick moves exactly
// maxAutoScrollLines — never more — so one tick can't leap a full screen.
func TestAutoScrollSpeedCaps(t *testing.T) {
	m, _ := selModel(t)
	m.vp.SetContent(hlContent(5000))
	m.vp.SetYOffset(2500)
	top := convTopRow(m)

	m, _ = pressMouse(m, tea.MouseLeft, 0, top+3)
	m, _ = motionMouse(m, 0, top) // arm scrollDown? no — top edge → scrollUp

	for i := 0; i < 12; i++ {
		before := m.vp.YOffset()
		var cmd tea.Cmd
		m, cmd = driveAutoScroll(t, m)
		moved := before - m.vp.YOffset() // scrolling up: YOffset decreases
		if moved > maxAutoScrollLines {
			t.Fatalf("tick %d moved %d lines, exceeds cap %d", i+1, moved, maxAutoScrollLines)
		}
		if cmd == nil {
			t.Fatalf("tick %d should still be scrolling against tall content", i+1)
		}
	}
	// After ~6 ticks the curve has saturated; the final step must be exactly the cap.
	before := m.vp.YOffset()
	m, _ = driveAutoScroll(t, m)
	if got := before - m.vp.YOffset(); got != maxAutoScrollLines {
		t.Errorf("saturated tick moved %d lines, want exactly cap %d", got, maxAutoScrollLines)
	}
}

// TestAutoScrollNeverOvershootsContentTop: with fewer than maxAutoScrollLines lines
// of room above, a ramped up-tick lands exactly on 0 and the next tick disarms — it
// never scrolls past the content top.
func TestAutoScrollNeverOvershootsContentTop(t *testing.T) {
	m, _ := selModel(t)
	m.vp.SetContent(hlContent(200))
	m.vp.SetYOffset(4) // < maxAutoScrollLines lines above the top
	top := convTopRow(m)

	m, _ = pressMouse(m, tea.MouseLeft, 0, top+3)
	m, _ = motionMouse(m, 0, top) // arm scrollUp (scrolls 1 → YOffset 3)

	// Accelerate until the loop disarms; assert YOffset never goes below 0.
	cmd := m.autoScrollCmd()
	for i := 0; cmd != nil && i < 20; i++ {
		m, cmd = driveAutoScroll(t, m)
		if m.vp.YOffset() < 0 {
			t.Fatalf("tick %d overshot content top: YOffset = %d", i+1, m.vp.YOffset())
		}
	}
	if m.vp.YOffset() != 0 {
		t.Errorf("after disarm YOffset = %d, want exactly 0 (content top)", m.vp.YOffset())
	}
	if cmd != nil {
		t.Error("the loop must disarm once it reaches the content top")
	}
	if m.sel.autoScroll != scrollNone {
		t.Errorf("autoScroll = %v, want scrollNone after content top", m.sel.autoScroll)
	}
	if m.sel.autoScrollRamp != 0 {
		t.Errorf("autoScrollRamp = %d, want reset to 0 at content top", m.sel.autoScrollRamp)
	}
}

// TestAutoScrollNeverOvershootsContentBottom: the down-edge mirror — lands exactly on
// the max offset and disarms, never past the content bottom.
func TestAutoScrollNeverOvershootsContentBottom(t *testing.T) {
	m, _ := selModel(t)
	m.vp.SetContent(hlContent(200))
	// Discover the max YOffset, then back off a few lines so a ramped tick would
	// overshoot if it weren't clamped.
	m.vp.GotoBottom()
	maxOff := m.vp.YOffset()
	m.vp.SetYOffset(maxOff - 4)
	top := convTopRow(m)
	bottom := top + m.vp.Height() - 1

	m, _ = pressMouse(m, tea.MouseLeft, 0, top+1)
	m, _ = motionMouse(m, 0, bottom) // arm scrollDown (scrolls 1)

	cmd := m.autoScrollCmd()
	for i := 0; cmd != nil && i < 20; i++ {
		m, cmd = driveAutoScroll(t, m)
		if m.vp.YOffset() > maxOff {
			t.Fatalf("tick %d overshot content bottom: YOffset = %d (max %d)", i+1, m.vp.YOffset(), maxOff)
		}
	}
	if m.vp.YOffset() != maxOff {
		t.Errorf("after disarm YOffset = %d, want exactly the max offset %d", m.vp.YOffset(), maxOff)
	}
	if cmd != nil {
		t.Error("the loop must disarm once it reaches the content bottom")
	}
	if m.sel.autoScroll != scrollNone {
		t.Errorf("autoScroll = %v, want scrollNone after content bottom", m.sel.autoScroll)
	}
}

// TestAutoScrollRampResetsOnRelease: accelerate, release, re-press + re-arm; the next
// tick scrolls one line again (a fresh hold restarts gentle).
func TestAutoScrollRampResetsOnRelease(t *testing.T) {
	m, _ := selModel(t)
	m.vp.SetContent(hlContent(2000))
	m.vp.SetYOffset(1000)
	top := convTopRow(m)

	m, _ = pressMouse(m, tea.MouseLeft, 0, top+3)
	m, _ = motionMouse(m, 0, top) // arm scrollUp
	// Accelerate several ticks.
	for i := 0; i < 4; i++ {
		m, _ = driveAutoScroll(t, m)
	}
	if m.sel.autoScrollRamp == 0 {
		t.Fatal("precondition: ramp should be advanced after accelerating")
	}

	// Release ends the drag; re-press starts a fresh selection (ramp zeroed).
	m, _ = releaseMouse(m, 0, top)
	m, _ = pressMouse(m, tea.MouseLeft, 0, top+3)
	m, _ = motionMouse(m, 0, top) // re-arm scrollUp
	armedOff := m.vp.YOffset()

	m, _ = driveAutoScroll(t, m)
	if got := armedOff - m.vp.YOffset(); got != 1 {
		t.Errorf("first tick after re-arm moved %d lines, want 1 (ramp reset on release)", got)
	}
}

// TestAutoScrollRampResetsOnMotionBackInside: accelerate, move the pointer back
// inside the region, then re-arm at the edge; the next tick scrolls one line again.
func TestAutoScrollRampResetsOnMotionBackInside(t *testing.T) {
	m, _ := selModel(t)
	m.vp.SetContent(hlContent(2000))
	m.vp.SetYOffset(1000)
	top := convTopRow(m)

	m, _ = pressMouse(m, tea.MouseLeft, 0, top+3)
	m, _ = motionMouse(m, 0, top) // arm scrollUp
	for i := 0; i < 4; i++ {
		m, _ = driveAutoScroll(t, m)
	}

	// Motion back inside disarms AND resets the ramp.
	m, _ = motionMouse(m, 2, top+5)
	if m.sel.autoScroll != scrollNone {
		t.Fatal("precondition: motion back inside should disarm")
	}
	if m.sel.autoScrollRamp != 0 {
		t.Fatalf("motion back inside should reset the ramp, got %d", m.sel.autoScrollRamp)
	}

	// Re-arm at the edge; the first held tick is gentle again.
	m, _ = motionMouse(m, 0, top)
	armedOff := m.vp.YOffset()
	m, _ = driveAutoScroll(t, m)
	if got := armedOff - m.vp.YOffset(); got != 1 {
		t.Errorf("first tick after re-hold moved %d lines, want 1 (ramp reset inside)", got)
	}
}

// TestAutoScrollRampResetsOnEsc: accelerate, esc-clear, re-select + re-arm; the next
// tick scrolls one line again.
func TestAutoScrollRampResetsOnEsc(t *testing.T) {
	m, _ := selModel(t)
	m.vp.SetContent(hlContent(2000))
	m.vp.SetYOffset(1000)
	top := convTopRow(m)

	m, _ = pressMouse(m, tea.MouseLeft, 0, top+3)
	m, _ = motionMouse(m, 0, top) // arm scrollUp
	for i := 0; i < 4; i++ {
		m, _ = driveAutoScroll(t, m)
	}

	m, _ = pressKey(m, tea.KeyPressMsg{Code: tea.KeyEscape})
	if m.sel.active || m.sel.autoScrollRamp != 0 {
		t.Fatalf("esc should clear selection and reset ramp: active=%v ramp=%d", m.sel.active, m.sel.autoScrollRamp)
	}

	// Re-select and re-arm; first held tick is gentle.
	m, _ = pressMouse(m, tea.MouseLeft, 0, top+3)
	m, _ = motionMouse(m, 0, top)
	armedOff := m.vp.YOffset()
	m, _ = driveAutoScroll(t, m)
	if got := armedOff - m.vp.YOffset(); got != 1 {
		t.Errorf("first tick after esc+re-arm moved %d lines, want 1 (ramp reset on esc)", got)
	}
}

// TestAutoScrollHeadTracksEdgeAfterMultiLineStep: after a multi-line accelerated
// step the selection head still tracks the scrolled edge at the drag column
// (extendHeadToEdge is step-size-agnostic).
func TestAutoScrollHeadTracksEdgeAfterMultiLineStep(t *testing.T) {
	m, _ := selModel(t)
	m.vp.SetContent(hlContent(2000))
	m.vp.SetYOffset(1000)
	top := convTopRow(m)

	m, _ = pressMouse(m, tea.MouseLeft, 0, top+3)
	m, _ = motionMouse(m, 0, top) // arm scrollUp
	// Accelerate to a multi-line step (tick 3 moves 3 lines, well past 1).
	for i := 0; i < 3; i++ {
		m, _ = driveAutoScroll(t, m)
	}
	if m.sel.headL != m.vp.YOffset() {
		t.Errorf("scrollUp head line = %d, want the top content line %d after a multi-line step", m.sel.headL, m.vp.YOffset())
	}

	// Now the down-edge mirror: head tracks the bottom visible line.
	m2, _ := selModel(t)
	m2.vp.SetContent(hlContent(2000))
	m2.vp.SetYOffset(1000)
	top2 := convTopRow(m2)
	bottom2 := top2 + m2.vp.Height() - 1
	m2, _ = pressMouse(m2, tea.MouseLeft, 0, top2+1)
	m2, _ = motionMouse(m2, 0, bottom2) // arm scrollDown
	for i := 0; i < 3; i++ {
		m2, _ = driveAutoScroll(t, m2)
	}
	wantHead := m2.vp.YOffset() + m2.vp.Height() - 1
	if m2.sel.headL != wantHead {
		t.Errorf("scrollDown head line = %d, want the bottom visible line %d after a multi-line step", m2.sel.headL, wantHead)
	}
}

// TestArmAutoScrollDoesNotAdvanceRamp is the load-bearing invariant: armAutoScroll
// must NOT advance the ramp. The ramp lives in onAutoScroll (the held tick), NOT in
// the per-cell motion handler — terminal drag reporting fires a motion event per
// cell at the edge, so if arming advanced the ramp every mouse jiggle would silently
// accelerate. Fire several edge motions WITHOUT any ticks between them, assert the
// ramp stays 0, then drive ONE tick and assert it moved exactly 1 line (ramp was
// still 0 → onAutoScroll advances to 1 → rampToLines(1)==1).
func TestArmAutoScrollDoesNotAdvanceRamp(t *testing.T) {
	m, _ := selModel(t)
	m.vp.SetContent(hlContent(2000))
	m.vp.SetYOffset(1000)
	top := convTopRow(m)

	m, _ = pressMouse(m, tea.MouseLeft, 0, top+3)
	// Re-fire the SAME-edge motion several times with NO autoScrollMsg ticks between
	// (the runaway-acceleration scenario): each re-arm scrolls 1 line but must not
	// touch the ramp.
	for i := 0; i < 5; i++ {
		m, _ = motionMouse(m, 0, top)
		if m.sel.autoScroll != scrollUp {
			t.Fatalf("motion %d should keep scrollUp armed, got %v", i+1, m.sel.autoScroll)
		}
		if m.sel.autoScrollRamp != 0 {
			t.Fatalf("motion %d advanced the ramp to %d — armAutoScroll must NOT touch it", i+1, m.sel.autoScrollRamp)
		}
	}

	before := m.vp.YOffset()
	m, _ = driveAutoScroll(t, m)
	if got := before - m.vp.YOffset(); got != 1 {
		t.Errorf("first held tick moved %d lines, want 1 (ramp was still 0)", got)
	}
}

// TestAutoScrollRampPersistsAcrossDirectionFlip pins the DELIBERATE persist-across-
// flip behavior: a direction flip mid-hold (top edge → bottom edge) is a CONTINUATION
// of an active edge-scroll, not a fresh hold, so the ramp is intentionally NOT reset
// — armAutoScroll never touches it and onAutoScroll keeps incrementing across the
// flip. The head re-anchors to the new edge each tick, so there is no desync. (The
// reset seams are release / motion-back-inside / esc / inactive / content-edge — a
// flip is none of those.)
func TestAutoScrollRampPersistsAcrossDirectionFlip(t *testing.T) {
	m, _ := selModel(t)
	m.vp.SetContent(hlContent(2000))
	m.vp.SetYOffset(1000)
	top := convTopRow(m)
	bottom := top + m.vp.Height() - 1

	m, _ = pressMouse(m, tea.MouseLeft, 0, top+3)
	m, _ = motionMouse(m, 0, top) // arm scrollUp
	// Accelerate a few ticks at the top edge: ramp ends at 3 after three ticks.
	for i := 0; i < 3; i++ {
		m, _ = driveAutoScroll(t, m)
	}
	if m.sel.autoScrollRamp != 3 {
		t.Fatalf("precondition: ramp = %d, want 3 after three ticks", m.sel.autoScrollRamp)
	}

	// Flip to the bottom edge: continuation, NOT a reset. The ramp must persist.
	m, _ = motionMouse(m, 0, bottom)
	if m.sel.autoScroll != scrollDown {
		t.Fatalf("flip should arm scrollDown, got %v", m.sel.autoScroll)
	}
	if m.sel.autoScrollRamp != 3 {
		t.Errorf("flip reset the ramp to %d — a mid-hold direction flip must NOT reset (it's a continuation)", m.sel.autoScrollRamp)
	}

	// The next tick continues the ramp (ramp 3→4 → rampToLines(4)==5), proving it did
	// NOT restart at 1.
	before := m.vp.YOffset()
	m, _ = driveAutoScroll(t, m)
	if got := m.vp.YOffset() - before; got != rampToLines(4) {
		t.Errorf("post-flip tick moved %d lines down, want the continued-ramp value %d (not a reset-to-1)", got, rampToLines(4))
	}
	if rampToLines(4) == 1 {
		t.Fatal("test premise broken: rampToLines(4) should be > 1")
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

// convModel builds a selectable model whose conversation renders the given plain
// assistant line VERBATIM into the viewport (a plain run of words round-trips
// through glamour unchanged), so a press maps to real, refreshView-stable content —
// unlike a bare vp.SetContent, which refreshView (called by the copy path) would
// clobber with the conversation render and then drop the now-mismatched selection.
// It returns the model, the logical line index of the assistant line, and the screen
// y to click it at; for an ASCII line the screen x equals the grapheme column.
func convModel(t *testing.T, line string) (Model, int, int) {
	t.Helper()
	cb := &fakeClipboard{}
	m, _, _ := newTestModel(t, theme.New("aztec", theme.AztecPalette()))
	m.deps.NoAltScreen = false
	m.deps.Clipboard = cb
	m = applyAll(m,
		tea.WindowSizeMsg{Width: 100, Height: 40},
		client.SessionReadyMsg{SessionID: "sess-conv-0001"},
	)
	m.conv.addUser("req")
	m.conv.appendAssistant(line)
	m.phase = phaseIdle
	m.stuck = true
	m.refreshView()
	m.deps.Clipboard = cb // ensure threaded after refresh
	idx := lineIndexContaining(m.vp.GetContent(), strings.Fields(line)[0])
	if idx < 0 {
		t.Fatalf("assistant line %q not found in rendered content", line)
	}
	top := convTopRow(m)
	y := top + (idx - m.vp.YOffset())
	if y < top || y >= top+m.vp.Height() {
		t.Fatalf("assistant line %d not on screen (YOffset=%d top=%d h=%d)", idx, m.vp.YOffset(), top, m.vp.Height())
	}
	return m, idx, y
}

// setColContent sets raw viewport content for the cases that do NOT trigger a
// copy-driven refreshView (empty/no-copy selections): a successful copy calls
// refreshView, which rebuilds the viewport from the CONVERSATION and would clobber
// this raw content, so use convModel for any test that copies. Pins YOffset to 0 so
// logical line index equals screen offset from convTopRow.
func setColContent(t *testing.T, m Model, content string) Model {
	t.Helper()
	m.vp.SetContent(content)
	m.vp.SetYOffset(0)
	return m
}

// TestWordAtWordChars exercises the pure word classifier on word/space/punct runs,
// boundaries, and multibyte/wide clusters. FAILS if any run is mis-bounded.
func TestWordAtWordChars(t *testing.T) {
	cases := []struct {
		name             string
		line             string
		col              int
		wantStart, wantE int
	}{
		{"mid word bar", "foo bar baz", 5, 4, 7},     // "bar" at cols 4..6
		{"on space", "foo bar baz", 3, 3, 4},         // the single space run
		{"snake token", "snake_case_id", 6, 0, 13},   // whole identifier
		{"a-b on a", "a-b", 0, 0, 1},                 // "a"
		{"a-b on dash", "a-b", 1, 1, 2},              // "-" punct run
		{"col 0", "foo bar", 0, 0, 3},                // "foo"
		{"col n-1", "foo bar", 6, 4, 7},              // last char of "bar"
		{"multibyte héllo", "héllo wörld", 0, 0, 5},  // "héllo" (5 graphemes)
		{"multibyte wörld", "héllo wörld", 6, 6, 11}, // "wörld"
		{"wide CJK run", "日本 語", 0, 0, 2},            // "日本" (space at col 2)
		{"wide CJK after space", "日本 語", 3, 3, 4},    // "語"
		{"multi-char punct run", "a::b", 1, 1, 3},    // "::" bounded as one punct run [1,3)
		// A DECOMPOSED combining cluster: 'e'+U+0301 (combining acute) is ONE grapheme
		// cluster at col 0. Written with explicit \u escapes so an editor NFC pass
		// cannot silently precompose it: the first word is 4 grapheme columns, so
		// wordAt(col 0) spans [0,4) — proving grapheme-cluster (not rune) indexing.
		// A regression to rune-indexing would see 5 runes in the first word -> [0,5).
		{"decomposed combining", "e\u0301llo wo\u0308rld", 0, 0, 4},
		{"past EOL", "abc", 3, 3, 3}, // col == n → empty
		{"empty line", "", 0, 0, 0},  // empty → (0,0)
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			s, e := wordAt(tc.line, tc.col)
			if s != tc.wantStart || e != tc.wantE {
				t.Errorf("wordAt(%q, %d) = (%d, %d), want (%d, %d)", tc.line, tc.col, s, e, tc.wantStart, tc.wantE)
			}
		})
	}
}

// TestDoubleClickSelectsWord: two left presses at the same spot over a word select
// the WHOLE word and copy it immediately. Asserted DETERMINISTICALLY on model state
// — selectedText is the exact string clickCopy hands to tea.SetClipboard (payload :=
// selectedText(...)), and statusMsg is set synchronously by clickCopy on a real copy
// — so no cmd is executed and the 400ms disarm tick never runs (zero timing
// dependence; this is why the test is ~0.00s).
func TestDoubleClickSelectsWord(t *testing.T) {
	m, _, y := convModel(t, "hello world after")
	// "world" starts at grapheme col 6; click mid-word at col 7 (== screen x for ASCII).
	m, _ = pressMouse(m, tea.MouseLeft, 7, y)
	m, _ = pressMouse(m, tea.MouseLeft, 7, y)

	if !m.sel.active {
		t.Fatal("double-click should leave an active selection")
	}
	if got := selectedText(m.vp.GetContent(), m.sel); got != "world" {
		t.Errorf("double-click selectedText = %q, want %q (the exact OSC52 payload)", got, "world")
	}
	if !strings.Contains(stripANSIstr(m.statusMsg), "copied") {
		t.Errorf("double-click status = %q, want a 'copied N chars' confirmation (copy fired)", stripANSIstr(m.statusMsg))
	}
}

// TestTripleClickSelectsLine: a third press at the same spot selects the WHOLE
// logical line (anchorC==0, headC==graphemeCount) and copies it. Asserted on model
// state (selectedText + statusMsg), so no cmd / disarm tick runs — see
// TestDoubleClickSelectsWord for why.
func TestTripleClickSelectsLine(t *testing.T) {
	m, idx, y := convModel(t, "hello world here")
	stripped := ansi.Strip(strings.Split(m.vp.GetContent(), "\n")[idx])

	m, _ = pressMouse(m, tea.MouseLeft, 7, y)
	m, _ = pressMouse(m, tea.MouseLeft, 7, y)
	m, _ = pressMouse(m, tea.MouseLeft, 7, y)

	if m.sel.anchorC != 0 {
		t.Errorf("triple-click anchorC = %d, want 0", m.sel.anchorC)
	}
	if want := graphemeCount(stripped); m.sel.headC != want {
		t.Errorf("triple-click headC = %d, want graphemeCount %d", m.sel.headC, want)
	}
	if got := selectedText(m.vp.GetContent(), m.sel); got != "hello world here" {
		t.Errorf("triple-click selectedText = %q, want %q (whole line, trailing trimmed)", got, "hello world here")
	}
	if !strings.Contains(stripANSIstr(m.statusMsg), "copied") {
		t.Errorf("triple-click status = %q, want a 'copied N chars' confirmation (copy fired)", stripANSIstr(m.statusMsg))
	}
}

// TestClickCountSamePositionAdvances: presses at the same logical position advance
// 1→2→3 and a 4th WRAPS to 1 (a fresh zero-width anchor, not a line).
func TestClickCountSamePositionAdvances(t *testing.T) {
	m, _, y := convModel(t, "hello world here")

	m, _ = pressMouse(m, tea.MouseLeft, 7, y)
	if m.clickCount != 1 {
		t.Fatalf("press1 clickCount = %d, want 1", m.clickCount)
	}
	m, _ = pressMouse(m, tea.MouseLeft, 7, y)
	if m.clickCount != 2 {
		t.Fatalf("press2 clickCount = %d, want 2", m.clickCount)
	}
	m, _ = pressMouse(m, tea.MouseLeft, 7, y)
	if m.clickCount != 3 {
		t.Fatalf("press3 clickCount = %d, want 3", m.clickCount)
	}
	m, _ = pressMouse(m, tea.MouseLeft, 7, y)
	if m.clickCount != 1 {
		t.Fatalf("press4 clickCount = %d, want 1 (wrap)", m.clickCount)
	}
	// The wrapped 4th press must be a fresh zero-width anchor, NOT a line select —
	// and a zero-width anchor copies NOTHING. An empty selection means clickCopy is
	// never reached (the count==1 branch returns before it) and selectedText is "",
	// so there is provably no clipboard payload. This is the deterministic no-copy
	// assertion (criterion 6): we never run the returned disarm tick.
	if !m.sel.empty() {
		t.Errorf("wrapped 4th press should be a zero-width anchor, got anchorC=%d headC=%d", m.sel.anchorC, m.sel.headC)
	}
	if got := selectedText(m.vp.GetContent(), m.sel); got != "" {
		t.Errorf("wrapped 4th press selectedText = %q, want \"\" (no copy)", got)
	}
}

// TestClickCountDifferentPositionResets: a press at a clearly different logical
// column resets the count to 1, updates clickL/clickC, and yields a single anchor
// (not a word).
func TestClickCountDifferentPositionResets(t *testing.T) {
	m, _, y := convModel(t, "hello world here")

	m, _ = pressMouse(m, tea.MouseLeft, 1, y) // col A
	if m.clickCount != 1 {
		t.Fatalf("first press clickCount = %d, want 1", m.clickCount)
	}
	m, _ = pressMouse(m, tea.MouseLeft, 9, y) // clearly different col B
	if m.clickCount != 1 {
		t.Errorf("press at a different position clickCount = %d, want 1 (reset)", m.clickCount)
	}
	if m.clickC != 9 {
		t.Errorf("clickC = %d, want 9 (recorded the new position)", m.clickC)
	}
	if !m.sel.empty() {
		t.Errorf("a reset press should be a zero-width anchor, not a word: anchorC=%d headC=%d", m.sel.anchorC, m.sel.headC)
	}
}

// TestClickDisarmResetsCount: a matching clickDisarmMsg resets the count to 0; a
// STALE gen (after a re-arm) does NOT.
func TestClickDisarmResetsCount(t *testing.T) {
	m, _, y := convModel(t, "hello world here")

	m, _ = pressMouse(m, tea.MouseLeft, 7, y)
	if m.clickCount != 1 {
		t.Fatalf("press clickCount = %d, want 1", m.clickCount)
	}
	gen := m.clickGen
	mm, _ := m.Update(clickDisarmMsg{gen: gen})
	m = mm.(Model)
	if m.clickCount != 0 {
		t.Errorf("matching clickDisarmMsg should reset count to 0, got %d", m.clickCount)
	}

	// Re-arm with a new press (bumps clickGen), then feed the STALE prior-gen tick.
	m, _ = pressMouse(m, tea.MouseLeft, 7, y)
	if m.clickCount != 1 {
		t.Fatalf("re-arm press clickCount = %d, want 1", m.clickCount)
	}
	staleGen := m.clickGen - 1
	// Guard: the stale gen must be genuinely PRIOR — if it accidentally equalled the
	// current gen the test would silently pass by feeding the live disarm.
	if staleGen == m.clickGen {
		t.Fatal("stale gen equals current — test can't distinguish")
	}
	mm, _ = m.Update(clickDisarmMsg{gen: staleGen})
	m = mm.(Model)
	if m.clickCount != 1 {
		t.Errorf("a stale clickDisarmMsg must NOT reset the count, got %d", m.clickCount)
	}
}

// TestDoubleClickOnWhitespaceSelectsSpaceRun: a double-click on whitespace selects
// the whitespace run (assert on the column span width, since selectedText trims
// trailing spaces).
func TestDoubleClickOnWhitespaceSelectsSpaceRun(t *testing.T) {
	// "ab    cd": four spaces at cols 2..5 (a whitespace payload trims to "" so the
	// click copies nothing and does NOT refreshView — the selection geometry persists).
	m, _, y := convModel(t, "ab    cd")

	m, _ = pressMouse(m, tea.MouseLeft, 3, y) // inside the space run
	m, _ = pressMouse(m, tea.MouseLeft, 3, y)

	if !m.sel.active {
		t.Fatal("double-click on whitespace should leave an active selection")
	}
	if w := m.sel.headC - m.sel.anchorC; w != 4 {
		t.Errorf("whitespace run width = %d, want 4 (cols 2..5)", w)
	}
	if m.sel.anchorC != 2 || m.sel.headC != 6 {
		t.Errorf("whitespace span = [%d,%d), want [2,6)", m.sel.anchorC, m.sel.headC)
	}
}

// assertNoCopy is the DETERMINISTIC no-copy check shared by the empty-gesture tests:
// the selection is empty (so clickCopy's payload := selectedText(...) is "" and the
// copy branch is never taken) AND the status carries no "copied" confirmation. It
// touches no command, so the 400ms disarm tick is never run.
func assertNoCopy(t *testing.T, m Model, what string) {
	t.Helper()
	if !m.sel.empty() {
		t.Errorf("%s should be an empty selection, got [%d,%d)-[%d,%d)", what, m.sel.anchorL, m.sel.anchorC, m.sel.headL, m.sel.headC)
	}
	if got := selectedText(m.vp.GetContent(), m.sel); got != "" {
		t.Errorf("%s selectedText = %q, want \"\" (no payload)", what, got)
	}
	if strings.Contains(stripANSIstr(m.statusMsg), "copied") {
		t.Errorf("%s set a 'copied' status %q, want none (no copy fired)", what, stripANSIstr(m.statusMsg))
	}
}

// TestDoubleClickPastEOLNoCopy: a double-click past the content width selects
// nothing and copies nothing.
func TestDoubleClickPastEOLNoCopy(t *testing.T) {
	m, _ := selModel(t)
	m = setColContent(t, m, "abc")
	tp := convTopRow(m)

	m, _ = pressMouse(m, tea.MouseLeft, 50, tp) // well past EOL (clamps to col 3 == n)
	m, _ = pressMouse(m, tea.MouseLeft, 50, tp)

	assertNoCopy(t, m, "double-click past EOL")
}

// TestDoubleClickEmptyLineNoCopy: a double-click on a blank line copies nothing.
func TestDoubleClickEmptyLineNoCopy(t *testing.T) {
	m, _ := selModel(t)
	m = setColContent(t, m, "first\n\nthird") // line 1 is blank
	tp := convTopRow(m)

	m, _ = pressMouse(m, tea.MouseLeft, 0, tp+1) // the blank line
	m, _ = pressMouse(m, tea.MouseLeft, 0, tp+1)

	assertNoCopy(t, m, "double-click on a blank line")
}

// TestTripleClickEmptyLineNoCopy: a triple-click on a blank line copies nothing
// (the whole-line span is still empty).
func TestTripleClickEmptyLineNoCopy(t *testing.T) {
	m, _ := selModel(t)
	m = setColContent(t, m, "first\n\nthird")
	tp := convTopRow(m)

	m, _ = pressMouse(m, tea.MouseLeft, 0, tp+1)
	m, _ = pressMouse(m, tea.MouseLeft, 0, tp+1)
	m, _ = pressMouse(m, tea.MouseLeft, 0, tp+1)

	assertNoCopy(t, m, "triple-click on a blank line")
}

// TestMultiClickInertUnderOverlay: under each non-selectable state, two presses
// start NO selection AND leave clickCount==0 (Req 9, count is AFTER the gate).
func TestMultiClickInertUnderOverlay(t *testing.T) {
	cases := []nonSelectableCase{
		{"help", func(m *Model) { m.showHelp = true }},
		{"noMouse", func(m *Model) { m.deps.NoMouse = true }},
		{"noAltScreen", func(m *Model) { m.deps.NoAltScreen = true }},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			m, _ := selModel(t)
			m = setColContent(t, m, "hello world")
			tc.enter(&m)
			if selectable(m) {
				t.Fatalf("precondition: %s should be non-selectable", tc.name)
			}
			tp := convTopRow(m)
			m, _ = pressMouse(m, tea.MouseLeft, 7, tp)
			m, _ = pressMouse(m, tea.MouseLeft, 7, tp)
			if m.sel.active {
				t.Errorf("%s: presses must not start a selection", tc.name)
			}
			if m.clickCount != 0 {
				t.Errorf("%s: presses must not advance clickCount (got %d)", tc.name, m.clickCount)
			}
		})
	}
}

// TestDragAfterDoubleClickResetsCount: a drag-extend after a double-click resets
// clickCount to 0 and moves the head (the word/line leaves a real anchor/head so a
// subsequent drag extends normally).
func TestDragAfterDoubleClickResetsCount(t *testing.T) {
	m, _, y := convModel(t, "hello world second line")

	m, _ = pressMouse(m, tea.MouseLeft, 7, y)
	m, _ = pressMouse(m, tea.MouseLeft, 7, y)
	if m.clickCount != 2 {
		t.Fatalf("precondition: double-click clickCount = %d, want 2", m.clickCount)
	}
	beforeHead := m.sel.headC

	m, _ = motionMouse(m, 20, y) // drag-extend to the right

	if m.clickCount != 0 {
		t.Errorf("a drag after double-click should reset clickCount to 0, got %d", m.clickCount)
	}
	if m.sel.headC == beforeHead {
		t.Errorf("the drag should have moved the head (%d → %d)", beforeHead, m.sel.headC)
	}
}

// TestDoubleClickIdentitySnapshotSurvivesRefresh: a double-click word selection
// survives a refreshView with appended content below it (snapshot still matches, so
// the highlight is reapplied rather than the selection dropped).
func TestDoubleClickIdentitySnapshotSurvivesRefresh(t *testing.T) {
	m, _, y := convModel(t, "hello world after")

	// "world" at cols 6..10; double-click selects it.
	m, _ = pressMouse(m, tea.MouseLeft, 7, y)
	m, _ = pressMouse(m, tea.MouseLeft, 7, y)
	if !m.sel.active || m.sel.empty() {
		t.Fatal("precondition: double-click should leave a non-empty selection")
	}
	anchorC, headC := m.sel.anchorC, m.sel.headC

	m.phase = phaseRunning
	m = applyAll(m,
		client.AssistantDeltaMsg{Turn: 1, Text: strings.Repeat("appended\n", 5)},
		renderTickMsg{},
	)

	if !m.sel.active {
		t.Error("double-click selection should survive a streaming delta below it")
	}
	if m.sel.anchorC != anchorC || m.sel.headC != headC {
		t.Errorf("delta moved the word selection columns: [%d,%d) → [%d,%d)", anchorC, headC, m.sel.anchorC, m.sel.headC)
	}
	if len(byteRanges(m.vp.GetContent(), m.sel)) == 0 {
		t.Error("the word-selection highlight should still be derivable after the refresh")
	}
}
