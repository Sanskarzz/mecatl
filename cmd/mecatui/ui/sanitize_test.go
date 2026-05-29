package ui

import (
	"strings"
	"testing"
)

// TestSanitizeTerminal asserts control bytes and ESC/DEL are stripped while
// printable text plus \n and \t survive.
func TestSanitizeTerminal(t *testing.T) {
	cases := []struct {
		name string
		in   string
		want string
	}{
		{"plain", "hello world", "hello world"},
		{"keeps newline and tab", "a\nb\tc", "a\nb\tc"},
		{"strips ESC sequence", "x\x1b[2Jy", "x[2Jy"},
		{"strips OSC title set", "\x1b]0;pwned\x07ok", "]0;pwnedok"},
		{"strips bare ESC", "a\x1bb", "ab"},
		{"strips DEL", "a\x7fb", "ab"},
		{"strips C0 controls", "a\x00\x01\x02\rb", "ab"},
		{"empty", "", ""},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if got := sanitizeTerminal(tc.in); got != tc.want {
				t.Errorf("sanitizeTerminal(%q) = %q, want %q", tc.in, got, tc.want)
			}
			if strings.ContainsRune(sanitizeTerminal(tc.in), 0x1b) {
				t.Errorf("sanitizeTerminal(%q) still contains ESC", tc.in)
			}
		})
	}
}
