package governance

// Effect is the outcome of a permission evaluation. The harness resolves a tool
// call across merged scopes with deny → ask → allow precedence: any deny wins,
// otherwise any ask wins, otherwise allow.
type Effect string

const (
	// Deny blocks the tool call; the Reason teaches the model why.
	Deny Effect = "deny"
	// Ask pauses the loop for client approval.
	Ask Effect = "ask"
	// Allow permits the tool call to execute.
	Allow Effect = "allow"
)

// PermissionDecision is the immutable result of evaluating a tool call across
// scopes. Reason is surfaced to the model on a deny so it can adapt (and to the
// client on an ask).
type PermissionDecision struct {
	// Effect is the resolved effect.
	Effect Effect
	// Reason explains the decision; especially important on Deny and Ask.
	Reason string
}

// Scope identifies the configuration layer a permission rule originates from.
// Higher-precedence scopes override lower ones when rules are merged. The
// ordering (highest first) is: Managed > CLI > LocalProject > SharedProject >
// User, so a smaller Scope value has higher precedence.
type Scope int

const (
	// ScopeManaged is enterprise/managed policy; highest precedence.
	ScopeManaged Scope = iota
	// ScopeCLI is policy supplied on the command line / at invocation.
	ScopeCLI
	// ScopeLocalProject is the developer's local, un-shared project settings.
	ScopeLocalProject
	// ScopeSharedProject is checked-in, shared project settings.
	ScopeSharedProject
	// ScopeUser is the user's global settings; lowest precedence.
	ScopeUser
)

// HasHigherPrecedenceThan reports whether s overrides other when rules conflict.
func (s Scope) HasHigherPrecedenceThan(other Scope) bool {
	return s < other
}

// Rule is a single permission rule: a pattern matched against a tool call,
// the effect it yields, and the scope it came from. Evaluation (WP4) merges
// rules across scopes honouring Scope precedence and deny→ask→allow.
type Rule struct {
	// Scope is the configuration layer this rule originates from.
	Scope Scope
	// Tool is the tool name this rule applies to (empty matches any tool).
	Tool string
	// Pattern is the matcher against the tool's arguments (tool-specific
	// syntax, e.g. a Bash command glob); empty matches any arguments.
	Pattern string
	// Effect is the effect this rule yields on a match.
	Effect Effect
	// Exact, when true, requires Pattern to match the (canonicalized) argument
	// string LITERALLY — never via glob expansion. It is the safety floor for a
	// LEARNED allow (LearnableRule sets it): a learned allow for `git status`
	// must match only `git status`, never let a stray `*`/`?` in the learned text
	// widen into a glob that green-lights commands the user never approved
	// (glob-escalation). Static config rules leave it false and keep glob matching.
	Exact bool
}
