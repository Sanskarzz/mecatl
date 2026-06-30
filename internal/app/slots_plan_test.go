package app

import (
	"context"
	"testing"

	"github.com/stacklok/mecatl/engine/adapter/memstore"
	"github.com/stacklok/mecatl/engine/adapter/mockllm"
	"github.com/stacklok/mecatl/engine/adapter/permpolicy"
	"github.com/stacklok/mecatl/engine/prompt"
	"github.com/stacklok/mecatl/engine/session"
	"github.com/stacklok/mecatl/internal/adapter/hookexec"
	"github.com/stacklok/mecatl/internal/adapter/server"
)

// TestPlanSlotDefaultTierIsReasoning pins the one DELIBERATE divergence in
// slotDefaultTier (ADR 0030 Layer 3): the `plan` slot falls through to the `reasoning`
// tier, NOT `cheap` like the three internal-call slots — a plan-mode model is a
// strong-reasoning model. A regression that points plan at cheap would silently demote
// planning turns to the cheapest model.
func TestPlanSlotDefaultTierIsReasoning(t *testing.T) {
	if got := slotDefaultTier[slotPlan]; got != slotReasoning {
		t.Fatalf("slotDefaultTier[plan] = %q, want %q (a plan model is a strong-reasoning model, NOT cheap)", got, slotReasoning)
	}
	// And the three internal-call slots still default to cheap (no accidental flip).
	for _, s := range []string{slotCompaction, slotAskReviewer, slotGuardrail} {
		if got := slotDefaultTier[s]; got != slotCheap {
			t.Fatalf("slotDefaultTier[%s] = %q, want %q (internal-call slots stay cheap)", s, got, slotCheap)
		}
	}
}

// TestPlanSlotResolves pins the resolution paths for the `plan` slot (ADR 0030 Layer 3):
// an explicit binding, the reasoning-tier default fallthrough, and an alias. It reuses
// resolveSlotModel UNCHANGED — the grammar is identical to the call-slots.
func TestPlanSlotResolves(t *testing.T) {
	tests := []struct {
		name      string
		cfg       Config
		wantModel string
		wantOK    bool
	}{
		{
			name:      "explicit plan binding (literal id)",
			cfg:       Config{ModelSlots: map[string]string{slotPlan: "opus-id"}},
			wantModel: "opus-id", wantOK: true,
		},
		{
			name:      "plan via the reasoning tier default fallthrough",
			cfg:       Config{ModelSlots: map[string]string{slotReasoning: "reason-id"}},
			wantModel: "reason-id", wantOK: true,
		},
		{
			name:      "plan via an alias",
			cfg:       Config{ModelSlots: map[string]string{slotPlan: "reasoning"}, ModelAliases: map[string]string{"reasoning": "reason-id"}},
			wantModel: "reason-id", wantOK: true,
		},
		{
			name:   "no plan slot, no reasoning tier ⇒ unset (byte-identical default)",
			cfg:    Config{ModelSlots: map[string]string{slotCheap: "cheap-id"}}, // cheap is unrelated to plan
			wantOK: false,
		},
		{
			name:   "no slots at all ⇒ unset",
			cfg:    Config{},
			wantOK: false,
		},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			diag := &capturingDiag{}
			cfg := tc.cfg
			cfg.Diagnostics = diag
			model, ok := resolveSlotModel(cfg, slotPlan, "session-model")
			if ok != tc.wantOK || model != tc.wantModel {
				t.Fatalf("resolveSlotModel(plan) = (%q, %v), want (%q, %v)", model, ok, tc.wantModel, tc.wantOK)
			}
			// resolveSlotModel is SILENT (the no-per-engine-duplication trap) — the plan
			// slot is no exception.
			if len(diag.lines) != 0 {
				t.Fatalf("resolveSlotModel(plan) must not log; got %v", diag.lines)
			}
		})
	}
}

// TestPlanSlotByteIdenticalWhenUnconfigured is the regression guard: with NO plan slot
// configured, resolveSlotModel(plan) returns ("", false) AND logSlotConfigFacts emits
// NOTHING — byte-identical to pre-Phase-3. A mode flip changes no model.
func TestPlanSlotByteIdenticalWhenUnconfigured(t *testing.T) {
	cfg := Config{Model: "session-model"} // no ModelSlots at all
	if model, ok := resolveSlotModel(cfg, slotPlan, cfg.Model); ok || model != "" {
		t.Fatalf("unconfigured plan slot resolveSlotModel = (%q, %v), want (\"\", false)", model, ok)
	}
	// modeNeedsEngine returns nil (never promote) when no plan slot is active.
	if fn := modeNeedsEngine(cfg); fn != nil {
		t.Fatalf("modeNeedsEngine must be nil with no plan slot (byte-identical default), got non-nil")
	}
	// logSlotConfigFacts logs nothing when ModelSlots is empty.
	diag := &capturingDiag{}
	cfg.Diagnostics = diag
	logSlotConfigFacts(cfg)
	if len(diag.lines) != 0 {
		t.Fatalf("logSlotConfigFacts must be silent with no slots; got %v", diag.lines)
	}
}

// TestModeNeedsEngine pins the composition predicate wired into
// server.Config.ModeNeedsEngine (ADR 0030 Layer 3): true ONLY for ModePlan when the
// plan slot resolves to a model DIFFERING from the shared engine model; nil otherwise.
func TestModeNeedsEngine(t *testing.T) {
	t.Run("active plan slot ⇒ true only for plan", func(t *testing.T) {
		cfg := Config{Model: "session-model", ModelSlots: map[string]string{slotPlan: "plan-id"}}
		fn := modeNeedsEngine(cfg)
		if fn == nil {
			t.Fatal("modeNeedsEngine must be non-nil with an active plan slot")
		}
		if !fn(session.ModePlan) {
			t.Fatal("ModePlan must need an engine (the plan model differs)")
		}
		if fn(session.ModeDefault) || fn(session.ModeAccept) {
			t.Fatal("only ModePlan needs an engine; default/acceptEdits keep the session model")
		}
	})
	t.Run("plan slot resolves to the shared-engine model ⇒ nil (no change)", func(t *testing.T) {
		cfg := Config{Model: "same-id", ModelSlots: map[string]string{slotPlan: "same-id"}}
		if fn := modeNeedsEngine(cfg); fn != nil {
			t.Fatal("a plan slot resolving to cfg.Model changes nothing — modeNeedsEngine must be nil")
		}
	})
}

// planFactory builds a real sessionEngineFactory over a single mock provider with a
// `plan` slot bound to planModel and a session default of cfg.Model.
func planFactory(t *testing.T, sessionModel, planModel string) server.SessionEngineFactory {
	t.Helper()
	cfg := Config{
		Model:      sessionModel,
		ModelSlots: map[string]string{slotPlan: planModel},
	}
	provider := mockllm.New(mockllm.TextTurn("X"))
	reg := regForTest(provider, providerOpenAI, sessionModel)
	store := memstore.New()
	policy := permpolicy.NewPolicy(defaultRules(), nil)
	return sessionEngineFactory(cfg, reg, provider, store, policy, hookexec.New(nil), nil, prompt.RootAssembler{}, catalogAssets{}, nil)
}

// TestSessionEngineFactoryPlanVsExecute is the FACTORY-level Phase 3 guard (ADR 0030
// Layer 3): the SAME factory, the SAME zero selector, called with mode=ModePlan vs
// mode=ModeDefault, resolves the engine to the PLAN model vs the SESSION model — and
// stamps BuiltForMode from the one source. The provider is unchanged (fixed per
// session); only the model differs.
func TestSessionEngineFactoryPlanVsExecute(t *testing.T) {
	const sessionModel, planModel = "gpt-5", "opus-plan-id"
	factory := planFactory(t, sessionModel, planModel)
	ctx := context.Background()

	plan, err := factory(ctx, server.ProviderSelector{}, nil, server.ProfileDefault, "", session.ModePlan)
	if err != nil {
		t.Fatalf("factory(plan): %v", err)
	}
	defer func() { _ = plan.Close() }()
	exec, err := factory(ctx, server.ProviderSelector{}, nil, server.ProfileDefault, "", session.ModeDefault)
	if err != nil {
		t.Fatalf("factory(default): %v", err)
	}
	defer func() { _ = exec.Close() }()

	if plan.ModelID != planModel {
		t.Fatalf("plan-mode engine ModelID = %q, want the plan slot model %q", plan.ModelID, planModel)
	}
	if exec.ModelID != sessionModel {
		t.Fatalf("default-mode engine ModelID = %q, want the session model %q (no plan re-resolution)", exec.ModelID, sessionModel)
	}
	if plan.BuiltForMode != session.ModePlan {
		t.Fatalf("plan-mode BuiltForMode = %q, want %q", plan.BuiltForMode, session.ModePlan)
	}
	if exec.BuiltForMode != session.ModeDefault {
		t.Fatalf("default-mode BuiltForMode = %q, want %q", exec.BuiltForMode, session.ModeDefault)
	}
	// Provider is FIXED per session — the plan re-resolution swaps the MODEL only.
	if plan.ProviderID != exec.ProviderID {
		t.Fatalf("plan vs default ProviderID diverged (%q vs %q) — the plan slot must NOT switch provider", plan.ProviderID, exec.ProviderID)
	}
}

// TestSessionEngineFactoryNoPlanSlotByteIdentical pins the factory byte-identical
// guarantee: with NO plan slot configured, calling the factory with ModePlan yields the
// SAME model as ModeDefault (the session model) — a mode flip changes nothing.
func TestSessionEngineFactoryNoPlanSlotByteIdentical(t *testing.T) {
	const sessionModel = "gpt-5"
	cfg := Config{Model: sessionModel} // no plan slot
	provider := mockllm.New(mockllm.TextTurn("X"))
	reg := regForTest(provider, providerOpenAI, sessionModel)
	factory := sessionEngineFactory(cfg, reg, provider, memstore.New(), permpolicy.NewPolicy(defaultRules(), nil), hookexec.New(nil), nil, prompt.RootAssembler{}, catalogAssets{}, nil)
	ctx := context.Background()

	plan, err := factory(ctx, server.ProviderSelector{}, nil, server.ProfileDefault, "", session.ModePlan)
	if err != nil {
		t.Fatalf("factory(plan): %v", err)
	}
	defer func() { _ = plan.Close() }()
	if plan.ModelID != sessionModel {
		t.Fatalf("plan-mode engine ModelID = %q with NO plan slot, want the unchanged session model %q (byte-identical)", plan.ModelID, sessionModel)
	}
}
