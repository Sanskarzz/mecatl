package app

import (
	"context"
	"iter"
	"sort"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/stacklok/mecatl/internal/adapter/agents"
	"github.com/stacklok/mecatl/internal/adapter/memfs"
	"github.com/stacklok/mecatl/internal/adapter/mockllm"
	"github.com/stacklok/mecatl/internal/adapter/permpolicy"
	"github.com/stacklok/mecatl/internal/agent"
	"github.com/stacklok/mecatl/internal/governance"
	"github.com/stacklok/mecatl/internal/port"
	"github.com/stacklok/mecatl/internal/session"
	"github.com/stacklok/mecatl/internal/tool"
)

// recordingProvider wraps a scripted provider and records the Model on the last
// LLMRequest it received, so a test can assert the resolved per-def model that an
// Engine carries (Deps.Model flows onto every request). This is the critique's
// preferred seam (m5): assert via a recorded LLMRequest.Model, not a test
// accessor on the unexported Engine.deps.
type recordingProvider struct {
	inner *mockllm.Provider
	mu    sync.Mutex
	model string
}

func (*recordingProvider) Capabilities() port.ProviderCapabilities {
	return port.ProviderCapabilities{}
}

func (p *recordingProvider) Stream(ctx context.Context, req port.LLMRequest) (iter.Seq2[port.Chunk, error], error) {
	p.mu.Lock()
	p.model = req.Model
	p.mu.Unlock()
	return p.inner.Stream(ctx, req)
}

func (p *recordingProvider) lastModel() string {
	p.mu.Lock()
	defer p.mu.Unlock()
	return p.model
}

// --- scopedToolNames ---------------------------------------------------------

func TestScopedToolNamesAllowlistIntersection(t *testing.T) {
	base := baseTaskTools(Config{}) // no shell => no Bash
	def := agents.AgentDef{
		Name:  "reviewer",
		Tools: []string{"Read", "Grep", "Bogus", "Task", "Fork", "ToolSearch"},
	}
	names, diags := scopedToolNames(def, base)
	sort.Strings(names)
	if strings.Join(names, ",") != "Grep,Read" {
		t.Fatalf("kept = %v, want [Grep Read] (allowlist ∩ available, RO only)", names)
	}
	// Distinct diagnostics: Bogus=unknown, Task/Fork/ToolSearch=excluded-at-callsite.
	var unknown, excluded int
	for _, d := range diags {
		switch {
		case d.tool == "Bogus" && strings.Contains(d.reason, "unknown tool"):
			unknown++
		case (d.tool == "Task" || d.tool == "Fork" || d.tool == "ToolSearch") && strings.Contains(d.reason, "excluded at this call site"):
			excluded++
		default:
			t.Fatalf("unexpected diag %+v", d)
		}
	}
	if unknown != 1 || excluded != 3 {
		t.Fatalf("diag kinds: unknown=%d excluded=%d (want 1, 3): %+v", unknown, excluded, diags)
	}
}

func TestScopedToolNamesDisallowedSubtraction(t *testing.T) {
	base := baseTaskTools(Config{})
	def := agents.AgentDef{Name: "x", Tools: []string{"Read", "Grep"}, DisallowedTools: []string{"Grep"}}
	names, _ := scopedToolNames(def, base)
	if strings.Join(names, ",") != "Read" {
		t.Fatalf("disallowed not subtracted: %v", names)
	}
}

func TestScopedToolNamesDropsMutatingForTask(t *testing.T) {
	// Bash is available (shell configured) but mutating: a Task def listing it (and
	// Edit/Write) must have them dropped with the read-only diagnostic, so
	// Task.ReadOnly() stays honestly true.
	cfg := Config{Shell: "/bin/sh", Workspace: t.TempDir()}
	base := baseTaskTools(cfg)
	if _, ok := base["Bash"]; !ok {
		t.Fatalf("expected Bash in base when a shell is configured")
	}
	def := agents.AgentDef{Name: "writer", Tools: []string{"Read", "Edit", "Write", "Bash"}}
	names, diags := scopedToolNames(def, base)
	if strings.Join(names, ",") != "Read" {
		t.Fatalf("mutating tools not dropped: %v", names)
	}
	dropped := map[string]bool{}
	for _, d := range diags {
		if strings.Contains(d.reason, "this call site is read-only") {
			dropped[d.tool] = true
		}
	}
	for _, want := range []string{"Edit", "Write", "Bash"} {
		if !dropped[want] {
			t.Fatalf("want %q dropped as mutating, diags=%+v", want, diags)
		}
	}
}

// TestScopedToolNamesModeAllowShell asserts the three allowMutating/allowShell
// combinations a team member can be scoped under:
//   - (false, true)  — a worktree-isolated read-only member: Bash survives,
//     Edit/Write are still dropped.
//   - (false, false) — a base-sharing read-only member (or a Task child): all three
//     mutating tools are dropped.
//   - (true, _)      — a mutating member: all three survive.
func TestScopedToolNamesModeAllowShell(t *testing.T) {
	cfg := Config{Shell: "/bin/sh", Workspace: t.TempDir()}
	base := baseTaskTools(cfg)
	if _, ok := base["Bash"]; !ok {
		t.Fatalf("expected Bash in base when a shell is configured")
	}
	def := agents.AgentDef{Name: "m", Tools: []string{"Read", "Edit", "Write", "Bash"}}

	// (false, true): worktree read-only member keeps Bash, drops Edit/Write.
	names, _ := scopedToolNamesMode(def, base, false, true)
	sort.Strings(names)
	if strings.Join(names, ",") != "Bash,Read" {
		t.Fatalf("(allowMutating=false, allowShell=true) kept = %v, want [Bash Read]", names)
	}

	// (false, false): base-sharing read-only member drops Bash too.
	names, _ = scopedToolNamesMode(def, base, false, false)
	if strings.Join(names, ",") != "Read" {
		t.Fatalf("(false,false) kept = %v, want [Read] (all mutating dropped)", names)
	}

	// (true, _): mutating member keeps everything.
	names, _ = scopedToolNamesMode(def, base, true, false)
	sort.Strings(names)
	if strings.Join(names, ",") != "Bash,Edit,Read,Write" {
		t.Fatalf("(true,_) kept = %v, want [Bash Edit Read Write]", names)
	}
}

func TestScopedToolNamesDefaultSetIsReadOnly(t *testing.T) {
	// No Tools allowlist => default to the available base, but still read-only-only.
	cfg := Config{Shell: "/bin/sh", Workspace: t.TempDir()}
	base := baseTaskTools(cfg)
	def := agents.AgentDef{Name: "x"}
	names, _ := scopedToolNames(def, base)
	sort.Strings(names)
	// Read-only core tools: Glob, Grep, Read, WebFetch. Edit/Write/Bash dropped.
	if strings.Join(names, ",") != "Glob,Grep,Read,WebFetch" {
		t.Fatalf("default set should be the read-only core tools, got %v", names)
	}
}

// --- resolveModel ------------------------------------------------------------

func TestResolveModelPrecedence(t *testing.T) {
	cfg := Config{Model: "parent-model", SubagentModel: "global-sub", ModelAliases: map[string]string{"fast": "cheap-id"}}

	cases := []struct {
		name string
		def  agents.AgentDef
		want string
	}{
		{"pinned full id wins", agents.AgentDef{Model: "pinned-id"}, "pinned-id"},
		{"pinned alias resolves", agents.AgentDef{Model: "fast"}, "cheap-id"},
		{"empty falls to global override", agents.AgentDef{}, "global-sub"},
		{"inherit falls to global override", agents.AgentDef{Model: "inherit"}, "global-sub"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if got := resolveModel(cfg, tc.def); got != tc.want {
				t.Fatalf("resolveModel = %q, want %q", got, tc.want)
			}
		})
	}
}

func TestResolveModelNoGlobalInheritsParent(t *testing.T) {
	cfg := Config{Model: "parent-model"} // no SubagentModel
	if got := resolveModel(cfg, agents.AgentDef{}); got != "parent-model" {
		t.Fatalf("empty def with no override should inherit parent, got %q", got)
	}
}

func TestResolveModelBuiltinAliasInherits(t *testing.T) {
	// A built-in CC alias with no operator override resolves to inherit (parent).
	cfg := Config{Model: "parent-model"}
	if got := resolveModel(cfg, agents.AgentDef{Model: "sonnet"}); got != "parent-model" {
		t.Fatalf("built-in alias 'sonnet' should inherit parent, got %q", got)
	}
}

func TestResolveModelUnknownAliasWarnsInherits(t *testing.T) {
	cfg := Config{Model: "parent-model"}
	// A bare unknown token (no separator) is treated as a mistyped alias => inherit.
	if got := resolveModel(cfg, agents.AgentDef{Name: "x", Model: "bogusalias"}); got != "parent-model" {
		t.Fatalf("unknown alias should warn+inherit, got %q", got)
	}
	// A separator-bearing token looks like a literal id and is kept verbatim.
	if got := resolveModel(cfg, agents.AgentDef{Model: "vendor-model-1.2"}); got != "vendor-model-1.2" {
		t.Fatalf("literal-looking id should be kept, got %q", got)
	}
}

// --- buildAgentTaskEngines + end-to-end model on the request -----------------

// --- agentSnapshot (ListAgents projection) -----------------------------------

func TestAgentSnapshotEmptyRegistry(t *testing.T) {
	if got := agentSnapshot(Config{}, agents.NewRegistry(nil)); got != nil {
		t.Fatalf("empty registry must yield a nil snapshot, got %+v", got)
	}
	if got := agentSnapshot(Config{}, nil); got != nil {
		t.Fatalf("nil registry must yield a nil snapshot, got %+v", got)
	}
}

func TestAgentSnapshotProjectsResolvedFields(t *testing.T) {
	cfg := Config{Model: "parent-model", ModelAliases: map[string]string{"fast": "cheap-id"}}
	reg := agents.NewRegistry([]agents.AgentDef{
		{
			Name:           "speedy",
			Description:    "fast one",
			Model:          "fast",                                   // alias => resolves to cheap-id
			Tools:          []string{"Read", "Grep", "Edit", "Task"}, // Edit mutating, Task excluded
			PermissionMode: "plan",
			Color:          "green",
		},
		{Name: "plain", Description: "no model"}, // model empty => inherit ("parent-model")
	})

	snap := agentSnapshot(cfg, reg)
	if len(snap) != 2 {
		t.Fatalf("snapshot len = %d, want 2", len(snap))
	}
	// Registry order is name-sorted: "plain" < "speedy".
	plain, speedy := snap[0], snap[1]
	if plain.GetName() != "plain" || speedy.GetName() != "speedy" {
		t.Fatalf("snapshot order = %q,%q want plain,speedy", plain.GetName(), speedy.GetName())
	}
	if speedy.GetModel() != "cheap-id" {
		t.Fatalf("speedy model = %q, want resolved alias cheap-id", speedy.GetModel())
	}
	if speedy.GetPermissionMode() != "plan" || speedy.GetColor() != "green" {
		t.Fatalf("speedy mode/color = %q/%q", speedy.GetPermissionMode(), speedy.GetColor())
	}
	// Effective read-only Task scope: Edit (mutating) and Task (excluded) dropped.
	if strings.Join(speedy.GetTools(), ",") != "Grep,Read" {
		t.Fatalf("speedy tools = %v, want [Grep Read] (read-only scope)", speedy.GetTools())
	}
	if plain.GetModel() != "parent-model" {
		t.Fatalf("plain model = %q, want inherited parent-model", plain.GetModel())
	}
}

func TestBuildAgentTaskEnginesEmptyRegistry(t *testing.T) {
	engines, meta, closeFn := agentTaskEnginesForTest(context.Background(), Config{}, mockllm.New(), agents.NewRegistry(nil), nil, nil, nil, nil)
	if engines != nil || meta != nil || closeFn != nil {
		t.Fatalf("empty registry must yield nil engines/meta/close, got engines=%v meta=%v close!=nil=%v", engines, meta, closeFn != nil)
	}
}

// TestDefLimitsPerFieldFallback proves defLimits maps a def's maxTurns/maxToolCalls
// into session.Limits, with each ZERO def field inheriting the fallback's field.
func TestDefLimitsPerFieldFallback(t *testing.T) {
	fallback := session.Limits{MaxTurns: 12, MaxToolCalls: 40, MaxConsecutiveFailures: 3}

	// No def limits => the fallback unchanged.
	if got := defLimits(agents.AgentDef{}, fallback); got != fallback {
		t.Fatalf("no def limits = %+v, want the fallback %+v", got, fallback)
	}
	// Only maxTurns set => MaxTurns overridden, the rest inherited.
	if got := defLimits(agents.AgentDef{MaxTurns: 2}, fallback); got != (session.Limits{MaxTurns: 2, MaxToolCalls: 40, MaxConsecutiveFailures: 3}) {
		t.Fatalf("maxTurns-only = %+v, want MaxTurns=2 with the rest inherited", got)
	}
	// Both set => both overridden, MaxConsecutiveFailures still inherited.
	if got := defLimits(agents.AgentDef{MaxTurns: 5, MaxToolCalls: 7}, fallback); got != (session.Limits{MaxTurns: 5, MaxToolCalls: 7, MaxConsecutiveFailures: 3}) {
		t.Fatalf("both = %+v, want MaxTurns=5 MaxToolCalls=7 failures inherited", got)
	}
}

// TestBuildAgentTaskEnginesCarriesPerDefLimits proves a def's maxTurns/maxToolCalls
// flow onto AgentMeta.Limits (per-field over the Task default child limits), and a
// def with no limits carries the default unchanged.
func TestBuildAgentTaskEnginesCarriesPerDefLimits(t *testing.T) {
	cfg := Config{Model: "parent-model"}
	reg := agents.NewRegistry([]agents.AgentDef{
		{Name: "bounded", Description: "b", MaxTurns: 2, MaxToolCalls: 9},
		{Name: "plain", Description: "p"},
	})
	_, meta, _ := agentTaskEnginesForTest(context.Background(), cfg, mockllm.New(), reg, nil, nil, nil, nil)

	byName := map[string]agent.AgentMeta{}
	for _, m := range meta {
		byName[m.Name] = m
	}
	def := agent.DefaultChildLimits()
	wantBounded := session.Limits{MaxTurns: 2, MaxToolCalls: 9, MaxConsecutiveFailures: def.MaxConsecutiveFailures}
	if got := byName["bounded"].Limits; got != wantBounded {
		t.Fatalf("bounded meta limits = %+v, want %+v", got, wantBounded)
	}
	if got := byName["plain"].Limits; got != def {
		t.Fatalf("plain meta limits = %+v, want the default child limits %+v", got, def)
	}
}

// TestBaseTaskToolsRepoMapGate proves RepoMap is in the def-scoping base ONLY when
// EnableRepoMap is set (FIX B): with it on a def can allowlist RepoMap; with it off
// the name is unknown to the base.
func TestBaseTaskToolsRepoMapGate(t *testing.T) {
	if _, ok := baseTaskTools(Config{})["RepoMap"]; ok {
		t.Fatal("RepoMap must NOT be in the base when EnableRepoMap is off")
	}
	base := baseTaskTools(Config{EnableRepoMap: true})
	rm, ok := base["RepoMap"]
	if !ok {
		t.Fatal("RepoMap must be in the base when EnableRepoMap is on")
	}
	if !rm.ReadOnly() {
		t.Fatal("RepoMap must report read-only so it survives read-only Task/member scoping")
	}
}

// TestScopedToolNamesRepoMapAllowlist proves a def can allowlist RepoMap when
// EnableRepoMap is on (it survives read-only Task scoping), and that listing it
// WITHOUT EnableRepoMap yields the DISTINCT "unknown tool" diagnostic.
func TestScopedToolNamesRepoMapAllowlist(t *testing.T) {
	def := agents.AgentDef{Name: "mapper", Description: "m", Tools: []string{"RepoMap"}}

	// Enabled: RepoMap is kept (read-only Task scope), no diagnostic.
	names, diags := scopedToolNames(def, baseTaskTools(Config{EnableRepoMap: true}))
	if len(names) != 1 || names[0] != "RepoMap" {
		t.Fatalf("with EnableRepoMap a def allowlisting RepoMap should keep it, got names=%v diags=%v", names, diags)
	}

	// Disabled: RepoMap is unknown to the base => dropped with an "unknown tool" diag.
	names2, diags2 := scopedToolNames(def, baseTaskTools(Config{}))
	if len(names2) != 0 {
		t.Fatalf("without EnableRepoMap RepoMap must be dropped, got %v", names2)
	}
	if len(diags2) != 1 || diags2[0].tool != "RepoMap" || !strings.Contains(diags2[0].reason, "unknown tool") {
		t.Fatalf("without EnableRepoMap RepoMap should yield an 'unknown tool' diagnostic, got %+v", diags2)
	}
}

// TestBuildAgentTaskEnginesResolvedModelOnRequest builds per-def engines, runs one
// via the Task tool, and asserts the recorded LLMRequest.Model equals the
// resolved per-def model (def.Model alias > parent).
func TestBuildAgentTaskEnginesResolvedModelOnRequest(t *testing.T) {
	rec := &recordingProvider{inner: mockllm.New(mockllm.TextTurn("done"))}
	cfg := Config{Model: "parent-model", ModelAliases: map[string]string{"fast": "cheap-id"}}
	reg := agents.NewRegistry([]agents.AgentDef{
		{Name: "speedy", Description: "fast one", Model: "fast", Body: "Be quick."},
	})

	engines, meta, _ := agentTaskEnginesForTest(context.Background(), cfg, rec, reg, nil, nil, nil, nil)
	if len(engines) != 1 || len(meta) != 1 || meta[0].Name != "speedy" {
		t.Fatalf("want 1 engine+meta for 'speedy', got engines=%d meta=%+v", len(engines), meta)
	}

	// Run the named engine via Task and assert the recorded request model.
	defaultEngine := agent.NewEngine(agent.Deps{LLM: mockllm.New(), Catalog: tool.NewCatalog(), Model: "parent-model"})
	task := agent.NewTaskTool(defaultEngine, agent.WithAgentEngines(engines, meta))

	parentCat := tool.NewCatalog()
	parentCat.MustRegister(task)
	parent := mockllm.New(
		mockllm.ToolCallTurn(session.NewToolCall("p1", "Task", []byte(`{"prompt":"go","agent":"speedy"}`))),
		mockllm.TextTurn("parent done"),
	)
	e := agent.NewEngine(agent.Deps{
		LLM:     parent,
		Catalog: parentCat,
		Policy:  permpolicy.NewPolicy([]governance.Rule{{Effect: governance.Allow}}, nil),
		Model:   "parent-model",
	})
	r := e.Run(context.Background(),
		session.New("s1", session.ModeDefault, "/ws", session.Limits{}, time.Unix(0, 0)),
		memfs.NewWorkspace("/ws"), "go")
	for range r.Events() {
	}
	if got := rec.lastModel(); got != "cheap-id" {
		t.Fatalf("recorded request model = %q, want the resolved per-def alias 'cheap-id'", got)
	}
}
