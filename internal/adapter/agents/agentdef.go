// Package agents implements configurable AGENT DEFINITIONS — named subagent
// specialists (a prompt persona + a scoped tool allowlist + a model + a
// permission mode + run limits) discovered from operator-controlled markdown
// files. It is the agents analogue of the skills Source seam
// (internal/adapter/skills): a pluggable Source over precedence-ordered
// directories, a forgiving frontmatter parser, and a name-indexed Registry the
// composition root threads into BOTH the Task subagent and (in a later slice)
// the team-member factory.
//
// ONE DEFINITION, TWO CONSUMERS. A `<name>.md` file under a conventional dir is
// reusable as a Task delegate (Task(agent="<name>")) and, in a following slice,
// as a team-member role (MemberSpec.AgentType). This package only PRODUCES the
// definitions; the registry→engine translation lives in internal/app, exactly
// where buildChildEngine/buildMemberEngine already live.
//
// LAYERING: this is an ADAPTER. It reads files (discovery is an adapter concern)
// and may import os/yaml and the domain (session). NOTHING here is imported by a
// domain package, by internal/agent, or by internal/app's hot path — the agent
// loop receives only plain map[string]*Engine + metadata structs, never this
// package's types, preserving the no-adapter-import-from-agent layering rule.
//
// TRUST BOUNDARY: an agent-definition body is OPERATOR-CONTROLLED content (like a
// SKILL.md / AGENTS.md) — it legitimately steers the model and belongs in the
// system prompt, NOT the untrusted-user channel. Conventional dirs are therefore
// strict opt-in (mirroring skills), and there is no model-writable agent-draft
// path in this tier.
package agents

// AgentDef is a pure value object: one discovered agent definition's metadata
// and system-prompt body. It carries no behaviour and no infrastructure types,
// so it is safe to construct in tests and to pass across the adapter boundary.
//
// Only Name and Description are REQUIRED (they are the routing metadata the Task
// tool enumerates always-in-context). Everything else is optional: a def with
// only name+description+body is a pure prompt persona on the call site's default
// tool set and the parent model.
type AgentDef struct {
	// Name is the def's stable identifier, from the frontmatter `name`. It is the
	// value the model passes to the Task tool's `agent` arg to route to this def,
	// and the future team-member AgentType handle. Agent names live in their OWN
	// namespace and are NOT validated against the tool catalog (an agent may share
	// a name with a tool without conflict).
	Name string
	// Description is the one-line summary from the frontmatter `description`: the
	// cheap, always-in-context metadata that steers the model on WHEN to route to
	// this def. Capped at maxDescriptionBytes.
	Description string
	// Tools is the OPTIONAL allowlist of catalog tool names. Absent => the call
	// site's default set. Accepts both a YAML array and a comma/space-separated
	// string (Claude-Code compatibility).
	Tools []string
	// DisallowedTools is an OPTIONAL subtractive filter applied AFTER Tools/default.
	// Accepts both a YAML array and a comma/space-separated string.
	DisallowedTools []string
	// Model is the OPTIONAL model selector: an alias (sonnet/opus/haiku), a full
	// id, or "inherit"/empty (=> parent model). Aliases are resolved ONLY in
	// internal/app, never here.
	Model string
	// PermissionMode is the OPTIONAL session permission mode hint
	// (default|plan|acceptEdits). Stored as the raw string; the domain mode value
	// object is resolved in the composition layer.
	PermissionMode string
	// MaxTurns is the OPTIONAL per-run turn cap. Zero => the caller default.
	MaxTurns int
	// Color is an OPTIONAL UX hint only (e.g. a TUI tag colour); it NEVER affects
	// execution.
	Color string
	// Body is the markdown content following the frontmatter: the specialist's
	// full instructions, composed into the engine's system prompt by the
	// composition layer.
	Body string
	// Path is the source <name>.md path the def was discovered at, retained for
	// diagnostics and so a reviewer can trace a def back to its file.
	Path string
}
