package agent_test

import (
	"context"
	"strings"
	"testing"

	"github.com/stacklok/mecatl/internal/adapter/memfs"
	"github.com/stacklok/mecatl/internal/adapter/mockllm"
	"github.com/stacklok/mecatl/internal/agent"
	"github.com/stacklok/mecatl/internal/session"
	"github.com/stacklok/mecatl/internal/tool"
)

// taskParentResults runs a parent engine whose only tool is the given Task tool,
// driving it with the supplied parent turns, and returns every ToolResult the
// parent observed plus the parent's final text.
func taskParentResults(t *testing.T, task tool.Tool, parentTurns ...mockllm.Turn) ([]*session.ToolResult, string) {
	t.Helper()
	parentLLM := mockllm.New(parentTurns...)
	e := newEngine(agent.Deps{LLM: parentLLM, Catalog: catalogWith(t, task)})
	r := e.Run(context.Background(), newSession(t, session.Limits{}), memfs.NewWorkspace("/ws"), "go")
	evs := drain(r)
	var results []*session.ToolResult
	for _, ev := range evs {
		if ev.Type == session.EvToolResult {
			results = append(results, ev.ToolResult)
		}
	}
	return results, lastResult(t, evs).Text
}

// TestTaskRoutesToNamedAgent proves that Task(agent="reviewer") runs the named
// engine (distinct behaviour) rather than the default explorer.
func TestTaskRoutesToNamedAgent(t *testing.T) {
	// Default explorer: a child whose summary is "DEFAULT".
	defaultEngine := childEngineWith(mockllm.New(mockllm.TextTurn("DEFAULT")), catalogWith(t))
	// Named specialist "reviewer": a distinct child whose summary is "REVIEWER".
	reviewerEngine := childEngineWith(mockllm.New(mockllm.TextTurn("REVIEWER")), catalogWith(t))

	task := agent.NewTaskTool(defaultEngine, agent.WithAgentEngines(
		map[string]*agent.Engine{"reviewer": reviewerEngine},
		[]agent.AgentMeta{{Name: "reviewer", Description: "reviews diffs"}},
	))

	results, _ := taskParentResults(t, task,
		mockllm.ToolCallTurn(toolCall("p1", "Task", `{"prompt":"check it","agent":"reviewer"}`)),
		mockllm.TextTurn("parent done"),
	)
	if len(results) != 1 {
		t.Fatalf("want 1 result, got %d", len(results))
	}
	if results[0].IsError || results[0].Content != "REVIEWER" {
		t.Fatalf("routed result = %+v, want the reviewer engine's summary", results[0])
	}
}

// TestTaskDefaultExplorerUnchanged proves no regression: a Task call with NO
// `agent` arg runs the default explorer even when specialists are configured.
func TestTaskDefaultExplorerUnchanged(t *testing.T) {
	defaultEngine := childEngineWith(mockllm.New(mockllm.TextTurn("DEFAULT")), catalogWith(t))
	reviewerEngine := childEngineWith(mockllm.New(mockllm.TextTurn("REVIEWER")), catalogWith(t))

	task := agent.NewTaskTool(defaultEngine, agent.WithAgentEngines(
		map[string]*agent.Engine{"reviewer": reviewerEngine},
		[]agent.AgentMeta{{Name: "reviewer", Description: "reviews diffs"}},
	))

	results, _ := taskParentResults(t, task,
		mockllm.ToolCallTurn(toolCall("p1", "Task", `{"prompt":"explore"}`)),
		mockllm.TextTurn("parent done"),
	)
	if len(results) != 1 || results[0].Content != "DEFAULT" {
		t.Fatalf("default route result = %+v, want DEFAULT explorer", results[0])
	}
}

// TestTaskUnknownAgentErrors proves an unknown name yields a model-addressable
// error result listing the valid names (no silent fallback).
func TestTaskUnknownAgentErrors(t *testing.T) {
	defaultEngine := childEngineWith(mockllm.New(mockllm.TextTurn("DEFAULT")), catalogWith(t))
	reviewerEngine := childEngineWith(mockllm.New(mockllm.TextTurn("REVIEWER")), catalogWith(t))

	task := agent.NewTaskTool(defaultEngine, agent.WithAgentEngines(
		map[string]*agent.Engine{"reviewer": reviewerEngine},
		[]agent.AgentMeta{{Name: "reviewer", Description: "reviews diffs"}},
	))

	results, _ := taskParentResults(t, task,
		mockllm.ToolCallTurn(toolCall("p1", "Task", `{"prompt":"x","agent":"nope"}`)),
		mockllm.TextTurn("parent recovered"),
	)
	if len(results) != 1 {
		t.Fatalf("want 1 result, got %d", len(results))
	}
	if !results[0].IsError {
		t.Fatalf("unknown agent should be an error result, got %+v", results[0])
	}
	if !strings.Contains(results[0].Content, "nope") || !strings.Contains(results[0].Content, "reviewer") {
		t.Fatalf("error should name the bad input and list valid names, got %q", results[0].Content)
	}
}

// TestTaskSpecEnumeratesAgents proves the available specialists appear in the
// Task tool's Spec().Description (progressive disclosure), and that Task.ReadOnly
// stays unconditionally true.
func TestTaskSpecEnumeratesAgents(t *testing.T) {
	defaultEngine := childEngineWith(mockllm.New(mockllm.TextTurn("x")), catalogWith(t))
	task := agent.NewTaskTool(defaultEngine, agent.WithAgentEngines(
		map[string]*agent.Engine{"reviewer": defaultEngine, "doc-writer": defaultEngine},
		[]agent.AgentMeta{
			{Name: "doc-writer", Description: "writes docs"},
			{Name: "reviewer", Description: "reviews diffs"},
		},
	))
	desc := task.Spec().Description
	if !strings.Contains(desc, "reviewer: reviews diffs") || !strings.Contains(desc, "doc-writer: writes docs") {
		t.Fatalf("spec must enumerate specialists, got:\n%s", desc)
	}
	if !task.ReadOnly() {
		t.Fatalf("Task.ReadOnly() must stay true")
	}
}

// TestTaskNoAgentsNoEnumeration proves the spec is unchanged when no specialists
// are configured (the default-explorer-only case).
func TestTaskNoAgentsNoEnumeration(t *testing.T) {
	defaultEngine := childEngineWith(mockllm.New(mockllm.TextTurn("x")), catalogWith(t))
	task := agent.NewTaskTool(defaultEngine)
	if strings.Contains(task.Spec().Description, "Available specialist agents") {
		t.Fatalf("no agents configured: spec must not have an enumeration tail")
	}
}
