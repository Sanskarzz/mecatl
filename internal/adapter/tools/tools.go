// Package tools implements the seven core model-facing tools of the ozzharness
// kit — Read, Edit, Write, Bash, Grep, Glob, and a WebFetch stub — as
// tool.Tool values executing against an injected tool.Workspace.
//
// Each tool parses its session.ToolCall.Args (JSON), runs against the Workspace
// seam, and returns a session.ToolResult. Recoverable, model-addressable
// failures (a missing argument, a failed Edit invariant, a non-existent file)
// are returned as an *error* ToolResult via session.NewToolError so the model
// can read and recover from them; the Go error return is reserved for
// harness-level faults the model cannot act on.
//
// Every tool carries a documentation-quality ToolSpec.Description: the
// description is the model's onboarding manual for the tool (gauntlet #10), so
// it states when to use the tool, when not to, one worked example, and its
// limits. Each tool also reports a correct ReadOnly() value, which drives the
// agent loop's read-parallel / mutate-serial dispatch (gauntlet #4).
package tools

import (
	"encoding/json"
	"fmt"

	"github.com/stacklok/ozzharness/internal/session"
	"github.com/stacklok/ozzharness/internal/tool"
)

// Output-shaping limits shared across the tools. These keep a single tool
// result from blowing the model's context window; each tool appends a clear
// truncation marker when it trims output.
const (
	// maxOutputBytes caps the byte length of a single tool's textual result.
	maxOutputBytes = 25_000
	// maxReadLines caps how many lines the Read tool returns in one call.
	maxReadLines = 2000
	// maxGrepMatches caps how many Grep hits are returned in one call.
	maxGrepMatches = 200
	// maxGlobResults caps how many paths Glob returns in one call.
	maxGlobResults = 1000
)

// All returns the seven core tools as a fresh slice, ready for registration in
// the composition root. The order is the canonical catalog order.
func All() []tool.Tool {
	return []tool.Tool{
		ReadTool{},
		EditTool{},
		WriteTool{},
		BashTool{},
		GrepTool{},
		GlobTool{},
		WebFetchTool{},
	}
}

// Register adds all seven core tools to cat. It returns the first registration
// error (e.g. a name collision) encountered, or nil on success.
func Register(cat *tool.Catalog) error {
	for _, t := range All() {
		if err := cat.Register(t); err != nil {
			return err
		}
	}
	return nil
}

// parseArgs unmarshals a tool call's JSON arguments into dst. An empty payload
// is treated as an empty object so tools with all-optional arguments work
// without an explicit "{}". It returns a model-facing error string (not a Go
// error) describing a malformed payload.
func parseArgs(in session.ToolCall, dst any) (string, bool) {
	raw := in.Args
	if len(raw) == 0 {
		raw = json.RawMessage("{}")
	}
	if err := json.Unmarshal(raw, dst); err != nil {
		return fmt.Sprintf("invalid arguments: %v", err), false
	}
	return "", true
}

// truncateBytes trims s to at most maxOutputBytes, appending a marker when it
// does. It cuts on a rune boundary to avoid emitting invalid UTF-8.
func truncateBytes(s string) string {
	if len(s) <= maxOutputBytes {
		return s
	}
	cut := maxOutputBytes
	for cut > 0 && !utf8RuneStart(s[cut]) {
		cut--
	}
	return s[:cut] + "\n... [output truncated: exceeded 25000 bytes]"
}

// utf8RuneStart reports whether b is the first byte of a UTF-8 rune (i.e. not a
// continuation byte 0b10xxxxxx).
func utf8RuneStart(b byte) bool { return b&0xC0 != 0x80 }

// schema wraps a static JSON-schema literal as json.RawMessage for a ToolSpec.
// The literals are authored by hand and are valid JSON; this is just a typed
// convenience for the Spec methods.
func schema(s string) json.RawMessage { return json.RawMessage(s) }
