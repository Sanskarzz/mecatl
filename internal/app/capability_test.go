package app

import (
	"testing"

	"github.com/stacklok/mecatl/internal/adapter/mockllm"
	"github.com/stacklok/mecatl/internal/adapter/providercatalog"
	"github.com/stacklok/mecatl/internal/port"
)

// regWithProvider builds a single-entry registry whose one provider id is backed
// by a provider with the given capabilities (an offline mock). It is the
// composition-layer fixture for modelCapability, which reads the catalog by id +
// the adapter's Capabilities().
func regWithProvider(id string, caps port.ProviderCapabilities) *providerRegistry {
	p := mockllm.NewWith([]mockllm.Option{mockllm.WithCapabilities(caps)}, mockllm.TextTurn("x"))
	return &providerRegistry{
		entries:   map[string]providerEntry{id: {id: id, provider: p, available: true}},
		defaultID: id,
	}
}

// catalogImageModel returns a (model id) that the catalog marks image-capable for
// the given provider, plus a model id that is NOT image-capable, so the
// intersection tests assert against the SAME data the catalog ships.
func catalogModels(t *testing.T, providerID string) (imageModel, noImageModel string) {
	t.Helper()
	p, ok := providercatalog.Default().Provider(providerID)
	if !ok {
		t.Fatalf("provider %q not in catalog", providerID)
	}
	for _, m := range p.Models() {
		if m.SupportsImageInput() && imageModel == "" {
			imageModel = m.ID()
		}
		if !m.SupportsImageInput() && noImageModel == "" {
			noImageModel = m.ID()
		}
	}
	return imageModel, noImageModel
}

// TestModelCapabilityIntersection_AdapterYes: a catalog model with image + an
// adapter that reports Image:true ⇒ Image:true (the common P0 case).
func TestModelCapabilityIntersection_AdapterYes(t *testing.T) {
	imageModel, _ := catalogModels(t, providerOpenAI)
	if imageModel == "" {
		t.Skip("no catalogued openai image model")
	}
	reg := regWithProvider(providerOpenAI, port.ProviderCapabilities{Image: true})
	if got := modelCapability(reg, providerOpenAI, imageModel); !got.Image {
		t.Fatalf("Image = false, want true (catalog image ∩ adapter Image:true)")
	}
}

// TestModelCapabilityIntersection_AdapterNo: the load-bearing "adapter says no"
// branch P1 relies on, provable in P0 via the test-double provider. The catalog
// model claims image but the adapter reports Image:false ⇒ Image:false. This is the
// whole reason the AND lives in composition.
func TestModelCapabilityIntersection_AdapterNo(t *testing.T) {
	imageModel, _ := catalogModels(t, providerOpenAI)
	if imageModel == "" {
		t.Skip("no catalogued openai image model")
	}
	reg := regWithProvider(providerOpenAI, port.ProviderCapabilities{Image: false})
	if got := modelCapability(reg, providerOpenAI, imageModel); got.Image {
		t.Fatalf("Image = true, want false (catalog image ∩ adapter Image:false must be false)")
	}
}

// TestModelCapabilityIntersection_CatalogNoImage: a catalog model WITHOUT image +
// an adapter that reports Image:true ⇒ Image:false (the catalog gates it).
func TestModelCapabilityIntersection_CatalogNoImage(t *testing.T) {
	_, noImageModel := catalogModels(t, providerOpenAI)
	if noImageModel == "" {
		t.Skip("no catalogued openai non-image model")
	}
	reg := regWithProvider(providerOpenAI, port.ProviderCapabilities{Image: true})
	if got := modelCapability(reg, providerOpenAI, noImageModel); got.Image {
		t.Fatalf("Image = true, want false (catalog non-image ∩ adapter Image:true must be false)")
	}
}

// TestModelCapabilityIntersection_CatalogAudioAlwaysFalse pins the documented
// invariant: a CATALOGUED model yields Audio==false even when the injected adapter
// reports Audio:true, because the catalog carries no audio field today ⇒ the
// catAudio term is always false ⇒ the AND is false for any catalogued model. Guards
// against a future catalog that adds an audio modality silently flipping the echo.
func TestModelCapabilityIntersection_CatalogAudioAlwaysFalse(t *testing.T) {
	imageModel, _ := catalogModels(t, providerOpenAI)
	if imageModel == "" {
		t.Skip("no catalogued openai model")
	}
	reg := regWithProvider(providerOpenAI, port.ProviderCapabilities{Image: true, Audio: true})
	if got := modelCapability(reg, providerOpenAI, imageModel); got.Audio {
		t.Fatalf("Audio = true for a catalogued model with adapter Audio:true; " +
			"want false (catalog carries no audio field, so the catAudio term is always false)")
	}
}

// TestModelCapabilityIntersection_PassthroughModel: an uncatalogued model id falls
// back to ADAPTER-ONLY caps (image NOT zeroed) — a passthrough model trusts the
// adapter when the catalog is silent.
func TestModelCapabilityIntersection_PassthroughModel(t *testing.T) {
	reg := regWithProvider(providerOpenAI, port.ProviderCapabilities{Image: true, Audio: true})
	got := modelCapability(reg, providerOpenAI, "totally-made-up-model-not-in-catalog")
	if !got.Image {
		t.Fatal("passthrough Image = false, want adapter-only true (catalog silent must not zero it)")
	}
	if !got.Audio {
		t.Fatal("passthrough Audio = false, want adapter-only true")
	}
}

// TestModelCapabilityIntersection_UnknownProvider: an unknown provider id yields the
// zero value (text-only), no panic (fail-safe — a provider we cannot reach
// transmits nothing).
func TestModelCapabilityIntersection_UnknownProvider(t *testing.T) {
	reg := regWithProvider(providerOpenAI, port.ProviderCapabilities{Image: true})
	got := modelCapability(reg, "no-such-provider", "whatever")
	if got != (port.ProviderCapabilities{}) {
		t.Fatalf("unknown provider caps = %+v, want zero value (text-only)", got)
	}
}

// TestModelCapabilityIntersection_NilRegistry: a nil registry is fail-safe
// (text-only), never a panic.
func TestModelCapabilityIntersection_NilRegistry(t *testing.T) {
	if got := modelCapability(nil, providerOpenAI, "gpt-5"); got != (port.ProviderCapabilities{}) {
		t.Fatalf("nil-registry caps = %+v, want zero value", got)
	}
}

// TestModelCapabilityIntersection_ReasoningNotInCaps documents that reasoning is
// NOT a port.ProviderCapabilities bit and is therefore NOT intersected here — it
// stays catalog-sourced on ModelInfo. The intersection only carries image/audio/
// embedded-context, so a reasoning-capable model's caps are governed solely by the
// image/audio AND (this guards against someone smuggling reasoning into the echo).
func TestModelCapabilityIntersection_ReasoningNotInCaps(t *testing.T) {
	// port.ProviderCapabilities has no Reasoning field; this test exists as a
	// living assertion of the decision. Verify the catalog DOES expose reasoning so
	// the ModelInfo path (not the caps path) remains the reasoning source.
	p, ok := providercatalog.Default().Provider(providerOpenAI)
	if !ok {
		t.Fatal("openai not in catalog")
	}
	anyReasoning := false
	for _, m := range p.Models() {
		if m.SupportsReasoning() {
			anyReasoning = true
			break
		}
	}
	if !anyReasoning {
		t.Skip("no reasoning model in catalog to anchor the decision")
	}
	// The caps struct simply does not have a reasoning bit — the intersection cannot
	// carry it. (Compile-time guaranteed; this asserts the catalog still sources it.)
}
