package soul

import (
	"os"
	"path/filepath"
	"testing"

	"github.com/stacklok/mecatl/engine/adapter/sourceconformance"
	"github.com/stacklok/mecatl/engine/prompt"
)

// TestStoreSoulSourceConformance runs the shared SoulSource conformance table
// over the local file store: each case's body is written to a temp file and
// loaded through the full Store discipline (bounded read, trim, validation,
// fail-soft) — the same contract the remote-driver client passes via its
// re-validation.
func TestStoreSoulSourceConformance(t *testing.T) {
	sourceconformance.RunSoulSource(t, func(t *testing.T, body string) prompt.SoulSource {
		path := filepath.Join(t.TempDir(), "soul.md")
		if err := os.WriteFile(path, []byte(body), 0o644); err != nil {
			t.Fatalf("write soul file: %v", err)
		}
		return New(Options{Path: path})
	})
}
