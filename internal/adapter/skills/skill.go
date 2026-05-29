// Package skills implements Agent Skills — progressive-disclosure instruction
// units (Claude Code / Agent Skills style) — as an OPT-IN adapter exposing a
// single tool.Tool to the model.
//
// PROGRESSIVE DISCLOSURE (corpus pattern 9, applied to INSTRUCTIONS rather than
// tool schemas): the cheap, always-in-context layer is each skill's METADATA
// header — its name plus a one-line description. The Skill tool's
// Spec().Description enumerates that header for every discovered skill, so it is
// stable across turns and cache-friendly. The expensive layer — a skill's full
// markdown body — loads only when the model ACTIVATES the skill by calling the
// tool with that skill's name; Execute returns the body as the tool result.
//
// LAYERING: this is an adapter. It reads files (discovery is an adapter concern)
// and implements the domain tool.Tool interface; nothing here is imported by a
// domain package. The composition root (cmd/mecated) wires it behind a flag,
// exactly like the memory adapter. A SKILL.md file is YAML frontmatter
// (`name` + `description`) followed by a markdown body, matching the wider
// Agent Skills ecosystem.
package skills

// Skill is a pure value object: one discovered skill's metadata and body. It
// carries no behaviour and no infrastructure types, so it is safe to construct
// in tests and to pass across the adapter boundary.
type Skill struct {
	// Name is the skill's stable identifier, from the frontmatter `name`. It is
	// the value the model passes to the Skill tool to activate this skill, and is
	// what the tool description enumerates.
	Name string
	// Description is the one-line summary from the frontmatter `description`. This
	// is the cheap, always-in-context metadata that steers the model on WHEN to
	// activate the skill.
	Description string
	// Body is the markdown content following the frontmatter: the full
	// instructions that load on activation.
	Body string
	// Path is the source SKILL.md path the skill was discovered at, retained for
	// diagnostics and so a reviewer can trace a skill back to its file.
	Path string
}
