package governance

import "encoding/json"

// HookPhase identifies the lifecycle point at which a hook fires. The run-level
// trio (SessionStart, UserPromptSubmit, Stop) fires from engine/agent/hooks.go,
// the per-tool pair (PreToolUse, PostToolUse) from engine/agent/dispatch.go, and
// SubagentStop from the Subagent tool. The agent-team trio (TeammateIdle,
// TaskCreated, TaskCompleted) fires from the team supervisor and coordination tools
// (engine/agent/teamsupervisor.go, teamtools.go).
type HookPhase string

const (
	// PhaseSessionStart fires once when a session begins.
	PhaseSessionStart HookPhase = "SessionStart"
	// PhaseUserPromptSubmit fires when the user submits a prompt.
	PhaseUserPromptSubmit HookPhase = "UserPromptSubmit"
	// PhasePreToolUse fires before a tool executes; a block aborts the call.
	PhasePreToolUse HookPhase = "PreToolUse"
	// PhasePostToolUse fires after a tool executes.
	PhasePostToolUse HookPhase = "PostToolUse"
	// PhaseStop fires when the main loop stops.
	PhaseStop HookPhase = "Stop"
	// PhaseSubagentStop fires when a subagent loop stops.
	PhaseSubagentStop HookPhase = "SubagentStop"
	// PhaseTeammateIdle fires when an agent-team member goes idle after a turn
	// (best-effort notification; the team lead can use it to detect quiescence).
	PhaseTeammateIdle HookPhase = "TeammateIdle"
	// PhaseTaskCreated fires before a team task is created; a Block vetoes the
	// creation (a quality gate on what work is allowed onto the shared list).
	PhaseTaskCreated HookPhase = "TaskCreated"
	// PhaseTaskCompleted fires before a team task is marked complete; a Block
	// vetoes the completion (a quality gate, e.g. "tests must pass first").
	PhaseTaskCompleted HookPhase = "TaskCompleted"
)

// HookEvent is the payload delivered to a hook. It is provider-neutral and
// JSON-serialized to the hook process's stdin by the HookRunner adapter.
type HookEvent struct {
	// Phase is the lifecycle point this event fires at.
	Phase HookPhase
	// Tool is the tool name for tool-use phases (empty otherwise).
	Tool string
	// Input is the phase-specific payload (e.g. the tool-call arguments).
	Input json.RawMessage
	// SessionID is the session this event belongs to.
	SessionID string
}

// HookOutcome is the result of running a hook. The exec adapter maps process
// exit code 0 to allow and exit code 2 to block (Block == true).
type HookOutcome struct {
	// Block reports whether the hook vetoed the action (exit code 2).
	Block bool
	// Message is the hook's explanation, surfaced to the model/client.
	Message string
	// Mutated, when non-empty, replaces the action's input payload, interpreted
	// symmetrically with the phase's HookEvent.Input. The loop applies it for:
	//   - UserPromptSubmit: rewrites the effective prompt ({"prompt": ...}) before
	//     the message is recorded and sent to the model;
	//   - PreToolUse: rewrites the tool call's arguments JSON before execution,
	//     preserving the CallID and tool Name;
	//   - PostToolUse: rewrites the tool result ({"content", "is_error"}) before it
	//     is emitted and recorded, preserving the CallID (redact/transform output).
	// A malformed (non-JSON) payload is ignored by the loop (the original payload
	// stands). NOTE for PreToolUse: the permission policy has already been
	// evaluated on the ORIGINAL, pre-mutation args; the mutated args are NOT
	// re-permission-checked, reflecting that a hook is more trusted than the model.
	// NOTE for PostToolUse: the loop emits the EFFECTIVE (rewritten) result, so the
	// client stream and the model's recorded history agree — no hidden divergence.
	Mutated json.RawMessage
}
