package session

import (
	"encoding/json"
	"strings"
	"testing"
)

// TestValidateToolPairing pins the bidirectional pairing contract: a clean,
// fully-paired history passes; a history that opens on an orphaned tool result
// fails; a history with a dangling assistant tool call (no following result)
// fails; an empty history is trivially valid.
func TestValidateToolPairing(t *testing.T) {
	call := func(id ToolCallID) Message {
		return NewAssistantMessage("", "", []ToolCall{NewToolCall(id, "Read", json.RawMessage(`{}`))})
	}
	multiCall := func(ids ...ToolCallID) Message {
		calls := make([]ToolCall, 0, len(ids))
		for _, id := range ids {
			calls = append(calls, NewToolCall(id, "Read", json.RawMessage(`{}`)))
		}
		return NewAssistantMessage("", "", calls)
	}
	result := func(id ToolCallID) Message { return NewToolMessage(NewToolResult(id, "ok")) }

	tests := []struct {
		name    string
		msgs    []Message
		wantErr bool
	}{
		{
			name:    "empty passes",
			msgs:    nil,
			wantErr: false,
		},
		{
			name: "clean paired history passes",
			msgs: []Message{
				NewSystemMessage("sys"),
				NewUserMessage("goal"),
				call("c1"),
				result("c1"),
				NewAssistantMessage("done", "", nil),
			},
			wantErr: false,
		},
		{
			name: "leading orphan tool result fails",
			msgs: []Message{
				NewUserMessage("goal"),
				result("c1"), // no preceding assistant call for c1
			},
			wantErr: true,
		},
		{
			name: "dangling tool call fails",
			msgs: []Message{
				NewUserMessage("goal"),
				call("c1"), // no following result for c1
			},
			wantErr: true,
		},
		{
			name: "result before its call fails (order matters)",
			msgs: []Message{
				result("c1"),
				call("c1"),
			},
			wantErr: true,
		},
		{
			name: "tool message with nil result fails",
			msgs: []Message{
				{Role: RoleTool},
			},
			wantErr: true,
		},
		{
			name: "multi-call assistant, all answered, passes",
			msgs: []Message{
				NewUserMessage("goal"),
				multiCall("c1", "c2", "c3"),
				result("c1"),
				result("c2"),
				result("c3"),
			},
			wantErr: false,
		},
		{
			name: "multi-call assistant, one unanswered, fails",
			msgs: []Message{
				NewUserMessage("goal"),
				multiCall("c1", "c2", "c3"),
				result("c1"),
				result("c3"), // c2 dangles
			},
			wantErr: true,
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			err := ValidateToolPairing(tt.msgs)
			if (err != nil) != tt.wantErr {
				t.Fatalf("ValidateToolPairing err = %v, wantErr = %v", err, tt.wantErr)
			}
		})
	}

	// The dangling-ID report is deterministic: with multiple unanswered calls it
	// names the lexicographically-smallest id (sorted), so the message is stable.
	t.Run("dangling report is deterministic", func(t *testing.T) {
		err := ValidateToolPairing([]Message{multiCall("c9", "c2", "c5")})
		if err == nil {
			t.Fatalf("expected error for all-dangling multi-call")
		}
		if !strings.Contains(err.Error(), `"c2"`) {
			t.Fatalf("dangling report = %q, want it to name the smallest id c2", err.Error())
		}
	})
}
