package client

import (
	"context"
	"errors"
	"testing"
	"time"

	tea "charm.land/bubbletea/v2"

	mecatlv1 "github.com/stacklok/mecatl/contracts/gen/go/mecatl/v1"
)

// TestEventToMsg covers the mapper over every documented event type, asserting
// both the msg variant and a representative carried field. This is the single
// translation point between proto and the ui model, so it gets exhaustive
// coverage.
func TestEventToMsg(t *testing.T) {
	cases := []struct {
		name string
		ev   *mecatlv1.Event
		want tea.Msg
	}{
		{"nil", nil, nil},
		{"unknown", &mecatlv1.Event{Type: "future.kind"}, nil},
		{"session.init", &mecatlv1.Event{Type: "session.init", Seq: 7}, SessionInitMsg{Seq: 7}},
		{"turn.start", &mecatlv1.Event{Type: "turn.start", Turn: 2}, TurnStartMsg{Turn: 2}},
		{
			"turn.end",
			&mecatlv1.Event{Type: "turn.end", Turn: 2, TurnEnd: &mecatlv1.TurnEnd{
				DurationMs: 4100, Usage: &mecatlv1.Usage{InputTokens: 1200, OutputTokens: 340}}},
			TurnEndMsg{Turn: 2, Usage: Usage{InputTokens: 1200, OutputTokens: 340}, DurationMs: 4100},
		},
		{"message.delta", &mecatlv1.Event{Type: "message.delta", Turn: 2, Text: "hi"}, AssistantDeltaMsg{Turn: 2, Text: "hi"}},
		{"reasoning.delta", &mecatlv1.Event{Type: "reasoning.delta", Turn: 2, Text: "pondering"}, ReasoningDeltaMsg{Turn: 2, Text: "pondering"}},
		{
			"tool.call",
			&mecatlv1.Event{Type: "tool.call", ToolCall: &mecatlv1.ToolCall{Id: "c1", Name: "Read", Args: `{"path":"x"}`}},
			ToolCallMsg{ID: "c1", Name: "Read", Args: `{"path":"x"}`},
		},
		{
			"tool.result",
			&mecatlv1.Event{Type: "tool.result", ToolResult: &mecatlv1.ToolResult{CallId: "c1", Content: "ok", IsError: true}},
			ToolResultMsg{CallID: "c1", Content: "ok", IsError: true},
		},
		{
			"permission.ask",
			&mecatlv1.Event{Type: "permission.ask", Ask: &mecatlv1.PermissionAsk{AskId: "a1", Tool: "Write", Args: "{}", Reason: "why"}},
			PermissionAskMsg{AskID: "a1", Tool: "Write", Args: "{}", Reason: "why"},
		},
		{"hook", &mecatlv1.Event{Type: "hook", Text: "ran hook"}, HookMsg{Text: "ran hook"}},
		{"compaction", &mecatlv1.Event{Type: "compaction", Text: "compacted"}, CompactionMsg{Text: "compacted"}},
		{
			"result",
			&mecatlv1.Event{Type: "result", Result: &mecatlv1.Result{
				Stop: "end_turn", Text: "done", Usage: &mecatlv1.Usage{InputTokens: 10, OutputTokens: 5},
			}},
			ResultMsg{Stop: "end_turn", Text: "done", Usage: Usage{InputTokens: 10, OutputTokens: 5}},
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got := EventToMsg(tc.ev)
			if got != tc.want {
				t.Errorf("EventToMsg(%s) = %#v, want %#v", tc.name, got, tc.want)
			}
		})
	}
}

// drain collects all msgs from a closed channel.
func drain(ch <-chan tea.Msg) []tea.Msg {
	var out []tea.Msg
	for m := range ch {
		out = append(out, m)
	}
	return out
}

// TestReadLoopEOF asserts ReadLoop translates the script in order and ends with
// StreamClosedMsg on a clean EOF, then closes the channel.
func TestReadLoopEOF(t *testing.T) {
	fs := newFakeStream(scriptedRunResult()...)
	st := NewStream(fs, fs)
	ch := make(chan tea.Msg, 64)
	go st.ReadLoop(context.Background(), ch)

	msgs := drain(ch)
	if len(msgs) == 0 {
		t.Fatal("no msgs")
	}
	if _, ok := msgs[0].(SessionInitMsg); !ok {
		t.Errorf("first msg = %T, want SessionInitMsg", msgs[0])
	}
	last := msgs[len(msgs)-1]
	if _, ok := last.(StreamClosedMsg); !ok {
		t.Errorf("last msg = %T, want StreamClosedMsg", last)
	}
	// The terminal result must appear just before the close.
	res, ok := msgs[len(msgs)-2].(ResultMsg)
	if !ok || res.Stop != "end_turn" {
		t.Errorf("penultimate msg = %#v, want ResultMsg{end_turn}", msgs[len(msgs)-2])
	}
}

// TestReadLoopError asserts a non-EOF Recv error becomes StreamErrMsg.
func TestReadLoopError(t *testing.T) {
	boom := errors.New("boom")
	fs := newFakeStream(resp(&mecatlv1.Event{Type: "session.init"}))
	fs.endErr = boom
	st := NewStream(fs, fs)
	ch := make(chan tea.Msg, 8)
	go st.ReadLoop(context.Background(), ch)

	msgs := drain(ch)
	last := msgs[len(msgs)-1]
	se, ok := last.(StreamErrMsg)
	if !ok {
		t.Fatalf("last msg = %T, want StreamErrMsg", last)
	}
	if !errors.Is(se.Err, boom) {
		t.Errorf("err = %v, want boom", se.Err)
	}
}

// TestReadLoopCancelUnblocks asserts the reader goroutine exits when its context
// is cancelled even though nobody is draining the (unbuffered) channel — the
// no-leak property is structural via the ctx select, not just reasoned.
func TestReadLoopCancelUnblocks(t *testing.T) {
	// A script with one event and an unbuffered, never-drained channel: the
	// reader will block trying to emit until the context is cancelled.
	fs := newFakeStream(resp(&mecatlv1.Event{Type: "message.delta", Text: "hi"}))
	st := NewStream(fs, fs)
	ch := make(chan tea.Msg) // unbuffered, never drained

	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan struct{})
	go func() {
		st.ReadLoop(ctx, ch)
		close(done)
	}()

	cancel()
	select {
	case <-done:
		// reader exited (and closed ch via defer) — good
	case <-time.After(2 * time.Second):
		t.Fatal("ReadLoop did not exit after context cancel (goroutine leak)")
	}
}

// TestAskRoundTrip asserts SendApproval emits a ResumeApproval frame carrying the
// EXACT ask_id (the only correlation) and the allow bool. This is the load-
// bearing round-trip the permission flow depends on.
func TestAskRoundTrip(t *testing.T) {
	fs := newFakeStream()
	st := NewStream(fs, fs)
	if err := st.SendApproval("ask-write-1", true); err != nil {
		t.Fatalf("SendApproval: %v", err)
	}
	frames := fs.sentFrames()
	if len(frames) != 1 {
		t.Fatalf("sent %d frames, want 1", len(frames))
	}
	ra := frames[0].GetResumeApproval()
	if ra == nil {
		t.Fatalf("frame is not a ResumeApproval: %#v", frames[0])
	}
	if ra.GetAskId() != "ask-write-1" {
		t.Errorf("ask_id = %q, want ask-write-1", ra.GetAskId())
	}
	if !ra.GetAllow() {
		t.Error("allow = false, want true")
	}
}

// TestPromptAndCancelFrames asserts the prompt-first contract and the cancel
// frame shape.
func TestPromptAndCancelFrames(t *testing.T) {
	fs := newFakeStream()
	st := NewStream(fs, fs)
	if err := st.SendPrompt("sess-1", "hello"); err != nil {
		t.Fatalf("SendPrompt: %v", err)
	}
	if err := st.SendCancel(); err != nil {
		t.Fatalf("SendCancel: %v", err)
	}
	frames := fs.sentFrames()
	if len(frames) != 2 {
		t.Fatalf("sent %d frames, want 2", len(frames))
	}
	p := frames[0].GetPrompt()
	if p == nil || p.GetSessionId() != "sess-1" || p.GetText() != "hello" {
		t.Errorf("first frame = %#v, want Prompt{sess-1,hello}", frames[0])
	}
	if frames[1].GetCancel() == nil {
		t.Errorf("second frame = %#v, want Cancel", frames[1])
	}
}
