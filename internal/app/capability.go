package app

import (
	"github.com/stacklok/mecatl/internal/adapter/providercatalog"
	"github.com/stacklok/mecatl/internal/port"
)

// modelCapability is the SINGLE SOURCE of a (provider, model)'s true input
// capability: the INTERSECTION of the catalog's per-model modalities and the
// wired adapter's port.ProviderCapabilities. The adapter is the AUTHORITY on what
// it can actually TRANSMIT; the catalog is the authority on what the model
// ACCEPTS. The AND of the two is the honest truth, and the only honest value to
// advertise. It lives HERE, in composition (internal/app) — the only layer that
// holds BOTH inputs (the catalog via providercatalog.Default() and the per-provider
// adapter via reg.Lookup(id).provider.Capabilities()).
//
// The result is a NEUTRAL port.ProviderCapabilities: neither the catalog nor the
// registry type crosses into the server/acp adapters — they receive this computed
// value only. This ONE value feeds three sinks so they CANNOT disagree:
//
//	(a) ModelInfo.Image in modelSnapshot (ListModels),
//	(b) the CreateSessionResponse.session_capabilities echo (per-session), and
//	(c) the ACP gate via Service.ProviderCapabilities() (default caps).
//
// P1's per-model divergence (e.g. native Anthropic image vs no-image, per model)
// is a PURE DATA change here: a new registry entry whose adapter Capabilities()
// reports its real transmit ability, plus catalog rows whose inputModalities differ
// per model. The intersection formula is unchanged; no server/acp/proto edit. That
// is the whole point of locating the AND in composition.
//
// Logic (all fail-safe toward text-only):
//   - adapterCaps = reg.Lookup(providerID).provider.Capabilities(). An
//     unknown/unavailable provider (or a nil registry) yields the zero value
//     (text-only) — a provider we cannot reach transmits nothing.
//   - catalog modalities: from providercatalog for (providerID, modelID). An
//     UNCATALOGUED or EMPTY modelID falls back to ADAPTER-ONLY caps (a passthrough
//     model: trust the adapter, the catalog is simply silent — do NOT zero it, or
//     every passthrough/uncatalogued model would lose image).
//   - Image = adapterCaps.Image AND catalog-image; Audio = adapterCaps.Audio AND
//     catalog-audio. The catalog carries NO audio field today, so the catalog-audio
//     term is false and the AND is false regardless — Audio stays effectively
//     adapter-driven (and the P0 adapter is Audio:false). EmbeddedContext is
//     adapter-driven (the catalog has no opinion).
//
// Reasoning is intentionally NOT intersected here: it is a ModelInfo field, not a
// port.ProviderCapabilities bit, and there is no adapter "can replay reasoning"
// authority bit in P0. ModelInfo.reasoning stays catalog-sourced. If P1 wants
// reasoning intersected, add the adapter bit then (anti-speculative).
func modelCapability(reg *providerRegistry, providerID, modelID string) port.ProviderCapabilities {
	var adapterCaps port.ProviderCapabilities
	if reg != nil {
		if entry, ok := reg.Lookup(providerID); ok && entry.provider != nil {
			adapterCaps = entry.provider.Capabilities()
		}
	}

	catImage, catAudio, catalogued := catalogModalities(providerID, modelID)
	if !catalogued {
		// Passthrough / uncatalogued model: the catalog is silent, so trust the
		// adapter alone. Zeroing here would strip image from every uncatalogued model.
		return adapterCaps
	}

	return port.ProviderCapabilities{
		Image:           adapterCaps.Image && catImage,
		Audio:           adapterCaps.Audio && catAudio,
		EmbeddedContext: adapterCaps.EmbeddedContext,
	}
}

// catalogModalities reports the catalog's per-model input modalities (image,
// audio) for (providerID, modelID) and whether the (provider, model) pair was
// found in the catalog at all. A miss (unknown provider, uncatalogued model, or an
// empty modelID) returns (false, false, false) so the caller falls back to
// adapter-only caps — a passthrough model is NOT a capability denial. The catalog
// carries no explicit audio modality today, so catAudio derives from the raw
// input-modality list ("audio") for forward-compatibility (false in the P0 data).
func catalogModalities(providerID, modelID string) (image, audio, found bool) {
	if providerID == "" || modelID == "" {
		return false, false, false
	}
	p, ok := providercatalog.Default().Provider(providerID)
	if !ok {
		return false, false, false
	}
	for _, m := range p.Models() {
		if m.ID() != modelID {
			continue
		}
		// Image derives from the catalog's OWN accessor (SupportsImageInput) — the
		// SAME predicate ListModels reads — so the session echo and ListModels
		// provably agree on a model's image-ness (the anti-divergence guarantee rests
		// on ONE function, not two copies of the "is image among inputModalities"
		// test). Audio has no catalog accessor (the catalog carries no audio field
		// today), so it derives from the raw modality list for forward-compatibility;
		// it is false in the P0 data, so the AND in modelCapability is false regardless.
		image = m.SupportsImageInput()
		for _, mod := range m.InputModalities() {
			if mod == "audio" {
				audio = true
			}
		}
		return image, audio, true
	}
	return false, false, false
}
