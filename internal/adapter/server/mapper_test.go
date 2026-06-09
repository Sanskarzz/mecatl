package server

import (
	"encoding/json"
	"testing"
	"time"

	mecatlv1 "github.com/stacklok/mecatl/contracts/gen/go/mecatl/v1"
	"github.com/stacklok/mecatl/internal/session"
)

// TestToProtoTable round-trips every EventType and each structured submessage
// through toProto, asserting the proto shape matches the domain Event.
func TestToProtoTable(t *testing.T) {
	cases := []struct {
		name   string
		in     session.Event
		assert func(t *testing.T, got *mecatlv1.Event)
	}{
		{
			name: "session.init",
			in:   session.Event{Type: session.EvSessionInit, Seq: 1, Turn: 0},
			assert: func(t *testing.T, got *mecatlv1.Event) {
				if got.GetType() != "session.init" || got.GetSeq() != 1 {
					t.Fatalf("got %+v", got)
				}
			},
		},
		{
			name: "turn.start",
			in:   session.Event{Type: session.EvTurnStart, Seq: 2, Turn: 3},
			assert: func(t *testing.T, got *mecatlv1.Event) {
				if got.GetType() != "turn.start" || got.GetTurn() != 3 {
					t.Fatalf("got %+v", got)
				}
			},
		},
		{
			name: "message.delta",
			in:   session.Event{Type: session.EvMessageDelta, Seq: 3, Turn: 1, Text: "hello"},
			assert: func(t *testing.T, got *mecatlv1.Event) {
				if got.GetType() != "message.delta" || got.GetText() != "hello" {
					t.Fatalf("got %+v", got)
				}
			},
		},
		{
			name: "reasoning.delta",
			in:   session.Event{Type: session.EvReasoningDelta, Seq: 11, Turn: 1, Text: "thinking…"},
			assert: func(t *testing.T, got *mecatlv1.Event) {
				if got.GetType() != "reasoning.delta" || got.GetText() != "thinking…" {
					t.Fatalf("got %+v", got)
				}
				if got.GetTurnEnd() != nil {
					t.Fatalf("reasoning.delta should not carry a turn_end payload: %+v", got)
				}
			},
		},
		{
			name: "turn.end",
			in: session.Event{Type: session.EvTurnEnd, Seq: 12, Turn: 2,
				TurnEnd: &session.TurnEndPayload{DurationMs: 4100,
					Usage: session.Usage{InputTokens: 1200, OutputTokens: 340, CacheReadTokens: 800, CacheWriteTokens: 100}}},
			assert: func(t *testing.T, got *mecatlv1.Event) {
				if got.GetType() != "turn.end" || got.GetTurn() != 2 {
					t.Fatalf("got %+v", got)
				}
				// turn.end carries its per-turn data in the typed turn_end submessage,
				// NOT in the shared Event.usage field (which is cumulative-on-result).
				if got.GetUsage() != nil {
					t.Fatalf("turn.end must not set the shared Event.usage field: %+v", got)
				}
				te := got.GetTurnEnd()
				if te == nil {
					t.Fatalf("turn.end missing turn_end payload: %+v", got)
				}
				if te.GetDurationMs() != 4100 {
					t.Fatalf("duration_ms = %d, want 4100", te.GetDurationMs())
				}
				u := te.GetUsage()
				if u.GetInputTokens() != 1200 || u.GetOutputTokens() != 340 ||
					u.GetCacheReadTokens() != 800 || u.GetCacheWriteTokens() != 100 {
					t.Fatalf("per-turn usage mismatch: %+v", u)
				}
			},
		},
		{
			name: "tool.call",
			in: session.Event{Type: session.EvToolCall, Seq: 4, Turn: 1,
				ToolCall: &session.ToolCall{ID: "c1", Name: "Read", Args: json.RawMessage(`{"path":"a.go"}`)}},
			assert: func(t *testing.T, got *mecatlv1.Event) {
				tc := got.GetToolCall()
				if tc == nil || tc.GetId() != "c1" || tc.GetName() != "Read" || tc.GetArgs() != `{"path":"a.go"}` {
					t.Fatalf("got %+v", got)
				}
			},
		},
		{
			name: "tool.result",
			in: session.Event{Type: session.EvToolResult, Seq: 5, Turn: 1,
				ToolResult: &session.ToolResult{CallID: "c1", Content: "body", IsError: true}},
			assert: func(t *testing.T, got *mecatlv1.Event) {
				tr := got.GetToolResult()
				if tr == nil || tr.GetCallId() != "c1" || tr.GetContent() != "body" || !tr.GetIsError() {
					t.Fatalf("got %+v", got)
				}
			},
		},
		{
			name: "permission.ask",
			in: session.Event{Type: session.EvPermissionAsk, Seq: 6, Turn: 1,
				Ask: &session.PendingAsk{AskID: "a1", Tool: "Write", Args: json.RawMessage(`{"path":"x"}`), Reason: "needs approval"}},
			assert: func(t *testing.T, got *mecatlv1.Event) {
				a := got.GetAsk()
				if a == nil || a.GetAskId() != "a1" || a.GetTool() != "Write" ||
					a.GetArgs() != `{"path":"x"}` || a.GetReason() != "needs approval" {
					t.Fatalf("got %+v", got)
				}
			},
		},
		{
			name: "hook",
			in: session.Event{Type: session.EvHook, Seq: 7, Turn: 1, Text: "blocked-by-policy",
				Hook: &session.HookPayload{Phase: "PreToolUse", Tool: "Bash", Decision: session.HookBlocked, CallID: "call-7"}},
			assert: func(t *testing.T, got *mecatlv1.Event) {
				if got.GetType() != "hook" || got.GetText() != "blocked-by-policy" {
					t.Fatalf("got %+v", got)
				}
				h := got.GetHook()
				if h == nil || h.GetPhase() != "PreToolUse" || h.GetTool() != "Bash" ||
					h.GetDecision() != mecatlv1.HookDecision_HOOK_DECISION_BLOCKED ||
					h.GetCallId() != "call-7" {
					t.Fatalf("hook payload mismatch: %+v", h)
				}
			},
		},
		{
			name: "hook info default",
			in:   session.Event{Type: session.EvHook, Seq: 7, Turn: 1, Text: "ran", Hook: &session.HookPayload{Phase: "Stop"}},
			assert: func(t *testing.T, got *mecatlv1.Event) {
				h := got.GetHook()
				if h == nil || h.GetDecision() != mecatlv1.HookDecision_HOOK_DECISION_INFO {
					t.Fatalf("empty decision should map to INFO: %+v", h)
				}
				if h.GetCallId() != "" {
					t.Errorf("a non-tool (Stop) hook should carry no call id, got %q", h.GetCallId())
				}
			},
		},
		{
			name: "subagent.start",
			in: session.Event{Type: session.EvSubagentStart, Seq: 20, Turn: 1,
				Subagent: &session.SubagentPayload{ParentCallID: "p1", ChildID: "subagent-p1", Goal: "investigate main.go"}},
			assert: func(t *testing.T, got *mecatlv1.Event) {
				s := got.GetSubagent()
				if s == nil || s.GetParentCallId() != "p1" || s.GetChildId() != "subagent-p1" || s.GetGoal() != "investigate main.go" {
					t.Fatalf("subagent.start payload mismatch: %+v", s)
				}
			},
		},
		{
			name: "subagent.tool",
			in: session.Event{Type: session.EvSubagentTool, Seq: 21, Turn: 1,
				Subagent: &session.SubagentPayload{ParentCallID: "p1", ChildID: "subagent-p1", ToolName: "Grep", IsError: true, ToolCount: 3}},
			assert: func(t *testing.T, got *mecatlv1.Event) {
				s := got.GetSubagent()
				if s == nil || s.GetToolName() != "Grep" || !s.GetIsError() || s.GetToolCount() != 3 {
					t.Fatalf("subagent.tool payload mismatch: %+v", s)
				}
			},
		},
		{
			name: "subagent.end",
			in: session.Event{Type: session.EvSubagentEnd, Seq: 22, Turn: 1,
				Subagent: &session.SubagentPayload{ParentCallID: "p1", ChildID: "subagent-p1", ToolCount: 5,
					Usage: session.Usage{InputTokens: 90, OutputTokens: 12, CacheReadTokens: 40, CacheWriteTokens: 8},
					Stop:  session.StopMaxToolCalls, DurationMs: 1234}},
			assert: func(t *testing.T, got *mecatlv1.Event) {
				s := got.GetSubagent()
				if s == nil || s.GetToolCount() != 5 || s.GetStop() != "max_tool_calls" || s.GetDurationMs() != 1234 {
					t.Fatalf("subagent.end payload mismatch: %+v", s)
				}
				u := s.GetUsage()
				if u.GetInputTokens() != 90 || u.GetOutputTokens() != 12 ||
					u.GetCacheReadTokens() != 40 || u.GetCacheWriteTokens() != 8 {
					t.Fatalf("subagent.end usage mismatch: %+v", u)
				}
			},
		},
		{
			name: "team.start",
			in: session.Event{Type: session.EvTeamStart, Seq: 30, Turn: 1,
				Team: &session.TeamPayload{ParentCallID: "p1", TeamID: "team-p1",
					Roster: []session.TeamMemberSpec{
						{Name: "lead", Role: "coordinate", Lead: true},
						{Name: "worker", Role: "investigate", Mutating: true},
					}}},
			assert: func(t *testing.T, got *mecatlv1.Event) {
				tm := got.GetTeam()
				if tm == nil || tm.GetParentCallId() != "p1" || tm.GetTeamId() != "team-p1" {
					t.Fatalf("team.start ids mismatch: %+v", tm)
				}
				r := tm.GetRoster()
				if len(r) != 2 || r[0].GetName() != "lead" || !r[0].GetLead() ||
					r[1].GetName() != "worker" || !r[1].GetMutating() || r[1].GetLead() {
					t.Fatalf("team.start roster mismatch: %+v", r)
				}
			},
		},
		{
			name: "team.member",
			in: session.Event{Type: session.EvTeamMember, Seq: 31, Turn: 1,
				Team: &session.TeamPayload{ParentCallID: "p1", TeamID: "team-p1", Member: "worker",
					InnerKind: session.EvToolResult, ToolName: "Read", Detail: "capped body", IsError: true}},
			assert: func(t *testing.T, got *mecatlv1.Event) {
				tm := got.GetTeam()
				if tm == nil || tm.GetMember() != "worker" || tm.GetInnerKind() != "tool.result" ||
					tm.GetToolName() != "Read" || tm.GetDetail() != "capped body" || !tm.GetIsError() {
					t.Fatalf("team.member payload mismatch: %+v", tm)
				}
			},
		},
		{
			name: "team.member turn.end context meter",
			in: session.Event{Type: session.EvTeamMember, Seq: 33, Turn: 1,
				Team: &session.TeamPayload{ParentCallID: "p1", TeamID: "team-p1", Member: "worker",
					InnerKind:     session.EvTurnEnd,
					Usage:         session.Usage{InputTokens: 40000, OutputTokens: 80},
					ContextUsed:   40000,
					ContextWindow: 200000}},
			assert: func(t *testing.T, got *mecatlv1.Event) {
				tm := got.GetTeam()
				if tm == nil || tm.GetContextUsed() != 40000 || tm.GetContextWindow() != 200000 {
					t.Fatalf("team.member context-meter fields mismatch: used=%d window=%d",
						tm.GetContextUsed(), tm.GetContextWindow())
				}
			},
		},
		{
			name: "team.end",
			in: session.Event{Type: session.EvTeamEnd, Seq: 32, Turn: 1,
				Team: &session.TeamPayload{ParentCallID: "p1", TeamID: "team-p1", Rounds: 3,
					Stop:  session.StopEndTurn,
					Usage: session.Usage{InputTokens: 50, OutputTokens: 9}}},
			assert: func(t *testing.T, got *mecatlv1.Event) {
				tm := got.GetTeam()
				if tm == nil || tm.GetRounds() != 3 || tm.GetStop() != "end_turn" {
					t.Fatalf("team.end payload mismatch: %+v", tm)
				}
				if tm.GetUsage().GetInputTokens() != 50 || tm.GetUsage().GetOutputTokens() != 9 {
					t.Fatalf("team.end usage mismatch: %+v", tm.GetUsage())
				}
			},
		},
		{
			name: "team.tasks snapshot",
			in: session.Event{Type: session.EvTeamTasks, Seq: 34, Turn: 1,
				Team: &session.TeamPayload{ParentCallID: "p1", TeamID: "team-p1",
					Tasks: []session.TeamTaskSnapshot{
						{ID: "task-1", Description: "investigate", State: "completed", Assignee: "scout"},
						{ID: "task-2", Description: "fix", State: "pending", Deps: []string{"task-1"}},
					}}},
			assert: func(t *testing.T, got *mecatlv1.Event) {
				if got.GetType() != "team.tasks" {
					t.Fatalf("event type = %q, want team.tasks", got.GetType())
				}
				tm := got.GetTeam()
				if tm == nil || tm.GetMember() != "" {
					t.Fatalf("team.tasks must carry no member: %+v", tm)
				}
				tasks := tm.GetTasks()
				if len(tasks) != 2 {
					t.Fatalf("tasks len = %d, want 2: %+v", len(tasks), tasks)
				}
				if tasks[0].GetId() != "task-1" || tasks[0].GetState() != "completed" ||
					tasks[0].GetAssignee() != "scout" {
					t.Errorf("task-1 mapping mismatch: %+v", tasks[0])
				}
				if tasks[1].GetId() != "task-2" || len(tasks[1].GetDeps()) != 1 ||
					tasks[1].GetDeps()[0] != "task-1" {
					t.Errorf("task-2 deps not preserved: %+v", tasks[1])
				}
			},
		},
		{
			name: "team.findings snapshot",
			in: session.Event{Type: session.EvTeamFindings, Seq: 35, Turn: 1,
				Team: &session.TeamPayload{ParentCallID: "p1", TeamID: "team-p1",
					Findings: []session.TeamFindingSnapshot{
						{Member: "scout", Body: "the cache key omits the tenant id"},
						{Member: "fixer", Body: "patched the key"},
					}}},
			assert: func(t *testing.T, got *mecatlv1.Event) {
				if got.GetType() != "team.findings" {
					t.Fatalf("event type = %q, want team.findings", got.GetType())
				}
				tm := got.GetTeam()
				if tm == nil || tm.GetMember() != "" {
					t.Fatalf("team.findings must carry no member: %+v", tm)
				}
				findings := tm.GetFindings()
				if len(findings) != 2 {
					t.Fatalf("findings len = %d, want 2: %+v", len(findings), findings)
				}
				if findings[0].GetMember() != "scout" || findings[0].GetBody() != "the cache key omits the tenant id" {
					t.Errorf("finding[0] mapping mismatch: %+v", findings[0])
				}
				if findings[1].GetMember() != "fixer" {
					t.Errorf("finding[1] member mismatch: %+v", findings[1])
				}
			},
		},
		{
			name: "team.end disposition snapshot",
			in: session.Event{Type: session.EvTeamEnd, Seq: 36, Turn: 1,
				Team: &session.TeamPayload{ParentCallID: "p1", TeamID: "team-p1", Rounds: 2,
					Stop: session.StopEndTurn,
					Dispositions: []session.TeamMemberDisposition{
						{Name: "lead", Disposition: "done"},
						{Name: "scout", Disposition: "stopped", Reason: "budget"},
						{Name: "fixer", Disposition: "stopped", Reason: "error"},
						{Name: "probe", Disposition: "stopped", Reason: "cancelled"},
					}}},
			assert: func(t *testing.T, got *mecatlv1.Event) {
				disps := got.GetTeam().GetDispositions()
				if len(disps) != 4 {
					t.Fatalf("dispositions len = %d, want 4: %+v", len(disps), disps)
				}
				// done → stopped=false, reason UNSPECIFIED.
				if disps[0].GetName() != "lead" || disps[0].GetStopped() ||
					disps[0].GetReason() != mecatlv1.TeamMemberStopReason_TEAM_MEMBER_STOP_REASON_UNSPECIFIED {
					t.Errorf("done disposition mismatch: %+v", disps[0])
				}
				if !disps[1].GetStopped() || disps[1].GetReason() != mecatlv1.TeamMemberStopReason_TEAM_MEMBER_STOP_REASON_BUDGET {
					t.Errorf("budget disposition mismatch: %+v", disps[1])
				}
				if !disps[2].GetStopped() || disps[2].GetReason() != mecatlv1.TeamMemberStopReason_TEAM_MEMBER_STOP_REASON_ERROR {
					t.Errorf("error disposition mismatch: %+v", disps[2])
				}
				if !disps[3].GetStopped() || disps[3].GetReason() != mecatlv1.TeamMemberStopReason_TEAM_MEMBER_STOP_REASON_CANCELLED {
					t.Errorf("cancelled disposition mismatch: %+v", disps[3])
				}
			},
		},
		{
			name: "parallel.start",
			in: session.Event{Type: session.EvParallelStart, Seq: 40, Turn: 1,
				Parallel: &session.ParallelPayload{ParentCallID: "p1", Join: "judge", BranchCount: 3}},
			assert: func(t *testing.T, got *mecatlv1.Event) {
				p := got.GetParallel()
				if got.GetType() != "parallel.start" {
					t.Fatalf("type = %q, want parallel.start", got.GetType())
				}
				if p == nil || p.GetParentCallId() != "p1" || p.GetJoin() != "judge" || p.GetBranchCount() != 3 {
					t.Fatalf("parallel.start payload mismatch: %+v", p)
				}
			},
		},
		{
			name: "parallel.branch branch_start",
			in: session.Event{Type: session.EvParallelBranch, Seq: 41, Turn: 1,
				Parallel: &session.ParallelPayload{ParentCallID: "p1", Kind: session.ParallelBranchStart,
					BranchIndex: 1, BranchLabel: "branch-2", Goal: "explore beta"}},
			assert: func(t *testing.T, got *mecatlv1.Event) {
				p := got.GetParallel()
				if p == nil || p.GetKind() != "branch_start" || p.GetBranchIndex() != 1 ||
					p.GetBranchLabel() != "branch-2" || p.GetGoal() != "explore beta" {
					t.Fatalf("parallel branch_start payload mismatch: %+v", p)
				}
			},
		},
		{
			name: "parallel.branch branch_tool",
			in: session.Event{Type: session.EvParallelBranch, Seq: 42, Turn: 1,
				Parallel: &session.ParallelPayload{ParentCallID: "p1", Kind: session.ParallelBranchTool,
					BranchIndex: 0, ToolName: "Grep", IsError: true, ToolCount: 2}},
			assert: func(t *testing.T, got *mecatlv1.Event) {
				p := got.GetParallel()
				if p == nil || p.GetKind() != "branch_tool" || p.GetToolName() != "Grep" ||
					!p.GetIsError() || p.GetToolCount() != 2 {
					t.Fatalf("parallel branch_tool payload mismatch: %+v", p)
				}
			},
		},
		{
			name: "parallel.branch branch_end",
			in: session.Event{Type: session.EvParallelBranch, Seq: 43, Turn: 1,
				Parallel: &session.ParallelPayload{ParentCallID: "p1", Kind: session.ParallelBranchEnd,
					BranchIndex: 2, ToolCount: 4, Failed: true, Workspace: "/fork/branch-3",
					Stop: session.StopError, DurationMs: 555,
					Usage: session.Usage{InputTokens: 12, OutputTokens: 3}}},
			assert: func(t *testing.T, got *mecatlv1.Event) {
				p := got.GetParallel()
				if p == nil || p.GetKind() != "branch_end" || p.GetBranchIndex() != 2 ||
					!p.GetFailed() || p.GetWorkspace() != "/fork/branch-3" ||
					p.GetStop() != "error" || p.GetDurationMs() != 555 || p.GetToolCount() != 4 {
					t.Fatalf("parallel branch_end payload mismatch: %+v", p)
				}
				if u := p.GetUsage(); u.GetInputTokens() != 12 || u.GetOutputTokens() != 3 {
					t.Fatalf("parallel branch_end usage mismatch: %+v", u)
				}
			},
		},
		{
			name: "parallel.end winner",
			in: session.Event{Type: session.EvParallelEnd, Seq: 44, Turn: 1,
				Parallel: &session.ParallelPayload{ParentCallID: "p1", Join: "judge", BranchCount: 3,
					Winner: 1, WinnerWorkspace: "/fork/branch-2", Stop: session.StopEndTurn,
					Usage: session.Usage{InputTokens: 100, OutputTokens: 20}}},
			assert: func(t *testing.T, got *mecatlv1.Event) {
				p := got.GetParallel()
				if got.GetType() != "parallel.end" {
					t.Fatalf("type = %q, want parallel.end", got.GetType())
				}
				if p == nil || p.GetWinner() != 1 || p.GetWinnerWorkspace() != "/fork/branch-2" ||
					p.GetJoin() != "judge" || p.GetBranchCount() != 3 || p.GetStop() != "end_turn" {
					t.Fatalf("parallel.end payload mismatch: %+v", p)
				}
				if u := p.GetUsage(); u.GetInputTokens() != 100 || u.GetOutputTokens() != 20 {
					t.Fatalf("parallel.end usage mismatch: %+v", u)
				}
			},
		},
		{
			name: "parallel.end join=all no winner",
			in: session.Event{Type: session.EvParallelEnd, Seq: 45, Turn: 1,
				Parallel: &session.ParallelPayload{ParentCallID: "p1", Join: "all", BranchCount: 2, Winner: -1}},
			assert: func(t *testing.T, got *mecatlv1.Event) {
				p := got.GetParallel()
				if p == nil || p.GetWinner() != -1 || p.GetWinnerWorkspace() != "" {
					t.Fatalf("parallel.end (all) should carry Winner=-1, no workspace: %+v", p)
				}
			},
		},
		{
			name: "compaction",
			in:   session.Event{Type: session.EvCompaction, Seq: 8, Turn: 2, Text: "summary"},
			assert: func(t *testing.T, got *mecatlv1.Event) {
				if got.GetType() != "compaction" || got.GetText() != "summary" {
					t.Fatalf("got %+v", got)
				}
			},
		},
		{
			name: "result",
			in: session.Event{Type: session.EvResult, Seq: 9, Turn: 2,
				Result: &session.ResultPayload{Stop: session.StopEndTurn, Text: "all done",
					Usage: session.Usage{InputTokens: 15, OutputTokens: 5, CacheReadTokens: 3, CacheWriteTokens: 1}},
				Usage: &session.Usage{InputTokens: 15, OutputTokens: 5}},
			assert: func(t *testing.T, got *mecatlv1.Event) {
				res := got.GetResult()
				if res == nil || res.GetStop() != "end_turn" || res.GetText() != "all done" {
					t.Fatalf("result mismatch: %+v", got)
				}
				u := res.GetUsage()
				if u.GetInputTokens() != 15 || u.GetOutputTokens() != 5 ||
					u.GetCacheReadTokens() != 3 || u.GetCacheWriteTokens() != 1 {
					t.Fatalf("usage mismatch: %+v", u)
				}
				if got.GetUsage().GetInputTokens() != 15 {
					t.Fatalf("event usage mismatch: %+v", got.GetUsage())
				}
			},
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got := toProto(tc.in)
			if got.GetType() != string(tc.in.Type) {
				t.Fatalf("type = %q, want %q", got.GetType(), tc.in.Type)
			}
			if got.GetSeq() != tc.in.Seq {
				t.Fatalf("seq = %d, want %d", got.GetSeq(), tc.in.Seq)
			}
			tc.assert(t, got)
		})
	}
}

// TestToProtoNoSubmessages confirms a bare event leaves all submessages nil.
func TestToProtoNoSubmessages(t *testing.T) {
	got := toProto(session.Event{Type: session.EvTurnStart})
	if got.GetToolCall() != nil || got.GetToolResult() != nil || got.GetAsk() != nil ||
		got.GetResult() != nil || got.GetTurnEnd() != nil || got.GetUsage() != nil ||
		got.GetSubagent() != nil || got.GetTeam() != nil || got.GetParallel() != nil {
		t.Fatalf("unexpected submessage on bare event: %+v", got)
	}
}

// TestSessionMapping checks the session snapshot mapping including mode and
// limits round-trips.
func TestSessionMapping(t *testing.T) {
	sess := session.New("s1", session.ModePlan, "/ws",
		session.Limits{MaxTurns: 4, MaxToolCalls: 8, MaxConsecutiveFailures: 2}, time.Unix(1000, 0))
	got := toProtoSession(sess)
	if got.GetSessionId() != "s1" || got.GetState() != "idle" {
		t.Fatalf("got %+v", got)
	}
	if got.GetMode() != mecatlv1.PermissionMode_PERMISSION_MODE_PLAN {
		t.Fatalf("mode = %v", got.GetMode())
	}
	if got.GetLimits().GetMaxTurns() != 4 || got.GetLimits().GetMaxToolCalls() != 8 {
		t.Fatalf("limits = %+v", got.GetLimits())
	}
	if got.GetCreatedAtUnix() != 1000 {
		t.Fatalf("created_at = %d", got.GetCreatedAtUnix())
	}
}

// TestModeRoundTrip checks mode mapping in both directions.
func TestModeRoundTrip(t *testing.T) {
	for _, m := range []session.PermissionMode{session.ModeDefault, session.ModePlan, session.ModeAccept} {
		if got := modeFromProto(modeToProto(m)); got != m {
			t.Fatalf("mode round-trip: %q -> %q", m, got)
		}
	}
	if got := modeFromProto(mecatlv1.PermissionMode_PERMISSION_MODE_UNSPECIFIED); got != session.ModeDefault {
		t.Fatalf("unspecified mode -> %q, want default", got)
	}
}

func TestContentFromProto(t *testing.T) {
	parts, err := contentFromProto([]*mecatlv1.Content{
		{Kind: mecatlv1.Content_KIND_IMAGE, MimeType: "image/png", Data: []byte{1, 2}},
		{Kind: mecatlv1.Content_KIND_AUDIO, MimeType: "audio/wav", Url: "https://media.example.com/a.wav"},
	})
	if err != nil {
		t.Fatalf("contentFromProto: %v", err)
	}
	if len(parts) != 2 {
		t.Fatalf("parts = %d, want 2", len(parts))
	}
	if parts[0].Kind != session.MediaImage || string(parts[0].Data) != string([]byte{1, 2}) {
		t.Fatalf("part 0 = %+v", parts[0])
	}
	if parts[1].Kind != session.MediaAudio || parts[1].URL != "https://media.example.com/a.wav" {
		t.Fatalf("part 1 = %+v", parts[1])
	}
}

func TestContentFromProtoEmpty(t *testing.T) {
	if got, err := contentFromProto(nil); err != nil || got != nil {
		t.Fatalf("contentFromProto(nil) = %v, %v; want nil, nil", got, err)
	}
}

func TestContentFromProtoRejectsUnspecifiedKind(t *testing.T) {
	_, err := contentFromProto([]*mecatlv1.Content{{Kind: mecatlv1.Content_KIND_UNSPECIFIED, MimeType: "image/png", Data: []byte{1}}})
	if err == nil {
		t.Fatal("expected reject for KIND_UNSPECIFIED")
	}
}

func TestContentFromProtoRejectsBothDataAndURL(t *testing.T) {
	_, err := contentFromProto([]*mecatlv1.Content{{
		Kind: mecatlv1.Content_KIND_IMAGE, MimeType: "image/png",
		Data: []byte{1}, Url: "https://x/y.png",
	}})
	if err == nil {
		t.Fatal("expected reject for both data and url set")
	}
}

func TestContentToProtoRoundTrip(t *testing.T) {
	in := []session.Content{
		{Kind: session.MediaImage, MIMEType: "image/png", Data: []byte{9}},
		{Kind: session.MediaAudio, MIMEType: "audio/wav", URL: "https://media.example.com/a.wav"},
	}
	out := contentToProto(in)
	back, err := contentFromProto(out)
	if err != nil {
		t.Fatalf("round-trip decode: %v", err)
	}
	if len(back) != 2 || back[0].Kind != session.MediaImage || back[1].URL != "https://media.example.com/a.wav" {
		t.Fatalf("round-trip mismatch: %+v", back)
	}
}

func TestContentFromProtoRejectsSSRFURL(t *testing.T) {
	for _, bad := range []string{
		"http://media.example.com/a.png",           // plaintext http
		"https://169.254.169.254/latest/meta-data", // metadata IP
		"https://127.0.0.1/a.png",                  // loopback
		"https://10.0.0.5/a.png",                   // RFC1918
		"https://localhost/a.png",                  // internal name
		"file:///etc/passwd",                       // file scheme
	} {
		_, err := contentFromProto([]*mecatlv1.Content{
			{Kind: mecatlv1.Content_KIND_IMAGE, MimeType: "image/png", Url: bad},
		})
		if err == nil {
			t.Fatalf("contentFromProto with url %q: expected reject, got nil", bad)
		}
	}
}

func TestContentFromProtoRejectsOversizedPart(t *testing.T) {
	_, err := contentFromProto([]*mecatlv1.Content{
		{Kind: mecatlv1.Content_KIND_IMAGE, MimeType: "image/png", Data: make([]byte, session.MaxMediaBytes+1)},
	})
	if err == nil {
		t.Fatal("expected reject for oversized inline part")
	}
}

func TestContentFromProtoRejectsTooManyParts(t *testing.T) {
	parts := make([]*mecatlv1.Content, session.MaxPromptMediaParts+1)
	for i := range parts {
		parts[i] = &mecatlv1.Content{Kind: mecatlv1.Content_KIND_IMAGE, MimeType: "image/png", Data: []byte{1}}
	}
	if _, err := contentFromProto(parts); err == nil {
		t.Fatal("expected reject for too many parts")
	}
}

func TestContentFromProtoRejectsMimeKindMismatch(t *testing.T) {
	_, err := contentFromProto([]*mecatlv1.Content{
		{Kind: mecatlv1.Content_KIND_IMAGE, MimeType: "audio/wav", Data: []byte{1}},
	})
	if err == nil {
		t.Fatal("expected reject for image kind with audio mime")
	}
	_, err = contentFromProto([]*mecatlv1.Content{
		{Kind: mecatlv1.Content_KIND_IMAGE, MimeType: "", Data: []byte{1}},
	})
	if err == nil {
		t.Fatal("expected reject for empty mime")
	}
}
