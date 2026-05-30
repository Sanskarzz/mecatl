package session

import (
	"encoding/json"
	"testing"
)

// TestParseArgs locks the canonical arg-parse mechanic the agent loop and the
// adapter toolkit both delegate to: empty payload → zero-value dst + ok, valid
// JSON decodes, malformed JSON yields a model-facing error string + !ok.
func TestParseArgs(t *testing.T) {
	type payload struct {
		Key string `json:"key"`
	}

	t.Run("valid", func(t *testing.T) {
		var p payload
		msg, ok := ParseArgs(ToolCall{Args: json.RawMessage(`{"key":"v"}`)}, &p)
		if !ok || msg != "" {
			t.Fatalf("valid payload: ok=%v msg=%q", ok, msg)
		}
		if p.Key != "v" {
			t.Fatalf("decoded Key=%q, want v", p.Key)
		}
	})

	t.Run("empty leaves zero value", func(t *testing.T) {
		p := payload{Key: "untouched"}
		// A zero-value (no Args) call must not error; dst is left as-is.
		var fresh payload
		if msg, ok := ParseArgs(ToolCall{}, &fresh); !ok || msg != "" {
			t.Fatalf("empty payload: ok=%v msg=%q", ok, msg)
		}
		if fresh != (payload{}) {
			t.Fatalf("empty payload mutated dst: %+v", fresh)
		}
		_ = p
	})

	t.Run("malformed", func(t *testing.T) {
		var p payload
		msg, ok := ParseArgs(ToolCall{Args: json.RawMessage(`{bad`)}, &p)
		if ok {
			t.Fatal("malformed JSON accepted")
		}
		if msg == "" {
			t.Fatal("malformed JSON produced no model-facing message")
		}
	})
}
