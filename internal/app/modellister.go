package app

import (
	"context"
	"sort"
	"sync"
	"time"

	mecatlv1 "github.com/stacklok/mecatl/contracts/gen/go/mecatl/v1"
	"github.com/stacklok/mecatl/engine/port"
	"github.com/stacklok/mecatl/internal/adapter/anthropic"
	"github.com/stacklok/mecatl/internal/adapter/openrouter"
	"github.com/stacklok/mecatl/internal/adapter/providercatalog"
	"github.com/stacklok/mecatl/internal/adapter/server"
)

// liveModelRefreshTimeout bounds the whole background refresh (all providers'
// fetches combined). A slow/hung provider endpoint must not keep the refresh
// goroutine — or, in the synchronous test path, Build — alive indefinitely.
const liveModelRefreshTimeout = 10 * time.Second

// modelSwapper is the narrow seam the live-model refresh writes through: the
// service's SetModels. Keeping it an interface (satisfied by *server.Service)
// keeps the refresh testable without a full service.
type modelSwapper interface {
	SetModels([]*mecatlv1.ModelInfo)
}

// startLiveModelRefresh kicks the ONE-SHOT background live-catalog refresh and
// returns a close func that cancels it (wired into Build's closeAll). When NO
// available provider has a lister it is a NO-OP (returns a no-op closer; no
// goroutine, no ctx) — so a mock/openai-only deployment spawns nothing. When sync
// is true (a test seam) the refresh runs INLINE before returning, so an offline
// e2e can assert the swapped snapshot deterministically without sleeps.
func startLiveModelRefresh(d port.Diagnostics, reg *providerRegistry, swap modelSwapper, runSync bool) func() {
	if reg == nil || !anyProviderHasLister(reg) {
		return func() {} // nothing to refresh
	}
	if runSync {
		ctx, cancel := context.WithTimeout(context.Background(), liveModelRefreshTimeout)
		defer cancel()
		models, byProvider := liveModelSnapshot(ctx, d, reg)
		swap.SetModels(models)
		reg.meta.Swap(byProvider)
		return func() {}
	}
	ctx, cancel := context.WithCancel(context.Background())
	var wg sync.WaitGroup
	wg.Add(1)
	go func() {
		defer wg.Done()
		fetchCtx, fetchCancel := context.WithTimeout(ctx, liveModelRefreshTimeout)
		defer fetchCancel()
		models, byProvider := liveModelSnapshot(fetchCtx, d, reg)
		// If the refresh ctx was cancelled (the closer ran — a shutdown — before the
		// fetch finished), the fetch was interrupted and its result is untrustworthy
		// (the embedded floor at best), so DO NOT overwrite the seed. Only swap when the
		// fetch ran to completion uninterrupted. On a normal run the goroutine reaches
		// here, ctx.Err() is nil, and the swap lands; a later closer just wg.Wait()s.
		if ctx.Err() != nil {
			return
		}
		// Both sinks are fed from the ONE modelEntry list per provider, so the picker
		// proto slice and the resolver-feeding meta store cannot drift.
		swap.SetModels(models)
		reg.meta.Swap(byProvider)
	}()
	return func() {
		cancel()
		wg.Wait()
	}
}

// anyProviderHasLister reports whether at least one AVAILABLE provider carries a
// live lister — the gate that decides whether the background refresh runs at all.
func anyProviderHasLister(reg *providerRegistry) bool {
	for _, pid := range reg.Available() {
		if entry, ok := reg.Lookup(pid); ok && entry.lister != nil {
			return true
		}
	}
	return false
}

// (compile-time) *server.Service satisfies modelSwapper.
var _ modelSwapper = (*server.Service)(nil)

// modelEntry is the NEUTRAL, composition-local, SOURCE-AGNOSTIC description of one
// selectable model — produced EITHER by a LIVE fetch (modelLister) OR by projecting
// the embedded providercatalog (embeddedModels). Both sources fold into this one
// type so the seed and the refresh-floor share ONE projection (projectModelEntry)
// and cannot drift. It carries exactly what the proto projection + the capability
// intersection consume: nothing provider-private, no key, no URL. It NEVER leaves
// internal/app (the server adapter receives only []*mecatlv1.ModelInfo).
//
// InputModalities is the authoritative modality list for THIS model — the single
// source the image capability is derived from (via hasImageModality), so a live
// model and an embedded model are tested by the SAME predicate.
type modelEntry struct {
	ID              string
	DisplayName     string
	ContextLimit    int
	OutputLimit     int // max_tokens output ceiling (0 = unknown ⇒ catalog/default floor)
	InputModalities []string
	Reasoning       bool
	ToolCall        bool
	Thinking        thinkingDescriptor // Anthropic-only; zero value = unknown ⇒ adapter prefix floor
}

// thinkingDescriptor is a NEUTRAL, source-agnostic projection of a model's
// extended-thinking capability — the live replacement for the adapter's hardcoded
// adaptive/enabled/none prefix lists. The zero value (Known=false) means "unknown",
// so the anthropic adapter falls back to its embedded prefix matrix (the offline
// floor). Only the live Anthropic lister populates it (Capabilities.Thinking.Types);
// every other source leaves it zero, which costs nothing and changes no behaviour.
type thinkingDescriptor struct {
	Known    bool // true only when a live source populated it
	Adaptive bool // Capabilities.Thinking.Types.adaptive.supported
	Enabled  bool // Capabilities.Thinking.Types.enabled.supported (manual)
}

// modelLister is the OPTIONAL live-catalog capability a provider may expose. It is
// a COMPOSITION-LOCAL interface (NOT a port) for the same reason providerRegistry
// is composition-only: it has a SINGLE consumer (liveModelSnapshot), and the
// agent/domain/server never enumerate a catalog. A provider opts in by having its
// adapter satisfy this interface and by composition setting providerEntry.lister
// at registry-build time — that one assignment is the ENTIRE opt-in; the
// merge/snapshot/registry plumbing is unchanged.
//
// ListModels is read-only, takes a ctx for timeout/cancel, and is FAIL-SAFE to the
// caller: an error means the caller falls back to the embedded catalog for that
// provider (never empty, never a crash).
type modelLister interface {
	ListModels(ctx context.Context) ([]modelEntry, error)
}

// openRouterLister adapts the *openrouter.Lister (which returns its OWN package
// type, []openrouter.Model — no import cycle) to the composition modelLister
// interface by mapping each openrouter.Model → modelEntry. This is the one place the
// adapter's type is mapped to the composition-neutral type; the adapter never
// imports internal/app.
type openRouterLister struct {
	inner *openrouter.Lister
}

func (l openRouterLister) ListModels(ctx context.Context) ([]modelEntry, error) {
	raw, err := l.inner.ListModels(ctx)
	if err != nil {
		return nil, err
	}
	out := make([]modelEntry, 0, len(raw))
	for _, m := range raw {
		out = append(out, modelEntry{
			ID:           m.ID,
			DisplayName:  m.DisplayName,
			ContextLimit: m.ContextLimit,
			// top_provider.max_completion_tokens (Slice C). CAPTURED into the meta store,
			// but currently OFF the OpenRouter request path: OpenRouter rides the openai
			// Responses adapter, which has no per-model max_tokens resolver today (only the
			// native anthropic adapter does). The composition UPPER clamp (clampLive in
			// livemeta.go) gates this value, so a future OpenRouter max_tokens consumer
			// cannot reintroduce the unbounded-live risk.
			OutputLimit:     m.OutputLimit,
			InputModalities: m.InputModalities,
			Reasoning:       m.Reasoning,
			ToolCall:        m.ToolCall,
			// Thinking stays zero: OpenRouter exposes only a coarse `reasoning` flag, not
			// the adaptive/enabled thinking-types matrix — so a model routed via OpenRouter
			// defers to the adapter's prefix floor (it is not the native anthropic provider).
		})
	}
	return out, nil
}

// anthropicLister adapts the *anthropic.Lister (which returns its OWN package type,
// []anthropic.Model — no import cycle) to the composition modelLister interface by
// mapping each anthropic.Model → modelEntry, INCLUDING the live thinking descriptor
// (the live replacement for the adapter's prefix matrix), the output ceiling, the
// context window, and image. This is the one place the adapter's type is mapped to
// the composition-neutral type; the adapter never imports internal/app.
type anthropicLister struct {
	inner *anthropic.Lister
}

func (l anthropicLister) ListModels(ctx context.Context) ([]modelEntry, error) {
	raw, err := l.inner.ListModels(ctx)
	if err != nil {
		return nil, err
	}
	out := make([]modelEntry, 0, len(raw))
	for _, m := range raw {
		var mods []string
		if m.Image {
			mods = []string{"image"} // feed the SHARED hasImageModality predicate
		}
		out = append(out, modelEntry{
			ID:              m.ID,
			DisplayName:     m.DisplayName,
			ContextLimit:    m.ContextLimit,
			OutputLimit:     m.OutputLimit,
			InputModalities: mods,
			Reasoning:       m.Thinking.Adaptive || m.Thinking.Enabled,
			ToolCall:        true, // every current Claude model supports tool use
			Thinking: thinkingDescriptor{
				Known:    true, // a live anthropic source populated this
				Adaptive: m.Thinking.Adaptive,
				Enabled:  m.Thinking.Enabled,
			},
		})
	}
	return out, nil
}

// embeddedModels projects the embedded providercatalog subset for a provider into
// []modelEntry — the FALLBACK FLOOR used by BOTH the synchronous seed
// (modelSnapshot) and the live refresh when a provider has no lister or its fetch
// fails/empties. Returns nil for an uncatalogued provider (an honest miss). Since
// both the seed and the floor go through THIS one projection + projectModelEntry,
// they cannot drift.
func embeddedModels(providerID string) []modelEntry {
	p, ok := providercatalog.Default().Provider(providerID)
	if !ok {
		return nil
	}
	out := make([]modelEntry, 0, len(p.Models()))
	for _, m := range p.Models() {
		out = append(out, modelEntry{
			ID:              m.ID(),
			DisplayName:     m.Name(),
			ContextLimit:    m.ContextLimit(),
			OutputLimit:     m.OutputLimit(),
			InputModalities: m.InputModalities(),
			Reasoning:       m.SupportsReasoning(),
			ToolCall:        m.SupportsToolCall(),
			// Thinking stays zero (Known=false): the embedded catalog has no thinking-
			// types bit, so an embedded/seed model defers to the adapter's prefix floor.
		})
	}
	return out
}

// projectModelEntry is the SINGLE projection of a (provider, modelEntry) into the
// proto ModelInfo — the ONE place the image intersection, display-name fallback,
// and field mapping live, shared by the seed (modelSnapshot) and the live refresh
// (liveModelSnapshot) so the two cannot hand-sync-drift. Image is adapterCaps.Image
// AND hasImageModality(modalities) — the adapter authority on transmit ∩ THIS
// model's modalities, via the shared predicate, whether the modalities came from
// live metadata or the embedded catalog.
func projectModelEntry(reg *providerRegistry, providerID string, m modelEntry) *mecatlv1.ModelInfo {
	name := m.DisplayName
	if name == "" {
		name = m.ID // display falls back to the id
	}
	return &mecatlv1.ModelInfo{
		Id:           m.ID,
		ProviderId:   providerID,
		DisplayName:  name,
		Image:        modelAdapterCaps(reg, providerID).Image && hasImageModality(m.InputModalities),
		Reasoning:    m.Reasoning,
		ContextLimit: int64(m.ContextLimit),
	}
}

// sortModelInfos orders a snapshot by (provider_id, id) for a deterministic picker.
// Both the seed and the live refresh sort through this one function.
func sortModelInfos(out []*mecatlv1.ModelInfo) {
	sort.Slice(out, func(i, j int) bool {
		if out[i].GetProviderId() != out[j].GetProviderId() {
			return out[i].GetProviderId() < out[j].GetProviderId()
		}
		return out[i].GetId() < out[j].GetId()
	})
}

// liveModelSnapshot builds the selectable-model inventory the way modelSnapshot
// does (availability-gated to reg.Available(), secret-free, sorted by
// provider_id,id), but consults each available provider's LIVE catalog when it has
// a lister. Merge semantics: live REPLACES the embedded subset for a provider on
// SUCCESS+NON-EMPTY; on ANY error/timeout/empty (or no lister) it falls back to the
// embedded subset (the fallback floor). It never returns a fabricated entry and
// never crashes.
//
// The fetch is attempted at most once per provider here; the CALLER (the
// background refresh in Build) owns concurrency/lifecycle. This function is pure
// w.r.t. composition state — it reads the registry and the network (through the
// listers) and returns BOTH the fresh proto slice (the picker sink) AND the
// per-provider []modelEntry map (the resolver-feeding meta-store sink). Both sinks
// project from the SAME modelEntry list per provider so the picker and the resolvers
// cannot drift. It REUSES projectModelEntry + sortModelInfos so the live floor and
// the embedded seed cannot drift.
func liveModelSnapshot(ctx context.Context, d port.Diagnostics, reg *providerRegistry) ([]*mecatlv1.ModelInfo, map[string][]modelEntry) {
	if reg == nil {
		return nil, nil
	}
	var out []*mecatlv1.ModelInfo
	byProvider := make(map[string][]modelEntry)
	for _, pid := range reg.Available() { // available (keyed) providers ONLY
		if pid == providerMock {
			continue // the mock never advertises selectable models
		}
		entries := resolveProviderModels(ctx, d, reg, pid)
		byProvider[pid] = entries
		for _, m := range entries {
			out = append(out, projectModelEntry(reg, pid, m))
		}
	}
	sortModelInfos(out)
	return out, byProvider
}

// resolveProviderModels returns the per-provider model list applying the merge +
// fail-safe rules: live REPLACES embedded on success+non-empty, else embedded
// fallback (with a single Warn on a live failure).
func resolveProviderModels(ctx context.Context, d port.Diagnostics, reg *providerRegistry, pid string) []modelEntry {
	entry, ok := reg.Lookup(pid)
	if !ok || entry.lister == nil {
		return embeddedModels(pid) // no lister: embedded floor, exactly as today
	}
	live, err := entry.lister.ListModels(ctx)
	if err != nil {
		d.Log(ctx, port.LevelWarn, "live model fetch failed, using embedded catalog", "provider", pid, "err", err)
		return embeddedModels(pid)
	}
	if len(live) == 0 {
		// An empty live list is never shown — the embedded floor always wins over
		// nothing (a transient upstream blip must not blank the picker).
		d.Log(ctx, port.LevelWarn, "live model fetch returned no models, using embedded catalog", "provider", pid)
		return embeddedModels(pid)
	}
	return live
}
