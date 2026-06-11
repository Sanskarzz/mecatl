package memconformance

import (
	"context"
	"fmt"
	"sort"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/stacklok/mecatl/engine/tool"
)

// TestConformanceSelfTest proves the suite itself is satisfiable: a minimal
// map-backed fake passes every subtest. If the suite ever over-pins an
// implementation detail (flock semantics, BM25 order, ...), this self-test
// breaks first, before any real adapter does.
func TestConformanceSelfTest(t *testing.T) {
	Run(t, func(*testing.T) tool.MemoryStore {
		return &fakeStore{entries: map[string]tool.MemoryEntry{}}
	})
}

// fakeStore is the simplest possible tool.MemoryStore: a mutex-guarded map
// with naive substring search. It exists only to self-test the suite.
type fakeStore struct {
	mu      sync.Mutex
	entries map[string]tool.MemoryEntry
}

func (f *fakeStore) RememberEntry(_ context.Context, e tool.MemoryEntry) error {
	if strings.TrimSpace(e.Key) == "" {
		return fmt.Errorf("fake: empty key")
	}
	f.mu.Lock()
	defer f.mu.Unlock()
	e.Description = strings.TrimSpace(e.Description)
	e.UpdatedAt = time.Now().UTC()
	f.entries[e.Key] = e
	return nil
}

func (f *fakeStore) Recall(_ context.Context, key string) (tool.MemoryEntry, bool, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	e, ok := f.entries[key]
	return e, ok, nil
}

func (f *fakeStore) List(_ context.Context, prefix string) ([]tool.MemoryEntry, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	var out []tool.MemoryEntry
	for k, e := range f.entries {
		if strings.HasPrefix(k, prefix) {
			out = append(out, e)
		}
	}
	sort.Slice(out, func(i, j int) bool { return out[i].Key < out[j].Key })
	return out, nil
}

func (f *fakeStore) Forget(_ context.Context, key string) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	delete(f.entries, key)
	return nil
}

func (f *fakeStore) Index(_ context.Context) ([]tool.MemoryEntry, error) {
	all, _ := f.List(context.Background(), "")
	for i := range all {
		all[i].Description = describe(all[i])
		all[i].Value = ""
	}
	return all, nil
}

func (f *fakeStore) Search(_ context.Context, query string, k int) ([]tool.MemoryEntry, error) {
	if strings.TrimSpace(query) == "" {
		return nil, nil
	}
	if k <= 0 {
		k = 10
	}
	all, _ := f.List(context.Background(), "")
	var out []tool.MemoryEntry
	for _, e := range all {
		hay := strings.ToLower(e.Key + " " + describe(e) + " " + e.Value)
		for _, term := range strings.Fields(strings.ToLower(query)) {
			if strings.Contains(hay, term) {
				e.Description = describe(e)
				e.Value = ""
				out = append(out, e)
				break
			}
		}
	}
	if len(out) > k {
		out = out[:k]
	}
	return out, nil
}

// describe mirrors the contract's tier-0 rule: explicit description, else the
// value's first non-empty line.
func describe(e tool.MemoryEntry) string {
	if d := strings.TrimSpace(e.Description); d != "" {
		return d
	}
	for _, line := range strings.Split(e.Value, "\n") {
		if t := strings.TrimSpace(line); t != "" {
			return t
		}
	}
	return ""
}
