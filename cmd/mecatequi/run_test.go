package main

import (
	"bufio"
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/stacklok/mecatl/engine/adapter/memfs"
	"github.com/stacklok/mecatl/engine/adapter/memstore"
	"github.com/stacklok/mecatl/engine/adapter/mockllm"
	"github.com/stacklok/mecatl/engine/adapter/permpolicy"
	"github.com/stacklok/mecatl/engine/agent"
	"github.com/stacklok/mecatl/engine/port"
	"github.com/stacklok/mecatl/engine/session"
	"github.com/stacklok/mecatl/engine/tool"
	"github.com/stacklok/mecatl/internal/adapter/osfs"
	"github.com/stacklok/mecatl/internal/adapter/server"
	"github.com/stacklok/mecatl/internal/adapter/tools"
	"github.com/stacklok/mecatl/internal/app"
)

// initTestRepo creates a hermetic git repo in dir with one commit. It mirrors the
// recipe in internal/app/agency_test.go so the gitDiffPatch path has a real repo to
// diff against, with a deterministic, leak-free identity.
func initTestRepo(t *testing.T, dir string) {
	t.Helper()
	run := func(args ...string) {
		cmd := exec.Command("git", args...)
		cmd.Dir = dir
		cmd.Env = append(os.Environ(),
			"GIT_CONFIG_NOSYSTEM=1",
			"GIT_CONFIG_GLOBAL=/dev/null",
			"GIT_AUTHOR_NAME=t", "GIT_AUTHOR_EMAIL=t@t",
			"GIT_COMMITTER_NAME=t", "GIT_COMMITTER_EMAIL=t@t",
		)
		if out, err := cmd.CombinedOutput(); err != nil {
			t.Fatalf("git %v: %v\n%s", args, err, out)
		}
	}
	run("init", "-q", "-b", "main")
	if err := os.WriteFile(filepath.Join(dir, "f.txt"), []byte("hello\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	run("add", "f.txt")
	run("commit", "-q", "-m", "initial commit")
}

// TestRunEndToEndMockProvider is the model-facing e2e: a full app.Build over a
// hermetic git repo with UseMock, then run() against the real Service. It asserts the
// run completes cleanly (end_turn), leaves no diff, carries a usage struct, writes a
// non-empty human log, and that the captured event slice carries an EvResult.
func TestRunEndToEndMockProvider(t *testing.T) {
	ctx := context.Background()
	repo := t.TempDir()
	initTestRepo(t, repo)

	built, err := app.Build(ctx, app.Config{
		Workspace:   repo,
		UseMock:     true,
		NoSoul:      true,
		Diagnostics: port.NopDiagnostics{},
	})
	if err != nil {
		t.Fatalf("app.Build: %v", err)
	}
	defer built.Close()

	var human bytes.Buffer
	outcome, err := run(ctx, built.Service, repo, "summarise the repo", &human)
	if err != nil {
		t.Fatalf("run: %v", err)
	}
	sum := outcome.Summary

	if sum.SchemaVersion != SummarySchemaVersion {
		t.Errorf("SchemaVersion = %d, want %d", sum.SchemaVersion, SummarySchemaVersion)
	}
	if sum.StopReason != string(session.StopEndTurn) {
		t.Errorf("StopReason = %q, want end_turn", sum.StopReason)
	}
	if sum.Error != "" {
		t.Errorf("clean run must carry no Error; got %q", sum.Error)
	}
	if outcome.NoApprover {
		t.Error("a clean mock run must not report NoApprover")
	}

	// Compute the diff like main does — the mock makes no tool calls, so the tree is
	// untouched.
	patch, nonEmpty, derr := gitDiffPatch(ctx, repo)
	if derr != nil {
		t.Fatalf("gitDiffPatch: %v", derr)
	}
	if nonEmpty || len(strings.TrimSpace(string(patch))) != 0 {
		t.Errorf("mock run made no edits; want empty diff, got %q", patch)
	}

	if human.Len() == 0 {
		t.Error("human log must be non-empty")
	}

	// The captured events must include a terminal EvResult (the durable-log source).
	if countResults(outcome.Events) != 1 {
		t.Errorf("want exactly one EvResult in the captured stream, got %d", countResults(outcome.Events))
	}

	// And the durable JSONL log must serialise that EvResult on its own line, and EVERY
	// line must decode.
	var jsonl bytes.Buffer
	if werr := writeDurableLog(&jsonl, outcome.Events); werr != nil {
		t.Fatalf("writeDurableLog: %v", werr)
	}
	if n := jsonlResultCount(t, jsonl.Bytes()); n != 1 {
		t.Errorf("durable JSONL log: want exactly 1 EvResult line, got %d\n%s", n, jsonl.String())
	}
}

// TestRunUsageAndFinalTextFaithfullyCopied proves the terminal EvResult's usage and
// final text are copied verbatim into the Summary (the honesty invariant on the
// success path): a scripted text turn carrying explicit usage shows up in the Summary.
func TestRunUsageAndFinalTextFaithfullyCopied(t *testing.T) {
	usage := session.Usage{InputTokens: 120, OutputTokens: 30, CacheReadTokens: 90}
	turn := mockllm.ChunksTurn(
		mockllm.TextChunk("the deliverable answer"),
		port.Chunk{Kind: port.ChunkUsage, Usage: &usage},
		port.Chunk{Kind: port.ChunkDone, Stop: session.StopEndTurn},
	)
	svc := scriptedService(t, nil, nil, turn)

	var human bytes.Buffer
	outcome, err := run(context.Background(), svc, "/ws", "answer", &human)
	if err != nil {
		t.Fatalf("run: %v", err)
	}
	sum := outcome.Summary

	if sum.FinalText != "the deliverable answer" {
		t.Errorf("FinalText = %q, want the terminal assistant text", sum.FinalText)
	}
	if sum.Usage.InputTokens != 120 || sum.Usage.OutputTokens != 30 || sum.Usage.CacheReadTokens != 90 {
		t.Errorf("usage not faithfully copied: %+v", sum.Usage)
	}
	if sum.Usage.TotalTokens != 150 {
		t.Errorf("TotalTokens = %d, want 150 (input+output, cache excluded)", sum.Usage.TotalTokens)
	}
}

// TestExitCodeCleanIsZero pins the exit-code contract: every CLEAN terminal maps to 0,
// error/cancelled/none map to 1.
func TestExitCodeCleanIsZero(t *testing.T) {
	clean := []session.StopReason{
		session.StopEndTurn,
		session.StopNoProgress,
		session.StopBudget,
		session.StopMaxTurns,
		session.StopMaxToolCalls,
		session.StopMaxConsecutiveFailures,
		session.StopStructuredOutput,
	}
	for _, s := range clean {
		if got := exitCode(Summary{StopReason: string(s)}); got != 0 {
			t.Errorf("exitCode(%q) = %d, want 0 (clean terminal)", s, got)
		}
	}
	fail := []session.StopReason{session.StopError, session.StopCancelled, session.StopNone}
	for _, s := range fail {
		if got := exitCode(Summary{StopReason: string(s)}); got != 1 {
			t.Errorf("exitCode(%q) = %d, want 1", s, got)
		}
	}
}

// scriptedService builds a real server.Service over a SCRIPTED mockllm provider — the
// adversarial-test seam (app.Build's canned mock cannot be scripted). It mirrors the
// construction in internal/adapter/server/budget_test.go. extraTools are registered
// into the catalog (e.g. a real Write tool); workspaces, when non-nil, overrides the
// default memfs factory (e.g. an osfs factory for a real-FS diff test).
func scriptedService(t *testing.T, extraTools []tool.Tool, workspaces func(string) tool.Workspace, turns ...mockllm.Turn) *server.Service {
	t.Helper()
	llm := mockllm.New(turns...)
	cat := tool.NewCatalog()
	for _, tl := range extraTools {
		cat.MustRegister(tl)
	}
	engine := agent.NewEngine(agent.Deps{
		LLM:     llm,
		Catalog: cat,
		Policy:  permpolicy.NewPolicy(permpolicy.AllowAllFloorRules(), nil),
		Model:   "test-model",
	})
	if workspaces == nil {
		workspaces = func(root string) tool.Workspace { return memfs.NewWorkspace(root) }
	}
	svc, err := server.NewService(server.Config{
		Engine:              engine,
		Store:               memstore.New(),
		Workspaces:          workspaces,
		Now:                 func() time.Time { return time.Unix(0, 0) },
		DefaultCapabilities: llm.Capabilities(),
	})
	if err != nil {
		t.Fatalf("new service: %v", err)
	}
	return svc
}

// TestRunNonEmptyDiffWhenAgentWritesFile drives a scripted Write tool call over a REAL
// osfs workspace inside a temp git repo and asserts the run's diff is non-empty and the
// patch body carries the new content. This is the honesty invariant on the
// files-CHANGED path (the empty-diff path is covered by the mock e2e).
func TestRunNonEmptyDiffWhenAgentWritesFile(t *testing.T) {
	repo := t.TempDir()
	initTestRepo(t, repo)

	// Read the tracked f.txt first (satisfying Write's read-before-overwrite
	// invariant), then overwrite it — so `git diff HEAD` shows a tracked-file
	// modification and the patch BODY carries the new content.
	readCall := session.NewToolCall("r1", "Read", []byte(`{"path":"f.txt"}`))
	writeCall := session.NewToolCall("w1", "Write", []byte(`{"path":"f.txt","content":"CHANGED BY THE AGENT\n"}`))
	svc := scriptedService(t,
		[]tool.Tool{tools.ReadTool{}, tools.WriteTool{}},
		func(root string) tool.Workspace {
			ws, err := osfs.NewWorkspace(root)
			if err != nil {
				t.Fatalf("osfs.NewWorkspace(%q): %v", root, err)
			}
			return ws
		},
		mockllm.ToolCallTurn(readCall),
		mockllm.ToolCallTurn(writeCall),
		mockllm.TextTurn("done"),
	)

	var human bytes.Buffer
	outcome, err := run(context.Background(), svc, repo, "edit f.txt", &human)
	if err != nil {
		t.Fatalf("run: %v", err)
	}
	if outcome.Summary.StopReason != string(session.StopEndTurn) {
		t.Fatalf("StopReason = %q, want end_turn (human log below)\n%s", outcome.Summary.StopReason, human.String())
	}

	patch, nonEmpty, derr := gitDiffPatch(context.Background(), repo)
	if derr != nil {
		t.Fatalf("gitDiffPatch: %v", derr)
	}
	if !nonEmpty {
		t.Fatalf("the agent edited a tracked file; NonEmptyDiff must be true\nhuman log:\n%s", human.String())
	}
	if !strings.Contains(string(patch), "CHANGED BY THE AGENT") {
		t.Errorf("patch must carry the new content; got:\n%s", patch)
	}
}

// TestRunAdversarialError drives a scripted provider that fails mid-stream and asserts
// the Summary HONESTLY reports the error terminal (StopError, non-empty Error) and that
// exitCode maps it to 1. It proves the summary is not hardcoded optimism.
func TestRunAdversarialError(t *testing.T) {
	svc := scriptedService(t, nil, nil, mockllm.ErrorTurn(errors.New("upstream 503 exhausted retries")))

	var human bytes.Buffer
	outcome, err := run(context.Background(), svc, "/ws", "do the thing", &human)
	if err != nil {
		t.Fatalf("run (a model error is reported in the Summary, NOT as a setup error): %v", err)
	}
	sum := outcome.Summary

	if sum.StopReason != string(session.StopError) {
		t.Errorf("StopReason = %q, want error", sum.StopReason)
	}
	if sum.Error == "" {
		t.Error("an error terminal must carry a non-empty Error in the Summary")
	}
	if got := exitCode(sum); got != 1 {
		t.Errorf("exitCode = %d, want 1 for an error terminal", got)
	}
	// The error terminal is still recorded in the durable log.
	if len(outcome.Events) == 0 {
		t.Error("an errored run must still capture events")
	}
}

// TestRunAdversarialNoProgress drives a scripted provider that emits only empty
// (no-text, no-tool) turns until the loop's no-progress budget exhausts. It asserts the
// Summary HONESTLY reports no_progress (NOT end_turn), with no diff, and exitCode 0
// (a clean terminal). This is the "the summary must not claim success" guard.
func TestRunAdversarialNoProgress(t *testing.T) {
	// Enough empty turns to exhaust the default no-progress nudge budget.
	turns := make([]mockllm.Turn, 0, 8)
	for i := 0; i < 8; i++ {
		turns = append(turns, mockllm.EmptyTurn())
	}
	svc := scriptedService(t, nil, nil, turns...)

	var human bytes.Buffer
	outcome, err := run(context.Background(), svc, "/ws", "do nothing useful", &human)
	if err != nil {
		t.Fatalf("run: %v", err)
	}
	sum := outcome.Summary

	if sum.StopReason != string(session.StopNoProgress) {
		t.Errorf("StopReason = %q, want no_progress (HONEST, not end_turn)", sum.StopReason)
	}
	if sum.StopReason == string(session.StopEndTurn) {
		t.Error("a no-progress run must NOT be relabelled end_turn (the honesty invariant)")
	}
	if sum.NonEmptyDiff {
		t.Error("a no-progress run made no edits; NonEmptyDiff must be false")
	}
	if got := exitCode(sum); got != 0 {
		t.Errorf("exitCode = %d, want 0 (no_progress is a CLEAN terminal)", got)
	}
}

// TestRunAdversarialCancelled drives a scripted provider that reports a cancelled
// terminal and asserts the Summary HONESTLY reports cancelled with exitCode 1 — the
// real loop drive, not just the unit table.
func TestRunAdversarialCancelled(t *testing.T) {
	svc := scriptedService(t, nil, nil, mockllm.EmptyTurnWithStop(session.StopCancelled))

	var human bytes.Buffer
	outcome, err := run(context.Background(), svc, "/ws", "abandon", &human)
	if err != nil {
		t.Fatalf("run: %v", err)
	}
	if outcome.Summary.StopReason != string(session.StopCancelled) {
		t.Errorf("StopReason = %q, want cancelled", outcome.Summary.StopReason)
	}
	if got := exitCode(outcome.Summary); got != 1 {
		t.Errorf("exitCode = %d, want 1 for a cancelled terminal", got)
	}
}

// TestRunCancelOnMainAskBoundsAndExits is the headless main-ask trap: a strict-posture
// engine asks on a mutate, but no approver is attached. The run() loop must detect the
// parent-own EvPermissionAsk, cancel, drain-to-close (NOT hang), set NoApprover, and
// surface a non-zero exit. A real Write tool over a memfs workspace exercises the gate;
// the policy is the BUILT-IN floor (which ASKS on a mutate), not allow-all.
func TestRunCancelOnMainAskBoundsAndExits(t *testing.T) {
	writeCall := session.NewToolCall("w1", "Write", []byte(`{"path":"x.txt","content":"hi\n"}`))
	llm := mockllm.New(mockllm.ToolCallTurn(writeCall), mockllm.TextTurn("done"))
	cat := tool.NewCatalog()
	cat.MustRegister(tools.WriteTool{})
	// The built-in default policy (no allow rules): a mutate (Write) floors to ASK.
	engine := agent.NewEngine(agent.Deps{
		LLM:     llm,
		Catalog: cat,
		Policy:  permpolicy.NewPolicy(nil, nil),
		Model:   "test-model",
	})
	svc, err := server.NewService(server.Config{
		Engine:              engine,
		Store:               memstore.New(),
		Workspaces:          func(root string) tool.Workspace { return memfs.NewWorkspace(root) },
		Now:                 func() time.Time { return time.Unix(0, 0) },
		DefaultCapabilities: llm.Capabilities(),
	})
	if err != nil {
		t.Fatalf("new service: %v", err)
	}

	// A timeout on the test context is the backstop: if the loop hangs (the bug this
	// guards), the test fails by deadline instead of blocking forever.
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()

	done := make(chan struct{})
	var outcome runOutcome
	var runErr error
	go func() {
		var human bytes.Buffer
		outcome, runErr = run(ctx, svc, "/ws", "write a file", &human)
		close(done)
	}()
	select {
	case <-done:
	case <-ctx.Done():
		t.Fatal("run() did not return after a main-engine ask with no approver — it hung (cancel-on-ask regression)")
	}

	if runErr != nil {
		t.Fatalf("run: %v", runErr)
	}
	if !outcome.NoApprover {
		t.Error("a main-engine ask with no approver must set NoApprover")
	}
	if got := exitCode(outcome.Summary); got != 1 {
		t.Errorf("exitCode = %d, want 1 (no-approver cancel)", got)
	}
}

// TestRealMainMissingPromptIsSetupFailure proves the SETUP-failure exit code (2): a
// missing prompt fails flag parsing before any build.
func TestRealMainMissingPromptIsSetupFailure(t *testing.T) {
	var out, errBuf bytes.Buffer
	if code := realMain(nil, &out, &errBuf); code != 2 {
		t.Errorf("realMain(no prompt) = %d, want 2 (setup failure)", code)
	}
}

// TestRealMainNonGitWorkspaceIsSetupFailure proves a non-git --workspace fails at setup
// (exit 2) — the validateWorkspaceRepo guard, exercised through the whole main path.
func TestRealMainNonGitWorkspaceIsSetupFailure(t *testing.T) {
	notARepo := t.TempDir() // a bare temp dir, no `git init`
	var out, errBuf bytes.Buffer
	code := realMain([]string{"--prompt", "x", "--mock", "--workspace", notARepo}, &out, &errBuf)
	if code != 2 {
		t.Errorf("realMain(non-git workspace) = %d, want 2\nstderr=%s", code, errBuf.String())
	}
	if !strings.Contains(errBuf.String(), "not a git repository") {
		t.Errorf("stderr should explain the non-git workspace; got %q", errBuf.String())
	}
}

// TestRealMainMockHappyPath drives the WHOLE main path (parse -> app.Build(UseMock) ->
// run -> emit diff/summary/events -> exit) over a hermetic git repo, asserting exit 0
// and that the summary JSON written to --out-summary parses with end_turn and an empty
// diff. It also confirms the durable JSONL log file is created and carries an EvResult.
func TestRealMainMockHappyPath(t *testing.T) {
	repo := t.TempDir()
	initTestRepo(t, repo)
	outDir := t.TempDir()
	summaryPath := filepath.Join(outDir, "summary.json")
	diffPath := filepath.Join(outDir, "diff.patch")
	eventsPath := filepath.Join(outDir, "events.jsonl")

	var stdout, stderr bytes.Buffer
	code := realMain([]string{
		"--prompt", "summarise the repo",
		"--mock",
		"--workspace", repo,
		"--out-summary", summaryPath,
		"--out-diff", diffPath,
		"--out-events", eventsPath,
	}, &stdout, &stderr)
	if code != 0 {
		t.Fatalf("realMain = %d, want 0\nstderr=%s", code, stderr.String())
	}

	raw, err := os.ReadFile(summaryPath)
	if err != nil {
		t.Fatalf("read summary: %v", err)
	}
	var sum Summary
	if uerr := json.Unmarshal(raw, &sum); uerr != nil {
		t.Fatalf("summary JSON: %v\n%s", uerr, raw)
	}
	if sum.SchemaVersion != SummarySchemaVersion {
		t.Errorf("summary SchemaVersion = %d, want %d", sum.SchemaVersion, SummarySchemaVersion)
	}
	if sum.StopReason != string(session.StopEndTurn) {
		t.Errorf("summary StopReason = %q, want end_turn", sum.StopReason)
	}
	if sum.NonEmptyDiff {
		t.Error("mock run made no edits; NonEmptyDiff must be false")
	}
	if sum.SessionID == "" {
		t.Error("summary must carry the session id")
	}

	// The final stderr verdict line must reflect the honest outcome.
	if !strings.Contains(stderr.String(), "stop=end_turn") {
		t.Errorf("stderr verdict line missing; got %q", stderr.String())
	}

	evData, err := os.ReadFile(eventsPath)
	if err != nil {
		t.Fatalf("read events log: %v", err)
	}
	if jsonlResultCount(t, evData) != 1 {
		t.Errorf("durable events log: want exactly 1 EvResult line\n%s", evData)
	}
}

// TestRealMainCollidingOutputsIsSetupFailure proves the output-collision guard: two
// outputs on stdout fail at setup (exit 2).
func TestRealMainCollidingOutputsIsSetupFailure(t *testing.T) {
	repo := t.TempDir()
	initTestRepo(t, repo)
	var out, errBuf bytes.Buffer
	code := realMain([]string{
		"--prompt", "x", "--mock", "--workspace", repo,
		"--out-summary", "-", "--out-diff", "-",
	}, &out, &errBuf)
	if code != 2 {
		t.Errorf("realMain(both stdout) = %d, want 2\nstderr=%s", code, errBuf.String())
	}
	if !strings.Contains(errBuf.String(), "both write to stdout") {
		t.Errorf("stderr should name the stdout collision; got %q", errBuf.String())
	}
}

// TestGitDiffPatchUntrackedFile proves a NEW untracked file makes nonEmpty true even
// though `git diff HEAD` patch bytes are empty (the status-porcelain fold).
func TestGitDiffPatchUntrackedFile(t *testing.T) {
	repo := t.TempDir()
	initTestRepo(t, repo)
	if err := os.WriteFile(filepath.Join(repo, "new.txt"), []byte("brand new\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	patch, nonEmpty, err := gitDiffPatch(context.Background(), repo)
	if err != nil {
		t.Fatalf("gitDiffPatch: %v", err)
	}
	if strings.TrimSpace(string(patch)) != "" {
		t.Errorf("git diff HEAD should be empty for an untracked file; got %q", patch)
	}
	if !nonEmpty {
		t.Error("an untracked file must fold into nonEmpty=true")
	}
}

// TestGitDiffPatchNotAGitRepo proves a non-git workspace is an error from gitDiffPatch.
func TestGitDiffPatchNotAGitRepo(t *testing.T) {
	if _, _, err := gitDiffPatch(context.Background(), t.TempDir()); err == nil {
		t.Fatal("gitDiffPatch(non-repo): want error, got nil")
	}
}

// TestRunUnknownToolTurnCleanTerminal proves an unknown-tool call does not panic and the
// run reaches a clean terminal (the loop opens a card then records an error result and
// continues).
func TestRunUnknownToolTurnCleanTerminal(t *testing.T) {
	svc := scriptedService(t, nil, nil,
		mockllm.ToolCallTurn(session.NewToolCall("u1", "Nonexistent", []byte(`{}`))),
		mockllm.TextTurn("recovered and done"),
	)
	var human bytes.Buffer
	outcome, err := run(context.Background(), svc, "/ws", "call a bad tool", &human)
	if err != nil {
		t.Fatalf("run: %v", err)
	}
	if got := exitCode(outcome.Summary); got != 0 {
		t.Errorf("an unknown-tool run that recovers must end clean; exitCode=%d stop=%q", got, outcome.Summary.StopReason)
	}
}

// countResults counts EvResult events in a captured stream.
func countResults(events []session.Event) int {
	n := 0
	for _, ev := range events {
		if ev.Type == session.EvResult {
			n++
		}
	}
	return n
}

// jsonlResultCount decodes EVERY JSONL line (asserting each is valid) and returns the
// number whose Type is EvResult.
func jsonlResultCount(t *testing.T, data []byte) int {
	t.Helper()
	sc := bufio.NewScanner(bytes.NewReader(data))
	n := 0
	for sc.Scan() {
		var ev session.Event
		if err := json.Unmarshal(sc.Bytes(), &ev); err != nil {
			t.Fatalf("decode JSONL line: %v\nline=%s", err, sc.Text())
		}
		if ev.Type == session.EvResult {
			n++
		}
	}
	if err := sc.Err(); err != nil {
		t.Fatalf("scan JSONL: %v", err)
	}
	return n
}
