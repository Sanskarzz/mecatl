package app

import (
	"sort"

	mecatlv1 "github.com/stacklok/mecatl/contracts/gen/go/mecatl/v1"
	"github.com/stacklok/mecatl/internal/adapter/providercatalog"
)

// modelSnapshot joins the provider registry's AVAILABLE providers to the embedded
// models.dev catalog and projects each model into the proto ModelInfo the server's
// ListModels RPC returns (multi-provider Phase 0, S3). It is the ONLY place the
// registry and the catalog meet the proto type; neither leaks into the server
// adapter (which holds only the projected []*mecatlv1.ModelInfo). It mirrors
// agentdefs.go:agentSnapshot — a pure, I/O-free, nil-safe projection.
//
// It carries PUBLIC metadata only — never a key, env-var name, or base URL
// (CWE-200; the snapshot reads only the catalog's public model fields, never the
// registry entry's credential/baseURL). A provider with no resolved credentials is
// NOT in reg.Available(), so it is omitted entirely — its very availability is
// concealed. The synthetic offline mock provider advertises no selectable models
// (you cannot pick a model against the canned mock), so it is skipped: a UseMock /
// offline-smoke run therefore returns nil here and ServerCapabilities.model_selection
// is false. An available-but-uncatalogued provider contributes no models (an honest
// catalog miss, never a fabricated entry).
//
// Output is sorted by (provider_id, id) for a deterministic picker. A nil registry
// (defensive) or zero available providers yields nil.
func modelSnapshot(reg *providerRegistry) []*mecatlv1.ModelInfo {
	if reg == nil {
		return nil
	}
	cat := providercatalog.Default()
	var out []*mecatlv1.ModelInfo
	for _, pid := range reg.Available() { // available providers ONLY
		if pid == providerMock {
			continue // the mock never advertises selectable models
		}
		p, ok := cat.Provider(pid)
		if !ok {
			continue // available but uncatalogued: no models to advertise (honest miss)
		}
		for _, m := range p.Models() {
			name := m.Name()
			if name == "" {
				name = m.ID() // display falls back to the id when the catalog has no name
			}
			out = append(out, &mecatlv1.ModelInfo{
				Id:           m.ID(),
				ProviderId:   pid,
				DisplayName:  name,
				Image:        m.SupportsImageInput(),
				Reasoning:    m.SupportsReasoning(),
				ContextLimit: int64(m.ContextLimit()),
			})
		}
	}
	// Sort by (provider_id, id) for determinism. reg.Available() is already sorted
	// and the catalog's Models() are id-sorted within a provider, but sort
	// explicitly so the contract holds regardless of upstream ordering.
	sort.Slice(out, func(i, j int) bool {
		if out[i].GetProviderId() != out[j].GetProviderId() {
			return out[i].GetProviderId() < out[j].GetProviderId()
		}
		return out[i].GetId() < out[j].GetId()
	})
	return out
}
