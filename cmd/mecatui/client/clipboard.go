package client

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"net/http"
	"os"
	"os/exec"
	"strings"
	"time"
)

// Clipboard reads the OS clipboard for ctrl+v paste. Read returns the clipboard's
// content as a (mime, data) pair: an image/* mime when the clipboard holds an
// image (which the UI stages as an inline media part), or a text mime (text/plain)
// when it holds text (which the UI inserts into the textarea). The mime tells the
// caller which branch to take. A nil Clipboard on Deps cleanly disables ctrl+v
// (the same convention as a nil MCP/Cmds collaborator). The UI imports client, so
// the interface + its sentinels live here, not in the ui package.
type Clipboard interface {
	Read(ctx context.Context) (mime string, data []byte, err error)
}

var (
	// ErrNoClipboardTool reports that NO backend clipboard binary exists (no
	// wl-paste / xclip / pbpaste / pngpaste / powershell). The UI surfaces an
	// actionable "install wl-clipboard / xclip" hint rather than a generic error.
	ErrNoClipboardTool = errors.New("no clipboard tool available")
	// ErrEmptyClipboard reports that a backend exists but the clipboard holds
	// neither a usable image nor any text. The UI shows a benign "clipboard is
	// empty" status (no transcript error).
	ErrEmptyClipboard = errors.New("clipboard is empty")
)

// clipboardTimeout bounds each clipboard subprocess so a wedged backend (e.g. a
// hung wl-paste) can never block the UI's command goroutine indefinitely.
const clipboardTimeout = 3 * time.Second

// shellClipboard is the production Clipboard: it shells out to the platform's
// clipboard binary. os/exec is allowed in the client package (it already does
// os/net/http in media.go) but FORBIDDEN in the ui/domain/port layers — hence
// this lives here behind the proto-free Clipboard interface the ui consumes. The
// runner/lookPath/getenv funcs are INJECTED so tests drive it fully offline with
// canned image AND text bytes, never touching a real clipboard or subprocess.
type shellClipboard struct {
	run      func(ctx context.Context, name string, args ...string) ([]byte, error)
	lookPath func(string) (string, error)
	getenv   func(string) string
	timeout  time.Duration
}

// NewClipboard wires the real backend: exec.CommandContext(...).Output(), the real
// PATH lookup + environment, and the 3s timeout.
func NewClipboard() Clipboard {
	return &shellClipboard{
		run: func(ctx context.Context, name string, args ...string) ([]byte, error) {
			return exec.CommandContext(ctx, name, args...).Output() //nolint:gosec // fixed backend binary names selected by capability probe; args are constant.
		},
		lookPath: exec.LookPath,
		getenv:   os.Getenv,
		timeout:  clipboardTimeout,
	}
}

// backend names the resolved clipboard backend and the exact argv to (a) list the
// clipboard's available types, (b) fetch a specific image type, and (c) fetch the
// clipboard text. A nil listImage/fetchImage means "this backend has no
// list-then-fetch image path" (macOS pngpaste fetches unconditionally); imageArgs
// is then the unconditional image fetch.
type backend struct {
	name string
	// listTypesArgs lists the MIME/target types currently on the clipboard. Empty
	// when the backend can't enumerate (macOS); the caller then tries imageArgs
	// directly and treats a non-image / error as "no image".
	listTypesArgs []string
	// imageArgs fetches the clipboard image bytes (PNG-preferred).
	imageArgs []string
	// textArgs fetches the clipboard text.
	textArgs []string
}

// selectBackend probes the environment + PATH for a usable clipboard backend, in
// the documented precedence: Wayland (wl-paste) → X11 (xclip) → macOS
// (pngpaste/pbpaste) → Windows (powershell). It returns ErrNoClipboardTool when no
// backend binary exists.
func (c *shellClipboard) selectBackend() (backend, error) {
	has := func(bin string) bool {
		_, err := c.lookPath(bin)
		return err == nil
	}
	switch {
	case c.getenv("WAYLAND_DISPLAY") != "" && has("wl-paste"):
		return backend{
			name:          "wl-paste",
			listTypesArgs: []string{"wl-paste", "--list-types"},
			imageArgs:     []string{"wl-paste", "--no-newline", "--type", "image/png"},
			textArgs:      []string{"wl-paste", "--no-newline"},
		}, nil
	case c.getenv("DISPLAY") != "" && has("xclip"):
		return backend{
			name:          "xclip",
			listTypesArgs: []string{"xclip", "-selection", "clipboard", "-t", "TARGETS", "-o"},
			imageArgs:     []string{"xclip", "-selection", "clipboard", "-t", "image/png", "-o"},
			textArgs:      []string{"xclip", "-selection", "clipboard", "-o"},
		}, nil
	case has("pngpaste") || has("pbpaste"):
		// macOS. pngpaste fetches a clipboard image to stdout ("-"); it has no
		// type-listing, so listTypesArgs is empty and a non-image clipboard makes
		// pngpaste fail/empty (treated as "no image"). pbpaste fetches the text.
		// NOTE: pngpaste reads the «class PNGf» pasteboard flavour; Chromium/Electron
		// apps copy images as the "public.png" flavour, which pngpaste MISSES — so a
		// screenshot copied from Chrome may not paste as an image (it falls back to
		// text/empty). This is a known macOS gap with no shell-only fix.
		b := backend{name: "pbpaste", textArgs: []string{"pbpaste"}}
		if has("pngpaste") {
			b.imageArgs = []string{"pngpaste", "-"}
		}
		return b, nil
	case has("powershell"):
		// Windows. Get-Clipboard yields text; GetImage() + the PNG stream yields the
		// image bytes (base64-free via Console.OpenStandardOutput). argv stays direct
		// (-Command with a constant script), never `sh -c`.
		return backend{
			name:      "powershell",
			imageArgs: []string{"powershell", "-NoProfile", "-Command", winImageScript},
			textArgs:  []string{"powershell", "-NoProfile", "-Command", "Get-Clipboard -Raw"},
		}, nil
	default:
		return backend{}, ErrNoClipboardTool
	}
}

// winImageScript fetches a clipboard image as raw PNG bytes on stdout. Kept as a
// constant so the argv stays direct (no `sh -c`); emits nothing when no image.
const winImageScript = `Add-Type -AssemblyName System.Windows.Forms; ` +
	`$img = [System.Windows.Forms.Clipboard]::GetImage(); ` +
	`if ($img -ne $null) { $ms = New-Object System.IO.MemoryStream; ` +
	`$img.Save($ms, [System.Drawing.Imaging.ImageFormat]::Png); ` +
	`$out = [System.Console]::OpenStandardOutput(); $ms.WriteTo($out); $out.Flush() }`

// Read implements Clipboard. It tries an image first (per the spec's image-first,
// text-fallback contract): if the clipboard holds an image type it fetches and
// returns it as image/png; otherwise it fetches the clipboard text and returns it
// as text/plain. A genuinely empty clipboard (no image, no text) is
// ErrEmptyClipboard; no backend at all is ErrNoClipboardTool.
func (c *shellClipboard) Read(ctx context.Context) (string, []byte, error) {
	b, err := c.selectBackend()
	if err != nil {
		return "", nil, err
	}
	if to := c.timeout; to > 0 {
		var cancel context.CancelFunc
		ctx, cancel = context.WithTimeout(ctx, to)
		defer cancel()
	}

	if mime, data, ok, err := c.tryImage(ctx, b); err != nil {
		return "", nil, err
	} else if ok {
		return mime, data, nil
	}

	// No image on the clipboard: fall back to text.
	if len(b.textArgs) > 0 {
		text, err := c.run(ctx, b.textArgs[0], b.textArgs[1:]...)
		if err == nil && len(text) > 0 {
			return "text/plain", text, nil
		}
	}
	return "", nil, ErrEmptyClipboard
}

// tryImage attempts to fetch a clipboard image. ok reports whether an image was
// returned. A backend that can enumerate types (Wayland/X11) is asked first and
// only fetched when an image/* type is present; a backend without enumeration
// (macOS pngpaste) is fetched unconditionally and its output sniffed. An oversize
// image is a hard error (the user pasted something too big — loud, not silent).
func (c *shellClipboard) tryImage(ctx context.Context, b backend) (string, []byte, bool, error) {
	if len(b.imageArgs) == 0 {
		return "", nil, false, nil // backend has no image path (e.g. pbpaste-only macOS)
	}
	if len(b.listTypesArgs) > 0 {
		types, err := c.run(ctx, b.listTypesArgs[0], b.listTypesArgs[1:]...)
		if err != nil || !hasImageType(types) {
			return "", nil, false, nil // can't list, or no image type → fall back to text
		}
	}
	data, err := c.run(ctx, b.imageArgs[0], b.imageArgs[1:]...)
	if err != nil || len(data) == 0 {
		return "", nil, false, nil // fetch failed / empty → treat as no image
	}
	// Defensive re-sniff: keep only genuine image bytes (a backend that emitted
	// non-image bytes when we expected an image falls through to text).
	mime := http.DetectContentType(data[:min(sniffLen, len(data))])
	if !strings.HasPrefix(mime, "image/") {
		return "", nil, false, nil
	}
	// Trade-off: exec.Output() buffers ALL of the backend's stdout before this cap
	// check, so a pathological clipboard image is fully read into RAM before being
	// rejected here. Accepted for a local, single-user TUI: the read is bounded by
	// the 3s context timeout AND this size cap, and the injected-runner seam (which
	// the tests depend on) is intentionally NOT restructured to stream+truncate.
	if len(data) > maxMediaBytes {
		return "", nil, false, fmt.Errorf("clipboard image is %d bytes, over the %d-byte limit", len(data), maxMediaBytes)
	}
	// Normalise to image/png defensively (the fetch requested PNG); the server
	// re-validates the kind regardless.
	return "image/png", data, true, nil
}

// hasImageType reports whether a wl-paste/xclip type listing (one type per line)
// advertises any image/* type.
func hasImageType(listing []byte) bool {
	for _, line := range strings.Split(string(listing), "\n") {
		if strings.HasPrefix(strings.TrimSpace(line), "image/") {
			return true
		}
	}
	return bytes.Contains(listing, []byte("image/")) // defensive: space-separated TARGETS
}
