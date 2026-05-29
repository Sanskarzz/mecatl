package ui

import "strings"

// sanitizeTerminal strips terminal control bytes from server-derived strings
// before they reach a lipgloss Render (which passes raw bytes through to the
// terminal). Without this, a malicious tool result, tool args, or permission-ask
// field could embed ANSI/OSC escapes to redraw the screen or spoof the approval
// modal (CWE-150 terminal-escape injection).
//
// It removes all C0 control bytes (0x00–0x1F), ESC (0x1B), and DEL (0x7F),
// preserving only newline (\n) and tab (\t) for layout. Assistant markdown is
// rendered through glamour (which neutralises escapes itself) and must NOT be
// passed through here — only the plain-lipgloss server strings are.
func sanitizeTerminal(s string) string {
	if !strings.ContainsFunc(s, isControl) {
		return s
	}
	var b strings.Builder
	b.Grow(len(s))
	for _, r := range s {
		if isControl(r) {
			continue
		}
		b.WriteRune(r)
	}
	return b.String()
}

// isControl reports whether r is a control byte we strip (C0 / ESC / DEL),
// except the layout-preserving \n and \t.
func isControl(r rune) bool {
	switch r {
	case '\n', '\t':
		return false
	case 0x7f:
		return true
	default:
		return r < 0x20
	}
}
