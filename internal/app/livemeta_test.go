package app

import (
	"testing"
)

// catalogued anthropic model with known catalog limits (models.dev.curated.json):
// claude-3-5-haiku-20241022 → ctx 200000, out 8192. Used as the catalog-floor anchor
// so the tests assert on stable embedded data, not a live endpoint.
const (
	catAnthropicModel  = "claude-3-5-haiku-20241022"
	catAnthropicCtx    = 200_000
	catAnthropicOutput = 8192
)

// TestLiveMetaStoreSeedFromCatalog: a freshly seeded store carries the catalog rows
// for every available provider, so a resolver read at t=0 (before any live swap)
// returns the catalog value — never an empty miss.
func TestLiveMetaStoreSeedFromCatalog(t *testing.T) {
	s := newLiveMetaStore()
	s.seedFromCatalog([]string{providerAnthropic})

	m, ok := s.lookup(providerAnthropic, catAnthropicModel)
	if !ok {
		t.Fatalf("seeded store missing catalogued model %q", catAnthropicModel)
	}
	if m.ContextLimit != catAnthropicCtx || m.OutputLimit != catAnthropicOutput {
		t.Fatalf("seed mismatch: ctx=%d out=%d, want %d/%d", m.ContextLimit, m.OutputLimit, catAnthropicCtx, catAnthropicOutput)
	}
	// The seed carries no live thinking descriptor (the catalog has no thinking bit).
	if m.Thinking.Known {
		t.Errorf("seeded entry must not claim a live thinking descriptor")
	}
}

// TestResolversByteIdenticalWithCatalogOnlyStore: with NO lister wired (the store is
// catalog-seeded only), the live-first helpers return EXACTLY the catalog floor — so
// the broadening is behaviour-preserving until a live source populates the store.
func TestResolversByteIdenticalWithCatalogOnlyStore(t *testing.T) {
	s := newLiveMetaStore()
	s.seedFromCatalog([]string{providerAnthropic})

	if got := s.outputLimitFor(providerAnthropic, catAnthropicModel); got != catAnthropicOutput {
		t.Errorf("outputLimitFor = %d, want catalog floor %d", got, catAnthropicOutput)
	}
	if got := s.contextWindowFor(providerAnthropic, catAnthropicModel); got != catAnthropicCtx {
		t.Errorf("contextWindowFor = %d, want catalog floor %d", got, catAnthropicCtx)
	}
	// The bare catalog functions must agree with the helper (the byte-identical claim).
	if anthropicOutputLimit(catAnthropicModel) != s.outputLimitFor(providerAnthropic, catAnthropicModel) {
		t.Error("outputLimitFor diverged from anthropicOutputLimit on a catalog-only store")
	}
	if catalogContextWindow(providerAnthropic, catAnthropicModel) != s.contextWindowFor(providerAnthropic, catAnthropicModel) {
		t.Error("contextWindowFor diverged from catalogContextWindow on a catalog-only store")
	}
}

// TestLiveFirstHelperPrefersLiveWhenPresent: a live entry with a non-zero field
// beats the catalog; a live entry whose field is ZERO falls back to the catalog
// floor (per-FIELD precedence, never a fabricated zero).
func TestLiveFirstHelperPrefersLiveWhenPresent(t *testing.T) {
	s := newLiveMetaStore()
	s.seedFromCatalog([]string{providerAnthropic})

	// A live swap: same model, a DIFFERENT (live) output ceiling and context window.
	const liveOut, liveCtx = 9999, 555_000
	s.Swap(map[string][]modelEntry{
		providerAnthropic: {{ID: catAnthropicModel, OutputLimit: liveOut, ContextLimit: liveCtx}},
	})
	if got := s.outputLimitFor(providerAnthropic, catAnthropicModel); got != liveOut {
		t.Errorf("outputLimitFor = %d, want live %d", got, liveOut)
	}
	if got := s.contextWindowFor(providerAnthropic, catAnthropicModel); got != liveCtx {
		t.Errorf("contextWindowFor = %d, want live %d", got, liveCtx)
	}

	// A live entry with ZERO fields ⇒ per-field fallback to the catalog floor.
	s.Swap(map[string][]modelEntry{
		providerAnthropic: {{ID: catAnthropicModel, OutputLimit: 0, ContextLimit: 0}},
	})
	if got := s.outputLimitFor(providerAnthropic, catAnthropicModel); got != catAnthropicOutput {
		t.Errorf("zero live output: outputLimitFor = %d, want catalog floor %d", got, catAnthropicOutput)
	}
	if got := s.contextWindowFor(providerAnthropic, catAnthropicModel); got != catAnthropicCtx {
		t.Errorf("zero live ctx: contextWindowFor = %d, want catalog floor %d", got, catAnthropicCtx)
	}
}

// TestLiveValueUpperClamped (Security #2): an ABSURD live value (hostile / MITM'd /
// buggy upstream) is UPPER-clamped before it leaves the helper, so it cannot flow
// verbatim into max_tokens (cost/400) or disable compaction (a gigantic window). A
// legitimate in-range live value is returned unchanged.
func TestLiveValueUpperClamped(t *testing.T) {
	s := newLiveMetaStore()
	s.seedFromCatalog([]string{providerAnthropic})

	// Way above any real Claude limit — must clamp to the caps.
	s.Swap(map[string][]modelEntry{
		providerAnthropic: {{ID: catAnthropicModel, OutputLimit: 999_999_999, ContextLimit: 999_999_999}},
	})
	if got := s.outputLimitFor(providerAnthropic, catAnthropicModel); got != maxLiveOutputLimit {
		t.Errorf("absurd live output: outputLimitFor = %d, want clamp %d", got, maxLiveOutputLimit)
	}
	if got := s.contextWindowFor(providerAnthropic, catAnthropicModel); got != maxLiveContextLimit {
		t.Errorf("absurd live ctx: contextWindowFor = %d, want clamp %d", got, maxLiveContextLimit)
	}

	// A legitimate in-range live value (above catalog, below the cap) is unchanged.
	const okOut, okCtx = 200_000, 1_500_000
	s.Swap(map[string][]modelEntry{
		providerAnthropic: {{ID: catAnthropicModel, OutputLimit: okOut, ContextLimit: okCtx}},
	})
	if got := s.outputLimitFor(providerAnthropic, catAnthropicModel); got != okOut {
		t.Errorf("in-range live output clamped unexpectedly: %d, want %d", got, okOut)
	}
	if got := s.contextWindowFor(providerAnthropic, catAnthropicModel); got != okCtx {
		t.Errorf("in-range live ctx clamped unexpectedly: %d, want %d", got, okCtx)
	}
}

// TestLiveMissFallsBackToCatalogRow: a model the LIVE swap omits but the catalog
// knows must still resolve via the catalog (live absence never erases the catalog).
func TestLiveMissFallsBackToCatalogRow(t *testing.T) {
	s := newLiveMetaStore()
	s.seedFromCatalog([]string{providerAnthropic})
	// A live swap that DROPS catAnthropicModel entirely (only an unrelated id present).
	s.Swap(map[string][]modelEntry{
		providerAnthropic: {{ID: "some-live-only-model", OutputLimit: 12345, ContextLimit: 99}},
	})
	// catAnthropicModel is absent from the live swap → catalog floor via the helper.
	if got := s.outputLimitFor(providerAnthropic, catAnthropicModel); got != catAnthropicOutput {
		t.Errorf("live miss: outputLimitFor = %d, want catalog floor %d", got, catAnthropicOutput)
	}
	if got := s.contextWindowFor(providerAnthropic, catAnthropicModel); got != catAnthropicCtx {
		t.Errorf("live miss: contextWindowFor = %d, want catalog floor %d", got, catAnthropicCtx)
	}
}

// TestThinkingForFromLiveStore: thinkingFor returns the live descriptor only when a
// live source populated it (Known=true); a seed-only or absent entry yields
// known=false (the adapter then falls back to its prefix matrix).
func TestThinkingForFromLiveStore(t *testing.T) {
	s := newLiveMetaStore()
	s.seedFromCatalog([]string{providerAnthropic})

	// Seed-only entry: not live-known.
	if _, _, known := s.thinkingFor(providerAnthropic, catAnthropicModel); known {
		t.Error("seed-only entry must report known=false (defer to prefix matrix)")
	}

	// A live swap with a populated thinking descriptor (adaptive).
	s.Swap(map[string][]modelEntry{
		providerAnthropic: {{ID: catAnthropicModel, Thinking: thinkingDescriptor{Known: true, Adaptive: true}}},
	})
	a, e, known := s.thinkingFor(providerAnthropic, catAnthropicModel)
	if !known || !a || e {
		t.Errorf("live thinking: got adaptive=%v enabled=%v known=%v, want adaptive=true enabled=false known=true", a, e, known)
	}

	// An absent model is never known.
	if _, _, known := s.thinkingFor(providerAnthropic, "no-such-model"); known {
		t.Error("absent model must report known=false")
	}
}

// TestLiveMetaStoreNilSafe: every read on a nil store is a safe miss, and Swap/seed
// on a nil receiver are no-ops — so a hand-built test registry with no meta store
// cannot panic.
func TestLiveMetaStoreNilSafe(t *testing.T) {
	var s *liveMetaStore
	if _, ok := s.lookup(providerAnthropic, catAnthropicModel); ok {
		t.Error("nil store lookup should miss")
	}
	if got := s.outputLimitFor(providerAnthropic, catAnthropicModel); got != catAnthropicOutput {
		// nil store falls through to the catalog floor for anthropic.
		t.Errorf("nil store outputLimitFor = %d, want catalog floor %d", got, catAnthropicOutput)
	}
	if got := s.contextWindowFor(providerAnthropic, catAnthropicModel); got != catAnthropicCtx {
		t.Errorf("nil store contextWindowFor = %d, want catalog floor %d", got, catAnthropicCtx)
	}
	if _, _, known := s.thinkingFor(providerAnthropic, catAnthropicModel); known {
		t.Error("nil store thinkingFor should report known=false")
	}
	s.Swap(map[string][]modelEntry{providerAnthropic: {{ID: "x"}}}) // must not panic
	s.seedFromCatalog([]string{providerAnthropic})                  // must not panic
}
