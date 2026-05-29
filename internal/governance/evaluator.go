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
func (e *Evaluator) Evaluate(tool string, args json.RawMessage, planMode bool) PermissionDecision {
	if planMode {
		if d, blocked := planModeDecision(tool, args); blocked {
			return d
		}
	}
	return e.resolve(tool, args)
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
func (e *Evaluator) resolve(tool string, args json.RawMessage) PermissionDecision {
	if tool == "Bash" {
		return e.resolveBash(args)
	}
	pattern := nonBashPattern(tool, args)
	return e.resolveSimple(tool, pattern)
}

// resolveBash evaluates each canonicalized sub-command of a (possibly compound)
// Bash line and folds them with deny → ask → allow: the worst outcome wins.
func (e *Evaluator) resolveBash(args json.RawMessage) PermissionDecision {
	cmd := bashCommand(args)
	subs := SplitCommands(cmd)
	if len(subs) == 0 {
		// No parseable command: evaluate against the raw (empty) pattern.
		return e.resolveSimple("Bash", "")
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
			seg := e.resolveSimple("Bash", Canonicalize(sub))
			if effectRank(seg.Effect) >= effectRank(Ask) {
				d = seg // already Ask or Deny: keep its (more specific) reason
			} else {
				d = PermissionDecision{
					Effect: Ask,
					Reason: "Bash command contains command/process substitution or subshell grouping that may hide an inner command; client approval required",
				}
			}
		} else {
			d = e.resolveSimple("Bash", Canonicalize(sub))
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
func (e *Evaluator) resolveSimple(tool, pattern string) PermissionDecision {
	// Collect the highest-precedence matching rule per effect.
	best := map[Effect]*Rule{}
	for i := range e.rules {
		r := &e.rules[i]
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
