package session

import (
	"encoding/json"
	"errors"
	"fmt"
	"time"
)

// SessionID uniquely identifies a session. Outside code holds a SessionID and
// reaches inner entities only through the Session aggregate root.
type SessionID string

// State is the session lifecycle state. The state machine is:
//
//	idle → running → awaiting → running → completed
//
// with Cancel permitted from any non-terminal state and Fail from any
// non-terminal state. completed, failed, and cancelled are terminal.
type State string

const (
	// StateIdle is the initial state: created, no turn running.
	StateIdle State = "idle"
	// StateRunning means a turn is in flight.
	StateRunning State = "running"
	// StateAwaiting means the loop is paused on a permission "ask".
	StateAwaiting State = "awaiting"
	// StateCompleted is a terminal success state.
	StateCompleted State = "completed"
	// StateFailed is a terminal failure state.
	StateFailed State = "failed"
	// StateCancelled is a terminal cancellation state.
	StateCancelled State = "cancelled"
)

// IsTerminal reports whether no further transitions are possible from s.
func (s State) IsTerminal() bool {
	switch s {
	case StateCompleted, StateFailed, StateCancelled:
		return true
	default:
		return false
	}
}

// PermissionMode is the session-wide permission posture, which drives plan-mode
// tool filtering and acceptEdits behaviour.
type PermissionMode string

const (
	// ModeDefault is the standard deny→ask→allow posture.
	ModeDefault PermissionMode = "default"
	// ModePlan enforces a read-only toolset (plan mode).
	ModePlan PermissionMode = "plan"
	// ModeAccept auto-accepts edits (acceptEdits).
	ModeAccept PermissionMode = "acceptEdits"
)

// ApprovalVerdict is the client's resolution of a permission.ask. It widens the
// historical allow/deny boolean into three outcomes so a client can ask the
// harness to LEARN an allow for the matching tool+pattern (allow_always) versus
// permitting only the current call (allow_once).
//
// VerdictDeny is the ZERO VALUE deliberately: a verdict that is never set, or a
// resolution path that abandons the ask (ctx cancel, transport error), fails
// SAFE to deny. A learned allow (VerdictAllowAlways) NEVER overrides a deny and
// NEVER bypasses plan-mode mutation denial — it only adds a narrow,
// session-scoped allow rule the Evaluator consults at the lowest precedence.
type ApprovalVerdict int

const (
	// VerdictDeny denies the call. It is the zero value (fail-safe default).
	VerdictDeny ApprovalVerdict = iota
	// VerdictAllowOnce allows the current call only; nothing is learned.
	VerdictAllowOnce
	// VerdictAllowAlways allows the current call AND asks the harness to learn a
	// per-session allow rule for the same tool + exact canonical pattern.
	VerdictAllowAlways
)

// StopReason explains why a run stopped. It is carried on terminal events and
// in the LLM provider's terminal chunk.
type StopReason string

const (
	// StopNone is the zero value: the run has not stopped.
	StopNone StopReason = ""
	// StopEndTurn means the model finished without requesting more tools.
	StopEndTurn StopReason = "end_turn"
	// StopMaxTurns means the MaxTurns limit was reached.
	StopMaxTurns StopReason = "max_turns"
	// StopMaxToolCalls means the MaxToolCalls limit was reached.
	StopMaxToolCalls StopReason = "max_tool_calls"
	// StopMaxConsecutiveFailures means too many tool calls failed in a row.
	StopMaxConsecutiveFailures StopReason = "max_consecutive_failures"
	// StopCancelled means the run was cancelled by the client or ctx.
	StopCancelled StopReason = "cancelled"
	// StopError means the run failed with an unrecoverable error.
	StopError StopReason = "error"
)

// Limits are the configured stop conditions for a session. A zero value in any
// field disables that particular limit.
type Limits struct {
	// MaxTurns caps the number of model calls; 0 disables.
	MaxTurns int
	// MaxToolCalls caps the total tool invocations; 0 disables.
	MaxToolCalls int
	// MaxConsecutiveFailures caps back-to-back tool failures; 0 disables.
	MaxConsecutiveFailures int
}

// Counters track running totals used to evaluate stop conditions.
type Counters struct {
	// Turns is the number of model calls begun.
	Turns int
	// ToolCalls is the total number of tool invocations recorded.
	ToolCalls int
	// ConsecutiveFailures is the current run of back-to-back tool failures; it
	// resets to zero on any successful tool result.
	ConsecutiveFailures int
}

// PendingAsk describes a permission prompt the loop is blocked on while in
// StateAwaiting. It is surfaced to the client via a permission.ask Event and
// resolved by ResumeWith.
type PendingAsk struct {
	// AskID correlates the ask with the client's resolution.
	AskID string
	// Tool is the name of the tool awaiting approval.
	Tool string
	// Args is the proposed tool-call argument payload.
	Args json.RawMessage
	// Reason explains why approval is required.
	Reason string
}

// Errors returned by the Session state machine.
var (
	// ErrIllegalTransition is returned when a method is called in a state that
	// does not permit it.
	ErrIllegalTransition = errors.New("session: illegal state transition")
	// ErrNoPendingAsk is returned by ResumeWith when the session is not awaiting
	// approval.
	ErrNoPendingAsk = errors.New("session: no pending ask to resume")
)

// Session is the aggregate root of the Agent Session context. All mutation of
// the conversation, counters, and lifecycle flows through its intention-revealing
// methods so the state machine and stop conditions always hold. Outside code
// holds a SessionID and reaches inner entities only through these methods, never
// through public setters.
type Session struct {
	// ID identifies this session.
	ID SessionID
	// State is the current lifecycle state.
	State State
	// Mode is the permission posture.
	Mode PermissionMode
	// Conversation is the model-visible history.
	Conversation *Conversation
	// Limits are the configured stop conditions.
	Limits Limits
	// Counters are the running totals for stop-condition evaluation.
	Counters Counters
	// Workspace is the root directory tools operate against (the session cwd).
	Workspace string
	// CreatedAt is the creation timestamp.
	CreatedAt time.Time

	// pending is set iff State == StateAwaiting.
	pending *PendingAsk
	// stop holds the terminal stop reason once the session has stopped.
	stop StopReason
}

// New constructs an idle Session with an empty conversation.
func New(id SessionID, mode PermissionMode, workspace string, limits Limits, createdAt time.Time) *Session {
	return &Session{
		ID:           id,
		State:        StateIdle,
		Mode:         mode,
		Conversation: &Conversation{},
		Limits:       limits,
		Workspace:    workspace,
		CreatedAt:    createdAt,
	}
}

// BeginTurn transitions the session into StateRunning at the start of a model
// call. It is legal from StateIdle (first turn) or StateRunning (a follow-up
// model call within the same active run). It increments the turn counter. If a
// turn limit is already reached it returns ErrIllegalTransition is NOT used;
// callers should consult StopReason before beginning a turn.
func (s *Session) BeginTurn() error {
	if s.State != StateIdle && s.State != StateRunning {
		return fmt.Errorf("%w: BeginTurn from %q", ErrIllegalTransition, s.State)
	}
	s.State = StateRunning
	s.Counters.Turns++
	return nil
}

// RecordAssistant appends the assistant message produced by a model call to the
// conversation. It is legal only while running.
func (s *Session) RecordAssistant(m Message) error {
	if s.State != StateRunning {
		return fmt.Errorf("%w: RecordAssistant from %q", ErrIllegalTransition, s.State)
	}
	s.Conversation.Append(m)
	return nil
}

// RecordToolResults appends tool-result messages to the conversation and updates
// the tool-call and consecutive-failure counters. It is legal only while
// running. Any successful result resets the consecutive-failure run; each error
// result extends it.
func (s *Session) RecordToolResults(results []ToolResult) error {
	if s.State != StateRunning {
		return fmt.Errorf("%w: RecordToolResults from %q", ErrIllegalTransition, s.State)
	}
	for _, r := range results {
		s.Conversation.Append(NewToolMessage(r))
		s.Counters.ToolCalls++
		if r.IsError {
			s.Counters.ConsecutiveFailures++
		} else {
			s.Counters.ConsecutiveFailures = 0
		}
	}
	return nil
}

// RecordUserPrompt appends a user prompt to the conversation through the
// aggregate root, optionally preceded by discovered project-instruction messages
// (AGENTS.md / CLAUDE.md). It is the intention-revealing seam the loop uses
// instead of poking the Conversation directly, so all history mutation flows
// through the root. It is legal from any non-terminal state (a prompt may be the
// first message while idle, or a follow-up while running).
func (s *Session) RecordUserPrompt(text string, instructions []Message) error {
	if s.State.IsTerminal() {
		return fmt.Errorf("%w: RecordUserPrompt from %q", ErrIllegalTransition, s.State)
	}
	for _, m := range instructions {
		s.Conversation.Append(m)
	}
	s.Conversation.Append(NewUserMessage(text))
	return nil
}

// ReplaceHistory atomically replaces the conversation history with messages. It
// is the compaction seam: the loop hands it the compacted message slice so the
// replacement flows through the root rather than mutating Conversation.Messages
// directly. It is legal only while running, when compaction occurs.
func (s *Session) ReplaceHistory(messages []Message) error {
	if s.State != StateRunning {
		return fmt.Errorf("%w: ReplaceHistory from %q", ErrIllegalTransition, s.State)
	}
	s.Conversation.Messages = messages
	return nil
}

// PauseForApproval suspends a running turn on a permission ask, transitioning to
// StateAwaiting. It is legal only while running.
func (s *Session) PauseForApproval(ask PendingAsk) error {
	if s.State != StateRunning {
		return fmt.Errorf("%w: PauseForApproval from %q", ErrIllegalTransition, s.State)
	}
	a := ask
	s.pending = &a
	s.State = StateAwaiting
	return nil
}

// PendingAsk returns the ask the session is blocked on and true when in
// StateAwaiting; otherwise it returns the zero value and false.
func (s *Session) PendingAsk() (PendingAsk, bool) {
	if s.State != StateAwaiting || s.pending == nil {
		return PendingAsk{}, false
	}
	return *s.pending, true
}

// ResumeWith clears the pending ask and returns the session to StateRunning so
// the loop can continue. It is legal only while awaiting; it returns
// ErrNoPendingAsk otherwise. The resolved ask is returned for reference. The
// caller (the loop) owns the permission decision and acts on it (allow →
// execute, deny → feed the reason back to the model); the aggregate only
// reconciles its own lifecycle, so it deliberately takes no governance type and
// keeps session a clean domain leaf.
func (s *Session) ResumeWith() (PendingAsk, error) {
	if s.State != StateAwaiting || s.pending == nil {
		return PendingAsk{}, ErrNoPendingAsk
	}
	ask := *s.pending
	s.pending = nil
	s.State = StateRunning
	return ask, nil
}

// Complete marks a successful terminal end of the run. It is legal from any
// non-terminal state and records StopEndTurn unless a stop reason is already set.
func (s *Session) Complete() error {
	if s.State.IsTerminal() {
		return fmt.Errorf("%w: Complete from %q", ErrIllegalTransition, s.State)
	}
	if s.stop == StopNone {
		s.stop = StopEndTurn
	}
	s.State = StateCompleted
	s.pending = nil
	return nil
}

// Stop marks a successful terminal end carrying an explicit stop reason (e.g. a
// limit was reached). It is legal from any non-terminal state.
func (s *Session) Stop(reason StopReason) error {
	if s.State.IsTerminal() {
		return fmt.Errorf("%w: Stop from %q", ErrIllegalTransition, s.State)
	}
	s.stop = reason
	s.State = StateCompleted
	s.pending = nil
	return nil
}

// Cancel transitions the session to StateCancelled. It is legal from any
// non-terminal state.
func (s *Session) Cancel() error {
	if s.State.IsTerminal() {
		return fmt.Errorf("%w: Cancel from %q", ErrIllegalTransition, s.State)
	}
	s.stop = StopCancelled
	s.State = StateCancelled
	s.pending = nil
	return nil
}

// Fail transitions the session to StateFailed with StopError. It is legal from
// any non-terminal state.
func (s *Session) Fail() error {
	if s.State.IsTerminal() {
		return fmt.Errorf("%w: Fail from %q", ErrIllegalTransition, s.State)
	}
	s.stop = StopError
	s.State = StateFailed
	s.pending = nil
	return nil
}

// Reopen returns a successfully-completed session to StateIdle so it can accept a
// new user prompt and run another turn-loop, preserving the conversation history.
// It is the multi-turn / long-lived-teammate continuation seam: the agent loop
// always drives a session to a terminal state within a single Run, so a session
// that must receive another prompt later — a team teammate awaiting a message, an
// interactive multi-turn chat — needs an explicit, guarded re-open rather than a
// fresh session that would lose its history.
//
// It is legal ONLY from StateCompleted (a clean end-of-run). A failed or cancelled
// run is NOT resumable, and a non-terminal session is already runnable, so every
// other state returns ErrIllegalTransition. Reopen clears the recorded stop reason
// and any pending ask, and RESETS the per-run Counters to zero so the configured
// Limits bound EACH prompt's work, matching their single-run meaning rather than
// silently becoming a session-lifetime cap. A caller that wants a lifetime budget
// (e.g. a team supervisor bounding total turns across a teammate's life) must
// enforce it separately. Conversation, Mode, Limits, and Workspace are preserved.
func (s *Session) Reopen() error {
	if s.State != StateCompleted {
		return fmt.Errorf("%w: Reopen from %q", ErrIllegalTransition, s.State)
	}
	s.State = StateIdle
	s.stop = StopNone
	s.pending = nil
	s.Counters = Counters{}
	return nil
}

// SetMode changes the session's permission posture. It is the intention-revealing
// seam an out-of-band control surface (e.g. ACP session/set_mode) uses to switch
// between default/plan/acceptEdits, so the change flows through the aggregate
// rather than poking the public Mode field.
//
// It is legal ONLY while the session is NOT actively progressing a turn — i.e.
// from StateIdle or any terminal state, but NOT from StateRunning or
// StateAwaiting. Changing the permission posture mid-turn would race the loop's
// own permission evaluation (plan mode hard-denies mutations; acceptEdits
// auto-allows them) against tool dispatch already in flight, so a mid-run switch
// is rejected with ErrIllegalTransition. A control surface that receives a
// set_mode while running must defer it (apply on the next prompt). Setting the
// mode it already has is a no-op success.
func (s *Session) SetMode(mode PermissionMode) error {
	if s.State == StateRunning || s.State == StateAwaiting {
		return fmt.Errorf("%w: SetMode from %q", ErrIllegalTransition, s.State)
	}
	s.Mode = mode
	return nil
}

// StopReason reports why the run should stop. It is a DERIVED predicate: it
// returns the recorded terminal reason if one is set, otherwise it computes a
// limit-tripped reason from the configured Limits and current Counters. It does
// not mutate state. The loop consults it as a pre-turn guard.
//
// Because it conflates the recorded reason with a limit derivation, it is NOT a
// faithful witness of terminal state. Persistence and any caller that needs the
// exact reason explicitly recorded via Stop/Cancel/Fail/Complete must use
// RecordedStopReason instead.
func (s *Session) StopReason() (StopReason, bool) {
	if r, ok := s.RecordedStopReason(); ok {
		return r, true
	}
	if r, ok := s.LimitTripped(); ok {
		return r, true
	}
	return StopNone, false
}

// RecordedStopReason returns the terminal stop reason explicitly recorded on the
// session (via Stop/Cancel/Fail/Complete) and true when one is set. It performs
// NO limit derivation, so it is a faithful witness of the recorded terminal
// state — use it for persistence and round-tripping rather than StopReason.
func (s *Session) RecordedStopReason() (StopReason, bool) {
	if s.stop == StopNone {
		return StopNone, false
	}
	return s.stop, true
}

// LimitTripped reports the stop reason implied by the configured Limits and
// current Counters, and true when a limit is reached. It is a pure predicate
// that ignores any recorded terminal reason; it never mutates state.
func (s *Session) LimitTripped() (StopReason, bool) {
	if s.Limits.MaxTurns > 0 && s.Counters.Turns >= s.Limits.MaxTurns {
		return StopMaxTurns, true
	}
	if s.Limits.MaxToolCalls > 0 && s.Counters.ToolCalls >= s.Limits.MaxToolCalls {
		return StopMaxToolCalls, true
	}
	if s.Limits.MaxConsecutiveFailures > 0 &&
		s.Counters.ConsecutiveFailures >= s.Limits.MaxConsecutiveFailures {
		return StopMaxConsecutiveFailures, true
	}
	return StopNone, false
}
