package agent_test

import (
	"encoding/json"
	"testing"

	"github.com/stacklok/mecatl/internal/agent"
	"github.com/stacklok/mecatl/internal/session"
)

// TestHeuristicTokenCounterDeterministic checks Count is deterministic and scales
// with byte length per the chars-per-token ratio.
func TestHeuristicTokenCounterDeterministic(t *testing.T) {
	c := agent.HeuristicTokenCounter{}
	const s = "the quick brown fox jumps over the lazy dog"
	first := c.Count(s)
	for i := 0; i < 5; i++ {
		if got := c.Count(s); got != first {
			t.Fatalf("Count not deterministic: %d vs %d", got, first)
		}
	}
	// ~4 chars/token: 43 chars / 4 = 10.
	if want := len(s) / 4; first != want {
		t.Fatalf("Count(%q) = %d, want %d", s, first, want)
	}
}

// TestHeuristicTokenCounterCharsPerTokenOverride checks the CharsPerToken knob.
func TestHeuristicTokenCounterCharsPerTokenOverride(t *testing.T) {
	c := agent.HeuristicTokenCounter{CharsPerToken: 2}
	s := "abcdefgh" // 8 chars
	if got := c.Count(s); got != 4 {
		t.Fatalf("Count with cpt=2 = %d, want 4", got)
	}
}

// TestHeuristicTokenCounterMessagesOverhead checks CountMessages adds per-message
// and per-tool-call framing overhead on top of the body estimate, so a history of
// many short messages is not undercounted to zero.
func TestHeuristicTokenCounterMessagesOverhead(t *testing.T) {
	c := agent.HeuristicTokenCounter{}
	msgs := []session.Message{
		session.NewSystemMessage(""),
		session.NewUserMessage(""),
		session.NewAssistantMessage("", "", []session.ToolCall{
			session.NewToolCall("c1", "Read", json.RawMessage(`{}`)),
		}),
		session.NewToolMessage(session.NewToolResult("c1", "")),
	}
	// Every message contributes its per-message overhead even with empty bodies,
	// and the tool call adds its own overhead, so the total must be positive.
	if got := c.CountMessages(msgs); got <= 0 {
		t.Fatalf("CountMessages with empty bodies = %d, want > 0 (framing overhead)", got)
	}
	// Deterministic across calls.
	if a, b := c.CountMessages(msgs), c.CountMessages(msgs); a != b {
		t.Fatalf("CountMessages not deterministic: %d vs %d", a, b)
	}
}

// TestCountMessagesCountsMediaBytes asserts a multimodal message's inline media
// bytes are counted, so a multimodal message is not undercounted vs the same
// text alone.
func TestCountMessagesCountsMediaBytes(t *testing.T) {
	c := agent.HeuristicTokenCounter{}
	textOnly := []session.Message{session.NewUserMessage("hello")}
	withMedia := []session.Message{session.NewUserMessageWithParts("hello", []session.Content{
		{Kind: session.MediaImage, MIMEType: "image/png", Data: make([]byte, 8000)},
	})}
	base := c.CountMessages(textOnly)
	media := c.CountMessages(withMedia)
	if media <= base {
		t.Fatalf("media count %d not greater than text-only %d — media not counted", media, base)
	}
	// 8000 bytes at 4 chars/token ~ 2000 tokens.
	if media-base < 1500 {
		t.Fatalf("media delta %d too small; image bytes undercounted", media-base)
	}
}
