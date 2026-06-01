// Package permpolicy adapts the session-free permission Evaluator in package
// governance to the port.PermissionPolicy interface, which is expressed in terms
// of session types (session.PermissionMode, session.ToolCall).
//
// It lives in the adapter layer rather than in package governance for a
// structural reason: governance MUST NOT import session (session already imports
// governance, so the reverse edge would form an import cycle, and governance's
// doc.go forbids it). port.PermissionPolicy's signature uses session types, so
// any concrete implementation of it must import session — and therefore cannot
// live in governance. This package is that thin translation seam; the actual
// deny → ask → allow logic, compound-Bash splitting and plan-mode gating all
// live in package governance and are unit-tested there.
package permpolicy

import (
	"context"

	"github.com/stacklok/mecatl/internal/governance"
	"github.com/stacklok/mecatl/internal/port"
	"github.com/stacklok/mecatl/internal/session"
)

// Policy implements port.PermissionPolicy by delegating to a governance
// Evaluator. It maps the session.PermissionMode posture onto the Evaluator's
// plan-mode flag and forwards the tool call's name and raw arguments.
//
// It also threads an optional per-session LEARNED-rule store (issue #3): on an
// "allow always" verdict the agent loop calls Learn, which derives a narrow
// tool+exact-pattern rule and records it in the store; every Evaluate then merges
// that session's learned rules in at the LOWEST scope. The store is optional —
// when nil, Learn is a no-op and Evaluate behaves exactly as the static policy.
type Policy struct {
	eval  *governance.Evaluator
	store port.PermissionStore
}

// NewPolicy constructs a Policy over the given merged permission rules (across
// any mix of Scopes) and an optional per-session learned-rule store. Precedence
// (deny → ask → allow, higher Scope wins among same-effect conflicts) is resolved
// per call by the underlying Evaluator. A nil store disables rule learning: Learn
// is a no-op and Evaluate consults only the static rules.
func NewPolicy(rules []governance.Rule, store port.PermissionStore) *Policy {
	return &Policy{eval: governance.NewEvaluator(rules), store: store}
}

// Evaluate returns the permission decision for tool call c under mode, scoped to
// sessionID. Plan mode (session.ModePlan) forces a deny for mutating tools
// (Edit/Write and non read-only Bash) BEFORE any learned rule is consulted; all
// other modes evaluate against the static rule set PLUS this session's learned
// allows (when a store is configured). Because resolution is deny-dominant across
// the whole merged set, a learned allow can never override a static deny/ask.
func (p *Policy) Evaluate(_ context.Context, sessionID session.SessionID, mode session.PermissionMode, c session.ToolCall) governance.PermissionDecision {
	var learned []governance.Rule
	if p.store != nil {
		learned = p.store.Rules(sessionID)
	}
	return p.eval.EvaluateWith(c.Name, c.Args, mode == session.ModePlan, learned)
}

// Learn records a per-session allow rule for tool call c (the model's "allow
// always" verdict). It derives the rule via governance.LearnableRule and stores
// it only when the call is safely learnable (a single, non-substituted Bash
// command, or a non-Bash call with a targetable pattern); otherwise it is a
// no-op. With no store configured it is always a no-op.
func (p *Policy) Learn(sessionID session.SessionID, c session.ToolCall) {
	if p.store == nil {
		return
	}
	if rule, ok := p.eval.LearnableRule(c.Name, c.Args); ok {
		p.store.Record(sessionID, rule)
	}
}
