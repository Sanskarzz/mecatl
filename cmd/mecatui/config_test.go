package main

import (
	"path/filepath"
	"strings"
	"testing"

	"github.com/adrg/xdg"
)

// TestEmbeddedConfigEnablesAgentDefs asserts the embedded server enables conventional
// agent-definition discovery (consistent with EnableTeams/EnableFork; inert until a
// <name>.md exists under a conventional dir).
func TestEmbeddedConfigEnablesAgentDefs(t *testing.T) {
	ac := embeddedConfig(config{workspace: "/ws", model: "m", mock: true})
	if !ac.AgentsConventional {
		t.Error("embeddedConfig AgentsConventional = false, want true")
	}
	if !ac.EnableTeams || !ac.EnableFork {
		t.Errorf("embeddedConfig should also keep teams/fork on (teams=%v fork=%v)", ac.EnableTeams, ac.EnableFork)
	}
}

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

// TestParseFlagsNoAltScreen asserts the inline opt-out is off by default and is
// set by either --no-alt-screen or its --inline alias.
func TestParseFlagsNoAltScreen(t *testing.T) {
	cfg, err := parseFlags(nil)
	if err != nil {
		t.Fatalf("parseFlags: %v", err)
	}
	if cfg.noAltScreen {
		t.Error("noAltScreen = true by default, want false (full-screen alt screen)")
	}
	for _, flag := range []string{"-no-alt-screen", "-inline"} {
		cfg, err := parseFlags([]string{flag})
		if err != nil {
			t.Fatalf("parseFlags(%q): %v", flag, err)
		}
		if !cfg.noAltScreen {
			t.Errorf("%s did not set noAltScreen", flag)
		}
	}
}

// TestParseFlagsMemoryDefaults asserts the memory flags default to off/empty;
// the per-project default PATH is computed later in embeddedConfig, not here.
func TestParseFlagsMemoryDefaults(t *testing.T) {
	cfg, err := parseFlags(nil)
	if err != nil {
		t.Fatalf("parseFlags: %v", err)
	}
	if cfg.memoryDir != "" {
		t.Errorf("memoryDir = %q, want \"\" (default computed in embeddedConfig)", cfg.memoryDir)
	}
	if cfg.noMemory {
		t.Error("noMemory = true by default, want false (memory on)")
	}
}

// TestParseFlagsMemoryFlags asserts --memory-dir and --no-memory map onto the
// config fields.
func TestParseFlagsMemoryFlags(t *testing.T) {
	cfg, err := parseFlags([]string{"-memory-dir", "/tmp/mem", "-no-memory"})
	if err != nil {
		t.Fatalf("parseFlags: %v", err)
	}
	if cfg.memoryDir != "/tmp/mem" {
		t.Errorf("memoryDir = %q, want /tmp/mem", cfg.memoryDir)
	}
	if !cfg.noMemory {
		t.Error("--no-memory did not set noMemory")
	}
}

// TestResolveMemoryDirPrecedence covers the precedence table: --no-memory wins,
// then an explicit --memory-dir, then the computed per-project default.
// setDataHome points adrg/xdg's DataHome at dir for the duration of the test.
// adrg/xdg snapshots the environment at package init, so a bare t.Setenv is NOT
// reflected — Reload() must re-read the env. The cleanup re-reads it again so the
// global does not leak a synthetic path into a later test.
func setDataHome(t *testing.T, dir string) {
	t.Helper()
	t.Setenv("XDG_DATA_HOME", dir)
	xdg.Reload()
	t.Cleanup(xdg.Reload)
}

func TestResolveMemoryDirPrecedence(t *testing.T) {
	setDataHome(t, "/xdg/data")

	// --no-memory wins even over an explicit --memory-dir.
	if got := resolveMemoryDir(config{workspace: "/ws", memoryDir: "/x", noMemory: true}); got != "" {
		t.Errorf("--no-memory should win: got %q, want \"\"", got)
	}
	// explicit --memory-dir overrides the default.
	if got := resolveMemoryDir(config{workspace: "/ws", memoryDir: "/x"}); got != "/x" {
		t.Errorf("explicit --memory-dir: got %q, want /x", got)
	}
	// neither -> computed default under XDG data.
	got := resolveMemoryDir(config{workspace: "/var/home/ozz/dev/mecatl"})
	want := filepath.Join("/xdg/data", "mecatui", "memory", "-var-home-ozz-dev-mecatl")
	if got != want {
		t.Errorf("default: got %q, want %q", got, want)
	}
}

// TestDefaultMemoryDirPathSlug asserts the leaf is the full path-slug (separator
// replaced by '-', leading separator preserved as a leading '-'), under
// xdg.DataHome/mecatui/memory.
func TestDefaultMemoryDirPathSlug(t *testing.T) {
	setDataHome(t, "/xdg/data")
	got := defaultMemoryDir("/var/home/jaosorior/Development/stacklok/mecatl")
	want := filepath.Join("/xdg/data", "mecatui", "memory",
		"-var-home-jaosorior-Development-stacklok-mecatl")
	if got != want {
		t.Errorf("defaultMemoryDir: got %q, want %q", got, want)
	}
	if !strings.HasPrefix(filepath.Base(got), "-") {
		t.Errorf("leaf %q should preserve the leading separator as a leading '-'", filepath.Base(got))
	}
}

// TestDefaultMemoryDirIsPerProject asserts distinct workspaces (even same
// basename) map to distinct leaves, while the same workspace is deterministic.
func TestDefaultMemoryDirIsPerProject(t *testing.T) {
	setDataHome(t, "/xdg/data")
	a := defaultMemoryDir("/a/proj")
	b := defaultMemoryDir("/b/proj")
	if a == b {
		t.Errorf("same-basename workspaces in different parents collided: %q == %q", a, b)
	}
	if again := defaultMemoryDir("/a/proj"); again != a {
		t.Errorf("defaultMemoryDir is not deterministic: %q != %q", again, a)
	}
}

// TestDefaultMemoryDirUsesLocalShareFallback asserts that with XDG_DATA_HOME
// unset, adrg/xdg falls back to ~/.local/share under a resolvable HOME.
func TestDefaultMemoryDirUsesLocalShareFallback(t *testing.T) {
	t.Setenv("XDG_DATA_HOME", "")
	t.Setenv("HOME", "/home/tester")
	xdg.Reload()
	t.Cleanup(xdg.Reload)
	got := defaultMemoryDir("/ws/proj")
	want := filepath.Join("/home/tester", ".local", "share", "mecatui", "memory", "-ws-proj")
	if got != want {
		t.Errorf("fallback base: got %q, want %q", got, want)
	}
}

// TestDefaultMemoryDirDegrades asserts the degraded guards return "" (memory off)
// rather than anchoring a store at a bogus path: an empty workspace, and (as a
// defensive belt) an empty xdg.DataHome. Note adrg/xdg practically always resolves
// a non-empty DataHome (it falls back to ~/.local/share, and to "/.local/share"
// even with no HOME), so the empty-DataHome branch is defensive; the empty-
// workspace branch is the live degraded path.
func TestDefaultMemoryDirDegrades(t *testing.T) {
	setDataHome(t, "/xdg/data")
	if got := defaultMemoryDir(""); got != "" {
		t.Errorf("empty workspace should yield \"\", got %q", got)
	}

	// Defensive: an empty DataHome disables memory rather than producing a
	// root-anchored "/mecatui/memory/..." path.
	setDataHome(t, "")
	xdg.DataHome = "" // adrg/xdg never yields "" itself; force the guard's input.
	if got := defaultMemoryDir("/ws/proj"); got != "" {
		t.Errorf("empty DataHome should yield \"\" (disabled), got %q", got)
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
