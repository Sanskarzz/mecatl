package skills

import (
	"context"
	"encoding/json"
	"strings"
	"testing"

	"github.com/stacklok/mecatl/internal/session"
	"github.com/stacklok/mecatl/internal/tool"
)

// sampleSkills is a small fixed set used across the tool tests.
func sampleSkills() []Skill {
	return []Skill{
		{Name: "review", Description: "Run a structured code review.", Body: "Look for correctness, then style."},
		{Name: "commit-style", Description: "Write conventional commits.", Body: "type(scope): subject\nWrap at 72."},
	}
}

// call builds a ToolCall with JSON args marshalled from m.
func call(t *testing.T, m map[string]any) session.ToolCall {
	t.Helper()
	raw, err := json.Marshal(m)
	if err != nil {
		t.Fatalf("marshal args: %v", err)
	}
	return session.NewToolCall("id-skill", ToolName, raw)
}

// exec runs the tool and fails on a harness-level error.
func exec(t *testing.T, tl tool.Tool, in session.ToolCall) session.ToolResult {
	t.Helper()
	res, err := tl.Execute(context.Background(), in, nil)
	if err != nil {
		t.Fatalf("Execute: unexpected harness error: %v", err)
	}
	return res
}

func TestToolSpecEnumeratesSkills(t *testing.T) {
	tl := NewTool(sampleSkills())
	spec := tl.Spec()
	if spec.Name != ToolName {
		t.Fatalf("Spec().Name = %q, want %q", spec.Name, ToolName)
	}
	// The always-in-context metadata: every skill's name + one-line description
	// must appear in the description.
	for _, s := range sampleSkills() {
		if !strings.Contains(spec.Description, s.Name) {
			t.Errorf("description missing skill name %q", s.Name)
		}
		if !strings.Contains(spec.Description, s.Description) {
			t.Errorf("description missing skill one-liner %q", s.Description)
		}
	}
	// Bodies must NOT be in the description (that is the load-on-activation layer).
	if strings.Contains(spec.Description, "type(scope): subject") {
		t.Error("description leaked a skill body; bodies must load only on activation")
	}
	// Listing must be sorted (commit-style before review) for cache stability.
	if strings.Index(spec.Description, "commit-style") > strings.Index(spec.Description, "review") {
		t.Error("skills not listed in sorted order")
	}
	// The schema must be valid JSON.
	var js any
	if err := json.Unmarshal(spec.Schema, &js); err != nil {
		t.Errorf("schema invalid JSON: %v", err)
	}
}

func TestToolExecuteReturnsBody(t *testing.T) {
	tl := NewTool(sampleSkills())
	res := exec(t, tl, call(t, map[string]any{"name": "review"}))
	if res.IsError {
		t.Fatalf("Execute errored: %s", res.Content)
	}
	if res.Content != "Look for correctness, then style." {
		t.Errorf("Execute returned %q, want the review body", res.Content)
	}
}

func TestToolExecuteUnknownSkill(t *testing.T) {
	tl := NewTool(sampleSkills())
	res := exec(t, tl, call(t, map[string]any{"name": "nope"}))
	if !res.IsError {
		t.Fatal("activating an unknown skill must be an error result")
	}
	// The error must list the available names so the model can recover.
	if !strings.Contains(res.Content, "review") || !strings.Contains(res.Content, "commit-style") {
		t.Errorf("unknown-skill error should list available skills, got %q", res.Content)
	}
}

func TestToolExecuteMissingName(t *testing.T) {
	tl := NewTool(sampleSkills())
	res := exec(t, tl, call(t, map[string]any{}))
	if !res.IsError {
		t.Error("missing name must be an error result")
	}
}

func TestToolExecuteMalformedArgs(t *testing.T) {
	tl := NewTool(sampleSkills())
	res := exec(t, tl, session.NewToolCall("id", ToolName, json.RawMessage("{not json")))
	if !res.IsError {
		t.Error("malformed args must be an error result")
	}
}

func TestToolReadOnlyAvailableInPlanMode(t *testing.T) {
	tl := NewTool(sampleSkills())
	if !tl.ReadOnly() {
		t.Fatal("Skill tool must be read-only")
	}
	// A read-only tool must survive the plan-mode catalog filter.
	cat := tool.NewCatalog()
	cat.MustRegister(tl)
	specs := cat.Specs(session.ModePlan)
	found := false
	for _, s := range specs {
		if s.Name == ToolName {
			found = true
		}
	}
	if !found {
		t.Error("Skill tool must be available in plan mode (it is read-only)")
	}
}

func TestNewToolEmptyDescription(t *testing.T) {
	tl := NewTool(nil)
	if !strings.Contains(tl.Spec().Description, "none configured") {
		t.Errorf("empty skill set should note none configured, got %q", tl.Spec().Description)
	}
	res := exec(t, tl, call(t, map[string]any{"name": "x"}))
	if !res.IsError || !strings.Contains(res.Content, "no skills are available") {
		t.Errorf("empty tool should report no skills, got %q", res.Content)
	}
}

func TestRegisterOptIn(t *testing.T) {
	// Empty dir: nothing registered, no error.
	cat := tool.NewCatalog()
	discovered, _, err := Register(cat, "")
	if err != nil {
		t.Fatalf("Register empty dir: %v", err)
	}
	if len(discovered) != 0 {
		t.Errorf("empty dir should discover nothing, got %d", len(discovered))
	}
	if _, ok := cat.Lookup(ToolName); ok {
		t.Error("Skill tool must NOT be registered when no skills are discovered")
	}

	// A dir with one valid skill: the tool is registered.
	dir := t.TempDir()
	writeSkill(t, dir, "commit-style", validSkill)
	cat2 := tool.NewCatalog()
	discovered, skips, err := Register(cat2, dir)
	if err != nil {
		t.Fatalf("Register: %v", err)
	}
	if len(skips) != 0 {
		t.Fatalf("unexpected skips: %v", skips)
	}
	if len(discovered) != 1 {
		t.Fatalf("expected 1 skill discovered, got %d", len(discovered))
	}
	if _, ok := cat2.Lookup(ToolName); !ok {
		t.Error("Skill tool should be registered when a valid skill exists")
	}
}

func TestRegisterSourceOptInAndMerge(t *testing.T) {
	// An empty MultiSource registers nothing (preserved opt-in behaviour).
	cat := tool.NewCatalog()
	discovered, _, err := RegisterSource(context.Background(), cat, NewMultiSource())
	if err != nil {
		t.Fatalf("RegisterSource empty: %v", err)
	}
	if len(discovered) != 0 {
		t.Errorf("empty source should discover nothing, got %d", len(discovered))
	}
	if _, ok := cat.Lookup(ToolName); ok {
		t.Error("Skill tool must NOT be registered when no skills are discovered")
	}

	// Two filesystem sources with a colliding name: the EARLIER source wins, and a
	// single Skill tool is registered over the merged set.
	high := t.TempDir()
	low := t.TempDir()
	writeSkill(t, high, "dup", "---\nname: dup\ndescription: high wins\n---\nhigh body\n")
	writeSkill(t, high, "only-high", "---\nname: only-high\ndescription: h\n---\nbody\n")
	writeSkill(t, low, "dup", "---\nname: dup\ndescription: low loses\n---\nlow body\n")
	writeSkill(t, low, "only-low", "---\nname: only-low\ndescription: l\n---\nbody\n")

	cat2 := tool.NewCatalog()
	src := NewMultiSource(DirSource{Dir: high, Label: "explicit"}, DirSource{Dir: low, Label: "project"})
	discovered, skips, err := RegisterSource(context.Background(), cat2, src)
	if err != nil {
		t.Fatalf("RegisterSource: %v", err)
	}
	if len(discovered) != 3 {
		t.Fatalf("merged set should hold 3 skills (dup once), got %d: %+v", len(discovered), discovered)
	}
	// The kept "dup" must be the high-precedence one.
	var dup Skill
	for _, s := range discovered {
		if s.Name == "dup" {
			dup = s
		}
	}
	if dup.Description != "high wins" {
		t.Errorf("colliding skill should keep the higher-precedence source, got %q", dup.Description)
	}
	if !hasReasonContaining(skips, "shadowed") {
		t.Errorf("a shadow notice was expected for the dropped low-precedence dup, got %v", skips)
	}
	if _, ok := cat2.Lookup(ToolName); !ok {
		t.Error("Skill tool should be registered when the merged set is non-empty")
	}
}

func TestToolBodyTruncated(t *testing.T) {
	big := strings.Repeat("x", 30_000)
	tl := NewTool([]Skill{{Name: "big", Description: "huge", Body: big}})
	res := exec(t, tl, call(t, map[string]any{"name": "big"}))
	if res.IsError {
		t.Fatalf("Execute errored: %s", res.Content)
	}
	if len(res.Content) >= len(big) {
		t.Error("oversized skill body should be truncated to the shared output cap")
	}
	if !strings.Contains(res.Content, "truncated") {
		t.Error("truncated body should carry a truncation marker")
	}
}
