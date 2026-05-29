package memory

import (
	"context"
	"encoding/json"
	"fmt"
	"strings"

	"github.com/stacklok/ozzharness/internal/adapter/toolkit"
	"github.com/stacklok/ozzharness/internal/session"
	"github.com/stacklok/ozzharness/internal/tool"
)

// --- Descriptions ---------------------------------------------------------
//
// The descriptions below are the model's onboarding manual for memory. They
// STEER the model to be conservative: memory is for durable user preferences and
// cross-cutting project facts that the model cannot cheaply rediscover, NOT for
// anything the filesystem already knows. This directly targets the "over-eager
// memory" anti-pattern (docs/harnesses/08-design-considerations.md): "Saving
// facts the file system already knows is the dominant memory failure mode."

// rememberDescription is the model-facing documentation for the Remember tool.
const rememberDescription = `Save a small, durable fact to cross-session project memory so it survives into future sessions.

When to use (be conservative):
- Durable USER PREFERENCES the user stated explicitly, e.g. "always run tests
  with gotestsum", "I prefer table-driven tests", "use conventional commits".
- CROSS-CUTTING PROJECT FACTS that are not obvious from any single file and that
  you cannot rediscover in a few tool calls, e.g. "the staging deploy is gated by
  a manual approval in CI", "issue tracker lives in Linear, not GitHub".

When NOT to use (the over-eager-memory anti-pattern):
- Do NOT save anything the filesystem already knows or that is rediscoverable in
  a few Read/Grep/Glob calls: file locations, function signatures, the build
  command in the Makefile, the module path, dependency versions. Let the code be
  the memory of the code. Saving such facts is the dominant memory failure mode:
  it goes stale and pollutes future context.
- Do NOT save transient task state, secrets, or large blobs.

Behavior:
- Memory is scoped to THIS project. Writing a key overwrites any prior value.
- Use short, namespaced keys, e.g. "pref/test-runner", "project/deploy-gate".

Arguments:
- key   (required): a short, stable, namespaced identifier.
- value (required): the concise fact to remember.

Example:
  {"key": "pref/test-runner", "value": "Run tests with: gotestsum --format dots"}`

// recallDescription is the model-facing documentation for the Recall tool.
const recallDescription = `Look up previously saved cross-session memory by exact key or by key prefix.

When to use:
- Early in a session to retrieve durable user preferences and project facts you
  saved before (e.g. recall the "pref/" prefix to see all stated preferences).
- Before asking the user something they may have already told you in a past
  session.

When NOT to use:
- To discover facts about the current code: use Read, Grep, and Glob instead.
  Memory holds only what was deliberately saved with Remember; it is not an index
  of the repository.

Behavior:
- A key that exactly matches an entry returns that entry's value.
- A key that matches no exact entry is treated as a PREFIX and returns every
  entry whose key starts with it (sorted by key).
- A lookup that finds nothing returns a clear "not found" result, NOT an error.

Arguments:
- key (required): the exact key, or a prefix such as "pref/".

Example:
  {"key": "pref/"}   lists every saved preference.`

// --- Remember tool --------------------------------------------------------

// RememberTool writes a fact to the injected tool.MemoryStore. It is mutating
// (ReadOnly() == false). The store is constructor-injected, mirroring how the
// Bash tool takes a CommandRunner, so command/memory side effects stay optional
// and testable with a fake.
type RememberTool struct {
	store tool.MemoryStore
}

// NewRememberTool constructs the Remember tool bound to store. store must be
// non-nil; the composition root registers this tool only when a store is wired.
func NewRememberTool(store tool.MemoryStore) tool.Tool {
	if store == nil {
		panic("memory: NewRememberTool requires a non-nil MemoryStore")
	}
	return RememberTool{store: store}
}

// Compile-time assertion that RememberTool implements tool.Tool.
var _ tool.Tool = RememberTool{}

// rememberArgs is the JSON argument shape for the Remember tool.
type rememberArgs struct {
	Key   string `json:"key"`
	Value string `json:"value"`
}

// Spec returns the model-facing specification of the Remember tool.
func (RememberTool) Spec() tool.ToolSpec {
	return tool.ToolSpec{
		Name:        "Remember",
		Description: rememberDescription,
		Schema: schema(`{
  "type": "object",
  "properties": {
    "key": {"type": "string", "description": "Short, stable, namespaced key, e.g. \"pref/test-runner\"."},
    "value": {"type": "string", "description": "Concise, durable fact to remember."}
  },
  "required": ["key", "value"]
}`),
	}
}

// ReadOnly reports that Remember mutates persistent state.
func (RememberTool) ReadOnly() bool { return false }

// Execute saves the key/value to the store.
func (rt RememberTool) Execute(ctx context.Context, in session.ToolCall, _ tool.Workspace) (session.ToolResult, error) {
	var args rememberArgs
	if msg, ok := parseArgs(in, &args); !ok {
		return session.NewToolError(in.ID, msg), nil
	}
	if strings.TrimSpace(args.Key) == "" {
		return session.NewToolError(in.ID, "the \"key\" argument is required"), nil
	}
	if args.Value == "" {
		return session.NewToolError(in.ID, "the \"value\" argument is required"), nil
	}
	if err := rt.store.Remember(ctx, args.Key, args.Value); err != nil {
		return session.NewToolError(in.ID, fmt.Sprintf("could not remember %q: %v", args.Key, err)), nil
	}
	return session.NewToolResult(in.ID, fmt.Sprintf("Remembered %q.", args.Key)), nil
}

// --- Recall tool ----------------------------------------------------------

// RecallTool reads facts from the injected tool.MemoryStore. It is read-only
// (ReadOnly() == true) so the loop may dispatch it in parallel with other reads.
type RecallTool struct {
	store tool.MemoryStore
}

// NewRecallTool constructs the Recall tool bound to store. store must be non-nil.
func NewRecallTool(store tool.MemoryStore) tool.Tool {
	if store == nil {
		panic("memory: NewRecallTool requires a non-nil MemoryStore")
	}
	return RecallTool{store: store}
}

// Compile-time assertion that RecallTool implements tool.Tool.
var _ tool.Tool = RecallTool{}

// recallArgs is the JSON argument shape for the Recall tool.
type recallArgs struct {
	Key string `json:"key"`
}

// Spec returns the model-facing specification of the Recall tool.
func (RecallTool) Spec() tool.ToolSpec {
	return tool.ToolSpec{
		Name:        "Recall",
		Description: recallDescription,
		Schema: schema(`{
  "type": "object",
  "properties": {
    "key": {"type": "string", "description": "Exact key, or a prefix such as \"pref/\"."}
  },
  "required": ["key"]
}`),
	}
}

// ReadOnly reports that Recall does not mutate state.
func (RecallTool) ReadOnly() bool { return true }

// Execute looks up key: an exact hit returns its value; otherwise key is treated
// as a prefix and matching entries are listed. A miss is a non-error result.
func (rt RecallTool) Execute(ctx context.Context, in session.ToolCall, _ tool.Workspace) (session.ToolResult, error) {
	var args recallArgs
	if msg, ok := parseArgs(in, &args); !ok {
		return session.NewToolError(in.ID, msg), nil
	}
	if strings.TrimSpace(args.Key) == "" {
		return session.NewToolError(in.ID, "the \"key\" argument is required"), nil
	}

	// Exact key first.
	entry, ok, err := rt.store.Recall(ctx, args.Key)
	if err != nil {
		return session.NewToolError(in.ID, fmt.Sprintf("could not recall %q: %v", args.Key, err)), nil
	}
	if ok {
		return session.NewToolResult(in.ID, truncateMemory(fmt.Sprintf("%s = %s", entry.Key, entry.Value))), nil
	}

	// Fall back to prefix listing.
	entries, err := rt.store.List(ctx, args.Key)
	if err != nil {
		return session.NewToolError(in.ID, fmt.Sprintf("could not recall %q: %v", args.Key, err)), nil
	}
	if len(entries) == 0 {
		// A miss is a clear, non-error result so the model can proceed.
		return session.NewToolResult(in.ID, fmt.Sprintf("No memory found for %q.", args.Key)), nil
	}

	var b strings.Builder
	fmt.Fprintf(&b, "%d entr%s matching prefix %q:\n", len(entries), plural(len(entries)), args.Key)
	for _, e := range entries {
		fmt.Fprintf(&b, "%s = %s\n", e.Key, e.Value)
	}
	return session.NewToolResult(in.ID, truncateMemory(b.String())), nil
}

// --- Registration helpers -------------------------------------------------

// Tools returns the memory tools (Recall + Remember) bound to store, ready for
// registration. Memory is OPT-IN: the composition root calls this only when it
// has constructed a store, so a deployment without memory simply never registers
// these tools. store must be non-nil.
func Tools(store tool.MemoryStore) []tool.Tool {
	return []tool.Tool{
		NewRecallTool(store),
		NewRememberTool(store),
	}
}

// Register adds the memory tools (bound to store) to cat, returning the first
// registration error (e.g. a name collision) or nil. It is the opt-in companion
// to tools.Register; call it only when a memory store is configured.
func Register(cat *tool.Catalog, store tool.MemoryStore) error {
	for _, t := range Tools(store) {
		if err := cat.Register(t); err != nil {
			return err
		}
	}
	return nil
}

// --- helpers --------------------------------------------------------------

// parseArgs unmarshals a tool call's JSON arguments into dst, delegating to the
// shared toolkit helper. It returns a model-facing error string (not a Go
// error) on malformed JSON.
func parseArgs(in session.ToolCall, dst any) (string, bool) {
	return toolkit.ParseArgs(in, dst)
}

// schema wraps a static JSON-schema literal as json.RawMessage for a ToolSpec,
// delegating to the shared toolkit helper.
func schema(s string) json.RawMessage { return toolkit.Schema(s) }

// truncateMemory trims s to at most toolkit.MaxOutputBytes on a rune boundary,
// appending a marker when it trims. It delegates to the shared toolkit helper so
// the cap stays in lockstep with the rest of the adapter layer.
func truncateMemory(s string) string {
	return toolkit.Truncate(s, toolkit.MaxOutputBytes)
}

// plural returns "y" for one entry and "ies" otherwise, for "entr{y,ies}".
func plural(n int) string {
	if n == 1 {
		return "y"
	}
	return "ies"
}
