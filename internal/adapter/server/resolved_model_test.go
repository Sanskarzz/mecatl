package server_test

import (
	"context"
	"testing"
	"time"

	mecatlv1 "github.com/stacklok/mecatl/contracts/gen/go/mecatl/v1"
	"github.com/stacklok/mecatl/engine/adapter/memfs"
	"github.com/stacklok/mecatl/engine/adapter/memstore"
	"github.com/stacklok/mecatl/engine/adapter/mockllm"
	"github.com/stacklok/mecatl/engine/adapter/permpolicy"
	"github.com/stacklok/mecatl/engine/agent"
	"github.com/stacklok/mecatl/engine/session"
	"github.com/stacklok/mecatl/engine/tool"
	"github.com/stacklok/mecatl/internal/adapter/mcp"
	"github.com/stacklok/mecatl/internal/adapter/server"
)

// newResolvedModelService builds a Service whose shared/default engine resolves to
// DefaultResolvedModel, with the supplied per-session factory. It mirrors
// newMCPServiceStore but pins a DefaultResolvedModel so the default-path echo can be
// asserted.
func newResolvedModelService(t *testing.T, dflt server.ResolvedModel, factory server.SessionEngineFactory) *server.Service {
	t.Helper()
	shared := agent.NewEngine(agent.Deps{
		LLM:     mockllm.New(mockllm.TextTurn("SHARED")),
		Catalog: tool.NewCatalog(),
		Policy:  permpolicy.NewPolicy(nil, nil),
		Model:   "test-model",
	})
	svc, err := server.NewService(server.Config{
		Engine:               shared,
		Store:                memstore.New(),
		Workspaces:           func(root string) tool.Workspace { return memfs.NewWorkspace(root) },
		DefaultLimits:        session.Limits{MaxTurns: 10, MaxToolCalls: 20},
		Now:                  func() time.Time { return time.Unix(0, 0) },
		SessionEngine:        factory,
		DefaultResolvedModel: dflt,
	})
	if err != nil {
		t.Fatalf("new service: %v", err)
	}
	return svc
}

// newResolvedModelServiceWithResolver mirrors newResolvedModelService but also wires
// the live-first Config.ResolveContextWindow closure (issue #66).
func newResolvedModelServiceWithResolver(t *testing.T, dflt server.ResolvedModel, factory server.SessionEngineFactory, resolve func(p, m string) int64) *server.Service {
	t.Helper()
	shared := agent.NewEngine(agent.Deps{
		LLM:     mockllm.New(mockllm.TextTurn("SHARED")),
		Catalog: tool.NewCatalog(),
		Policy:  permpolicy.NewPolicy(nil, nil),
		Model:   "test-model",
	})
	svc, err := server.NewService(server.Config{
		Engine:               shared,
		Store:                memstore.New(),
		Workspaces:           func(root string) tool.Workspace { return memfs.NewWorkspace(root) },
		DefaultLimits:        session.Limits{MaxTurns: 10, MaxToolCalls: 20},
		Now:                  func() time.Time { return time.Unix(0, 0) },
		SessionEngine:        factory,
		DefaultResolvedModel: dflt,
		ResolveContextWindow: resolve,
	})
	if err != nil {
		t.Fatalf("new service: %v", err)
	}
	return svc
}

// TestServiceResolvedModelLiveFirstWindow (issue #66): a DEFAULT session whose model
// is in the live listing but NOT the curated catalog bakes ContextWindow == 0
// (no footer bar). With ResolveContextWindow wired, the default-path echo overlays
// the live window at call time — provider/model identity stays the baked value.
func TestServiceResolvedModelLiveFirstWindow(t *testing.T) {
	// The baked seed: a live-only model, catalog floor 0 (the bug repro).
	dflt := server.ResolvedModel{ProviderID: "openrouter", ModelID: "openai/gpt-5.5", ContextWindow: 0}
	resolve := func(p, m string) int64 {
		if p == "openrouter" && m == "openai/gpt-5.5" {
			return 1_050_000
		}
		return 0
	}
	svc := newResolvedModelServiceWithResolver(t, dflt, nil, resolve)

	sess, err := svc.CreateSessionWithProvider(context.Background(), "/ws", session.ModeDefault, session.Limits{}, server.ProviderSelector{})
	if err != nil {
		t.Fatalf("CreateSessionWithProvider(zero): %v", err)
	}
	got := svc.ResolvedModel(sess.ID)
	if got.ContextWindow != 1_050_000 {
		t.Fatalf("ResolvedModel.ContextWindow = %d, want the live-first 1050000 (issue #66 overlay)", got.ContextWindow)
	}
	if got.ProviderID != "openrouter" || got.ModelID != "openai/gpt-5.5" {
		t.Fatalf("ResolvedModel identity = %s/%s, want the verbatim baked openrouter/openai/gpt-5.5", got.ProviderID, got.ModelID)
	}
}

// TestServiceResolvedModelNilResolverByteIdentical: with ResolveContextWindow nil
// (the memstore/driver/test paths), the default-path echo is the verbatim baked
// DefaultResolvedModel — byte-identical to the pre-issue-#66 behaviour.
func TestServiceResolvedModelNilResolverByteIdentical(t *testing.T) {
	dflt := server.ResolvedModel{ProviderID: "openai", ModelID: "gpt-default", ContextWindow: 128000}
	svc := newResolvedModelServiceWithResolver(t, dflt, nil, nil) // nil resolver

	sess, err := svc.CreateSessionWithProvider(context.Background(), "/ws", session.ModeDefault, session.Limits{}, server.ProviderSelector{})
	if err != nil {
		t.Fatalf("CreateSessionWithProvider(zero): %v", err)
	}
	if got := svc.ResolvedModel(sess.ID); got != dflt {
		t.Fatalf("nil-resolver ResolvedModel = %+v, want the verbatim baked %+v (byte-identical to pre-#66)", got, dflt)
	}
}

// TestServiceResolvedModelResolverZeroKeepsBaked (issue #66 review gap): a DEFAULT
// session with a baked NON-ZERO DefaultResolvedModel.ContextWindow and a
// ResolveContextWindow returning 0 (not-yet-swapped / unknown to the live store) must
// echo the BAKED window — the resolver's 0 must NOT clobber a known baked value. This
// pins the `if w > 0` overlay guard.
//
// MUTATION-VERIFY: dropping the `if w > 0` guard in Service.ResolvedModel (so the
// resolver's 0 overwrites rm.ContextWindow) makes this fail.
func TestServiceResolvedModelResolverZeroKeepsBaked(t *testing.T) {
	const baked = 200000
	dflt := server.ResolvedModel{ProviderID: "openai", ModelID: "gpt-baked", ContextWindow: baked}
	// A resolver that returns 0 for everything (pre-swap: the live store has no entry
	// for this model and — unlike contextWindowFor — this stand-in does not floor to
	// the catalog, so it returns a bare 0).
	resolve := func(_, _ string) int64 { return 0 }
	svc := newResolvedModelServiceWithResolver(t, dflt, nil, resolve)

	sess, err := svc.CreateSessionWithProvider(context.Background(), "/ws", session.ModeDefault, session.Limits{}, server.ProviderSelector{})
	if err != nil {
		t.Fatalf("CreateSessionWithProvider(zero): %v", err)
	}
	if got := svc.ResolvedModel(sess.ID); got.ContextWindow != baked {
		t.Fatalf("ResolvedModel.ContextWindow = %d, want the baked %d (a resolver 0 must NOT clobber a known baked window)", got.ContextWindow, baked)
	}
}

// TestServiceResolvedModelResolverNeverLowers (issue #66 regression): a CATALOGUED
// default model with the resolver wired still echoes its catalog window — the
// resolver returns the catalog floor when no live entry exists, so the fix never
// lowers a known window. (A zero from the resolver keeps the baked seed.)
func TestServiceResolvedModelResolverNeverLowers(t *testing.T) {
	dflt := server.ResolvedModel{ProviderID: "openai", ModelID: "gpt-5", ContextWindow: 400000}
	// Resolver returns the catalog floor for the catalogued model (mirrors
	// contextWindowFor falling back to the catalog on a live miss).
	resolve := func(p, m string) int64 {
		if p == "openai" && m == "gpt-5" {
			return 400000
		}
		return 0
	}
	svc := newResolvedModelServiceWithResolver(t, dflt, nil, resolve)

	sess, err := svc.CreateSessionWithProvider(context.Background(), "/ws", session.ModeDefault, session.Limits{}, server.ProviderSelector{})
	if err != nil {
		t.Fatalf("CreateSessionWithProvider(zero): %v", err)
	}
	if got := svc.ResolvedModel(sess.ID); got != dflt {
		t.Fatalf("ResolvedModel = %+v, want the unchanged catalogued %+v (fix must never lower a known window)", got, dflt)
	}
}

// TestServiceResolvedModelResolverDoesNotTouchSelector (issue #66): a per-session
// SELECTOR session reads se.resolvedModel from its factory; the default-path live
// overlay must NOT touch the sessionEngines[id] branch even with a resolver wired
// (the factory already carries the live-first window).
func TestServiceResolvedModelResolverDoesNotTouchSelector(t *testing.T) {
	dflt := server.ResolvedModel{ProviderID: "openrouter", ModelID: "openai/gpt-5.5", ContextWindow: 0}
	// A resolver that would return a DIFFERENT window if (wrongly) consulted on the
	// selector path — proving the overlay is skipped for per-session engines.
	resolve := func(_, _ string) int64 { return 999_999 }
	perSession := agent.NewEngine(agent.Deps{
		LLM:     mockllm.New(mockllm.TextTurn("PER-SESSION")),
		Catalog: tool.NewCatalog(),
		Policy:  permpolicy.NewPolicy(nil, nil),
		Model:   "anthropic/claude-opus-4.5",
	})
	factory := func(_ context.Context, sel server.ProviderSelector, _ []mcp.ServerConfig, _ server.SessionProfile, _ string) (server.SessionEngineResult, error) {
		return server.SessionEngineResult{
			Engine:        perSession,
			ProviderID:    sel.ProviderID,
			ModelID:       sel.ModelID,
			ContextWindow: 200000,
			Close:         func() error { return nil },
		}, nil
	}
	svc := newResolvedModelServiceWithResolver(t, dflt, factory, resolve)

	sel := server.ProviderSelector{ProviderID: "anthropic", ModelID: "claude-opus-4.5"}
	sess, err := svc.CreateSessionWithProvider(context.Background(), "/ws", session.ModeDefault, session.Limits{}, sel)
	if err != nil {
		t.Fatalf("CreateSessionWithProvider: %v", err)
	}
	want := server.ResolvedModel{ProviderID: "anthropic", ModelID: "claude-opus-4.5", ContextWindow: 200000}
	if got := svc.ResolvedModel(sess.ID); got != want {
		t.Fatalf("per-session ResolvedModel = %+v, want the factory %+v (resolver must NOT overlay the selector branch)", got, want)
	}
}

// TestServiceResolvedModelDefaultPath: a zero-selector session (no per-session
// engine) reports Config.DefaultResolvedModel verbatim — the same composition
// single-source discipline as SessionCapabilities. The value is NOT read back from
// the request (which carries an empty model_id for the default session).
func TestServiceResolvedModelDefaultPath(t *testing.T) {
	dflt := server.ResolvedModel{ProviderID: "openai", ModelID: "gpt-default", ContextWindow: 128000}
	svc := newResolvedModelService(t, dflt, nil)

	sess, err := svc.CreateSessionWithProvider(context.Background(), "/ws", session.ModeDefault, session.Limits{}, server.ProviderSelector{})
	if err != nil {
		t.Fatalf("CreateSessionWithProvider(zero): %v", err)
	}
	got := svc.ResolvedModel(sess.ID)
	if got != dflt {
		t.Fatalf("ResolvedModel(default session) = %+v, want %+v (the composition DefaultResolvedModel)", got, dflt)
	}
	// An unregistered id also falls back to the default (mirrors SessionCapabilities).
	if got := svc.ResolvedModel("no-such-session"); got != dflt {
		t.Fatalf("ResolvedModel(unknown) = %+v, want the default %+v", got, dflt)
	}
}

// TestServiceResolvedModelPerSession: an explicit selector registers a per-session
// engine carrying the factory's resolved ProviderID/ModelID/ContextWindow, and
// ResolvedModel returns THAT (not the default). After CloseSession evicts the
// per-session engine, it falls back to the default.
func TestServiceResolvedModelPerSession(t *testing.T) {
	dflt := server.ResolvedModel{ProviderID: "openai", ModelID: "gpt-default", ContextWindow: 128000}
	perSession := agent.NewEngine(agent.Deps{
		LLM:     mockllm.New(mockllm.TextTurn("PER-SESSION")),
		Catalog: tool.NewCatalog(),
		Policy:  permpolicy.NewPolicy(nil, nil),
		Model:   "anthropic/claude-opus-4.5",
	})
	factory := func(_ context.Context, sel server.ProviderSelector, _ []mcp.ServerConfig, _ server.SessionProfile, _ string) (server.SessionEngineResult, error) {
		return server.SessionEngineResult{
			Engine:        perSession,
			ProviderID:    sel.ProviderID,
			ModelID:       sel.ModelID,
			ContextWindow: 200000,
			Close:         func() error { return nil },
		}, nil
	}
	svc := newResolvedModelService(t, dflt, factory)

	sel := server.ProviderSelector{ProviderID: "anthropic", ModelID: "claude-opus-4.5"}
	sess, err := svc.CreateSessionWithProvider(context.Background(), "/ws", session.ModeDefault, session.Limits{}, sel)
	if err != nil {
		t.Fatalf("CreateSessionWithProvider: %v", err)
	}
	want := server.ResolvedModel{ProviderID: "anthropic", ModelID: "claude-opus-4.5", ContextWindow: 200000}
	if got := svc.ResolvedModel(sess.ID); got != want {
		t.Fatalf("ResolvedModel(per-session) = %+v, want the resolved selector %+v", got, want)
	}
	svc.CloseSession(sess.ID)
	if got := svc.ResolvedModel(sess.ID); got != dflt {
		t.Fatalf("ResolvedModel after CloseSession = %+v, want fallback to default %+v", got, dflt)
	}
}

// TestGRPCCreateSessionEchoesResolvedModel: the gRPC CreateSession handler echoes
// resolved_model from Service.ResolvedModel for both the default path AND an
// explicit selector — asserting the EFFECTIVE (resolved) values cross the wire, not
// the raw request fields.
func TestGRPCCreateSessionEchoesResolvedModel(t *testing.T) {
	dflt := server.ResolvedModel{ProviderID: "openai", ModelID: "gpt-default", ContextWindow: 128000}
	factory := func(_ context.Context, sel server.ProviderSelector, _ []mcp.ServerConfig, _ server.SessionProfile, _ string) (server.SessionEngineResult, error) {
		return server.SessionEngineResult{
			Engine:        agent.NewEngine(agent.Deps{LLM: mockllm.New(mockllm.TextTurn("X")), Catalog: tool.NewCatalog(), Policy: permpolicy.NewPolicy(nil, nil), Model: "m"}),
			ProviderID:    sel.ProviderID,
			ModelID:       sel.ModelID,
			ContextWindow: 200000,
			Close:         func() error { return nil },
		}, nil
	}
	svc := newResolvedModelService(t, dflt, factory)
	h := server.NewHarnessServer(svc)

	t.Run("default selector echoes the composition default", func(t *testing.T) {
		resp, err := h.CreateSession(context.Background(), &mecatlv1.CreateSessionRequest{Workspace: "/ws"})
		if err != nil {
			t.Fatalf("CreateSession: %v", err)
		}
		rm := resp.GetResolvedModel()
		if rm.GetProviderId() != "openai" || rm.GetModelId() != "gpt-default" || rm.GetContextWindow() != 128000 {
			t.Fatalf("resolved_model = %+v, want the composition default openai/gpt-default/128000", rm)
		}
	})

	t.Run("explicit selector echoes the resolved values, not the raw request model", func(t *testing.T) {
		// The request model_id is "claude-opus-4.5"; the factory resolved it under the
		// "anthropic" provider with a 200000 window. The echo must reflect the RESOLVED
		// values from Service.ResolvedModel, not be a naive read-back of the request.
		resp, err := h.CreateSession(context.Background(), &mecatlv1.CreateSessionRequest{
			Workspace:  "/ws",
			ProviderId: "anthropic",
			ModelId:    "claude-opus-4.5",
		})
		if err != nil {
			t.Fatalf("CreateSession(selector): %v", err)
		}
		rm := resp.GetResolvedModel()
		if rm.GetProviderId() != "anthropic" || rm.GetModelId() != "claude-opus-4.5" || rm.GetContextWindow() != 200000 {
			t.Fatalf("resolved_model = %+v, want anthropic/claude-opus-4.5/200000", rm)
		}
	})
}

// TestGRPCGetSessionEchoesResolvedModel proves the gRPC GetSession handler threads
// Service.ResolvedModel(sess.ID) into the Session snapshot (toProtoSession), not a
// zero ResolvedModel. Uses an explicit selector so the per-session resolved value is
// distinct from the default — a handler dropping it would echo zero and fail.
func TestGRPCGetSessionEchoesResolvedModel(t *testing.T) {
	dflt := server.ResolvedModel{ProviderID: "openai", ModelID: "gpt-default", ContextWindow: 128000}
	factory := func(_ context.Context, sel server.ProviderSelector, _ []mcp.ServerConfig, _ server.SessionProfile, _ string) (server.SessionEngineResult, error) {
		return server.SessionEngineResult{
			Engine:        agent.NewEngine(agent.Deps{LLM: mockllm.New(mockllm.TextTurn("X")), Catalog: tool.NewCatalog(), Policy: permpolicy.NewPolicy(nil, nil), Model: "m"}),
			ProviderID:    sel.ProviderID,
			ModelID:       sel.ModelID,
			ContextWindow: 200000,
			Close:         func() error { return nil },
		}, nil
	}
	svc := newResolvedModelService(t, dflt, factory)
	h := server.NewHarnessServer(svc)

	createResp, err := h.CreateSession(context.Background(), &mecatlv1.CreateSessionRequest{
		Workspace:  "/ws",
		ProviderId: "anthropic",
		ModelId:    "claude-opus-4.5",
	})
	if err != nil {
		t.Fatalf("CreateSession(selector): %v", err)
	}
	getResp, err := h.GetSession(context.Background(), &mecatlv1.GetSessionRequest{SessionId: createResp.GetSessionId()})
	if err != nil {
		t.Fatalf("GetSession: %v", err)
	}
	rm := getResp.GetSession().GetResolvedModel()
	if rm.GetProviderId() != "anthropic" || rm.GetModelId() != "claude-opus-4.5" || rm.GetContextWindow() != 200000 {
		t.Fatalf("Session snapshot resolved_model = %+v, want the Service-resolved anthropic/claude-opus-4.5/200000 (handler must thread Service.ResolvedModel, not zero)", rm)
	}
}

// liveWindowEngineFactory returns a SessionEngineFactory that builds a REAL engine
// whose ContextWindowTokens == win (so Engine.ContextWindow() is directly
// observable), recording each call in *calls. It models the composition factory's
// post-swap behaviour: the rebuilt engine carries the live window. reply scripts the
// engine's single turn so a driven run is observable. The result echoes win on
// ContextWindow too (the factory sets both from the same resolved local upstream).
func liveWindowEngineFactory(provider *mockllm.Provider, providerID, modelID string, win int64, calls *int) server.SessionEngineFactory {
	return func(_ context.Context, _ server.ProviderSelector, _ []mcp.ServerConfig, _ server.SessionProfile, _ string) (server.SessionEngineResult, error) {
		*calls++
		eng := agent.NewEngine(agent.Deps{
			LLM:                 provider,
			Catalog:             tool.NewCatalog(),
			Policy:              permpolicy.NewPolicy(nil, nil),
			Model:               modelID,
			ContextWindowTokens: int(win),
		})
		return server.SessionEngineResult{
			Engine:        eng,
			ProviderID:    providerID,
			ModelID:       modelID,
			ContextWindow: win,
			Close:         func() error { return nil },
		}, nil
	}
}

// TestRehydratedDefaultEngineWindowIsLive is the DIRECT engine-window assertion
// (issue #66 engine-window fix, review finding 2): after a live-only default session
// rehydrates, the per-session engine's ACTUAL compaction window —
// Engine.ContextWindow(), the value maybeCompact divides by CompactionRatio — is the
// live window, not the build-time baked floor. The app-level proxy test reads
// se.resolvedModel.ContextWindow; THIS reads the engine's own Deps.ContextWindowTokens
// via the export_test accessor, so a future floor/cap change that diverged the echo
// from the engine would be caught here.
func TestRehydratedDefaultEngineWindowIsLive(t *testing.T) {
	const (
		liveModel  = "openai/gpt-5.5"
		liveWindow = 1_050_000
		bakedFloor = 128_000
	)
	provider := mockllm.New(mockllm.TextTurn("REHYDRATED"))
	var calls int
	factory := liveWindowEngineFactory(provider, "openrouter", liveModel, liveWindow, &calls)

	shared := agent.NewEngine(agent.Deps{
		LLM:                 mockllm.New(mockllm.TextTurn("SHARED")),
		Catalog:             tool.NewCatalog(),
		Policy:              permpolicy.NewPolicy(nil, nil),
		Model:               liveModel,
		ContextWindowTokens: bakedFloor,
	})
	svc, err := server.NewService(server.Config{
		Engine:               shared,
		Store:                memstore.New(),
		Workspaces:           func(root string) tool.Workspace { return memfs.NewWorkspace(root) },
		DefaultLimits:        session.Limits{MaxTurns: 5},
		Now:                  func() time.Time { return time.Unix(0, 0) },
		SessionEngine:        factory,
		DefaultResolvedModel: server.ResolvedModel{ProviderID: "openrouter", ModelID: liveModel, ContextWindow: bakedFloor},
		// The live resolver reports the live window (the post-swap state) — strictly
		// greater than the baked floor, so defaultSessionNeedsLiveWindow fires.
		ResolveContextWindow: func(_, _ string) int64 { return liveWindow },
	})
	if err != nil {
		t.Fatalf("NewService: %v", err)
	}

	sess, err := svc.CreateSession(context.Background(), "/work/win", session.ModeDefault, session.Limits{})
	if err != nil {
		t.Fatalf("CreateSession: %v", err)
	}
	run, err := svc.StartRunContent(context.Background(), sess.ID, "turn", nil)
	if err != nil {
		t.Fatalf("StartRunContent: %v", err)
	}
	if got := drainServerRun(run); got != "REHYDRATED" {
		t.Fatalf("reply = %q, want REHYDRATED (the rehydrated per-session engine)", got)
	}

	win, registered := svc.SessionEngineContextWindowForTest(sess.ID)
	if !registered {
		t.Fatal("no per-session engine registered after the run (the live-only default session must have rehydrated)")
	}
	if win != liveWindow {
		t.Fatalf("rehydrated Engine.ContextWindow() = %d, want the live %d (the engine compacts at THIS value; %d would be the baked floor)", win, liveWindow, bakedFloor)
	}
}

// TestRehydratedDefaultEngineRehydratesExactlyOnce pins the CRITICAL idempotency
// property (review finding 1): a live-only default session rehydrates EXACTLY ONCE,
// then rides the per-session engine on every subsequent run — it must NOT re-mint a
// fresh per-session engine (+ MCP manager + close func) per run-entry, which would
// leak engines/managers for the life of the session. The `!hasEngine` short-circuit
// in Service.engineAndWorkspaceFor is what stops the gate re-evaluating once an
// engine is registered.
//
// MUTATION-VERIFY: removing the `!hasEngine &&` short-circuit (so the gate
// re-evaluates defaultSessionNeedsLiveWindow on every run-entry) makes the factory
// fire a SECOND time on the second post-swap run and fails this test.
func TestRehydratedDefaultEngineRehydratesExactlyOnce(t *testing.T) {
	const (
		liveModel  = "openai/gpt-5.5"
		liveWindow = 1_050_000
		bakedFloor = 128_000
	)
	// Two scripted turns on the per-session engine (the two post-swap runs); the
	// pre-swap run rides the shared engine.
	provider := mockllm.New(mockllm.TextTurn("POST-1"), mockllm.TextTurn("POST-2"))
	var calls int
	factory := liveWindowEngineFactory(provider, "openrouter", liveModel, liveWindow, &calls)

	// The live window starts EQUAL to the baked floor (pre-swap state: no live entry,
	// resolver floors to baked) and flips to the live value (post-swap) via the toggle.
	live := int64(bakedFloor)
	shared := agent.NewEngine(agent.Deps{
		LLM:                 mockllm.New(mockllm.TextTurn("PRE")),
		Catalog:             tool.NewCatalog(),
		Policy:              permpolicy.NewPolicy(nil, nil),
		Model:               liveModel,
		ContextWindowTokens: bakedFloor,
	})
	svc, err := server.NewService(server.Config{
		Engine:               shared,
		Store:                memstore.New(),
		Workspaces:           func(root string) tool.Workspace { return memfs.NewWorkspace(root) },
		DefaultLimits:        session.Limits{MaxTurns: 5},
		Now:                  func() time.Time { return time.Unix(0, 0) },
		SessionEngine:        factory,
		DefaultResolvedModel: server.ResolvedModel{ProviderID: "openrouter", ModelID: liveModel, ContextWindow: bakedFloor},
		ResolveContextWindow: func(_, _ string) int64 { return live },
	})
	if err != nil {
		t.Fatalf("NewService: %v", err)
	}

	sess, err := svc.CreateSession(context.Background(), "/work/once", session.ModeDefault, session.Limits{})
	if err != nil {
		t.Fatalf("CreateSession: %v", err)
	}

	// Pre-swap run: live == baked ⇒ no rehydration, shared engine, factory untouched.
	run0, err := svc.StartRunContent(context.Background(), sess.ID, "pre", nil)
	if err != nil {
		t.Fatalf("StartRunContent (pre-swap): %v", err)
	}
	if got := drainServerRun(run0); got != "PRE" {
		t.Fatalf("pre-swap reply = %q, want PRE (shared engine)", got)
	}
	if calls != 0 {
		t.Fatalf("factory called %d times pre-swap, want 0 (shared engine)", calls)
	}

	// THE SWAP: the live window now exceeds the baked floor.
	live = liveWindow

	// First post-swap run: rehydrates exactly once.
	run1, err := svc.StartRunContent(context.Background(), sess.ID, "post-1", nil)
	if err != nil {
		t.Fatalf("StartRunContent (post-swap 1): %v", err)
	}
	if got := drainServerRun(run1); got != "POST-1" {
		t.Fatalf("post-swap-1 reply = %q, want POST-1 (rehydrated engine)", got)
	}
	if calls != 1 {
		t.Fatalf("factory called %d times after the FIRST post-swap run, want exactly 1 (the one rehydration)", calls)
	}

	// Second post-swap run: MUST ride the already-registered per-session engine — the
	// factory is NOT consulted again (no engine re-mint, no MCP-manager leak).
	run2, err := svc.StartRunContent(context.Background(), sess.ID, "post-2", nil)
	if err != nil {
		t.Fatalf("StartRunContent (post-swap 2): %v", err)
	}
	if got := drainServerRun(run2); got != "POST-2" {
		t.Fatalf("post-swap-2 reply = %q, want POST-2 (same per-session engine)", got)
	}
	if calls != 1 {
		t.Fatalf("factory called %d times after the SECOND post-swap run, want STILL exactly 1 (re-minting per run leaks engines/MCP managers — the !hasEngine short-circuit must hold)", calls)
	}
	// The engine window is stable across the second run (same engine, no re-mint).
	if win, ok := svc.SessionEngineContextWindowForTest(sess.ID); !ok || win != liveWindow {
		t.Fatalf("after the second run Engine.ContextWindow() = %d (registered=%v), want the same %d", win, ok, liveWindow)
	}
}
