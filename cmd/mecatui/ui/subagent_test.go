package ui

// Tests for inline Task subagent visibility: a Task tool card renders a REDACTED,
// metadata-only projection of its child run (goal title, live counts, expanded
// tool-name chips with an "args/results hidden" honesty note, and a resolved stat
// line with stop reason). The child's interior is isolated by design and never
// rendered.

import (
	"strings"
	"testing"

	"charm.land/lipgloss/v2"

	"github.com/stacklok/mecatl/cmd/mecatui/client"
)

// subagentCard builds a Task tool block, applies the given subagent.* projection
// via the conversation accumulators (by ParentCallID == toolID), and renders it.
func subagentCard(t *testing.T, expand bool, build func(c *conversation)) string {
	t.Helper()
	r := newTestRenderer()
	c := &conversation{}
	c.addTool("p1", "Task", `{"prompt":"investigate the loop"}`)
	build(c)
	return stripANSIstr(r.renderBlock(&c.blocks[0], expand))
}

// TestSubagentLiveCollapsed asserts the default (collapsed, unresolved) card: the
// goal title, a calm counts-only status line (tokens + tool count) with the
// ctrl+t trace affordance — and NO live current-tool name and NO elapsed clock.
func TestSubagentLiveCollapsed(t *testing.T) {
	out := subagentCard(t, false, func(c *conversation) {
		c.setSubagentStart("p1", "investigate the loop")
		c.addSubagentTool("p1", "Grep", false, 1)
		c.addSubagentTool("p1", "Read", false, 2)
	})
	if !strings.Contains(out, "investigate the loop") {
		t.Errorf("live card should show the goal title, got %q", out)
	}
	if !strings.Contains(out, "subagent ·") || !strings.Contains(out, "2 tools") {
		t.Errorf("live card should show a counts-only status line, got %q", out)
	}
	if !strings.Contains(out, "ctrl+t trace") {
		t.Errorf("live card should advertise the ctrl+t trace, got %q", out)
	}
	// No live current-tool name leaks into the collapsed line.
	if strings.Contains(out, "Grep") || strings.Contains(out, "Read") {
		t.Errorf("collapsed live card must not name the current child tool, got %q", out)
	}
}

// TestSubagentExpandedChips asserts the expanded (ctrl+t) card: a row of glyph +
// name chips for each child tool, under the "args/results hidden" honesty note.
func TestSubagentExpandedChips(t *testing.T) {
	out := subagentCard(t, true, func(c *conversation) {
		c.setSubagentStart("p1", "investigate the loop")
		c.addSubagentTool("p1", "Grep", false, 1)
		c.addSubagentTool("p1", "Read", true, 2)
	})
	if !strings.Contains(out, "args/results hidden") {
		t.Errorf("expanded card should carry the hidden-interior note, got %q", out)
	}
	if !strings.Contains(out, "Grep") || !strings.Contains(out, "Read") {
		t.Errorf("expanded card should show child tool chips, got %q", out)
	}
	if !strings.Contains(out, "✓") || !strings.Contains(out, "✗") {
		t.Errorf("expanded chips should carry ok/error glyphs, got %q", out)
	}
}

// TestSubagentExpandedCapsTrace asserts the expanded chip trace is capped at
// maxSubagentTrace, dropping the oldest chips.
func TestSubagentExpandedCapsTrace(t *testing.T) {
	out := subagentCard(t, true, func(c *conversation) {
		c.setSubagentStart("p1", "big investigation")
		for i := 0; i < maxSubagentTrace+5; i++ {
			c.addSubagentTool("p1", "Read", false, i+1)
		}
	})
	// The trace itself caps at maxSubagentTrace chips. Count the glyphs as a proxy.
	if got := strings.Count(out, "✓"); got != maxSubagentTrace {
		t.Errorf("expanded trace should cap at %d chips, got %d (%q)", maxSubagentTrace, got, out)
	}
}

// TestWrapChips asserts the expanded chip row wraps BETWEEN chips at the content
// width (visible-width measured) and never splits a chip: each output line stays
// within width, and every chip survives intact across the wrap.
func TestWrapChips(t *testing.T) {
	// Plain chips (no ANSI) so the width math is easy to reason about: each "✓ Read"
	// has visible width 6; with the 2-space separator, two chips + sep = 14.
	chips := []string{"✓ Read", "✓ Grep", "✗ Read", "✓ Glob", "✓ Read"}

	// Width 14 fits exactly two chips per line (6 + 2 + 6 = 14).
	out := wrapChips(chips, 14)
	lines := strings.Split(out, "\n")
	if len(lines) != 3 { // 2 + 2 + 1
		t.Fatalf("want 3 wrapped lines at width 14, got %d: %q", len(lines), lines)
	}
	for _, ln := range lines {
		if w := lipgloss.Width(ln); w > 14 {
			t.Errorf("line exceeds width 14 (got %d): %q", w, ln)
		}
	}
	// Every chip survives intact (no mid-chip split).
	for _, chip := range chips {
		if !strings.Contains(out, chip) {
			t.Errorf("chip %q was split or dropped by wrapping: %q", chip, out)
		}
	}

	// Width 0 disables wrapping: a single row joined by the separator.
	if got := wrapChips(chips, 0); strings.Contains(got, "\n") {
		t.Errorf("width 0 should not wrap, got %q", got)
	}
}

// TestSubagentExpandedWrapsNarrow asserts that, rendered into a narrow card, the
// expanded chip trace spans multiple lines (it wraps) and no rendered line is
// absurdly long — exercising the full renderTool → renderSubagent → wrapChips path.
func TestSubagentExpandedWrapsNarrow(t *testing.T) {
	r := newTestRenderer()
	r.setWidth(24) // narrow card
	c := &conversation{}
	c.addTool("p1", "Task", `{"prompt":"x"}`)
	c.setSubagentStart("p1", "narrow")
	for i := 0; i < 6; i++ {
		c.addSubagentTool("p1", "Read", false, i+1)
	}
	out := stripANSIstr(r.renderBlock(&c.blocks[0], true))
	// The chip region must occupy more than one visual line (it wrapped).
	if strings.Count(out, "✓") != 6 {
		t.Fatalf("want all 6 chips present, got %q", out)
	}
	// Find the chip lines and confirm at least two of them carry chips.
	chipLines := 0
	for _, ln := range strings.Split(out, "\n") {
		if strings.Contains(ln, "✓ Read") {
			chipLines++
		}
	}
	if chipLines < 2 {
		t.Errorf("expected the chip row to wrap across >=2 lines in a narrow card, got %d: %q", chipLines, out)
	}
}

// TestSubagentResolved asserts the resolved card: a single muted stat line with
// duration, token totals, final tool count, and the human stop label — no
// "0 writes", and the child's summary renders via the normal result body path.
func TestSubagentResolved(t *testing.T) {
	out := subagentCard(t, false, func(c *conversation) {
		c.setSubagentStart("p1", "investigate the loop")
		c.addSubagentTool("p1", "Grep", false, 1)
		c.setSubagentEnd("p1", client.Usage{InputTokens: 1200, OutputTokens: 80}, 4, "end_turn", 2500)
		c.resolveTool("p1", "found the bug in dispatch.go", false)
	})
	if !strings.Contains(out, "stop:done") {
		t.Errorf("resolved card should map end_turn → stop:done, got %q", out)
	}
	if !strings.Contains(out, "4 tools") {
		t.Errorf("resolved card should show the final tool count, got %q", out)
	}
	if !strings.Contains(out, "2.5s") {
		t.Errorf("resolved card should show a human duration, got %q", out)
	}
	if strings.Contains(out, "0 writes") {
		t.Errorf("resolved card must not show a writes count, got %q", out)
	}
	if !strings.Contains(out, "found the bug in dispatch.go") {
		t.Errorf("resolved card should render the child summary via the result body, got %q", out)
	}
}

// TestSubagentErrorResolves asserts a child error resolves the Task card with a
// "✗" glyph, stop:error, and the error text in the result slot.
func TestSubagentErrorResolves(t *testing.T) {
	out := subagentCard(t, false, func(c *conversation) {
		c.setSubagentStart("p1", "investigate the loop")
		c.setSubagentEnd("p1", client.Usage{}, 0, "error", 100)
		c.resolveTool("p1", "Task: subagent failed without producing a summary", true)
	})
	if !strings.Contains(out, "✗") {
		t.Errorf("errored card should carry the error glyph, got %q", out)
	}
	if !strings.Contains(out, "stop:error") {
		t.Errorf("errored card should show stop:error, got %q", out)
	}
	if !strings.Contains(out, "subagent failed") {
		t.Errorf("errored card should render the error text, got %q", out)
	}
}

// TestSubagentAttributionByParentCallID asserts subagent.* events are attributed
// to the correct Task card by ParentCallID, even with two Task cards interleaved.
func TestSubagentAttributionByParentCallID(t *testing.T) {
	c := &conversation{}
	c.addTool("pa", "Task", `{"prompt":"alpha"}`)
	c.addTool("pb", "Task", `{"prompt":"bravo"}`)

	if !c.setSubagentStart("pa", "alpha goal") || !c.setSubagentStart("pb", "bravo goal") {
		t.Fatalf("both starts should attribute")
	}
	c.addSubagentTool("pa", "Grep", false, 1)
	c.addSubagentTool("pb", "Read", false, 1)

	if c.blocks[0].subGoal != "alpha goal" || c.blocks[0].subTrace[0].name != "Grep" {
		t.Errorf("card pa mis-attributed: goal=%q trace=%+v", c.blocks[0].subGoal, c.blocks[0].subTrace)
	}
	if c.blocks[1].subGoal != "bravo goal" || c.blocks[1].subTrace[0].name != "Read" {
		t.Errorf("card pb mis-attributed: goal=%q trace=%+v", c.blocks[1].subGoal, c.blocks[1].subTrace)
	}
}

// TestSubagentMissAttributionIsSafe asserts a subagent.* event with no matching
// Task card is silently dropped (no panic, returns false).
func TestSubagentMissAttributionIsSafe(t *testing.T) {
	c := &conversation{}
	if c.setSubagentStart("nope", "goal") {
		t.Errorf("setSubagentStart should miss when no Task card matches")
	}
	if c.addSubagentTool("nope", "Read", false, 1) {
		t.Errorf("addSubagentTool should miss when no Task card matches")
	}
	if c.setSubagentEnd("nope", client.Usage{}, 0, "end_turn", 0) {
		t.Errorf("setSubagentEnd should miss when no Task card matches")
	}
}
