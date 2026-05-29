package agent_test

import (
	"context"
	"encoding/json"
	"strings"
	"testing"
	"time"

	"github.com/stacklok/ozzharness/internal/adapter/memfs"
	"github.com/stacklok/ozzharness/internal/adapter/mockllm"
	"github.com/stacklok/ozzharness/internal/agent"
	"github.com/stacklok/ozzharness/internal/session"
)

// TestHeuristicCompactorPreservesPaths checks the default compactor preserves the
// system prompt, the goal, and every touched file path, while truncating large
// tool-output bodies (gauntlet #12-lite).
func TestHeuristicCompactorPreservesPaths(t *testing.T) {
	conv := &session.Conversation{}
	conv.Append(session.NewSystemMessage("system rules"))
	conv.Append(session.NewUserMessage("fix the bug in handler.go"))
	conv.Append(session.NewAssistantMessage("", "", []session.ToolCall{
		session.NewToolCall("c1", "Read", json.RawMessage(`{"path":"handler.go"}`)),
	}))
	conv.Append(session.NewToolMessage(session.NewToolResult("c1", strings.Repeat("X", 5000))))
	conv.Append(session.NewAssistantMessage("", "", []session.ToolCall{
		session.NewToolCall("c2", "Edit", json.RawMessage(`{"file_path":"util.go"}`)),
	}))
	conv.Append(session.NewToolMessage(session.NewToolResult("c2", "edited")))
	for i := 0; i < 10; i++ {
		conv.Append(session.NewUserMessage("more chatter"))
	}

	compacted, summary, err := agent.HeuristicCompactor{}.Compact(context.Background(), conv)
	if err != nil {
		t.Fatalf("Compact: %v", err)
	}

	// Paths survive (in summary).
	for _, p := range []string{"handler.go", "util.go"} {
		if !strings.Contains(summary, p) {
			t.Fatalf("summary missing path %q: %s", p, summary)
		}
	}
	// System prompt and goal survive.
	var haveSystem, haveGoal bool
	for _, m := range compacted {
		if m.Role == session.RoleSystem && m.Text == "system rules" {
			haveSystem = true
		}
		if m.Role == session.RoleUser && strings.Contains(m.Text, "fix the bug") {
			haveGoal = true
		}
	}
	if !haveSystem {
		t.Fatalf("system prompt not preserved")
	}
	if !haveGoal {
		t.Fatalf("goal not preserved")
	}
	// Big bodies are dropped: no message should carry the 5000-char blob.
	for _, m := range compacted {
		if m.ToolResult != nil && len(m.ToolResult.Content) >= 5000 {
			t.Fatalf("large tool body survived compaction: %d chars", len(m.ToolResult.Content))
		}
	}
	// The result is shorter than the input.
	if len(compacted) >= len(conv.Messages) {
		t.Fatalf("compaction did not shrink history: %d -> %d", len(conv.Messages), len(compacted))
	}
}

// recordingCompactor records that it was invoked and returns a minimal history.
type recordingCompactor struct{ called int }

func (c *recordingCompactor) Compact(_ context.Context, _ *session.Conversation) ([]session.Message, string, error) {
	c.called++
	return []session.Message{session.NewUserMessage("compacted goal")}, "compacted summary", nil
}

// TestCompactionTriggersAtThreshold drives the loop with a tiny context window so
// the threshold trips, and asserts the Compactor runs and a compaction Event is
// emitted.
func TestCompactionTriggersAtThreshold(t *testing.T) {
	rc := &recordingCompactor{}
	llm := mockllm.New(mockllm.TextTurn("done"))
	e := agent.NewEngine(agent.Deps{
		LLM:                 llm,
		Catalog:             catalogWith(t),
		Policy:              allowAll(),
		Model:               "m",
		Compactor:           rc,
		ContextWindowTokens: 10, // tiny: a long prompt blows past 80% of it
		CompactionRatio:     0.8,
	})

	sess := session.New("s1", session.ModeDefault, "/ws", session.Limits{}, time.Unix(0, 0))
	bigPrompt := strings.Repeat("word ", 200) // ~250 tokens >> threshold of 8
	r := e.Run(context.Background(), sess, memfs.NewWorkspace("/ws"), bigPrompt)

	var sawCompaction bool
	for ev := range r.Events() {
		if ev.Type == session.EvCompaction {
			sawCompaction = true
		}
	}
	if rc.called == 0 {
		t.Fatalf("compactor was not invoked")
	}
	if !sawCompaction {
		t.Fatalf("no compaction event emitted")
	}
}
