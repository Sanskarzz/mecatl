package memory

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
	"reflect"
	"sync"
	"testing"
	"time"

	"github.com/stacklok/mecatl/internal/tool"
)

func TestStoreRememberRecallRoundTrip(t *testing.T) {
	st, err := New(t.TempDir())
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	ctx := context.Background()
	if err := st.Remember(ctx, "pref/test-runner", "gotestsum"); err != nil {
		t.Fatalf("Remember: %v", err)
	}
	got, ok, err := st.Recall(ctx, "pref/test-runner")
	if err != nil || !ok {
		t.Fatalf("Recall: ok=%v err=%v", ok, err)
	}
	if got.Value != "gotestsum" {
		t.Errorf("value = %q, want %q", got.Value, "gotestsum")
	}
	if got.UpdatedAt.IsZero() {
		t.Error("UpdatedAt not set")
	}
}

func TestStoreRecallMissIsNotError(t *testing.T) {
	st, _ := New(t.TempDir())
	_, ok, err := st.Recall(context.Background(), "nope")
	if err != nil {
		t.Fatalf("Recall miss should not error: %v", err)
	}
	if ok {
		t.Error("Recall of absent key returned ok=true")
	}
}

func TestStoreOverwriteBumpsUpdatedAt(t *testing.T) {
	st, _ := New(t.TempDir())
	ctx := context.Background()
	if err := st.Remember(ctx, "k", "v1"); err != nil {
		t.Fatalf("Remember: %v", err)
	}
	first, _, _ := st.Recall(ctx, "k")
	time.Sleep(2 * time.Millisecond)
	if err := st.Remember(ctx, "k", "v2"); err != nil {
		t.Fatalf("Remember overwrite: %v", err)
	}
	second, _, _ := st.Recall(ctx, "k")
	if second.Value != "v2" {
		t.Errorf("overwrite value = %q, want v2", second.Value)
	}
	if !second.UpdatedAt.After(first.UpdatedAt) {
		t.Errorf("UpdatedAt not bumped: first=%v second=%v", first.UpdatedAt, second.UpdatedAt)
	}
}

func TestStoreListByPrefixSorted(t *testing.T) {
	st, _ := New(t.TempDir())
	ctx := context.Background()
	// Insert out of order across two namespaces.
	for _, kv := range [][2]string{
		{"pref/b", "2"}, {"pref/a", "1"}, {"project/x", "9"}, {"pref/c", "3"},
	} {
		if err := st.Remember(ctx, kv[0], kv[1]); err != nil {
			t.Fatalf("Remember %s: %v", kv[0], err)
		}
	}
	got, err := st.List(ctx, "pref/")
	if err != nil {
		t.Fatalf("List: %v", err)
	}
	wantKeys := []string{"pref/a", "pref/b", "pref/c"}
	if len(got) != len(wantKeys) {
		t.Fatalf("List returned %d entries, want %d", len(got), len(wantKeys))
	}
	for i, w := range wantKeys {
		if got[i].Key != w {
			t.Errorf("entry[%d].Key = %q, want %q (must be sorted)", i, got[i].Key, w)
		}
	}

	// Empty prefix returns everything.
	all, _ := st.List(ctx, "")
	if len(all) != 4 {
		t.Errorf("List(\"\") = %d entries, want 4", len(all))
	}
}

func TestStoreForget(t *testing.T) {
	st, _ := New(t.TempDir())
	ctx := context.Background()
	_ = st.Remember(ctx, "k", "v")
	if err := st.Forget(ctx, "k"); err != nil {
		t.Fatalf("Forget: %v", err)
	}
	if _, ok, _ := st.Recall(ctx, "k"); ok {
		t.Error("entry still present after Forget")
	}
	// Forgetting a missing key is a no-op, not an error.
	if err := st.Forget(ctx, "absent"); err != nil {
		t.Errorf("Forget of missing key errored: %v", err)
	}
}

func TestStorePersistenceAcrossReopen(t *testing.T) {
	dir := t.TempDir()
	ctx := context.Background()
	st1, _ := New(dir)
	if err := st1.Remember(ctx, "pref/editor", "vim"); err != nil {
		t.Fatalf("Remember: %v", err)
	}
	// A fresh Store over the same dir must see the entry (durable across restart).
	st2, err := New(dir)
	if err != nil {
		t.Fatalf("reopen New: %v", err)
	}
	got, ok, err := st2.Recall(ctx, "pref/editor")
	if err != nil || !ok {
		t.Fatalf("reopened Recall: ok=%v err=%v", ok, err)
	}
	if got.Value != "vim" {
		t.Errorf("reopened value = %q, want vim", got.Value)
	}
}

func TestIndexOmitsValuesAndDerivesDescription(t *testing.T) {
	st, _ := New(t.TempDir())
	ctx := context.Background()
	// One entry WITH an explicit description, one WITHOUT (derive from value).
	if err := st.RememberEntry(ctx, tool.MemoryEntry{
		Key: "pref/test-runner", Value: "gotestsum --format dots", Description: "preferred test runner",
	}); err != nil {
		t.Fatalf("RememberEntry: %v", err)
	}
	if err := st.Remember(ctx, "project/deploy-gate", "staging deploy needs manual approval\nsecond line ignored"); err != nil {
		t.Fatalf("Remember: %v", err)
	}

	idx, err := st.Index(ctx)
	if err != nil {
		t.Fatalf("Index: %v", err)
	}
	if len(idx) != 2 {
		t.Fatalf("Index returned %d entries, want 2", len(idx))
	}
	// Sorted by key lexically: "pref/..." < "project/...".
	if idx[0].Key != "pref/test-runner" || idx[1].Key != "project/deploy-gate" {
		t.Fatalf("Index not sorted by key: %q, %q", idx[0].Key, idx[1].Key)
	}
	// Values are OMITTED in the index.
	for _, e := range idx {
		if e.Value != "" {
			t.Errorf("Index entry %q leaked a value: %q", e.Key, e.Value)
		}
	}
	// Explicit description preserved.
	if idx[0].Description != "preferred test runner" {
		t.Errorf("explicit description = %q, want it preserved", idx[0].Description)
	}
	// Derived description = first non-empty line of the value.
	if idx[1].Description != "staging deploy needs manual approval" {
		t.Errorf("derived description = %q, want first line of value", idx[1].Description)
	}
}

func TestRememberEntryRoundTripsDescription(t *testing.T) {
	st, _ := New(t.TempDir())
	ctx := context.Background()
	if err := st.RememberEntry(ctx, tool.MemoryEntry{
		Key: "k", Value: "the full value", Description: "short summary",
	}); err != nil {
		t.Fatalf("RememberEntry: %v", err)
	}
	// Recall returns the FULL value AND the description.
	got, ok, err := st.Recall(ctx, "k")
	if err != nil || !ok {
		t.Fatalf("Recall: ok=%v err=%v", ok, err)
	}
	if got.Value != "the full value" {
		t.Errorf("Recall value = %q, want full value", got.Value)
	}
	if got.Description != "short summary" {
		t.Errorf("Recall description = %q, want it round-tripped", got.Description)
	}
	// Index shows the description, omits the value.
	idx, _ := st.Index(ctx)
	if len(idx) != 1 || idx[0].Description != "short summary" || idx[0].Value != "" {
		t.Errorf("Index = %+v, want one entry with description and no value", idx)
	}
}

func TestMigrationReadsTask1FlatFile(t *testing.T) {
	dir := t.TempDir()
	// A literal Task-1 memory.json: records carry only value + updated_at, NO
	// description key. The additive omitempty schema must read it cleanly.
	flat := `{
  "entries": {
    "pref/editor": {"value": "vim is my editor\nignored", "updated_at": "2024-01-02T03:04:05Z"}
  }
}`
	if err := os.WriteFile(filepath.Join(dir, memoryFileName), []byte(flat), 0o600); err != nil {
		t.Fatalf("seed flat file: %v", err)
	}
	st, err := New(dir)
	if err != nil {
		t.Fatalf("New over flat file: %v", err)
	}
	ctx := context.Background()
	// Recall returns the value (proving the old format decodes).
	got, ok, err := st.Recall(ctx, "pref/editor")
	if err != nil || !ok {
		t.Fatalf("Recall flat entry: ok=%v err=%v", ok, err)
	}
	if got.Value != "vim is my editor\nignored" {
		t.Errorf("flat Recall value = %q", got.Value)
	}
	// Index derives a description from the value's first line.
	idx, err := st.Index(ctx)
	if err != nil {
		t.Fatalf("Index over flat file: %v", err)
	}
	if len(idx) != 1 || idx[0].Description != "vim is my editor" {
		t.Errorf("flat-file index = %+v, want derived first-line description", idx)
	}
}

// TestCrossProcessRememberNoLostUpdates simulates several PROCESSES (distinct
// Store instances over one dir, each with its own flock handle) concurrently
// Remembering distinct keys. The flock-guarded read-modify-write must let every
// write survive — the lost-update bug the bare temp+rename did not prevent.
func TestCrossProcessRememberNoLostUpdates(t *testing.T) {
	dir := t.TempDir()
	ctx := context.Background()

	const n = 40
	var wg sync.WaitGroup
	for i := 0; i < n; i++ {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			// A FRESH Store per goroutine == a distinct process's view (its own
			// flock fd), so this exercises the cross-process lock, not just s.mu.
			st, err := New(dir)
			if err != nil {
				t.Errorf("New: %v", err)
				return
			}
			if err := st.Remember(ctx, fmt.Sprintf("k/%03d", i), fmt.Sprintf("v%d", i)); err != nil {
				t.Errorf("Remember: %v", err)
			}
		}(i)
	}
	wg.Wait()

	st, _ := New(dir)
	all, err := st.List(ctx, "k/")
	if err != nil {
		t.Fatalf("List after concurrent cross-process writes: %v", err)
	}
	if len(all) != n {
		t.Errorf("after %d cross-process writes, got %d entries (lost updates!)", n, len(all))
	}
}

func TestStoreConcurrentRememberNoCorruption(t *testing.T) {
	dir := t.TempDir()
	st, _ := New(dir)
	ctx := context.Background()

	const n = 50
	var wg sync.WaitGroup
	for i := 0; i < n; i++ {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			key := fmt.Sprintf("k/%03d", i)
			if err := st.Remember(ctx, key, fmt.Sprintf("v%d", i)); err != nil {
				t.Errorf("concurrent Remember: %v", err)
			}
		}(i)
	}
	wg.Wait()

	// All entries present and the file is valid JSON (a fresh Store can parse it).
	st2, err := New(dir)
	if err != nil {
		t.Fatalf("reopen: %v", err)
	}
	all, err := st2.List(ctx, "k/")
	if err != nil {
		t.Fatalf("List after concurrent writes (corrupt file?): %v", err)
	}
	if len(all) != n {
		t.Errorf("after %d concurrent writes, got %d entries", n, len(all))
	}
}

func TestStoreSearchRanksAndOmitsValues(t *testing.T) {
	st, err := New(t.TempDir())
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	ctx := context.Background()
	const secret = "SECRET-VALUE-SHOULD-NOT-RENDER"
	if err := st.RememberEntry(ctx, tool.MemoryEntry{
		Key: "pref/test-runner", Value: secret, Description: "preferred test runner",
	}); err != nil {
		t.Fatalf("RememberEntry: %v", err)
	}
	if err := st.RememberEntry(ctx, tool.MemoryEntry{
		Key: "project/deploy-gate", Value: "staging deploy needs manual approval", Description: "deploy gate",
	}); err != nil {
		t.Fatalf("RememberEntry: %v", err)
	}
	if err := st.RememberEntry(ctx, tool.MemoryEntry{
		Key: "pref/editor", Value: "vim", Description: "favourite editor",
	}); err != nil {
		t.Fatalf("RememberEntry: %v", err)
	}

	got, err := st.Search(ctx, "preferred test runner", 10)
	if err != nil {
		t.Fatalf("Search: %v", err)
	}
	if len(got) == 0 {
		t.Fatal("expected at least one search result")
	}
	if got[0].Key != "pref/test-runner" {
		t.Errorf("top result = %q, want pref/test-runner", got[0].Key)
	}
	for _, e := range got {
		if e.Value != "" {
			t.Errorf("Search result %q leaked a value: %q", e.Key, e.Value)
		}
	}
}

// TestStoreSearchDeterministicAcrossRuns pins the map-order→stable-output
// contract. Several entries score EQUALLY for the query (each value is the bare
// query term, with keys that differ only in their namespace), so the only thing
// that makes the result order total is bm25Rank's (score desc, key asc)
// tie-break. Because the store iterates a Go map (randomised order) to build its
// entry slice, a missing tie-break would surface here as a flaky order. We run
// the search many times and assert the returned keys are byte-identical every
// iteration AND in ascending-key order.
func TestStoreSearchDeterministicAcrossRuns(t *testing.T) {
	st, err := New(t.TempDir())
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	ctx := context.Background()
	for _, key := range []string{"a/x", "b/x", "c/x"} {
		if err := st.RememberEntry(ctx, tool.MemoryEntry{Key: key, Value: "topic"}); err != nil {
			t.Fatalf("RememberEntry %q: %v", key, err)
		}
	}

	wantKeys := []string{"a/x", "b/x", "c/x"} // ascending-key tie-break order
	const runs = 20
	for i := 0; i < runs; i++ {
		got, err := st.Search(ctx, "topic", 10)
		if err != nil {
			t.Fatalf("Search run %d: %v", i, err)
		}
		gotKeys := keysOf(got)
		if !reflect.DeepEqual(gotKeys, wantKeys) {
			t.Fatalf("run %d: keys = %v, want %v (deterministic ascending-key order)", i, gotKeys, wantKeys)
		}
	}
}

func TestStoreSearchEmptyQuery(t *testing.T) {
	st, err := New(t.TempDir())
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	got, err := st.Search(context.Background(), "  ", 10)
	if err != nil {
		t.Fatalf("Search empty query should not error: %v", err)
	}
	if len(got) != 0 {
		t.Errorf("empty query should return no results, got %d", len(got))
	}
}
