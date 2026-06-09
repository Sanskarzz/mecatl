package welcome

import (
	"bytes"
	"fmt"
	"os"
	"strings"

	"github.com/charmbracelet/x/ansi"
	"github.com/charmbracelet/x/ansi/kitty"
)

// kittyPlaceholderWidth returns the display width of one placeholder cell
// (placeholder rune + a row + a column diacritic). The Kitty spec says the
// placeholder is normal width-1; this verifies the renderer/runewidth agrees, so
// the placeholder grid lays out as a cols-wide block (a guard test reads it).
func kittyPlaceholderWidth() int {
	cluster := string(kitty.Placeholder) + string(kitty.Diacritic(0)) + string(kitty.Diacritic(0))
	return ansi.StringWidth(cluster)
}

// MascotImageID is the fixed Kitty graphics image ID the splash transmits the
// mascot under and references from the placeholder grid. A constant (not a
// per-run counter) is fine: the splash transmits at most one image, and a stable
// id lets a re-transmit on a size-tier change replace the same slot rather than
// leak placements.
const MascotImageID = 0x6D63 // 'm','c' — arbitrary but stable

// KittyCapable is the exported entry point for the ui caller: it reports whether
// the terminal is likely Kitty-graphics-capable (see kittyCapable). Conservative
// and env-based; a miss falls back to the always-correct half-block path.
func KittyCapable() bool { return kittyCapable() }

// kittyCapable reports whether the terminal is likely to support the Kitty
// graphics protocol with Unicode placeholders. It is deliberately CONSERVATIVE
// and ENV-BASED (no terminal round-trip): a miss falls back to the half-block
// path, which is always correct, so a false negative only costs resolution, never
// correctness. A false positive would paint nothing (the placeholder cells render
// as blanks), which is also non-fatal.
//
// Two override envs gate testing and user control:
//   - MECATUI_FORCE_KITTY=1 forces capable (true) regardless of detection.
//   - MECATUI_NO_KITTY=1 forces incapable (false) and WINS over force.
//
// Detection (any one is sufficient): KITTY_WINDOW_ID set (kitty), TERM contains
// "kitty", TERM_PROGRAM in {ghostty, WezTerm}, any GHOSTTY_* env present, or
// KONSOLE_VERSION set (Konsole's Kitty support).
func kittyCapable() bool {
	return detectKitty(osEnvLookup, osEnviron)
}

// envLookup / environLister abstract the environment for testability — production
// passes the os-backed pair, tests pass maps.
type envLookup func(key string) (string, bool)

func osEnvLookup(key string) (string, bool) { return os.LookupEnv(key) }

func osEnviron() []string { return os.Environ() }

// detectKitty is the pure core of kittyCapable, taking its environment as
// injectable functions so the truth table is unit-testable without mutating the
// process environment.
func detectKitty(look envLookup, environ func() []string) bool {
	if v, ok := look("MECATUI_NO_KITTY"); ok && truthy(v) {
		return false // explicit opt-out wins over everything.
	}
	if v, ok := look("MECATUI_FORCE_KITTY"); ok && truthy(v) {
		return true
	}
	if _, ok := look("KITTY_WINDOW_ID"); ok {
		return true
	}
	if term, ok := look("TERM"); ok && strings.Contains(strings.ToLower(term), "kitty") {
		return true
	}
	switch tp, _ := look("TERM_PROGRAM"); tp {
	case "ghostty", "Ghostty", "WezTerm", "wezterm":
		return true
	}
	if _, ok := look("KONSOLE_VERSION"); ok {
		return true
	}
	// Any GHOSTTY_* env present (Ghostty exports several, e.g. GHOSTTY_RESOURCES_DIR)
	// is a strong signal even when TERM_PROGRAM is unset (tmux strips it).
	for _, kv := range environ() {
		if strings.HasPrefix(kv, "GHOSTTY_") {
			return true
		}
	}
	return false
}

// truthy reports whether an env value means "on".
func truthy(v string) bool {
	switch strings.ToLower(strings.TrimSpace(v)) {
	case "1", "true", "yes", "on":
		return true
	}
	return false
}

// TransmitMascot builds the OUT-OF-BAND Kitty escape that transmits the mascot
// image data to the terminal AND registers a virtual placement (U=1) under
// MascotImageID at cols×rows cells. It is a control sequence: it MUST be written
// via tea.Raw (NOT placed in View content, where the ultraviolet renderer would
// parse it into cells and desync the cursor). A transmit + virtual-placement
// escape produces no visible output and no cursor movement, so interleaving it
// with frames is harmless.
//
// The full-resolution mascot is sent once as PNG (the embedded image is decoded
// then re-encoded to PNG by kitty.EncodeGraphics under f=100/Transmission=Direct —
// equivalent bytes, no temp file, no os/exec); the terminal scales it into the
// cols×rows placement cell area, so the same transmit serves every layout (only the
// placeholder grid in content changes with the tier). The data is chunked at
// kitty.MaxChunkSize. Returns "" if the image can't be decoded (caller then keeps
// the half-block path).
func TransmitMascot(cols, rows int) string {
	if cols <= 0 || rows <= 0 {
		return ""
	}
	if len(rawMascotPNG()) == 0 {
		return ""
	}
	img, err := DecodeMascot()
	if err != nil {
		return ""
	}
	var buf bytes.Buffer
	// f=100 (PNG), a=t (transmit), i=ID, virtual placement (U=1), c=cols r=rows so the
	// terminal scales the image into the placeholder grid footprint. Transmission is
	// Direct (the image is re-encoded to PNG and rides the escape, base64+chunked) — no
	// temp file, no os/exec.
	opts := &kitty.Options{
		Action:           kitty.Transmit,
		Format:           kitty.PNG,
		Transmission:     kitty.Direct,
		ID:               MascotImageID,
		VirtualPlacement: true,
		Columns:          cols,
		Rows:             rows,
		Chunk:            true,
	}
	if err := kitty.EncodeGraphics(&buf, img, opts); err != nil {
		return ""
	}
	return buf.String()
}

// DeleteMascot builds the escape that deletes the mascot image (a=d, by id) so the
// terminal frees the held image once the splash leaves. The ui reducer fires it on
// the empty→non-empty (splash→first-block) transition, via tea.Raw like the
// transmit. The image would stop PAINTING regardless once the placeholder cells
// leave the content (Unicode placeholders only paint where their cells are), so
// this is about freeing the terminal-side resource and keeping the
// transmit/delete lifecycle symmetric, not about hiding a lingering image.
func DeleteMascot() string {
	return ansi.KittyGraphics(nil,
		fmt.Sprintf("a=%c", kitty.Delete),
		fmt.Sprintf("d=%c", kitty.DeleteID),
		fmt.Sprintf("i=%d", MascotImageID),
	)
}

// PlaceholderGrid builds the IN-CONTENT placeholder cell grid that paints the
// already-transmitted virtual image. It is rows×cols cells of kitty.Placeholder
// (U+10EEEE, a width-1 rune — verified), each cell carrying:
//   - the image ID in its FOREGROUND colour (the Kitty Unicode-placeholder spec
//     encodes the 24-bit image id as an RGB foreground: r=id>>16, g=id>>8, b=id),
//   - a ROW diacritic and a COLUMN diacritic (kitty.Diacritic(row)/(col)) so the
//     terminal knows which image cell each placeholder maps to.
//
// margin spaces indent each row to match the half-block path's placement, so the
// splash layout is identical whichever mascot path is active (same cols×rows
// footprint). The grid is what welcome.Splash emits INSTEAD of the half-block
// mascot when kitty is active.
func PlaceholderGrid(cols, rows, margin int) string {
	if cols <= 0 || rows <= 0 {
		return ""
	}
	fg := idForeground(MascotImageID)
	pad := strings.Repeat(" ", margin)
	ph := string(kitty.Placeholder)
	var b strings.Builder
	for r := 0; r < rows; r++ {
		b.WriteString(pad)
		// One SGR run per row (the id fg is constant across the row); each cell is the
		// placeholder rune followed by its row+column diacritics.
		b.WriteString(fg)
		rowDia := string(kitty.Diacritic(r))
		for c := 0; c < cols; c++ {
			b.WriteString(ph)
			b.WriteString(rowDia)
			b.WriteString(string(kitty.Diacritic(c)))
		}
		b.WriteString("\x1b[0m\n")
	}
	return b.String()
}

// idForeground returns the SGR truecolor foreground escape that encodes a 24-bit
// Kitty image id, per the Unicode-placeholder spec (the placeholder cell's
// foreground colour IS the image id). Only the low 24 bits are used.
func idForeground(id int) string {
	r := (id >> 16) & 0xFF
	g := (id >> 8) & 0xFF
	bl := id & 0xFF
	return fmt.Sprintf("\x1b[38;2;%d;%d;%dm", r, g, bl)
}
