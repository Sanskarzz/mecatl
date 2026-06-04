package ui

import (
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strings"

	tea "charm.land/bubbletea/v2"

	"github.com/stacklok/mecatl/cmd/mecatui/client"
)

// mediaExts is the cheap allowlist tryPasteMediaPath uses to fast-reject a pasted
// token before touching the filesystem: only a path whose extension looks like an
// image/audio file is a candidate for media staging. The authoritative decision is
// still the content sniff in client.StagePathMedia — this is just a quick filter so
// ordinary prose pastes never stat/read a file.
var mediaExts = map[string]struct{}{
	".png": {}, ".jpg": {}, ".jpeg": {}, ".gif": {}, ".webp": {}, ".bmp": {},
	".wav": {}, ".mp3": {}, ".m4a": {}, ".ogg": {}, ".flac": {},
}

// isMediaExt reports whether path has a known image/audio extension (case-folded).
func isMediaExt(path string) bool {
	_, ok := mediaExts[strings.ToLower(filepath.Ext(path))]
	return ok
}

// stagedAttachment is a clipboard image the user has pasted (ctrl+v) but not yet
// sent. It is proto-free — just the sniffed mime and the raw bytes — so the ui
// never names a proto type; client.StageClipboardImage rebuilds it into a Content
// part at submit time. It is keyed in Model.stagedMedia by its literal "[Image
// #N]" marker, which also appears in the textarea text.
type stagedAttachment struct {
	mime string
	data []byte
}

// clipboardResultMsg carries a successful clipboard read back to the reducer: a
// mime (image/* → stage a part; text → insert into the textarea) and its bytes.
type clipboardResultMsg struct {
	mime string
	data []byte
}

// clipboardErrMsg carries a clipboard read failure (no backend, empty, oversize,
// backend error) back to the reducer for an appropriate status / transcript line.
type clipboardErrMsg struct{ err error }

// onClipboardPaste handles ctrl+v. A nil Clipboard collaborator (the inject-time
// disable, mirroring nil MCP/Cmds) yields a benign "unavailable" status. Otherwise
// it returns a command that reads the OS clipboard off the update goroutine,
// surfacing a clipboardResultMsg or clipboardErrMsg. The caps.Image gate is decided
// at RESULT time (onClipboardResult), NOT here: ctrl+v must still paste TEXT on an
// image-incapable model (the spec's image-first, text-fallback contract), so a read
// is always attempted; only an IMAGE result is refused when caps.Image is false.
func (m Model) onClipboardPaste() (tea.Model, tea.Cmd) {
	if m.deps.Clipboard == nil {
		m.statusMsg = m.deps.Theme.Style("muted").Render("clipboard paste unavailable")
		return m, nil
	}
	cb := m.deps.Clipboard
	ctx := m.deps.Ctx
	return m, func() tea.Msg {
		mime, data, err := cb.Read(ctx)
		if err != nil {
			return clipboardErrMsg{err: err}
		}
		return clipboardResultMsg{mime: mime, data: data}
	}
}

// onClipboardResult reduces a successful clipboard read. An image is cap-gated
// (refused with a clear message when the server's model takes no images) and,
// when accepted, staged under a fresh monotonic "[Image #N]" marker that is also
// inserted into the textarea; the part is rebuilt and attached at submit time. A
// text result is inserted into the textarea verbatim regardless of caps (the
// text-fallback), through the same afterInputEdit funnel typed input uses so the
// /@ palettes resync.
func (m Model) onClipboardResult(msg clipboardResultMsg) (tea.Model, tea.Cmd) {
	if strings.HasPrefix(msg.mime, "image/") {
		if !m.caps.Image {
			m.statusMsg = m.deps.Theme.Style("muted").Render("this model takes no images — clipboard image not attached")
			return m, nil
		}
		m.nextMediaN++
		marker := fmt.Sprintf("[Image #%d]", m.nextMediaN)
		if m.stagedMedia == nil {
			m.stagedMedia = make(map[string]stagedAttachment)
		}
		m.stagedMedia[marker] = stagedAttachment(msg)
		m.insertText(marker)
		return m.afterInputEdit(nil)
	}
	// Text fallback: insert the clipboard text into the prompt.
	m.insertText(string(msg.data))
	return m.afterInputEdit(nil)
}

// onClipboardErr reduces a clipboard read failure. A missing backend gets an
// actionable install hint; an empty clipboard is a benign status (no transcript
// noise); any other error (oversize image, backend failure) is a loud transcript
// error.
func (m Model) onClipboardErr(msg clipboardErrMsg) tea.Model {
	switch {
	case errors.Is(msg.err, client.ErrNoClipboardTool):
		m.statusMsg = m.deps.Theme.Style("muted").Render("image paste needs wl-clipboard (Wayland) / xclip (X11) installed")
	case errors.Is(msg.err, client.ErrEmptyClipboard):
		m.statusMsg = m.deps.Theme.Style("muted").Render("clipboard is empty")
	default:
		m.conv.addError("clipboard: " + msg.err.Error())
		m.refreshView()
	}
	return m
}

// insertText appends s (plus a trailing space when s ends a marker) at the end of
// the current input, mirroring mentionComplete's SetValue pattern: a single space
// separates it from any preceding text so a marker or pasted text never fuses with
// the prior word. The cursor lands after the inserted text.
func (m *Model) insertText(s string) {
	val := m.ta.Value()
	if val != "" && !strings.HasSuffix(val, " ") && !strings.HasSuffix(val, "\n") {
		val += " "
	}
	m.ta.SetValue(val + s + " ")
}

// tryPasteMediaPath handles a bracketed paste whose payload is a single FILE PATH
// to a media file (the common "drag an image onto the terminal" flow, which many
// terminals deliver as a pasted path): it stages the file as a clipboard-style
// attachment under a fresh "[Image #N]" marker and reports ok=true. It is
// conservative so an ordinary text paste is never mis-handled — it requires a
// single whitespace-free token with a media extension, that resolves to an
// EXISTING REGULAR FILE, and that client.StagePathMedia accepts (sniffs as a
// cap-allowed, in-size image/audio). On ANY miss it returns ok=false so onPaste
// falls through to the literal-text insert (iteration-1 behaviour) — a pasted path
// that isn't a stageable media file stays literal prose, never a loud error.
func (m Model) tryPasteMediaPath(content string) (tea.Model, tea.Cmd, bool) {
	tok := strings.TrimSpace(content)
	if tok == "" || strings.ContainsAny(tok, " \t\n") {
		return m, nil, false // multi-token / multi-line paste is prose, not a path
	}
	if !isMediaExt(tok) {
		return m, nil, false
	}
	abs := resolveMention(m.deps.Workspace, tok)
	if fi, err := os.Stat(abs); err != nil || !fi.Mode().IsRegular() {
		return m, nil, false
	}
	mime, data, _, err := client.StagePathMedia(abs, m.caps)
	if err != nil {
		return m, nil, false // unreadable / non-media / cap-gated / oversize → literal
	}
	m.nextMediaN++
	marker := fmt.Sprintf("[Image #%d]", m.nextMediaN)
	if m.stagedMedia == nil {
		m.stagedMedia = make(map[string]stagedAttachment)
	}
	m.stagedMedia[marker] = stagedAttachment{mime: mime, data: data}
	m.insertText(marker)
	mm, cmd := m.afterInputEdit(nil)
	return mm, cmd, true
}

// survivingMarkers returns the staged-media markers (sorted ascending by their N,
// so attachments ride in display order) whose literal "[Image #N]" string still
// appears in text — i.e. the user did not delete them while editing. Deleted
// markers are dropped from the send, which is how an over-eager paste is undone.
func survivingMarkers(text string, staged map[string]stagedAttachment) []string {
	var out []string
	for marker := range staged {
		if strings.Contains(text, marker) {
			out = append(out, marker)
		}
	}
	sort.Slice(out, func(i, j int) bool {
		return markerN(out[i]) < markerN(out[j])
	})
	return out
}

// markerN extracts the integer N from an "[Image #N]" marker for ordering; a
// malformed marker sorts first (N=0), which is harmless (markers are well-formed
// by construction).
func markerN(marker string) int {
	var n int
	_, _ = fmt.Sscanf(marker, "[Image #%d]", &n)
	return n
}

// stripMarker removes a "[Image #N]" marker from text WITHOUT collapsing the
// prompt's newlines or other structure: it drops the marker together with exactly
// one adjacent space (the separator insertText added), trying "marker " then
// " marker" then a bare marker, so no double space is left where the marker sat.
// Newlines, blank lines, and code-block indentation are untouched — only the stray
// space around the stripped token is removed. The caller does a final TrimSpace.
func stripMarker(text, marker string) string {
	switch {
	case strings.Contains(text, marker+" "):
		return strings.ReplaceAll(text, marker+" ", "")
	case strings.Contains(text, " "+marker):
		return strings.ReplaceAll(text, " "+marker, "")
	default:
		return strings.ReplaceAll(text, marker, "")
	}
}
