// Package permconfig is the file-based permission-config adapter (issue #13). It
// loads `.mecatl/settings.yaml`-style permission rules from disk and from
// Claude-Code-compatible `settings.json`, and resolves them PER SESSION against
// each session's workspace root via a permpolicy.RuleResolver.
//
// # What it produces
//
// Every loaded entry becomes a governance.Rule tagged with a config Scope
// (project vs user) and an Audience (issue #32): top-level allow/ask bind the
// MAIN engine only, the `subagent:` block binds child engines only, and
// top-level deny binds BOTH (a deny only tightens). The Resolver hands those
// rules to the policy on the SAME lowest-scope `extra` channel the learned-rule
// store rides — so a config rule never out-ranks a static/configured deny or
// ask, and a config Allow can only LOOSEN the built-in Ask floor
// (ScopeBuiltinDefault), never a deny.
//
// # The trust gate
//
// Project-level config is part of the repository the model is editing, so its
// ALLOW rules are gated behind a trust flag: an UNTRUSTED project's allows are
// dropped (the rule reverts to the built-in Ask/whatever a higher scope says),
// while its DENY and ASK rules are ALWAYS honoured (they only tighten). User-
// global config (under the user's XDG dir / home) is the operator's own and is
// always fully trusted. See Resolver.
//
// # Layering
//
// permconfig is an ADAPTER: it reads the filesystem (project files via the
// session tool.Workspace, user-global files via an injectable env), parses YAML/
// JSON, and emits domain governance.Rule values. It implements
// permpolicy.RuleResolver and is wired in internal/app. It imports no other
// adapter and is never imported by the domain/port/agent layers.
package permconfig

import (
	"fmt"
	"sort"
	"strings"

	yaml "go.yaml.in/yaml/v3"
)

// Config is the on-disk `.mecatl/settings.yaml` (or user-global settings.yaml)
// schema for file-based permissions. It is intentionally a small mirror of the
// Claude-Code permissions shape so a user familiar with one can read the other.
//
// Each list entry is a RULE SPEC string of the form "Tool(pattern)" or bare
// "Tool" (tool-wide). For Bash the pattern is a command glob, e.g.
// "Bash(go test*)". See parseSpec / normalizeGlob for the exact grammar.
//
// The TOP level of Config stays LENIENT (other keys — trustedWorkspaces etc. —
// must keep parsing); strictness applies only INSIDE the permissions: subtree,
// where a typo'd key would silently disable a rule list (see the custom
// UnmarshalYAML on Permissions / SubagentPermissions).
type Config struct {
	// Permissions holds the allow/ask/deny rule-spec lists plus the child-scoped
	// `subagent:` block.
	Permissions Permissions `yaml:"permissions"`
	// Guardrails holds the OPERATOR-TIER LLM-content-checker config (issue #27). It
	// is parsed STRICTLY (unknown sub-keys = error, like permissions:) so a typo
	// cannot silently disable a guardrail. It is honoured ONLY from the user-global +
	// CLI tiers; a project-tier file's guardrails: block is IGNORED with a WARN (a
	// project repo weakening/disabling a checker is a security DOWNGRADE — the usual
	// tighten-only gate reverses here). The presence flag (whether the key appeared at
	// all) is tracked via GuardrailsPresent so the resolver can WARN about an ignored
	// project block. A nil Guardrails means the key was absent.
	Guardrails *GuardrailsSection `yaml:"guardrails"`
}

// GuardrailsSection is the operator-tier `guardrails:` YAML subtree (issue #27): a
// checker model, the per-session check cap, a master-disable, and the rule list. It
// is parsed STRICTLY (unknown keys error).
type GuardrailsSection struct {
	// Model is the checker model id / alias. Empty leaves the CLI --guardrails-model
	// to supply it; a value here is overridden by the CLI flag when both are set.
	Model string `yaml:"model"`
	// MaxChecks is the per-session checker-call cap. 0 = unbounded.
	MaxChecks int `yaml:"maxChecks"`
	// MinContentBytes skips the checker for content shorter than this. 0 = check all.
	MinContentBytes int `yaml:"minContentBytes"`
	// Disabled is the YAML-level kill switch (the CLI --guardrails=off also sets it).
	Disabled bool `yaml:"disabled"`
	// Rules is the guardrail rule list.
	Rules []GuardrailRuleSpec `yaml:"rules"`
}

// GuardrailRuleSpec is one operator-tier guardrail rule as parsed from YAML. It is
// the on-disk mirror of app.GuardrailRule; composition maps the two. Parsed strictly.
type GuardrailRuleSpec struct {
	// Match is the tool-name matcher (exact / "prefix*" / "*").
	Match string `yaml:"match"`
	// Phases lists "pre"/"post"; empty = both.
	Phases []string `yaml:"phases"`
	// Mode is "block"/"sanitize"/"advisory"; empty defaults to block.
	Mode string `yaml:"mode"`
	// Prompt overrides the built-in inspection rubric.
	Prompt string `yaml:"prompt"`
	// FailClosed flips the fail-open default for enforcing modes.
	FailClosed bool `yaml:"failClosed"`
}

// UnmarshalYAML decodes the guardrails: mapping STRICTLY (issue #27): an unknown key
// inside the guardrails subtree is a parse error — a typo like `moddel:` or `rulez:`
// must not silently disable a guardrail. Same rationale as Permissions.UnmarshalYAML.
func (g *GuardrailsSection) UnmarshalYAML(node *yaml.Node) error {
	return decodeStrictMapping(node, "guardrails", map[string]any{
		"model":           &g.Model,
		"maxChecks":       &g.MaxChecks,
		"minContentBytes": &g.MinContentBytes,
		"disabled":        &g.Disabled,
		"rules":           &g.Rules,
	})
}

// UnmarshalYAML decodes a guardrails rule mapping STRICTLY.
func (r *GuardrailRuleSpec) UnmarshalYAML(node *yaml.Node) error {
	return decodeStrictMapping(node, "guardrails.rules[]", map[string]any{
		"match":      &r.Match,
		"phases":     &r.Phases,
		"mode":       &r.Mode,
		"prompt":     &r.Prompt,
		"failClosed": &r.FailClosed,
	})
}

// Permissions is the three-bucket rule-spec set plus the child-scoped
// `subagent:` block (issue #32). Resolution is deny-dominant, so a spec
// appearing in Deny always wins over the same spec in Allow regardless of
// bucket order here. Audience semantics (applied by rulesFromConfig):
// Allow/Ask bind the MAIN engine only; Deny binds BOTH main and subagents
// (tighten-only); the Subagent block binds child engines only.
type Permissions struct {
	// Allow lists rule specs that GRANT a tool call (effect Allow) on the MAIN
	// engine. Under an untrusted project these are DROPPED by the trust gate
	// (see Resolver).
	Allow []string `yaml:"allow"`
	// Ask lists rule specs that REQUIRE approval (effect Ask) on the MAIN
	// engine. Always honoured.
	Ask []string `yaml:"ask"`
	// Deny lists rule specs that BLOCK a tool call (effect Deny) EVERYWHERE —
	// main engine and subagents (a deny only tightens). Always honoured.
	Deny []string `yaml:"deny"`
	// Subagent holds the child-scoped rule-spec lists (issue #32): rules that
	// bind ONLY subagent/member/branch engines, resolved through the child-ask
	// model (a subagent allow can clear a substitution-floored ask; a subagent
	// ask surfaces to the human or auto-denies; a subagent deny blocks).
	Subagent SubagentPermissions `yaml:"subagent"`
}

// SubagentPermissions is the child-scoped allow/ask/deny rule-spec set
// (issue #32). Same spec grammar as the top-level buckets; every parsed rule is
// tagged AudienceSubagent so it binds only child engines. Project-tier subagent
// ALLOWS are trust-gated exactly like top-level allows; ask/deny always hold.
type SubagentPermissions struct {
	// Allow lists child-scoped rule specs with effect Allow (trust-gated for
	// project tiers).
	Allow []string `yaml:"allow"`
	// Ask lists child-scoped rule specs with effect Ask (always honoured). A
	// configured subagent Ask is NEVER auto-approved by the isolation carve-out —
	// it surfaces to a human or auto-denies.
	Ask []string `yaml:"ask"`
	// Deny lists child-scoped rule specs with effect Deny (always honoured).
	Deny []string `yaml:"deny"`
}

// UnmarshalYAML decodes the permissions: mapping STRICTLY (issue #32): an
// unknown key inside the permissions subtree is a parse error — surfaced
// through the existing per-file fail-soft log-and-skip — rather than silently
// ignored config (a typo like `alow:` or `subagnet:` would otherwise disable a
// whole rule list without a trace). The top level of Config stays lenient.
func (p *Permissions) UnmarshalYAML(node *yaml.Node) error {
	return decodeStrictMapping(node, "permissions", map[string]any{
		"allow":    &p.Allow,
		"ask":      &p.Ask,
		"deny":     &p.Deny,
		"subagent": &p.Subagent,
	})
}

// UnmarshalYAML decodes the permissions.subagent: mapping STRICTLY — same
// rationale as Permissions.UnmarshalYAML.
func (s *SubagentPermissions) UnmarshalYAML(node *yaml.Node) error {
	return decodeStrictMapping(node, "permissions.subagent", map[string]any{
		"allow": &s.Allow,
		"ask":   &s.Ask,
		"deny":  &s.Deny,
	})
}

// decodeStrictMapping walks a YAML mapping node and decodes each known key's
// value into its target, erroring on any unknown key (with the key's line for
// the operator). A null/absent node (e.g. a bare `permissions:` line) decodes
// to the zero value. A non-mapping node is an error — the subtree's shape is
// part of the strict contract.
func decodeStrictMapping(node *yaml.Node, where string, known map[string]any) error {
	if node == nil || node.Tag == "!!null" {
		return nil
	}
	if node.Kind != yaml.MappingNode {
		return fmt.Errorf("%s: expected a mapping (line %d)", where, node.Line)
	}
	for i := 0; i+1 < len(node.Content); i += 2 {
		keyNode, valNode := node.Content[i], node.Content[i+1]
		target, ok := known[keyNode.Value]
		if !ok {
			return fmt.Errorf("%s: unknown key %q (line %d); known keys: %s",
				where, keyNode.Value, keyNode.Line, knownKeyList(known))
		}
		if err := valNode.Decode(target); err != nil {
			return fmt.Errorf("%s.%s: %w", where, keyNode.Value, err)
		}
	}
	return nil
}

// knownKeyList renders the known-key set for the unknown-key error message in a
// stable (sorted) order, so a typo'd key names exactly the keys this mapping accepts.
func knownKeyList(known map[string]any) string {
	keys := make([]string, 0, len(known))
	for k := range known {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	return strings.Join(keys, ", ")
}
