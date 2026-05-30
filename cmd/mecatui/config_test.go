package main

import (
	"path/filepath"
	"testing"
)

// TestParseFlagsDefaults asserts --server defaults to empty (AUTO: probe-then-embed)
// and that an empty workspace resolves to an absolute path (cwd).
func TestParseFlagsDefaults(t *testing.T) {
	cfg, err := parseFlags(nil)
	if err != nil {
		t.Fatalf("parseFlags: %v", err)
	}
	if cfg.server != "" {
		t.Errorf("server = %q, want \"\" (auto)", cfg.server)
	}
	if !filepath.IsAbs(cfg.workspace) {
		t.Errorf("workspace = %q, want absolute", cfg.workspace)
	}
}

// TestParseFlagsWorkspaceAbs asserts a relative --workspace is made absolute.
func TestParseFlagsWorkspaceAbs(t *testing.T) {
	cfg, err := parseFlags([]string{"-workspace", "rel/dir"})
	if err != nil {
		t.Fatalf("parseFlags: %v", err)
	}
	if !filepath.IsAbs(cfg.workspace) {
		t.Errorf("workspace = %q, want absolute", cfg.workspace)
	}
}

// TestParseFlagsAuthEnv asserts MECATL_AUTH_TOKEN is picked up when the flag is
// unset.
func TestParseFlagsAuthEnv(t *testing.T) {
	t.Setenv("MECATL_AUTH_TOKEN", "tok-123")
	cfg, err := parseFlags(nil)
	if err != nil {
		t.Fatalf("parseFlags: %v", err)
	}
	if cfg.authToken != "tok-123" {
		t.Errorf("authToken = %q, want tok-123", cfg.authToken)
	}
}

// TestValidateMode rejects unknown modes and accepts the three valid ones. The
// configs set mock so the embedded-provider check (validated last) passes and the
// test stays focused on mode handling.
func TestValidateMode(t *testing.T) {
	for _, mode := range []string{"default", "plan", "accept-edits"} {
		cfg := config{workspace: "/abs", mode: mode, mock: true}
		if err := cfg.validate(); err != nil {
			t.Errorf("mode %q rejected: %v", mode, err)
		}
	}
	bad := config{workspace: "/abs", mode: "nope", mock: true}
	if err := bad.validate(); err == nil {
		t.Error("invalid mode accepted")
	}
}

// TestValidateWorkspaceRequired asserts a non-absolute or empty workspace fails
// validation (the server requires absolute).
func TestValidateWorkspaceRequired(t *testing.T) {
	if err := (config{mode: "default"}).validate(); err == nil {
		t.Error("empty workspace accepted")
	}
	if err := (config{workspace: "rel", mode: "default"}).validate(); err == nil {
		t.Error("relative workspace accepted")
	}
}

// TestValidateEmbeddedProviderRequired asserts that with no external --server the
// embedded path needs a resolvable provider (OpenAI key or --mock), and that an
// external server or a provider satisfies the check.
func TestValidateEmbeddedProviderRequired(t *testing.T) {
	// No server, no key, no mock -> error (cannot host an embedded server).
	if err := (config{workspace: "/abs", mode: "default"}).validate(); err == nil {
		t.Error("expected an error when embedding with no provider")
	}
	// --mock resolves the provider.
	if err := (config{workspace: "/abs", mode: "default", mock: true}).validate(); err != nil {
		t.Errorf("--mock should satisfy the provider check: %v", err)
	}
	// An OpenAI key resolves the provider.
	if err := (config{workspace: "/abs", mode: "default", openAIKey: "sk-x"}).validate(); err != nil {
		t.Errorf("OPENAI_API_KEY should satisfy the provider check: %v", err)
	}
	// An external server means no embedded provider is needed.
	if err := (config{workspace: "/abs", mode: "default", server: "127.0.0.1:8080"}).validate(); err != nil {
		t.Errorf("an external --server should not require a provider: %v", err)
	}
}
