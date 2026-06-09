package app

import (
	"strings"
	"testing"
)

// TestExplorerPromptInstructsReferences pins the References convention (iteration-5
// D5b): the DEFAULT read-only explorer child's Role instructs the child to END its
// summary with a "References:" section listing relevant file paths, so the most common
// Subagent deliverable is navigable without re-searching. It asserts on explorerPromptConfig
// (the single site that augments the explorer Role) — distinct from the shared
// defaultTone "Cite code as file_path:line" inline-citation sentence, which is a
// different instruction.
func TestExplorerPromptInstructsReferences(t *testing.T) {
	cfg := Config{Model: "m"}

	base := promptConfig(cfg, cfg.gitStatus).Role
	got := explorerPromptConfig(cfg).Role

	if !strings.Contains(got, "References:") {
		t.Fatalf("explorer Role must instruct a References: block, got:\n%s", got)
	}
	// It is an ADDITION to the explorer Role, not a replacement of the base framing.
	if !strings.HasPrefix(got, base) {
		t.Fatalf("explorer Role must extend the base Role, not replace it")
	}
	// Guard that this is NOT just the pre-existing defaultTone citation sentence: the
	// base Role (without the explorer augmentation) must NOT already contain "References:".
	if strings.Contains(base, "References:") {
		t.Fatalf("the References instruction must come from explorerPromptConfig, not the shared base Role")
	}
	// The instruction asks for file PATHS specifically (path or path:line).
	if !strings.Contains(got, "path") {
		t.Fatalf("References instruction should name file paths, got:\n%s", got)
	}
}
