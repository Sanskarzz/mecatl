package ui

import (
	"strings"

	"github.com/charmbracelet/x/ansi"
)

// headerH is the screen height of the top header bar: one styled identity line
// plus its bottom border. It is the SINGLE source of truth for the header height —
// onResize subtracts it from the window to size the viewport, and convTopRow uses
// it as the viewport's first screen row. Keeping it one constant means a header
// layout change can't silently mis-map click coordinates against the viewport.
const headerH = 2

// autoScrollDir is the edge-autoscroll direction stashed while a drag is held at a
// viewport border (so a self-re-arming tick keeps scrolling without further mouse
// movement). scrollNone disarms any pending tick.
type autoScrollDir int

const (
	scrollNone autoScrollDir = iota
	scrollUp
	scrollDown
)

// selection is the in-app text-selection state: a left-click-drag over the
// conversation viewport that highlights runes and copies them on release (OSC52 +
// shell fallback). Its coordinates are LOGICAL CONTENT positions, NOT screen
// positions: anchorL/headL index a real line of the viewport content
// (m.vp.GetContent() split on \n), and anchorC/headC are GRAPHEME COLUMNS into the
// ANSI-STRIPPED form of that line. Logical anchors are why the highlight survives
// scrolling and a streaming re-render: the byte ranges are recomputed against the
// CURRENT content each frame (applySelectionHighlight in refreshView), so a
// scroll only moves which logical lines are on screen, and a streamed delta that
// grows the content leaves the selected logical span untouched. The zero value is
// an inactive selection.
//
// autoScroll is the edge-drag direction: non-scrollNone while the held pointer sits
// at the top/bottom viewport border, driving a self-re-arming tick that scrolls one
// line at a time and extends the head to the newly-revealed line. It is cleared
// (back to scrollNone) on release, on motion back inside the region, on esc-clear,
// and whenever the selection goes inactive — so any in-flight tick no-ops.
type selection struct {
	active           bool
	anchorL, anchorC int
	headL, headC     int
	autoScroll       autoScrollDir
	// dragX is the last drag pointer's CELL X, stashed while edge-autoscroll is armed
	// so the self-re-arming tick (which carries no fresh mouse coordinate) can keep
	// the head at the same horizontal column as the view scrolls.
	dragX int
}

// convTopRow is the screen row where the conversation viewport's first row sits. It
// equals the header height (headerH). Returns -1 (sentinel "unknown") before the
// first resize, when width/height are unset — callers then refuse to start a
// selection.
func convTopRow(m Model) int {
	if m.width <= 0 || m.height <= 0 {
		return -1
	}
	return headerH
}

// selectable reports whether a left-click may START a selection right now. It is
// the SAME overlay/mode gate the body switch in view.go uses to decide what owns
// the conversation region: a selection may only begin when the plain viewport is
// showing it. Mouse capture exists only on the alt screen, so --inline/--no-alt-
// screen (NoAltScreen) is never selectable; an open overlay/modal/help or the
// fatal screen owns the body and blocks a new selection (and opening one mid-drag
// clears the active selection — see the overlay-open paths in update.go).
func selectable(m Model) bool {
	return !m.deps.NoAltScreen &&
		m.phase != phaseFatal &&
		m.phase != phaseAwaitingApproval &&
		!m.showHelp &&
		m.mcp.view == mcpNone &&
		m.team.view == teamNone &&
		m.agentsInv.view == agentsInvNone &&
		m.skills.view == skillsNone &&
		m.soul.view == soulNone &&
		m.userModel.view == userModelNone &&
		m.models.view == modelsNone
}

// screenToContent maps a screen cell (x, y) to a LOGICAL content position (line
// index into the viewport content, grapheme column into the ansi-stripped line).
// ok is false when (x, y) is outside the conversation region — above the top row,
// at/below the bottom of the viewport, or before width/height are known (Req 9:
// header/input/footer clicks start no selection). SoftWrap is OFF in mecatui (it
// is never enabled), so the screen→line mapping is a simple offset: a screen row
// is YOffset()+(screenY-convTop), and the screen X is a grapheme column directly
// (XOffset is 0 in normal use, added defensively).
func screenToContent(m Model, x, y int) (line, col int, ok bool) {
	top := convTopRow(m)
	if top < 0 {
		return 0, 0, false
	}
	vpH := m.vp.Height()
	if y < top || y >= top+vpH {
		return 0, 0, false
	}
	line = m.vp.YOffset() + (y - top)
	lines := strings.Split(m.vp.GetContent(), "\n")
	if len(lines) == 0 {
		return 0, 0, false
	}
	if line < 0 {
		line = 0
	}
	if line >= len(lines) {
		// A click in the blank region below the last content line clamps to the end
		// of the last line (a natural "select to the end" gesture), not a miss.
		line = len(lines) - 1
	}
	stripped := ansi.Strip(lines[line])
	col = graphemeColForCellX(stripped, x+m.vp.XOffset())
	return line, col, true
}

// graphemeColForCellX converts a display cell X into a GRAPHEME-CLUSTER column on
// the ansi-stripped line by walking clusters and summing their display width until
// the accumulated width exceeds cellX. The result is clamped to the line's
// grapheme count, so a click past end-of-line lands at the line end (Req 6's
// "click past EOL = line end"). It uses ansi.FirstGraphemeCluster + ansi.StringWidth
// — the SAME segmentation/width engine the renderer uses — so the column can never
// disagree with the on-screen layout.
func graphemeColForCellX(stripped string, cellX int) int {
	if cellX <= 0 {
		return 0
	}
	col, width := 0, 0
	rest := stripped
	for len(rest) > 0 {
		cl, _ := ansi.FirstGraphemeCluster(rest, ansi.GraphemeWidth)
		if cl == "" {
			break
		}
		w := ansi.StringWidth(cl)
		if w <= 0 {
			w = 1
		}
		if width+w > cellX {
			return col
		}
		width += w
		col++
		rest = rest[len(cl):]
	}
	return col
}

// normalize returns the selection's (start, end) in document order — start <= end
// by (line, then column) — so byteRanges and selectedText are order-independent
// (a top-down drag and a bottom-up drag select the same span).
func (s selection) normalize() (startL, startC, endL, endC int) {
	startL, startC, endL, endC = s.anchorL, s.anchorC, s.headL, s.headC
	if endL < startL || (endL == startL && endC < startC) {
		startL, startC, endL, endC = endL, endC, startL, startC
	}
	return startL, startC, endL, endC
}

// empty reports a degenerate (zero-width) selection: anchor == head. An empty
// selection copies nothing and is cleared on release rather than copied.
func (s selection) empty() bool {
	return s.anchorL == s.headL && s.anchorC == s.headC
}

// byteRanges converts the selection into the viewport's native SetHighlights
// argument: ascending, non-overlapping [start,end] byte-offset pairs, ONE per
// spanned content line. The offsets are bytes into the ANSI-STRIPPED content
// (verified against bubbles viewport v2.1.0's parseMatches, which walks
// ansi.Strip(content) — a per-line range into the stripped content highlights the
// right cells, where a single range straddling a \n does not). Each line's range
// runs from the selection's start column (or column 0 for an interior/last line)
// to its end column (or the line's full grapheme width for an interior/first
// line), translated grapheme-column → stripped-byte offset, plus the cumulative
// stripped-byte offset of that line (including the \n separators before it).
func byteRanges(content string, sel selection) [][]int {
	if !sel.active || sel.empty() {
		return nil
	}
	startL, startC, endL, endC := sel.normalize()
	lines := strings.Split(content, "\n")
	if startL < 0 || startL >= len(lines) {
		return nil
	}
	if endL >= len(lines) {
		endL = len(lines) - 1
	}

	// Cumulative stripped-byte offset of the start of each stripped line. The
	// separator between stripped lines is a single '\n' (1 byte), matching how the
	// viewport joins m.lines with "\n".
	ranges := make([][]int, 0, endL-startL+1)
	off := 0
	for li := 0; li < len(lines); li++ {
		stripped := ansi.Strip(lines[li])
		if li >= startL && li <= endL {
			fromCol := 0
			if li == startL {
				fromCol = startC
			}
			toCol := graphemeCount(stripped)
			if li == endL {
				toCol = endC
			}
			b0 := off + colToByte(stripped, fromCol)
			b1 := off + colToByte(stripped, toCol)
			if b1 > b0 {
				ranges = append(ranges, []int{b0, b1})
			}
		}
		off += len(stripped) + 1 // +1 for the '\n' separator
	}
	if len(ranges) == 0 {
		return nil
	}
	return ranges
}

// selectedText is the ANSI-STRIPPED, copy-ready payload for the selection: for
// each spanned line, the ansi-stripped text sliced by the grapheme-column range,
// joined with "\n", with per-line trailing padding trimmed (mirroring render.go's
// trimTrailingSpaces so the glamour right-padding never bloats a copy). Returns ""
// for an inactive/empty selection.
func selectedText(content string, sel selection) string {
	if !sel.active || sel.empty() {
		return ""
	}
	startL, startC, endL, endC := sel.normalize()
	lines := strings.Split(content, "\n")
	if startL < 0 || startL >= len(lines) {
		return ""
	}
	if endL >= len(lines) {
		endL = len(lines) - 1
	}
	out := make([]string, 0, endL-startL+1)
	for li := startL; li <= endL; li++ {
		stripped := ansi.Strip(lines[li])
		fromCol := 0
		if li == startL {
			fromCol = startC
		}
		toCol := graphemeCount(stripped)
		if li == endL {
			toCol = endC
		}
		seg := stripped[colToByte(stripped, fromCol):colToByte(stripped, toCol)]
		out = append(out, strings.TrimRight(seg, " "))
	}
	return strings.Join(out, "\n")
}

// graphemeCount returns the number of grapheme clusters in an ansi-stripped line —
// the maximum valid grapheme column (a column equal to it is the line end).
func graphemeCount(stripped string) int {
	n := 0
	rest := stripped
	for len(rest) > 0 {
		cl, _ := ansi.FirstGraphemeCluster(rest, ansi.GraphemeWidth)
		if cl == "" {
			break
		}
		n++
		rest = rest[len(cl):]
	}
	return n
}

// colToByte converts a grapheme-cluster column into a BYTE offset within an
// ansi-stripped line, clamped to the line length when col is at/past the end.
func colToByte(stripped string, col int) int {
	if col <= 0 {
		return 0
	}
	pos, n := 0, 0
	rest := stripped
	for len(rest) > 0 && n < col {
		cl, _ := ansi.FirstGraphemeCluster(rest, ansi.GraphemeWidth)
		if cl == "" {
			break
		}
		pos += len(cl)
		rest = rest[len(cl):]
		n++
	}
	return pos
}
