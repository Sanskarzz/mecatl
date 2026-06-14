package ui

// Offline performance benchmark for the mecatui scrollback render path
// (perf-tracking.md Phase 2, "tui-scrollback"): it targets the SetContent
// O(scrollback) class — the per-frame string JOIN of all blocks plus the
// viewport's line split/measure in refreshView. It lives in the ui package (an
// internal _test file) so it can reach the unexported render path; perf/kpi is
// imported ONLY here, never by the production ui package (the production package
// must stay free of the perf dependency).
//
// It is a Benchmark, so `task test` (default -run) never runs it; it runs under
// `task perf:scenarios`. It records a kpi.ScenarioResult with NO token KPIs (a
// render bench has no model usage) into a package-local accumulator flushed by
// the TestMain in scrollback_bench_main_test.go.

import (
	"context"
	"strconv"
	"testing"
	"time"

	tea "charm.land/bubbletea/v2"

	"github.com/stacklok/mecatl/cmd/mecatui/client"
	"github.com/stacklok/mecatl/cmd/mecatui/theme"
	"github.com/stacklok/mecatl/perf/kpi"
)

// scrollbackBlocks is the scrollback depth the bench builds: a large, settled
// conversation so the per-frame join + SetContent dominates and a regression in
// the O(scrollback) cost shows up in allocs/op and ns/op.
const scrollbackBlocks = 400

// buildScrollbackModel constructs a connected, sized Model and pushes
// scrollbackBlocks settled blocks through the conversation reducer path (a mix of
// user prompts, resolved tool cards, and assistant turns — the common settled
// shapes). It returns the model ready to refreshView.
func buildScrollbackModel(tb testing.TB) Model {
	tb.Helper()
	m := New(Deps{
		Theme:       theme.New("aztec", theme.AztecPalette()),
		Ctx:         context.Background(),
		NoAltScreen: true,
	})
	m = applyAll(m,
		tea.WindowSizeMsg{Width: 100, Height: 40},
		client.SessionReadyMsg{SessionID: "perf-scrollback-0001"},
		client.TurnStartMsg{Turn: 1},
	)
	m.phase = phaseRunning

	for i := 0; i < scrollbackBlocks; i++ {
		id := "call-" + strconv.Itoa(i)
		m.conv.addUser("Question number " + strconv.Itoa(i) + ": please inspect the file and summarise the result.")
		m.conv.startAssistant()
		m.conv.appendAssistant("Here is **the** answer for step " + strconv.Itoa(i) +
			".\n\n- read the file\n- made the edit\n- ran the tests\n\nThe change is small and self-contained.\n")
		m.conv.addTool(id, "Read", `{"path":"pkg/file`+strconv.Itoa(i)+`.go"}`)
		m.conv.resolveTool(id, "package main\n\nfunc main() {}\n", false)
	}
	return m
}

// BenchmarkScrollbackView measures the conversation render path over a large
// settled scrollback: the measured region is m.refreshView() (the string join +
// vp.SetContent line split/measure). Allocations are the gated KPI.
func BenchmarkScrollbackView(b *testing.B) {
	m := buildScrollbackModel(b)
	// Prime once so the per-block render cache is warm; the measured region then
	// reflects the steady-state per-frame join + SetContent cost, not first-render.
	m.refreshView()

	// Baseline BEFORE the measured region: GoroutinesEnd is a leak DELTA, the same
	// treatment all scenarios use (the render path spawns no goroutines, so this is
	// expected to stay 0 — kept consistent with the delegation scenarios).
	baselineGoroutines := kpi.GoroutinesAfterSettle(20 * time.Millisecond)
	capt := kpi.NewCapture()
	b.ReportAllocs()
	capt.Begin()
	for b.Loop() {
		// Bump the live block's revision each iteration so refreshView does real
		// work (re-joins the scrollback and re-runs SetContent), rather than
		// short-circuiting. This mirrors the steady-state streaming frame: one live
		// block changes, the whole scrollback is re-joined into the viewport.
		m.conv.appendAssistant(".")
		m.refreshView()
	}
	mtr := capt.End()

	addScrollbackResult(kpi.ScenarioResult{
		Name:          "tui_scrollback_view",
		Iterations:    b.N,
		AllocsPerOp:   scrollbackPerOp(mtr.Allocs, b.N),
		BytesPerOp:    scrollbackPerOp(mtr.Bytes, b.N),
		GoroutinesEnd: kpi.GoroutineDelta(baselineGoroutines, 20*time.Millisecond),
		RSSPeakBytes:  mtr.RSSPeak,
		RSSFinalBytes: mtr.RSSFinal,
		WallClockNs:   mtr.WallNs,
	})
}

// scrollbackPerOp divides a captured total by the iteration count, guarding n==0.
func scrollbackPerOp(total uint64, n int) uint64 {
	if n <= 0 {
		return 0
	}
	return total / uint64(n)
}
