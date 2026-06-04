package app

import (
	"context"
	"testing"

	"github.com/stacklok/mecatl/internal/adapter/hookexec"
	"github.com/stacklok/mecatl/internal/adapter/memory"
	"github.com/stacklok/mecatl/internal/adapter/mockllm"
)

// TestBuildCatalogRegistersMemorySearchWhenEnabled proves that the REAL wiring
// (buildCatalog, the same function buildEngine calls) registers all three memory
// tools — Remember, Recall, AND SearchMemory — into the catalog exactly when a
// per-project memory directory is configured (cfg.MemoryDir set), and registers
// NONE of them when memory is disabled (cfg.MemoryDir == ""). This guards the
// "memory enabled ⇒ SearchMemory is available" claim through the composition
// layer, not just the adapter's own Register helper.
func TestBuildCatalogRegistersMemorySearchWhenEnabled(t *testing.T) {
	ctx := context.Background()
	provider := mockllm.New(mockllm.TextTurn("x"))
	hooks := hookexec.New(nil)

	memoryToolNames := []string{
		memory.SearchMemoryToolName,
		memory.RecallToolName,
		memory.RememberToolName,
	}

	t.Run("enabled", func(t *testing.T) {
		cfg := Config{MemoryDir: t.TempDir()}
		cat, _, _, _, _, _, _, closeFn := buildCatalog(ctx, cfg, provider, hooks)
		defer closeFn()

		for _, name := range memoryToolNames {
			if _, ok := cat.Lookup(name); !ok {
				t.Errorf("memory enabled (MemoryDir set): catalog is missing %q", name)
			}
		}
	})

	t.Run("disabled", func(t *testing.T) {
		cfg := Config{MemoryDir: ""}
		cat, _, _, _, _, _, _, closeFn := buildCatalog(ctx, cfg, provider, hooks)
		defer closeFn()

		for _, name := range memoryToolNames {
			if _, ok := cat.Lookup(name); ok {
				t.Errorf("memory disabled (MemoryDir empty): catalog should NOT contain %q", name)
			}
		}
	})
}
