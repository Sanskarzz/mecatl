package memory

import (
	"context"
	"encoding/json"
	"sort"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/stacklok/mecatl/internal/session"
	"github.com/stacklok/mecatl/internal/tool"
)

// fakeStore is an in-memory tool.MemoryStore for tool tests — no filesystem.
type fakeStore struct {
	mu sync.Mutex
	m  map[string]tool.MemoryEntry
}

func newFakeStore() *fakeStore { return &fakeStore{m: map[string]tool.MemoryEntry{}} }

func (f *fakeStore) RememberEntry(_ context.Context, e tool.MemoryEntry) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	e.UpdatedAt = time.Now()
	f.m[e.Key] = e
	return nil
}

func (f *fakeStore) Remember(ctx context.Context, key, value string) error {
	return f.RememberEntry(ctx, tool.MemoryEntry{Key: key, Value: value})
}

func (f *fakeStore) Index(_ context.Context) ([]tool.MemoryEntry, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	out := make([]tool.MemoryEntry, 0, len(f.m))
	for k, e := range f.m {
		desc := e.Description
		if desc == "" {
			desc = e.Value
		}
		out = append(out, tool.MemoryEntry{Key: k, Description: desc, UpdatedAt: e.UpdatedAt})
	}
	sort.Slice(out, func(i, j int) bool { return out[i].Key < out[j].Key })
	return out, nil
}

func (f *fakeStore) Recall(_ context.Context, key string) (tool.MemoryEntry, bool, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	e, ok := f.m[key]
	return e, ok, nil
}

func (f *fakeStore) List(_ context.Context, prefix string) ([]tool.MemoryEntry, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	var out []tool.MemoryEntry
	for k, e := range f.m {
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
	delete(f.m, key)
	return nil
}

var _ tool.MemoryStore = (*fakeStore)(nil)

// call builds a ToolCall with JSON args marshalled from m.
func call(t *testing.T, name string, m map[string]any) session.ToolCall {
	t.Helper()
	raw, err := json.Marshal(m)
	if err != nil {
		t.Fatalf("marshal args: %v", err)
	}
	return session.NewToolCall(session.ToolCallID("id-"+name), name, raw)
}

// exec runs a tool and fails on a harness-level error.
func exec(t *testing.T, tl tool.Tool, in session.ToolCall) session.ToolResult {
	t.Helper()
	res, err := tl.Execute(context.Background(), in, nil)
	if err != nil {
		t.Fatalf("%s: unexpected harness error: %v", tl.Spec().Name, err)
	}
	return res
}

func TestRememberToolWrites(t *testing.T) {
	fs := newFakeStore()
	res := exec(t, NewRememberTool(fs), call(t, "Remember", map[string]any{
		"key": "pref/test-runner", "value": "gotestsum",
	}))
	if res.IsError {
		t.Fatalf("Remember errored: %s", res.Content)
	}
	got, ok, _ := fs.Recall(context.Background(), "pref/test-runner")
	if !ok || got.Value != "gotestsum" {
		t.Errorf("store after Remember: ok=%v value=%q", ok, got.Value)
	}
}

func TestRecallToolReturnsValue(t *testing.T) {
	fs := newFakeStore()
	_ = fs.Remember(context.Background(), "pref/editor", "vim")
	res := exec(t, NewRecallTool(fs), call(t, "Recall", map[string]any{"key": "pref/editor"}))
	if res.IsError {
		t.Fatalf("Recall errored: %s", res.Content)
	}
	if !strings.Contains(res.Content, "vim") {
		t.Errorf("Recall content = %q, want it to contain the value", res.Content)
	}
}

func TestRecallToolMissingKeyIsNotError(t *testing.T) {
	fs := newFakeStore()
	res := exec(t, NewRecallTool(fs), call(t, "Recall", map[string]any{"key": "nope"}))
	if res.IsError {
		t.Errorf("Recall of missing key must NOT be an error result: %s", res.Content)
	}
	if !strings.Contains(strings.ToLower(res.Content), "no memory found") {
		t.Errorf("expected a clear 'not found' result, got %q", res.Content)
	}
}

func TestRecallToolPrefixLists(t *testing.T) {
	fs := newFakeStore()
	ctx := context.Background()
	_ = fs.Remember(ctx, "pref/a", "1")
	_ = fs.Remember(ctx, "pref/b", "2")
	_ = fs.Remember(ctx, "other/x", "9")
	res := exec(t, NewRecallTool(fs), call(t, "Recall", map[string]any{"key": "pref/"}))
	if res.IsError {
		t.Fatalf("Recall prefix errored: %s", res.Content)
	}
	if !strings.Contains(res.Content, "pref/a") || !strings.Contains(res.Content, "pref/b") {
		t.Errorf("prefix recall missing entries: %q", res.Content)
	}
	if strings.Contains(res.Content, "other/x") {
		t.Errorf("prefix recall leaked non-matching entry: %q", res.Content)
	}
}

func TestRememberAcceptsDescriptionAndEchoesIndexLine(t *testing.T) {
	fs := newFakeStore()
	res := exec(t, NewRememberTool(fs), call(t, "Remember", map[string]any{
		"key": "pref/test-runner", "value": "gotestsum --format dots", "description": "preferred test runner",
	}))
	if res.IsError {
		t.Fatalf("Remember errored: %s", res.Content)
	}
	// The result echoes the exact index line (key — description) the write produced.
	if !strings.Contains(res.Content, "pref/test-runner") || !strings.Contains(res.Content, "preferred test runner") {
		t.Errorf("Remember result should echo the index line, got %q", res.Content)
	}
	// The description was persisted.
	got, _, _ := fs.Recall(context.Background(), "pref/test-runner")
	if got.Description != "preferred test runner" {
		t.Errorf("stored description = %q, want it persisted", got.Description)
	}
}

func TestRememberWithoutDescriptionEchoesValueFirstLine(t *testing.T) {
	fs := newFakeStore()
	res := exec(t, NewRememberTool(fs), call(t, "Remember", map[string]any{
		"key": "k", "value": "first line\nsecond line",
	}))
	if res.IsError {
		t.Fatalf("Remember errored: %s", res.Content)
	}
	// With no explicit description, the echo uses the value's first line.
	if !strings.Contains(res.Content, "first line") || strings.Contains(res.Content, "second line") {
		t.Errorf("Remember echo should use value's first line only, got %q", res.Content)
	}
}

func TestRecallStillFetchesFullValue(t *testing.T) {
	fs := newFakeStore()
	_ = fs.RememberEntry(context.Background(), tool.MemoryEntry{
		Key: "pref/editor", Value: "vim, with a long full body", Description: "short",
	})
	res := exec(t, NewRecallTool(fs), call(t, "Recall", map[string]any{"key": "pref/editor"}))
	if res.IsError {
		t.Fatalf("Recall errored: %s", res.Content)
	}
	// Tier-1 load returns the FULL value, not just the index description.
	if !strings.Contains(res.Content, "vim, with a long full body") {
		t.Errorf("Recall should return the full value, got %q", res.Content)
	}
}

func TestMemoryToolsReadOnlyFlags(t *testing.T) {
	fs := newFakeStore()
	if NewRecallTool(fs).ReadOnly() != true {
		t.Error("Recall.ReadOnly() must be true")
	}
	if NewRememberTool(fs).ReadOnly() != false {
		t.Error("Remember.ReadOnly() must be false")
	}
}

func TestMemoryToolsMalformedArgs(t *testing.T) {
	fs := newFakeStore()
	bad := session.NewToolCall("id", "Remember", json.RawMessage("{not json"))
	res := exec(t, NewRememberTool(fs), bad)
	if !res.IsError {
		t.Error("malformed Remember args should be a tool error")
	}
	res = exec(t, NewRecallTool(fs), session.NewToolCall("id", "Recall", json.RawMessage("{not json")))
	if !res.IsError {
		t.Error("malformed Recall args should be a tool error")
	}
	// Missing required args also error.
	if !exec(t, NewRememberTool(fs), call(t, "Remember", map[string]any{"key": "k"})).IsError {
		t.Error("Remember without value should be a tool error")
	}
	if !exec(t, NewRecallTool(fs), call(t, "Recall", map[string]any{})).IsError {
		t.Error("Recall without key should be a tool error")
	}
}

func TestMemoryToolsRegistration(t *testing.T) {
	fs := newFakeStore()
	if len(Tools(fs)) != 2 {
		t.Fatalf("Tools() = %d, want 2", len(Tools(fs)))
	}
	cat := tool.NewCatalog()
	if err := Register(cat, fs); err != nil {
		t.Fatalf("Register: %v", err)
	}
	for _, name := range []string{"Recall", "Remember"} {
		if _, ok := cat.Lookup(name); !ok {
			t.Errorf("catalog missing %q after Register", name)
		}
	}
}

func TestMemoryToolSpecsHaveDocs(t *testing.T) {
	fs := newFakeStore()
	for _, tl := range Tools(fs) {
		s := tl.Spec()
		if len(s.Description) < 80 {
			t.Errorf("%s: description too short to be onboarding docs", s.Name)
		}
		// The descriptions must steer against the over-eager-memory anti-pattern.
		if !strings.Contains(strings.ToLower(s.Description), "when not to use") {
			t.Errorf("%s: description lacks a 'when NOT to use' section", s.Name)
		}
		var js any
		if err := json.Unmarshal(s.Schema, &js); err != nil {
			t.Errorf("%s: schema invalid JSON: %v", s.Name, err)
		}
	}
}

func TestNewMemoryToolsNilStorePanics(t *testing.T) {
	for _, ctor := range []func(tool.MemoryStore) tool.Tool{NewRememberTool, NewRecallTool} {
		func() {
			defer func() {
				if recover() == nil {
					t.Error("constructor with nil store should panic")
				}
			}()
			_ = ctor(nil)
		}()
	}
}
