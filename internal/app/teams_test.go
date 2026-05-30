package app

import (
	"context"
	"errors"
	"testing"
	"time"

	"github.com/stacklok/mecatl/internal/adapter/forker"
	"github.com/stacklok/mecatl/internal/adapter/mockllm"
	"github.com/stacklok/mecatl/internal/adapter/osfs"
	"github.com/stacklok/mecatl/internal/adapter/permpolicy"
	"github.com/stacklok/mecatl/internal/adapter/server"
	"github.com/stacklok/mecatl/internal/adapter/store/memstore"
	"github.com/stacklok/mecatl/internal/agent"
	"github.com/stacklok/mecatl/internal/tool"
)

// teamCfg is the minimal app Config a member engine factory needs: a model and a
// shell so the Mutating branch can attempt to register Bash. The workspace is a
// throwaway temp dir (the command runner roots there, but no command is run in
// these tests).
func teamCfg(t *testing.T) Config {
	t.Helper()
	return Config{
		Workspace: t.TempDir(),
		Model:     "mock",
		Shell:     "/bin/sh",
	}
}

// TestBuildMemberEngineReadOnlySpawnSucceeds exercises fix A's ACCEPTANCE path: a
// read-only member built by the app factory carries only Read/Grep/Glob + the
// coordination tools and NO workspace-mutating tools, so the supervisor's AddMember
// invariant accepts it and SpawnTeammate succeeds. (If buildMemberEngine wrongly
// handed a read-only member Edit/Write/Bash, AddMember would reject it with
// ErrReadOnlyMemberMutating and the spawn would fail.)
func TestBuildMemberEngineReadOnlySpawnSucceeds(t *testing.T) {
	cfg := teamCfg(t)
	provider := mockllm.New(mockllm.TextTurn("ok"))
	svc := teamServiceWithFactory(t, buildMemberEngine(cfg, provider, nil))

	ctx := context.Background()
	teamID, _, err := svc.CreateTeam(ctx, t.TempDir(), "test", nil)
	if err != nil {
		t.Fatalf("CreateTeam: %v", err)
	}
	// Read-only lead (Mutating defaults false): must be accepted by AddMember.
	if _, err := svc.SpawnTeammate(ctx, teamID, agent.MemberSpec{Name: "lead", Lead: true, InitialPrompt: "go"}); err != nil {
		t.Fatalf("SpawnTeammate(read-only): %v", err)
	}
}

// TestBuildMemberEngineMutatingSpawnSucceeds asserts a Mutating member (which the
// factory gives Edit/Write/Bash) is accepted when a Forker is configured — it runs
// in an isolated fork, so the workspace-mutating tools are permitted. This proves
// the Mutating branch of buildMemberEngine produces a catalog the supervisor admits.
func TestBuildMemberEngineMutatingSpawnSucceeds(t *testing.T) {
	cfg := teamCfg(t)
	provider := mockllm.New(mockllm.TextTurn("ok"))
	svc := teamServiceWithFactory(t, buildMemberEngine(cfg, provider, nil))

	ctx := context.Background()
	teamID, _, err := svc.CreateTeam(ctx, t.TempDir(), "test", nil)
	if err != nil {
		t.Fatalf("CreateTeam: %v", err)
	}
	if _, err := svc.SpawnTeammate(ctx, teamID, agent.MemberSpec{Name: "writer", Mutating: true}); err != nil {
		t.Fatalf("SpawnTeammate(Mutating): %v", err)
	}
}

// TestTeamsEnabledEndToEnd is the end-to-end wiring proof: a Service built with the
// app member-engine factory runs a single-member team to quiescence with no error.
// A single LEAD member runs its InitialPrompt against a mockllm scripted with one
// text turn; the team then plans no further work and reaches quiescence. (A single
// member keeps the shared-provider scripting deterministic — multi-member
// coordination is covered by the supervisor unit tests.)
func TestTeamsEnabledEndToEnd(t *testing.T) {
	cfg := teamCfg(t)
	provider := mockllm.New(mockllm.TextTurn("all done"))
	svc := teamServiceWithFactory(t, buildMemberEngine(cfg, provider, nil))

	ctx := context.Background()
	// Atomic create+populate: the initial roster is enrolled by CreateTeam itself, so
	// no separate SpawnTeammate call is needed before RunTeam.
	teamID, enrolled, err := svc.CreateTeam(ctx, t.TempDir(), "test",
		[]agent.MemberSpec{{Name: "lead", Lead: true, InitialPrompt: "go"}})
	if err != nil {
		t.Fatalf("CreateTeam: %v", err)
	}
	if errors.Is(err, server.ErrTeamsDisabled) {
		t.Fatal("CreateTeam returned ErrTeamsDisabled with a factory wired")
	}
	if len(enrolled) != 1 || enrolled[0].Name != "lead" {
		t.Fatalf("CreateTeam enrolled roster = %+v, want one member named lead", enrolled)
	}

	outcome, err := svc.RunTeam(ctx, teamID, func(agent.TeamEvent) {})
	if err != nil {
		t.Fatalf("RunTeam: %v", err)
	}
	if !outcome.Quiescent {
		t.Errorf("RunTeam outcome: Quiescent = false, want true (outcome=%+v)", outcome)
	}
}

// TestTeamsDisabledWhenNoFactory asserts that with EnableTeams effectively off
// (MemberEngine nil) CreateTeam returns ErrTeamsDisabled. It also drives the real
// Build seam to prove the flag controls the wiring: Build with EnableTeams:false
// yields a Service whose CreateTeam is disabled.
func TestTeamsDisabledWhenNoFactory(t *testing.T) {
	built, err := Build(context.Background(), Config{
		Workspace: t.TempDir(),
		Model:     "mock",
		UseMock:   true,
		// EnableTeams omitted → false.
	})
	if err != nil {
		t.Fatalf("Build: %v", err)
	}
	defer built.Close()

	_, _, err = built.Service.CreateTeam(context.Background(), "/ws", "test", nil)
	if !errors.Is(err, server.ErrTeamsDisabled) {
		t.Fatalf("CreateTeam (teams off): err = %v, want ErrTeamsDisabled", err)
	}
}

// TestBuildEnableTeamsRunsTeam proves the FULL Build path: app.Build with
// EnableTeams:true yields a Service whose CreateTeam succeeds (not ErrTeamsDisabled)
// and whose single-member team runs to quiescence — the flag really wires the
// factory, forker, and team hooks into server.Config.
func TestBuildEnableTeamsRunsTeam(t *testing.T) {
	built, err := Build(context.Background(), Config{
		Workspace:   t.TempDir(),
		Model:       "mock",
		UseMock:     true,
		EnableTeams: true,
	})
	if err != nil {
		t.Fatalf("Build: %v", err)
	}
	defer built.Close()

	ctx := context.Background()
	teamID, _, err := built.Service.CreateTeam(ctx, t.TempDir(), "test", nil)
	if errors.Is(err, server.ErrTeamsDisabled) {
		t.Fatal("CreateTeam returned ErrTeamsDisabled with EnableTeams:true")
	}
	if err != nil {
		t.Fatalf("CreateTeam: %v", err)
	}
	if _, err := built.Service.SpawnTeammate(ctx, teamID, agent.MemberSpec{Name: "lead", Lead: true, InitialPrompt: "go"}); err != nil {
		t.Fatalf("SpawnTeammate: %v", err)
	}
	outcome, err := built.Service.RunTeam(ctx, teamID, func(agent.TeamEvent) {})
	if err != nil {
		t.Fatalf("RunTeam: %v", err)
	}
	if !outcome.Quiescent {
		t.Errorf("RunTeam outcome: Quiescent = false, want true (outcome=%+v)", outcome)
	}
}

// teamServiceWithFactory builds a server.Service wired with the given member-engine
// factory, the osfs-backed forker (so Mutating members can fork a real base dir), an
// osfs workspace factory, and a deterministic clock. osfs (not memfs) is used because
// the forker copies the base tree off real disk for a Mutating member.
func teamServiceWithFactory(t *testing.T, factory server.MemberEngineFactory) *server.Service {
	t.Helper()
	osfsWS := func(root string) tool.Workspace {
		ws, err := osfs.NewWorkspace(root)
		if err != nil {
			t.Fatalf("osfs workspace %q: %v", root, err)
		}
		return ws
	}
	svc, err := server.NewService(server.Config{
		Engine:       noopEngine(),
		Store:        memstore.New(),
		Workspaces:   osfsWS,
		Now:          func() time.Time { return time.Unix(0, 0) },
		MemberEngine: factory,
		Forker:       forker.New(func(root string) (tool.Workspace, error) { return osfs.NewWorkspace(root) }),
	})
	if err != nil {
		t.Fatalf("new service: %v", err)
	}
	return svc
}

// noopEngine is a minimal engine for the Service's plain-session path, unused by the
// team tests but required by NewService.
func noopEngine() *agent.Engine {
	return agent.NewEngine(agent.Deps{
		LLM:     mockllm.New(mockllm.TextTurn("x")),
		Catalog: tool.NewCatalog(),
		Policy:  permpolicy.NewPolicy(defaultRules()),
		Model:   "mock",
	})
}
