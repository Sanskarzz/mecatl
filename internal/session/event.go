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
)

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
	// Usage is set on usage-bearing events. On EvResult it is the cumulative run
	// total; turn.end carries its per-turn usage in TurnEnd, NOT here.
	Usage *Usage
}
