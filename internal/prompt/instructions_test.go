package prompt_test

import (
	"context"
	"testing"

	"github.com/stacklok/ozzharness/internal/adapter/memfs"
	"github.com/stacklok/ozzharness/internal/prompt"
)

// TestRootAssemblerMatchesDiscoverInstructions verifies the default assembler
// reproduces DiscoverInstructions byte-for-byte (P3 default-preserves-behaviour).
func TestRootAssemblerMatchesDiscoverInstructions(t *testing.T) {
	ws := memfs.NewWorkspace("/proj")
	if err := ws.Write(context.Background(), "AGENTS.md", []byte("be terse")); err != nil {
		t.Fatalf("seed: %v", err)
	}

	want, err := prompt.DiscoverInstructions(context.Background(), ws)
	if err != nil {
		t.Fatalf("DiscoverInstructions: %v", err)
	}
	got, err := prompt.RootAssembler{}.Assemble(context.Background(), ws)
	if err != nil {
		t.Fatalf("Assemble: %v", err)
	}
	if len(got) != len(want) {
		t.Fatalf("len(Assemble)=%d, len(Discover)=%d", len(got), len(want))
	}
	for i := range want {
		if got[i].Text != want[i].Text || got[i].Role != want[i].Role {
			t.Fatalf("message[%d]: Assemble=%+v want %+v", i, got[i], want[i])
		}
	}
}

// TestRootAssemblerEmptyWorkspace verifies no instructions and no error when no
// instruction files are present.
func TestRootAssemblerEmptyWorkspace(t *testing.T) {
	ws := memfs.NewWorkspace("/proj")
	got, err := prompt.RootAssembler{}.Assemble(context.Background(), ws)
	if err != nil {
		t.Fatalf("Assemble: %v", err)
	}
	if len(got) != 0 {
		t.Fatalf("got %d messages, want 0", len(got))
	}
}
