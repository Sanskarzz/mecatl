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

	// EvTeamStart is emitted when a Team tool run begins. It is a BOUNDED
	// observability projection of an in-process team — it carries the parent call
	// id, the team id, and the roster the model formed (member names/roles, never
	// member content). See TeamPayload for the redaction contract.
	EvTeamStart EventType = "team.start"
	// EvTeamMember is emitted for each forwarded member-session event during a Team
	// run. Unlike the metadata-only subagent.tool projection, it is deliberately
	// FULLER — a team is meant to be watched — so it carries the member's message
	// text and BOUNDED tool-call/result previews, tagged by member name. It ALWAYS
	// sets Member (the member whose activity it projects) and InnerKind (that
	// member's underlying session event type). It is STILL bounded and redacted:
	// every preview is capped, and a member's permission.ask is DROPPED entirely
	// (never forwarded). See TeamPayload.
	EvTeamMember EventType = "team.member"
	// EvTeamTasks is emitted when the team's SHARED TASK LIST changes during a Team
	// run (and as a terminal snapshot on EvTeamEnd's payload). It is a team-WIDE
	// projection — NOT per-member — so it carries no Member; only TeamPayload.Tasks
	// (the id/state/assignee/deps snapshot in creation order). It is the discriminant
	// the client routes to the ctrl+a agents task sub-view. Snapshots are emitted
	// only on change (de-duped) to bound wire volume.
	EvTeamTasks EventType = "team.tasks"
	// EvTeamEnd is emitted when a Team run terminates. It is a BOUNDED projection
	// carrying only aggregate metadata — the number of rounds, the stop reason, and
	// the team's cumulative usage — never member content. The team's joined summary
	// folds back into the parent conversation exclusively via the Team tool's
	// ToolResult.
	EvTeamEnd EventType = "team.end"
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
	// CallID is the id of the tool call this hook fired against, for the per-tool
	// phases (PreToolUse / PostToolUse); empty for non-tool phases (e.g. Stop,
	// SessionStart) and for tool phases where no call is in scope. It lets a client
	// address the hook notice to the originating tool card (e.g. mark that exact
	// tool_call as failed) instead of falling back to a free-standing note.
	CallID ToolCallID
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

// TeamMemberSpec is one roster entry forwarded on EvTeamStart: the member name,
// its role label, and the read-only/mutating and lead flags. It is a small value
// type carrying ONLY model-supplied metadata about the team's shape — never any
// member content (no prompt body, no transcript). It mirrors the proto
// TeamMemberSpec.
type TeamMemberSpec struct {
	// Name is the member's unique handle.
	Name string
	// Role is the member's short role label (the model-supplied role string).
	Role string
	// Mutating reports whether the member runs in an isolated fork with
	// workspace-mutating tools (true) or shares the base read-only (false).
	Mutating bool
	// Lead marks the coordinating member.
	Lead bool
}

// TeamTaskSnapshot is one entry in the team's shared task list, projected onto the
// event stream so the ctrl+a agents task sub-view can render the team's task state
// (id · state · assignee · deps) without an out-of-band ListTeam RPC — the team is
// a Team-tool-local object the TUI cannot address. It is a plain value type
// mirroring the proto TeamTask; it carries only task metadata (no member content).
// Deps are the task ids this task depends on (it is blocked until they complete).
type TeamTaskSnapshot struct {
	// ID is the stable task identifier.
	ID string
	// Description is a BOUNDED preview of the work to do (capped like every other
	// member-derived preview).
	Description string
	// State mirrors team.TaskState: "pending" / "in_progress" / "completed".
	State string
	// Assignee is the member name that claimed the task, or empty if unclaimed.
	Assignee string
	// Deps lists the task ids that must complete before this task is claimable.
	Deps []string
}

// TeamPayload is the BOUNDED observability projection carried by the team.* events
// (EvTeamStart / EvTeamMember / EvTeamTasks / EvTeamEnd). It is the ONLY information
// about an in-process team's run that surfaces to clients on the event stream.
//
// REDACTION CONTRACT — fuller-but-bounded. Unlike SubagentPayload (metadata only),
// a team is meant to be WATCHED, so this payload deliberately forwards member
// CONTENT on team.member events: the member's streamed/terminal message text and
// BOUNDED previews of its tool calls (name + capped arg preview) and tool results
// (error bool + capped body preview). Every such preview is CAPPED (see
// maxTeamPreview) so an unbounded args/result body can never be copied verbatim,
// and a member's permission.ask is DROPPED entirely — it is NEVER forwarded, so a
// pending-ask reason (which can quote secrets or sensitive args) never reaches the
// stream. This forwarding is orthogonal to the parent conversation: the team's
// per-member transcripts NEVER enter the parent Session's Conversation; only the
// Team tool's joined-summary ToolResult does. So the LLM's context still sees only
// the summary, exactly like Task/Fork.
//
// Which fields are set depends on the event kind:
//   - EvTeamStart:  ParentCallID, TeamID, Roster.
//   - EvTeamMember: ParentCallID, TeamID, Member, InnerKind, and the subset of
//     {Text, ToolName, Detail, IsError, Usage, ContextUsed, ContextWindow}
//     relevant to InnerKind.
//   - EvTeamTasks:  ParentCallID, TeamID, Tasks (the team-wide task snapshot; no
//     Member).
//   - EvTeamEnd:    ParentCallID, TeamID, Rounds, Stop, Usage (cumulative), Tasks
//     (the terminal task snapshot).
type TeamPayload struct {
	// ParentCallID is the parent's Team tool-call id, attributing every team.*
	// event to the originating Team card. Set on all three kinds.
	ParentCallID string
	// TeamID is the team id, distinguishing concurrent teams. Set on all kinds.
	TeamID string
	// Roster is the team's membership as the model formed it. Set on EvTeamStart
	// only. It carries only member metadata, never member content.
	Roster []TeamMemberSpec
	// Member is the name of the member whose activity this event projects. Set on
	// EvTeamMember only.
	Member string
	// InnerKind is the member's underlying session event kind being projected
	// (e.g. "message.delta", "tool.call", "tool.result", "turn.end", "result").
	// Set on EvTeamMember only. permission.ask is never projected.
	InnerKind EventType
	// Text is the member's message/result text or a BOUNDED preview of it. Set on
	// EvTeamMember for message.delta / result inner kinds.
	Text string
	// ToolName is the name of a member tool that was called. Set on EvTeamMember
	// for tool.call / tool.result inner kinds.
	ToolName string
	// Detail is a BOUNDED preview of a member tool call's args (tool.call) or
	// result body (tool.result) — capped at maxTeamPreview runes. It is never the
	// raw, unbounded args/result body. Set on EvTeamMember for tool.* inner kinds.
	Detail string
	// IsError reports whether a member tool.result failed. Set on EvTeamMember for
	// the tool.result inner kind.
	IsError bool
	// Rounds is the number of scheduling rounds that ran work. Set on EvTeamEnd
	// only.
	Rounds int
	// Stop is the team run's terminal stop reason. Set on EvTeamEnd only.
	Stop StopReason
	// Usage is the member's per-event usage (EvTeamMember turn.end/result) or, on
	// EvTeamEnd, the TEAM TOTAL — the sum of every member's per-turn usage.
	Usage Usage
	// ContextUsed is the member's CURRENT context occupancy — the most recent
	// turn's input-token count (Usage.InputTokens of the turn just ended), i.e.
	// what the next turn would carry into the model, not a cumulative sum. Set on
	// EvTeamMember turn.end; 0 when unknown. It feeds the per-member context meter
	// in the ctrl+a agents overlay (the team analogue of the main context meter).
	ContextUsed int64
	// ContextWindow is the producing member engine's context window in tokens (the
	// meter's denominator). Set on EvTeamMember turn.end; 0 when unknown (no meter
	// is drawn in that case).
	ContextWindow int64
	// Tasks is a snapshot of the team's SHARED TASK LIST in creation order. It is
	// set on an EvTeamTasks event (emitted on change, de-duped, from the Team tool's
	// member-event sink) and on EvTeamEnd (the terminal snapshot, so the final task
	// state always lands). It feeds the ctrl+a agents task sub-view; it carries only
	// task metadata, never member content.
	Tasks []TeamTaskSnapshot
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
	// Team is set on the team.* events (start / member / tasks / end): the BOUNDED
	// observability projection of an in-process team run (fuller-but-bounded; member
	// content is capped and permission.ask is dropped, and never enters the parent
	// conversation).
	Team *TeamPayload
}
