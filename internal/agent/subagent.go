package agent

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"strings"
	"sync"
	"time"

	"github.com/stacklok/mecatl/internal/governance"
	"github.com/stacklok/mecatl/internal/port"
	"github.com/stacklok/mecatl/internal/session"
	"github.com/stacklok/mecatl/internal/tool"
)

// subagentToolName is the catalog name of the subagent delegation tool.
const subagentToolName = "Subagent"

// maxSubagentGoalLen caps the prompt-derived goal label forwarded on
// EvSubagentStart when no explicit description is supplied. It keeps the
// subagent card title compact and bounds how much of the (model-authored) prompt
// is echoed to the event stream.
const maxSubagentGoalLen = 60

// defaultMaxConcurrentChildren bounds how many Subagent children may run at once.
// Subagent is read-only (ReadOnly()==true), so the dispatcher runs Subagent calls
// concurrently and the model can fan MANY out in a single turn; each child consumes
// a child session + an LLM slot (and, when shell-bearing, a forked git worktree —
// disk + a `git worktree add` process), so an unbounded fan-out is real resource
// pressure. The gate bounds ALL Subagent children — forking AND forker-less — so the
// read-parallel fan-out cannot create unbounded child runs at once. The default
// mirrors the team supervisor's defaultTeamConcurrency and the fork concurrency cap.
const defaultMaxConcurrentChildren = 4

// observableTool is the agent-internal seam by which a tool may forward a
// REDACTED, allowlisted projection of its internal activity to the parent run's
// event stream WITHOUT widening the public tool.Tool interface. A tool that
// implements it is given an emit closure (bound by the dispatcher to the parent
// Run, so events are sequenced and mirrored to the sink exactly like the loop's
// own emits); a tool that does not is executed via the ordinary Execute path.
//
// The Subagent tool implements this to surface subagent.start/tool/end metadata.
// Crucially, the emit closure only sequences and channels events — it NEVER
// touches the parent's session.Conversation — so this observability is orthogonal
// to the context-isolation guarantee (gauntlet #7).
type observableTool interface {
	ExecuteObserved(ctx context.Context, call session.ToolCall, ws tool.Workspace, emit func(session.Event)) (session.ToolResult, error)
}

// parentCaps carries the PARENT run's interactivity and the surface seam down to a
// subagent-spawning tool (Subagent/Team/Fork), so a child's permission ask can be SURFACED
// to the human when the parent is interactive (and auto-denied with an accurate message
// + operator diagnostic when it is headless). It is the symmetric back-channel to the
// emit closure: where emit pushes child observability UP, surfaceAsk routes a parent
// verdict back DOWN to the child Run.
//
// It is layering-clean: every field is an agent-layer closure or a plain bool; no
// adapter/server/proto type crosses. A tool that does not implement childCapableTool
// (or a nil caps) gets the legacy headless auto-deny posture, unchanged.
type parentCaps struct {
	// interactive is the PARENT run's interactivity: true when a human approver is
	// attached (the surfaced ask can be answered), false for a headless run.
	interactive bool
	// surfaceAsk registers the child Run in the parent router (so the parent's
	// Approve routes the verdict to it), records the ask's OWNERSHIP against childID in
	// the parent's child-run registry (so a CancelChild can retract it), and emits a
	// REDACTED parent EvPermissionAsk for the child's ask. It is register-then-emit:
	// registration happens before the emit so a fast verdict cannot race ahead. nil when
	// the parent installed no router (headless / no surface). The emitted ask carries
	// the SURFACED askID (the child's own askID, already parent-distinguishable).
	// childID is the child SESSION id (childPosture.childID) the ask is owned by.
	// the router auto-unregisters the askID on the routed verdict
	// (childAskRouter.route); a stale entry (child cancelled while parked) is a harmless
	// no-op against the idempotent registry, so no explicit unsurface seam is needed.
	// requester is the pre-composed, already-clamp-safe attribution phrase (e.g.
	// `subagent "fix flaky tests"`, `team member "researcher"`, `parallel branch
	// "branch-2"`) the parent frames the surfaced ask with; empty keeps the legacy
	// generic "subagent" framing.
	surfaceAsk func(askID, childID string, child *Run, ask session.PendingAsk, requester string)
	// children is the parent run's child-run registry, handed down DIRECTLY — it is
	// an agent-package type, so passing the handle has zero layering cost (unlike
	// surfaceAsk, which stays a closure because it genuinely composes router
	// registration + redaction + the parent emit). The spawning tool registers each
	// child's per-CALL cancel here (BEFORE the concurrency gate, so a queued child
	// is already cancellable), lands its terminal stop, and reads the
	// clientCancelled disambiguation — all via the nil-safe wrappers below; a later
	// iteration's SubagentStatus reads come free off the same handle. nil when the
	// tool is driven without parent caps (plain Execute/ExecuteObserved) — the
	// child then simply is not client-cancellable, unchanged behaviour.
	children *childRunRegistry
	// diag is the parent run's run-scoped diagnostics, used to emit the headless
	// auto-deny operator diagnostic (LevelInfo, tagged agent=<child identity>: the child
	// session id "subagent-<callID>" for Subagent children; the member name / fork label
	// for the others). nil → no diagnostic (NopDiagnostics-safe via the caller).
	diag port.Diagnostics
	// hardAbort is the parent Run's explicit unwedge signal (Run.hardAbort, fired
	// a short grace after Run.Cancel — see hardAbortGrace), handed down so a delegation tool's own
	// internal forwarding sends (the team supervisor's member→evCh forward) can give
	// up when the parent run is cancelled while its consumer stopped draining — the
	// state in which the parent's terminate paths (and so the registry seal) are
	// unreachable. nil on a zero parentCaps (plain Execute/ExecuteObserved): a nil
	// channel in a select blocks forever, which IS the correct no-abort behaviour.
	hardAbort <-chan struct{}
}

// registerChildRun is the nil-safe registration wrapper a spawning tool calls: a
// zero parentCaps (plain Execute/ExecuteObserved, no parent run threaded) makes it a
// no-op — the child then simply is not client-cancellable, unchanged behaviour.
// background marks a detached-delivery Subagent child (the other families always
// pass false).
func (c parentCaps) registerChildRun(childID session.SessionID, family childFamily, goal string, cancel context.CancelFunc, background bool) {
	if c.children != nil {
		c.children.register(string(childID), family, goal, cancel, background)
	}
}

// startChildRun is the nil-safe running-state advance (the child's drive has
// genuinely started: concurrency slot held / first round driven).
func (c parentCaps) startChildRun(childID session.SessionID) {
	if c.children != nil {
		c.children.markRunning(string(childID))
	}
}

// finishChildRun is the nil-safe terminal-stop wrapper (deferred by the spawning
// tool so every exit path lands the terminal stop in the registry).
func (c parentCaps) finishChildRun(childID session.SessionID, stop session.StopReason) {
	if c.children != nil {
		c.children.markDone(string(childID), stop)
	}
}

// finishChildRunResult is finishChildRun plus the rendered terminal ToolResult a
// BACKGROUND child stores for SubagentStatus collection (A2: the sole body
// channel).
func (c parentCaps) finishChildRunResult(childID session.SessionID, stop session.StopReason, res *session.ToolResult) {
	if c.children != nil {
		c.children.markDoneResult(string(childID), stop, res)
	}
}

// abortChildRun is the nil-safe PRE-START abort (the A5 state vocabulary): the
// child never started driving and its failure already surfaced inline, so its
// registry entry is removed rather than left as a done+StopNone phantom.
func (c parentCaps) abortChildRun(childID session.SessionID) {
	if c.children != nil {
		c.children.remove(string(childID))
	}
}

// liveBackgroundChildIDs is the nil-safe read of the currently-live background
// child ids (the gate-full fail-fast error's list — ids only, A9).
func (c parentCaps) liveBackgroundChildIDs() []string {
	if c.children == nil {
		return nil
	}
	return c.children.liveBackgroundIDs()
}

// childWasClientCancelled is the nil-safe clientCancelled read (false when no
// parent caps were threaded).
func (c parentCaps) childWasClientCancelled(childID session.SessionID) bool {
	return c.children != nil && c.children.clientCancelled(string(childID))
}

// childCapableTool is the optional seam by which a subagent-spawning tool also receives
// the parent's capabilities (interactivity + the surface back-channel). A tool that
// implements it is driven via ExecuteWithParent when the dispatcher has a parentCaps to
// pass; one that does not falls back to ExecuteObserved/Execute with the legacy headless
// posture. Subagent/Team/Fork implement it.
type childCapableTool interface {
	ExecuteWithParent(ctx context.Context, call session.ToolCall, ws tool.Workspace, emit func(session.Event), caps parentCaps) (session.ToolResult, error)
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
// Subagent tool (and a team member, via WithTeamLimits) runs under when the
// caller does not override them. The composition layer uses it as the per-field
// FALLBACK when deriving a def's session.Limits from its maxTurns/maxToolCalls:
// a zero def field inherits the matching default here, so a def that sets neither
// is bounded exactly as before.
func DefaultChildLimits() session.Limits { return defaultChildLimits }

// subagentArgs is the argument payload the model supplies when calling the Subagent tool.
// A subagent gets a single, self-contained instruction (its whole prompt — it has
// no shared context with the parent) and an optional short description used only
// for observability.
type subagentArgs struct {
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

	// Resume optionally CONTINUES a previously-run subagent by its persisted session id
	// (the value of the result trailer's `agentId:` line, verbatim). The child's prior
	// conversation is reloaded and Prompt becomes its next instruction; the run gets a
	// FRESH workspace fork (the original worktree is gone — a harness staleness note is
	// prepended so the child knows). Mutually exclusive with Agent AND Model (v1 resumes
	// on the default explorer engine only). Empty = start fresh (unchanged).
	Resume string `json:"resume,omitempty"`

	// MaxTurns optionally TIGHTENS the child's per-run turn limit for THIS call. It is
	// a pointer so an omitted value (nil) is distinguishable from an explicit 0; when
	// present it overrides the inherited limit only if it is LOWER (tighten-only — the
	// model may make its child stricter than the operator's bound, never looser, so a
	// per-call arg can't be used to escape the configured ceiling). A non-positive value
	// is ignored (treated as "no override").
	MaxTurns *int `json:"max_turns,omitempty"`
	// MaxToolCalls optionally TIGHTENS the child's per-run tool-call limit for THIS
	// call. Same pointer + tighten-only + non-positive-ignored semantics as MaxTurns.
	MaxToolCalls *int `json:"max_tool_calls,omitempty"`
	// TimeoutMs optionally imposes a WALL-CLOCK deadline (milliseconds) on this child
	// run via context.WithTimeout. It is a hard ceiling independent of the turn/tool
	// limits: a child that exceeds it is cancelled and the result is a time-budget tool
	// error. A non-positive value is ignored (no deadline).
	TimeoutMs *int `json:"timeout_ms,omitempty"`

	// Model optionally PINS this child to a specific provider model for THIS call
	// (cheaper for fan-out, stronger for deep analysis), overriding the inherited
	// parent/explorer model. It is an opaque provider-model string; the composition
	// root resolves it to a contamination-safe child engine via the injected engine
	// factory (so Compactor/TokenCounter/Env.Model/ContextWindow are re-derived for the
	// override model — never a clone-and-swap of the LLM on an existing engine). An
	// unknown/unroutable model is a model-addressable error. Empty = inherit. It is
	// REJECTED together with `agent` (a specialist already pins its own engine/model).
	Model string `json:"model,omitempty"`

	// OutputSchema optionally requests STRUCTURED output: a model-authored JSON schema
	// (a SUBSET — object/array/string/number/integer/boolean/null/properties/required/
	// items/enum). When present the child is given a synthetic SubmitResult tool whose
	// parameters ARE this schema and is instructed to call it to deliver; the submitted
	// payload is validated against the schema (session.ValidateJSON) and, on a mismatch,
	// a model-visible correction is re-injected and the child re-driven, BOUNDED. The
	// validated payload becomes the Subagent result text. Omitted (the default) = today's
	// free-text behaviour, unchanged.
	OutputSchema json.RawMessage `json:"output_schema,omitempty"`

	// MaxTokens optionally imposes a per-call TIGHTEN-ONLY cumulative TOKEN ceiling on
	// this child run (input+output, the loop-level token budget). It rides a Run-scoped
	// override on the SHARED child engine (no fresh engine needed), folded tighten-only
	// with the operator default: the lower non-zero value wins, so a per-call ceiling can
	// make the child stricter than the operator's bound, never looser. A non-positive
	// value is ignored (inherit the engine's budget). A budget-stopped child returns its
	// best-effort summary (StopBudget is a clean terminal), not an error.
	MaxTokens *int `json:"max_tokens,omitempty"`

	// Background detaches this child: the call returns IMMEDIATELY with a started-
	// result carrying the agentId, the child keeps driving in its own goroutine, and
	// its rendered result is stored in the parent run's child registry for collection
	// via SubagentStatus (the SOLE body channel — A2). It composes with resume/
	// output_schema/limits/timeout_ms/agent/model unchanged. Background lifetime is
	// RUN-scoped (D8): a background child still live when the parent run terminates is
	// cancelled, joined (bounded drain), and persisted — resumable in a later run.
	// Acquiring the shared childGate is FAIL-FAST for background (D12): a full gate is
	// a model-addressable error listing the live background ids, never a cross-turn
	// block the model could deadlock itself on.
	Background bool `json:"background,omitempty"`
}

// AgentMeta is the plain (name, description) summary of one registered agent
// definition, surfaced in the Subagent tool's Spec().Description for progressive
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
	// maxTurns/maxToolCalls (per-field falling back to the Subagent tool's default
	// limits), so a def with no limits carries the same bound as the default
	// explorer. A zero Limits value is treated as "no per-def override" — Execute
	// then uses the Subagent tool's default limits, exactly as the no-`agent` path does.
	Limits session.Limits
}

// subagentSchema is the JSON schema the model sees for the Subagent tool's arguments. The
// `agent` property is always present (optional); the available agent NAMES are
// enumerated in the tool's Spec().Description tail (progressive disclosure), not
// baked into this schema, so the schema stays byte-stable regardless of how many
// defs are configured.
var subagentSchema = json.RawMessage(`{
  "type": "object",
  "properties": {
    "prompt": {
      "type": "string",
      "description": "The full, self-contained instruction for the subagent. It has a FRESH context window and cannot see this conversation, so include everything it needs — and state the expected output format of its final report (e.g. 'a bulleted list of file:line findings with a one-line conclusion')."
    },
    "description": {
      "type": "string",
      "description": "A short (3-8 word) human-readable label for this task, shown wherever the subagent's progress is displayed (e.g. 'audit auth error paths'). Recommended."
    },
    "agent": {
      "type": "string",
      "description": "Optional name of a configured specialist agent to route this delegation to (see the list in the tool description). Omit to use the default read-only explorer."
    },
    "resume": {
      "type": "string",
      "description": "Optional id of a previous subagent to RESUME (the value of the 'agentId:' line on its earlier Subagent result). The subagent continues with its full prior conversation, taking this call's prompt as its next instruction. It runs in a FRESH workspace checkout: file changes and build state from its earlier run are gone (its conversational memory survives; the working tree does not). Cannot be combined with the agent or model arguments. Omit to start a fresh subagent."
    },
    "max_turns": {
      "type": "integer",
      "description": "Optional cap on the subagent's model turns for THIS call. Tighten-only: it can make the subagent stricter than the default, never looser. Omit to use the default."
    },
    "max_tool_calls": {
      "type": "integer",
      "description": "Optional cap on the subagent's total tool calls for THIS call. Tighten-only (as max_turns). Omit to use the default."
    },
    "timeout_ms": {
      "type": "integer",
      "description": "Optional wall-clock deadline in milliseconds for the whole subagent run; if it exceeds this it is cancelled and returns a time-budget error. Omit for no deadline."
    },
    "model": {
      "type": "string",
      "description": "Optional provider model id to pin THIS subagent to (e.g. a cheaper model for wide fan-out, a stronger one for deep analysis). Omit to inherit the parent's model. Cannot be combined with the agent argument (a specialist already pins its own model)."
    },
    "max_tokens": {
      "type": "integer",
      "description": "Optional cap on the subagent's total token spend (input+output) for THIS call. Tighten-only: it can make the subagent stricter than the default, never looser. When reached the subagent stops cleanly and returns its best-effort summary. Omit to use the default."
    },
    "output_schema": {
      "type": "object",
      "description": "Optional JSON schema describing the structured result you want back. Use it when you will mechanically consume the result (e.g. comparing or aggregating several subagents' answers); omit for a free-text summary. When present, the subagent must deliver by calling a SubmitResult tool with JSON matching this schema; the validated JSON is returned as the result. Supports a subset: type/properties/required/items/enum."
    },
    "background": {
      "type": "boolean",
      "description": "Run the subagent in the BACKGROUND: this call returns immediately with its agentId and the subagent keeps working while you continue (a note tells you when it finishes). Use it for long investigations whose result you do not need before your next steps. Collect the result with SubagentStatus (pass the agentId; use wait_ms to wait on it). A background subagent still running when this run ends is CANCELLED (its transcript persists and is resumable). Omit (default false) to wait for the result inline."
    }
  },
  "required": ["prompt"]
}`)

// SubagentTool is the subagent delegation tool (gauntlet #7). It is a tool.Tool that,
// when executed, spins up a CHILD agent loop with its own fresh Session, its own
// (tighter) Limits, and a SCOPED tool catalog — supplied by the injected child
// *Engine — runs it to completion, and returns ONLY the child's final summary
// string as a single ToolResult.
//
// Workspace: when a child forker is wired (WithChildForker — the composition root
// wires it iff the child catalog includes Bash), each child runs in its OWN isolated
// git WORKTREE (shares the base repo's `.git` ⇒ full history) so the explorer's shell
// can inspect (git log/show, cat, build, test) without its writes touching the shared
// parent workspace; the worktree is torn down after the child drains. Without a
// forker the child has no Bash and runs against the parent workspace, exactly as it
// originally did. Either way Subagent stays read-parallel-safe (see ReadOnly).
//
// Context isolation is the whole point: the parent never observes the child's
// intermediate tool.call / tool.result / message.delta events. The child's Event
// stream is drained entirely inside Execute; only the terminal result text folds
// back into the parent conversation. This keeps a noisy "search → read N files →
// summarize" investigation from bloating the main context window.
//
// The child Engine is built by the composition root (cmd/mecated, WP11) with a
// read-only explorer catalog (Read, Grep, Glob) that NEVER includes the Subagent tool
// itself — so a subagent cannot recurse — and an allow-all policy over those
// read-only tools so the child never needs to prompt a human. See NewSubagentTool.
//
// Resume: when a store is wired (WithSubagentStore), a Subagent call carrying `resume`
// CONTINUES a previously-run child by its persisted id (the result trailer's `agentId:`
// line) — the prior conversation is reloaded and its terminal state recovered (completed
// →Reopen, cancelled→Interrupt; failed is not resumable), then it runs on the default
// explorer engine in a FRESH workspace fork (the original worktree is gone; a staleness
// note is prepended). An in-flight guard rejects a concurrent run on the same id.
type SubagentTool struct {
	// childEngine runs the subagent loop. It is pre-wired by the composition root
	// with the scoped catalog, the (optionally cheaper) model, and an allow/deny
	// policy appropriate for a non-interactive child. It is never the parent
	// Engine: the parent Engine is not mutated. It is the fallback for the
	// no-`agent` (default explorer) case.
	childEngine *Engine

	// agentEngines maps an agent-definition NAME to its pre-built, read-only child
	// Engine. The composition root builds one per def (scoped catalog + resolved
	// model + body→Role prompt) and injects the map via WithAgentEngines. A Subagent
	// call with a known `agent` runs that engine instead of childEngine; an empty
	// map (the default) means no specialists are configured and Subagent behaves
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

	// childForker, when non-nil, isolates each child run in its OWN forked workspace
	// (a cheap git WORKTREE — shares the base repo's `.git` ⇒ full history) instead of
	// running against the shared parent workspace. The composition root wires it ONLY
	// when the child catalog includes Bash, so a shell-bearing read-only explorer runs
	// its (mutating-classified) Bash in a throwaway worktree, never the shared base —
	// which is what keeps Subagent read-parallel-safe (see ReadOnly). When nil, the child
	// runs against the parent ws exactly as before (no shell wired). A fork FAILURE on
	// this path is a tool error, NOT a silent fallback to the shared ws: the child's
	// catalog has Bash precisely because isolation was available, so running it in the
	// shared base would be the exact hazard isolation exists to prevent.
	childForker tool.WorkspaceForker

	// childGate bounds how many Subagent children may run CONCURRENTLY — forking AND
	// forker-less. It is a buffered channel used as a counting semaphore, acquired at
	// the top of run() (before any fork) and released when the call returns, so the
	// dispatcher's read-parallel fan-out of N Subagent calls in one turn can never start more
	// than cap children at once (each consumes a child session + an LLM slot, and a
	// shell-bearing child additionally a forked worktree). It is always sized in
	// NewSubagentTool (never nil), so the gate is the single fan-out brake for every Subagent
	// child. Capacity is defaultMaxConcurrentChildren unless overridden by
	// WithMaxConcurrentChildren.
	childGate chan struct{}

	// engineFactory, when non-nil, mints a child engine for a per-call `model`
	// override. It is a composition-supplied closure (WithSubagentEngineFactory) closing
	// over the provider registry: given an opaque model string it returns a child
	// engine built through the SAME contamination-safe per-provider path the named-agent
	// engines use (engineDepsForProvider re-derives Compactor/TokenCounter/Env.Model/
	// ContextWindow for the override model) — NEVER a clone-and-swap of the LLM on an
	// existing engine. It returns ok=false for an unknown/unroutable model, which Subagent
	// renders as a model-addressable error. nil (the default) means no per-call model
	// override is wired (a `model` arg then errors with a clear "not supported" message).
	// It is layering-clean: the closure takes a string and returns *Engine — both
	// agent-layer types — and no adapter/proto/server type crosses (same shape as
	// WithAgentEngines).
	engineFactory func(model string) (*Engine, bool)

	// idPrefix seeds the generated child SessionID so child sessions are
	// distinguishable in logs/stores.
	idPrefix string

	// store, when non-nil, best-effort persists each child session after its run so an
	// out-of-band reader (the InspectSubagent tool) can load its transcript by the
	// agentId the result trailer surfaces. nil disables persistence (persistMember
	// discipline: advisory, save failures swallowed). NAMESPACE NOTE (document-don't-
	// engineer): child ids ("subagent-<callID>") share the one session store with the
	// service's crypto-random session ids and the team members' "team-<teamID>-<member>"
	// ids; the prefixes keep them disjoint by convention, and jsonlstore sanitizes any
	// id into a safe filename, so no collision engineering is needed.
	store port.SessionStore

	// mu guards inFlight. It is a plain mutex held only for the map's read-modify-write,
	// never across the child run.
	mu sync.Mutex
	// inFlight registers every child session id (FRESH and RESUME) currently being driven
	// by this tool, so a second concurrent Execute on the SAME id is rejected with a
	// model-visible error rather than racing two runs over one unlocked Session aggregate.
	// It is correctness (the aggregate is not goroutine-safe) AND liveness (waiting would
	// park a dispatcher goroutine + a gate slot). Registered BEFORE acquireChildSlot, so
	// the conflict is detected even while the second call would otherwise block on the gate.
	inFlight map[session.SessionID]struct{}
}

// defaultStructuredOutputRetries bounds how many CORRECTION re-drives a
// structured-output child gets after a SubmitResult payload fails schema validation
// (or the child never calls SubmitResult), before the Subagent tool gives up with
// StopStructuredOutput. It mirrors defaultNoProgressNudges (2): the FIRST attempt plus
// this many corrections. It is a bounded retry counter — NOT tool_choice forcing
// (incompatible with Anthropic thinking + the OpenAI reasoning path).
//
// LIMIT SEMANTICS (per-attempt vs cross-attempt): each correction re-drive Reopen()s
// the child session, which RESETS its Counters — so the per-call MaxTurns/MaxToolCalls
// (and a def's WithChildLimits) bound EACH ATTEMPT independently, giving a
// structured-output child effectively up to (1+defaultStructuredOutputRetries)× its
// per-child turn/tool budget across the whole call. That is BOUNDED (a small constant
// multiplier), not a runaway. The cross-attempt ceiling is the TOKEN budget
// (Deps.MaxRunTokens / the per-call max_tokens override): driveChild SUMS usage across
// every drive (usage = usage.Add(u)) and RE-PASSES the same runOpts (carrying the
// tighten-only override) to each RunContentWith, so the token budget genuinely
// accumulates across attempts and is the real cross-attempt brake.
const defaultStructuredOutputRetries = 2

// submitResultToolName is the catalog name of the synthetic deliverable tool a
// structured-output child is given. It is run-scoped (RunOptions.ExtraTools), never
// registered into any shared catalog.
const submitResultToolName = "SubmitResult"

// resumeStalenessNote is the honest harness preface prepended to a resumed child's
// prompt: the conversation survives but the workspace does not (the original
// worktree was torn down; this run gets a fresh fork), so the child must not trust
// earlier filesystem observations. Same accuracy discipline as childAutoDenyMessage.
const resumeStalenessNote = "[harness note: your conversation has been resumed, but you are running in a FRESH workspace checkout — file changes, build artifacts, and running processes from your earlier run are GONE. Re-run commands and re-read files before relying on earlier observations.]"

// SubagentOption configures a SubagentTool.
type SubagentOption func(*SubagentTool)

// WithChildLimits overrides the subagent's stop conditions. Use it to make a
// child even tighter (or, rarely, looser) than the defaults.
func WithChildLimits(l session.Limits) SubagentOption {
	return func(t *SubagentTool) { t.limits = l }
}

// WithChildMode sets the permission mode the child session runs under (default
// session.ModeDefault). session.ModePlan additionally hides any non-read-only
// tools from the child at the catalog level.
func WithChildMode(m session.PermissionMode) SubagentOption {
	return func(t *SubagentTool) { t.childMode = m }
}

// WithSubagentStopHook injects the HookRunner that fires the SubagentStop hook
// when a child run finishes. It is best-effort: a hook error or block never fails
// the Subagent call. Passing nil disables the hook.
func WithSubagentStopHook(h port.HookRunner) SubagentOption {
	return func(t *SubagentTool) { t.hooks = h }
}

// WithChildSessionPrefix sets the prefix used to derive child SessionIDs (default
// "subagent"). Child ids are of the form "<prefix>-<callID>".
func WithChildSessionPrefix(p string) SubagentOption {
	return func(t *SubagentTool) { t.idPrefix = p }
}

// WithChildForker injects the workspace-isolation seam each child run forks before
// executing. The composition root wires it ONLY when the child catalog includes Bash
// (the read-only explorer's shell), so the child's mutating-classified Bash lands in
// a throwaway git worktree, never the shared parent base — preserving Subagent's
// read-parallel safety (see ReadOnly). It should be the forker's DEFAULT mode (git
// worktree: shares the base repo's `.git` ⇒ full history for git log/show). When the
// forker is nil (the default), the child runs against the parent workspace exactly as
// before. A fork failure on this path is a tool error, not a silent fallback.
func WithChildForker(f tool.WorkspaceForker) SubagentOption {
	return func(t *SubagentTool) { t.childForker = f }
}

// WithSubagentStore injects the optional session store each child session is
// best-effort persisted to after its run (ALL terminals: clean, limit-stopped,
// structured-output-exhausted, errored, cancelled/timed out — the final state after
// any structured-output re-drives). Nil disables persistence. Mirrors the team
// supervisor's WithMemberStore/persistMember discipline: the save is advisory and a
// failure is swallowed (no diagnostic). The tool consumes the port.SessionStore
// interface, never a concrete adapter, so no layering rule is crossed.
func WithSubagentStore(store port.SessionStore) SubagentOption {
	return func(t *SubagentTool) { t.store = store }
}

// WithMaxConcurrentChildren bounds how many Subagent children may run CONCURRENTLY —
// forking AND forker-less (default defaultMaxConcurrentChildren). It is the single
// fan-out brake on the dispatcher's read-parallel batch: N Subagent calls in one turn each
// block on the gate, so at most cap children run at once. A value < 1 is clamped to 1
// (a zero-capacity gate would deadlock).
func WithMaxConcurrentChildren(n int) SubagentOption {
	return func(t *SubagentTool) {
		if n < 1 {
			n = 1
		}
		t.childGate = make(chan struct{}, n)
	}
}

// WithSubagentEngineFactory injects the composition-supplied factory that mints a child
// engine for a per-call `model` override. The closure closes over the provider
// registry and builds the override child through the contamination-safe per-provider
// path (engineDepsForProvider) — Compactor/TokenCounter/Env.Model/ContextWindow are
// re-derived for the override model, NEVER a clone-and-swap of the LLM on an existing
// engine. It returns (engine, true) for a routable model and (nil, false) otherwise
// (an unknown/unroutable model, which Subagent surfaces as a model-addressable error).
// nil (the default) leaves Subagent without a per-call model override (a `model` arg then
// errors). It is the layering-clean seam: only func(string)(*Engine,bool) crosses into
// internal/agent (same shape as WithAgentEngines).
func WithSubagentEngineFactory(f func(model string) (*Engine, bool)) SubagentOption {
	return func(t *SubagentTool) { t.engineFactory = f }
}

// WithAgentEngines injects the per-definition child engines (keyed by agent name)
// and their (name, description) metadata for progressive disclosure. The
// composition root builds each engine with a SCOPED, read-only catalog (the Subagent
// read-only invariant is preserved — see ReadOnly) and the def's resolved
// model/prompt, then passes the map and a name-sorted meta slice here.
//
// engines and meta should describe the same set of names; meta drives the Spec
// enumeration while engines drives routing. A nil/empty map leaves Subagent with only
// the default explorer (no behaviour change). It is the agent-package boundary the
// agents adapter never crosses: only plain map + structs flow in.
func WithAgentEngines(engines map[string]*Engine, meta []AgentMeta) SubagentOption {
	return func(t *SubagentTool) {
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

// NewSubagentTool constructs the Subagent tool tool over a pre-built child *Engine.
//
// The composition root (cmd/mecated, WP11) is responsible for building childEngine
// with the SCOPED child catalog and policy. The recommended, deterministic wiring
// is:
//
//   - Catalog: a read-only explorer set — Read, Grep, Glob ONLY. It MUST NOT
//     contain the Subagent tool (otherwise a subagent could spawn subagents — infinite
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
// childEngine must be non-nil; NewSubagentTool panics otherwise, because a Subagent tool
// with no child loop to delegate to is a programming error at the composition
// root.
func NewSubagentTool(childEngine *Engine, opts ...SubagentOption) tool.Tool {
	if childEngine == nil {
		panic("agent: NewSubagentTool requires a non-nil child Engine")
	}
	t := &SubagentTool{
		childEngine: childEngine,
		limits:      defaultChildLimits,
		childMode:   session.ModeDefault,
		idPrefix:    "subagent",
		inFlight:    make(map[session.SessionID]struct{}),
	}
	for _, o := range opts {
		o(t)
	}
	// Always size the child-concurrency gate (forking AND forker-less): a read-parallel
	// fan-out of N Subagent calls in one turn each consumes a child session + an LLM slot, so
	// the gate is the single fan-out brake bounding how many children run at once. The
	// operator may override the default via WithMaxConcurrentChildren.
	if t.childGate == nil {
		t.childGate = make(chan struct{}, defaultMaxConcurrentChildren)
	}
	return t
}

// Spec returns the model-facing specification for the Subagent tool. When named agent
// definitions are configured, their names+descriptions are appended to the
// description (progressive disclosure, like the Skill tool enumerates skills) so
// the model can choose a specialist via the optional `agent` arg.
func (t *SubagentTool) Spec() tool.ToolSpec {
	desc := "Delegate a focused, self-contained task to a subagent with its own fresh context: " +
		"a multi-step investigation ('search → summarize', 'trace this code path') or build/test/git " +
		"work ('run the tests and report failures', 'bisect the history'). It runs read-only tools " +
		"(Read/Grep/Glob) plus a full shell in an isolated, throwaway git worktree — it can build, " +
		"test, and inspect history, but its file changes are DISCARDED (no Edit/Write) and it cannot " +
		"delegate further. The subagent's FINAL MESSAGE is its deliverable — you receive only that " +
		"(see `prompt`; with `background: true` the call instead returns at once and you collect the " +
		"result later). You may issue several Subagent calls in ONE turn. Do NOT use it when you need " +
		"the intermediate outputs in this conversation (do the work yourself), when file changes must " +
		"be kept (use Parallel), or when workers must coordinate (use Team) — and don't delegate a " +
		"single quick read you can do with Read/Grep." +
		" Every result starts with an 'agentId:' line — pass that id to SubagentStatus (this run's " +
		"live state; collects background results), to InspectSubagent to read the full transcript, " +
		"or as `resume` to continue that subagent with a follow-up prompt (fresh workspace; its " +
		"conversation survives)."
	desc += t.agentEnumeration()
	return tool.ToolSpec{
		Name:        subagentToolName,
		Description: desc,
		Schema:      subagentSchema,
	}
}

// agentEnumeration renders the "Available agents:" tail listing each configured
// def's "name: description", or "" when none are configured. The list is taken in
// the (already name-sorted) order the composition root supplied, so the spec is
// byte-stable across turns.
func (t *SubagentTool) agentEnumeration() string {
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

// ReadOnly reports that the Subagent tool is read-only, which lets the parent's
// dispatcher run Subagent CONCURRENTLY with other read-only tools that share the same
// Workspace (read-parallel / mutate-serial; see dispatch.go).
//
// INVARIANT — what keeps this safe is WORKSPACE ISOLATION, not catalog
// read-only-ness. A Subagent child may now WRITE via Bash (the read-only explorer's
// shell — git, build, test, cat), but when a child forker is wired (childForker !=
// nil — the composition root wires it iff the child catalog has Bash) the child runs
// in an ISOLATED git WORKTREE, so its writes land in a throwaway checkout and NEVER
// touch the shared parent workspace the parent's other read-only calls race over.
// The read-parallel guarantee therefore holds exactly as before: no two concurrent
// dispatched tools ever mutate the same tree.
//
// The only surface a worktree child shares with the parent is the `.git` object
// DB/refs (git-locked for concurrent access; config-driven code-execution vectors
// — hooks/pager/fsmonitor/external-diff — neutralized by the sandboxed runner's
// gitenv env in the composition layer), and a detached-HEAD worktree's stray commit
// is dangling and gc-able. A fork FAILURE is surfaced as a tool error, never a
// silent fallback to the shared ws (which WOULD break this), so the invariant cannot
// be violated by a degraded fork.
//
// When NO forker is wired the child has no Bash (the catalog stays a pure read-only
// explorer) and runs against the shared ws — also safe, by catalog read-only-ness,
// exactly as it always was. Either way Subagent is read-parallel-safe and ReadOnly()
// honestly returns true.
func (*SubagentTool) ReadOnly() bool { return true }

// Execute runs one subagent: it builds a FRESH child Session (own conversation,
// own Limits, its configured mode) — or, on `resume`, reloads the persisted child
// session and recovers its terminal state (completed→Reopen, cancelled→Interrupt;
// failed is not resumable) before driving it in a NEW workspace fork — runs the
// child loop via the injected child Engine, drains the child's entire Event stream
// internally, and returns only the child's final summary text as a single
// ToolResult. The parent therefore never observes the child's intermediate
// events (gauntlet #7).
//
// The child run is bounded by the parent ctx: cancelling the parent cancels the
// child. Any permission ask the child raises is auto-denied so the child is
// non-interactive. When the child finishes, the SubagentStop hook fires
// best-effort.
func (t *SubagentTool) Execute(ctx context.Context, call session.ToolCall, ws tool.Workspace) (session.ToolResult, error) {
	// The plain Execute path forwards nothing: a nil emit makes the run silent, so
	// existing callers (and the team supervisor's reuse of the drain contract) are
	// unaffected by the observability seam.
	return t.run(ctx, call, ws, nil, parentCaps{})
}

// ExecuteObserved runs the subagent like Execute but, when emit is non-nil,
// forwards a REDACTED, metadata-only projection of the child's activity to the
// parent run's event stream via the three subagent.* events. emit only sequences
// and channels events; it never touches the parent's Conversation, so this is
// orthogonal to context isolation (gauntlet #7): the child's CONTENT still never
// enters the parent context. It is the observableTool seam the dispatcher calls.
func (t *SubagentTool) ExecuteObserved(ctx context.Context, call session.ToolCall, ws tool.Workspace, emit func(session.Event)) (session.ToolResult, error) {
	return t.run(ctx, call, ws, emit, parentCaps{})
}

// ExecuteWithParent is the childCapableTool seam: it runs the subagent like
// ExecuteObserved but threads the PARENT's capabilities (interactivity + the surface
// back-channel) into the child posture, so a child Bash ask that A1/A2 did not
// auto-resolve is SURFACED to the human (interactive) or auto-denied with the accurate
// message + operator diagnostic (headless).
func (t *SubagentTool) ExecuteWithParent(ctx context.Context, call session.ToolCall, ws tool.Workspace, emit func(session.Event), caps parentCaps) (session.ToolResult, error) {
	return t.run(ctx, call, ws, emit, caps)
}

// run is the shared implementation behind Execute (emit == nil) and
// ExecuteObserved (emit != nil). It builds a FRESH child session, runs the child
// loop against the SAME workspace, drains the child's entire Event stream
// internally, optionally forwards a redacted projection of that activity, and
// returns only the child's final summary text as a single ToolResult.
// selectChildEngine resolves the child engine + base session limits for a Subagent call
// from its `agent` / `model` arguments (mutually exclusive — R9). It returns
// ok=false with a model-addressable error ToolResult on a bad selection (agent+model
// together, an unknown agent, an unwired/unroutable model), and the chosen engine +
// limits on success. The default explorer + the Subagent tool's default limits is the
// no-arg case.
func (t *SubagentTool) selectChildEngine(callID session.ToolCallID, args subagentArgs) (engine *Engine, limits session.Limits, errResult session.ToolResult, ok bool) {
	// `agent` and `model` are mutually exclusive: a named specialist already pins its
	// own engine/model/prompt/scope, so layering a call-time model over it would
	// silently break the def's contract. Reject the combination with a clear error.
	wantAgent := strings.TrimSpace(args.Agent)
	wantModel := strings.TrimSpace(args.Model)
	if wantAgent != "" && wantModel != "" {
		return nil, session.Limits{}, session.NewToolError(callID,
			"Subagent: specify `agent` OR `model`, not both — a specialist agent already pins its own model"), false
	}

	// Route to a named specialist when requested; otherwise the default explorer. An
	// unknown name is a model-addressable error listing the valid names, so the model
	// can retry — it never silently falls back (which would run the wrong scope/prompt).
	engine = t.childEngine
	limits = t.limits // default explorer bound; a named agent with per-def limits overrides.
	if wantAgent != "" {
		eng, found := t.agentEngines[wantAgent]
		if !found {
			return nil, session.Limits{}, session.NewToolError(callID, "Subagent: "+t.unknownAgentHint(wantAgent)), false
		}
		engine = eng
		if l, found := t.agentLimits[wantAgent]; found {
			limits = l
		}
	}

	// Per-call model override (R9/D6): mint a child engine for the requested model via
	// the composition-supplied factory, which re-derives Compactor/TokenCounter/
	// Env.Model/ContextWindow for the override model (no clone-and-swap). An unknown/
	// unroutable model is a model-addressable error; without a wired factory the
	// override is unsupported (an honest error, never a silent inherit).
	if wantModel != "" {
		if t.engineFactory == nil {
			return nil, session.Limits{}, session.NewToolError(callID,
				"Subagent: per-call `model` override is not supported in this deployment"), false
		}
		eng, found := t.engineFactory(wantModel)
		if !found || eng == nil {
			return nil, session.Limits{}, session.NewToolError(callID,
				fmt.Sprintf("Subagent: unknown or unroutable model %q; omit `model` to inherit the parent's model", wantModel)), false
		}
		engine = eng
	}
	return engine, limits, session.ToolResult{}, true
}

// buildSubagentRunOptions assembles the per-call RunOptions, the synthetic SubmitResult
// tool, and the effective child prompt for one Subagent run:
//   - Per-call token ceiling (R4): a Run-scoped TIGHTEN-ONLY MaxRunTokens override carried
//     via RunContentWith, bounding the SHARED child engine WITHOUT minting a fresh engine.
//     0 ⇒ inherit the engine's operator-default budget; it folds tighten-only in the loop.
//   - Resume staleness note: on RESUME the effective prompt is prefixed with the honest
//     resumeStalenessNote (the conversation survives but the workspace does not) BEFORE the
//     structured-output wrap, so the note rides inside the structured prompt too. A fresh
//     call is unchanged.
//   - Structured output (D1/D2): when an output_schema is supplied, a synthetic SubmitResult
//     tool (run-scoped — never registered into the shared catalog) whose parameters ARE the
//     schema is created and the prompt is wrapped to instruct the child to call it. Omitted
//     ⇒ today's free-text path.
func buildSubagentRunOptions(args subagentArgs, resuming bool) (RunOptions, *submitResultTool, string) {
	var runOpts RunOptions
	if args.MaxTokens != nil && *args.MaxTokens > 0 {
		runOpts.MaxRunTokensOverride = *args.MaxTokens
	}
	prompt := args.Prompt
	if resuming {
		prompt = resumeStalenessNote + "\n\n" + args.Prompt
	}
	var submit *submitResultTool
	if len(args.OutputSchema) > 0 && strings.TrimSpace(string(args.OutputSchema)) != "" {
		submit = newSubmitResultTool(args.OutputSchema)
		runOpts.ExtraTools = []tool.Tool{submit}
		// Wrap the (possibly staleness-noted) prompt so a resumed structured-output child
		// still sees the staleness note inside the structured-output instruction.
		prompt = structuredOutputPrompt(prompt, args.OutputSchema)
	}
	return runOpts, submit, prompt
}

// validateResume checks a `resume` Subagent call's preconditions BEFORE any engine
// selection or load: a store must be wired (resume needs persistence), `resume` is
// mutually exclusive with `agent`/`model` (a resumed child runs on the default explorer
// engine only), and the resume id must be a SUBAGENT id (the prefix gate rejects team
// member / service ids — keyed on t.idPrefix+"-", NOT a literal). It returns the default
// explorer engine on success, or a model-addressable error ToolResult (ok=false).
func (t *SubagentTool) validateResume(callID session.ToolCallID, args subagentArgs) (engine *Engine, errResult session.ToolResult, ok bool) {
	if t.store == nil {
		return nil, session.NewToolError(callID, "Subagent: `resume` is not supported in this deployment (no session store wired)"), false
	}
	if strings.TrimSpace(args.Agent) != "" || strings.TrimSpace(args.Model) != "" {
		return nil, session.NewToolError(callID,
			"Subagent: `resume` cannot be combined with `agent` or `model` — a resumed subagent continues on the default explorer engine"), false
	}
	if !strings.HasPrefix(args.Resume, t.idPrefix+"-") {
		return nil, session.NewToolError(callID,
			fmt.Sprintf("Subagent: resume id %q is not a subagent session; only ids from a Subagent result's 'agentId:' line can be resumed (team member transcripts are read-only via InspectMember)", args.Resume)), false
	}
	return t.childEngine, session.ToolResult{}, true
}

// resolveEngineAndLimits resolves one Subagent call's child engine and base
// session limits, then applies the per-call tighten-only overrides.
//
// Resume mode CONTINUES a previously-run subagent by its persisted id: it forces
// the default explorer engine (v1: a resumed child runs on childEngine only) and
// is mutually exclusive with `agent`/`model` (those pin their own engine) — the
// validation runs BEFORE selectChildEngine so the exclusivity error is the
// model's first signal, and the prefix check rejects non-subagent ids (e.g. a
// team member's transcript). On resume, limits is later overwritten from the
// LOADED session's preserved Limits (resolveResumeSession), then tightened
// against the per-call args there.
//
// The tighten-only discipline: the model may make THIS child stricter than the
// inherited bound, never looser, so a per-call arg can't escape the operator's
// ceiling (tightenLimit ignores nil / non-positive values and only lowers).
func (t *SubagentTool) resolveEngineAndLimits(callID session.ToolCallID, args subagentArgs, resuming bool) (engine *Engine, limits session.Limits, errResult session.ToolResult, ok bool) {
	if resuming {
		eng, errRes, vok := t.validateResume(callID, args)
		if !vok {
			return nil, session.Limits{}, errRes, false
		}
		engine = eng
	} else {
		eng, lim, errRes, sok := t.selectChildEngine(callID, args)
		if !sok {
			return nil, session.Limits{}, errRes, false
		}
		engine, limits = eng, lim
	}
	limits.MaxTurns = tightenLimit(limits.MaxTurns, args.MaxTurns)
	limits.MaxToolCalls = tightenLimit(limits.MaxToolCalls, args.MaxToolCalls)
	return engine, limits, session.ToolResult{}, true
}

// prepareChildSession resolves everything between the concurrency slot and the
// drive for a FOREGROUND call: the resume load (+ terminal recovery), the
// workspace fork, and the child session build.
//
// Ordering is load-bearing: resume load + terminal recovery happen BEFORE the
// fork, so the common error cases (unknown id, failed/non-resumable state,
// broken store) FAIL FAST without paying a fork/unfork round-trip; the recovered
// session is re-homed AFTER the fork, once the fresh root exists (see
// buildChildSession). A fork failure is a tool error, never a silent fallback
// (forkChildWorkspace). The returned cleanup is ALWAYS non-nil (a no-op when
// nothing survives) so the caller can defer it unconditionally; on a
// session-build failure the just-created fork is torn down here.
func (t *SubagentTool) prepareChildSession(ctx context.Context, call session.ToolCall, ws tool.Workspace, args subagentArgs, resuming bool, childID session.SessionID, limits session.Limits) (child *session.Session, runWS tool.Workspace, cleanup func() error, errResult session.ToolResult, ok bool) {
	noop := func() error { return nil }
	var resumedChild *session.Session
	if resuming {
		loaded, errRes, rok := t.resolveResumeSession(ctx, call.ID, childID, args)
		if !rok {
			return nil, nil, noop, errRes, false
		}
		resumedChild = loaded
	}
	runWS, cleanupWS, errRes, fok := t.forkChildWorkspace(ctx, call.ID, ws, subagentGoal(args))
	if !fok {
		return nil, nil, noop, errRes, false
	}
	child, errRes, bok := t.buildChildSession(call.ID, childID, resumedChild, runWS.Root(), limits)
	if !bok {
		_ = cleanupWS()
		return nil, nil, noop, errRes, false
	}
	return child, runWS, cleanupWS, session.ToolResult{}, true
}

func (t *SubagentTool) run(ctx context.Context, call session.ToolCall, ws tool.Workspace, emit func(session.Event), caps parentCaps) (session.ToolResult, error) {
	var args subagentArgs
	if msg, ok := session.ParseArgs(call, &args); !ok {
		return session.NewToolError(call.ID, "Subagent: "+msg), nil
	}
	if strings.TrimSpace(args.Prompt) == "" {
		return session.NewToolError(call.ID, "Subagent: 'prompt' is required and must be non-empty"), nil
	}

	resuming := strings.TrimSpace(args.Resume) != ""
	engine, limits, errResult, ok := t.resolveEngineAndLimits(call.ID, args, resuming)
	if !ok {
		return errResult, nil
	}

	// Background requires the parent run's child registry: it is where the started
	// child's rendered result lands for SubagentStatus collection and what the
	// run-end drain joins. A caps-less drive (plain Execute/ExecuteObserved) has
	// neither, so a background request there is an honest error, never a silent
	// foreground fallback (the model was promised detached delivery).
	if args.Background && caps.children == nil {
		return session.NewToolError(call.ID,
			"Subagent: `background` is not supported on this run (no child registry); omit it to run in the foreground"), nil
	}

	// Compute the child session id early (a FRESH call derives it from the parent call id;
	// a RESUME continues the persisted id verbatim) so the in-flight guard can register it
	// BEFORE acquiring the concurrency slot. The guard rejects a SECOND concurrent run on
	// the SAME id with a model-visible error rather than waiting: two runs over one unlocked
	// Session aggregate is a data race (correctness), and waiting would park a dispatcher
	// goroutine + a gate slot (liveness). Registered BEFORE acquireChildSlot so the conflict
	// is detected even while the second call would otherwise block on the gate. A BACKGROUND
	// child holds its id until its detached goroutine ends, so `resume` of a still-running
	// background child is rejected here, unchanged.
	childID := t.childSessionID(call.ID)
	if resuming {
		childID = session.SessionID(args.Resume)
	}
	if !t.tryAcquireChildID(childID) {
		return session.NewToolError(call.ID,
			fmt.Sprintf("Subagent: subagent %q is already running; wait for its result before resuming it", childID)), nil
	}
	// From here every exit path must release childID: the foreground path defers it
	// below; the background path transfers ownership to the detached goroutine.

	// Per-call wall-clock deadline: a hard ceiling on the whole child run, independent
	// of the turn/tool limits. A child that exceeds it is ctx-cancelled (the loop
	// terminates with StopCancelled), which the terminal rendering turns into a
	// time-budget tool error. nil / non-positive ⇒ no deadline (ctx unchanged). The
	// timeout ctx is held separately (timeoutCtx) so the terminal rendering can tell a
	// deadline-kill (DeadlineExceeded) apart from a parent cancellation. cancelTimeout
	// is an explicit func (not a bare defer) because the background path hands it to
	// the detached goroutine.
	var timeoutCtx context.Context
	cancelTimeout := func() {}
	if args.TimeoutMs != nil && *args.TimeoutMs > 0 {
		var ct context.CancelFunc
		ctx, ct = context.WithTimeout(ctx, time.Duration(*args.TimeoutMs)*time.Millisecond)
		cancelTimeout = ct
		timeoutCtx = ctx
	}

	// Per-child cancel: mint the per-CALL cancelable context the parent's CancelChild
	// targets and register it in the parent run's child registry. The per-CALL ctx (not
	// any single drive's) is the cancel target so a structured-output re-drive sequence
	// is cancelled as a whole; it wraps the (possibly timeout-bearing) ctx, so the
	// per-call timeout and a parent-run cancel keep their existing semantics — the
	// timeoutCtx deadline check below stays first, and a parent cancel leaves
	// clientCancelled false. Registration happens BEFORE acquireChildSlot so a child
	// queued on the gate is already cancellable (the gate's ctx select unblocks). A
	// `resume` of an id already run THIS run re-registers and OVERWRITES the done
	// entry (fresh doneCh — A5).
	ctx, cancelCall := context.WithCancel(ctx)
	caps.registerChildRun(childID, childFamilySubagent, subagentGoal(args), cancelCall, args.Background)

	// BACKGROUND (D7/D8): fail-fast gate, synchronous start event, detach the drive.
	if args.Background {
		return t.startBackground(ctx, backgroundChild{
			call: call, ws: ws, emit: emit, caps: caps, args: args,
			engine: engine, limits: limits, resuming: resuming, childID: childID,
			timeoutCtx: timeoutCtx, cancelCall: cancelCall, cancelTimeout: cancelTimeout,
		}), nil
	}

	// FOREGROUND: the call owns its whole lifecycle inline.
	defer t.releaseChildID(childID)
	defer cancelCall()
	defer cancelTimeout()
	// Terminal accounting (the A5 state vocabulary): a child whose drive STARTED
	// lands its real terminal stop; a pre-start CANCELLATION (gate wait) lands the
	// meaningful StopCancelled; any other pre-start failure (resume-load / fork /
	// session-build — its error already returned inline) ABORTS the entry (removed,
	// never a done+StopNone phantom in the SubagentStatus roster).
	started := false
	var terminalStop session.StopReason
	defer func() {
		switch {
		case started || terminalStop != session.StopNone:
			caps.finishChildRun(childID, terminalStop)
		default:
			caps.abortChildRun(childID)
		}
	}()

	// Bound concurrent children FIRST, for ALL Subagent children (forking AND forker-less):
	// the dispatcher fans Subagent calls out read-parallel, and each child consumes a child
	// session + an LLM slot (and, when shell-bearing, a forked worktree). Acquire at the
	// top of the call and release when it returns, so at most cap children run at once.
	release := t.acquireChildSlot(ctx)
	if release == nil {
		// ctx cancelled while waiting for a slot — surface it as a tool error; the parent
		// ctx governs the whole call. A CLIENT cancel (CancelChild while queued) is named
		// accurately so the model knows the user withdrew this delegation, not that the
		// run is collapsing. Either way the cancellation is a MEANINGFUL pre-start
		// terminal (StopCancelled), not an abort.
		terminalStop = session.StopCancelled
		if caps.childWasClientCancelled(childID) {
			return session.NewToolError(call.ID,
				"Subagent: subagent was cancelled by the user while waiting for a concurrency slot"), nil
		}
		return session.NewToolError(call.ID, "Subagent: cancelled before acquiring a concurrency slot"), nil
	}
	defer release()
	caps.startChildRun(childID)

	// Resume load + fork + session build (see prepareChildSession). The cleanup is
	// always non-nil and tears the worktree down after the child fully drains (the
	// run is drained below in this call), so a deferred cleanup is correct.
	child, runWS, cleanupWS, errResult, ok := t.prepareChildSession(ctx, call, ws, args, resuming, childID, limits)
	if !ok {
		return errResult, nil
	}
	defer func() { _ = cleanupWS() }()

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

	// Build the run options (per-call token ceiling), the synthetic SubmitResult tool (when
	// structured output is requested), and the effective prompt (with the resume staleness
	// note prepended BEFORE the structured-output wrap). See buildSubagentRunOptions.
	runOpts, submit, prompt := buildSubagentRunOptions(args, resuming)

	// A child forking a worktree (childForker != nil) runs ISOLATED, so its Bash asks
	// are eligible for the A2 worktree-safe auto-approve; a forker-less child is
	// base-sharing (no auto-approve). The parent caps carry interactivity + the surface
	// back-channel for an interactive parent; headless leaves them zero (auto-deny).
	posture := childPosture{isolated: t.childForker != nil, caps: caps, role: string(childID),
		childID:  string(childID),
		askLabel: fmt.Sprintf("subagent %q", subagentGoal(args))}

	start := time.Now()
	started = true
	// Drain the child's Event stream entirely INSIDE the Subagent tool. Nothing from the
	// child surfaces to the parent except the final summary string and, when observed,
	// the redacted subagent.* metadata. The structured-output retry loop re-drives the
	// SAME child session (Reopen) with a correction prompt on a validation miss; the
	// free-text path runs exactly one drive.
	final, stop, usage, toolCount := driveChild(ctx, engine, child, runWS, prompt, runOpts, emit, call, childID, posture, submit, args.OutputSchema)
	terminalStop = stop

	// Best-effort persist of the child's FINAL state (after any structured-output
	// re-drives) so InspectSubagent can load it by the trailer id. persistMember
	// discipline: nil store disables; a save failure is advisory and swallowed.
	// NOTE: ctx may already be cancelled here (parent cancel / timeout_ms) — both
	// shipped stores ignore ctx on Save, mirroring persistMember; a future
	// ctx-honouring store would drop the save on that path (same documented
	// residual the supervisor carries).
	t.persistChild(ctx, child)

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

	// Time-budget terminal: the per-call deadline fired (timeoutCtx deadline exceeded)
	// rather than a parent cancellation, so the child stopped because it ran out of its
	// allotted wall-clock time. Render it as a model-addressable time-budget tool error
	// so the model learns the call hit its own limit (distinct from a generic failure).
	if timeoutCtx != nil && timeoutCtx.Err() == context.DeadlineExceeded {
		return session.NewToolError(call.ID,
			fmt.Sprintf("Subagent: subagent exceeded its time budget (%dms) and was stopped\n\nagentId: %s", *args.TimeoutMs, childID)), nil
	}

	// Client cancel (CancelChild): distinguished from a parent-run cancel by the
	// registry flag, read AFTER the timeout check above so a real deadline keeps its
	// time-budget error.
	return renderSubagentResult(call.ID, childID, final, stop, submit, caps.childWasClientCancelled(childID)), nil
}

// backgroundStartedBody is the immediate started-result body a background Subagent
// call returns (D7, amended by A2: SubagentStatus is the SOLE body channel — the
// model is told to COLLECT, not promised an auto-delivery). It is rendered through
// renderSubagentTrailer, so the agentId rides the FIRST line exactly like every
// other Subagent result (the existing trailer convention the model and the resume
// path already parse).
const backgroundStartedBody = "subagent started in the background.\n\n" +
	"It keeps working while you continue; a note will tell you when it finishes. " +
	"Collect its result with SubagentStatus (or wait on it with wait_ms); it will be " +
	"cancelled if it is still running when this run ends."

// backgroundChild bundles everything one detached background Subagent drive owns:
// the original call/workspace/emit/caps, the resolved engine+limits, and the
// lifecycle handles (per-call cancel, timeout cancel, gate release, in-flight id)
// whose ownership the synchronous path TRANSFERS to the goroutine.
type backgroundChild struct {
	call     session.ToolCall
	ws       tool.Workspace
	emit     func(session.Event)
	caps     parentCaps
	args     subagentArgs
	engine   *Engine
	limits   session.Limits
	resuming bool
	// resumed is the loaded+recovered session on a resume call (loaded SYNCHRONOUSLY
	// in startBackground so an unknown id / non-resumable state fails fast inline,
	// not as a collectible background error).
	resumed *session.Session
	childID session.SessionID
	// timeoutCtx is non-nil iff a per-call timeout_ms deadline applies (the
	// DeadlineExceeded disambiguation read, same as the foreground path).
	timeoutCtx    context.Context //nolint:containedctx // deadline-disambiguation handle, mirrors run()'s timeoutCtx local
	cancelCall    context.CancelFunc
	cancelTimeout context.CancelFunc
	release       func()
}

// startBackground is the synchronous half of a background Subagent call: fail-fast
// gate acquisition (D12), fail-fast resume load, the SYNCHRONOUS EvSubagentStart
// (A5 — deterministic start-before-started-result ordering on the stream), then
// the detached goroutine spawn and the immediate started-result. Every pre-spawn
// failure unwinds completely (registry entry removed — no phantom queued entries,
// A5 — gate slot and in-flight id released) and returns inline.
func (t *SubagentTool) startBackground(ctx context.Context, b backgroundChild) session.ToolResult {
	abort := func() {
		b.caps.abortChildRun(b.childID)
		b.cancelCall()
		b.cancelTimeout()
		t.releaseChildID(b.childID)
	}
	// D12: background acquisition is FAIL-FAST — a background child holds its slot
	// ACROSS turns, so blocking here could deadlock the model against itself. The
	// error lists the live background ids (ids only — A9) and the recoverable
	// actions. ORDER is load-bearing: abort() FIRST, so the failing call's own
	// pre-gate registration is gone before the live-ids read — read first, the
	// error would list the very id that just failed to start as "currently
	// running" (and a failed resume attempt would shadow the prior done entry the
	// abort reinstates).
	release, ok := t.tryAcquireChildSlot()
	if !ok {
		abort()
		ids := b.caps.liveBackgroundChildIDs()
		return session.NewToolError(b.call.ID, backgroundGateFullError(ids))
	}
	b.release = release
	// Resume load fails FAST and inline (a cheap store read): an unknown id or a
	// non-resumable state is the model's immediate, addressable error — never a
	// "started" result whose collection later reveals the call never could run.
	if b.resuming {
		loaded, errResult, lok := t.resolveResumeSession(ctx, b.call.ID, b.childID, b.args)
		if !lok {
			b.release()
			abort()
			return errResult
		}
		b.resumed = loaded
	}
	b.caps.startChildRun(b.childID)
	// A5: the start event is emitted SYNCHRONOUSLY before the goroutine spawns, so
	// subagent.start always precedes the started-result on the stream.
	if b.emit != nil {
		b.emit(session.Event{Type: session.EvSubagentStart, Subagent: &session.SubagentPayload{
			ParentCallID: string(b.call.ID),
			ChildID:      string(b.childID),
			Goal:         subagentGoal(b.args),
			Background:   true,
		}})
	}
	go t.driveBackground(ctx, b)
	return session.NewToolResult(b.call.ID, renderSubagentTrailer(b.childID, backgroundStartedBody))
}

// driveBackground is the detached goroutine owning a background child's whole
// remaining lifecycle: fork → drive → persist → markDone(rendered result + stop).
// It is run-scoped (D8): its ctx derives from the parent run's, the run-end drain
// cancels and joins it (doneCh closes in the deferred finishChildRunResult), and
// every emit it makes goes through the registry's seal guard, so a residual emit
// after an abandon is a safe no-op. The deferred block releases the gate slot,
// the in-flight id, and both cancels — the ownership transferred from run() —
// strictly BEFORE the doneCh close, so a drain-join happens-after every release.
func (t *SubagentTool) driveBackground(ctx context.Context, b backgroundChild) {
	var (
		res  session.ToolResult
		stop = session.StopError
	)
	defer func() {
		// Ownership releases FIRST, the done signal LAST. markDoneResult (inside
		// finishChildRunResult) closes the registry doneCh the run-end drain JOINS
		// on, so everything a successor run can observe — the gate slot, the
		// in-flight child id — must be released happens-before that close.
		// Signalling first let a freshly-joined drain's NEXT run race the deferred
		// releaseChildID and bounce a legitimate resume of this very child with a
		// spurious "already running" (the TestBackgroundChildCancelledAtRunEnd CI
		// flake — a real model-visible bug, not test noise).
		b.cancelCall()
		b.cancelTimeout()
		b.release()
		t.releaseChildID(b.childID)
		b.caps.finishChildRunResult(b.childID, stop, &res)
	}()

	goal := subagentGoal(b.args)
	// A post-spawn failure (fork / session build) is NOT a registry abort: the model
	// already holds the started-result, so the failure must be COLLECTIBLE — it lands
	// as a done(StopError) entry whose stored result is the error text, and the start
	// event gets its closing subagent.end so no client lane dangles.
	endOnError := func(errResult session.ToolResult) {
		res = errResult
		if b.emit != nil {
			b.emit(session.Event{Type: session.EvSubagentEnd, Subagent: &session.SubagentPayload{
				ParentCallID: string(b.call.ID),
				ChildID:      string(b.childID),
				Stop:         session.StopError,
			}})
		}
	}
	runWS, cleanupWS, errResult, ok := t.forkChildWorkspace(ctx, b.call.ID, b.ws, goal)
	if !ok {
		endOnError(errResult)
		return
	}
	defer func() { _ = cleanupWS() }()
	child, errResult, ok := t.buildChildSession(b.call.ID, b.childID, b.resumed, runWS.Root(), b.limits)
	if !ok {
		endOnError(errResult)
		return
	}

	runOpts, submit, prompt := buildSubagentRunOptions(b.args, b.resuming)
	posture := childPosture{isolated: t.childForker != nil, caps: b.caps, role: string(b.childID),
		childID:  string(b.childID),
		askLabel: fmt.Sprintf("subagent %q", goal)}

	start := time.Now()
	final, st, usage, toolCount := driveChild(ctx, b.engine, child, runWS, prompt, runOpts, b.emit, b.call, b.childID, posture, submit, b.args.OutputSchema)
	stop = st

	// Best-effort persist on EVERY terminal — including the run-end drain's cancel —
	// so the child is resumable in a later run (the loss-mitigation that makes
	// cancel-at-end acceptable). Same documented ctx-on-Save residual as foreground.
	t.persistChild(ctx, child)

	if b.emit != nil {
		b.emit(session.Event{Type: session.EvSubagentEnd, Subagent: &session.SubagentPayload{
			ParentCallID: string(b.call.ID),
			ChildID:      string(b.childID),
			ToolCount:    toolCount,
			Usage:        usage,
			Stop:         st,
			DurationMs:   time.Since(start).Milliseconds(),
		}})
	}
	t.fireSubagentStop(ctx, child)

	// Terminal rendering mirrors the foreground call exactly (renderSubagentResult is
	// the single rendering chokepoint): time-budget first, then the client-cancel
	// disambiguation. The rendered result is what SubagentStatus delivers verbatim.
	if b.timeoutCtx != nil && b.timeoutCtx.Err() == context.DeadlineExceeded {
		res = session.NewToolError(b.call.ID,
			fmt.Sprintf("Subagent: subagent exceeded its time budget (%dms) and was stopped\n\nagentId: %s", *b.args.TimeoutMs, b.childID))
		return
	}
	res = renderSubagentResult(b.call.ID, b.childID, final, st, submit, b.caps.childWasClientCancelled(b.childID))
}

// tryAcquireChildSlot is acquireChildSlot's NON-BLOCKING sibling for background
// acquisition (D12 fail-fast): it returns (release, true) when a slot is free and
// (nil, false) when the gate is full — never waits.
func (t *SubagentTool) tryAcquireChildSlot() (func(), bool) {
	if t.childGate == nil {
		return func() {}, true
	}
	select {
	case t.childGate <- struct{}{}:
		var once sync.Once
		return func() { once.Do(func() { <-t.childGate }) }, true
	default:
		return nil, false
	}
}

// backgroundGateFullError renders the D12 fail-fast error: model-addressable,
// listing the currently-live background ids (ids ONLY — A9, nothing
// model/child-authored) and the recoverable actions.
func backgroundGateFullError(ids []string) string {
	msg := "Subagent: background subagent concurrency limit reached"
	if len(ids) > 0 {
		msg += "; currently running in the background: " + strings.Join(ids, ", ")
	}
	return msg + ". Wait for one to finish with SubagentStatus (use wait_ms), or run this task in the foreground."
}

// renderSubagentResult labels the child's terminal by stop reason (D4 — the typed result
// taxonomy), surfaced in the MODEL-VISIBLE result, and stamps the agentId trailer (D5).
// The mapping:
//   - StopError                         → tool error (the child crashed).
//   - StopStructuredOutput              → tool error carrying the last validation
//     failure (the child never produced a schema-valid payload within the retry budget).
//   - StopMaxTurns / StopMaxToolCalls   → success-with-note (stopped at a limit).
//   - StopBudget                        → success-with-note (stopped at the token budget).
//   - StopCancelled + clientCancelled   → success-with-note "[subagent cancelled by
//     user]": the partial work is usable and the trailer keeps the child resumable
//     (cancel-then-resume-with-a-narrower-prompt is the intended workflow); an error
//     result would teach the model the delegation mechanism failed. A parent-run
//     cancel (clientCancelled false) keeps today's un-noted success rendering.
//   - everything else (StopEndTurn / StopNoProgress / …) → success.
//
// On EVERY terminal the result text carries the structured payload (when a
// schema was satisfied) else the free-text summary, prefixed with the agentId trailer
// so the parent MODEL can discover the child id (mirroring renderTeamResult's Team-id
// line — the runtime-discoverability axis: the id must be where the model reads it, not
// only on the client-only subagent.* events).
func renderSubagentResult(callID session.ToolCallID, childID session.SessionID, final string, stop session.StopReason, submit *submitResultTool, clientCancelled bool) session.ToolResult {
	// Structured-output failure: the retry budget was exhausted without a schema-valid
	// payload. Surface the last validation error AS the tool error (model-visible),
	// never only a log line.
	if stop == session.StopStructuredOutput {
		msg := "subagent did not produce output matching the requested schema"
		if submit != nil {
			if last := submit.lastError(); last != "" {
				msg += ": " + last
			}
		}
		return session.NewToolError(callID, "Subagent: "+msg+"\n\nagentId: "+string(childID))
	}
	if stop == session.StopError {
		msg := final
		if msg == "" {
			msg = "subagent failed without producing a summary"
		}
		return session.NewToolError(callID, "Subagent: "+msg+"\n\nagentId: "+string(childID))
	}

	// Success family. A structured-output run returns the validated payload; otherwise
	// the free-text summary.
	body := final
	if submit != nil {
		if payload := submit.payload(); payload != "" {
			body = payload
		}
	}
	if strings.TrimSpace(body) == "" {
		body = "(subagent produced no summary)"
	}
	// Limit / budget notes: the child stopped at a bound rather than finishing. The
	// result is still a success (the partial work is usable), annotated so the model
	// knows the deliverable may be incomplete.
	switch stop {
	case session.StopMaxTurns:
		body = "[subagent stopped: reached its max-turns limit]\n\n" + body
	case session.StopMaxToolCalls:
		body = "[subagent stopped: reached its max-tool-calls limit]\n\n" + body
	case session.StopBudget:
		body = "[subagent stopped: reached its token budget]\n\n" + body
	case session.StopCancelled:
		// Only a CLIENT cancel (CancelChild) is noted; a parent-run cancel keeps the
		// legacy un-noted rendering (every child dies with the run anyway).
		if clientCancelled {
			body = "[subagent cancelled by user]\n\n" + body
		}
	}
	return session.NewToolResult(callID, renderSubagentTrailer(childID, body))
}

// renderSubagentTrailer prepends the model-visible agentId line to a Subagent result body,
// mirroring renderTeamResult's Team-id line. The childID is rendered VERBATIM (the
// deterministic t.childSessionID(callID)) so a human can correlate the overlay row and
// the parent can refer to "the subagent that did X" by id. It is the model's REAL
// handle: InspectSubagent loads the persisted child transcript by this id (verbatim —
// the id IS the session id, no derivation) and `resume` continues the child by it.
func renderSubagentTrailer(childID session.SessionID, body string) string {
	return fmt.Sprintf("agentId: %s\n\n%s", childID, body)
}

// driveChild runs the child loop and, when a structured-output schema is in play,
// applies the bounded SubmitResult validation-retry. It returns the terminal text,
// stop reason, cumulative usage, and observed tool-call count.
//
// FREE-TEXT path (submit == nil): exactly one RunContentWith drive — byte-identical to
// the prior engine.Run(...) behaviour.
//
// STRUCTURED path (submit != nil): drive the child; if it called SubmitResult with a
// VALID payload, the run is done (submit.payload() holds it). If the submitted payload
// was INVALID or SubmitResult was never called, re-inject a model-visible correction
// (Reopen + re-drive) up to defaultStructuredOutputRetries times, then give up with
// StopStructuredOutput. The retry is a SEPARATE bounded loop owned here (NOT a change
// to finishTurnNoTools — that hot shared path stays Subagent-agnostic, decision D2), and
// uses NO tool_choice forcing (incompatible with the reasoning paths).
func driveChild(ctx context.Context, engine *Engine, child *session.Session, runWS tool.Workspace, prompt string, runOpts RunOptions, emit func(session.Event), call session.ToolCall, childID session.SessionID, posture childPosture, submit *submitResultTool, schema json.RawMessage) (finalText string, stop session.StopReason, usage session.Usage, toolCount int) {
	drivePrompt := prompt
	// attempts = 1 (initial) + defaultStructuredOutputRetries corrections, but only the
	// structured path retries; the free-text path runs once.
	maxAttempts := 1
	if submit != nil {
		maxAttempts = 1 + defaultStructuredOutputRetries
	}
	for attempt := 0; attempt < maxAttempts; attempt++ {
		// A re-drive reuses the SAME child session: Reopen the completed session so
		// RecordUserPrompt accepts the correction prompt (the run drove it to a terminal
		// state). A non-recoverable session (failed/cancelled) ends the retry loop.
		// NOTE: Reopen() RESETS Counters, so the per-call MaxTurns/MaxToolCalls bound EACH
		// attempt independently (≤(1+defaultStructuredOutputRetries)× across the call —
		// bounded). The cross-attempt ceiling is the TOKEN budget: usage is summed below
		// and runOpts (carrying the tighten-only override) is re-passed to every drive.
		if attempt > 0 {
			if err := child.Reopen(); err != nil {
				return finalText, stop, usage, toolCount
			}
		}
		run := engine.RunContentWith(ctx, child, runWS, drivePrompt, nil, runOpts)
		text, st, u, tc := drainChildObserved(run, emit, string(call.ID), string(childID), posture)
		finalText, stop = text, st
		usage = usage.Add(u)
		toolCount += tc

		// Free-text path, or a structured run that produced a valid payload: done.
		if submit == nil || submit.valid() {
			return finalText, stop, usage, toolCount
		}
		// A child that crashed or was cancelled must not be re-driven — surface it.
		if stop == session.StopError || stop == session.StopCancelled || ctx.Err() != nil {
			return finalText, stop, usage, toolCount
		}
		// Structured miss: build the correction prompt for the next attempt (if any).
		drivePrompt = structuredCorrectionPrompt(schema, submit.lastError())
	}
	// Retry budget exhausted with no valid payload: a CLEAN terminal the Subagent result
	// renders as a model-visible validation-failure tool error (recoverable, not failed).
	return finalText, session.StopStructuredOutput, usage, toolCount
}

// tightenLimit applies a per-call TIGHTEN-ONLY override to an inherited session limit:
// it returns the LOWER of the inherited value and the requested override, ignoring a
// nil or non-positive request. Because a 0 inherited value means "unlimited", a present
// positive override always wins against 0; otherwise the override applies only when it
// is strictly lower than the inherited bound. The model can therefore make its child
// stricter than the operator's configured limit, never looser.
//
// It relies on session.Limits treating a 0 field as UNLIMITED (see session.Limits): a
// positive override against an unlimited (0) inherited bound TIGHTENS, which is why
// inherited<=0 returns the override rather than the (looser) 0. If session.Limits ever
// changes its zero-semantics, this branch must change with it.
func tightenLimit(inherited int, override *int) int {
	if override == nil || *override <= 0 {
		return inherited
	}
	if inherited <= 0 || *override < inherited {
		return *override
	}
	return inherited
}

// acquireChildSlot acquires one slot of the per-child concurrency gate, blocking
// until a slot is free or ctx is cancelled. It returns a release func to return the
// slot (idempotent-safe to call once), or nil if ctx was cancelled while waiting — the
// caller then aborts the call without spawning a child. A nil gate (no bound) returns
// an inert release immediately, though NewSubagentTool always sizes one so every Subagent child
// is bounded.
func (t *SubagentTool) acquireChildSlot(ctx context.Context) func() {
	if t.childGate == nil {
		return func() {}
	}
	select {
	case t.childGate <- struct{}{}:
		var once sync.Once
		return func() { once.Do(func() { <-t.childGate }) }
	case <-ctx.Done():
		return nil
	}
}

// tryAcquireChildID registers childID as in-flight, returning false if a run on the
// SAME id is already in progress (a fresh duplicate is impossible — call ids are unique
// — so this fires only for a concurrent RESUME of the same persisted id, or a fresh+resume
// collision). It is the correctness brake: two runs over one unlocked Session aggregate
// would race; the second call gets a model-visible "already running" error, not a wait.
func (t *SubagentTool) tryAcquireChildID(childID session.SessionID) bool {
	t.mu.Lock()
	defer t.mu.Unlock()
	if t.inFlight == nil {
		t.inFlight = make(map[session.SessionID]struct{})
	}
	if _, busy := t.inFlight[childID]; busy {
		return false
	}
	t.inFlight[childID] = struct{}{}
	return true
}

// releaseChildID unregisters a previously-acquired in-flight child id.
func (t *SubagentTool) releaseChildID(childID session.SessionID) {
	t.mu.Lock()
	defer t.mu.Unlock()
	delete(t.inFlight, childID)
}

// resolveResumeSession loads a persisted subagent session for a `resume` call, recovers
// its terminal state to StateIdle so it is runnable again, and tightens its preserved
// Limits by the per-call args. It runs BEFORE the workspace fork so the common error
// cases (unknown id, failed/non-resumable state, broken store) fail fast without paying
// a fork/unfork round-trip; the caller re-homes the returned session onto the fresh fork
// root afterwards (Session.Rehome — a field-consistency repair, not a prompt input). The
// terminal recovery is the loadAndReopen discipline applied at the agent layer:
// StateCompleted → Reopen, StateCancelled → Interrupt (history-repair), StateIdle → run
// as-is, StateFailed → not resumable, any other state → not in a resumable state. It
// returns the recovered session on success, or a model-addressable error ToolResult
// (ok=false) on a load failure or non-resumable state.
func (t *SubagentTool) resolveResumeSession(ctx context.Context, callID session.ToolCallID, id session.SessionID, args subagentArgs) (*session.Session, session.ToolResult, bool) {
	loaded, err := t.store.Load(ctx, id)
	switch {
	case errors.Is(err, port.ErrSessionNotFound) || (err == nil && loaded == nil):
		return nil, session.NewToolError(callID,
			fmt.Sprintf("Subagent: no subagent found for resume id %q; use the id exactly as shown on the 'agentId:' line of a previous Subagent result", id)), false
	case err != nil:
		return nil, session.NewToolError(callID,
			fmt.Sprintf("Subagent: failed to load subagent %q for resume: %v", id, err)), false
	}
	switch loaded.State {
	case session.StateCompleted:
		if rerr := loaded.Reopen(); rerr != nil {
			return nil, session.NewToolError(callID,
				fmt.Sprintf("Subagent: subagent %q is not in a resumable state (%q): %v", id, loaded.State, rerr)), false
		}
	case session.StateCancelled:
		if rerr := loaded.Interrupt(); rerr != nil {
			return nil, session.NewToolError(callID,
				fmt.Sprintf("Subagent: subagent %q is not in a resumable state (%q): %v", id, loaded.State, rerr)), false
		}
	case session.StateIdle:
		// Already runnable; run as-is.
	case session.StateFailed:
		return nil, session.NewToolError(callID,
			fmt.Sprintf("Subagent: subagent %q ended in a failed state and is not resumable; start a fresh subagent instead", id)), false
	default:
		return nil, session.NewToolError(callID,
			fmt.Sprintf("Subagent: subagent %q is not in a resumable state (%q)", id, loaded.State)), false
	}
	// Tighten the LOADED session's preserved Limits by the per-call args (tighten-only).
	// Reopen/Interrupt already reset Counters, so each per-call bound applies afresh.
	loaded.Limits.MaxTurns = tightenLimit(loaded.Limits.MaxTurns, args.MaxTurns)
	loaded.Limits.MaxToolCalls = tightenLimit(loaded.Limits.MaxToolCalls, args.MaxToolCalls)
	return loaded, session.ToolResult{}, true
}

// forkChildWorkspace selects the workspace one child run executes against. When a child
// forker is wired (the child catalog has Bash), the child gets its OWN isolated git
// worktree so its shell's writes never touch the shared parent base — what keeps
// Subagent read-parallel-safe (see ReadOnly). A fork FAILURE is a tool error (ok=false),
// NOT a silent fallback to the shared ws: the child has Bash precisely because isolation
// was available, so running it shared would be the exact hazard. Without a forker the
// child runs against the parent ws unchanged. The returned cleanup is ALWAYS non-nil
// (a no-op when nothing was forked) so the caller can defer it unconditionally.
func (t *SubagentTool) forkChildWorkspace(ctx context.Context, callID session.ToolCallID, ws tool.Workspace, label string) (runWS tool.Workspace, cleanup func() error, errResult session.ToolResult, ok bool) {
	if t.childForker == nil {
		return ws, func() error { return nil }, session.ToolResult{}, true
	}
	forkWS, forkCleanup, err := t.childForker.Fork(ctx, ws, label)
	if err != nil {
		return nil, nil, session.NewToolError(callID, "Subagent: workspace isolation failed: "+err.Error()), false
	}
	if forkCleanup == nil {
		forkCleanup = func() error { return nil }
	}
	return forkWS, forkCleanup, session.ToolResult{}, true
}

// buildChildSession produces the session one child run drives. On a FRESH call
// (resumedChild == nil): a new session — own conversation, own (tighter) Limits, scoped
// to the run workspace root (the isolated worktree when forked, else the parent base).
// On RESUME: the session was already reloaded + recovered (before the fork, in
// resolveResumeSession); here it is RE-HOMED onto the run root so its recorded workspace
// stays consistent with where this run actually executes — the original worktree is
// torn down, and without the re-home the re-persisted snapshot would record a dead
// path. (The child's prompt cwd is independently sourced from the engine's PromptConfig
// and is NOT affected by this field.)
func (t *SubagentTool) buildChildSession(callID session.ToolCallID, childID session.SessionID, resumedChild *session.Session, root string, limits session.Limits) (*session.Session, session.ToolResult, bool) {
	if resumedChild == nil {
		// When a named agent def pins limits, the child runs under THOSE; otherwise it uses
		// the Subagent tool's default limits.
		return session.New(childID, t.childMode, root, limits, time.Now()), session.ToolResult{}, true
	}
	if err := resumedChild.Rehome(root); err != nil {
		return nil, session.NewToolError(callID,
			fmt.Sprintf("Subagent: failed to re-home resumed subagent %q: %v", childID, err)), false
	}
	return resumedChild, session.ToolResult{}, true
}

// subagentGoal derives the short, plain-text goal label forwarded on
// EvSubagentStart: the explicit description when supplied, else the (model-
// authored) prompt. It is metadata for the card title; it is NOT child content
// (the prompt is the parent's own instruction to the child). Both paths are
// clamped identically so the goal always stays a single, bounded line — an
// explicit description is just as capable of being long or multi-line as a prompt.
func subagentGoal(args subagentArgs) string {
	if g := strings.TrimSpace(args.Description); g != "" {
		return truncateGoal(g)
	}
	return truncateGoal(strings.TrimSpace(args.Prompt))
}

// truncateGoal normalizes a goal label into a single bounded line: it collapses
// any newlines (and tabs) to spaces so a multi-line value can't break the one-line
// Subagent-card title, then clamps to maxSubagentGoalLen runes, appending an ellipsis
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
func drainChildObserved(run *Run, emit func(session.Event), parentCallID, childID string, posture childPosture) (finalText string, stop session.StopReason, usage session.Usage, toolCount int) {
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
		if text, st, ok := handleChildEvent(run, ev, posture); ok {
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
// the observed Subagent path uses drainChildObserved.
func drainChild(run *Run, posture childPosture) (finalText string, stop session.StopReason) {
	final, st, _, _ := drainChildObserved(run, nil, "", "", posture)
	return final, st
}

// childPosture carries the per-child permission resolution context applied to a child/
// member run's permission asks (the 4-step model). It is threaded by every drain path
// (drainChildObserved, drainChild, the team supervisor's driveOneTurn) so the resolution
// order — isolation auto-approve → surface-to-human → headless auto-deny — is identical
// everywhere and cannot drift.
//
// The zero value is the legacy posture: not isolated, not interactive, no surface →
// every ask auto-denies (with the accurate message). A child run that is isolated sets
// isolated=true; an interactive parent supplies caps with a non-nil surfaceAsk.
type childPosture struct {
	// isolated reports that the child runs in an ISOLATED workspace (a git worktree or a
	// force-copy fork) — so an IsolationApprovable Bash ask (read-only ∪ worktree-safe
	// go verbs) auto-APPROVES (A2). false for a base-sharing child (no auto-approve).
	isolated bool
	// caps carries the parent's interactivity + surface back-channel (zero value =
	// headless: no surface). When caps.interactive && caps.surfaceAsk != nil, an ask that
	// steps 1-2 did not resolve is SURFACED to the human; otherwise it auto-denies.
	caps parentCaps
	// childID is the child SESSION id (the registry/cancel handle: "subagent-<callID>",
	// "team-<teamID>-<member>", "parallel-<callID>-<i>") — threaded EXPLICITLY because
	// role does NOT universally carry it (a team posture's role is the member NAME, a
	// parallel posture's the branch label). surfaceAsk passes it to recordAsk so a
	// surfaced ask's ownership is recorded uniformly across all three families.
	childID string
	// role is the child's identity for the headless auto-deny operator diagnostic: the
	// child session id ("subagent-<callID>") for Subagent children, the member name for
	// team members, the branch/judge label for forks. Empty falls back to a generic label.
	role string
	// askLabel is the pre-composed human-facing requester phrase passed to surfaceAsk so
	// a surfaced ask is ATTRIBUTED to its delegation (e.g. `subagent "fix flaky tests"`,
	// `team member "researcher"`, `parallel branch "branch-2"`). It is plain metadata
	// (no raw args; the goal/name/label is already collapsed + clamped at the call site),
	// re-clamped by the parent before emission. Empty for headless postures that never
	// surface (fork judge, user-model review) — they keep the legacy framing.
	askLabel string
}

// childAutoDenyMessage is the ACCURATE message a headless (non-interactive) subagent's
// auto-denied ask carries — NOT the misleading "denied by user … client approval
// required" of the interactive path. It names the real cause (a non-interactive subagent
// shell) and what the model can do about it. reason is the policy's ask reason.
func childAutoDenyMessage(reason string) string {
	return "not permitted in a non-interactive subagent shell: " + reason +
		"; rephrase to avoid command substitution/subshell grouping, or use an auto-approved tool (read-only commands, or go test/build/vet/list)"
}

// handleChildEvent applies the per-child permission contract to a single event of a
// child/member run and, when the event is the terminal result, reports its text and
// stop reason via isResult=true. It is the SINGLE definition of that contract, shared
// by drainChildObserved (Subagent), drainChild (Fork, silent), and the team supervisor's
// driveOneTurn, so the resolution order cannot drift.
//
// Resolution order on a permission ask (the 4-step model):
//  1. ISOLATION auto-approve (A2): an isolated child whose ask is IsolationApprovable
//     (read-only ∪ worktree-safe go verbs, no worktree-escape verb) → AllowOnce. (A1's
//     read-only substitution carve-out already turns most read-only substitutions into
//     Allow upstream so they never reach here as an ask; this catches the worktree-safe
//     `go test` superset.)
//  2. SURFACE to human: an interactive parent with a surface seam registers the child in
//     the parent router and emits a REDACTED parent EvPermissionAsk, then RETURNS without
//     resolving — the child's authorize stays parked in await until the parent routes a
//     verdict back via the router (child.Approve). The drain loop blocks on this child's
//     channel until then (single-child) or keeps consuming peers (concurrent members).
//  3. HEADLESS auto-deny: no surface (headless / no router) → Deny with the ACCURATE
//     message + a correlated operator diagnostic (LevelInfo, agent=<child-session-id>
//     for Subagent children, e.g. "subagent-<callID>"; member name / fork label for the
//     others) — never the misleading "denied by user".
func handleChildEvent(run *Run, ev session.Event, posture childPosture) (text string, stop session.StopReason, isResult bool) {
	if ev.Type == session.EvPermissionAsk && ev.Ask != nil {
		resolveChildAsk(run, *ev.Ask, posture)
	}
	if ev.Type == session.EvResult && ev.Result != nil {
		return ev.Result.Text, ev.Result.Stop, true
	}
	return "", session.StopNone, false
}

// resolveChildAsk applies the 4-step resolution to one child permission ask.
func resolveChildAsk(run *Run, ask session.PendingAsk, posture childPosture) {
	// Step 1-2 (A2): isolated child + isolation-approvable Bash → auto-approve.
	if posture.isolated && ask.Tool == "Bash" && governance.IsolationApprovable(bashCmdFromArgs(ask.Args)) {
		run.Approve(ask.AskID, session.VerdictAllowOnce)
		return
	}
	// Step 3: surface to the human when the parent is interactive and a surface seam is
	// wired. Register-then-emit lives inside surfaceAsk; we DO NOT resolve here — the
	// child stays parked until the parent routes a verdict back.
	if posture.caps.interactive && posture.caps.surfaceAsk != nil {
		posture.caps.surfaceAsk(ask.AskID, posture.childID, run, ask, posture.askLabel)
		return
	}
	// Step 4: headless / no surface → auto-deny with the accurate message + an operator
	// diagnostic, then resolve the child's own ask. The child's authorize maps a Deny
	// verdict to a denied result carrying decision.Reason (the policy's), so we ALSO emit
	// the operator-visible diagnostic here (the deny otherwise reaches only the child's
	// errored tool-result, which default clients bury). The denied RESULT message the
	// model sees is rebuilt by authorize; the accurate phrasing is surfaced via the
	// diagnostic and the bash tool description.
	if posture.caps.diag != nil {
		posture.caps.diag.Log(context.Background(), port.LevelInfo,
			"subagent permission ask auto-denied (non-interactive shell)",
			"agent", posture.role, "tool", ask.Tool, "reason", ask.Reason)
	}
	run.autoDenyChildAsk(ask.AskID, childAutoDenyMessage(ask.Reason))
}

// bashCmdFromArgs extracts the Bash command string from a pending ask's raw args,
// reusing the same field tolerance the governance evaluator uses. Empty on a parse
// failure (then IsolationApprovable("") is false — fail safe).
func bashCmdFromArgs(args json.RawMessage) string {
	if len(args) == 0 {
		return ""
	}
	var m map[string]json.RawMessage
	if err := json.Unmarshal(args, &m); err != nil {
		return ""
	}
	for _, key := range []string{"command", "cmd"} {
		if raw, ok := m[key]; ok {
			var s string
			if json.Unmarshal(raw, &s) == nil && s != "" {
				return s
			}
		}
	}
	return ""
}

// fireSubagentStop runs the SubagentStop lifecycle hook for a finished child run.
// It is best-effort: a hook error or a block outcome is ignored (a subagent's
// completion cannot be vetoed after the fact).
func (t *SubagentTool) fireSubagentStop(ctx context.Context, child *session.Session) {
	fireNotify(ctx, t.hooks, governance.HookEvent{
		Phase:     governance.PhaseSubagentStop,
		SessionID: string(child.ID),
	})
}

// persistChild best-effort saves the child session to the injected store so the
// InspectSubagent tool can later load its transcript by the agentId trailer. A nil
// store disables persistence; a save failure is advisory and swallowed (persistMember
// discipline).
func (t *SubagentTool) persistChild(ctx context.Context, child *session.Session) {
	if t.store == nil {
		return
	}
	_ = t.store.Save(ctx, child)
}

// unknownAgentHint builds the model-addressable error text for a Subagent call that
// names an agent that is not registered. It lists the valid names so the model can
// retry, mirroring the Skill tool's available-names hint.
func (t *SubagentTool) unknownAgentHint(name string) string {
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
func (t *SubagentTool) childSessionID(callID session.ToolCallID) session.SessionID {
	return session.SessionID(fmt.Sprintf("%s-%s", t.idPrefix, callID))
}

// Compile-time assertion that SubagentTool satisfies the Tool contract and the
// agent-internal observableTool seam.
var (
	_ tool.Tool      = (*SubagentTool)(nil)
	_ observableTool = (*SubagentTool)(nil)
)
