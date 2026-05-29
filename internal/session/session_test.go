package session

import (
	"errors"
	"reflect"
	"testing"
	"time"
)

func newTestSession(limits Limits) *Session {
	return New("s1", ModeDefault, "/tmp/ws", limits, time.Unix(0, 0))
}

func TestValidLifecycle_IdleRunningAwaitingRunningCompleted(t *testing.T) {
	s := newTestSession(Limits{})
	if s.State != StateIdle {
		t.Fatalf("new session state = %q, want idle", s.State)
	}
	if err := s.BeginTurn(); err != nil {
		t.Fatalf("BeginTurn: %v", err)
	}
	if s.State != StateRunning {
		t.Fatalf("after BeginTurn state = %q, want running", s.State)
	}
	if err := s.RecordAssistant(NewAssistantMessage("hi", "", nil)); err != nil {
		t.Fatalf("RecordAssistant: %v", err)
	}
	if err := s.PauseForApproval(PendingAsk{AskID: "a1", Tool: "Edit"}); err != nil {
		t.Fatalf("PauseForApproval: %v", err)
	}
	if s.State != StateAwaiting {
		t.Fatalf("after PauseForApproval state = %q, want awaiting", s.State)
	}
	ask, ok := s.PendingAsk()
	if !ok || ask.AskID != "a1" {
		t.Fatalf("PendingAsk = %+v, %v", ask, ok)
	}
	if _, err := s.ResumeWith(); err != nil {
		t.Fatalf("ResumeWith: %v", err)
	}
	if s.State != StateRunning {
		t.Fatalf("after ResumeWith state = %q, want running", s.State)
	}
	if _, ok := s.PendingAsk(); ok {
		t.Fatalf("PendingAsk should be cleared after ResumeWith")
	}
	if err := s.Complete(); err != nil {
		t.Fatalf("Complete: %v", err)
	}
	if s.State != StateCompleted {
		t.Fatalf("after Complete state = %q, want completed", s.State)
	}
	if r, ok := s.StopReason(); !ok || r != StopEndTurn {
		t.Fatalf("StopReason = %q, %v; want end_turn,true", r, ok)
	}
}

func TestIllegalTransitions(t *testing.T) {
	tests := []struct {
		name  string
		setup func(*Session)
		op    func(*Session) error
	}{
		{
			name:  "RecordAssistant from idle",
			setup: func(*Session) {},
			op:    func(s *Session) error { return s.RecordAssistant(NewAssistantMessage("x", "", nil)) },
		},
		{
			name:  "RecordToolResults from idle",
			setup: func(*Session) {},
			op:    func(s *Session) error { return s.RecordToolResults([]ToolResult{NewToolResult("c", "ok")}) },
		},
		{
			name:  "PauseForApproval from idle",
			setup: func(*Session) {},
			op:    func(s *Session) error { return s.PauseForApproval(PendingAsk{}) },
		},
		{
			name:  "BeginTurn from awaiting",
			setup: func(s *Session) { _ = s.BeginTurn(); _ = s.PauseForApproval(PendingAsk{}) },
			op:    func(s *Session) error { return s.BeginTurn() },
		},
		{
			name:  "Complete after completed",
			setup: func(s *Session) { _ = s.BeginTurn(); _ = s.Complete() },
			op:    func(s *Session) error { return s.Complete() },
		},
		{
			name:  "Cancel after completed",
			setup: func(s *Session) { _ = s.BeginTurn(); _ = s.Complete() },
			op:    func(s *Session) error { return s.Cancel() },
		},
		{
			name:  "Fail after cancelled",
			setup: func(s *Session) { _ = s.Cancel() },
			op:    func(s *Session) error { return s.Fail() },
		},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			s := newTestSession(Limits{})
			tc.setup(s)
			err := tc.op(s)
			if !errors.Is(err, ErrIllegalTransition) {
				t.Fatalf("err = %v, want ErrIllegalTransition", err)
			}
		})
	}
}

func TestResumeWithNoPendingAsk(t *testing.T) {
	s := newTestSession(Limits{})
	_ = s.BeginTurn()
	if _, err := s.ResumeWith(); !errors.Is(err, ErrNoPendingAsk) {
		t.Fatalf("err = %v, want ErrNoPendingAsk", err)
	}
}

func TestCancelFromEachNonTerminalState(t *testing.T) {
	states := []struct {
		name  string
		setup func(*Session)
		want  State
	}{
		{"idle", func(*Session) {}, StateIdle},
		{"running", func(s *Session) { _ = s.BeginTurn() }, StateRunning},
		{"awaiting", func(s *Session) { _ = s.BeginTurn(); _ = s.PauseForApproval(PendingAsk{}) }, StateAwaiting},
	}
	for _, st := range states {
		t.Run(st.name, func(t *testing.T) {
			s := newTestSession(Limits{})
			st.setup(s)
			if s.State != st.want {
				t.Fatalf("precondition state = %q, want %q", s.State, st.want)
			}
			if err := s.Cancel(); err != nil {
				t.Fatalf("Cancel: %v", err)
			}
			if s.State != StateCancelled {
				t.Fatalf("after Cancel state = %q, want cancelled", s.State)
			}
			if r, ok := s.StopReason(); !ok || r != StopCancelled {
				t.Fatalf("StopReason = %q,%v; want cancelled,true", r, ok)
			}
		})
	}
}

func TestCancelFromTerminalIsIllegal(t *testing.T) {
	for _, mk := range []struct {
		name  string
		setup func(*Session)
	}{
		{"completed", func(s *Session) { _ = s.BeginTurn(); _ = s.Complete() }},
		{"failed", func(s *Session) { _ = s.Fail() }},
		{"cancelled", func(s *Session) { _ = s.Cancel() }},
	} {
		t.Run(mk.name, func(t *testing.T) {
			s := newTestSession(Limits{})
			mk.setup(s)
			if err := s.Cancel(); !errors.Is(err, ErrIllegalTransition) {
				t.Fatalf("Cancel from %s: err = %v, want ErrIllegalTransition", mk.name, err)
			}
		})
	}
}

func TestStopConditionsTrip(t *testing.T) {
	tests := []struct {
		name   string
		limits Limits
		drive  func(*Session)
		want   StopReason
	}{
		{
			name:   "max turns",
			limits: Limits{MaxTurns: 2},
			drive:  func(s *Session) { _ = s.BeginTurn(); _ = s.BeginTurn() },
			want:   StopMaxTurns,
		},
		{
			name:   "max tool calls",
			limits: Limits{MaxToolCalls: 2},
			drive: func(s *Session) {
				_ = s.BeginTurn()
				_ = s.RecordToolResults([]ToolResult{NewToolResult("a", "ok"), NewToolResult("b", "ok")})
			},
			want: StopMaxToolCalls,
		},
		{
			name:   "max consecutive failures",
			limits: Limits{MaxConsecutiveFailures: 2},
			drive: func(s *Session) {
				_ = s.BeginTurn()
				_ = s.RecordToolResults([]ToolResult{NewToolError("a", "boom"), NewToolError("b", "boom")})
			},
			want: StopMaxConsecutiveFailures,
		},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			s := newTestSession(tc.limits)
			tc.drive(s)
			r, ok := s.StopReason()
			if !ok || r != tc.want {
				t.Fatalf("StopReason = %q,%v; want %q,true", r, ok, tc.want)
			}
		})
	}
}

func TestStopConditionsDoNotTripUnderLimit(t *testing.T) {
	s := newTestSession(Limits{MaxTurns: 5, MaxToolCalls: 5, MaxConsecutiveFailures: 5})
	_ = s.BeginTurn()
	_ = s.RecordToolResults([]ToolResult{NewToolResult("a", "ok")})
	if r, ok := s.StopReason(); ok {
		t.Fatalf("StopReason = %q,%v; want none,false", r, ok)
	}
}

func TestConsecutiveFailuresResetOnSuccess(t *testing.T) {
	s := newTestSession(Limits{MaxConsecutiveFailures: 2})
	_ = s.BeginTurn()
	_ = s.RecordToolResults([]ToolResult{NewToolError("a", "boom")})
	_ = s.RecordToolResults([]ToolResult{NewToolResult("b", "ok")})
	if s.Counters.ConsecutiveFailures != 0 {
		t.Fatalf("ConsecutiveFailures = %d, want 0 after success", s.Counters.ConsecutiveFailures)
	}
	if _, ok := s.StopReason(); ok {
		t.Fatalf("StopReason should not trip after reset")
	}
}

func TestRecordedStopReasonDoesNotDerive(t *testing.T) {
	// A session whose limit would trip must NOT report a recorded reason until one
	// is explicitly set; RecordedStopReason performs no derivation.
	s := newTestSession(Limits{MaxTurns: 1})
	_ = s.BeginTurn() // trips MaxTurns under StopReason's derivation

	if r, ok := s.RecordedStopReason(); ok {
		t.Fatalf("RecordedStopReason = %q,%v; want none,false (no derivation)", r, ok)
	}
	// StopReason (derived) still sees the tripped limit.
	if r, ok := s.StopReason(); !ok || r != StopMaxTurns {
		t.Fatalf("StopReason = %q,%v; want max_turns,true", r, ok)
	}

	if err := s.Stop(StopMaxToolCalls); err != nil {
		t.Fatalf("Stop: %v", err)
	}
	if r, ok := s.RecordedStopReason(); !ok || r != StopMaxToolCalls {
		t.Fatalf("RecordedStopReason = %q,%v; want max_tool_calls,true", r, ok)
	}
}

func TestLimitTrippedIgnoresRecordedReason(t *testing.T) {
	s := newTestSession(Limits{MaxTurns: 1})
	if r, ok := s.LimitTripped(); ok {
		t.Fatalf("LimitTripped before turn = %q,%v; want none,false", r, ok)
	}
	_ = s.BeginTurn()
	if r, ok := s.LimitTripped(); !ok || r != StopMaxTurns {
		t.Fatalf("LimitTripped = %q,%v; want max_turns,true", r, ok)
	}
	// Recording a terminal reason does not change the pure limit predicate.
	_ = s.Cancel()
	if r, ok := s.LimitTripped(); !ok || r != StopMaxTurns {
		t.Fatalf("LimitTripped after Cancel = %q,%v; want max_turns,true", r, ok)
	}
}

func TestRecordUserPromptAppendsInstructionsThenPrompt(t *testing.T) {
	s := newTestSession(Limits{})
	instr := []Message{NewUserMessage("AGENTS.md"), NewUserMessage("CLAUDE.md")}
	if err := s.RecordUserPrompt("hello", instr); err != nil {
		t.Fatalf("RecordUserPrompt: %v", err)
	}
	msgs := s.Conversation.Messages
	if len(msgs) != 3 {
		t.Fatalf("len(messages) = %d, want 3", len(msgs))
	}
	if msgs[0].Text != "AGENTS.md" || msgs[1].Text != "CLAUDE.md" || msgs[2].Text != "hello" {
		t.Fatalf("messages = %+v; want instructions then prompt", msgs)
	}
	if msgs[2].Role != RoleUser {
		t.Fatalf("prompt role = %q, want user", msgs[2].Role)
	}
}

func TestRecordUserPromptFromTerminalIsIllegal(t *testing.T) {
	s := newTestSession(Limits{})
	_ = s.BeginTurn()
	_ = s.Complete()
	if err := s.RecordUserPrompt("x", nil); !errors.Is(err, ErrIllegalTransition) {
		t.Fatalf("err = %v, want ErrIllegalTransition", err)
	}
}

func TestReplaceHistoryAtomicWhileRunning(t *testing.T) {
	s := newTestSession(Limits{})
	_ = s.BeginTurn()
	_ = s.RecordAssistant(NewAssistantMessage("old", "", nil))
	compacted := []Message{NewSystemMessage("summary"), NewUserMessage("continue")}
	if err := s.ReplaceHistory(compacted); err != nil {
		t.Fatalf("ReplaceHistory: %v", err)
	}
	if !reflect.DeepEqual(s.Conversation.Messages, compacted) {
		t.Fatalf("messages = %+v, want %+v", s.Conversation.Messages, compacted)
	}
}

func TestReplaceHistoryOnlyWhileRunning(t *testing.T) {
	s := newTestSession(Limits{}) // idle
	if err := s.ReplaceHistory(nil); !errors.Is(err, ErrIllegalTransition) {
		t.Fatalf("err = %v, want ErrIllegalTransition", err)
	}
}

func TestExplicitStopReasonTakesPrecedence(t *testing.T) {
	s := newTestSession(Limits{MaxTurns: 1})
	_ = s.BeginTurn() // would trip MaxTurns
	if err := s.Stop(StopMaxToolCalls); err != nil {
		t.Fatalf("Stop: %v", err)
	}
	if r, _ := s.StopReason(); r != StopMaxToolCalls {
		t.Fatalf("StopReason = %q, want explicit max_tool_calls", r)
	}
}
