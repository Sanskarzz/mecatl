package main

import (
	"io"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/stacklok/mecatl/internal/adapter/skills"
)

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

// TestParseFlagsAgentDefs asserts the Tier 1b agent-definition flags parse into the
// config: repeatable --agents-dir, the conventional toggle (default ON), the global
// --subagent-model, and repeatable key=value --model-alias.
func TestParseFlagsAgentDefs(t *testing.T) {
	// Defaults: conventional discovery ON (inert when absent), no explicit dirs.
	def, err := parseFlags(nil)
	if err != nil {
		t.Fatalf("parseFlags(nil): %v", err)
	}
	if !def.agentsConventional {
		t.Errorf("agentsConventional default = false, want true (on-but-inert)")
	}
	if len(def.agentsDirs) != 0 {
		t.Errorf("agentsDirs default = %v, want empty", def.agentsDirs)
	}

	cfg, err := parseFlags([]string{
		"--agents-dir", "/a/one",
		"--agents-dir", "/a/two",
		"--agents-conventional=false",
		"--subagent-model", "cheap-id",
		"--model-alias", "fast=gpt-4o-mini",
		"--model-alias", "smart=gpt-5",
	})
	if err != nil {
		t.Fatalf("parseFlags: %v", err)
	}
	if got := []string(cfg.agentsDirs); len(got) != 2 || got[0] != "/a/one" || got[1] != "/a/two" {
		t.Errorf("agentsDirs = %v, want [/a/one /a/two]", got)
	}
	if cfg.agentsConventional {
		t.Errorf("agentsConventional = true, want false (explicitly disabled)")
	}
	if cfg.subagentModel != "cheap-id" {
		t.Errorf("subagentModel = %q, want cheap-id", cfg.subagentModel)
	}
	if cfg.modelAliases["fast"] != "gpt-4o-mini" || cfg.modelAliases["smart"] != "gpt-5" {
		t.Errorf("modelAliases = %v, want fast=gpt-4o-mini smart=gpt-5", cfg.modelAliases)
	}

	// A malformed alias (no '=') is a parse error.
	if _, err := parseFlags([]string{"--model-alias", "bogus"}); err == nil {
		t.Error("parseFlags(--model-alias bogus) should error on a missing '='")
	}
}

// TestAppConfigMapsAgentDefs asserts appConfig threads the agent-def fields onto the
// shared app.Config.
func TestAppConfigMapsAgentDefs(t *testing.T) {
	cfg, err := parseFlags([]string{
		"--agents-dir", "/x",
		"--subagent-model", "sub",
		"--model-alias", "fast=cheap",
	})
	if err != nil {
		t.Fatalf("parseFlags: %v", err)
	}
	ac := appConfig(cfg, nil, nil)
	if len(ac.AgentsDirs) != 1 || ac.AgentsDirs[0] != "/x" {
		t.Errorf("AgentsDirs = %v", ac.AgentsDirs)
	}
	if !ac.AgentsConventional {
		t.Errorf("AgentsConventional = false, want true (default)")
	}
	if ac.SubagentModel != "sub" {
		t.Errorf("SubagentModel = %q", ac.SubagentModel)
	}
	if ac.ModelAliases["fast"] != "cheap" {
		t.Errorf("ModelAliases = %v", ac.ModelAliases)
	}
}
