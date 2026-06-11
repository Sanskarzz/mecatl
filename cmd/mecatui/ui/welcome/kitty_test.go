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

// transmitChunkControls splits a chunked Kitty graphics escape on the APC
// delimiter (ESC \ terminator) and returns each chunk's CONTROL portion (the
// key=value list between "\x1b_G" and the payload-separating ";").
func transmitChunkControls(t *testing.T, out string) []string {
	t.Helper()
	var controls []string
	for _, chunk := range strings.Split(out, "\x1b\\") {
		if chunk == "" {
			continue
		}
		body, ok := strings.CutPrefix(chunk, "\x1b_G")
		if !ok {
			t.Fatalf("chunk missing APC G prefix: %q", chunk[:min(len(chunk), 40)])
		}
		ctrl, _, _ := strings.Cut(body, ";")
		controls = append(controls, ctrl)
	}
	if len(controls) == 0 {
		t.Fatal("no chunks found in transmit escape")
	}
	return controls
}

// controlKeys returns the set of keys (left of '=') in a chunk control string.
func controlKeys(ctrl string) map[string]string {
	keys := map[string]string{}
	for _, kv := range strings.Split(ctrl, ",") {
		if k, v, ok := strings.Cut(kv, "="); ok {
			keys[k] = v
		}
	}
	return keys
}

// TestTransmitMascotCreatesVirtualPlacement is the issue #44 regression pin:
// the FIRST chunk's control set must carry a=T (transmit AND put) together with
// U=1, c=<cols>, r=<rows>, i=<MascotImageID>. Under a bare transmit (a=t) the
// placement keys are inert — the terminal stores the image but creates NO
// virtual placement, and the placeholder grid paints nothing.
func TestTransmitMascotCreatesVirtualPlacement(t *testing.T) {
	const cols, rows = 40, 20
	out := TransmitMascot(cols, rows)
	if out == "" {
		t.Fatal("TransmitMascot returned empty")
	}
	first := controlKeys(transmitChunkControls(t, out)[0])
	if got := first["a"]; got != "T" {
		t.Errorf("first chunk action a=%q, want a=T (transmit-and-put; a=t leaves U=1/c=/r= inert — issue #44)", got)
	}
	if got := first["U"]; got != "1" {
		t.Errorf("first chunk U=%q, want U=1 (virtual placement)", got)
	}
	if got := first["c"]; got != fmt.Sprint(cols) {
		t.Errorf("first chunk c=%q, want c=%d", got, cols)
	}
	if got := first["r"]; got != fmt.Sprint(rows) {
		t.Errorf("first chunk r=%q, want r=%d", got, rows)
	}
	if got := first["i"]; got != fmt.Sprint(MascotImageID) {
		t.Errorf("first chunk i=%q, want i=%d (MascotImageID)", got, MascotImageID)
	}
}

// TestTransmitMascotSuppressesResponses asserts q=2 is present: without quiet
// mode the terminal replies with OK/error APC responses that surface as
// unhandled input msgs in the TUI.
func TestTransmitMascotSuppressesResponses(t *testing.T) {
	out := TransmitMascot(40, 20)
	if out == "" {
		t.Fatal("TransmitMascot returned empty")
	}
	for n, ctrl := range transmitChunkControls(t, out) {
		if got := controlKeys(ctrl)["q"]; got != "2" {
			t.Errorf("chunk %d q=%q, want q=2 (suppress terminal responses)", n, got)
		}
	}
}

// TestTransmitMascotWellFormed asserts the transmit escape is a valid Kitty
// graphics control sequence: it carries the transmit-and-put action, PNG
// format, the fixed image ID, virtual placement, and is chunked (multiple m=
// markers) — with the control keys (a=T, f=, i=, U=, c=, r=) on the FIRST chunk
// only; continuation chunks carry only q= and m=.
func TestTransmitMascotWellFormed(t *testing.T) {
	out := TransmitMascot(40, 20)
	if out == "" {
		t.Fatal("TransmitMascot returned empty")
	}
	// Kitty graphics APC framing.
	if !strings.Contains(out, "\x1b_G") || !strings.Contains(out, "\x1b\\") {
		t.Fatal("transmit escape missing APC G ... ST framing")
	}
	controls := transmitChunkControls(t, out)
	first := controlKeys(controls[0])
	if first["f"] != "100" {
		t.Errorf("transmit missing f=100 (PNG format): %q", controls[0])
	}
	if first["i"] != fmt.Sprint(MascotImageID) {
		t.Error("transmit missing the image ID")
	}
	if first["U"] != "1" {
		t.Error("transmit missing virtual placement (U=1)")
	}
	if first["a"] != "T" {
		t.Error("transmit missing a=T (transmit-and-put)")
	}
	// Chunked: the first chunk is m=1, the last m=0, and the control keys appear
	// on the first chunk ONLY (continuations may carry only q= and m=).
	if len(controls) < 2 {
		t.Fatalf("transmit not chunked: %d chunk(s)", len(controls))
	}
	if first["m"] != "1" {
		t.Errorf("first chunk m=%q, want m=1", first["m"])
	}
	last := controlKeys(controls[len(controls)-1])
	if last["m"] != "0" {
		t.Errorf("last chunk m=%q, want m=0", last["m"])
	}
	for n, ctrl := range controls[1:] {
		keys := controlKeys(ctrl)
		for k := range keys {
			if k != "q" && k != "m" {
				t.Errorf("continuation chunk %d carries control key %s=%s (only q=/m= allowed)", n+1, k, keys[k])
			}
		}
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

// TestPlaceholderForegroundIsTrueColorSGR documents the TrueColor-profile
// dependency: the Unicode-placeholder spec encodes the 24-bit image id as an
// RGB FOREGROUND (38;2;r;g;b with r=id>>16, g=(id>>8)&0xff, b=id&0xff). A
// renderer that downsamples the foreground to 256-color/ANSI would corrupt the
// id and the terminal would paint nothing.
func TestPlaceholderForegroundIsTrueColorSGR(t *testing.T) {
	const id = MascotImageID
	want := fmt.Sprintf("\x1b[38;2;%d;%d;%dm", (id>>16)&0xff, (id>>8)&0xff, id&0xff)
	if got := idForeground(id); got != want {
		t.Errorf("idForeground(%#x) = %q, want %q", id, got, want)
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
	// Uppercase d=I frees the stored image data; lowercase d=i would delete the
	// placement but LEAK the image in the terminal (kitty graphics spec).
	if !strings.Contains(out, "d=I") {
		t.Error("delete escape must use d=I (free image data), not d=i")
	}
}
