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
	footer := stripANSIstr(m.renderFooter())
	if !strings.Contains(footer, "ctrl+t collapse") {
		t.Errorf("footer help should reflect expanded state:\n%s", footer)
	}
	m = applyAll(m, tea.KeyPressMsg{Code: 't', Mod: tea.ModCtrl})
	if m.expandTools {
		t.Error("ctrl+t should toggle expandTools back to false")
	}
}
