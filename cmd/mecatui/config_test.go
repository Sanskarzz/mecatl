package main

import (
	"path/filepath"
	"testing"
)

// TestParseFlagsDefaults asserts the loopback default server and that an empty
// workspace resolves to an absolute path (cwd).
func TestParseFlagsDefaults(t *testing.T) {
	cfg, err := parseFlags(nil)
	if err != nil {
		t.Fatalf("parseFlags: %v", err)
	}
	if cfg.server != defaultServer {
		t.Errorf("server = %q, want %q", cfg.server, defaultServer)
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

// TestValidateMode rejects unknown modes and accepts the three valid ones.
func TestValidateMode(t *testing.T) {
	for _, mode := range []string{"default", "plan", "accept-edits"} {
		cfg := config{workspace: "/abs", mode: mode}
		if err := cfg.validate(); err != nil {
			t.Errorf("mode %q rejected: %v", mode, err)
		}
	}
	bad := config{workspace: "/abs", mode: "nope"}
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
