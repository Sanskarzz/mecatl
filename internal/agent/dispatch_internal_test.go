package agent

import (
	"strings"
	"testing"

	"github.com/stacklok/mecatl/internal/session"
)

// TestNewAskIDSessionPrefixContract pins the askID namespace contract at the
// PRODUCER: every askID starts with "<sessionID>:". cmd/mecatui's isChildAsk
// consumes this prefix to classify a surfaced ask as main-agent vs subagent (the
// child session id IS the namespace), so changing newAskID's format silently
// breaks that client-side classification — this test makes the break loud here,
// where the format is minted.
func TestNewAskIDSessionPrefixContract(t *testing.T) {
	cases := []struct {
		name   string
		sessID session.SessionID
		n      int
		callID session.ToolCallID
	}{
		{"main session id", "sess-abc123", 1, "call-9"},
		{"child session id", "subagent-call-9", 3, "k1"},
		{"zero counter", "sess-x", 0, "c0"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got := newAskID(tc.sessID, tc.n, tc.callID, 7)
			if !strings.HasPrefix(got, string(tc.sessID)+":") {
				t.Fatalf("newAskID(%q, %d, %q) = %q, must start with %q (the prefix cmd/mecatui isChildAsk consumes)",
					tc.sessID, tc.n, tc.callID, got, string(tc.sessID)+":")
			}
		})
	}
}

// TestNewAskIDRunSerialDisjoint pins the per-RUN suffix: identical (session, n,
// call) inputs under DIFFERENT runs must mint DIFFERENT askIDs — the CWE-863
// brake (a retracted run's replayed verdict must never resolve a later run's
// re-minted ask). The suffix must not disturb the consumed prefix.
func TestNewAskIDRunSerialDisjoint(t *testing.T) {
	a := newAskID("subagent-p1", 0, "k1", 1)
	b := newAskID("subagent-p1", 0, "k1", 2)
	if a == b {
		t.Fatalf("askIDs of two runs over the same (session, n, call) must differ; both = %q", a)
	}
	if !strings.HasPrefix(a, "subagent-p1:") || !strings.HasPrefix(b, "subagent-p1:") {
		t.Fatalf("the run-serial suffix must not disturb the consumed session-id prefix: %q / %q", a, b)
	}
}
