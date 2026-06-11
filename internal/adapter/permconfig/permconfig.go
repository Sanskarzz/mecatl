package permconfig

import (
	"fmt"
	"strings"

	yaml "go.yaml.in/yaml/v3"

	"github.com/stacklok/mecatl/engine/governance"
)

// Defense-in-depth caps (CWE-770), mirroring permstore's per-session rule cap. A
// permission config is operator/project-authored, but the file is still untrusted
// input read on the hot path; an unbounded file or rule list would let a single
// pathological config balloon memory or per-evaluate work.
const (
	// maxConfigBytes rejects an over-large config file before parsing. A real
	// permission config is a short allow/ask/deny list — a few hundred KB is far
	// beyond any legitimate use.
	maxConfigBytes = 256 * 1024
	// maxRulesPerConfig caps how many rules one config file contributes; specs
	// beyond the cap are dropped (and reported), so the file keeps asking for the
	// uncovered calls rather than silently growing the merged rule set.
	maxRulesPerConfig = 1024
)

// errConfigTooLarge is returned by parse paths when the input exceeds
// maxConfigBytes. Callers log-and-skip it (fail-soft), like any other parse error.
func errConfigTooLarge(n int) error {
	return fmt.Errorf("permission config too large: %d bytes exceeds the %d-byte cap", n, maxConfigBytes)
}

// parseYAML unmarshals the `.mecatl/settings.yaml` bytes into a Config. A nil/
// empty input yields a zero Config (no rules). A malformed document is a hard
// error the caller surfaces (a config file that cannot be parsed must not be
// silently ignored — that would hide a typo that disables a deny rule).
func parseYAML(data []byte) (Config, error) {
	var cfg Config
	if len(data) > maxConfigBytes {
		return Config{}, errConfigTooLarge(len(data))
	}
	if len(strings.TrimSpace(string(data))) == 0 {
		return Config{}, nil
	}
	if err := yaml.Unmarshal(data, &cfg); err != nil {
		return Config{}, fmt.Errorf("parse permission config: %w", err)
	}
	return cfg, nil
}

// rulesFromConfig converts a parsed Config into governance.Rule values tagged
// with the given scope. The deny → ask → allow ordering in the returned slice is
// irrelevant (the Evaluator folds deny-dominant regardless); unparseable specs
// are collected into report so the caller can surface them.
func rulesFromConfig(cfg Config, scope governance.Scope, report *Report) []governance.Rule {
	var rules []governance.Rule
	add := func(specs []string, effect governance.Effect) {
		for _, spec := range specs {
			if len(rules) >= maxRulesPerConfig {
				report.addDropped(spec, "rule-count cap reached; rule dropped")
				continue
			}
			rule, ok := parseSpec(spec, scope, effect)
			if !ok {
				report.addDropped(spec, "unparseable rule spec")
				continue
			}
			rules = append(rules, rule)
		}
	}
	// Deny first for readability; effect, not order, decides precedence. Deny/ask
	// are added before allow so that, at the cap, the SAFER effects are kept and an
	// allow is the first to be dropped.
	add(cfg.Permissions.Deny, governance.Deny)
	add(cfg.Permissions.Ask, governance.Ask)
	add(cfg.Permissions.Allow, governance.Allow)
	return rules
}

// parseSpec parses a single rule spec of the form "Tool(pattern)" or bare "Tool"
// into a governance.Rule with the given scope and effect. It returns ok=false
// when the spec is empty or malformed (e.g. an unmatched parenthesis).
//
// The pattern is normalised via normalizeGlob so the Claude-style ":" prefix-glob
// ("go test:*") and the mecatl glob ("go test*") both resolve to the evaluator's
// glob grammar. Exact stays FALSE: config rules use glob semantics (only LEARNED
// rules are Exact — that invariant is preserved here).
func parseSpec(spec string, scope governance.Scope, effect governance.Effect) (governance.Rule, bool) {
	s := strings.TrimSpace(spec)
	if s == "" {
		return governance.Rule{}, false
	}
	open := strings.IndexByte(s, '(')
	if open < 0 {
		// Bare tool name, tool-wide rule (empty pattern matches any args).
		return governance.Rule{Scope: scope, Tool: s, Effect: effect}, true
	}
	if !strings.HasSuffix(s, ")") {
		return governance.Rule{}, false
	}
	toolName := strings.TrimSpace(s[:open])
	if toolName == "" {
		return governance.Rule{}, false
	}
	inner := s[open+1 : len(s)-1]
	return governance.Rule{
		Scope:   scope,
		Tool:    toolName,
		Pattern: normalizeGlob(inner),
		Effect:  effect,
	}, true
}

// normalizeGlob normalises a rule-spec pattern into the evaluator's glob grammar.
//
//   - The Claude-Code convention "<prefix>:*" (and the bare "<prefix>:") means a
//     prefix match: "go test:*" → "go test*", "git push:" → "git push*". This is
//     applied to ANY tool (Claude uses it for Bash; harmless elsewhere since the
//     ":" form is Claude-specific).
//   - An empty pattern stays empty (tool-wide).
//   - Everything else passes through unchanged (it is already a mecatl glob, e.g.
//     "go test*" or a file glob "src/**").
func normalizeGlob(pattern string) string {
	p := strings.TrimSpace(pattern)
	if p == "" {
		return ""
	}
	if i := strings.LastIndexByte(p, ':'); i >= 0 {
		head := p[:i]
		tail := p[i+1:]
		switch tail {
		case "*", "":
			// Prefix glob: match the head followed by anything.
			return head + "*"
		}
	}
	return p
}
