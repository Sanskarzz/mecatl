package app

import (
	"context"
	"strings"

	"github.com/stacklok/mecatl/engine/agent"
	"github.com/stacklok/mecatl/engine/port"
	"github.com/stacklok/mecatl/engine/tool"
	"github.com/stacklok/mecatl/internal/adapter/modelhook"
	"github.com/stacklok/mecatl/internal/adapter/permconfig"
)

// foldOperatorGuardrails merges the OPERATOR-TIER `guardrails:` YAML subtree (read by
// the permconfig resolver from the user-global + CLI tiers ONLY — never the project
// file) onto cfg. CLI flags out-rank YAML for the scalar knobs that have a flag
// (GuardrailsModel from --guardrails-model, GuardrailsDisabled from --guardrails=off);
// the rule list comes from YAML (a flag cannot express it). It is a no-op when no
// operator-tier guardrails block was configured. cfg is taken and returned by value
// (Build holds a local cfg).
func foldOperatorGuardrails(cfg Config) Config {
	res, ok := cfg.permResolver.(*permconfig.Resolver)
	if !ok || res == nil {
		return cfg
	}
	g := res.OperatorGuardrails()
	if g == nil {
		return cfg
	}
	// Model: a CLI --guardrails-model wins; else adopt the YAML model.
	if strings.TrimSpace(cfg.GuardrailsModel) == "" {
		cfg.GuardrailsModel = strings.TrimSpace(g.Model)
	}
	// Disabled: OR the YAML kill-switch with the CLI one (either disables).
	if g.Disabled {
		cfg.GuardrailsDisabled = true
	}
	// Scalar cost knobs: YAML supplies them (no flag), but a non-zero CLI value (if a
	// flag is ever added) would win; today these come only from YAML.
	if cfg.GuardrailsMaxChecks == 0 {
		cfg.GuardrailsMaxChecks = g.MaxChecks
	}
	if cfg.GuardrailsMinContentBytes == 0 {
		cfg.GuardrailsMinContentBytes = g.MinContentBytes
	}
	// Rules: YAML is the sole source. Map the on-disk specs to app.GuardrailRule.
	if len(cfg.GuardrailsRules) == 0 && len(g.Rules) > 0 {
		rules := make([]GuardrailRule, 0, len(g.Rules))
		for _, r := range g.Rules {
			rules = append(rules, GuardrailRule{
				Match:      r.Match,
				Phases:     r.Phases,
				Mode:       r.Mode,
				Prompt:     r.Prompt,
				FailClosed: r.FailClosed,
			})
		}
		cfg.GuardrailsRules = rules
	}
	return cfg
}

// guardrails.go is the COMPOSITION wiring for the issue #27 LLM-backed guardrails
// (the modelhook adapter). It builds the engine-backed VerdictChecker, compiles the
// operator-tier rule config into the adapter's compiled rules, and decorates the
// MAIN engine's HookRunner with a modelhook.Runner. It is OFF-by-default and
// byte-identical to "no guardrails" when unconfigured: buildGuardrailsHooks returns
// inner UNCHANGED.
//
// Recursion guard (the #1 invariant): the modelhook.Runner is wired ONLY into the
// MAIN engine's hooks (mainHooks in buildEngine + the per-session factory's
// re-derivation), NEVER into buildCatalog's child hooks. The checker engine is built
// via childEngineDepsForProvider (forced inert Hooks + nil ChildAskReviewer +
// Interactive false + tool-less catalog), so a checker call fires no hooks and can
// never re-trigger the runner.

// engineGuardrailsChecker is the composition's modelhook.VerdictChecker: it drives a
// tool-less one-turn checker Engine over the assembled prompt and parses the reply
// into a modelhook.Verdict with the adapter's whole-output-single-object ParseVerdict
// (a prose-extracting, fail-open validator would be wrong for attacker-adjacent
// content). A run error / cancellation is an ERROR (never a fabricated verdict), and
// an unparseable/ambiguous reply is an ERROR too — so the Runner takes its
// fail-open/closed path rather than trusting a malformed verdict.
type engineGuardrailsChecker struct {
	engine *agent.Engine
}

// Check drives the checker engine and parses the verdict. The prompt is fully
// assembled by the Runner (trusted rubric + fenced/neutralised content); this only
// drives + parses.
func (c engineGuardrailsChecker) Check(ctx context.Context, req modelhook.CheckRequest) (modelhook.Verdict, error) {
	text, err := agent.RunGuardrailCheck(ctx, c.engine, req.Prompt)
	if err != nil {
		return modelhook.Verdict{}, err
	}
	v, ok := modelhook.ParseVerdict(text)
	if !ok {
		return modelhook.Verdict{}, errGuardrailVerdictUnparseable
	}
	return v, nil
}

// errGuardrailVerdictUnparseable is the sentinel for a checker reply that was not a
// single unambiguous JSON verdict object. It is treated as a checker FAILURE by the
// Runner (fail-open by default, fail-closed when the rule opts in) — never a
// fabricated "safe".
var errGuardrailVerdictUnparseable = guardrailError("guardrail checker verdict unparseable or ambiguous")

type guardrailError string

func (e guardrailError) Error() string { return string(e) }

// buildGuardrailsHooks decorates inner with the issue #27 guardrails Runner, or
// returns inner UNCHANGED when guardrails are unconfigured (OFF-by-default,
// byte-identical to the pre-feature posture). It is called at BOTH main-engine hook
// sites — buildEngine (the shared default-provider engine) and sessionEngineFactory
// (each per-session engine, re-derived on the session's resolved provider/model) —
// so a FRESH Runner (and thus a FRESH per-session cost budget) is built per session.
//
// The checker engine is built over the supplied (provider, model, window) via the
// child deps path (childEngineDepsForProvider), so it compacts/counts on the
// session's provider and carries the recursion-guard posture (inert hooks, nil
// reviewer, Interactive false, tool-less catalog).
func buildGuardrailsHooks(cfg Config, provReg *providerRegistry, provider port.LLMProvider, parentProviderID, parentModel string, inner port.HookRunner) port.HookRunner {
	if !guardrailsConfigured(cfg) {
		return inner // OFF: byte-identical to no guardrails
	}
	specs, usedDefaults := effectiveGuardrailSpecs(cfg)
	rules, ok := compileGuardrailRules(cfg, specs)
	if !ok {
		// No usable rule (every spec was invalid and logged): leave inner unchanged
		// rather than wire a Runner that matches nothing.
		return inner
	}
	checker := buildGuardrailsChecker(cfg, provReg, provider, parentProviderID, parentModel)
	if checker == nil {
		return inner
	}
	maxChecks := cfg.GuardrailsMaxChecks
	if maxChecks == 0 && usedDefaults {
		// The DEFAULT advisory rule set matches mcp__* on both directions, so it can
		// surprise-bill on an MCP-heavy session. When the operator only set a model
		// (taking the default rules) and did not pin maxChecks, apply a sane non-zero
		// per-session cap so default-on guardrails cannot bill without bound. An
		// explicit maxChecks (or explicit rules) keeps the operator's value, including
		// a deliberate 0 = unbounded.
		maxChecks = defaultGuardrailsMaxChecks
	}
	return modelhook.New(inner, modelhook.Options{
		Rules:           rules,
		Checker:         checker,
		Diagnostics:     cfg.diag(),
		MaxChecks:       maxChecks,
		MinContentBytes: cfg.GuardrailsMinContentBytes,
	})
}

// defaultGuardrailsMaxChecks is the per-session checker-call cap applied when the
// operator enabled guardrails with only a model (taking the DEFAULT advisory rule
// set) and did not pin maxChecks — so default-on guardrails cannot bill without
// bound on an MCP-heavy session. An explicit maxChecks (including a deliberate 0 =
// unbounded) or an explicit rule list keeps the operator's value untouched.
const defaultGuardrailsMaxChecks = 200

// defaultGuardrailSpecs is the built-in ADVISORY rule set applied when a guardrails
// model is configured but the operator authored no explicit rules. It honours the
// issue's headline default — ON (advisory) for the network/MCP surfaces, OFF for
// local tools — without spending tokens until a model is configured (the model IS
// the opt-in-to-spend). Advisory means observe-only: a finding is an operator
// diagnostic, the call/result is byte-unchanged, so the operator measures the
// false-positive rate before promoting a rule to block/sanitize. Local tools
// (Read/Edit/Write/Bash/Grep/Glob) are deliberately NOT matched.
var defaultGuardrailSpecs = []modelhook.RuleSpec{
	// Outbound search/fetch args (a query/URL carrying a secret) AND inbound results
	// (a fetched page / search snippet carrying an injection).
	{Match: "WebSearch", Phases: []string{"pre", "post"}, Mode: string(modelhook.ModeAdvisory)},
	// WebFetch's risk is overwhelmingly the INBOUND page (injection); its outbound arg
	// is just a URL. Post only, matching the issue's default.
	{Match: "WebFetch", Phases: []string{"post"}, Mode: string(modelhook.ModeAdvisory)},
	// All MCP tools, both directions: outbound args (exfil into an MCP call body) and
	// inbound results (injection in an MCP server's response).
	{Match: "mcp__*", Phases: []string{"pre", "post"}, Mode: string(modelhook.ModeAdvisory)},
}

// effectiveGuardrailSpecs returns the rule specs to compile: the operator's explicit
// rules when any are configured, else the built-in default advisory set. usedDefaults
// reports which, so buildGuardrailsHooks can apply the default cost cap only when the
// defaults are in force.
func effectiveGuardrailSpecs(cfg Config) (specs []modelhook.RuleSpec, usedDefaults bool) {
	if len(cfg.GuardrailsRules) > 0 {
		out := make([]modelhook.RuleSpec, 0, len(cfg.GuardrailsRules))
		for i, gr := range cfg.GuardrailsRules {
			out = append(out, modelhook.RuleSpec{
				Match:      gr.Match,
				Phases:     gr.Phases,
				Mode:       gr.Mode,
				Prompt:     gr.Prompt,
				FailClosed: gr.FailClosed,
				Order:      i,
			})
		}
		return out, false
	}
	// No explicit rules: ship the default advisory set (the model being configured is
	// the opt-in). Copy with Order stamped so the matcher tiebreak is deterministic.
	out := make([]modelhook.RuleSpec, len(defaultGuardrailSpecs))
	for i, s := range defaultGuardrailSpecs {
		s.Order = i
		out[i] = s
	}
	return out, true
}

// guardrailsConfigured reports whether guardrails are switched on: a model is
// configured AND the master kill-switch is not set. A model with NO explicit rules
// is still ON — it takes the default advisory rule set (effectiveGuardrailSpecs),
// honouring the headline default. The kill-switch (--guardrails=off →
// GuardrailsDisabled) wins over any config.
func guardrailsConfigured(cfg Config) bool {
	if cfg.GuardrailsDisabled {
		return false
	}
	return cfg.GuardrailsModel != ""
}

// buildGuardrailsChecker constructs the engine-backed VerdictChecker over the
// supplied provider+model, mirroring buildAskAdjudicator: a tool-less one-turn child
// Engine, role "guardrail-checker" (which lands in roleFamily's "child" bucket — no
// new metrics label), the no-progress nudge disabled. Returns nil when the model
// does not resolve (defensive — Build already failed fast via
// normalizeGuardrailsModel).
func buildGuardrailsChecker(cfg Config, provReg *providerRegistry, provider port.LLMProvider, parentProviderID, parentModel string) modelhook.VerdictChecker {
	sel := strings.TrimSpace(cfg.GuardrailsModel)
	if sel == "" {
		// GuardrailsModel STAYS the enable gate: empty ⇒ no checker, byte-identical.
		return nil
	}
	// GUARDRAIL SLOT (ADR 0030, Phase 2): a configured `guardrail` slot SUPERSEDES the
	// --guardrails-model value (the model field still gates ON/OFF). Otherwise resolve
	// the configured model through the alias machinery exactly as before.
	var resolved string
	if gm, ok := resolveSlotModel(cfg, slotGuardrail, parentModel); ok {
		resolved = gm
	} else {
		resolved, _ = lookupModelAlias(cfg, sel)
	}
	if resolved == "" {
		// UseMock passes the literal through; an empty resolve here is the defensive
		// unreachable path (Build normalized the value fail-fast).
		resolved = sel
	}
	if resolved == "" {
		return nil
	}
	windowFn := childWindowFor(cfg, provReg, parentProviderID, resolved)
	deps := childEngineDepsForProvider(cfg, "guardrail-checker", provider, resolved, windowFn,
		tool.NewCatalog(), promptConfig(modelCfgFor(cfg, resolved), cfg.gitStatus), nil)
	// Disable the no-progress nudge: the checker caps at MaxTurns=1 and an empty
	// (verdict-less) first turn must end in exactly ONE provider call (treated as a
	// no-verdict failure), not be nudged into a second.
	deps.MaxNoProgressNudges = -1
	return engineGuardrailsChecker{engine: agent.NewEngine(deps)}
}

// compileGuardrailRules converts the supplied rule specs into the adapter's compiled
// rules. ok=false when NO rule compiled (every spec invalid). Invalid modes / empty
// matchers are logged and skipped (fail-soft per rule). A load-time WARN flags two
// equally-specific matchers (the resolve() tiebreak then favours the earlier).
func compileGuardrailRules(cfg Config, specs []modelhook.RuleSpec) ([]modelhook.CompiledRule, bool) {
	out := make([]modelhook.CompiledRule, 0, len(specs))
	seen := map[string]int{} // matcher → first order, for the equal-specificity WARN
	for i, gr := range specs {
		cr, ok := modelhook.CompileRule(gr)
		if !ok {
			cfg.diag().Log(context.Background(), port.LevelWarn,
				"guardrails: invalid rule skipped (bad match/mode/phases)",
				"match", gr.Match, "mode", gr.Mode)
			continue
		}
		if prev, dup := seen[gr.Match]; dup {
			cfg.diag().Log(context.Background(), port.LevelWarn,
				"guardrails: two rules share a matcher; the EARLIER configured rule wins on a tie",
				"match", gr.Match, "first_index", prev, "duplicate_index", i)
		} else {
			seen[gr.Match] = i
		}
		out = append(out, cr)
	}
	if len(out) == 0 {
		return nil, false
	}
	return out, true
}
