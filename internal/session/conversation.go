package session

// Role identifies the author of a Message in the conversation.
type Role string

const (
	// RoleSystem is the system prompt author.
	RoleSystem Role = "system"
	// RoleUser is the human/client author.
	RoleUser Role = "user"
	// RoleAssistant is the model author.
	RoleAssistant Role = "assistant"
	// RoleTool is a tool-result author.
	RoleTool Role = "tool"
)

// Message is an immutable value object: one entry in the model-visible
// conversation history. Construct it with one of the constructors below; it
// carries no mutating methods.
type Message struct {
	// Role is the author of this message.
	Role Role
	// Text is the message body (assistant text, user prompt, etc.).
	Text string
	// ToolCalls holds the tool invocations requested by an assistant message.
	ToolCalls []ToolCall
	// ToolResult holds the result carried by a tool-role message; nil otherwise.
	ToolResult *ToolResult
	// Reasoning is the provider's opaque reasoning REPLAY blob (OpenAI's
	// reasoning-item encrypted_content), replayed back verbatim on subsequent
	// calls and never interpreted or displayed by the harness. It is distinct
	// from the human-readable reasoning SUMMARY surfaced via reasoning.delta
	// events for display: that prose is never stored here.
	Reasoning string
	// Parts carries non-text media (image/audio) on a USER message; it is nil for
	// assistant/tool/system messages. Text remains the flattened text body
	// (embedded-text resources collapse into it); Parts carries only the binary or
	// URL-referenced media that rides alongside the text. Do not mutate Parts (or a
	// Part's Data) after construction.
	Parts []Content
}

// NewUserMessage constructs a user-role message.
func NewUserMessage(text string) Message {
	return Message{Role: RoleUser, Text: text}
}

// NewUserMessageWithParts constructs a user-role message carrying flattened
// text plus non-text media parts. text may be empty when parts carries the
// content; parts may be nil for a text-only message (equivalent to
// NewUserMessage).
func NewUserMessageWithParts(text string, parts []Content) Message {
	return Message{Role: RoleUser, Text: text, Parts: parts}
}

// NewSystemMessage constructs a system-role message.
func NewSystemMessage(text string) Message {
	return Message{Role: RoleSystem, Text: text}
}

// NewAssistantMessage constructs an assistant-role message carrying optional
// text, reasoning, and tool calls.
func NewAssistantMessage(text, reasoning string, calls []ToolCall) Message {
	return Message{
		Role:      RoleAssistant,
		Text:      text,
		Reasoning: reasoning,
		ToolCalls: calls,
	}
}

// NewToolMessage constructs a tool-role message carrying a single tool result.
func NewToolMessage(result ToolResult) Message {
	r := result
	return Message{Role: RoleTool, ToolResult: &r}
}

// Conversation is the model-visible message history of a session. It is an
// entity owned by the Session aggregate; mutate it only through Session methods.
type Conversation struct {
	// Messages is the ordered history sent to the model.
	Messages []Message
}

// Append adds a message to the conversation history.
func (c *Conversation) Append(m Message) {
	c.Messages = append(c.Messages, m)
}

// Len reports the number of messages in the conversation.
func (c *Conversation) Len() int {
	return len(c.Messages)
}

// Turn records one model call together with the tools it triggered. It is a
// value object summarizing a single iteration of the agent loop.
type Turn struct {
	// Index is the zero-based position of this turn in the session.
	Index int
	// Assistant is the assistant message produced by the model call.
	Assistant Message
	// Results holds the results of the tools the assistant requested.
	Results []ToolResult
	// Usage is the token accounting for this turn's model call.
	Usage Usage
}
