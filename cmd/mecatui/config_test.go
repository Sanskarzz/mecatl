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

// TestParseFlagsCommandDefaults asserts slash commands are on by default (no flags
// set) — the resolution to "enabled with the conventional dirs" happens in
// embeddedConfig, so the raw config fields are empty/false here.
func TestParseFlagsCommandDefaults(t *testing.T) {
	cfg, err := parseFlags(nil)
	if err != nil {
		t.Fatalf("parseFlags: %v", err)
	}
	if cfg.commandsDir != "" {
		t.Errorf("commandsDir = %q, want \"\" (default resolved in embeddedConfig)", cfg.commandsDir)
	}
	if cfg.noCommands {
		t.Error("noCommands = true by default, want false (commands on)")
	}
}

// TestParseFlagsCommandFlags asserts --commands-dir and --no-commands map onto the
// config fields.
func TestParseFlagsCommandFlags(t *testing.T) {
	cfg, err := parseFlags([]string{"-commands-dir", "/tmp/cmds", "-no-commands"})
	if err != nil {
		t.Fatalf("parseFlags: %v", err)
	}
	if cfg.commandsDir != "/tmp/cmds" {
		t.Errorf("commandsDir = %q, want /tmp/cmds", cfg.commandsDir)
	}
	if !cfg.noCommands {
		t.Error("--no-commands did not set noCommands")
	}
}

// TestResolveCommands covers the slash-command precedence: --no-commands disables
// (wins over an explicit dir); an explicit --commands-dir overrides; otherwise
// commands are on with the conventional dirs (empty dir, enabled).
func TestResolveCommands(t *testing.T) {
	t.Parallel()
	// --no-commands wins, even with a dir set.
	if dir, enable := resolveCommands(config{commandsDir: "/x", noCommands: true}); dir != "" || enable {
		t.Errorf("no-commands: got (%q, %v), want (\"\", false)", dir, enable)
	}
	// explicit dir overrides.
	if dir, enable := resolveCommands(config{commandsDir: "/x"}); dir != "/x" || !enable {
		t.Errorf("commands-dir: got (%q, %v), want (\"/x\", true)", dir, enable)
	}
	// default: on with the conventional dirs (empty dir).
	if dir, enable := resolveCommands(config{}); dir != "" || !enable {
		t.Errorf("default: got (%q, %v), want (\"\", true)", dir, enable)
	}
}

// TestEmbeddedConfigCommands asserts embeddedConfig turns slash commands ON by
// default and that --no-commands turns them fully off.
func TestEmbeddedConfigCommands(t *testing.T) {
	on := embeddedConfig(config{workspace: "/ws", model: "m", mock: true})
	if !on.EnableCommands || on.CommandsDir != "" {
		t.Errorf("default: EnableCommands=%v CommandsDir=%q, want (true, \"\")", on.EnableCommands, on.CommandsDir)
	}
	off := embeddedConfig(config{workspace: "/ws", model: "m", mock: true, noCommands: true})
	if off.EnableCommands || off.CommandsDir != "" {
		t.Errorf("--no-commands: EnableCommands=%v CommandsDir=%q, want (false, \"\")", off.EnableCommands, off.CommandsDir)
	}
}

// TestParseFlagsSkillDefaults asserts skill discovery is on by default (no flags) —
// the resolution to conventional discovery happens in embeddedConfig, so the raw
// config fields are empty/false here.
func TestParseFlagsSkillDefaults(t *testing.T) {
	cfg, err := parseFlags(nil)
	if err != nil {
		t.Fatalf("parseFlags: %v", err)
	}
	if cfg.skillsDir != "" {
		t.Errorf("skillsDir = %q, want \"\" (default resolved in embeddedConfig)", cfg.skillsDir)
	}
	if cfg.noSkills {
		t.Error("noSkills = true by default, want false (skills on)")
	}
}

// TestParseFlagsSkillFlags asserts --skills-dir and --no-skills map onto the config.
func TestParseFlagsSkillFlags(t *testing.T) {
	cfg, err := parseFlags([]string{"-skills-dir", "/tmp/skills", "-no-skills"})
	if err != nil {
		t.Fatalf("parseFlags: %v", err)
	}
	if cfg.skillsDir != "/tmp/skills" {
		t.Errorf("skillsDir = %q, want /tmp/skills", cfg.skillsDir)
	}
	if !cfg.noSkills {
		t.Error("--no-skills did not set noSkills")
	}
}

// TestResolveSkills covers the skill precedence: --no-skills disables (wins over an
// explicit dir); an explicit --skills-dir scopes to that one dir (no conventional);
// otherwise conventional discovery is on (nil dirs, true).
func TestResolveSkills(t *testing.T) {
	t.Parallel()
	// --no-skills wins, even with a dir set.
	if dirs, conv := resolveSkills(config{skillsDir: "/x", noSkills: true}); dirs != nil || conv {
		t.Errorf("no-skills: got (%v, %v), want (nil, false)", dirs, conv)
	}
	// explicit dir scopes discovery, no conventional.
	if dirs, conv := resolveSkills(config{skillsDir: "/x"}); len(dirs) != 1 || dirs[0] != "/x" || conv {
		t.Errorf("skills-dir: got (%v, %v), want ([/x], false)", dirs, conv)
	}
	// default: conventional discovery on, no explicit dirs.
	if dirs, conv := resolveSkills(config{}); dirs != nil || !conv {
		t.Errorf("default: got (%v, %v), want (nil, true)", dirs, conv)
	}
}

// TestEmbeddedConfigSkills asserts embeddedConfig turns conventional skill discovery
// ON by default and that --no-skills turns it off (no dirs, no conventional).
func TestEmbeddedConfigSkills(t *testing.T) {
	on := embeddedConfig(config{workspace: "/ws", model: "m", mock: true})
	if !on.SkillsConventional || on.SkillsDirs != nil {
		t.Errorf("default: SkillsConventional=%v SkillsDirs=%v, want (true, nil)", on.SkillsConventional, on.SkillsDirs)
	}
	off := embeddedConfig(config{workspace: "/ws", model: "m", mock: true, noSkills: true})
	if off.SkillsConventional || off.SkillsDirs != nil {
		t.Errorf("--no-skills: SkillsConventional=%v SkillsDirs=%v, want (false, nil)", off.SkillsConventional, off.SkillsDirs)
	}
}

// TestDefaultMemoryDir is a pure table test of defaultMemoryDir: it reads no
// globals (the dataHome base is injected), so it needs no env/xdg.Reload dance and
// is safe to run in parallel. Covers the path-slug encoding, per-project
// disambiguation, determinism, and both degraded guards (empty base, empty
// workspace).
func TestDefaultMemoryDir(t *testing.T) {
	t.Parallel()
	cases := []struct {
		name         string
		dataHome, ws string
		want         string
	}{
		{
			name:     "path slug under data home",
			dataHome: "/xdg/data",
			ws:       "/var/home/jaosorior/Development/stacklok/mecatl",
			want:     filepath.Join("/xdg/data", "mecatui", "memory", "-var-home-jaosorior-Development-stacklok-mecatl"),
		},
		{
			name:     "local-share fallback base (resolved by the caller)",
			dataHome: "/home/tester/.local/share",
			ws:       "/ws/proj",
			want:     filepath.Join("/home/tester/.local/share", "mecatui", "memory", "-ws-proj"),
		},
		{
			name:     "distinct parents, same basename -> distinct leaves (a)",
			dataHome: "/xdg/data",
			ws:       "/a/proj",
			want:     filepath.Join("/xdg/data", "mecatui", "memory", "-a-proj"),
		},
		{
			name:     "distinct parents, same basename -> distinct leaves (b)",
			dataHome: "/xdg/data",
			ws:       "/b/proj",
			want:     filepath.Join("/xdg/data", "mecatui", "memory", "-b-proj"),
		},
		{
			name:     "empty data home degrades to disabled",
			dataHome: "",
			ws:       "/ws/proj",
			want:     "",
		},
		{
			name:     "empty workspace degrades to disabled",
			dataHome: "/xdg/data",
			ws:       "",
			want:     "",
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			got := defaultMemoryDir(tc.dataHome, tc.ws)
			if got != tc.want {
				t.Errorf("defaultMemoryDir(%q, %q) = %q, want %q", tc.dataHome, tc.ws, got, tc.want)
			}
			// The leaf must preserve the leading separator as a leading '-'.
			if tc.want != "" && !strings.HasPrefix(filepath.Base(got), "-") {
				t.Errorf("leaf %q should preserve the leading separator as a leading '-'", filepath.Base(got))
			}
		})
	}

	// Determinism: the same inputs always produce the same path.
	a1 := defaultMemoryDir("/xdg/data", "/a/proj")
	a2 := defaultMemoryDir("/xdg/data", "/a/proj")
	if a1 != a2 {
		t.Errorf("defaultMemoryDir is not deterministic: %q != %q", a1, a2)
	}
}

// setDataHome points adrg/xdg's DataHome at dir for the duration of the test.
// adrg/xdg snapshots the environment at package init, so a bare t.Setenv is NOT
// reflected — Reload() must re-read the env. The cleanup re-reads it again so the
// global does not leak a synthetic path into a later test. Only the
// resolveMemoryDir integration test needs this; defaultMemoryDir is pure.
func setDataHome(t *testing.T, dir string) {
	t.Helper()
	t.Setenv("XDG_DATA_HOME", dir)
	xdg.Reload()
	t.Cleanup(xdg.Reload)
}

// TestResolveMemoryDirPrecedence covers the precedence table on the real
// resolveMemoryDir path (which legitimately reads the xdg.DataHome global, hence
// setDataHome): --no-memory wins, then an explicit --memory-dir, then the computed
// per-project default under the resolved data base.
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
	// neither -> computed default under the resolved XDG data base.
	got := resolveMemoryDir(config{workspace: "/var/home/ozz/dev/mecatl"})
	want := filepath.Join("/xdg/data", "mecatui", "memory", "-var-home-ozz-dev-mecatl")
	if got != want {
		t.Errorf("default: got %q, want %q", got, want)
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
