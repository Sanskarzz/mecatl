package session

import (
	"errors"
	"reflect"
	"strings"
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

func TestSetMode(t *testing.T) {
	// Idle: a mode change is legal and takes effect.
	s := newTestSession(Limits{})
	if err := s.SetMode(ModePlan); err != nil {
		t.Fatalf("SetMode idle: %v", err)
	}
	if s.Mode != ModePlan {
		t.Fatalf("mode = %q, want plan", s.Mode)
	}

	// Running: a mid-turn change is rejected.
	if err := s.BeginTurn(); err != nil {
		t.Fatalf("BeginTurn: %v", err)
	}
	if err := s.SetMode(ModeAccept); !errors.Is(err, ErrIllegalTransition) {
		t.Fatalf("SetMode running = %v, want ErrIllegalTransition", err)
	}
	if s.Mode != ModePlan {
		t.Fatalf("mode changed mid-run to %q", s.Mode)
	}

	// Awaiting: also rejected.
	if err := s.RecordAssistant(NewAssistantMessage("hi", "", nil)); err != nil {
		t.Fatalf("RecordAssistant: %v", err)
	}
	if err := s.PauseForApproval(PendingAsk{AskID: "a1", Tool: "Edit"}); err != nil {
		t.Fatalf("PauseForApproval: %v", err)
	}
	if err := s.SetMode(ModeDefault); !errors.Is(err, ErrIllegalTransition) {
		t.Fatalf("SetMode awaiting = %v, want ErrIllegalTransition", err)
	}

	// Terminal (completed): legal again (applies to the next reopened run).
	if _, err := s.ResumeWith(); err != nil {
		t.Fatalf("ResumeWith: %v", err)
	}
	if err := s.Complete(); err != nil {
		t.Fatalf("Complete: %v", err)
	}
	if err := s.SetMode(ModeDefault); err != nil {
		t.Fatalf("SetMode completed: %v", err)
	}
	if s.Mode != ModeDefault {
		t.Fatalf("mode = %q, want default", s.Mode)
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

// TestReplaceHistoryRejectsUnpairedHistory pins the aggregate-level guard: while
// running, ReplaceHistory refuses a tool-pairing-invalid slice (a leading orphan
// tool result) — wrapping the pairing error — and leaves the conversation
// untouched, but accepts a clean paired slice and actually applies the swap.
func TestReplaceHistoryRejectsUnpairedHistory(t *testing.T) {
	s := newTestSession(Limits{})
	_ = s.BeginTurn()
	_ = s.RecordAssistant(NewAssistantMessage("old", "", nil))
	before := append([]Message(nil), s.Conversation.Messages...)

	// (a) Leading-orphan slice: a tool result whose call never preceded it.
	orphan := []Message{
		NewUserMessage("goal"),
		NewToolMessage(NewToolResult("c1", "result")),
	}
	err := s.ReplaceHistory(orphan)
	if err == nil {
		t.Fatalf("ReplaceHistory accepted an unpaired (orphan) history")
	}
	if got := ValidateToolPairing(orphan); got == nil || !strings.Contains(err.Error(), "orphaned tool result") {
		t.Fatalf("error %v does not wrap the pairing message", err)
	}
	if !reflect.DeepEqual(s.Conversation.Messages, before) {
		t.Fatalf("conversation mutated despite rejected replacement: %+v", s.Conversation.Messages)
	}

	// (b) Clean paired slice: accepted, and the swap is applied.
	clean := []Message{
		NewSystemMessage("summary"),
		NewUserMessage("continue"),
		NewAssistantMessage("", "", []ToolCall{NewToolCall("c1", "Read", nil)}),
		NewToolMessage(NewToolResult("c1", "ok")),
	}
	if err := s.ReplaceHistory(clean); err != nil {
		t.Fatalf("ReplaceHistory rejected a clean paired history: %v", err)
	}
	if !reflect.DeepEqual(s.Conversation.Messages, clean) {
		t.Fatalf("clean replacement not applied: %+v", s.Conversation.Messages)
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

// TestStopBudgetIsCleanReopenableTerminal pins that StopBudget is a CLEAN terminal,
// parallel to StopNoProgress: Stop(StopBudget) drives the session to COMPLETED (not
// failed) and the completed session is Reopen-recoverable.
func TestStopBudgetIsCleanReopenableTerminal(t *testing.T) {
	s := newTestSession(Limits{})
	if err := s.BeginTurn(); err != nil {
		t.Fatalf("BeginTurn: %v", err)
	}
	if err := s.Stop(StopBudget); err != nil {
		t.Fatalf("Stop(StopBudget): %v", err)
	}
	if s.State != StateCompleted {
		t.Fatalf("state after Stop(StopBudget) = %q, want completed (clean terminal)", s.State)
	}
	if r, ok := s.RecordedStopReason(); !ok || r != StopBudget {
		t.Fatalf("recorded stop = %q (ok=%v), want %q", r, ok, StopBudget)
	}
	if err := s.Reopen(); err != nil {
		t.Fatalf("Reopen after StopBudget: %v (a budget terminal must stay recoverable)", err)
	}
	if s.State != StateIdle {
		t.Fatalf("state after Reopen = %q, want idle", s.State)
	}
}

func TestReopenFromCompletedReturnsToIdleAndResetsCounters(t *testing.T) {
	s := newTestSession(Limits{MaxTurns: 5})
	// Drive one turn and complete cleanly.
	if err := s.BeginTurn(); err != nil {
		t.Fatalf("BeginTurn: %v", err)
	}
	if err := s.RecordAssistant(NewAssistantMessage("first answer", "", nil)); err != nil {
		t.Fatalf("RecordAssistant: %v", err)
	}
	if err := s.Complete(); err != nil {
		t.Fatalf("Complete: %v", err)
	}
	if s.State != StateCompleted || s.Counters.Turns != 1 {
		t.Fatalf("precondition: state=%q turns=%d, want completed/1", s.State, s.Counters.Turns)
	}
	convLen := len(s.Conversation.Messages)

	if err := s.Reopen(); err != nil {
		t.Fatalf("Reopen: %v", err)
	}
	if s.State != StateIdle {
		t.Fatalf("after Reopen state = %q, want idle", s.State)
	}
	if s.Counters != (Counters{}) {
		t.Fatalf("after Reopen counters = %+v, want zero (per-prompt budget)", s.Counters)
	}
	if r, ok := s.RecordedStopReason(); ok {
		t.Fatalf("after Reopen recorded stop reason = %q, want none", r)
	}
	if len(s.Conversation.Messages) != convLen {
		t.Fatalf("Reopen dropped conversation history: len=%d, want %d", len(s.Conversation.Messages), convLen)
	}
	// The reopened session accepts a new prompt and another turn (continuation).
	if err := s.RecordUserPrompt("second prompt", nil); err != nil {
		t.Fatalf("RecordUserPrompt after Reopen: %v", err)
	}
	if err := s.BeginTurn(); err != nil {
		t.Fatalf("BeginTurn after Reopen: %v", err)
	}
	if s.Counters.Turns != 1 {
		t.Fatalf("turns after reopened BeginTurn = %d, want 1 (counter was reset)", s.Counters.Turns)
	}
}

func TestReopenIllegalFromNonCompletedStates(t *testing.T) {
	for _, mk := range []struct {
		name  string
		setup func(*Session)
	}{
		{"idle", func(*Session) {}},
		{"running", func(s *Session) { _ = s.BeginTurn() }},
		{"awaiting", func(s *Session) { _ = s.BeginTurn(); _ = s.PauseForApproval(PendingAsk{}) }},
		{"failed", func(s *Session) { _ = s.Fail() }},
		{"cancelled", func(s *Session) { _ = s.Cancel() }},
	} {
		t.Run(mk.name, func(t *testing.T) {
			s := newTestSession(Limits{})
			mk.setup(s)
			if err := s.Reopen(); !errors.Is(err, ErrIllegalTransition) {
				t.Fatalf("Reopen from %s: err = %v, want ErrIllegalTransition", mk.name, err)
			}
		})
	}
}

func TestInterruptFromCancelledReturnsToIdleAndResetsCounters(t *testing.T) {
	s := newTestSession(Limits{MaxTurns: 5})
	if err := s.RecordUserPrompt("first prompt", nil); err != nil {
		t.Fatalf("RecordUserPrompt: %v", err)
	}
	if err := s.BeginTurn(); err != nil {
		t.Fatalf("BeginTurn: %v", err)
	}
	if err := s.RecordAssistant(NewAssistantMessage("partial answer", "", nil)); err != nil {
		t.Fatalf("RecordAssistant: %v", err)
	}
	if err := s.Cancel(); err != nil {
		t.Fatalf("Cancel: %v", err)
	}
	if s.State != StateCancelled || s.Counters.Turns != 1 {
		t.Fatalf("precondition: state=%q turns=%d, want cancelled/1", s.State, s.Counters.Turns)
	}
	convLen := len(s.Conversation.Messages)

	if err := s.Interrupt(); err != nil {
		t.Fatalf("Interrupt: %v", err)
	}
	if s.State != StateIdle {
		t.Fatalf("after Interrupt state = %q, want idle", s.State)
	}
	if s.Counters != (Counters{}) {
		t.Fatalf("after Interrupt counters = %+v, want zero (per-prompt budget)", s.Counters)
	}
	if r, ok := s.RecordedStopReason(); ok {
		t.Fatalf("after Interrupt recorded stop reason = %q, want none", r)
	}
	// No orphaned tool calls here, so history is preserved unchanged.
	if len(s.Conversation.Messages) != convLen {
		t.Fatalf("Interrupt changed clean history: len=%d, want %d", len(s.Conversation.Messages), convLen)
	}
	// The interrupted session accepts a new prompt and another turn.
	if err := s.RecordUserPrompt("second prompt", nil); err != nil {
		t.Fatalf("RecordUserPrompt after Interrupt: %v", err)
	}
	if err := s.BeginTurn(); err != nil {
		t.Fatalf("BeginTurn after Interrupt: %v", err)
	}
	if s.Counters.Turns != 1 {
		t.Fatalf("turns after interrupted BeginTurn = %d, want 1 (counter was reset)", s.Counters.Turns)
	}
}

func TestInterruptIllegalFromNonCancelledStates(t *testing.T) {
	for _, mk := range []struct {
		name  string
		setup func(*Session)
	}{
		{"idle", func(*Session) {}},
		{"running", func(s *Session) { _ = s.BeginTurn() }},
		{"awaiting", func(s *Session) { _ = s.BeginTurn(); _ = s.PauseForApproval(PendingAsk{}) }},
		{"completed", func(s *Session) { _ = s.BeginTurn(); _ = s.Complete() }},
		{"failed", func(s *Session) { _ = s.Fail() }},
	} {
		t.Run(mk.name, func(t *testing.T) {
			s := newTestSession(Limits{})
			mk.setup(s)
			if err := s.Interrupt(); !errors.Is(err, ErrIllegalTransition) {
				t.Fatalf("Interrupt from %s: err = %v, want ErrIllegalTransition", mk.name, err)
			}
		})
	}
}

func TestInterruptClosesOutOrphanedToolCalls(t *testing.T) {
	s := newTestSession(Limits{})
	if err := s.RecordUserPrompt("do two things", nil); err != nil {
		t.Fatalf("RecordUserPrompt: %v", err)
	}
	if err := s.BeginTurn(); err != nil {
		t.Fatalf("BeginTurn: %v", err)
	}
	calls := []ToolCall{
		NewToolCall("call-a", "read_file", nil),
		NewToolCall("call-b", "list_dir", nil),
	}
	if err := s.RecordAssistant(NewAssistantMessage("", "", calls)); err != nil {
		t.Fatalf("RecordAssistant: %v", err)
	}
	if err := s.Cancel(); err != nil {
		t.Fatalf("Cancel: %v", err)
	}
	before := len(s.Conversation.Messages)

	if err := s.Interrupt(); err != nil {
		t.Fatalf("Interrupt: %v", err)
	}
	added := s.Conversation.Messages[before:]
	if len(added) != 2 {
		t.Fatalf("appended %d tool results, want 2", len(added))
	}
	wantIDs := []ToolCallID{"call-a", "call-b"}
	for i, m := range added {
		if m.Role != RoleTool {
			t.Fatalf("appended[%d] role = %q, want tool", i, m.Role)
		}
		if m.ToolResult == nil {
			t.Fatalf("appended[%d] has nil ToolResult", i)
		}
		if m.ToolResult.CallID != wantIDs[i] {
			t.Fatalf("appended[%d] CallID = %q, want %q (in order)", i, m.ToolResult.CallID, wantIDs[i])
		}
		if !m.ToolResult.IsError {
			t.Fatalf("appended[%d] IsError = false, want true (cancellation sentinel)", i)
		}
	}
}

func TestInterruptPartialToolResultsCloseOutRemainder(t *testing.T) {
	s := newTestSession(Limits{})
	if err := s.RecordUserPrompt("do two things", nil); err != nil {
		t.Fatalf("RecordUserPrompt: %v", err)
	}
	if err := s.BeginTurn(); err != nil {
		t.Fatalf("BeginTurn: %v", err)
	}
	calls := []ToolCall{
		NewToolCall("call-a", "read_file", nil),
		NewToolCall("call-b", "list_dir", nil),
	}
	if err := s.RecordAssistant(NewAssistantMessage("", "", calls)); err != nil {
		t.Fatalf("RecordAssistant: %v", err)
	}
	// call-a already answered before the cancel.
	if err := s.RecordToolResults([]ToolResult{NewToolResult("call-a", "ok")}); err != nil {
		t.Fatalf("RecordToolResults: %v", err)
	}
	if err := s.Cancel(); err != nil {
		t.Fatalf("Cancel: %v", err)
	}
	before := len(s.Conversation.Messages)

	if err := s.Interrupt(); err != nil {
		t.Fatalf("Interrupt: %v", err)
	}
	added := s.Conversation.Messages[before:]
	if len(added) != 1 {
		t.Fatalf("appended %d tool results, want 1 (only the missing call-b)", len(added))
	}
	if added[0].ToolResult == nil || added[0].ToolResult.CallID != "call-b" {
		t.Fatalf("appended result = %+v, want a synthetic result for call-b", added[0].ToolResult)
	}
	if !added[0].ToolResult.IsError {
		t.Fatalf("appended call-b IsError = false, want true")
	}
}

func TestInterruptNoOpWhenHistoryClean(t *testing.T) {
	t.Run("assistant with no tool calls", func(t *testing.T) {
		s := newTestSession(Limits{})
		if err := s.RecordUserPrompt("just answer", nil); err != nil {
			t.Fatalf("RecordUserPrompt: %v", err)
		}
		if err := s.BeginTurn(); err != nil {
			t.Fatalf("BeginTurn: %v", err)
		}
		if err := s.RecordAssistant(NewAssistantMessage("the answer", "", nil)); err != nil {
			t.Fatalf("RecordAssistant: %v", err)
		}
		if err := s.Cancel(); err != nil {
			t.Fatalf("Cancel: %v", err)
		}
		before := len(s.Conversation.Messages)
		if err := s.Interrupt(); err != nil {
			t.Fatalf("Interrupt: %v", err)
		}
		if len(s.Conversation.Messages) != before {
			t.Fatalf("Interrupt appended to clean history: len=%d, want %d", len(s.Conversation.Messages), before)
		}
	})

	t.Run("trailing user prompt (cancel before first token)", func(t *testing.T) {
		s := newTestSession(Limits{})
		if err := s.RecordUserPrompt("a prompt", nil); err != nil {
			t.Fatalf("RecordUserPrompt: %v", err)
		}
		if err := s.BeginTurn(); err != nil {
			t.Fatalf("BeginTurn: %v", err)
		}
		// Cancel before any assistant message is recorded.
		if err := s.Cancel(); err != nil {
			t.Fatalf("Cancel: %v", err)
		}
		before := len(s.Conversation.Messages)
		if err := s.Interrupt(); err != nil {
			t.Fatalf("Interrupt: %v", err)
		}
		if len(s.Conversation.Messages) != before {
			t.Fatalf("Interrupt appended to dangling-user history: len=%d, want %d", len(s.Conversation.Messages), before)
		}
	})

	t.Run("trailing user after a clean prior assistant+results", func(t *testing.T) {
		// Multi-turn: user → assistant(call) → toolresult → user → cancel. The prior
		// assistant turn is FULLY answered, and the trailing message is a user prompt
		// (no orphan), so Interrupt must be a no-op — a different code path from the
		// [user]-only and [assistant-no-calls] cases above.
		s := newTestSession(Limits{})
		if err := s.RecordUserPrompt("turn one", nil); err != nil {
			t.Fatalf("RecordUserPrompt: %v", err)
		}
		if err := s.BeginTurn(); err != nil {
			t.Fatalf("BeginTurn: %v", err)
		}
		if err := s.RecordAssistant(NewAssistantMessage("", "", []ToolCall{NewToolCall("call-a", "read_file", nil)})); err != nil {
			t.Fatalf("RecordAssistant: %v", err)
		}
		if err := s.RecordToolResults([]ToolResult{NewToolResult("call-a", "ok")}); err != nil {
			t.Fatalf("RecordToolResults: %v", err)
		}
		if err := s.Complete(); err != nil {
			t.Fatalf("Complete: %v", err)
		}
		if err := s.Reopen(); err != nil {
			t.Fatalf("Reopen: %v", err)
		}
		if err := s.RecordUserPrompt("turn two", nil); err != nil {
			t.Fatalf("RecordUserPrompt turn two: %v", err)
		}
		if err := s.BeginTurn(); err != nil {
			t.Fatalf("BeginTurn turn two: %v", err)
		}
		// Cancel before any assistant message for turn two.
		if err := s.Cancel(); err != nil {
			t.Fatalf("Cancel: %v", err)
		}
		before := len(s.Conversation.Messages)
		if err := s.Interrupt(); err != nil {
			t.Fatalf("Interrupt: %v", err)
		}
		if len(s.Conversation.Messages) != before {
			t.Fatalf("Interrupt appended despite a clean prior turn + trailing user: len=%d, want %d", len(s.Conversation.Messages), before)
		}
	})
}

// TestInterruptClosesOnlyTrailingOrphan asserts that when an earlier assistant turn
// is fully answered and only the trailing assistant turn is orphaned, Interrupt
// closes out ONLY the trailing orphan and leaves the earlier (answered) turn
// untouched — closeOutInterruptedTurn scopes to the LAST assistant message.
func TestInterruptClosesOnlyTrailingOrphan(t *testing.T) {
	s := newTestSession(Limits{})
	if err := s.RecordUserPrompt("turn one", nil); err != nil {
		t.Fatalf("RecordUserPrompt: %v", err)
	}
	if err := s.BeginTurn(); err != nil {
		t.Fatalf("BeginTurn: %v", err)
	}
	// First assistant turn: a tool call that IS answered.
	if err := s.RecordAssistant(NewAssistantMessage("", "", []ToolCall{NewToolCall("call-old", "read_file", nil)})); err != nil {
		t.Fatalf("RecordAssistant turn one: %v", err)
	}
	if err := s.RecordToolResults([]ToolResult{NewToolResult("call-old", "ok")}); err != nil {
		t.Fatalf("RecordToolResults: %v", err)
	}
	// Second assistant turn: an UNANSWERED tool call (the orphan).
	if err := s.RecordAssistant(NewAssistantMessage("", "", []ToolCall{NewToolCall("call-new", "list_dir", nil)})); err != nil {
		t.Fatalf("RecordAssistant turn two: %v", err)
	}
	if err := s.Cancel(); err != nil {
		t.Fatalf("Cancel: %v", err)
	}
	before := len(s.Conversation.Messages)

	if err := s.Interrupt(); err != nil {
		t.Fatalf("Interrupt: %v", err)
	}
	added := s.Conversation.Messages[before:]
	if len(added) != 1 {
		t.Fatalf("appended %d tool results, want exactly 1 (only the trailing orphan)", len(added))
	}
	if added[0].ToolResult == nil || added[0].ToolResult.CallID != "call-new" {
		t.Fatalf("appended result = %+v, want a synthetic result for the trailing call-new", added[0].ToolResult)
	}
	if !added[0].ToolResult.IsError {
		t.Fatalf("appended call-new IsError = false, want true")
	}
	// The earlier answered turn must NOT receive a second (duplicate) result.
	var oldResults int
	for _, m := range s.Conversation.Messages {
		if m.Role == RoleTool && m.ToolResult != nil && m.ToolResult.CallID == "call-old" {
			oldResults++
		}
	}
	if oldResults != 1 {
		t.Fatalf("call-old has %d tool results, want 1 (earlier turn untouched)", oldResults)
	}
}
