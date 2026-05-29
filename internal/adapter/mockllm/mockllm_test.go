package mockllm

import (
	"context"
	"encoding/json"
	"testing"

	"github.com/stacklok/ozzharness/internal/port"
	"github.com/stacklok/ozzharness/internal/session"
)

func collect(t *testing.T, p *Provider) []port.Chunk {
	t.Helper()
	seq, err := p.Stream(context.Background(), port.LLMRequest{})
	if err != nil {
		t.Fatalf("Stream returned error: %v", err)
	}
	var got []port.Chunk
	for c, err := range seq {
		if err != nil {
			t.Fatalf("iterator error: %v", err)
		}
		got = append(got, c)
	}
	return got
}

func TestTextTurn(t *testing.T) {
	p := New(TextTurn("hi"))
	got := collect(t, p)
	if len(got) != 3 {
		t.Fatalf("got %d chunks, want 3: %+v", len(got), got)
	}
	if got[0].Kind != port.ChunkText || got[0].Text != "hi" {
		t.Errorf("chunk 0 = %+v, want text 'hi'", got[0])
	}
	if got[1].Kind != port.ChunkUsage {
		t.Errorf("chunk 1 = %+v, want usage", got[1])
	}
	if got[2].Kind != port.ChunkDone || got[2].Stop != session.StopEndTurn {
		t.Errorf("chunk 2 = %+v, want done end_turn", got[2])
	}
}

func TestToolCallTurn(t *testing.T) {
	call := session.NewToolCall("call_1", "read_file", json.RawMessage(`{"path":"a.go"}`))
	p := New(ToolCallTurn(call))
	got := collect(t, p)
	if got[0].Kind != port.ChunkToolCall {
		t.Fatalf("chunk 0 = %+v, want tool call", got[0])
	}
	if got[0].ToolCall.ID != "call_1" || got[0].ToolCall.Name != "read_file" {
		t.Errorf("tool call = %+v", got[0].ToolCall)
	}
}

func TestDeterminismAndCursorAdvance(t *testing.T) {
	call := session.NewToolCall("c1", "t", nil)
	p := New(TextTurn("first"), ToolCallTurn(call), TextTurn("third"))

	// Each Stream call advances to the next turn.
	t1 := collect(t, p)
	if t1[0].Text != "first" {
		t.Fatalf("turn 1 first chunk = %q, want 'first'", t1[0].Text)
	}
	t2 := collect(t, p)
	if t2[0].Kind != port.ChunkToolCall {
		t.Fatalf("turn 2 first chunk kind = %v, want tool call", t2[0].Kind)
	}
	t3 := collect(t, p)
	if t3[0].Text != "third" {
		t.Fatalf("turn 3 first chunk = %q, want 'third'", t3[0].Text)
	}
	if p.Calls() != 3 {
		t.Fatalf("Calls() = %d, want 3", p.Calls())
	}

	// Exhausted script yields an empty stream, no panic.
	t4 := collect(t, p)
	if len(t4) != 0 {
		t.Fatalf("exhausted turn yielded %d chunks, want 0", len(t4))
	}

	// Reset replays from the start deterministically.
	p.Reset()
	again := collect(t, p)
	if again[0].Text != "first" {
		t.Fatalf("after Reset first chunk = %q, want 'first'", again[0].Text)
	}
}

func TestContextCancelStopsYielding(t *testing.T) {
	// A turn with many chunks; cancel after the first to prove the iterator
	// stops yielding mid-stream.
	turn := ChunksTurn(
		TextChunk("a"),
		TextChunk("b"),
		TextChunk("c"),
		DoneChunk(session.StopEndTurn),
	)
	p := New(turn)

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	seq, err := p.Stream(ctx, port.LLMRequest{})
	if err != nil {
		t.Fatalf("Stream error: %v", err)
	}

	var got []port.Chunk
	for c, err := range seq {
		if err != nil {
			t.Fatalf("iterator error: %v", err)
		}
		got = append(got, c)
		cancel() // cancel after the first chunk
	}
	if len(got) != 1 {
		t.Fatalf("got %d chunks after cancel, want 1", len(got))
	}
	if got[0].Text != "a" {
		t.Fatalf("first chunk = %q, want 'a'", got[0].Text)
	}
}

func TestCancelBeforeIterationYieldsNothing(t *testing.T) {
	p := New(TextTurn("hi"))
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	seq, err := p.Stream(ctx, port.LLMRequest{})
	if err != nil {
		t.Fatalf("Stream error: %v", err)
	}
	var n int
	for range seq {
		n++
	}
	if n != 0 {
		t.Fatalf("got %d chunks with pre-cancelled ctx, want 0", n)
	}
}
