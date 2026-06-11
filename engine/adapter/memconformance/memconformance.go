// Package memconformance provides a shared conformance test suite for the
// tool.MemoryStore interface. Adapters (the flock-file reference store, remote
// drivers, ...) call Run with a factory that constructs a fresh store, and the
// suite exercises only the tool.MemoryStore interface.
//
// Importing "testing" in a non-_test.go file is intentional here: this is a
// test-helper package whose sole purpose is to be imported by adapter tests,
// the conventional Go pattern for shared conformance suites (cf. testing/fstest).
//
// The suite pins the CONTRACT, not the implementation: durability across
// reopen, file-locking semantics, and the exact ranking order of Search are
// adapter-internal and deliberately NOT asserted here.
package memconformance

import (
	"context"
	"fmt"
	"sync"
	"testing"

	"github.com/stacklok/mecatl/engine/tool"
)

// Run executes the shared MemoryStore conformance table against the store
// produced by newStore. newStore must return a fresh, isolated store each call.
func Run(t *testing.T, newStore func(t *testing.T) tool.MemoryStore) {
	t.Helper()
	ctx := context.Background()

	t.Run("remember-recall round trip", func(t *testing.T) {
		st := newStore(t)
		want := tool.MemoryEntry{Key: "pref/test-runner", Value: "use task test\nsecond line", Description: "how to run tests"}
		if err := st.RememberEntry(ctx, want); err != nil {
			t.Fatalf("RememberEntry: %v", err)
		}
		got, found, err := st.Recall(ctx, want.Key)
		if err != nil {
			t.Fatalf("Recall: %v", err)
		}
		if !found {
			t.Fatalf("Recall(%q) found = false, want true", want.Key)
		}
		if got.Key != want.Key {
			t.Errorf("Key = %q want %q", got.Key, want.Key)
		}
		if got.Value != want.Value {
			t.Errorf("Value = %q want %q", got.Value, want.Value)
		}
		if got.UpdatedAt.IsZero() {
			t.Errorf("UpdatedAt is zero, want a non-zero write time")
		}
	})

	t.Run("overwrite bumps updated-at", func(t *testing.T) {
		st := newStore(t)
		if err := st.RememberEntry(ctx, tool.MemoryEntry{Key: "k", Value: "v1"}); err != nil {
			t.Fatalf("RememberEntry #1: %v", err)
		}
		first, found, err := st.Recall(ctx, "k")
		if err != nil || !found {
			t.Fatalf("Recall #1: found=%v err=%v", found, err)
		}
		if err := st.RememberEntry(ctx, tool.MemoryEntry{Key: "k", Value: "v2"}); err != nil {
			t.Fatalf("RememberEntry #2: %v", err)
		}
		second, found, err := st.Recall(ctx, "k")
		if err != nil || !found {
			t.Fatalf("Recall #2: found=%v err=%v", found, err)
		}
		if second.Value != "v2" {
			t.Errorf("Value after overwrite = %q want %q", second.Value, "v2")
		}
		if second.UpdatedAt.Before(first.UpdatedAt) {
			t.Errorf("UpdatedAt decreased on overwrite: %v -> %v", first.UpdatedAt, second.UpdatedAt)
		}
	})

	t.Run("empty key rejected", func(t *testing.T) {
		st := newStore(t)
		for _, key := range []string{"", "   ", "\t\n"} {
			if err := st.RememberEntry(ctx, tool.MemoryEntry{Key: key, Value: "v"}); err == nil {
				t.Errorf("RememberEntry(key=%q) = nil error, want rejection", key)
			}
		}
	})

	t.Run("recall miss", func(t *testing.T) {
		st := newStore(t)
		got, found, err := st.Recall(ctx, "no/such/key")
		if err != nil {
			t.Fatalf("Recall miss: err = %v, want nil", err)
		}
		if found {
			t.Errorf("Recall miss: found = true, want false")
		}
		if got != (tool.MemoryEntry{}) {
			t.Errorf("Recall miss: entry = %+v, want zero value", got)
		}
	})

	t.Run("list", func(t *testing.T) {
		st := newStore(t)
		for _, e := range []tool.MemoryEntry{
			{Key: "pref/b", Value: "B"},
			{Key: "pref/a", Value: "A"},
			{Key: "fact/x", Value: "X"},
		} {
			if err := st.RememberEntry(ctx, e); err != nil {
				t.Fatalf("RememberEntry(%q): %v", e.Key, err)
			}
		}
		prefixed, err := st.List(ctx, "pref/")
		if err != nil {
			t.Fatalf("List(pref/): %v", err)
		}
		if got, want := keysOf(prefixed), []string{"pref/a", "pref/b"}; !equalKeys(got, want) {
			t.Errorf("List(pref/) keys = %v want %v (prefix-filtered, key-sorted)", got, want)
		}
		// Unlike Index/Search, List returns FULL entries — Value included.
		// Consumers (dream's consolidation planner, Recall's prefix fallback)
		// load payloads from List, so a value-stripping driver must FAIL here.
		for _, e := range prefixed {
			if e.Value == "" {
				t.Errorf("List(pref/) entry %q has empty Value, want the stored value present", e.Key)
			}
		}
		all, err := st.List(ctx, "")
		if err != nil {
			t.Fatalf("List(\"\"): %v", err)
		}
		if got, want := keysOf(all), []string{"fact/x", "pref/a", "pref/b"}; !equalKeys(got, want) {
			t.Errorf("List(\"\") keys = %v want %v (every entry, key-sorted)", got, want)
		}
		for _, e := range all {
			if e.Value == "" {
				t.Errorf("List(\"\") entry %q has empty Value, want the stored value present", e.Key)
			}
		}
		none, err := st.List(ctx, "zzz/")
		if err != nil {
			t.Fatalf("List(zzz/): %v", err)
		}
		if len(none) != 0 {
			t.Errorf("List(zzz/) = %v, want empty", keysOf(none))
		}
	})

	t.Run("forget", func(t *testing.T) {
		st := newStore(t)
		if err := st.RememberEntry(ctx, tool.MemoryEntry{Key: "k", Value: "v"}); err != nil {
			t.Fatalf("RememberEntry: %v", err)
		}
		if err := st.Forget(ctx, "k"); err != nil {
			t.Fatalf("Forget: %v", err)
		}
		if _, found, err := st.Recall(ctx, "k"); err != nil || found {
			t.Errorf("Recall after Forget: found=%v err=%v, want miss", found, err)
		}
		if err := st.Forget(ctx, "never/existed"); err != nil {
			t.Errorf("Forget(missing key) = %v, want nil", err)
		}
	})

	t.Run("index", func(t *testing.T) {
		st := newStore(t)
		for _, e := range []tool.MemoryEntry{
			{Key: "b/explicit", Value: "the long value body", Description: "an explicit hook"},
			{Key: "a/derived", Value: "\n\n  first real line  \nrest of the value"},
		} {
			if err := st.RememberEntry(ctx, e); err != nil {
				t.Fatalf("RememberEntry(%q): %v", e.Key, err)
			}
		}
		idx, err := st.Index(ctx)
		if err != nil {
			t.Fatalf("Index: %v", err)
		}
		if got, want := keysOf(idx), []string{"a/derived", "b/explicit"}; !equalKeys(got, want) {
			t.Fatalf("Index keys = %v want %v (key-sorted)", got, want)
		}
		for _, e := range idx {
			if e.Value != "" {
				t.Errorf("Index entry %q carries Value %q, want omitted", e.Key, e.Value)
			}
			if e.UpdatedAt.IsZero() {
				t.Errorf("Index entry %q has zero UpdatedAt", e.Key)
			}
		}
		if got := idx[0].Description; got != "first real line" {
			t.Errorf("derived Description = %q want %q (value's first non-empty line)", got, "first real line")
		}
		if got := idx[1].Description; got != "an explicit hook" {
			t.Errorf("explicit Description = %q want %q", got, "an explicit hook")
		}
	})

	t.Run("search", func(t *testing.T) {
		st := newStore(t)
		// Four "preference" entries are deliberately TIED (same shape, same term
		// frequency) so the determinism check below cannot pass on luck — a
		// nondeterministic ordering of 4 tied entries across 5 calls is overwhelmingly
		// likely to disagree at least once.
		for _, e := range []tool.MemoryEntry{
			{Key: "fact/capital", Value: "the capital of zanzibar is zanzibar city"},
			{Key: "pref/editor", Value: "the editor preference is helix"},
			{Key: "pref/pager", Value: "the pager preference is less"},
			{Key: "pref/shell", Value: "the shell preference is fish"},
			{Key: "pref/terminal", Value: "the terminal preference is foot"},
		} {
			if err := st.RememberEntry(ctx, e); err != nil {
				t.Fatalf("RememberEntry(%q): %v", e.Key, err)
			}
		}

		for _, query := range []string{"", "   "} {
			empty, err := st.Search(ctx, query, 5)
			if err != nil {
				t.Fatalf("Search(query=%q): err = %v, want nil", query, err)
			}
			if len(empty) != 0 {
				t.Errorf("Search(query=%q) = %v, want empty", query, keysOf(empty))
			}
		}

		hit, err := st.Search(ctx, "zanzibar", 5)
		if err != nil {
			t.Fatalf("Search(zanzibar): %v", err)
		}
		if len(hit) != 1 || hit[0].Key != "fact/capital" {
			t.Fatalf("Search(zanzibar) keys = %v, want exactly [fact/capital]", keysOf(hit))
		}
		for _, e := range hit {
			if e.Value != "" {
				t.Errorf("Search result %q carries Value %q, want omitted", e.Key, e.Value)
			}
		}

		capped, err := st.Search(ctx, "preference", 1)
		if err != nil {
			t.Fatalf("Search(preference, 1): %v", err)
		}
		if len(capped) != 1 {
			t.Errorf("Search(preference, 1) returned %d entries, want k=1 cap", len(capped))
		}

		// k <= 0 selects the implementation's default page size — it must NOT
		// mean "no results" (or the model's default-k searches go blind).
		defaulted, err := st.Search(ctx, "preference", 0)
		if err != nil {
			t.Fatalf("Search(preference, 0): %v", err)
		}
		if len(defaulted) == 0 {
			t.Errorf("Search(preference, 0) = empty, want non-empty (k<=0 selects the default page size)")
		}

		first, err := st.Search(ctx, "preference", 5)
		if err != nil {
			t.Fatalf("Search(preference) #1: %v", err)
		}
		for i := 2; i <= 5; i++ {
			again, err := st.Search(ctx, "preference", 5)
			if err != nil {
				t.Fatalf("Search(preference) #%d: %v", i, err)
			}
			if !equalKeys(keysOf(first), keysOf(again)) {
				t.Fatalf("Search not deterministic on tied entries: call #1 %v vs call #%d %v", keysOf(first), i, keysOf(again))
			}
		}
	})

	t.Run("concurrent writes", func(t *testing.T) {
		st := newStore(t)
		const n = 8
		var wg sync.WaitGroup
		errs := make([]error, n)
		for i := range n {
			wg.Add(1)
			go func() {
				defer wg.Done()
				errs[i] = st.RememberEntry(ctx, tool.MemoryEntry{
					Key:   fmt.Sprintf("conc/%02d", i),
					Value: fmt.Sprintf("value %d", i),
				})
			}()
		}
		wg.Wait()
		for i, err := range errs {
			if err != nil {
				t.Fatalf("concurrent RememberEntry #%d: %v", i, err)
			}
		}
		got, err := st.List(ctx, "conc/")
		if err != nil {
			t.Fatalf("List(conc/): %v", err)
		}
		if len(got) != n {
			t.Fatalf("List(conc/) saw %d entries, want %d: %v", len(got), n, keysOf(got))
		}
	})
}

// keysOf projects entries onto their keys, preserving order.
func keysOf(entries []tool.MemoryEntry) []string {
	keys := make([]string, len(entries))
	for i, e := range entries {
		keys[i] = e.Key
	}
	return keys
}

// equalKeys reports whether two key slices are element-wise equal.
func equalKeys(a, b []string) bool {
	if len(a) != len(b) {
		return false
	}
	for i := range a {
		if a[i] != b[i] {
			return false
		}
	}
	return true
}
