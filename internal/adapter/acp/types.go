package acp

import "encoding/json"

// This file defines the ACP wire types this adapter (de)serializes. They are the
// adapter's OWN JSON shapes — deliberately NOT the proto contract (contracts/gen)
// — mirroring the relevant subset of the ACP schema
// (github.com/zed-industries/agent-client-protocol). Only the Phase 1 fields are
// modelled; unmodelled fields are simply not emitted (ACP treats absent optional
// fields as defaults).

// protocolVersion is the ACP protocol version this adapter implements. ACP bumps
// it only for breaking changes; capabilities cover the rest.
const protocolVersion = 1

// ACP JSON-RPC method names.
const (
	methodInitialize        = "initialize"
	methodSessionNew        = "session/new"
	methodSessionPrompt     = "session/prompt"
	methodSessionCancel     = "session/cancel"
	methodSessionUpdate     = "session/update"             // agent -> client notification
	methodRequestPermission = "session/request_permission" // agent -> client request
)

// --- initialize --------------------------------------------------------------

type initializeRequest struct {
	ProtocolVersion    int             `json:"protocolVersion"`
	ClientCapabilities json.RawMessage `json:"clientCapabilities,omitempty"`
	ClientInfo         *implementation `json:"clientInfo,omitempty"`
}

type initializeResponse struct {
	ProtocolVersion   int               `json:"protocolVersion"`
	AgentCapabilities agentCapabilities `json:"agentCapabilities"`
	AuthMethods       []any             `json:"authMethods"`
	AgentInfo         *implementation   `json:"agentInfo,omitempty"`
}

type implementation struct {
	Name    string `json:"name"`
	Version string `json:"version,omitempty"`
}

type agentCapabilities struct {
	LoadSession        bool               `json:"loadSession"`
	McpCapabilities    mcpCapabilities    `json:"mcpCapabilities"`
	PromptCapabilities promptCapabilities `json:"promptCapabilities"`
}

// mcpCapabilities advertises which MCP transports the agent accepts from the
// client's session/new mcpServers. mecatl is streaming-HTTP MCP ONLY (see
// CLAUDE.md: "No stdio MCP, ever"), so http is the only transport we could ever
// accept; sse is false. This phase rejects all client-provided MCP servers (see
// session/new handling), so both are conservatively advertised false until that
// is wired in a later phase.
type mcpCapabilities struct {
	HTTP bool `json:"http"`
	SSE  bool `json:"sse"`
}

type promptCapabilities struct {
	Audio           bool `json:"audio"`
	EmbeddedContext bool `json:"embeddedContext"`
	Image           bool `json:"image"`
}

// --- session/new -------------------------------------------------------------

type newSessionRequest struct {
	Cwd        string      `json:"cwd"`
	McpServers []mcpServer `json:"mcpServers"`
}

// mcpServer is a client-provided MCP server entry. We model only the transport
// discriminant fields enough to REJECT a stdio server (no URL) this phase.
type mcpServer struct {
	Name    string          `json:"name"`
	Command string          `json:"command,omitempty"`
	URL     string          `json:"url,omitempty"`
	Type    string          `json:"type,omitempty"`
	Raw     json.RawMessage `json:"-"`
}

type newSessionResponse struct {
	SessionID string            `json:"sessionId"`
	Modes     *sessionModeState `json:"modes,omitempty"`
}

// sessionModeState reflects mecatl's permission modes (default/plan/acceptEdits)
// to the client. The current mode is always "default" on a fresh session this
// phase (mode switching is deferred).
type sessionModeState struct {
	CurrentModeID  string        `json:"currentModeId"`
	AvailableModes []sessionMode `json:"availableModes"`
}

type sessionMode struct {
	ID          string `json:"id"`
	Name        string `json:"name"`
	Description string `json:"description,omitempty"`
}

// --- session/prompt ----------------------------------------------------------

type promptRequest struct {
	SessionID string         `json:"sessionId"`
	Prompt    []contentBlock `json:"prompt"`
}

type promptResponse struct {
	StopReason string `json:"stopReason"`
}

// ACP stop reasons (PromptResponse.stopReason).
const (
	stopEndTurn         = "end_turn"
	stopMaxTokens       = "max_tokens"
	stopMaxTurnRequests = "max_turn_requests"
	stopRefusal         = "refusal"
	stopCancelled       = "cancelled"
)

// --- session/cancel ----------------------------------------------------------

type cancelNotification struct {
	SessionID string `json:"sessionId"`
}

// --- content blocks ----------------------------------------------------------

// contentBlock is the ACP ContentBlock. This phase models only the text variant
// (the baseline every agent must support); other variants decode with Type set
// and Text empty, so a non-text block contributes no text when flattened.
type contentBlock struct {
	Type string `json:"type"`
	Text string `json:"text,omitempty"`
}

// textBlock constructs a text ContentBlock for an outbound chunk.
func textBlock(text string) contentBlock { return contentBlock{Type: "text", Text: text} }

// --- session/update (agent -> client) ----------------------------------------

// sessionNotification is the session/update params envelope. Update holds one of
// the union variants (chunkUpdate or toolCallUpdate) — the variants carry
// DIFFERENT "content" shapes (a single ContentBlock for the *_chunk variants vs a
// []ToolCallContent for the tool_call variants), which a single Go struct cannot
// express on one JSON tag, so Update is `any` and the projector supplies the
// correct purpose-built variant value (each marshals the right shape).
type sessionNotification struct {
	SessionID string `json:"sessionId"`
	Update    any    `json:"update"`
}

// session/update "sessionUpdate" discriminator const values.
const (
	updateAgentMessageChunk = "agent_message_chunk"
	updateAgentThoughtChunk = "agent_thought_chunk"
	updateToolCall          = "tool_call"
	updateToolCallUpdate    = "tool_call_update"
)

// chunkUpdate is the *_chunk session/update variant (a single ContentBlock).
type chunkUpdate struct {
	SessionUpdate string       `json:"sessionUpdate"`
	Content       contentBlock `json:"content"`
}

// toolCallUpdate is the tool_call / tool_call_update session/update variant. It
// is also the shape embedded in a request_permission's toolCall field.
type toolCallUpdate struct {
	SessionUpdate string            `json:"sessionUpdate,omitempty"`
	ToolCallID    string            `json:"toolCallId"`
	Title         string            `json:"title,omitempty"`
	Kind          string            `json:"kind,omitempty"`
	Status        string            `json:"status,omitempty"`
	RawInput      json.RawMessage   `json:"rawInput,omitempty"`
	Content       []toolCallContent `json:"content,omitempty"`
}

// ACP ToolCallStatus values.
const (
	toolStatusPending    = "pending"
	toolStatusInProgress = "in_progress"
	toolStatusCompleted  = "completed"
	toolStatusFailed     = "failed"
)

// toolCallContent is the ToolCallContent union. Two variants are emitted:
//   - the "content" variant wraps a text ContentBlock (results, progress lines);
//   - the "diff" variant carries a file path + old/new text so the editor renders
//     a native inline diff for an Edit/Write (Phase 2).
//
// The two variants carry mutually-exclusive fields, so the unused ones are
// omitempty and a single struct expresses both. (terminal is still deferred.)
type toolCallContent struct {
	Type string `json:"type"`
	// Content is set on the "content" variant only.
	Content *contentBlock `json:"content,omitempty"`
	// Path/OldText/NewText are set on the "diff" variant only. OldText is a pointer
	// so it is OMITTED for a new/overwritten file (ACP: absent oldText means the
	// file did not exist / is fully replaced) rather than serialized as "".
	Path    string  `json:"path,omitempty"`
	OldText *string `json:"oldText,omitempty"`
	NewText string  `json:"newText,omitempty"`
}

// textToolContent wraps result text as a single ToolCallContent of the content
// variant.
func textToolContent(text string) []toolCallContent {
	cb := textBlock(text)
	return []toolCallContent{{Type: "content", Content: &cb}}
}

// diffToolContent builds a single "diff" ToolCallContent. oldText is a pointer so
// the caller can omit it (nil) for a new/overwritten file.
func diffToolContent(path string, oldText *string, newText string) []toolCallContent {
	return []toolCallContent{{Type: "diff", Path: path, OldText: oldText, NewText: newText}}
}

// --- session/request_permission (agent -> client) ----------------------------

type requestPermissionRequest struct {
	SessionID string             `json:"sessionId"`
	ToolCall  toolCallUpdate     `json:"toolCall"`
	Options   []permissionOption `json:"options"`
}

type permissionOption struct {
	OptionID string `json:"optionId"`
	Name     string `json:"name"`
	Kind     string `json:"kind"`
}

// ACP PermissionOptionKind values.
const (
	permAllowOnce    = "allow_once"
	permAllowAlways  = "allow_always"
	permRejectOnce   = "reject_once"
	permRejectAlways = "reject_always"
)

type requestPermissionResponse struct {
	Outcome permissionOutcome `json:"outcome"`
}

// permissionOutcome is the discriminated outcome (discriminator: "outcome").
// For "selected" OptionID names the chosen option; for "cancelled" it is empty.
type permissionOutcome struct {
	Outcome  string `json:"outcome"`
	OptionID string `json:"optionId,omitempty"`
}

const (
	outcomeSelected  = "selected"
	outcomeCancelled = "cancelled"
)
