package agent

import "testing"

// TestTightenLimit is the unit table for the tighten-only clamp, including the
// inherited==0 (unlimited) branch: a positive override against an unlimited (0) bound
// TIGHTENS to the override; a nil/zero override is a no-op; a higher override never
// loosens. It relies on session.Limits treating 0 as unlimited (see tightenLimit's
// doc) — this table is the regression guard if that zero-semantics ever changes.
func TestTightenLimit(t *testing.T) {
	ptr := func(n int) *int { return &n }
	tests := []struct {
		name      string
		inherited int
		override  *int
		want      int
	}{
		{"unlimited inherited tightens to override", 0, ptr(5), 5},
		{"override lower wins", 5, ptr(3), 3},
		{"override higher does not loosen", 5, ptr(10), 5},
		{"nil override is a no-op", 5, nil, 5},
		{"zero override is a no-op", 5, ptr(0), 5},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			if got := tightenLimit(tc.inherited, tc.override); got != tc.want {
				t.Fatalf("tightenLimit(%d, %v) = %d, want %d", tc.inherited, tc.override, got, tc.want)
			}
		})
	}
}
