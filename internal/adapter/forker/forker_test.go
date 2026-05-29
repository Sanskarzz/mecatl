package forker_test

import (
	"context"
	"os"
	"os/exec"
	"path/filepath"
	"testing"

	"github.com/stacklok/ozzharness/internal/adapter/forker"
	"github.com/stacklok/ozzharness/internal/adapter/osfs"
	"github.com/stacklok/ozzharness/internal/tool"
)

// osfsWorkspace adapts osfs.NewWorkspace to the forker's constructor signature.
func osfsWorkspace(root string) (tool.Workspace, error) {
	return osfs.NewWorkspace(root)
}

// TestForkGitWorktree exercises the git-repo path: forking a repo yields a real
// worktree directory derived from the base, and cleanup removes it. Skipped when
// git is unavailable.
func TestForkGitWorktree(t *testing.T) {
	if _, err := exec.LookPath("git"); err != nil {
		t.Skip("git not available")
	}
	base := t.TempDir()
	initGitRepo(t, base)
	writeFile(t, filepath.Join(base, "tracked.txt"), "from base\n")
	gitCommit(t, base)

	baseWS, err := osfs.NewWorkspace(base)
	if err != nil {
		t.Fatalf("base workspace: %v", err)
	}

	f := forker.New(osfsWorkspace)
	child, cleanup, err := f.Fork(context.Background(), baseWS, "idea-a")
	if err != nil {
		t.Fatalf("Fork: %v", err)
	}

	// The child root must be a distinct directory from the base, and a worktree
	// (carries a .git file/dir pointing back at the repo).
	if child.Root() == base {
		t.Fatalf("child root must differ from base; both are %q", base)
	}
	if _, err := os.Stat(filepath.Join(child.Root(), ".git")); err != nil {
		t.Fatalf("child is not a git worktree (.git missing): %v", err)
	}
	// The committed file is present in the worktree.
	if _, err := os.Stat(filepath.Join(child.Root(), "tracked.txt")); err != nil {
		t.Fatalf("worktree missing committed file: %v", err)
	}

	// A write in the child does NOT touch the base.
	if err := child.Write(context.Background(), "child-only.txt", []byte("x")); err != nil {
		t.Fatalf("child write: %v", err)
	}
	if _, err := os.Stat(filepath.Join(base, "child-only.txt")); !os.IsNotExist(err) {
		t.Fatalf("child write leaked into base (err=%v)", err)
	}

	childRoot := child.Root()
	if err := cleanup(); err != nil {
		t.Fatalf("cleanup: %v", err)
	}
	if _, err := os.Stat(childRoot); !os.IsNotExist(err) {
		t.Fatalf("cleanup did not remove worktree dir %q (err=%v)", childRoot, err)
	}
}

// TestForkCopyFallback exercises the non-git path: forking a plain directory
// yields an isolated recursive copy; writes in the child do not affect the base,
// and cleanup removes the copy.
func TestForkCopyFallback(t *testing.T) {
	base := t.TempDir()
	writeFile(t, filepath.Join(base, "a.txt"), "alpha\n")
	writeFile(t, filepath.Join(base, "sub", "b.txt"), "beta\n")

	baseWS, err := osfs.NewWorkspace(base)
	if err != nil {
		t.Fatalf("base workspace: %v", err)
	}

	f := forker.New(osfsWorkspace)
	child, cleanup, err := f.Fork(context.Background(), baseWS, "copy idea!")
	if err != nil {
		t.Fatalf("Fork: %v", err)
	}
	if child.Root() == base {
		t.Fatalf("child root must differ from base")
	}

	// The copy carries the base files.
	got, err := child.Read(context.Background(), "a.txt")
	if err != nil || string(got) != "alpha\n" {
		t.Fatalf("child a.txt = %q, err=%v", got, err)
	}
	got, err = child.Read(context.Background(), "sub/b.txt")
	if err != nil || string(got) != "beta\n" {
		t.Fatalf("child sub/b.txt = %q, err=%v", got, err)
	}

	// Mutating the child must not affect the base.
	if err := child.Write(context.Background(), "a.txt", []byte("CHANGED")); err != nil {
		t.Fatalf("child write: %v", err)
	}
	baseData, err := os.ReadFile(filepath.Join(base, "a.txt"))
	if err != nil {
		t.Fatalf("read base a.txt: %v", err)
	}
	if string(baseData) != "alpha\n" {
		t.Fatalf("base a.txt mutated by child: %q", baseData)
	}

	childRoot := child.Root()
	if err := cleanup(); err != nil {
		t.Fatalf("cleanup: %v", err)
	}
	if _, err := os.Stat(childRoot); !os.IsNotExist(err) {
		t.Fatalf("cleanup did not remove copy dir %q (err=%v)", childRoot, err)
	}
}

// TestForkConcurrentCopiesAreDistinct forks the same base several times in
// parallel and asserts every child gets a distinct, isolated root.
func TestForkConcurrentCopiesAreDistinct(t *testing.T) {
	base := t.TempDir()
	writeFile(t, filepath.Join(base, "f.txt"), "x")

	baseWS, err := osfs.NewWorkspace(base)
	if err != nil {
		t.Fatalf("base workspace: %v", err)
	}
	f := forker.New(osfsWorkspace)

	const n = 5
	roots := make([]string, n)
	cleanups := make([]func() error, n)
	errs := make([]error, n)
	done := make(chan int, n)
	for i := 0; i < n; i++ {
		go func(i int) {
			child, cleanup, err := f.Fork(context.Background(), baseWS, "x")
			if err != nil {
				errs[i] = err
			} else {
				roots[i] = child.Root()
				cleanups[i] = cleanup
			}
			done <- i
		}(i)
	}
	for i := 0; i < n; i++ {
		<-done
	}
	seen := map[string]bool{}
	for i := 0; i < n; i++ {
		if errs[i] != nil {
			t.Fatalf("fork %d: %v", i, errs[i])
		}
		if seen[roots[i]] {
			t.Fatalf("duplicate child root %q", roots[i])
		}
		seen[roots[i]] = true
		if cleanups[i] != nil {
			_ = cleanups[i]()
		}
	}
}

// TestNewNilConstructorPanics asserts the composition-root contract.
func TestNewNilConstructorPanics(t *testing.T) {
	defer func() {
		if recover() == nil {
			t.Fatalf("New(nil) did not panic")
		}
	}()
	_ = forker.New(nil)
}

// --- helpers ---------------------------------------------------------------

func writeFile(t *testing.T, path, content string) {
	t.Helper()
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		t.Fatalf("mkdir: %v", err)
	}
	if err := os.WriteFile(path, []byte(content), 0o644); err != nil {
		t.Fatalf("write %s: %v", path, err)
	}
}

func initGitRepo(t *testing.T, dir string) {
	t.Helper()
	runGit(t, dir, "init")
	runGit(t, dir, "config", "user.email", "test@example.com")
	runGit(t, dir, "config", "user.name", "Test")
}

func gitCommit(t *testing.T, dir string) {
	t.Helper()
	runGit(t, dir, "add", "-A")
	runGit(t, dir, "commit", "-m", "init")
}

func runGit(t *testing.T, dir string, args ...string) {
	t.Helper()
	cmd := exec.Command("git", append([]string{"-C", dir}, args...)...)
	if out, err := cmd.CombinedOutput(); err != nil {
		t.Fatalf("git %v: %v\n%s", args, err, out)
	}
}
