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
	allow := permpolicy.NewPolicy([]governance.Rule{{Effect: governance.Allow}}, nil)
	return func(spec agent.MemberSpec) agent.MemberBuild {
		prov, ok := providers[spec.Name]
		if !ok {
			t.Fatalf("memberFactory: no provider scripted for member %q", spec.Name)
		}
		cat := tool.NewCatalog()
		for _, tl := range agent.MemberTools(tm, spec.Name, nil) {
			cat.MustRegister(tl)
		}
		return agent.MemberBuild{Engine: agent.NewEngine(agent.Deps{
			LLM:     prov,
			Catalog: cat,
			Policy:  allow,
			Hooks:   hookexec.New(nil),
			Model:   "mock",
		})}
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

// TestSupervisorStuckTaskQuiescesNoSpin asserts Fix E's stuck-task handling: a
// worker claims a task but its mock NEVER calls CompleteTask and the run then ends
// (the worker stops being scheduled). The team must NOT dead-spin to maxRounds; the
// stopped worker's task is released back to pending and, with no member left to
// claim it, the next round plans no work and the team stops well under the cap.
func TestSupervisorStuckTaskQuiescesNoSpin(t *testing.T) {
	tm := team.New("stuck")

	// The lead adds one task in round 0, then only ever emits text.
	addTask := session.NewToolCall("l1", "AddTask",
		json.RawMessage(`{"description":"do the thing"}`))
	leadProv := mockllm.New(
		mockllm.ToolCallTurn(addTask),
		mockllm.TextTurn("delegated"),
		mockllm.TextTurn("idle"),
		mockllm.TextTurn("idle"),
		mockllm.TextTurn("idle"),
	)
	// The worker claims (auto-claimed by the supervisor) and just reports text —
	// it NEVER calls CompleteTask, leaving its claimed task in_progress.
	workerProv := mockllm.New(
		mockllm.TextTurn("working but never completing"),
		mockllm.TextTurn("still not done"),
		mockllm.TextTurn("nope"),
		mockllm.TextTurn("nope"),
		mockllm.TextTurn("nope"),
	)

	providers := map[string]*mockllm.Provider{"lead": leadProv, "worker": workerProv}
	maxRounds := 8
	sup := agent.NewSupervisor(tm, memfs.NewWorkspace("/ws"),
		memberFactory(t, tm, providers), agent.WithMaxRounds(maxRounds))

	ctx := context.Background()
	if err := sup.AddMember(ctx, agent.MemberSpec{
		Name: "lead", Lead: true, InitialPrompt: "delegate work",
	}); err != nil {
		t.Fatalf("AddMember(lead): %v", err)
	}
	if err := sup.AddMember(ctx, agent.MemberSpec{Name: "worker"}); err != nil {
		t.Fatalf("AddMember(worker): %v", err)
	}

	out := sup.Run(ctx, nil)

	// The team must not run the full cap: once the worker has claimed once and gone
	// idle (no message, no new claimable task since it still holds one), rounds plan
	// no work and the run stops.
	if out.Rounds >= maxRounds {
		t.Errorf("team spun to the round cap (rounds=%d, cap=%d); a stuck task should not dead-spin",
			out.Rounds, maxRounds)
	}
	// A task left in_progress (never completed) means the team is NOT genuinely
	// quiescent — Quiescent distinguishes completion from a stuck dependency.
	if out.Quiescent {
		t.Errorf("team reported quiescent, but a task was never completed: %+v", tm.Tasks())
	}
	tasks := tm.Tasks()
	if len(tasks) != 1 {
		t.Fatalf("tasks = %+v, want exactly one", tasks)
	}
	// The one task ends pending or in_progress — never completed (nobody completed
	// it). It must not be stuck wedging the loop forever, which the round-cap check
	// above guarantees.
	if tasks[0].State == team.TaskCompleted {
		t.Errorf("task state = %s, want it never completed", tasks[0].State)
	}
}

// TestSupervisorMemberHoldsAtMostOneTask asserts Fix E's single-claim bound: a
// worker that claims a task but does not complete it is NOT auto-claimed a second
// task in a later round. The team has two tasks and one worker; with the worker
// never completing, it must hold at most one in_progress task at a time, leaving
// the second pending.
func TestSupervisorMemberHoldsAtMostOneTask(t *testing.T) {
	tm := team.New("oneclaim")
	// Two independent tasks seeded directly on the team so the worker is the only
	// claimant and there is no lead to interfere.
	if _, err := tm.CreateTask("task A"); err != nil {
		t.Fatalf("CreateTask A: %v", err)
	}
	if _, err := tm.CreateTask("task B"); err != nil {
		t.Fatalf("CreateTask B: %v", err)
	}

	// The worker just emits text every round; it never completes its claim.
	workerProv := mockllm.New(
		mockllm.TextTurn("r1"),
		mockllm.TextTurn("r2"),
		mockllm.TextTurn("r3"),
		mockllm.TextTurn("r4"),
	)
	providers := map[string]*mockllm.Provider{"worker": workerProv}

	// A sink that, after each round's events, checks the team never has two
	// in_progress tasks assigned to the worker. Run serialises sink calls.
	var maxInProgress int
	sup := agent.NewSupervisor(tm, memfs.NewWorkspace("/ws"),
		memberFactory(t, tm, providers), agent.WithMaxRounds(6))

	if err := sup.AddMember(context.Background(), agent.MemberSpec{Name: "worker"}); err != nil {
		t.Fatalf("AddMember(worker): %v", err)
	}

	sup.Run(context.Background(), func(agent.TeamEvent) {
		n := 0
		for _, tk := range tm.Tasks() {
			if tk.State == team.TaskInProgress && tk.Assignee == "worker" {
				n++
			}
		}
		if n > maxInProgress {
			maxInProgress = n
		}
	})

	if maxInProgress > 1 {
		t.Errorf("worker held %d in-progress tasks at once, want at most 1", maxInProgress)
	}
	// Exactly one task should ever have been claimed (the worker never frees it), so
	// the other stays pending.
	var pending, inProgress int
	for _, tk := range tm.Tasks() {
		switch tk.State {
		case team.TaskPending:
			pending++
		case team.TaskInProgress:
			inProgress++
		}
	}
	if inProgress != 1 || pending != 1 {
		t.Errorf("task split = %d in_progress / %d pending, want 1/1 (single-claim bound)", inProgress, pending)
	}
}

// TestSupervisorMemberTurnBudgetStops asserts Fix F's lifetime turn budget: a
// member that would otherwise loop forever — it re-queues a message to itself every
// round, so it is always re-scheduled and never finishes — is stopped by the
// cumulative per-member turn budget rather than running to the round cap. The
// per-round Limits cannot do this because session.Reopen resets their counters each
// round; only the lifetime budget, which accumulates across rounds, can.
func TestSupervisorMemberTurnBudgetStops(t *testing.T) {
	tm := team.New("loop")

	// Each round the worker emits a tool call sending a message to ITSELF (so it is
	// planned again next round) and then a line of text (ending the round's run
	// cleanly via StopEndTurn — never an error). The script is long enough that,
	// without a lifetime budget, the worker would be re-scheduled every round up to
	// the (large) round cap. Two turns per round.
	selfPing := session.NewToolCall("p", "SendMessage",
		json.RawMessage(`{"to":"worker","body":"keep going"}`))
	var turns []mockllm.Turn
	for i := 0; i < 60; i++ {
		turns = append(turns, mockllm.ToolCallTurn(selfPing), mockllm.TextTurn("still working"))
	}
	workerProv := mockllm.New(turns...)
	providers := map[string]*mockllm.Provider{"worker": workerProv}

	const maxRounds = 30
	const budget = 5 // lifetime turns; ~2 turns/round → stops after ~3 rounds
	sup := agent.NewSupervisor(tm, memfs.NewWorkspace("/ws"),
		memberFactory(t, tm, providers),
		agent.WithMaxRounds(maxRounds),
		agent.WithMemberTurnBudget(budget),
	)

	if err := sup.AddMember(context.Background(), agent.MemberSpec{
		Name: "worker", InitialPrompt: "begin the loop and ping yourself each round",
	}); err != nil {
		t.Fatalf("AddMember(worker): %v", err)
	}

	out := sup.Run(context.Background(), nil)

	// The worker must have been stopped by the budget, well short of the round cap.
	if out.Rounds >= maxRounds {
		t.Errorf("team ran to the round cap (rounds=%d, cap=%d); the lifetime turn budget should have stopped the looping member",
			out.Rounds, maxRounds)
	}
	if len(out.Members) != 1 || !out.Members[0].Stopped {
		t.Fatalf("worker outcome = %+v, want it stopped by the budget", out.Members)
	}
	// It must not have consumed anywhere near maxRounds worth of turns: the budget is
	// a hard ceiling the per-round-reset counters could not provide. Allow one extra
	// round's worth of turns (the round in which the budget is crossed runs to
	// completion before the member is marked stopped).
	if got := workerProv.Calls(); got > budget+4 {
		t.Errorf("worker consumed %d turns, want bounded near the budget %d", got, budget)
	}
}

// limitedMemberFactory builds a member factory that returns the given per-member
// Limits on every MemberBuild (the composition-layer analogue: a def's
// maxTurns/maxToolCalls mapped onto MemberBuild.Limits). The engine carries only
// the member's coordination tools.
func limitedMemberFactory(t *testing.T, tm *team.Team, providers map[string]*mockllm.Provider, limits session.Limits) agent.MemberEngine {
	t.Helper()
	allow := permpolicy.NewPolicy([]governance.Rule{{Effect: governance.Allow}}, nil)
	return func(spec agent.MemberSpec) agent.MemberBuild {
		prov, ok := providers[spec.Name]
		if !ok {
			t.Fatalf("limitedMemberFactory: no provider scripted for member %q", spec.Name)
		}
		cat := tool.NewCatalog()
		for _, tl := range agent.MemberTools(tm, spec.Name, nil) {
			cat.MustRegister(tl)
		}
		return agent.MemberBuild{
			Engine: agent.NewEngine(agent.Deps{LLM: prov, Catalog: cat, Policy: allow, Hooks: hookexec.New(nil), Model: "mock"}),
			Limits: limits,
		}
	}
}

// TestSupervisorMemberLimitsBindSession proves a member's per-def Limits
// (MemberBuild.Limits) bound its per-round session: with MaxTurns=2 the member makes
// exactly 2 model calls in round 0, even though its script and the team-wide default
// (WithTeamLimits) would allow many more. WithMaxRounds(1) isolates a single round.
func TestSupervisorMemberLimitsBindSession(t *testing.T) {
	tm := team.New("limits")

	// A self-pinging worker that would loop indefinitely; two turns per round.
	selfPing := session.NewToolCall("p", "SendMessage", json.RawMessage(`{"to":"worker","body":"again"}`))
	var turns []mockllm.Turn
	for i := 0; i < 12; i++ {
		turns = append(turns, mockllm.ToolCallTurn(selfPing))
	}
	workerProv := mockllm.New(turns...)
	providers := map[string]*mockllm.Provider{"worker": workerProv}

	sup := agent.NewSupervisor(tm, memfs.NewWorkspace("/ws"),
		limitedMemberFactory(t, tm, providers, session.Limits{MaxTurns: 2, MaxToolCalls: 40, MaxConsecutiveFailures: 3}),
		agent.WithMaxRounds(1),
		// A deliberately LOOSE team default, so the per-member override (2) is what bites.
		agent.WithTeamLimits(session.Limits{MaxTurns: 20, MaxToolCalls: 200, MaxConsecutiveFailures: 5}),
	)
	if err := sup.AddMember(context.Background(), agent.MemberSpec{Name: "worker", InitialPrompt: "loop"}); err != nil {
		t.Fatalf("AddMember(worker): %v", err)
	}

	sup.Run(context.Background(), nil)

	if got := workerProv.Calls(); got != 2 {
		t.Fatalf("member made %d model calls in one round, want 2 (MemberBuild.Limits MaxTurns=2 should bind the session)", got)
	}
}

// TestSupervisorMemberZeroLimitsUsesTeamDefault proves a member with a ZERO
// MemberBuild.Limits runs under the team-wide default (WithTeamLimits), per-field:
// with the team default MaxTurns=3 the member makes 3 model calls in round 0.
func TestSupervisorMemberZeroLimitsUsesTeamDefault(t *testing.T) {
	tm := team.New("default")

	selfPing := session.NewToolCall("p", "SendMessage", json.RawMessage(`{"to":"worker","body":"again"}`))
	var turns []mockllm.Turn
	for i := 0; i < 12; i++ {
		turns = append(turns, mockllm.ToolCallTurn(selfPing))
	}
	workerProv := mockllm.New(turns...)
	providers := map[string]*mockllm.Provider{"worker": workerProv}

	sup := agent.NewSupervisor(tm, memfs.NewWorkspace("/ws"),
		limitedMemberFactory(t, tm, providers, session.Limits{}), // zero => team default
		agent.WithMaxRounds(1),
		agent.WithTeamLimits(session.Limits{MaxTurns: 3, MaxToolCalls: 200, MaxConsecutiveFailures: 5}),
	)
	if err := sup.AddMember(context.Background(), agent.MemberSpec{Name: "worker", InitialPrompt: "loop"}); err != nil {
		t.Fatalf("AddMember(worker): %v", err)
	}

	sup.Run(context.Background(), nil)

	if got := workerProv.Calls(); got != 3 {
		t.Fatalf("member made %d model calls in one round, want 3 (zero MemberBuild.Limits falls back to the team default MaxTurns=3)", got)
	}
}

// TestSupervisorMemberPartialLimitsMergePerField proves the per-field merge: a
// member that pins ONLY MaxTurns keeps the team default for the other fields. With a
// per-member MaxTurns=2 over a team default MaxTurns=9, the member is bounded to 2.
func TestSupervisorMemberPartialLimitsMergePerField(t *testing.T) {
	tm := team.New("merge")

	selfPing := session.NewToolCall("p", "SendMessage", json.RawMessage(`{"to":"worker","body":"again"}`))
	var turns []mockllm.Turn
	for i := 0; i < 12; i++ {
		turns = append(turns, mockllm.ToolCallTurn(selfPing))
	}
	workerProv := mockllm.New(turns...)
	providers := map[string]*mockllm.Provider{"worker": workerProv}

	sup := agent.NewSupervisor(tm, memfs.NewWorkspace("/ws"),
		limitedMemberFactory(t, tm, providers, session.Limits{MaxTurns: 2}), // only MaxTurns pinned
		agent.WithMaxRounds(1),
		agent.WithTeamLimits(session.Limits{MaxTurns: 9, MaxToolCalls: 200, MaxConsecutiveFailures: 5}),
	)
	if err := sup.AddMember(context.Background(), agent.MemberSpec{Name: "worker", InitialPrompt: "loop"}); err != nil {
		t.Fatalf("AddMember(worker): %v", err)
	}

	sup.Run(context.Background(), nil)

	if got := workerProv.Calls(); got != 2 {
		t.Fatalf("member made %d model calls, want 2 (per-member MaxTurns=2 overrides the team default 9)", got)
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

// fakeMutatingTool is a stand-in workspace-mutating tool: it reports
// ReadOnly()==false and carries an arbitrary name (e.g. "Edit"). It lets the team
// tests assert the supervisor's catalog inspection without importing the
// adapter-layer concrete tool types (which the layering rule forbids).
type fakeMutatingTool struct{ name string }

func (f fakeMutatingTool) Spec() tool.ToolSpec {
	return tool.ToolSpec{Name: f.name, Description: f.name, Schema: json.RawMessage(`{"type":"object"}`)}
}
func (fakeMutatingTool) ReadOnly() bool { return false }
func (fakeMutatingTool) Execute(_ context.Context, c session.ToolCall, _ tool.Workspace) (session.ToolResult, error) {
	return session.NewToolResult(c.ID, "ok"), nil
}

// catalogFactory builds a member Engine whose catalog is exactly the supplied
// tools plus the member's coordination tools — the seam Fix A's tests drive.
func catalogFactory(t *testing.T, tm *team.Team, extra ...tool.Tool) agent.MemberEngine {
	t.Helper()
	allow := permpolicy.NewPolicy([]governance.Rule{{Effect: governance.Allow}}, nil)
	return func(spec agent.MemberSpec) agent.MemberBuild {
		cat := tool.NewCatalog()
		for _, tl := range agent.MemberTools(tm, spec.Name, nil) {
			cat.MustRegister(tl)
		}
		for _, tl := range extra {
			cat.MustRegister(tl)
		}
		return agent.MemberBuild{Engine: agent.NewEngine(agent.Deps{
			LLM:     mockllm.New(mockllm.TextTurn("x")),
			Catalog: cat,
			Policy:  allow,
			Model:   "mock",
		})}
	}
}

// TestSupervisorRejectsReadOnlyMemberWithMutatingTool asserts Fix A: a read-only
// (base-sharing) member whose factory hands back a WORKSPACE-mutating tool is
// rejected by AddMember — the supervisor is authoritative and will not let a
// base-sharing member corrupt the shared workspace.
func TestSupervisorRejectsReadOnlyMemberWithMutatingTool(t *testing.T) {
	tm := team.New("t")
	sup := agent.NewSupervisor(tm, memfs.NewWorkspace("/ws"),
		catalogFactory(t, tm, fakeMutatingTool{name: "Edit"}))
	err := sup.AddMember(context.Background(), agent.MemberSpec{Name: "ro"})
	if err == nil {
		t.Fatal("AddMember of a read-only member with a workspace-mutating tool should be rejected")
	}
	if !strings.Contains(err.Error(), "Edit") {
		t.Errorf("error %q should name the offending tool", err)
	}
	// The rejected member must not linger on the roster.
	if got := tm.Members(); len(got) != 0 {
		t.Errorf("roster = %v, want empty after rejection", got)
	}
}

// TestSupervisorAcceptsReadOnlyMemberWithCoordinationTools asserts the CRUCIAL
// nuance: the coordination tools report ReadOnly()==false but only mutate TEAM
// state, so a read-only member carrying just those is ACCEPTED.
func TestSupervisorAcceptsReadOnlyMemberWithCoordinationTools(t *testing.T) {
	tm := team.New("t")
	sup := agent.NewSupervisor(tm, memfs.NewWorkspace("/ws"), catalogFactory(t, tm))
	if err := sup.AddMember(context.Background(), agent.MemberSpec{Name: "ro"}); err != nil {
		t.Fatalf("read-only member with only coordination tools should be accepted: %v", err)
	}
}

// TestSupervisorAcceptsMutatingMemberWithMutatingTool asserts a Mutating member —
// which runs in its own isolated fork — MAY carry the same workspace-mutating tool.
func TestSupervisorAcceptsMutatingMemberWithMutatingTool(t *testing.T) {
	tm := team.New("t")
	sup := agent.NewSupervisor(tm, memfs.NewWorkspace("/ws"),
		catalogFactory(t, tm, fakeMutatingTool{name: "Edit"}),
		agent.WithForker(&recordingForker{}))
	if err := sup.AddMember(context.Background(), agent.MemberSpec{Name: "impl", Mutating: true}); err != nil {
		t.Fatalf("mutating member with a workspace-mutating tool should be accepted: %v", err)
	}
}

// TestSupervisorAcceptsReadOnlyMemberWithMCPTool asserts the per-agent-MCP backstop
// exemption: a read-only member whose catalog holds a tool that reports
// ReadOnly()==false but is named in MemberBuild.MCPToolNames (an MCP tool — never
// touches the workspace) is ACCEPTED, exactly like a coordination tool.
func TestSupervisorAcceptsReadOnlyMemberWithMCPTool(t *testing.T) {
	tm := team.New("t")
	mcpTool := fakeMutatingTool{name: "mcp__remote__do"}
	factory := func(spec agent.MemberSpec) agent.MemberBuild {
		b := catalogFactory(t, tm, mcpTool)(spec)
		b.MCPToolNames = []string{"mcp__remote__do"}
		return b
	}
	sup := agent.NewSupervisor(tm, memfs.NewWorkspace("/ws"), factory)
	if err := sup.AddMember(context.Background(), agent.MemberSpec{Name: "ro"}); err != nil {
		t.Fatalf("read-only member holding an exempt MCP tool should be accepted: %v", err)
	}
}

// TestSupervisorStillRejectsRealMutatingDespiteMCPExempt asserts the exemption is
// SCOPED to the named MCP tools: a read-only member that ALSO holds a genuine
// workspace-mutating tool (not in MCPToolNames) is still rejected.
func TestSupervisorStillRejectsRealMutatingDespiteMCPExempt(t *testing.T) {
	tm := team.New("t")
	factory := func(spec agent.MemberSpec) agent.MemberBuild {
		b := catalogFactory(t, tm, fakeMutatingTool{name: "mcp__remote__do"}, fakeMutatingTool{name: "Edit"})(spec)
		b.MCPToolNames = []string{"mcp__remote__do"} // exempt the MCP tool only
		return b
	}
	sup := agent.NewSupervisor(tm, memfs.NewWorkspace("/ws"), factory)
	err := sup.AddMember(context.Background(), agent.MemberSpec{Name: "ro"})
	if err == nil || !strings.Contains(err.Error(), "Edit") {
		t.Fatalf("read-only member with a real mutating tool must still be rejected naming Edit, got %v", err)
	}
}

// TestSupervisorRunsMemberCloseOnCleanup asserts the MemberBuild.Close teardown seam:
// the supervisor composes build.Close with the fork cleanup and runs it on Run's
// cleanupAll, so a per-member inline MCP manager is torn down (no leak).
func TestSupervisorRunsMemberCloseOnCleanup(t *testing.T) {
	tm := team.New("t")
	var closed int
	factory := func(spec agent.MemberSpec) agent.MemberBuild {
		b := catalogFactory(t, tm)(spec)
		b.Close = func() error { closed++; return nil }
		return b
	}
	sup := agent.NewSupervisor(tm, memfs.NewWorkspace("/ws"), factory)
	if err := sup.AddMember(context.Background(), agent.MemberSpec{Name: "ro", InitialPrompt: "go"}); err != nil {
		t.Fatalf("AddMember: %v", err)
	}
	sup.Run(context.Background(), nil)
	if closed != 1 {
		t.Fatalf("member Close should run exactly once on cleanup, ran %d times", closed)
	}
}
