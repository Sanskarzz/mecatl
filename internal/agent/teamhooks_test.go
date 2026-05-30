package agent_test

import (
	"context"
	"sync"
	"testing"

	"github.com/stacklok/mecatl/internal/adapter/memfs"
	"github.com/stacklok/mecatl/internal/adapter/mockllm"
	"github.com/stacklok/mecatl/internal/agent"
	"github.com/stacklok/mecatl/internal/governance"
	"github.com/stacklok/mecatl/internal/team"
)

// teamHooks is a fake port.HookRunner that records the phases it was fired
// for and can optionally veto (Block) every call.
type teamHooks struct {
	mu       sync.Mutex
	phases   []governance.HookPhase
	block    bool
	blockMsg string
}

func (h *teamHooks) Run(_ context.Context, ev governance.HookEvent) (governance.HookOutcome, error) {
	h.mu.Lock()
	h.phases = append(h.phases, ev.Phase)
	h.mu.Unlock()
	if h.block {
		return governance.HookOutcome{Block: true, Message: h.blockMsg}, nil
	}
	return governance.HookOutcome{}, nil
}

func (h *teamHooks) fired(p governance.HookPhase) bool {
	h.mu.Lock()
	defer h.mu.Unlock()
	for _, got := range h.phases {
		if got == p {
			return true
		}
	}
	return false
}

func TestAddTaskFiresTaskCreatedGate(t *testing.T) {
	tm := team.New("t")
	_ = tm.AddMember("alice", "")
	hooks := &teamHooks{}
	add := toolByName(t, agent.MemberTools(tm, "alice", hooks), "AddTask")

	if res := call(t, add, `{"description":"do the thing"}`); res.IsError {
		t.Fatalf("AddTask errored: %s", res.Content)
	}
	if !hooks.fired(governance.PhaseTaskCreated) {
		t.Error("TaskCreated hook was not fired by AddTask")
	}
	if len(tm.Tasks()) != 1 {
		t.Fatalf("want 1 task created, got %d", len(tm.Tasks()))
	}
}

func TestAddTaskVetoedByTaskCreatedGate(t *testing.T) {
	tm := team.New("t")
	_ = tm.AddMember("alice", "")
	hooks := &teamHooks{block: true, blockMsg: "no new tasks allowed"}
	add := toolByName(t, agent.MemberTools(tm, "alice", hooks), "AddTask")

	res := call(t, add, `{"description":"do the thing"}`)
	if !res.IsError {
		t.Fatal("AddTask should be vetoed (error result) when TaskCreated blocks")
	}
	if len(tm.Tasks()) != 0 {
		t.Fatalf("vetoed AddTask still created a task: %+v", tm.Tasks())
	}
}

func TestCompleteTaskVetoedByTaskCompletedGate(t *testing.T) {
	tm := team.New("t")
	_ = tm.AddMember("alice", "")
	hooks := &teamHooks{block: true, blockMsg: "tests must pass first"}
	tools := agent.MemberTools(tm, "alice", hooks)
	id, _ := tm.CreateTask("work")
	_ = tm.ClaimTask(id, "alice")

	complete := toolByName(t, tools, "CompleteTask")
	res := call(t, complete, `{"task_id":"`+string(id)+`"}`)
	if !res.IsError {
		t.Fatal("CompleteTask should be vetoed when TaskCompleted blocks")
	}
	if tm.Tasks()[0].State != team.TaskInProgress {
		t.Fatalf("vetoed CompleteTask still completed the task: %s", tm.Tasks()[0].State)
	}
}

func TestSupervisorFiresTeammateIdle(t *testing.T) {
	tm := team.New("t")
	providers := map[string]*mockllm.Provider{"solo": mockllm.New(mockllm.TextTurn("done"))}
	hooks := &teamHooks{}
	sup := agent.NewSupervisor(tm, memfs.NewWorkspace("/ws"), memberFactory(t, tm, providers),
		agent.WithTeamHooks(hooks), agent.WithMaxRounds(5))

	if err := sup.AddMember(context.Background(), agent.MemberSpec{Name: "solo", InitialPrompt: "do it"}); err != nil {
		t.Fatalf("AddMember: %v", err)
	}
	_ = sup.Run(context.Background(), nil)

	if !hooks.fired(governance.PhaseTeammateIdle) {
		t.Error("TeammateIdle hook was not fired after the member went idle")
	}
}
