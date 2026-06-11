package app

import (
	"os"
	"path/filepath"
	"testing"
)

// TestBuildUserModelStoreDisabledReturnsNilInterface guards the typed-nil
// discipline on catalogAssets (see the field doc): buildUserModelStore returns
// the tool.MemoryStore INTERFACE, so its disabled paths must yield an untyped
// nil — a typed-nil *memory.Store smuggled into the interface would make every
// downstream `!= nil` check (registerMemoryFamilies, buildInstructionAssembler,
// maybeWrapUserModelReview, userModelLister) wrongly treat the disabled store
// as wired. Two of the three nil paths are covered (the xdg no-dir path needs
// env surgery and is the same `return nil` shape).
func TestBuildUserModelStoreDisabledReturnsNilInterface(t *testing.T) {
	t.Run("disabled via --no-user-model", func(t *testing.T) {
		cfg := Config{NoUserModel: true}
		if got := buildUserModelStore(cfg); got != nil {
			t.Fatalf("buildUserModelStore(NoUserModel) = %#v, want a nil interface value", got)
		}
	})

	t.Run("unopenable store dir", func(t *testing.T) {
		// UserModelDir pointing at an existing regular FILE makes memory.New's
		// MkdirAll fail, exercising the fail-soft could-not-open path.
		file := filepath.Join(t.TempDir(), "not-a-dir")
		if err := os.WriteFile(file, []byte("occupied"), 0o600); err != nil {
			t.Fatalf("WriteFile: %v", err)
		}
		cfg := Config{UserModelDir: file}
		if got := buildUserModelStore(cfg); got != nil {
			t.Fatalf("buildUserModelStore(UserModelDir=regular file) = %#v, want a nil interface value", got)
		}
	})
}
