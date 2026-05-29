package session

import "encoding/json"

// ToolCallID uniquely identifies a tool invocation within a session. It is
// produced by the LLM and used to pair a ToolCall with its ToolResult.
type ToolCallID string

// ToolCall is an immutable value object: a request from the model to invoke a
// named tool with tool-specific arguments. It is produced by the LLM provider
// and consumed by both the Tooling and Governance contexts. Construct it with
// NewToolCall; it carries no mutating methods.
type ToolCall struct {
	// ID pairs this call with its ToolResult.
	ID ToolCallID
	// Name is the tool name as registered in the catalog.
	Name string
	// Args is the raw, tool-specific argument payload, validated against the
	// tool's JSON schema by the Tool itself.
	Args json.RawMessage
}

// NewToolCall constructs a ToolCall value object.
func NewToolCall(id ToolCallID, name string, args json.RawMessage) ToolCall {
	return ToolCall{ID: id, Name: name, Args: args}
}

// ToolResult is an immutable value object: the outcome of executing a ToolCall,
// paired to it by CallID. Construct it with NewToolResult or NewToolError; it
// carries no mutating methods.
type ToolResult struct {
	// CallID is the ID of the ToolCall this result answers.
	CallID ToolCallID
	// Content is the result body, already token-shaped/truncated by the tool.
	Content string
	// IsError reports whether the tool failed; an error result is still fed
	// back to the model so it can recover.
	IsError bool
}

// NewToolResult constructs a successful ToolResult for the given call.
func NewToolResult(callID ToolCallID, content string) ToolResult {
	return ToolResult{CallID: callID, Content: content}
}

// NewToolError constructs an error ToolResult for the given call.
func NewToolError(callID ToolCallID, content string) ToolResult {
	return ToolResult{CallID: callID, Content: content, IsError: true}
}
