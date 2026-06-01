package governance

import (
	"encoding/json"
	"sort"
)

// readOnlyTools are tools whose use never mutates the workspace and are therefore
// always permitted under plan mode. Bash is classified per-command via
// ReadOnlyBash rather than appearing here.
var readOnlyTools = map[string]bool{
	"Read": true,
	"Grep": true,
	"Glob": true,
}

// mutatingTools are tools that always mutate and are unconditionally denied by
// plan mode. Bash is not listed: a Bash call is mutating only when its command
// is not read-only (see ReadOnlyBash).
var mutatingTools = map[string]bool{
	"Edit":  true,
	"Write": true,
}

// Evaluator resolves a tool call against a merged set of permission Rules using
// deny → ask → allow precedence across Scopes. It is session-free by design so
// that package governance can stay free of a session import (doc.go): callers in
// the adapter/agent layer translate session types into the primitive arguments
// Evaluate accepts.
//
// Resolution rules (doc 08 §10):
//   - A Deny in ANY scope beats an Allow or Ask in ANY scope.
//   - Otherwise an Ask in any scope beats an Allow.
//   - Among rules of the SAME effect, the highest-precedence Scope wins (its
//     Reason is reported).
//   - With no matching rule the result is Ask (the safe default: pause for the
//     client) — the harness never silently allows an unconfigured call.
type Evaluator struct {
	rules []Rule
}

// NewEvaluator constructs an Evaluator over the given merged rules. The rules may
// come from any mix of Scopes; precedence is resolved at evaluation time. The
// slice is copied so later mutation by the caller cannot affect the policy.
func NewEvaluator(rules []Rule) *Evaluator {
	cp := make([]Rule, len(rules))
	copy(cp, rules)
	return &Evaluator{rules: cp}
}

// Evaluate resolves the decision for a tool call. tool is the tool name, args is
// its raw JSON argument payload (used for Bash compound-command splitting and
// pattern matching), and planMode forces deny for mutating actions (plan mode).
// It evaluates against the Evaluator's static rules only (no learned extras).
func (e *Evaluator) Evaluate(tool string, args json.RawMessage, planMode bool) PermissionDecision {
	return e.EvaluateWith(tool, args, planMode, nil)
}

// EvaluateWith resolves the decision for a tool call against the Evaluator's
// static rules PLUS the caller-supplied extra rules (e.g. per-session LEARNED
// allows). The ordering is security-load-bearing:
//
//  1. The plan-mode gate runs FIRST, BEFORE any learned rule is consulted — so a
//     learned allow can NEVER bypass plan mode's hard-deny of a mutating action.
//  2. Only then does the deny → ask → allow rule engine run over the static rules
//     with extra APPENDED. Because resolution is deny-dominant across the WHOLE
//     merged set, a static (or any) Deny still beats a learned Allow, and a static
//     Ask still beats a learned Allow — the extras only ever ADD allows at the
//     lowest scope; they can never weaken a deny/ask that already matches.
//
// The static rules are never mutated: a fresh slice (static followed by extra) is
// resolved per call, preserving the Evaluator's immutability.
func (e *Evaluator) EvaluateWith(tool string, args json.RawMessage, planMode bool, extra []Rule) PermissionDecision {
	if planMode {
		if d, blocked := planModeDecision(tool, args); blocked {
			return d
		}
	}
	if len(extra) == 0 {
		return e.resolve(e.rules, tool, args)
	}
	merged := make([]Rule, 0, len(e.rules)+len(extra))
	merged = append(merged, e.rules...)
	merged = append(merged, extra...)
	return e.resolve(merged, tool, args)
}

// LearnableRule derives the per-session allow rule the Evaluator WOULD learn for
// a (tool, args) pair the user chose to "allow always", reusing the SAME pattern
// derivation the evaluator matches against — so a learned rule matches exactly the
// call it was learned from, no broader.
//
// It returns (rule, false) — DO NOT learn — in any case where learning would be
// unsafe or untargetable:
//
//   - Bash whose command splits into != 1 segment (a compound `a && b` / `a; b`),
//     OR contains command/process substitution or subshell grouping ($(...),
//     backticks, (...)), OR is empty. Learning a single literal from a compound or
//     substituted line could green-light a hidden destructive command, so we refuse.
//   - Any tool whose derived pattern is EMPTY (no targetable path/command field):
//     an empty pattern would match the tool TOOL-WIDE, which is broader than the
//     conservative tool+exact-pattern grant this feature promises. We refuse rather
//     than learn a tool-wide allow.
//
// When learnable, the returned Rule is {Tool, Pattern, Effect: Allow, Exact: true,
// Scope: <lowest precedence>} — Exact so it matches literally (never via glob),
// and lowest scope so it can never out-rank a configured rule of any effect.
func (*Evaluator) LearnableRule(tool string, args json.RawMessage) (Rule, bool) {
	pattern, ok := learnablePattern(tool, args)
	if !ok || pattern == "" {
		return Rule{}, false
	}
	return Rule{
		// ScopeUser is the LOWEST precedence (largest Scope value); a learned allow
		// can never out-rank a configured rule. Precedence only breaks SAME-effect
		// ties anyway, and deny/ask always beat allow regardless of scope.
		Scope:   ScopeUser,
		Tool:    tool,
		Pattern: pattern,
		Effect:  Allow,
		Exact:   true,
	}, true
}

// learnablePattern derives the exact canonical pattern a learned rule would carry
// for (tool, args), or ok=false when the call must not be learned. For Bash it
// requires EXACTLY one canonicalized segment with no substitution/grouping; for
// every other tool it reuses nonBashPattern (the path/target field). An empty
// derived pattern is returned as-is so the caller refuses a tool-wide grant.
func learnablePattern(tool string, args json.RawMessage) (string, bool) {
	if tool == "Bash" {
		cmd := bashCommand(args)
		if cmd == "" {
			return "", false
		}
		subs := SplitCommands(cmd)
		if len(subs) != 1 {
			// Compound (a && b, a; b, pipelines, ...): refuse — a single learned
			// literal could green-light a hidden command in another segment.
			return "", false
		}
		seg := subs[0]
		if HasSubstitutionOrGrouping(seg) {
			// $(...), backticks, (...): the inner program can't be soundly extracted,
			// so we never learn it.
			return "", false
		}
		return Canonicalize(seg), true
	}
	return nonBashPattern(tool, args), true
}

// planModeDecision applies the plan-mode read-only gate. It returns a Deny
// decision and true when the call is a mutating action that plan mode forbids;
// otherwise it returns false and the rule engine proceeds as normal.
func planModeDecision(tool string, args json.RawMessage) (PermissionDecision, bool) {
	if mutatingTools[tool] {
		return PermissionDecision{
			Effect: Deny,
			Reason: "plan mode is active: " + tool + " mutates the workspace and is not permitted; present a plan and exit plan mode first",
		}, true
	}
	if tool == "Bash" {
		if cmd := bashCommand(args); cmd != "" && !ReadOnlyBash(cmd) {
			return PermissionDecision{
				Effect: Deny,
				Reason: "plan mode is active: this Bash command is not read-only and is not permitted; present a plan and exit plan mode first",
			}, true
		}
	}
	// Read-only tools (Read/Grep/Glob) and read-only Bash fall through.
	return PermissionDecision{}, false
}

// resolve runs the deny → ask → allow rule engine for a single tool call. For
// Bash it splits compound commands and requires EVERY sub-command to be allowed:
// a deny on any sub-command (e.g. `rm` in `git status && rm -rf /`) denies the
// whole compound.
func (e *Evaluator) resolve(rules []Rule, tool string, args json.RawMessage) PermissionDecision {
	if tool == "Bash" {
		return e.resolveBash(rules, args)
	}
	pattern := nonBashPattern(tool, args)
	return e.resolveSimple(rules, tool, pattern)
}

// resolveBash evaluates each canonicalized sub-command of a (possibly compound)
// Bash line and folds them with deny → ask → allow: the worst outcome wins.
func (e *Evaluator) resolveBash(rules []Rule, args json.RawMessage) PermissionDecision {
	cmd := bashCommand(args)
	subs := SplitCommands(cmd)
	if len(subs) == 0 {
		// No parseable command: evaluate against the raw (empty) pattern.
		return e.resolveSimple(rules, "Bash", "")
	}
	worst := PermissionDecision{Effect: Allow, Reason: ""}
	haveDecision := false
	for _, sub := range subs {
		var d PermissionDecision
		if HasSubstitutionOrGrouping(sub) {
			// Command/process substitution or subshell grouping can smuggle an
			// arbitrary inner command past the operator splitter. We cannot
			// soundly extract the inner program, so fail safe: evaluate the
			// segment AND floor the result at Ask so an allow rule for the outer
			// literal can never silently approve a hidden destructive command.
			seg := e.resolveSimple(rules, "Bash", Canonicalize(sub))
			if effectRank(seg.Effect) >= effectRank(Ask) {
				d = seg // already Ask or Deny: keep its (more specific) reason
			} else {
				d = PermissionDecision{
					Effect: Ask,
					Reason: "Bash command contains command/process substitution or subshell grouping that may hide an inner command; client approval required",
				}
			}
		} else {
			d = e.resolveSimple(rules, "Bash", Canonicalize(sub))
		}
		if !haveDecision || effectRank(d.Effect) > effectRank(worst.Effect) {
			worst = d
			haveDecision = true
		}
	}
	return worst
}

// resolveSimple finds the winning decision for a single tool+pattern against the
// rule set using deny → ask → allow precedence, with Scope breaking same-effect
// ties.
func (*Evaluator) resolveSimple(rules []Rule, tool, pattern string) PermissionDecision {
	// Collect the highest-precedence matching rule per effect.
	best := map[Effect]*Rule{}
	for i := range rules {
		r := &rules[i]
		if !ruleMatches(r, tool, pattern) {
			continue
		}
		cur := best[r.Effect]
		if cur == nil || r.Scope.HasHigherPrecedenceThan(cur.Scope) {
			best[r.Effect] = r
		}
	}
	// deny → ask → allow precedence on the effect itself.
	for _, eff := range []Effect{Deny, Ask, Allow} {
		if r := best[eff]; r != nil {
			return PermissionDecision{Effect: eff, Reason: ruleReason(r, tool, pattern)}
		}
	}
	// No matching rule: default to Ask so an unconfigured call pauses for the
	// client rather than being silently allowed.
	return PermissionDecision{
		Effect: Ask,
		Reason: "no permission rule matched " + tool + "; client approval required",
	}
}

// effectRank orders effects by severity for compound folding: deny is worst.
func effectRank(e Effect) int {
	switch e {
	case Deny:
		return 3
	case Ask:
		return 2
	case Allow:
		return 1
	default:
		return 0
	}
}

// ruleMatches reports whether rule r applies to the given tool and pattern. An
// empty Rule.Tool matches any tool; an empty Rule.Pattern matches any arguments.
// A non-empty pattern is matched as a shell-style glob against the (already
// canonicalized) command/argument string, with an exact-match fast path.
func ruleMatches(r *Rule, tool, pattern string) bool {
	if r.Tool != "" && r.Tool != tool {
		return false
	}
	if r.Pattern == "" {
		return true
	}
	if r.Pattern == pattern {
		return true
	}
	// Exact rules (e.g. a LEARNED allow) match LITERALLY only — never via glob.
	// This is the glob-escalation floor: a learned pattern that happens to contain
	// a `*`/`?` must not widen into a glob that approves un-approved commands. We
	// already returned true above on the literal equality fast-path, so an Exact
	// rule that did not equal `pattern` simply does not match.
	if r.Exact {
		return false
	}
	return globMatch(r.Pattern, pattern)
}

// globMatch reports whether the glob pattern matches s. Unlike path.Match it does
// NOT treat "/" as a path separator (command strings contain slashes that "*"
// must be able to span, e.g. `rm *` matching `rm -rf /`). It supports "*" (any
// run, including empty) and "?" (any single rune). It is intentionally simple;
// rules needing richer matching should use exact patterns.
func globMatch(pattern, s string) bool {
	p := []rune(pattern)
	str := []rune(s)
	// Iterative backtracking matcher (linear in practice).
	var pi, si, star, mark int
	star = -1
	for si < len(str) {
		switch {
		case pi < len(p) && (p[pi] == str[si] || p[pi] == '?'):
			pi++
			si++
		case pi < len(p) && p[pi] == '*':
			star = pi
			mark = si
			pi++
		case star != -1:
			pi = star + 1
			mark++
			si = mark
		default:
			return false
		}
	}
	for pi < len(p) && p[pi] == '*' {
		pi++
	}
	return pi == len(p)
}

// ruleReason returns a human-readable reason for the winning rule, falling back
// to a synthesized message when the rule carries none implicitly.
func ruleReason(r *Rule, tool, pattern string) string {
	switch r.Effect {
	case Deny:
		return "denied by rule for " + describeTarget(tool, r.Pattern, pattern)
	case Ask:
		return "approval required by rule for " + describeTarget(tool, r.Pattern, pattern)
	default:
		return "allowed by rule for " + describeTarget(tool, r.Pattern, pattern)
	}
}

func describeTarget(tool, rulePattern, pattern string) string {
	if rulePattern != "" {
		return tool + " (" + rulePattern + ")"
	}
	if pattern != "" {
		return tool + " (" + pattern + ")"
	}
	return tool
}

// nonBashPattern derives the pattern string to match a non-Bash tool call
// against. It uses the most common path/target field of the built-in tools so
// rules can target specific files; unknown shapes fall back to the empty string
// (which only matches tool-wide rules).
func nonBashPattern(_ string, args json.RawMessage) string {
	if len(args) == 0 {
		return ""
	}
	var m map[string]json.RawMessage
	if err := json.Unmarshal(args, &m); err != nil {
		return ""
	}
	for _, key := range []string{"path", "file_path", "pattern", "url"} {
		if raw, ok := m[key]; ok {
			var s string
			if json.Unmarshal(raw, &s) == nil {
				return s
			}
		}
	}
	return ""
}

// bashCommand extracts the command string from a Bash tool call's arguments,
// tolerating the common "command" / "cmd" field names. An empty result means the
// arguments could not be parsed.
func bashCommand(args json.RawMessage) string {
	if len(args) == 0 {
		return ""
	}
	var m map[string]json.RawMessage
	if err := json.Unmarshal(args, &m); err != nil {
		return ""
	}
	for _, key := range sortedKeysPreferred(m, "command", "cmd") {
		var s string
		if json.Unmarshal(m[key], &s) == nil && s != "" {
			return s
		}
	}
	return ""
}

// sortedKeysPreferred returns preferred keys first (in the given order) followed
// by any remaining keys sorted, so command extraction is deterministic.
func sortedKeysPreferred(m map[string]json.RawMessage, preferred ...string) []string {
	out := make([]string, 0, len(m))
	seen := map[string]bool{}
	for _, p := range preferred {
		if _, ok := m[p]; ok {
			out = append(out, p)
			seen[p] = true
		}
	}
	rest := make([]string, 0, len(m))
	for k := range m {
		if !seen[k] {
			rest = append(rest, k)
		}
	}
	sort.Strings(rest)
	return append(out, rest...)
}

// IsReadOnlyTool reports whether a non-Bash tool is unconditionally read-only.
// Exposed for callers (dispatch, plan-mode) that need the same classification.
func IsReadOnlyTool(tool string) bool {
	return readOnlyTools[tool]
}
