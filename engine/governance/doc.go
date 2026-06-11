// Package governance is the Governance bounded context: permission evaluation,
// hooks, and plan-mode gating. WP1 freezes only the value objects here
// (PermissionDecision, Scope, Rule, HookEvent, HookOutcome, the enums); the
// evaluation logic itself lands in WP4.
//
// Allowed imports (ARCHITECTURE.md §3): the standard library only (and other
// domain packages). It MUST NOT import adapter, agent, api, os, the OpenAI SDK,
// or any third-party library. In particular it does not import session, so that
// session may import it without a cycle.
package governance
