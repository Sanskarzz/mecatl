package toolkit

import (
	"encoding/json"
	"strings"
	"testing"

	"github.com/stacklok/ozzharness/internal/session"
)

func TestTruncate(t *testing.T) {
	const marker = "\n... [output truncated: exceeded 25000 bytes]"

	t.Run("under the cap is returned unchanged", func(t *testing.T) {
		s := strings.Repeat("a", 10)
		if got := Truncate(s, 10); got != s {
			t.Fatalf("Truncate at exact cap mutated input: got %q", got)
		}
		if got := Truncate(s, 11); got != s {
			t.Fatalf("Truncate below cap mutated input: got %q", got)
		}
	})

	t.Run("over the cap is trimmed with the marker", func(t *testing.T) {
		s := strings.Repeat("a", 20)
		got := Truncate(s, 10)
		want := strings.Repeat("a", 10) + marker
		if got != want {
			t.Fatalf("Truncate over cap = %q, want %q", got, want)
		}
	})

	t.Run("cuts on a rune boundary to keep valid UTF-8", func(t *testing.T) {
		// "é" is two bytes (0xC3 0xA9). A cap landing mid-rune must back off.
		s := "aaaé" + strings.Repeat("b", 10)
		got := Truncate(s, 4) // byte 4 is the second byte of "é".
		if !strings.HasPrefix(got, "aaa") {
			t.Fatalf("expected prefix aaa, got %q", got)
		}
		body := strings.TrimSuffix(got, marker)
		if body != "aaa" {
			t.Fatalf("expected rune-safe body %q, got %q", "aaa", body)
		}
	})
}

func TestParseArgs(t *testing.T) {
	type args struct {
		Key string `json:"key"`
	}

	t.Run("valid payload unmarshals", func(t *testing.T) {
		var a args
		msg, ok := ParseArgs(session.ToolCall{Args: json.RawMessage(`{"key":"v"}`)}, &a)
		if !ok {
			t.Fatalf("ParseArgs failed on valid payload: %q", msg)
		}
		if a.Key != "v" {
			t.Fatalf("Key = %q, want v", a.Key)
		}
	})

	t.Run("empty payload is treated as empty object", func(t *testing.T) {
		var a args
		if msg, ok := ParseArgs(session.ToolCall{}, &a); !ok {
			t.Fatalf("ParseArgs failed on empty payload: %q", msg)
		}
	})

	t.Run("malformed payload returns a model-facing error", func(t *testing.T) {
		var a args
		msg, ok := ParseArgs(session.ToolCall{Args: json.RawMessage(`{bad`)}, &a)
		if ok {
			t.Fatal("ParseArgs accepted malformed JSON")
		}
		if !strings.HasPrefix(msg, "invalid arguments:") {
			t.Fatalf("error message = %q, want prefix %q", msg, "invalid arguments:")
		}
	})
}

func TestSchema(t *testing.T) {
	lit := `{"type":"object"}`
	got := Schema(lit)
	if string(got) != lit {
		t.Fatalf("Schema = %q, want %q", string(got), lit)
	}
	// Result is valid JSON usable as a RawMessage.
	var v any
	if err := json.Unmarshal(got, &v); err != nil {
		t.Fatalf("Schema output is not valid JSON: %v", err)
	}
}

func TestMaxOutputBytes(t *testing.T) {
	if MaxOutputBytes != 25_000 {
		t.Fatalf("MaxOutputBytes = %d, want 25000", MaxOutputBytes)
	}
}
