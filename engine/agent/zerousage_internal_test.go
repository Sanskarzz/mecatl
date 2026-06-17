package agent

import (
	"testing"

	"github.com/stacklok/mecatl/engine/session"
)

// emptyOverheadCounter is a TokenCounter that attributes a fixed positive cost EVEN to
// an empty conversation (CountMessages([]) == 100). The default HeuristicTokenCounter
// returns 0 for an empty slice, which would make the non-empty guard vacuous; this
// counter makes the boundary observable so the guard mutation-check is real.
type emptyOverheadCounter struct{}

func (emptyOverheadCounter) Count(string) int                    { return 0 }
func (emptyOverheadCounter) CountMessages([]session.Message) int { return 100 }

// TestEstimateZeroUsageInput pins the issue-#82 DISPLAY-ONLY zero-usage input fallback
// (Fix A1), including the empty-conversation BOUNDARY (SHOULD 1).
func TestEstimateZeroUsageInput(t *testing.T) {
	msgs := []session.Message{{Role: session.RoleUser, Text: "hello"}}

	t.Run("reported non-zero -> no estimate", func(t *testing.T) {
		// A provider-reported figure is authoritative; the fallback never fires.
		if est, ok := estimateZeroUsageInput(42, msgs, HeuristicTokenCounter{}); ok || est != 0 {
			t.Fatalf("estimateZeroUsageInput(42, ...) = (%d, %v), want (0, false)", est, ok)
		}
	})

	t.Run("zero reported, non-empty conversation -> estimate", func(t *testing.T) {
		est, ok := estimateZeroUsageInput(0, msgs, HeuristicTokenCounter{})
		if !ok || est <= 0 {
			t.Fatalf("estimateZeroUsageInput(0, non-empty, ...) = (%d, %v), want a positive estimate", est, ok)
		}
	})

	// BOUNDARY (SHOULD 1): zero reported AND an EMPTY conversation must NOT produce a
	// phantom estimate, EVEN with a counter that would attribute overhead to an empty
	// slice. This is the mutation-check for the `len(msgs) > 0` clause: drop it and a
	// counter like emptyOverheadCounter fabricates 100 input tokens from nothing.
	t.Run("zero reported, empty conversation -> no estimate (the load-bearing guard)", func(t *testing.T) {
		if est, ok := estimateZeroUsageInput(0, nil, emptyOverheadCounter{}); ok || est != 0 {
			t.Fatalf("estimateZeroUsageInput(0, empty, overhead-counter) = (%d, %v), want (0, false) — no phantom estimate", est, ok)
		}
	})
}
