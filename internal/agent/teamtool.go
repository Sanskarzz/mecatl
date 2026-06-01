package agent

import (
	"context"
	"encoding/json"
	"fmt"
	"slices"
	"strings"

	"github.com/stacklok/mecatl/internal/port"
	"github.com/stacklok/mecatl/internal/session"
	"github.com/stacklok/mecatl/internal/team"
	"github.com/stacklok/mecatl/internal/tool"
)

// teamToolName is the catalog name of the team-formation tool.
const teamToolName = "Team"

// maxTeamPreview caps every member tool-call/result preview (TeamPayload.Detail)
// and message text forwarded on a team.member event. A team is meant to be
// WATCHED — so unlike the metadata-only subagent projection, member content IS
// forwarded — but it is forwarded BOUNDED: an unbounded args blob or result body
// can never be copied verbatim onto the parent's event stream. The cap is
// rune-aware (see clampPreview) so it never splits a multi-byte character.
const maxTeamPreview = 200

// TeamMemberEngineFactory builds a team member's engine (as a MemberBuild carrying
// the constructed *Engine plus the member's optional per-member permission mode)
// from the shared team and the member spec. It is the SINGLE canonical factory shape
// consumed by every team entry point — the gRPC CreateTeam path
// (server.Config.MemberEngine) AND the Team tool — so the per-member engine wiring
// lives in one place (internal/app) and cannot drift between the two paths. The
// composition root supplies it; it is expected to capture nothing (the team is
// passed per-call) and to shape the member's catalog from spec.Mutating (a read-only
// member must NOT be handed workspace-mutating tools) plus the team coordination
// tools (MemberTools). Returning a MemberBuild (rather than a bare *Engine) is how a
// member's agent-definition permissionMode reaches the supervisor's per-member
// session — it is the exact shape server.MemberEngineFactory has, so one factory
// serves both paths.
type TeamMemberEngineFactory func(t *team.Team, spec MemberSpec) MemberBuild

// TeamMemberArg is one roster entry the model supplies in a Team call. It maps
// 1:1 onto MemberSpec: Name → Name, Role → InitialPrompt, Mutating → Mutating.
type TeamMemberArg struct {
	// Name is the member's unique handle peers address messages to.
	Name string `json:"name"`
	// Role is the member's role briefing — its first-turn instruction. It becomes
	// the member's InitialPrompt.
	Role string `json:"role"`
	// Mutating requests an isolated forked workspace with workspace-mutating tools
	// (Edit/Write/Bash). A read-only member (the default) shares the base.
	Mutating bool `json:"mutating,omitempty"`
}

// teamArgs is the argument payload the model supplies when calling the Team tool.
// The MODEL specifies the roster: the first member is synthesized as the
// coordinating lead.
type teamArgs struct {
	// Goal is the team's top-level objective, handed to the lead as its briefing.
	Goal string `json:"goal"`
	// Members is the roster the model formed. The first member is the lead.
	Members []TeamMemberArg `json:"members"`
}

// teamSchema is the JSON schema the model sees for the Team tool's arguments.
var teamSchema = json.RawMessage(`{
  "type": "object",
  "properties": {
    "goal": {
      "type": "string",
      "description": "The team's top-level objective. Handed to the lead (the first member) as its briefing."
    },
    "members": {
      "type": "array",
      "minItems": 1,
      "description": "The roster. The FIRST member is the coordinating lead. Each member runs as a long-lived subagent coordinating via a shared task list and mailbox.",
      "items": {
        "type": "object",
        "properties": {
          "name": {"type": "string", "description": "Unique member handle peers address messages to."},
          "role": {"type": "string", "description": "The member's role briefing — its first-turn instruction (e.g. 'Investigate the auth code path and report findings')."},
          "mutating": {"type": "boolean", "description": "True if the member needs to edit/write files (runs in an isolated forked workspace); false (default) shares the base read-only."}
        },
        "required": ["name", "role"]
      }
    }
  },
  "required": ["goal", "members"]
}`)

// TeamTool forms a team of coordinating subagents in-process (see
// teamsupervisor.go). When executed it builds a fresh team.Team, enrols the
// model-supplied roster (synthesizing the first member as the lead), drives the
// existing Supervisor to quiescence over the SAME base workspace as the parent,
// and returns ONLY the team's joined per-member summary as a single ToolResult.
//
// It is the team analogue of TaskTool/ForkTool, with two deliberate differences:
//
//   - ReadOnly() == false. A team spawns Mutating members and is long-lived and
//     stateful, so the dispatcher must SERIALISE it (mutate-serial) rather than
//     run it read-parallel like Task. (Mutating members run in isolated forks, but
//     the supervisor's member maps are not safe to drive alongside other tools.)
//   - Member activity is OBSERVABLE and fuller. Via the observableTool seam, each
//     member event is projected to the parent run's stream as a team.member event
//     carrying the member's message text and BOUNDED tool previews — a team is
//     meant to be watched. permission.ask is dropped; every preview is capped.
//
// Context isolation holds exactly as for Task/Fork: the per-member transcripts are
// never written to the parent Session's Conversation. Only the joined summary
// (the ToolResult) folds back, so the LLM's context stays summary-only.
//
// The composition root injects the member-engine factory, the workspace Forker,
// and the team hooks runner (mirroring Service.CreateTeam's wiring) so this tool
// drives the SAME Supervisor the gRPC team path drives.
type TeamTool struct {
	// factory builds each member's Engine, bound to the per-call team. Required.
	factory TeamMemberEngineFactory
	// forker isolates a Mutating member's workspace. Required only if any member is
	// Mutating; a Mutating member without it yields a tool error (the model can
	// retry with a read-only roster).
	forker tool.WorkspaceForker
	// hooks fires the team lifecycle hooks (TeammateIdle) — shared with the member
	// coordination tools by the composition root. nil disables them.
	hooks port.HookRunner
	// idPrefix seeds the generated team name from the parent call id.
	idPrefix string
}

// TeamOption configures a TeamTool.
type TeamOption func(*TeamTool)

// WithTeamToolForker injects the workspace forker used to isolate a Mutating
// member's workspace. It is required only if the model forms a Mutating roster.
func WithTeamToolForker(f tool.WorkspaceForker) TeamOption {
	return func(t *TeamTool) { t.forker = f }
}

// WithTeamToolHooks injects the HookRunner threaded into the Supervisor (and, by
// the composition root, the member coordination tools) so a team's lifecycle hooks
// flow through one runner. nil disables them.
func WithTeamToolHooks(h port.HookRunner) TeamOption {
	return func(t *TeamTool) { t.hooks = h }
}

// NewTeamTool constructs the Team tool over a per-member engine factory. factory
// must be non-nil; NewTeamTool panics otherwise (a composition-root programming
// error — a Team tool with no way to build member engines cannot run a team).
func NewTeamTool(factory TeamMemberEngineFactory, opts ...TeamOption) tool.Tool {
	if factory == nil {
		panic("agent: NewTeamTool requires a non-nil member engine factory")
	}
	t := &TeamTool{factory: factory, idPrefix: "team"}
	for _, o := range opts {
		o(t)
	}
	return t
}

// Spec returns the model-facing specification for the Team tool.
func (*TeamTool) Spec() tool.ToolSpec {
	return tool.ToolSpec{
		Name: teamToolName,
		Description: "Form a team of coordinating subagents to tackle a goal that benefits from " +
			"parallel specialists — e.g. an investigator + a fixer, or several role-focused " +
			"workers sharing a task list and mailbox. You specify the roster: each member has a " +
			"name, a role (its briefing), and whether it needs to edit files (mutating). The " +
			"FIRST member is the coordinating lead. Members run as long-lived subagents that " +
			"coordinate via a shared task list and direct messages; mutating members run in " +
			"isolated workspaces. Returns ONLY the team's final joined summary — the per-member " +
			"transcripts stay out of this conversation. Use for work that splits into " +
			"specialist roles; for a single one-shot investigation use Task instead.",
		Schema: teamSchema,
	}
}

// ReadOnly reports that the Team tool is NOT read-only, so the parent dispatcher
// SERIALISES it (mutate-serial) — it never runs concurrently with another tool.
// A team is long-lived, stateful, and may spawn Mutating members; its Supervisor
// drives unsynchronised member state, so it must not race the parent's other tool
// calls. This is the deliberate opposite of TaskTool/ForkTool, which are
// read-parallel.
func (*TeamTool) ReadOnly() bool { return false }

// Execute runs a team with no observability (the emit == nil path): the team's
// member activity is not forwarded, only the joined summary is returned. Existing
// non-observing callers are unaffected by the observability seam.
func (t *TeamTool) Execute(ctx context.Context, call session.ToolCall, ws tool.Workspace) (session.ToolResult, error) {
	return t.run(ctx, call, ws, nil)
}

// ExecuteObserved runs a team like Execute but, when emit is non-nil, forwards a
// BOUNDED projection of member activity to the parent run's stream via the three
// team.* events. emit only sequences and channels events; it never touches the
// parent's Conversation, so member content still never enters the parent context
// (only the joined-summary ToolResult does). It is the observableTool seam the
// dispatcher calls.
func (t *TeamTool) ExecuteObserved(ctx context.Context, call session.ToolCall, ws tool.Workspace, emit func(session.Event)) (session.ToolResult, error) {
	return t.run(ctx, call, ws, emit)
}

// run is the shared implementation behind Execute (emit == nil) and
// ExecuteObserved (emit != nil). It validates the roster, builds and drives the
// Supervisor over the SAME base workspace, optionally forwards a bounded
// projection of member activity, and returns the joined summary as the single
// ToolResult that folds back into the parent conversation.
func (t *TeamTool) run(ctx context.Context, call session.ToolCall, ws tool.Workspace, emit func(session.Event)) (session.ToolResult, error) {
	var args teamArgs
	if msg, ok := session.ParseArgs(call, &args); !ok {
		return session.NewToolError(call.ID, "Team: "+msg), nil
	}
	if msg, ok := validateTeamArgs(args); !ok {
		return session.NewToolError(call.ID, "Team: "+msg), nil
	}

	teamID := string(call.ID)
	tm := team.New(teamID)
	factory := func(spec MemberSpec) MemberBuild { return t.factory(tm, spec) }

	opts := []SupervisorOption{}
	if t.forker != nil {
		opts = append(opts, WithForker(t.forker))
	}
	if t.hooks != nil {
		opts = append(opts, WithTeamHooks(t.hooks))
	}
	sup := NewSupervisor(tm, ws, factory, opts...)

	roster := teamRoster(args.Members)
	for i, spec := range memberSpecs(args.Members) {
		if err := sup.AddMember(ctx, spec); err != nil {
			// A bad roster (e.g. a Mutating member with no forker wired) is a tool error
			// the model can recover from. AddMember already tore down the FAILING
			// member's own fork; but Run (whose deferred cleanupAll releases the earlier
			// successful members' forks) is never reached on failure, so we tear those
			// down explicitly here to avoid leaking them.
			sup.cleanupAll()
			return session.NewToolError(call.ID, fmt.Sprintf("forming team failed at member %d (%q): %v", i, spec.Name, err)), nil
		}
	}

	if emit != nil {
		emit(session.Event{Type: session.EvTeamStart, Team: &session.TeamPayload{
			ParentCallID: string(call.ID),
			TeamID:       teamID,
			Roster:       roster,
		}})
	}

	// The sink forwards each member event as a BOUNDED, redacted team.member event
	// AND accumulates the team's total token cost from the usage-bearing member
	// events (inner turn.end / result), so team.end can report the team total rather
	// than zero. Supervisor.Run serialises sink calls through a single forwarder
	// goroutine, so both the accumulation and emit run one-at-a-time even though
	// member turns run concurrently — total needs no lock.
	var total session.Usage
	var lastTasks []session.TeamTaskSnapshot
	sink := func(te TeamEvent) {
		total = total.Add(memberEventUsage(te.Event))
		if emit == nil {
			return
		}
		if ev, ok := projectTeamEvent(string(call.ID), teamID, te); ok {
			emit(ev)
		}
		// Project the shared task list as a first-class team.tasks event, but only
		// when it CHANGED since the last snapshot. The de-dup is load-bearing:
		// the sink fires for every member event (deltas, tool calls, turn ends), so
		// emitting an unchanged task snapshot on each would flood the wire. A team
		// has at most MaxTasks tasks, so the equality scan is cheap.
		snap := projectTeamTasksSnapshot(tm.Tasks())
		if !tasksEqual(snap, lastTasks) {
			lastTasks = snap
			emit(projectTeamTasks(string(call.ID), teamID, snap))
		}
	}

	// The parent ctx flows into Run: cancelling the parent stops scheduling further
	// rounds and lets the in-flight round finish. Run's deferred cleanupAll tears
	// down every forked member workspace on every exit (success, cancel, or panic).
	outcome := sup.Run(ctx, sink)

	if emit != nil {
		emit(session.Event{Type: session.EvTeamEnd, Team: &session.TeamPayload{
			ParentCallID: string(call.ID),
			TeamID:       teamID,
			Rounds:       outcome.Rounds,
			Stop:         teamStop(outcome),
			Usage:        total,
			// The terminal task snapshot always lands on team.end, so the task
			// sub-view reflects the final state even if no member event followed the
			// last task transition.
			Tasks: projectTeamTasksSnapshot(tm.Tasks()),
		}})
	}

	return session.NewToolResult(call.ID, joinTeam(outcome)), nil
}

// validateTeamArgs enforces the roster preconditions: a non-empty goal, at least
// one member, and non-empty unique member names. It returns a model-readable
// message and ok=false on the first violation.
func validateTeamArgs(args teamArgs) (msg string, ok bool) {
	if strings.TrimSpace(args.Goal) == "" {
		return "'goal' is required and must be non-empty", false
	}
	if len(args.Members) == 0 {
		return "'members' is required and must contain at least one member", false
	}
	seen := make(map[string]struct{}, len(args.Members))
	for i, m := range args.Members {
		name := strings.TrimSpace(m.Name)
		if name == "" {
			return fmt.Sprintf("member %d has an empty 'name'", i), false
		}
		if _, dup := seen[name]; dup {
			return fmt.Sprintf("duplicate member name %q", name), false
		}
		seen[name] = struct{}{}
		if strings.TrimSpace(m.Role) == "" {
			return fmt.Sprintf("member %q has an empty 'role'", name), false
		}
	}
	return "", true
}

// memberSpecs maps the model's roster args onto MemberSpec values, synthesizing
// the FIRST member as the coordinating lead. Each member's Role becomes its
// InitialPrompt (its first-turn briefing).
func memberSpecs(members []TeamMemberArg) []MemberSpec {
	specs := make([]MemberSpec, 0, len(members))
	for i, m := range members {
		specs = append(specs, MemberSpec{
			Name:          strings.TrimSpace(m.Name),
			Lead:          i == 0,
			Mutating:      m.Mutating,
			InitialPrompt: m.Role,
		})
	}
	return specs
}

// teamRoster builds the EvTeamStart roster projection from the model's args. It
// forwards only member metadata (name/role/mutating/lead), never member content.
func teamRoster(members []TeamMemberArg) []session.TeamMemberSpec {
	roster := make([]session.TeamMemberSpec, 0, len(members))
	for i, m := range members {
		roster = append(roster, session.TeamMemberSpec{
			Name:     strings.TrimSpace(m.Name),
			Role:     clampPreview(m.Role),
			Mutating: m.Mutating,
			Lead:     i == 0,
		})
	}
	return roster
}

// projectTeamEvent builds a REDACTED, BOUNDED team.member Event from one member's
// inner session event. It is the single chokepoint that enforces the
// fuller-but-bounded contract:
//
//   - message.delta → forward the member's text (capped).
//   - tool.call     → forward the tool NAME + a CAPPED preview of its args.
//   - tool.result   → forward the error bool + a CAPPED preview of its body.
//   - turn.end      → forward the per-turn usage (no content).
//   - result        → forward the terminal text (capped) + cumulative usage.
//   - permission.ask → DROPPED entirely (ok=false): an ask reason can quote
//     secrets/sensitive args, so it is NEVER forwarded.
//   - anything else  → DROPPED (ok=false): unrecognised kinds are not projected, so
//     a new event type cannot leak content by default.
//
// It NEVER copies a raw args blob or result body unbounded — every text/preview
// passes through clampPreview.
func projectTeamEvent(parentCallID, teamID string, te TeamEvent) (session.Event, bool) {
	ev := te.Event
	base := &session.TeamPayload{
		ParentCallID: parentCallID,
		TeamID:       teamID,
		Member:       te.Member,
		InnerKind:    ev.Type,
	}
	switch ev.Type {
	case session.EvMessageDelta:
		if strings.TrimSpace(ev.Text) == "" {
			return session.Event{}, false
		}
		base.Text = clampPreview(ev.Text)
	case session.EvToolCall:
		if ev.ToolCall == nil {
			return session.Event{}, false
		}
		base.ToolName = ev.ToolCall.Name
		base.Detail = clampPreview(string(ev.ToolCall.Args))
	case session.EvToolResult:
		if ev.ToolResult == nil {
			return session.Event{}, false
		}
		base.IsError = ev.ToolResult.IsError
		base.Detail = clampPreview(ev.ToolResult.Content)
	case session.EvTurnEnd:
		if ev.TurnEnd != nil {
			base.Usage = ev.TurnEnd.Usage
			// The per-member context meter (ctrl+a overlay) reads the CURRENT context
			// occupancy — this turn's input-token count — as its numerator, and the
			// producing member engine's window as its denominator. Both ride the
			// turn.end projection so a client can draw a band bar per member lane.
			base.ContextUsed = int64(ev.TurnEnd.Usage.InputTokens)
			base.ContextWindow = int64(te.ContextWindow)
		}
	case session.EvResult:
		if ev.Result != nil {
			base.Text = clampPreview(ev.Result.Text)
			base.Usage = ev.Result.Usage
		}
	default:
		// permission.ask, turn.start, hook, compaction, reasoning.delta, session.init,
		// subagent.*, team.* and any future kind are NOT projected — a member's
		// permission.ask in particular is dropped so its (possibly secret-bearing)
		// reason never reaches the stream.
		return session.Event{}, false
	}
	return session.Event{Type: session.EvTeamMember, Team: base}, true
}

// clampPreview normalises a forwarded text/preview (a member tool-call/result
// Detail, a member's message Text, or a task Description) into a single bounded
// line: it collapses newlines/tabs to spaces (so a multi-line body cannot break the
// one-line stream rendering) and clamps to maxTeamPreview runes, appending an
// ellipsis on overflow. It is rune-aware, so it never splits a multi-byte
// character. This is the cap that keeps the fuller member content BOUNDED.
func clampPreview(s string) string {
	s = strings.Map(func(r rune) rune {
		if r == '\n' || r == '\r' || r == '\t' {
			return ' '
		}
		return r
	}, s)
	r := []rune(s)
	if len(r) <= maxTeamPreview {
		return s
	}
	return strings.TrimRight(string(r[:maxTeamPreview]), " ") + "…"
}

// projectTeamTasksSnapshot maps the team's shared task list (team.Task copies) onto
// the domain TeamTaskSnapshot projection carried on the event stream. The mapping
// lives HERE (internal/agent), not in session: session must not import internal/team
// (team imports session, never the reverse), so the team.Task→session.TeamTaskSnapshot
// bridge belongs in the application layer. Descriptions pass through clampPreview
// (the same cap every member-derived preview uses), and Deps are copied as []string.
func projectTeamTasksSnapshot(tasks []team.Task) []session.TeamTaskSnapshot {
	out := make([]session.TeamTaskSnapshot, 0, len(tasks))
	for _, tk := range tasks {
		deps := make([]string, 0, len(tk.Deps))
		for _, d := range tk.Deps {
			deps = append(deps, string(d))
		}
		out = append(out, session.TeamTaskSnapshot{
			ID:          string(tk.ID),
			Description: clampPreview(tk.Description),
			State:       string(tk.State),
			Assignee:    tk.Assignee,
			Deps:        deps,
		})
	}
	return out
}

// projectTeamTasks wraps a task snapshot in a first-class EvTeamTasks event — the
// team-WIDE projection the client routes to the ctrl+a task sub-view. It carries no
// Member (the task list is team-wide, not per-member), so it does not borrow the
// per-member EvTeamMember envelope.
func projectTeamTasks(parentCallID, teamID string, tasks []session.TeamTaskSnapshot) session.Event {
	return session.Event{Type: session.EvTeamTasks, Team: &session.TeamPayload{
		ParentCallID: parentCallID,
		TeamID:       teamID,
		Tasks:        tasks,
	}}
}

// tasksEqual reports whether two task snapshots are equal on the fields that drive
// the task sub-view (id / state / assignee / deps). It is the de-dup guard in run's
// sink: a snapshot equal to the last emitted one is NOT re-sent, bounding wire
// volume (the sink fires per member event). Description is excluded — it never
// changes after CreateTask, so it cannot drive a spurious re-emit.
func tasksEqual(a, b []session.TeamTaskSnapshot) bool {
	if len(a) != len(b) {
		return false
	}
	for i := range a {
		if a[i].ID != b[i].ID || a[i].State != b[i].State || a[i].Assignee != b[i].Assignee {
			return false
		}
		if !slices.Equal(a[i].Deps, b[i].Deps) {
			return false
		}
	}
	return true
}

// teamStop maps the team outcome to a terminal StopReason for EvTeamEnd: a
// genuinely-quiescent team stopped on success; a non-quiescent one hit the round
// cap or a stuck dependency.
func teamStop(o TeamOutcome) session.StopReason {
	if o.Quiescent {
		return session.StopEndTurn
	}
	return session.StopMaxTurns
}

// memberEventUsage extracts the token usage a single member event carries, for the
// running team total accumulated in run's sink. Only the usage-bearing inner kinds
// contribute: turn.end (this turn's usage) and result (the member run's cumulative
// usage). Summing turn.end across a member's turns reconstructs that member's spend
// without double-counting the result's cumulative figure, so summing only turn.end
// events yields the team total. The terminal result is excluded from the sum to
// avoid double-counting; every other kind contributes the zero Usage.
func memberEventUsage(ev session.Event) session.Usage {
	if ev.Type == session.EvTurnEnd && ev.TurnEnd != nil {
		return ev.TurnEnd.Usage
	}
	return session.Usage{}
}

// joinTeam renders the per-member terminal summaries into a single, clearly
// delimited string — the ONLY thing that enters the parent conversation. It
// mirrors fork.go's joinBranches: each member reports its status (stopped or done)
// and its last text. The output is deterministic (enrolment order).
func joinTeam(o TeamOutcome) string {
	var b strings.Builder
	stopped := 0
	for _, m := range o.Members {
		if m.Stopped {
			stopped++
		}
	}
	fmt.Fprintf(&b, "Team finished in %d round(s) (%s): %d member(s), %d stopped.\n",
		o.Rounds, quiescenceLabel(o.Quiescent), len(o.Members), stopped)
	for _, m := range o.Members {
		b.WriteString("\n=== ")
		b.WriteString(m.Name)
		if m.Stopped {
			b.WriteString(" [STOPPED] ===\n")
		} else {
			b.WriteString(" [DONE] ===\n")
		}
		if strings.TrimSpace(m.LastText) != "" {
			b.WriteString(m.LastText)
			b.WriteString("\n")
		}
	}
	return b.String()
}

// quiescenceLabel renders the team's completion class for the joined summary.
func quiescenceLabel(quiescent bool) string {
	if quiescent {
		return "quiescent"
	}
	return "stopped without quiescence"
}

// Compile-time assertion that TeamTool satisfies the Tool contract and the
// agent-internal observableTool seam.
var (
	_ tool.Tool      = (*TeamTool)(nil)
	_ observableTool = (*TeamTool)(nil)
)
