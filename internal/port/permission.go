package port

import (
	"context"

	"github.com/stacklok/mecatl/internal/governance"
	"github.com/stacklok/mecatl/internal/session"
)

// PermissionPolicy evaluates a tool call under a permission mode, resolving
// across merged scopes with deny → ask → allow precedence. It is implemented by
// the permpolicy adapter (a session-aware wrapper over the session-free
// governance.Evaluator), not by governance itself, which cannot import session.
type PermissionPolicy interface {
	// Evaluate returns the permission decision for tool call c under mode, scoped
	// to sessionID so per-session LEARNED rules (see Learn) are consulted in
	// addition to the static rule set. The learned rules only ever ADD allows at
	// the lowest scope: a static deny/ask still wins, and plan mode still
	// hard-denies mutations BEFORE any learned rule is consulted.
	Evaluate(ctx context.Context, sessionID session.SessionID, mode session.PermissionMode, c session.ToolCall) governance.PermissionDecision

	// Learn records a per-session allow rule derived from tool call c (the model's
	// "allow always" verdict). It is a no-op when c is not safely learnable (a
	// compound/substituted Bash command, or a call with no targetable pattern —
	// see governance.LearnableRule). It NEVER overrides a deny or bypasses plan
	// mode: the learned rule is consulted by Evaluate at the lowest scope only.
	Learn(sessionID session.SessionID, c session.ToolCall)
}

// PermissionStore holds the per-session LEARNED permission rules an "allow
// always" verdict records. It is the small mutable seam the otherwise-immutable
// governance.Evaluator is missing: the Evaluator stays session-free and
// immutable; this store keys learned rules by session so the policy can merge
// them in per call. Implementations must be safe for concurrent use.
type PermissionStore interface {
	// Record stores a learned rule for sessionID. Implementations should dedupe
	// identical rules so repeated "allow always" for the same call does not grow
	// the set unbounded.
	Record(sessionID session.SessionID, rule governance.Rule)
	// Rules returns a snapshot (copy) of the rules learned for sessionID, safe for
	// the caller to read without holding any lock. An unknown session yields nil.
	Rules(sessionID session.SessionID) []governance.Rule
}
