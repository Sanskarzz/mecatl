// Package mockllm provides a deterministic, scripted implementation of
// port.LLMProvider for testing the agent loop without any network access.
//
// A Provider is constructed from an ordered list of "turns"; each turn is a
// canned sequence of port.Chunk values. Every call to Stream emits the next
// turn's chunks and advances an internal cursor, so successive model calls in a
// loop replay successive scripted turns. Turn-builder helpers (TextTurn,
// ToolCallTurn, ...) make scripts terse:
//
//	p := mockllm.New(
//		mockllm.TextTurn("hello"),
//		mockllm.ToolCallTurn(call),
//	)
//
// Stream honours context cancellation: if ctx is done it stops yielding.
package mockllm

import (
	"context"
	"iter"
	"sync"

	"github.com/stacklok/mecatl/internal/port"
	"github.com/stacklok/mecatl/internal/session"
)

// Turn is one scripted model response: the ordered chunks Stream emits for a
// single call. Build one with the Turn* helpers or assemble Chunks by hand.
type Turn struct {
	// Chunks is the ordered sequence emitted for this turn.
	Chunks []port.Chunk
}

// Provider is a deterministic, scripted port.LLMProvider. Each Stream call
// replays the next programmed Turn. It is safe for concurrent use; the cursor is
// guarded by a mutex.
type Provider struct {
	mu     sync.Mutex
	turns  []Turn
	cursor int
	caps   port.ProviderCapabilities
}

// Option configures a Provider.
type Option func(*Provider)

// WithCapabilities sets the capabilities the mock advertises. The default is
// text-only (the zero ProviderCapabilities). Tests use it to flip the mock to
// image- or audio-capable without reaching for the OpenAI adapter.
func WithCapabilities(caps port.ProviderCapabilities) Option {
	return func(p *Provider) { p.caps = caps }
}

// New constructs a Provider that replays the given turns in order, one per
// Stream call. It advertises text-only capabilities unless NewWith is used.
func New(turns ...Turn) *Provider {
	return &Provider{turns: turns}
}

// NewWith constructs a Provider with the given options (e.g. WithCapabilities)
// and scripted turns.
func NewWith(opts []Option, turns ...Turn) *Provider {
	p := &Provider{turns: turns}
	for _, o := range opts {
		o(p)
	}
	return p
}

// Capabilities reports the configured capabilities (text-only by default).
func (p *Provider) Capabilities() port.ProviderCapabilities {
	return p.caps
}

// Calls reports how many times Stream has been invoked (i.e. the current cursor
// position). It is useful for assertions in tests.
func (p *Provider) Calls() int {
	p.mu.Lock()
	defer p.mu.Unlock()
	return p.cursor
}

// Reset rewinds the cursor to the first turn, so the same Provider can be
// replayed again.
func (p *Provider) Reset() {
	p.mu.Lock()
	defer p.mu.Unlock()
	p.cursor = 0
}

// Stream returns an iterator over the next scripted turn's chunks, advancing the
// internal cursor. If the script is exhausted it returns an empty iterator (no
// chunks, no error). The returned iterator stops early if ctx is cancelled. The
// outer error is always nil; the mock never fails to start a stream.
func (p *Provider) Stream(ctx context.Context, _ port.LLMRequest) (iter.Seq2[port.Chunk, error], error) {
	p.mu.Lock()
	var chunks []port.Chunk
	if p.cursor < len(p.turns) {
		chunks = p.turns[p.cursor].Chunks
		p.cursor++
	}
	p.mu.Unlock()

	return func(yield func(port.Chunk, error) bool) {
		for _, c := range chunks {
			select {
			case <-ctx.Done():
				return
			default:
			}
			if !yield(c, nil) {
				return
			}
		}
	}, nil
}

// Compile-time assertion that Provider satisfies the port.
var _ port.LLMProvider = (*Provider)(nil)

// --- Turn builders ---------------------------------------------------------

// TextTurn builds a turn that streams text as a single text delta, then a usage
// chunk (zero usage) and a ChunkDone carrying StopEndTurn. It is the common
// "model answered with prose and stopped" case.
func TextTurn(text string) Turn {
	return Turn{Chunks: []port.Chunk{
		{Kind: port.ChunkText, Text: text},
		{Kind: port.ChunkUsage, Usage: &session.Usage{}},
		{Kind: port.ChunkDone, Stop: session.StopEndTurn},
	}}
}

// ToolCallTurn builds a turn that emits one or more fully-assembled tool calls,
// then a usage chunk (zero usage) and a ChunkDone carrying StopEndTurn (the loop
// continues because tool calls were produced). At least one call is expected;
// calling it with none yields a turn that just stops.
func ToolCallTurn(calls ...session.ToolCall) Turn {
	chunks := make([]port.Chunk, 0, len(calls)+2)
	for i := range calls {
		c := calls[i]
		chunks = append(chunks, port.Chunk{Kind: port.ChunkToolCall, ToolCall: &c})
	}
	chunks = append(chunks,
		port.Chunk{Kind: port.ChunkUsage, Usage: &session.Usage{}},
		port.Chunk{Kind: port.ChunkDone, Stop: session.StopEndTurn},
	)
	return Turn{Chunks: chunks}
}

// ReasoningTurn builds a turn that emits a reasoning delta and then text,
// followed by a zero usage chunk and a ChunkDone carrying StopEndTurn.
func ReasoningTurn(reasoning, text string) Turn {
	return Turn{Chunks: []port.Chunk{
		{Kind: port.ChunkReasoning, Text: reasoning},
		{Kind: port.ChunkText, Text: text},
		{Kind: port.ChunkUsage, Usage: &session.Usage{}},
		{Kind: port.ChunkDone, Stop: session.StopEndTurn},
	}}
}

// ChunksTurn wraps an explicit chunk sequence as a Turn for full control over
// the script (e.g. custom usage, a specific StopReason, or interleaved kinds).
func ChunksTurn(chunks ...port.Chunk) Turn {
	return Turn{Chunks: chunks}
}

// TextChunk builds a ChunkText.
func TextChunk(text string) port.Chunk { return port.Chunk{Kind: port.ChunkText, Text: text} }

// ReasoningChunk builds a ChunkReasoning (human-readable DISPLAY summary).
func ReasoningChunk(text string) port.Chunk {
	return port.Chunk{Kind: port.ChunkReasoning, Text: text}
}

// ReasoningItemChunk builds a ChunkReasoningItem carrying the opaque REPLAY blob
// (the analogue of OpenAI's reasoning-item encrypted_content).
func ReasoningItemChunk(blob string) port.Chunk {
	return port.Chunk{Kind: port.ChunkReasoningItem, Text: blob}
}

// ToolCallChunk builds a ChunkToolCall.
func ToolCallChunk(call session.ToolCall) port.Chunk {
	return port.Chunk{Kind: port.ChunkToolCall, ToolCall: &call}
}

// UsageChunk builds a ChunkUsage.
func UsageChunk(u session.Usage) port.Chunk {
	return port.Chunk{Kind: port.ChunkUsage, Usage: &u}
}

// DoneChunk builds a ChunkDone carrying the given stop reason.
func DoneChunk(stop session.StopReason) port.Chunk {
	return port.Chunk{Kind: port.ChunkDone, Stop: stop}
}
