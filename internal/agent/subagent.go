package agent

import (
	"context"
	"encoding/json"
	"fmt"
	"strings"
	"time"

	"github.com/stacklok/mecatl/internal/governance"
	"github.com/stacklok/mecatl/internal/port"
	"github.com/stacklok/mecatl/internal/session"
	"github.com/stacklok/mecatl/internal/tool"
)

// taskToolName is the catalog name of the subagent delegation tool.
const taskToolName = "Task"

// maxSubagentGoalLen caps the prompt-derived goal label forwarded on
// EvSubagentStart when no explicit description is supplied. It keeps the
// subagent card title compact and bounds how much of the (model-authored) prompt
// is echoed to the event stream.
const maxSubagentGoalLen = 60

// observableTool is the agent-internal seam by which a tool may forward a
// REDACTED, allowlisted projection of its internal activity to the parent run's
// event stream WITHOUT widening the public tool.Tool interface. A tool that
// implements it is given an emit closure (bound by the dispatcher to the parent
// Run, so events are sequenced and mirrored to the sink exactly like the loop's
// own emits); a tool that does not is executed via the ordinary Execute path.
//
// The Task subagent implements this to surface subagent.start/tool/end metadata.
// Crucially, the emit closure only sequences and channels events — it NEVER
// touches the parent's session.Conversation — so this observability is orthogonal
// to the context-isolation guarantee (gauntlet #7).
type observableTool interface {
	ExecuteObserved(ctx context.Context, call session.ToolCall, ws tool.Workspace, emit func(session.Event)) (session.ToolResult, error)
}

// defaultChildLimits are the (deliberately tight) stop conditions a subagent run
// is bounded by when the caller does not override them via WithChildLimits. A
// subagent is a one-shot, focused investigation: it must not run away. These
// defaults are intentionally tighter than a typical parent session.
var defaultChildLimits = session.Limits{
	MaxTurns:               12,
	MaxToolCalls:           40,
	MaxConsecutiveFailures: 3,
}

// DefaultChildLimits returns the default per-child/per-member stop conditions a
// Task subagent (and a team member, via WithTeamLimits) runs under when the
// caller does not override them. The composition layer uses it as the per-field
// FALLBACK when deriving a def's session.Limits from its maxTurns/maxToolCalls:
// a zero def field inherits the matching default here, so a def that sets neither
// is bounded exactly as before.
func DefaultChildLimits() session.Limits { return defaultChildLimits }

// taskArgs is the argument payload the model supplies when calling the Task tool.
// A subagent gets a single, self-contained instruction (its whole prompt — it has
// no shared context with the parent) and an optional short description used only
// for observability.
type taskArgs struct {
	// Prompt is the full, self-contained instruction the subagent runs against.
	// Because the child has a FRESH context window, this must include everything
	// the subagent needs; it cannot see the parent conversation.
	Prompt string `json:"prompt"`
	// Description is an optional short label for the delegated task (logging/UX
	// only); it is not required and does not affect execution.
	Description string `json:"description,omitempty"`
	// Agent optionally routes the delegation to a NAMED agent definition (a
	// specialist with its own prompt/model/scoped read-only catalog). When empty,
	// the default anonymous read-only explorer runs (unchanged behaviour). An
	// unknown name returns a model-addressable error listing the valid names.
	Agent string `json:"agent,omitempty"`
}

// AgentMeta is the plain (name, description) summary of one registered agent
// definition, surfaced in the Task tool's Spec().Description for progressive
// disclosure. It is a layering-clean value type: the composition root translates
// the agents adapter's Registry into a []AgentMeta + a map[string]*Engine and
// injects both via WithAgentEngines, so internal/agent never imports the agents
// adapter.
type AgentMeta struct {
	// Name is the agent def's routing key (the value the model passes as `agent`).
	Name string
	// Description is the one-line summary the model uses to choose a specialist.
	Description string
	// Limits are the per-def session stop conditions the child session runs under
	// when this agent is selected. The composition root derives them from the def's
	// maxTurns/maxToolCalls (per-field falling back to the Task tool's default
	// limits), so a def with no limits carries the same bound as the default
	// explorer. A zero Limits value is treated as "no per-def override" — Execute
	// then uses the Task tool's default limits, exactly as the no-`agent` path does.
	Limits session.Limits
}

// taskSchema is the JSON schema the model sees for the Task tool's arguments. The
// `agent` property is always present (optional); the available agent NAMES are
// enumerated in the tool's Spec().Description tail (progressive disclosure), not
// baked into this schema, so the schema stays byte-stable regardless of how many
// defs are configured.
var taskSchema = json.RawMessage(`{
  "type": "object",
  "properties": {
    "prompt": {
      "type": "string",
      "description": "The full, self-contained instruction for the subagent. The subagent has a FRESH context window and cannot see this conversation, so include everything it needs."
    },
    "description": {
      "type": "string",
      "description": "Optional short label for the delegated task (for logs/UX only)."
    },
    "agent": {
      "type": "string",
      "description": "Optional name of a configured specialist agent to route this delegation to (see the list in the tool description). Omit to use the default read-only explorer."
    }
  },
  "required": ["prompt"]
}`)

// TaskTool is the subagent delegation tool (gauntlet #7). It is a tool.Tool that,
// when executed, spins up a CHILD agent loop with its own fresh Session, its own
// (tighter) Limits, and a SCOPED tool catalog — supplied by the injected child
// *Engine — runs it to completion against the SAME workspace as the parent, and
// returns ONLY the child's final summary string as a single ToolResult.
//
// Context isolation is the whole point: the parent never observes the child's
// intermediate tool.call / tool.result / message.delta events. The child's Event
// stream is drained entirely inside Execute; only the terminal result text folds
// back into the parent conversation. This keeps a noisy "search → read N files →
// summarize" investigation from bloating the main context window.
//
// The child Engine is built by the composition root (cmd/mecated, WP11) with a
// read-only explorer catalog (Read, Grep, Glob) that NEVER includes the Task tool
// itself — so a subagent cannot recurse — and an allow-all policy over those
// read-only tools so the child never needs to prompt a human. See NewTaskTool.
type TaskTool struct {
	// childEngine runs the subagent loop. It is pre-wired by the composition root
	// with the scoped catalog, the (optionally cheaper) model, and an allow/deny
	// policy appropriate for a non-interactive child. It is never the parent
	// Engine: the parent Engine is not mutated. It is the fallback for the
	// no-`agent` (default explorer) case.
	childEngine *Engine

	// agentEngines maps an agent-definition NAME to its pre-built, read-only child
	// Engine. The composition root builds one per def (scoped catalog + resolved
	// model + body→Role prompt) and injects the map via WithAgentEngines. A Task
	// call with a known `agent` runs that engine instead of childEngine; an empty
	// map (the default) means no specialists are configured and Task behaves
	// exactly as before. nil/empty is valid.
	agentEngines map[string]*Engine

	// agentMeta is the (name, description) list surfaced in Spec().Description for
	// progressive disclosure. It is sorted by the composition root for stable
	// output and kept in lockstep with agentEngines.
	agentMeta []AgentMeta

	// agentLimits maps an agent-definition NAME to the per-def session.Limits the
	// child session runs under when that agent is selected. It is derived from
	// agentMeta in WithAgentEngines so it stays in lockstep with agentEngines. A
	// name absent from the map (or a zero Limits) means "use t.limits" — the same
	// default the no-`agent` explorer path uses.
	agentLimits map[string]session.Limits

	// limits bound a single child run. Defaults to defaultChildLimits.
	limits session.Limits

	// childMode is the permission mode the child session runs under. Defaults to
	// session.ModeDefault.
	childMode session.PermissionMode

	// hooks fires the SubagentStop lifecycle hook when a child finishes
	// (best-effort; nil disables it). It is separate from the child Engine's own
	// PreToolUse/PostToolUse hooks.
	hooks port.HookRunner

	// idPrefix seeds the generated child SessionID so child sessions are
	// distinguishable in logs/stores.
	idPrefix string
}

// TaskOption configures a TaskTool.
type TaskOption func(*TaskTool)

// WithChildLimits overrides the subagent's stop conditions. Use it to make a
// child even tighter (or, rarely, looser) than the defaults.
func WithChildLimits(l session.Limits) TaskOption {
	return func(t *TaskTool) { t.limits = l }
}

// WithChildMode sets the permission mode the child session runs under (default
// session.ModeDefault). session.ModePlan additionally hides any non-read-only
// tools from the child at the catalog level.
func WithChildMode(m session.PermissionMode) TaskOption {
	return func(t *TaskTool) { t.childMode = m }
}

// WithSubagentStopHook injects the HookRunner that fires the SubagentStop hook
// when a child run finishes. It is best-effort: a hook error or block never fails
// the Task call. Passing nil disables the hook.
func WithSubagentStopHook(h port.HookRunner) TaskOption {
	return func(t *TaskTool) { t.hooks = h }
}

// WithChildSessionPrefix sets the prefix used to derive child SessionIDs (default
// "subagent"). Child ids are of the form "<prefix>-<callID>".
func WithChildSessionPrefix(p string) TaskOption {
	return func(t *TaskTool) { t.idPrefix = p }
}

// WithAgentEngines injects the per-definition child engines (keyed by agent name)
// and their (name, description) metadata for progressive disclosure. The
// composition root builds each engine with a SCOPED, read-only catalog (the Task
// read-only invariant is preserved — see ReadOnly) and the def's resolved
// model/prompt, then passes the map and a name-sorted meta slice here.
//
// engines and meta should describe the same set of names; meta drives the Spec
// enumeration while engines drives routing. A nil/empty map leaves Task with only
// the default explorer (no behaviour change). It is the agent-package boundary the
// agents adapter never crosses: only plain map + structs flow in.
func WithAgentEngines(engines map[string]*Engine, meta []AgentMeta) TaskOption {
	return func(t *TaskTool) {
		t.agentEngines = engines
		t.agentMeta = meta
		// Index each def's per-run limits by name so Execute can bound the child
		// session with THAT def's limits (instead of the default t.limits) when the
		// agent is selected. A zero Limits is skipped — the name then falls back to
		// t.limits in Execute, identical to the no-`agent` path.
		t.agentLimits = nil
		for _, m := range meta {
			if m.Limits == (session.Limits{}) {
				continue
			}
			if t.agentLimits == nil {
				t.agentLimits = make(map[string]session.Limits, len(meta))
			}
			t.agentLimits[m.Name] = m.Limits
		}
	}
}

// NewTaskTool constructs the Task subagent tool over a pre-built child *Engine.
//
// The composition root (cmd/mecated, WP11) is responsible for building childEngine
// with the SCOPED child catalog and policy. The recommended, deterministic wiring
// is:
//
//   - Catalog: a read-only explorer set — Read, Grep, Glob ONLY. It MUST NOT
//     contain the Task tool (otherwise a subagent could spawn subagents — infinite
//     recursion) and SHOULD NOT contain mutating tools (Edit/Write/non-RO Bash):
//     the default explorer subagent cannot mutate the workspace.
//   - Policy: allow-all over those read-only tools (e.g.
//     permpolicy.NewPolicy([]governance.Rule{{Effect: governance.Allow}})), so the
//     child never produces a permission "ask". Subagents are one-shot and
//     non-interactive — there is no human on the other end of a child run.
//
// Even with that wiring, Execute defends the non-interactive invariant: if the
// child loop ever pauses on a permission ask, Execute auto-resolves it as DENY so
// the child can never block waiting for a human. This keeps the subagent
// deterministic regardless of the policy it is given.
//
// childEngine must be non-nil; NewTaskTool panics otherwise, because a Task tool
// with no child loop to delegate to is a programming error at the composition
// root.
func NewTaskTool(childEngine *Engine, opts ...TaskOption) tool.Tool {
	if childEngine == nil {
		panic("agent: NewTaskTool requires a non-nil child Engine")
	}
	t := &TaskTool{
		childEngine: childEngine,
		limits:      defaultChildLimits,
		childMode:   session.ModeDefault,
		idPrefix:    "subagent",
	}
	for _, o := range opts {
		o(t)
	}
	return t
}

// Spec returns the model-facing specification for the Task tool. When named agent
// definitions are configured, their names+descriptions are appended to the
// description (progressive disclosure, like the Skill tool enumerates skills) so
// the model can choose a specialist via the optional `agent` arg.
func (t *TaskTool) Spec() tool.ToolSpec {
	desc := "Delegate a focused read-only investigation — 'search → summarize', " +
		"'read N files → report findings' — to a subagent with its own fresh context. " +
		"Returns only the subagent's final summary. Use when exploration would bloat the " +
		"main context. The subagent cannot see this conversation, so put everything it " +
		"needs in `prompt`; it runs read-only tools (Read/Grep/Glob), cannot make changes, " +
		"and cannot delegate further."
	desc += t.agentEnumeration()
	return tool.ToolSpec{
		Name:        taskToolName,
		Description: desc,
		Schema:      taskSchema,
	}
}

// agentEnumeration renders the "Available agents:" tail listing each configured
// def's "name: description", or "" when none are configured. The list is taken in
// the (already name-sorted) order the composition root supplied, so the spec is
// byte-stable across turns.
func (t *TaskTool) agentEnumeration() string {
	if len(t.agentMeta) == 0 {
		return ""
	}
	var b strings.Builder
	b.WriteString("\n\nAvailable specialist agents (pass the name as `agent`):")
	for _, m := range t.agentMeta {
		fmt.Fprintf(&b, "\n- %s: %s", m.Name, m.Description)
	}
	return b.String()
}

// ReadOnly reports that the Task tool is read-only, which lets the parent's
// dispatcher run Task CONCURRENTLY with other read-only tools that share the same
// Workspace (read-parallel / mutate-serial; see dispatch.go).
//
// INVARIANT — this is safe ONLY while the child catalog stays read-only. The
// composition root MUST wire childEngine with a read-only explorer catalog (Read,
// Grep, Glob; see NewTaskTool). Wiring a mutating tool (Edit/Write/non-RO Bash)
// into a child while ReadOnly() still returns true would let a subagent mutate
// the shared Workspace concurrently with the parent's other read-only calls,
// breaking the read-parallel safety guarantee and racing on the filesystem. If a
// mutating child is ever needed, ReadOnly() must return false so the dispatcher
// serializes Task with everything else.
func (*TaskTool) ReadOnly() bool { return true }

// Execute runs one subagent: it builds a FRESH child Session (own conversation,
// own Limits, its configured mode), runs the child loop via the injected child
// Engine against the SAME workspace ws, drains the child's entire Event stream
// internally, and returns only the child's final summary text as a single
// ToolResult. The parent therefore never observes the child's intermediate
// events (gauntlet #7).
//
// The child run is bounded by the parent ctx: cancelling the parent cancels the
// child. Any permission ask the child raises is auto-denied so the child is
// non-interactive. When the child finishes, the SubagentStop hook fires
// best-effort.
func (t *TaskTool) Execute(ctx context.Context, call session.ToolCall, ws tool.Workspace) (session.ToolResult, error) {
	// The plain Execute path forwards nothing: a nil emit makes the run silent, so
	// existing callers (and the team supervisor's reuse of the drain contract) are
	// unaffected by the observability seam.
	return t.run(ctx, call, ws, nil)
}

// ExecuteObserved runs the subagent like Execute but, when emit is non-nil,
// forwards a REDACTED, metadata-only projection of the child's activity to the
// parent run's event stream via the three subagent.* events. emit only sequences
// and channels events; it never touches the parent's Conversation, so this is
// orthogonal to context isolation (gauntlet #7): the child's CONTENT still never
// enters the parent context. It is the observableTool seam the dispatcher calls.
func (t *TaskTool) ExecuteObserved(ctx context.Context, call session.ToolCall, ws tool.Workspace, emit func(session.Event)) (session.ToolResult, error) {
	return t.run(ctx, call, ws, emit)
}

// run is the shared implementation behind Execute (emit == nil) and
// ExecuteObserved (emit != nil). It builds a FRESH child session, runs the child
// loop against the SAME workspace, drains the child's entire Event stream
// internally, optionally forwards a redacted projection of that activity, and
// returns only the child's final summary text as a single ToolResult.
func (t *TaskTool) run(ctx context.Context, call session.ToolCall, ws tool.Workspace, emit func(session.Event)) (session.ToolResult, error) {
	var args taskArgs
	if msg, ok := session.ParseArgs(call, &args); !ok {
		return session.NewToolError(call.ID, "Task: "+msg), nil
	}
	if strings.TrimSpace(args.Prompt) == "" {
		return session.NewToolError(call.ID, "Task: 'prompt' is required and must be non-empty"), nil
	}

	// Route to a named specialist when requested; otherwise the default explorer.
	// An unknown name is a model-addressable error listing the valid names, so the
	// model can retry — it never silently falls back (which would run the wrong
	// scope/prompt under the requested name).
	engine := t.childEngine
	// limits default to the Task tool's own (the no-`agent` explorer bound); a named
	// agent with per-def limits overrides them below.
	limits := t.limits
	if name := strings.TrimSpace(args.Agent); name != "" {
		eng, ok := t.agentEngines[name]
		if !ok {
			return session.NewToolError(call.ID, "Task: "+t.unknownAgentHint(name)), nil
		}
		engine = eng
		if l, ok := t.agentLimits[name]; ok {
			limits = l
		}
	}

	// A fresh child session: own conversation, own (tighter) Limits, scoped to the
	// SAME workspace root as the parent so the subagent explores the same project.
	// When a named agent def pins limits, the child runs under THOSE; otherwise it
	// uses the Task tool's default limits.
	childID := t.childSessionID(call.ID)
	child := session.New(
		childID,
		t.childMode,
		ws.Root(),
		limits,
		time.Now(),
	)

	// Announce the subagent before it runs, carrying only the parent call id, the
	// child id, and a short, plain-text goal label (sanitization happens in the
	// UI). No child content.
	if emit != nil {
		emit(session.Event{Type: session.EvSubagentStart, Subagent: &session.SubagentPayload{
			ParentCallID: string(call.ID),
			ChildID:      string(childID),
			Goal:         subagentGoal(args),
		}})
	}

	start := time.Now()
	run := engine.Run(ctx, child, ws, args.Prompt)

	// Drain the child's Event stream entirely INSIDE the Task tool. Nothing from
	// the child surfaces to the parent except the final summary string and, when
	// observed, the redacted subagent.* metadata. Auto-deny any permission ask so
	// the child can never block on a human (defensive: the recommended wiring is an
	// allow-all read-only policy that never asks).
	final, stop, usage, toolCount := drainChildObserved(run, emit, string(call.ID), string(childID))

	if emit != nil {
		emit(session.Event{Type: session.EvSubagentEnd, Subagent: &session.SubagentPayload{
			ParentCallID: string(call.ID),
			ChildID:      string(childID),
			ToolCount:    toolCount,
			Usage:        usage,
			Stop:         stop,
			DurationMs:   time.Since(start).Milliseconds(),
		}})
	}

	// Fire SubagentStop best-effort, regardless of how the child ended.
	t.fireSubagentStop(ctx, child)

	if stop == session.StopError {
		msg := final
		if msg == "" {
			msg = "subagent failed without producing a summary"
		}
		return session.NewToolError(call.ID, "Task: "+msg), nil
	}
	if final == "" {
		final = "(subagent produced no summary)"
	}
	return session.NewToolResult(call.ID, final), nil
}

// subagentGoal derives the short, plain-text goal label forwarded on
// EvSubagentStart: the explicit description when supplied, else the (model-
// authored) prompt. It is metadata for the card title; it is NOT child content
// (the prompt is the parent's own instruction to the child). Both paths are
// clamped identically so the goal always stays a single, bounded line — an
// explicit description is just as capable of being long or multi-line as a prompt.
func subagentGoal(args taskArgs) string {
	if g := strings.TrimSpace(args.Description); g != "" {
		return truncateGoal(g)
	}
	return truncateGoal(strings.TrimSpace(args.Prompt))
}

// truncateGoal normalizes a goal label into a single bounded line: it collapses
// any newlines (and tabs) to spaces so a multi-line value can't break the one-line
// Task-card title, then clamps to maxSubagentGoalLen runes, appending an ellipsis
// when it overflows. It is rune-aware so it never splits a multi-byte character.
func truncateGoal(s string) string {
	s = strings.Map(func(r rune) rune {
		if r == '\n' || r == '\r' || r == '\t' {
			return ' '
		}
		return r
	}, s)
	r := []rune(s)
	if len(r) <= maxSubagentGoalLen {
		return s
	}
	return strings.TrimRight(string(r[:maxSubagentGoalLen]), " ") + "…"
}

// drainChildObserved consumes the child Run's Event channel to completion,
// applying the non-interactive child contract (auto-deny asks) via
// handleChildEvent, and returns the terminal result text, stop reason, the
// child's cumulative usage, and the number of child tool calls observed.
//
// When emit is non-nil it ALSO forwards a REDACTED projection of the child's
// activity: on each child tool RESULT it emits an EvSubagentTool carrying ONLY
// the tool name (looked up from the matching tool.call) + the error bool + a
// running count. It forwards NO child tool args, NO child result content, and NO
// child message.delta text. This keeps gauntlet #7 intact while giving the UI
// metadata-only visibility. With a nil emit it discards every intermediate event
// exactly as the original drainChild did.
func drainChildObserved(run *Run, emit func(session.Event), parentCallID, childID string) (finalText string, stop session.StopReason, usage session.Usage, toolCount int) {
	// Track child callID → tool name so a tool.result can be attributed to its
	// tool.call name without forwarding the call's (redacted) args.
	names := map[session.ToolCallID]string{}
	for ev := range run.Events() {
		if emit != nil {
			switch {
			case ev.Type == session.EvToolCall && ev.ToolCall != nil:
				names[ev.ToolCall.ID] = ev.ToolCall.Name
			case ev.Type == session.EvToolResult && ev.ToolResult != nil:
				toolCount++
				emit(session.Event{Type: session.EvSubagentTool, Subagent: &session.SubagentPayload{
					ParentCallID: parentCallID,
					ChildID:      childID,
					ToolName:     names[ev.ToolResult.CallID],
					IsError:      ev.ToolResult.IsError,
					ToolCount:    toolCount,
				}})
			}
		}
		if text, st, ok := handleChildEvent(run, ev); ok {
			finalText, stop = text, st
			if ev.Result != nil {
				usage = ev.Result.Usage
			}
		}
	}
	return finalText, stop, usage, toolCount
}

// drainChild consumes the child Run's Event channel to completion, auto-denying
// any permission ask (subagents are non-interactive), and returns the terminal
// result text and stop reason. It deliberately discards every intermediate event
// (turn.start, message.delta, tool.call, tool.result, hook, compaction) so none
// of them can reach the parent — this is the context-isolation guarantee of
// gauntlet #7. It is the silent variant used by fork.go and the team supervisor;
// the observed Task path uses drainChildObserved.
func drainChild(run *Run) (finalText string, stop session.StopReason) {
	final, st, _, _ := drainChildObserved(run, nil, "", "")
	return final, st
}

// handleChildEvent applies the non-interactive CHILD contract to a single event of
// a child/member run: it auto-denies any permission ask (no human is attached to a
// child loop, so it must never block) and, when the event is the terminal result,
// reports its text and stop reason via isResult=true. It is the SINGLE definition
// of that contract, shared by drainChild (which discards events) and the team
// supervisor's runTurn (which forwards them) so the auto-deny rule and the
// result/stop capture cannot drift between the two.
func handleChildEvent(run *Run, ev session.Event) (text string, stop session.StopReason, isResult bool) {
	if ev.Type == session.EvPermissionAsk && ev.Ask != nil {
		run.Approve(ev.Ask.AskID, false)
	}
	if ev.Type == session.EvResult && ev.Result != nil {
		return ev.Result.Text, ev.Result.Stop, true
	}
	return "", session.StopNone, false
}

// fireSubagentStop runs the SubagentStop lifecycle hook for a finished child run.
// It is best-effort: a hook error or a block outcome is ignored (a subagent's
// completion cannot be vetoed after the fact).
func (t *TaskTool) fireSubagentStop(ctx context.Context, child *session.Session) {
	fireNotify(ctx, t.hooks, governance.HookEvent{
		Phase:     governance.PhaseSubagentStop,
		SessionID: string(child.ID),
	})
}

// unknownAgentHint builds the model-addressable error text for a Task call that
// names an agent that is not registered. It lists the valid names so the model can
// retry, mirroring the Skill tool's available-names hint.
func (t *TaskTool) unknownAgentHint(name string) string {
	if len(t.agentMeta) == 0 {
		return fmt.Sprintf("unknown agent %q (no specialist agents are configured; omit `agent` to use the default explorer)", name)
	}
	names := make([]string, 0, len(t.agentMeta))
	for _, m := range t.agentMeta {
		names = append(names, m.Name)
	}
	return fmt.Sprintf("unknown agent %q; available agents: %s", name, strings.Join(names, ", "))
}

// childSessionID derives a stable, unique id for a child session from the parent
// tool call id.
func (t *TaskTool) childSessionID(callID session.ToolCallID) session.SessionID {
	return session.SessionID(fmt.Sprintf("%s-%s", t.idPrefix, callID))
}

// Compile-time assertion that TaskTool satisfies the Tool contract and the
// agent-internal observableTool seam.
var (
	_ tool.Tool      = (*TaskTool)(nil)
	_ observableTool = (*TaskTool)(nil)
)
