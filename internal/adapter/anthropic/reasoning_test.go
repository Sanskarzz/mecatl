package anthropic

import (
	"reflect"
	"testing"
)

// TestReasoningRoundTrip is the load-bearing correctness assertion: an ordered
// list of thinking + redacted blocks packs into the opaque string and unpacks
// byte-identically, preserving ORDER and signatures.
func TestReasoningRoundTrip(t *testing.T) {
	in := []reasoningBlock{
		{Kind: reasoningKindThinking, Thinking: "first thought", Signature: "SIG-1=="},
		{Kind: reasoningKindRedacted, Data: "REDACTED=="},
		{Kind: reasoningKindThinking, Thinking: "", Signature: "SIG-OMITTED=="}, // omitted-display: empty text, signature carries it
	}
	packed := packReasoning(in)
	if packed == "" {
		t.Fatal("packReasoning produced empty string for a non-empty list")
	}
	out := unpackReasoning(packed)
	if !reflect.DeepEqual(in, out) {
		t.Fatalf("round-trip mismatch:\n in: %+v\nout: %+v", in, out)
	}
}

func TestReasoningEmptyIsNoOp(t *testing.T) {
	if got := packReasoning(nil); got != "" {
		t.Errorf("packReasoning(nil) = %q, want empty", got)
	}
	if got := unpackReasoning(""); got != nil {
		t.Errorf("unpackReasoning(\"\") = %+v, want nil", got)
	}
}

func TestReasoningOrderPreserved(t *testing.T) {
	in := []reasoningBlock{
		{Kind: reasoningKindThinking, Thinking: "A", Signature: "a"},
		{Kind: reasoningKindThinking, Thinking: "B", Signature: "b"},
		{Kind: reasoningKindThinking, Thinking: "C", Signature: "c"},
	}
	out := unpackReasoning(packReasoning(in))
	for i := range in {
		if out[i].Thinking != in[i].Thinking {
			t.Fatalf("block[%d] reordered: got %q want %q", i, out[i].Thinking, in[i].Thinking)
		}
	}
}

func TestUnpackReasoningFailSoft(t *testing.T) {
	cases := []string{
		"not json",
		"{}",
		`{"v":99,"blocks":[{"t":"thinking","x":"a"}]}`, // wrong version
		`{"v":1,"blocks":[{"t":"unknown"}]}`,           // unknown kind dropped
		`{"v":1,"blocks":[]}`,
	}
	for _, c := range cases {
		if got := unpackReasoning(c); got != nil {
			t.Errorf("unpackReasoning(%q) = %+v, want nil (fail-soft)", c, got)
		}
	}
}
