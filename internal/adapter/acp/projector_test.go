package acp

import (
	"encoding/json"
	"testing"

	"github.com/stacklok/mecatl/internal/session"
)

func TestProjectUpdateMessageDelta(t *testing.T) {
	got, ok := projectUpdate(session.Event{Type: session.EvMessageDelta, Text: "hi"})
	if !ok {
		t.Fatal("expected a projection")
	}
	cu, isChunk := got.(chunkUpdate)
	if !isChunk {
		t.Fatalf("want chunkUpdate, got %T", got)
	}
	if cu.SessionUpdate != updateAgentMessageChunk {
		t.Errorf("sessionUpdate = %q", cu.SessionUpdate)
	}
	if cu.Content.Type != "text" || cu.Content.Text != "hi" {
		t.Errorf("content = %+v", cu.Content)
	}
}

func TestProjectUpdateReasoningDelta(t *testing.T) {
	got, ok := projectUpdate(session.Event{Type: session.EvReasoningDelta, Text: "thinking"})
	if !ok {
		t.Fatal("expected a projection")
	}
	cu := got.(chunkUpdate)
	if cu.SessionUpdate != updateAgentThoughtChunk {
		t.Errorf("sessionUpdate = %q, want %q", cu.SessionUpdate, updateAgentThoughtChunk)
	}
	if cu.Content.Text != "thinking" {
		t.Errorf("text = %q", cu.Content.Text)
	}
}

func TestProjectUpdateToolCall(t *testing.T) {
	call := session.NewToolCall("call-1", "Read", json.RawMessage(`{"path":"/x"}`))
	got, ok := projectUpdate(session.Event{Type: session.EvToolCall, ToolCall: &call})
	if !ok {
		t.Fatal("expected a projection")
	}
	tc := got.(toolCallUpdate)
	if tc.SessionUpdate != updateToolCall {
		t.Errorf("sessionUpdate = %q", tc.SessionUpdate)
	}
	if tc.ToolCallID != "call-1" {
		t.Errorf("toolCallId = %q", tc.ToolCallID)
	}
	if tc.Title != "Read" || tc.Kind != "read" {
		t.Errorf("title/kind = %q/%q", tc.Title, tc.Kind)
	}
	if tc.Status != toolStatusPending {
		t.Errorf("status = %q, want pending", tc.Status)
	}
	if string(tc.RawInput) != `{"path":"/x"}` {
		t.Errorf("rawInput = %s", tc.RawInput)
	}
}

func TestProjectUpdateToolResult(t *testing.T) {
	tests := []struct {
		name       string
		result     session.ToolResult
		wantStatus string
	}{
		{"success", session.NewToolResult("call-2", "done"), toolStatusCompleted},
		{"error", session.NewToolError("call-3", "boom"), toolStatusFailed},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			got, ok := projectUpdate(session.Event{Type: session.EvToolResult, ToolResult: &tc.result})
			if !ok {
				t.Fatal("expected a projection")
			}
			u := got.(toolCallUpdate)
			if u.SessionUpdate != updateToolCallUpdate {
				t.Errorf("sessionUpdate = %q", u.SessionUpdate)
			}
			if u.ToolCallID != string(tc.result.CallID) {
				t.Errorf("toolCallId = %q", u.ToolCallID)
			}
			if u.Status != tc.wantStatus {
				t.Errorf("status = %q, want %q", u.Status, tc.wantStatus)
			}
			if len(u.Content) != 1 || u.Content[0].Type != "content" || u.Content[0].Content.Text != tc.result.Content {
				t.Errorf("content = %+v", u.Content)
			}
		})
	}
}

// TestProjectUpdateDropped asserts the events with no session/update projection
// this phase are dropped (folding/fidelity is a later phase).
func TestProjectUpdateDropped(t *testing.T) {
	dropped := []session.EventType{
		session.EvSessionInit,
		session.EvTurnStart,
		session.EvTurnEnd,
		session.EvCompaction,
		session.EvHook,
		session.EvSubagentStart,
		session.EvSubagentTool,
		session.EvSubagentEnd,
		session.EvTeamStart,
		session.EvTeamMember,
		session.EvTeamEnd,
		session.EvResult,        // handled out of band (stopReason)
		session.EvPermissionAsk, // handled out of band (request_permission)
	}
	for _, et := range dropped {
		if _, ok := projectUpdate(session.Event{Type: et}); ok {
			t.Errorf("event %q should not project to a session/update", et)
		}
	}
}

func TestStopReasonFor(t *testing.T) {
	tests := []struct {
		in   session.StopReason
		want string
	}{
		{session.StopEndTurn, stopEndTurn},
		{session.StopCancelled, stopCancelled},
		{session.StopMaxTurns, stopMaxTurnRequests},
		{session.StopMaxToolCalls, stopMaxTurnRequests},
		{session.StopMaxConsecutiveFailures, stopMaxTurnRequests},
		{session.StopError, stopEndTurn},
	}
	for _, tc := range tests {
		if got := stopReasonFor(tc.in); got != tc.want {
			t.Errorf("stopReasonFor(%q) = %q, want %q", tc.in, got, tc.want)
		}
	}
}

func TestApprovalFor(t *testing.T) {
	tests := []struct {
		outcome permissionOutcome
		want    bool
	}{
		{permissionOutcome{Outcome: outcomeSelected, OptionID: permAllowOnce}, true},
		{permissionOutcome{Outcome: outcomeSelected, OptionID: permAllowAlways}, true},
		{permissionOutcome{Outcome: outcomeSelected, OptionID: permRejectOnce}, false},
		{permissionOutcome{Outcome: outcomeSelected, OptionID: permRejectAlways}, false},
		{permissionOutcome{Outcome: outcomeCancelled}, false},
		{permissionOutcome{Outcome: "weird"}, false},
	}
	for _, tc := range tests {
		if got := approvalFor(tc.outcome); got != tc.want {
			t.Errorf("approvalFor(%+v) = %v, want %v", tc.outcome, got, tc.want)
		}
	}
}

func TestPermissionRequestFor(t *testing.T) {
	ask := session.PendingAsk{AskID: "ask-1", Tool: "Bash", Args: json.RawMessage(`{"cmd":"ls"}`), Reason: "mutating"}
	req := permissionRequestFor("sess-1", ask)
	if req.SessionID != "sess-1" {
		t.Errorf("sessionId = %q", req.SessionID)
	}
	if req.ToolCall.ToolCallID != "ask-1" || req.ToolCall.Title != "Bash" || req.ToolCall.Kind != "execute" {
		t.Errorf("toolCall = %+v", req.ToolCall)
	}
	if len(req.Options) != 4 {
		t.Fatalf("want 4 options, got %d", len(req.Options))
	}
	wantKinds := []string{permAllowOnce, permAllowAlways, permRejectOnce, permRejectAlways}
	for i, o := range req.Options {
		if o.Kind != wantKinds[i] || o.OptionID != wantKinds[i] {
			t.Errorf("option %d = %+v", i, o)
		}
	}
}

// TestChunkUpdateMarshalShape verifies the on-wire JSON of a chunk update matches
// the ACP SessionUpdate shape (discriminator "sessionUpdate", a single nested
// "content" ContentBlock).
func TestChunkUpdateMarshalShape(t *testing.T) {
	b, _ := json.Marshal(chunkUpdate{SessionUpdate: updateAgentMessageChunk, Content: textBlock("hi")})
	var m map[string]any
	_ = json.Unmarshal(b, &m)
	if m["sessionUpdate"] != "agent_message_chunk" {
		t.Errorf("sessionUpdate = %v", m["sessionUpdate"])
	}
	content, ok := m["content"].(map[string]any)
	if !ok || content["type"] != "text" || content["text"] != "hi" {
		t.Errorf("content = %v", m["content"])
	}
}
