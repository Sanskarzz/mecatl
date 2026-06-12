package app

import (
	"context"
	"errors"
	"strings"
	"testing"
	"time"

	"github.com/stacklok/mecatl/engine/adapter/memfs"
	"github.com/stacklok/mecatl/engine/adapter/memstore"
	"github.com/stacklok/mecatl/engine/adapter/mockllm"
	"github.com/stacklok/mecatl/engine/adapter/permpolicy"
	"github.com/stacklok/mecatl/engine/agent"
	"github.com/stacklok/mecatl/engine/port"
	"github.com/stacklok/mecatl/engine/prompt"
	"github.com/stacklok/mecatl/engine/session"
	"github.com/stacklok/mecatl/internal/adapter/hookexec"
	"github.com/stacklok/mecatl/internal/adapter/mcp"
	"github.com/stacklok/mecatl/internal/adapter/providercatalog"
	"github.com/stacklok/mecatl/internal/adapter/server"
)

// fakeSink records nothing; it exists only so a test can pass a non-nil EventSink
// and assert it threads through baseEngineDeps onto the per-session engine.
type fakeSink struct{}

func (fakeSink) Emit(context.Context, session.Event) {}

// configWithCollaborators returns a Config that turns ON the optional collaborators
// whose silent loss the [High] review flagged: a cascade compactor (a DISTINCT type
// from NewEngine's HeuristicCompactor default), the tiktoken counter, and slash
// commands (so CommandExpander is a real DirCommandExpander, not the NoopExpander).
func configWithCollaborators() Config {
	return Config{
		Model:          "test-model",
		Compaction:     "cascade",
		Tokenizer:      "tiktoken",
		EnableCommands: true,
		Sink:           fakeSink{},
	}
}

// TestBaseEngineDepsCarriesFullCollaboratorSet asserts the shared deps builder
// populates EVERY collaborator the main engine needs — the drift guard the [High]
// review required. Because both buildEngine and sessionEngineFactory build their
// agent.Deps through baseEngineDeps, a populated result here proves the per-session
// engine cannot silently drop the compactor, token counter, command expander,
// store, sink, logger, policy, or hooks.
func TestBaseEngineDepsCarriesFullCollaboratorSet(t *testing.T) {
	cfg := configWithCollaborators()
	provider := mockllm.New(mockllm.TextTurn("x"))
	store := memstore.New()
	policy := permpolicy.NewPolicy(defaultRules(), nil)
	hooks := hookexec.New(nil)

	deps := baseEngineDeps(cfg, provider, store, policy, hooks, nil, prompt.RootAssembler{})

	if deps.Instructions == nil {
		t.Fatal("Instructions is nil — turn-0 project instructions / memory index would not assemble")
	}
	if deps.Compactor == nil {
		t.Fatal("Compactor is nil — per-session engine would never compact")
	}
	if _, ok := deps.Compactor.(agent.CascadeCompactor); !ok {
		t.Fatalf("Compactor = %T, want the configured agent.CascadeCompactor (not NewEngine's default)", deps.Compactor)
	}
	if deps.TokenCounter == nil {
		t.Fatal("TokenCounter is nil — compaction trigger would have no counter")
	}
	if deps.CommandExpander == nil {
		t.Fatal("CommandExpander is nil")
	}
	if _, isNoop := deps.CommandExpander.(prompt.NoopExpander); isNoop {
		t.Fatal("CommandExpander is the NoopExpander — slash commands would silently stop expanding")
	}
	if deps.Store == nil {
		t.Fatal("Store is nil — weaker durability")
	}
	if deps.Sink == nil {
		t.Fatal("Sink is nil — operator observability would not fire")
	}
	if deps.Policy == nil {
		t.Fatal("Policy is nil")
	}
	if deps.Hooks == nil {
		t.Fatal("Hooks is nil")
	}
	if deps.ContextWindowTokens != defaultContextWindowTokens {
		t.Fatalf("ContextWindowTokens = %d, want %d", deps.ContextWindowTokens, defaultContextWindowTokens)
	}
	if deps.CompactionRatio != defaultCompactionRatio {
		t.Fatalf("CompactionRatio = %v, want %v", deps.CompactionRatio, defaultCompactionRatio)
	}
}

// TestSessionEngineFactoryBuildsUsableEngine asserts the per-session factory builds
// a non-nil engine and a non-nil close even with NO MCP specs reachable (the
// best-effort path), proving it wires through baseEngineDeps. The factory is given
// an empty spec list so it connects nothing (offline — no real MCP server), and we
// assert it still returns a usable engine + close.
func TestSessionEngineFactoryBuildsUsableEngine(t *testing.T) {
	cfg := configWithCollaborators()
	provider := mockllm.New(mockllm.TextTurn("x"))
	store := memstore.New()
	policy := permpolicy.NewPolicy(defaultRules(), nil)
	hooks := hookexec.New(nil)

	reg := &providerRegistry{
		entries:   map[string]providerEntry{providerMock: {id: providerMock, provider: provider, available: true}},
		defaultID: providerMock,
	}
	factory := sessionEngineFactory(cfg, reg, provider, store, policy, hooks, nil, prompt.RootAssembler{}, catalogAssets{})

	// Zero selector + no specs: the per-session engine binds the DEFAULT provider.
	res, err := factory(context.Background(), server.ProviderSelector{}, []mcp.ServerConfig{}, server.ProfileDefault, "")
	if err != nil {
		t.Fatalf("factory: %v", err)
	}
	eng, closeFn := res.Engine, res.Close
	if eng == nil {
		t.Fatal("factory returned a nil engine")
	}
	if closeFn == nil {
		t.Fatal("factory returned a nil close")
	}
	if cerr := closeFn(); cerr != nil {
		t.Fatalf("close: %v", cerr)
	}
}

// twoProviderFactory builds a sessionEngineFactory over a registry with TWO
// DISTINCT mock-backed providers (openai / openrouter), each returning an
// identifying canned reply, so a routing test can assert WHICH provider the
// selected engine bound. The default provider is openai's mock.
func twoProviderFactory(t *testing.T) (server.SessionEngineFactory, *providerRegistry) {
	t.Helper()
	cfg := Config{Model: "default-model"}
	oa := mockllm.New(mockllm.TextTurn("OPENAI-REPLY"))
	or := mockllm.New(mockllm.TextTurn("OPENROUTER-REPLY"))
	reg := &providerRegistry{
		entries: map[string]providerEntry{
			providerOpenAI:     {id: providerOpenAI, provider: oa, available: true},
			providerOpenRouter: {id: providerOpenRouter, provider: or, available: true},
		},
		defaultID: providerOpenAI,
	}
	store := memstore.New()
	policy := permpolicy.NewPolicy(defaultRules(), nil)
	factory := sessionEngineFactory(cfg, reg, oa, store, policy, hookexec.New(nil), nil, prompt.RootAssembler{}, catalogAssets{})
	return factory, reg
}

// runFactoryEngine builds an engine via the factory for sel, drives one turn, and
// returns the terminal text (so a test can assert the bound provider's reply).
func runFactoryEngine(t *testing.T, factory server.SessionEngineFactory, sel server.ProviderSelector) string {
	t.Helper()
	res, err := factory(context.Background(), sel, nil, server.ProfileDefault, "")
	if err != nil {
		t.Fatalf("factory(%+v): %v", sel, err)
	}
	eng, closeFn := res.Engine, res.Close
	defer func() { _ = closeFn() }()
	sess := session.New("s1", session.ModeDefault, "/ws", session.Limits{MaxTurns: 5}, time.Now())
	ws := memfs.NewWorkspace("/ws")
	return drainRun(eng.RunContent(context.Background(), sess, ws, "hi", nil))
}

// TestSessionEngineFactoryUnknownProvider: an unknown/unavailable provider id is a
// loud error wrapping server.ErrInvalidArgument, naming the id — never a silent
// fallback to the default.
func TestSessionEngineFactoryUnknownProvider(t *testing.T) {
	factory, _ := twoProviderFactory(t)
	_, err := factory(context.Background(), server.ProviderSelector{ProviderID: "anthropic"}, nil, server.ProfileDefault, "")
	if err == nil {
		t.Fatal("expected an error for an unknown provider id, got nil")
	}
	if !errors.Is(err, server.ErrInvalidArgument) {
		t.Fatalf("error = %v, want it to wrap server.ErrInvalidArgument", err)
	}
	if !strings.Contains(err.Error(), "anthropic") {
		t.Fatalf("error %q does not name the unknown provider id", err.Error())
	}
}

// TestSessionEngineFactorySelectsProviderDeps: selecting "openrouter" binds the
// OpenRouter provider — NOT the default (openai) — proving the factory routes the
// SELECTED provider's Deps. (Contamination guard at the factory level.)
func TestSessionEngineFactorySelectsProviderDeps(t *testing.T) {
	factory, _ := twoProviderFactory(t)
	if got := runFactoryEngine(t, factory, server.ProviderSelector{ProviderID: providerOpenRouter}); got != "OPENROUTER-REPLY" {
		t.Fatalf("selected openrouter but got %q, want the OpenRouter provider's reply", got)
	}
	// And the default (zero selector) routes to openai.
	if got := runFactoryEngine(t, factory, server.ProviderSelector{}); got != "OPENAI-REPLY" {
		t.Fatalf("zero selector got %q, want the default (openai) provider's reply", got)
	}
}

// TestSessionEngineFactoryModelPassthrough: an unknown-to-the-catalog model on an
// available provider flows through THE FACTORY verbatim — sel.ModelID reaches the
// provider's LLMRequest.Model unchanged (a regression that dropped or overrode
// sel.ModelID must fail this). It drives the factory end to end and captures the
// request the engine sends via a mock request observer (NOT a direct
// engineDepsForProvider call — that would only prove the helper, not that the
// factory forwards the selector).
func TestSessionEngineFactoryModelPassthrough(t *testing.T) {
	cfg := Config{Model: "default-model"}
	var gotModel string
	// The mock records the model that reached the provider on the run's LLMRequest.
	oa := mockllm.NewWith(
		[]mockllm.Option{mockllm.WithRequestObserver(func(req port.LLMRequest) { gotModel = req.Model })},
		mockllm.TextTurn("ok"),
	)
	reg := &providerRegistry{
		entries:   map[string]providerEntry{providerOpenAI: {id: providerOpenAI, provider: oa, available: true}},
		defaultID: providerOpenAI,
	}
	store := memstore.New()
	policy := permpolicy.NewPolicy(defaultRules(), nil)
	factory := sessionEngineFactory(cfg, reg, oa, store, policy, hookexec.New(nil), nil, prompt.RootAssembler{}, catalogAssets{})

	const unknownModel = "gpt-5-preview-not-in-catalog"
	res, err := factory(context.Background(),
		server.ProviderSelector{ProviderID: providerOpenAI, ModelID: unknownModel}, nil, server.ProfileDefault, "")
	if err != nil {
		t.Fatalf("factory: %v", err)
	}
	eng, closeFn := res.Engine, res.Close
	defer func() { _ = closeFn() }()
	sess := session.New("s1", session.ModeDefault, "/ws", session.Limits{MaxTurns: 5}, time.Now())
	drainRun(eng.RunContent(context.Background(), sess, memfs.NewWorkspace("/ws"), "hi", nil))

	if gotModel != unknownModel {
		t.Fatalf("provider saw model %q, want the verbatim passthrough %q (factory dropped/overrode sel.ModelID)", gotModel, unknownModel)
	}
}

// TestSessionEngineFactoryContextWindowFromCatalog: a selected model with a KNOWN
// catalog context limit produces that ContextWindowTokens on the built engine, and a
// passthrough (uncatalogued) model falls back to the 128k default — proving the
// compaction trigger agrees with the ListModels-advertised context_limit (Medium #2).
func TestSessionEngineFactoryContextWindowFromCatalog(t *testing.T) {
	// Pick a real catalogued openai model + its catalog context limit, so the test
	// asserts against the SAME data ListModels projects.
	cat := providercatalog.Default()
	p, ok := cat.Provider(providerOpenAI)
	if !ok {
		t.Fatal("openai not in catalog")
	}
	var knownModel string
	var knownLimit int
	for _, m := range p.Models() {
		if m.ContextLimit() > 0 {
			knownModel, knownLimit = m.ID(), m.ContextLimit()
			break
		}
	}
	if knownModel == "" {
		t.Skip("no catalogued openai model with a positive context limit")
	}

	factory, _ := twoProviderFactory(t)

	// Known model ⇒ the engine's window is the catalog limit.
	res, err := factory(context.Background(),
		server.ProviderSelector{ProviderID: providerOpenAI, ModelID: knownModel}, nil, server.ProfileDefault, "")
	if err != nil {
		t.Fatalf("factory(known): %v", err)
	}
	eng, closeFn := res.Engine, res.Close
	defer func() { _ = closeFn() }()
	if got := eng.ContextWindow(); got != knownLimit {
		t.Fatalf("ContextWindowTokens = %d, want the catalog limit %d for %q", got, knownLimit, knownModel)
	}

	// Passthrough (uncatalogued) model ⇒ the 128k default fallback.
	resPT, err := factory(context.Background(),
		server.ProviderSelector{ProviderID: providerOpenAI, ModelID: "totally-made-up-model"}, nil, server.ProfileDefault, "")
	if err != nil {
		t.Fatalf("factory(passthrough): %v", err)
	}
	engPT, closePT := resPT.Engine, resPT.Close
	defer func() { _ = closePT() }()
	if got := engPT.ContextWindow(); got != defaultContextWindowTokens {
		t.Fatalf("passthrough ContextWindowTokens = %d, want the %d default fallback", got, defaultContextWindowTokens)
	}
}

// TestSessionEngineFactorySelectorMCPCoexist: a non-default selector AND a
// (best-effort, offline) spec list resolve in ONE factory call — the engine binds
// the SELECTED provider and the call returns a usable engine + close (the MCP path
// is best-effort, so an unreachable server still yields a core-tool engine). Proves
// sel and specs are orthogonal inputs to one engine over one catalog.
func TestSessionEngineFactorySelectorMCPCoexist(t *testing.T) {
	factory, _ := twoProviderFactory(t)
	// One spec to an unreachable URL: best-effort connect logs-and-skips, the engine
	// is still built (core tools only) and bound to the SELECTED provider.
	res, err := factory(context.Background(),
		server.ProviderSelector{ProviderID: providerOpenRouter},
		[]mcp.ServerConfig{{Name: "docs", URL: "https://127.0.0.1:0/mcp"}}, server.ProfileDefault, "")
	if err != nil {
		t.Fatalf("factory(sel+specs): %v", err)
	}
	eng, closeFn := res.Engine, res.Close
	if eng == nil || closeFn == nil {
		t.Fatal("factory returned nil engine/close for sel+specs")
	}
	defer func() { _ = closeFn() }()
	// The bound provider is the SELECTED one (openrouter), not the default.
	sess := session.New("s1", session.ModeDefault, "/ws", session.Limits{MaxTurns: 5}, time.Now())
	if got := drainRun(eng.RunContent(context.Background(), sess, memfs.NewWorkspace("/ws"), "hi", nil)); got != "OPENROUTER-REPLY" {
		t.Fatalf("sel+specs turn routed to %q, want the selected (openrouter) provider's reply", got)
	}
}
