package osfs

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"testing"
)

// osfs_abspath_test.go pins the absolute-path resolution contract (issue #154):
// an absolute path that canonicalizes INSIDE the workspace root is accepted by
// all five FS tools (reduced to its root-relative form and served through the
// workspace *os.Root), while an absolute path that resolves OUTSIDE the root —
// including an in-workspace symlink whose target escapes — is rejected with
// ErrPathEscape. The security-critical invariant: a symlink inside the workspace
// whose target is OUTSIDE the workspace, addressed by ABSOLUTE path, is rejected
// by resolveInRoot at resolution time, not deferred to os.Root.

// TestAbsoluteInRootReadWriteStat writes a file by relative path and reads it
// back by its absolute in-root alias, then Stats it by absolute path. The
// absolute path is the same physical file, reduced to root-relative form.
func TestAbsoluteInRootReadWriteStat(t *testing.T) {
	root := t.TempDir()
	ws, err := NewWorkspace(root)
	if err != nil {
		t.Fatalf("NewWorkspace: %v", err)
	}
	ctx := context.Background()

	rel := filepath.ToSlash(filepath.Join("sub", "file.go"))
	if err := ws.Write(ctx, rel, []byte("package main\n")); err != nil {
		t.Fatalf("Write(relative): %v", err)
	}

	abs := filepath.Join(ws.Root(), "sub", "file.go")
	got, err := ws.Read(ctx, abs)
	if err != nil {
		t.Fatalf("Read(absolute): %v", err)
	}
	if string(got) != "package main\n" {
		t.Errorf("Read(absolute) = %q, want %q", got, "package main\n")
	}

	fi, err := ws.Stat(ctx, abs)
	if err != nil {
		t.Fatalf("Stat(absolute): %v", err)
	}
	if fi.Name != "file.go" {
		t.Errorf("Stat(absolute).Name = %q, want file.go", fi.Name)
	}

	// Write by absolute path (in-root) must also succeed.
	if err := ws.Write(ctx, abs, []byte("package main // edited\n")); err != nil {
		t.Fatalf("Write(absolute): %v", err)
	}
	got, err = ws.Read(ctx, rel)
	if err != nil {
		t.Fatalf("Read(relative after absolute Write): %v", err)
	}
	if string(got) != "package main // edited\n" {
		t.Errorf("content after absolute Write = %q", got)
	}
}

// TestAbsoluteInRootEditLedgerCrossForm pins the ledger key normalization: a
// file read by absolute path and checked by relative path (and the reverse)
// share ONE ledger entry, so Edit's read-before-edit-and-unchanged invariant
// holds across the two path forms. A mutation via either form flips
// WasReadUnchanged to false for both.
func TestAbsoluteInRootEditLedgerCrossForm(t *testing.T) {
	root := t.TempDir()
	ws, err := NewWorkspace(root)
	if err != nil {
		t.Fatalf("NewWorkspace: %v", err)
	}
	ctx := context.Background()

	rel := "led.txt"
	abs := filepath.Join(ws.Root(), "led.txt")
	if err := ws.Write(ctx, rel, []byte("original")); err != nil {
		t.Fatalf("Write: %v", err)
	}

	// Read by ABSOLUTE, check by RELATIVE → unchanged.
	ws.RecordRead(abs, "")
	if ok, err := ws.WasReadUnchanged(ctx, rel); err != nil || !ok {
		t.Fatalf("read abs / check rel: (%v,%v), want (true,nil)", ok, err)
	}
	// Mutate by RELATIVE → check by ABSOLUTE → changed.
	if err := ws.Write(ctx, rel, []byte("mutated")); err != nil {
		t.Fatalf("Write change: %v", err)
	}
	if ok, err := ws.WasReadUnchanged(ctx, abs); err != nil || ok {
		t.Fatalf("after relative mutation, check abs: (%v,%v), want (false,nil)", ok, err)
	}

	// Reverse: read by RELATIVE, check by ABSOLUTE → unchanged.
	ws.RecordRead(rel, "")
	if ok, err := ws.WasReadUnchanged(ctx, abs); err != nil || !ok {
		t.Fatalf("read rel / check abs: (%v,%v), want (true,nil)", ok, err)
	}
	// Mutate by ABSOLUTE → check by RELATIVE → changed.
	if err := ws.Write(ctx, abs, []byte("mutated again")); err != nil {
		t.Fatalf("Write change (abs): %v", err)
	}
	if ok, err := ws.WasReadUnchanged(ctx, rel); err != nil || ok {
		t.Fatalf("after absolute mutation, check rel: (%v,%v), want (false,nil)", ok, err)
	}
}

// TestAbsoluteOutOfWorkspaceRootStillEscapes pins that an absolute path
// resolving OUTSIDE the workspace root is rejected by Read/Write/Stat with
// ErrPathEscape — including a system path (/etc/passwd) and an arbitrary
// out-of-root path under a fresh tempdir.
func TestAbsoluteOutOfWorkspaceRootStillEscapes(t *testing.T) {
	root := t.TempDir()
	ws, err := NewWorkspace(root)
	if err != nil {
		t.Fatalf("NewWorkspace: %v", err)
	}
	ctx := context.Background()

	other := filepath.Join(t.TempDir(), "other", "file")
	if err := os.MkdirAll(filepath.Dir(other), 0o755); err != nil {
		t.Fatalf("mkdir: %v", err)
	}
	if err := os.WriteFile(other, []byte("out"), 0o644); err != nil {
		t.Fatalf("write: %v", err)
	}

	for _, p := range []string{
		"/etc/passwd",
		other,
	} {
		if _, err := ws.Read(ctx, p); !errors.Is(err, ErrPathEscape) {
			t.Errorf("Read(%q) error = %v, want ErrPathEscape", p, err)
		}
		if err := ws.Write(ctx, p, []byte("x")); !errors.Is(err, ErrPathEscape) {
			t.Errorf("Write(%q) error = %v, want ErrPathEscape", p, err)
		}
		if _, err := ws.Stat(ctx, p); !errors.Is(err, ErrPathEscape) {
			t.Errorf("Stat(%q) error = %v, want ErrPathEscape", p, err)
		}
	}
}

// TestAbsoluteSymlinkedLeafInRootAccepted: a symlink INSIDE root pointing to
// another in-root file, addressed by ABSOLUTE path, resolves in-root and is
// accepted (both the symlink and its target are under the workspace root).
func TestAbsoluteSymlinkedLeafInRootAccepted(t *testing.T) {
	root := t.TempDir()
	ws, err := NewWorkspace(root)
	if err != nil {
		t.Fatalf("NewWorkspace: %v", err)
	}
	ctx := context.Background()

	target := filepath.Join(ws.Root(), "real.txt")
	if err := os.WriteFile(target, []byte("real content"), 0o644); err != nil {
		t.Fatalf("write target: %v", err)
	}
	link := filepath.Join(ws.Root(), "link.txt")
	if err := os.Symlink(target, link); err != nil {
		t.Fatalf("symlink: %v", err)
	}

	got, err := ws.Read(ctx, link)
	if err != nil {
		t.Fatalf("Read(absolute in-root symlink): %v", err)
	}
	if string(got) != "real content" {
		t.Errorf("Read(symlink) = %q, want %q", got, "real content")
	}
}

// TestAbsoluteSymlinkedParentEscapeRejected: a symlinked PARENT component
// pointing outside root, addressed by ABSOLUTE path, is rejected by resolveInRoot
// at resolution time (defense-in-depth; the new resolution catches it before
// os.Root would).
func TestAbsoluteSymlinkedParentEscapeRejected(t *testing.T) {
	root := t.TempDir()
	ws, err := NewWorkspace(root)
	if err != nil {
		t.Fatalf("NewWorkspace: %v", err)
	}
	ctx := context.Background()

	// An outside directory with a file in it.
	outsideDir := t.TempDir()
	outsideFile := filepath.Join(outsideDir, "secret.txt")
	if err := os.WriteFile(outsideFile, []byte("secret"), 0o644); err != nil {
		t.Fatalf("write outside: %v", err)
	}
	// A symlink INSIDE root whose target is the OUTSIDE directory (a parent
	// component that escapes).
	escapeLink := filepath.Join(ws.Root(), "escape")
	if err := os.Symlink(outsideDir, escapeLink); err != nil {
		t.Fatalf("symlink: %v", err)
	}
	abs := filepath.Join(escapeLink, "secret.txt")

	if _, err := ws.Read(ctx, abs); !errors.Is(err, ErrPathEscape) {
		t.Errorf("Read(escaping symlinked parent, absolute) error = %v, want ErrPathEscape", err)
	}
	if err := ws.Write(ctx, abs, []byte("x")); !errors.Is(err, ErrPathEscape) {
		t.Errorf("Write(escaping symlinked parent, absolute) error = %v, want ErrPathEscape", err)
	}
}

// TestAbsoluteSymlinkToOutsideRejectedByResolveInRoot is the SECURITY-CRITICAL
// invariant: a symlink inside the workspace whose target is OUTSIDE the
// workspace, addressed by ABSOLUTE path, must be rejected by resolveInRoot (not
// deferred to os.Root). This pins that the canonicalize-then-reject step in
// resolveInRoot catches an escaping symlink target before any os.Root op.
func TestAbsoluteSymlinkToOutsideRejectedByResolveInRoot(t *testing.T) {
	root := t.TempDir()
	fsys, err := NewFileSystem(root)
	if err != nil {
		t.Fatalf("NewFileSystem: %v", err)
	}

	outside := filepath.Join(t.TempDir(), "passwd")
	if err := os.WriteFile(outside, []byte("root:x:0:0"), 0o644); err != nil {
		t.Fatalf("write outside: %v", err)
	}
	link := filepath.Join(fsys.Root(), "evil")
	if err := os.Symlink(outside, link); err != nil {
		t.Fatalf("symlink: %v", err)
	}

	// resolveInRoot must reject the absolute path through the escaping symlink.
	if _, err := fsys.resolveInRoot(link); !errors.Is(err, ErrPathEscape) {
		t.Errorf("resolveInRoot(escaping symlink) error = %v, want ErrPathEscape", err)
	}
	// And the full Read path must reject it too (not defer to a silent os.Root
	// follow that would read /etc/passwd-equivalent).
	if _, err := fsys.Read(context.Background(), link); !errors.Is(err, ErrPathEscape) {
		t.Errorf("Read(escaping symlink) error = %v, want ErrPathEscape", err)
	}
}
