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

	"github.com/gofrs/flock"

	"github.com/stacklok/mecatl/engine/tool"
)

const (
	// memoryFileName is the JSON file, under the store's directory, that holds
	// all entries for one project.
	memoryFileName = "memory.json"
	// lockFileName is a STABLE sentinel co-located with the data file, used only
	// for cross-process advisory locking (flock). It is deliberately NOT the data
	// file itself: every save renames a temp file over memory.json, which would
	// break a flock held against the old inode. The sentinel is never renamed, so
	// the flock association is stable for the store's lifetime.
	lockFileName = "memory.lock"
	// lockRetryDelay is how often TryLock(Context)/TryRLock(Context) re-probes a
	// contended lock while waiting. Small enough to feel instant under light
	// contention.
	lockRetryDelay = 5 * time.Millisecond
	// lockTimeout bounds how long any single read-modify-write waits for the
	// cross-process lock before giving up with a clear error, so a stuck or
	// crashed holder cannot deadlock a run indefinitely.
	lockTimeout = 5 * time.Second
)

// Store is a file-backed, cross-process-safe tool.MemoryStore. It persists
// entries as a single JSON document at <dir>/memory.json and commits every write
// atomically (temp file + rename) so a crash mid-write cannot corrupt or truncate
// the on-disk file. A fresh Store opened over the same dir sees previously written
// entries, giving durability across process restarts.
//
// Concurrency / locking. The store is safe for both in-process and cross-process
// concurrent use, and — critically — does not LOSE updates under either:
//
//   - In-process: s.mu serialises every method on a single Store, and is also
//     held across the full read-modify-write of a mutation. This is the only lock
//     that protects the flock handle, which is NOT goroutine-safe when shared
//     across goroutines on one fd.
//   - Cross-process (several mecated/mecatui instances, agent-team / subagent runs
//     sharing one memory.json): a gofrs/flock advisory lock on the STABLE sentinel
//     <dir>/memory.lock guards the read-modify-write. Writes (RememberEntry,
//     Remember, Forget) take an EXCLUSIVE lock; reads (Recall, List, Index) take a
//     SHARED lock. The lock spans the whole load→mutate→save sequence, so two
//     processes can no longer interleave read-modify-write and clobber each other
//     (the lost-update bug the bare temp+rename did not prevent).
//
// Acquisition order is always s.mu THEN flock; the flock is acquired with a
// bounded TryLockContext/TryRLockContext (lockRetryDelay / lockTimeout, also
// honouring ctx cancellation) so a stuck holder fails loud instead of deadlocking,
// and released (Unlock) before return on every path including errors.
//
// INVARIANT — at most ONE *Store per directory per process. gofrs/flock uses BSD
// flock(2), which contends across file descriptors even WITHIN a single process.
// Two *Store values opened over the SAME dir in the SAME process therefore hold
// DISTINCT fds on the sentinel and would self-deadlock: the second writer's
// exclusive acquire blocks on the first's lock until the lockTimeout expires (a
// loud error, but a needless one). The composition root upholds this today by
// constructing one shared *Store per project (see internal/app). If per-session
// memory is ever needed, SHARE a single *Store keyed by absolute dir (the way the
// session-engine map is keyed) rather than calling New per session — do NOT open a
// second *Store over a dir already owned in-process.
type Store struct {
	mu   sync.Mutex
	path string
	lock *flock.Flock
}

// Compile-time assertion that *Store implements tool.MemoryStore.
var _ tool.MemoryStore = (*Store)(nil)

// New constructs a file-backed Store rooted at dir, creating dir (and parents)
// if it does not exist. The store is scoped to dir: it owns <dir>/memory.json and
// the cross-process lock sentinel <dir>/memory.lock. Pass a per-project directory
// so memory is isolated per project.
//
// Construct AT MOST ONE *Store per dir per process. Because the cross-process lock
// uses BSD flock(2) (per-fd, cross-fd-contending even in one process), a SECOND
// *Store opened over the same dir in this process would self-deadlock its own lock
// acquisition until the lockTimeout, since the two *Store values hold separate fds
// on the sentinel. Share one *Store keyed by absolute dir instead. See the Store
// type doc for the full rationale.
func New(dir string) (*Store, error) {
	if strings.TrimSpace(dir) == "" {
		return nil, fmt.Errorf("memory: New requires a non-empty dir")
	}
	// 0o700: memory can hold sensitive curated facts; the per-project store has
	// no reason to be group/other-readable.
	if err := os.MkdirAll(dir, 0o700); err != nil {
		return nil, fmt.Errorf("memory: create dir %q: %w", dir, err)
	}
	return &Store{
		path: filepath.Join(dir, memoryFileName),
		lock: flock.New(filepath.Join(dir, lockFileName)),
	}, nil
}

// persisted is the on-disk JSON shape: a map from key to its record. A map keeps
// the file order-independent; List sorts for deterministic output.
type persisted struct {
	Entries map[string]record `json:"entries"`
}

// record is one stored entry on disk. Description is additive: Task-1 files have
// no "description" key, and omitempty + Go's zero-value decode means they load
// cleanly with Description == "" (the index then derives one from the value). So
// migration from the flat Task-1 format is a no-op read — no version bump, no
// rewrite pass.
type record struct {
	Value       string    `json:"value"`
	Description string    `json:"description,omitempty"`
	UpdatedAt   time.Time `json:"updated_at"`
}

// withExclusiveLock acquires s.mu THEN the cross-process EXCLUSIVE flock, runs fn
// against the loaded store, and saves only if fn returns no error. The flock is
// released before return on every path. ctx bounds the lock wait.
func (s *Store) withExclusiveLock(ctx context.Context, fn func(data *persisted) error) error {
	s.mu.Lock()
	defer s.mu.Unlock()

	locked, err := s.lock.TryLockContext(ctx, lockRetryDelay)
	if err != nil {
		return fmt.Errorf("memory: acquire write lock %q: %w", s.lock.Path(), err)
	}
	if !locked {
		return fmt.Errorf("memory: could not acquire write lock %q within %s (held by another process?)", s.lock.Path(), lockBudget(ctx))
	}
	defer func() { _ = s.lock.Unlock() }()

	data, err := s.load()
	if err != nil {
		return err
	}
	if err := fn(&data); err != nil {
		return err
	}
	return s.save(data)
}

// withSharedLock acquires s.mu THEN the cross-process SHARED flock and runs fn
// against the loaded store. The flock is released before return on every path.
// ctx bounds the lock wait.
func (s *Store) withSharedLock(ctx context.Context, fn func(data persisted) error) error {
	s.mu.Lock()
	defer s.mu.Unlock()

	locked, err := s.lock.TryRLockContext(ctx, lockRetryDelay)
	if err != nil {
		return fmt.Errorf("memory: acquire read lock %q: %w", s.lock.Path(), err)
	}
	if !locked {
		return fmt.Errorf("memory: could not acquire read lock %q within %s (held by another process?)", s.lock.Path(), lockBudget(ctx))
	}
	defer func() { _ = s.lock.Unlock() }()

	data, err := s.load()
	if err != nil {
		return err
	}
	return fn(data)
}

// lockCtx derives a context with the lock timeout from the caller's ctx, so the
// flock wait is bounded even when the caller passes context.Background(). If the
// caller's ctx already has a shorter deadline, that shorter deadline wins (the
// timeout is a CEILING, not a floor).
func lockCtx(ctx context.Context) (context.Context, context.CancelFunc) {
	return context.WithTimeout(ctx, lockTimeout)
}

// lockBudget reports the effective wait budget remaining on ctx, for use in the
// timeout error message so it reflects the ACTUAL deadline (which may be shorter
// than lockTimeout if the caller passed a tighter ctx) rather than hardcoding the
// ceiling. It returns the rounded time until ctx's deadline, falling back to the
// lockTimeout ceiling when ctx carries no deadline.
func lockBudget(ctx context.Context) time.Duration {
	dl, ok := ctx.Deadline()
	if !ok {
		return lockTimeout
	}
	return time.Until(dl).Round(time.Millisecond)
}

// RememberEntry stores e, overwriting any existing entry under e.Key and bumping
// UpdatedAt to now. e.Description is stored as-is (empty is allowed; the index
// derives one). An empty key is rejected.
func (s *Store) RememberEntry(ctx context.Context, e tool.MemoryEntry) error {
	if strings.TrimSpace(e.Key) == "" {
		return fmt.Errorf("memory: RememberEntry requires a non-empty key")
	}
	lctx, cancel := lockCtx(ctx)
	defer cancel()
	return s.withExclusiveLock(lctx, func(data *persisted) error {
		data.Entries[e.Key] = record{
			Value:       e.Value,
			Description: strings.TrimSpace(e.Description),
			UpdatedAt:   time.Now().UTC(),
		}
		return nil
	})
}

// Remember stores value under key with no explicit description, overwriting any
// existing entry and bumping UpdatedAt to now. An empty key is rejected. It is a
// convenience wrapper over RememberEntry.
func (s *Store) Remember(ctx context.Context, key, value string) error {
	return s.RememberEntry(ctx, tool.MemoryEntry{Key: key, Value: value})
}

// Recall returns the entry for the exact key. A miss is (zero, false, nil).
func (s *Store) Recall(ctx context.Context, key string) (tool.MemoryEntry, bool, error) {
	lctx, cancel := lockCtx(ctx)
	defer cancel()
	var (
		out   tool.MemoryEntry
		found bool
	)
	err := s.withSharedLock(lctx, func(data persisted) error {
		r, ok := data.Entries[key]
		if !ok {
			return nil
		}
		out = tool.MemoryEntry{Key: key, Value: r.Value, Description: r.Description, UpdatedAt: r.UpdatedAt}
		found = true
		return nil
	})
	if err != nil {
		return tool.MemoryEntry{}, false, err
	}
	return out, found, nil
}

// List returns all entries whose key has the given prefix, sorted by key. An
// empty prefix returns every entry.
func (s *Store) List(ctx context.Context, prefix string) ([]tool.MemoryEntry, error) {
	lctx, cancel := lockCtx(ctx)
	defer cancel()
	var out []tool.MemoryEntry
	err := s.withSharedLock(lctx, func(data persisted) error {
		out = make([]tool.MemoryEntry, 0, len(data.Entries))
		for k, r := range data.Entries {
			if strings.HasPrefix(k, prefix) {
				out = append(out, tool.MemoryEntry{Key: k, Value: r.Value, Description: r.Description, UpdatedAt: r.UpdatedAt})
			}
		}
		sort.Slice(out, func(i, j int) bool { return out[i].Key < out[j].Key })
		return nil
	})
	if err != nil {
		return nil, err
	}
	return out, nil
}

// Index returns the tier-0 routing table: every entry with the VALUE OMITTED and
// Description filled (explicit, else derived from the value's first line), sorted
// by key. It applies NO size cap — the consumer (the prompt assembler) caps and
// renders. It is the cheap, always-in-context summary view.
func (s *Store) Index(ctx context.Context) ([]tool.MemoryEntry, error) {
	lctx, cancel := lockCtx(ctx)
	defer cancel()
	var out []tool.MemoryEntry
	err := s.withSharedLock(lctx, func(data persisted) error {
		out = make([]tool.MemoryEntry, 0, len(data.Entries))
		for k, r := range data.Entries {
			out = append(out, tool.MemoryEntry{
				Key:         k,
				Description: deriveDescription(r),
				UpdatedAt:   r.UpdatedAt,
			})
		}
		sort.Slice(out, func(i, j int) bool { return out[i].Key < out[j].Key })
		return nil
	})
	if err != nil {
		return nil, err
	}
	return out, nil
}

// Search ranks entries by BM25 lexical relevance to query (over each entry's
// key + derived description + value) and returns the top k best-first, with the
// VALUE OMITTED and Description filled — mirroring Index's result shape. Ranking
// runs INSIDE the shared lock, over the freshly loaded data, so it sees a
// consistent snapshot and never races a concurrent write. An empty or
// whitespace-only query short-circuits to an empty result (no lock, no error);
// k <= 0 uses the default page size. See bm25Rank for the scoring/ordering rules.
func (s *Store) Search(ctx context.Context, query string, k int) ([]tool.MemoryEntry, error) {
	if strings.TrimSpace(query) == "" {
		return nil, nil
	}
	lctx, cancel := lockCtx(ctx)
	defer cancel()
	var out []tool.MemoryEntry
	err := s.withSharedLock(lctx, func(data persisted) error {
		entries := make([]tool.MemoryEntry, 0, len(data.Entries))
		for key, r := range data.Entries {
			entries = append(entries, tool.MemoryEntry{
				Key:         key,
				Value:       r.Value,
				Description: deriveDescription(r),
				UpdatedAt:   r.UpdatedAt,
			})
		}
		out = bm25Rank(entries, query, k)
		return nil
	})
	if err != nil {
		return nil, err
	}
	// Defence in depth: bm25Rank already omits values, but guarantee no value
	// ever escapes Search regardless of future ranking changes.
	for i := range out {
		out[i].Value = ""
	}
	return out, nil
}

// deriveDescription returns r's tier-0 one-line description, delegating to the
// shared derivation: explicit Description if set, else the value's first non-empty
// line.
func deriveDescription(r record) string {
	return descriptionOrFirstLine(r.Description, r.Value)
}

// descriptionOrFirstLine is the single source of truth for the tier-0 one-liner
// derivation rule, shared by the store's Index (deriveDescription) and the
// Remember tool's index-line echo (tools.go). It returns the explicit description
// (trimmed) if non-empty, else the value's first non-empty line. It never returns
// more than one line, so the index stays one line per entry.
func descriptionOrFirstLine(description, value string) string {
	if d := strings.TrimSpace(description); d != "" {
		return d
	}
	for _, line := range strings.Split(value, "\n") {
		if t := strings.TrimSpace(line); t != "" {
			return t
		}
	}
	return ""
}

// Forget deletes the entry for key. Deleting a missing key is a no-op (not an
// error).
func (s *Store) Forget(ctx context.Context, key string) error {
	lctx, cancel := lockCtx(ctx)
	defer cancel()
	return s.withExclusiveLock(lctx, func(data *persisted) error {
		delete(data.Entries, key)
		return nil
	})
}

// load reads and decodes the on-disk file. A missing file is an empty store, not
// an error. The caller must hold s.mu AND the (shared or exclusive) flock.
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
// The caller must hold s.mu AND the exclusive flock.
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
