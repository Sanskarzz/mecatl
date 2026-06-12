package nofs_test

import (
	"context"
	"errors"
	"io/fs"
	"testing"

	"github.com/stacklok/mecatl/engine/adapter/nofs"
	"github.com/stacklok/mecatl/engine/tool"
)

// TestNoFSWorkspaceContract pins the honest-empty contract of the no-FS
// Workspace: every read fails as fs.ErrNotExist (the standard absent-file
// shape), every search is empty, every write is a loud, model-readable refusal,
// and the root is "". The contract is what makes the no-fs profile HONEST —
// nothing exists that can be lost (the explicit not-memfs decision).
func TestNoFSWorkspaceContract(t *testing.T) {
	ctx := context.Background()
	var ws tool.Workspace = nofs.New()

	if got := ws.Root(); got != "" {
		t.Errorf("Root() = %q, want \"\" (a no-FS session has no session root)", got)
	}

	if _, err := ws.Read(ctx, "any/file.txt"); !errors.Is(err, fs.ErrNotExist) {
		t.Errorf("Read error = %v, want errors.Is(_, fs.ErrNotExist)", err)
	}
	if _, err := ws.Stat(ctx, "any/file.txt"); !errors.Is(err, fs.ErrNotExist) {
		t.Errorf("Stat error = %v, want errors.Is(_, fs.ErrNotExist)", err)
	}

	if matches, err := ws.Glob(ctx, "**/*.go"); err != nil || len(matches) != 0 {
		t.Errorf("Glob = (%v, %v), want empty with no error", matches, err)
	}
	if matches, err := ws.Grep(ctx, "anything", ""); err != nil || len(matches) != 0 {
		t.Errorf("Grep = (%v, %v), want empty with no error", matches, err)
	}

	err := ws.Write(ctx, "new.txt", []byte("data"))
	if !errors.Is(err, nofs.ErrNoFilesystem) {
		t.Errorf("Write error = %v, want ErrNoFilesystem", err)
	}
	if err == nil || err.Error() == "" {
		t.Error("Write must return a non-empty, model-readable refusal")
	}

	// The Edit read-ledger surface is inert: recording is a no-op and the
	// read-before-edit precondition can never hold.
	ws.RecordRead("a.txt", "v1")
	ok, err := ws.WasReadUnchanged(ctx, "a.txt")
	if err != nil || ok {
		t.Errorf("WasReadUnchanged = (%v, %v), want (false, nil)", ok, err)
	}
}

// TestNoFSPathErrorCarriesPath pins that the not-exist errors carry the asked
// path (a *fs.PathError), so a tool's error body names the file the model asked
// for instead of a bare "file does not exist".
func TestNoFSPathErrorCarriesPath(t *testing.T) {
	_, err := nofs.New().Read(context.Background(), "docs/missing.md")
	var pe *fs.PathError
	if !errors.As(err, &pe) {
		t.Fatalf("Read error = %T, want *fs.PathError", err)
	}
	if pe.Path != "docs/missing.md" {
		t.Errorf("PathError.Path = %q, want the asked path", pe.Path)
	}
}
