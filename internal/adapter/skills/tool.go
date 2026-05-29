package skills

import (
	"context"
	"fmt"
	"sort"
	"strings"

	"github.com/stacklok/mecatl/internal/adapter/toolkit"
	"github.com/stacklok/mecatl/internal/session"
	"github.com/stacklok/mecatl/internal/tool"
)

// ToolName is the catalog name of the single skills tool.
const ToolName = "Skill"

// descriptionPreamble is the static head of the Skill tool's description. The
// per-skill metadata (name + one-line description) is appended to it at
// construction time, so the always-in-context inventory of skills lives in the
// ToolSpec.Description — cheap and cache-stable across turns.
const descriptionPreamble = `Activate a skill: load a named, progressive-disclosure instruction set into the conversation.

Skills are curated, reusable playbooks (workflows, conventions, domain procedures) authored as files. Only each skill's NAME and one-line description are shown below; the FULL instructions load only when you activate a skill here. This keeps your context small until a skill is actually needed.

When to use:
- When the task matches one of the skills listed below. Read the one-line
  descriptions, pick the best match, then activate it by name to get its full
  instructions, and follow them.
- You may activate more than one skill across a task, one call at a time.

When NOT to use:
- For reading project files (use Read) or searching code (use Grep/Glob). A skill
  is curated guidance, not a file browser.
- When no skill below fits the task — just proceed without one.

Arguments:
- name (required): the exact name of one of the skills listed below.

Available skills:`

// Tool is the single model-facing skills tool. Its Spec().Description enumerates
// every discovered skill's metadata (the always-in-context layer); Execute
// returns a single skill's full body (the load-on-activation layer). It is
// read-only — it returns instructions and mutates nothing — so the dispatcher may
// run it in parallel with other reads and it remains available in plan mode.
type Tool struct {
	// byName indexes skills by their activation name for O(1) Execute lookup.
	byName map[string]Skill
	// description is the precomputed, cache-stable tool description: the static
	// preamble plus the enumerated skill metadata.
	description string
}

// Compile-time assertion that Tool implements tool.Tool.
var _ tool.Tool = Tool{}

// NewTool builds the Skill tool over the given discovered skills. The skills are
// indexed by name and their metadata is rendered into the tool description once,
// at construction, so Spec() is allocation-free per call. Callers should not pass
// an empty slice — the composition root omits the tool entirely when no skills
// are discovered (see Register); NewTool with no skills yields a tool whose
// Execute always reports "no skills available".
func NewTool(discovered []Skill) Tool {
	byName := make(map[string]Skill, len(discovered))
	// Copy + sort by name so the description ordering is deterministic regardless
	// of the input slice's order.
	sorted := append([]Skill(nil), discovered...)
	sort.Slice(sorted, func(i, j int) bool { return sorted[i].Name < sorted[j].Name })

	var b strings.Builder
	b.WriteString(descriptionPreamble)
	if len(sorted) == 0 {
		b.WriteString("\n  (none configured)")
	}
	for _, s := range sorted {
		byName[s.Name] = s
		fmt.Fprintf(&b, "\n- %s: %s", s.Name, s.Description)
	}
	return Tool{byName: byName, description: b.String()}
}

// skillArgs is the JSON argument shape for the Skill tool.
type skillArgs struct {
	Name string `json:"name"`
}

// Spec returns the model-facing specification. The Description carries the
// always-in-context skill inventory (name + one-line description per skill); the
// schema is a single required "name" string.
func (t Tool) Spec() tool.ToolSpec {
	return tool.ToolSpec{
		Name:        ToolName,
		Description: t.description,
		Schema: toolkit.Schema(`{
  "type": "object",
  "properties": {
    "name": {"type": "string", "description": "Exact name of the skill to activate, as listed in this tool's description."}
  },
  "required": ["name"]
}`),
	}
}

// ReadOnly reports that activating a skill only returns instructions and mutates
// no state. This keeps the tool dispatchable in parallel with other reads and
// available in plan mode (read-only tools survive the plan-mode catalog filter).
func (Tool) ReadOnly() bool { return true }

// Execute looks up the named skill and returns its full body as the tool result.
// An unknown (or empty) name is a model-addressable error result that lists the
// available skill names so the model can recover, NOT a harness-level error.
func (t Tool) Execute(_ context.Context, in session.ToolCall, _ tool.Workspace) (session.ToolResult, error) {
	var args skillArgs
	if msg, ok := toolkit.ParseArgs(in, &args); !ok {
		return session.NewToolError(in.ID, msg), nil
	}
	name := strings.TrimSpace(args.Name)
	if name == "" {
		return session.NewToolError(in.ID, "the \"name\" argument is required; "+t.availableHint()), nil
	}
	sk, ok := t.byName[name]
	if !ok {
		return session.NewToolError(in.ID,
			fmt.Sprintf("unknown skill %q; %s", name, t.availableHint())), nil
	}
	return session.NewToolResult(in.ID, toolkit.Truncate(sk.Body, toolkit.MaxOutputBytes)), nil
}

// availableHint returns a short "available skills are: ..." sentence (or a clear
// "no skills" message) for error results, so the model always learns the valid
// names from a failed call.
func (t Tool) availableHint() string {
	if len(t.byName) == 0 {
		return "no skills are available"
	}
	names := make([]string, 0, len(t.byName))
	for n := range t.byName {
		names = append(names, n)
	}
	sort.Strings(names)
	return "available skills are: " + strings.Join(names, ", ")
}

// --- Registration helpers -------------------------------------------------

// Register discovers skills under dir and, when at least one valid skill is
// found, registers a single Skill tool into cat. It returns the discovered
// skills, the per-skill skip diagnostics (malformed/duplicate entries), and the
// first registration error (e.g. a name collision) or a discovery I/O fault.
//
// Skills are OPT-IN and the tool is registered ONLY when there is something to
// expose: if dir is empty or yields zero valid skills, Register registers
// NOTHING and returns (nil, skips, nil) — there is no value in advertising a
// Skill tool with an empty inventory. The composition root logs the skip
// diagnostics and the enabled/disabled state.
func Register(cat *tool.Catalog, dir string) ([]Skill, []SkipError, error) {
	discovered, skips, err := Discover(dir)
	if err != nil {
		return nil, skips, err
	}
	if len(discovered) == 0 {
		return nil, skips, nil
	}
	if err := cat.Register(NewTool(discovered)); err != nil {
		return discovered, skips, err
	}
	return discovered, skips, nil
}
