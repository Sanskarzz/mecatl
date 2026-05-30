package server_test

import (
	"context"
	"errors"
	"testing"
	"time"

	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"

	mecatlv1 "github.com/stacklok/mecatl/contracts/gen/go/mecatl/v1"
	"github.com/stacklok/mecatl/internal/adapter/memfs"
	"github.com/stacklok/mecatl/internal/adapter/mockllm"
	"github.com/stacklok/mecatl/internal/adapter/permpolicy"
	"github.com/stacklok/mecatl/internal/adapter/server"
	"github.com/stacklok/mecatl/internal/adapter/store/memstore"
	"github.com/stacklok/mecatl/internal/agent"
	"github.com/stacklok/mecatl/internal/governance"
	"github.com/stacklok/mecatl/internal/team"
	"github.com/stacklok/mecatl/internal/tool"
)

// teamService builds a team-enabled Service whose per-member Engine uses the
// supplied mockllm provider (shared by all members) and carries that member's
// coordination tools.
func teamService(t *testing.T, llm *mockllm.Provider) *server.Service {
	t.Helper()
	allow := permpolicy.NewPolicy([]governance.Rule{{Effect: governance.Allow}})
	memberEngine := func(tm *team.Team, spec agent.MemberSpec) *agent.Engine {
		cat := tool.NewCatalog()
		for _, tl := range agent.MemberTools(tm, spec.Name, nil) {
			cat.MustRegister(tl)
		}
		return agent.NewEngine(agent.Deps{
			LLM:     llm,
			Catalog: cat,
			Policy:  allow,
			Model:   "mock",
		})
	}
	// A no-op engine for plain sessions (unused by these team tests).
	engine := agent.NewEngine(agent.Deps{
		LLM:     mockllm.New(mockllm.TextTurn("x")),
		Catalog: tool.NewCatalog(),
		Policy:  allow,
		Model:   "mock",
	})
	svc, err := server.NewService(server.Config{
		Engine:       engine,
		Store:        memstore.New(),
		Workspaces:   func(root string) tool.Workspace { return memfs.NewWorkspace(root) },
		Now:          func() time.Time { return time.Unix(0, 0) },
		MemberEngine: memberEngine,
	})
	if err != nil {
		t.Fatalf("new service: %v", err)
	}
	return svc
}

func newCreateTeam(workspace string) *mecatlv1.CreateTeamRequest {
	return &mecatlv1.CreateTeamRequest{Workspace: workspace, Name: "test"}
}

func newSpawn(teamID, name string, lead bool, initialPrompt string) *mecatlv1.SpawnTeammateRequest {
	return &mecatlv1.SpawnTeammateRequest{
		TeamId: teamID, Name: name, Lead: lead, InitialPrompt: initialPrompt,
	}
}

// newCreateTeamWith builds a CreateTeamRequest carrying an initial roster — the
// atomic create+populate path.
func newCreateTeamWith(workspace string, members ...*mecatlv1.TeammateSpec) *mecatlv1.CreateTeamRequest {
	return &mecatlv1.CreateTeamRequest{Workspace: workspace, Name: "test", Members: members}
}

// wantRunningSentinel asserts a Service-level method returned the ErrTeamRunning
// sentinel (the Service returns raw sentinels; the gRPC layer maps them).
func wantRunningSentinel(t *testing.T, err error, what string) {
	t.Helper()
	if !errors.Is(err, server.ErrTeamRunning) {
		t.Errorf("%s: err = %v, want ErrTeamRunning", what, err)
	}
}

// wantFailedPrecondition asserts a gRPC-layer (HarnessServer) call returned a
// FailedPrecondition status — verifying ErrTeamRunning is mapped by toStatus.
func wantFailedPrecondition(t *testing.T, err error, what string) {
	t.Helper()
	if err == nil {
		t.Fatalf("%s: expected an error, got nil", what)
	}
	if status.Code(err) != codes.FailedPrecondition {
		t.Errorf("%s: code = %v, want FailedPrecondition (err=%v)", what, status.Code(err), err)
	}
}

// TestTeamRunStateMachineRejectsConcurrent asserts Fix B at the gRPC boundary: once
// a team is running, a second RunTeam and a SpawnTeammate are both rejected with
// FailedPrecondition. The first RunTeam is held busy by a member whose mock blocks
// on ctx (blockingChunks); the test observes the busy stream, probes the
// rejections, then cancels to let the run finish.
func TestTeamRunStateMachineRejectsConcurrent(t *testing.T) {
	// The member's single turn streams text then blocks until ctx is cancelled, so
	// RunTeam stays in the running phase for the duration of the probes.
	llm := mockllm.New(mockllm.ChunksTurn(blockingChunks()...))
	svc := teamService(t, llm)
	h := server.NewHarnessServer(svc)

	ctx := context.Background()
	createResp, err := h.CreateTeam(ctx, newCreateTeam("/ws"))
	if err != nil {
		t.Fatalf("CreateTeam: %v", err)
	}
	teamID := createResp.GetTeamId()

	if _, err := h.SpawnTeammate(ctx, newSpawn(teamID, "lead", true, "go")); err != nil {
		t.Fatalf("SpawnTeammate(lead): %v", err)
	}

	// Drive RunTeam on a goroutine via the Service (no stream plumbing needed). The
	// first member event tells us the run is live and in the running phase.
	runCtx, cancelRun := context.WithCancel(context.Background())
	defer cancelRun()
	firstEvent := make(chan struct{}, 1)
	runDone := make(chan struct{})
	go func() {
		defer close(runDone)
		_, _ = svc.RunTeam(runCtx, teamID, func(agent.TeamEvent) {
			select {
			case firstEvent <- struct{}{}:
			default:
			}
		})
	}()

	select {
	case <-firstEvent:
	case <-time.After(5 * time.Second):
		t.Fatal("timed out waiting for the team run to start emitting events")
	}

	// Concurrent second RunTeam is rejected (Service-level sentinel).
	_, err = svc.RunTeam(context.Background(), teamID, func(agent.TeamEvent) {})
	wantRunningSentinel(t, err, "concurrent RunTeam")

	// SpawnTeammate after the run started is rejected — through the gRPC layer, so
	// it must surface as a FailedPrecondition status (toStatus mapping).
	_, err = h.SpawnTeammate(ctx, newSpawn(teamID, "late", false, ""))
	wantFailedPrecondition(t, err, "SpawnTeammate after run")

	cancelRun()
	select {
	case <-runDone:
	case <-time.After(5 * time.Second):
		t.Fatal("timed out waiting for the cancelled run to return")
	}

	// After the run is done, a second RunTeam is still rejected (created→running is
	// one-shot; the team is now done).
	_, err = svc.RunTeam(context.Background(), teamID, func(agent.TeamEvent) {})
	wantRunningSentinel(t, err, "RunTeam after done")
}

// TestTeamRunStateMachineDeterministic exercises the state machine directly,
// without a blocking member: a normal run completes, after which a second RunTeam
// and a SpawnTeammate are both rejected (the team is done, not created).
func TestTeamRunStateMachineDeterministic(t *testing.T) {
	llm := mockllm.New(mockllm.TextTurn("done"))
	svc := teamService(t, llm)
	h := server.NewHarnessServer(svc)
	ctx := context.Background()

	createResp, err := h.CreateTeam(ctx, newCreateTeam("/ws"))
	if err != nil {
		t.Fatalf("CreateTeam: %v", err)
	}
	teamID := createResp.GetTeamId()
	if _, err := h.SpawnTeammate(ctx, newSpawn(teamID, "solo", true, "go")); err != nil {
		t.Fatalf("SpawnTeammate: %v", err)
	}

	if _, err := svc.RunTeam(ctx, teamID, func(agent.TeamEvent) {}); err != nil {
		t.Fatalf("RunTeam: %v", err)
	}

	_, err = svc.RunTeam(ctx, teamID, func(agent.TeamEvent) {})
	wantRunningSentinel(t, err, "second RunTeam after completion")

	_, err = h.SpawnTeammate(ctx, newSpawn(teamID, "late", false, ""))
	wantFailedPrecondition(t, err, "SpawnTeammate after completion")
}

// TestSpawnTeammateErrorClassification asserts finding J: SpawnTeammate no longer
// collapses every AddMember failure to InvalidArgument. A Mutating member spawned
// into a Service with no WorkspaceForker is a server misconfiguration the client
// cannot fix by changing its args, so it must surface as FailedPrecondition (not
// InvalidArgument). A duplicate-name spawn stays InvalidArgument (a real bad
// request), confirming the classifier discriminates rather than blanket-remapping.
func TestSpawnTeammateErrorClassification(t *testing.T) {
	// teamService wires no Forker, so a Mutating member trips ErrNoForker.
	svc := teamService(t, mockllm.New(mockllm.TextTurn("done")))
	h := server.NewHarnessServer(svc)
	ctx := context.Background()

	createResp, err := h.CreateTeam(ctx, newCreateTeam("/ws"))
	if err != nil {
		t.Fatalf("CreateTeam: %v", err)
	}
	teamID := createResp.GetTeamId()

	// Mutating member, no forker configured → FailedPrecondition, NOT InvalidArgument.
	mutating := &mecatlv1.SpawnTeammateRequest{TeamId: teamID, Name: "writer", Mutating: true}
	_, err = h.SpawnTeammate(ctx, mutating)
	wantFailedPrecondition(t, err, "SpawnTeammate(Mutating, no forker)")

	// A genuine bad request — duplicate name — still maps to InvalidArgument.
	if _, err := h.SpawnTeammate(ctx, newSpawn(teamID, "lead", true, "go")); err != nil {
		t.Fatalf("SpawnTeammate(lead): %v", err)
	}
	_, err = h.SpawnTeammate(ctx, newSpawn(teamID, "lead", false, ""))
	if status.Code(err) != codes.InvalidArgument {
		t.Errorf("SpawnTeammate(duplicate): code = %v, want InvalidArgument (err=%v)", status.Code(err), err)
	}
}

// TestUnknownTeamNotFound asserts a lookup on an unknown team id returns the
// team-specific ErrTeamNotFound sentinel (Service level) and maps to NotFound at
// the gRPC boundary — not the session-flavoured ErrNotFound.
func TestUnknownTeamNotFound(t *testing.T) {
	svc := teamService(t, mockllm.New(mockllm.TextTurn("done")))
	h := server.NewHarnessServer(svc)
	ctx := context.Background()

	if _, _, _, err := svc.ListTeam(ctx, "team-nope"); !errors.Is(err, server.ErrTeamNotFound) {
		t.Errorf("ListTeam(unknown): err = %v, want ErrTeamNotFound", err)
	}
	if err := svc.CleanupTeam(ctx, "team-nope"); !errors.Is(err, server.ErrTeamNotFound) {
		t.Errorf("CleanupTeam(unknown): err = %v, want ErrTeamNotFound", err)
	}
	_, err := h.ListTeam(ctx, &mecatlv1.ListTeamRequest{TeamId: "team-nope"})
	if status.Code(err) != codes.NotFound {
		t.Errorf("ListTeam(unknown) gRPC: code = %v, want NotFound (err=%v)", status.Code(err), err)
	}
}

// teamServiceMaxTeams builds a team-enabled Service with an explicit MaxTeams cap.
func teamServiceMaxTeams(t *testing.T, llm *mockllm.Provider, maxTeams int) *server.Service {
	t.Helper()
	allow := permpolicy.NewPolicy([]governance.Rule{{Effect: governance.Allow}})
	memberEngine := func(tm *team.Team, spec agent.MemberSpec) *agent.Engine {
		cat := tool.NewCatalog()
		for _, tl := range agent.MemberTools(tm, spec.Name, nil) {
			cat.MustRegister(tl)
		}
		return agent.NewEngine(agent.Deps{LLM: llm, Catalog: cat, Policy: allow, Model: "mock"})
	}
	engine := agent.NewEngine(agent.Deps{
		LLM: mockllm.New(mockllm.TextTurn("x")), Catalog: tool.NewCatalog(), Policy: allow, Model: "mock",
	})
	svc, err := server.NewService(server.Config{
		Engine:       engine,
		Store:        memstore.New(),
		Workspaces:   func(root string) tool.Workspace { return memfs.NewWorkspace(root) },
		Now:          func() time.Time { return time.Unix(0, 0) },
		MemberEngine: memberEngine,
		MaxTeams:     maxTeams,
	})
	if err != nil {
		t.Fatalf("new service: %v", err)
	}
	return svc
}

// TestCreateTeamMaxTeams asserts Fix D's registry cap: CreateTeam past MaxTeams is
// rejected with ResourceExhausted, and cleaning up a (created) team frees a slot so
// a subsequent CreateTeam succeeds again.
func TestCreateTeamMaxTeams(t *testing.T) {
	svc := teamServiceMaxTeams(t, mockllm.New(mockllm.TextTurn("x")), 2)
	h := server.NewHarnessServer(svc)
	ctx := context.Background()

	r1, err := h.CreateTeam(ctx, newCreateTeam("/ws"))
	if err != nil {
		t.Fatalf("CreateTeam #1: %v", err)
	}
	if _, err := h.CreateTeam(ctx, newCreateTeam("/ws")); err != nil {
		t.Fatalf("CreateTeam #2: %v", err)
	}

	// The registry is full; the third create is rejected with ResourceExhausted.
	_, err = h.CreateTeam(ctx, newCreateTeam("/ws"))
	if err == nil {
		t.Fatal("CreateTeam past MaxTeams: expected an error, got nil")
	}
	if status.Code(err) != codes.ResourceExhausted {
		t.Fatalf("CreateTeam past MaxTeams: code = %v, want ResourceExhausted (err=%v)", status.Code(err), err)
	}
	// And the Service returns the mapped sentinel.
	if _, _, serr := svc.CreateTeam(ctx, "/ws", "x", nil); !errors.Is(serr, server.ErrTooManyTeams) {
		t.Fatalf("Service.CreateTeam past cap: err = %v, want ErrTooManyTeams", serr)
	}

	// Cleaning up a created team frees a slot; the next CreateTeam succeeds.
	if _, err := h.CleanupTeam(ctx, &mecatlv1.CleanupTeamRequest{TeamId: r1.GetTeamId()}); err != nil {
		t.Fatalf("CleanupTeam: %v", err)
	}
	if _, err := h.CreateTeam(ctx, newCreateTeam("/ws")); err != nil {
		t.Fatalf("CreateTeam after freeing a slot: %v", err)
	}
}

// TestCleanupTeamRejectsRunning asserts Fix D: CleanupTeam on a running team is
// rejected with FailedPrecondition (deleting it would orphan the live supervisor),
// while a created or done team can be cleaned up and frees its slot.
func TestCleanupTeamRejectsRunning(t *testing.T) {
	// A member whose single turn blocks until ctx is cancelled keeps the team in the
	// running phase for the duration of the probe.
	llm := mockllm.New(mockllm.ChunksTurn(blockingChunks()...))
	svc := teamService(t, llm)
	h := server.NewHarnessServer(svc)
	ctx := context.Background()

	createResp, err := h.CreateTeam(ctx, newCreateTeam("/ws"))
	if err != nil {
		t.Fatalf("CreateTeam: %v", err)
	}
	teamID := createResp.GetTeamId()

	// A created (not-yet-running) team can be cleaned up.
	otherResp, err := h.CreateTeam(ctx, newCreateTeam("/ws"))
	if err != nil {
		t.Fatalf("CreateTeam #2: %v", err)
	}
	if _, err := h.CleanupTeam(ctx, &mecatlv1.CleanupTeamRequest{TeamId: otherResp.GetTeamId()}); err != nil {
		t.Fatalf("CleanupTeam(created): %v", err)
	}

	if _, err := h.SpawnTeammate(ctx, newSpawn(teamID, "lead", true, "go")); err != nil {
		t.Fatalf("SpawnTeammate: %v", err)
	}

	runCtx, cancelRun := context.WithCancel(context.Background())
	defer cancelRun()
	firstEvent := make(chan struct{}, 1)
	runDone := make(chan struct{})
	go func() {
		defer close(runDone)
		_, _ = svc.RunTeam(runCtx, teamID, func(agent.TeamEvent) {
			select {
			case firstEvent <- struct{}{}:
			default:
			}
		})
	}()

	select {
	case <-firstEvent:
	case <-time.After(5 * time.Second):
		t.Fatal("timed out waiting for the team run to start")
	}

	// CleanupTeam on the running team is rejected (FailedPrecondition).
	_, err = h.CleanupTeam(ctx, &mecatlv1.CleanupTeamRequest{TeamId: teamID})
	wantFailedPrecondition(t, err, "CleanupTeam(running)")
	// And the Service returns the ErrTeamRunning sentinel.
	if serr := svc.CleanupTeam(ctx, teamID); !errors.Is(serr, server.ErrTeamRunning) {
		t.Fatalf("Service.CleanupTeam(running): err = %v, want ErrTeamRunning", serr)
	}

	cancelRun()
	select {
	case <-runDone:
	case <-time.After(5 * time.Second):
		t.Fatal("timed out waiting for the cancelled run to return")
	}

	// Once done, the team can be cleaned up.
	if _, err := h.CleanupTeam(ctx, &mecatlv1.CleanupTeamRequest{TeamId: teamID}); err != nil {
		t.Fatalf("CleanupTeam(done): %v", err)
	}
	// And it is gone: a second cleanup is NotFound.
	_, err = h.CleanupTeam(ctx, &mecatlv1.CleanupTeamRequest{TeamId: teamID})
	if status.Code(err) != codes.NotFound {
		t.Fatalf("CleanupTeam(already gone): code = %v, want NotFound (err=%v)", status.Code(err), err)
	}
}

// TestCreateTeamWithRoster asserts the atomic create+populate path: CreateTeam with
// an initial roster (a lead + a read-only worker) returns the team id AND the
// enrolled members, ListTeam shows both, and RunTeam works without any separate
// SpawnTeammate call.
func TestCreateTeamWithRoster(t *testing.T) {
	llm := mockllm.New(mockllm.TextTurn("done"))
	svc := teamService(t, llm)
	h := server.NewHarnessServer(svc)
	ctx := context.Background()

	createResp, err := h.CreateTeam(ctx, newCreateTeamWith("/ws",
		&mecatlv1.TeammateSpec{Name: "lead", Lead: true, InitialPrompt: "go"},
		&mecatlv1.TeammateSpec{Name: "worker"}, // read-only (Mutating defaults false)
	))
	if err != nil {
		t.Fatalf("CreateTeam(roster): %v", err)
	}
	teamID := createResp.GetTeamId()
	if teamID == "" {
		t.Fatal("CreateTeam(roster): empty team id")
	}

	// The response echoes the enrolled roster in enrolment order — no follow-up
	// ListTeam needed.
	got := createResp.GetMembers()
	if len(got) != 2 || got[0].GetName() != "lead" || got[1].GetName() != "worker" {
		t.Fatalf("CreateTeam(roster) members = %v, want [lead worker]", got)
	}

	// ListTeam shows both members.
	listResp, err := h.ListTeam(ctx, &mecatlv1.ListTeamRequest{TeamId: teamID})
	if err != nil {
		t.Fatalf("ListTeam: %v", err)
	}
	if len(listResp.GetMembers()) != 2 {
		t.Fatalf("ListTeam members = %d, want 2", len(listResp.GetMembers()))
	}

	// RunTeam works with no separate SpawnTeammate call. The lead runs its initial
	// prompt and reports back; the bare read-only worker (no task, no message) is never
	// scheduled, which is fine — the run completes without error and the lead produced
	// its terminal text.
	outcome, err := svc.RunTeam(ctx, teamID, func(agent.TeamEvent) {})
	if err != nil {
		t.Fatalf("RunTeam: %v", err)
	}
	if len(outcome.Members) != 2 {
		t.Fatalf("RunTeam outcome members = %d, want 2 (outcome=%+v)", len(outcome.Members), outcome)
	}
	if outcome.Members[0].Name != "lead" || outcome.Members[0].LastText != "done" {
		t.Errorf("RunTeam lead outcome = %+v, want LastText=%q", outcome.Members[0], "done")
	}
}

// TestCreateTeamWithRosterAtomicFailure asserts the atomicity guarantee: a roster
// containing a member that cannot enrol (here a duplicate name within the roster)
// fails the WHOLE CreateTeam, and the would-be team is never registered — a
// subsequent ListTeam on no team exists, and the MaxTeams slot was NOT consumed (the
// caller can still create up to the cap).
func TestCreateTeamWithRosterAtomicFailure(t *testing.T) {
	// MaxTeams=1 so we can prove the failed create did not consume the only slot.
	svc := teamServiceMaxTeams(t, mockllm.New(mockllm.TextTurn("done")), 1)
	h := server.NewHarnessServer(svc)
	ctx := context.Background()

	// A roster with a duplicate name: the second "dup" trips ErrMemberAlreadyAdded,
	// which classifies to InvalidArgument. The whole team must be abandoned.
	_, err := h.CreateTeam(ctx, newCreateTeamWith("/ws",
		&mecatlv1.TeammateSpec{Name: "dup", Lead: true, InitialPrompt: "go"},
		&mecatlv1.TeammateSpec{Name: "dup"},
	))
	if err == nil {
		t.Fatal("CreateTeam(duplicate in roster): expected an error, got nil")
	}
	if status.Code(err) != codes.InvalidArgument {
		t.Fatalf("CreateTeam(duplicate in roster): code = %v, want InvalidArgument (err=%v)", status.Code(err), err)
	}

	// No team leaked: the registry is empty, so a fresh create against the MaxTeams=1
	// cap succeeds (the failed create did not consume the slot).
	createResp, err := h.CreateTeam(ctx, newCreateTeam("/ws"))
	if err != nil {
		t.Fatalf("CreateTeam after a failed atomic create: %v (the failed create must not consume a slot)", err)
	}
	if createResp.GetTeamId() == "" {
		t.Fatal("CreateTeam after a failed atomic create: empty team id")
	}
}

// TestCreateTeamWithRosterMutatingNoForkerAtomicFailure asserts the same atomicity
// for a server-misconfiguration failure class: a Mutating member with no Forker
// configured (teamServiceMaxTeams wires none) is a FailedPrecondition, and again the
// team is abandoned — no id is returned and the MaxTeams slot is untouched.
func TestCreateTeamWithRosterMutatingNoForkerAtomicFailure(t *testing.T) {
	svc := teamServiceMaxTeams(t, mockllm.New(mockllm.TextTurn("done")), 1)
	h := server.NewHarnessServer(svc)
	ctx := context.Background()

	_, err := h.CreateTeam(ctx, newCreateTeamWith("/ws",
		&mecatlv1.TeammateSpec{Name: "lead", Lead: true, InitialPrompt: "go"},
		&mecatlv1.TeammateSpec{Name: "writer", Mutating: true}, // no forker → ErrNoForker
	))
	if status.Code(err) != codes.FailedPrecondition {
		t.Fatalf("CreateTeam(Mutating, no forker): code = %v, want FailedPrecondition (err=%v)", status.Code(err), err)
	}

	// The slot was not consumed: a fresh create succeeds under MaxTeams=1.
	if _, err := h.CreateTeam(ctx, newCreateTeam("/ws")); err != nil {
		t.Fatalf("CreateTeam after a failed atomic create: %v", err)
	}
}
