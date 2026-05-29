package openai

import (
	"bufio"
	"encoding/json"
	"io"
	"strings"

	"github.com/openai/openai-go/v3/responses"

	"github.com/stacklok/ozzharness/internal/port"
	"github.com/stacklok/ozzharness/internal/session"
)

// streamState carries the small amount of state the translation needs across
// events. The OpenAI Responses SSE stream is semantic and mostly self-describing
// per event, so the only carried state is the assembled final response (for the
// terminal usage/stop), threaded via the events themselves.
type streamState struct {
	// done guards against emitting a second ChunkDone if both response.completed
	// and a later terminal event arrive.
	done bool
}

// translate converts a single Responses SSE event into zero or more
// provider-neutral chunks. It is a pure function (apart from the small carried
// streamState) so it can be driven directly from recorded fixtures in tests,
// with no real client.
//
// Mapping:
//   - response.output_text.delta            -> ChunkText (event.Delta)
//   - response.reasoning_summary_text.delta -> ChunkReasoning (event.Delta)
//   - response.output_item.done (function_call) -> ChunkToolCall
//   - response.completed                    -> ChunkUsage then ChunkDone(end_turn)
//   - response.failed / response.incomplete -> ChunkDone(error)
//   - error                                 -> ChunkDone(error)
func translate(event responses.ResponseStreamEventUnion, st *streamState) []port.Chunk {
	switch event.Type {
	case "response.output_text.delta":
		if event.Delta == "" {
			return nil
		}
		return []port.Chunk{{Kind: port.ChunkText, Text: event.Delta}}

	case "response.reasoning_summary_text.delta":
		if event.Delta == "" {
			return nil
		}
		return []port.Chunk{{Kind: port.ChunkReasoning, Text: event.Delta}}

	case "response.output_item.done":
		// The assembled function_call carries call_id, name, and the final
		// arguments JSON string. Act on the .done payload, not concatenated
		// deltas (per the brief's assembly rule).
		item := event.Item
		if item.Type != "function_call" {
			return nil
		}
		call := session.ToolCall{
			ID:   session.ToolCallID(item.CallID),
			Name: item.Name,
			Args: json.RawMessage(item.Arguments.OfString),
		}
		return []port.Chunk{{Kind: port.ChunkToolCall, ToolCall: &call}}

	case "response.completed":
		if st.done {
			return nil
		}
		st.done = true
		usage := mapUsage(event.Response.Usage)
		return []port.Chunk{
			{Kind: port.ChunkUsage, Usage: &usage},
			{Kind: port.ChunkDone, Stop: mapStop(event.Response.Status)},
		}

	case "response.failed", "response.incomplete", "error":
		if st.done {
			return nil
		}
		st.done = true
		return []port.Chunk{{Kind: port.ChunkDone, Stop: session.StopError}}

	default:
		return nil
	}
}

// mapUsage maps Responses usage accounting into the domain Usage, including the
// cached-tokens subset into CacheReadTokens.
func mapUsage(u responses.ResponseUsage) session.Usage {
	return session.Usage{
		InputTokens:     int(u.InputTokens),
		OutputTokens:    int(u.OutputTokens),
		CacheReadTokens: int(u.InputTokensDetails.CachedTokens),
	}
}

// mapStop maps a terminal Responses status to a domain StopReason. A completed
// response is reported as StopEndTurn; the loop decides whether tool calls in the
// turn mean it should continue.
func mapStop(status responses.ResponseStatus) session.StopReason {
	switch status {
	case responses.ResponseStatusCompleted:
		return session.StopEndTurn
	case responses.ResponseStatusIncomplete, responses.ResponseStatusFailed:
		return session.StopError
	case responses.ResponseStatusCancelled:
		return session.StopCancelled
	default:
		return session.StopEndTurn
	}
}

// decodeSSE reads an SSE byte stream (the wire form of a Responses streaming
// response) and translates it into a flat slice of chunks, applying translate to
// each event in order. It exists so tests can drive the exact translation path
// from a recorded golden fixture without a real client. Lines are parsed as
// "data: <json>" records separated by blank lines; "event:" lines are ignored
// because the event JSON carries its own "type".
func decodeSSE(r io.Reader) ([]port.Chunk, error) {
	var out []port.Chunk
	var st streamState
	sc := bufio.NewScanner(r)
	sc.Buffer(make([]byte, 0, 64*1024), 4*1024*1024)
	for sc.Scan() {
		line := strings.TrimRight(sc.Text(), "\r")
		if !strings.HasPrefix(line, "data:") {
			continue
		}
		data := strings.TrimSpace(strings.TrimPrefix(line, "data:"))
		if data == "" || data == "[DONE]" {
			continue
		}
		var event responses.ResponseStreamEventUnion
		if err := json.Unmarshal([]byte(data), &event); err != nil {
			return nil, err
		}
		out = append(out, translate(event, &st)...)
	}
	if err := sc.Err(); err != nil {
		return nil, err
	}
	return out, nil
}
