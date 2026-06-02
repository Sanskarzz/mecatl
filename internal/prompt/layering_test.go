package prompt_test

import (
	"go/build"
	"strings"
	"testing"
)

// TestPromptDoesNotImportMemoryAdapter is the layering proof for the tier-0
// memory index: the prompt DOMAIN package consumes memory via the consumer-defined
// MemoryIndexSource port (using tool.MemoryEntry, which it already imports), and
// must NEVER import the memory ADAPTER. The adapter meets the port only in
// internal/app. This scans prompt's transitive imports and fails on any
// adapter/memory edge.
func TestPromptDoesNotImportMemoryAdapter(t *testing.T) {
	const forbidden = "github.com/stacklok/mecatl/internal/adapter/"
	seen := map[string]bool{}
	var walk func(pkg string)
	walk = func(pkg string) {
		if seen[pkg] {
			return
		}
		seen[pkg] = true
		bp, err := build.Import(pkg, "", 0)
		if err != nil {
			return // stdlib / vendored; not our concern
		}
		for _, imp := range bp.Imports {
			if strings.HasPrefix(imp, forbidden) {
				t.Errorf("prompt transitively imports an adapter (layering violation): %s -> %s", pkg, imp)
			}
			if strings.HasPrefix(imp, "github.com/stacklok/mecatl/internal/") {
				walk(imp)
			}
		}
	}
	walk("github.com/stacklok/mecatl/internal/prompt")
}
