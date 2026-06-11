package app

import (
	"context"
	"encoding/json"
	"strings"
	"testing"

	"github.com/stacklok/mecatl/engine/session"
	"github.com/stacklok/mecatl/engine/tool"
	"github.com/stacklok/mecatl/internal/adapter/memory"
)

// drainRun consumes a run's events to completion, returning the terminal result
// text. Any permission ask is auto-allowed.
func drainRun(run interface {
	Events() <-chan session.Event
	Approve(string, session.ApprovalVerdict)
}) string {
	var final string
	for ev := range run.Events() {
		if ev.Type == session.EvPermissionAsk && ev.Ask != nil {
			run.Approve(ev.Ask.AskID, session.VerdictAllowOnce)
		}
		if ev.Type == session.EvResult && ev.Result != nil {
			final = ev.Result.Text
		}
	}
	return final
}

// TestUserModelE2E is the headline (R8) Phase-2a proof through the FULL composition
// (app.Build → server.Service):
//
//   - "Session A" writes a fact through the REAL RememberUser tool the catalog wires
//     (memory.NewUserModelTools over the configured user-model dir) — exercising the
//     enforced "user/" prefix, the write-time injection scan, and persistence.
//   - A Service built over the SAME user-model dir then runs a session and asserts
//     the turn-0 conversation contains the <user-model> fence with the saved fact —
//     proving the cross-session store round-trips and the UserModelAssembler injects
//     it on a subsequent session's turn 0.
//
// (The canned mock provider cannot be scripted to emit a tool call through Build,
// so session A's WRITE is driven via the tool directly — the same tool.Execute the
// model would invoke; the injection HALF runs through the full Build→Service→run
// path. The agent-driven RememberUser write is covered end to end in the agent
// reviewer unit test and the adapter tool tests.)
//
// All offline: mock provider, real temp dirs.
func TestUserModelE2E(t *testing.T) {
	ctx := context.Background()
	workspace := t.TempDir()
	userModelDir := t.TempDir()

	// --- Session A's effect: write a fact through the real RememberUser tool. ----
	storeA, err := memory.New(userModelDir)
	if err != nil {
		t.Fatalf("memory.New: %v", err)
	}
	var remember tool.Tool
	for _, tl := range memory.NewUserModelTools(storeA) {
		if tl.Spec().Name == memory.RememberUserToolName {
			remember = tl
		}
	}
	rememberArgs, _ := json.Marshal(map[string]any{
		"key":         "comm-style",
		"value":       "Prefers terse, direct answers with no preamble.",
		"description": "communication style",
	})
	res, err := remember.Execute(ctx, session.NewToolCall("c1", memory.RememberUserToolName, rememberArgs), nil)
	if err != nil || res.IsError {
		t.Fatalf("RememberUser write: err=%v isError=%v content=%q", err, res.IsError, res.Content)
	}

	// Persistence to disk: the write is durable under the dir (storeA released its
	// lock after the op). The cross-PROCESS round-trip is then proven below by the
	// SEPARATE store the Service's Build opens over the SAME dir.
	if _, ok, _ := storeA.Recall(ctx, "user/comm-style"); !ok {
		t.Fatalf("RememberUser did not persist to the user-model store")
	}

	// --- Session B: a Service over the SAME user-model dir; turn 0 must carry the --
	// <user-model> fence with the saved fact.
	built, err := Build(ctx, Config{
		Workspace:    workspace,
		UseMock:      true,
		UserModelDir: userModelDir,
		NoSoul:       true, // isolate the user-model block from the soul
	})
	if err != nil {
		t.Fatalf("Build: %v", err)
	}
	defer built.Close()

	svc := built.Service
	sess, err := svc.CreateSession(ctx, workspace, session.ModeDefault, defaultLimits())
	if err != nil {
		t.Fatalf("CreateSession: %v", err)
	}
	run, err := svc.StartRun(ctx, sess.ID, "hello")
	if err != nil {
		t.Fatalf("StartRun: %v", err)
	}
	drainRun(run)

	got, err := svc.GetSession(ctx, sess.ID)
	if err != nil {
		t.Fatalf("GetSession: %v", err)
	}
	var found bool
	for _, m := range got.Conversation.Messages {
		if m.Role == session.RoleUser && strings.Contains(m.Text, "<user-model>") && strings.Contains(m.Text, "comm-style") {
			found = true
			break
		}
	}
	if !found {
		t.Fatalf("session turn-0 conversation is missing the <user-model> fence with the saved fact:\n%+v", messageTexts(got.Conversation.Messages))
	}
}

func messageTexts(msgs []session.Message) []string {
	out := make([]string, 0, len(msgs))
	for _, m := range msgs {
		out = append(out, string(m.Role)+": "+m.Text)
	}
	return out
}
