package openai

import (
	"testing"

	"github.com/stacklok/mecatl/engine/port"
	"github.com/stacklok/mecatl/engine/session"
)

// TestReasoningItemIDMixedItemBoundary is the cold reproducer for the
// reasoning-item id="" Responses-replay fix. It drives a recorded SSE fixture
// end-to-end through decodeSSE (the exact translate path the live adapter uses)
// and asserts the reasoning item's OWN provider id ("rs_123") survives the
// SSE→port.Chunk boundary intact and is NOT confused with the assistant message
// item's id ("msg_abc") that the text deltas ride on.
//
// The fixture models the mixed-item stream the bug lives in: a reasoning item
// rs_123 emits summary deltas and an assembled output_item.done carrying the
// encrypted replay blob, THEN a message item msg_abc emits the visible text.
// Under the PRE-FIX conversion the reasoning arm emitted the ChunkReasoningItem
// with NO id (it dropped item.ID), so the loop could not stamp
// Message.ReasoningItemID, replay serialised id:"" and strict gateways 400'd
// on turn 2+ (see HANDOVER-reasoning-item-id-replay.md). This test fails on that
// old behavior and passes now.
func TestReasoningItemIDMixedItemBoundary(t *testing.T) {
	chunks := decodeFixture(t, "reasoning_item_id_mixed.sse")

	// (a) The reasoning replay-blob chunk (ChunkReasoningItem) carries the
	// reasoning item's OWN id "rs_123" — the id the loop stamps onto
	// Message.ReasoningItemID for verbatim replay. Under the pre-fix code this
	// chunk was emitted with ReasoningItemID="" (item.ID was dropped), so this
	// assertion discriminates: an empty (or "msg_abc") id fails it.
	var reasoningItemChunk *port.Chunk
	for i := range chunks {
		if chunks[i].Kind == port.ChunkReasoningItem {
			reasoningItemChunk = &chunks[i]
		}
	}
	if reasoningItemChunk == nil {
		t.Fatalf("no ChunkReasoningItem in stream; chunks=%+v", chunks)
	}
	if reasoningItemChunk.ReasoningItemID != "rs_123" {
		t.Errorf("ChunkReasoningItem.ReasoningItemID = %q, want %q (must be the reasoning item's own id, not the message item's and not empty)",
			reasoningItemChunk.ReasoningItemID, "rs_123")
	}
	if reasoningItemChunk.Text != "ENCRYPTED_RS_123" {
		t.Errorf("ChunkReasoningItem.Text = %q, want %q (the encrypted_content replay blob)", reasoningItemChunk.Text, "ENCRYPTED_RS_123")
	}

	// (b) The assistant TEXT chunks must NOT carry the reasoning item id — the
	// reasoning id belongs only to the reasoning item, not the message item.
	// (ChunkText has no ReasoningItemID carrier at all, so any non-empty value
	// here — including "rs_123" or "msg_abc" — is a boundary leak.)
	for i, c := range chunks {
		if c.Kind != port.ChunkText {
			continue
		}
		if c.ReasoningItemID != "" {
			t.Errorf("text chunk %d leaks ReasoningItemID = %q; text deltas must carry no reasoning item id", i, c.ReasoningItemID)
		}
	}

	// (c) The final converted assistant message — replicating the loop's
	// last-non-empty-wins threading of ChunkReasoningItem.ReasoningItemID onto
	// Message.ReasoningItemID (engine/agent/loop.go) — must carry BOTH the
	// reasoning blob AND "rs_123". Under the pre-fix code the blob threaded
	// through (Reasoning was already accumulated) but ReasoningItemID stayed
	// empty, so this asserts the id half specifically (the empty-id case is the
	// bug). It also asserts the id is NOT the message item's id, which would
	// mean the reasoning was attributed to the wrong boundary.
	msg := aggregateAssistantMessage(chunks)
	if msg.Reasoning != "ENCRYPTED_RS_123" {
		t.Errorf("Message.Reasoning = %q, want %q (the replay blob)", msg.Reasoning, "ENCRYPTED_RS_123")
	}
	if msg.ReasoningItemID != "rs_123" {
		t.Errorf("Message.ReasoningItemID = %q, want %q (reasoning attributed to the reasoning item, not the message item and not empty)",
			msg.ReasoningItemID, "rs_123")
	}
	if msg.Text != "The answer is 42." {
		t.Errorf("Message.Text = %q, want %q", msg.Text, "The answer is 42.")
	}
}

// aggregateAssistantMessage mirrors the chunk→session.Message aggregation the
// engine loop performs (engine/agent/loop.go: ReasoningItemID is
// last-non-empty-wins alongside the additive Reasoning blob). It lives here as
// the provider package's view of what the loop will build from this stream, so
// the regression is pinned at the adapter boundary without importing engine/agent
// (which the provider module deliberately does not depend on).
func aggregateAssistantMessage(chunks []port.Chunk) session.Message {
	var text, reasoning, reasoningItemID string
	var calls []session.ToolCall
	stop := session.StopNone
	var usage session.Usage
	for _, c := range chunks {
		switch c.Kind {
		case port.ChunkText:
			text += c.Text
		case port.ChunkReasoning:
			// display-only summary; not the replay blob
		case port.ChunkReasoningItem:
			reasoning += c.Text
			if c.ReasoningItemID != "" {
				reasoningItemID = c.ReasoningItemID
			}
		case port.ChunkToolCall:
			if c.ToolCall != nil {
				calls = append(calls, *c.ToolCall)
			}
		case port.ChunkUsage:
			if c.Usage != nil {
				usage = usage.Add(*c.Usage)
			}
		case port.ChunkDone:
			stop = c.Stop
		}
	}
	msg := session.NewAssistantMessage(text, reasoning, calls)
	msg.ReasoningItemID = reasoningItemID
	_ = stop
	_ = usage
	return msg
}
