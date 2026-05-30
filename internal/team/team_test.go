package team_test

import (
	"sync"
	"testing"

	"github.com/stacklok/mecatl/internal/session"
	"github.com/stacklok/mecatl/internal/team"
)

// newTeamWith builds a team with the given member names enrolled.
func newTeamWith(t *testing.T, names ...string) *team.Team {
	t.Helper()
	tm := team.New("spike")
	for _, n := range names {
		if err := tm.AddMember(n, ""); err != nil {
			t.Fatalf("AddMember(%q): %v", n, err)
		}
	}
	return tm
}

func TestAddMemberRejectsDuplicate(t *testing.T) {
	tm := newTeamWith(t, "alice")
	if err := tm.AddMember("alice", ""); err == nil {
		t.Fatal("expected ErrMemberExists for duplicate member, got nil")
	}
	if got := tm.Members(); len(got) != 1 {
		t.Fatalf("roster size = %d, want 1", len(got))
	}
}

func TestSetMemberSessionAndState(t *testing.T) {
	tm := newTeamWith(t, "alice")
	if err := tm.SetMemberSession("alice", session.SessionID("sess-1")); err != nil {
		t.Fatalf("SetMemberSession: %v", err)
	}
	if err := tm.SetMemberState("alice", team.MemberWorking); err != nil {
		t.Fatalf("SetMemberState: %v", err)
	}
	m := tm.Members()[0]
	if m.Session != "sess-1" || m.State != team.MemberWorking {
		t.Fatalf("member = %+v, want session sess-1 / working", m)
	}
	if err := tm.SetMemberState("ghost", team.MemberIdle); err == nil {
		t.Fatal("expected ErrUnknownMember for unknown member, got nil")
	}
}

// TestDependencyGating asserts a task with an incomplete dependency is not
// claimable until that dependency is completed.
func TestDependencyGating(t *testing.T) {
	tm := newTeamWith(t, "alice")

	a, err := tm.CreateTask("build A")
	if err != nil {
		t.Fatalf("CreateTask A: %v", err)
	}
	b, err := tm.CreateTask("build B (needs A)", a)
	if err != nil {
		t.Fatalf("CreateTask B: %v", err)
	}

	// First claim returns A (B is blocked by A).
	got, ok, err := tm.ClaimNext("alice")
	if err != nil || !ok {
		t.Fatalf("ClaimNext #1: ok=%v err=%v", ok, err)
	}
	if got.ID != a {
		t.Fatalf("claimed %q, want A (%q) — B should be blocked", got.ID, a)
	}

	// B is still blocked: nothing else claimable while A is in progress.
	if _, ok, _ := tm.ClaimNext("alice"); ok {
		t.Fatal("ClaimNext #2 claimed a task while B's dependency A is incomplete")
	}

	// Explicit claim of B must also be refused.
	if err := tm.ClaimTask(b, "alice"); err == nil {
		t.Fatal("ClaimTask(B) succeeded while A incomplete; want ErrTaskNotClaimable")
	}

	// Complete A; now B becomes claimable.
	if err := tm.CompleteTask(a, "alice"); err != nil {
		t.Fatalf("CompleteTask A: %v", err)
	}
	got, ok, err = tm.ClaimNext("alice")
	if err != nil || !ok {
		t.Fatalf("ClaimNext after A done: ok=%v err=%v", ok, err)
	}
	if got.ID != b {
		t.Fatalf("claimed %q after A done, want B (%q)", got.ID, b)
	}
}

// TestCompleteTaskGuards asserts CompleteTask rejects wrong-state / wrong-owner.
func TestCompleteTaskGuards(t *testing.T) {
	tm := newTeamWith(t, "alice", "bob")
	a, _ := tm.CreateTask("work")

	// Cannot complete a pending (unclaimed) task.
	if err := tm.CompleteTask(a, "alice"); err == nil {
		t.Fatal("CompleteTask on pending task succeeded; want ErrTaskState")
	}
	if err := tm.ClaimTask(a, "alice"); err != nil {
		t.Fatalf("ClaimTask: %v", err)
	}
	// Cannot complete a task you don't own.
	if err := tm.CompleteTask(a, "bob"); err == nil {
		t.Fatal("CompleteTask by non-assignee succeeded; want ErrTaskState")
	}
	if err := tm.CompleteTask(a, "alice"); err != nil {
		t.Fatalf("CompleteTask by assignee: %v", err)
	}
}

func TestCreateTaskUnknownDep(t *testing.T) {
	tm := newTeamWith(t, "alice")
	if _, err := tm.CreateTask("orphan", team.TaskID("task-999")); err == nil {
		t.Fatal("CreateTask with unknown dependency succeeded; want ErrUnknownTask")
	}
}

// TestConcurrentClaimNoDoubleAssign spins many goroutines all racing to claim from
// a fixed pool of tasks and asserts each task is claimed exactly once and the total
// number of successful claims equals the pool size. Run under -race.
func TestConcurrentClaimNoDoubleAssign(t *testing.T) {
	const (
		workers = 8
		tasks   = 50
	)
	names := make([]string, workers)
	for i := range names {
		names[i] = workerName(i)
	}
	tm := newTeamWith(t, names...)
	for i := 0; i < tasks; i++ {
		if _, err := tm.CreateTask("unit"); err != nil {
			t.Fatalf("CreateTask: %v", err)
		}
	}

	var (
		mu      sync.Mutex
		claimed = make(map[team.TaskID]string)
		total   int
		wg      sync.WaitGroup
	)
	for w := 0; w < workers; w++ {
		wg.Add(1)
		go func(name string) {
			defer wg.Done()
			for {
				task, ok, err := tm.ClaimNext(name)
				if err != nil {
					t.Errorf("ClaimNext(%q): %v", name, err)
					return
				}
				if !ok {
					return // pool drained
				}
				mu.Lock()
				if prev, dup := claimed[task.ID]; dup {
					t.Errorf("task %q double-claimed by %q and %q", task.ID, prev, name)
				}
				claimed[task.ID] = name
				total++
				mu.Unlock()
			}
		}(names[w])
	}
	wg.Wait()

	if total != tasks {
		t.Fatalf("total claims = %d, want %d", total, tasks)
	}
	if len(claimed) != tasks {
		t.Fatalf("distinct claimed tasks = %d, want %d", len(claimed), tasks)
	}
}

// TestMailboxDelivery asserts at-most-once delivery and unknown-recipient errors.
func TestMailboxDelivery(t *testing.T) {
	tm := newTeamWith(t, "lead", "alice")

	if err := tm.Send("lead", "alice", "hello"); err != nil {
		t.Fatalf("Send: %v", err)
	}
	if err := tm.Send("lead", "alice", "world"); err != nil {
		t.Fatalf("Send: %v", err)
	}
	if err := tm.Send("lead", "ghost", "nope"); err == nil {
		t.Fatal("Send to unknown recipient succeeded; want ErrUnknownMember")
	}

	msgs, err := tm.Drain("alice")
	if err != nil {
		t.Fatalf("Drain: %v", err)
	}
	if len(msgs) != 2 || msgs[0].Body != "hello" || msgs[1].Body != "world" {
		t.Fatalf("drained %+v, want [hello, world] in order", msgs)
	}
	if msgs[0].Seq >= msgs[1].Seq {
		t.Fatalf("message Seq not increasing: %d then %d", msgs[0].Seq, msgs[1].Seq)
	}

	// Second drain is empty (at-most-once).
	again, err := tm.Drain("alice")
	if err != nil {
		t.Fatalf("Drain #2: %v", err)
	}
	if len(again) != 0 {
		t.Fatalf("second drain returned %d messages, want 0", len(again))
	}
}

// TestQuiescence walks the team from "work outstanding" to "done" and asserts the
// predicate flips only when tasks are complete, mailboxes empty, and no member is
// working.
func TestQuiescence(t *testing.T) {
	tm := newTeamWith(t, "alice")

	a, _ := tm.CreateTask("only task")
	if tm.Quiescent() {
		t.Fatal("quiescent with a pending task")
	}

	if _, _, err := tm.ClaimNext("alice"); err != nil {
		t.Fatalf("ClaimNext: %v", err)
	}
	_ = tm.SetMemberState("alice", team.MemberWorking)
	if tm.Quiescent() {
		t.Fatal("quiescent with an in-progress task and a working member")
	}

	if err := tm.CompleteTask(a, "alice"); err != nil {
		t.Fatalf("CompleteTask: %v", err)
	}
	// Task done but a message is outstanding → not quiescent.
	_ = tm.Send("alice", "alice", "note to self")
	_ = tm.SetMemberState("alice", team.MemberIdle)
	if tm.Quiescent() {
		t.Fatal("quiescent with an undelivered message")
	}

	if _, err := tm.Drain("alice"); err != nil {
		t.Fatalf("Drain: %v", err)
	}
	if !tm.Quiescent() {
		t.Fatal("not quiescent after all tasks complete, mailbox empty, member idle")
	}
}

func workerName(i int) string {
	return "w" + string(rune('a'+i))
}

func TestReleaseTasksFreesInProgressForReclaim(t *testing.T) {
	tm := newTeamWith(t, "alice", "bob")
	a, _ := tm.CreateTask("in progress")
	done, _ := tm.CreateTask("finished")
	pending, _ := tm.CreateTask("untouched")

	if err := tm.ClaimTask(a, "alice"); err != nil {
		t.Fatalf("ClaimTask a: %v", err)
	}
	if err := tm.ClaimTask(done, "alice"); err != nil {
		t.Fatalf("ClaimTask done: %v", err)
	}
	if err := tm.CompleteTask(done, "alice"); err != nil {
		t.Fatalf("CompleteTask done: %v", err)
	}

	tm.ReleaseTasks("alice")

	byID := map[team.TaskID]team.Task{}
	for _, task := range tm.Tasks() {
		byID[task.ID] = task
	}
	if got := byID[a]; got.State != team.TaskPending || got.Assignee != "" {
		t.Fatalf("released task = %+v, want pending/unassigned", got)
	}
	if got := byID[done]; got.State != team.TaskCompleted {
		t.Fatalf("completed task = %+v, want still completed (not released)", got)
	}
	if got := byID[pending]; got.State != team.TaskPending {
		t.Fatalf("untouched task = %+v, want still pending", got)
	}
	// The released task is claimable again, by anyone.
	if claimed, ok, _ := tm.ClaimNext("bob"); !ok || claimed.ID != a {
		t.Fatalf("released task not reclaimable: claimed=%q ok=%v", claimed.ID, ok)
	}
}
