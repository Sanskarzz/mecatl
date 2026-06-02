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

	"github.com/stacklok/mecatl/internal/session"
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
	//
	// workdir is the absolute working directory the command runs in — the
	// session/fork Workspace root the tool executes against, so a SINGLE runner
	// can serve both the main session (rooted at the configured workspace) and a
	// forked child (rooted at an isolated temp base OUTSIDE that configured root).
	// Implementations MUST honor a workdir outside their configured root and MUST
	// NOT confine/reject it — fork isolation depends on this. An EMPTY workdir
	// falls back to the runner's own configured root, so a runner can still be
	// used standalone.
	Run(ctx context.Context, command, workdir string) (CommandResult, error)
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

// MemoryEntry is a single cross-session memory record: an opaque key, its stored
// value, an optional one-line description, and the wall-clock time it was last
// written. It is the unit returned by MemoryStore.Recall, MemoryStore.List and
// MemoryStore.Index.
type MemoryEntry struct {
	// Key is the opaque lookup key (e.g. "pref/test-runner").
	Key string
	// Value is the stored text. The store treats it as opaque bytes. It is
	// EMPTY in MemoryStore.Index results, which omit values by design (the
	// tier-0 index carries only the routing table, not the payload).
	Value string
	// Description is an optional one-line summary used as the tier-0 index hook.
	// When empty on write, the store derives one from the value's first line; so
	// Index results always carry a non-empty Description even for entries that
	// were stored without one.
	Description string
	// UpdatedAt is the wall-clock time the entry was last written.
	UpdatedAt time.Time
}

// MemoryStore is the seam for conservative, cross-session ("tiered") memory
// (harness pattern 3). It is defined here, alongside Workspace and CommandRunner,
// for the same layering reason: the memory tools depend on it the way the Bash
// tool depends on CommandRunner, and keeping the interface in internal/tool
// avoids the port↔tool import cycle a separate package would risk.
//
// SCOPING: a MemoryStore is scoped per-PROJECT — the composition root constructs
// one store instance per workspace/project directory, so entries written in one
// session are visible to later sessions over the SAME project dir and are NOT
// shared across unrelated projects. Implementations must be safe for concurrent
// use and durable across process restarts.
type MemoryStore interface {
	// RememberEntry stores e, overwriting any existing entry under e.Key and
	// bumping its UpdatedAt. e.Description is the optional one-line tier-0 hook;
	// an empty description means "derive from the value's first line on Index".
	// An empty key is rejected. RememberEntry is the full-fidelity write;
	// Remember is a convenience wrapper over it.
	RememberEntry(ctx context.Context, e MemoryEntry) error
	// Remember stores value under key with no explicit description (the index
	// derives one from the value), overwriting any existing entry and bumping its
	// UpdatedAt. An empty key is rejected. It is a convenience wrapper over
	// RememberEntry, kept so callers that do not care about descriptions stay
	// unchanged.
	Remember(ctx context.Context, key, value string) error
	// Recall returns the entry for the exact key. The boolean reports whether an
	// entry was found; a miss is (zero, false, nil), not an error.
	Recall(ctx context.Context, key string) (MemoryEntry, bool, error)
	// List returns all entries whose key has the given prefix, sorted by key for
	// deterministic output. An empty prefix returns every entry.
	List(ctx context.Context, prefix string) ([]MemoryEntry, error)
	// Forget deletes the entry for key. Deleting a missing key is not an error.
	Forget(ctx context.Context, key string) error
	// Index returns the tier-0 routing table: every entry as (key, description,
	// updated-at) with the VALUE OMITTED, sorted by key for deterministic output.
	// The store fills Description (explicit, else derived from the value's first
	// line) but does NOT apply the tier-0 size cap — capping/rendering is the
	// consumer's concern. It is the cheap, always-in-context summary view that
	// lets the model see what it has stored without loading every value.
	Index(ctx context.Context) ([]MemoryEntry, error)
	// Search ranks entries by lexical relevance to query (a local, dependency-free
	// BM25 over each entry's key + derived description + value) and returns the top
	// k matches best-first. Like Index, results carry (key, description, updated-at)
	// with the VALUE OMITTED — the value participates in scoring but is never
	// returned; callers Recall a key to load it. Results are deterministically
	// ordered (score descending, then key ascending). Entries with no query-term
	// overlap (zero score) are dropped. An empty or whitespace-only query yields an
	// empty slice, NOT an error. k <= 0 selects the store's default page size.
	Search(ctx context.Context, query string, k int) ([]MemoryEntry, error)
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
