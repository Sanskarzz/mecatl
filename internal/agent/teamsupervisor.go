package agent

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"sort"
	"strings"
	"time"

	"golang.org/x/sync/errgroup"

	"github.com/stacklok/mecatl/internal/governance"
	"github.com/stacklok/mecatl/internal/port"
	"github.com/stacklok/mecatl/internal/session"
	"github.com/stacklok/mecatl/internal/team"
	"github.com/stacklok/mecatl/internal/tool"
)

// teamsupervisor.go is the APPLICATION-layer orchestrator for agent teams (see
// docs/design/AGENT-TEAMS-SPIKE.md). It owns one shared *team.Team and drives a
// set of long-lived member sessions that coordinate through that team's task list
// and mailbox. Unlike Task/Fork (one-shot, drained internally), team members are
// re-driven across rounds and their events are STREAMED to the caller (tagged with
// the member name), never drained — the headless analogue of Claude Code's
// split-pane teammates.
//
// Scheduling is round-based and deterministic at the round level (concurrent WITHIN
// a round): in each round the supervisor plans which members have work — a pending
// message, or, for non-lead members, a claimable task — renders each one's next
// user turn, and runs those turns concurrently via Engine.Run, then re-opens each
// session (session.Reopen) for the next round. Messages are therefore delivered at
// TURN BOUNDARIES, never mid-turn, so a running loop is never corrupted. The team
// finishes when a round plans no work; team.Quiescent reports whether that is
// genuine completion (all tasks done, mailboxes empty) or a stuck dependency.
//
// Workspace policy (the agreed read-only-share / mutating-fork stance): a read-only
// member shares the base workspace; a Mutating member runs in its OWN forked
// workspace (via the injected tool.WorkspaceForker), so parallel writes are safe
// because isolated — exactly like Fork. v1 does not auto-merge forks.

// defaultMaxRounds bounds a team Run so a non-converging team cannot loop forever.
const defaultMaxRounds = 24

// defaultTeamConcurrency bounds how many member turns run at once within a single
// scheduling round, mirroring fork.go's defaultForkConcurrency. Running every
// planned member concurrently is the point of a round, but it is also N times the
// resource cost, so a worker limit keeps it bounded. Override with
// WithTeamConcurrency.
const defaultTeamConcurrency = 4

// TeamEvent tags a member session Event with the member that produced it, for the
// multiplexed team event stream the caller observes.
type TeamEvent struct {
	// Member is the name of the member whose session produced Event.
	Member string
	// Event is the underlying session Event (turn.start, tool.call, result, ...).
	Event session.Event
}

// MemberSpec describes a member to enrol before running the team.
type MemberSpec struct {
	// Name is the unique member handle peers address messages to.
	Name string
	// AgentType is the optional agent-definition name this member adopts; passed
	// through to the engine factory and recorded on the team roster.
	AgentType string
	// Lead marks the coordinating member. The lead is NOT auto-assigned tasks (it
	// coordinates); it runs on its initial prompt and whenever it has messages.
	Lead bool
	// Mutating requests an isolated forked workspace for this member (it may
	// Edit/Write). A read-only member (the default) shares the base workspace.
	Mutating bool
	// InitialPrompt is the member's first-turn input, run in round 0 (typically the
	// lead's top-level task, or a teammate's role briefing).
	InitialPrompt string
}

// MemberEngine builds the per-member *Engine from its spec. The composition root
// supplies it; it is expected to capture the shared *team.Team so the member's
// catalog includes MemberTools(team, spec.Name) (always available to a member, even
// under a restrictive agent definition) plus the member's scoped base tools, model,
// and policy. It MUST consult spec.Mutating: a read-only member shares the base
// workspace, so it must NOT be given mutating tools (Edit/Write/non-RO Bash) — only
// a Mutating member (which runs in an isolated fork) may have them.
type MemberEngine func(spec MemberSpec) *Engine

// Supervisor orchestrates one agent team. Build it with NewSupervisor, enrol
// members with AddMember (before Run), then call Run.
type Supervisor struct {
	team    *team.Team
	base    tool.Workspace
	forker  tool.WorkspaceForker
	factory MemberEngine

	limits      session.Limits
	mode        session.PermissionMode
	maxRounds   int
	concurrency int
	idPrefix    string
	hooks       port.HookRunner

	members map[string]*memberRT
	order   []string
}

// memberRT is a member's runtime state. Each member appears in at most one round
// plan per round, so exactly one goroutine touches a given memberRT at a time;
// fields are read by the (single) planning goroutine between rounds.
type memberRT struct {
	spec       MemberSpec
	engine     *Engine
	ws         tool.Workspace
	cleanup    func() error
	sess       *session.Session
	ranInitial bool
	stopped    bool
	lastText   string
}

// SupervisorOption configures a Supervisor.
type SupervisorOption func(*Supervisor)

// WithForker injects the workspace-isolation seam used to fork a Mutating member's
// workspace. It is required only if any member is Mutating.
func WithForker(f tool.WorkspaceForker) SupervisorOption {
	return func(s *Supervisor) { s.forker = f }
}

// WithTeamLimits overrides the per-member, per-round stop conditions (default
// defaultChildLimits). Reopen resets these counters each round, so they bound one
// turn-loop, not the member's whole life.
func WithTeamLimits(l session.Limits) SupervisorOption {
	return func(s *Supervisor) { s.limits = l }
}

// WithTeamMode sets the permission mode each member session runs under (default
// session.ModeDefault).
func WithTeamMode(m session.PermissionMode) SupervisorOption {
	return func(s *Supervisor) { s.mode = m }
}

// WithMaxRounds caps the number of scheduling rounds (default defaultMaxRounds). A
// non-positive value is ignored.
func WithMaxRounds(n int) SupervisorOption {
	return func(s *Supervisor) {
		if n > 0 {
			s.maxRounds = n
		}
	}
}

// WithTeamHooks injects the HookRunner that fires the TeammateIdle lifecycle hook
// when a member goes idle after a turn (best-effort; nil disables it). It is the
// same runner the composition root should pass to MemberTools for the TaskCreated /
// TaskCompleted gates, so a team's lifecycle hooks all flow through one runner.
func WithTeamHooks(h port.HookRunner) SupervisorOption {
	return func(s *Supervisor) { s.hooks = h }
}

// WithTeamConcurrency bounds how many member turns run simultaneously within a
// scheduling round (default defaultTeamConcurrency). A non-positive value is
// ignored.
func WithTeamConcurrency(n int) SupervisorOption {
	return func(s *Supervisor) {
		if n > 0 {
			s.concurrency = n
		}
	}
}

// WithMemberSessionPrefix sets the prefix used to derive member session ids
// (default "team"). Ids are of the form "<prefix>-<member>".
func WithMemberSessionPrefix(p string) SupervisorOption {
	return func(s *Supervisor) {
		if p != "" {
			s.idPrefix = p
		}
	}
}

// NewSupervisor constructs a team supervisor over a shared team, a base workspace,
// and a per-member engine factory. team, base, and factory must be non-nil;
// NewSupervisor panics otherwise (a composition-root programming error).
func NewSupervisor(t *team.Team, base tool.Workspace, factory MemberEngine, opts ...SupervisorOption) *Supervisor {
	if t == nil {
		panic("agent: NewSupervisor requires a non-nil team")
	}
	if base == nil {
		panic("agent: NewSupervisor requires a non-nil base workspace")
	}
	if factory == nil {
		panic("agent: NewSupervisor requires a non-nil member engine factory")
	}
	s := &Supervisor{
		team:        t,
		base:        base,
		factory:     factory,
		limits:      defaultChildLimits,
		mode:        session.ModeDefault,
		maxRounds:   defaultMaxRounds,
		concurrency: defaultTeamConcurrency,
		idPrefix:    "team",
		members:     make(map[string]*memberRT),
	}
	for _, o := range opts {
		o(s)
	}
	return s
}

// AddMember enrols a member: it registers it on the team roster, builds its
// workspace (the shared base for a read-only member, an isolated fork for a
// Mutating one), constructs its session and Engine, and records it. It must be
// called before Run. A Mutating member without a configured forker is an error.
func (s *Supervisor) AddMember(ctx context.Context, spec MemberSpec) error {
	if strings.TrimSpace(spec.Name) == "" {
		return errors.New("agent: team member name is required")
	}
	if _, ok := s.members[spec.Name]; ok {
		return fmt.Errorf("agent: member %q already added", spec.Name)
	}
	if err := s.team.AddMember(spec.Name, spec.AgentType); err != nil {
		return fmt.Errorf("agent: enrol member: %w", err)
	}

	ws := s.base
	var cleanup func() error
	if spec.Mutating {
		if s.forker == nil {
			s.team.RemoveMember(spec.Name)
			return fmt.Errorf("agent: member %q is Mutating but no WorkspaceForker is configured", spec.Name)
		}
		child, cl, err := s.forker.Fork(ctx, s.base, spec.Name)
		if err != nil {
			s.team.RemoveMember(spec.Name)
			return fmt.Errorf("agent: fork workspace for %q: %w", spec.Name, err)
		}
		ws, cleanup = child, cl
	}

	eng := s.factory(spec)
	if eng == nil {
		if cleanup != nil {
			_ = cleanup()
		}
		s.team.RemoveMember(spec.Name)
		return fmt.Errorf("agent: member factory returned a nil Engine for %q", spec.Name)
	}

	// The supervisor is authoritative on the read-only-share / mutating-fork stance:
	// it chose ws above from spec.Mutating, but the factory builds the Engine's
	// catalog independently. Verify they agree. A non-Mutating member shares the base
	// workspace, so it must NOT be handed a WORKSPACE-mutating tool (Edit / Write /
	// non-read-only Bash) — that would let it corrupt the shared base concurrently
	// with peers. Team coordination tools report ReadOnly() == false but only mutate
	// TEAM state, so they are exempted by name.
	if !spec.Mutating {
		if bad := workspaceMutatingTools(eng.catalogTools()); len(bad) > 0 {
			if cleanup != nil {
				_ = cleanup()
			}
			s.team.RemoveMember(spec.Name)
			return fmt.Errorf("agent: read-only member %q was given workspace-mutating tool(s) %s; "+
				"a base-sharing member must not be able to mutate the shared workspace (mark it Mutating to run in an isolated fork)",
				spec.Name, strings.Join(bad, ", "))
		}
	}

	sess := session.New(s.sessionID(spec.Name), s.mode, ws.Root(), s.limits, time.Now())
	_ = s.team.SetMemberSession(spec.Name, sess.ID)

	s.members[spec.Name] = &memberRT{spec: spec, engine: eng, ws: ws, cleanup: cleanup, sess: sess}
	s.order = append(s.order, spec.Name)
	return nil
}

// TeamOutcome is the result of a team Run.
type TeamOutcome struct {
	// Rounds is the number of scheduling rounds that ran work.
	Rounds int
	// Quiescent reports whether the team reached genuine completion (all tasks
	// done, mailboxes empty, no member still working) versus stopping because a
	// round planned no work while tasks remained (a stuck dependency / deadlock).
	Quiescent bool
	// Members holds each member's terminal summary in enrolment order.
	Members []MemberOutcome
}

// MemberOutcome summarises one member at the end of a Run.
type MemberOutcome struct {
	// Name is the member name.
	Name string
	// LastText is the member's most recent terminal assistant text.
	LastText string
	// Stopped reports whether the member ended in a non-resumable state (its last
	// run failed or was cancelled, so it could not be re-opened for another round).
	Stopped bool
}

// turnInput pairs a member with the rendered prompt for its next turn this round.
type turnInput struct {
	m      *memberRT
	prompt string
}

// Run drives the team to quiescence (or the round cap), invoking sink for every
// member event as it is produced, and returns the outcome. sink may be nil. The
// run is bounded by ctx: cancelling it stops scheduling further rounds and lets the
// in-flight round finish. Forked member workspaces are cleaned up on return.
func (s *Supervisor) Run(ctx context.Context, sink func(TeamEvent)) TeamOutcome {
	defer s.cleanupAll()
	if sink == nil {
		sink = func(TeamEvent) {}
	}

	// A single forwarder serialises sink calls even though member turns run
	// concurrently, so the caller's sink need not be concurrency-safe.
	evCh := make(chan TeamEvent, 64)
	done := make(chan struct{})
	go func() {
		for ev := range evCh {
			sink(ev)
		}
		close(done)
	}()

	rounds := 0
	for r := 0; r < s.maxRounds; r++ {
		if ctx.Err() != nil {
			break
		}
		plan := s.planRound(r)
		if len(plan) == 0 {
			break
		}
		rounds++
		// Run the round's planned member turns concurrently, but bounded: a worker
		// limit caps how many run at once (mirroring fork.go). runTurn returns no
		// error — a member's failure is captured on its memberRT (stopped) — so the
		// group's Wait error is always nil and ignored.
		var g errgroup.Group
		g.SetLimit(s.concurrency)
		for _, ti := range plan {
			g.Go(func() error {
				s.runTurn(ctx, ti, evCh)
				return nil
			})
		}
		_ = g.Wait()
	}

	close(evCh)
	<-done
	return s.outcome(rounds)
}

// planRound decides which members run this round and with what prompt. It runs on
// the single Run goroutine between rounds, so it reads/writes member runtime state
// without a lock; the team's own methods are internally synchronised. Round 0 runs
// members' initial prompts; later rounds run any member with pending messages and
// auto-claim the next task for non-lead members.
func (s *Supervisor) planRound(r int) []turnInput {
	var plan []turnInput
	for _, name := range s.order {
		m := s.members[name]
		if m.stopped {
			continue
		}
		if r == 0 && !m.ranInitial && strings.TrimSpace(m.spec.InitialPrompt) != "" {
			m.ranInitial = true
			plan = append(plan, turnInput{m: m, prompt: m.spec.InitialPrompt})
			continue
		}
		msgs, _ := s.team.Drain(name)
		var claimed *team.Task
		// Auto-claim the next task only for a non-lead member that is NOT already
		// holding an in-progress task. Without this guard a member would accumulate
		// unbounded claims across rounds (one new task per round), starving peers and
		// holding work it is not yet running. A member finishes (CompleteTask) or
		// stops (its tasks are released) before it claims again.
		if !m.spec.Lead && !s.team.InProgressFor(name) {
			if task, ok, _ := s.team.ClaimNext(name); ok {
				t := task
				claimed = &t
			}
		}
		if len(msgs) == 0 && claimed == nil {
			continue
		}
		plan = append(plan, turnInput{m: m, prompt: renderTurnPrompt(name, msgs, claimed)})
	}
	return plan
}

// runTurn runs one member's turn-loop to completion, forwarding every event (tagged
// with the member name) to evCh, auto-denying any permission ask (members are
// non-interactive in v1, matching Task/Fork), then re-opening the session for the
// next round. A run that ends non-resumably marks the member stopped.
func (s *Supervisor) runTurn(ctx context.Context, ti turnInput, evCh chan<- TeamEvent) {
	m := ti.m
	_ = s.team.SetMemberState(m.spec.Name, team.MemberWorking)

	run := m.engine.Run(ctx, m.sess, m.ws, ti.prompt)
	stop := session.StopNone
	for ev := range run.Events() {
		if text, st, ok := handleChildEvent(run, ev); ok {
			stop = st
			if text != "" {
				m.lastText = text
			}
		}
		evCh <- TeamEvent{Member: m.spec.Name, Event: ev}
	}

	// A member whose run ENDED IN ERROR, or that cannot be re-opened, is stopped: it
	// will not be scheduled again. Honouring the result's stop reason (not just a
	// failing Reopen) is what makes drainChild's contract and this loop agree. A
	// stopped member RELEASES any task it claimed but never completed, so the team
	// does not dead-spin on an in-progress task owned by a dead member.
	if stop == session.StopError || m.sess.Reopen() != nil {
		m.stopped = true
		_ = s.team.SetMemberState(m.spec.Name, team.MemberStopped)
		s.team.ReleaseTasks(m.spec.Name)
		return
	}
	_ = s.team.SetMemberState(m.spec.Name, team.MemberIdle)
	s.fireTeammateIdle(ctx, m)
}

// fireTeammateIdle runs the TeammateIdle hook for a member that just went idle
// (best-effort; a hook error or block is ignored — going idle cannot be vetoed in
// v1, matching SubagentStop). It delegates to fireNotify, which owns the
// cancelled-ctx detach rule shared with the Task/Fork SubagentStop fires.
func (s *Supervisor) fireTeammateIdle(ctx context.Context, m *memberRT) {
	input, _ := json.Marshal(map[string]string{"member": m.spec.Name})
	fireNotify(ctx, s.hooks, governance.HookEvent{
		Phase:     governance.PhaseTeammateIdle,
		Input:     input,
		SessionID: string(m.sess.ID),
	})
}

// outcome assembles the final TeamOutcome from member runtime state.
func (s *Supervisor) outcome(rounds int) TeamOutcome {
	o := TeamOutcome{Rounds: rounds, Quiescent: s.team.Quiescent()}
	for _, name := range s.order {
		m := s.members[name]
		o.Members = append(o.Members, MemberOutcome{Name: name, LastText: m.lastText, Stopped: m.stopped})
	}
	return o
}

// cleanupAll tears down every forked member workspace.
func (s *Supervisor) cleanupAll() {
	for _, name := range s.order {
		if m := s.members[name]; m != nil && m.cleanup != nil {
			_ = m.cleanup()
		}
	}
}

// sessionID derives a stable session id for a member.
func (s *Supervisor) sessionID(name string) session.SessionID {
	return session.SessionID(fmt.Sprintf("%s-%s", s.idPrefix, name))
}

// workspaceMutatingTools returns the names of tools in info that mutate the
// WORKSPACE — i.e. report ReadOnly() == false and are NOT team coordination tools
// (which mutate only team state). The names are sorted for a deterministic error
// message. The coordination set is derived from MemberToolNames, so it stays in
// lock-step with the tools MemberTools actually installs.
func workspaceMutatingTools(info []catalogToolInfo) []string {
	coord := MemberToolNames()
	var bad []string
	for _, t := range info {
		if t.readOnly {
			continue
		}
		if _, ok := coord[t.name]; ok {
			continue
		}
		bad = append(bad, t.name)
	}
	sort.Strings(bad)
	return bad
}

// renderTurnPrompt composes the user-turn text a member sees for its next turn:
// its identity, any new messages, and its claimed task (if any), plus a reminder of
// the coordination tools. The model is expected to act and, when done, complete its
// task and report back via SendMessage.
func renderTurnPrompt(self string, msgs []team.Message, claimed *team.Task) string {
	var b strings.Builder
	fmt.Fprintf(&b, "You are %q, a member of the agent team.\n", self)
	if len(msgs) > 0 {
		b.WriteString("\nNew messages for you:\n")
		for _, msg := range msgs {
			fmt.Fprintf(&b, "- from %s: %s\n", msg.From, msg.Body)
		}
	}
	if claimed != nil {
		fmt.Fprintf(&b, "\nYou have claimed task %s: %s\n", claimed.ID, claimed.Description)
		fmt.Fprintf(&b, "When finished, call CompleteTask with task_id=%q, then report back to the lead with SendMessage.\n", claimed.ID)
	}
	b.WriteString("\nUse the team coordination tools (ListTasks, AddTask, ClaimTask, CompleteTask, SendMessage) " +
		"to organise the work. Respond with a brief status when your turn's work is done.")
	return b.String()
}
