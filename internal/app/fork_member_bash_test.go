package app

import (
	"context"
	"encoding/json"
	"strings"
	"testing"
	"time"

	"github.com/stacklok/mecatl/internal/adapter/agents"
	"github.com/stacklok/mecatl/internal/adapter/hookexec"
	"github.com/stacklok/mecatl/internal/adapter/memfs"
	"github.com/stacklok/mecatl/internal/adapter/mockllm"
	"github.com/stacklok/mecatl/internal/agent"
	"github.com/stacklok/mecatl/internal/session"
	"github.com/stacklok/mecatl/internal/team"
)

// These tests cover FIX 1 (isolation): a forked/other-workspace child — a Fork
// branch and a Mutating team member — must NOT be given Bash, because the Bash
// runner is rooted at the PARENT base and would escape the fork. Edit/Write MUST
// remain (a fork child can still implement). The cfg used by every test configures
// a real shell so buildCommandRunner returns a non-nil runner; if the production
// code wrongly registered Bash it WOULD be in the catalog, so these tests genuinely
// prove the deliberate exclusion (not mere absence of a runner).

// bashThenEdit scripts a child/member turn that first calls Bash, then Edit, then a
// text turn. Whether each tool is in the catalog is observable from the resulting
// tool.result: an unknown tool surfaces as an error result reading `unknown tool ...`.
func bashThenEdit() *mockllm.Provider {
	bash := session.ToolCall{
		ID:   "b1",
		Name: "Bash",
		Args: json.RawMessage(`{"command":"echo hi"}`),
	}
	edit := session.ToolCall{
		ID:   "e1",
		Name: "Edit",
		Args: json.RawMessage(`{"file_path":"/ws/f.txt","old_string":"a","new_string":"b"}`),
	}
	return mockllm.New(
		mockllm.ToolCallTurn(bash),
		mockllm.ToolCallTurn(edit),
		mockllm.TextTurn("done"),
	)
}

// unknownToolResult reports whether the event stream contains a tool.result for
// callID that is an error reading "unknown tool" (the loop's signal that the tool
// is NOT in the engine's catalog).
func unknownToolResult(events []session.Event, callID session.ToolCallID) bool {
	for _, ev := range events {
		if ev.Type == session.EvToolResult && ev.ToolResult != nil &&
			ev.ToolResult.CallID == callID && ev.ToolResult.IsError &&
			strings.Contains(ev.ToolResult.Content, "unknown tool") {
			return true
		}
	}
	return false
}

// sawCatalogToolCall reports whether the stream dispatched a (known) tool.call for
// the named tool — i.e. a tool.call event for it that was NOT answered by an
// "unknown tool" error.
func sawDispatchedTool(events []session.Event, callID session.ToolCallID) bool {
	var called bool
	for _, ev := range events {
		if ev.Type == session.EvToolCall && ev.ToolCall != nil && ev.ToolCall.ID == callID {
			called = true
		}
	}
	return called && !unknownToolResult(events, callID)
}

// drainEngine runs an engine over a fresh session and returns the full event stream.
func drainEngine(t *testing.T, eng *agent.Engine) []session.Event {
	t.Helper()
	sess := session.New("s", session.ModeDefault, "/ws", session.Limits{MaxTurns: 5}, time.Now())
	run := eng.Run(context.Background(), sess, memfs.NewWorkspace("/ws"), "go")
	var events []session.Event
	for ev := range run.Events() {
		events = append(events, ev)
	}
	return events
}

// TestForkChildEngineHasEditNotBash proves buildForkChildEngine's catalog contains
// Edit (a fork child can implement) but NOT Bash (Bash would escape the fork to the
// parent base), even though a shell is configured.
func TestForkChildEngineHasEditNotBash(t *testing.T) {
	cfg := teamCfg(t) // configures Shell, so a runner exists and Bash COULD be wired
	if buildCommandRunner(cfg) == nil {
		t.Fatal("precondition: expected a non-nil command runner with Shell set")
	}
	eng := buildForkChildEngine(cfg, bashThenEdit())

	events := drainEngine(t, eng)

	if !unknownToolResult(events, "b1") {
		t.Error("Fork child dispatched Bash; Bash MUST be excluded from a forked child (it would escape the fork to the parent base)")
	}
	if !sawDispatchedTool(events, "e1") {
		t.Error("Fork child did not dispatch Edit; a fork child must keep Edit/Write to implement in its fork")
	}
}

// TestMutatingMemberHasEditNotBash proves a default (no-def) Mutating team member's
// engine contains Edit but NOT Bash, for the same isolation reason — Bash's runner
// is rooted at the parent base and would escape the member's fork.
func TestMutatingMemberHasEditNotBash(t *testing.T) {
	cfg := teamCfg(t)
	if buildCommandRunner(cfg) == nil {
		t.Fatal("precondition: expected a non-nil command runner with Shell set")
	}
	tm := team.New("t")
	factory := buildMemberEngine(cfg, bashThenEdit(), hookexec.New(nil), agents.NewRegistry(nil))
	build := factory(tm, agent.MemberSpec{Name: "writer", Mutating: true})
	if build.Engine == nil {
		t.Fatal("factory returned a nil engine")
	}

	events := drainEngine(t, build.Engine)

	if !unknownToolResult(events, "b1") {
		t.Error("Mutating member dispatched Bash; Bash MUST be excluded from a Mutating (forked) member")
	}
	if !sawDispatchedTool(events, "e1") {
		t.Error("Mutating member did not dispatch Edit; a Mutating member must keep Edit/Write to implement in its fork")
	}
}

// TestMutatingMemberDefCannotScopeInBash proves the DEFINED-member path also excludes
// Bash: even a Mutating member whose agent def explicitly allowlists Bash does NOT
// get it (Bash is removed from the available base for forked members), while a
// listed Edit survives.
func TestMutatingMemberDefCannotScopeInBash(t *testing.T) {
	cfg := teamCfg(t)
	tm := team.New("t")
	def := agents.AgentDef{Name: "writer", Description: "w", Tools: []string{"Read", "Edit", "Bash"}}
	factory := buildMemberEngine(cfg, bashThenEdit(), hookexec.New(nil), regOf(def))
	build := factory(tm, agent.MemberSpec{Name: "writer", AgentType: "writer", Mutating: true})
	if build.Engine == nil {
		t.Fatal("factory returned a nil engine")
	}

	events := drainEngine(t, build.Engine)

	if !unknownToolResult(events, "b1") {
		t.Error("Mutating member def allowlisting Bash still got Bash; it must be excluded for forked members")
	}
	if !sawDispatchedTool(events, "e1") {
		t.Error("Mutating member def listing Edit did not dispatch Edit; Edit must survive scoping")
	}
}
