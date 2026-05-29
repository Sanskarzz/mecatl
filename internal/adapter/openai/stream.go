package openai

import (
	"bufio"
	"encoding/json"
	"fmt"
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
// A terminal failure event (the top-level "error" event, or a "response.failed"
// status) is reported as a non-nil error carrying the provider's human-readable
// message rather than as a bare StopError chunk: the loop surfaces a stream
// error verbatim, so the real reason ("rate_limit_exceeded: ...", "<model> is
// not a valid model ID", ...) reaches the result instead of an opaque "error".
//
// Unknown / unhandled event types (the long tail of audio, image, web/file
// search, MCP, reasoning-part, content-part, *.added / in_progress, etc.) are
// ignored: the Responses stream is forward-compatible, and treating an
// unrecognised event as fatal would break against any spec-compliant endpoint
// that emits events we do not consume.
//
// Mapping:
//   - response.output_text.delta            -> ChunkText (event.Delta)
//   - response.reasoning_summary_text.delta -> ChunkReasoning (event.Delta)
//   - response.reasoning_text.delta         -> ChunkReasoning (event.Delta)
//   - response.output_item.done (function_call) -> ChunkToolCall
//   - response.completed                    -> ChunkUsage then ChunkDone(end_turn)
//   - response.incomplete                   -> ChunkUsage then ChunkDone(error)
//   - response.failed / error               -> non-nil error (provider message)
func translate(event responses.ResponseStreamEventUnion, st *streamState) ([]port.Chunk, error) {
	switch event.Type {
	case "response.output_text.delta":
		if event.Delta == "" {
			return nil, nil
		}
		return []port.Chunk{{Kind: port.ChunkText, Text: event.Delta}}, nil

	case "response.reasoning_summary_text.delta", "response.reasoning_text.delta":
		if event.Delta == "" {
			return nil, nil
		}
		return []port.Chunk{{Kind: port.ChunkReasoning, Text: event.Delta}}, nil

	case "response.output_item.done":
		// The assembled function_call carries call_id, name, and the final
		// arguments JSON string. Act on the .done payload, not concatenated
		// deltas (per the brief's assembly rule).
		item := event.Item
		if item.Type != "function_call" {
			return nil, nil
		}
		call := session.ToolCall{
			ID:   session.ToolCallID(item.CallID),
			Name: item.Name,
			Args: json.RawMessage(item.Arguments.OfString),
		}
		return []port.Chunk{{Kind: port.ChunkToolCall, ToolCall: &call}}, nil

	case "response.completed":
		if st.done {
			return nil, nil
		}
		st.done = true
		usage := mapUsage(event.Response.Usage)
		return []port.Chunk{
			{Kind: port.ChunkUsage, Usage: &usage},
			{Kind: port.ChunkDone, Stop: mapStop(event.Response.Status)},
		}, nil

	case "response.incomplete":
		// An incomplete response still carries usage and a reason (e.g.
		// max_output_tokens, content_filter); emit usage, then stop as an error
		// with the reason so the run is diagnosable but accounted for.
		if st.done {
			return nil, nil
		}
		st.done = true
		usage := mapUsage(event.Response.Usage)
		return []port.Chunk{
			{Kind: port.ChunkUsage, Usage: &usage},
		}, fmt.Errorf("response incomplete: %s", incompleteReason(event.Response))

	case "response.failed":
		if st.done {
			return nil, nil
		}
		st.done = true
		return nil, fmt.Errorf("response failed: %s", responseErrorString(event.Response.Error))

	case "error":
		// Top-level transport/protocol error event. The message/code/param live
		// directly on the event union (variant ResponseErrorEvent).
		if st.done {
			return nil, nil
		}
		st.done = true
		return nil, fmt.Errorf("stream error: %s", streamErrorString(event))

	default:
		return nil, nil
	}
}

// responseErrorString renders a Responses ResponseError (on a failed response)
// as "code: message", tolerating either part being absent.
func responseErrorString(e responses.ResponseError) string {
	switch {
	case e.Message != "" && e.Code != "":
		return fmt.Sprintf("%s: %s", e.Code, e.Message)
	case e.Message != "":
		return e.Message
	case e.Code != "":
		return string(e.Code)
	default:
		return "unknown error"
	}
}

// streamErrorString renders a top-level "error" event union as "code: message",
// optionally appending the offending param, tolerating absent parts.
func streamErrorString(event responses.ResponseStreamEventUnion) string {
	msg := event.Message
	switch {
	case msg != "" && event.Code != "":
		msg = fmt.Sprintf("%s: %s", event.Code, msg)
	case msg == "" && event.Code != "":
		msg = event.Code
	case msg == "":
		msg = "unknown error"
	}
	if event.Param != "" {
		msg = fmt.Sprintf("%s (param: %s)", msg, event.Param)
	}
	return msg
}

// incompleteReason extracts the incomplete_details.reason from a response,
// falling back to the status when no reason was provided.
func incompleteReason(r responses.Response) string {
	if r.IncompleteDetails.Reason != "" {
		return r.IncompleteDetails.Reason
	}
	return string(r.Status)
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
		chunks, terr := translate(event, &st)
		out = append(out, chunks...)
		if terr != nil {
			return out, terr
		}
	}
	if err := sc.Err(); err != nil {
		return nil, err
	}
	return out, nil
}
