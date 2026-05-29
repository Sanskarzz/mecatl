// Package memory implements harness pattern 3 (tiered memory): a conservative,
// cross-session memory facility exposed to the model as two tools (Remember and
// Recall) backed by a pluggable tool.MemoryStore.
//
// This package contains BOTH the file-backed store adapter AND the tool.Tool
// values that drive it. They are kept together deliberately: the tools are thin
// adapters over the store seam (constructor-injected, mirroring NewBashTool),
// and shipping them as one opt-in unit lets the composition root wire memory with
// a single import. The tools depend only on the tool.MemoryStore interface, so a
// fake in-memory store can stand in for tests.
//
// SCOPING: a Store is per-PROJECT. The composition root calls New(dir) once per
// project/workspace directory; entries persist across process restarts for that
// directory and are isolated from other projects. See tool.MemoryStore.
package memory

import (
	"context"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"sync"
	"time"

	"github.com/stacklok/mecatl/internal/tool"
)

// memoryFileName is the JSON file, under the store's directory, that holds all
// entries for one project.
const memoryFileName = "memory.json"

// Store is a file-backed, concurrency-safe tool.MemoryStore. It persists entries
// as a single JSON document at <dir>/memory.json. All writes are serialized by a
// mutex and committed atomically (temp file + rename) so a crash mid-write cannot
// corrupt or truncate the on-disk file. A fresh Store opened over the same dir
// sees previously written entries, giving durability across process restarts.
type Store struct {
	mu   sync.Mutex
	path string
}

// Compile-time assertion that *Store implements tool.MemoryStore.
var _ tool.MemoryStore = (*Store)(nil)

// New constructs a file-backed Store rooted at dir, creating dir (and parents)
// if it does not exist. The store is scoped to dir: it owns <dir>/memory.json.
// Pass a per-project directory so memory is isolated per project.
func New(dir string) (*Store, error) {
	if strings.TrimSpace(dir) == "" {
		return nil, fmt.Errorf("memory: New requires a non-empty dir")
	}
	if err := os.MkdirAll(dir, 0o755); err != nil {
		return nil, fmt.Errorf("memory: create dir %q: %w", dir, err)
	}
	return &Store{path: filepath.Join(dir, memoryFileName)}, nil
}

// persisted is the on-disk JSON shape: a map from key to its record. A map keeps
// the file order-independent; List sorts for deterministic output.
type persisted struct {
	Entries map[string]record `json:"entries"`
}

// record is one stored entry on disk.
type record struct {
	Value     string    `json:"value"`
	UpdatedAt time.Time `json:"updated_at"`
}

// Remember stores value under key, overwriting any existing entry and bumping
// UpdatedAt to now. An empty key is rejected.
func (s *Store) Remember(_ context.Context, key, value string) error {
	if strings.TrimSpace(key) == "" {
		return fmt.Errorf("memory: Remember requires a non-empty key")
	}
	s.mu.Lock()
	defer s.mu.Unlock()

	data, err := s.load()
	if err != nil {
		return err
	}
	data.Entries[key] = record{Value: value, UpdatedAt: time.Now().UTC()}
	return s.save(data)
}

// Recall returns the entry for the exact key. A miss is (zero, false, nil).
func (s *Store) Recall(_ context.Context, key string) (tool.MemoryEntry, bool, error) {
	s.mu.Lock()
	defer s.mu.Unlock()

	data, err := s.load()
	if err != nil {
		return tool.MemoryEntry{}, false, err
	}
	r, ok := data.Entries[key]
	if !ok {
		return tool.MemoryEntry{}, false, nil
	}
	return tool.MemoryEntry{Key: key, Value: r.Value, UpdatedAt: r.UpdatedAt}, true, nil
}

// List returns all entries whose key has the given prefix, sorted by key. An
// empty prefix returns every entry.
func (s *Store) List(_ context.Context, prefix string) ([]tool.MemoryEntry, error) {
	s.mu.Lock()
	defer s.mu.Unlock()

	data, err := s.load()
	if err != nil {
		return nil, err
	}
	out := make([]tool.MemoryEntry, 0, len(data.Entries))
	for k, r := range data.Entries {
		if strings.HasPrefix(k, prefix) {
			out = append(out, tool.MemoryEntry{Key: k, Value: r.Value, UpdatedAt: r.UpdatedAt})
		}
	}
	sort.Slice(out, func(i, j int) bool { return out[i].Key < out[j].Key })
	return out, nil
}

// Forget deletes the entry for key. Deleting a missing key is a no-op (not an
// error).
func (s *Store) Forget(_ context.Context, key string) error {
	s.mu.Lock()
	defer s.mu.Unlock()

	data, err := s.load()
	if err != nil {
		return err
	}
	if _, ok := data.Entries[key]; !ok {
		return nil
	}
	delete(data.Entries, key)
	return s.save(data)
}

// load reads and decodes the on-disk file. A missing file is an empty store, not
// an error. The caller must hold s.mu.
func (s *Store) load() (persisted, error) {
	raw, err := os.ReadFile(s.path)
	if err != nil {
		if os.IsNotExist(err) {
			return persisted{Entries: map[string]record{}}, nil
		}
		return persisted{}, fmt.Errorf("memory: read %q: %w", s.path, err)
	}
	var data persisted
	if err := json.Unmarshal(raw, &data); err != nil {
		return persisted{}, fmt.Errorf("memory: parse %q: %w", s.path, err)
	}
	if data.Entries == nil {
		data.Entries = map[string]record{}
	}
	return data, nil
}

// save atomically writes data: it encodes to a temp file in the same directory,
// fsyncs it, then renames over the target so a reader never sees a partial file.
// The caller must hold s.mu.
func (s *Store) save(data persisted) error {
	raw, err := json.MarshalIndent(data, "", "  ")
	if err != nil {
		return fmt.Errorf("memory: encode: %w", err)
	}
	dir := filepath.Dir(s.path)
	tmp, err := os.CreateTemp(dir, ".memory-*.json.tmp")
	if err != nil {
		return fmt.Errorf("memory: create temp: %w", err)
	}
	tmpName := tmp.Name()
	// Best-effort cleanup if we bail before the rename succeeds.
	defer func() { _ = os.Remove(tmpName) }()

	if _, err := tmp.Write(raw); err != nil {
		_ = tmp.Close()
		return fmt.Errorf("memory: write temp: %w", err)
	}
	if err := tmp.Sync(); err != nil {
		_ = tmp.Close()
		return fmt.Errorf("memory: sync temp: %w", err)
	}
	if err := tmp.Close(); err != nil {
		return fmt.Errorf("memory: close temp: %w", err)
	}
	if err := os.Rename(tmpName, s.path); err != nil {
		return fmt.Errorf("memory: rename temp into place: %w", err)
	}
	return nil
}
