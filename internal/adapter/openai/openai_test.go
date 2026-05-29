package openai

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/stacklok/ozzharness/internal/port"
	"github.com/stacklok/ozzharness/internal/prompt"
	"github.com/stacklok/ozzharness/internal/session"
	"github.com/stacklok/ozzharness/internal/tool"
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

// decodeFixtureErr decodes a fixture expecting a terminal error, returning the
// chunks emitted before the error and the error itself.
func decodeFixtureErr(t *testing.T, name string) ([]port.Chunk, error) {
	t.Helper()
	f, err := os.Open(filepath.Join("testdata", name))
	if err != nil {
		t.Fatalf("open fixture: %v", err)
	}
	defer f.Close()
	return decodeSSE(f)
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

func TestTranslateFunctionCallTurn(t *testing.T) {
	got := decodeFixture(t, "function_call_turn.sse")
	want := []port.Chunk{
		{Kind: port.ChunkToolCall, ToolCall: &session.ToolCall{
			ID:   "call_abc",
			Name: "read_file",
			Args: json.RawMessage(`{"path":"main.go"}`),
		}},
		{Kind: port.ChunkUsage, Usage: &session.Usage{InputTokens: 40, OutputTokens: 9, CacheReadTokens: 32}},
		{Kind: port.ChunkDone, Stop: session.StopEndTurn},
	}
	assertChunks(t, got, want)
}

func TestTranslateReasoningTurnWithCachedTokens(t *testing.T) {
	got := decodeFixture(t, "reasoning_turn.sse")
	want := []port.Chunk{
		{Kind: port.ChunkReasoning, Text: "Let me think"},
		{Kind: port.ChunkReasoning, Text: " about this."},
		{Kind: port.ChunkText, Text: "Answer."},
		{Kind: port.ChunkUsage, Usage: &session.Usage{InputTokens: 100, OutputTokens: 50, CacheReadTokens: 80}},
		{Kind: port.ChunkDone, Stop: session.StopEndTurn},
	}
	assertChunks(t, got, want)
}

// TestTranslateErrorEvent verifies a top-level "error" stream event surfaces a
// non-nil error carrying the provider's code, message, and offending param,
// rather than a bare StopError chunk that drops the reason.
func TestTranslateErrorEvent(t *testing.T) {
	chunks, err := decodeFixtureErr(t, "error_event.sse")
	if err == nil {
		t.Fatal("expected an error from the error event, got nil")
	}
	if len(chunks) != 0 {
		t.Errorf("expected no chunks before the error, got %+v", chunks)
	}
	for _, want := range []string{"rate_limit_exceeded", "Rate limit reached for requests", "model"} {
		if !strings.Contains(err.Error(), want) {
			t.Errorf("error %q does not contain %q", err.Error(), want)
		}
	}
}

// TestTranslateResponseFailed verifies a "response.failed" event surfaces the
// response.error code+message as a non-nil error.
func TestTranslateResponseFailed(t *testing.T) {
	chunks, err := decodeFixtureErr(t, "response_failed.sse")
	if err == nil {
		t.Fatal("expected an error from response.failed, got nil")
	}
	if len(chunks) != 0 {
		t.Errorf("expected no chunks before the error, got %+v", chunks)
	}
	for _, want := range []string{"server_error", "The model produced an internal error"} {
		if !strings.Contains(err.Error(), want) {
			t.Errorf("error %q does not contain %q", err.Error(), want)
		}
	}
}

func assertChunks(t *testing.T, got, want []port.Chunk) {
	t.Helper()
	if len(got) != len(want) {
		t.Fatalf("got %d chunks, want %d:\n got=%+v\nwant=%+v", len(got), len(want), got, want)
	}
	for i := range want {
		g, w := got[i], want[i]
		if g.Kind != w.Kind {
			t.Errorf("chunk %d kind = %v, want %v", i, g.Kind, w.Kind)
		}
		if g.Text != w.Text {
			t.Errorf("chunk %d text = %q, want %q", i, g.Text, w.Text)
		}
		if g.Stop != w.Stop {
			t.Errorf("chunk %d stop = %q, want %q", i, g.Stop, w.Stop)
		}
		if (g.ToolCall == nil) != (w.ToolCall == nil) {
			t.Errorf("chunk %d tool-call presence mismatch: got %v want %v", i, g.ToolCall, w.ToolCall)
		} else if w.ToolCall != nil {
			if g.ToolCall.ID != w.ToolCall.ID || g.ToolCall.Name != w.ToolCall.Name || string(g.ToolCall.Args) != string(w.ToolCall.Args) {
				t.Errorf("chunk %d tool call = %+v, want %+v", i, g.ToolCall, w.ToolCall)
			}
		}
		if (g.Usage == nil) != (w.Usage == nil) {
			t.Errorf("chunk %d usage presence mismatch", i)
		} else if w.Usage != nil && *g.Usage != *w.Usage {
			t.Errorf("chunk %d usage = %+v, want %+v", i, *g.Usage, *w.Usage)
		}
	}
}

func TestBuildParams(t *testing.T) {
	schema := json.RawMessage(`{"type":"object","properties":{"path":{"type":"string"}},"required":["path"],"additionalProperties":false}`)
	req := port.LLMRequest{
		System: prompt.Layered{StablePrefix: "You are a coding agent.", VolatileSuffix: "<env>linux</env>"},
		Model:  "gpt-5.2",
		Tools: []tool.ToolSpec{
			{Name: "read_file", Description: "Read a file.", Schema: schema},
		},
		Messages: []session.Message{
			session.NewUserMessage("open main.go"),
			session.NewAssistantMessage("", "REASONING_BLOB", []session.ToolCall{
				session.NewToolCall("call_1", "read_file", json.RawMessage(`{"path":"main.go"}`)),
			}),
			session.NewToolMessage(session.NewToolResult("call_1", "package main")),
			session.NewAssistantMessage("done", "", nil),
		},
	}
	params, err := buildParams(req)
	if err != nil {
		t.Fatalf("buildParams: %v", err)
	}

	if params.Model != "gpt-5.2" {
		t.Errorf("Model = %q, want gpt-5.2", params.Model)
	}
	if got := params.Instructions.Value; got != "You are a coding agent.\n\n<env>linux</env>" {
		t.Errorf("Instructions = %q", got)
	}
	if v := params.Store.Value; v != false {
		t.Errorf("Store = %v, want false", v)
	}
	if len(params.Include) != 1 || params.Include[0] != "reasoning.encrypted_content" {
		t.Errorf("Include = %v, want [reasoning.encrypted_content]", params.Include)
	}
	if len(params.Tools) != 1 {
		t.Fatalf("Tools len = %d, want 1", len(params.Tools))
	}
	fn := params.Tools[0].OfFunction
	if fn == nil || fn.Name != "read_file" {
		t.Fatalf("tool 0 = %+v, want function read_file", params.Tools[0])
	}
	if fn.Parameters["type"] != "object" {
		t.Errorf("tool params type = %v, want object", fn.Parameters["type"])
	}

	// The input item list must round-trip via JSON to the expected wire shape:
	// user message, reasoning (encrypted), function_call, function_call_output,
	// assistant message.
	raw, err := json.Marshal(params.Input.OfInputItemList)
	if err != nil {
		t.Fatalf("marshal input: %v", err)
	}
	var items []map[string]any
	if err := json.Unmarshal(raw, &items); err != nil {
		t.Fatalf("unmarshal input: %v", err)
	}
	if len(items) != 5 {
		t.Fatalf("input items = %d, want 5:\n%s", len(items), raw)
	}
	wantTypesOrSentinels := []func(map[string]any) bool{
		func(m map[string]any) bool { return m["role"] == "user" },
		func(m map[string]any) bool {
			return m["type"] == "reasoning" && m["encrypted_content"] == "REASONING_BLOB"
		},
		func(m map[string]any) bool { return m["type"] == "function_call" && m["call_id"] == "call_1" },
		func(m map[string]any) bool { return m["type"] == "function_call_output" && m["call_id"] == "call_1" },
		func(m map[string]any) bool { return m["role"] == "assistant" },
	}
	for i, check := range wantTypesOrSentinels {
		if !check(items[i]) {
			t.Errorf("input item %d unexpected: %v", i, items[i])
		}
	}
}

func TestBuildParamsInvalidSchema(t *testing.T) {
	req := port.LLMRequest{
		Model: "m",
		Tools: []tool.ToolSpec{{Name: "bad", Schema: json.RawMessage(`{not json`)}},
	}
	if _, err := buildParams(req); err == nil {
		t.Fatal("expected error for invalid tool schema, got nil")
	}
}

// TestStreamContextCancel drives the real Stream method against a stub SSE
// server that streams slowly, and cancels the context mid-stream. The iterator
// must stop yielding without reporting the cancellation as a stream error.
func TestStreamContextCancel(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/event-stream")
		fl, ok := w.(http.Flusher)
		if !ok {
			t.Error("response writer is not a flusher")
			return
		}
		w.WriteHeader(http.StatusOK)
		// Emit a couple of deltas slowly so the client can cancel mid-stream.
		for i := 0; i < 50; i++ {
			_, _ = w.Write([]byte("event: response.output_text.delta\n"))
			_, _ = w.Write([]byte(`data: {"type":"response.output_text.delta","sequence_number":` +
				itoa(i) + `,"delta":"x"}` + "\n\n"))
			fl.Flush()
			select {
			case <-r.Context().Done():
				return
			case <-time.After(20 * time.Millisecond):
			}
		}
	}))
	defer srv.Close()

	p := New(WithAPIKey("test-key"), WithBaseURL(srv.URL+"/v1"))

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	seq, err := p.Stream(ctx, port.LLMRequest{Model: "m", Messages: []session.Message{session.NewUserMessage("hi")}})
	if err != nil {
		t.Fatalf("Stream returned outer error: %v", err)
	}

	var n int
	for c, err := range seq {
		if err != nil {
			t.Fatalf("iterator surfaced error on cancel: %v", err)
		}
		_ = c
		n++
		if n == 2 {
			cancel()
		}
	}
	if n < 1 {
		t.Fatal("expected at least one chunk before cancel")
	}
	if n >= 50 {
		t.Fatalf("got %d chunks; cancellation did not stop the stream", n)
	}
}

func itoa(i int) string {
	return string(rune('0' + i%10)) // single-digit-ish; sequence_number value is not asserted
}
