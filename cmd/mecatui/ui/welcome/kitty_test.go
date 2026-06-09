package welcome

import (
	"fmt"
	"strings"
	"testing"

	"github.com/charmbracelet/x/ansi/kitty"
)

// mapLookup adapts a map to the envLookup signature.
func mapLookup(m map[string]string) envLookup {
	return func(k string) (string, bool) {
		v, ok := m[k]
		return v, ok
	}
}

func mapEnviron(m map[string]string) func() []string {
	return func() []string {
		out := make([]string, 0, len(m))
		for k, v := range m {
			out = append(out, k+"="+v)
		}
		return out
	}
}

// TestDetectKittyTruthTable exercises the conservative env-based detection plus
// the force/no override envs.
func TestDetectKittyTruthTable(t *testing.T) {
	cases := []struct {
		name string
		env  map[string]string
		want bool
	}{
		{"empty", map[string]string{}, false},
		{"kitty window id", map[string]string{"KITTY_WINDOW_ID": "1"}, true},
		{"TERM kitty", map[string]string{"TERM": "xterm-kitty"}, true},
		{"ghostty term_program", map[string]string{"TERM_PROGRAM": "ghostty"}, true},
		{"wezterm term_program", map[string]string{"TERM_PROGRAM": "WezTerm"}, true},
		{"konsole", map[string]string{"KONSOLE_VERSION": "220400"}, true},
		{"ghostty env", map[string]string{"GHOSTTY_RESOURCES_DIR": "/x"}, true},
		{"plain xterm", map[string]string{"TERM": "xterm-256color"}, false},
		{"force on", map[string]string{"MECATUI_FORCE_KITTY": "1"}, true},
		{"no wins over force", map[string]string{"MECATUI_FORCE_KITTY": "1", "MECATUI_NO_KITTY": "1"}, false},
		{"no over detection", map[string]string{"KITTY_WINDOW_ID": "1", "MECATUI_NO_KITTY": "true"}, false},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got := detectKitty(mapLookup(tc.env), mapEnviron(tc.env))
			if got != tc.want {
				t.Errorf("detectKitty(%v) = %v, want %v", tc.env, got, tc.want)
			}
		})
	}
}

// TestTransmitMascotWellFormed asserts the transmit escape is a valid Kitty
// graphics control sequence: it carries the transmit action, PNG format, the
// fixed image ID, virtual placement, and is chunked (multiple m= markers).
func TestTransmitMascotWellFormed(t *testing.T) {
	out := TransmitMascot(40, 20)
	if out == "" {
		t.Fatal("TransmitMascot returned empty")
	}
	// Kitty graphics APC framing.
	if !strings.Contains(out, "\x1b_G") || !strings.Contains(out, "\x1b\\") {
		t.Fatal("transmit escape missing APC G ... ST framing")
	}
	if !strings.Contains(out, "f=100") {
		t.Errorf("transmit missing f=100 (PNG format):\n%q", out[:min(len(out), 120)])
	}
	if !strings.Contains(out, fmt.Sprintf("i=%d", MascotImageID)) {
		t.Error("transmit missing the image ID")
	}
	if !strings.Contains(out, "U=1") {
		t.Error("transmit missing virtual placement (U=1)")
	}
	// Chunked: the first chunk is m=1, the last m=0.
	if !strings.Contains(out, "m=1") || !strings.Contains(out, "m=0") {
		t.Error("transmit not chunked (missing m=1 / m=0 markers)")
	}
}

// TestPlaceholderGridShape asserts the in-content grid is rows×cols placeholder
// cells, each carrying the row+column diacritics and the ID-bearing foreground,
// with the requested left margin and a per-row reset.
func TestPlaceholderGridShape(t *testing.T) {
	const cols, rows, margin = 8, 4, 4
	out := PlaceholderGrid(cols, rows, margin)
	lines := strings.Split(strings.TrimRight(out, "\n"), "\n")
	if len(lines) != rows {
		t.Fatalf("placeholder grid rows = %d, want %d", len(lines), rows)
	}
	ph := string(kitty.Placeholder)
	idFG := idForeground(MascotImageID)
	for r, ln := range lines {
		if !strings.HasPrefix(ln, strings.Repeat(" ", margin)) {
			t.Errorf("row %d missing %d-space margin", r, margin)
		}
		if !strings.Contains(ln, idFG) {
			t.Errorf("row %d missing the ID-bearing foreground escape", r)
		}
		if got := strings.Count(ln, ph); got != cols {
			t.Errorf("row %d placeholder count = %d, want %d (square footprint)", r, got, cols)
		}
		// The row diacritic for this row must appear; column 0's diacritic too.
		if !strings.Contains(ln, string(kitty.Diacritic(r))) {
			t.Errorf("row %d missing its row diacritic", r)
		}
		if !strings.HasSuffix(ln, "\x1b[0m") {
			t.Errorf("row %d missing trailing reset", r)
		}
	}
}

// TestPlaceholderWidthIsOne is the spec assumption probe: U+10EEEE and a
// placeholder+diacritics grapheme cluster are width-1, so the grid lays out as a
// cols-wide block (the renderer/runewidth treats it as a normal-width cell).
func TestPlaceholderWidthIsOne(t *testing.T) {
	if w := kittyPlaceholderWidth(); w != 1 {
		t.Fatalf("placeholder cluster width = %d, want 1 (layout assumption broken)", w)
	}
}

// TestDeleteMascotWellFormed asserts the delete escape targets the mascot id.
func TestDeleteMascotWellFormed(t *testing.T) {
	out := DeleteMascot()
	if !strings.Contains(out, "\x1b_G") {
		t.Fatal("delete escape missing APC G framing")
	}
	if !strings.Contains(out, "a=d") {
		t.Error("delete escape missing a=d (delete action)")
	}
	if !strings.Contains(out, fmt.Sprintf("i=%d", MascotImageID)) {
		t.Error("delete escape missing the image ID")
	}
}
