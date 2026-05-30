package agent_test

import (
	"context"
	"encoding/json"
	"strings"
	"sync"
	"testing"

	"github.com/stacklok/mecatl/internal/adapter/hookexec"
	"github.com/stacklok/mecatl/internal/adapter/memfs"
	"github.com/stacklok/mecatl/internal/adapter/mockllm"
	"github.com/stacklok/mecatl/internal/adapter/permpolicy"
	"github.com/stacklok/mecatl/internal/agent"
	"github.com/stacklok/mecatl/internal/governance"
	"github.com/stacklok/mecatl/internal/session"
	"github.com/stacklok/mecatl/internal/team"
	"github.com/stacklok/mecatl/internal/tool"
)

// memberFactory builds a per-member Engine whose catalog carries that member's
// coordination tools (bound to tm and self) and whose provider is looked up by
// member name — the per-member scripting the supervisor relies on.
func memberFactory(t *testing.T, tm *team.Team, providers map[string]*mockllm.Provider) agent.MemberEngine {
	t.Helper()
	allow := permpolicy.NewPolicy([]governance.Rule{{Effect: governance.Allow}})
	return func(spec agent.MemberSpec) *agent.Engine {
		prov, ok := providers[spec.Name]
		if !ok {
			t.Fatalf("memberFactory: no provider scripted for member %q", spec.Name)
		}
		cat := tool.NewCatalog()
		for _, tl := range agent.MemberTools(tm, spec.Name, nil) {
			cat.MustRegister(tl)
		}
		return agent.NewEngine(agent.Deps{
			LLM:     prov,
			Catalog: cat,
			Policy:  allow,
			Hooks:   hookexec.New(nil),
			Model:   "mock",
		})
	}
}

// hasToolCall reports whether the tagged event stream contains a tool.call for the
// named tool produced by the named member.
func hasToolCall(events []agent.TeamEvent, member, toolName string) bool {
	for _, ev := range events {
		if ev.Member == member && ev.Event.Type == session.EvToolCall &&
			ev.Event.ToolCall != nil && ev.Event.ToolCall.Name == toolName {
			return true
		}
	}
	return false
}

// TestSupervisorTwoMemberFlow exercises a full lead→worker collaboration entirely
// offline: the lead adds a task, a worker auto-claims it, does the work, completes
// the task and reports back via the mailbox, and the lead synthesises — then the
// team reaches genuine quiescence. It asserts round count, task completion, the
// tagged event stream, and that each member's script was fully consumed.
func TestSupervisorTwoMemberFlow(t *testing.T) {
	tm := team.New("demo")

	addTask := session.NewToolCall("l1", "AddTask",
		json.RawMessage(`{"description":"investigate the reported bug"}`))
	leadProv := mockllm.New(
		mockllm.ToolCallTurn(addTask),                           // round 0, turn 1
		mockllm.TextTurn("Task created; waiting for worker."),   // round 0, turn 2 (ends run)
		mockllm.TextTurn("Worker reports done. Team complete."), // round 2 (after worker's message)
	)

	complete := session.NewToolCall("w1", "CompleteTask", json.RawMessage(`{"task_id":"task-1"}`))
	report := session.NewToolCall("w2", "SendMessage",
		json.RawMessage(`{"to":"lead","body":"done investigating; root cause found"}`))
	workerProv := mockllm.New(
		mockllm.ToolCallTurn(complete, report),      // round 1, turn 1
		mockllm.TextTurn("Investigation complete."), // round 1, turn 2 (ends run)
	)

	providers := map[string]*mockllm.Provider{"lead": leadProv, "worker": workerProv}
	base := memfs.NewWorkspace("/ws")
	sup := agent.NewSupervisor(tm, base, memberFactory(t, tm, providers), agent.WithMaxRounds(10))

	ctx := context.Background()
	if err := sup.AddMember(ctx, agent.MemberSpec{
		Name: "lead", Lead: true,
		InitialPrompt: "Investigate the reported bug. Create a task and let a teammate handle it.",
	}); err != nil {
		t.Fatalf("AddMember(lead): %v", err)
	}
	if err := sup.AddMember(ctx, agent.MemberSpec{Name: "worker"}); err != nil {
		t.Fatalf("AddMember(worker): %v", err)
	}

	var events []agent.TeamEvent
	out := sup.Run(ctx, func(ev agent.TeamEvent) { events = append(events, ev) })

	if !out.Quiescent {
		t.Errorf("team did not reach quiescence: %+v", out)
	}
	if out.Rounds != 3 {
		t.Errorf("rounds = %d, want 3 (lead delegates / worker works / lead synthesises)", out.Rounds)
	}
	if got := leadProv.Calls(); got != 3 {
		t.Errorf("lead consumed %d turns, want 3", got)
	}
	if got := workerProv.Calls(); got != 2 {
		t.Errorf("worker consumed %d turns, want 2", got)
	}

	tasks := tm.Tasks()
	if len(tasks) != 1 || tasks[0].State != team.TaskCompleted {
		t.Fatalf("task state = %+v, want exactly one completed task", tasks)
	}

	if !hasToolCall(events, "lead", "AddTask") {
		t.Error("event stream missing lead's AddTask tool.call")
	}
	if !hasToolCall(events, "worker", "CompleteTask") {
		t.Error("event stream missing worker's CompleteTask tool.call")
	}
	if !hasToolCall(events, "worker", "SendMessage") {
		t.Error("event stream missing worker's SendMessage tool.call")
	}

	last := map[string]string{}
	for _, m := range out.Members {
		last[m.Name] = m.LastText
		if m.Stopped {
			t.Errorf("member %q ended stopped, want a clean finish", m.Name)
		}
	}
	if !strings.Contains(last["lead"], "Team complete") {
		t.Errorf("lead last text = %q, want it to mention completion", last["lead"])
	}
	if !strings.Contains(last["worker"], "Investigation complete") {
		t.Errorf("worker last text = %q", last["worker"])
	}
}

// recordingForker is a fake tool.WorkspaceForker that hands out in-memory
// workspaces and records the fork labels and cleanup calls.
type recordingForker struct {
	mu       sync.Mutex
	labels   []string
	cleanups int
}

func (f *recordingForker) Fork(_ context.Context, _ tool.Workspace, label string) (tool.Workspace, func() error, error) {
	f.mu.Lock()
	f.labels = append(f.labels, label)
	f.mu.Unlock()
	ws := memfs.NewWorkspace("/fork/" + label)
	cleanup := func() error {
		f.mu.Lock()
		f.cleanups++
		f.mu.Unlock()
		return nil
	}
	return ws, cleanup, nil
}

// TestSupervisorMutatingMemberForksWorkspace asserts a Mutating member runs in an
// isolated forked workspace (not the shared base) and that the fork is cleaned up.
func TestSupervisorMutatingMemberForksWorkspace(t *testing.T) {
	tm := team.New("fork-demo")
	providers := map[string]*mockllm.Provider{"impl": mockllm.New(mockllm.TextTurn("done"))}
	base := memfs.NewWorkspace("/ws")
	ff := &recordingForker{}

	sup := agent.NewSupervisor(tm, base, memberFactory(t, tm, providers),
		agent.WithForker(ff), agent.WithMaxRounds(5))

	ctx := context.Background()
	if err := sup.AddMember(ctx, agent.MemberSpec{
		Name: "impl", Mutating: true, InitialPrompt: "make the change",
	}); err != nil {
		t.Fatalf("AddMember(impl): %v", err)
	}

	out := sup.Run(ctx, nil)

	ff.mu.Lock()
	defer ff.mu.Unlock()
	if len(ff.labels) != 1 || ff.labels[0] != "impl" {
		t.Fatalf("forker labels = %v, want [impl] (mutating member forks its workspace)", ff.labels)
	}
	if ff.cleanups != 1 {
		t.Errorf("fork cleanups = %d, want 1", ff.cleanups)
	}
	if !out.Quiescent {
		t.Errorf("single-member team not quiescent: %+v", out)
	}
}

// TestSupervisorRequiresForkerForMutatingMember asserts enrolling a Mutating member
// without a forker is rejected.
func TestSupervisorRequiresForkerForMutatingMember(t *testing.T) {
	tm := team.New("t")
	providers := map[string]*mockllm.Provider{"impl": mockllm.New(mockllm.TextTurn("x"))}
	sup := agent.NewSupervisor(tm, memfs.NewWorkspace("/ws"), memberFactory(t, tm, providers))
	err := sup.AddMember(context.Background(), agent.MemberSpec{Name: "impl", Mutating: true})
	if err == nil {
		t.Fatal("AddMember of a Mutating member without a forker should fail")
	}
}
