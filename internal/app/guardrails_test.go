package app

import (
	"context"
	"os"
	"path/filepath"
	"testing"

	"github.com/stacklok/mecatl/engine/adapter/mockllm"
	"github.com/stacklok/mecatl/engine/governance"
	"github.com/stacklok/mecatl/engine/port"
	"github.com/stacklok/mecatl/engine/tool"
	"github.com/stacklok/mecatl/internal/adapter/hookexec"
	"github.com/stacklok/mecatl/internal/adapter/modelhook"
	"github.com/stacklok/mecatl/internal/adapter/permconfig"
)

// (15) fail-fast unknown guardrails model: the loud-misconfig posture, mirroring the
// ask-reviewer normalization. Empty = off, an unknown bare token / inherit-alias is a
// BUILD error, a concrete id / mapped alias passes verbatim, UseMock skips validation.
func TestNormalizeGuardrailsModelFailFast(t *testing.T) {
	if got, err := normalizeGuardrailsModel(Config{}); err != nil || got != "" {
		t.Fatalf("empty must be off: got %q err %v", got, err)
	}
	if _, err := normalizeGuardrailsModel(Config{GuardrailsModel: "bogus"}); err == nil {
		t.Fatal("an unknown bare token must fail fast")
	}
	if _, err := normalizeGuardrailsModel(Config{GuardrailsModel: "sonnet"}); err == nil {
		t.Fatal("a builtin inherit-alias must fail fast (the checker needs a concrete model)")
	}
	if got, err := normalizeGuardrailsModel(Config{GuardrailsModel: "gpt-5-mini"}); err != nil || got != "gpt-5-mini" {
		t.Fatalf("a concrete id must pass verbatim: got %q err %v", got, err)
	}
	if got, err := normalizeGuardrailsModel(Config{UseMock: true, GuardrailsModel: "bogus"}); err != nil || got != "bogus" {
		t.Fatalf("UseMock must skip validation: got %q err %v", got, err)
	}
	// The kill-switch short-circuits validation (off regardless of the value).
	if got, err := normalizeGuardrailsModel(Config{GuardrailsModel: "bogus", GuardrailsDisabled: true}); err != nil || got != "bogus" {
		t.Fatalf("a disabled guardrail must skip model validation: got %q err %v", got, err)
	}
}

// the build-once ACTIVE / INERT facts.
func TestNormalizeGuardrailsModelNarratesFacts(t *testing.T) {
	active := &capturingDiag{}
	if _, err := normalizeGuardrailsModel(Config{
		GuardrailsModel: "gpt-5-mini",
		GuardrailsRules: []GuardrailRule{{Match: "WebFetch"}},
		Diagnostics:     active,
	}); err != nil {
		t.Fatalf("valid config: %v", err)
	}
	if !active.has("guardrails ACTIVE") {
		t.Fatalf("a model + rules must narrate ACTIVE; lines=%v", active.lines)
	}

	// A model with NO explicit rules is ACTIVE on the DEFAULT advisory rule set (the
	// headline default), narrated as such — NOT inert.
	defaults := &capturingDiag{}
	if _, err := normalizeGuardrailsModel(Config{GuardrailsModel: "gpt-5-mini", Diagnostics: defaults}); err != nil {
		t.Fatalf("model-without-rules: %v", err)
	}
	if !defaults.has("default advisory rules") {
		t.Fatalf("a model with no rules must narrate ACTIVE on the default advisory set; lines=%v", defaults.lines)
	}
}

// (7) OFF-by-default: buildGuardrailsHooks returns inner UNCHANGED when guardrails are
// unconfigured (byte-identical to no guardrails — the same inner pointer).
func TestGuardrailsOffReturnsInnerUnchanged(t *testing.T) {
	inner := hookexec.New(nil)
	llm := mockllm.New()

	// No model at all → OFF (byte-identical to no guardrails).
	if got := buildGuardrailsHooks(Config{UseMock: true}, nil, llm, "mock", "m", inner); got != port.HookRunner(inner) {
		t.Fatal("unconfigured guardrails (no model) must return inner unchanged (OFF byte-identical)")
	}
	// The master kill-switch wins over a full config.
	cfg := Config{UseMock: true, GuardrailsModel: "x", GuardrailsRules: []GuardrailRule{{Match: "*"}}, GuardrailsDisabled: true}
	if got := buildGuardrailsHooks(cfg, nil, llm, "mock", "m", inner); got != port.HookRunner(inner) {
		t.Fatal("--guardrails=off must return inner unchanged")
	}
}

// A model with NO explicit rules WRAPS inner with the DEFAULT advisory rule set (the
// headline default: ON advisory for WebSearch/WebFetch/mcp__*).
func TestGuardrailsModelOnlyShipsDefaultAdvisory(t *testing.T) {
	inner := hookexec.New(nil)
	llm := mockllm.New()
	cfg := Config{UseMock: true, GuardrailsModel: "checker-model"}
	got := buildGuardrailsHooks(cfg, nil, llm, "mock", "m", inner)
	if got == port.HookRunner(inner) {
		t.Fatal("a guardrails model with no explicit rules must ship the DEFAULT advisory rules, not stay inert")
	}
	// The default set is exactly WebSearch/WebFetch/mcp__*; assert it compiles to 3.
	specs, usedDefaults := effectiveGuardrailSpecs(cfg)
	if !usedDefaults || len(specs) != 3 {
		t.Fatalf("model-only must use the 3-rule default advisory set; usedDefaults=%v n=%d", usedDefaults, len(specs))
	}
	for _, s := range specs {
		if s.Mode != string(modelhook.ModeAdvisory) {
			t.Fatalf("default rules must be advisory (observe-only); got %q for %q", s.Mode, s.Match)
		}
	}
}

// The default cost cap is applied only when the defaults are in force AND the operator
// did not pin maxChecks; an explicit rule list or explicit maxChecks keeps the
// operator's value (including a deliberate 0 = unbounded).
func TestGuardrailsDefaultMaxChecksOnlyWithDefaults(t *testing.T) {
	// Explicit rules → defaults NOT used → no default cap injected.
	_, usedDefaults := effectiveGuardrailSpecs(Config{GuardrailsRules: []GuardrailRule{{Match: "*"}}})
	if usedDefaults {
		t.Fatal("explicit rules must not be flagged as defaults")
	}
	// Model only → defaults used.
	_, usedDefaults = effectiveGuardrailSpecs(Config{GuardrailsModel: "x"})
	if !usedDefaults {
		t.Fatal("model-only must use the defaults")
	}
}

// a configured guardrail wraps inner (no longer the same pointer).
func TestGuardrailsConfiguredWrapsInner(t *testing.T) {
	inner := hookexec.New(nil)
	llm := mockllm.New()
	cfg := Config{UseMock: true, GuardrailsModel: "checker-model", GuardrailsRules: []GuardrailRule{{Match: "WebFetch", Phases: []string{"post"}}}}
	got := buildGuardrailsHooks(cfg, nil, llm, "mock", "m", inner)
	if got == port.HookRunner(inner) {
		t.Fatal("a configured guardrail must WRAP inner, not return it unchanged")
	}
}

// (8) quarantine / no-recursion: the checker engine is built via the CHILD deps path,
// which forces inert Hooks + nil ChildAskReviewer + Interactive false + a tool-less
// catalog — so a checker call fires no hooks and can never re-trigger the runner.
func TestGuardrailsCheckerEngineQuarantined(t *testing.T) {
	cfg := Config{UseMock: true, Model: "m"}
	deps := childEngineDepsForProvider(cfg, "guardrail-checker", mockllm.New(), "checker-model", 0,
		tool.NewCatalog(), promptConfig(modelCfgFor(cfg, "checker-model"), cfg.gitStatus), nil)

	if deps.ChildAskReviewer != nil {
		t.Fatal("the checker engine must carry NO ask reviewer (no nesting)")
	}
	if deps.Interactive {
		t.Fatal("the checker engine must be non-interactive")
	}
	if deps.Hooks == nil {
		t.Fatal("the checker engine must carry inert (non-nil hookexec) Hooks, never the guardrail runner")
	}
	// The hooks must NOT be a modelhook.Runner (that would recurse): a plain hookexec
	// allows every phase, so a checker tool phase fires no guardrail.
	out, err := deps.Hooks.Run(context.Background(), governance.HookEvent{Phase: governance.PhasePreToolUse, Tool: "WebFetch"})
	if err != nil || out.Block || len(out.Mutated) != 0 {
		t.Fatalf("the checker engine's hooks must be inert (allow-all), got %+v err %v", out, err)
	}
	if deps.Catalog == nil || len(deps.Catalog.Tools()) != 0 {
		t.Fatal("the checker engine must be tool-less")
	}
}

// foldOperatorGuardrails: a nil resolver / no operator block is a no-op; a YAML block
// is folded (rules + model + cost knobs); CLI flags out-rank YAML for model + disable.
func TestFoldOperatorGuardrailsNoResolverNoOp(t *testing.T) {
	cfg := Config{Model: "m"}
	if got := foldOperatorGuardrails(cfg); len(got.GuardrailsRules) != 0 || got.GuardrailsModel != "" {
		t.Fatal("with no resolver the fold must be a no-op")
	}
}

// foldOperatorGuardrails folds the OPERATOR-TIER YAML (model + maxChecks +
// minContentBytes + rules) onto Config; a CLI --guardrails-model wins over the YAML
// model. This also proves MinContentBytes is LIVE config (folded end-to-end), not dead.
func TestFoldOperatorGuardrailsFromYAML(t *testing.T) {
	const yamlCfg = `
guardrails:
  model: "yaml-model"
  maxChecks: 9
  minContentBytes: 24
  rules:
    - match: "WebFetch"
      phases: ["post"]
      mode: "block"
`
	path := filepath.Join(t.TempDir(), "guardrails.yaml")
	if err := os.WriteFile(path, []byte(yamlCfg), 0o600); err != nil {
		t.Fatalf("write temp config: %v", err)
	}
	res := permconfig.New(permconfig.Options{ExplicitFiles: []string{path}})
	if res == nil {
		t.Fatal("resolver should be non-nil")
	}

	// No CLI model: adopt the YAML model + cost knobs + rules.
	cfg := foldOperatorGuardrails(Config{permResolver: res})
	if cfg.GuardrailsModel != "yaml-model" {
		t.Fatalf("YAML model must fold when no CLI model; got %q", cfg.GuardrailsModel)
	}
	if cfg.GuardrailsMaxChecks != 9 || cfg.GuardrailsMinContentBytes != 24 {
		t.Fatalf("cost knobs must fold (maxChecks=%d minContentBytes=%d)", cfg.GuardrailsMaxChecks, cfg.GuardrailsMinContentBytes)
	}
	if len(cfg.GuardrailsRules) != 1 || cfg.GuardrailsRules[0].Match != "WebFetch" {
		t.Fatalf("rules must fold from YAML; got %+v", cfg.GuardrailsRules)
	}

	// A CLI --guardrails-model out-ranks the YAML model.
	cliCfg := foldOperatorGuardrails(Config{permResolver: res, GuardrailsModel: "cli-model"})
	if cliCfg.GuardrailsModel != "cli-model" {
		t.Fatalf("CLI model must out-rank the YAML model; got %q", cliCfg.GuardrailsModel)
	}
}
