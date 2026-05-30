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

// MemberEngineFactory builds a team member's Engine, binding it to the shared team
// (so the member's catalog includes that team's coordination tools) and shaping it
// from the member spec (read-only base vs mutating tools). The composition root
// supplies it via Config.MemberEngine; CreateTeam adapts it to an agent.MemberEngine
// by capturing the per-team aggregate.
type MemberEngineFactory func(t *team.Team, spec agent.MemberSpec) *agent.Engine

// teamState couples a team's shared coordination aggregate with the supervisor
// that drives it and the base workspace it was created over.
type teamState struct {
	team *team.Team
	sup  *agent.Supervisor
	base string
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
// entry. A Mutating member requires a configured Forker.
func (s *Service) SpawnTeammate(ctx context.Context, teamID string, spec agent.MemberSpec) (team.Member, error) {
	ts, err := s.lookupTeam(teamID)
	if err != nil {
		return team.Member{}, err
	}
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
