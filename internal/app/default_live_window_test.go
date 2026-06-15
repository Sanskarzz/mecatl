package app

import (
	"context"
	"testing"
	"time"

	"github.com/stacklok/mecatl/engine/adapter/memfs"
	"github.com/stacklok/mecatl/engine/adapter/memstore"
	"github.com/stacklok/mecatl/engine/adapter/mockllm"
	"github.com/stacklok/mecatl/engine/adapter/permpolicy"
	"github.com/stacklok/mecatl/engine/agent"
	"github.com/stacklok/mecatl/engine/prompt"
	"github.com/stacklok/mecatl/engine/session"
	"github.com/stacklok/mecatl/engine/tool"
	"github.com/stacklok/mecatl/internal/adapter/hookexec"
	"github.com/stacklok/mecatl/internal/adapter/mcp"
	"github.com/stacklok/mecatl/internal/adapter/server"
)

// countingSessionEngineFactory wraps a real SessionEngineFactory, incrementing
// *calls on each invocation — the does-it-rehydrate probe (mirrors
// rehydrate_selector_test.go's recording factory).
func countingSessionEngineFactory(inner server.SessionEngineFactory, calls *int) server.SessionEngineFactory {
	return func(ctx context.Context, sel server.ProviderSelector, specs []mcp.ServerConfig, profile server.SessionProfile, ws string) (server.SessionEngineResult, error) {
		*calls++
		return inner(ctx, sel, specs, profile, ws)
	}
}

// liveWindowReg builds a single-provider registry whose default model is
// LIVE-ONLY: uncatalogued (catalog floor 0 ⇒ contextWindowFor returns 0 pre-swap),
// with an attached meta store the test can Swap to simulate the live model-catalog
// refresh populating a real window. It is the offline analogue of the OpenRouter
// openai/gpt-5.5 case — a default model present in the live listing but absent from
// the embedded catalog, whose build-time baked window is the 128k compaction floor.
func liveWindowReg(provider *mockllm.Provider, id, model string) *providerRegistry {
	meta := newLiveMetaStore()
	return &providerRegistry{
		entries:      map[string]providerEntry{id: {id: id, provider: provider, available: true}},
		defaultID:    id,
		defaultModel: model,
		meta:         meta,
	}
}

// defaultLiveWindowService builds a server.Service whose SessionEngine is the REAL
// composition sessionEngineFactory over reg, and whose ResolveContextWindow is the
// SAME live-first closure Build wires (over reg.meta) — so the engine-window
// rehydration and the #66 echo overlay both read the one live store. The shared
// engine bakes bakedWindow (the t=0 floor, here the catalog 0). DefaultResolvedModel
// carries the baked identity + window, exactly as Build seeds it.
func defaultLiveWindowService(t *testing.T, reg *providerRegistry, provider *mockllm.Provider, model string, bakedWindow int64) *server.Service {
	t.Helper()
	store := memstore.New()
	policy := permpolicy.NewPolicy(defaultRules(), nil)
	cfg := Config{Model: model}
	factory := sessionEngineFactory(cfg, reg, provider, store, policy, hookexec.New(nil), nil, prompt.RootAssembler{}, catalogAssets{})

	// The shared engine bakes the t=0 window — the bug: it never rebuilds, so a
	// live-only default model stays pinned to this floor.
	shared := agent.NewEngine(agent.Deps{
		LLM:                 provider,
		Catalog:             tool.NewCatalog(),
		Policy:              policy,
		Model:               model,
		ContextWindowTokens: int(bakedWindow),
	})
	svc, err := server.NewService(server.Config{
		Engine:               shared,
		Store:                store,
		Workspaces:           func(root string) tool.Workspace { return memfs.NewWorkspace(root) },
		DefaultLimits:        session.Limits{MaxTurns: 5, MaxToolCalls: 10},
		Now:                  func() time.Time { return time.Unix(0, 0) },
		SessionEngine:        factory,
		DefaultResolvedModel: server.ResolvedModel{ProviderID: reg.Default(), ModelID: model, ContextWindow: bakedWindow},
		ResolveContextWindow: func(p, m string) int64 { return int64(reg.meta.contextWindowFor(p, m)) },
	})
	if err != nil {
		t.Fatalf("NewService: %v", err)
	}
	return svc
}

// TestDefaultLiveOnlyModelRehydratesToLiveWindow is THE BUG (issue #66 engine-window
// fix): a DEFAULT-model session whose model is live-only (catalog floor 0 → baked
// 128k) must, AFTER the live model-catalog swap, compact at the LIVE window, not the
// build-time floor. The shared engine bakes the floor pre-swap and never rebuilds, so
// without the run-entry engine-window rehydration the session would compact at ~102k
// forever. The fix rebuilds it into a per-session engine carrying the live window via
// the SAME rehydration seam the selector/no-fs paths use.
//
// MUTATION-VERIFY: reverting build.go's sessionEngineFactory line to
// `catalogContextWindow(reg.Default(), cfg.Model)` (catalog floor 0, ignoring the
// live store) makes the rehydrated engine carry 0 and fails this test.
func TestDefaultLiveOnlyModelRehydratesToLiveWindow(t *testing.T) {
	ctx := context.Background()
	const (
		liveModel  = "openai/gpt-5.5" // live-only: NOT in the embedded catalog
		liveWindow = 1_050_000
		bakedFloor = 128_000 // the build-time baked floor a live-only model is stuck on
	)
	provider := mockllm.New(mockllm.TextTurn("PRE-SWAP"), mockllm.TextTurn("POST-SWAP"))
	reg := liveWindowReg(provider, providerOpenAI, liveModel)
	svc := defaultLiveWindowService(t, reg, provider, liveModel, bakedFloor)

	// Pre-swap: the live store has NO entry for the live-only model (catalog floor 0).
	if got := reg.meta.contextWindowFor(providerOpenAI, liveModel); got != 0 {
		t.Fatalf("pre-swap contextWindowFor(%q) = %d, want 0 (live-only model, catalog floor)", liveModel, got)
	}

	// Create a plain DEFAULT session (empty selector, default profile, real workspace)
	// — it rides the shared engine, no per-session engine registered.
	sess, err := svc.CreateSession(ctx, "/work/livewin", session.ModeDefault, session.Limits{})
	if err != nil {
		t.Fatalf("CreateSession: %v", err)
	}

	// First run pre-swap: live == baked (0 vs 128k → live not > baked), so NO
	// rehydration; the session keeps the shared engine. (The pre-swap-race branch.)
	run1, err := svc.StartRunContent(ctx, sess.ID, "first turn", nil)
	if err != nil {
		t.Fatalf("StartRunContent (pre-swap): %v", err)
	}
	if got := drainRun(run1); got != "PRE-SWAP" {
		t.Fatalf("pre-swap reply = %q, want PRE-SWAP (shared engine)", got)
	}

	// THE LIVE SWAP: the background refresh populates the live window for the model.
	reg.meta.Swap(map[string][]modelEntry{
		providerOpenAI: {{ID: liveModel, ContextLimit: liveWindow}},
	})
	if got := reg.meta.contextWindowFor(providerOpenAI, liveModel); got != liveWindow {
		t.Fatalf("post-swap contextWindowFor(%q) = %d, want %d", liveModel, got, liveWindow)
	}

	// Second run post-swap: live (1,050,000) now strictly exceeds the baked floor
	// (128,000), so defaultSessionNeedsLiveWindow fires and the run-entry seam
	// rehydrates the session into a per-session engine carrying the live window.
	run2, err := svc.StartRunContent(ctx, sess.ID, "second turn", nil)
	if err != nil {
		t.Fatalf("StartRunContent (post-swap): %v", err)
	}
	if got := drainRun(run2); got != "POST-SWAP" {
		t.Fatalf("post-swap reply = %q, want POST-SWAP (rehydrated per-session engine)", got)
	}

	// THE KEY ASSERTION: the now-registered per-session engine carries the LIVE
	// window. ResolvedModel reads se.resolvedModel.ContextWindow, which the factory
	// set from the SAME `contextWindow` local fed into the engine's
	// ContextWindowTokens — so this proves the engine compacts at 1,050,000, NOT the
	// 128k floor.
	got := svc.ResolvedModel(sess.ID)
	if got.ContextWindow != liveWindow {
		t.Fatalf("post-rehydration ResolvedModel.ContextWindow = %d, want the live %d (NOT the %d baked floor — the engine would still compact at the floor)",
			got.ContextWindow, liveWindow, bakedFloor)
	}
	if got.ProviderID != providerOpenAI || got.ModelID != liveModel {
		t.Fatalf("post-rehydration identity = %s/%s, want the verbatim default %s/%s", got.ProviderID, got.ModelID, providerOpenAI, liveModel)
	}
	// (The DIRECT Engine.ContextWindow() assertion AND the rehydrate-exactly-once
	// idempotency check live in the server package — TestRehydratedDefaultEngineWindow*
	// in internal/adapter/server/resolved_model_test.go — where the SessionEngine
	// registry is reachable via the export_test accessor. This app-level test proves the
	// REAL sessionEngineFactory carries the live window into the result; the proxy above
	// reads exactly that.)
}

// TestCataloguedDefaultModelDoesNotRehydrateForWindow pins the bound: a CATALOGUED
// default model (live window == baked window) must NOT needlessly rehydrate — the
// new defaultSessionNeedsLiveWindow trigger fires ONLY when the live window STRICTLY
// exceeds the baked one. A catalogued session keeps the shared engine, exactly like
// TestDefaultFSSessionDoesNotRehydrate.
//
// The recording factory asserts calls==0: a needless rehydration would consult it.
func TestCataloguedDefaultModelDoesNotRehydrateForWindow(t *testing.T) {
	ctx := context.Background()
	const (
		model       = "gpt-5"
		bakedWindow = 400_000 // the catalogued window, also what the live store reports
	)
	provider := mockllm.New(mockllm.TextTurn("SHARED-A"), mockllm.TextTurn("SHARED-B"))
	reg := liveWindowReg(provider, providerOpenAI, model)
	// Seed the live store so contextWindowFor returns the SAME window the engine baked
	// (live == baked ⇒ NOT strictly greater ⇒ no rehydration).
	reg.meta.Swap(map[string][]modelEntry{
		providerOpenAI: {{ID: model, ContextLimit: bakedWindow}},
	})

	store := memstore.New()
	policy := permpolicy.NewPolicy(defaultRules(), nil)
	cfg := Config{Model: model}

	// A counting factory: the per-session-engine path is NEVER consulted for this
	// catalogued default session.
	var factoryCalls int
	realFactory := sessionEngineFactory(cfg, reg, provider, store, policy, hookexec.New(nil), nil, prompt.RootAssembler{}, catalogAssets{})
	countingFactory := countingSessionEngineFactory(realFactory, &factoryCalls)

	shared := agent.NewEngine(agent.Deps{
		LLM:                 provider,
		Catalog:             tool.NewCatalog(),
		Policy:              policy,
		Model:               model,
		ContextWindowTokens: bakedWindow,
	})
	svc, err := server.NewService(server.Config{
		Engine:               shared,
		Store:                store,
		Workspaces:           func(root string) tool.Workspace { return memfs.NewWorkspace(root) },
		DefaultLimits:        session.Limits{MaxTurns: 5, MaxToolCalls: 10},
		Now:                  func() time.Time { return time.Unix(0, 0) },
		SessionEngine:        countingFactory,
		DefaultResolvedModel: server.ResolvedModel{ProviderID: providerOpenAI, ModelID: model, ContextWindow: bakedWindow},
		ResolveContextWindow: func(p, m string) int64 { return int64(reg.meta.contextWindowFor(p, m)) },
	})
	if err != nil {
		t.Fatalf("NewService: %v", err)
	}

	sess, err := svc.CreateSession(ctx, "/work/catalogued", session.ModeDefault, session.Limits{})
	if err != nil {
		t.Fatalf("CreateSession: %v", err)
	}
	run, err := svc.StartRunContent(ctx, sess.ID, "turn", nil)
	if err != nil {
		t.Fatalf("StartRunContent: %v", err)
	}
	if got := drainRun(run); got != "SHARED-A" {
		t.Fatalf("reply = %q, want SHARED-A (a catalogued default session keeps the shared engine)", got)
	}
	if factoryCalls != 0 {
		t.Fatalf("session-engine factory called %d times for a catalogued default session, want 0 (defaultSessionNeedsLiveWindow must not fire when live == baked)", factoryCalls)
	}
	// The echo also reports the baked/live window (they agree — no overlay divergence).
	if got := svc.ResolvedModel(sess.ID).ContextWindow; got != bakedWindow {
		t.Fatalf("ResolvedModel.ContextWindow = %d, want %d (live == baked, no rehydration)", got, bakedWindow)
	}
}

// TestDefaultLiveOnlyModelEchoAndEngineConverge proves the echo and the engine
// CONVERGE on the live window (issue #66): BEFORE rehydration the #66 default-branch
// overlay already yields the live window (honest echo even pre-rehydration); AFTER a
// post-swap run rehydrates the session, the echo takes the per-session-engine branch
// and still reports the live window — the engine the session runs on and the echo
// agree.
func TestDefaultLiveOnlyModelEchoAndEngineConverge(t *testing.T) {
	ctx := context.Background()
	const (
		liveModel  = "openai/gpt-5.5"
		liveWindow = 1_050_000
		bakedFloor = 128_000
	)
	provider := mockllm.New(mockllm.TextTurn("RUN-ONCE"))
	reg := liveWindowReg(provider, providerOpenAI, liveModel)
	svc := defaultLiveWindowService(t, reg, provider, liveModel, bakedFloor)

	sess, err := svc.CreateSession(ctx, "/work/converge", session.ModeDefault, session.Limits{})
	if err != nil {
		t.Fatalf("CreateSession: %v", err)
	}

	// The live swap happens (model-catalog refresh) before the first run reaches the seam.
	reg.meta.Swap(map[string][]modelEntry{
		providerOpenAI: {{ID: liveModel, ContextLimit: liveWindow}},
	})

	// PRE-REHYDRATION echo: no per-session engine yet, so ResolvedModel takes the
	// DEFAULT branch — and the #66 overlay (over the same live store) already reports
	// the live window. The echo is honest before the engine catches up.
	if got := svc.ResolvedModel(sess.ID).ContextWindow; got != liveWindow {
		t.Fatalf("pre-rehydration echo ContextWindow = %d, want the #66 overlay live %d", got, liveWindow)
	}

	// Now run: the seam rehydrates into a per-session engine (live > baked).
	run, err := svc.StartRunContent(ctx, sess.ID, "turn", nil)
	if err != nil {
		t.Fatalf("StartRunContent: %v", err)
	}
	if got := drainRun(run); got != "RUN-ONCE" {
		t.Fatalf("reply = %q, want RUN-ONCE", got)
	}

	// POST-REHYDRATION echo: now the per-session-engine branch — still the live window.
	// Echo and engine have converged.
	if got := svc.ResolvedModel(sess.ID).ContextWindow; got != liveWindow {
		t.Fatalf("post-rehydration echo ContextWindow = %d, want the converged live %d (the engine the session runs on carries it)", got, liveWindow)
	}
}
