package ui

import (
	"os"
	"path/filepath"
	"testing"

	"github.com/stacklok/mecatl/cmd/mecatui/theme"
)

// TestEmitSelectionSamples renders REAL ANSI frames of the selection highlight
// across a theme × scenario matrix and writes them to the repo-local .scratch/
// dir for visual sign-off. It is GATED behind MECATUI_EMIT_SAMPLES (skipped in
// the normal -race suite) so it never runs in CI and never touches the working
// tree by default. It is fully deterministic: no Tick, no goroutine, no
// wall-clock — it sets crafted viewport content, points the selection at a known
// span, applies the highlight through the production applySelectionHighlight
// path, and captures m.View().
//
// Run it with:
//
//	MECATUI_EMIT_SAMPLES=1 go test ./cmd/mecatui/ui/ -run TestEmitSelectionSamples
//
// Open the files in a true-color terminal (`cat .scratch/selection-sample-*.txt`)
// to eyeball the block: it should be a solid, legible high-contrast rectangle on
// every theme, dark and light.
func TestEmitSelectionSamples(t *testing.T) {
	if os.Getenv("MECATUI_EMIT_SAMPLES") == "" {
		t.Skip("set MECATUI_EMIT_SAMPLES=1 to emit the .scratch/ selection sample frames")
	}

	// Each scenario crafts a viewport body and a logical selection span over it.
	// The body lines are deliberately PLAIN (no glamour) so the sample isolates the
	// highlight block itself — the selection style strips and re-renders the span
	// regardless of the underlying ANSI, so a plain body is a faithful probe.
	type scenario struct {
		name string
		body string
		sel  selection
	}
	scenarios := []scenario{
		{
			name: "code-block",
			body: "func add(a, b int) int {\n\treturn a + b\n}",
			// Select "add(a, b int) int" on line 0 (cols 5..22).
			sel: selection{active: true, anchorL: 0, anchorC: 5, headL: 0, headC: 22},
		},
		{
			name: "prose",
			body: "The quick brown fox jumps over the lazy dog.\nA second line of prose follows here.",
			// Select "quick brown fox jumps over" on line 0 (cols 4..30).
			sel: selection{active: true, anchorL: 0, anchorC: 4, headL: 0, headC: 30},
		},
		{
			name: "multiline-code-empty-prose",
			body: "x := compute()\n\nNow the prose explains it.",
			// Span all three logical lines (code → empty → prose); the empty interior
			// line is a deliberate gap (known limitation).
			sel: selection{active: true, anchorL: 0, anchorC: 0, headL: 2, headC: 26},
		},
		{
			name: "trailing-whitespace",
			body: "line with trailing spaces      \nnext line",
			// Select the whole first line INCLUDING its trailing spaces (col 0..31).
			sel: selection{active: true, anchorL: 0, anchorC: 0, headL: 0, headC: 31},
		},
	}

	// Resolve the built-in dark (aztec) and light (solar) themes through the
	// registry so no new exported palette accessor is needed.
	reg := theme.NewRegistry()
	themeNames := []string{"aztec", "solar"}

	outDir := scratchDir(t)
	for _, name := range themeNames {
		th, ok := reg.Get(name)
		if !ok {
			t.Fatalf("built-in theme %q not registered", name)
		}
		for _, sc := range scenarios {
			m, _ := selModel(t)
			m.deps.Theme = th
			// Re-point BOTH viewport highlight styles at the per-sample theme (model.go
			// wires them once at New; this test swaps the theme after construction). Both
			// matter: the viewport renders the focused range with SelectedHighlightStyle
			// on top of HighlightStyle, so leaving the latter at the default-theme value
			// would mis-colour (or, if empty, wipe) the highlight.
			m.vp.HighlightStyle = th.Style("selection")
			m.vp.SelectedHighlightStyle = th.Style("selection")
			m.vp.SetContent(sc.body)
			m.vp.SetYOffset(0)
			m.phase = phaseIdle
			m.sel = sc.sel
			m.sel.snapshot = selectedText(m.vp.GetContent(), m.sel)
			applySelectionHighlight(&m)

			view := m.View().Content // tea.View struct; Content is the rendered ANSI
			path := filepath.Join(outDir, "selection-sample-"+name+"-"+sc.name+".txt")
			if err := os.WriteFile(path, []byte(view), 0o644); err != nil { //nolint:gosec // sample artifact in repo-local .scratch
				t.Fatalf("write %s: %v", path, err)
			}
			t.Logf("wrote %s", path)
		}
	}
}

// scratchDir returns the repo-local .scratch/ dir (gitignored), creating it if
// needed. Sample output goes here per the project's scratch convention — never
// /tmp. It walks up from the package dir to the repo root (the dir holding go.mod).
func scratchDir(t *testing.T) string {
	t.Helper()
	dir, err := os.Getwd()
	if err != nil {
		t.Fatalf("getwd: %v", err)
	}
	for {
		if _, err := os.Stat(filepath.Join(dir, "go.mod")); err == nil {
			break
		}
		parent := filepath.Dir(dir)
		if parent == dir {
			t.Fatalf("could not locate repo root (go.mod) above %q", dir)
		}
		dir = parent
	}
	scratch := filepath.Join(dir, ".scratch")
	if err := os.MkdirAll(scratch, 0o755); err != nil {
		t.Fatalf("mkdir .scratch: %v", err)
	}
	return scratch
}
