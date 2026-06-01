// Package sessnap provides a stable, JSON-friendly serialization ("snapshot")
// of a session.Session that the store adapters (memstore, jsonlstore) share.
//
// The Session aggregate exposes its lifecycle data through exported fields
// (ID, State, Mode, Conversation, Limits, Counters, Workspace, CreatedAt) and
// through the PendingAsk accessor. Two pieces of its state are unexported and
// not directly addressable from outside the session package:
//
//   - pending *PendingAsk — readable via Session.PendingAsk() (only while
//     StateAwaiting) and restorable via Session.PauseForApproval() (only from
//     StateRunning). The snapshot round-trips it for the awaiting case.
//   - stop StopReason — the recorded terminal stop reason. It is captured
//     faithfully via Session.RecordedStopReason() (which performs no limit
//     derivation) and restored via the matching terminal transition
//     Complete/Stop/Cancel/Fail. Capturing the recorded value directly means the
//     snapshot no longer has to infer the reason from the conflated
//     Session.StopReason(), so terminal round-trips are exact.
package sessnap

import (
	"encoding/json"
	"errors"
	"fmt"
	"time"

	"github.com/stacklok/mecatl/internal/session"
)

// Snapshot is the on-the-wire form of a session.Session. It is a plain data
// struct with JSON tags so it serializes deterministically regardless of the
// (untagged) layout of the domain types.
type Snapshot struct {
	ID         session.SessionID      `json:"id"`
	State      session.State          `json:"state"`
	Mode       session.PermissionMode `json:"mode"`
	Limits     session.Limits         `json:"limits"`
	Counters   session.Counters       `json:"counters"`
	Workspace  string                 `json:"workspace"`
	CreatedAt  time.Time              `json:"created_at"`
	Messages   []messageDTO           `json:"messages"`
	Pending    *session.PendingAsk    `json:"pending,omitempty"`
	StopReason session.StopReason     `json:"stop_reason,omitempty"`
}

// messageDTO mirrors session.Message with JSON tags. session.Message is
// untagged; encoding it directly would still work, but a DTO keeps the wire
// format explicit and stable against future field reordering.
type messageDTO struct {
	Role       session.Role        `json:"role"`
	Text       string              `json:"text,omitempty"`
	ToolCalls  []session.ToolCall  `json:"tool_calls,omitempty"`
	ToolResult *session.ToolResult `json:"tool_result,omitempty"`
	Reasoning  string              `json:"reasoning,omitempty"`
	// Parts carries non-text media on a user message. It is omitempty so a v1
	// snapshot with no "parts" key decodes to nil Parts — a text-only message,
	// exactly correct; the field is purely additive and needs no version bump.
	Parts []contentDTO `json:"parts,omitempty"`
}

// contentDTO mirrors session.Content with JSON tags. Data []byte marshals as
// base64 automatically. Exactly one of data/url is set on a well-formed part.
type contentDTO struct {
	Kind     session.MediaKind `json:"kind"`
	MIMEType string            `json:"mime_type,omitempty"`
	Data     []byte            `json:"data,omitempty"`
	URL      string            `json:"url,omitempty"`
}

// ErrNilSession is returned by Of when given a nil session.
var ErrNilSession = errors.New("sessnap: nil session")

// Of builds a Snapshot from a live Session, reading everything reachable
// through the Session's public surface.
func Of(s *session.Session) (Snapshot, error) {
	if s == nil {
		return Snapshot{}, ErrNilSession
	}
	snap := Snapshot{
		ID:        s.ID,
		State:     s.State,
		Mode:      s.Mode,
		Limits:    s.Limits,
		Counters:  s.Counters,
		Workspace: s.Workspace,
		CreatedAt: s.CreatedAt,
	}
	if s.Conversation != nil {
		snap.Messages = make([]messageDTO, len(s.Conversation.Messages))
		for i, m := range s.Conversation.Messages {
			snap.Messages[i] = toDTO(m)
		}
	}
	if ask, ok := s.PendingAsk(); ok {
		a := ask
		snap.Pending = &a
	}
	// Capture the recorded terminal reason faithfully (no limit derivation) so a
	// terminal session round-trips through the matching transition on restore.
	if r, ok := s.RecordedStopReason(); ok {
		snap.StopReason = r
	}
	return snap, nil
}

// Restore reconstructs a Session from a Snapshot by driving the session state
// machine through its public constructors and transitions, so all invariants
// hold on the rebuilt aggregate.
func (snap Snapshot) Restore() (*session.Session, error) {
	s := session.New(snap.ID, snap.Mode, snap.Workspace, snap.Limits, snap.CreatedAt)

	// Rebuild the conversation history verbatim.
	for _, dto := range snap.Messages {
		s.Conversation.Append(fromDTO(dto))
	}
	// Restore running totals directly; these are exported and authoritative.
	s.Counters = snap.Counters

	// Drive the state machine to the recorded lifecycle state. New() lands in
	// StateIdle; we advance from there.
	switch snap.State {
	case session.StateIdle:
		// already idle
	case session.StateRunning:
		if err := beginTurnPreservingCounters(s, snap.Counters); err != nil {
			return nil, err
		}
	case session.StateAwaiting:
		if err := beginTurnPreservingCounters(s, snap.Counters); err != nil {
			return nil, err
		}
		ask := session.PendingAsk{}
		if snap.Pending != nil {
			ask = *snap.Pending
		}
		if err := s.PauseForApproval(ask); err != nil {
			return nil, fmt.Errorf("sessnap: restore awaiting: %w", err)
		}
	case session.StateCompleted:
		// Stop(reason) records the exact captured reason; Complete is the special
		// case for a plain end-of-turn (StopEndTurn, or StopNone for an older
		// snapshot that predates the recorded reason). Because RecordedStopReason
		// captured the value faithfully, there is no inference here.
		if snap.StopReason == session.StopNone || snap.StopReason == session.StopEndTurn {
			if err := s.Complete(); err != nil {
				return nil, fmt.Errorf("sessnap: restore completed: %w", err)
			}
		} else if err := s.Stop(snap.StopReason); err != nil {
			return nil, fmt.Errorf("sessnap: restore completed: %w", err)
		}
	case session.StateCancelled:
		if err := s.Cancel(); err != nil {
			return nil, fmt.Errorf("sessnap: restore cancelled: %w", err)
		}
	case session.StateFailed:
		if err := s.Fail(); err != nil {
			return nil, fmt.Errorf("sessnap: restore failed: %w", err)
		}
	default:
		return nil, fmt.Errorf("sessnap: unknown state %q", snap.State)
	}
	return s, nil
}

// beginTurnPreservingCounters enters StateRunning without letting BeginTurn's
// turn increment clobber the restored counters.
func beginTurnPreservingCounters(s *session.Session, want session.Counters) error {
	if err := s.BeginTurn(); err != nil {
		return fmt.Errorf("sessnap: restore running: %w", err)
	}
	s.Counters = want
	return nil
}

// Marshal encodes the snapshot of s as a single JSON line (no trailing newline).
func Marshal(s *session.Session) ([]byte, error) {
	snap, err := Of(s)
	if err != nil {
		return nil, err
	}
	return json.Marshal(snap)
}

// Unmarshal decodes a JSON snapshot line and restores it into a Session.
func Unmarshal(line []byte) (*session.Session, error) {
	var snap Snapshot
	if err := json.Unmarshal(line, &snap); err != nil {
		return nil, fmt.Errorf("sessnap: decode snapshot: %w", err)
	}
	return snap.Restore()
}

func toDTO(m session.Message) messageDTO {
	return messageDTO{
		Role:       m.Role,
		Text:       m.Text,
		ToolCalls:  m.ToolCalls,
		ToolResult: m.ToolResult,
		Reasoning:  m.Reasoning,
		Parts:      contentToDTO(m.Parts),
	}
}

func fromDTO(dto messageDTO) session.Message {
	return session.Message{
		Role:       dto.Role,
		Text:       dto.Text,
		ToolCalls:  dto.ToolCalls,
		ToolResult: dto.ToolResult,
		Reasoning:  dto.Reasoning,
		Parts:      contentFromDTO(dto.Parts),
	}
}

func contentToDTO(parts []session.Content) []contentDTO {
	if len(parts) == 0 {
		return nil
	}
	out := make([]contentDTO, len(parts))
	for i, p := range parts {
		out[i] = contentDTO{Kind: p.Kind, MIMEType: p.MIMEType, Data: p.Data, URL: p.URL}
	}
	return out
}

func contentFromDTO(parts []contentDTO) []session.Content {
	if len(parts) == 0 {
		return nil
	}
	out := make([]session.Content, len(parts))
	for i, p := range parts {
		out[i] = session.Content{Kind: p.Kind, MIMEType: p.MIMEType, Data: p.Data, URL: p.URL}
	}
	return out
}
