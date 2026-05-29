package tokenizer_test

import (
	"testing"

	"github.com/stacklok/ozzharness/internal/adapter/tokenizer"
	"github.com/stacklok/ozzharness/internal/session"
)

// TestCountKnownStrings checks the tiktoken-backed counter returns the canonical
// token counts for known strings, fully offline (the vocab is compiled in). These
// counts match OpenAI's tiktoken reference for the respective encodings.
func TestCountKnownStrings(t *testing.T) {
	cases := []struct {
		enc   tokenizer.Encoding
		text  string
		count int
	}{
		{tokenizer.Cl100kBase, "hello world", 2},
		{tokenizer.O200kBase, "hello world", 2},
		{tokenizer.Cl100kBase, "tiktoken is great!", 6},
		{tokenizer.Cl100kBase, "", 0},
	}
	for _, tc := range cases {
		c, err := tokenizer.New(tc.enc)
		if err != nil {
			t.Fatalf("New(%q): %v", tc.enc, err)
		}
		if got := c.Count(tc.text); got != tc.count {
			t.Fatalf("Count(%q) with %s = %d, want %d", tc.text, tc.enc, got, tc.count)
		}
	}
}

// TestNewForModel resolves a known model to a working counter and falls back to
// o200k_base for an unknown model rather than erroring.
func TestNewForModel(t *testing.T) {
	for _, model := range []string{"gpt-4o", "gpt-5", "some-unknown-model"} {
		c, err := tokenizer.NewForModel(model)
		if err != nil {
			t.Fatalf("NewForModel(%q): %v", model, err)
		}
		if got := c.Count("hello world"); got != 2 {
			t.Fatalf("NewForModel(%q).Count = %d, want 2", model, got)
		}
	}
}

// TestCountMessages sums encoded bodies plus framing overhead and is deterministic.
func TestCountMessages(t *testing.T) {
	c, err := tokenizer.New(tokenizer.O200kBase)
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	msgs := []session.Message{
		session.NewSystemMessage("hello world"),
		session.NewUserMessage("hello world"),
	}
	got := c.CountMessages(msgs)
	// Two messages: 2*(perMessageOverhead=4) framing + 2*(2 tokens body) = 12.
	if got != 12 {
		t.Fatalf("CountMessages = %d, want 12", got)
	}
	if a, b := c.CountMessages(msgs), c.CountMessages(msgs); a != b {
		t.Fatalf("CountMessages not deterministic: %d vs %d", a, b)
	}
}

// TestNewUnknownEncoding returns an error for an unrecognised encoding name.
func TestNewUnknownEncoding(t *testing.T) {
	if _, err := tokenizer.New("not-a-real-encoding"); err == nil {
		t.Fatalf("expected error for unknown encoding")
	}
}
