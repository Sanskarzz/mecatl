package app

import (
	"testing"

	"github.com/stacklok/mecatl/internal/prompt"
)

// TestBuildSoulSourceDefaultOnAndDisable proves the REAL composition wiring
// (buildSoulSource, the same function buildEngine calls) honours the R10 product
// decision: soul is ON BY DEFAULT (a non-nil prompt.SoulSource for a zero Config),
// an explicit --soul-file path is honoured, and --no-soul (NoSoul) turns it OFF —
// returning an UNTYPED nil so buildInstructionAssembler's nil guard holds (the
// typed-nil gotcha the wiring comment flags).
func TestBuildSoulSourceDefaultOnAndDisable(t *testing.T) {
	t.Run("default on", func(t *testing.T) {
		// (a) default Config{} → soul is on by default (a missing file is fail-soft,
		// so on-by-default costs nothing at run time).
		if src := buildSoulSource(Config{}); src == nil {
			t.Fatal("buildSoulSource(Config{}) returned nil; soul must be ON by default (R10)")
		}
	})

	t.Run("explicit path on", func(t *testing.T) {
		// (c) an explicit --soul-file path is honoured (non-nil source).
		if src := buildSoulSource(Config{SoulPath: "/explicit/path"}); src == nil {
			t.Fatal("buildSoulSource with SoulPath returned nil; the override path must be honoured")
		}
	})

	t.Run("no-soul off", func(t *testing.T) {
		// (b) Config{NoSoul:true} → nil source, AND buildInstructionAssembler with
		// that nil source (and nil memStore) returns a BARE RootAssembler, not a
		// MultiAssembler — exercising the typed-nil guard end to end.
		src := buildSoulSource(Config{NoSoul: true})
		if src != nil {
			t.Fatalf("buildSoulSource(Config{NoSoul:true}) = %v, want nil (soul disabled)", src)
		}
		asm := buildInstructionAssembler(src, nil, nil)
		if _, ok := asm.(prompt.RootAssembler); !ok {
			t.Fatalf("with nil soul + nil memStore, buildInstructionAssembler returned %T, want a bare prompt.RootAssembler (typed-nil guard)", asm)
		}
	})
}
