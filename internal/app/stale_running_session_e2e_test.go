package app

import (
	"context"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/stacklok/mecatl/engine/adapter/mockllm"
	"github.com/stacklok/mecatl/engine/port"
	"github.com/stacklok/mecatl/engine/session"
	"github.com/stacklok/mecatl/internal/adapter/store/jsonlstore"
)

// TestBuildSettlesStaleRunningSnapshotAcrossRestart is the issue #475
// falsifiable end-to-end gate through the FULL composition (app.Build →
// server.Service), offline, over two SEPARATE Build calls — mirroring
// TestApproveAfterRestartE2E's two-Build shape. It proves the actual reported
// bug is closed for the population it was confirmed against: a
// subagent-*-prefixed child session whose crash left a trailing tool_use with
// no matching result and a persisted "state":"running" snapshot that nobody
// ever prompts or resumes again.
//
//  1. built1 ("the crashed process"): seed a StateRunning snapshot carrying a
//     dangling tool_use DIRECTLY into the durable jsonlstore — bypassing
//     CreateSession/StartRunContent entirely, since a normal prompt flow
//     through Step 3's funnel repair would mask the population this test
//     targets (a child id nothing ever re-opens). Age the snapshot file's
//     mtime past staleSessionWindow, then close built1 without ever starting
//     a run for this id — genuinely untouched, dying mid-crash.
//  2. built2: a brand-new Build over the SAME store dir (the restart). Drive
//     one deterministic sweep pass (the exact function
//     startStaleSessionReconcile's goroutine calls at startup) and assert:
//     ListSessions reports the id settled (no longer "running"), and the
//     reloaded conversation passes ValidateToolPairing (no dangling
//     tool_use) — with NO prompt, resume, or manual repair call in between.
//
// Mutation-verified: reverting issue #475 Steps 2-4 (SessionStale/
// SettleIfStale/the composition sweep) to a pre-fix worktree makes this test
// fail to COMPILE (sweepStaleSessions/SessionStale/SettleIfStale/
// LeaseSweepDisabled do not exist yet at that point in the branch's history)
// — see the commit message for the exact commit and verification transcript.
func TestBuildSettlesStaleRunningSnapshotAcrossRestart(t *testing.T) {
	ctx := context.Background()
	storeDir := t.TempDir()
	memoryDir := t.TempDir()
	workspace := t.TempDir()

	baseCfg := func() Config {
		return Config{
			Workspace:   workspace,
			NoSoul:      true,
			StoreDir:    storeDir,
			MemoryDir:   memoryDir,
			envDetector: fakeEnv(map[string]string{"OPENAI_API_KEY": "sk-openai"}),
			providerConstructor: func(_ Config, _, _, _ string) port.LLMProvider {
				return mockllm.New(mockllm.TextTurn("unused"))
			},
			liveModelHTTPClient: offlineHTTPClient(),
		}
	}

	const childID session.SessionID = "subagent-crash-orphan-475"

	// built1: the crashed process. Never start a run for childID — seed the
	// orphaned snapshot straight into the store, exactly as if built1's own
	// (never-invoked) store.Save had been the crash's last write.
	cfg1 := baseCfg()
	built1, err := Build(ctx, cfg1)
	if err != nil {
		t.Fatalf("Build #1: %v", err)
	}

	orphan := crashOrphanedSessionFixture(t, childID, time.Now())
	seedStore, err := jsonlstore.New(storeDir)
	if err != nil {
		built1.Close()
		t.Fatalf("open store for direct seed: %v", err)
	}
	if err := seedStore.Save(ctx, orphan); err != nil {
		built1.Close()
		t.Fatalf("seed crash-orphaned snapshot: %v", err)
	}

	// Age the snapshot file's mtime past staleSessionWindow — jsonlstore
	// derives ModifiedAt from the file mtime, and Service.SessionStale gates
	// on that age BEFORE any liveness/lease signal (issue #475 Step 2).
	matches, err := filepath.Glob(filepath.Join(storeDir, "*.session.jsonl"))
	if err != nil || len(matches) != 1 {
		built1.Close()
		t.Fatalf("find seeded session file: matches=%v err=%v", matches, err)
	}
	old := time.Now().Add(-2 * time.Hour)
	if err := os.Chtimes(matches[0], old, old); err != nil {
		built1.Close()
		t.Fatalf("age session file mtime: %v", err)
	}

	built1.Close() // process death: childID was never touched by any run.

	// built2: a brand-new Build over the SAME store — the restart.
	cfg2 := baseCfg()
	built2, err := Build(ctx, cfg2)
	if err != nil {
		t.Fatalf("Build #2: %v", err)
	}
	defer built2.Close()

	// Drive one deterministic sweep pass — the exact function
	// startStaleSessionReconcile's goroutine invokes at startup, called
	// synchronously here instead of racing its background ticker (mirroring
	// session_reconcile_test.go's own convention within this package).
	sweepStaleSessions(ctx, built2.Service, port.NopDiagnostics{})

	rows, err := built2.Service.ListSessions(ctx)
	if err != nil {
		t.Fatalf("ListSessions: %v", err)
	}
	var gotState string
	found := false
	for _, row := range rows {
		if row.SessionID == string(childID) {
			found = true
			gotState = row.State
			break
		}
	}
	if !found {
		t.Fatalf("ListSessions never reported %q", childID)
	}
	if gotState == string(session.StateRunning) {
		t.Fatalf("ListSessions state for %q = %q, want settled (no prompt/resume ever happened)", childID, gotState)
	}
	if gotState != string(session.StateIdle) {
		t.Fatalf("ListSessions state for %q = %q, want %q", childID, gotState, session.StateIdle)
	}

	reloaded, err := built2.Service.GetSession(ctx, childID)
	if err != nil {
		t.Fatalf("GetSession after settle: %v", err)
	}
	if err := session.ValidateToolPairing(reloaded.Conversation.Messages); err != nil {
		t.Fatalf("tool pairing invalid after settle: %v", err)
	}
}
