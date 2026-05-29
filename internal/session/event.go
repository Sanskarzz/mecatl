package session

// EventType is the kind of a domain Event. This is the single event taxonomy
// shared by the agent loop and the API; the API serializes it to proto and it is
// never a provider-specific type.
type EventType string

const (
	// EvSessionInit is emitted once when a run starts.
	EvSessionInit EventType = "session.init"
	// EvTurnStart is emitted at the beginning of each turn.
	EvTurnStart EventType = "turn.start"
	// EvMessageDelta carries streamed assistant text.
	EvMessageDelta EventType = "message.delta"
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
	// Usage is set on usage-bearing events.
	Usage *Usage
}
