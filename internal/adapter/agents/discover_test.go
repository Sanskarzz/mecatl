package agents

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// writeDef writes a <name>.md file into dir, creating dir if needed.
func writeDef(t *testing.T, dir, file, content string) string {
	t.Helper()
	if err := os.MkdirAll(dir, 0o755); err != nil {
		t.Fatalf("mkdir: %v", err)
	}
	p := filepath.Join(dir, file)
	if err := os.WriteFile(p, []byte(content), 0o644); err != nil {
		t.Fatalf("write %s: %v", p, err)
	}
	return p
}

func TestParseAgentDefValid(t *testing.T) {
	raw := []byte(`---
name: code-reviewer
description: Reviews a diff for correctness bugs.
tools: [Read, Grep, Glob]
disallowedTools: [Write]
model: sonnet
permissionMode: plan
maxTurns: 9
color: blue
---
You are a meticulous code reviewer.
Inspect the change and report findings.`)

	def, perr, notes := parseAgentDef(raw, "code-reviewer.md")
	if perr != "" {
		t.Fatalf("unexpected parse error: %s", perr)
	}
	if len(notes) != 0 {
		t.Fatalf("unexpected notes: %v", notes)
	}
	if def.Name != "code-reviewer" || def.Description != "Reviews a diff for correctness bugs." {
		t.Fatalf("name/desc mismatch: %+v", def)
	}
	if got, want := strings.Join(def.Tools, ","), "Read,Grep,Glob"; got != want {
		t.Fatalf("tools = %q, want %q", got, want)
	}
	if got, want := strings.Join(def.DisallowedTools, ","), "Write"; got != want {
		t.Fatalf("disallowedTools = %q, want %q", got, want)
	}
	if def.Model != "sonnet" || def.PermissionMode != "plan" || def.MaxTurns != 9 || def.Color != "blue" {
		t.Fatalf("optional fields mismatch: %+v", def)
	}
	if !strings.HasPrefix(def.Body, "You are a meticulous code reviewer.") {
		t.Fatalf("body mismatch: %q", def.Body)
	}
}

// TestParseAgentDefToolsStringForm asserts Claude-Code compatibility: a `tools`
// scalar string ("Read, Edit Grep") parses identically to the array form.
func TestParseAgentDefToolsStringForm(t *testing.T) {
	raw := []byte(`---
name: x
description: d
tools: Read, Edit Grep
disallowedTools: Write
---
body`)
	def, perr, _ := parseAgentDef(raw, "x.md")
	if perr != "" {
		t.Fatalf("parse error: %s", perr)
	}
	if got, want := strings.Join(def.Tools, ","), "Read,Edit,Grep"; got != want {
		t.Fatalf("tools = %q, want %q", got, want)
	}
	if got, want := strings.Join(def.DisallowedTools, ","), "Write"; got != want {
		t.Fatalf("disallowedTools = %q, want %q", got, want)
	}
}

// TestParseAgentDefSkillsMCPHooks asserts the Claude-Code-parity frontmatter
// fields parse: skills/mcpServers accept both array and scalar forms (like tools),
// and hooks parses as a phase→command map with empty entries dropped.
func TestParseAgentDefSkillsMCPHooks(t *testing.T) {
	raw := []byte(`---
name: specialist
description: a specialist
skills: refactoring, testing
mcpServers: [github, jira]
hooks:
  PreToolUse: "echo pre"
  PostToolUse: "  "
  "": "echo orphan"
---
body`)
	def, perr, _ := parseAgentDef(raw, "s.md")
	if perr != "" {
		t.Fatalf("parse error: %s", perr)
	}
	if got, want := strings.Join(def.Skills, ","), "refactoring,testing"; got != want {
		t.Fatalf("skills = %q, want %q", got, want)
	}
	if got, want := strings.Join(def.MCPServers, ","), "github,jira"; got != want {
		t.Fatalf("mcpServers = %q, want %q", got, want)
	}
	// Empty-command and empty-key hook entries are dropped; only PreToolUse survives.
	if len(def.Hooks) != 1 || def.Hooks["PreToolUse"] != "echo pre" {
		t.Fatalf("hooks = %+v, want only PreToolUse=echo pre", def.Hooks)
	}
}

// TestParseAgentDefNoExtraFieldsNil asserts a def without the new fields carries
// nil slices/map (not empty non-nil), so the composition layer can treat absence
// uniformly.
func TestParseAgentDefNoExtraFieldsNil(t *testing.T) {
	def, perr, _ := parseAgentDef([]byte("---\nname: n\ndescription: d\n---\nbody"), "n.md")
	if perr != "" {
		t.Fatalf("parse error: %s", perr)
	}
	if def.Skills != nil || def.MCPServers != nil || def.Hooks != nil {
		t.Fatalf("absent fields should be nil: skills=%v mcp=%v hooks=%v", def.Skills, def.MCPServers, def.Hooks)
	}
}

func TestParseAgentDefRequiredFields(t *testing.T) {
	cases := []struct {
		name string
		raw  string
		want string
	}{
		{"no frontmatter", "just a body, no delimiters", "missing YAML frontmatter"},
		{"missing name", "---\ndescription: d\n---\nbody", "missing a non-empty \"name\""},
		{"missing description", "---\nname: n\n---\nbody", "missing a non-empty \"description\""},
		{"blank name", "---\nname: \"  \"\ndescription: d\n---\nbody", "missing a non-empty \"name\""},
		{"malformed yaml", "---\nname: [unterminated\n---\nbody", "malformed YAML frontmatter"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			_, perr, _ := parseAgentDef([]byte(tc.raw), "f.md")
			if perr == "" || !strings.Contains(perr, tc.want) {
				t.Fatalf("err = %q, want containing %q", perr, tc.want)
			}
		})
	}
}

func TestParseAgentDefTruncatesDescriptionAndBody(t *testing.T) {
	longDesc := strings.Repeat("d", maxDescriptionBytes+50)
	longBody := strings.Repeat("b", maxPromptBodyBytes+50)
	raw := []byte("---\nname: n\ndescription: " + longDesc + "\n---\n" + longBody)
	def, perr, notes := parseAgentDef(raw, "n.md")
	if perr != "" {
		t.Fatalf("parse error: %s", perr)
	}
	if len(def.Description) > maxDescriptionBytes {
		t.Fatalf("description not truncated: %d bytes", len(def.Description))
	}
	if len(def.Body) > maxPromptBodyBytes {
		t.Fatalf("body not truncated: %d bytes", len(def.Body))
	}
	if len(notes) != 2 {
		t.Fatalf("want 2 truncation notes, got %d: %v", len(notes), notes)
	}
}

// TestParseAgentDefUnknownKeysIgnored proves forward-compat: an unknown
// frontmatter key does not fail the parse.
func TestParseAgentDefUnknownKeysIgnored(t *testing.T) {
	raw := []byte("---\nname: n\ndescription: d\nfutureField: whatever\n---\nbody")
	_, perr, _ := parseAgentDef(raw, "n.md")
	if perr != "" {
		t.Fatalf("unknown key should be ignored, got %q", perr)
	}
}

func TestDirSourceDiscovery(t *testing.T) {
	dir := t.TempDir()
	writeDef(t, dir, "alpha.md", "---\nname: alpha\ndescription: A\n---\nbody A")
	writeDef(t, dir, "beta.md", "---\nname: beta\ndescription: B\n---\nbody B")
	writeDef(t, dir, "broken.md", "no frontmatter here")
	writeDef(t, dir, "README.txt", "not a def") // wrong extension, ignored

	defs, skips, err := DirSource{Dir: dir}.Agents(t.Context())
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if len(defs) != 2 {
		t.Fatalf("want 2 defs, got %d: %+v", len(defs), defs)
	}
	if defs[0].Name != "alpha" || defs[1].Name != "beta" {
		t.Fatalf("defs not sorted by name: %+v", defs)
	}
	// broken.md yields exactly one skip; README.txt is silently ignored.
	if len(skips) != 1 || !strings.Contains(skips[0].Path, "broken.md") {
		t.Fatalf("want 1 skip for broken.md, got %v", skips)
	}
}

func TestDirSourceMissingDirIsOptIn(t *testing.T) {
	defs, skips, err := DirSource{Dir: filepath.Join(t.TempDir(), "nope")}.Agents(t.Context())
	if err != nil || len(defs) != 0 || len(skips) != 0 {
		t.Fatalf("missing dir must be no-defs/no-error, got defs=%v skips=%v err=%v", defs, skips, err)
	}
	// Empty Dir is also opt-in.
	defs, _, err = DirSource{Dir: ""}.Agents(t.Context())
	if err != nil || len(defs) != 0 {
		t.Fatalf("empty dir must be no-defs, got %v err=%v", defs, err)
	}
}

// TestDirSourceDuplicateNameWithinDir asserts keep-first dedup by frontmatter
// name within one directory (the filename does not decide identity).
func TestDirSourceDuplicateNameWithinDir(t *testing.T) {
	dir := t.TempDir()
	// Both files declare name "dup"; sorted path order keeps a.md.
	writeDef(t, dir, "a.md", "---\nname: dup\ndescription: from a\n---\nbody")
	writeDef(t, dir, "b.md", "---\nname: dup\ndescription: from b\n---\nbody")

	defs, skips, err := DirSource{Dir: dir}.Agents(t.Context())
	if err != nil {
		t.Fatalf("err: %v", err)
	}
	if len(defs) != 1 || defs[0].Description != "from a" {
		t.Fatalf("want keep-first a.md, got %+v", defs)
	}
	if len(skips) != 1 || !strings.Contains(skips[0].Reason, "duplicate") {
		t.Fatalf("want duplicate skip, got %v", skips)
	}
}
