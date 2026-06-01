package permstore

import (
	"sync"
	"testing"

	"github.com/stacklok/mecatl/internal/governance"
	"github.com/stacklok/mecatl/internal/session"
)

func rule(tool, pattern string) governance.Rule {
	return governance.Rule{Scope: governance.ScopeUser, Tool: tool, Pattern: pattern, Effect: governance.Allow, Exact: true}
}

func TestRecordAndList(t *testing.T) {
	m := New()
	r := rule("Bash", "git status")
	m.Record("s1", r)
	got := m.Rules("s1")
	if len(got) != 1 || got[0] != r {
		t.Fatalf("expected one learned rule, got %+v", got)
	}
	// Idempotent: recording the identical rule does not grow the set.
	m.Record("s1", r)
	if got := m.Rules("s1"); len(got) != 1 {
		t.Fatalf("expected dedup to keep one rule, got %d", len(got))
	}
	// A distinct rule is added.
	m.Record("s1", rule("Bash", "git diff"))
	if got := m.Rules("s1"); len(got) != 2 {
		t.Fatalf("expected two distinct rules, got %d", len(got))
	}
}

func TestPerSessionKeying(t *testing.T) {
	m := New()
	m.Record("a", rule("Bash", "git status"))
	if got := m.Rules("b"); got != nil {
		t.Fatalf("session b must not see session a's rules, got %+v", got)
	}
	if got := m.Rules("a"); len(got) != 1 {
		t.Fatalf("session a should have its own rule, got %+v", got)
	}
}

func TestRulesReturnsCopy(t *testing.T) {
	m := New()
	m.Record("s1", rule("Bash", "git status"))
	snap := m.Rules("s1")
	snap[0] = rule("Bash", "rm -rf /") // mutate the returned slice
	if got := m.Rules("s1"); got[0].Pattern != "git status" {
		t.Fatalf("Rules must return a copy; internal state was mutated to %q", got[0].Pattern)
	}
}

func TestForgetEviction(t *testing.T) {
	m := New()
	m.Record("s1", rule("Bash", "git status"))
	m.Forget("s1")
	if got := m.Rules("s1"); got != nil {
		t.Fatalf("expected eviction to clear the session, got %+v", got)
	}
	m.Forget("unknown") // idempotent no-op
}

func TestConcurrentAccess(t *testing.T) {
	m := New()
	var wg sync.WaitGroup
	for i := 0; i < 50; i++ {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			id := session.SessionID("s")
			m.Record(id, rule("Bash", "cmd"))
			_ = m.Rules(id)
			if i%10 == 0 {
				m.Forget(session.SessionID("other"))
			}
		}(i)
	}
	wg.Wait()
	// All records were the same rule; dedup keeps exactly one.
	if got := m.Rules("s"); len(got) != 1 {
		t.Fatalf("expected one deduped rule after concurrent records, got %d", len(got))
	}
}
