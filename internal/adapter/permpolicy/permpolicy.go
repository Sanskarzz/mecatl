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
	"github.com/stacklok/mecatl/internal/session"
)

// Policy implements port.PermissionPolicy by delegating to a governance
// Evaluator. It maps the session.PermissionMode posture onto the Evaluator's
// plan-mode flag and forwards the tool call's name and raw arguments.
type Policy struct {
	eval *governance.Evaluator
}

// NewPolicy constructs a Policy over the given merged permission rules (across
// any mix of Scopes). Precedence (deny → ask → allow, higher Scope wins among
// same-effect conflicts) is resolved per call by the underlying Evaluator.
func NewPolicy(rules []governance.Rule) *Policy {
	return &Policy{eval: governance.NewEvaluator(rules)}
}

// Evaluate returns the permission decision for tool call c under mode. Plan mode
// (session.ModePlan) forces a deny for mutating tools (Edit/Write and non
// read-only Bash); all other modes evaluate purely against the rule set.
func (p *Policy) Evaluate(_ context.Context, mode session.PermissionMode, c session.ToolCall) governance.PermissionDecision {
	return p.eval.Evaluate(c.Name, c.Args, mode == session.ModePlan)
}
