package app

import (
	"sync/atomic"
)

// liveMetaStore is the COMPOSITION-OWNED, race-free cache of per-(provider,model)
// live model metadata that feeds the request-path RESOLVERS — the output ceiling
// (max_tokens), the context window, the input modalities, and the Anthropic
// thinking descriptor. It is the broadening seam: today the live model snapshot
// reaches ONLY the ListModels picker (Service.SetModels); this store carries the
// SAME modelEntry list to the resolvers so a live value can improve a request, not
// just a cosmetic picker row.
//
// # Lifecycle (t=0 catalog seed → live swap)
//
// Build SEEDS the store from the embedded catalog (seedFromCatalog) BEFORE any
// network call, so every resolver has a correct-enough value at t=0 (the same
// guarantee the picker relies on). The SAME one-shot background refresh that calls
// Service.SetModels also calls store.Swap with the merged live list, so the picker
// proto slice and the meta store project from the ONE modelEntry list per refresh
// and cannot drift. The swap only ever IMPROVES the store; a request that arrives
// before the swap reads the catalog seed.
//
// # Precedence (live-first, catalog floor, per field)
//
// The store holds the WINNING modelEntry per (provider, model): a live success
// REPLACES the catalog row wholesale for that provider (resolveProviderModels
// already enforces live-on-success / catalog-on-failure), and within a row each
// FIELD's live-first-else-catalog-else-default precedence is applied by the small
// helpers below (outputLimitFor / contextWindowFor / modalitiesFor / thinkingFor).
// A live MISS for a model the catalog knows falls back WHOLESALE to the catalog row
// (the seed entry) — live absence NEVER erases the catalog. modalitiesFor really
// exists below and feeds modelCapability's live-first modality input, so the session
// echo / ACP gate now derive image/audio from the SAME live modalities the picker does.
// Note modalities are PRESENCE-keyed, not value-keyed: a PRESENT live entry is
// authoritative even with an EMPTY modality list (treated as text-only, matching the
// picker), unlike the >0/Known scalar fields above — only a true miss falls back.
//
// It is composition-only (held on providerRegistry); it never crosses into a port,
// the domain, the agent, or the server adapter. Adapters receive CLOSURES over it
// (e.g. the max-tokens / thinking resolvers), never the store itself.
type liveMetaStore struct {
	// models is map[providerID]map[modelID]modelEntry, behind an atomic.Pointer for
	// the same lock-free swap the picker uses. Never mutated in place: a refresh
	// builds a fresh map and Swaps the pointer.
	models atomic.Pointer[map[string]map[string]modelEntry]
}

// newLiveMetaStore returns an empty store. Callers seed it (seedFromCatalog) before
// the first resolver read so a lookup is never a nil-map miss.
func newLiveMetaStore() *liveMetaStore {
	s := &liveMetaStore{}
	empty := map[string]map[string]modelEntry{}
	s.models.Store(&empty)
	return s
}

// seedFromCatalog populates the store from the embedded catalog for the supplied
// available providers — the t=0 floor, computed at Build with NO network. It mirrors
// modelSnapshot's per-provider embedded projection so the seed and the picker seed
// share the same source. The mock provider is skipped (no selectable models).
func (s *liveMetaStore) seedFromCatalog(pids []string) {
	if s == nil {
		return
	}
	next := make(map[string]map[string]modelEntry, len(pids))
	for _, pid := range pids {
		if pid == providerMock {
			continue
		}
		put(next, pid, embeddedModels(pid))
	}
	s.models.Store(&next)
}

// Swap atomically REPLACES the whole store with the per-provider model lists from a
// refresh. It is the resolver-side twin of Service.SetModels: the background refresh
// projects the SAME []modelEntry per provider into both sinks. A nil/empty map is
// stored as-is (defensive: a refresh that produced nothing leaves the resolvers on
// whatever the seed last held only if the caller declines to Swap — Build only Swaps
// the merged result, which is never below the catalog floor).
func (s *liveMetaStore) Swap(byProvider map[string][]modelEntry) {
	if s == nil {
		return // a hand-built test registry may carry no meta store (resolvers stay nil-safe)
	}
	next := make(map[string]map[string]modelEntry, len(byProvider))
	for pid, list := range byProvider {
		if pid == providerMock {
			continue
		}
		put(next, pid, list)
	}
	s.models.Store(&next)
}

// put indexes a provider's []modelEntry by model id into the destination map. A
// later duplicate id wins (the live list is authoritative for its own ordering).
func put(dst map[string]map[string]modelEntry, pid string, list []modelEntry) {
	if len(list) == 0 {
		return
	}
	byID := make(map[string]modelEntry, len(list))
	for _, m := range list {
		if m.ID == "" {
			continue
		}
		byID[m.ID] = m
	}
	if len(byID) > 0 {
		dst[pid] = byID
	}
}

// lookup returns the winning modelEntry for (providerID, modelID) and whether it was
// found. A nil store or an unseeded/empty map is a safe miss (the caller falls back
// to the catalog). It is the single read path all the per-field helpers share.
func (s *liveMetaStore) lookup(providerID, modelID string) (modelEntry, bool) {
	if s == nil || providerID == "" || modelID == "" {
		return modelEntry{}, false
	}
	cur := s.models.Load()
	if cur == nil {
		return modelEntry{}, false
	}
	byID, ok := (*cur)[providerID]
	if !ok {
		return modelEntry{}, false
	}
	m, ok := byID[modelID]
	return m, ok
}

// Upper bounds on LIVE-sourced metadata before it drives the request hot path. The
// >0 check is the lower floor; these are the symmetric UPPER clamp (Security LOW):
// a hostile / MITM'd / buggy live endpoint must not flow a huge value verbatim into
// max_tokens (a too-high ceiling 400s and inflates cost) or into the compaction
// window (a gigantic window effectively DISABLES compaction). The caps are chosen
// comfortably ABOVE any real Claude limit (today: context 1M, output 128k), so a
// legitimate live value is never clipped; only an absurd one is. The catalog and
// default paths are already bounded — this bounds ONLY the live path.
const (
	maxLiveContextLimit = 2_000_000 // ~2× the largest real Claude context window
	maxLiveOutputLimit  = 512_000   // ~4× the largest real Claude output ceiling
)

// clampLive bounds a positive live value to [1, limit]. A value already in range is
// returned unchanged; an absurd value is clipped to limit (never to 0 — that would
// drop to the catalog floor, masking the clamp). The caller has already established
// v > 0, so this only ever lowers an over-cap value.
func clampLive(v, limit int) int {
	if v > limit {
		return limit
	}
	return v
}

// --- Per-field live-first helpers (live-when-present-&->0 ELSE catalog ELSE 0) ---
//
// Each helper is the resolver call site's replacement for a bare catalog read. The
// precedence is PER FIELD: take the live value only when the store has the model AND
// the field is present (non-zero / Known); otherwise fall back to the catalog floor
// (catalogContextWindow / anthropicOutputLimit), then to the conservative default
// the downstream consumer already applies (engineDepsForProvider's 128k / the
// adapter's defaultMaxTokens). Live absence never erases the catalog. A live value
// present & >0 is additionally UPPER-clamped (clampLive) before it leaves the helper.

// outputLimitFor resolves a model's max_tokens output ceiling: live (>0) else the
// catalogued ceiling (anthropicOutputLimit for anthropic; 0 otherwise — only
// anthropic needs a per-model ceiling resolver today). 0 ⇒ the adapter's
// conservative defaultMaxTokens floor. providerID is a parameter for symmetry with
// contextWindowFor/thinkingFor (one resolver seam, one shape) and so a future keyed
// provider that needs a per-model ceiling threads through here unchanged.
//
//nolint:unparam // providerID is the generic resolver seam; only anthropic calls it today.
func (s *liveMetaStore) outputLimitFor(providerID, modelID string) int {
	if m, ok := s.lookup(providerID, modelID); ok && m.OutputLimit > 0 {
		return clampLive(m.OutputLimit, maxLiveOutputLimit)
	}
	if providerID == providerAnthropic {
		return anthropicOutputLimit(modelID)
	}
	return 0
}

// contextWindowFor resolves a model's input/context window: live (>0) else the
// catalogued window (catalogContextWindow). 0 ⇒ engineDepsForProvider's 128k floor.
func (s *liveMetaStore) contextWindowFor(providerID, modelID string) int {
	if m, ok := s.lookup(providerID, modelID); ok && m.ContextLimit > 0 {
		return clampLive(m.ContextLimit, maxLiveContextLimit)
	}
	return catalogContextWindow(providerID, modelID)
}

// modalitiesFor resolves a model's input modalities from the live store. A PRESENT
// live entry is AUTHORITATIVE — found=true — EVEN when its modality list is empty/nil:
// a live source that lists the model but omits architecture.input_modalities is
// asserting "no declared modalities" (text-only), exactly as the picker treats it
// (hasImageModality(empty)=false). Returning found=false here for present-but-empty
// would let modelCapability fall through to the catalog floor and re-introduce
// picker≠echo divergence (picker=false via the empty list, echo=true via a catalogued
// image row). Only a true MISS (no entry, or a nil/unseeded store) returns (nil,false),
// so the caller falls through to the catalog floor then the adapter-only passthrough.
// nil-safe via lookup. This is the modality twin of contextWindowFor — the seam that
// makes the session echo and the picker (projectModelEntry, which reads the SAME
// modelEntry.InputModalities) derive image/audio from ONE live-first source.
func (s *liveMetaStore) modalitiesFor(providerID, modelID string) (mods []string, found bool) {
	if m, ok := s.lookup(providerID, modelID); ok {
		return m.InputModalities, true
	}
	return nil, false
}

// thinkingFor resolves a model's thinking descriptor from the live store. It returns
// known=false (so the adapter falls back to its prefix matrix) whenever the model is
// absent from the live store OR its entry was not populated by a live source
// (Known=false). This is the seam WithThinkingResolver consumes. providerID is the
// generic resolver seam (only anthropic has a thinking matrix today).
//
//nolint:unparam // providerID is the generic resolver seam; only anthropic calls it today.
func (s *liveMetaStore) thinkingFor(providerID, modelID string) (adaptive, enabled, known bool) {
	if m, ok := s.lookup(providerID, modelID); ok && m.Thinking.Known {
		return m.Thinking.Adaptive, m.Thinking.Enabled, true
	}
	return false, false, false
}
