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

import "strings"

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
	// Skills is an OPTIONAL list of skill names to PRELOAD into this def's engine.
	// The composition layer resolves each name against the active skills registry
	// and injects the matched skill's body into the def's system prompt (Claude
	// Code-style skill preloading), so the specialist starts with those playbooks
	// already in context rather than having to activate them. An unknown name is a
	// non-fatal composition-time diagnostic. Accepts a YAML array or a
	// comma/space-separated scalar.
	Skills []string
	// MCPServers is an OPTIONAL list of MCP servers to scope to this def's engine
	// (Claude Code-style per-agent MCP). Each entry is EITHER a REFERENCE (a bare
	// server name — the def gets that already-configured main server's tools) OR an
	// INLINE streamable-HTTP server spec (name + url + optional headers — the def
	// connects its OWN server, whose tools never enter the main conversation). An
	// inline entry with no URL collapses to a reference; a non-HTTP/stdio inline
	// entry is rejected with a composition-time diagnostic (streamable-HTTP only).
	// The frontmatter accepts a scalar ("a, b"), a YAML array of scalars, and a YAML
	// array of mappings ({name,url,headers}) interchangeably.
	MCPServers []AgentMCPServer
	// Hooks is an OPTIONAL phase → shell-command map scoping lifecycle hooks to this
	// def's engine. The composition layer builds the def's engine HookRunner from
	// this map (instead of the default inert runner), so a specialist can enforce
	// its own PreToolUse/PostToolUse/etc. gates. Keys are governance hook phase
	// names; an unknown phase is a non-fatal composition-time diagnostic.
	Hooks map[string]string
	// Body is the markdown content following the frontmatter: the specialist's
	// full instructions, composed into the engine's system prompt by the
	// composition layer.
	Body string
	// Path is the source <name>.md path the def was discovered at, retained for
	// diagnostics and so a reviewer can trace a def back to its file.
	Path string
}

// AgentMCPServer is one entry of a def's `mcpServers`. It is EITHER a reference to
// an already-configured (main) server — Name set, URL empty — OR an inline
// streamable-HTTP server the def connects on its own — Name + URL (+ optional
// Headers). IsReference reports which. It carries no transport object and no
// infrastructure type, so it is safe across the adapter boundary like AgentDef.
type AgentMCPServer struct {
	// Name is the server's identifier. For a reference it must match a configured
	// main server's Name; for an inline server it becomes the mcp__<name>__ tool
	// namespace. Required for both forms.
	Name string
	// URL is the inline server's streamable-HTTP endpoint. Empty => this entry is a
	// REFERENCE to an already-configured main server (no new connection).
	URL string
	// Headers are extra HTTP headers for an inline server (e.g. Authorization).
	// Ignored for a reference entry.
	Headers map[string]string
}

// IsReference reports whether this entry references an already-configured main
// server (URL empty) rather than describing an inline server to connect.
func (s AgentMCPServer) IsReference() bool { return strings.TrimSpace(s.URL) == "" }
