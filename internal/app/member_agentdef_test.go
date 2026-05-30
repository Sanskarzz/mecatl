package app

import (
	"context"
	"encoding/json"
	"testing"

	"github.com/stacklok/mecatl/internal/adapter/agents"
	"github.com/stacklok/mecatl/internal/adapter/hookexec"
	"github.com/stacklok/mecatl/internal/adapter/memfs"
	"github.com/stacklok/mecatl/internal/adapter/mockllm"
	"github.com/stacklok/mecatl/internal/agent"
	"github.com/stacklok/mecatl/internal/session"
	"github.com/stacklok/mecatl/internal/team"
	"github.com/stacklok/mecatl/internal/tool"
)

// regOf builds a one-def registry for the member-resolution tests.
func regOf(defs ...agents.AgentDef) *agents.Registry { return agents.NewRegistry(defs) }

// editCall scripts a member turn that calls Edit (a workspace-mutating tool), then a
// text turn, so we can observe whether Edit is in the member's catalog by whether
// the loop dispatches a tool.call for it (the catalog rejects an unknown tool).
func editCall() *mockllm.Provider {
	call := session.ToolCall{
		ID:   "c1",
		Name: "Edit",
		Args: json.RawMessage(`{"file_path":"/ws/f.txt","old_string":"a","new_string":"b"}`),
	}
	return mockllm.New(mockllm.ToolCallTurn(call), mockllm.TextTurn("done"))
}

// TestMemberMutatingDefKeepsEditWhenMutating proves a Mutating member whose def lists
// Edit actually dispatches an Edit tool.call (the tool is in the catalog), end to end
// through the supervisor against a forked memfs workspace.
func TestMemberMutatingDefKeepsEditWhenMutating(t *testing.T) {
	cfg := Config{Workspace: t.TempDir(), Model: "m"}
	def := agents.AgentDef{Name: "writer", Description: "w", Tools: []string{"Read", "Edit"}}
	tm := team.New("t")
	factory := buildMemberEngine(cfg, editCall(), hookexec.New(nil), regOf(def))

	sup := agent.NewSupervisor(tm, memfs.NewWorkspace("/ws"),
		func(spec agent.MemberSpec) agent.MemberBuild { return factory(tm, spec) },
		agent.WithForker(memfsForker{}))
	if err := sup.AddMember(context.Background(), agent.MemberSpec{
		Name: "w", AgentType: "writer", Mutating: true, InitialPrompt: "edit it",
	}); err != nil {
		t.Fatalf("AddMember(mutating): %v", err)
	}

	var events []agent.TeamEvent
	sup.Run(context.Background(), func(ev agent.TeamEvent) { events = append(events, ev) })

	if !sawToolCall(events, "w", "Edit") {
		t.Fatalf("Mutating member with a def listing Edit should dispatch an Edit tool.call; events=%d", len(events))
	}
}

// TestMemberReadOnlyDefDropsMutating asserts a READ-ONLY (base-sharing) member whose
// def lists Edit has Edit DROPPED — so the supervisor's AddMember backstop
// (ErrReadOnlyMemberMutating) is NOT tripped and the spawn succeeds. Symmetrically,
// the member never dispatches an Edit tool.call (it is not in the catalog).
func TestMemberReadOnlyDefDropsMutating(t *testing.T) {
	cfg := Config{Workspace: t.TempDir(), Model: "m"}
	def := agents.AgentDef{Name: "reviewer", Description: "r", Tools: []string{"Read", "Edit", "Write"}}
	tm := team.New("t")
	factory := buildMemberEngine(cfg, editCall(), hookexec.New(nil), regOf(def))

	sup := agent.NewSupervisor(tm, memfs.NewWorkspace("/ws"),
		func(spec agent.MemberSpec) agent.MemberBuild { return factory(tm, spec) })
	// A read-only member with a mutating-tool def must be ACCEPTED (tools dropped),
	// not rejected with ErrReadOnlyMemberMutating.
	if err := sup.AddMember(context.Background(), agent.MemberSpec{
		Name: "reviewer", AgentType: "reviewer", InitialPrompt: "review",
	}); err != nil {
		t.Fatalf("read-only member with a mutating def should be accepted (Edit dropped), got %v", err)
	}

	var events []agent.TeamEvent
	sup.Run(context.Background(), func(ev agent.TeamEvent) { events = append(events, ev) })
	if sawToolCall(events, "reviewer", "Edit") {
		t.Fatal("read-only member dispatched Edit; it should have been dropped from the catalog")
	}
}

// TestMemberPerMemberPlanMode asserts a member whose def says permissionMode: plan
// runs its session in plan mode even when the team default differs (acceptEdits).
func TestMemberPerMemberPlanMode(t *testing.T) {
	cfg := Config{Workspace: t.TempDir(), Model: "m"}
	planDef := agents.AgentDef{Name: "planner", Description: "p", PermissionMode: "plan"}
	tm := team.New("t")
	factory := buildMemberEngine(cfg, mockllm.New(mockllm.TextTurn("x")), hookexec.New(nil), regOf(planDef))

	build := factory(tm, agent.MemberSpec{Name: "planner", AgentType: "planner"})
	if build.Mode != session.ModePlan {
		t.Fatalf("planner member mode = %q, want %q", build.Mode, session.ModePlan)
	}

	// A member without a def (or a default-mode def) returns the empty Mode, so the
	// supervisor falls back to the team default.
	noDefBuild := factory(tm, agent.MemberSpec{Name: "other"})
	if noDefBuild.Mode != "" {
		t.Fatalf("member with no def mode = %q, want empty (team default)", noDefBuild.Mode)
	}
}

// TestMemberUnknownAgentTypeFallsBack asserts an unknown AgentType is forgiving: the
// factory falls back to the default member catalog (read-only base) and the spawn
// succeeds rather than failing.
func TestMemberUnknownAgentTypeFallsBack(t *testing.T) {
	cfg := Config{Workspace: t.TempDir(), Model: "m"}
	tm := team.New("t")
	factory := buildMemberEngine(cfg, mockllm.New(mockllm.TextTurn("x")), hookexec.New(nil), regOf())

	build := factory(tm, agent.MemberSpec{Name: "ghost", AgentType: "does-not-exist"})
	if build.Engine == nil {
		t.Fatal("unknown AgentType should fall back to a default engine, got nil")
	}
	if build.Mode != "" {
		t.Fatalf("unknown AgentType mode = %q, want empty (team default)", build.Mode)
	}

	sup := agent.NewSupervisor(tm, memfs.NewWorkspace("/ws"),
		func(spec agent.MemberSpec) agent.MemberBuild { return factory(tm, spec) })
	if err := sup.AddMember(context.Background(), agent.MemberSpec{Name: "ghost", AgentType: "does-not-exist"}); err != nil {
		t.Fatalf("unknown AgentType member should still enrol, got %v", err)
	}
}

// sawToolCall reports whether the tagged event stream contains a tool.call for the
// named tool produced by the named member.
func sawToolCall(events []agent.TeamEvent, member, toolName string) bool {
	for _, ev := range events {
		if ev.Member == member && ev.Event.Type == session.EvToolCall &&
			ev.Event.ToolCall != nil && ev.Event.ToolCall.Name == toolName {
			return true
		}
	}
	return false
}

// memfsForker forks a memfs workspace for a Mutating member (a deterministic,
// offline isolation seam for the tests — it just roots a fresh memfs under a label).
type memfsForker struct{}

func (memfsForker) Fork(_ context.Context, _ tool.Workspace, label string) (tool.Workspace, func() error, error) {
	return memfs.NewWorkspace("/fork/" + label), func() error { return nil }, nil
}
