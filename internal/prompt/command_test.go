package prompt_test

import (
	"context"
	"strings"
	"testing"

	"github.com/stacklok/mecatl/internal/adapter/memfs"
	"github.com/stacklok/mecatl/internal/prompt"
	"github.com/stacklok/mecatl/internal/tool"
)

func writeFile(t *testing.T, ws tool.Workspace, path, content string) {
	t.Helper()
	if err := ws.Write(context.Background(), path, []byte(content)); err != nil {
		t.Fatalf("write %s: %v", path, err)
	}
}

// TestNoopExpanderReturnsInputUnchanged verifies the default expander never
// rewrites the input and never reports an expansion.
func TestNoopExpanderReturnsInputUnchanged(t *testing.T) {
	ws := memfs.NewWorkspace("/proj")
	for _, in := range []string{"hello world", "/review foo.go", ""} {
		out, ok, err := prompt.NoopExpander{}.Expand(context.Background(), ws, in)
		if err != nil {
			t.Fatalf("unexpected error: %v", err)
		}
		if ok {
			t.Errorf("NoopExpander reported expanded=true for %q", in)
		}
		if out != in {
			t.Errorf("NoopExpander changed %q -> %q", in, out)
		}
	}
}

// TestDirCommandExpanderSubstitutes verifies $ARGUMENTS and positional ($1, $2)
// substitution against a seeded command file.
func TestDirCommandExpanderSubstitutes(t *testing.T) {
	ws := memfs.NewWorkspace("/proj")
	writeFile(t, ws, ".mecatl/commands/review.md",
		"Please review $1 and also $2.\nAll args: $ARGUMENTS")

	exp := prompt.NewDirCommandExpander()
	out, ok, err := exp.Expand(context.Background(), ws, "/review foo.go bar.go")
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if !ok {
		t.Fatalf("want expanded=true")
	}
	if !strings.Contains(out, "Please review foo.go and also bar.go.") {
		t.Errorf("positional substitution wrong: %q", out)
	}
	if !strings.Contains(out, "All args: foo.go bar.go") {
		t.Errorf("$ARGUMENTS substitution wrong: %q", out)
	}
}

// TestDirCommandExpanderLeavesUnknownPlaceholders verifies unknown $-tokens and
// out-of-range positionals are handled: unknown left intact, out-of-range empty.
func TestDirCommandExpanderLeavesUnknownPlaceholders(t *testing.T) {
	ws := memfs.NewWorkspace("/proj")
	writeFile(t, ws, ".mecatl/commands/c.md", "keep $HOME but drop [$3] and use $1")

	exp := prompt.NewDirCommandExpander()
	out, ok, err := exp.Expand(context.Background(), ws, "/c only")
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if !ok {
		t.Fatalf("want expanded=true")
	}
	want := "keep $HOME but drop [] and use only"
	if out != want {
		t.Errorf("got %q, want %q", out, want)
	}
}

// TestDirCommandExpanderStripsFrontmatter verifies a leading YAML frontmatter
// block is removed and never reaches the expanded body.
func TestDirCommandExpanderStripsFrontmatter(t *testing.T) {
	ws := memfs.NewWorkspace("/proj")
	writeFile(t, ws, ".mecatl/commands/review.md",
		"---\ndescription: Review a file\n---\nReview $1 now.")

	exp := prompt.NewDirCommandExpander()
	out, ok, err := exp.Expand(context.Background(), ws, "/review foo.go")
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if !ok {
		t.Fatalf("want expanded=true")
	}
	if strings.Contains(out, "description") || strings.Contains(out, "---") {
		t.Errorf("frontmatter not stripped: %q", out)
	}
	if out != "Review foo.go now." {
		t.Errorf("got %q, want %q", out, "Review foo.go now.")
	}
}

// TestDirCommandExpanderClaudeDir verifies the .claude/commands/ fallback dir is
// also searched.
func TestDirCommandExpanderClaudeDir(t *testing.T) {
	ws := memfs.NewWorkspace("/proj")
	writeFile(t, ws, ".claude/commands/hi.md", "Hello $ARGUMENTS")

	exp := prompt.NewDirCommandExpander()
	out, ok, err := exp.Expand(context.Background(), ws, "/hi there")
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if !ok {
		t.Fatalf("want expanded=true")
	}
	if out != "Hello there" {
		t.Errorf("got %q, want %q", out, "Hello there")
	}
}

// TestDirCommandExpanderUnknownCommand verifies an unknown command is left
// unchanged with expanded=false and no error (does not abort the run).
func TestDirCommandExpanderUnknownCommand(t *testing.T) {
	ws := memfs.NewWorkspace("/proj")
	writeFile(t, ws, ".mecatl/commands/review.md", "body")

	exp := prompt.NewDirCommandExpander()
	out, ok, err := exp.Expand(context.Background(), ws, "/nope foo")
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if ok {
		t.Errorf("unknown command reported expanded=true")
	}
	if out != "/nope foo" {
		t.Errorf("unknown command changed input: %q", out)
	}
}

// TestDirCommandExpanderNonCommand verifies plain (non-slash) input is left
// unchanged with expanded=false.
func TestDirCommandExpanderNonCommand(t *testing.T) {
	ws := memfs.NewWorkspace("/proj")
	writeFile(t, ws, ".mecatl/commands/review.md", "body")

	exp := prompt.NewDirCommandExpander()
	for _, in := range []string{"just chatting", "look at /etc/hosts", "/", "/ space"} {
		out, ok, err := exp.Expand(context.Background(), ws, in)
		if err != nil {
			t.Fatalf("unexpected error for %q: %v", in, err)
		}
		if ok {
			t.Errorf("non-command %q reported expanded=true", in)
		}
		if out != in {
			t.Errorf("non-command %q changed to %q", in, out)
		}
	}
}

// Compile-time assertions that both types satisfy the interface.
var (
	_ prompt.CommandExpander = prompt.NoopExpander{}
	_ prompt.CommandExpander = (*prompt.DirCommandExpander)(nil)
)

// stubExpander is a controllable CommandExpander for MultiExpander tests. It
// expands inputs equal to match into out (reporting expanded=true); otherwise it
// passes the input through. err, when set, is returned for any input.
type stubExpander struct {
	match string
	out   string
	err   error
}

func (s stubExpander) Expand(_ context.Context, _ tool.Workspace, input string) (string, bool, error) {
	if s.err != nil {
		return input, false, s.err
	}
	if input == s.match {
		return s.out, true, nil
	}
	return input, false, nil
}

func TestMultiExpanderFirstWins(t *testing.T) {
	ws := memfs.NewWorkspace("/proj")
	// Both match "/x"; the first (highest precedence) should win and shadow the
	// second.
	exp := prompt.NewMultiExpander(
		stubExpander{match: "/x", out: "FIRST"},
		stubExpander{match: "/x", out: "SECOND"},
	)
	out, ok, err := exp.Expand(context.Background(), ws, "/x")
	if err != nil {
		t.Fatalf("Expand: %v", err)
	}
	if !ok || out != "FIRST" {
		t.Errorf("Expand = (%q, %v), want (FIRST, true)", out, ok)
	}
}

func TestMultiExpanderFallsThrough(t *testing.T) {
	ws := memfs.NewWorkspace("/proj")
	// Only the second matches; the first passes through to it.
	exp := prompt.NewMultiExpander(
		stubExpander{match: "/a", out: "A"},
		stubExpander{match: "/b", out: "B"},
	)
	out, ok, err := exp.Expand(context.Background(), ws, "/b")
	if err != nil {
		t.Fatalf("Expand: %v", err)
	}
	if !ok || out != "B" {
		t.Errorf("Expand = (%q, %v), want (B, true)", out, ok)
	}
}

func TestMultiExpanderNoneMatches(t *testing.T) {
	ws := memfs.NewWorkspace("/proj")
	exp := prompt.NewMultiExpander(
		stubExpander{match: "/a", out: "A"},
		stubExpander{match: "/b", out: "B"},
	)
	out, ok, err := exp.Expand(context.Background(), ws, "/z")
	if err != nil {
		t.Fatalf("Expand: %v", err)
	}
	if ok {
		t.Errorf("expected no expansion for /z")
	}
	if out != "/z" {
		t.Errorf("input changed: %q", out)
	}
}

func TestMultiExpanderDirShadowsLater(t *testing.T) {
	// A file-backed DirCommandExpander command shadows a later expander that would
	// also claim the same name — the file command wins because it is listed first.
	ws := memfs.NewWorkspace("/proj")
	writeFile(t, ws, ".mecatl/commands/dup.md", "FROM FILE")

	exp := prompt.NewMultiExpander(
		prompt.NewDirCommandExpander(), // highest precedence
		stubExpander{match: "/dup", out: "FROM STUB"},
	)
	out, ok, err := exp.Expand(context.Background(), ws, "/dup")
	if err != nil {
		t.Fatalf("Expand: %v", err)
	}
	if !ok || out != "FROM FILE" {
		t.Errorf("Expand = (%q, %v), want (FROM FILE, true) — file command must shadow the later expander", out, ok)
	}
}

func TestMultiExpanderErrorStopsChain(t *testing.T) {
	ws := memfs.NewWorkspace("/proj")
	sentinel := errStub("boom")
	exp := prompt.NewMultiExpander(
		stubExpander{err: sentinel},
		stubExpander{match: "/x", out: "X"}, // never reached
	)
	_, ok, err := exp.Expand(context.Background(), ws, "/x")
	if err == nil {
		t.Fatalf("expected the chain to surface the first expander's error")
	}
	if ok {
		t.Errorf("expected expanded=false on error")
	}
}

type errStub string

func (e errStub) Error() string { return string(e) }

var _ prompt.CommandExpander = (*prompt.MultiExpander)(nil)
