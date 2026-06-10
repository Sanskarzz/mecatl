package ui

// TEMPORARY workaround for the unbounded wordLeft() loop in
// charm.land/bubbles/v2@v2.1.0's textarea.
//
// Upstream refs: charmbracelet/bubbletea#1652 (the report) and bubbles PRs
// #948 (incomplete — its top-of-function guard misses the leading-whitespace
// case), #959, #987 (both unmerged at v2.1.0). textarea.wordLeft() walks the
// cursor left until it lands on a non-space rune; when no non-space rune
// exists strictly before the cursor it spins forever, wedging the Bubble Tea
// update goroutine at 100% CPU. The ONE safe shape is the origin case: cursor
// at (0,0) with a non-empty line 0 whose first rune is non-space — the loop
// checks the rune UNDER the cursor and breaks immediately. Subtly, " foo"
// with the cursor at (0,0) HANGS (the rune under the origin cursor is a
// space).
//
// We deliberately guard here instead of forking/vendoring bubbles: the two
// update.go call sites swallow the would-hang WordBackward keypress before it
// reaches textarea.Update. Swallowing is semantically a no-op — upstream's
// intended "no word to the left" behaviour (by symmetry with doWordRight) is
// don't-move.
//
// Removal condition: once we are on a bubbles release whose textarea.wordLeft
// has an in-loop boundary guard, delete this file, the two call sites in
// update.go, and the workaround paragraph in
// docs/design/IMPLEMENTATION-NOTES.md.
//
// Scope notes: the bubbles textINPUT widget has the boundary guard upstream —
// the mcp.go/models.go textinput overlays need nothing. Any FUTURE
// textarea-bearing overlay DOES need the same one-line guard in front of its
// textarea.Update.

import (
	"strings"
	"unicode"

	"charm.land/bubbles/v2/key"
	"charm.land/bubbles/v2/textarea"
	tea "charm.land/bubbletea/v2"
)

// swallowWordLeftHang reports whether msg is a WordBackward keypress that
// would send ta's wordLeft() into its unbounded loop. It matches against the
// instance's OWN keymap so rebinds stay in sync with the guard.
func swallowWordLeftHang(ta textarea.Model, msg tea.KeyPressMsg) bool {
	return key.Matches(msg, ta.KeyMap.WordBackward) && wordLeftWouldHang(ta)
}

// wordLeftWouldHang mirrors the termination condition of bubbles v2.1.0's
// textarea.wordLeft(): the loop terminates iff a non-space rune exists
// strictly before the cursor (any earlier row, or the current row before the
// cursor column) — with one exception at the buffer origin, where the loop
// checks the rune UNDER the cursor: a leading non-space on line 0 breaks
// immediately and is safe.
func wordLeftWouldHang(ta textarea.Model) bool {
	row := ta.Line()                       // buffer row
	col := ta.Column()                     // RUNE index into the row
	raw := strings.Split(ta.Value(), "\n") // Value joins rows with \n (trailing \n trimmed)
	lines := make([][]rune, len(raw))
	for i, l := range raw {
		lines[i] = []rune(l) // rune-slice, never byte-slice: col is a rune index
	}
	// Defensive clamps — Line/Column should always be in range of Value, but a
	// stale read must fail toward "no swallow" mistakes, not a panic.
	if row > len(lines)-1 {
		row = len(lines) - 1
	}
	if row < 0 {
		row = 0
	}
	cur := lines[row]
	if col > len(cur) {
		col = len(cur)
	}
	if col < 0 {
		col = 0
	}
	// A non-space rune strictly before the cursor means wordLeft terminates.
	for r := 0; r < row; r++ {
		for _, rn := range lines[r] {
			if !unicode.IsSpace(rn) {
				return false
			}
		}
	}
	for _, rn := range cur[:col] {
		if !unicode.IsSpace(rn) {
			return false
		}
	}
	if row == 0 && col == 0 {
		// Origin: the loop inspects the rune UNDER the cursor. A leading
		// non-space breaks immediately (safe — do not swallow); an empty line
		// or a leading space spins.
		return len(cur) == 0 || unicode.IsSpace(cur[0])
	}
	return true
}
