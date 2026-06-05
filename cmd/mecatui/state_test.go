package main

import (
	"os"
	"path/filepath"
	"testing"

	"github.com/stacklok/mecatl/cmd/mecatui/client"
	"github.com/stacklok/mecatl/internal/adapter/xdgconfig"
)

// fakeStateEnv returns a ResolveEnv whose XDG_STATE_HOME points at stateHome, so
// the store resolves models.yaml under a temp dir — fully offline, no real $HOME.
func fakeStateEnv(stateHome string) xdgconfig.ResolveEnv {
	return xdgconfig.ResolveEnv{
		Getenv: func(k string) string {
			if k == "XDG_STATE_HOME" {
				return stateHome
			}
			return ""
		},
		UserHomeDir: func() (string, error) { return "", os.ErrNotExist },
		ReadFile:    os.ReadFile,
	}
}

// TestSelectionStoreRoundTrip covers save→load for both the global default and a
// per-workspace entry, and that Save updates BOTH (a fresh repo inherits default).
func TestSelectionStoreRoundTrip(t *testing.T) {
	stateHome := t.TempDir()
	wsA := t.TempDir()
	wsB := t.TempDir()
	store := newSelectionStore(fakeStateEnv(stateHome))

	selA := client.ModelSelection{ProviderID: "openrouter", ModelID: "anthropic/claude"}
	if err := store.Save(wsA, selA); err != nil {
		t.Fatalf("Save(wsA): %v", err)
	}

	// A fresh store (simulating a relaunch) reads the per-workspace entry back.
	store2 := newSelectionStore(fakeStateEnv(stateHome))
	if got := store2.Load(wsA); got != selA {
		t.Fatalf("Load(wsA) = %+v, want %+v", got, selA)
	}
	// wsB has no entry yet → inherits the global default (== the last Save).
	if got := store2.Load(wsB); got != selA {
		t.Fatalf("Load(wsB) = %+v, want the default %+v", got, selA)
	}

	// A second Save for wsB updates wsB AND the default, leaving wsA intact.
	selB := client.ModelSelection{ProviderID: "openai", ModelID: "gpt-5"}
	if err := store2.Save(wsB, selB); err != nil {
		t.Fatalf("Save(wsB): %v", err)
	}
	store3 := newSelectionStore(fakeStateEnv(stateHome))
	if got := store3.Load(wsA); got != selA {
		t.Fatalf("Load(wsA) after wsB save = %+v, want %+v (per-workspace preserved)", got, selA)
	}
	if got := store3.Load(wsB); got != selB {
		t.Fatalf("Load(wsB) = %+v, want %+v", got, selB)
	}
	// A brand-new repo now inherits selB (the new default).
	if got := store3.Load(t.TempDir()); got != selB {
		t.Fatalf("Load(new repo) = %+v, want the updated default %+v", got, selB)
	}
}

// TestSelectionStoreRealpathKeying asserts a symlinked workspace resolves to the
// SAME entry as its target (realpath-keyed, mirroring the trust registry).
func TestSelectionStoreRealpathKeying(t *testing.T) {
	stateHome := t.TempDir()
	target := t.TempDir()
	link := filepath.Join(t.TempDir(), "link")
	if err := os.Symlink(target, link); err != nil {
		t.Skipf("symlink unsupported: %v", err)
	}
	store := newSelectionStore(fakeStateEnv(stateHome))

	sel := client.ModelSelection{ProviderID: "openai", ModelID: "gpt-5"}
	if err := store.Save(target, sel); err != nil {
		t.Fatalf("Save(target): %v", err)
	}
	// Loading via the symlink must hit the SAME realpath-keyed entry, not fall to
	// the (here identical) default — verify by also setting a DIFFERENT default.
	if err := store.Save(t.TempDir(), client.ModelSelection{ProviderID: "openai", ModelID: "other"}); err != nil {
		t.Fatalf("Save(other): %v", err)
	}
	got := newSelectionStore(fakeStateEnv(stateHome)).Load(link)
	if got != sel {
		t.Fatalf("Load(symlink) = %+v, want the target's entry %+v (realpath-keyed)", got, sel)
	}
}

// TestSelectionStoreFailSoftRead covers the fail-soft read paths: a missing file
// and a malformed file both yield the zero selection, never a crash.
func TestSelectionStoreFailSoftRead(t *testing.T) {
	t.Run("missing file", func(t *testing.T) {
		store := newSelectionStore(fakeStateEnv(t.TempDir()))
		if got := store.Load(t.TempDir()); !got.IsZero() {
			t.Fatalf("Load on missing file = %+v, want zero", got)
		}
	})
	t.Run("malformed yaml", func(t *testing.T) {
		stateHome := t.TempDir()
		path := filepath.Join(stateHome, stateSubpath)
		if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(path, []byte(":\n  not: [valid"), 0o600); err != nil {
			t.Fatal(err)
		}
		store := newSelectionStore(fakeStateEnv(stateHome))
		if got := store.Load(t.TempDir()); !got.IsZero() {
			t.Fatalf("Load on malformed file = %+v, want zero (fail-soft)", got)
		}
	})
	t.Run("wrong version", func(t *testing.T) {
		stateHome := t.TempDir()
		path := filepath.Join(stateHome, stateSubpath)
		if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(path, []byte("version: 999\ndefault: {providerId: x, modelId: y}\n"), 0o600); err != nil {
			t.Fatal(err)
		}
		store := newSelectionStore(fakeStateEnv(stateHome))
		if got := store.Load(t.TempDir()); !got.IsZero() {
			t.Fatalf("Load on wrong-version file = %+v, want zero (fail-safe)", got)
		}
	})
}

// TestSelectionStoreAtomicWritePerms asserts the persisted file is owner-only
// (0o600), matching the trust registry discipline.
func TestSelectionStoreAtomicWritePerms(t *testing.T) {
	stateHome := t.TempDir()
	store := newSelectionStore(fakeStateEnv(stateHome))
	if err := store.Save(t.TempDir(), client.ModelSelection{ProviderID: "openai", ModelID: "gpt-5"}); err != nil {
		t.Fatalf("Save: %v", err)
	}
	fi, err := os.Stat(filepath.Join(stateHome, stateSubpath))
	if err != nil {
		t.Fatalf("stat models.yaml: %v", err)
	}
	if perm := fi.Mode().Perm(); perm != 0o600 {
		t.Fatalf("models.yaml perms = %o, want 0600", perm)
	}
}

// TestSelectionStoreNoStateDir asserts that with no XDG state dir resolvable,
// persistence degrades to a no-op (Load zero, Save errors) rather than crashing or
// writing somewhere bogus.
func TestSelectionStoreNoStateDir(t *testing.T) {
	env := xdgconfig.ResolveEnv{
		Getenv:      func(string) string { return "" },
		UserHomeDir: func() (string, error) { return "", os.ErrNotExist },
		ReadFile:    os.ReadFile,
	}
	store := newSelectionStore(env)
	if got := store.Load(t.TempDir()); !got.IsZero() {
		t.Fatalf("Load with no state dir = %+v, want zero", got)
	}
	if err := store.Save(t.TempDir(), client.ModelSelection{ProviderID: "openai"}); err == nil {
		t.Fatal("Save with no state dir should report an error (the ui treats it fail-soft)")
	}
}
