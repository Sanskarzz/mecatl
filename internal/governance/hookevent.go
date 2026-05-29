package governance

import "encoding/json"

// HookPhase identifies the lifecycle point at which a hook fires. All six
// phases below fire: the run-level trio (SessionStart, UserPromptSubmit, Stop)
// from internal/agent/hooks.go, the per-tool pair (PreToolUse, PostToolUse)
// from internal/agent/dispatch.go, and SubagentStop from the Task subagent.
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
	// symmetrically with the phase's HookEvent.Input. The loop applies it for the
	// UserPromptSubmit phase, where it rewrites the effective prompt ({"prompt":
	// ...}) before the message is recorded and sent to the model. For tool phases
	// (PreToolUse) the field is the rewrite-tool-arguments seam; it is not yet
	// applied there.
	Mutated json.RawMessage
}
