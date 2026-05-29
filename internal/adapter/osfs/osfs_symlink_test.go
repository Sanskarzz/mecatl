package osfs

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"testing"
)

// Finding 2: a symlink created INSIDE the workspace (the model can do this via
// Bash `ln -s`) must not let Read/Write/Stat/fingerprint follow it out of the
// root. os.Root refuses the traversal and we map the error to ErrPathEscape.

// newSymlinkWorkspace builds a Workspace under t.TempDir() and plants two
// escaping symlinks: "evil" -> an absolute path outside the root, and "up" -> a
// relative ".."-targeting path. It returns the workspace and the root dir.
func newSymlinkWorkspace(t *testing.T) (*Workspace, string) {
	t.Helper()
	root := t.TempDir()

	// An absolute escape target outside the workspace.
	outside := filepath.Join(t.TempDir(), "secret.txt")
	if err := os.WriteFile(outside, []byte("top secret"), 0o600); err != nil {
		t.Fatalf("write outside file: %v", err)
	}
	if err := os.Symlink(outside, filepath.Join(root, "evil")); err != nil {
		t.Fatalf("symlink evil: %v", err)
	}

	// A relative ".."-targeting link that climbs out of the root.
	if err := os.Symlink(filepath.Join("..", ".."), filepath.Join(root, "up")); err != nil {
		t.Fatalf("symlink up: %v", err)
	}

	ws, err := NewWorkspace(root)
	if err != nil {
		t.Fatalf("NewWorkspace: %v", err)
	}
	return ws, root
}

func TestReadThroughSymlinkEscapesRejected(t *testing.T) {
	ws, _ := newSymlinkWorkspace(t)
	ctx := context.Background()

	for _, p := range []string{"evil", "up/secret.txt", "../secret.txt", "/etc/passwd"} {
		if _, err := ws.Read(ctx, p); !errors.Is(err, ErrPathEscape) {
			t.Errorf("Read(%q) error = %v, want ErrPathEscape", p, err)
		}
	}
}

func TestWriteThroughSymlinkEscapesRejected(t *testing.T) {
	ws, root := newSymlinkWorkspace(t)
	ctx := context.Background()

	if err := ws.Write(ctx, "evil", []byte("pwned")); !errors.Is(err, ErrPathEscape) {
		t.Errorf("Write(evil) error = %v, want ErrPathEscape", err)
	}
	if err := ws.Write(ctx, "up/escape.txt", []byte("pwned")); !errors.Is(err, ErrPathEscape) {
		t.Errorf("Write(up/escape.txt) error = %v, want ErrPathEscape", err)
	}
	if err := ws.Write(ctx, "/etc/escape", []byte("pwned")); !errors.Is(err, ErrPathEscape) {
		t.Errorf("Write(/etc/escape) error = %v, want ErrPathEscape", err)
	}

	// The original symlink target must remain untouched: nothing was written
	// through it, and no file appeared outside the root via the link.
	if _, err := os.Lstat(filepath.Join(root, "evil")); err != nil {
		t.Fatalf("evil link vanished: %v", err)
	}
}

func TestStatThroughSymlinkEscapesRejected(t *testing.T) {
	ws, _ := newSymlinkWorkspace(t)
	ctx := context.Background()

	if _, err := ws.Stat(ctx, "evil"); !errors.Is(err, ErrPathEscape) {
		t.Errorf("Stat(evil) error = %v, want ErrPathEscape", err)
	}
	if _, err := ws.Stat(ctx, "up/secret.txt"); !errors.Is(err, ErrPathEscape) {
		t.Errorf("Stat(up/secret.txt) error = %v, want ErrPathEscape", err)
	}
}

// A legitimate file inside the root must still be readable/writable/stat-able so
// the os.Root confinement does not break normal operation.
func TestInRootFileStillWorks(t *testing.T) {
	ws, _ := newSymlinkWorkspace(t)
	ctx := context.Background()

	if err := ws.Write(ctx, "sub/ok.txt", []byte("hello")); err != nil {
		t.Fatalf("Write(sub/ok.txt): %v", err)
	}
	got, err := ws.Read(ctx, "sub/ok.txt")
	if err != nil {
		t.Fatalf("Read(sub/ok.txt): %v", err)
	}
	if string(got) != "hello" {
		t.Fatalf("Read = %q, want %q", got, "hello")
	}
	if _, err := ws.Stat(ctx, "sub/ok.txt"); err != nil {
		t.Fatalf("Stat(sub/ok.txt): %v", err)
	}
}

// fingerprint (the read-before-edit ledger seam) must also refuse to follow an
// escaping symlink.
func TestFingerprintThroughSymlinkEscapesRejected(t *testing.T) {
	ws, _ := newSymlinkWorkspace(t)
	if _, err := ws.fingerprint("evil"); !errors.Is(err, ErrPathEscape) {
		t.Errorf("fingerprint(evil) error = %v, want ErrPathEscape", err)
	}
}

// Glob and the recursive Grep walk must not surface content reached through an
// escaping symlink.
func TestGlobAndGrepDoNotFollowEscapingSymlinks(t *testing.T) {
	ws, _ := newSymlinkWorkspace(t)
	ctx := context.Background()

	// "evil" is a symlink to an outside file; Glob("*") must not list it.
	matches, err := ws.Glob(ctx, "*")
	if err != nil {
		t.Fatalf("Glob: %v", err)
	}
	for _, m := range matches {
		if m == "evil" || m == "up" {
			t.Errorf("Glob surfaced escaping symlink %q", m)
		}
	}

	// Grep across the tree must not read the outside target's content.
	hits, err := ws.Grep(ctx, "top secret", "")
	if err != nil {
		t.Fatalf("Grep: %v", err)
	}
	if len(hits) != 0 {
		t.Errorf("Grep followed escaping symlink, got %d hits", len(hits))
	}
}
