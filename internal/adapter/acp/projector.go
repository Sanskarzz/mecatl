package acp

import (
	"github.com/stacklok/mecatl/internal/session"
)

// projector.go is the ACP analogue of internal/adapter/server/mapper.go: it
// translates a domain session.Event into the ACP session/update payload(s) the
// editor renders. It is PURE (no I/O, no shared state) so it is exhaustively
// table-testable, and it is the single place that decides which events map, which
// fold, and which drop this phase.
//
// PROJECTION TABLE (Phase 1):
//
//	EvMessageDelta   -> agent_message_chunk{ content: text }
//	EvReasoningDelta -> agent_thought_chunk{ content: text }   (display summary
//	                    only — NEVER the encrypted_content replay blob, which the
//	                    loop never emits as a reasoning.delta anyway)
//	EvToolCall       -> tool_call{ toolCallId, title, kind, rawInput, status: pending }
//	EvToolResult     -> tool_call_update{ toolCallId, status: completed|failed,
//	                    content: [text] }
//	EvPermissionAsk  -> handled out of band (an OUTBOUND request_permission), not a
//	                    session/update; see permissionRequest below.
//	EvResult         -> the prompt's terminal stopReason (see stopReasonFor).
//
//	DROPPED/FOLDED this phase (no clean ACP mapping yet; full fidelity is a later
//	phase, see the ADR):
//	  - turn.start / turn.end      -> dropped (lifecycle bookkeeping).
//	  - compaction                 -> dropped.
//	  - hook                       -> dropped.
//	  - subagent.* / team.*        -> dropped (these are redacted child projections;
//	                                  surfacing them faithfully needs ACP plan/
//	                                  nested-tool modelling deferred to a later phase).
//	  - session.init               -> dropped.

// projectUpdate maps a domain Event to the session/update variant value to send,
// or (nil,false) when the event has no session/update projection this phase
// (dropped, folded elsewhere, or handled out of band like permission.ask and the
// terminal result). The returned value is one of the union variant types
// (chunkUpdate / toolCallUpdate) ready to marshal as the notification's "update".
func projectUpdate(ev session.Event) (any, bool) {
	switch ev.Type {
	case session.EvMessageDelta:
		if ev.Text == "" {
			return nil, false
		}
		return chunkUpdate{SessionUpdate: updateAgentMessageChunk, Content: textBlock(ev.Text)}, true

	case session.EvReasoningDelta:
		if ev.Text == "" {
			return nil, false
		}
		return chunkUpdate{SessionUpdate: updateAgentThoughtChunk, Content: textBlock(ev.Text)}, true

	case session.EvToolCall:
		if ev.ToolCall == nil {
			return nil, false
		}
		return toolCallUpdate{
			SessionUpdate: updateToolCall,
			ToolCallID:    string(ev.ToolCall.ID),
			Title:         ev.ToolCall.Name,
			Kind:          toolKindFor(ev.ToolCall.Name),
			RawInput:      rawInput(ev.ToolCall.Args),
			Status:        toolStatusPending,
		}, true

	case session.EvToolResult:
		if ev.ToolResult == nil {
			return nil, false
		}
		status := toolStatusCompleted
		if ev.ToolResult.IsError {
			status = toolStatusFailed
		}
		return toolCallUpdate{
			SessionUpdate: updateToolCallUpdate,
			ToolCallID:    string(ev.ToolResult.CallID),
			Status:        status,
			Content:       textToolContent(ev.ToolResult.Content),
		}, true

	default:
		// turn.*, compaction, hook, subagent.*, team.*, session.init, result,
		// permission.ask: no session/update projection here.
		return nil, false
	}
}

// rawInput normalizes a tool call's raw JSON args for the ACP rawInput field. An
// empty payload becomes nil (omitted) so the field is absent rather than an empty
// blob.
func rawInput(args []byte) []byte {
	if len(args) == 0 {
		return nil
	}
	return args
}

// toolKindFor maps a mecatl tool name to the nearest ACP ToolKind so the editor
// can pick an icon. Unknown tools (including MCP tools) fall back to "other".
func toolKindFor(name string) string {
	switch name {
	case "Read":
		return "read"
	case "Edit", "Write":
		return "edit"
	case "Grep", "Glob":
		return "search"
	case "Bash":
		return "execute"
	case "WebFetch":
		return "fetch"
	case "Task", "Team", "Fork":
		return "think"
	default:
		return "other"
	}
}

// stopReasonFor maps the domain terminal StopReason (carried on EvResult) to the
// ACP PromptResponse.stopReason. The two limit reasons collapse to ACP's single
// max_turn_requests; a domain error has no clean ACP terminal, so it maps to
// end_turn (the editor still receives any error text via the preceding message
// chunks) — this is the documented "clean terminal" for an error this phase.
func stopReasonFor(r session.StopReason) string {
	switch r {
	case session.StopEndTurn:
		return stopEndTurn
	case session.StopCancelled:
		return stopCancelled
	case session.StopMaxTurns, session.StopMaxToolCalls, session.StopMaxConsecutiveFailures:
		return stopMaxTurnRequests
	case session.StopError:
		return stopEndTurn
	default:
		return stopEndTurn
	}
}

// permissionRequestFor builds the OUTBOUND session/request_permission params for
// a domain permission.ask. It offers the four standard options; the title/kind
// reflect the tool awaiting approval, and rawInput carries the proposed args so
// the editor can show what is about to run. The toolCall has no sessionUpdate
// field (it is the request's toolCall, not a notification).
func permissionRequestFor(sessionID string, ask session.PendingAsk) requestPermissionRequest {
	return requestPermissionRequest{
		SessionID: sessionID,
		ToolCall: toolCallUpdate{
			ToolCallID: ask.AskID,
			Title:      ask.Tool,
			Kind:       toolKindFor(ask.Tool),
			Status:     toolStatusPending,
			RawInput:   rawInput(ask.Args),
		},
		Options: []permissionOption{
			{OptionID: permAllowOnce, Name: "Allow once", Kind: permAllowOnce},
			{OptionID: permAllowAlways, Name: "Allow always", Kind: permAllowAlways},
			{OptionID: permRejectOnce, Name: "Reject once", Kind: permRejectOnce},
			{OptionID: permRejectAlways, Name: "Reject always", Kind: permRejectAlways},
		},
	}
}

// approvalFor maps a request_permission outcome to the boolean run.Approve takes.
// A "selected" allow_once/allow_always approves; reject_* denies; a "cancelled"
// outcome (the editor aborted the turn) denies. allow_always behaves as
// allow_once this phase — there is no rule persistence yet (documented gap).
func approvalFor(outcome permissionOutcome) bool {
	if outcome.Outcome != outcomeSelected {
		return false // cancelled (or unknown) -> deny
	}
	switch outcome.OptionID {
	case permAllowOnce, permAllowAlways:
		return true
	default:
		return false
	}
}
