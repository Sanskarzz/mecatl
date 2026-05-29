package memory

import (
	"context"
	"fmt"
	"sync"
	"testing"
	"time"
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
