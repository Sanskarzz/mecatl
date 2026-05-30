package ui

import (
	"bytes"
	"encoding/json"
	"fmt"
	"strconv"
	"strings"
	"sync"

	"charm.land/glamour/v2"
	"charm.land/lipgloss/v2"

	"github.com/stacklok/mecatl/cmd/mecatui/theme"
)

// maxToolResultLines caps how many lines of a tool result are shown inline; the
// rest collapse into a "+N more lines" affordance so a giant Read result doesn't
// drown the scrollback.
const maxToolResultLines = 12

// maxDiffLines caps how many lines of each diff side (Edit old/new, Write
// content) show inline when collapsed; ctrl+t expands to the full diff.
const maxDiffLines = 12

// renderer turns conversation blocks into the viewport string. It owns the
// glamour TermRenderer cache (keyed by wrap width) and the active theme. glamour
// is NOT thread-safe, so renderer is only ever touched from the Bubble Tea
// update goroutine — never from the stream reader. The mutex guards the cache map
// against the (currently single-goroutine) access defensively and documents the
// invariant; it does not make glamour itself concurrency-safe.
type renderer struct {
	th    theme.Theme
	width int

	mu    sync.Mutex
	cache map[int]*glamour.TermRenderer
}

// newRenderer builds a renderer for a theme.
func newRenderer(th theme.Theme) *renderer {
	return &renderer{th: th, cache: map[int]*glamour.TermRenderer{}}
}

// setWidth records the current wrap width. Width changes are handled by the
// cache key, so no explicit invalidation is needed.
func (r *renderer) setWidth(w int) { r.width = w }

// markdown renders src to ANSI through a width-cached glamour renderer themed by
// the active theme. On any glamour error it falls back to the raw text so the
// stream is never lost. MUST be called only on the update goroutine.
func (r *renderer) markdown(src string) string {
	if strings.TrimSpace(src) == "" {
		return ""
	}
	w := r.width
	if w <= 0 {
		w = 80
	}
	r.mu.Lock()
	tr, ok := r.cache[w]
	if !ok {
		built, err := glamour.NewTermRenderer(
			glamour.WithStyles(r.th.GlamourStyle()),
			glamour.WithWordWrap(w),
		)
		if err != nil {
			r.mu.Unlock()
			return src
		}
		tr = built
		r.cache[w] = tr
	}
	r.mu.Unlock()

	out, err := tr.Render(src)
	if err != nil {
		return src
	}
	return strings.TrimRight(out, "\n")
}

// renderConversation joins every block into the viewport content string. expand
// is the global tool-output toggle (ctrl+t): when true, tool result bodies and
// Edit/Write diffs render in full instead of line-capped.
func (r *renderer) renderConversation(c *conversation, expand bool) string {
	var b strings.Builder
	for i := range c.blocks {
		if i > 0 {
			b.WriteString("\n")
		}
		b.WriteString(r.renderBlock(&c.blocks[i], expand))
		b.WriteString("\n")
	}
	return b.String()
}

// renderBlock renders one block per its kind. Assistant text goes through
// glamour; everything else is plain themed lipgloss.
func (r *renderer) renderBlock(b *block, expand bool) string {
	switch b.kind {
	case blockUser:
		label := r.th.Style("userLabel").Render("you")
		body := r.th.Style("userBlock").Render(sanitizeTerminal(b.raw))
		return label + "\n" + body
	case blockAssistant:
		// Assistant text is rendered through glamour, which neutralises escape
		// sequences itself — do NOT sanitize here or markdown breaks.
		label := r.th.Style("assistantLabel").Render("mecatl")
		return label + "\n" + r.markdown(b.raw)
	case blockTool:
		return r.renderTool(b, expand)
	case blockNotice:
		return r.th.Style("muted").Render("• " + sanitizeTerminal(b.raw))
	case blockError:
		return r.th.Style("errorText").Render("✗ " + sanitizeTerminal(b.raw))
	default:
		return sanitizeTerminal(b.raw)
	}
}

// renderTool renders a tool-call card: status glyph + name + body, and, once
// resolved, a truncated result body beneath it. For Edit/Write the args are
// shown as a colourised diff instead of raw JSON (falling back to pretty JSON if
// the args don't parse as the expected shape). expand removes the line cap on
// the result body and the diff.
func (r *renderer) renderTool(b *block, expand bool) string {
	var glyph string
	switch {
	case !b.resolved:
		glyph = r.th.Style("toolName").Render("…")
	case b.resultError:
		glyph = r.th.Style("toolErr").Render("✗")
	default:
		glyph = r.th.Style("toolOk").Render("✓")
	}

	head := glyph + " " + r.th.Style("toolName").Render(sanitizeTerminal(b.toolName))

	// Edit/Write render their change as a diff in place of the raw JSON args.
	if diff, ok := r.renderToolDiff(b.toolName, b.toolArgs, expand); ok {
		if diff != "" {
			head += "\n" + diff
		}
	} else if args := prettyJSON(b.toolArgs); args != "" {
		head += "\n" + r.th.Style("toolArgs").Render(args)
	}

	if b.resolved {
		body := resultBody(b.resultBody, expand)
		if body != "" {
			style := r.th.Style("toolArgs")
			if b.resultError {
				style = r.th.Style("errorText")
			}
			head += "\n" + style.Render(body)
		}
	}

	card := r.th.Style("toolCard")
	if r.width > 4 {
		card = card.Width(r.width - 2)
	}
	return card.Render(head)
}

// resultBody renders a tool result body: full when expanded, else line-capped.
func resultBody(body string, expand bool) string {
	if expand {
		return sanitizeTerminal(strings.TrimRight(body, "\n"))
	}
	return truncateLines(body, maxToolResultLines)
}

// renderToolDiff renders a colourised diff for the Edit and Write tools. It
// returns (rendered, true) when name is a diff-capable tool AND its args parse
// into the expected shape; otherwise (",", false) so the caller falls back to
// the existing pretty-JSON rendering. All server-derived text is sanitized
// before it reaches lipgloss.
func (r *renderer) renderToolDiff(name, rawArgs string, expand bool) (string, bool) {
	switch name {
	case "Edit":
		return r.renderEditDiff(rawArgs, expand)
	case "Write":
		return r.renderWriteDiff(rawArgs, expand)
	default:
		return "", false
	}
}

// editDiffArgs mirrors internal/adapter/tools/edit.go's editArgs JSON shape.
type editDiffArgs struct {
	Path       string `json:"path"`
	OldString  string `json:"old_string"`
	NewString  string `json:"new_string"`
	ReplaceAll bool   `json:"replace_all"`
}

// renderEditDiff renders an Edit as a red/green unified-style diff:
// removed (old_string) lines prefixed "-", added (new_string) lines prefixed
// "+", under a muted path header (with a "(replace all)" tag when set). Returns
// false on malformed/empty args so the caller falls back to JSON.
func (r *renderer) renderEditDiff(rawArgs string, expand bool) (string, bool) {
	var args editDiffArgs
	if err := json.Unmarshal([]byte(strings.TrimSpace(rawArgs)), &args); err != nil {
		return "", false
	}
	if args.Path == "" || (args.OldString == "" && args.NewString == "") {
		return "", false
	}

	// Size signal: removed/added line counts (empty side = 0 lines).
	removed := lineCount(args.OldString)
	added := lineCount(args.NewString)
	header := fmt.Sprintf("%s  -%d +%d", args.Path, removed, added)
	if args.ReplaceAll {
		header += " (replace all)"
	}
	var b strings.Builder
	b.WriteString(r.th.Style("diffMeta").Render(sanitizeTerminal(header)))
	b.WriteString("\n")
	b.WriteString(r.diffSide(args.OldString, "-", "diffRemove", expand))
	b.WriteString(r.diffSide(args.NewString, "+", "diffAdd", expand))
	return strings.TrimRight(b.String(), "\n"), true
}

// writeDiffArgs mirrors internal/adapter/tools/write.go's writeArgs JSON shape.
type writeDiffArgs struct {
	Path    string `json:"path"`
	Content string `json:"content"`
}

// renderWriteDiff renders a Write as an all-green "new content" block under a
// muted path header. Returns false on malformed args so the caller falls back.
func (r *renderer) renderWriteDiff(rawArgs string, expand bool) (string, bool) {
	var args writeDiffArgs
	if err := json.Unmarshal([]byte(strings.TrimSpace(rawArgs)), &args); err != nil {
		return "", false
	}
	if args.Path == "" {
		return "", false
	}
	n := lineCount(args.Content)
	noun := "lines"
	if n == 1 {
		noun = "line"
	}
	header := fmt.Sprintf("%s (new file, %d %s)", args.Path, n, noun)
	var b strings.Builder
	b.WriteString(r.th.Style("diffMeta").Render(sanitizeTerminal(header)))
	if args.Content != "" {
		b.WriteString("\n")
		b.WriteString(r.diffSide(args.Content, "+", "diffAdd", expand))
	}
	return strings.TrimRight(b.String(), "\n"), true
}

// diffSide renders one side of a diff (all-removed or all-added): every line of
// text gets the prefix and the themed style, line-capped unless expanded. An
// empty side renders nothing. The text is sanitized (these go through lipgloss).
func (r *renderer) diffSide(text, prefix, slot string, expand bool) string {
	text = sanitizeTerminal(strings.TrimRight(text, "\n"))
	if text == "" {
		return ""
	}
	lines := strings.Split(text, "\n")
	var marker string
	if !expand && len(lines) > maxDiffLines {
		extra := len(lines) - maxDiffLines
		lines = lines[:maxDiffLines]
		marker = collapseMarker(extra)
	}
	style := r.th.Style(slot)
	var b strings.Builder
	for _, ln := range lines {
		b.WriteString(style.Render(prefix + " " + ln))
		b.WriteString("\n")
	}
	if marker != "" {
		// The collapse marker is muted, not coloured as a diff line.
		b.WriteString(lipgloss.NewStyle().Render(marker))
		b.WriteString("\n")
	}
	return b.String()
}

// prettyJSON indents a raw JSON args string for display; non-JSON is returned
// as-is (single line). The result is terminal-sanitized since the payload is
// server-derived and rendered via lipgloss (not glamour). Empty/blank yields "".
func prettyJSON(raw string) string {
	raw = strings.TrimSpace(raw)
	if raw == "" || raw == "{}" || raw == "null" {
		return ""
	}
	var buf bytes.Buffer
	if err := json.Indent(&buf, []byte(raw), "", "  "); err != nil {
		return sanitizeTerminal(raw)
	}
	return sanitizeTerminal(buf.String())
}

// truncateLines clamps s to max lines, appending a "+N more lines" affordance
// when it overflows. The (server-derived) body is terminal-sanitized.
func truncateLines(s string, maxLines int) string {
	s = sanitizeTerminal(strings.TrimRight(s, "\n"))
	if s == "" {
		return ""
	}
	lines := strings.Split(s, "\n")
	if len(lines) <= maxLines {
		return s
	}
	kept := lines[:maxLines]
	extra := len(lines) - maxLines
	return strings.Join(kept, "\n") + "\n" + lipgloss.NewStyle().Render(collapseMarker(extra))
}

// collapseMarker formats the "+N more line(s) · ctrl+t expand" affordance shown
// when a tool result or diff side is line-capped. The verb matches the footer
// help line's collapsed-state hint ("ctrl+t expand") — the expand/collapse pair
// is used consistently across help line, keybinding help, and this marker.
func collapseMarker(n int) string {
	noun := "lines"
	if n == 1 {
		noun = "line"
	}
	return "  … +" + strconv.Itoa(n) + " more " + noun + " · ctrl+t expand"
}

// lineCount returns the number of text lines in s (0 for empty, otherwise one
// more than the number of newlines, ignoring a single trailing newline). Used
// for the diff header size signals.
func lineCount(s string) int {
	s = strings.TrimRight(s, "\n")
	if s == "" {
		return 0
	}
	return strings.Count(s, "\n") + 1
}
