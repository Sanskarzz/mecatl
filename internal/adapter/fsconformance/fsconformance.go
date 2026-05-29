// Package fsconformance provides a shared conformance test suite for the
// tool.Workspace interface. Adapters (osfs, memfs, ...) call Run with a factory
// that constructs a fresh workspace, and the suite exercises only the
// tool.Workspace interface.
//
// Importing "testing" in a non-_test.go file is intentional here: this is a
// test-helper package whose sole purpose is to be imported by adapter tests,
// the conventional Go pattern for shared conformance suites (cf. testing/fstest).
package fsconformance

import (
	"context"
	"strings"
	"testing"

	"github.com/stacklok/ozzharness/internal/tool"
)

// Run executes the shared Workspace conformance table against the workspace
// produced by newWS. newWS must return a fresh, isolated workspace each call.
func Run(t *testing.T, newWS func(t *testing.T) tool.Workspace) {
	t.Helper()
	ctx := context.Background()

	t.Run("write-read round trip", func(t *testing.T) {
		ws := newWS(t)
		want := []byte("hello world\n")
		if err := ws.Write(ctx, "dir/file.txt", want); err != nil {
			t.Fatalf("Write: %v", err)
		}
		got, err := ws.Read(ctx, "dir/file.txt")
		if err != nil {
			t.Fatalf("Read: %v", err)
		}
		if string(got) != string(want) {
			t.Fatalf("round trip = %q want %q", got, want)
		}
	})

	t.Run("stat", func(t *testing.T) {
		ws := newWS(t)
		if err := ws.Write(ctx, "a/b.txt", []byte("12345")); err != nil {
			t.Fatalf("Write: %v", err)
		}
		fi, err := ws.Stat(ctx, "a/b.txt")
		if err != nil {
			t.Fatalf("Stat: %v", err)
		}
		if fi.Name != "b.txt" {
			t.Errorf("Name = %q want b.txt", fi.Name)
		}
		if fi.Size != 5 {
			t.Errorf("Size = %d want 5", fi.Size)
		}
		if fi.IsDir {
			t.Errorf("IsDir = true want false")
		}
	})

	t.Run("glob", func(t *testing.T) {
		ws := newWS(t)
		for _, p := range []string{"x/one.go", "x/two.go", "x/three.txt"} {
			if err := ws.Write(ctx, p, []byte("z")); err != nil {
				t.Fatalf("Write %s: %v", p, err)
			}
		}
		got, err := ws.Glob(ctx, "x/*.go")
		if err != nil {
			t.Fatalf("Glob: %v", err)
		}
		if len(got) != 2 {
			t.Fatalf("Glob returned %d matches (%v) want 2", len(got), got)
		}
		for _, m := range got {
			if !strings.HasSuffix(m, ".go") {
				t.Errorf("unexpected glob match %q", m)
			}
		}
	})

	t.Run("path escape rejected", func(t *testing.T) {
		ws := newWS(t)
		escapes := []string{
			"../etc/passwd",
			"../../secret",
			"/etc/passwd",
			"a/../../b",
		}
		for _, p := range escapes {
			if _, err := ws.Read(ctx, p); err == nil {
				t.Errorf("Read(%q) = nil err, want escape rejection", p)
			}
			if err := ws.Write(ctx, p, []byte("x")); err == nil {
				t.Errorf("Write(%q) = nil err, want escape rejection", p)
			}
			if _, err := ws.Stat(ctx, p); err == nil {
				t.Errorf("Stat(%q) = nil err, want escape rejection", p)
			}
		}
	})

	t.Run("read ledger detects change", func(t *testing.T) {
		ws := newWS(t)
		if err := ws.Write(ctx, "led.txt", []byte("original")); err != nil {
			t.Fatalf("Write: %v", err)
		}
		// Never recorded -> false.
		if ok, err := ws.WasReadUnchanged(ctx, "led.txt"); err != nil || ok {
			t.Fatalf("WasReadUnchanged before record = (%v,%v) want (false,nil)", ok, err)
		}
		// Record a read; unchanged should be true.
		ws.RecordRead("led.txt", "v1")
		ok, err := ws.WasReadUnchanged(ctx, "led.txt")
		if err != nil {
			t.Fatalf("WasReadUnchanged: %v", err)
		}
		if !ok {
			t.Fatalf("WasReadUnchanged after record = false want true")
		}
		// Change the file; unchanged should flip to false.
		if err := ws.Write(ctx, "led.txt", []byte("modified content")); err != nil {
			t.Fatalf("Write change: %v", err)
		}
		ok, err = ws.WasReadUnchanged(ctx, "led.txt")
		if err != nil {
			t.Fatalf("WasReadUnchanged after change: %v", err)
		}
		if ok {
			t.Fatalf("WasReadUnchanged after change = true want false")
		}
	})
}
