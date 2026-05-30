package server

import (
	"encoding/json"
	"testing"
	"time"

	mecatlv1 "github.com/stacklok/mecatl/contracts/gen/go/mecatl/v1"
	"github.com/stacklok/mecatl/internal/session"
)

// TestToProtoTable round-trips every EventType and each structured submessage
// through toProto, asserting the proto shape matches the domain Event.
func TestToProtoTable(t *testing.T) {
	cases := []struct {
		name   string
		in     session.Event
		assert func(t *testing.T, got *mecatlv1.Event)
	}{
		{
			name: "session.init",
			in:   session.Event{Type: session.EvSessionInit, Seq: 1, Turn: 0},
			assert: func(t *testing.T, got *mecatlv1.Event) {
				if got.GetType() != "session.init" || got.GetSeq() != 1 {
					t.Fatalf("got %+v", got)
				}
			},
		},
		{
			name: "turn.start",
			in:   session.Event{Type: session.EvTurnStart, Seq: 2, Turn: 3},
			assert: func(t *testing.T, got *mecatlv1.Event) {
				if got.GetType() != "turn.start" || got.GetTurn() != 3 {
					t.Fatalf("got %+v", got)
				}
			},
		},
		{
			name: "message.delta",
			in:   session.Event{Type: session.EvMessageDelta, Seq: 3, Turn: 1, Text: "hello"},
			assert: func(t *testing.T, got *mecatlv1.Event) {
				if got.GetType() != "message.delta" || got.GetText() != "hello" {
					t.Fatalf("got %+v", got)
				}
			},
		},
		{
			name: "reasoning.delta",
			in:   session.Event{Type: session.EvReasoningDelta, Seq: 11, Turn: 1, Text: "thinking…"},
			assert: func(t *testing.T, got *mecatlv1.Event) {
				if got.GetType() != "reasoning.delta" || got.GetText() != "thinking…" {
					t.Fatalf("got %+v", got)
				}
				if got.GetTurnEnd() != nil {
					t.Fatalf("reasoning.delta should not carry a turn_end payload: %+v", got)
				}
			},
		},
		{
			name: "turn.end",
			in: session.Event{Type: session.EvTurnEnd, Seq: 12, Turn: 2,
				TurnEnd: &session.TurnEndPayload{DurationMs: 4100,
					Usage: session.Usage{InputTokens: 1200, OutputTokens: 340, CacheReadTokens: 800, CacheWriteTokens: 100}}},
			assert: func(t *testing.T, got *mecatlv1.Event) {
				if got.GetType() != "turn.end" || got.GetTurn() != 2 {
					t.Fatalf("got %+v", got)
				}
				// turn.end carries its per-turn data in the typed turn_end submessage,
				// NOT in the shared Event.usage field (which is cumulative-on-result).
				if got.GetUsage() != nil {
					t.Fatalf("turn.end must not set the shared Event.usage field: %+v", got)
				}
				te := got.GetTurnEnd()
				if te == nil {
					t.Fatalf("turn.end missing turn_end payload: %+v", got)
				}
				if te.GetDurationMs() != 4100 {
					t.Fatalf("duration_ms = %d, want 4100", te.GetDurationMs())
				}
				u := te.GetUsage()
				if u.GetInputTokens() != 1200 || u.GetOutputTokens() != 340 ||
					u.GetCacheReadTokens() != 800 || u.GetCacheWriteTokens() != 100 {
					t.Fatalf("per-turn usage mismatch: %+v", u)
				}
			},
		},
		{
			name: "tool.call",
			in: session.Event{Type: session.EvToolCall, Seq: 4, Turn: 1,
				ToolCall: &session.ToolCall{ID: "c1", Name: "Read", Args: json.RawMessage(`{"path":"a.go"}`)}},
			assert: func(t *testing.T, got *mecatlv1.Event) {
				tc := got.GetToolCall()
				if tc == nil || tc.GetId() != "c1" || tc.GetName() != "Read" || tc.GetArgs() != `{"path":"a.go"}` {
					t.Fatalf("got %+v", got)
				}
			},
		},
		{
			name: "tool.result",
			in: session.Event{Type: session.EvToolResult, Seq: 5, Turn: 1,
				ToolResult: &session.ToolResult{CallID: "c1", Content: "body", IsError: true}},
			assert: func(t *testing.T, got *mecatlv1.Event) {
				tr := got.GetToolResult()
				if tr == nil || tr.GetCallId() != "c1" || tr.GetContent() != "body" || !tr.GetIsError() {
					t.Fatalf("got %+v", got)
				}
			},
		},
		{
			name: "permission.ask",
			in: session.Event{Type: session.EvPermissionAsk, Seq: 6, Turn: 1,
				Ask: &session.PendingAsk{AskID: "a1", Tool: "Write", Args: json.RawMessage(`{"path":"x"}`), Reason: "needs approval"}},
			assert: func(t *testing.T, got *mecatlv1.Event) {
				a := got.GetAsk()
				if a == nil || a.GetAskId() != "a1" || a.GetTool() != "Write" ||
					a.GetArgs() != `{"path":"x"}` || a.GetReason() != "needs approval" {
					t.Fatalf("got %+v", got)
				}
			},
		},
		{
			name: "hook",
			in: session.Event{Type: session.EvHook, Seq: 7, Turn: 1, Text: "blocked-by-policy",
				Hook: &session.HookPayload{Phase: "PreToolUse", Tool: "Bash", Decision: session.HookBlocked}},
			assert: func(t *testing.T, got *mecatlv1.Event) {
				if got.GetType() != "hook" || got.GetText() != "blocked-by-policy" {
					t.Fatalf("got %+v", got)
				}
				h := got.GetHook()
				if h == nil || h.GetPhase() != "PreToolUse" || h.GetTool() != "Bash" ||
					h.GetDecision() != mecatlv1.HookDecision_HOOK_DECISION_BLOCKED {
					t.Fatalf("hook payload mismatch: %+v", h)
				}
			},
		},
		{
			name: "hook info default",
			in:   session.Event{Type: session.EvHook, Seq: 7, Turn: 1, Text: "ran", Hook: &session.HookPayload{Phase: "Stop"}},
			assert: func(t *testing.T, got *mecatlv1.Event) {
				h := got.GetHook()
				if h == nil || h.GetDecision() != mecatlv1.HookDecision_HOOK_DECISION_INFO {
					t.Fatalf("empty decision should map to INFO: %+v", h)
				}
			},
		},
		{
			name: "subagent.start",
			in: session.Event{Type: session.EvSubagentStart, Seq: 20, Turn: 1,
				Subagent: &session.SubagentPayload{ParentCallID: "p1", ChildID: "subagent-p1", Goal: "investigate main.go"}},
			assert: func(t *testing.T, got *mecatlv1.Event) {
				s := got.GetSubagent()
				if s == nil || s.GetParentCallId() != "p1" || s.GetChildId() != "subagent-p1" || s.GetGoal() != "investigate main.go" {
					t.Fatalf("subagent.start payload mismatch: %+v", s)
				}
			},
		},
		{
			name: "subagent.tool",
			in: session.Event{Type: session.EvSubagentTool, Seq: 21, Turn: 1,
				Subagent: &session.SubagentPayload{ParentCallID: "p1", ChildID: "subagent-p1", ToolName: "Grep", IsError: true, ToolCount: 3}},
			assert: func(t *testing.T, got *mecatlv1.Event) {
				s := got.GetSubagent()
				if s == nil || s.GetToolName() != "Grep" || !s.GetIsError() || s.GetToolCount() != 3 {
					t.Fatalf("subagent.tool payload mismatch: %+v", s)
				}
			},
		},
		{
			name: "subagent.end",
			in: session.Event{Type: session.EvSubagentEnd, Seq: 22, Turn: 1,
				Subagent: &session.SubagentPayload{ParentCallID: "p1", ChildID: "subagent-p1", ToolCount: 5,
					Usage: session.Usage{InputTokens: 90, OutputTokens: 12, CacheReadTokens: 40, CacheWriteTokens: 8},
					Stop:  session.StopMaxToolCalls, DurationMs: 1234}},
			assert: func(t *testing.T, got *mecatlv1.Event) {
				s := got.GetSubagent()
				if s == nil || s.GetToolCount() != 5 || s.GetStop() != "max_tool_calls" || s.GetDurationMs() != 1234 {
					t.Fatalf("subagent.end payload mismatch: %+v", s)
				}
				u := s.GetUsage()
				if u.GetInputTokens() != 90 || u.GetOutputTokens() != 12 ||
					u.GetCacheReadTokens() != 40 || u.GetCacheWriteTokens() != 8 {
					t.Fatalf("subagent.end usage mismatch: %+v", u)
				}
			},
		},
		{
			name: "compaction",
			in:   session.Event{Type: session.EvCompaction, Seq: 8, Turn: 2, Text: "summary"},
			assert: func(t *testing.T, got *mecatlv1.Event) {
				if got.GetType() != "compaction" || got.GetText() != "summary" {
					t.Fatalf("got %+v", got)
				}
			},
		},
		{
			name: "result",
			in: session.Event{Type: session.EvResult, Seq: 9, Turn: 2,
				Result: &session.ResultPayload{Stop: session.StopEndTurn, Text: "all done",
					Usage: session.Usage{InputTokens: 15, OutputTokens: 5, CacheReadTokens: 3, CacheWriteTokens: 1}},
				Usage: &session.Usage{InputTokens: 15, OutputTokens: 5}},
			assert: func(t *testing.T, got *mecatlv1.Event) {
				res := got.GetResult()
				if res == nil || res.GetStop() != "end_turn" || res.GetText() != "all done" {
					t.Fatalf("result mismatch: %+v", got)
				}
				u := res.GetUsage()
				if u.GetInputTokens() != 15 || u.GetOutputTokens() != 5 ||
					u.GetCacheReadTokens() != 3 || u.GetCacheWriteTokens() != 1 {
					t.Fatalf("usage mismatch: %+v", u)
				}
				if got.GetUsage().GetInputTokens() != 15 {
					t.Fatalf("event usage mismatch: %+v", got.GetUsage())
				}
			},
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got := toProto(tc.in)
			if got.GetType() != string(tc.in.Type) {
				t.Fatalf("type = %q, want %q", got.GetType(), tc.in.Type)
			}
			if got.GetSeq() != tc.in.Seq {
				t.Fatalf("seq = %d, want %d", got.GetSeq(), tc.in.Seq)
			}
			tc.assert(t, got)
		})
	}
}

// TestToProtoNoSubmessages confirms a bare event leaves all submessages nil.
func TestToProtoNoSubmessages(t *testing.T) {
	got := toProto(session.Event{Type: session.EvTurnStart})
	if got.GetToolCall() != nil || got.GetToolResult() != nil || got.GetAsk() != nil ||
		got.GetResult() != nil || got.GetTurnEnd() != nil || got.GetUsage() != nil {
		t.Fatalf("unexpected submessage on bare event: %+v", got)
	}
}

// TestSessionMapping checks the session snapshot mapping including mode and
// limits round-trips.
func TestSessionMapping(t *testing.T) {
	sess := session.New("s1", session.ModePlan, "/ws",
		session.Limits{MaxTurns: 4, MaxToolCalls: 8, MaxConsecutiveFailures: 2}, time.Unix(1000, 0))
	got := toProtoSession(sess)
	if got.GetSessionId() != "s1" || got.GetState() != "idle" {
		t.Fatalf("got %+v", got)
	}
	if got.GetMode() != mecatlv1.PermissionMode_PERMISSION_MODE_PLAN {
		t.Fatalf("mode = %v", got.GetMode())
	}
	if got.GetLimits().GetMaxTurns() != 4 || got.GetLimits().GetMaxToolCalls() != 8 {
		t.Fatalf("limits = %+v", got.GetLimits())
	}
	if got.GetCreatedAtUnix() != 1000 {
		t.Fatalf("created_at = %d", got.GetCreatedAtUnix())
	}
}

// TestModeRoundTrip checks mode mapping in both directions.
func TestModeRoundTrip(t *testing.T) {
	for _, m := range []session.PermissionMode{session.ModeDefault, session.ModePlan, session.ModeAccept} {
		if got := modeFromProto(modeToProto(m)); got != m {
			t.Fatalf("mode round-trip: %q -> %q", m, got)
		}
	}
	if got := modeFromProto(mecatlv1.PermissionMode_PERMISSION_MODE_UNSPECIFIED); got != session.ModeDefault {
		t.Fatalf("unspecified mode -> %q, want default", got)
	}
}
