package agent

import (
	"context"
	"encoding/json"
	"fmt"
	"strings"

	"github.com/stacklok/mecatl/internal/governance"
	"github.com/stacklok/mecatl/internal/port"
	"github.com/stacklok/mecatl/internal/session"
	"github.com/stacklok/mecatl/internal/team"
	"github.com/stacklok/mecatl/internal/tool"
)

// teamtools.go implements the LLM-facing coordination tools a team member (the
// lead or a teammate) uses to self-organise: send a message to a peer, add a task
// to the shared list, claim and complete tasks, and inspect the list. Each tool is
// bound at construction to (1) the shared *team.Team aggregate and (2) the calling
// member's name ("self"), because tool.Tool.Execute carries no caller identity —
// the supervisor stamps identity in when it builds each member's catalog. The team
// aggregate is internally synchronised, so these tools are safe to share across the
// concurrently-running member sessions.
//
// The mutating tools report ReadOnly() == false so the dispatcher serialises them
// (mutate-serial); they never touch the workspace, so this is about honouring the
// tool contract, not workspace-race safety. ListTasks is read-only.

// MemberTools returns the coordination tools bound to the shared team t and the
// member name self. The composition root registers these into every member's
// catalog (including the lead's) so team coordination is always available to a
// member even when an agent definition otherwise restricts its tools. hooks, when
// non-nil, fires the TaskCreated / TaskCompleted lifecycle gates from AddTask /
// CompleteTask (a Block outcome vetoes the action); pass nil to disable them.
func MemberTools(t *team.Team, self string, hooks port.HookRunner) []tool.Tool {
	return []tool.Tool{
		sendMessageTool{team: t, self: self},
		addTaskTool{team: t, self: self, hooks: hooks},
		claimTaskTool{team: t, self: self},
		completeTaskTool{team: t, self: self, hooks: hooks},
		listTasksTool{team: t, self: self},
	}
}

// MemberToolNames returns the set of coordination-tool names MemberTools installs
// into every member's catalog. These tools report ReadOnly() == false because they
// mutate TEAM state, but they NEVER touch the workspace, so they are safe for a
// read-only (base-sharing) member. The supervisor uses this set to distinguish a
// member's coordination tools from genuine WORKSPACE-mutating tools (Edit / Write /
// non-read-only Bash) when it enforces the read-only-member invariant in AddMember.
// It is kept in lock-step with MemberTools by construction: it derives the names
// from MemberTools over a throwaway team.
func MemberToolNames() map[string]struct{} {
	tools := MemberTools(team.New(""), "", nil)
	set := make(map[string]struct{}, len(tools))
	for _, t := range tools {
		set[t.Spec().Name] = struct{}{}
	}
	return set
}

// fireTeamGate fires a best-effort team lifecycle gate hook and reports whether the
// action is vetoed (Block) along with the veto message. A nil runner or a hook
// error never vetoes (fail-open: a broken hook must not wedge coordination).
func fireTeamGate(ctx context.Context, hooks port.HookRunner, phase governance.HookPhase, toolName, self string, payload any) (blocked bool, msg string) {
	if hooks == nil {
		return false, ""
	}
	input, _ := json.Marshal(payload)
	out, err := hooks.Run(ctx, governance.HookEvent{
		Phase:     phase,
		Tool:      toolName,
		Input:     input,
		SessionID: self,
	})
	if err != nil || !out.Block {
		return false, ""
	}
	if out.Message == "" {
		return true, "vetoed by " + string(phase) + " hook"
	}
	return true, out.Message
}

// --- SendMessage ----------------------------------------------------------

type sendMessageTool struct {
	team *team.Team
	self string
}

type sendMessageArgs struct {
	To   string `json:"to"`
	Body string `json:"body"`
}

func (sendMessageTool) Spec() tool.ToolSpec {
	return tool.ToolSpec{
		Name: "SendMessage",
		Description: "Send a direct message to another team member by name. The message is " +
			"delivered to that member's inbox and read at the start of its next turn. Use it to " +
			"share a finding, ask a peer to do something, challenge a hypothesis, or report back " +
			"to the lead. The recipient must be a current team member.",
		Schema: json.RawMessage(`{
  "type": "object",
  "properties": {
    "to": {"type": "string", "description": "The recipient member's name."},
    "body": {"type": "string", "description": "The message text."}
  },
  "required": ["to", "body"]
}`),
	}
}

func (sendMessageTool) ReadOnly() bool { return false }

func (t sendMessageTool) Execute(_ context.Context, call session.ToolCall, _ tool.Workspace) (session.ToolResult, error) {
	var args sendMessageArgs
	if msg, ok := parseTeamArgs(call, &args); !ok {
		return session.NewToolError(call.ID, msg), nil
	}
	if strings.TrimSpace(args.To) == "" {
		return session.NewToolError(call.ID, "SendMessage: 'to' is required"), nil
	}
	if strings.TrimSpace(args.Body) == "" {
		return session.NewToolError(call.ID, "SendMessage: 'body' is required"), nil
	}
	if err := t.team.Send(t.self, args.To, args.Body); err != nil {
		return session.NewToolError(call.ID, fmt.Sprintf("SendMessage: %v", err)), nil
	}
	return session.NewToolResult(call.ID, fmt.Sprintf("Message sent to %q.", args.To)), nil
}

// --- AddTask --------------------------------------------------------------

type addTaskTool struct {
	team  *team.Team
	self  string
	hooks port.HookRunner
}

type addTaskArgs struct {
	Description string   `json:"description"`
	Deps        []string `json:"deps,omitempty"`
}

func (addTaskTool) Spec() tool.ToolSpec {
	return tool.ToolSpec{
		Name: "AddTask",
		Description: "Add a task to the shared team task list. Optionally declare dependencies on " +
			"other task ids that must complete first — a task with unmet dependencies cannot be " +
			"claimed until they finish. Returns the new task id. Typically the lead breaks work " +
			"into tasks this way; any member may add follow-up tasks it discovers.",
		Schema: json.RawMessage(`{
  "type": "object",
  "properties": {
    "description": {"type": "string", "description": "What the task is — the instruction its claimant will work on."},
    "deps": {"type": "array", "items": {"type": "string"}, "description": "Task ids that must complete before this task is claimable."}
  },
  "required": ["description"]
}`),
	}
}

func (addTaskTool) ReadOnly() bool { return false }

func (t addTaskTool) Execute(ctx context.Context, call session.ToolCall, _ tool.Workspace) (session.ToolResult, error) {
	var args addTaskArgs
	if msg, ok := parseTeamArgs(call, &args); !ok {
		return session.NewToolError(call.ID, msg), nil
	}
	if strings.TrimSpace(args.Description) == "" {
		return session.NewToolError(call.ID, "AddTask: 'description' is required"), nil
	}
	if blocked, msg := fireTeamGate(ctx, t.hooks, governance.PhaseTaskCreated, "AddTask", t.self,
		map[string]any{"description": args.Description, "deps": args.Deps, "by": t.self}); blocked {
		return session.NewToolError(call.ID, "AddTask: "+msg), nil
	}
	deps := make([]team.TaskID, 0, len(args.Deps))
	for _, d := range args.Deps {
		deps = append(deps, team.TaskID(d))
	}
	id, err := t.team.CreateTask(args.Description, deps...)
	if err != nil {
		return session.NewToolError(call.ID, fmt.Sprintf("AddTask: %v", err)), nil
	}
	return session.NewToolResult(call.ID, fmt.Sprintf("Created task %q.", id)), nil
}

// --- ClaimTask ------------------------------------------------------------

type claimTaskTool struct {
	team *team.Team
	self string
}

type claimTaskArgs struct {
	TaskID string `json:"task_id,omitempty"`
}

func (claimTaskTool) Spec() tool.ToolSpec {
	return tool.ToolSpec{
		Name: "ClaimTask",
		Description: "Claim a task to work on, marking it in-progress and assigned to you. With a " +
			"'task_id', claims that specific task (fails if it is already claimed or blocked by an " +
			"unfinished dependency). With no 'task_id', claims the next available unclaimed, " +
			"unblocked task. Returns the claimed task's id and description, or reports that nothing " +
			"is claimable.",
		Schema: json.RawMessage(`{
  "type": "object",
  "properties": {
    "task_id": {"type": "string", "description": "Optional specific task id to claim; omit to claim the next available task."}
  }
}`),
	}
}

func (claimTaskTool) ReadOnly() bool { return false }

func (t claimTaskTool) Execute(_ context.Context, call session.ToolCall, _ tool.Workspace) (session.ToolResult, error) {
	var args claimTaskArgs
	if msg, ok := parseTeamArgs(call, &args); !ok {
		return session.NewToolError(call.ID, msg), nil
	}
	if id := strings.TrimSpace(args.TaskID); id != "" {
		if err := t.team.ClaimTask(team.TaskID(id), t.self); err != nil {
			return session.NewToolError(call.ID, fmt.Sprintf("ClaimTask: %v", err)), nil
		}
		return session.NewToolResult(call.ID, fmt.Sprintf("Claimed task %q.", id)), nil
	}
	task, ok, err := t.team.ClaimNext(t.self)
	if err != nil {
		return session.NewToolError(call.ID, fmt.Sprintf("ClaimTask: %v", err)), nil
	}
	if !ok {
		return session.NewToolResult(call.ID, "No claimable task is available right now."), nil
	}
	return session.NewToolResult(call.ID, fmt.Sprintf("Claimed task %q: %s", task.ID, task.Description)), nil
}

// --- CompleteTask ---------------------------------------------------------

type completeTaskTool struct {
	team  *team.Team
	self  string
	hooks port.HookRunner
}

type completeTaskArgs struct {
	TaskID string `json:"task_id"`
}

func (completeTaskTool) Spec() tool.ToolSpec {
	return tool.ToolSpec{
		Name: "CompleteTask",
		Description: "Mark a task you have claimed as complete. This unblocks any task that depends " +
			"on it. You may only complete a task that is in-progress and assigned to you.",
		Schema: json.RawMessage(`{
  "type": "object",
  "properties": {
    "task_id": {"type": "string", "description": "The id of the in-progress task you own."}
  },
  "required": ["task_id"]
}`),
	}
}

func (completeTaskTool) ReadOnly() bool { return false }

func (t completeTaskTool) Execute(ctx context.Context, call session.ToolCall, _ tool.Workspace) (session.ToolResult, error) {
	var args completeTaskArgs
	if msg, ok := parseTeamArgs(call, &args); !ok {
		return session.NewToolError(call.ID, msg), nil
	}
	if strings.TrimSpace(args.TaskID) == "" {
		return session.NewToolError(call.ID, "CompleteTask: 'task_id' is required"), nil
	}
	if blocked, msg := fireTeamGate(ctx, t.hooks, governance.PhaseTaskCompleted, "CompleteTask", t.self,
		map[string]any{"task_id": args.TaskID, "by": t.self}); blocked {
		return session.NewToolError(call.ID, "CompleteTask: "+msg), nil
	}
	if err := t.team.CompleteTask(team.TaskID(args.TaskID), t.self); err != nil {
		return session.NewToolError(call.ID, fmt.Sprintf("CompleteTask: %v", err)), nil
	}
	return session.NewToolResult(call.ID, fmt.Sprintf("Completed task %q.", args.TaskID)), nil
}

// --- ListTasks ------------------------------------------------------------

type listTasksTool struct {
	team *team.Team
	self string
}

func (listTasksTool) Spec() tool.ToolSpec {
	return tool.ToolSpec{
		Name: "ListTasks",
		Description: "List the shared team task list with each task's id, state (pending / " +
			"in_progress / completed), assignee, dependencies, and description. Use it to see what " +
			"work remains, what others are doing, and what is unblocked.",
		Schema: json.RawMessage(`{"type": "object", "properties": {}}`),
	}
}

func (listTasksTool) ReadOnly() bool { return true }

func (t listTasksTool) Execute(_ context.Context, call session.ToolCall, _ tool.Workspace) (session.ToolResult, error) {
	tasks := t.team.Tasks()
	if len(tasks) == 0 {
		return session.NewToolResult(call.ID, "The team task list is empty."), nil
	}
	var b strings.Builder
	fmt.Fprintf(&b, "%d task(s):\n", len(tasks))
	for _, task := range tasks {
		assignee := task.Assignee
		if assignee == "" {
			assignee = "-"
		}
		fmt.Fprintf(&b, "- %s [%s] assignee=%s", task.ID, task.State, assignee)
		if len(task.Deps) > 0 {
			deps := make([]string, len(task.Deps))
			for i, d := range task.Deps {
				deps[i] = string(d)
			}
			fmt.Fprintf(&b, " deps=%s", strings.Join(deps, ","))
		}
		fmt.Fprintf(&b, ": %s\n", task.Description)
	}
	return session.NewToolResult(call.ID, b.String()), nil
}

// --- helpers --------------------------------------------------------------

// parseTeamArgs unmarshals a tool call's JSON arguments into dst. It returns a
// model-facing error string (not a Go error) and ok=false on malformed JSON. It
// mirrors the inline parsing in subagent.go rather than importing the adapter-layer
// toolkit, since internal/agent must not depend on an adapter.
func parseTeamArgs(call session.ToolCall, dst any) (string, bool) {
	if len(call.Args) == 0 {
		return "", true
	}
	if err := json.Unmarshal(call.Args, dst); err != nil {
		return fmt.Sprintf("invalid arguments: %v", err), false
	}
	return "", true
}
