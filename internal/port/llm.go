// Package port — cycle note: LLMRequest references tool.ToolSpec and
// prompt.Layered, so port imports tool and prompt (and session, governance).
// FileSystem/Workspace deliberately live in internal/tool, not here, to avoid a
// port↔tool import cycle (Tool.Execute takes a Workspace).
package port

import (
	"context"
	"iter"

	"github.com/stacklok/mecatl/internal/prompt"
	"github.com/stacklok/mecatl/internal/session"
	"github.com/stacklok/mecatl/internal/tool"
)

// LLMRequest is the provider-neutral input to a model call. System is the
// two-layer system prompt (stable prefix + volatile suffix for cache
// breakpoints); Tools are the schemas, stable across turns for caching.
type LLMRequest struct {
	// System is the layered system prompt.
	System prompt.Layered
	// Messages is the conversation history to send.
	Messages []session.Message
	// Tools are the tool schemas the model may call.
	Tools []tool.ToolSpec
	// Model is the provider model identifier.
	Model string
}

// ChunkKind is the kind of a streamed Chunk.
type ChunkKind int

const (
	// ChunkText is an assistant text delta.
	ChunkText ChunkKind = iota
	// ChunkReasoning is a human-readable reasoning summary delta. It is
	// DISPLAY-ONLY: clients render it for visibility into the model's thinking;
	// it is NOT the blob replayed to the provider. (See ChunkReasoningItem.)
	ChunkReasoning
	// ChunkReasoningItem carries the provider's opaque reasoning REPLAY blob
	// (OpenAI's reasoning-item encrypted_content), emitted once the reasoning
	// output item is assembled. The Text field holds the encrypted blob, which
	// the loop stores on Message.Reasoning and the adapter sends back verbatim on
	// subsequent stateless calls. It is never displayed or interpreted.
	ChunkReasoningItem
	// ChunkToolCall is a fully-assembled tool call, emitted once complete.
	ChunkToolCall
	// ChunkUsage is the terminal usage/cache accounting.
	ChunkUsage
	// ChunkDone is the end of stream; it carries the StopReason.
	ChunkDone
)

// Chunk is a single provider-neutral unit of a model stream. The loop assembles
// a sequence of Chunks into a domain Message. The OpenAI Responses specifics
// (function_call items, reasoning items, SSE framing, cache accounting) live
// entirely inside the openai adapter; the loop never sees a provider type.
type Chunk struct {
	// Kind discriminates the payload.
	Kind ChunkKind
	// Text carries the assistant text on ChunkText, the human-readable reasoning
	// summary on ChunkReasoning (display-only), and the opaque reasoning replay
	// blob on ChunkReasoningItem (encrypted_content, never displayed).
	Text string
	// ToolCall is set on ChunkToolCall.
	ToolCall *session.ToolCall
	// Usage is set on ChunkUsage.
	Usage *session.Usage
	// Stop is set on ChunkDone.
	Stop session.StopReason
}

// LLMProvider is the provider-agnostic seam for model calls. Stream yields
// provider-neutral chunks until ctx is cancelled or the model stops; ctx
// cancellation is how the API "cancel" verb interrupts an in-flight turn. The
// returned iter.Seq2 yields (Chunk, error) pairs; a non-nil error terminates the
// stream. The outer error reports a failure to start the stream.
type LLMProvider interface {
	Stream(ctx context.Context, req LLMRequest) (iter.Seq2[Chunk, error], error)
}
