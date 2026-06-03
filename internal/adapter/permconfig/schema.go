// Package permconfig is the file-based permission-config adapter (issue #13). It
// loads `.mecatl/settings.yaml`-style permission rules from disk and from
// Claude-Code-compatible `settings.json`, and resolves them PER SESSION against
// each session's workspace root via a permpolicy.RuleResolver.
//
// # What it produces
//
// Every loaded entry becomes a governance.Rule tagged with a config Scope
// (project vs user). The Resolver hands those rules to the policy on the SAME
// lowest-scope `extra` channel the learned-rule store rides — so a config rule
// never out-ranks a static/configured deny or ask, and a config Allow can only
// LOOSEN the built-in Ask floor (ScopeBuiltinDefault), never a deny.
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

// Config is the on-disk `.mecatl/settings.yaml` (or user-global settings.yaml)
// schema for file-based permissions. It is intentionally a small mirror of the
// Claude-Code permissions shape so a user familiar with one can read the other.
//
// Each list entry is a RULE SPEC string of the form "Tool(pattern)" or bare
// "Tool" (tool-wide). For Bash the pattern is a command glob, e.g.
// "Bash(go test*)". See parseSpec / normalizeGlob for the exact grammar.
type Config struct {
	// Permissions holds the allow/ask/deny rule-spec lists.
	Permissions Permissions `yaml:"permissions"`
}

// Permissions is the three-bucket rule-spec set. Resolution is deny-dominant, so
// a spec appearing in Deny always wins over the same spec in Allow regardless of
// bucket order here.
type Permissions struct {
	// Allow lists rule specs that GRANT a tool call (effect Allow). Under an
	// untrusted project these are DROPPED by the trust gate (see Resolver).
	Allow []string `yaml:"allow"`
	// Ask lists rule specs that REQUIRE approval (effect Ask). Always honoured.
	Ask []string `yaml:"ask"`
	// Deny lists rule specs that BLOCK a tool call (effect Deny). Always honoured.
	Deny []string `yaml:"deny"`
}
