package scenarios_test

import (
	"context"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/stacklok/mecatl/engine/adapter/mockllm"
	"github.com/stacklok/mecatl/engine/agent"
	"github.com/stacklok/mecatl/engine/session"
	"github.com/stacklok/mecatl/perf/kpi"
)

// outputEconomyTurns is the scripted session length for the output-economy
// scenario. It is long enough that the per-run output-token + assistant-prose
// totals are a stable signal (an output-verbosity regression shifts them), and
// short enough that the benchmark runs in well under a second. Each scripted turn
// carries BOTH a prose chunk (the verbose failure mode the economy prompt targets)
// and a read-only tool call (so the loop continues to the next turn), with fixed
// usage so the KPI row is deterministic.
const outputEconomyTurns = 40

// verboseProse is the assistant prose emitted on every scripted turn — the kind
// of restating/narrating the output-economy prompt wording is meant to suppress.
// It is intentionally larger than a minimal turn would need, so a prompt change
// that successfully suppresses it shows up as a measurable drop in the KPI row
// when the same scenario is re-scripted to a terser provider.
const verboseProse = "Let me go ahead and inspect the next file now. I'll read it " +
	"so I can understand the current state of the code before I make any changes. " +
	"Here is what I found after reading it: the file looks as expected."

// buildOutputEconomyScript returns a mockllm provider scripting
// outputEconomyTurns verbose tool-using turns + a terminal no-tool turn. Token
// figures are fixed (deterministic); output tokens are the headline KPI.
func buildOutputEconomyScript() *mockllm.Provider {
	const inputTokens = 800
	const outputTokens = 120 // prose + tool-call framing
	turns := make([]mockllm.Turn, 0, outputEconomyTurns+1)
	for i := 0; i < outputEconomyTurns; i++ {
		turns = append(turns, mockllm.ChunksTurn(
			mockllm.TextChunk(verboseProse),
			mockllm.ToolCallChunk(session.NewToolCall(
				session.ToolCallID("oe-"+strconv.Itoa(i)), "Read", []byte(`{"path":"a.go"}`))),
			mockllm.UsageChunk(session.Usage{InputTokens: inputTokens, OutputTokens: outputTokens}),
			mockllm.DoneChunk(session.StopEndTurn),
		))
	}
	turns = append(turns, mockllm.ChunksTurn(
		mockllm.TextChunk("done"),
		mockllm.UsageChunk(session.Usage{InputTokens: inputTokens, OutputTokens: 10}),
		mockllm.DoneChunk(session.StopEndTurn),
	))
	return mockllm.New(turns...)
}

// BenchmarkOutputEconomy drives a scripted verbose-prose session through the real
// engine, offline, capturing the per-run output-token total and the
// assistant-prose byte count as headline KPIs. The scenario is the measurement
// harness for output-economy prompt changes (ADR 0041): a before/after prompt
// wording change, or a real-model A/B, is compared against this row.
//
// Because mockllm scripts fixed output, the absolute figures here do not move
// with the prompt wording alone — they capture the ACCOUNTING INFRASTRUCTURE
// (session.Usage.OutputTokens, assistant-prose byte tally) so a future real-model
// A/B or a re-scripted terser provider has a ready, deterministic comparison
// path. The assistant-prose byte count is the deterministic, prompt-sensitive
// signal: it sums the Text of every recorded assistant turn, so a wording change
// that suppresses verbose prose shows up even under a scripted provider once the
// script is adjusted.
func BenchmarkOutputEconomy(b *testing.B) {
	llm := buildOutputEconomyScript()
	e := buildEngine(agent.Deps{
		LLM:     llm,
		Catalog: scenarioCatalog(readTool{}),
	})
	limits := session.Limits{MaxTurns: outputEconomyTurns + 5, MaxToolCalls: outputEconomyTurns + 5}

	var lastSess *session.Session
	baselineGoroutines := kpi.GoroutinesAfterSettle(20 * time.Millisecond)
	capt := kpi.NewCapture()
	b.ReportAllocs()
	capt.Begin()
	for b.Loop() {
		llm.Reset()
		sess := scenarioSession("perf-output-economy", limits)
		r := e.Run(context.Background(), sess, scenarioWorkspace(), "inspect every file then summarise")
		sinkInt = drain(r)
		lastSess = sess
	}
	m := capt.End()

	u := session.Usage{}
	var proseBytes int64
	if lastSess != nil {
		u = lastSess.Usage
		for _, msg := range lastSess.Conversation.Messages {
			if msg.Role == session.RoleAssistant {
				proseBytes += int64(len(msg.Text))
			}
		}
	}
	addResult(kpi.ScenarioResult{
		Name:          "output_economy",
		Iterations:    b.N,
		AllocsPerOp:   perOp(m.Allocs, b.N),
		BytesPerOp:    perOp(m.Bytes, b.N),
		GoroutinesEnd: kpi.GoroutineDelta(baselineGoroutines, 20*time.Millisecond),
		TokensInput:   int64(u.InputTokens),
		TokensOutput:  int64(u.OutputTokens),
		// prose_bytes is not a first-class ScenarioResult field; it rides the
		// trend store as a deterministic, prompt-sensitive companion to the
		// output-token total. Reported on stdout for local A/B inspection.
		RSSPeakBytes:  m.RSSPeak,
		RSSFinalBytes: m.RSSFinal,
		WallClockNs:   m.WallNs,
	})
	// Report the prompt-sensitive prose-byte total so a local A/B run can eyeball
	// the before/after delta without parsing the trend JSON.
	b.ReportMetric(float64(proseBytes), "prose_bytes/op")
}

// ensure the strings import stays honest if the scenario body is edited.
var _ = strings.Contains
