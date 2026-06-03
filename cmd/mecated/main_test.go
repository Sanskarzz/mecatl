package main

import (
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"runtime/trace"
	"strings"
	"testing"

	"github.com/prometheus/client_golang/prometheus"

	"github.com/stacklok/mecatl/internal/adapter/skills"
	"github.com/stacklok/mecatl/internal/adapter/telemetry"
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

// TestParseFlagsPermissionConfig asserts the issue #13 permission-config flags
// parse into the config: --permissions-conventional defaults ON, --trust-project /
// --import-claude-permissions default OFF, and --permission-config is repeatable.
func TestParseFlagsPermissionConfig(t *testing.T) {
	def, err := parseFlags(nil)
	if err != nil {
		t.Fatalf("parseFlags(nil): %v", err)
	}
	if !def.permissionsConventional {
		t.Errorf("permissionsConventional default = false, want true (auto-discover ON)")
	}
	if def.trustProject {
		t.Errorf("trustProject default = true, want false (safe stance)")
	}
	if def.importClaudePermissions {
		t.Errorf("importClaudePermissions default = true, want false")
	}

	cfg, err := parseFlags([]string{
		"--permission-config", "/etc/a.yaml",
		"--permission-config", "/etc/b.yaml",
		"--permissions-conventional=false",
		"--import-claude-permissions",
		"--trust-project",
	})
	if err != nil {
		t.Fatalf("parseFlags: %v", err)
	}
	if got := []string(cfg.permissionConfigs); len(got) != 2 || got[0] != "/etc/a.yaml" || got[1] != "/etc/b.yaml" {
		t.Errorf("permissionConfigs = %v, want [/etc/a.yaml /etc/b.yaml]", got)
	}
	if cfg.permissionsConventional {
		t.Errorf("permissionsConventional = true, want false (explicitly disabled)")
	}
	if !cfg.importClaudePermissions {
		t.Errorf("importClaudePermissions = false, want true")
	}
	if !cfg.trustProject {
		t.Errorf("trustProject = false, want true")
	}
}

// TestAppConfigMapsPermissionConfig asserts appConfig threads the 4 permission-
// config fields onto the shared app.Config.
func TestAppConfigMapsPermissionConfig(t *testing.T) {
	cfg, err := parseFlags([]string{
		"--permission-config", "/etc/a.yaml",
		"--import-claude-permissions",
		"--trust-project",
	})
	if err != nil {
		t.Fatalf("parseFlags: %v", err)
	}
	ac := appConfig(cfg, nil, nil)
	if !ac.PermissionsConventional {
		t.Errorf("PermissionsConventional = false, want true (default)")
	}
	if !ac.ImportClaudePermissions {
		t.Errorf("ImportClaudePermissions = false, want true")
	}
	if !ac.TrustProject {
		t.Errorf("TrustProject = false, want true")
	}
	if len(ac.PermissionConfigs) != 1 || ac.PermissionConfigs[0] != "/etc/a.yaml" {
		t.Errorf("PermissionConfigs = %v, want [/etc/a.yaml]", ac.PermissionConfigs)
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

func TestParseFlagsAllowAll(t *testing.T) {
	def, err := parseFlags(nil)
	if err != nil {
		t.Fatalf("parseFlags(nil): %v", err)
	}
	if def.allowAllTools {
		t.Errorf("allowAllTools default = true, want false")
	}

	cfg, err := parseFlags([]string{"--yolo"})
	if err != nil {
		t.Fatalf("parseFlags: %v", err)
	}
	if !cfg.allowAllTools {
		t.Errorf("allowAllTools = false, want true (flag set)")
	}
}

func TestAppConfigMapsAllowAll(t *testing.T) {
	cfg, err := parseFlags([]string{"--yolo"})
	if err != nil {
		t.Fatalf("parseFlags: %v", err)
	}
	if ac := appConfig(cfg, nil, nil); !ac.AllowAllTools {
		t.Errorf("appConfig.AllowAllTools = false, want true")
	}

	off, err := parseFlags(nil)
	if err != nil {
		t.Fatalf("parseFlags(nil): %v", err)
	}
	if ac := appConfig(off, nil, nil); ac.AllowAllTools {
		t.Errorf("appConfig.AllowAllTools = true with flag off, want false")
	}
}

// TestNewAdminMuxServesIntrospectionEndpoints drives the REAL telemetry.NewAdminMux
// helper (the one serve mounts) and asserts each runtime-introspection endpoint
// serves a non-trivial body. Building the mux through the production helper —
// rather than a parallel hand-rolled mux — means deleting a mux.Handle in
// telemetry.NewAdminMux would fail this test.
func TestNewAdminMuxServesIntrospectionEndpoints(t *testing.T) {
	reg := prometheus.NewRegistry()
	// Seed one series so /metrics renders a non-empty exposition body (an empty
	// registry would otherwise produce an empty body and mask whether the handler
	// is even mounted).
	reg.MustRegister(prometheus.NewGauge(prometheus.GaugeOpts{
		Name: "mecatl_admin_mux_test_seed",
		Help: "Test seed series so /metrics is non-empty.",
	}))
	recorder := telemetry.NewFlightRecorder(trace.FlightRecorderConfig{})
	if err := recorder.Start(); err != nil {
		t.Fatalf("flight recorder Start: %v", err)
	}
	defer recorder.Stop()

	srv := httptest.NewServer(telemetry.NewAdminMux(reg, recorder))
	defer srv.Close()

	cases := []struct {
		path       string
		wantSubstr string // a marker that must appear in the body (empty = any non-empty body)
	}{
		{"/metrics", "mecatl_admin_mux_test_seed"}, // the seeded series proves the exporter is wired
		{"/debug/pprof/", "Types of profiles"},     // the pprof index page
		{"/debug/vars", "mecatl_runtime"},          // the curated expvar key
		{"/debug/flightrecorder", ""},              // a binary trace snapshot
	}
	for _, tc := range cases {
		t.Run(tc.path, func(t *testing.T) {
			resp, err := http.Get(srv.URL + tc.path)
			if err != nil {
				t.Fatalf("GET %s: %v", tc.path, err)
			}
			defer func() { _ = resp.Body.Close() }()
			if resp.StatusCode != http.StatusOK {
				t.Fatalf("GET %s status = %d, want 200", tc.path, resp.StatusCode)
			}
			body := readBody(t, resp)
			if len(body) == 0 {
				t.Fatalf("GET %s returned an empty body", tc.path)
			}
			if tc.wantSubstr != "" && !strings.Contains(string(body), tc.wantSubstr) {
				t.Errorf("GET %s body missing %q; got %.160q", tc.path, tc.wantSubstr, body)
			}
		})
	}
}

// TestNewAdminMuxOmitsFlightRecorderWhenDisabled asserts the recorder==nil
// (FlightRecorder disabled) branch: /debug/flightrecorder is ABSENT (404) while
// the always-on endpoints still serve.
func TestNewAdminMuxOmitsFlightRecorderWhenDisabled(t *testing.T) {
	srv := httptest.NewServer(telemetry.NewAdminMux(prometheus.NewRegistry(), nil))
	defer srv.Close()

	resp, err := http.Get(srv.URL + "/debug/flightrecorder")
	if err != nil {
		t.Fatalf("GET /debug/flightrecorder: %v", err)
	}
	defer func() { _ = resp.Body.Close() }()
	if resp.StatusCode != http.StatusNotFound {
		t.Errorf("GET /debug/flightrecorder status = %d, want 404 when the recorder is disabled", resp.StatusCode)
	}

	// An always-on endpoint must still be mounted.
	varsResp, err := http.Get(srv.URL + "/debug/vars")
	if err != nil {
		t.Fatalf("GET /debug/vars: %v", err)
	}
	defer func() { _ = varsResp.Body.Close() }()
	if varsResp.StatusCode != http.StatusOK {
		t.Errorf("GET /debug/vars status = %d, want 200 even with the recorder disabled", varsResp.StatusCode)
	}
}

// TestParseFlagsRuntimeIntrospection asserts the runtime-introspection flags
// parse: --flight-recorder defaults ON and --flight-recorder=false flips it, and
// --mutex-profile-fraction / --block-profile-rate parse into the config.
func TestParseFlagsRuntimeIntrospection(t *testing.T) {
	def, err := parseFlags(nil)
	if err != nil {
		t.Fatalf("parseFlags(nil): %v", err)
	}
	if !def.flightRecorder {
		t.Errorf("flightRecorder default = false, want true (ON by default)")
	}
	if def.mutexProfileFraction != 0 {
		t.Errorf("mutexProfileFraction default = %d, want 0 (off)", def.mutexProfileFraction)
	}
	if def.blockProfileRate != 0 {
		t.Errorf("blockProfileRate default = %d, want 0 (off)", def.blockProfileRate)
	}

	cfg, err := parseFlags([]string{
		"--flight-recorder=false",
		"--mutex-profile-fraction=5",
		"--block-profile-rate=1000",
	})
	if err != nil {
		t.Fatalf("parseFlags: %v", err)
	}
	if cfg.flightRecorder {
		t.Errorf("flightRecorder = true, want false (--flight-recorder=false)")
	}
	if cfg.mutexProfileFraction != 5 {
		t.Errorf("mutexProfileFraction = %d, want 5", cfg.mutexProfileFraction)
	}
	if cfg.blockProfileRate != 1000 {
		t.Errorf("blockProfileRate = %d, want 1000", cfg.blockProfileRate)
	}
}

// readBody reads an entire response body, failing the test on error.
func readBody(t *testing.T, resp *http.Response) []byte {
	t.Helper()
	b, err := io.ReadAll(resp.Body)
	if err != nil {
		t.Fatalf("read body: %v", err)
	}
	return b
}

func TestAllowAllRefusalReason(t *testing.T) {
	tests := []struct {
		name     string
		allowAll bool
		euid     int
		sandbox  bool
		wantErr  bool
	}{
		{"root no sandbox refused", true, 0, false, true},
		{"root with sandbox ok", true, 0, true, false},
		{"non-root no sandbox ok", true, 1000, false, false},
		{"non-root with sandbox ok", true, 1000, true, false},
		{"flag off root ok", false, 0, false, false},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			err := allowAllRefusalReason(tt.allowAll, tt.euid, tt.sandbox)
			if tt.wantErr && err == nil {
				t.Fatalf("expected a refusal error, got nil")
			}
			if !tt.wantErr && err != nil {
				t.Fatalf("expected no error, got %v", err)
			}
		})
	}
}
