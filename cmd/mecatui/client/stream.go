package client

import (
	"context"
	"errors"
	"io"
	"sync"

	tea "charm.land/bubbletea/v2"

	mecatlv1 "github.com/stacklok/mecatl/contracts/gen/go/mecatl/v1"
)

// Recver is the minimal receive side of a Converse stream: exactly what the
// reader goroutine needs. The generated grpc.BidiStreamingClient satisfies it,
// and tests supply a scripted fake — so the whole event pipeline runs offline,
// with no gRPC and no network. (The send side is the Sender interface below.)
type Recver interface {
	Recv() (*mecatlv1.ConverseResponse, error)
}

// Sender is the send side of the Converse stream. Separated from Recver so the
// reader goroutine holds only what it reads and the ui-side send helpers hold
// only what they send. The generated bidi client satisfies both.
type Sender interface {
	Send(*mecatlv1.ConverseRequest) error
}

// Stream wraps one open Converse run: a receive side, a send side, and a mutex
// that serialises Sends. gRPC permits concurrent Send and Recv from different
// goroutines but NOT concurrent Sends; the reader goroutine only Recvs, while
// approve/cancel commands Send — so a single send-mutex is sufficient and keeps
// the control frames ordered.
type Stream struct {
	recv Recver
	send Sender

	mu sync.Mutex // serialises Send (Recv is single-goroutine in the reader)
}

// NewStream binds a receive and send side into a Stream. Pass the same
// grpc.BidiStreamingClient for both in production; pass a fake Recver (and a
// no-op or recording Sender) in tests.
func NewStream(recv Recver, send Sender) *Stream {
	return &Stream{recv: recv, send: send}
}

// ReadLoop runs the receive loop on its OWN goroutine: it drains Recv and pushes
// translated tea.Msgs onto out, then closes out when the stream ends. It MUST
// run off the Bubble Tea update goroutine (it does no rendering and touches no
// model state) — glamour and the model are driven only from Update via the
// drained channel. A clean EOF yields StreamClosedMsg; any other error yields
// StreamErrMsg; both then close the channel so WaitForMsg stops re-arming.
//
// Every send selects on ctx.Done() as well as out, so the goroutine can never
// wedge if the ui drops the channel (e.g. endRun finalised the run and stopped
// draining). Pass the run's context — cancelling it unblocks and exits the
// reader. This makes the no-leak property structural, not just a reasoned
// invariant.
//
// Note ordering: the terminal "result" event arrives as a ResultMsg BEFORE the
// server closes the stream, so the ui finalises on ResultMsg and treats a later
// StreamClosedMsg as a no-op.
func (s *Stream) ReadLoop(ctx context.Context, out chan<- tea.Msg) {
	defer close(out)
	for {
		resp, err := s.recv.Recv()
		if err != nil {
			if errors.Is(err, io.EOF) {
				emit(ctx, out, StreamClosedMsg{})
				return
			}
			emit(ctx, out, StreamErrMsg{Err: err})
			return
		}
		if m := EventToMsg(resp.GetEvent()); m != nil {
			if !emit(ctx, out, m) {
				return // context cancelled — stop reading
			}
		}
	}
}

// emit pushes one msg onto out unless ctx is cancelled first; it reports whether
// the send succeeded. A cancelled context means the ui has torn the run down, so
// the reader stops rather than blocking on a no-longer-drained channel.
func emit(ctx context.Context, out chan<- tea.Msg, m tea.Msg) bool {
	select {
	case out <- m:
		return true
	case <-ctx.Done():
		return false
	}
}

// WaitForMsg is the canonical Bubble Tea fan-in command: it blocks on one msg
// from ch and returns it, so Update can re-arm it (return WaitForMsg(ch) again)
// to pull the next one. When ch is closed it returns StreamClosedMsg so the ui
// can tear down cleanly without a nil-msg storm.
func WaitForMsg(ch <-chan tea.Msg) tea.Cmd {
	return func() tea.Msg {
		m, ok := <-ch
		if !ok {
			return StreamClosedMsg{}
		}
		return m
	}
}

// SendPrompt sends the mandatory first frame. It MUST be the first Send on a
// fresh stream (the server rejects a non-prompt first frame). parts carries the
// non-text media (image/audio) built by ExpandMentions; nil for a text-only
// prompt. The server enforces the cross-field "text or parts non-empty" rule and
// re-validates every part (session.ValidateMediaParts), so a media-only prompt
// (empty text, non-nil parts) is legal here.
func (s *Stream) SendPrompt(sessionID, text string, parts []*mecatlv1.Content) error {
	return s.sendFrame(&mecatlv1.ConverseRequest{
		Kind: &mecatlv1.ConverseRequest_Prompt{
			Prompt: &mecatlv1.Prompt{SessionId: sessionID, Text: text, Parts: parts},
		},
	})
}

// Verdict is the client-local three-way resolution of a permission.ask. It keeps
// the proto ApprovalVerdict enum out of the ui package (which never imports
// contracts/gen): the ui chooses a Verdict, SendApproval translates it. The zero
// value is VerdictAllowOnce (the safe, transient allow).
type Verdict int

const (
	// VerdictAllowOnce permits this single call only (no rule learned).
	VerdictAllowOnce Verdict = iota
	// VerdictAllowAlways permits this call AND learns a session-scoped rule so the
	// same exact command is not re-asked for the rest of the session.
	VerdictAllowAlways
	// VerdictDeny denies this call.
	VerdictDeny
)

// SendApproval resolves a paused permission.ask. askID is the exact value from
// the PermissionAskMsg. It sets BOTH the legacy allow bool (so an older server
// that ignores the verdict enum still gets the right allow/deny) AND the verdict
// enum (so a newer server can learn the always-allow rule); the server's mapper
// prefers the verdict and falls back to the bool for UNSPECIFIED.
func (s *Stream) SendApproval(askID string, v Verdict) error {
	allow := v == VerdictAllowOnce || v == VerdictAllowAlways
	var verdict mecatlv1.ApprovalVerdict
	switch v {
	case VerdictAllowOnce:
		verdict = mecatlv1.ApprovalVerdict_APPROVAL_VERDICT_ALLOW_ONCE
	case VerdictAllowAlways:
		verdict = mecatlv1.ApprovalVerdict_APPROVAL_VERDICT_ALLOW_ALWAYS
	case VerdictDeny:
		verdict = mecatlv1.ApprovalVerdict_APPROVAL_VERDICT_DENY
	}
	return s.sendFrame(&mecatlv1.ConverseRequest{
		Kind: &mecatlv1.ConverseRequest_ResumeApproval{
			ResumeApproval: &mecatlv1.ResumeApproval{AskId: askID, Allow: allow, Verdict: verdict},
		},
	})
}

// SendCancel aborts the in-flight run; the loop ends with a result whose stop is
// "cancelled". The ui keeps the stream open until that terminal result arrives.
func (s *Stream) SendCancel() error {
	return s.sendFrame(&mecatlv1.ConverseRequest{
		Kind: &mecatlv1.ConverseRequest_Cancel{Cancel: &mecatlv1.Cancel{}},
	})
}

// sendFrame serialises one Send under the mutex.
func (s *Stream) sendFrame(req *mecatlv1.ConverseRequest) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.send.Send(req)
}
