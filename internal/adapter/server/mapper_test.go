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
			in:   session.Event{Type: session.EvHook, Seq: 7, Turn: 1, Text: "blocked-by-policy"},
			assert: func(t *testing.T, got *mecatlv1.Event) {
				if got.GetType() != "hook" || got.GetText() != "blocked-by-policy" {
					t.Fatalf("got %+v", got)
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
		got.GetResult() != nil || got.GetUsage() != nil {
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
