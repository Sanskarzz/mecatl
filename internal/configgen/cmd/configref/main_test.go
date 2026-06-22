package main

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// TestGenerateSmoke drives the generator end-to-end (the go/ast harvest + reflect +
// render + write) to a temp dir and asserts both artifacts are non-empty and carry a
// representative key. It localizes a generator failure here rather than only surfacing
// it as a CI diff-gate mismatch.
func TestGenerateSmoke(t *testing.T) {
	dir := t.TempDir()
	skeleton := filepath.Join(dir, "settings.skeleton.yaml")
	reference := filepath.Join(dir, "configuration-reference.md")

	if err := generate(skeleton, reference); err != nil {
		t.Fatalf("generate: %v", err)
	}

	sk, err := os.ReadFile(skeleton)
	if err != nil {
		t.Fatalf("reading skeleton: %v", err)
	}
	ref, err := os.ReadFile(reference)
	if err != nil {
		t.Fatalf("reading reference: %v", err)
	}
	if len(sk) == 0 || len(ref) == 0 {
		t.Fatalf("generated artifacts are empty (skeleton=%d bytes, reference=%d bytes)", len(sk), len(ref))
	}
	// A representative key from each of the four subtrees proves the harvest+render ran,
	// not just that a file was written.
	for _, key := range []string{"permissions:", "guardrails:", "posture:", "models:"} {
		if !strings.Contains(string(sk), key) {
			t.Errorf("generated skeleton missing %q", key)
		}
	}
	if !strings.Contains(string(ref), "`models.router.disabled`") {
		// The reference path drops the "[]" array markers; check the normalized form.
		if !strings.Contains(strings.ReplaceAll(string(ref), "[]", ""), "`models.router.disabled`") {
			t.Error("generated reference missing the models.router.disabled row")
		}
	}
	// The doc-comment harvest must have run: the reference carries real field prose, not
	// just the bare key.
	if !strings.Contains(string(ref), "kill switch") && !strings.Contains(string(ref), "kill-switch") {
		t.Error("generated reference appears to lack harvested doc-comments")
	}
}

// TestGenerateSkipsEmptyPath confirms an empty path skips that artifact (no panic, no
// stray write).
func TestGenerateSkipsEmptyPath(t *testing.T) {
	dir := t.TempDir()
	only := filepath.Join(dir, "only.md")
	if err := generate("", only); err != nil {
		t.Fatalf("generate reference-only: %v", err)
	}
	if _, err := os.Stat(only); err != nil {
		t.Fatalf("reference-only write missing: %v", err)
	}
	entries, _ := os.ReadDir(dir)
	if len(entries) != 1 {
		t.Errorf("expected exactly one written file, got %d", len(entries))
	}
}
