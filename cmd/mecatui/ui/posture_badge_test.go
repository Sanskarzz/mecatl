package ui

import (
	"strings"
	"testing"

	tea "charm.land/bubbletea/v2"

	"github.com/stacklok/mecatl/cmd/mecatui/client"
	"github.com/stacklok/mecatl/cmd/mecatui/theme"
)

// TestPostureBadgeShownForAutoYolo asserts the header renders a "⚠ auto"/"⚠ yolo"
// chrome badge when the server reports an allow-all posture, and renders NO badge for
// strict/trusted (and an empty/older-server posture) — the goldens-stability guarantee.
// A regression that rendered the badge unconditionally (or dropped it for yolo) flips
// one of these. The badge is sourced from caps.Posture (SessionReadyMsg), NOT the
// per-session mode segment.
func TestPostureBadgeShownForAutoYolo(t *testing.T) {
	cases := []struct {
		posture   string
		wantBadge string // "" = no badge
	}{
		{"", ""},
		{"strict", ""},
		{"trusted", ""},
		{"auto", "⚠ auto"},
		{"yolo", "⚠ yolo"},
	}
	for _, tc := range cases {
		m, _, _ := newTestModel(t, theme.New("aztec", theme.AztecPalette()))
		m = applyAll(m, tea.WindowSizeMsg{Width: 100, Height: 30},
			client.SessionReadyMsg{SessionID: "sess-test-0001", Capabilities: client.Capabilities{Posture: tc.posture}})

		header := stripANSIstr(m.renderHeader())
		if tc.wantBadge == "" {
			if strings.Contains(header, "⚠") {
				t.Errorf("posture %q: header must show NO badge, got %q", tc.posture, header)
			}
			continue
		}
		if !strings.Contains(header, tc.wantBadge) {
			t.Errorf("posture %q: header missing %q badge, got %q", tc.posture, tc.wantBadge, header)
		}
	}
}

// TestPostureBadgeCarriesWarningStyle asserts the badge is rendered in the THEME's
// "warning" style, not the muted style the benign scroll/changed-files cues use — the
// one persistent in-session danger cue must READ as danger. It checks the RAW (un-
// stripped) header for the exact warning-styled badge substring and confirms the badge
// is NOT muted-styled. Stripping ANSI (as TestPostureBadgeShownForAutoYolo does) cannot
// see colour, so this guards the styling against a silent regression to muted.
func TestPostureBadgeCarriesWarningStyle(t *testing.T) {
	th := theme.New("aztec", theme.AztecPalette())
	for _, posture := range []string{"auto", "yolo"} {
		m, _, _ := newTestModel(t, th)
		m = applyAll(m, tea.WindowSizeMsg{Width: 100, Height: 30},
			client.SessionReadyMsg{SessionID: "sess-test-0001", Capabilities: client.Capabilities{Posture: posture}})

		raw := m.renderHeader()
		badge := "⚠ " + posture
		wantWarning := th.Style("warning").Render(badge)
		if !strings.Contains(raw, wantWarning) {
			t.Errorf("posture %q: header must render the badge in the WARNING style; want substring %q in %q", posture, wantWarning, raw)
		}
		// Guard against a regression to the muted style (the benign-cue weight).
		mutedBadge := th.Style("muted").Render(badge)
		if mutedBadge != wantWarning && strings.Contains(raw, mutedBadge) {
			t.Errorf("posture %q: badge must NOT be muted-styled (it is a danger cue); found muted render %q", posture, mutedBadge)
		}
	}
}

// TestPostureBadgeIsNotModeSegment guards that the badge is DISTINCT from the per-session
// `mode` segment: with posture auto and a default permission mode, the header carries
// the auto badge but no "mode auto" text (the mode segment renders the PermissionMode,
// here unset, never the posture).
func TestPostureBadgeIsNotModeSegment(t *testing.T) {
	m, _, _ := newTestModel(t, theme.New("aztec", theme.AztecPalette()))
	m = applyAll(m, tea.WindowSizeMsg{Width: 120, Height: 30},
		client.SessionReadyMsg{SessionID: "sess-test-0001", Capabilities: client.Capabilities{Posture: "auto"}})
	header := stripANSIstr(m.renderHeader())
	if !strings.Contains(header, "⚠ auto") {
		t.Fatalf("expected the auto posture badge; got %q", header)
	}
	if strings.Contains(header, "mode auto") {
		t.Fatalf("posture must not leak into the mode segment; got %q", header)
	}
}

// TestPostureSummary pins the /posture one-line summary for each tier (the runPosture
// status text). It would fail if a defense's on/off mapping drifted (e.g. child auto-run
// reported on at auto).
func TestPostureSummary(t *testing.T) {
	cases := []struct {
		posture string
		want    []string // substrings that MUST be present
	}{
		{"strict", []string{"posture strict", "allow-all off", "child $()/heredoc auto-run (injection-defense off) off", "project-trust off"}},
		{"trusted", []string{"posture trusted", "allow-all off", "project-trust on"}},
		{"auto", []string{"posture auto", "allow-all on", "main $()/heredoc auto-run on", "child $()/heredoc auto-run (injection-defense off) off", "project-trust on"}},
		{"yolo", []string{"posture yolo", "allow-all on", "child $()/heredoc auto-run (injection-defense off) on", "project-trust on"}},
	}
	for _, tc := range cases {
		got := postureSummary(tc.posture)
		for _, sub := range tc.want {
			if !strings.Contains(got, sub) {
				t.Errorf("postureSummary(%q) missing %q; got %q", tc.posture, sub, got)
			}
		}
	}
}
