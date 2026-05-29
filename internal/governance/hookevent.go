package governance

import "encoding/json"

// HookPhase identifies the lifecycle point at which a hook fires. v1 implements
// PreToolUse and PostToolUse; the remaining phases are designed in for later.
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
	// Mutated, when non-nil, replaces the action's input payload (e.g. a hook
	// rewriting tool arguments before execution).
	Mutated json.RawMessage
}
