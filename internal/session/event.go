package session

// EventType is the kind of a domain Event. This is the single event taxonomy
// shared by the agent loop and the API; the API serializes it to proto and it is
// never a provider-specific type.
type EventType string

const (
	// EvSessionInit is emitted once when a run starts.
	EvSessionInit EventType = "session.init"
	// EvTurnStart is emitted at the beginning of each turn. Its Turn field is the
	// 0-based turn index (turnIdx = Counters.Turns - 1); turn.end mirrors it. The
	// 0-based wire contract is load-bearing — clients that surface a human-facing
	// "turn N" must add 1 themselves; do not shift the wire value.
	EvTurnStart EventType = "turn.start"
	// EvTurnEnd closes a turn's model exchange, carrying the typed TurnEndPayload
	// (this turn's Usage + elapsed model-call time). Emitted once per successful
	// turn, before the assistant message is recorded; not emitted on error/cancel.
	EvTurnEnd EventType = "turn.end"
	// EvMessageDelta carries streamed assistant text.
	EvMessageDelta EventType = "message.delta"
	// EvReasoningDelta carries streamed, human-readable reasoning summary text.
	// It is display-only and distinct from the opaque Message.Reasoning replay
	// item: it is emitted in addition to (never in place of) the reasoning
	// accumulation that is replayed back to the provider as encrypted content.
	EvReasoningDelta EventType = "reasoning.delta"
	// EvToolCall is emitted when a tool is about to run.
	EvToolCall EventType = "tool.call"
	// EvToolResult carries the result of a tool execution.
	EvToolResult EventType = "tool.result"
	// EvPermissionAsk is emitted when the loop pauses for client approval.
	EvPermissionAsk EventType = "permission.ask"
	// EvHook is emitted when a hook fires (e.g. PreToolUse blocked).
	EvHook EventType = "hook"
	// EvCompaction is emitted when a compaction boundary is crossed.
	EvCompaction EventType = "compaction"
	// EvResult is the terminal event: success / limit / error / cancelled.
	EvResult EventType = "result"

	// EvSubagentStart is emitted when a Task subagent run begins. It is a
	// REDACTED observability projection of a child loop — never the child's
	// content. It carries only the parent call id, the child session id, and a
	// short goal label so a client can attribute and title the subagent card.
	EvSubagentStart EventType = "subagent.start"
	// EvSubagentTool is emitted each time a Task subagent's child tool call
	// resolves. It is a REDACTED observability projection: it forwards ONLY the
	// child tool's NAME and error bool plus a running count — never the child's
	// tool args or result content, and never the child's message text. This keeps
	// the context-isolation guarantee (gauntlet #7) intact: nothing the child
	// produces enters the parent's conversation.
	EvSubagentTool EventType = "subagent.tool"
	// EvSubagentEnd is emitted when a Task subagent run terminates. It is a
	// REDACTED observability projection carrying only aggregate metadata — the
	// child's tool count, token usage, stop reason, and wall-clock duration —
	// never any child content. The child's terminal summary still folds back into
	// the parent conversation exclusively via the Task tool's ToolResult.
	EvSubagentEnd EventType = "subagent.end"
)

// HookDecision is the outcome a hook fire produced, so a client can colour and
// rank a hook notice without parsing its prose. It is provider-neutral and maps
// 1:1 to a proto enum.
type HookDecision string

const (
	// HookInfo is a benign, informational hook notice (the default): the hook
	// fired and allowed the action, or reported something non-blocking.
	HookInfo HookDecision = "info"
	// HookBlocked means the hook vetoed the action (a PreToolUse/UserPromptSubmit
	// block, or a fail-safe hook error). These can abort a run and must read as
	// the most severe hook notice.
	HookBlocked HookDecision = "blocked"
	// HookModified means the hook rewrote the action's payload (prompt rewrite,
	// tool-arg or tool-result mutation) without blocking it.
	HookModified HookDecision = "modified"
)

// HookPayload is the structured detail carried by an EvHook Event, in addition
// to the human-readable Event.Text. It lets a client render a hook notice
// distinctly from a compaction notice — labelling the lifecycle Phase and
// colouring the Decision (e.g. a blocked hook in an error colour) — instead of
// string-parsing the free text. All fields are optional; a zero value renders as
// a generic informational hook.
type HookPayload struct {
	// Phase is the lifecycle point the hook fired at (e.g. "PreToolUse"); empty
	// when not applicable.
	Phase string
	// Tool is the tool the hook relates to for the per-tool phases; empty
	// otherwise.
	Tool string
	// Decision is the outcome (info / blocked / modified).
	Decision HookDecision
}

// ResultPayload is the terminal payload carried by an EvResult Event.
type ResultPayload struct {
	// Stop is the reason the run ended.
	Stop StopReason
	// Text is the final assistant text, if any.
	Text string
	// Usage is the cumulative token accounting for the run.
	Usage Usage
	// Error carries the failure detail when Stop is StopError (empty otherwise).
	// It surfaces the error the loop would otherwise drop so callers (the demo,
	// API clients) can see why a run failed instead of an opaque "error".
	Error string
}

// TurnEndPayload is the payload carried by an EvTurnEnd Event. It is a typed
// envelope (mirroring ResultPayload) so turn.end owns its own usage semantics
// and has room to grow (finish reason, model id, retries) without overloading
// the shared Event fields. This keeps Event.Usage with a single meaning — the
// cumulative run total on EvResult — rather than two semantics on one field.
type TurnEndPayload struct {
	// Usage is THIS turn's model-call usage (not the cumulative run total).
	Usage Usage
	// DurationMs is the elapsed milliseconds for the turn's model call; 0 when no
	// Clock is injected.
	DurationMs int64
}

// SubagentPayload is the REDACTED observability projection carried by the three
// subagent.* events (EvSubagentStart / EvSubagentTool / EvSubagentEnd). It is the
// ONLY information about a Task subagent's child run that surfaces to clients, and
// it deliberately carries no child content — no message text, no tool args, no
// tool result bodies — only metadata. This is orthogonal to the context-isolation
// guarantee (gauntlet #7): forwarding metadata to the event stream never touches
// the parent's Conversation, so the child's content still never enters the context
// sent to the LLM.
//
// Which fields are set depends on the event kind:
//   - EvSubagentStart: ParentCallID, ChildID, Goal.
//   - EvSubagentTool:  ParentCallID, ChildID, ToolName, IsError, ToolCount.
//   - EvSubagentEnd:   ParentCallID, ChildID, ToolCount, Usage, Stop, DurationMs.
type SubagentPayload struct {
	// ParentCallID is the parent's Task tool-call id, used by clients to attribute
	// this event to the originating Task card. Set on all three kinds.
	ParentCallID string
	// ChildID is the child session id, distinguishing concurrent subagents. Set on
	// all three kinds.
	ChildID string
	// Goal is a short, plain-text label for the delegated task (the Task call's
	// description, or a truncation of its prompt). Set on EvSubagentStart only.
	Goal string
	// ToolName is the name of a child tool that just ran. Set on EvSubagentTool
	// only. It is the tool NAME alone — never the child's tool args or result.
	ToolName string
	// IsError reports whether the child tool call failed. Set on EvSubagentTool
	// only.
	IsError bool
	// ToolCount is the running (EvSubagentTool) or final (EvSubagentEnd) number of
	// child tool calls observed.
	ToolCount int
	// Usage is the child run's cumulative token accounting. Set on EvSubagentEnd
	// only.
	Usage Usage
	// Stop is the child run's terminal stop reason. Set on EvSubagentEnd only.
	Stop StopReason
	// DurationMs is the child run's wall-clock duration in milliseconds
	// (best-effort). Set on EvSubagentEnd only.
	DurationMs int64
}

// Event is the domain-owned, provider-neutral unit of the streaming model. The
// loop runs as a producer writing Events to a channel; server adapters relay
// them to the gRPC server-stream or HTTP SSE.
type Event struct {
	// Type is the event kind.
	Type EventType
	// Seq is a monotonically increasing sequence number within a run.
	Seq int64
	// Turn is the turn index this event belongs to.
	Turn int
	// Text carries streamed or final text where applicable.
	Text string
	// ToolCall is set on EvToolCall.
	ToolCall *ToolCall
	// ToolResult is set on EvToolResult.
	ToolResult *ToolResult
	// Ask is set on EvPermissionAsk.
	Ask *PendingAsk
	// Result is set on EvResult.
	Result *ResultPayload
	// TurnEnd is set on EvTurnEnd (this turn's usage + elapsed time).
	TurnEnd *TurnEndPayload
	// Hook is set on EvHook: the structured phase/tool/decision so clients render
	// hook notices distinctly (and colour blocked ones) rather than parsing Text.
	Hook *HookPayload
	// Usage is set on usage-bearing events. On EvResult it is the cumulative run
	// total; turn.end carries its per-turn usage in TurnEnd, NOT here.
	Usage *Usage
	// Subagent is set on the three subagent.* events: the REDACTED observability
	// projection of a Task child run (metadata only, never child content).
	Subagent *SubagentPayload
}
