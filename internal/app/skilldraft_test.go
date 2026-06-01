package app

import (
	"context"
	"encoding/json"
	"os"
	"path/filepath"
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
		if err := validateSkillDraftConfig(Config{Workspace: t.TempDir()}); err != nil {
			t.Fatalf("disabled draft must not error: %v", err)
		}
	})

	t.Run("quarantine inside the workspace is fatal", func(t *testing.T) {
		ws := t.TempDir()
		cfg := Config{Workspace: ws, SkillsDraftDir: filepath.Join(ws, "quarantine")}
		if err := validateSkillDraftConfig(cfg); err == nil {
			t.Fatal("expected a fatal error when the quarantine is inside the workspace")
		}
	})

	t.Run("quarantine overlapping an active skills dir is fatal", func(t *testing.T) {
		base := t.TempDir()
		ws := filepath.Join(base, "ws")
		shared := filepath.Join(base, "shared")
		cfg := Config{
			Workspace:      ws,
			SkillsDirs:     []string{shared},
			SkillsDraftDir: filepath.Join(shared, "quarantine"), // outside ws, but inside an active dir
		}
		if err := validateSkillDraftConfig(cfg); err == nil {
			t.Fatal("expected a fatal error when the quarantine overlaps an active skills dir")
		}
	})

	t.Run("outside-workspace, disjoint quarantine is accepted", func(t *testing.T) {
		base := t.TempDir()
		cfg := Config{
			Workspace:      filepath.Join(base, "ws"),
			SkillsDirs:     []string{filepath.Join(base, "active")},
			SkillsDraftDir: filepath.Join(base, "quarantine"),
		}
		if err := validateSkillDraftConfig(cfg); err != nil {
			t.Fatalf("a valid out-of-workspace, disjoint quarantine must be accepted: %v", err)
		}
	})
}

// TestValidateSkillDraftConfigSymlinkedWorkspace pins the fix for the symlink
// divergence: validation must canonicalize the workspace the SAME way the osfs
// Workspace does (abs + EvalSymlinks), not with filepath.Abs alone. Here the
// workspace is a symlink whose target is an ANCESTOR of the quarantine — with plain
// Abs the two look disjoint (false PASS) and the model's Write/Edit could reach the
// quarantine through the resolved os.Root; with EvalSymlinks the quarantine is
// correctly seen as inside the workspace and rejected.
func TestValidateSkillDraftConfigSymlinkedWorkspace(t *testing.T) {
	base := t.TempDir()
	realDir := filepath.Join(base, "real")
	quar := filepath.Join(realDir, "quar")
	if err := os.MkdirAll(quar, 0o755); err != nil {
		t.Fatal(err)
	}
	wslink := filepath.Join(base, "wslink")
	if err := os.Symlink(realDir, wslink); err != nil {
		t.Skipf("symlinks unsupported on this platform/filesystem: %v", err)
	}
	cfg := Config{Workspace: wslink, SkillsDraftDir: quar}
	if err := validateSkillDraftConfig(cfg); err == nil {
		t.Fatal("quarantine inside the symlink-resolved workspace must be fatal; " +
			"validation must use EvalSymlinks like the enforcement layer, not filepath.Abs")
	}
}

func TestDefaultRulesSkillDraftAsks(t *testing.T) {
	policy := permpolicy.NewPolicy(defaultRules(), nil)
	got := policy.Evaluate(context.Background(), "s1", session.ModeDefault,
		session.NewToolCall("id", skills.DraftToolName, json.RawMessage(`{}`)))
	if got.Effect != governance.Ask {
		t.Fatalf("SkillDraft should default to Ask, got %v", got.Effect)
	}
}

func TestRegisterSkillDraftGating(t *testing.T) {
	t.Run("disabled when no draft dir", func(t *testing.T) {
		cat := tool.NewCatalog()
		registerSkillDraft(Config{}, cat, nil)
		if _, ok := cat.Lookup(skills.DraftToolName); ok {
			t.Error("SkillDraft must NOT be registered without a skills-draft dir")
		}
	})
	t.Run("registered when draft dir set", func(t *testing.T) {
		cat := tool.NewCatalog()
		registerSkillDraft(Config{SkillsDraftDir: t.TempDir(), SkillsDraftThreshold: 0.5}, cat, nil)
		if _, ok := cat.Lookup(skills.DraftToolName); !ok {
			t.Error("SkillDraft must be registered when a skills-draft dir is set")
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
