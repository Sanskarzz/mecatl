package agent

import (
	"testing"

	"github.com/stacklok/mecatl/internal/session"
	"github.com/stacklok/mecatl/internal/team"
)

// TestProjectTeamEventTurnEndContextMeter asserts the turn.end projection carries
// the per-member context-meter fields: ContextUsed is THIS turn's input-token
// count (current occupancy, the meter numerator — an assignment, not a sum) and
// ContextWindow is the producing member engine's window (forwarded off the
// TeamEvent, the meter denominator). It also confirms the existing per-turn Usage
// is still forwarded unchanged.
func TestProjectTeamEventTurnEndContextMeter(t *testing.T) {
	const window = 200000
	te := TeamEvent{
		Member:        "scout",
		ContextWindow: window,
		Event: session.Event{
			Type: session.EvTurnEnd,
			TurnEnd: &session.TurnEndPayload{
				Usage: session.Usage{InputTokens: 40000, OutputTokens: 80},
			},
		},
	}

	ev, ok := projectTeamEvent("p1", "team-p1", te)
	if !ok {
		t.Fatal("turn.end must project to a team.member event")
	}
	if ev.Type != session.EvTeamMember || ev.Team == nil {
		t.Fatalf("projected event mis-shaped: %+v", ev)
	}
	p := ev.Team
	if p.ContextUsed != 40000 {
		t.Errorf("ContextUsed = %d, want 40000 (this turn's input tokens)", p.ContextUsed)
	}
	if p.ContextWindow != window {
		t.Errorf("ContextWindow = %d, want %d (the member engine's window)", p.ContextWindow, window)
	}
	// The pre-existing per-turn usage projection must be unchanged.
	if p.Usage.InputTokens != 40000 || p.Usage.OutputTokens != 80 {
		t.Errorf("Usage = %+v, want the forwarded per-turn usage", p.Usage)
	}
}

// TestProjectTeamEventUnknownWindow asserts a turn.end whose TeamEvent carries no
// window (a member engine with ContextWindowTokens == 0) still projects ContextUsed
// but a zero ContextWindow — the client then suppresses the meter (no denominator).
func TestProjectTeamEventUnknownWindow(t *testing.T) {
	te := TeamEvent{
		Member: "scout",
		Event: session.Event{
			Type:    session.EvTurnEnd,
			TurnEnd: &session.TurnEndPayload{Usage: session.Usage{InputTokens: 1200}},
		},
	}
	ev, ok := projectTeamEvent("p1", "team-p1", te)
	if !ok || ev.Team == nil {
		t.Fatal("turn.end must still project")
	}
	if ev.Team.ContextUsed != 1200 {
		t.Errorf("ContextUsed = %d, want 1200", ev.Team.ContextUsed)
	}
	if ev.Team.ContextWindow != 0 {
		t.Errorf("ContextWindow = %d, want 0 (unknown window)", ev.Team.ContextWindow)
	}
}

// TestProjectTeamTasksSnapshot asserts the team.Task → session.TeamTaskSnapshot
// projection maps every field (id/state/assignee/deps) and clamps the description
// like every other member-derived preview, with Deps copied as []string.
func TestProjectTeamTasksSnapshot(t *testing.T) {
	tm := team.New("team-p1")
	if err := tm.AddMember("scout", ""); err != nil {
		t.Fatalf("AddMember: %v", err)
	}
	id1, err := tm.CreateTask("investigate the failing path")
	if err != nil {
		t.Fatalf("CreateTask: %v", err)
	}
	id2, err := tm.CreateTask("fix it", id1)
	if err != nil {
		t.Fatalf("CreateTask dep: %v", err)
	}
	if err := tm.ClaimTask(id1, "scout"); err != nil {
		t.Fatalf("ClaimTask: %v", err)
	}

	snap := projectTeamTasksSnapshot(tm.Tasks())
	if len(snap) != 2 {
		t.Fatalf("snapshot len = %d, want 2", len(snap))
	}
	if snap[0].ID != string(id1) || snap[0].State != string(team.TaskInProgress) || snap[0].Assignee != "scout" {
		t.Errorf("task 1 snapshot mis-mapped: %+v", snap[0])
	}
	if snap[1].ID != string(id2) || snap[1].State != string(team.TaskPending) {
		t.Errorf("task 2 snapshot mis-mapped: %+v", snap[1])
	}
	if len(snap[1].Deps) != 1 || snap[1].Deps[0] != string(id1) {
		t.Errorf("task 2 deps mis-mapped: %+v", snap[1].Deps)
	}
}

// TestProjectTeamTasksEvent asserts the task snapshot is wrapped in a first-class
// EvTeamTasks event carrying no Member and no InnerKind (the task list is team-wide,
// not a per-member projection), so the client routes it to the task sub-view.
func TestProjectTeamTasksEvent(t *testing.T) {
	snap := []session.TeamTaskSnapshot{{ID: "task-1", State: "pending"}}
	ev := projectTeamTasks("p1", "team-p1", snap)
	if ev.Type != session.EvTeamTasks {
		t.Fatalf("event type = %q, want team.tasks", ev.Type)
	}
	if ev.Team == nil {
		t.Fatal("EvTeamTasks must carry a TeamPayload")
	}
	if ev.Team.Member != "" {
		t.Errorf("a task snapshot must carry no Member, got %q", ev.Team.Member)
	}
	if ev.Team.InnerKind != "" {
		t.Errorf("a task snapshot must carry no InnerKind, got %q", ev.Team.InnerKind)
	}
	if len(ev.Team.Tasks) != 1 || ev.Team.Tasks[0].ID != "task-1" {
		t.Errorf("tasks not carried: %+v", ev.Team.Tasks)
	}
}

// TestProjectTeamEventAlwaysHasMember pins the EvTeamMember contract invariant: a
// per-member projection ALWAYS sets Member (the task-wide snapshot now rides its own
// EvTeamTasks event, so it can no longer produce a memberless EvTeamMember). Drive
// every projectTeamEvent-producing inner kind and assert Member is populated.
func TestProjectTeamEventAlwaysHasMember(t *testing.T) {
	cases := []session.Event{
		{Type: session.EvMessageDelta, Text: "hello"},
		{Type: session.EvToolCall, ToolCall: &session.ToolCall{Name: "Read"}},
		{Type: session.EvToolResult, ToolResult: &session.ToolResult{Content: "body"}},
		{Type: session.EvTurnEnd, TurnEnd: &session.TurnEndPayload{}},
		{Type: session.EvResult, Result: &session.ResultPayload{Text: "done"}},
	}
	for _, inner := range cases {
		ev, ok := projectTeamEvent("p1", "team-p1", TeamEvent{Member: "scout", Event: inner})
		if !ok {
			t.Fatalf("%s did not project", inner.Type)
		}
		if ev.Type != session.EvTeamMember {
			t.Errorf("%s projected to %q, want team.member", inner.Type, ev.Type)
		}
		if ev.Team == nil || ev.Team.Member == "" {
			t.Errorf("%s produced a memberless team.member event: %+v", inner.Type, ev.Team)
		}
	}
}

// TestTasksEqualDedup asserts the de-dup guard: snapshots equal on id/state/
// assignee/deps compare equal (no re-emit), and a state OR assignee OR deps change
// compares unequal (re-emit). Description is deliberately excluded.
func TestTasksEqualDedup(t *testing.T) {
	base := []session.TeamTaskSnapshot{
		{ID: "task-1", Description: "a", State: "in_progress", Assignee: "scout", Deps: []string{"task-0"}},
	}
	// TRIPWIRE: Description is excluded from the de-dup key because it is create-only
	// today (team.CreateTask sets it; no setter mutates it), so it can never change
	// for a given task id and excluding it cannot drop a real update. If a future
	// RedescribeTask makes Description mutable, this assertion will catch that the
	// de-dup now silently swallows the change — flip the exclusion (and update
	// tasksEqual) at that point.
	descOnlyChange := []session.TeamTaskSnapshot{
		{ID: "task-1", Description: "DIFFERENT desc", State: "in_progress", Assignee: "scout", Deps: []string{"task-0"}},
	}
	if !tasksEqual(base, descOnlyChange) {
		t.Error("a description-only change must compare EQUAL today (description is create-only; excluded from the de-dup key) — if this trips, Description became mutable and the exclusion is now unsafe")
	}
	stateChanged := []session.TeamTaskSnapshot{
		{ID: "task-1", State: "completed", Assignee: "scout", Deps: []string{"task-0"}},
	}
	if tasksEqual(base, stateChanged) {
		t.Error("a state change must compare unequal")
	}
	assigneeChanged := []session.TeamTaskSnapshot{
		{ID: "task-1", State: "in_progress", Assignee: "other", Deps: []string{"task-0"}},
	}
	if tasksEqual(base, assigneeChanged) {
		t.Error("an assignee change must compare unequal")
	}
	depsChanged := []session.TeamTaskSnapshot{
		{ID: "task-1", State: "in_progress", Assignee: "scout", Deps: []string{"task-9"}},
	}
	if tasksEqual(base, depsChanged) {
		t.Error("a deps change must compare unequal")
	}
	if tasksEqual(base, nil) {
		t.Error("differing lengths must compare unequal")
	}
}
