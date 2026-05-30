// Package client is the gRPC-facing layer of mecatui: it dials mecated, creates
// sessions, opens the bidi Converse stream, and translates proto Events into the
// plain Go tea.Msg structs the ui consumes. It is the ONLY mecatui package that
// imports contracts/gen + grpc; the ui never sees a proto type. This boundary is
// deliberate: it keeps the Elm model rendering "pure data" and makes the whole
// event pipeline testable from a scripted fake (see Recver / fakeStream in
// tests) with no network.
package client

import (
	tea "charm.land/bubbletea/v2"

	mecatlv1 "github.com/stacklok/mecatl/contracts/gen/go/mecatl/v1"
)

// The msg taxonomy: one struct per proto Event type, plus transport/lifecycle
// msgs that don't originate from the stream. These are plain data — no proto,
// no grpc — so ui can switch over them freely.

// SessionInitMsg marks the run stream as live (proto type "session.init").
type SessionInitMsg struct{ Seq int64 }

// TurnStartMsg opens a new assistant turn; the spinner starts here.
type TurnStartMsg struct{ Turn int32 }

// AssistantDeltaMsg is a streamed chunk of assistant markdown to append+rerender.
type AssistantDeltaMsg struct {
	Turn int32
	Text string
}

// ReasoningDeltaMsg is a streamed chunk of the model's human-readable reasoning
// summary for a turn. Display-only and clearly subordinate to the assistant
// text; the ui renders it collapsed by default.
type ReasoningDeltaMsg struct {
	Turn int32
	Text string
}

// TurnEndMsg closes a turn's model exchange, carrying that turn's token usage
// and the elapsed model-call time. DurationMs is 0 when the server had no clock.
type TurnEndMsg struct {
	Turn       int32
	Usage      Usage
	DurationMs int64
}

// ToolCallMsg announces a tool invocation (status: running until its result).
type ToolCallMsg struct {
	ID   string
	Name string
	Args string // raw JSON
}

// ToolResultMsg resolves the matching ToolCallMsg by CallID.
type ToolResultMsg struct {
	CallID  string
	Content string
	IsError bool
}

// PermissionAskMsg opens the approval modal; AskID is the exact correlation key
// echoed back in ResumeApproval — never inferred from the tool name.
type PermissionAskMsg struct {
	AskID  string
	Tool   string
	Args   string // raw JSON
	Reason string
}

// HookMsg is a muted inline hook notice.
type HookMsg struct{ Text string }

// CompactionMsg is a muted "history compacted" notice.
type CompactionMsg struct{ Text string }

// ResultMsg is the terminal event: stop reason, final text, error, usage.
type ResultMsg struct {
	Stop  string
	Text  string
	Error string
	Usage Usage
}

// Usage is the token accounting carried by ResultMsg (and usage-bearing events).
// Duplicated as a plain struct so ui stays proto-free.
type Usage struct {
	InputTokens      int64
	OutputTokens     int64
	CacheReadTokens  int64
	CacheWriteTokens int64
}

// Transport / lifecycle msgs (NOT from the proto stream).

// SessionReadyMsg carries the session id from the async CreateSession.
type SessionReadyMsg struct{ SessionID string }

// ConnectErrMsg reports a dial/CreateSession failure.
type ConnectErrMsg struct{ Err error }

// StreamErrMsg reports a non-EOF Recv error on the Converse stream.
type StreamErrMsg struct{ Err error }

// StreamClosedMsg reports a clean stream close (io.EOF) without a result event
// (e.g. server closed early). Normal completion arrives as ResultMsg first.
type StreamClosedMsg struct{}

// usageFrom converts a proto Usage (nil-safe) to the plain struct.
func usageFrom(u *mecatlv1.Usage) Usage {
	if u == nil {
		return Usage{}
	}
	return Usage{
		InputTokens:      u.GetInputTokens(),
		OutputTokens:     u.GetOutputTokens(),
		CacheReadTokens:  u.GetCacheReadTokens(),
		CacheWriteTokens: u.GetCacheWriteTokens(),
	}
}

// EventToMsg maps a single proto Event onto its tea.Msg. It is a total function
// over the documented type strings; an unknown/empty type returns nil (the
// reader skips nil so unknown future event kinds are ignored, not fatal). This
// is the single translation point between the proto schema and the ui model and
// is unit-tested over every type.
func EventToMsg(ev *mecatlv1.Event) tea.Msg {
	if ev == nil {
		return nil
	}
	switch ev.GetType() {
	case "session.init":
		return SessionInitMsg{Seq: ev.GetSeq()}
	case "turn.start":
		return TurnStartMsg{Turn: ev.GetTurn()}
	case "turn.end":
		te := ev.GetTurnEnd()
		return TurnEndMsg{Turn: ev.GetTurn(), Usage: usageFrom(te.GetUsage()), DurationMs: te.GetDurationMs()}
	case "message.delta":
		return AssistantDeltaMsg{Turn: ev.GetTurn(), Text: ev.GetText()}
	case "reasoning.delta":
		return ReasoningDeltaMsg{Turn: ev.GetTurn(), Text: ev.GetText()}
	case "tool.call":
		tc := ev.GetToolCall()
		return ToolCallMsg{ID: tc.GetId(), Name: tc.GetName(), Args: tc.GetArgs()}
	case "tool.result":
		tr := ev.GetToolResult()
		return ToolResultMsg{CallID: tr.GetCallId(), Content: tr.GetContent(), IsError: tr.GetIsError()}
	case "permission.ask":
		a := ev.GetAsk()
		return PermissionAskMsg{AskID: a.GetAskId(), Tool: a.GetTool(), Args: a.GetArgs(), Reason: a.GetReason()}
	case "hook":
		return HookMsg{Text: ev.GetText()}
	case "compaction":
		return CompactionMsg{Text: ev.GetText()}
	case "result":
		r := ev.GetResult()
		return ResultMsg{
			Stop:  r.GetStop(),
			Text:  r.GetText(),
			Error: r.GetError(),
			Usage: usageFrom(r.GetUsage()),
		}
	default:
		return nil
	}
}
