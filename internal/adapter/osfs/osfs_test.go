package osfs_test

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/stacklok/mecatl/internal/adapter/fsconformance"
	"github.com/stacklok/mecatl/internal/adapter/osfs"
	"github.com/stacklok/mecatl/internal/tool"
)

// TestConformance runs the shared Workspace conformance table against osfs.
func TestConformance(t *testing.T) {
	fsconformance.Run(t, func(t *testing.T) tool.Workspace {
		ws, err := osfs.NewWorkspace(t.TempDir())
		if err != nil {
			t.Fatalf("NewWorkspace: %v", err)
		}
		return ws
	})
}

// --- osfs CommandRunner tests (real shell, under t.TempDir) ---

func newRunner(t *testing.T, dir string) tool.CommandRunner {
	t.Helper()
	r, err := osfs.NewCommandRunner(dir)
	if err != nil {
		t.Fatalf("NewCommandRunner: %v", err)
	}
	return r
}

func TestCommandRunnerRun(t *testing.T) {
	ctx := context.Background()
	r := newRunner(t, t.TempDir())

	res, err := r.Run(ctx, "echo hi && echo err >&2", "")
	if err != nil {
		t.Fatalf("Run: %v", err)
	}
	if strings.TrimSpace(res.Stdout) != "hi" {
		t.Errorf("Stdout = %q want hi", res.Stdout)
	}
	if strings.TrimSpace(res.Stderr) != "err" {
		t.Errorf("Stderr = %q want err", res.Stderr)
	}
	if res.ExitCode != 0 {
		t.Errorf("ExitCode = %d want 0", res.ExitCode)
	}
}

func TestCommandRunnerExitCode(t *testing.T) {
	ctx := context.Background()
	r := newRunner(t, t.TempDir())
	res, err := r.Run(ctx, "exit 3", "")
	if err != nil {
		t.Fatalf("Run returned harness error for non-zero exit: %v", err)
	}
	if res.ExitCode != 3 {
		t.Errorf("ExitCode = %d want 3", res.ExitCode)
	}
}

func TestCommandRunnerWorkingDir(t *testing.T) {
	ctx := context.Background()
	root := t.TempDir()
	if err := os.WriteFile(filepath.Join(root, "marker.txt"), []byte("x"), 0o644); err != nil {
		t.Fatalf("seed marker: %v", err)
	}
	r := newRunner(t, root)
	res, err := r.Run(ctx, "ls", "")
	if err != nil {
		t.Fatalf("Run: %v", err)
	}
	if !strings.Contains(res.Stdout, "marker.txt") {
		t.Errorf("ls output %q does not contain marker.txt (wrong cwd?)", res.Stdout)
	}
}

// TestCommandRunnerWorkdirOverride proves the runner runs in the per-call workdir
// when one is supplied — including a workdir OUTSIDE the runner's configured root
// (which is exactly the fork case: a fork lives under a temp base, not under the
// configured workspace). It must NOT confine/reject the out-of-root workdir, and
// an EMPTY workdir must fall back to the configured root.
func TestCommandRunnerWorkdirOverride(t *testing.T) {
	ctx := context.Background()
	base := t.TempDir()
	if err := os.WriteFile(filepath.Join(base, "base.txt"), []byte("x"), 0o644); err != nil {
		t.Fatalf("seed base marker: %v", err)
	}
	// A SEPARATE directory, not under base — the runner's configured root is base.
	other := t.TempDir()
	if err := os.WriteFile(filepath.Join(other, "other.txt"), []byte("y"), 0o644); err != nil {
		t.Fatalf("seed other marker: %v", err)
	}
	r := newRunner(t, base)

	// Workdir == the out-of-root "other" dir: the command must run there.
	res, err := r.Run(ctx, "ls", other)
	if err != nil {
		t.Fatalf("Run with out-of-root workdir: %v", err)
	}
	if !strings.Contains(res.Stdout, "other.txt") || strings.Contains(res.Stdout, "base.txt") {
		t.Errorf("ls in out-of-root workdir = %q; want other.txt (not base.txt) — workdir not honored", res.Stdout)
	}

	// Writing a relative-path marker via the out-of-root workdir lands in THAT dir,
	// not the configured root — the fork-isolation property the Bash fix needs.
	if _, err := r.Run(ctx, "echo hi > written.txt", other); err != nil {
		t.Fatalf("Run write in out-of-root workdir: %v", err)
	}
	if _, err := os.Stat(filepath.Join(other, "written.txt")); err != nil {
		t.Errorf("relative write did not land in the supplied workdir: %v", err)
	}
	if _, err := os.Stat(filepath.Join(base, "written.txt")); !os.IsNotExist(err) {
		t.Errorf("relative write leaked into the configured root (escaped the workdir)")
	}

	// Empty workdir falls back to the configured root.
	res, err = r.Run(ctx, "ls", "")
	if err != nil {
		t.Fatalf("Run with empty workdir: %v", err)
	}
	if !strings.Contains(res.Stdout, "base.txt") {
		t.Errorf("empty workdir did not fall back to the configured root: %q", res.Stdout)
	}
}

func TestCommandRunnerCancel(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	cancel() // cancel immediately
	r := newRunner(t, t.TempDir())
	_, err := r.Run(ctx, "echo hi", "")
	if !errors.Is(err, context.Canceled) {
		t.Fatalf("Run with cancelled ctx err = %v want context.Canceled", err)
	}
}

func TestCommandRunnerTimeout(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 50*time.Millisecond)
	defer cancel()
	r := newRunner(t, t.TempDir())
	_, err := r.Run(ctx, "sleep 5", "")
	if !errors.Is(err, context.DeadlineExceeded) {
		t.Fatalf("Run with expired deadline err = %v want context.DeadlineExceeded", err)
	}
}

func TestCommandRunnerEmptyShellRejected(t *testing.T) {
	if _, err := osfs.NewCommandRunnerShell(t.TempDir(), ""); err == nil {
		t.Fatal("NewCommandRunnerShell with empty shell = nil err, want error")
	}
}

func TestGrep(t *testing.T) {
	ctx := context.Background()
	ws, _ := osfs.NewWorkspace(t.TempDir())
	files := map[string]string{
		"a.go":     "package main\nfunc Foo() {}\n",
		"b.go":     "package main\nfunc Bar() {}\n",
		"notes.md": "Foo is documented here\n",
	}
	for p, c := range files {
		if err := ws.Write(ctx, p, []byte(c)); err != nil {
			t.Fatalf("Write %s: %v", p, err)
		}
	}

	// Across all files.
	all, err := ws.Grep(ctx, "Foo", "")
	if err != nil {
		t.Fatalf("Grep: %v", err)
	}
	if len(all) != 2 {
		t.Errorf("Grep Foo all = %d matches (%v) want 2", len(all), all)
	}

	// Restricted by glob.
	goOnly, err := ws.Grep(ctx, "func", "*.go")
	if err != nil {
		t.Fatalf("Grep glob: %v", err)
	}
	if len(goOnly) != 2 {
		t.Errorf("Grep func *.go = %d matches want 2", len(goOnly))
	}
	for _, m := range goOnly {
		if !strings.HasSuffix(m.Path, ".go") {
			t.Errorf("match outside glob: %q", m.Path)
		}
		if m.Line != 2 {
			t.Errorf("match line = %d want 2", m.Line)
		}
	}
}

func TestGrepInvalidPattern(t *testing.T) {
	ctx := context.Background()
	ws, _ := osfs.NewWorkspace(t.TempDir())
	if _, err := ws.Grep(ctx, "(", ""); err == nil {
		t.Fatal("Grep with invalid regex = nil err, want error")
	}
}

func TestRootIsAbsolute(t *testing.T) {
	root := t.TempDir()
	ws, _ := osfs.NewWorkspace(root)
	if !filepath.IsAbs(ws.Root()) {
		t.Errorf("Root() = %q is not absolute", ws.Root())
	}
}

func TestWriteCreatesParents(t *testing.T) {
	ctx := context.Background()
	root := t.TempDir()
	ws, _ := osfs.NewWorkspace(root)
	if err := ws.Write(ctx, "deep/nested/path/f.txt", []byte("x")); err != nil {
		t.Fatalf("Write nested: %v", err)
	}
	if _, err := os.Stat(filepath.Join(root, "deep", "nested", "path", "f.txt")); err != nil {
		t.Fatalf("expected nested file on disk: %v", err)
	}
}

// TestGlobDoesNotLeakThroughSymlink asserts Glob never returns paths reachable
// only by traversing a symlink out of the workspace — neither a leaf symlink nor
// a symlinked intermediate directory component. The latter is the filename
// enumeration leak: filepath.Glob follows an in-root directory symlink to an
// out-of-root target, and the match looks like an ordinary in-root regular file.
func TestGlobDoesNotLeakThroughSymlink(t *testing.T) {
	ctx := context.Background()

	outside := t.TempDir()
	if err := os.WriteFile(filepath.Join(outside, "secret.txt"), []byte("s"), 0o600); err != nil {
		t.Fatalf("seed outside file: %v", err)
	}

	root := t.TempDir()
	ws, err := osfs.NewWorkspace(root)
	if err != nil {
		t.Fatalf("NewWorkspace: %v", err)
	}
	// An in-root regular file Glob must still find.
	if err := ws.Write(ctx, "real.txt", []byte("ok")); err != nil {
		t.Fatalf("Write real.txt: %v", err)
	}
	// (a) symlinked intermediate directory component: link/ -> outside/.
	if err := os.Symlink(outside, filepath.Join(root, "link")); err != nil {
		t.Fatalf("symlink dir: %v", err)
	}
	// (b) leaf symlink to an out-of-root file.
	if err := os.Symlink(filepath.Join(outside, "secret.txt"), filepath.Join(root, "leaf.txt")); err != nil {
		t.Fatalf("symlink leaf: %v", err)
	}

	for _, pattern := range []string{"link/*", "*", "*.txt"} {
		got, err := ws.Glob(ctx, pattern)
		if err != nil {
			t.Fatalf("Glob(%q): %v", pattern, err)
		}
		for _, m := range got {
			if strings.HasPrefix(m, "link/") || m == "leaf.txt" {
				t.Errorf("Glob(%q) leaked out-of-root match %q", pattern, m)
			}
		}
	}

	// Sanity: the genuine in-root file is still matched.
	got, err := ws.Glob(ctx, "*.txt")
	if err != nil {
		t.Fatalf("Glob(*.txt): %v", err)
	}
	var sawReal bool
	for _, m := range got {
		if m == "real.txt" {
			sawReal = true
		}
	}
	if !sawReal {
		t.Errorf("Glob(*.txt) = %v, expected to contain real.txt", got)
	}
}
