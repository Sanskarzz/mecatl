package server

import (
	"math"

	mecatlv1 "github.com/stacklok/mecatl/contracts/gen/go/mecatl/v1"
	"github.com/stacklok/mecatl/internal/session"
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
func toProto(ev session.Event) *mecatlv1.Event {
	out := &mecatlv1.Event{
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
func toProtoToolCall(c session.ToolCall) *mecatlv1.ToolCall {
	return &mecatlv1.ToolCall{
		Id:   string(c.ID),
		Name: c.Name,
		Args: string(c.Args),
	}
}

// toProtoToolResult maps a session.ToolResult to its proto form.
func toProtoToolResult(r session.ToolResult) *mecatlv1.ToolResult {
	return &mecatlv1.ToolResult{
		CallId:  string(r.CallID),
		Content: r.Content,
		IsError: r.IsError,
	}
}

// toProtoAsk maps a session.PendingAsk to its proto PermissionAsk form.
func toProtoAsk(a session.PendingAsk) *mecatlv1.PermissionAsk {
	return &mecatlv1.PermissionAsk{
		AskId:  a.AskID,
		Tool:   a.Tool,
		Args:   string(a.Args),
		Reason: a.Reason,
	}
}

// toProtoResult maps a session.ResultPayload to its proto Result form.
func toProtoResult(p session.ResultPayload) *mecatlv1.Result {
	return &mecatlv1.Result{
		Stop:  string(p.Stop),
		Text:  p.Text,
		Usage: toProtoUsage(p.Usage),
		Error: p.Error,
	}
}

// toProtoUsage maps a session.Usage to its proto form.
func toProtoUsage(u session.Usage) *mecatlv1.Usage {
	return &mecatlv1.Usage{
		InputTokens:      int64(u.InputTokens),
		OutputTokens:     int64(u.OutputTokens),
		CacheReadTokens:  int64(u.CacheReadTokens),
		CacheWriteTokens: int64(u.CacheWriteTokens),
	}
}

// toProtoSession maps a session.Session aggregate to its proto snapshot.
func toProtoSession(s *session.Session) *mecatlv1.Session {
	return &mecatlv1.Session{
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
func limitsToProto(l session.Limits) *mecatlv1.Limits {
	return &mecatlv1.Limits{
		MaxTurns:               clampInt32(l.MaxTurns),
		MaxToolCalls:           clampInt32(l.MaxToolCalls),
		MaxConsecutiveFailures: clampInt32(l.MaxConsecutiveFailures),
	}
}

// limitsFromProto maps the proto Limits message to session.Limits, treating a
// nil message as the zero (all-disabled) value.
func limitsFromProto(l *mecatlv1.Limits) session.Limits {
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
func modeToProto(m session.PermissionMode) mecatlv1.PermissionMode {
	switch m {
	case session.ModePlan:
		return mecatlv1.PermissionMode_PERMISSION_MODE_PLAN
	case session.ModeAccept:
		return mecatlv1.PermissionMode_PERMISSION_MODE_ACCEPT_EDITS
	case session.ModeDefault:
		return mecatlv1.PermissionMode_PERMISSION_MODE_DEFAULT
	default:
		return mecatlv1.PermissionMode_PERMISSION_MODE_DEFAULT
	}
}

// modeFromProto maps a proto enum to a session.PermissionMode, defaulting an
// unspecified value to ModeDefault.
func modeFromProto(m mecatlv1.PermissionMode) session.PermissionMode {
	switch m {
	case mecatlv1.PermissionMode_PERMISSION_MODE_PLAN:
		return session.ModePlan
	case mecatlv1.PermissionMode_PERMISSION_MODE_ACCEPT_EDITS:
		return session.ModeAccept
	case mecatlv1.PermissionMode_PERMISSION_MODE_DEFAULT, mecatlv1.PermissionMode_PERMISSION_MODE_UNSPECIFIED:
		return session.ModeDefault
	default:
		return session.ModeDefault
	}
}
