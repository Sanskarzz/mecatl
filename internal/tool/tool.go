// Package tool — cycle note: FileSystem and Workspace are defined here, in the
// Tooling context that owns them (ARCHITECTURE.md §2), rather than in
// internal/port. This breaks the port↔tool import cycle that would form because
// port already imports tool (LLMRequest.Tools is []tool.ToolSpec) while
// Tool.Execute takes a Workspace.
package tool

import (
	"context"
	"encoding/json"
	"errors"
	"io/fs"
	"time"

	"github.com/stacklok/ozzharness/internal/session"
)

// ErrNoShell is the sentinel a CommandRunner returns when it has no shell to
// execute against (e.g. the in-memory runner, or a shell-less remote pod). The
// Bash tool surfaces it to the model as a tool-level error rather than aborting
// the harness.
var ErrNoShell = errors.New("tool: no shell available")

// ToolSpec is what the model sees for a tool: its name, a documentation-quality
// description (when to use / when not / example / limits), and the JSON schema
// for its arguments. ToolSpecs are stable across turns so the LLM adapter can
// cache them.
type ToolSpec struct {
	// Name is the tool's catalog name.
	Name string
	// Description is the model-facing documentation for the tool.
	Description string
	// Schema is the JSON schema describing the tool's Args.
	Schema json.RawMessage
}

// Tool is the contract every tool implements. ReadOnly drives the loop's
// read-parallel / mutate-serial dispatch. Execute runs the tool against a
// session-scoped Workspace and returns a domain ToolResult.
type Tool interface {
	// Spec returns the model-facing specification of the tool.
	Spec() ToolSpec
	// ReadOnly reports whether the tool only reads state (so the dispatcher may
	// run it in parallel with other read-only tools) versus mutating state
	// (which must run serially).
	ReadOnly() bool
	// Execute runs the tool. ctx carries cancellation; in is the model's call;
	// ws is the session-scoped filesystem/command seam. It returns a ToolResult
	// (with IsError set on a tool-level failure that should be fed back to the
	// model) and a non-nil error only for harness-level failures.
	Execute(ctx context.Context, in session.ToolCall, ws Workspace) (session.ToolResult, error)
}

// Disclosable is the OPTIONAL capability a Tool MAY implement to participate in
// progressive tool disclosure (pattern 9). A disclosable tool advertises a
// lightweight, metadata-only ToolSpec (typically name + a one-line description,
// with no or an empty Schema) until the model hydrates the full Spec() on demand
// via the ToolSearch tool. A tool that does NOT implement Disclosable is always
// advertised with its full Spec(), so the default catalog view is unchanged.
type Disclosable interface {
	Tool
	// Advertised returns the cheap, metadata-only spec rendered into the per-turn
	// tool inventory under progressive disclosure. Spec() remains the full,
	// hydrate-on-demand specification.
	Advertised() ToolSpec
}

// FileInfo is the minimal, provider-neutral file metadata the tools need. It is
// a subset of io/fs.FileInfo carried as plain fields so adapters (osfs, memfs)
// can populate it without leaking os types into the domain.
type FileInfo struct {
	// Name is the base name of the file.
	Name string
	// Size is the length in bytes.
	Size int64
	// Mode is the file mode bits.
	Mode fs.FileMode
	// ModTime is the last-modification time.
	ModTime time.Time
	// IsDir reports whether the entry is a directory.
	IsDir bool
}

// CommandRunner executes a shell command. Implementations may run it locally
// (/bin/sh), in a remote environment, or refuse it (no shell available). The
// agent loop never references this type — only the Bash tool depends on it,
// which is what makes the Bash tool (and therefore any command execution)
// optional in the catalog.
type CommandRunner interface {
	// Run executes command and returns its result. A non-zero exit is reported
	// via CommandResult.ExitCode (not error); error is for execution faults
	// (cancellation, timeout, or a missing shell — see ErrNoShell).
	Run(ctx context.Context, command string) (CommandResult, error)
}

// CommandResult is the outcome of a CommandRunner.Run invocation.
type CommandResult struct {
	// Stdout is the captured standard output (already truncated by the adapter).
	Stdout string
	// Stderr is the captured standard error (already truncated by the adapter).
	Stderr string
	// ExitCode is the process exit status.
	ExitCode int
}

// FileSystem is the low-level, path-oriented filesystem seam. Adapters implement
// it over the real OS (osfs) and over memory (memfs). Paths are interpreted by
// the adapter; the session-scoping and the Edit read-ledger live in Workspace,
// which composes a FileSystem.
type FileSystem interface {
	// Read returns the entire contents of the file at path.
	Read(ctx context.Context, path string) ([]byte, error)
	// Write replaces the contents of the file at path, creating it if needed.
	Write(ctx context.Context, path string, data []byte) error
	// Stat returns metadata for the file at path.
	Stat(ctx context.Context, path string) (FileInfo, error)
	// Glob returns the paths matching the shell-style pattern.
	Glob(ctx context.Context, pattern string) ([]string, error)
}

// Workspace is the session-scoped seam every Tool executes against. It scopes
// all paths to a single session root (rejecting escapes such as "../"), exposes
// the read/search/run operations the 7 core tools need, and carries the
// per-session Edit read-ledger that lets the Edit tool enforce its invariants.
//
// All paths are relative to the session root unless documented otherwise;
// adapters must reject any path that resolves outside the root.
type Workspace interface {
	// Root returns the absolute session root all paths are scoped to.
	Root() string

	// Read returns the contents of the file at the session-relative path.
	Read(ctx context.Context, path string) ([]byte, error)
	// Write replaces the contents of the file at the session-relative path,
	// creating it (and parent directories) if needed.
	Write(ctx context.Context, path string, data []byte) error
	// Stat returns metadata for the file at the session-relative path.
	Stat(ctx context.Context, path string) (FileInfo, error)
	// Glob returns session-relative paths matching the shell-style pattern.
	Glob(ctx context.Context, pattern string) ([]string, error)
	// Grep returns the matches of a regular expression across files selected by
	// an optional path glob. Results are capped/shaped by the adapter.
	Grep(ctx context.Context, pattern, pathGlob string) ([]GrepMatch, error)

	// RecordRead marks path as having been read at the given content version so
	// the Edit tool can later assert read-before-edit. version is an opaque
	// fingerprint (e.g. a content hash or mtime) the adapter chooses; the Edit
	// tool treats it as a comparable token, not a meaning-bearing value.
	RecordRead(path string, version string)
	// WasReadUnchanged reports whether path was previously recorded via
	// RecordRead AND its current on-disk version still equals the recorded one.
	// This is the read-before-edit-and-unchanged check the Edit tool's first
	// invariant depends on. It returns false if path was never read or if the
	// file changed since it was read.
	WasReadUnchanged(ctx context.Context, path string) (bool, error)
}

// GrepMatch is a single Workspace.Grep hit.
type GrepMatch struct {
	// Path is the session-relative file the match occurred in.
	Path string
	// Line is the 1-based line number of the match.
	Line int
	// Text is the matching line's content.
	Text string
}
