package cliconfig

import (
	"flag"
	"testing"

	"github.com/stacklok/mecatl/internal/app"
)

// TestRegisterProviderFlagsRegistersThree proves all three base-URL flags are
// registered on the passed FlagSet with the supplied (or defaulted) help text.
func TestRegisterProviderFlagsRegistersThree(t *testing.T) {
	fs := flag.NewFlagSet("t", flag.ContinueOnError)
	_ = RegisterProviderFlags(fs, ProviderFlagHelp{})
	for _, name := range []string{"openai-base-url", "openrouter-base-url", "anthropic-base-url"} {
		f := fs.Lookup(name)
		if f == nil {
			t.Fatalf("flag --%s not registered", name)
		}
		if f.Usage == "" {
			t.Errorf("flag --%s has empty help (default fallback should fill it)", name)
		}
	}
	// A zero ProviderFlagHelp uses the mecated-style defaults.
	if got := fs.Lookup("openai-base-url").Usage; got != DefaultProviderFlagHelp.OpenAIBaseURL {
		t.Errorf("openai-base-url help = %q, want the default", got)
	}
}

// TestRegisterProviderFlagsHelpOverride proves a supplied help string wins over the
// default (so mecated/mecatui keep their own wording).
func TestRegisterProviderFlagsHelpOverride(t *testing.T) {
	fs := flag.NewFlagSet("t", flag.ContinueOnError)
	_ = RegisterProviderFlags(fs, ProviderFlagHelp{OpenAIBaseURL: "CUSTOM HELP"})
	if got := fs.Lookup("openai-base-url").Usage; got != "CUSTOM HELP" {
		t.Errorf("openai-base-url help = %q, want CUSTOM HELP", got)
	}
	// The unspecified ones still fall back to the default.
	if got := fs.Lookup("anthropic-base-url").Usage; got != DefaultProviderFlagHelp.AnthropicBaseURL {
		t.Errorf("anthropic-base-url help = %q, want the default", got)
	}
}

// TestApplyMapsAllSixFields proves the env keys and the parsed base URLs land on every
// one of the six app.Config fields, for all three providers.
func TestApplyMapsAllSixFields(t *testing.T) {
	t.Setenv(envOpenAIKey, "sk-openai")
	t.Setenv(envOpenRouterKey, "sk-openrouter")
	t.Setenv(envAnthropicKey, "sk-anthropic")

	fs := flag.NewFlagSet("t", flag.ContinueOnError)
	pf := RegisterProviderFlags(fs, ProviderFlagHelp{})
	if err := fs.Parse([]string{
		"--openai-base-url", "https://oai.example",
		"--openrouter-base-url", "https://or.example",
		"--anthropic-base-url", "https://ant.example",
	}); err != nil {
		t.Fatalf("parse: %v", err)
	}

	var cfg app.Config
	keys := pf.Apply(&cfg)

	if cfg.OpenAIKey != "sk-openai" || cfg.OpenRouterKey != "sk-openrouter" || cfg.AnthropicKey != "sk-anthropic" {
		t.Errorf("keys not mapped: %q / %q / %q", cfg.OpenAIKey, cfg.OpenRouterKey, cfg.AnthropicKey)
	}
	if cfg.OpenAIBaseURL != "https://oai.example" || cfg.OpenRouterBaseURL != "https://or.example" || cfg.AnthropicBaseURL != "https://ant.example" {
		t.Errorf("base URLs not mapped: %q / %q / %q", cfg.OpenAIBaseURL, cfg.OpenRouterBaseURL, cfg.AnthropicBaseURL)
	}
	if !keys.Any() {
		t.Errorf("returned keys should be present; Any()=%v", keys.Any())
	}
	if keys.OpenAI != "sk-openai" || keys.OpenRouter != "sk-openrouter" || keys.Anthropic != "sk-anthropic" {
		t.Errorf("returned ResolvedKeys mismatch: %+v", keys)
	}
}

// TestApplyEmptyEnvLeavesEmptyFields proves an unset environment yields empty key
// fields (and Any() is false), so a caller's "no provider configured" guard works.
func TestApplyEmptyEnvLeavesEmptyFields(t *testing.T) {
	t.Setenv(envOpenAIKey, "")
	t.Setenv(envOpenRouterKey, "")
	t.Setenv(envAnthropicKey, "")

	fs := flag.NewFlagSet("t", flag.ContinueOnError)
	pf := RegisterProviderFlags(fs, ProviderFlagHelp{})
	if err := fs.Parse(nil); err != nil {
		t.Fatalf("parse: %v", err)
	}
	var cfg app.Config
	keys := pf.Apply(&cfg)

	if cfg.OpenAIKey != "" || cfg.OpenRouterKey != "" || cfg.AnthropicKey != "" {
		t.Errorf("empty env must leave empty key fields; got %q / %q / %q", cfg.OpenAIKey, cfg.OpenRouterKey, cfg.AnthropicKey)
	}
	if cfg.OpenAIBaseURL != "" || cfg.OpenRouterBaseURL != "" || cfg.AnthropicBaseURL != "" {
		t.Errorf("unset base URLs must be empty; got %q / %q / %q", cfg.OpenAIBaseURL, cfg.OpenRouterBaseURL, cfg.AnthropicBaseURL)
	}
	if keys.Any() {
		t.Error("ResolvedKeys.Any() must be false with no credentials")
	}
}

// TestReadProviderKeysIsTheSingleSeam proves ReadProviderKeys reads the same three env
// vars Apply uses — the one definition both the guard path and the wiring path share.
func TestReadProviderKeysIsTheSingleSeam(t *testing.T) {
	t.Setenv(envOpenAIKey, "a")
	t.Setenv(envOpenRouterKey, "b")
	t.Setenv(envAnthropicKey, "c")
	keys := ReadProviderKeys()
	if keys.OpenAI != "a" || keys.OpenRouter != "b" || keys.Anthropic != "c" {
		t.Errorf("ReadProviderKeys mismatch: %+v", keys)
	}
}
