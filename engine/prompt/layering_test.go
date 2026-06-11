package prompt_test

import (
	"go/build"
	"strings"
	"testing"
)

// TestPromptDoesNotImportMemoryAdapter is the layering proof for the tier-0
// memory index: the prompt DOMAIN package consumes memory via the consumer-defined
// MemoryIndexSource port (using tool.MemoryEntry, which it already imports), and
// must NEVER import the memory ADAPTER. The adapter meets the port only in the
// composition layer. This scans prompt's transitive imports and fails on any
// adapter edge: anything under engine/adapter/ (the in-tree reference adapters,
// test-only for the core) or any module-internal package OUTSIDE the engine/
// subtree (the host repo's heavy adapters and composition live there).
func TestPromptDoesNotImportMemoryAdapter(t *testing.T) {
	const (
		module        = "github.com/stacklok/mecatl/"
		engineSubtree = module + "engine/"
		adapterClass  = module + "engine/adapter/"
	)
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
			if strings.HasPrefix(imp, adapterClass) ||
				(strings.HasPrefix(imp, module) && !strings.HasPrefix(imp, engineSubtree)) {
				t.Errorf("prompt transitively imports an adapter (layering violation): %s -> %s", pkg, imp)
			}
			if strings.HasPrefix(imp, module) {
				walk(imp)
			}
		}
	}
	walk("github.com/stacklok/mecatl/engine/prompt")
}
