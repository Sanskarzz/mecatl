package permconfig

import (
	"encoding/json"
	"fmt"
	"strings"

	"github.com/stacklok/mecatl/engine/governance"
)

// claudeSettings is the subset of a Claude-Code `settings.json` this importer
// reads: the permissions allow/ask/deny rule-spec lists. Other keys are ignored.
type claudeSettings struct {
	Permissions struct {
		Allow []string `json:"allow"`
		Ask   []string `json:"ask"`
		Deny  []string `json:"deny"`
	} `json:"permissions"`
}

// Report records the LOSSY outcomes of importing/loading permission config so the
// composition layer can log exactly what was demoted, left inert, or dropped.
// Honest reporting is the contract: a fail-safe must never SILENTLY weaken intent.
type Report struct {
	// Demoted records Allow specs that were demoted to Ask for safety (e.g. a
	// Claude WebFetch(domain:...) allow — see importClaudeRules).
	Demoted []ReportEntry
	// Inert records specs that parsed but cannot match anything as written (e.g. a
	// Read(~/path) whose "~" is left unexpanded), so the operator knows the rule
	// has no effect.
	Inert []ReportEntry
	// Dropped records specs that could not be parsed at all and were discarded.
	Dropped []ReportEntry
}

// ReportEntry is one lossy outcome: the original spec and a human reason.
type ReportEntry struct {
	Spec   string
	Reason string
}

func (r *Report) addDemoted(spec, reason string) {
	r.Demoted = append(r.Demoted, ReportEntry{Spec: spec, Reason: reason})
}
func (r *Report) addInert(spec, reason string) {
	r.Inert = append(r.Inert, ReportEntry{Spec: spec, Reason: reason})
}
func (r *Report) addDropped(spec, reason string) {
	r.Dropped = append(r.Dropped, ReportEntry{Spec: spec, Reason: reason})
}

// Empty reports whether the report recorded any lossy outcome.
func (r *Report) Empty() bool {
	return len(r.Demoted) == 0 && len(r.Inert) == 0 && len(r.Dropped) == 0
}

// importClaude parses a Claude-Code settings.json byte payload and converts its
// permissions into governance.Rule values at the given scope, applying the
// fail-safe table below. It appends every lossy outcome to report. A malformed
// JSON document is a hard error (same stance as the YAML loader: a typo must not
// silently disable a deny).
func importClaude(data []byte, scope governance.Scope, report *Report) ([]governance.Rule, error) {
	if len(data) > maxConfigBytes {
		return nil, errConfigTooLarge(len(data))
	}
	if len(strings.TrimSpace(string(data))) == 0 {
		return nil, nil
	}
	var cs claudeSettings
	if err := json.Unmarshal(data, &cs); err != nil {
		return nil, fmt.Errorf("parse claude settings.json: %w", err)
	}
	var rules []governance.Rule
	rules = append(rules, importClaudeBucket(cs.Permissions.Deny, scope, governance.Deny, report)...)
	rules = append(rules, importClaudeBucket(cs.Permissions.Ask, scope, governance.Ask, report)...)
	rules = append(rules, importClaudeBucket(cs.Permissions.Allow, scope, governance.Allow, report)...)
	return rules, nil
}

// importClaudeBucket converts one Claude permission bucket, applying the fail-safe
// rules. The fail-safe table (issue #13 user decisions 4):
//
//	WebFetch(domain:x) in an ALLOW list  -> DEMOTE to Ask (substring/domain match
//	                                        is too risky to honour as a blanket
//	                                        allow; the user still gets to approve).
//	Read(~/...) (an unexpanded "~")       -> keep the rule but report it INERT (we
//	                                        deliberately do NOT expand "~" — the
//	                                        pattern won't match the absolute path a
//	                                        tool sees, so it is inert by design and
//	                                        reported so the operator knows).
//	unparseable spec                      -> DROP + report.
//
// The fail-safe NEVER widens: a demotion only ever moves Allow -> Ask (tighter),
// never the reverse, and deny/ask buckets are imported verbatim (they only
// tighten).
//
// Audience (issue #32): Claude settings have no subagent block, so imported
// buckets tag exactly like the mecatl TOP-LEVEL buckets (rulesFromConfig D1) —
// deny → AudienceAll (binds children too; tighten-only), ask/allow →
// AudienceMain (a demoted allow→ask is still from the allow bucket, so it stays
// AudienceMain). The audience is keyed on the BUCKET's effect, not the
// post-demotion effect.
func importClaudeBucket(specs []string, scope governance.Scope, effect governance.Effect, report *Report) []governance.Rule {
	audience := governance.AudienceMain
	if effect == governance.Deny {
		audience = governance.AudienceAll
	}
	var rules []governance.Rule
	for _, spec := range specs {
		eff := effect
		// Fail-safe 1: a WebFetch domain ALLOW is demoted to Ask.
		if eff == governance.Allow && isWebFetchDomainSpec(spec) {
			eff = governance.Ask
			report.addDemoted(spec, "WebFetch domain allow demoted to ask (domain match too risky to auto-allow)")
		}
		rule, ok := parseSpec(spec, scope, eff)
		if !ok {
			report.addDropped(spec, "unparseable rule spec")
			continue
		}
		rule.Audience = audience
		// Fail-safe 2: an unexpanded "~" in the pattern is left as-is and reported
		// inert (it cannot match the absolute path a tool resolves).
		if strings.Contains(rule.Pattern, "~") {
			report.addInert(spec, "leading \"~\" left unexpanded; rule is inert (never matches an absolute path)")
		}
		rules = append(rules, rule)
	}
	return rules
}

// isWebFetchDomainSpec reports whether spec is a WebFetch rule whose pattern uses
// the Claude "domain:" qualifier (e.g. "WebFetch(domain:example.com)"). Those are
// the specs we demote from Allow to Ask.
func isWebFetchDomainSpec(spec string) bool {
	s := strings.TrimSpace(spec)
	open := strings.IndexByte(s, '(')
	if open < 0 || !strings.HasSuffix(s, ")") {
		return false
	}
	if strings.TrimSpace(s[:open]) != "WebFetch" {
		return false
	}
	inner := strings.TrimSpace(s[open+1 : len(s)-1])
	return strings.HasPrefix(inner, "domain:")
}
