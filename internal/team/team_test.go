package team_test

import (
	"errors"
	"fmt"
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

// TestClaimDepsAreDeepCopied asserts the claim paths return a Task whose Deps
// slice does not alias the aggregate's backing array, so a caller mutating the
// returned slice cannot corrupt team state outside the lock (matching Tasks()).
func TestClaimDepsAreDeepCopied(t *testing.T) {
	tm := newTeamWith(t, "alice")
	a, err := tm.CreateTask("A")
	if err != nil {
		t.Fatalf("CreateTask A: %v", err)
	}
	b, err := tm.CreateTask("B (needs A)", a)
	if err != nil {
		t.Fatalf("CreateTask B: %v", err)
	}
	if err := tm.CompleteTask(a, "alice"); err != nil {
		// Claim+complete A so B is claimable.
		if _, _, cerr := tm.ClaimNext("alice"); cerr != nil {
			t.Fatalf("ClaimNext A: %v", cerr)
		}
		if err := tm.CompleteTask(a, "alice"); err != nil {
			t.Fatalf("CompleteTask A: %v", err)
		}
	}

	got, ok, err := tm.ClaimNext("alice")
	if err != nil || !ok || got.ID != b {
		t.Fatalf("ClaimNext B: got=%q ok=%v err=%v", got.ID, ok, err)
	}
	if len(got.Deps) != 1 || got.Deps[0] != a {
		t.Fatalf("claimed B Deps = %v, want [%q]", got.Deps, a)
	}

	// Mutate the returned Deps; the aggregate snapshot must be unaffected.
	got.Deps[0] = "tampered"
	for _, task := range tm.Tasks() {
		if task.ID == b {
			if len(task.Deps) != 1 || task.Deps[0] != a {
				t.Fatalf("aggregate B Deps were corrupted via the claimed copy: %v", task.Deps)
			}
		}
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

// TestSendAuthenticatesSender asserts Send accepts a real member and the reserved
// operator identity as `from`, but rejects any other forged sender (Fix C): a
// caller must not be able to impersonate the lead or invent a sender label.
func TestSendAuthenticatesSender(t *testing.T) {
	tm := newTeamWith(t, "lead", "alice")

	// A real member may send.
	if err := tm.Send("alice", "lead", "status"); err != nil {
		t.Fatalf("Send from a member: %v", err)
	}
	// The reserved operator identity may send (the out-of-band wire path).
	if err := tm.Send(team.OperatorSender, "lead", "operator note"); err != nil {
		t.Fatalf("Send from operator: %v", err)
	}
	// An arbitrary, non-member, non-operator sender is rejected.
	err := tm.Send("ghost-lead", "lead", "you have been pwned")
	if !errors.Is(err, team.ErrUnknownSender) {
		t.Fatalf("Send from forged sender: err = %v, want ErrUnknownSender", err)
	}
}

// TestAddMemberRejectsReservedName asserts a member cannot be named the reserved
// operator identity (Fix C), which would let it impersonate operator messages.
func TestAddMemberRejectsReservedName(t *testing.T) {
	tm := team.New("spike")
	err := tm.AddMember(team.OperatorSender, "")
	if !errors.Is(err, team.ErrReservedName) {
		t.Fatalf("AddMember(operator): err = %v, want ErrReservedName", err)
	}
	if got := tm.Members(); len(got) != 0 {
		t.Fatalf("roster size = %d after rejected reserved name, want 0", len(got))
	}
}

// TestCreateTaskCap asserts CreateTask returns ErrTooManyTasks at MaxTasks (Fix F).
func TestCreateTaskCap(t *testing.T) {
	tm := newTeamWith(t, "alice")
	for i := 0; i < team.MaxTasks; i++ {
		if _, err := tm.CreateTask("work"); err != nil {
			t.Fatalf("CreateTask #%d: %v", i, err)
		}
	}
	if _, err := tm.CreateTask("one too many"); !errors.Is(err, team.ErrTooManyTasks) {
		t.Fatalf("CreateTask past cap: err = %v, want ErrTooManyTasks", err)
	}
}

// TestSendInboxCap asserts Send returns ErrTooManyMessages once a recipient's
// undelivered backlog reaches MaxInboxMessages (Fix F), and that draining the
// inbox frees the budget again.
func TestSendInboxCap(t *testing.T) {
	tm := newTeamWith(t, "alice", "bob")
	for i := 0; i < team.MaxInboxMessages; i++ {
		if err := tm.Send("bob", "alice", "spam"); err != nil {
			t.Fatalf("Send #%d: %v", i, err)
		}
	}
	if err := tm.Send("bob", "alice", "overflow"); !errors.Is(err, team.ErrTooManyMessages) {
		t.Fatalf("Send past inbox cap: err = %v, want ErrTooManyMessages", err)
	}
	// Draining frees the budget: a subsequent Send succeeds again.
	if _, err := tm.Drain("alice"); err != nil {
		t.Fatalf("Drain: %v", err)
	}
	if err := tm.Send("bob", "alice", "after drain"); err != nil {
		t.Fatalf("Send after drain: %v", err)
	}
}

// TestAddMemberCap asserts AddMember returns ErrTooManyMembers at MaxMembers
// (Fix F).
func TestAddMemberCap(t *testing.T) {
	tm := team.New("spike")
	for i := 0; i < team.MaxMembers; i++ {
		if err := tm.AddMember(fmt.Sprintf("m%d", i), ""); err != nil {
			t.Fatalf("AddMember #%d: %v", i, err)
		}
	}
	if err := tm.AddMember("one-too-many", ""); !errors.Is(err, team.ErrTooManyMembers) {
		t.Fatalf("AddMember past cap: err = %v, want ErrTooManyMembers", err)
	}
}

// TestAppendFindingUnknownMemberRejected asserts AppendFinding authenticates the
// recording member against the roster (mirroring Send's sender authentication).
func TestAppendFindingUnknownMemberRejected(t *testing.T) {
	tm := newTeamWith(t, "lead")
	if err := tm.AppendFinding("ghost", "the build passes"); !errors.Is(err, team.ErrUnknownMember) {
		t.Fatalf("AppendFinding from non-member: err = %v, want ErrUnknownMember", err)
	}
	if err := tm.AppendFinding("lead", "the build passes"); err != nil {
		t.Fatalf("AppendFinding from a roster member: %v", err)
	}
}

// TestFindingsReturnedInAppendOrder asserts Findings returns recorded findings in
// append order with a monotonic Seq, and that the returned slice is an independent
// copy (mutating it does not affect the aggregate).
func TestFindingsReturnedInAppendOrder(t *testing.T) {
	tm := newTeamWith(t, "lead", "worker")
	// Interleave two members' findings.
	for _, f := range []struct{ who, body string }{
		{"lead", "scoped the work"},
		{"worker", "found the regression"},
		{"lead", "drafted the plan"},
		{"worker", "wrote a repro"},
	} {
		if err := tm.AppendFinding(f.who, f.body); err != nil {
			t.Fatalf("AppendFinding(%q): %v", f.who, err)
		}
	}
	got := tm.Findings()
	want := []team.Finding{
		{Seq: 1, Member: "lead", Body: "scoped the work"},
		{Seq: 2, Member: "worker", Body: "found the regression"},
		{Seq: 3, Member: "lead", Body: "drafted the plan"},
		{Seq: 4, Member: "worker", Body: "wrote a repro"},
	}
	if len(got) != len(want) {
		t.Fatalf("Findings len = %d, want %d", len(got), len(want))
	}
	for i := range want {
		if got[i] != want[i] {
			t.Fatalf("Findings[%d] = %+v, want %+v", i, got[i], want[i])
		}
	}
	// Mutating the returned copy must not affect the aggregate.
	got[0].Body = "tampered"
	if again := tm.Findings(); again[0].Body != "scoped the work" {
		t.Fatalf("Findings returned an aliasing slice: aggregate mutated to %q", again[0].Body)
	}
}

// TestAppendFindingLedgerCapEnforced asserts AppendFinding returns
// ErrTooManyFindings once the ledger is at MaxFindings.
func TestAppendFindingLedgerCapEnforced(t *testing.T) {
	tm := newTeamWith(t, "lead")
	for i := 0; i < team.MaxFindings; i++ {
		if err := tm.AppendFinding("lead", "datum"); err != nil {
			t.Fatalf("AppendFinding #%d: %v", i, err)
		}
	}
	if err := tm.AppendFinding("lead", "one too many"); !errors.Is(err, team.ErrTooManyFindings) {
		t.Fatalf("AppendFinding past cap: err = %v, want ErrTooManyFindings", err)
	}
}

// TestConcurrentAppendFindingNoLostAppends spins N goroutines recording findings
// concurrently and asserts the ledger length and per-member counts are exact (no
// lost or torn appends). Run under -race.
func TestConcurrentAppendFindingNoLostAppends(t *testing.T) {
	const (
		workers          = 8
		perWorkerEntries = 20
	)
	names := make([]string, workers)
	for i := range names {
		names[i] = workerName(i)
	}
	tm := newTeamWith(t, names...)

	var wg sync.WaitGroup
	for w := 0; w < workers; w++ {
		wg.Add(1)
		go func(name string) {
			defer wg.Done()
			for i := 0; i < perWorkerEntries; i++ {
				if err := tm.AppendFinding(name, "datum"); err != nil {
					t.Errorf("AppendFinding(%q): %v", name, err)
					return
				}
			}
		}(names[w])
	}
	wg.Wait()

	got := tm.Findings()
	if len(got) != workers*perWorkerEntries {
		t.Fatalf("ledger len = %d, want %d", len(got), workers*perWorkerEntries)
	}
	perMember := make(map[string]int)
	seqSeen := make(map[int]bool)
	for _, f := range got {
		perMember[f.Member]++
		if seqSeen[f.Seq] {
			t.Fatalf("duplicate Seq %d (torn append)", f.Seq)
		}
		seqSeen[f.Seq] = true
	}
	for _, name := range names {
		if perMember[name] != perWorkerEntries {
			t.Fatalf("member %q recorded %d findings, want %d", name, perMember[name], perWorkerEntries)
		}
	}
}
