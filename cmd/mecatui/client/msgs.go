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

// HookDecision is the outcome a hook fire produced, as plain data the ui colours
// and ranks without touching proto. Mirrors mecatlv1.HookDecision.
type HookDecision string

const (
	// HookInfo is a benign, informational hook notice (the default).
	HookInfo HookDecision = "info"
	// HookBlocked means the hook vetoed the action — the most severe notice.
	HookBlocked HookDecision = "blocked"
	// HookModified means the hook rewrote the action's payload without blocking.
	HookModified HookDecision = "modified"
)

// HookMsg is an inline hook notice. Beyond the human-readable Text it carries the
// structured Phase (lifecycle point, e.g. "PreToolUse"), the related Tool (for
// per-tool phases), and the Decision (info/blocked/modified) so the ui can render
// it distinctly from a compaction notice and colour a blocked hook.
type HookMsg struct {
	Text     string
	Phase    string
	Tool     string
	Decision HookDecision
}

// SubagentKind discriminates the three subagent.* event kinds carried by a
// SubagentMsg, so the ui switches on a plain value rather than re-deriving it.
type SubagentKind string

const (
	// SubagentStart marks a Task subagent run beginning (Goal set).
	SubagentStart SubagentKind = "start"
	// SubagentTool marks a child tool call resolving (ToolName/IsError/ToolCount set).
	SubagentTool SubagentKind = "tool"
	// SubagentEnd marks a Task subagent run finishing (ToolCount/Usage/Stop/DurationMs set).
	SubagentEnd SubagentKind = "end"
)

// SubagentMsg is the REDACTED, metadata-only projection of a Task subagent's
// child run. It carries NO child content — only ids, a goal label, child tool
// names/counts, usage, stop, and duration — so the ui can render a subagent's
// activity under its Task card while the child's content stays isolated.
// ParentCallID attributes the msg to the originating Task tool block.
type SubagentMsg struct {
	Kind         SubagentKind
	ParentCallID string
	ChildID      string
	Goal         string
	ToolName     string
	IsError      bool
	ToolCount    int
	Usage        Usage
	Stop         string
	DurationMs   int64
}

// TeamKind discriminates the three team.* event kinds carried by a TeamMsg, so
// the ui switches on a plain value rather than re-deriving it from the proto.
type TeamKind string

const (
	// TeamStart marks a Team run beginning (Roster set).
	TeamStart TeamKind = "start"
	// TeamMember marks one forwarded member-session event (Member/InnerKind set,
	// plus the subset of Text/ToolName/Detail/IsError/Usage relevant to InnerKind).
	TeamMember TeamKind = "member"
	// TeamEnd marks a Team run finishing (Rounds/Stop/Usage set).
	TeamEnd TeamKind = "end"
)

// TeamMemberSpec is one roster entry forwarded on team.start, as plain data.
// Mirrors mecatlv1.TeamMemberSpec; carries only member metadata, never content.
type TeamMemberSpec struct {
	Name     string
	Role     string
	Mutating bool
	Lead     bool
}

// TeamMsg is the BOUNDED projection of an in-process team's run, as plain data
// the ui renders on the Team tool card. Unlike the metadata-only SubagentMsg, a
// team.member event carries BOUNDED member CONTENT (Text / a capped Detail
// preview) — the team is meant to be watched. It is still bounded and redacted
// server-side, and never enters the parent conversation. ParentCallID attributes
// the msg to the originating Team tool block.
type TeamMsg struct {
	Kind         TeamKind
	ParentCallID string
	TeamID       string
	// Roster is set on TeamStart.
	Roster []TeamMemberSpec
	// Member / InnerKind and the per-event content are set on TeamMember.
	Member    string
	InnerKind string
	Text      string
	ToolName  string
	Detail    string
	IsError   bool
	// Rounds / Stop are set on TeamEnd.
	Rounds int
	Stop   string
	// Usage is a member's per-event usage (TeamMember turn.end/result) or, on
	// TeamEnd, the summed team total.
	Usage Usage
}

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

// hookDecisionFrom converts a proto HookDecision enum to the plain HookDecision
// string the ui keys off. An unspecified/unknown value (including a nil Hook,
// since GetDecision is nil-safe) maps to HookInfo, the benign baseline.
func hookDecisionFrom(d mecatlv1.HookDecision) HookDecision {
	switch d {
	case mecatlv1.HookDecision_HOOK_DECISION_BLOCKED:
		return HookBlocked
	case mecatlv1.HookDecision_HOOK_DECISION_MODIFIED:
		return HookModified
	default:
		return HookInfo
	}
}

// subagentMsg builds a SubagentMsg of the given kind from a proto Subagent
// payload (nil-safe via the generated getters). It is the single translation
// point for the three subagent.* event kinds.
func subagentMsg(kind SubagentKind, s *mecatlv1.Subagent) SubagentMsg {
	return SubagentMsg{
		Kind:         kind,
		ParentCallID: s.GetParentCallId(),
		ChildID:      s.GetChildId(),
		Goal:         s.GetGoal(),
		ToolName:     s.GetToolName(),
		IsError:      s.GetIsError(),
		ToolCount:    int(s.GetToolCount()),
		Usage:        usageFrom(s.GetUsage()),
		Stop:         s.GetStop(),
		DurationMs:   s.GetDurationMs(),
	}
}

// teamMsg builds a TeamMsg of the given kind from a proto Team payload (nil-safe
// via the generated getters). It is the single translation point for the three
// team.* event kinds; the roster is converted to plain TeamMemberSpec values.
func teamMsg(kind TeamKind, t *mecatlv1.Team) TeamMsg {
	msg := TeamMsg{
		Kind:         kind,
		ParentCallID: t.GetParentCallId(),
		TeamID:       t.GetTeamId(),
		Member:       t.GetMember(),
		InnerKind:    t.GetInnerKind(),
		Text:         t.GetText(),
		ToolName:     t.GetToolName(),
		Detail:       t.GetDetail(),
		IsError:      t.GetIsError(),
		Rounds:       int(t.GetRounds()),
		Stop:         t.GetStop(),
		Usage:        usageFrom(t.GetUsage()),
	}
	for _, r := range t.GetRoster() {
		msg.Roster = append(msg.Roster, TeamMemberSpec{
			Name:     r.GetName(),
			Role:     r.GetRole(),
			Mutating: r.GetMutating(),
			Lead:     r.GetLead(),
		})
	}
	return msg
}

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
		h := ev.GetHook()
		return HookMsg{
			Text:     ev.GetText(),
			Phase:    h.GetPhase(),
			Tool:     h.GetTool(),
			Decision: hookDecisionFrom(h.GetDecision()),
		}
	case "subagent.start":
		return subagentMsg(SubagentStart, ev.GetSubagent())
	case "subagent.tool":
		return subagentMsg(SubagentTool, ev.GetSubagent())
	case "subagent.end":
		return subagentMsg(SubagentEnd, ev.GetSubagent())
	case "team.start":
		return teamMsg(TeamStart, ev.GetTeam())
	case "team.member":
		return teamMsg(TeamMember, ev.GetTeam())
	case "team.end":
		return teamMsg(TeamEnd, ev.GetTeam())
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
