package client

import (
	"io"
	"sync"

	mecatlv1 "github.com/stacklok/mecatl/contracts/gen/go/mecatl/v1"
)

// fakeStream is a scripted, in-memory Converse stream used by tests: Recv replays
// a fixed slice of responses then returns io.EOF (or a configured error), and
// Send records the frames the ui sends back. It satisfies both Recver and Sender
// so the real Stream/ReadLoop run with no gRPC and no network — the offline,
// deterministic test substrate the brief mandates.
type fakeStream struct {
	mu sync.Mutex

	script []*mecatlv1.ConverseResponse
	idx    int
	endErr error // returned after the script drains (nil ⇒ io.EOF)

	sent []*mecatlv1.ConverseRequest
}

func newFakeStream(script ...*mecatlv1.ConverseResponse) *fakeStream {
	return &fakeStream{script: script}
}

func (f *fakeStream) Recv() (*mecatlv1.ConverseResponse, error) {
	f.mu.Lock()
	if f.idx >= len(f.script) {
		err := f.endErr
		f.mu.Unlock()
		if err != nil {
			return nil, err
		}
		return nil, io.EOF
	}
	resp := f.script[f.idx]
	f.idx++
	f.mu.Unlock()
	return resp, nil
}

func (f *fakeStream) Send(req *mecatlv1.ConverseRequest) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.sent = append(f.sent, req)
	return nil
}

func (f *fakeStream) sentFrames() []*mecatlv1.ConverseRequest {
	f.mu.Lock()
	defer f.mu.Unlock()
	out := make([]*mecatlv1.ConverseRequest, len(f.sent))
	copy(out, f.sent)
	return out
}

// resp builds a ConverseResponse wrapping an Event for the script.
func resp(ev *mecatlv1.Event) *mecatlv1.ConverseResponse {
	return &mecatlv1.ConverseResponse{Event: ev}
}

// scriptedRun mirrors cmd/mecademo's three-act scenario as proto events:
// session.init → turn.start → deltas → tool.call(Read) → tool.result →
// turn.start → delta → permission.ask(Write) → ... → result. The permission ask
// is left UNRESOLVED here; tests drive the approval.
func scriptedRun() []*mecatlv1.ConverseResponse {
	return []*mecatlv1.ConverseResponse{
		resp(&mecatlv1.Event{Type: "session.init", Seq: 1}),
		resp(&mecatlv1.Event{Type: "turn.start", Seq: 2, Turn: 1}),
		resp(&mecatlv1.Event{Type: "message.delta", Seq: 3, Turn: 1, Text: "I'll read the greeting file first."}),
		resp(&mecatlv1.Event{Type: "tool.call", Seq: 4, Turn: 1, ToolCall: &mecatlv1.ToolCall{
			Id: "call-read-1", Name: "Read", Args: `{"path":"greeting.txt"}`,
		}}),
		resp(&mecatlv1.Event{Type: "tool.result", Seq: 5, Turn: 1, ToolResult: &mecatlv1.ToolResult{
			CallId: "call-read-1", Content: "hello from the mecatl demo workspace\n",
		}}),
		resp(&mecatlv1.Event{Type: "turn.start", Seq: 6, Turn: 2}),
		resp(&mecatlv1.Event{Type: "message.delta", Seq: 7, Turn: 2, Text: "Now I'll save a note, which needs your approval."}),
		resp(&mecatlv1.Event{Type: "permission.ask", Seq: 8, Turn: 2, Ask: &mecatlv1.PermissionAsk{
			AskId: "ask-write-1", Tool: "Write", Args: `{"path":"note.txt","content":"reviewed"}`, Reason: "Write requires approval",
		}}),
	}
}

// scriptedRunResult appends the post-approval tail to scriptedRun: the Write
// result and the terminal result event.
func scriptedRunResult() []*mecatlv1.ConverseResponse {
	tail := []*mecatlv1.ConverseResponse{
		resp(&mecatlv1.Event{Type: "tool.result", Seq: 9, Turn: 2, ToolResult: &mecatlv1.ToolResult{
			CallId: "call-write-1", Content: "wrote note.txt",
		}}),
		resp(&mecatlv1.Event{Type: "message.delta", Seq: 10, Turn: 3, Text: "Done: read greeting.txt and saved note.txt."}),
		resp(&mecatlv1.Event{Type: "result", Seq: 11, Turn: 3, Result: &mecatlv1.Result{
			Stop: "end_turn", Text: "Done.", Usage: &mecatlv1.Usage{InputTokens: 1500, OutputTokens: 30, CacheReadTokens: 1400},
		}}),
	}
	return append(scriptedRun(), tail...)
}
