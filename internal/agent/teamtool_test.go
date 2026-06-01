package agent_test

import (
	"context"
	"encoding/json"
	"strings"
	"testing"

	"github.com/stacklok/mecatl/internal/adapter/hookexec"
	"github.com/stacklok/mecatl/internal/adapter/memfs"
	"github.com/stacklok/mecatl/internal/adapter/mockllm"
	"github.com/stacklok/mecatl/internal/adapter/permpolicy"
	"github.com/stacklok/mecatl/internal/agent"
	"github.com/stacklok/mecatl/internal/governance"
	"github.com/stacklok/mecatl/internal/session"
	"github.com/stacklok/mecatl/internal/team"
	"github.com/stacklok/mecatl/internal/tool"
)

// teamToolFactory builds the unified member-engine factory the Team tool needs: a
// per-member Engine whose provider is looked up by member name and whose catalog
// carries that member's coordination tools (bound to the per-call team). It mirrors
// the composition root's buildMemberEngine shape, scoped for an offline test.
func teamToolFactory(t *testing.T, providers map[string]*mockllm.Provider) agent.TeamMemberEngineFactory {
	t.Helper()
	allow := permpolicy.NewPolicy([]governance.Rule{{Effect: governance.Allow}}, nil)
	return func(tm *team.Team, spec agent.MemberSpec) agent.MemberBuild {
		prov, ok := providers[spec.Name]
		if !ok {
			t.Fatalf("teamToolFactory: no provider scripted for member %q", spec.Name)
		}
		cat := tool.NewCatalog()
		for _, tl := range agent.MemberTools(tm, spec.Name, nil) {
			cat.MustRegister(tl)
		}
		eng := agent.NewEngine(agent.Deps{
			LLM:     prov,
			Catalog: cat,
			Policy:  allow,
			Hooks:   hookexec.New(nil),
			Model:   "member-model",
		})
		return agent.MemberBuild{Engine: eng}
	}
}

// TestTeamToolFormsTeamAndIsolatesContent is the team analogue of gauntlet #7
// (content isolation), adapted for the Team tool. The main model calls Team with a
// 2-member roster; the test asserts:
//   - team.start carries the roster the model formed (names/roles/lead flag).
//   - interleaved team.member events are tagged by member and carry CONTENT (the
//     member's message text and tool names) — the fuller-but-bounded projection.
//   - team.end carries the round count.
//   - CRITICALLY, the parent's terminal ToolResult and the parent Conversation
//     contain ONLY the joined summary — none of the per-member transcripts.
func TestTeamToolFormsTeamAndIsolatesContent(t *testing.T) {
	// Lead: creates a task, then (after the worker reports) synthesises.
	addTask := session.NewToolCall("l1", "AddTask",
		json.RawMessage(`{"description":"investigate the reported bug"}`))
	// Each member turn carries a known per-turn usage (via UsageChunk on turn.end)
	// so the test can assert team.end's Usage is the SUM of every member turn.
	leadProv := mockllm.New(
		mockllm.ChunksTurn(
			mockllm.ToolCallChunk(addTask),
			mockllm.UsageChunk(session.Usage{InputTokens: 10, OutputTokens: 2}),
			mockllm.DoneChunk(session.StopEndTurn),
		),
		mockllm.ChunksTurn(
			mockllm.TextChunk("LEAD_SECRET: delegated, waiting for worker"),
			mockllm.UsageChunk(session.Usage{InputTokens: 20, OutputTokens: 4}),
			mockllm.DoneChunk(session.StopEndTurn),
		),
		mockllm.ChunksTurn(
			mockllm.TextChunk("LEAD_SECRET: worker reported; team complete"),
			mockllm.UsageChunk(session.Usage{InputTokens: 30, OutputTokens: 6}),
			mockllm.DoneChunk(session.StopEndTurn),
		),
	)

	// Worker: completes the task and reports back.
	complete := session.NewToolCall("w1", "CompleteTask", json.RawMessage(`{"task_id":"task-1"}`))
	report := session.NewToolCall("w2", "SendMessage",
		json.RawMessage(`{"to":"lead","body":"WORKER_SECRET: root cause found"}`))
	workerProv := mockllm.New(
		mockllm.ChunksTurn(
			mockllm.ToolCallChunk(complete),
			mockllm.ToolCallChunk(report),
			mockllm.UsageChunk(session.Usage{InputTokens: 40, OutputTokens: 8}),
			mockllm.DoneChunk(session.StopEndTurn),
		),
		mockllm.ChunksTurn(
			mockllm.TextChunk("WORKER_SECRET: investigation complete"),
			mockllm.UsageChunk(session.Usage{InputTokens: 50, OutputTokens: 10}),
			mockllm.DoneChunk(session.StopEndTurn),
		),
	)
	providers := map[string]*mockllm.Provider{"lead": leadProv, "worker": workerProv}

	teamTool := agent.NewTeamTool(teamToolFactory(t, providers))
	parentCat := catalogWith(t, teamTool)

	parentLLM := mockllm.New(
		mockllm.ToolCallTurn(toolCall("p1", "Team",
			`{"goal":"fix the bug","members":[{"name":"lead","role":"coordinate the fix"},{"name":"worker","role":"investigate and report"}]}`)),
		mockllm.TextTurn("parent received the team summary"),
	)
	e := newEngine(agent.Deps{LLM: parentLLM, Catalog: parentCat})
	sess := newSession(t, session.Limits{})
	r := e.Run(context.Background(), sess, memfs.NewWorkspace("/ws"), "fix the bug")
	evs := drain(r)

	// --- team.start: roster the model formed -------------------------------
	var start *session.TeamPayload
	for _, ev := range evs {
		if ev.Type == session.EvTeamStart {
			start = ev.Team
		}
	}
	if start == nil {
		t.Fatalf("no team.start event in %v", typesOf(evs))
	}
	if len(start.Roster) != 2 {
		t.Fatalf("roster len = %d, want 2: %+v", len(start.Roster), start.Roster)
	}
	if start.Roster[0].Name != "lead" || !start.Roster[0].Lead {
		t.Errorf("first roster member should be the synthesized lead: %+v", start.Roster[0])
	}
	if start.Roster[1].Name != "worker" || start.Roster[1].Lead {
		t.Errorf("second roster member should be a non-lead worker: %+v", start.Roster[1])
	}

	// --- team.member: interleaved, tagged, WITH content --------------------
	var members []*session.TeamPayload
	sawLeadText, sawWorkerToolCall := false, false
	for _, ev := range evs {
		if ev.Type != session.EvTeamMember {
			continue
		}
		members = append(members, ev.Team)
		if ev.Team.Member == "lead" && strings.Contains(ev.Team.Text, "delegated") {
			sawLeadText = true
		}
		if ev.Team.Member == "worker" && ev.Team.InnerKind == session.EvToolCall &&
			ev.Team.ToolName == "CompleteTask" {
			sawWorkerToolCall = true
		}
	}
	if len(members) == 0 {
		t.Fatalf("no team.member events forwarded")
	}
	if !sawLeadText {
		t.Errorf("expected a lead team.member event carrying message text (fuller projection)")
	}
	if !sawWorkerToolCall {
		t.Errorf("expected a worker team.member event carrying the CompleteTask tool name")
	}

	// --- team.end: round count + summed team-total usage -------------------
	var end *session.TeamPayload
	for _, ev := range evs {
		if ev.Type == session.EvTeamEnd {
			end = ev.Team
		}
	}
	if end == nil {
		t.Fatalf("no team.end event")
	}
	if end.Rounds < 1 {
		t.Errorf("team.end rounds = %d, want >= 1", end.Rounds)
	}

	// team.end.Usage must be the SUM of every member's per-turn usage, reconstructed
	// here from the per-event usage carried on the forwarded team.member turn.end
	// events. With the scripted turns above this is in=150 out=30; deriving the
	// expected sum from the stream (rather than hardcoding) keeps the assertion
	// robust to the exact number of rounds.
	var wantUsage session.Usage
	for _, ev := range evs {
		if ev.Type == session.EvTeamMember && ev.Team.InnerKind == session.EvTurnEnd {
			wantUsage = wantUsage.Add(ev.Team.Usage)
		}
	}
	if wantUsage == (session.Usage{}) {
		t.Fatalf("test setup: no member turn.end usage was forwarded to sum")
	}
	if end.Usage != wantUsage {
		t.Errorf("team.end usage = %+v, want the summed member total %+v", end.Usage, wantUsage)
	}
	if end.Usage.InputTokens != 150 || end.Usage.OutputTokens != 30 {
		t.Errorf("team.end usage = %+v, want in=150 out=30 (sum of the 5 scripted turns)", end.Usage)
	}

	// --- CONTENT ISOLATION: only the joined summary enters the parent ------
	var parentResults []*session.ToolResult
	for _, ev := range evs {
		if ev.Type == session.EvToolResult {
			parentResults = append(parentResults, ev.ToolResult)
		}
	}
	if len(parentResults) != 1 {
		t.Fatalf("parent saw %d tool results, want exactly 1 (the Team summary)", len(parentResults))
	}
	summary := parentResults[0].Content
	// The joined summary names members and reports their LAST text, which legitimately
	// includes the lead/worker terminal lines. The isolation guarantee is about the
	// parent CONVERSATION (the LLM context), asserted below: the per-member transcript
	// (intermediate tool calls/results, mid-run messages) must not be in Conversation.
	if !strings.Contains(summary, "lead") || !strings.Contains(summary, "worker") {
		t.Errorf("joined summary should name both members: %q", summary)
	}

	// The parent Conversation must contain ONLY: the user prompt, the parent's Team
	// tool call, the Team ToolResult (the joined summary), and the parent's final
	// text. It must NOT contain any member's intermediate tool calls (AddTask /
	// CompleteTask / SendMessage) nor any member message text other than via the
	// summary ToolResult.
	for _, msg := range sess.Conversation.Messages {
		for _, tc := range msg.ToolCalls {
			if tc.Name == "AddTask" || tc.Name == "CompleteTask" || tc.Name == "SendMessage" {
				t.Fatalf("member tool call %q leaked into parent Conversation", tc.Name)
			}
		}
		// A member's intermediate message text ("delegated, waiting") must not appear
		// in any parent message TEXT (it may legitimately appear only inside the Team
		// ToolResult content, which is the summary's member LastText — checked via the
		// ToolResult body, not message Text).
		if strings.Contains(msg.Text, "delegated, waiting") {
			t.Fatalf("member intermediate message text leaked into parent Conversation message text: %q", msg.Text)
		}
	}
}

// TestTeamToolReadOnlyIsFalse pins the mutate-serial contract: unlike the
// read-parallel Task/Fork tools, the Team tool reports ReadOnly() == false so the
// dispatcher serialises it.
func TestTeamToolReadOnlyIsFalse(t *testing.T) {
	tt := agent.NewTeamTool(func(*team.Team, agent.MemberSpec) agent.MemberBuild { return agent.MemberBuild{} })
	ro, ok := tt.(interface{ ReadOnly() bool })
	if !ok {
		t.Fatalf("Team tool does not expose ReadOnly()")
	}
	if ro.ReadOnly() {
		t.Errorf("Team.ReadOnly() = true, want false (teams are mutate-serial)")
	}
}

// TestTeamToolBadRoster asserts a malformed roster yields a recoverable tool ERROR
// result (not a harness error), so the model can retry. Cases: empty goal, empty
// members, empty member name, duplicate names, empty role.
func TestTeamToolBadRoster(t *testing.T) {
	tt := agent.NewTeamTool(func(*team.Team, agent.MemberSpec) agent.MemberBuild {
		t.Fatalf("factory must not be called for an invalid roster")
		return agent.MemberBuild{}
	})
	cases := []struct {
		name string
		args string
	}{
		{"empty goal", `{"goal":"  ","members":[{"name":"a","role":"r"}]}`},
		{"no members", `{"goal":"g","members":[]}`},
		{"empty name", `{"goal":"g","members":[{"name":"","role":"r"}]}`},
		{"duplicate names", `{"goal":"g","members":[{"name":"a","role":"r"},{"name":"a","role":"r"}]}`},
		{"empty role", `{"goal":"g","members":[{"name":"a","role":" "}]}`},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			res, err := tt.Execute(context.Background(),
				toolCall("c1", "Team", tc.args), memfs.NewWorkspace("/ws"))
			if err != nil {
				t.Fatalf("Execute returned a harness error, want a tool error result: %v", err)
			}
			if !res.IsError {
				t.Fatalf("expected an error tool result for %s, got: %q", tc.name, res.Content)
			}
		})
	}
}

// TestTeamToolMutatingMemberNoForker asserts that a Mutating member with no Forker
// wired yields a recoverable tool error (the model can retry with a read-only
// roster) rather than panicking or returning a harness error.
func TestTeamToolMutatingMemberNoForker(t *testing.T) {
	providers := map[string]*mockllm.Provider{
		"lead": mockllm.New(mockllm.TextTurn("ok")),
	}
	// No WithTeamToolForker → a Mutating member cannot be enrolled.
	tt := agent.NewTeamTool(teamToolFactory(t, providers))
	res, err := tt.Execute(context.Background(),
		toolCall("c1", "Team",
			`{"goal":"g","members":[{"name":"lead","role":"do it","mutating":true}]}`),
		memfs.NewWorkspace("/ws"))
	if err != nil {
		t.Fatalf("Execute returned a harness error: %v", err)
	}
	if !res.IsError {
		t.Fatalf("expected a tool error for a Mutating member with no forker, got: %q", res.Content)
	}
}

// TestTeamToolNilFactoryPanics pins the composition-root contract: a Team tool with
// no member-engine factory is a programming error.
func TestTeamToolNilFactoryPanics(t *testing.T) {
	defer func() {
		if recover() == nil {
			t.Fatalf("NewTeamTool(nil) did not panic")
		}
	}()
	_ = agent.NewTeamTool(nil)
}

// TestTeamToolParentCancelStops asserts the parent ctx flows into the supervisor: a
// cancelled parent context stops the team promptly and the tool still returns a
// (joined-summary) result. The supervisor's deferred cleanupAll tears down member
// workspaces on exit; with read-only members there are no forks to leak, and the
// run must not hang.
func TestTeamToolParentCancelStops(t *testing.T) {
	providers := map[string]*mockllm.Provider{
		"lead": mockllm.New(
			mockllm.TextTurn("r1"), mockllm.TextTurn("r2"), mockllm.TextTurn("r3"),
		),
	}
	tt := agent.NewTeamTool(teamToolFactory(t, providers))

	ctx, cancel := context.WithCancel(context.Background())
	cancel() // cancel before running

	res, err := tt.Execute(ctx,
		toolCall("c1", "Team", `{"goal":"g","members":[{"name":"lead","role":"loop"}]}`),
		memfs.NewWorkspace("/ws"))
	if err != nil {
		t.Fatalf("Execute returned a harness error on cancel: %v", err)
	}
	// A cancelled run returns a result (the joined summary); it must not hang or error
	// at the harness level. The content is the summary of however little ran.
	if strings.TrimSpace(res.Content) == "" {
		t.Fatalf("expected a non-empty joined summary even on cancel")
	}
}
