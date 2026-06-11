package app

import (
	"testing"

	"github.com/stacklok/mecatl/engine/adapter/mockllm"
)

// TestBuildSubagentEngineFactoryReDerivesForOverrideModel is the CONTAMINATION guard for the
// per-call model override (Iteration 4): the factory must mint the override child engine
// through newChildEngineForProvider (engineDepsForProvider re-derives Compactor/
// TokenCounter/Env.Model/ContextWindow for the OVERRIDE model), NEVER a clone-and-swap of
// an existing engine. We prove re-derivation observably via the engine's ContextWindow():
// an override model with a catalogued 200k window yields a child engine whose window is
// 200k, distinct from the parent default's 128k. A clone-and-swap that reused the parent's
// derived window (or window=0 → 128k) would FAIL this.
func TestBuildSubagentEngineFactoryReDerivesForOverrideModel(t *testing.T) {
	// Parent provider = anthropic; the override model has a catalogued 200k window.
	const overrideModel = catAnthropicModel // catalogued 200k
	prov := mockllm.New()
	reg := regForTest(prov, providerAnthropic, "claude-default")
	cfg := Config{Model: "claude-default"}

	factory := buildSubagentEngineFactory(cfg, reg, prov, providerAnthropic, nil)

	// An empty model is unroutable (ok=false), so Subagent surfaces a model-addressable error.
	if _, ok := factory(""); ok {
		t.Fatal("empty model must be unroutable (ok=false)")
	}

	eng, ok := factory(overrideModel)
	if !ok || eng == nil {
		t.Fatalf("factory(%q) = (%v, %v), want a non-nil engine", overrideModel, eng, ok)
	}
	if got := eng.ContextWindow(); got != catAnthropicCtx {
		t.Fatalf("override child ContextWindow = %d, want the OVERRIDE model's catalogued window %d "+
			"(re-derived via engineDepsForProvider, not the parent default %d)",
			got, catAnthropicCtx, defaultContextWindowTokens)
	}
}
