package main

import (
	"errors"
	"fmt"
	"io"
	"io/fs"
	"os"
	"path/filepath"
	"strings"
	"syscall"

	yaml "go.yaml.in/yaml/v3"

	"github.com/stacklok/mecatl/cmd/mecatui/client"
	"github.com/stacklok/mecatl/internal/adapter/xdgconfig"
)

// state.go is the mecatui CLIENT-SIDE model-selection state file: the last-used
// (provider, model) the /models picker writes and the next launch reads. It lives
// in the cmd/mecatui MAIN (the composition root), NOT in cmd/mecatui/client — the
// client stays proto/grpc-only and never touches os/xdg. main already imports
// internal/app + adapters, so reusing internal/adapter/xdgconfig here is the same
// legal composition-side import as trust.go's internal/app use.
//
// # File: settings-vs-state split
//
// The file is machine-written STATE, so it lives under XDG_STATE_HOME (not the
// human config base) — the same split the workspace-trust feature established
// (trust.yaml is state under XDG, settings.yaml is human config). Path:
// <XDG_STATE_HOME or ~/.local/state>/mecatui/models.yaml.
//
// # Format: per-workspace map + a global default
//
//	version: 1
//	default:
//	  providerId: openai
//	  modelId: gpt-5
//	workspaces:
//	  /abs/realpath/repo-a: { providerId: openrouter, modelId: anthropic/claude-... }
//
// Read order on launch: workspaces[realpath(ws)] if present, else default. Write on
// select: update BOTH the per-workspace entry AND default (so a brand-new repo
// inherits the last choice rather than falling to the catalog default). Workspaces
// are realpath-keyed (filepath.Abs + EvalSymlinks), mirroring the trust registry,
// so a moved/symlinked repo doesn't fork its state.
//
// # Safety
//
// Read is fail-soft: a missing/oversized/malformed file ⇒ the zero selection (the
// server default), never an error that aborts launch. Write is atomic (temp-file +
// rename, 0o600, O_NOFOLLOW on the final-path symlink guard), mirroring the trust
// registry's Remember discipline; a write failure is returned to the caller (the
// ui surfaces it as a muted notice) but never crashes the picker.

// stateVersion is the schema version main writes and expects. A file with a
// different (unknown) version is ignored fail-safe (treated as no persisted state).
const stateVersion = 1

// maxStateBytes caps the models.yaml read so a pathological file can't blow memory;
// over the cap it is ignored fail-safe (no persisted selection). Mirrors the trust
// registry's cap.
const maxStateBytes = 1 << 20 // 1 MiB

// stateSubpath is the state file relative to the XDG state base.
var stateSubpath = filepath.Join("mecatui", "models.yaml")

// modelSelectionEntry is one persisted (provider, model) pair on disk.
type modelSelectionEntry struct {
	ProviderID string `yaml:"providerId"`
	ModelID    string `yaml:"modelId"`
}

// modelStateFile is the on-disk schema of models.yaml.
type modelStateFile struct {
	Version    int                            `yaml:"version"`
	Default    modelSelectionEntry            `yaml:"default,omitempty"`
	Workspaces map[string]modelSelectionEntry `yaml:"workspaces,omitempty"`
}

// selectionStore is the main-owned concrete SelectionStore (ui.SelectionStore): it
// loads the persisted last-used selection at launch and persists a pick. It is
// bound to a resolved state-file path; tests inject a temp path. A "" path means
// no XDG state dir resolved — persistence is then a no-op (Load returns zero, Save
// reports an error the ui treats fail-soft).
type selectionStore struct {
	path string
}

// newSelectionStore resolves the models.yaml path under XDG_STATE_HOME (via the
// shared xdgconfig leaf) and returns a store bound to it. When no state dir
// resolves (no XDG_STATE_HOME and no home) the path is "" — persistence degrades to
// a no-op rather than anchoring state at a bogus path.
func newSelectionStore(env xdgconfig.ResolveEnv) *selectionStore {
	base := xdgconfig.UserStateDir(env)
	if base == "" {
		return &selectionStore{}
	}
	return &selectionStore{path: filepath.Join(base, stateSubpath)}
}

// Load returns the persisted selection for workspace: the per-workspace entry
// (realpath-keyed) if present, else the global default, else the zero selection.
// Fail-soft: a missing/oversized/malformed/wrong-version file yields the zero
// selection (the server default), never an error.
func (s *selectionStore) Load(workspace string) client.ModelSelection {
	if s == nil || s.path == "" {
		return client.ModelSelection{}
	}
	sf, ok := s.read()
	if !ok {
		return client.ModelSelection{}
	}
	if key, err := realpathState(workspace); err == nil {
		if e, found := sf.Workspaces[key]; found {
			return client.ModelSelection{ProviderID: e.ProviderID, ModelID: e.ModelID}
		}
	}
	return client.ModelSelection{ProviderID: sf.Default.ProviderID, ModelID: sf.Default.ModelID}
}

// Save persists sel as BOTH the per-workspace entry (realpath-keyed) and the global
// default (so a new repo inherits the last choice). It is read-modify-write: it
// preserves other workspaces' entries. A zero (empty) workspace skips the
// per-workspace entry but still updates default. The write is atomic.
func (s *selectionStore) Save(workspace string, sel client.ModelSelection) error {
	if s == nil || s.path == "" {
		return errors.New("mecatui: no XDG state dir to persist the model selection")
	}
	sf, _ := s.read() // a corrupt/absent file ⇒ start fresh (never block a write)
	sf.Version = stateVersion
	if sf.Workspaces == nil {
		sf.Workspaces = make(map[string]modelSelectionEntry)
	}
	entry := modelSelectionEntry{ProviderID: sel.ProviderID, ModelID: sel.ModelID}
	sf.Default = entry
	if key, err := realpathState(workspace); err == nil {
		sf.Workspaces[key] = entry
	}
	out, err := yaml.Marshal(sf)
	if err != nil {
		return err
	}
	return writeStateFile(s.path, out)
}

// read loads + parses the state file. ok is false (and the file is ignored
// fail-soft) on any read/parse failure or an unknown schema version. The open is
// O_NOFOLLOW (symmetric with the WRITE path's symlink guard, CWE-59): a symlinked
// state path fails the open ⇒ treated as no-state, the same fail-soft as a
// malformed file — a planted symlink can never redirect the read.
func (s *selectionStore) read() (modelStateFile, bool) {
	data, err := readStateFile(s.path)
	if err != nil {
		return modelStateFile{}, false
	}
	if len(data) > maxStateBytes {
		return modelStateFile{}, false
	}
	if len(strings.TrimSpace(string(data))) == 0 {
		return modelStateFile{}, false
	}
	var sf modelStateFile
	if err := yaml.Unmarshal(data, &sf); err != nil {
		return modelStateFile{}, false
	}
	if sf.Version != stateVersion {
		return modelStateFile{}, false
	}
	return sf, true
}

// realpathState canonicalizes a workspace to its realpath (filepath.Abs +
// EvalSymlinks), symmetric on Load and Save, so a moved/aliased symlink can't fork
// the state. An empty or unresolvable workspace returns an error (the caller then
// skips the per-workspace entry and uses default).
func realpathState(workspace string) (string, error) {
	if workspace == "" {
		return "", errors.New("empty workspace")
	}
	abs, err := filepath.Abs(workspace)
	if err != nil {
		return "", err
	}
	resolved, err := filepath.EvalSymlinks(abs)
	if err != nil {
		return "", err
	}
	return resolved, nil
}

// readStateFile reads the state file with O_NOFOLLOW, so a symlink at the final
// path fails the open (CWE-59) — symmetric with writeStateFile's symlink guard.
// The caller treats any error fail-soft (no persisted state).
func readStateFile(path string) ([]byte, error) {
	f, err := os.OpenFile(path, os.O_RDONLY|syscall.O_NOFOLLOW, 0) //nolint:gosec // path derived solely from XDG state base, not repo-controlled
	if err != nil {
		return nil, err
	}
	defer func() { _ = f.Close() }()
	return io.ReadAll(f)
}

// writeStateFile atomically replaces the state file with the trust-registry
// discipline: mkdir -p the parent, refuse to write THROUGH a pre-planted symlink at
// the final path (O_NOFOLLOW spirit via an Lstat guard, CWE-59), write to a sibling
// temp file (O_EXCL|0o600), then rename over the target.
func writeStateFile(path string, data []byte) error {
	dir := filepath.Dir(path)
	if err := os.MkdirAll(dir, 0o700); err != nil {
		return err
	}
	if fi, err := os.Lstat(path); err == nil && fi.Mode()&fs.ModeSymlink != 0 {
		return &os.PathError{Op: "open", Path: path, Err: syscall.ELOOP}
	}
	tmp, err := os.CreateTemp(dir, ".models-*.yaml.tmp")
	if err != nil {
		return err
	}
	tmpName := tmp.Name()
	defer func() { _ = os.Remove(tmpName) }()
	if err := tmp.Chmod(0o600); err != nil {
		_ = tmp.Close()
		return err
	}
	if _, err := tmp.Write(data); err != nil {
		_ = tmp.Close()
		return err
	}
	if err := tmp.Close(); err != nil {
		return err
	}
	if err := os.Rename(tmpName, path); err != nil {
		return fmt.Errorf("rename model state: %w", err)
	}
	return nil
}
