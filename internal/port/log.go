package port

import (
	"time"

	"github.com/stacklok/ozzharness/internal/session"
)

// EventSink receives domain Events from the loop and relays them to the API
// stream (gRPC server-stream / HTTP SSE).
type EventSink interface {
	// Emit publishes a single Event. Implementations must not block the loop
	// indefinitely.
	Emit(ev session.Event)
}

// Logger records structured observability for tool execution. It is an
// observability seam, distinct from the model-visible conversation.
type Logger interface {
	// ToolCall records that a tool was executed, with its result and the wall
	// time it took.
	ToolCall(id session.SessionID, call session.ToolCall, result session.ToolResult, took time.Duration)
}
