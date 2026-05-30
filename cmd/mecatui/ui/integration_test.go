package ui

import (
	"strings"
	"testing"

	tea "charm.land/bubbletea/v2"

	"github.com/stacklok/mecatl/cmd/mecatui/client"
	"github.com/stacklok/mecatl/cmd/mecatui/theme"
)

// TestFooterContextMeterWithWindow drives a result through Update and asserts the
// footer shows a populated context meter (bar + percentage + used/total) when a
// context window is configured, plus the session usage facets.
func TestFooterContextMeterWithWindow(t *testing.T) {
	th := theme.New("aztec", theme.AztecPalette())
	m := New(Deps{
		Theme:         th,
		ContextWindow: 200000,
		Model:         "mock-model",
	})
	m = applyAll(m, tea.WindowSizeMsg{Width: 120, Height: 30})
	m = applyAll(m, client.ResultMsg{
		Stop:  "end_turn",
		Usage: client.Usage{InputTokens: 40000, OutputTokens: 345, CacheReadTokens: 35200},
	})

	footer := stripANSIstr(m.renderFooter())
	if !strings.Contains(footer, "ctx ") || !strings.ContainsAny(footer, ctxGlyphOk+ctxGlyphEmpty) {
		t.Errorf("expected a context bar in footer:\n%s", footer)
	}
	if !strings.Contains(footer, "20%") {
		t.Errorf("expected 20%% context in footer:\n%s", footer)
	}
	if !strings.Contains(footer, "40K/200K") {
		t.Errorf("expected used/total in footer:\n%s", footer)
	}
	if !strings.Contains(footer, "↑40K") || !strings.Contains(footer, "↓345") {
		t.Errorf("expected usage facets in footer:\n%s", footer)
	}
	if !strings.Contains(footer, "cache 88%") {
		t.Errorf("expected cache-hit rate in footer:\n%s", footer)
	}
}

// TestFooterContextMeterUnknownWindow asserts the meter degrades to just the
// current size when no window is known.
func TestFooterContextMeterUnknownWindow(t *testing.T) {
	m := New(Deps{Theme: theme.New("aztec", theme.AztecPalette())})
	m = applyAll(m, tea.WindowSizeMsg{Width: 120, Height: 30})
	m = applyAll(m, client.ResultMsg{Stop: "end_turn", Usage: client.Usage{InputTokens: 7903}})

	footer := stripANSIstr(m.renderFooter())
	if !strings.Contains(footer, "ctx 7.9K") {
		t.Errorf("expected bare context size in footer:\n%s", footer)
	}
	if strings.ContainsAny(footer, ctxGlyphOk+ctxGlyphWarn+ctxGlyphDanger+ctxGlyphEmpty) {
		t.Errorf("expected no bar without a window:\n%s", footer)
	}
}

// TestFooterNarrowWidthTiers asserts the footer sheds detail in priority order
// as width shrinks — facets first, the context signal last. Context % must
// survive in every tier where anything fits beside the left status.
func TestFooterNarrowWidthTiers(t *testing.T) {
	th := theme.New("aztec", theme.AztecPalette())
	m := New(Deps{Theme: th, ContextWindow: 200000})
	m = applyAll(m, client.ResultMsg{
		Stop:  "end_turn",
		Usage: client.Usage{InputTokens: 140000, OutputTokens: 345, CacheReadTokens: 70000},
	})
	left := "ready"

	// Wide: full tier — meter (with used/total) AND facets.
	wide := stripANSIstr(m.fitFooter(left, 120))
	if !strings.Contains(wide, "140K/200K") || !strings.Contains(wide, "↑140K") {
		t.Errorf("wide footer should be the full tier:\n%q", wide)
	}

	// Medium: meter survives, facets dropped.
	med := stripANSIstr(m.fitFooter(left, 40))
	if strings.Contains(med, "↑140K") {
		t.Errorf("medium footer should drop io/cache facets first:\n%q", med)
	}
	if !strings.Contains(med, "ctx ") || !strings.Contains(med, "70%") {
		t.Errorf("medium footer should keep the context signal:\n%q", med)
	}

	// Tight: even the minimal bar-less percentage must carry the context %.
	tight := stripANSIstr(m.fitFooter(left, 22))
	if !strings.Contains(tight, "ctx ") || !strings.Contains(tight, "70%") {
		t.Errorf("tight footer should still show ctx %%:\n%q", tight)
	}
	if strings.Contains(tight, "140K/200K") {
		t.Errorf("tight footer should not carry used/total:\n%q", tight)
	}

	// Too narrow for anything: just the left status, no usage bleed-through.
	none := stripANSIstr(m.fitFooter(left, 10))
	if strings.Contains(none, "ctx") {
		t.Errorf("ultra-narrow footer should drop the usage segment entirely:\n%q", none)
	}
}

// TestHeaderTruncatesLongModel asserts a long model id is capped in the header.
func TestHeaderTruncatesLongModel(t *testing.T) {
	long := "anthropic/claude-opus-4-8-with-a-really-long-suffix-2026"
	m := New(Deps{Theme: theme.New("aztec", theme.AztecPalette()), Model: long})
	m = applyAll(m, tea.WindowSizeMsg{Width: 200, Height: 30})
	header := stripANSIstr(m.renderHeader())
	if strings.Contains(header, long) {
		t.Errorf("long model id should be truncated in header:\n%q", header)
	}
	if !strings.Contains(header, "…") {
		t.Errorf("truncated model should carry an ellipsis:\n%q", header)
	}
	if !strings.Contains(header, "anthropic/claude") {
		t.Errorf("truncation should keep the model prefix:\n%q", header)
	}
}

// TestEditCardRendersDiffInConversation drives an Edit tool call through the
// conversation and asserts the rendered viewport shows a red/green diff (not raw
// JSON args).
func TestEditCardRendersDiffInConversation(t *testing.T) {
	m := New(Deps{Theme: theme.New("aztec", theme.AztecPalette())})
	m = applyAll(m, tea.WindowSizeMsg{Width: 100, Height: 30})
	m.phase = phaseRunning
	m = applyAll(m,
		client.ToolCallMsg{ID: "e1", Name: "Edit", Args: `{"path":"x.go","old_string":"foo","new_string":"bar"}`},
	)
	view := stripANSIstr(m.rend.renderConversation(&m.conv, m.expandTools))
	if !strings.Contains(view, "- foo") || !strings.Contains(view, "+ bar") {
		t.Errorf("expected diff lines in conversation, got:\n%s", view)
	}
	if strings.Contains(view, `"old_string"`) {
		t.Errorf("Edit card should not show raw JSON args:\n%s", view)
	}
}

// TestExpandToolsToggle asserts ctrl+t flips the global expand flag.
func TestExpandToolsToggle(t *testing.T) {
	m := New(Deps{Theme: theme.New("aztec", theme.AztecPalette())})
	m = applyAll(m, tea.WindowSizeMsg{Width: 100, Height: 30})
	if m.expandTools {
		t.Fatal("expandTools should start false")
	}
	m = applyAll(m, tea.KeyPressMsg{Code: 't', Mod: tea.ModCtrl})
	if !m.expandTools {
		t.Error("ctrl+t should set expandTools true")
	}
	// The footer help is the terse, state-independent "ctrl+t details"; the
	// expand/collapse affordances live inline on each collapsible header instead.
	footer := stripANSIstr(m.renderFooter())
	if !strings.Contains(footer, "ctrl+t details") {
		t.Errorf("footer help should carry the terse details hint:\n%s", footer)
	}
	m = applyAll(m, tea.KeyPressMsg{Code: 't', Mod: tea.ModCtrl})
	if m.expandTools {
		t.Error("ctrl+t should toggle expandTools back to false")
	}
}

// TestReasoningBlockCollapsedThenExpanded asserts a reasoning delta renders as a
// dim, collapsed one-line "reasoning summary" header by default (the full text
// hidden), and that ctrl+t expands it to show the streamed reasoning text under
// a lossy-summary caveat. It also asserts the reasoning renders ABOVE the
// assistant answer of the same turn.
func TestReasoningBlockCollapsedThenExpanded(t *testing.T) {
	m := New(Deps{Theme: theme.New("aztec", theme.AztecPalette())})
	m = applyAll(m, tea.WindowSizeMsg{Width: 100, Height: 30})
	m.phase = phaseRunning
	m = applyAll(m,
		client.TurnStartMsg{Turn: 1},
		client.ReasoningDeltaMsg{Turn: 1, Text: "first I will inspect the file\nthen I will edit it"},
		client.AssistantDeltaMsg{Turn: 1, Text: "Here is the answer."},
	)

	// Collapsed (default): the "reasoning summary" header is shown, body hidden.
	collapsed := stripANSIstr(m.rend.renderConversation(&m.conv, m.expandTools))
	if !strings.Contains(collapsed, "reasoning summary · 2 lines · ctrl+t expand") {
		t.Errorf("expected collapsed reasoning-summary header, got:\n%s", collapsed)
	}
	if strings.Contains(collapsed, "inspect the file") {
		t.Errorf("collapsed reasoning must hide the body text:\n%s", collapsed)
	}
	if strings.Contains(collapsed, "may not reflect") {
		t.Errorf("collapsed reasoning must not show the expanded caveat:\n%s", collapsed)
	}

	// Reasoning must precede the assistant answer.
	if ri, ai := strings.Index(collapsed, "reasoning summary"), strings.Index(collapsed, "Here is the answer"); ri < 0 || ai < 0 || ri > ai {
		t.Errorf("reasoning (%d) should render above the answer (%d):\n%s", ri, ai, collapsed)
	}

	// Expanded (ctrl+t): the caveat + the full reasoning text become visible.
	m = applyAll(m, tea.KeyPressMsg{Code: 't', Mod: tea.ModCtrl})
	expanded := stripANSIstr(m.rend.renderConversation(&m.conv, m.expandTools))
	if !strings.Contains(expanded, "may not reflect its actual process") {
		t.Errorf("expanded reasoning should carry the lossy-summary caveat:\n%s", expanded)
	}
	if !strings.Contains(expanded, "inspect the file") || !strings.Contains(expanded, "then I will edit it") {
		t.Errorf("expanded reasoning should show the body text:\n%s", expanded)
	}
}

// TestReasoningInterleavedRendersOnce asserts that interleaved reasoning/answer
// deltas within one turn (reasoning→text→reasoning→text) fold into a SINGLE
// reasoning region above the merged answer, with a truthful line count — not
// multiple stacked reasoning blocks. This is the correctness fix: reasoning is
// an attribute of the turn's assistant block, not a reordered sibling.
func TestReasoningInterleavedRendersOnce(t *testing.T) {
	m := New(Deps{Theme: theme.New("aztec", theme.AztecPalette())})
	m = applyAll(m, tea.WindowSizeMsg{Width: 100, Height: 30})
	m.phase = phaseRunning
	m = applyAll(m,
		client.TurnStartMsg{Turn: 1},
		client.ReasoningDeltaMsg{Turn: 1, Text: "step one\n"},
		client.AssistantDeltaMsg{Turn: 1, Text: "Answer part A. "},
		client.ReasoningDeltaMsg{Turn: 1, Text: "step two\n"},
		client.AssistantDeltaMsg{Turn: 1, Text: "Answer part B."},
		client.TurnEndMsg{Turn: 1, Usage: client.Usage{InputTokens: 1200, OutputTokens: 340}, DurationMs: 4100},
	)

	// Exactly one assistant block, with both reasoning fragments merged into it.
	var asst int
	for i := range m.conv.blocks {
		if m.conv.blocks[i].kind == blockAssistant {
			asst++
			if m.conv.blocks[i].reasoning != "step one\nstep two\n" {
				t.Errorf("reasoning not merged onto the block: %q", m.conv.blocks[i].reasoning)
			}
		}
	}
	if asst != 1 {
		t.Fatalf("want exactly 1 assistant block, got %d", asst)
	}

	// Exactly one collapsed reasoning header, with the true 2-line count.
	view := stripANSIstr(m.rend.renderConversation(&m.conv, false))
	if got := strings.Count(view, "reasoning summary"); got != 1 {
		t.Errorf("want exactly one reasoning region, got %d:\n%s", got, view)
	}
	if !strings.Contains(view, "reasoning summary · 2 lines · ctrl+t expand") {
		t.Errorf("merged reasoning should report 2 lines:\n%s", view)
	}
	// Reasoning above both answer fragments.
	ri := strings.Index(view, "reasoning summary")
	ai := strings.Index(view, "Answer part A")
	if ri < 0 || ai < 0 || ri > ai {
		t.Errorf("reasoning (%d) should render above the merged answer (%d):\n%s", ri, ai, view)
	}
}

// TestReasoningLiveAffordance asserts that while reasoning is streaming and no
// answer text has arrived, the collapsed header reads "reasoning…", flipping to
// the static expandable form once answer text begins.
func TestReasoningLiveAffordance(t *testing.T) {
	m := New(Deps{Theme: theme.New("aztec", theme.AztecPalette())})
	m = applyAll(m, tea.WindowSizeMsg{Width: 100, Height: 30})
	m.phase = phaseRunning
	m = applyAll(m,
		client.TurnStartMsg{Turn: 1},
		client.ReasoningDeltaMsg{Turn: 1, Text: "thinking about it\n"},
	)
	live := stripANSIstr(m.rend.renderConversation(&m.conv, false))
	if !strings.Contains(live, "reasoning…") {
		t.Errorf("streaming reasoning (no answer yet) should show the live affordance:\n%s", live)
	}
	if strings.Contains(live, "ctrl+t expand") {
		t.Errorf("live reasoning should not yet show the static expand hint:\n%s", live)
	}

	// Answer text begins → flips to the static, expandable header.
	m = applyAll(m, client.AssistantDeltaMsg{Turn: 1, Text: "Done."})
	settled := stripANSIstr(m.rend.renderConversation(&m.conv, false))
	if strings.Contains(settled, "reasoning…") {
		t.Errorf("reasoning should stop showing the live affordance once answer begins:\n%s", settled)
	}
	if !strings.Contains(settled, "reasoning summary · 1 line · ctrl+t expand") {
		t.Errorf("settled reasoning should show the static header:\n%s", settled)
	}
}

// TestTurnEndStatLine asserts a non-trivial TurnEndMsg appends a muted inline
// stat line that leads with cost (tokens) then time, carries no turn index, and
// omits the duration segment when no clock reported one.
func TestTurnEndStatLine(t *testing.T) {
	m := New(Deps{Theme: theme.New("aztec", theme.AztecPalette())})
	m = applyAll(m, tea.WindowSizeMsg{Width: 100, Height: 30})
	m.phase = phaseRunning

	m = applyAll(m, client.TurnEndMsg{Turn: 2, Usage: client.Usage{InputTokens: 1200, OutputTokens: 340}, DurationMs: 4100})
	withDur := stripANSIstr(m.rend.renderConversation(&m.conv, m.expandTools))
	if !strings.Contains(withDur, "↑1.2K ↓340 · 4.1s") {
		t.Errorf("expected cost-first per-turn stat line with duration, got:\n%s", withDur)
	}
	if strings.Contains(withDur, "turn 2") {
		t.Errorf("stat line should not carry a turn index:\n%s", withDur)
	}

	// A turn with substantial tokens but no duration (no clock) omits the elapsed
	// segment but still renders (it is not trivial).
	m = applyAll(m, client.TurnEndMsg{Turn: 3, Usage: client.Usage{InputTokens: 500, OutputTokens: 20}, DurationMs: 0})
	noDur := stripANSIstr(m.rend.renderConversation(&m.conv, m.expandTools))
	if !strings.Contains(noDur, "↑500 ↓20") {
		t.Errorf("expected per-turn stat line, got:\n%s", noDur)
	}
	if strings.Contains(noDur, "↑500 ↓20 ·") {
		t.Errorf("turn with no clock should omit the duration segment:\n%s", noDur)
	}
}

// TestTurnEndTrivialSuppressed asserts a near-empty turn (tiny tokens, sub-second
// or no duration) produces NO stat line, so a long run isn't littered.
func TestTurnEndTrivialSuppressed(t *testing.T) {
	m := New(Deps{Theme: theme.New("aztec", theme.AztecPalette())})
	m = applyAll(m, tea.WindowSizeMsg{Width: 100, Height: 30})
	m.phase = phaseRunning

	before := len(m.conv.blocks)
	// Tiny tokens, no clock → trivial → suppressed.
	m = applyAll(m, client.TurnEndMsg{Turn: 1, Usage: client.Usage{InputTokens: 5, OutputTokens: 2}, DurationMs: 0})
	// Tiny tokens, sub-second duration → still trivial → suppressed.
	m = applyAll(m, client.TurnEndMsg{Turn: 2, Usage: client.Usage{InputTokens: 10, OutputTokens: 0}, DurationMs: 300})
	if len(m.conv.blocks) != before {
		t.Errorf("trivial turns should add no stat block; blocks grew %d→%d", before, len(m.conv.blocks))
	}

	// A turn over the duration threshold is NOT trivial even with tiny tokens.
	m = applyAll(m, client.TurnEndMsg{Turn: 3, Usage: client.Usage{InputTokens: 10, OutputTokens: 0}, DurationMs: 1500})
	view := stripANSIstr(m.rend.renderConversation(&m.conv, false))
	if !strings.Contains(view, "↑10 ↓0 · 1.5s") {
		t.Errorf("a turn with a measurable duration should not be suppressed:\n%s", view)
	}
}
