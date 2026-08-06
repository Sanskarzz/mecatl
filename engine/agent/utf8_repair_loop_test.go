package agent_test

import (
	"context"
	"strings"
	"sync"
	"testing"
	"unicode/utf8"

	"github.com/stacklok/mecatl/engine/adapter/memfs"
	"github.com/stacklok/mecatl/engine/adapter/mockllm"
	"github.com/stacklok/mecatl/engine/agent"
	"github.com/stacklok/mecatl/engine/port"
	"github.com/stacklok/mecatl/engine/session"
	"github.com/stacklok/mecatl/engine/tool"
)

// invalidUTF8 is the orphaned-lead-byte sequence from issue #402: BSD `cat -t`
// turns a valid em dash (E2 80 94) into E2 4D 2D 5E 40 4D 2D 5E 54 — the E2
// lead byte is retained while its continuation bytes are rendered as ASCII,
// leaving an invalid sequence.
const invalidUTF8 = "\xe2M-^@M-^T"

// TestLoopRepairsInvalidUTF8ToolResult proves the loop normalizes a tool's
// invalid-UTF-8 result at the effective-payload choke point (issue #402), so
// the THREE consumers — the emitted EvToolResult, the recorded conversation
// (the model's replayed history), and the next-turn provider request — all
// carry the SAME U+FFFD-repaired text. A protobuf string field rejects invalid
// UTF-8 at marshal time, so without this repair the gRPC Converse stream dies
// with codes.Internal; this test pins the domain-side fix that keeps every
// view consistent.
func TestLoopRepairsInvalidUTF8ToolResult(t *testing.T) {
	bad := &fakeTool{name: "Bash", readOnly: true,
		exec: func(_ context.Context, in session.ToolCall, _ tool.Workspace) (session.ToolResult, error) {
			return session.NewToolResult(in.ID, "out "+invalidUTF8+" end"), nil
		}}
	cat := catalogWith(t, bad)

	// Capture every request the loop sends the provider; the SECOND request is
	// the one that replays the tool result back to the model.
	var mu sync.Mutex
	var reqs []port.LLMRequest
	llm := mockllm.NewWith(
		[]mockllm.Option{mockllm.WithRequestObserver(func(r port.LLMRequest) {
			mu.Lock()
			reqs = append(reqs, r)
			mu.Unlock()
		})},
		mockllm.ChunksTurn(
			mockllm.ToolCallChunk(toolCall("c1", "Bash", `{"command":"x"}`)),
			mockllm.UsageChunk(session.Usage{}),
			mockllm.DoneChunk(session.StopEndTurn),
		),
		mockllm.ChunksTurn(
			mockllm.TextChunk("done"),
			mockllm.UsageChunk(session.Usage{}),
			mockllm.DoneChunk(session.StopEndTurn),
		),
	)

	e := newEngine(agent.Deps{LLM: llm, Catalog: cat})
	sess := newSession(t, session.Limits{})
	r := e.Run(context.Background(), sess, memfs.NewWorkspace("/ws"), "run it")
	evs := drain(r)

	// 1. The emitted EvToolResult is repaired.
	var emitted *session.ToolResult
	for i := range evs {
		if evs[i].Type == session.EvToolResult && evs[i].ToolResult != nil && evs[i].ToolResult.CallID == "c1" {
			emitted = evs[i].ToolResult
			break
		}
	}
	if emitted == nil {
		t.Fatalf("no EvToolResult for c1 in %v", typesOf(evs))
	}
	if !utf8.ValidString(emitted.Content) {
		t.Fatalf("emitted Content invalid: %q", emitted.Content)
	}
	if !strings.ContainsRune(emitted.Content, '�') {
		t.Fatalf("emitted Content not repaired: %q", emitted.Content)
	}

	// 2. The recorded conversation (the model's replayed history) is repaired.
	var recorded *session.ToolResult
	for i := range sess.Conversation.Messages {
		m := &sess.Conversation.Messages[i]
		if m.Role == session.RoleTool && m.ToolResult != nil && m.ToolResult.CallID == "c1" {
			recorded = m.ToolResult
			break
		}
	}
	if recorded == nil {
		t.Fatalf("no recorded tool result for c1")
	}
	if recorded.Content != emitted.Content {
		t.Fatalf("recorded != emitted:\nrecorded %q\nemitted  %q", recorded.Content, emitted.Content)
	}

	// 3. The next-turn provider request carries the SAME repaired text.
	mu.Lock()
	defer mu.Unlock()
	if len(reqs) < 2 {
		t.Fatalf("expected >=2 provider calls, got %d", len(reqs))
	}
	var modelFacing string
	for _, m := range reqs[1].Messages {
		if m.Role == session.RoleTool && m.ToolResult != nil && m.ToolResult.CallID == "c1" {
			modelFacing = m.ToolResult.Content
		}
	}
	if modelFacing == "" {
		t.Fatalf("no tool-role message for c1 in second request")
	}
	if modelFacing != emitted.Content {
		t.Fatalf("model-facing != emitted:\nmodel %q\nemitted %q", modelFacing, emitted.Content)
	}
}
