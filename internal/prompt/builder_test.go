package prompt_test

import (
	"context"
	"strings"
	"testing"

	"github.com/stacklok/mecatl/internal/adapter/memfs"
	"github.com/stacklok/mecatl/internal/prompt"
	"github.com/stacklok/mecatl/internal/session"
	"github.com/stacklok/mecatl/internal/tool"
)

func sampleTools() []tool.ToolSpec {
	return []tool.ToolSpec{
		{Name: "Read", Description: "Read a file's contents.\nMore detail here."},
		{Name: "Edit", Description: "Edit a file in place."},
		{Name: "Bash", Description: "Run a shell command."},
	}
}

// TestStablePrefixByteStableAcrossEnv is the gauntlet #6 cache invariant: the
// StablePrefix must be byte-identical across two builds that differ only in env
// (date, cwd, model, mode).
func TestStablePrefixByteStableAcrossEnv(t *testing.T) {
	base := prompt.Config{Tools: sampleTools()}

	cfgA := base
	cfgA.Env = prompt.Env{
		Cwd: "/home/a/proj", OS: "linux", Model: "model-x",
		Date: "2026-05-29", Mode: "default",
	}
	cfgB := base
	cfgB.Env = prompt.Env{
		Cwd: "/tmp/other", OS: "darwin", Model: "model-y",
		Date: "1999-12-31", Mode: "plan",
	}

	a := prompt.Build(cfgA)
	b := prompt.Build(cfgB)

	if a.StablePrefix != b.StablePrefix {
		t.Fatalf("StablePrefix changed when only env changed:\nA=%q\nB=%q",
			a.StablePrefix, b.StablePrefix)
	}

	// Determinism: same config builds the same prefix.
	if prompt.Build(cfgA).StablePrefix != a.StablePrefix {
		t.Fatal("StablePrefix not deterministic for identical config")
	}

	// The volatile suffix MUST differ since the env differs.
	if a.VolatileSuffix == b.VolatileSuffix {
		t.Fatal("VolatileSuffix identical despite differing env")
	}
}

// TestStablePrefixContainsToolsAndNoVolatile asserts the prefix lists tool names
// and contains no date/cwd/model leakage.
func TestStablePrefixContainsToolsAndNoVolatile(t *testing.T) {
	cfg := prompt.Config{
		Tools: sampleTools(),
		Env: prompt.Env{
			Cwd: "/secret/cwd/path", OS: "linux", Model: "leaky-model-id",
			Date: "2026-05-29", Mode: "acceptEdits",
		},
	}
	got := prompt.Build(cfg).StablePrefix

	for _, name := range []string{"Read", "Edit", "Bash"} {
		if !strings.Contains(got, name) {
			t.Errorf("StablePrefix missing tool name %q\nprefix=%q", name, got)
		}
	}
	// One-line purpose should come from the first line only.
	if strings.Contains(got, "More detail here.") {
		t.Errorf("StablePrefix included non-first-line description text")
	}

	for _, vol := range []string{
		"/secret/cwd/path", "leaky-model-id", "2026-05-29", "acceptEdits", "<env>",
	} {
		if strings.Contains(got, vol) {
			t.Errorf("StablePrefix leaked volatile value %q\nprefix=%q", vol, got)
		}
	}
}

func TestEnvBlockDeterministicAndComplete(t *testing.T) {
	env := prompt.Env{
		Cwd: "/w", OS: "linux", Model: "m1", Date: "2026-05-29", Mode: "default",
	}
	first := prompt.EnvBlock(env)
	second := prompt.EnvBlock(env)
	if first != second {
		t.Fatalf("EnvBlock not deterministic:\n1=%q\n2=%q", first, second)
	}

	for _, want := range []string{
		"<env>", "</env>",
		"cwd: /w", "os: linux", "model: m1",
		"date: 2026-05-29", "permission-mode: default",
	} {
		if !strings.Contains(first, want) {
			t.Errorf("EnvBlock missing %q\ngot=%q", want, first)
		}
	}

	// Stable key order: keys appear sorted alphabetically.
	wantOrder := []string{"cwd:", "date:", "model:", "os:", "permission-mode:"}
	idx := -1
	for _, k := range wantOrder {
		at := strings.Index(first, k)
		if at <= idx {
			t.Fatalf("EnvBlock key %q out of order in %q", k, first)
		}
		idx = at
	}
}

func TestDiscoverInstructionsAgentsPresent(t *testing.T) {
	ws := memfs.NewWorkspace("/proj")
	mustWrite(t, ws, "AGENTS.md", "Use tabs, not spaces.")

	msgs, err := prompt.DiscoverInstructions(context.Background(), ws)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if len(msgs) != 1 {
		t.Fatalf("want 1 message, got %d", len(msgs))
	}
	m := msgs[0]
	if m.Role != session.RoleUser {
		t.Errorf("want RoleUser, got %q", m.Role)
	}
	if !strings.Contains(m.Text, "Project instructions (AGENTS.md):") {
		t.Errorf("missing provenance marker: %q", m.Text)
	}
	if !strings.Contains(m.Text, "Use tabs, not spaces.") {
		t.Errorf("missing file content: %q", m.Text)
	}
}

func TestDiscoverInstructionsNeitherPresent(t *testing.T) {
	ws := memfs.NewWorkspace("/proj")
	msgs, err := prompt.DiscoverInstructions(context.Background(), ws)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if len(msgs) != 0 {
		t.Fatalf("want 0 messages, got %d", len(msgs))
	}
}

// TestDiscoverInstructionsAgentsWinsOverClaude documents and verifies the chosen
// precedence: when both files exist, AGENTS.md wins and CLAUDE.md is ignored.
func TestDiscoverInstructionsAgentsWinsOverClaude(t *testing.T) {
	ws := memfs.NewWorkspace("/proj")
	mustWrite(t, ws, "AGENTS.md", "AGENTS content")
	mustWrite(t, ws, "CLAUDE.md", "CLAUDE content")

	msgs, err := prompt.DiscoverInstructions(context.Background(), ws)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if len(msgs) != 1 {
		t.Fatalf("want exactly 1 message (AGENTS wins), got %d", len(msgs))
	}
	if !strings.Contains(msgs[0].Text, "AGENTS content") {
		t.Errorf("expected AGENTS.md content, got %q", msgs[0].Text)
	}
	if strings.Contains(msgs[0].Text, "CLAUDE content") {
		t.Errorf("CLAUDE.md should be ignored when AGENTS.md present: %q", msgs[0].Text)
	}
}

// TestDiscoverInstructionsClaudeFallback verifies CLAUDE.md is used when only it
// exists, and that an empty AGENTS.md falls through to it.
func TestDiscoverInstructionsClaudeFallback(t *testing.T) {
	ws := memfs.NewWorkspace("/proj")
	mustWrite(t, ws, "AGENTS.md", "   \n\t  ") // whitespace-only -> treated as absent
	mustWrite(t, ws, "CLAUDE.md", "CLAUDE fallback content")

	msgs, err := prompt.DiscoverInstructions(context.Background(), ws)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if len(msgs) != 1 {
		t.Fatalf("want 1 message, got %d", len(msgs))
	}
	if !strings.Contains(msgs[0].Text, "Project instructions (CLAUDE.md):") {
		t.Errorf("missing CLAUDE.md provenance marker: %q", msgs[0].Text)
	}
	if !strings.Contains(msgs[0].Text, "CLAUDE fallback content") {
		t.Errorf("missing CLAUDE.md content: %q", msgs[0].Text)
	}
}

func mustWrite(t *testing.T, ws tool.Workspace, path, content string) {
	t.Helper()
	if err := ws.Write(context.Background(), path, []byte(content)); err != nil {
		t.Fatalf("write %s: %v", path, err)
	}
}
