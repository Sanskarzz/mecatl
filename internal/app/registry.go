package app

import (
	"errors"
	"log/slog"
	"slices"
	"sort"

	"github.com/stacklok/mecatl/internal/adapter/llmresilience"
	"github.com/stacklok/mecatl/internal/adapter/mockllm"
	"github.com/stacklok/mecatl/internal/adapter/openai"
	"github.com/stacklok/mecatl/internal/adapter/providercatalog"
	"github.com/stacklok/mecatl/internal/port"
)

// Provider id strings. These are WIRE-STABLE once they reach the wire (S3's
// CreateSession provider_id field): they MUST match models.dev's provider ids
// so the S2 catalog join is a direct key lookup. Lowercase, never localized.
const (
	providerOpenAI     = "openai"
	providerOpenRouter = "openrouter"
	// providerMock is the synthetic offline provider id used only when UseMock is
	// set. It never reaches the wire as a selectable provider; it exists so the
	// registry has exactly one entry in offline/smoke-test runs.
	providerMock = "mock"
)

// builtinDefaultModel is the per-provider default model used when the operator did
// NOT pass an explicit --model (cfg.Model == ""). The default must be a VALID id for
// that provider's endpoint: OpenAI's Responses API takes the bare "gpt-5", but
// OpenRouter namespaces every model, so the same model is "openai/gpt-5" there
// (catalogued in providercatalog, so it resolves cleanly through modelCapability). A
// provider absent from this table resolves to "" — the adapter/endpoint default —
// which is the safe, non-presumptuous fallback for a future provider.
var builtinDefaultModel = map[string]string{
	providerOpenAI:     "gpt-5",
	providerOpenRouter: "openai/gpt-5",
}

// openRouterDefaultBaseURL is the OpenRouter Responses-compatible API base URL.
// OpenRouter rides the SAME stateless openai adapter (it speaks the Responses
// API) with this base URL substituted — there is NO separate wire adapter in P0.
const openRouterDefaultBaseURL = "https://openrouter.ai/api/v1"

// envDetector resolves an environment variable to its value. It is the injectable
// seam (defaults to os.Getenv, set in Build) that keeps the registry's
// credential-availability detection OFFLINE-testable, mirroring xdgconfig.OSEnv's
// env-injection idiom already used in this package. A test passes a fake map-backed
// lookup so registry construction never touches the real process environment.
type envDetector func(name string) string

// providerEnvVars returns the ordered list of environment variables whose
// non-empty value makes a provider AVAILABLE (any one suffices). The var NAMES
// come from the embedded models.dev catalog's per-provider env[] (S2 replaced
// S1's inline map with this catalog read, leaving the registry's interface and
// behaviour unchanged).
//
// One mecatl-specific augmentation lives HERE in composition, never in the
// vendored catalog data (which stays honest to upstream — openrouter's env[] is
// ["OPENROUTER_API_KEY"] only): OpenRouter rides the same Responses-speaking
// openai adapter, so by mecatl convention it ALSO accepts an OpenAI key. We
// append OPENAI_API_KEY for the openrouter provider so an operator who set only
// OPENAI_API_KEY (pointing the base URL at OpenRouter) still resolves — exactly
// S1's behaviour. The first non-empty value wins.
//
// A provider id absent from the catalog returns nil (unavailable), exactly like
// an unset env var.
func providerEnvVars(providerID string) []string {
	p, ok := providercatalog.Default().Provider(providerID)
	if !ok {
		return nil
	}
	vars := p.EnvVars()
	if providerID == providerOpenRouter {
		// mecatl convention: OpenRouter also accepts an OpenAI key.
		if !slices.Contains(vars, "OPENAI_API_KEY") {
			vars = append(vars, "OPENAI_API_KEY")
		}
	}
	return vars
}

// providerEntry is one configured provider in the registry: its stable id, the
// constructed (resilience-wrapped) port.LLMProvider, whether its credentials
// resolved from the environment (availability), and its base URL for logging
// only. Composition-only — the agent/server never see this type.
type providerEntry struct {
	id        string           // "openai", "openrouter", or "mock"
	provider  port.LLMProvider // resilience-wrapped, ready to hand to an engine
	available bool             // ≥1 of the provider's env[] keys resolved
	baseURL   string           // for logging/diagnostics ONLY; never wired
}

// providerRegistry holds the N configured providers. It is built once in Build
// from Config + the environment, lives entirely in the composition layer, and is
// the single source of "which provider backs model X / session Y". The agent and
// server never see it — they receive a bare port.LLMProvider selected here.
//
// In S1 the registry's only consumer is buildProvider (which returns the default
// provider so the Build call site is unchanged); per-session multi-provider
// routing is S3.
type providerRegistry struct {
	entries      map[string]providerEntry // keyed by provider id; only AVAILABLE entries
	defaultID    string                   // resolved default provider (precedence: resolveDefaultModel)
	defaultModel string                   // resolved default model for defaultID ("" => adapter/endpoint default)
}

// Lookup returns the entry for id and whether it exists (and is therefore
// available — the registry only holds available entries).
func (r *providerRegistry) Lookup(id string) (providerEntry, bool) {
	e, ok := r.entries[id]
	return e, ok
}

// Available returns the available provider ids, sorted, for ListModels (S3) and
// the zero-keys diagnostic.
func (r *providerRegistry) Available() []string {
	ids := make([]string, 0, len(r.entries))
	for id := range r.entries {
		ids = append(ids, id)
	}
	sort.Strings(ids)
	return ids
}

// Default returns the default provider id, or "" when zero providers are
// available (the zero-keys case).
func (r *providerRegistry) Default() string { return r.defaultID }

// DefaultModel returns the resolved default model for the default provider, or ""
// when no model resolved (the adapter/endpoint default). It is the per-provider
// default-model table value when the operator passed no explicit --model, or the
// explicit cfg.Model otherwise (see resolveDefaultModel).
func (r *providerRegistry) DefaultModel() string { return r.defaultModel }

// errNoProvider is the named, actionable zero-keys error: when no provider's
// credentials resolved AND the mock is not selected, Build cannot serve a useful
// engine. It names BOTH env vars and the offline escape hatches so first-run is
// self-explanatory (S4's picker renders the same copy as an empty-picker note).
var errNoProvider = errors.New(
	"no LLM provider available: set OPENAI_API_KEY or OPENROUTER_API_KEY (run with --openai/--mock for offline)")

// buildProviderRegistry constructs the registry from cfg and the injected env
// detector. It builds (and resilience-wraps) ONLY the available providers — there
// is no point holding an unkeyed provider — keyed by their stable id. When UseMock
// is set it short-circuits to a single synthetic "mock" entry (preserving the
// offline mockllm test/smoke path) regardless of the environment.
//
// It returns errNoProvider when no provider resolves credentials and the mock is
// not selected. Startup logging emits one line per available provider with the
// provider id and base URL ONLY — NEVER the key (CWE-200; S5 verifies).
func buildProviderRegistry(cfg Config, detect envDetector) (*providerRegistry, error) {
	if detect == nil {
		detect = osGetenv
	}

	// UseMock short-circuit: a single synthetic entry, offline, regardless of env.
	if cfg.UseMock {
		slog.Warn("LLM provider: mock (canned, offline) — for smoke tests only")
		mock := mockllm.New(
			mockllm.TextTurn("Mock provider: no real model is configured. Set OPENAI_API_KEY for live use."),
		)
		// The mock is intentionally left UNWRAPPED by resilience: it never fails over
		// the network, so retries/breaker would be inert.
		return &providerRegistry{
			entries:   map[string]providerEntry{providerMock: {id: providerMock, provider: mock, available: true}},
			defaultID: providerMock,
			// The mock ignores the model entirely; carry cfg.Model so an explicit
			// --model is still echoed (snapshots/capabilities) without inventing one.
			defaultModel: cfg.Model,
		}, nil
	}

	entries := make(map[string]providerEntry)

	// openai: AVAILABLE iff a key resolves — from cfg.OpenAIKey (which the cmd layer
	// reads from OPENAI_API_KEY) or, failing that, the OPENAI_API_KEY env var via
	// detect. Availability is purely KEY-DRIVEN; the legacy cfg.UseOpenAI flag is a
	// back-compat selector the cmd layer still sets when a key is present, but it does
	// NOT gate the registry (a key alone suffices — env auto-detection is the S1 model).
	if key := providerKey(cfg.OpenAIKey, providerOpenAI, detect); key != "" {
		entries[providerOpenAI] = newOpenAIEntry(cfg, providerOpenAI, key, cfg.OpenAIBaseURL)
	}

	// openrouter: same stateless openai adapter, OpenRouter base URL, keyed by
	// OPENROUTER_API_KEY (falling back to OPENAI_API_KEY by convention).
	if key := providerKey(cfg.OpenRouterKey, providerOpenRouter, detect); key != "" {
		baseURL := cfg.OpenRouterBaseURL
		if baseURL == "" {
			baseURL = openRouterDefaultBaseURL
		}
		entries[providerOpenRouter] = newOpenAIEntry(cfg, providerOpenRouter, key, baseURL)
	}

	if len(entries) == 0 {
		return nil, errNoProvider
	}

	reg := &providerRegistry{entries: entries}
	reg.defaultID, reg.defaultModel = resolveDefaultModel(cfg, reg)
	return reg, nil
}

// providerKey resolves the credential for a provider: an explicit cfg-supplied key
// wins (it was read from the env by the cmd layer), otherwise the first non-empty
// value among the provider's catalog-driven env vars (providerEnvVars — the
// multi-env-var slice, any one suffices). Returns "" when nothing resolves
// (provider unavailable).
func providerKey(cfgKey, providerID string, detect envDetector) string {
	if cfgKey != "" {
		return cfgKey
	}
	for _, v := range providerEnvVars(providerID) {
		if val := detect(v); val != "" {
			return val
		}
	}
	return ""
}

// newOpenAIEntry constructs a resilience-wrapped openai-adapter provider entry. It
// is shared by the openai and openrouter ids (OpenRouter is the SAME adapter with a
// different base URL + key), so the two cannot drift on resilience wrapping. It logs
// the provider id and base URL ONLY — never the key.
func newOpenAIEntry(cfg Config, id, key, baseURL string) providerEntry {
	slog.Info("LLM provider available", "provider", id, "model", cfg.Model, "base_url", baseURL)
	// Composition-only test seam (S3 e2e): when a providerConstructor is injected,
	// it builds the provider (e.g. a distinct mock per id) instead of the real
	// openai adapter, so the offline multi-provider e2e can hold TWO real provider
	// ids backed by mocks. Production leaves it nil and uses the openai adapter.
	if cfg.providerConstructor != nil {
		return providerEntry{id: id, provider: cfg.providerConstructor(cfg, id, key, baseURL), available: true, baseURL: baseURL}
	}
	opts := []openai.Option{openai.WithAPIKey(key)}
	if baseURL != "" {
		opts = append(opts, openai.WithBaseURL(baseURL))
	}
	var llm port.LLMProvider = openai.New(opts...)
	llm = llmresilience.Wrap(llm, llmresilience.Config{
		MaxAttempts:       cfg.LLMMaxAttempts,
		BaseBackoff:       llmBaseBackoff,
		MaxBackoff:        llmMaxBackoff,
		PerAttemptTimeout: cfg.LLMPerAttemptTimeout,
		BreakerThreshold:  cfg.LLMBreakerThreshold,
		BreakerCooldown:   cfg.LLMBreakerCooldown,
	})
	slog.Info("LLM resilience enabled",
		"provider", id,
		"max_attempts", cfg.LLMMaxAttempts,
		"per_attempt_timeout", cfg.LLMPerAttemptTimeout,
		"breaker_threshold", cfg.LLMBreakerThreshold,
		"breaker_cooldown", cfg.LLMBreakerCooldown)
	return providerEntry{id: id, provider: llm, available: true, baseURL: baseURL}
}

// resolveDefaultModel resolves the default (providerID, modelID) pair from cfg and
// the registry. Precedence (per the Phase-0 brief):
//
//  1. --model flag (cfg.Model) — an EXPLICIT operator override (non-empty): a bare
//     string paired with the default provider id; the catalog-aware "which provider
//     owns this model" resolution is deferred (S-later).
//  2. settings default_model — TODO(S-later): settings plumbing not yet present; do
//     NOT invent the field yet.
//  3. client last-used state — S4 (client-side), not the server.
//  4. per-provider default model — when no explicit --model, the builtinDefaultModel
//     table entry for the default provider (e.g. openai => "gpt-5", openrouter =>
//     "openai/gpt-5"). A provider absent from the table yields "" — the
//     adapter/endpoint default — the safe fallback for a future provider.
//
// The provider preference among available providers is openai first (back-compat
// with the single-provider default), then sorted order.
func resolveDefaultModel(cfg Config, reg *providerRegistry) (providerID, modelID string) {
	defID := preferredDefaultProvider(reg)
	// (1) --model flag: an EXPLICIT override wins, paired with the default provider.
	if cfg.Model != "" {
		return defID, cfg.Model
	}
	// (2) settings default_model: TODO(S-later).
	// (3) client last-used: S4.
	// (4) per-provider default model from the table (no entry => "" => endpoint default).
	return defID, builtinDefaultModel[defID]
}

// preferredDefaultProvider picks the default provider id from the available
// entries: openai when present (preserving the single-provider default), else the
// first id in sorted order, else "" (zero available).
func preferredDefaultProvider(reg *providerRegistry) string {
	if _, ok := reg.entries[providerOpenAI]; ok {
		return providerOpenAI
	}
	avail := reg.Available()
	if len(avail) == 0 {
		return ""
	}
	return avail[0]
}
