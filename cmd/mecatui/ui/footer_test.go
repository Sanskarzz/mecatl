package ui

import (
	"strings"
	"testing"

	"github.com/stacklok/mecatl/cmd/mecatui/client"
	"github.com/stacklok/mecatl/cmd/mecatui/theme"
)

func TestHumanizeTokens(t *testing.T) {
	cases := []struct {
		in   int64
		want string
	}{
		{0, "0"},
		{1, "1"},
		{999, "999"},
		{1000, "1K"},
		{2000, "2K"},
		{7903, "7.9K"},
		{1500, "1.5K"},
		{999999, "1000K"},
		{1_000_000, "1M"},
		{1_200_000, "1.2M"},
		{-5, "0"},
	}
	for _, c := range cases {
		if got := humanizeTokens(c.in); got != c.want {
			t.Errorf("humanizeTokens(%d) = %q, want %q", c.in, got, c.want)
		}
	}
}

func TestCacheHitRate(t *testing.T) {
	cases := []struct {
		name string
		u    client.Usage
		want float64
	}{
		{"zero input", client.Usage{}, 0},
		{"half", client.Usage{InputTokens: 100, CacheReadTokens: 50}, 0.5},
		{"full", client.Usage{InputTokens: 100, CacheReadTokens: 100}, 1.0},
		{"88pct", client.Usage{InputTokens: 1500, CacheReadTokens: 1320}, 0.88},
	}
	for _, c := range cases {
		got := cacheHitRate(c.u)
		if diff := got - c.want; diff > 1e-9 || diff < -1e-9 {
			t.Errorf("%s: cacheHitRate = %v, want %v", c.name, got, c.want)
		}
	}
}

func TestPctString(t *testing.T) {
	cases := []struct {
		in   float64
		want string
	}{
		{0, "0%"},
		{0.5, "50%"},
		{0.881, "88%"},
		{1.0, "100%"},
		{1.5, "100%"},
		{-0.2, "0%"},
	}
	for _, c := range cases {
		if got := pctString(c.in); got != c.want {
			t.Errorf("pctString(%v) = %q, want %q", c.in, got, c.want)
		}
	}
}

func TestRenderContextMeterUnknownWindow(t *testing.T) {
	th := theme.New("aztec", theme.AztecPalette())
	got := renderContextMeter(th, 7903, 0)
	if got != "ctx 7.9K" {
		t.Errorf("unknown window: got %q, want %q", got, "ctx 7.9K")
	}
	// No bar glyphs when the window is unknown.
	if strings.ContainsAny(got, ctxGlyphOk+ctxGlyphWarn+ctxGlyphDanger+ctxGlyphEmpty) {
		t.Errorf("unknown window should have no bar, got %q", got)
	}
	// Negative used clamps to 0.
	if got := renderContextMeter(th, -10, 0); got != "ctx 0" {
		t.Errorf("negative used: got %q, want %q", got, "ctx 0")
	}
}

func TestRenderContextMeterKnownWindow(t *testing.T) {
	th := theme.New("aztec", theme.AztecPalette())
	// 40K / 200K = 20% → ok band.
	got := stripANSIstr(renderContextMeter(th, 40000, 200000))
	if !strings.Contains(got, "20%") {
		t.Errorf("expected 20%% in %q", got)
	}
	if !strings.Contains(got, "40K/200K") {
		t.Errorf("expected used/total 40K/200K in %q", got)
	}
	if !strings.ContainsAny(got, ctxGlyphOk+ctxGlyphEmpty) {
		t.Errorf("expected a bar in %q", got)
	}
}

// TestContextMeterPressureColours asserts each pressure band selects its own
// theme slot (colours differ) by comparing the raw ANSI output across bands.
func TestContextMeterPressureColours(t *testing.T) {
	th := theme.New("aztec", theme.AztecPalette())
	const window = 200000
	ok := renderContextMeter(th, 40000, window)      // 20% → ctxOk
	warn := renderContextMeter(th, 140000, window)   // 70% → ctxWarn
	danger := renderContextMeter(th, 190000, window) // 95% → ctxDanger

	if ok == warn || warn == danger || ok == danger {
		t.Errorf("expected distinct colouring across pressure bands:\nok=%q\nwarn=%q\ndanger=%q", ok, warn, danger)
	}
	// Sanity: slot selection by fraction.
	if s := ctxPressureSlot(0.2); s != "ctxOk" {
		t.Errorf("ctxPressureSlot(0.2) = %q, want ctxOk", s)
	}
	if s := ctxPressureSlot(0.70); s != "ctxWarn" {
		t.Errorf("ctxPressureSlot(0.70) = %q, want ctxWarn", s)
	}
	if s := ctxPressureSlot(0.90); s != "ctxDanger" {
		t.Errorf("ctxPressureSlot(0.90) = %q, want ctxDanger", s)
	}
}

// TestContextMeterPressureNonColour is the ACCESSIBILITY guard: pressure must be
// legible WITHOUT colour. It strips ANSI and asserts the three bands still
// differ — via the per-band fill glyph and the danger ⚠ marker — so a
// red/green-colourblind user (or anyone reading the stripped golden) can tell a
// 5% context from a 95% one.
func TestContextMeterPressureNonColour(t *testing.T) {
	th := theme.New("aztec", theme.AztecPalette())
	const window = 200000
	ok := stripANSIstr(renderContextMeter(th, 80000, window))      // 40% → ok (fills cells)
	warn := stripANSIstr(renderContextMeter(th, 140000, window))   // 70% → warn
	danger := stripANSIstr(renderContextMeter(th, 190000, window)) // 95% → danger

	if ok == warn || warn == danger || ok == danger {
		t.Errorf("pressure must differ with ANSI stripped:\nok=%q\nwarn=%q\ndanger=%q", ok, warn, danger)
	}
	// The bands carry distinct fill glyphs.
	if !strings.Contains(ok, ctxGlyphOk) {
		t.Errorf("ok band should use %q glyph: %q", ctxGlyphOk, ok)
	}
	if !strings.Contains(warn, ctxGlyphWarn) {
		t.Errorf("warn band should use %q glyph: %q", ctxGlyphWarn, warn)
	}
	if !strings.Contains(danger, ctxGlyphDanger) {
		t.Errorf("danger band should use %q glyph: %q", ctxGlyphDanger, danger)
	}
	// Danger also appends a non-colour textual cue; the others must not.
	if !strings.Contains(danger, ctxDangerMark) {
		t.Errorf("danger band should append the ⚠ marker: %q", danger)
	}
	if strings.Contains(ok, ctxDangerMark) || strings.Contains(warn, ctxDangerMark) {
		t.Errorf("only the danger band may show the ⚠ marker")
	}
}

func TestRenderUsageFacets(t *testing.T) {
	got := renderUsageFacets(client.Usage{
		InputTokens:      7903,
		OutputTokens:     345,
		CacheReadTokens:  6955,
		CacheWriteTokens: 1200,
	})
	for _, want := range []string{"↑7.9K", "↓345", "⊕1.2K", "cache 88%"} {
		if !strings.Contains(got, want) {
			t.Errorf("expected %q in facets %q", want, got)
		}
	}
}

// TestRenderUsageFacetsOmitsZeroCacheWrite keeps the segment scannable: the
// cache-write facet is hidden when zero.
func TestRenderUsageFacetsOmitsZeroCacheWrite(t *testing.T) {
	got := renderUsageFacets(client.Usage{InputTokens: 100, OutputTokens: 10, CacheReadTokens: 50})
	if strings.Contains(got, "⊕") {
		t.Errorf("zero cache-write should be omitted, got %q", got)
	}
}

// stripANSIstr is a string convenience over stripANSI for assertions.
func stripANSIstr(s string) string { return string(stripANSI([]byte(s))) }

// TestStopReasonLabel locks the human phrasing + style slot for every stop
// reason in the session.StopReason / proto Result.stop vocabulary, plus the
// empty and unknown fallbacks. The limit stops carry the warning slot; a clean
// end_turn / cancelled is muted; error is the error slot.
func TestStopReasonLabel(t *testing.T) {
	cases := []struct {
		stop string
		text string
		slot string
	}{
		{"end_turn", "done", "muted"},
		{"", "done", "muted"},
		{"max_turns", "stopped · turn limit", "ctxWarn"},
		{"max_tool_calls", "stopped · tool-call limit", "ctxWarn"},
		{"max_consecutive_failures", "stopped · repeated failures", "ctxWarn"},
		{"cancelled", "cancelled", "muted"},
		{"error", "error", "errorText"},
		{"some_future_reason", "some_future_reason", "muted"},
	}
	for _, c := range cases {
		text, slot := stopReasonLabel(c.stop)
		if text != c.text {
			t.Errorf("stopReasonLabel(%q) text = %q, want %q", c.stop, text, c.text)
		}
		if slot != c.slot {
			t.Errorf("stopReasonLabel(%q) slot = %q, want %q", c.stop, slot, c.slot)
		}
	}
}

// TestStopReasonLabelSanitizesUnknown asserts an unknown reason carrying an ESC
// byte is stripped before it reaches the footer (it is rendered via lipgloss,
// which would otherwise pass the escape through).
func TestStopReasonLabelSanitizesUnknown(t *testing.T) {
	text, _ := stopReasonLabel("evil\x1b[2Jreason")
	if strings.ContainsRune(text, 0x1b) {
		t.Errorf("unknown stop reason should be sanitized, got %q", text)
	}
}

// TestTeamWorkingCounts locks the (working, total) classification the footer
// k/N segment derives from a team's lanes: total is the lane count, working is the
// count of lanes NOT done — the SAME !ln.done predicate the roster glyphs use.
func TestTeamWorkingCounts(t *testing.T) {
	cases := []struct {
		name            string
		lanes           []teamLane
		wantWk, wantTot int
	}{
		{"empty", nil, 0, 0},
		{"all working", []teamLane{{}, {}, {}}, 3, 3},
		{"all done", []teamLane{{done: true}, {done: true}}, 0, 2},
		{"mixed", []teamLane{{done: true}, {}, {done: true}, {}}, 2, 4},
		{"single working", []teamLane{{}}, 1, 1},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			wk, tot := teamWorkingCounts(tc.lanes)
			if wk != tc.wantWk || tot != tc.wantTot {
				t.Errorf("teamWorkingCounts = (%d, %d), want (%d, %d)", wk, tot, tc.wantWk, tc.wantTot)
			}
		})
	}
}
