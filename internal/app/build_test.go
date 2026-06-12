package app

import (
	"testing"

	"github.com/stacklok/mecatl/engine/adapter/memstore"
	"github.com/stacklok/mecatl/engine/adapter/mockllm"
	"github.com/stacklok/mecatl/engine/adapter/permpolicy"
	"github.com/stacklok/mecatl/engine/adapter/wallclock"
	"github.com/stacklok/mecatl/engine/agent"
	"github.com/stacklok/mecatl/engine/port"
	"github.com/stacklok/mecatl/engine/prompt"
	"github.com/stacklok/mecatl/engine/tool"
	"github.com/stacklok/mecatl/internal/adapter/mcp"
)

// depsTestFixture builds the shared (non-provider) collaborators the two
// Deps-constructing paths consume, so a test can compare baseEngineDeps against
// engineDepsForProvider on equal footing. All offline (mockllm/memstore).
func depsTestFixture(t *testing.T) (
	provider port.LLMProvider,
	store port.SessionStore,
	policy port.PermissionPolicy,
	hooks port.HookRunner,
	mcpProvider mcp.Provider,
	instructions prompt.InstructionAssembler,
) {
	t.Helper()
	provider = mockllm.New(mockllm.TextTurn("ok"))
	store = memstore.New()
	policy = permpolicy.NewPolicy(defaultRules(), nil)
	return provider, store, policy, nil, nil, prompt.RootAssembler{}
}

// TestBaseEngineDepsDelegatesToProviderSeam: baseEngineDeps and
// engineDepsForProvider with the SAME provider+model produce IDENTICAL
// provider-closing fields. They are the same enumeration point; if they drift, a
// per-session engine (S3) could silently bind the wrong provider/model. (This is the
// drift guard the [High] review and the no-per-session-engine memory call for,
// extended to the provider axis.)
//
// The TokenCounter is now derived INSIDE engineDepsForProvider (panel finding #1), so
// the two paths build their own counters: for the SAME model they are semantically
// equivalent (same encoding ⇒ same counts), which the tiktoken assertion below
// verifies by behaviour rather than by pointer identity.
func TestBaseEngineDepsDelegatesToProviderSeam(t *testing.T) {
	cfg := Config{Model: "gpt-5", Compaction: "cascade", Tokenizer: "tiktoken", Workspace: "/repo"}
	provider, store, policy, hooks, mcpP, instr := depsTestFixture(t)

	base := baseEngineDeps(cfg, provider, store, policy, hooks, mcpP, instr)
	direct := engineDepsForProvider(cfg, provider, cfg.Model, 0, store, policy, hooks, mcpP, instr)

	if base.Model != direct.Model {
		t.Errorf("Model: base=%q direct=%q", base.Model, direct.Model)
	}
	if base.LLM != direct.LLM {
		t.Error("LLM provider differs between baseEngineDeps and engineDepsForProvider")
	}
	if base.PromptConfig.Env.Model != direct.PromptConfig.Env.Model {
		t.Errorf("PromptConfig.Env.Model: base=%q direct=%q",
			base.PromptConfig.Env.Model, direct.PromptConfig.Env.Model)
	}
	if base.PromptConfig.Role != direct.PromptConfig.Role {
		t.Error("PromptConfig.Role (agency delta) differs")
	}
	if base.ContextWindowTokens != direct.ContextWindowTokens {
		t.Errorf("ContextWindowTokens: base=%d direct=%d", base.ContextWindowTokens, direct.ContextWindowTokens)
	}
	if base.CompactionRatio != direct.CompactionRatio {
		t.Errorf("CompactionRatio: base=%v direct=%v", base.CompactionRatio, direct.CompactionRatio)
	}
	// The default path's counter must be semantically identical to the seam's: same
	// model ⇒ same encoding ⇒ identical counts on a probe string.
	const probe = "tokenization differences 12345 café 日本語"
	if a, b := base.TokenCounter.Count(probe), direct.TokenCounter.Count(probe); a != b {
		t.Errorf("TokenCounter differs for the default model: base=%d direct=%d", a, b)
	}
	bc, ok := base.Compactor.(agent.CascadeCompactor)
	if !ok {
		t.Fatalf("base Compactor type = %T, want CascadeCompactor", base.Compactor)
	}
	dc, ok := direct.Compactor.(agent.CascadeCompactor)
	if !ok {
		t.Fatalf("direct Compactor type = %T, want CascadeCompactor", direct.Compactor)
	}
	if bc.Model != dc.Model {
		t.Errorf("Compactor.Model: base=%q direct=%q", bc.Model, dc.Model)
	}
	if bc.LLM != dc.LLM {
		t.Error("Compactor.LLM differs")
	}
}

// TestEngineDepsForProviderRebindsModel: the CONTAMINATION guard. A non-default
// model must re-derive the model-keyed fields (Model, PromptConfig.Env.Model, the
// agency delta, the Compactor.Model, and the model-specific TokenCounter) — NOT
// inherit the default model's. A shallow clone that swapped only LLM would fail this.
//
// The TokenCounter half is made LOAD-BEARING with Tokenizer:"tiktoken" and two models
// whose tiktoken encodings differ (gpt-4 → cl100k_base vs gpt-4o → o200k_base): the
// counters must produce DIFFERENT counts on a probe string (panel finding #2). Under
// the heuristic counter the two would be indistinguishable (a zero-size value), so the
// guard would pass by accident; tiktoken proves the counter is actually model-keyed.
func TestEngineDepsForProviderRebindsModel(t *testing.T) {
	cfg := Config{Model: "gpt-4o", Compaction: "cascade", Tokenizer: "tiktoken"}
	provider, store, policy, hooks, mcpP, instr := depsTestFixture(t)

	const altModel = "gpt-4" // cl100k_base, a DIFFERENT encoding from gpt-4o (o200k_base)

	deps := engineDepsForProvider(cfg, provider, altModel, 0, store, policy, hooks, mcpP, instr)

	if deps.Model != altModel {
		t.Errorf("Deps.Model = %q, want %q (not the default gpt-4o)", deps.Model, altModel)
	}
	if deps.PromptConfig.Env.Model != altModel {
		t.Errorf("PromptConfig.Env.Model = %q, want %q", deps.PromptConfig.Env.Model, altModel)
	}
	cc, ok := deps.Compactor.(agent.CascadeCompactor)
	if !ok {
		t.Fatalf("Compactor type = %T, want CascadeCompactor", deps.Compactor)
	}
	if cc.Model != altModel {
		t.Errorf("Compactor.Model = %q, want %q (compaction would route to the WRONG model)", cc.Model, altModel)
	}

	// LOAD-BEARING counter assertion (finding #2): the counter derived for altModel
	// (cl100k_base) must DIFFER from a counter built for the default cfg.Model
	// (o200k_base) on a probe string. If engineDepsForProvider leaked the default
	// model's counter, these would be equal and the guard would be vacuous.
	defaultDeps := engineDepsForProvider(cfg, provider, cfg.Model, 0, store, policy, hooks, mcpP, instr)
	const probe = "tokenization differences 12345 café 日本語"
	altCount := deps.TokenCounter.Count(probe)
	defCount := defaultDeps.TokenCounter.Count(probe)
	if altCount == defCount {
		t.Errorf("TokenCounter did not re-key on model: gpt-4 count=%d == gpt-4o count=%d "+
			"(the counter half of the contamination guard is not load-bearing)", altCount, defCount)
	}
	// The Compactor's Counter must be the SAME counter the trigger uses (one per model).
	if cc.Counter.Count(probe) != altCount {
		t.Error("Compactor.Counter and Deps.TokenCounter disagree — they must be ONE counter for the model")
	}

	// The agency Role is keyed on the model: assert it re-derived for the requested
	// model (the contract is uniform across families since issue #49, but the Role
	// must still be rebuilt from the requested model's promptConfig, not leaked).
	wantRole := promptConfig(Config{Model: altModel}, "").Role
	if deps.PromptConfig.Role != wantRole {
		t.Errorf("PromptConfig.Role did not re-derive for the alternate model (agency-delta contamination):\n got %q\nwant %q",
			deps.PromptConfig.Role, wantRole)
	}
}

// TestEngineDepsCarryWallClock is the issue #53 regression guard: every
// production Deps-constructing path must inject the wall clock, or the loop's
// latency instrumentation (EvTurnEnd.DurationMs/TTFT/inter-token and tool
// queued/took) silently reads zero forever. It pins the concrete type too —
// the production clock is engine/adapter/wallclock, never a fake.
func TestEngineDepsCarryWallClock(t *testing.T) {
	cfg := Config{Model: "gpt-5", Workspace: "/repo"}
	provider, store, policy, hooks, mcpP, instr := depsTestFixture(t)

	base := baseEngineDeps(cfg, provider, store, policy, hooks, mcpP, instr)
	if base.Clock == nil {
		t.Fatal("baseEngineDeps Deps.Clock is nil (latency metrics dead, issue #53)")
	}
	if _, ok := base.Clock.(wallclock.Clock); !ok {
		t.Fatalf("baseEngineDeps Deps.Clock = %T, want wallclock.Clock", base.Clock)
	}

	direct := engineDepsForProvider(cfg, provider, cfg.Model, 0, store, policy, hooks, mcpP, instr)
	if direct.Clock == nil {
		t.Fatal("engineDepsForProvider Deps.Clock is nil (latency metrics dead, issue #53)")
	}
	if _, ok := direct.Clock.(wallclock.Clock); !ok {
		t.Fatalf("engineDepsForProvider Deps.Clock = %T, want wallclock.Clock", direct.Clock)
	}

	// Children INHERIT the clock — childEngineDepsForProvider clears the telemetry
	// seams (Sink/ToolCallRecorder) but must NOT clear Clock.
	child := childEngineDepsForProvider(cfg, "member:explorer", provider, cfg.Model, 0, tool.NewCatalog(), promptConfig(cfg, ""), nil)
	if child.Clock == nil {
		t.Fatal("childEngineDepsForProvider Deps.Clock is nil (children must inherit the wall clock)")
	}
	if _, ok := child.Clock.(wallclock.Clock); !ok {
		t.Fatalf("childEngineDepsForProvider Deps.Clock = %T, want wallclock.Clock", child.Clock)
	}

	// The DEFAULT-provider child shape (newChildEngineWithHooks → childEngineDeps:
	// the default Subagent explorer, Parallel branch/judge, usermodel-review
	// children) builds its own Deps literal — assert its Clock too, or deleting
	// the field there would pass the suite while silently zeroing child latency.
	defChild := childEngineDeps(cfg, "explorer", provider, tool.NewCatalog(), cfg.Model, promptConfig(cfg, ""), nil)
	if defChild.Clock == nil {
		t.Fatal("childEngineDeps Deps.Clock is nil (default child engines must carry the wall clock)")
	}
	if _, ok := defChild.Clock.(wallclock.Clock); !ok {
		t.Fatalf("childEngineDeps Deps.Clock = %T, want wallclock.Clock", defChild.Clock)
	}
}
