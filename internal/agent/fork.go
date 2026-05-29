package agent

import (
	"context"
	"encoding/json"
	"fmt"
	"sort"
	"strings"
	"sync"
	"time"

	"github.com/stacklok/ozzharness/internal/governance"
	"github.com/stacklok/ozzharness/internal/port"
	"github.com/stacklok/ozzharness/internal/session"
	"github.com/stacklok/ozzharness/internal/tool"
)

// forkToolName is the catalog name of the fork-join fan-out tool.
const forkToolName = "Fork"

// defaultMaxBranches caps the fan-out of a single Fork call. A model that asks
// for an absurd number of branches is rejected rather than allowed to spawn an
// unbounded number of child loops (and forked workspaces). Override with
// WithMaxBranches.
const defaultMaxBranches = 8

// defaultForkConcurrency bounds how many child branches run at once. Forking and
// running N child loops simultaneously is the point of fork-join, but it is also
// N times the resource cost, so a worker limit keeps it bounded. Override with
// WithForkConcurrency.
const defaultForkConcurrency = 4

// forkArgs is the argument payload the model supplies when calling the Fork tool.
type forkArgs struct {
	// Tasks is the list of self-contained branch prompts. Each runs in its OWN
	// isolated forked workspace and its OWN fresh child context. Because every
	// child has a fresh context window and cannot see this conversation, each task
	// must be self-contained.
	Tasks []string `json:"tasks"`
	// Shared is an optional instruction prepended to every task prompt (e.g. common
	// context all branches need). It is convenience only; it could equally be
	// repeated into each task.
	Shared string `json:"shared,omitempty"`
}

// forkSchema is the JSON schema the model sees for the Fork tool's arguments.
var forkSchema = json.RawMessage(`{
  "type": "object",
  "properties": {
    "tasks": {
      "type": "array",
      "items": {"type": "string"},
      "minItems": 1,
      "description": "Self-contained branch prompts. Each runs in PARALLEL in its own isolated forked workspace and its own fresh context window; include everything each branch needs."
    },
    "shared": {
      "type": "string",
      "description": "Optional shared instruction prepended to every branch's prompt."
    }
  },
  "required": ["tasks"]
}`)

// ForkTool is the fork-join fan-out tool (harness pattern 8). When executed it
// forks N ISOLATED child workspaces from the parent's workspace (via the injected
// tool.WorkspaceForker), runs one CHILD agent loop per branch in PARALLEL (bounded
// by a worker limit) over the injected child *Engine — each with its own fresh
// Session, tighter Limits, and the child Engine's scoped catalog — drains every
// child's Event stream internally, and JOINS the results into a SINGLE
// session.ToolResult that summarizes all branches.
//
// Like TaskTool (gauntlet #7), the parent NEVER observes any child's intermediate
// tool.call / tool.result / message.delta / permission.ask events: each child's
// stream is drained entirely inside Execute and only the terminal summary folds
// back. A child permission ask is auto-denied so children stay non-interactive.
//
// Isolation: each branch runs in its OWN forked workspace, so even a child wired
// with mutating tools writes only to its fork — parallel writes are SAFE because
// they are isolated (this is strictly safer than concurrent TaskTool calls, which
// share the base). v1 does NOT auto-merge: Execute returns the per-branch
// summaries and the child workspace ROOT paths so a human or the parent can
// inspect/merge the forked trees. cleanup tears each fork down after its summary
// has been captured.
//
// The child Engine is built by the composition root with a scoped catalog (no
// Fork, no Task — children cannot fan out further) exactly as for TaskTool.
type ForkTool struct {
	// childEngine runs each branch's child loop. It is pre-wired by the composition
	// root with a scoped catalog and a non-interactive policy. It is never the
	// parent Engine.
	childEngine *Engine

	// forker isolates each branch's workspace from the shared base.
	forker tool.WorkspaceForker

	// limits bound a single child branch run. Defaults to defaultChildLimits.
	limits session.Limits

	// childMode is the permission mode each child session runs under. Defaults to
	// session.ModeDefault.
	childMode session.PermissionMode

	// maxBranches caps the fan-out per Fork call.
	maxBranches int

	// concurrency bounds how many branches run at once.
	concurrency int

	// hooks fires SubagentStop per finished branch (best-effort; nil disables it).
	hooks port.HookRunner

	// idPrefix seeds generated child SessionIDs.
	idPrefix string
}

// ForkOption configures a ForkTool.
type ForkOption func(*ForkTool)

// WithForkChildLimits overrides the per-branch stop conditions (default
// defaultChildLimits).
func WithForkChildLimits(l session.Limits) ForkOption {
	return func(t *ForkTool) { t.limits = l }
}

// WithForkChildMode sets the permission mode each child branch session runs under
// (default session.ModeDefault).
func WithForkChildMode(m session.PermissionMode) ForkOption {
	return func(t *ForkTool) { t.childMode = m }
}

// WithMaxBranches caps the number of branches a single Fork call may fan out to
// (default defaultMaxBranches). A non-positive value is ignored.
func WithMaxBranches(n int) ForkOption {
	return func(t *ForkTool) {
		if n > 0 {
			t.maxBranches = n
		}
	}
}

// WithForkConcurrency bounds how many branches run simultaneously (default
// defaultForkConcurrency). A non-positive value is ignored.
func WithForkConcurrency(n int) ForkOption {
	return func(t *ForkTool) {
		if n > 0 {
			t.concurrency = n
		}
	}
}

// WithForkSubagentStopHook injects the HookRunner that fires SubagentStop when a
// branch run finishes (best-effort; nil disables it).
func WithForkSubagentStopHook(h port.HookRunner) ForkOption {
	return func(t *ForkTool) { t.hooks = h }
}

// WithForkChildSessionPrefix sets the prefix used to derive child branch
// SessionIDs (default "fork"). Child ids are of the form "<prefix>-<callID>-<i>".
func WithForkChildSessionPrefix(p string) ForkOption {
	return func(t *ForkTool) { t.idPrefix = p }
}

// NewForkTool constructs the Fork fan-out tool over a pre-built child *Engine and
// a WorkspaceForker. The composition root builds childEngine with the SCOPED
// child catalog and a non-interactive policy (see NewTaskTool's guidance); the
// child catalog MUST NOT contain Fork or Task (so a branch cannot fan out
// further). childEngine and forker must be non-nil; NewForkTool panics otherwise,
// because a Fork tool with no child loop or no isolation seam is a composition-root
// programming error.
func NewForkTool(childEngine *Engine, forker tool.WorkspaceForker, opts ...ForkOption) tool.Tool {
	if childEngine == nil {
		panic("agent: NewForkTool requires a non-nil child Engine")
	}
	if forker == nil {
		panic("agent: NewForkTool requires a non-nil WorkspaceForker")
	}
	t := &ForkTool{
		childEngine: childEngine,
		forker:      forker,
		limits:      defaultChildLimits,
		childMode:   session.ModeDefault,
		maxBranches: defaultMaxBranches,
		concurrency: defaultForkConcurrency,
		idPrefix:    "fork",
	}
	for _, o := range opts {
		o(t)
	}
	return t
}

// Spec returns the model-facing specification for the Fork tool.
func (*ForkTool) Spec() tool.ToolSpec {
	return tool.ToolSpec{
		Name: forkToolName,
		Description: "Fan out several independent tasks to run in PARALLEL, each in its own " +
			"isolated forked workspace and fresh context, then join their results into one " +
			"summary. Use to explore multiple approaches at once or to split independent work. " +
			"Each branch cannot see this conversation or the other branches, so make every " +
			"task in `tasks` self-contained (use `shared` for common context). Returns one " +
			"combined summary delimited per branch; branches do NOT auto-merge — their forked " +
			"workspace paths are reported so you can inspect or merge them yourself.",
		Schema: forkSchema,
	}
}

// ReadOnly reports that the Fork tool is read-only with respect to the PARENT's
// shared workspace, which lets the parent dispatcher run it concurrently with
// other read-only tools (read-parallel / mutate-serial; see dispatch.go).
//
// INVARIANT — this is the same invariant TaskTool documents, but Fork makes it
// strictly safer: every child branch runs in its OWN forked workspace, never the
// shared base. So even if the child catalog includes mutating tools (Edit / Write
// / non-RO Bash), those writes land in the isolated fork and CANNOT race on, or
// mutate, the parent's shared base. The parent base is therefore untouched by a
// Fork call regardless of the child catalog, which is why ReadOnly() can safely
// return true even for mutating children — unlike TaskTool, where a mutating
// child shares the base and would force ReadOnly() to false.
func (*ForkTool) ReadOnly() bool { return true }

// branchResult is the joined outcome of one branch.
type branchResult struct {
	index      int
	label      string
	childRoot  string
	summary    string
	failed     bool
	failReason string
}

// Execute forks N isolated child workspaces (one per task), runs a child loop in
// each IN PARALLEL bounded by the worker limit, drains every child stream
// internally, cleans up each fork, and returns ONE ToolResult that joins all
// branch summaries. The whole operation is bounded by the parent ctx: cancelling
// it cancels every in-flight branch. A branch that fails is reported in the joined
// summary without aborting the others; the call returns a harness-level error only
// for a setup failure (invalid args / cap exceeded).
func (t *ForkTool) Execute(ctx context.Context, call session.ToolCall, ws tool.Workspace) (session.ToolResult, error) {
	var args forkArgs
	if len(call.Args) > 0 {
		if err := json.Unmarshal(call.Args, &args); err != nil {
			return session.NewToolError(call.ID, fmt.Sprintf("Fork: invalid arguments: %v", err)), nil
		}
	}
	tasks := nonEmptyTasks(args.Tasks)
	if len(tasks) == 0 {
		return session.NewToolError(call.ID, "Fork: 'tasks' is required and must contain at least one non-empty prompt"), nil
	}
	if len(tasks) > t.maxBranches {
		return session.NewToolError(call.ID, fmt.Sprintf(
			"Fork: %d tasks exceeds the maximum fan-out of %d; split the work or batch it",
			len(tasks), t.maxBranches)), nil
	}

	results := t.runBranches(ctx, call.ID, tasks, args.Shared, ws)
	return session.NewToolResult(call.ID, joinBranches(results)), nil
}

// runBranches forks and runs every branch in parallel under a worker-limited
// semaphore, returning the per-branch results in branch order.
func (t *ForkTool) runBranches(ctx context.Context, callID session.ToolCallID, tasks []string, shared string, ws tool.Workspace) []branchResult {
	results := make([]branchResult, len(tasks))
	sem := make(chan struct{}, t.concurrency)
	var wg sync.WaitGroup

	for i, task := range tasks {
		wg.Add(1)
		go func(i int, task string) {
			defer wg.Done()
			select {
			case sem <- struct{}{}:
				defer func() { <-sem }()
			case <-ctx.Done():
				results[i] = branchResult{index: i, label: branchLabel(i), failed: true, failReason: "cancelled before start"}
				return
			}
			results[i] = t.runBranch(ctx, callID, i, task, shared, ws)
		}(i, task)
	}
	wg.Wait()
	return results
}

// runBranch forks an isolated workspace, runs one child loop in it, drains the
// child stream internally, fires SubagentStop, cleans up the fork, and returns the
// branch's joined result. A fork or child failure is captured in the result, never
// propagated as a harness error (one failing branch must not kill the others).
func (t *ForkTool) runBranch(ctx context.Context, callID session.ToolCallID, i int, task, shared string, ws tool.Workspace) branchResult {
	label := branchLabel(i)
	res := branchResult{index: i, label: label}

	child, cleanup, err := t.forker.Fork(ctx, ws, label)
	if err != nil {
		res.failed = true
		res.failReason = fmt.Sprintf("fork failed: %v", err)
		return res
	}
	defer func() {
		if cleanup != nil {
			_ = cleanup()
		}
	}()
	res.childRoot = child.Root()

	childSess := session.New(
		t.childSessionID(callID, i),
		t.childMode,
		child.Root(),
		t.limits,
		time.Now(),
	)

	run := t.childEngine.Run(ctx, childSess, child, composePrompt(shared, task))
	final, stop := drainChild(run)
	t.fireSubagentStop(ctx, childSess)

	switch stop {
	case session.StopError:
		res.failed = true
		if final == "" {
			final = "branch failed without producing a summary"
		}
		res.failReason = final
	case session.StopCancelled:
		res.failed = true
		res.failReason = "cancelled"
		res.summary = final
	default:
		if final == "" {
			final = "(branch produced no summary)"
		}
		res.summary = final
	}
	return res
}

// fireSubagentStop runs the SubagentStop hook for a finished branch run
// (best-effort; mirrors TaskTool.fireSubagentStop).
func (t *ForkTool) fireSubagentStop(ctx context.Context, child *session.Session) {
	if t.hooks == nil {
		return
	}
	hookCtx := ctx
	if ctx.Err() != nil {
		var cancel context.CancelFunc
		hookCtx, cancel = context.WithTimeout(context.WithoutCancel(ctx), 5*time.Second)
		defer cancel()
	}
	ev := governance.HookEvent{
		Phase:     governance.PhaseSubagentStop,
		SessionID: string(child.ID),
	}
	_, _ = t.hooks.Run(hookCtx, ev)
}

// childSessionID derives a stable, unique id for a branch's child session.
func (t *ForkTool) childSessionID(callID session.ToolCallID, i int) session.SessionID {
	return session.SessionID(fmt.Sprintf("%s-%s-%d", t.idPrefix, callID, i))
}

// nonEmptyTasks trims and drops blank task prompts, preserving order.
func nonEmptyTasks(tasks []string) []string {
	out := make([]string, 0, len(tasks))
	for _, task := range tasks {
		if strings.TrimSpace(task) != "" {
			out = append(out, task)
		}
	}
	return out
}

// composePrompt prepends an optional shared instruction to a branch task prompt.
func composePrompt(shared, task string) string {
	if strings.TrimSpace(shared) == "" {
		return task
	}
	return shared + "\n\n" + task
}

// branchLabel is the stable, human-meaningful label for branch i (1-based).
func branchLabel(i int) string {
	return fmt.Sprintf("branch-%d", i+1)
}

// joinBranches renders the per-branch results into a single, clearly-delimited
// summary string. Branches are sorted by index so the joined output is
// deterministic regardless of completion order. Each branch reports its status,
// its isolated workspace path (the no-auto-merge artifact), and its summary.
func joinBranches(results []branchResult) string {
	sorted := make([]branchResult, len(results))
	copy(sorted, results)
	sort.Slice(sorted, func(a, b int) bool { return sorted[a].index < sorted[b].index })

	ok := 0
	for _, r := range sorted {
		if !r.failed {
			ok++
		}
	}

	var b strings.Builder
	fmt.Fprintf(&b, "Fork joined %d branch(es): %d succeeded, %d failed.\n",
		len(sorted), ok, len(sorted)-ok)
	for _, r := range sorted {
		b.WriteString("\n=== ")
		b.WriteString(r.label)
		if r.failed {
			b.WriteString(" [FAILED] ===\n")
			b.WriteString(r.failReason)
			b.WriteString("\n")
		} else {
			b.WriteString(" [OK] ===\n")
		}
		if r.childRoot != "" {
			fmt.Fprintf(&b, "workspace: %s\n", r.childRoot)
		}
		if r.summary != "" {
			b.WriteString(r.summary)
			b.WriteString("\n")
		}
	}
	return b.String()
}

// Compile-time assertion that ForkTool satisfies the Tool contract.
var _ tool.Tool = (*ForkTool)(nil)
