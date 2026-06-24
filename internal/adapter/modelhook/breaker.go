package modelhook

import "sync"

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
