// Package tools implements the core model-facing tools of the mecatl kit —
// Read, Edit, Write, Grep, Glob, a WebFetch stub, and an OPTIONAL Bash tool — as
// tool.Tool values executing against an injected tool.Workspace.
//
// All() and Register() cover the six always-available tools that need only a
// Workspace. Bash is special: it needs a tool.CommandRunner and is therefore not
// part of All(); construct it explicitly with NewBashTool(runner) and register it
// only when a runner is configured. A deployment with no shell simply omits it.
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

	"github.com/stacklok/mecatl/internal/adapter/toolkit"
	"github.com/stacklok/mecatl/internal/session"
	"github.com/stacklok/mecatl/internal/tool"
)

// Output-shaping limits shared across the tools. These keep a single tool
// result from blowing the model's context window; each tool appends a clear
// truncation marker when it trims output. The byte cap lives in toolkit as
// toolkit.MaxOutputBytes (the single shared definition).
const (
	// maxReadLines caps how many lines the Read tool returns in one call.
	maxReadLines = 2000
	// maxGrepMatches caps how many Grep hits are returned in one call.
	maxGrepMatches = 200
	// maxGlobResults caps how many paths Glob returns in one call.
	maxGlobResults = 1000
)

// All returns the always-available core tools as a fresh slice, ready for
// registration in the composition root. The order is the canonical catalog
// order. Bash is NOT included: it requires a tool.CommandRunner and is optional
// — add it separately via NewBashTool when a runner is configured.
func All() []tool.Tool {
	return []tool.Tool{
		ReadTool{},
		EditTool{},
		WriteTool{},
		GrepTool{},
		GlobTool{},
		WebFetchTool{},
	}
}

// Register adds the always-available core tools (everything in All(), i.e. NOT
// Bash) to cat. It returns the first registration error (e.g. a name collision)
// encountered, or nil on success. To enable command execution, additionally
// register NewBashTool(runner), e.g.
// cat.MustRegister(tools.NewBashTool(runner)).
func Register(cat *tool.Catalog) error {
	for _, t := range All() {
		if err := cat.Register(t); err != nil {
			return err
		}
	}
	return nil
}

// parseArgs unmarshals a tool call's JSON arguments into dst, delegating to the
// shared toolkit helper. It returns a model-facing error string (not a Go
// error) describing a malformed payload.
func parseArgs(in session.ToolCall, dst any) (string, bool) {
	return toolkit.ParseArgs(in, dst)
}

// truncateBytes trims s to at most toolkit.MaxOutputBytes on a rune boundary,
// appending a marker when it does. It delegates to the shared toolkit helper.
func truncateBytes(s string) string {
	return toolkit.Truncate(s, toolkit.MaxOutputBytes)
}

// schema wraps a static JSON-schema literal as json.RawMessage for a ToolSpec,
// delegating to the shared toolkit helper.
func schema(s string) json.RawMessage { return toolkit.Schema(s) }
