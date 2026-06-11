package wallclock

import (
	"testing"
	"time"
)

// TestNowTracksWallTime pins that Clock.Now reads the real wall clock:
// successive reads are non-decreasing and land within a sane delta of
// time.Now itself.
func TestNowTracksWallTime(t *testing.T) {
	c := Clock{}

	before := time.Now()
	first := c.Now()
	second := c.Now()
	after := time.Now()

	if second.Before(first) {
		t.Fatalf("Now went backwards: first=%v second=%v", first, second)
	}
	// Both reads must sit inside the [before, after] wall-time window,
	// widened by a generous slop for coarse clocks / scheduler delays.
	const slop = 5 * time.Second
	if first.Before(before.Add(-slop)) || first.After(after.Add(slop)) {
		t.Fatalf("first Now()=%v outside wall window [%v, %v]", first, before, after)
	}
	if second.Before(before.Add(-slop)) || second.After(after.Add(slop)) {
		t.Fatalf("second Now()=%v outside wall window [%v, %v]", second, before, after)
	}
}
