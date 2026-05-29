package ui

import (
	"bytes"
	"encoding/json"
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

// renderConversation joins every block into the viewport content string.
func (r *renderer) renderConversation(c *conversation) string {
	var b strings.Builder
	for i := range c.blocks {
		if i > 0 {
			b.WriteString("\n")
		}
		b.WriteString(r.renderBlock(&c.blocks[i]))
		b.WriteString("\n")
	}
	return b.String()
}

// renderBlock renders one block per its kind. Assistant text goes through
// glamour; everything else is plain themed lipgloss.
func (r *renderer) renderBlock(b *block) string {
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
		return r.renderTool(b)
	case blockNotice:
		return r.th.Style("muted").Render("• " + sanitizeTerminal(b.raw))
	case blockError:
		return r.th.Style("errorText").Render("✗ " + sanitizeTerminal(b.raw))
	default:
		return sanitizeTerminal(b.raw)
	}
}

// renderTool renders a tool-call card: status glyph + name + pretty args, and,
// once resolved, a truncated result body beneath it.
func (r *renderer) renderTool(b *block) string {
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
	args := prettyJSON(b.toolArgs)
	if args != "" {
		head += "\n" + r.th.Style("toolArgs").Render(args)
	}

	if b.resolved {
		body := truncateLines(b.resultBody, maxToolResultLines)
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
	return strings.Join(kept, "\n") + "\n" + lipgloss.NewStyle().Render(plural(extra))
}

// plural formats the "+N more line(s)" affordance.
func plural(n int) string {
	if n == 1 {
		return "  … +1 more line"
	}
	return "  … +" + strconv.Itoa(n) + " more lines"
}
