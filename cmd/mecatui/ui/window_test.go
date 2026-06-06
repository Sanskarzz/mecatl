package ui

import "testing"

// TestScrollWindow is the direct unit test for the shared follow-the-cursor window
// helper used by the slash palette, the @-mention menu, and the /models picker. It
// has no other direct test, so this protects all three call sites at once.
func TestScrollWindow(t *testing.T) {
	cases := []struct {
		name             string
		cursor, n, limit int
		wantStart        int
		wantEnd          int
	}{
		// Whole list fits ⇒ the full [0,n) regardless of cursor.
		{"fits exactly", 3, 5, 5, 0, 5},
		{"fits under limit", 0, 3, 8, 0, 3},
		// Cursor at the top of a clipped list ⇒ window anchored at 0.
		{"cursor at top", 0, 20, 6, 0, 6},
		// Cursor at the bottom ⇒ window ends at n, start == n-limit.
		{"cursor at bottom", 19, 20, 6, 14, 20},
		// Mid-list ⇒ centered on the cursor (start = cursor - limit/2).
		{"mid centered", 10, 20, 6, 7, 13},
		// Edge inputs the n<=limit short-circuit handles: an empty list and a zero
		// limit return safe [0,n) bounds, never panic. Callers always pass limit>=1
		// and n>=0, so a negative limit (which the short-circuit would not catch) is
		// unreachable and deliberately not asserted.
		{"empty list", 0, 0, 6, 0, 0},
		{"zero limit, empty", 0, 0, 0, 0, 0},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			start, end := scrollWindow(tc.cursor, tc.n, tc.limit)
			if start != tc.wantStart || end != tc.wantEnd {
				t.Errorf("scrollWindow(%d,%d,%d) = (%d,%d), want (%d,%d)",
					tc.cursor, tc.n, tc.limit, start, end, tc.wantStart, tc.wantEnd)
			}
			// Invariants that must hold for every covered case: bounds ordered + within
			// [0,n].
			if start < 0 || end > tc.n || start > end {
				t.Errorf("scrollWindow(%d,%d,%d) returned out-of-range bounds (%d,%d) for n=%d",
					tc.cursor, tc.n, tc.limit, start, end, tc.n)
			}
		})
	}
}
