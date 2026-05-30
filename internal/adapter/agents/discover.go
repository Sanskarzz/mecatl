package agents

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strings"

	yaml "go.yaml.in/yaml/v3"
)

// AgentFileExt is the conventional extension of an agent-definition file. A def
// lives at <dir>/<name>.md (a FLAT file, not a <name>/AGENT.md subdir), matching
// Claude Code's .claude/agents/ layout.
const AgentFileExt = ".md"

// maxDescriptionBytes caps a def's one-line description. The description is the
// ALWAYS-IN-CONTEXT routing metadata (it lives in the Task tool's
// Spec().Description tail, on every request), so an unbounded one would inflate
// every prompt and break the byte-stable prompt-prefix caching the OpenAI adapter
// relies on. parseAgentDef truncates (rune-safe, with an ellipsis) and records a
// non-fatal warning when it trims.
const maxDescriptionBytes = 800

// maxPromptBodyBytes caps a def body. The body becomes a system-prompt layer that
// is always-in-context every turn for that engine, so an oversized body is a real
// per-turn token cost. It is capped harder than a skill body (which loads only on
// activation). parseAgentDef truncates and warns.
const maxPromptBodyBytes = 8 * 1024

// frontmatter is the parsed YAML header of an agent-def file. Only name and
// description are required; the rest are optional. Unknown/extra keys are ignored
// (forward-compat). The `tools`/`disallowedTools` fields use stringOrSlice so a
// real Claude-Code `.claude/agents` file ("tools: Read, Edit") parses the same as
// the YAML-array form.
type frontmatter struct {
	Name            string        `yaml:"name"`
	Description     string        `yaml:"description"`
	Tools           stringOrSlice `yaml:"tools"`
	DisallowedTools stringOrSlice `yaml:"disallowedTools"`
	Model           string        `yaml:"model"`
	PermissionMode  string        `yaml:"permissionMode"`
	MaxTurns        int           `yaml:"maxTurns"`
	Color           string        `yaml:"color"`
}

// stringOrSlice is a YAML field that accepts BOTH a sequence (["Read","Grep"])
// AND a single comma/space-separated scalar ("Read, Grep" or "Read Grep") for
// Claude-Code compatibility — CC's agent frontmatter writes `tools` as a string
// in some versions and an array in others. UnmarshalYAML normalises either form
// into a trimmed, empty-free []string.
type stringOrSlice []string

func (s *stringOrSlice) UnmarshalYAML(node *yaml.Node) error {
	switch node.Kind {
	case yaml.SequenceNode:
		var arr []string
		if err := node.Decode(&arr); err != nil {
			return err
		}
		*s = splitList(arr...)
		return nil
	case yaml.ScalarNode:
		*s = splitList(node.Value)
		return nil
	default:
		return fmt.Errorf("expected a string or a list, got YAML kind %d", node.Kind)
	}
}

// splitList flattens its inputs, splitting any entry on commas and whitespace,
// trimming each token and dropping empties. It is the single normaliser for both
// the array and scalar `tools`/`disallowedTools` forms.
func splitList(in ...string) []string {
	var out []string
	for _, raw := range in {
		for _, tok := range strings.FieldsFunc(raw, func(r rune) bool {
			return r == ',' || r == ' ' || r == '\t' || r == '\n'
		}) {
			if t := strings.TrimSpace(tok); t != "" {
				out = append(out, t)
			}
		}
	}
	return out
}

// DirSource is the local-OS-filesystem implementation of AgentSource: it produces
// the defs laid out as <Dir>/<name>.md (flat files) under a single directory.
type DirSource struct {
	// Dir is the directory to scan. An empty or absent Dir yields no defs (opt-in).
	Dir string
	// Label is an optional human-readable name for this source (e.g. "project",
	// "user", "explicit"), surfaced in diagnostics. It does not affect discovery.
	Label string
}

// Agents implements AgentSource for a single local directory. It scans Dir for
// flat <name>.md files, parses each one's YAML frontmatter and markdown body, and
// returns the valid defs sorted by name.
//
// It is forgiving by design: an empty or missing Dir yields no defs and no error
// (opt-in); a malformed or frontmatter-less file is SKIPPED and reported via the
// returned []SkipError rather than aborting the scan. It returns a non-nil error
// only for a genuine I/O fault reading the directory itself.
//
// A def's frontmatter `name` is the source of truth (NOT the filename); duplicate
// effective names WITHIN this directory are resolved keep-first in sorted-path
// order. Cross-source collisions are resolved one level up by MultiSource.
func (s DirSource) Agents(_ context.Context) ([]AgentDef, []SkipError, error) {
	dir := strings.TrimSpace(s.Dir)
	if dir == "" {
		return nil, nil, nil
	}
	entries, err := os.ReadDir(dir)
	if err != nil {
		if os.IsNotExist(err) {
			return nil, nil, nil
		}
		return nil, nil, fmt.Errorf("agents: read dir %q: %w", dir, err)
	}

	sort.Slice(entries, func(i, j int) bool { return entries[i].Name() < entries[j].Name() })

	var (
		out   []AgentDef
		skips []SkipError
		seen  = map[string]string{} // effective name -> path that claimed it
	)
	for _, e := range entries {
		if e.IsDir() || !strings.HasSuffix(e.Name(), AgentFileExt) {
			continue
		}
		path := filepath.Join(dir, e.Name())
		raw, rerr := os.ReadFile(path)
		if rerr != nil {
			skips = append(skips, SkipError{Path: path, Reason: fmt.Sprintf("cannot read: %v", rerr)})
			continue
		}
		def, perr, notes := parseAgentDef(raw, path)
		if perr != "" {
			skips = append(skips, SkipError{Path: path, Reason: perr})
			continue
		}
		for _, n := range notes {
			skips = append(skips, SkipError{Path: path, Reason: n})
		}
		if prev, dup := seen[def.Name]; dup {
			skips = append(skips, SkipError{
				Path:   path,
				Reason: fmt.Sprintf("duplicate agent name %q (already defined at %q)", def.Name, prev),
			})
			continue
		}
		seen[def.Name] = path
		out = append(out, def)
	}

	sort.Slice(out, func(i, j int) bool { return out[i].Name < out[j].Name })
	return out, skips, nil
}

// Discover scans dir for agent defs and returns them. It is a thin convenience
// wrapper over DirSource for callers (and tests) that want single-directory
// discovery without composing a Source.
func Discover(dir string) ([]AgentDef, []SkipError, error) {
	return DirSource{Dir: dir}.Agents(context.Background())
}

// parseAgentDef splits raw into YAML frontmatter and a markdown body and
// validates the required header fields. It returns a fatal reason string (with a
// zero AgentDef) on any structural problem so the caller records a SkipError and
// EXCLUDES the def (reason is "" on success), plus a slice of non-fatal warning
// notes for a def that IS kept (e.g. truncation). It is filesystem-free so every
// Source implementation can reuse it.
//
// Tool names in Tools/DisallowedTools are NOT validated here — the parser is
// catalog-free. An unknown tool name becomes a resolve-time diagnostic in
// internal/app (against a concrete catalog), never a parse failure.
func parseAgentDef(raw []byte, path string) (AgentDef, string, []string) {
	fmText, body, ok := splitFrontmatter(string(raw))
	if !ok {
		return AgentDef{}, "missing YAML frontmatter (expected a leading '---' delimited block)", nil
	}
	var fm frontmatter
	if err := yaml.Unmarshal([]byte(fmText), &fm); err != nil {
		return AgentDef{}, fmt.Sprintf("malformed YAML frontmatter: %v", err), nil
	}
	name := strings.TrimSpace(fm.Name)
	if name == "" {
		return AgentDef{}, "frontmatter is missing a non-empty \"name\"", nil
	}
	desc := strings.TrimSpace(fm.Description)
	if desc == "" {
		return AgentDef{}, "frontmatter is missing a non-empty \"description\"", nil
	}

	var notes []string
	if len(desc) > maxDescriptionBytes {
		notes = append(notes, fmt.Sprintf(
			"description is %d bytes; truncated to the always-in-context cap of %d bytes",
			len(desc), maxDescriptionBytes))
		desc = truncateRunes(desc, maxDescriptionBytes)
	}

	trimmedBody := strings.TrimSpace(body)
	if len(trimmedBody) > maxPromptBodyBytes {
		notes = append(notes, fmt.Sprintf(
			"body is %d bytes; truncated to the prompt-body cap of %d bytes (it is in-context every turn)",
			len(trimmedBody), maxPromptBodyBytes))
		trimmedBody = truncateRunes(trimmedBody, maxPromptBodyBytes)
	}

	return AgentDef{
		Name:            name,
		Description:     desc,
		Tools:           []string(fm.Tools),
		DisallowedTools: []string(fm.DisallowedTools),
		Model:           strings.TrimSpace(fm.Model),
		PermissionMode:  strings.TrimSpace(fm.PermissionMode),
		MaxTurns:        fm.MaxTurns,
		Color:           strings.TrimSpace(fm.Color),
		Body:            trimmedBody,
		Path:            path,
	}, "", notes
}

// truncateRunes trims s to at most maxBytes on a rune boundary and appends a
// single-character ellipsis.
func truncateRunes(s string, maxBytes int) string {
	if len(s) <= maxBytes {
		return s
	}
	const ellipsis = "…"
	cut := maxBytes - len(ellipsis)
	if cut < 0 {
		cut = 0
	}
	for cut > 0 && !utf8RuneStart(s[cut]) {
		cut--
	}
	return s[:cut] + ellipsis
}

// utf8RuneStart reports whether b is the first byte of a UTF-8 rune.
func utf8RuneStart(b byte) bool { return b&0xC0 != 0x80 }

// splitFrontmatter separates a leading YAML frontmatter block, delimited by a
// line containing only "---" at the very start and a matching closing "---" line,
// from the markdown body that follows. A leading UTF-8 BOM is tolerated.
func splitFrontmatter(s string) (fm, body string, ok bool) {
	s = strings.TrimPrefix(s, "\ufeff")
	s = strings.ReplaceAll(s, "\r\n", "\n")
	if !strings.HasPrefix(s, "---\n") && s != "---" {
		return "", "", false
	}
	rest := strings.TrimPrefix(s, "---\n")
	idx := indexClosingDelim(rest)
	if idx < 0 {
		return "", "", false
	}
	fm = rest[:idx]
	after := rest[idx:]
	after = strings.TrimPrefix(after, "---")
	after = strings.TrimPrefix(after, "\n")
	return fm, after, true
}

// indexClosingDelim returns the byte offset, within s, of the start of the first
// line that is exactly "---" (the closing frontmatter delimiter), or -1 if none.
func indexClosingDelim(s string) int {
	offset := 0
	for _, line := range strings.SplitAfter(s, "\n") {
		trimmed := strings.TrimSuffix(line, "\n")
		if trimmed == "---" {
			return offset
		}
		offset += len(line)
	}
	return -1
}
