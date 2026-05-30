package server

import (
	"context"
	"errors"
	"fmt"

	"github.com/stacklok/mecatl/internal/agent"
	"github.com/stacklok/mecatl/internal/team"
)

// ErrTeamsDisabled is returned by the team methods when no MemberEngine is wired
// in Config — agent teams are an opt-in capability the composition root enables.
var ErrTeamsDisabled = errors.New("server: agent teams are not enabled")

// ErrTeamRunning is returned when an operation is rejected because the team is
// already running: a second concurrent RunTeam, or a SpawnTeammate after the team
// has started. It maps to codes.FailedPrecondition.
var ErrTeamRunning = errors.New("server: team is already running")

// teamPhase is a team's lifecycle phase in the registry, guarded by Service.mu. A
// team is created on CreateTeam, transitions atomically to running on RunTeam
// (rejecting a second RunTeam and any SpawnTeammate), and to done when RunTeam
// returns. The phase serialises access to the Supervisor's unsynchronised
// members/order maps: only one RunTeam may drive a team, and no member may be
// spawned once driving has begun.
type teamPhase int

const (
	teamCreated teamPhase = iota
	teamRunning
	teamDone
)

// MemberEngineFactory builds a team member's Engine, binding it to the shared team
// (so the member's catalog includes that team's coordination tools) and shaping it
// from the member spec (read-only base vs mutating tools). The composition root
// supplies it via Config.MemberEngine; CreateTeam adapts it to an agent.MemberEngine
// by capturing the per-team aggregate.
type MemberEngineFactory func(t *team.Team, spec agent.MemberSpec) *agent.Engine

// teamState couples a team's shared coordination aggregate with the supervisor
// that drives it and the base workspace it was created over.
type teamState struct {
	team  *team.Team
	sup   *agent.Supervisor
	base  string
	phase teamPhase
}

// CreateTeam allocates a new agent team over the given base workspace and returns
// its server-assigned id. It builds the supervisor (binding the member-engine
// factory to the shared team) but spawns no members; use SpawnTeammate then
// RunTeam. It returns ErrTeamsDisabled when teams are not enabled.
func (s *Service) CreateTeam(_ context.Context, workspace, name string) (string, error) {
	if s.cfg.MemberEngine == nil {
		return "", ErrTeamsDisabled
	}
	if workspace == "" {
		return "", fmt.Errorf("%w: workspace is required", ErrInvalidArgument)
	}

	t := team.New(name)
	base := s.cfg.Workspaces(workspace)
	factory := func(spec agent.MemberSpec) *agent.Engine { return s.cfg.MemberEngine(t, spec) }

	opts := []agent.SupervisorOption{}
	if s.cfg.Forker != nil {
		opts = append(opts, agent.WithForker(s.cfg.Forker))
	}
	if s.cfg.TeamHooks != nil {
		opts = append(opts, agent.WithTeamHooks(s.cfg.TeamHooks))
	}
	sup := agent.NewSupervisor(t, base, factory, opts...)

	id := "team-" + string(s.cfg.NewID())
	s.mu.Lock()
	s.teams[id] = &teamState{team: t, sup: sup, base: workspace}
	s.mu.Unlock()
	return id, nil
}

// lookupTeam returns the registered team state for id, or ErrNotFound.
func (s *Service) lookupTeam(id string) (*teamState, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	ts, ok := s.teams[id]
	if !ok {
		return nil, fmt.Errorf("%w: team %q", ErrNotFound, id)
	}
	return ts, nil
}

// SpawnTeammate enrols a member in a team (before RunTeam) and returns its roster
// entry. A Mutating member requires a configured Forker. It is rejected with
// ErrTeamRunning once the team has started running: AddMember mutates the
// Supervisor's unsynchronised member maps, which the in-flight RunTeam is reading.
func (s *Service) SpawnTeammate(ctx context.Context, teamID string, spec agent.MemberSpec) (team.Member, error) {
	ts, err := s.lookupTeam(teamID)
	if err != nil {
		return team.Member{}, err
	}
	s.mu.Lock()
	if ts.phase != teamCreated {
		s.mu.Unlock()
		return team.Member{}, fmt.Errorf("%w: cannot spawn into a team that has started", ErrTeamRunning)
	}
	s.mu.Unlock()
	if err := ts.sup.AddMember(ctx, spec); err != nil {
		return team.Member{}, fmt.Errorf("%w: %v", ErrInvalidArgument, err)
	}
	for _, m := range ts.team.Members() {
		if m.Name == spec.Name {
			return m, nil
		}
	}
	return team.Member{}, fmt.Errorf("%w: member %q not found after spawn", ErrInternal, spec.Name)
}

// SendTeammateMessage posts a message into a member's inbox, delivered at that
// member's next turn boundary. An empty from defaults to "operator".
func (s *Service) SendTeammateMessage(_ context.Context, teamID, from, to, body string) error {
	ts, err := s.lookupTeam(teamID)
	if err != nil {
		return err
	}
	if from == "" {
		from = "operator"
	}
	if err := ts.team.Send(from, to, body); err != nil {
		return fmt.Errorf("%w: %v", ErrInvalidArgument, err)
	}
	return nil
}

// RunTeam drives the team to quiescence, invoking sink for every member event,
// and returns the outcome. It blocks for the team's lifetime; the gRPC handler
// runs it on the request goroutine and forwards events to the stream.
func (s *Service) RunTeam(ctx context.Context, teamID string, sink func(agent.TeamEvent)) (agent.TeamOutcome, error) {
	ts, err := s.lookupTeam(teamID)
	if err != nil {
		return agent.TeamOutcome{}, err
	}
	// Atomically claim the team for this run. A second concurrent RunTeam (or one
	// after a completed run) is rejected — the Supervisor's member state is not safe
	// to drive twice. The transition is guarded by Service.mu so two callers race to
	// claim exactly one wins.
	s.mu.Lock()
	if ts.phase != teamCreated {
		s.mu.Unlock()
		return agent.TeamOutcome{}, fmt.Errorf("%w: %q", ErrTeamRunning, teamID)
	}
	ts.phase = teamRunning
	s.mu.Unlock()

	defer func() {
		s.mu.Lock()
		ts.phase = teamDone
		s.mu.Unlock()
	}()
	return ts.sup.Run(ctx, sink), nil
}

// ListTeam returns the team roster, the shared task list, and whether the team
// has reached quiescence.
func (s *Service) ListTeam(_ context.Context, teamID string) ([]team.Member, []team.Task, bool, error) {
	ts, err := s.lookupTeam(teamID)
	if err != nil {
		return nil, nil, false, err
	}
	return ts.team.Members(), ts.team.Tasks(), ts.team.Quiescent(), nil
}

// CleanupTeam drops a finished team from the registry. Forked member workspaces
// are torn down by the supervisor when RunTeam returns; this releases the
// registry entry. It returns ErrNotFound for an unknown team.
func (s *Service) CleanupTeam(_ context.Context, teamID string) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	if _, ok := s.teams[teamID]; !ok {
		return fmt.Errorf("%w: team %q", ErrNotFound, teamID)
	}
	delete(s.teams, teamID)
	return nil
}
