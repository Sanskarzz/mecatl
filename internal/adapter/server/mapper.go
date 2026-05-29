package server

import (
	"math"

	ozzv1 "github.com/stacklok/ozzharness/contracts/gen/go/ozz/v1"
	"github.com/stacklok/ozzharness/internal/session"
)

// clampInt32 narrows a Go int (counter/index) to the proto int32 wire type,
// saturating at the int32 bounds rather than wrapping. These values (turn
// indices, counters) never realistically approach the limit; the clamp exists
// only so the conversion is provably overflow-safe.
func clampInt32(v int) int32 {
	switch {
	case v > math.MaxInt32:
		return math.MaxInt32
	case v < math.MinInt32:
		return math.MinInt32
	default:
		return int32(v)
	}
}

// toProto translates a domain session.Event into its wire-level proto Event.
// It is pure (no I/O, no shared state) so it can be unit-tested exhaustively
// across every EventType and submessage. The string type field mirrors
// session.EventType verbatim; the structured submessages are populated only
// when the corresponding domain pointer is set.
func toProto(ev session.Event) *ozzv1.Event {
	out := &ozzv1.Event{
		Type: string(ev.Type),
		Seq:  ev.Seq,
		Turn: clampInt32(ev.Turn),
		Text: ev.Text,
	}
	if ev.ToolCall != nil {
		out.ToolCall = toProtoToolCall(*ev.ToolCall)
	}
	if ev.ToolResult != nil {
		out.ToolResult = toProtoToolResult(*ev.ToolResult)
	}
	if ev.Ask != nil {
		out.Ask = toProtoAsk(*ev.Ask)
	}
	if ev.Result != nil {
		out.Result = toProtoResult(*ev.Result)
	}
	if ev.Usage != nil {
		out.Usage = toProtoUsage(*ev.Usage)
	}
	return out
}

// toProtoToolCall maps a session.ToolCall to its proto form.
func toProtoToolCall(c session.ToolCall) *ozzv1.ToolCall {
	return &ozzv1.ToolCall{
		Id:   string(c.ID),
		Name: c.Name,
		Args: string(c.Args),
	}
}

// toProtoToolResult maps a session.ToolResult to its proto form.
func toProtoToolResult(r session.ToolResult) *ozzv1.ToolResult {
	return &ozzv1.ToolResult{
		CallId:  string(r.CallID),
		Content: r.Content,
		IsError: r.IsError,
	}
}

// toProtoAsk maps a session.PendingAsk to its proto PermissionAsk form.
func toProtoAsk(a session.PendingAsk) *ozzv1.PermissionAsk {
	return &ozzv1.PermissionAsk{
		AskId:  a.AskID,
		Tool:   a.Tool,
		Args:   string(a.Args),
		Reason: a.Reason,
	}
}

// toProtoResult maps a session.ResultPayload to its proto Result form.
func toProtoResult(p session.ResultPayload) *ozzv1.Result {
	return &ozzv1.Result{
		Stop:  string(p.Stop),
		Text:  p.Text,
		Usage: toProtoUsage(p.Usage),
	}
}

// toProtoUsage maps a session.Usage to its proto form.
func toProtoUsage(u session.Usage) *ozzv1.Usage {
	return &ozzv1.Usage{
		InputTokens:      int64(u.InputTokens),
		OutputTokens:     int64(u.OutputTokens),
		CacheReadTokens:  int64(u.CacheReadTokens),
		CacheWriteTokens: int64(u.CacheWriteTokens),
	}
}

// toProtoSession maps a session.Session aggregate to its proto snapshot.
func toProtoSession(s *session.Session) *ozzv1.Session {
	return &ozzv1.Session{
		SessionId:     string(s.ID),
		State:         string(s.State),
		Mode:          modeToProto(s.Mode),
		Workspace:     s.Workspace,
		Limits:        limitsToProto(s.Limits),
		Turns:         clampInt32(s.Counters.Turns),
		ToolCalls:     clampInt32(s.Counters.ToolCalls),
		CreatedAtUnix: s.CreatedAt.Unix(),
	}
}

// limitsToProto maps session.Limits to the proto Limits message.
func limitsToProto(l session.Limits) *ozzv1.Limits {
	return &ozzv1.Limits{
		MaxTurns:               clampInt32(l.MaxTurns),
		MaxToolCalls:           clampInt32(l.MaxToolCalls),
		MaxConsecutiveFailures: clampInt32(l.MaxConsecutiveFailures),
	}
}

// limitsFromProto maps the proto Limits message to session.Limits, treating a
// nil message as the zero (all-disabled) value.
func limitsFromProto(l *ozzv1.Limits) session.Limits {
	if l == nil {
		return session.Limits{}
	}
	return session.Limits{
		MaxTurns:               int(l.GetMaxTurns()),
		MaxToolCalls:           int(l.GetMaxToolCalls()),
		MaxConsecutiveFailures: int(l.GetMaxConsecutiveFailures()),
	}
}

// modeToProto maps a session.PermissionMode to its proto enum.
func modeToProto(m session.PermissionMode) ozzv1.PermissionMode {
	switch m {
	case session.ModePlan:
		return ozzv1.PermissionMode_PERMISSION_MODE_PLAN
	case session.ModeAccept:
		return ozzv1.PermissionMode_PERMISSION_MODE_ACCEPT_EDITS
	case session.ModeDefault:
		return ozzv1.PermissionMode_PERMISSION_MODE_DEFAULT
	default:
		return ozzv1.PermissionMode_PERMISSION_MODE_DEFAULT
	}
}

// modeFromProto maps a proto enum to a session.PermissionMode, defaulting an
// unspecified value to ModeDefault.
func modeFromProto(m ozzv1.PermissionMode) session.PermissionMode {
	switch m {
	case ozzv1.PermissionMode_PERMISSION_MODE_PLAN:
		return session.ModePlan
	case ozzv1.PermissionMode_PERMISSION_MODE_ACCEPT_EDITS:
		return session.ModeAccept
	case ozzv1.PermissionMode_PERMISSION_MODE_DEFAULT, ozzv1.PermissionMode_PERMISSION_MODE_UNSPECIFIED:
		return session.ModeDefault
	default:
		return session.ModeDefault
	}
}
