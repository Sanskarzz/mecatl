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
