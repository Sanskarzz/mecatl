package permconfig

import (
	"context"
	"errors"
	"sync"
	"testing"

	"github.com/stacklok/mecatl/internal/adapter/memfs"
	"github.com/stacklok/mecatl/internal/adapter/xdgconfig"
	"github.com/stacklok/mecatl/internal/governance"
	"github.com/stacklok/mecatl/internal/tool"
)

// countingWS wraps a memfs Workspace to count Read calls, so a test can assert the
// resolver reads project files ONCE per root (then serves the revalidated cache).
type countingWS struct {
	*memfs.Workspace
	mu    sync.Mutex
	reads int
}

func (c *countingWS) Read(ctx context.Context, p string) ([]byte, error) {
	c.mu.Lock()
	c.reads++
	c.mu.Unlock()
	return c.Workspace.Read(ctx, p)
}

func (c *countingWS) readCount() int {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.reads
}

// seed writes a file into the underlying workspace (the resolver reads through it).
func (c *countingWS) seed(t *testing.T, p, content string) {
	t.Helper()
	if err := c.Write(context.Background(), p, []byte(content)); err != nil {
		t.Fatalf("seed %s: %v", p, err)
	}
}

func newProjectWS(t *testing.T, root string, settings string) *countingWS {
	t.Helper()
	ws := &countingWS{Workspace: memfs.NewWorkspace(root)}
	if settings != "" {
		ws.seed(t, projectFileMecatl, settings)
	}
	return ws
}

// fakeEnv is an injectable environment with no user-global files and no home.
func fakeEnv() xdgconfig.ResolveEnv {
	return xdgconfig.ResolveEnv{
		Getenv:      func(string) string { return "" },
		UserHomeDir: func() (string, error) { return "", errors.New("no home") },
		ReadFile:    func(string) ([]byte, error) { return nil, errors.New("not found") },
	}
}

const trustedAllowYAML = `
permissions:
  allow:
    - "Bash(go test:*)"
  deny:
    - "Bash(rm:*)"
`

// A trusted project's allow + deny both resolve; the cache reads the file once per
// root and serves subsequent calls from memory.
func TestResolveTrustedProjectAndCache(t *testing.T) {
	r := newWithEnv(Options{Conventional: true, TrustProject: true}, fakeEnv())
	if r == nil {
		t.Fatal("resolver should be non-nil when Conventional is set")
	}
	ws := newProjectWS(t, "/repo", trustedAllowYAML)

	rules := r.Resolve(context.Background(), ws)
	if findRule(rules, "Bash", "go test*") == nil {
		t.Fatalf("trusted project allow should resolve: %+v", rules)
	}
	if findRule(rules, "Bash", "rm*") == nil {
		t.Fatalf("project deny should resolve: %+v", rules)
	}
	readsAfterFirst := ws.readCount()
	if readsAfterFirst == 0 {
		t.Fatal("expected the resolver to read the project file at least once")
	}
	// Second Resolve for the SAME root hits the cache: revalidation is Stat-only
	// (not counted), so no further Reads.
	_ = r.Resolve(context.Background(), ws)
	if ws.readCount() != readsAfterFirst {
		t.Fatalf("cache miss: reads grew from %d to %d on a repeated unchanged Resolve", readsAfterFirst, ws.readCount())
	}
}

// An UNTRUSTED project drops its ALLOW rules but keeps deny/ask.
func TestResolveUntrustedProjectDropsAllowKeepsDeny(t *testing.T) {
	r := newWithEnv(Options{Conventional: true, TrustProject: false}, fakeEnv())
	ws := newProjectWS(t, "/repo", trustedAllowYAML)

	rules := r.Resolve(context.Background(), ws)
	if findRule(rules, "Bash", "go test*") != nil {
		t.Fatalf("untrusted project allow must be DROPPED: %+v", rules)
	}
	if findRule(rules, "Bash", "rm*") == nil {
		t.Fatalf("project deny must be kept even when untrusted: %+v", rules)
	}
}

// Two different roots resolve independently (different files → different rules).
func TestResolvePerRootIndependent(t *testing.T) {
	r := newWithEnv(Options{Conventional: true, TrustProject: true}, fakeEnv())
	wsA := newProjectWS(t, "/a", "permissions:\n  allow:\n    - \"Bash(go test:*)\"\n")
	wsB := newProjectWS(t, "/b", "permissions:\n  deny:\n    - \"Bash(go test:*)\"\n")

	ra := r.Resolve(context.Background(), wsA)
	rb := r.Resolve(context.Background(), wsB)
	if got := findRule(ra, "Bash", "go test*"); got == nil || got.Effect != governance.Allow {
		t.Fatalf("/a should resolve an allow, got %+v", ra)
	}
	if got := findRule(rb, "Bash", "go test*"); got == nil || got.Effect != governance.Deny {
		t.Fatalf("/b should resolve a deny, got %+v", rb)
	}
}

// Conventional OFF (and no explicit files) → New returns a nil resolver: nothing
// is discovered.
func TestResolveConventionalOffYieldsNilResolver(t *testing.T) {
	if r := newWithEnv(Options{Conventional: false}, fakeEnv()); r != nil {
		t.Fatalf("expected a nil resolver when nothing is configured, got %#v", r)
	}
}

// A nil workspace yields only the user-global rules (no project to read).
func TestResolveNilWorkspace(t *testing.T) {
	env := fakeEnv()
	env.ReadFile = func(_ string) ([]byte, error) {
		return []byte("permissions:\n  deny:\n    - \"Bash(curl:*)\"\n"), nil
	}
	r := newWithEnv(Options{ExplicitFiles: []string{"/etc/mecatl/perms.yaml"}}, env)
	if r == nil {
		t.Fatal("explicit files should produce a non-nil resolver")
	}
	rules := r.Resolve(context.Background(), nil)
	if got := findRule(rules, "Bash", "curl*"); got == nil || got.Scope != governance.ScopeCLI {
		t.Fatalf("explicit-file rule should resolve at ScopeCLI even with a nil ws: %+v", rules)
	}
}

// Scope assignment per tier (issue #13): explicit (--permission-config) → ScopeCLI;
// .mecatl/settings.local.yaml → ScopeLocalProject; .mecatl/settings.yaml →
// ScopeSharedProject; user-global → ScopeUser.
func TestResolveScopeAssignmentPerTier(t *testing.T) {
	env := fakeEnv()
	env.ReadFile = func(_ string) ([]byte, error) {
		return []byte("permissions:\n  deny:\n    - \"Bash(curl:*)\"\n"), nil
	}
	r := newWithEnv(Options{
		Conventional:  true,
		TrustProject:  true,
		ExplicitFiles: []string{"/etc/mecatl/perms.yaml"},
	}, env)
	ws := newProjectWS(t, "/repo", "permissions:\n  deny:\n    - \"Bash(rm:*)\"\n")
	ws.seed(t, projectFileMecatlLocal, "permissions:\n  deny:\n    - \"Bash(sudo:*)\"\n")

	rules := r.Resolve(context.Background(), ws)
	if got := findRule(rules, "Bash", "rm*"); got == nil || got.Scope != governance.ScopeSharedProject {
		t.Fatalf("shared project rule should be ScopeSharedProject: %+v", rules)
	}
	if got := findRule(rules, "Bash", "sudo*"); got == nil || got.Scope != governance.ScopeLocalProject {
		t.Fatalf("local project rule should be ScopeLocalProject: %+v", rules)
	}
	if got := findRule(rules, "Bash", "curl*"); got == nil || got.Scope != governance.ScopeCLI {
		t.Fatalf("explicit (CLI) rule should be ScopeCLI: %+v", rules)
	}
}

// Cache REVALIDATION (issue #13 fix #4): a deny added to the config mid-process
// takes effect on the NEXT Resolve — the per-root cache is invalidated by the
// file's mtime/size change, not held until restart.
func TestResolveCacheRevalidatesOnEdit(t *testing.T) {
	r := newWithEnv(Options{Conventional: true, TrustProject: true}, fakeEnv())
	ws := newProjectWS(t, "/repo", "permissions:\n  allow:\n    - \"Bash(go test:*)\"\n")

	first := r.Resolve(context.Background(), ws)
	if findRule(first, "Bash", "rm*") != nil {
		t.Fatalf("rm deny should not exist yet: %+v", first)
	}
	// Edit the config: add a deny. memfs stamps a fresh modTime + a new size on
	// Write, so the cached entry's fingerprint no longer matches.
	ws.seed(t, projectFileMecatl, "permissions:\n  allow:\n    - \"Bash(go test:*)\"\n  deny:\n    - \"Bash(rm:*)\"\n")

	second := r.Resolve(context.Background(), ws)
	if findRule(second, "Bash", "rm*") == nil {
		t.Fatalf("the newly-added deny must take effect on the next Resolve (cache went stale): %+v", second)
	}
}

// Cache MISS on a DISTINCT root: a second root must trigger its own Read (a bug
// that collapsed the cache key to a constant would skip this and fail).
func TestResolveCacheMissOnDistinctRoot(t *testing.T) {
	r := newWithEnv(Options{Conventional: true, TrustProject: true}, fakeEnv())
	wsA := newProjectWS(t, "/a", "permissions:\n  deny:\n    - \"Bash(a:*)\"\n")
	wsB := newProjectWS(t, "/b", "permissions:\n  deny:\n    - \"Bash(b:*)\"\n")

	_ = r.Resolve(context.Background(), wsA)
	if wsB.readCount() != 0 {
		t.Fatalf("ws-b should not have been read yet, got %d", wsB.readCount())
	}
	_ = r.Resolve(context.Background(), wsB)
	if wsB.readCount() == 0 {
		t.Fatal("ws-b should have been read on its first (distinct-root) Resolve — cache key collapsed?")
	}
}

// Concurrent Resolve across goroutines (and two roots) so `go test -race`
// exercises the RWMutex around the per-root cache. A dropped write-lock would be
// caught by the race detector here.
func TestResolveConcurrent(t *testing.T) {
	r := newWithEnv(Options{Conventional: true, TrustProject: true}, fakeEnv())
	wsA := newProjectWS(t, "/a", "permissions:\n  deny:\n    - \"Bash(a:*)\"\n")
	wsB := newProjectWS(t, "/b", "permissions:\n  deny:\n    - \"Bash(b:*)\"\n")

	var wg sync.WaitGroup
	for i := 0; i < 64; i++ {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			ws := wsA
			want := "a*"
			if i%2 == 0 {
				ws, want = wsB, "b*"
			}
			rules := r.Resolve(context.Background(), ws)
			if findRule(rules, "Bash", want) == nil {
				t.Errorf("concurrent Resolve missing rule %q: %+v", want, rules)
			}
		}(i)
	}
	wg.Wait()
}

// Partial-malformed fail-soft (issue #13 QA): a BAD shared .mecatl/settings.yaml
// must NOT suppress a GOOD .claude/settings.json under the same root — each file
// is parsed independently and a bad one is skipped (warned), not fatal.
func TestResolvePartialMalformedFailSoft(t *testing.T) {
	r := newWithEnv(Options{Conventional: true, ImportClaude: true, TrustProject: true}, fakeEnv())
	ws := newProjectWS(t, "/repo", "permissions: [this is: not: valid")
	ws.seed(t, projectFileClaude, `{"permissions":{"deny":["Bash(rm:*)"]}}`)

	rules := r.Resolve(context.Background(), ws)
	if findRule(rules, "Bash", "rm*") == nil {
		t.Fatalf("the good Claude file must still load despite the bad YAML sibling: %+v", rules)
	}
}

// Compile-time: a *countingWS is a tool.Workspace (so the resolver accepts it).
var _ tool.Workspace = (*countingWS)(nil)
