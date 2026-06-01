// Package permstore is the in-memory, per-session store of LEARNED permission
// rules — the rules an ACP "allow always" verdict records (issue #3).
//
// It is deliberately the SMALLEST possible mutable seam around the otherwise
// immutable, session-free governance.Evaluator: the Evaluator never learns; the
// agent loop, on an allow-always verdict, asks the permission policy to Learn,
// which derives a narrow tool+exact-pattern rule (governance.LearnableRule) and
// Records it here, keyed by session. On every subsequent evaluation the policy
// reads this session's rules back and merges them in at the LOWEST scope.
//
// Scope of the store (by design, conservative slice):
//
//   - PER-SESSION: a rule learned in session A is invisible to session B.
//   - IN-MEMORY / NON-DURABLE: rules are lost on process restart and on
//     Forget(sessionID) (wired into Service.CloseSession). Durable cross-restart
//     persistence is a tracked follow-up, not this slice.
//   - EVICTION is best-effort: Forget runs from Service.CloseSession, which today
//     only the ACP adapter calls (on editor disconnect). Over gRPC/HTTP (no
//     session-end signal) a session's rules persist until process exit — bounded
//     and per-session-isolated, but not reclaimed mid-process. There is no cap on
//     the number of distinct rules per session; each one still requires a human
//     allow-always approval. A per-session cap and a gRPC/HTTP session-end hook
//     are tracked follow-ups.
//
// It implements port.PermissionStore and is safe for concurrent use.
package permstore

import (
	"sync"

	"github.com/stacklok/mecatl/internal/governance"
	"github.com/stacklok/mecatl/internal/session"
)

// Memory is the in-memory per-session learned-rule store. The zero value is NOT
// usable; construct it with New.
type Memory struct {
	mu        sync.Mutex
	bySession map[session.SessionID][]governance.Rule
}

// New constructs an empty Memory store.
func New() *Memory {
	return &Memory{bySession: make(map[session.SessionID][]governance.Rule)}
}

// Record appends rule to sessionID's learned set, deduping identical rules so a
// repeated "allow always" for the same call does not grow the slice unbounded.
// It is concurrency-safe.
func (m *Memory) Record(sessionID session.SessionID, rule governance.Rule) {
	m.mu.Lock()
	defer m.mu.Unlock()
	for _, existing := range m.bySession[sessionID] {
		if existing == rule {
			return // idempotent: identical rule already learned
		}
	}
	m.bySession[sessionID] = append(m.bySession[sessionID], rule)
}

// Rules returns a COPY of sessionID's learned rules, so the caller can read it
// without holding the lock and a later Record cannot mutate the returned slice.
// An unknown session yields nil.
func (m *Memory) Rules(sessionID session.SessionID) []governance.Rule {
	m.mu.Lock()
	defer m.mu.Unlock()
	src := m.bySession[sessionID]
	if len(src) == 0 {
		return nil
	}
	out := make([]governance.Rule, len(src))
	copy(out, src)
	return out
}

// Forget evicts all learned rules for sessionID. The composition layer wires it
// into Service.CloseSession so a session's learned rules do not outlive it (they
// are non-durable by design). It is idempotent: an unknown session is a no-op.
func (m *Memory) Forget(sessionID session.SessionID) {
	m.mu.Lock()
	defer m.mu.Unlock()
	delete(m.bySession, sessionID)
}
