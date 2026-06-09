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

// TestSelectionStoreRoundTrip covers save→load for a per-workspace entry, and that a
// pick is scoped to ITS workspace ONLY — an unseen repo falls back to the server
// default (zero selection), never inheriting another workspace's pick.
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
	// wsB has no entry → falls back to the server default (zero selection), NOT wsA's
	// pick. A pick must not leak across workspaces.
	if got := (store2.Load(wsB)); got != (client.ModelSelection{}) {
		t.Fatalf("Load(wsB) = %+v, want the zero selection (server default), not wsA's pick", got)
	}

	// A second Save for wsB updates wsB ONLY, leaving wsA intact and the global
	// default still unset.
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
	// A brand-new repo still falls back to the server default — it does NOT inherit
	// the most-recent pick.
	if got := store3.Load(t.TempDir()); got != (client.ModelSelection{}) {
		t.Fatalf("Load(new repo) = %+v, want the zero selection (server default)", got)
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
	// Loading via the symlink must hit the SAME realpath-keyed entry as the target —
	// verify it's the target's entry and not some other workspace's by also saving a
	// DIFFERENT entry for an unrelated workspace.
	if err := store.Save(t.TempDir(), client.ModelSelection{ProviderID: "openai", ModelID: "other"}); err != nil {
		t.Fatalf("Save(other): %v", err)
	}
	got := newSelectionStore(fakeStateEnv(stateHome)).Load(link)
	if got != sel {
		t.Fatalf("Load(symlink) = %+v, want the target's entry %+v (realpath-keyed)", got, sel)
	}
}

// TestSelectionStoreSaveGlobalDefault covers the global-default writer: it
// round-trips, is read by LoadGlobalDefault, and PRESERVES the per-workspace map +
// version (read-modify-write touching only the default block).
func TestSelectionStoreSaveGlobalDefault(t *testing.T) {
	stateHome := t.TempDir()
	ws := t.TempDir()
	store := newSelectionStore(fakeStateEnv(stateHome))

	// Seed a per-workspace entry first, so the global save must preserve it.
	wsSel := client.ModelSelection{ProviderID: "openrouter", ModelID: "anthropic/claude"}
	if err := store.Save(ws, wsSel); err != nil {
		t.Fatalf("Save(ws): %v", err)
	}
	def := client.ModelSelection{ProviderID: "openai", ModelID: "gpt-5"}
	if err := store.SaveGlobalDefault(def); err != nil {
		t.Fatalf("SaveGlobalDefault: %v", err)
	}

	// A fresh store (relaunch) reads the global default back AND still has the
	// per-workspace entry — the global write did not clobber it.
	store2 := newSelectionStore(fakeStateEnv(stateHome))
	if got := store2.LoadGlobalDefault(); got != def {
		t.Fatalf("LoadGlobalDefault = %+v, want %+v", got, def)
	}
	if got, ok := store2.LoadWorkspace(ws); !ok || got != wsSel {
		t.Fatalf("LoadWorkspace(ws) = %+v ok=%v, want %+v (global save must preserve workspaces)", got, ok, wsSel)
	}

	// Updating the global default again leaves the per-workspace entry intact.
	def2 := client.ModelSelection{ProviderID: "openai", ModelID: "gpt-5-mini"}
	if err := store2.SaveGlobalDefault(def2); err != nil {
		t.Fatalf("SaveGlobalDefault(2): %v", err)
	}
	store3 := newSelectionStore(fakeStateEnv(stateHome))
	if got := store3.LoadGlobalDefault(); got != def2 {
		t.Fatalf("LoadGlobalDefault(2) = %+v, want %+v", got, def2)
	}
	if got, ok := store3.LoadWorkspace(ws); !ok || got != wsSel {
		t.Fatalf("LoadWorkspace(ws) after global update = %+v ok=%v, want %+v", got, ok, wsSel)
	}
}

// TestSelectionStoreLoadPrefersWorkspaceOverDefault asserts Load() precedence: a
// workspace WITH a per-workspace entry resolves to THAT entry (not the global
// default), while an unseen workspace falls back to the global default. LoadWorkspace
// reports the distinction (ok=false for the unseen one) the picker provenance needs.
func TestSelectionStoreLoadPrefersWorkspaceOverDefault(t *testing.T) {
	stateHome := t.TempDir()
	wsWith := t.TempDir()
	wsWithout := t.TempDir()
	store := newSelectionStore(fakeStateEnv(stateHome))

	def := client.ModelSelection{ProviderID: "openai", ModelID: "gpt-5"}
	if err := store.SaveGlobalDefault(def); err != nil {
		t.Fatalf("SaveGlobalDefault: %v", err)
	}
	wsSel := client.ModelSelection{ProviderID: "openrouter", ModelID: "anthropic/claude"}
	if err := store.Save(wsWith, wsSel); err != nil {
		t.Fatalf("Save(wsWith): %v", err)
	}

	store2 := newSelectionStore(fakeStateEnv(stateHome))
	// The workspace WITH an entry resolves to its entry, NOT the global default.
	if got := store2.Load(wsWith); got != wsSel {
		t.Fatalf("Load(wsWith) = %+v, want the workspace entry %+v (workspace wins over default)", got, wsSel)
	}
	if got, ok := store2.LoadWorkspace(wsWith); !ok || got != wsSel {
		t.Fatalf("LoadWorkspace(wsWith) = %+v ok=%v, want %+v / true", got, ok, wsSel)
	}
	// The unseen workspace falls back to the global default; LoadWorkspace says "no
	// per-workspace entry" (ok=false) so provenance can distinguish the two cases.
	if got := store2.Load(wsWithout); got != def {
		t.Fatalf("Load(wsWithout) = %+v, want the global default %+v", got, def)
	}
	if _, ok := store2.LoadWorkspace(wsWithout); ok {
		t.Fatalf("LoadWorkspace(wsWithout) ok=true, want false (no per-workspace entry)")
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
