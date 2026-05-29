// Package toolkit holds the small, tool-agnostic helpers shared by the
// adapter-layer tool packages (internal/adapter/tools and
// internal/adapter/memory). It exists to keep a single source of truth for the
// cross-cutting mechanics every tool repeats — argument parsing, output
// truncation, and the JSON-schema literal wrapper — so they cannot drift apart.
//
// In particular the output cap lives here as the single MaxOutputBytes constant:
// previously each package carried its own 25_000 literal, a latent drift bug.
//
// What deliberately does NOT belong here: per-tool argument validation and the
// per-tool descriptions/schemas. Those are intentionally authored inline in each
// tool because they are the tool's own contract, not shared mechanics.
package toolkit

import (
	"encoding/json"
	"fmt"

	"github.com/stacklok/mecatl/internal/session"
)

// MaxOutputBytes caps the byte length of a single tool's textual result. It is
// the one shared output cap for the adapter layer; tools append a truncation
// marker (see Truncate) when they trim to it. Keeping it here prevents the cap
// from drifting between tool packages.
const MaxOutputBytes = 25_000

// ParseArgs unmarshals a tool call's JSON arguments into dst. An empty payload
// is treated as an empty object so tools with all-optional arguments work
// without an explicit "{}". On malformed JSON it returns a model-facing error
// string (not a Go error) and false; on success it returns "" and true.
func ParseArgs(in session.ToolCall, dst any) (string, bool) {
	raw := in.Args
	if len(raw) == 0 {
		raw = json.RawMessage("{}")
	}
	if err := json.Unmarshal(raw, dst); err != nil {
		return fmt.Sprintf("invalid arguments: %v", err), false
	}
	return "", true
}

// Schema wraps a static JSON-schema literal as json.RawMessage for a ToolSpec.
// The literals are authored by hand and are valid JSON; this is just a typed
// convenience for the Spec methods.
func Schema(s string) json.RawMessage { return json.RawMessage(s) }

// Truncate trims s to at most maxBytes, appending a clear truncation marker
// when it does. It cuts on a rune boundary so the result is never invalid UTF-8.
// Most callers pass MaxOutputBytes.
func Truncate(s string, maxBytes int) string {
	if len(s) <= maxBytes {
		return s
	}
	cut := maxBytes
	for cut > 0 && !utf8RuneStart(s[cut]) {
		cut--
	}
	return s[:cut] + "\n... [output truncated: exceeded 25000 bytes]"
}

// utf8RuneStart reports whether b is the first byte of a UTF-8 rune (i.e. not a
// continuation byte 0b10xxxxxx).
func utf8RuneStart(b byte) bool { return b&0xC0 != 0x80 }
