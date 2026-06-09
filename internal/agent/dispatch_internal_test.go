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
			got := newAskID(tc.sessID, tc.n, tc.callID)
			if !strings.HasPrefix(got, string(tc.sessID)+":") {
				t.Fatalf("newAskID(%q, %d, %q) = %q, must start with %q (the prefix cmd/mecatui isChildAsk consumes)",
					tc.sessID, tc.n, tc.callID, got, string(tc.sessID)+":")
			}
		})
	}
}
