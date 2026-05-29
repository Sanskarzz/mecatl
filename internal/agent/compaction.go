package agent

import (
	"context"
	"encoding/json"
	"fmt"
	"sort"
	"strings"

	"github.com/stacklok/mecatl/internal/session"
)

// Compactor compresses a Conversation that has grown past the context-window
// threshold into a shorter, semantically-equivalent history. It is a seam
// (ARCHITECTURE.md §8, gauntlet #12): the default implementation is a pure,
// offline heuristic, but an LLM-backed summariser can be slotted in behind the
// same interface.
//
// Compact returns the replacement message slice, a human-readable summary of
// what was dropped (surfaced on the compaction Event), and an error only on a
// genuine failure. Implementations MUST preserve file paths, decisions, and
// unresolved questions while dropping large tool-output bodies and stale file
// contents.
type Compactor interface {
	Compact(ctx context.Context, conv *session.Conversation) (compacted []session.Message, summary string, err error)
}

// maxToolBodyChars is the per-tool-result body budget the heuristic compactor
// keeps; longer bodies are truncated with an elision marker. File paths and the
// head of each body survive so the model retains orientation.
const maxToolBodyChars = 400

// keepLastTurns is the number of trailing conversation messages the heuristic
// compactor preserves verbatim (the recent working set the model is mid-task on).
const keepLastTurns = 6

// HeuristicCompactor is the default, network-free Compactor. It keeps the system
// prompt and the user goal, synthesises a single summary message that lists every
// file path touched so far, truncates large tool-result bodies, and preserves the
// last keepLastTurns messages verbatim. It performs no LLM call, so it is fully
// deterministic and testable offline (gauntlet #12-lite).
type HeuristicCompactor struct {
	// MaxToolBodyChars overrides maxToolBodyChars when > 0.
	MaxToolBodyChars int
	// KeepLastTurns overrides keepLastTurns when > 0.
	KeepLastTurns int
}

// Compact implements Compactor with the heuristic described on HeuristicCompactor.
func (h HeuristicCompactor) Compact(_ context.Context, conv *session.Conversation) ([]session.Message, string, error) {
	bodyBudget := maxToolBodyChars
	if h.MaxToolBodyChars > 0 {
		bodyBudget = h.MaxToolBodyChars
	}
	keep := keepLastTurns
	if h.KeepLastTurns > 0 {
		keep = h.KeepLastTurns
	}

	msgs := conv.Messages
	paths := touchedPaths(msgs)

	// Split the head (to be summarised) from the preserved tail.
	cut := len(msgs) - keep
	if cut < 0 {
		cut = 0
	}
	head := msgs[:cut]
	tail := msgs[cut:]

	out := make([]session.Message, 0, len(tail)+3)

	// Preserve the leading system prompt(s) verbatim, if any.
	for _, m := range head {
		if m.Role == session.RoleSystem {
			out = append(out, m)
		}
	}

	// Preserve the first user message (the goal) verbatim.
	if goal, ok := firstUser(head); ok {
		out = append(out, goal)
	}

	// Synthesise the summary message listing touched paths.
	summary := buildSummary(paths)
	out = append(out, session.NewUserMessage(summary))

	// Preserve the tail verbatim, but truncate any oversized tool-result bodies
	// so old file dumps do not dominate the kept window.
	for _, m := range tail {
		out = append(out, truncateToolBody(m, bodyBudget))
	}

	return out, summary, nil
}

// firstUser returns the first user-role message in msgs.
func firstUser(msgs []session.Message) (session.Message, bool) {
	for _, m := range msgs {
		if m.Role == session.RoleUser {
			return m, true
		}
	}
	return session.Message{}, false
}

// touchedPaths collects the distinct file paths referenced by tool calls across
// the conversation, sorted for determinism. It looks for a "path" or "file_path"
// field in each tool call's JSON args.
func touchedPaths(msgs []session.Message) []string {
	seen := make(map[string]struct{})
	for _, m := range msgs {
		for _, c := range m.ToolCalls {
			if p := extractPath(c.Args); p != "" {
				seen[p] = struct{}{}
			}
		}
	}
	out := make([]string, 0, len(seen))
	for p := range seen {
		out = append(out, p)
	}
	sort.Strings(out)
	return out
}

// extractPath pulls a file path out of a tool call's JSON args, checking the
// conventional "path" and "file_path" fields the core tools use. It returns ""
// when args carry no recognisable path (or are not an object).
func extractPath(args json.RawMessage) string {
	if len(args) == 0 {
		return ""
	}
	var obj struct {
		Path     string `json:"path"`
		FilePath string `json:"file_path"`
	}
	if err := json.Unmarshal(args, &obj); err != nil {
		return ""
	}
	if obj.Path != "" {
		return obj.Path
	}
	return obj.FilePath
}

// buildSummary renders the compaction summary message body. It always names the
// preserved-context contract so the model knows history was elided, then lists
// the touched file paths (the load-bearing facts gauntlet #12 requires to
// survive).
func buildSummary(paths []string) string {
	var b strings.Builder
	b.WriteString("[conversation compacted] Earlier turns were summarised to fit the context window. ")
	b.WriteString("Decisions, the original goal, and the files touched so far are preserved; ")
	b.WriteString("large tool outputs and stale file contents were dropped.")
	if len(paths) > 0 {
		b.WriteString("\n\nFiles touched so far:")
		for _, p := range paths {
			b.WriteString("\n- ")
			b.WriteString(p)
		}
	}
	return b.String()
}

// truncateToolBody returns m unchanged unless it carries an oversized tool result
// body, in which case it returns a copy with the body truncated to budget chars
// plus an elision marker. Non-tool messages pass through untouched.
func truncateToolBody(m session.Message, budget int) session.Message {
	if m.Role != session.RoleTool || m.ToolResult == nil {
		return m
	}
	body := m.ToolResult.Content
	if len(body) <= budget {
		return m
	}
	truncated := fmt.Sprintf("%s\n... [%d bytes elided by compaction]",
		body[:budget], len(body)-budget)
	if m.ToolResult.IsError {
		return session.NewToolMessage(session.NewToolError(m.ToolResult.CallID, truncated))
	}
	return session.NewToolMessage(session.NewToolResult(m.ToolResult.CallID, truncated))
}
