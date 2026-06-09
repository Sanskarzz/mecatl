package agent_test

import (
	"context"
	"strings"
	"testing"

	"github.com/stacklok/mecatl/internal/adapter/memfs"
	"github.com/stacklok/mecatl/internal/adapter/mockllm"
	"github.com/stacklok/mecatl/internal/adapter/store/memstore"
	"github.com/stacklok/mecatl/internal/agent"
	"github.com/stacklok/mecatl/internal/session"
	"github.com/stacklok/mecatl/internal/tool"
)

// subagentParentResults runs a parent engine whose only tool is the given Subagent tool,
// driving it with the supplied parent turns, and returns every ToolResult the
// parent observed plus the parent's final text.
func subagentParentResults(t *testing.T, task tool.Tool, parentTurns ...mockllm.Turn) ([]*session.ToolResult, string) {
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

// TestSubagentRoutesToNamedAgent proves that Subagent(agent="reviewer") runs the named
// engine (distinct behaviour) rather than the default explorer.
func TestSubagentRoutesToNamedAgent(t *testing.T) {
	// Default explorer: a child whose summary is "DEFAULT".
	defaultEngine := childEngineWith(mockllm.New(mockllm.TextTurn("DEFAULT")), catalogWith(t))
	// Named specialist "reviewer": a distinct child whose summary is "REVIEWER".
	reviewerEngine := childEngineWith(mockllm.New(mockllm.TextTurn("REVIEWER")), catalogWith(t))

	task := agent.NewSubagentTool(defaultEngine, agent.WithAgentEngines(
		map[string]*agent.Engine{"reviewer": reviewerEngine},
		[]agent.AgentMeta{{Name: "reviewer", Description: "reviews diffs"}},
	))

	results, _ := subagentParentResults(t, task,
		mockllm.ToolCallTurn(toolCall("p1", "Subagent", `{"prompt":"check it","agent":"reviewer"}`)),
		mockllm.TextTurn("parent done"),
	)
	if len(results) != 1 {
		t.Fatalf("want 1 result, got %d", len(results))
	}
	if results[0].IsError || !strings.Contains(results[0].Content, "REVIEWER") {
		t.Fatalf("routed result = %+v, want the reviewer engine's summary", results[0])
	}
}

// TestSubagentDefaultExplorerUnchanged proves no regression: a Subagent call with NO
// `agent` arg runs the default explorer even when specialists are configured.
func TestSubagentDefaultExplorerUnchanged(t *testing.T) {
	defaultEngine := childEngineWith(mockllm.New(mockllm.TextTurn("DEFAULT")), catalogWith(t))
	reviewerEngine := childEngineWith(mockllm.New(mockllm.TextTurn("REVIEWER")), catalogWith(t))

	task := agent.NewSubagentTool(defaultEngine, agent.WithAgentEngines(
		map[string]*agent.Engine{"reviewer": reviewerEngine},
		[]agent.AgentMeta{{Name: "reviewer", Description: "reviews diffs"}},
	))

	results, _ := subagentParentResults(t, task,
		mockllm.ToolCallTurn(toolCall("p1", "Subagent", `{"prompt":"explore"}`)),
		mockllm.TextTurn("parent done"),
	)
	if len(results) != 1 || !strings.Contains(results[0].Content, "DEFAULT") {
		t.Fatalf("default route result = %+v, want DEFAULT explorer", results[0])
	}
}

// TestSubagentUnknownAgentErrors proves an unknown name yields a model-addressable
// error result listing the valid names (no silent fallback).
func TestSubagentUnknownAgentErrors(t *testing.T) {
	defaultEngine := childEngineWith(mockllm.New(mockllm.TextTurn("DEFAULT")), catalogWith(t))
	reviewerEngine := childEngineWith(mockllm.New(mockllm.TextTurn("REVIEWER")), catalogWith(t))

	task := agent.NewSubagentTool(defaultEngine, agent.WithAgentEngines(
		map[string]*agent.Engine{"reviewer": reviewerEngine},
		[]agent.AgentMeta{{Name: "reviewer", Description: "reviews diffs"}},
	))

	results, _ := subagentParentResults(t, task,
		mockllm.ToolCallTurn(toolCall("p1", "Subagent", `{"prompt":"x","agent":"nope"}`)),
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

// TestSubagentSpecEnumeratesAgents proves the available specialists appear in the
// Subagent tool's Spec().Description (progressive disclosure), and that Subagent.ReadOnly
// stays unconditionally true.
func TestSubagentSpecEnumeratesAgents(t *testing.T) {
	defaultEngine := childEngineWith(mockllm.New(mockllm.TextTurn("x")), catalogWith(t))
	task := agent.NewSubagentTool(defaultEngine, agent.WithAgentEngines(
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
		t.Fatalf("Subagent.ReadOnly() must stay true")
	}
	// The description must name the agentId discovery channel, the InspectSubagent tool,
	// AND the `resume` continuation so the model knows to pass the trailer id along
	// (runtime-discoverability), both to read the transcript and to resume the child.
	if !strings.Contains(desc, "agentId:") || !strings.Contains(desc, "InspectSubagent") || !strings.Contains(desc, "resume") {
		t.Fatalf("spec must name 'agentId:', InspectSubagent, and resume, got:\n%s", desc)
	}
}

// TestInspectSubagentSpecNamesProvenance proves the InspectSubagent description names the
// 'agentId:' line provenance and the ~40-message bound (kept in sync with
// maxInspectMessages).
func TestInspectSubagentSpecNamesProvenance(t *testing.T) {
	desc := agent.NewInspectSubagentTool(memstore.New()).Spec().Description
	if !strings.Contains(desc, "'agentId:' line") || !strings.Contains(desc, "~40") {
		t.Fatalf("InspectSubagent spec must name the 'agentId:' line and the ~40-message bound, got:\n%s", desc)
	}
}

// readLoopChild builds a child engine + its mockllm whose script ALWAYS emits
// another Read tool call (never a terminal text turn), so an unbounded run would
// keep issuing model calls. The number of model calls it actually makes equals the
// child session's MaxTurns, which lets a test assert the per-def turn cap bites.
// turns is the number of scripted tool-call turns (must exceed any limit under
// test so the cap, not script exhaustion, is what stops the run).
func readLoopChild(t *testing.T, turns int) (*agent.Engine, *mockllm.Provider) {
	t.Helper()
	read := &fakeTool{name: "Read", readOnly: true,
		exec: func(_ context.Context, in session.ToolCall, _ tool.Workspace) (session.ToolResult, error) {
			return session.NewToolResult(in.ID, "read ok"), nil
		}}
	script := make([]mockllm.Turn, 0, turns)
	for i := 0; i < turns; i++ {
		script = append(script, mockllm.ToolCallTurn(toolCall("k", "Read", `{"path":"x"}`)))
	}
	llm := mockllm.New(script...)
	return childEngineWith(llm, catalogWith(t, read)), llm
}

// TestSubagentNamedAgentLimitsBindChildSession proves a def's per-run limits (carried
// on AgentMeta.Limits) bound the child session: a def with MaxTurns=2 makes exactly
// 2 model calls even though the child would otherwise loop far longer.
func TestSubagentNamedAgentLimitsBindChildSession(t *testing.T) {
	defaultEngine := childEngineWith(mockllm.New(mockllm.TextTurn("DEFAULT")), catalogWith(t))
	boundedEngine, boundedLLM := readLoopChild(t, 8)

	task := agent.NewSubagentTool(defaultEngine, agent.WithAgentEngines(
		map[string]*agent.Engine{"bounded": boundedEngine},
		[]agent.AgentMeta{{
			Name:        "bounded",
			Description: "a tightly-bounded specialist",
			Limits:      session.Limits{MaxTurns: 2, MaxToolCalls: 40, MaxConsecutiveFailures: 3},
		}},
	))

	subagentParentResults(t, task,
		mockllm.ToolCallTurn(toolCall("p1", "Subagent", `{"prompt":"loop","agent":"bounded"}`)),
		mockllm.TextTurn("parent done"),
	)
	if got := boundedLLM.Calls(); got != 2 {
		t.Fatalf("bounded child made %d model calls, want 2 (MaxTurns=2 should bind the child session)", got)
	}
}

// TestSubagentNamedAgentNoLimitsUsesDefault proves a def with NO per-run limits (a zero
// AgentMeta.Limits) runs under the Subagent tool's DEFAULT child limits, unchanged: the
// child loops past 2 turns up to the default MaxTurns (12).
func TestSubagentNamedAgentNoLimitsUsesDefault(t *testing.T) {
	defaultEngine := childEngineWith(mockllm.New(mockllm.TextTurn("DEFAULT")), catalogWith(t))
	// Script more turns than the default MaxTurns (12) so the DEFAULT cap, not script
	// exhaustion, is what stops the run.
	looseEngine, looseLLM := readLoopChild(t, 20)

	task := agent.NewSubagentTool(defaultEngine, agent.WithAgentEngines(
		map[string]*agent.Engine{"loose": looseEngine},
		[]agent.AgentMeta{{Name: "loose", Description: "no pinned limits"}}, // zero Limits
	))

	subagentParentResults(t, task,
		mockllm.ToolCallTurn(toolCall("p1", "Subagent", `{"prompt":"loop","agent":"loose"}`)),
		mockllm.TextTurn("parent done"),
	)
	if got := looseLLM.Calls(); got != agent.DefaultChildLimits().MaxTurns {
		t.Fatalf("loose child made %d model calls, want the default MaxTurns=%d (no per-def override)",
			got, agent.DefaultChildLimits().MaxTurns)
	}
}

// TestSubagentNoAgentsNoEnumeration proves the spec is unchanged when no specialists
// are configured (the default-explorer-only case).
func TestSubagentNoAgentsNoEnumeration(t *testing.T) {
	defaultEngine := childEngineWith(mockllm.New(mockllm.TextTurn("x")), catalogWith(t))
	task := agent.NewSubagentTool(defaultEngine)
	if strings.Contains(task.Spec().Description, "Available specialist agents") {
		t.Fatalf("no agents configured: spec must not have an enumeration tail")
	}
}
