package main

import (
	"context"
	"encoding/json"
	"io"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/stacklok/mecatl/internal/adapter/permpolicy"
	"github.com/stacklok/mecatl/internal/adapter/skills"
	"github.com/stacklok/mecatl/internal/governance"
	"github.com/stacklok/mecatl/internal/session"
	"github.com/stacklok/mecatl/internal/tool"
)

// TestValidateSkillDraftConfig pins the STRUCTURAL trust boundary: the quarantine
// must live outside the workspace root (so the model's workspace-confined Write/Edit
// cannot reach it) and be disjoint from every active skills dir. The previous
// approach (governance deny rules on raw model paths) was removed: it matched
// absolute deny patterns against the workspace-RELATIVE paths the tools actually
// use, so it never fired — a false boundary. Structural confinement replaces it.
func TestValidateSkillDraftConfig(t *testing.T) {
	t.Run("disabled yields no error", func(t *testing.T) {
		if err := validateSkillDraftConfig(config{workspace: t.TempDir()}); err != nil {
			t.Fatalf("disabled draft must not error: %v", err)
		}
	})

	t.Run("quarantine inside the workspace is fatal", func(t *testing.T) {
		ws := t.TempDir()
		cfg := config{workspace: ws, skillsDraftDir: filepath.Join(ws, "quarantine")}
		if err := validateSkillDraftConfig(cfg); err == nil {
			t.Fatal("expected a fatal error when the quarantine is inside the workspace")
		}
	})

	t.Run("quarantine overlapping an active skills dir is fatal", func(t *testing.T) {
		base := t.TempDir()
		ws := filepath.Join(base, "ws")
		shared := filepath.Join(base, "shared")
		cfg := config{
			workspace:      ws,
			skillsDirs:     stringList{shared},
			skillsDraftDir: filepath.Join(shared, "quarantine"), // outside ws, but inside an active dir
		}
		if err := validateSkillDraftConfig(cfg); err == nil {
			t.Fatal("expected a fatal error when the quarantine overlaps an active skills dir")
		}
	})

	t.Run("outside-workspace, disjoint quarantine is accepted", func(t *testing.T) {
		base := t.TempDir()
		cfg := config{
			workspace:      filepath.Join(base, "ws"),
			skillsDirs:     stringList{filepath.Join(base, "active")},
			skillsDraftDir: filepath.Join(base, "quarantine"),
		}
		if err := validateSkillDraftConfig(cfg); err != nil {
			t.Fatalf("a valid out-of-workspace, disjoint quarantine must be accepted: %v", err)
		}
	})
}

func TestDefaultRulesSkillDraftAsks(t *testing.T) {
	policy := permpolicy.NewPolicy(defaultRules())
	got := policy.Evaluate(context.Background(), session.ModeDefault,
		session.NewToolCall("id", skills.DraftToolName, json.RawMessage(`{}`)))
	if got.Effect != governance.Ask {
		t.Fatalf("SkillDraft should default to Ask, got %v", got.Effect)
	}
}

func TestRegisterSkillDraftGating(t *testing.T) {
	t.Run("disabled when no draft dir", func(t *testing.T) {
		cat := tool.NewCatalog()
		registerSkillDraft(config{}, cat, nil)
		if _, ok := cat.Lookup(skills.DraftToolName); ok {
			t.Error("SkillDraft must NOT be registered without --skills-draft-dir")
		}
	})
	t.Run("registered when draft dir set", func(t *testing.T) {
		cat := tool.NewCatalog()
		registerSkillDraft(config{skillsDraftDir: t.TempDir(), skillsDraftThreshold: 0.5}, cat, nil)
		if _, ok := cat.Lookup(skills.DraftToolName); !ok {
			t.Error("SkillDraft must be registered when --skills-draft-dir is set")
		}
	})
}

func TestDirsOverlap(t *testing.T) {
	cases := []struct {
		a, b string
		want bool
	}{
		{"/a/b", "/a/b", true},
		{"/a", "/a/b", true},
		{"/a/b", "/a", true},
		{"/a/b", "/a/c", false},
		{"/a", "/ab", false},
	}
	for _, tc := range cases {
		if got := dirsOverlap(tc.a, tc.b); got != tc.want {
			t.Errorf("dirsOverlap(%q,%q) = %v, want %v", tc.a, tc.b, got, tc.want)
		}
	}
}

// seedQuarantine writes a model-drafted-looking SKILL.md (with origin: model
// provenance) into <quarantine>/<name>/SKILL.md so the promote CLI has something
// to review and move.
func seedQuarantine(t *testing.T, quarantine, name, body string) {
	t.Helper()
	dir := filepath.Join(quarantine, name)
	if err := os.MkdirAll(dir, 0o755); err != nil {
		t.Fatal(err)
	}
	content := "---\nname: " + name + "\ndescription: \"a seeded candidate\"\norigin: model\ndrafted_at: 2026-01-01T00:00:00Z\n---\n\n" + body + "\n"
	if err := os.WriteFile(filepath.Join(dir, skills.SkillFileName), []byte(content), 0o644); err != nil {
		t.Fatal(err)
	}
}

func TestRunSkillsPromote(t *testing.T) {
	t.Run("missing flags is a usage error", func(t *testing.T) {
		err := runSkillsPromote([]string{}, strings.NewReader(""), io.Discard)
		if err == nil {
			t.Fatal("expected a usage error with no name/dirs")
		}
	})

	t.Run("--yes promotes a valid candidate", func(t *testing.T) {
		base := t.TempDir()
		quarantine := filepath.Join(base, "quarantine")
		active := filepath.Join(base, "active")
		seedQuarantine(t, quarantine, "deploy-thing", "1. do it\nDone when: done.")

		err := runSkillsPromote(
			[]string{"--skills-draft-dir", quarantine, "--skills-dir", active, "--yes", "deploy-thing"},
			strings.NewReader(""), io.Discard)
		if err != nil {
			t.Fatalf("promote: %v", err)
		}
		if _, statErr := os.Stat(filepath.Join(active, "deploy-thing", skills.SkillFileName)); statErr != nil {
			t.Fatalf("promoted skill not present under active: %v", statErr)
		}
		if _, statErr := os.Stat(filepath.Join(quarantine, "deploy-thing")); !os.IsNotExist(statErr) {
			t.Fatal("candidate should have been moved out of quarantine")
		}
	})

	t.Run("interactive 'n' aborts the promotion", func(t *testing.T) {
		base := t.TempDir()
		quarantine := filepath.Join(base, "quarantine")
		active := filepath.Join(base, "active")
		seedQuarantine(t, quarantine, "risky", "1. step\nDone when: ok.")

		err := runSkillsPromote(
			[]string{"--skills-draft-dir", quarantine, "--skills-dir", active, "risky"},
			strings.NewReader("n\n"), io.Discard)
		if err == nil {
			t.Fatal("expected an abort error when the operator declines")
		}
		if _, statErr := os.Stat(filepath.Join(active, "risky")); !os.IsNotExist(statErr) {
			t.Fatal("a declined candidate must not be promoted")
		}
	})
}

// sanity: the skills-draft-dir flag parses and the threshold default is wired.
func TestParseFlagsSkillsDraft(t *testing.T) {
	cfg, err := parseFlags([]string{"--skills-draft-dir", "/tmp/q"})
	if err != nil {
		t.Fatalf("parseFlags: %v", err)
	}
	if cfg.skillsDraftDir != "/tmp/q" {
		t.Errorf("skillsDraftDir = %q", cfg.skillsDraftDir)
	}
	if cfg.skillsDraftThreshold != skills.DefaultSimilarityThreshold {
		t.Errorf("default threshold = %v, want %v", cfg.skillsDraftThreshold, skills.DefaultSimilarityThreshold)
	}
}
