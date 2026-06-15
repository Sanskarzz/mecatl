package main

import (
	"os"
	"path/filepath"
	"testing"

	"github.com/stacklok/mecatl/internal/app"
)

// TestParseFlagsPromptInputs covers the prompt-input validation matrix: a literal
// prompt, a prompt file, both, neither (error), and an unreadable file (error).
func TestParseFlagsPromptInputs(t *testing.T) {
	dir := t.TempDir()
	good := filepath.Join(dir, "prompt.txt")
	if err := os.WriteFile(good, []byte("file body\n"), 0o600); err != nil {
		t.Fatal(err)
	}

	t.Run("literal only", func(t *testing.T) {
		f, err := parseFlags([]string{"--prompt", "hi"})
		if err != nil {
			t.Fatalf("parseFlags: %v", err)
		}
		if f.prompt != "hi" || f.promptFileBody != "" {
			t.Errorf("got prompt=%q fileBody=%q", f.prompt, f.promptFileBody)
		}
	})

	t.Run("prompt-file only", func(t *testing.T) {
		f, err := parseFlags([]string{"--prompt-file", good})
		if err != nil {
			t.Fatalf("parseFlags: %v", err)
		}
		if f.promptFileBody != "file body\n" {
			t.Errorf("fileBody = %q, want file content", f.promptFileBody)
		}
	})

	t.Run("both", func(t *testing.T) {
		f, err := parseFlags([]string{"--prompt", "lead", "--prompt-file", good})
		if err != nil {
			t.Fatalf("parseFlags: %v", err)
		}
		if f.prompt != "lead" || f.promptFileBody == "" {
			t.Errorf("both sources must be captured; got prompt=%q fileBody=%q", f.prompt, f.promptFileBody)
		}
	})

	t.Run("neither is an error", func(t *testing.T) {
		if _, err := parseFlags(nil); err == nil {
			t.Fatal("parseFlags with no prompt: want error, got nil")
		}
	})

	t.Run("unreadable file is an error", func(t *testing.T) {
		missing := filepath.Join(dir, "does-not-exist.txt")
		if _, err := parseFlags([]string{"--prompt-file", missing}); err == nil {
			t.Fatal("parseFlags with an unreadable --prompt-file: want error, got nil")
		}
	})
}

// TestAppConfigMapping is a table over the flag->app.Config mapping, including the
// deliberate headless->Interactive inversion, the --guardrails=off kill switch, and
// the ask-reviewer trio.
func TestAppConfigMapping(t *testing.T) {
	t.Run("headless default inverts to Interactive=false", func(t *testing.T) {
		f, err := parseFlags([]string{"--prompt", "x"})
		if err != nil {
			t.Fatalf("parseFlags: %v", err)
		}
		if !f.headless {
			t.Fatal("headless must default to true (the inversion from mecated)")
		}
		cfg := appConfig(f, newDiagnostics())
		if cfg.Interactive {
			t.Error("default headless must map to Interactive=false")
		}
	})

	t.Run("--headless=false maps to Interactive=true", func(t *testing.T) {
		f, err := parseFlags([]string{"--prompt", "x", "--headless=false"})
		if err != nil {
			t.Fatalf("parseFlags: %v", err)
		}
		cfg := appConfig(f, newDiagnostics())
		if !cfg.Interactive {
			t.Error("--headless=false must map to Interactive=true")
		}
	})

	t.Run("--guardrails=off sets GuardrailsDisabled", func(t *testing.T) {
		f, err := parseFlags([]string{"--prompt", "x", "--guardrails", "off", "--guardrails-model", "m"})
		if err != nil {
			t.Fatalf("parseFlags: %v", err)
		}
		if !f.guardrailsOff {
			t.Fatal("--guardrails=off must set guardrailsOff")
		}
		cfg := appConfig(f, newDiagnostics())
		if !cfg.GuardrailsDisabled {
			t.Error("guardrailsOff must map to GuardrailsDisabled=true")
		}
		if cfg.GuardrailsModel != "m" {
			t.Errorf("GuardrailsModel = %q, want m", cfg.GuardrailsModel)
		}
	})

	t.Run("guardrails left on by default", func(t *testing.T) {
		f, err := parseFlags([]string{"--prompt", "x", "--guardrails-model", "m"})
		if err != nil {
			t.Fatalf("parseFlags: %v", err)
		}
		cfg := appConfig(f, newDiagnostics())
		if cfg.GuardrailsDisabled {
			t.Error("guardrails must NOT be disabled when --guardrails is unset")
		}
	})

	t.Run("ask-reviewer trio", func(t *testing.T) {
		policy := filepath.Join(t.TempDir(), "rubric.md")
		if err := os.WriteFile(policy, []byte("RUBRIC BODY"), 0o600); err != nil {
			t.Fatal(err)
		}
		f, err := parseFlags([]string{
			"--prompt", "x",
			"--subagent-ask-reviewer", "reviewer-model",
			"--subagent-ask-reviewer-max-denies", "7",
			"--subagent-ask-reviewer-policy", policy,
		})
		if err != nil {
			t.Fatalf("parseFlags: %v", err)
		}
		cfg := appConfig(f, newDiagnostics())
		if cfg.SubagentAskReviewerModel != "reviewer-model" {
			t.Errorf("SubagentAskReviewerModel = %q", cfg.SubagentAskReviewerModel)
		}
		if cfg.SubagentAskReviewerMaxDenies != 7 {
			t.Errorf("SubagentAskReviewerMaxDenies = %d, want 7", cfg.SubagentAskReviewerMaxDenies)
		}
		if cfg.SubagentAskReviewerPolicy != "RUBRIC BODY" {
			t.Errorf("SubagentAskReviewerPolicy = %q, want the file CONTENT", cfg.SubagentAskReviewerPolicy)
		}
	})

	t.Run("core knobs pass through", func(t *testing.T) {
		f, err := parseFlags([]string{
			"--prompt", "x",
			"--workspace", "/repo",
			"--model", "gpt-x",
			"--mock",
			"--max-run-tokens", "1234",
			"--max-team-tokens", "5678",
			"--no-bash",
			"--posture", "auto",
		})
		if err != nil {
			t.Fatalf("parseFlags: %v", err)
		}
		if !f.postureFlagSet {
			t.Error("explicit --posture must set postureFlagSet")
		}
		cfg := appConfig(f, newDiagnostics())
		if cfg.Workspace != "/repo" || cfg.Model != "gpt-x" || !cfg.UseMock {
			t.Errorf("core knobs not mapped: %+v", cfg)
		}
		if cfg.MaxRunTokens != 1234 || cfg.MaxTeamTokens != 5678 {
			t.Errorf("budgets not mapped: run=%d team=%d", cfg.MaxRunTokens, cfg.MaxTeamTokens)
		}
		if !cfg.NoBash {
			t.Error("--no-bash not mapped")
		}
		if cfg.Posture != app.PostureAuto {
			t.Errorf("Posture = %v, want auto", cfg.Posture)
		}
		if !cfg.PostureFlagSet {
			t.Error("PostureFlagSet not mapped")
		}
	})
}
