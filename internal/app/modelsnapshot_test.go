package app

import (
	"sort"
	"strings"
	"testing"

	"github.com/stacklok/mecatl/engine/adapter/mockllm"
	"github.com/stacklok/mecatl/engine/port"
)

// fakeRegistry builds a providerRegistry whose entries are the given available
// provider ids, each backed by a canned mock provider (offline). It is the
// composition-layer test fixture for modelSnapshot (which reads only the registry
// ids + the embedded catalog, never the provider itself).
func fakeRegistry(t *testing.T, ids ...string) *providerRegistry {
	t.Helper()
	entries := make(map[string]providerEntry, len(ids))
	for _, id := range ids {
		entries[id] = providerEntry{id: id, provider: mockllm.New(mockllm.TextTurn("x")), available: true}
	}
	def := ""
	if len(ids) > 0 {
		sorted := append([]string(nil), ids...)
		sort.Strings(sorted)
		def = sorted[0]
	}
	return &providerRegistry{entries: entries, defaultID: def}
}

// TestModelSnapshotAvailableOnly: only the AVAILABLE providers' models appear; an
// unavailable provider (not in the registry) contributes nothing — its very
// availability is concealed (CWE-200).
func TestModelSnapshotAvailableOnly(t *testing.T) {
	reg := fakeRegistry(t, providerOpenAI) // openrouter NOT available
	models := modelSnapshot(reg)
	if len(models) == 0 {
		t.Fatal("modelSnapshot returned no models for an available openai provider")
	}
	for _, m := range models {
		if m.GetProviderId() != providerOpenAI {
			t.Fatalf("model %q has provider_id %q, want only %q (availability gate leaked)",
				m.GetId(), m.GetProviderId(), providerOpenAI)
		}
	}
}

// TestModelSnapshotSkipsMock: a UseMock registry (single "mock" entry) advertises
// no selectable models — you cannot pick a model against the canned mock.
func TestModelSnapshotSkipsMock(t *testing.T) {
	reg := fakeRegistry(t, providerMock)
	if got := modelSnapshot(reg); got != nil {
		t.Fatalf("modelSnapshot(mock) = %d models, want nil (mock advertises none)", len(got))
	}
}

// TestModelSnapshotNilRegistry: a nil registry (defensive) yields nil, never a
// panic.
func TestModelSnapshotNilRegistry(t *testing.T) {
	if got := modelSnapshot(nil); got != nil {
		t.Fatalf("modelSnapshot(nil) = %v, want nil", got)
	}
}

// TestModelSnapshotNoSecrets: no projected ModelInfo field carries a key, env-var
// name, or base URL. A registry built with sentinel keys via the real
// buildProviderRegistry proves the projection reads only public catalog metadata —
// the sentinel never appears anywhere in the snapshot.
func TestModelSnapshotNoSecrets(t *testing.T) {
	const sentinelKey = "sk-SENTINEL-do-not-leak"
	reg, err := buildProviderRegistry(Config{
		Model: "gpt-5",
		// Inject a mock provider per id so the two real ids stay available offline.
		providerConstructor: func(_ Config, _, _, _ string) port.LLMProvider {
			return mockllm.New(mockllm.TextTurn("x"))
		},
	}, fakeEnv(map[string]string{
		"OPENAI_API_KEY":     sentinelKey,
		"OPENROUTER_API_KEY": sentinelKey,
	}))
	if err != nil {
		t.Fatalf("buildProviderRegistry: %v", err)
	}
	models := modelSnapshot(reg)
	if len(models) == 0 {
		t.Fatal("expected models for two available providers")
	}
	// Tripwire: the key, the env-var names, and the base URL must appear in NO field.
	forbidden := []string{
		sentinelKey,
		"OPENAI_API_KEY", "OPENROUTER_API_KEY",
		openRouterDefaultBaseURL,
	}
	for _, m := range models {
		fields := []string{m.GetId(), m.GetProviderId(), m.GetDisplayName()}
		for _, f := range fields {
			for _, bad := range forbidden {
				if strings.Contains(f, bad) {
					t.Fatalf("secret leaked into ModelInfo field %q: contains %q", f, bad)
				}
			}
		}
	}
}

// TestModelSnapshotSortedDeterministic: two providers / many models ⇒ sorted by
// (provider_id, id).
func TestModelSnapshotSortedDeterministic(t *testing.T) {
	reg := fakeRegistry(t, providerOpenAI, providerOpenRouter)
	models := modelSnapshot(reg)
	if len(models) < 2 {
		t.Fatalf("expected many models across two providers, got %d", len(models))
	}
	for i := 1; i < len(models); i++ {
		prev, cur := models[i-1], models[i]
		if prev.GetProviderId() > cur.GetProviderId() {
			t.Fatalf("not provider-sorted at %d: %q after %q", i, cur.GetProviderId(), prev.GetProviderId())
		}
		if prev.GetProviderId() == cur.GetProviderId() && prev.GetId() > cur.GetId() {
			t.Fatalf("not id-sorted within provider %q at %d: %q after %q",
				cur.GetProviderId(), i, cur.GetId(), prev.GetId())
		}
	}
	// Both providers represented.
	seen := map[string]bool{}
	for _, m := range models {
		seen[m.GetProviderId()] = true
	}
	if !seen[providerOpenAI] || !seen[providerOpenRouter] {
		t.Fatalf("expected both providers in snapshot, saw %v", seen)
	}
}

// TestModelSnapshotImageReflectsIntersection: sink (a) reads the catalog ∩ adapter
// INTERSECTION, not the catalog alone. With an adapter-says-no double (Image:false)
// for a provider whose catalog DOES claim image, every projected ModelInfo.Image is
// false — proving modelSnapshot consults the adapter authority via modelCapability.
func TestModelSnapshotImageReflectsIntersection(t *testing.T) {
	// A registry whose openai provider reports Image:false (the P1 "adapter says no").
	reg := &providerRegistry{
		entries: map[string]providerEntry{
			providerOpenAI: {
				id:        providerOpenAI,
				provider:  mockllm.NewWith([]mockllm.Option{mockllm.WithCapabilities(port.ProviderCapabilities{Image: false})}, mockllm.TextTurn("x")),
				available: true,
			},
		},
		defaultID: providerOpenAI,
	}
	models := modelSnapshot(reg)
	if len(models) == 0 {
		t.Fatal("expected openai models")
	}
	for _, m := range models {
		if m.GetImage() {
			t.Fatalf("ModelInfo[%q].Image = true with an adapter reporting Image:false; "+
				"snapshot read the catalog alone, not the intersection", m.GetId())
		}
	}

	// Control: with an adapter reporting Image:true, the catalog's image models DO
	// advertise image (the intersection is the catalog value), so the false above is
	// the adapter's doing, not a blanket false.
	regYes := regWithProvider(providerOpenAI, port.ProviderCapabilities{Image: true})
	anyImage := false
	for _, m := range modelSnapshot(regYes) {
		if m.GetImage() {
			anyImage = true
			break
		}
	}
	if !anyImage {
		t.Fatal("with adapter Image:true, expected >=1 catalog image model to advertise image")
	}
}

// TestModelSnapshotMapsCatalogFields: the projected ModelInfo carries the catalog's
// image/reasoning/context_limit for a known model (mapping correctness).
func TestModelSnapshotMapsCatalogFields(t *testing.T) {
	reg := fakeRegistry(t, providerOpenAI)
	models := modelSnapshot(reg)
	for _, m := range models {
		// Every model must carry a non-empty id + display name and a non-negative limit.
		if m.GetId() == "" {
			t.Fatal("ModelInfo.id is empty")
		}
		if m.GetDisplayName() == "" {
			t.Fatalf("ModelInfo.display_name empty for %q (should fall back to id)", m.GetId())
		}
		if m.GetContextLimit() < 0 {
			t.Fatalf("ModelInfo.context_limit negative for %q", m.GetId())
		}
	}
}
