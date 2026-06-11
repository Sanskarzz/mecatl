package agent_test

import (
	"context"
	"encoding/json"
	"iter"
	"strings"
	"testing"

	"github.com/stacklok/mecatl/engine/agent"
	"github.com/stacklok/mecatl/engine/port"
	"github.com/stacklok/mecatl/engine/session"
	"github.com/stacklok/mecatl/internal/adapter/mockllm"
)

// overBudgetConversation builds a long history with a system prompt, a clear user
// goal, several tool calls touching distinct file paths, large tool-result bodies,
// and trailing chatter — the shape compaction must reduce.
func overBudgetConversation() *session.Conversation {
	conv := &session.Conversation{}
	conv.Append(session.NewSystemMessage("system rules"))
	conv.Append(session.NewUserMessage("fix the bug in handler.go and util.go"))
	for i, path := range []string{"handler.go", "util.go", "main.go", "server.go"} {
		id := session.ToolCallID(string(rune('a' + i)))
		conv.Append(session.NewAssistantMessage("", "", []session.ToolCall{
			session.NewToolCall(id, "Read", json.RawMessage(`{"path":"`+path+`"}`)),
		}))
		conv.Append(session.NewToolMessage(session.NewToolResult(id, strings.Repeat("X", 4000))))
	}
	for i := 0; i < 8; i++ {
		conv.Append(session.NewUserMessage("more chatter about the task"))
	}
	return conv
}

// assertGoalAndPathsSurvive checks the user goal and every file path are present
// in the compacted history, and that no large tool/file body survived.
func assertGoalAndPathsSurvive(t *testing.T, compacted []session.Message) {
	t.Helper()
	var blob strings.Builder
	var haveSystem, haveGoal bool
	for _, m := range compacted {
		blob.WriteString(m.Text)
		if m.ToolResult != nil {
			blob.WriteString(m.ToolResult.Content)
			if len(m.ToolResult.Content) >= 4000 {
				t.Fatalf("large tool body survived compaction: %d chars", len(m.ToolResult.Content))
			}
		}
		if m.Role == session.RoleSystem && m.Text == "system rules" {
			haveSystem = true
		}
		if m.Role == session.RoleUser && strings.Contains(m.Text, "fix the bug") {
			haveGoal = true
		}
	}
	if !haveSystem {
		t.Fatalf("system prompt not preserved")
	}
	if !haveGoal {
		t.Fatalf("goal not preserved")
	}
	for _, p := range []string{"handler.go", "util.go", "main.go", "server.go"} {
		if !strings.Contains(blob.String(), p) {
			t.Fatalf("file path %q did not survive compaction", p)
		}
	}
}

// TestCascadeReducesWithoutLLM checks that with NO LLM injected the cascade runs
// its deterministic tiers, reduces the history, and preserves the goal/paths while
// dropping large bodies. It never makes a model call (none is available).
func TestCascadeReducesWithoutLLM(t *testing.T) {
	conv := overBudgetConversation()
	counter := agent.HeuristicTokenCounter{}
	before := counter.CountMessages(conv.Messages)

	cc := agent.CascadeCompactor{Counter: counter} // no LLM, no budget → run every deterministic tier once
	compacted, summary, err := cc.Compact(context.Background(), conv)
	if err != nil {
		t.Fatalf("Compact: %v", err)
	}

	after := counter.CountMessages(compacted)
	if after >= before {
		t.Fatalf("cascade did not reduce token estimate: %d -> %d", before, after)
	}
	assertGoalAndPathsSurvive(t, compacted)

	// With no LLM, the summarize tier must NOT have run.
	if strings.Contains(summary, "summarize:") {
		t.Fatalf("summarize tier ran without an LLM injected: %s", summary)
	}
	// At least one deterministic tier should be reported.
	if !strings.Contains(summary, "snip:") && !strings.Contains(summary, "strip:") && !strings.Contains(summary, "collapse:") {
		t.Fatalf("no deterministic tier reported in summary: %s", summary)
	}
}

// TestCascadeTierProgression checks the tiers fire in cheapest-first order as the
// budget tightens: a generous budget needs only snip; a very tight budget forces
// strip and collapse as well.
func TestCascadeTierProgression(t *testing.T) {
	counter := agent.HeuristicTokenCounter{}

	// Tight budget: should drive past snip into strip/collapse (body rewriting).
	tight := agent.CascadeCompactor{Counter: counter, BudgetTokens: 60}
	compacted, summary, err := tight.Compact(context.Background(), overBudgetConversation())
	if err != nil {
		t.Fatalf("Compact: %v", err)
	}
	assertGoalAndPathsSurvive(t, compacted)
	if !strings.Contains(summary, "collapse:") && !strings.Contains(summary, "strip:") {
		t.Fatalf("tight budget did not engage strip/collapse tiers: %s", summary)
	}
}

// TestCascadeTier4SummarizesWithLLM checks that when tiers 1–3 cannot reach the
// budget and a (fake) LLM is injected, tier 4 replaces the oldest segment with the
// model's summary. We force this by setting an impossibly small budget so the
// deterministic tiers never fit.
func TestCascadeTier4SummarizesWithLLM(t *testing.T) {
	llm := mockllm.New(mockllm.TextTurn("PLAN: fix bugs. DECISIONS: none. PENDING: handler.go, util.go."))
	cc := agent.CascadeCompactor{
		Counter:      agent.HeuristicTokenCounter{},
		BudgetTokens: 1, // unreachable by tiers 1–3 → forces tier 4
		LLM:          llm,
		Model:        "m",
	}
	compacted, summary, err := cc.Compact(context.Background(), overBudgetConversation())
	if err != nil {
		t.Fatalf("Compact: %v", err)
	}
	if llm.Calls() != 1 {
		t.Fatalf("expected exactly 1 LLM summary call, got %d", llm.Calls())
	}
	if !strings.Contains(summary, "summarize:") {
		t.Fatalf("summarize tier not reported: %s", summary)
	}
	// The model's summary text must appear in the compacted history.
	var found bool
	for _, m := range compacted {
		if strings.Contains(m.Text, "PLAN: fix bugs") {
			found = true
		}
	}
	if !found {
		t.Fatalf("LLM summary not present in compacted history")
	}
	// Goal and paths still survive (paths via the synthesised summary message).
	assertGoalAndPathsSurvive(t, compacted)
}

// TestCascadeImplementsCompactor is a compile-and-behaviour check that the cascade
// is usable wherever a Compactor is expected.
func TestCascadeImplementsCompactor(t *testing.T) {
	var _ agent.Compactor = agent.CascadeCompactor{}
	conv := &session.Conversation{}
	conv.Append(session.NewSystemMessage("sys"))
	conv.Append(session.NewUserMessage("goal"))
	compacted, _, err := agent.CascadeCompactor{}.Compact(context.Background(), conv)
	if err != nil {
		t.Fatalf("Compact on tiny history: %v", err)
	}
	if len(compacted) == 0 {
		t.Fatalf("expected non-empty compacted history")
	}
}

// capturingSummaryProvider records the request messages of each Stream call so a
// test can assert the tier-4 summary call carries no media bytes, then replies
// with a fixed summary.
type capturingSummaryProvider struct {
	gotMessages []session.Message
}

func (*capturingSummaryProvider) Capabilities() port.ProviderCapabilities {
	return port.ProviderCapabilities{}
}

func (p *capturingSummaryProvider) Stream(_ context.Context, req port.LLMRequest) (iter.Seq2[port.Chunk, error], error) {
	p.gotMessages = append([]session.Message(nil), req.Messages...)
	return func(yield func(port.Chunk, error) bool) {
		yield(port.Chunk{Kind: port.ChunkText, Text: "SUMMARY of earlier turns"}, nil)
		yield(port.Chunk{Kind: port.ChunkDone, Stop: session.StopEndTurn}, nil)
	}, nil
}

// TestCascadeTier4SubstitutesMediaPlaceholder asserts the tier-4 summary call
// NEVER ships media bytes: a media-bearing user message in the summarised segment
// is replaced by a text placeholder, and the synthesised summary is text-only.
func TestCascadeTier4SubstitutesMediaPlaceholder(t *testing.T) {
	conv := &session.Conversation{}
	conv.Append(session.NewSystemMessage("system rules"))
	// The first user message is the goal (preserved verbatim by the head).
	conv.Append(session.NewUserMessage("fix the bug"))
	// Leading chatter (the older half of the middle, which tier-1 snip drops).
	for i := 0; i < 8; i++ {
		conv.Append(session.NewUserMessage("older chatter"))
	}
	// The media message sits in the NEWER half of the middle (survives snip) but
	// still ahead of the preserved tail, so tier 4 summarises it — substituting a
	// text placeholder for the image, never the bytes.
	conv.Append(session.NewUserMessageWithParts("here is the screenshot", []session.Content{
		{Kind: session.MediaImage, MIMEType: "image/png", Data: make([]byte, 4096)},
	}))
	for i := 0; i < 4; i++ {
		conv.Append(session.NewUserMessage("follow-up chatter"))
	}

	prov := &capturingSummaryProvider{}
	cc := agent.CascadeCompactor{
		Counter:       agent.HeuristicTokenCounter{},
		BudgetTokens:  1, // force tier 4
		KeepLastTurns: 2,
		LLM:           prov,
		Model:         "m",
	}
	compacted, _, err := cc.Compact(context.Background(), conv)
	if err != nil {
		t.Fatalf("Compact: %v", err)
	}

	// 1) No media bytes reached the summary call.
	for _, m := range prov.gotMessages {
		if len(m.Parts) != 0 {
			t.Fatalf("media Parts reached the summary call: %+v", m.Parts)
		}
		for _, p := range m.Parts {
			if len(p.Data) > 0 {
				t.Fatal("image bytes shipped to summariser")
			}
		}
	}
	// 2) A text placeholder appeared for the image somewhere in the summary input.
	var sawPlaceholder bool
	for _, m := range prov.gotMessages {
		if strings.Contains(m.Text, "[image: image/png") {
			sawPlaceholder = true
		}
	}
	if !sawPlaceholder {
		t.Fatalf("no image placeholder in summary input: %+v", prov.gotMessages)
	}
	// 3) The synthesised summary message (the one carrying the summariser output)
	// is text-only — tier 4 never produces a media-bearing message.
	var sawSummary bool
	for _, m := range compacted {
		if strings.Contains(m.Text, "earlier turns summarised") {
			sawSummary = true
			if len(m.Parts) != 0 {
				t.Fatalf("synthesised summary carries media Parts: %+v", m.Parts)
			}
		}
	}
	if !sawSummary {
		t.Fatalf("no synthesised summary in compacted history: %+v", compacted)
	}
}
