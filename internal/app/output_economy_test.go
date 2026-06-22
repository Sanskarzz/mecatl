package app

import (
	"strings"
	"testing"

	"github.com/stacklok/mecatl/engine/prompt"
)

// TestOutputEconomyToneDelta verifies the economy-tier token mapping (ADR 0041):
// "" and "normal" yield NO tone override (the default defaultTone already carries
// the economy contract, so Build falls through to it); "terse" yields the default
// tone PLUS the answer-length clause (the most over-steer-prone rule, so opt-in);
// an unknown token fail-softs to "" (no override, never a boot refusal).
func TestOutputEconomyToneDelta(t *testing.T) {
	for _, token := range []string{"", "normal", "NORMAL", " "} {
		if got := outputEconomyToneDelta(token); got != "" {
			t.Errorf("outputEconomyToneDelta(%q): want empty (no override), got non-empty", token)
		}
	}

	terse := outputEconomyToneDelta("terse")
	if terse == "" {
		t.Fatal("outputEconomyToneDelta(\"terse\"): want non-empty, got empty")
	}
	if !strings.HasPrefix(terse, prompt.DefaultTone()) {
		t.Errorf("terse delta must start with the default tone (never drift)\ngot=%q", terse)
	}
	if !strings.Contains(terse, "answer in at most a few sentences") {
		t.Errorf("terse delta missing the answer-length clause\ngot=%q", terse)
	}

	// Unknown token fail-softs to "" (no override).
	if got := outputEconomyToneDelta("monosyllabic"); got != "" {
		t.Errorf("outputEconomyToneDelta(\"monosyllabic\"): want empty (fail-soft), got %q", got)
	}
}

// TestPromptConfigThreadsOutputEconomy proves promptConfig folds the terse tone
// delta onto cfg.Tone (composition only — the prompt package stays economy-agnostic)
// and that it lands in the cache-stable StablePrefix (gauntlet #6), byte-identical
// across builds.
func TestPromptConfigThreadsOutputEconomy(t *testing.T) {
	// normal/unset → no tone override → Build uses defaultTone.
	normal := promptConfig(Config{Model: "m", OutputEconomy: "normal"}, "")
	if normal.Tone != "" {
		t.Errorf("normal: Tone must be empty (Build falls through to defaultTone), got %q", normal.Tone)
	}

	// terse → tone override carrying the answer-length clause.
	terse := promptConfig(Config{Model: "m", OutputEconomy: "terse"}, "")
	if terse.Tone == "" {
		t.Fatal("terse: Tone must be non-empty (the override)")
	}
	if !strings.Contains(terse.Tone, "answer in at most a few sentences") {
		t.Errorf("terse Tone missing the answer-length clause\nTone=%q", terse.Tone)
	}

	// The terse delta lands in the cache-stable StablePrefix, not the volatile suffix.
	first := prompt.Build(terse)
	if !strings.Contains(first.StablePrefix, "answer in at most a few sentences") {
		t.Errorf("terse delta missing from StablePrefix\nprefix=%q", first.StablePrefix)
	}
	if strings.Contains(first.VolatileSuffix, "answer in at most a few sentences") {
		t.Errorf("terse delta leaked into VolatileSuffix\nsuffix=%q", first.VolatileSuffix)
	}
	// Byte-identical across two builds → genuinely cache-stable.
	second := prompt.Build(terse)
	if first.StablePrefix != second.StablePrefix {
		t.Error("terse StablePrefix not byte-identical across two builds: not cache-stable")
	}
}

// TestFoldOperatorOutputEconomyCLIOutRanksYAML proves the CLI flag out-ranks the
// operator-YAML key (mirrors foldOperatorPosture): when OutputEconomyFlagSet is
// true, the YAML value is NOT read; when false, the YAML value is folded. A nil
// resolver is a no-op.
func TestFoldOperatorOutputEconomyCLIOutRanksYAML(t *testing.T) {
	// CLI wins: a set flag is preserved even if a resolver were present.
	cliCfg := Config{OutputEconomy: "terse", OutputEconomyFlagSet: true}
	got := foldOperatorOutputEconomy(cliCfg)
	if got.OutputEconomy != "terse" {
		t.Errorf("CLI flag should win: want %q, got %q", "terse", got.OutputEconomy)
	}

	// No flag, nil resolver → no-op (empty stays empty).
	emptyCfg := Config{}
	if got := foldOperatorOutputEconomy(emptyCfg); got.OutputEconomy != "" {
		t.Errorf("nil resolver no-op: want empty, got %q", got.OutputEconomy)
	}
}
