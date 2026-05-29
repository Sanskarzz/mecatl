package osfs_test

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/stacklok/ozzharness/internal/adapter/fsconformance"
	"github.com/stacklok/ozzharness/internal/adapter/osfs"
	"github.com/stacklok/ozzharness/internal/tool"
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

// --- osfs-specific tests (real disk, under t.TempDir) ---

func TestRunCommand(t *testing.T) {
	ctx := context.Background()
	ws, err := osfs.NewWorkspace(t.TempDir())
	if err != nil {
		t.Fatalf("NewWorkspace: %v", err)
	}

	res, err := ws.RunCommand(ctx, "echo hi && echo err >&2")
	if err != nil {
		t.Fatalf("RunCommand: %v", err)
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

func TestRunCommandExitCode(t *testing.T) {
	ctx := context.Background()
	ws, _ := osfs.NewWorkspace(t.TempDir())
	res, err := ws.RunCommand(ctx, "exit 3")
	if err != nil {
		t.Fatalf("RunCommand returned harness error for non-zero exit: %v", err)
	}
	if res.ExitCode != 3 {
		t.Errorf("ExitCode = %d want 3", res.ExitCode)
	}
}

func TestRunCommandWorkingDir(t *testing.T) {
	ctx := context.Background()
	root := t.TempDir()
	ws, _ := osfs.NewWorkspace(root)
	if err := ws.Write(ctx, "marker.txt", []byte("x")); err != nil {
		t.Fatalf("Write: %v", err)
	}
	res, err := ws.RunCommand(ctx, "ls")
	if err != nil {
		t.Fatalf("RunCommand: %v", err)
	}
	if !strings.Contains(res.Stdout, "marker.txt") {
		t.Errorf("ls output %q does not contain marker.txt (wrong cwd?)", res.Stdout)
	}
}

func TestRunCommandCancel(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	cancel() // cancel immediately
	ws, _ := osfs.NewWorkspace(t.TempDir())
	_, err := ws.RunCommand(ctx, "echo hi")
	if !errors.Is(err, context.Canceled) {
		t.Fatalf("RunCommand with cancelled ctx err = %v want context.Canceled", err)
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
