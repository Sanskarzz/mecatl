package modelhook

import "sync"

// checkBudget is the per-SESSION guardrail checker call cap: infrastructure spend
// must not starve the agent, so the number of checker LLM calls a session may incur
// is bounded SEPARATELY from the parent's token budget — not folded into it. A fresh
// checkBudget is constructed per session in the composition factory (one Runner per
// session), so the cap is naturally per-session and resets when a new session's
// Runner is built.
//
// Its mutex SERIALIZES the budget check + decrement so a concurrent tool fan-out
// cannot overspend the cap, and warned latches the one-time "budget exhausted"
// diagnostic so the operator channel is not spammed once the cap is hit.
type checkBudget struct {
	mu     sync.Mutex
	used   int
	max    int  // 0 disables the cap (unbounded checks)
	warned bool // the budget-exhausted diagnostic fired once this session
}

// newCheckBudget builds a budget with the given cap. limit <= 0 disables the cap.
func newCheckBudget(limit int) *checkBudget {
	return &checkBudget{max: limit}
}

// admit reserves one checker call against the budget. It returns ok=true when a call
// is permitted (and counts it), ok=false when the cap is exhausted. firstExhaustion
// is true exactly once — on the call that first hits the cap — so the caller emits
// the budget-exhausted diagnostic a single time per session.
func (b *checkBudget) admit() (ok bool, firstExhaustion bool) {
	b.mu.Lock()
	defer b.mu.Unlock()
	if b.max <= 0 {
		return true, false // unbounded
	}
	if b.used >= b.max {
		if !b.warned {
			b.warned = true
			return false, true
		}
		return false, false
	}
	b.used++
	return true, false
}

// failureStreak tracks CONSECUTIVE checker failures within a session so a
// persistently-down checker escalates to a single sticky "checker DOWN" WARN rather
// than a per-call WARN flood an operator tunes out. A completed verdict (safe or
// unsafe) resets the streak. It is per-session (one per Runner) and its mutex
// serializes the count under a concurrent tool fan-out.
type failureStreak struct {
	mu        sync.Mutex
	count     int
	threshold int
	escalated bool // the DOWN WARN fired once for the current outage
}

// fail records one consecutive checker failure and reports whether THIS failure is
// the one that first crosses the threshold (down=true exactly once per outage), so
// the caller emits the sticky DOWN WARN a single time until a verdict resets it. n is
// the current consecutive count for the WARN.
func (f *failureStreak) fail() (down bool, n int) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.count++
	if f.count >= f.threshold && !f.escalated {
		f.escalated = true
		return true, f.count
	}
	return false, f.count
}

// reset clears the consecutive-failure streak (a completed verdict means the checker
// is answering again), re-arming the one-time DOWN WARN for any future outage.
func (f *failureStreak) reset() {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.count = 0
	f.escalated = false
}
