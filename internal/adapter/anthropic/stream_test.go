package anthropic

import (
	"encoding/json"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"

	sdk "github.com/anthropics/anthropic-sdk-go"

	"github.com/stacklok/mecatl/engine/port"
	"github.com/stacklok/mecatl/engine/session"
)

func decodeFixture(t *testing.T, name string) []port.Chunk {
	t.Helper()
	f, err := os.Open(filepath.Join("testdata", name))
	if err != nil {
		t.Fatalf("open fixture: %v", err)
	}
	defer f.Close()
	chunks, err := decodeSSE(f)
	if err != nil {
		t.Fatalf("decodeSSE: %v", err)
	}
	return chunks
}

func decodeFixtureErr(t *testing.T, name string) ([]port.Chunk, error) {
	t.Helper()
	f, err := os.Open(filepath.Join("testdata", name))
	if err != nil {
		t.Fatalf("open fixture: %v", err)
	}
	defer f.Close()
	return decodeSSE(f)
}

func assertChunks(t *testing.T, got, want []port.Chunk) {
	t.Helper()
	if len(got) != len(want) {
		t.Fatalf("chunk count = %d, want %d\n got: %+v\nwant: %+v", len(got), len(want), got, want)
	}
	for i := range want {
		if got[i].Kind != want[i].Kind {
			t.Errorf("chunk[%d].Kind = %v, want %v", i, got[i].Kind, want[i].Kind)
		}
		if got[i].Text != want[i].Text {
			t.Errorf("chunk[%d].Text = %q, want %q", i, got[i].Text, want[i].Text)
		}
		if !reflect.DeepEqual(got[i].ToolCall, want[i].ToolCall) {
			t.Errorf("chunk[%d].ToolCall = %+v, want %+v", i, got[i].ToolCall, want[i].ToolCall)
		}
		if !reflect.DeepEqual(got[i].Usage, want[i].Usage) {
			t.Errorf("chunk[%d].Usage = %+v, want %+v", i, got[i].Usage, want[i].Usage)
		}
		if got[i].Stop != want[i].Stop {
			t.Errorf("chunk[%d].Stop = %v, want %v", i, got[i].Stop, want[i].Stop)
		}
	}
}

func TestTranslateTextTurn(t *testing.T) {
	got := decodeFixture(t, "text_turn.sse")
	want := []port.Chunk{
		{Kind: port.ChunkText, Text: "Hello"},
		{Kind: port.ChunkText, Text: ", world"},
		{Kind: port.ChunkUsage, Usage: &session.Usage{InputTokens: 12, OutputTokens: 4, CacheReadTokens: 0}},
		{Kind: port.ChunkDone, Stop: session.StopEndTurn},
	}
	assertChunks(t, got, want)
}

func TestTranslateToolCallTurn(t *testing.T) {
	got := decodeFixture(t, "tool_call_turn.sse")
	want := []port.Chunk{
		{Kind: port.ChunkText, Text: "Let me read it."},
		{Kind: port.ChunkToolCall, ToolCall: &session.ToolCall{
			ID:   "toolu_abc",
			Name: "read_file",
			Args: json.RawMessage(`{"path":"main.go"}`),
		}},
		{Kind: port.ChunkUsage, Usage: &session.Usage{InputTokens: 40, OutputTokens: 9, CacheReadTokens: 32}},
		{Kind: port.ChunkDone, Stop: session.StopEndTurn},
	}
	assertChunks(t, got, want)
}

// TestTranslateThinkingToolTurn is the streaming half of the 400-trap round-trip:
// a thinking block (thinking deltas + a signature delta) precedes a tool_use, and
// the adapter emits the DISPLAY reasoning deltas, ONE packed ChunkReasoningItem
// (the replay blob), and the assembled ChunkToolCall.
func TestTranslateThinkingToolTurn(t *testing.T) {
	got := decodeFixture(t, "thinking_tool_turn.sse")

	// Display reasoning deltas first.
	if got[0].Kind != port.ChunkReasoning || got[0].Text != "I should " {
		t.Fatalf("chunk[0] = %+v, want ChunkReasoning 'I should '", got[0])
	}
	if got[1].Kind != port.ChunkReasoning || got[1].Text != "read the file." {
		t.Fatalf("chunk[1] = %+v, want ChunkReasoning 'read the file.'", got[1])
	}
	// Tool call assembled at its content_block_stop.
	if got[2].Kind != port.ChunkToolCall || got[2].ToolCall.Name != "read_file" {
		t.Fatalf("chunk[2] = %+v, want ChunkToolCall read_file", got[2])
	}
	if string(got[2].ToolCall.Args) != `{"path":"main.go"}` {
		t.Fatalf("tool args = %q", got[2].ToolCall.Args)
	}
	// The packed reasoning replay blob is emitted ONCE at message_stop.
	if got[3].Kind != port.ChunkReasoningItem {
		t.Fatalf("chunk[3].Kind = %v, want ChunkReasoningItem", got[3].Kind)
	}
	blocks := unpackReasoning(got[3].Text)
	if len(blocks) != 1 || blocks[0].Kind != reasoningKindThinking {
		t.Fatalf("packed reasoning = %+v, want one thinking block", blocks)
	}
	if blocks[0].Thinking != "I should read the file." {
		t.Errorf("packed thinking text = %q", blocks[0].Thinking)
	}
	if blocks[0].Signature != "SIG-abc123==" {
		t.Errorf("packed signature = %q, want SIG-abc123==", blocks[0].Signature)
	}
	// Then usage + done.
	if got[4].Kind != port.ChunkUsage {
		t.Fatalf("chunk[4].Kind = %v, want ChunkUsage", got[4].Kind)
	}
	if got[5].Kind != port.ChunkDone || got[5].Stop != session.StopEndTurn {
		t.Fatalf("chunk[5] = %+v, want ChunkDone StopEndTurn", got[5])
	}
}

func TestTranslateRedactedThinkingTurn(t *testing.T) {
	got := decodeFixture(t, "redacted_thinking_turn.sse")
	// text delta, then packed reasoning item, usage, done.
	if got[0].Kind != port.ChunkText || got[0].Text != "Done." {
		t.Fatalf("chunk[0] = %+v, want ChunkText 'Done.'", got[0])
	}
	if got[1].Kind != port.ChunkReasoningItem {
		t.Fatalf("chunk[1].Kind = %v, want ChunkReasoningItem", got[1].Kind)
	}
	blocks := unpackReasoning(got[1].Text)
	if len(blocks) != 1 || blocks[0].Kind != reasoningKindRedacted {
		t.Fatalf("packed reasoning = %+v, want one redacted block", blocks)
	}
	if blocks[0].Data != "REDACTED-OPAQUE-DATA==" {
		t.Errorf("packed redacted data = %q", blocks[0].Data)
	}
}

func TestTranslateErrorEvent(t *testing.T) {
	_, err := decodeFixtureErr(t, "error_event.sse")
	if err == nil {
		t.Fatal("expected a terminal error from the error event")
	}
	if got := err.Error(); got != "stream error: overloaded_error: Overloaded" {
		t.Errorf("error = %q", got)
	}
}

func TestTranslateMaxTokensStop(t *testing.T) {
	got := decodeFixture(t, "max_tokens_turn.sse")
	last := got[len(got)-1]
	if last.Kind != port.ChunkDone || last.Stop != session.StopError {
		t.Fatalf("terminal chunk = %+v, want ChunkDone StopError (max_tokens)", last)
	}
}

// TestTranslateToolArgsSplitAcrossDeltas guards the accumulator's one corruption
// point: a tool_use whose JSON is split across THREE input_json_delta events must
// reassemble byte-exact.
func TestTranslateToolArgsSplitAcrossDeltas(t *testing.T) {
	got := decodeFixture(t, "tool_call_split_args.sse")
	var call *session.ToolCall
	for _, c := range got {
		if c.Kind == port.ChunkToolCall {
			call = c.ToolCall
		}
	}
	if call == nil {
		t.Fatal("no tool call assembled")
	}
	want := `{"path":"a/b.go","content":"package main"}`
	if string(call.Args) != want {
		t.Fatalf("reassembled args = %q, want %q", call.Args, want)
	}
}

// TestTranslateEmptyArgsToolCall: a tool_use with no input_json_delta yields an
// empty JSON object {} (a valid argument-less call).
func TestTranslateEmptyArgsToolCall(t *testing.T) {
	got := decodeFixture(t, "tool_call_empty_args.sse")
	var call *session.ToolCall
	for _, c := range got {
		if c.Kind == port.ChunkToolCall {
			call = c.ToolCall
		}
	}
	if call == nil {
		t.Fatal("no tool call assembled")
	}
	if string(call.Args) != "{}" {
		t.Fatalf("empty-args call Args = %q, want {}", call.Args)
	}
}

// TestTranslateTwoTextBlocksTripwire: two distinct assistant text blocks in one
// turn is a LOUD error (the domain Message.Text is a single string).
func TestTranslateTwoTextBlocksTripwire(t *testing.T) {
	_, err := decodeFixtureErr(t, "two_text_blocks.sse")
	if err == nil {
		t.Fatal("expected a loud error for two distinct text blocks in one turn")
	}
	if !strings.Contains(err.Error(), "single visible text part") &&
		!strings.Contains(err.Error(), "second assistant text block") {
		t.Fatalf("error = %q, want the multi-text tripwire", err)
	}
}

// TestStreamBufferCap proves the per-block accumulation is bounded: a flood of
// input_json_delta fragments exceeding maxBlockBufBytes fails the translation
// rather than growing unbounded (cheap MITM/DoS hardening).
func TestStreamBufferCap(t *testing.T) {
	var st streamState
	start := sdk.MessageStreamEventUnion{Type: "content_block_start", Index: 0}
	start.ContentBlock.Type = "tool_use"
	start.ContentBlock.ID = "toolu_flood"
	start.ContentBlock.Name = "x"
	if _, err := translate(start, &st); err != nil {
		t.Fatalf("block start: %v", err)
	}
	chunk := strings.Repeat("a", 1<<20) // 1 MiB
	var capErr error
	for i := 0; i < (maxBlockBufBytes/(1<<20))+2; i++ {
		ev := sdk.MessageStreamEventUnion{Type: "content_block_delta", Index: 0}
		ev.Delta.Type = "input_json_delta"
		ev.Delta.PartialJSON = chunk
		if _, err := translate(ev, &st); err != nil {
			capErr = err
			break
		}
	}
	if capErr == nil {
		t.Fatal("tool-args buffer grew past the cap without failing the stream")
	}
	if !strings.Contains(capErr.Error(), "buffer exceeded") {
		t.Fatalf("cap error = %q, want a buffer-exceeded error", capErr)
	}
}
