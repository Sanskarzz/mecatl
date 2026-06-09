package app

import (
	"context"
	"encoding/json"
	"iter"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"sync"
	"testing"

	"github.com/stacklok/mecatl/internal/adapter/forker"
	"github.com/stacklok/mecatl/internal/adapter/osfs"
	"github.com/stacklok/mecatl/internal/agent"
	"github.com/stacklok/mecatl/internal/port"
	"github.com/stacklok/mecatl/internal/session"
	"github.com/stacklok/mecatl/internal/tool"
)

// bashWriteProvider is a STATELESS scripted LLM (safe for the concurrent child
// runs Fork fans out): on the first turn of a branch it calls Bash to write a
// relative-path marker file; once a tool result is present it ends the turn with a
// summary. It decides from the request's own history, not a shared cursor.
type bashWriteProvider struct {
	command string
	marker  string
}

func (*bashWriteProvider) Capabilities() port.ProviderCapabilities {
	return port.ProviderCapabilities{}
}

func (p *bashWriteProvider) Stream(ctx context.Context, req port.LLMRequest) (iter.Seq2[port.Chunk, error], error) {
	hasToolResult := false
	for _, m := range req.Messages {
		if m.ToolResult != nil {
			hasToolResult = true
			break
		}
	}
	var chunks []port.Chunk
	if hasToolResult {
		chunks = []port.Chunk{
			{Kind: port.ChunkText, Text: "wrote " + p.marker},
			{Kind: port.ChunkUsage, Usage: &session.Usage{}},
			{Kind: port.ChunkDone, Stop: session.StopEndTurn},
		}
	} else {
		call := session.NewToolCall("bash1", "Bash", json.RawMessage(`{"command":`+strconvQuote(p.command)+`}`))
		chunks = []port.Chunk{
			{Kind: port.ChunkToolCall, ToolCall: &call},
			{Kind: port.ChunkUsage, Usage: &session.Usage{}},
			{Kind: port.ChunkDone, Stop: session.StopEndTurn},
		}
	}
	return func(yield func(port.Chunk, error) bool) {
		for _, c := range chunks {
			select {
			case <-ctx.Done():
				return
			default:
			}
			if !yield(c, nil) {
				return
			}
		}
	}, nil
}

func strconvQuote(s string) string {
	b, _ := json.Marshal(s)
	return string(b)
}

// recordingForker wraps the real osfs forker and records the child workspace roots
// it hands out, so the test can read the marker from the exact fork directory.
type recordingForker struct {
	inner  tool.WorkspaceForker
	mu     sync.Mutex
	childs []string
}

func (rf *recordingForker) Fork(ctx context.Context, base tool.Workspace, label string) (tool.Workspace, func() error, error) {
	child, cleanup, err := rf.inner.Fork(ctx, base, label)
	if err != nil {
		return nil, nil, err
	}
	rf.mu.Lock()
	rf.childs = append(rf.childs, child.Root())
	rf.mu.Unlock()
	return child, cleanup, nil
}

func (rf *recordingForker) roots() []string {
	rf.mu.Lock()
	defer rf.mu.Unlock()
	out := make([]string, len(rf.childs))
	copy(out, rf.childs)
	return out
}

// TestForkBashWritesIntoForkNotBase is the end-to-end isolation proof: a Fork
// MUTATING branch whose scripted turn runs Bash (`echo hi > marker.txt`) lands the
// marker in the branch's ISOLATED fork — NOT in the shared parent base. This is the
// behavior the workspace-aware-Bash fix exists for; before the fix the Bash runner
// was rooted at the parent base and the marker would have appeared THERE (the
// assertion at the end would fail if Bash were pointed back at the base).
func TestForkBashWritesIntoForkNotBase(t *testing.T) {
	base := t.TempDir()
	cfg := Config{Workspace: base, Model: "mock", Shell: "/bin/sh"}

	runner := buildCommandRunner(cfg)
	if runner == nil {
		t.Fatal("precondition: expected a non-nil command runner (Shell set)")
	}

	baseWS, err := osfs.NewWorkspace(base)
	if err != nil {
		t.Fatalf("base workspace: %v", err)
	}

	rf := &recordingForker{inner: forker.New(func(root string) (tool.Workspace, error) {
		return osfs.NewWorkspace(root)
	})}

	provider := &bashWriteProvider{command: "echo hi > marker.txt", marker: "marker.txt"}
	childEngine := buildParallelChildEngine(cfg, provider, runner)

	// join=first PRESERVES the winning branch's fork (cleanup not called), so the
	// marker survives for the assertion below.
	fork := agent.NewParallelTool(childEngine, rf)

	call := session.NewToolCall("c1", "Parallel",
		json.RawMessage(`{"tasks":["write the marker"],"join":"first"}`))
	res, err := fork.Execute(context.Background(), call, baseWS)
	if err != nil {
		t.Fatalf("Fork.Execute: %v", err)
	}
	if res.IsError {
		t.Fatalf("Fork result is an error: %s", res.Content)
	}

	roots := rf.roots()
	if len(roots) != 1 {
		t.Fatalf("expected exactly 1 forked child workspace, got %d: %v", len(roots), roots)
	}
	forkRoot := roots[0]

	// The marker MUST be in the fork.
	forkMarker := filepath.Join(forkRoot, "marker.txt")
	if _, err := os.Stat(forkMarker); err != nil {
		t.Errorf("marker.txt NOT found in the branch fork %q: %v (Bash did not run in the fork)", forkRoot, err)
	}

	// The marker MUST be ABSENT from the shared parent base — this is the isolation
	// guarantee. (If Bash were rooted at the base, this is exactly where it would
	// have landed, and this assertion would FAIL.)
	baseMarker := filepath.Join(base, "marker.txt")
	if _, err := os.Stat(baseMarker); !os.IsNotExist(err) {
		t.Errorf("marker.txt LEAKED into the shared parent base %q (Bash escaped the fork)", base)
	}
}

// TestForkGitCommitDoesNotTouchBaseRepo is the end-to-end git-isolation proof: a
// Fork MUTATING branch whose scripted turn runs `git commit` via Bash lands the
// commit in the branch's ISOLATED fork's OWN .git — NOT the shared base repo. This
// is the gap WithForceCopy closes: with the old worktree path the commit object/ref
// would have written into the base's shared .git. The composition root wires the
// Fork forker WithForceCopy, so this end-to-end build uses the same one. Skipped
// when git is unavailable.
func TestForkGitCommitDoesNotTouchBaseRepo(t *testing.T) {
	if _, err := exec.LookPath("git"); err != nil {
		t.Skip("git not available")
	}
	base := t.TempDir()
	initBaseGitRepo(t, base)
	cfg := Config{Workspace: base, Model: "mock", Shell: "/bin/sh"}

	runner := buildCommandRunner(cfg)
	if runner == nil {
		t.Fatal("precondition: expected a non-nil command runner (Shell set)")
	}

	baseHeadBefore := gitOut(t, base, "rev-parse", "HEAD")
	baseRefsBefore := gitOut(t, base, "show-ref")

	baseWS, err := osfs.NewWorkspace(base)
	if err != nil {
		t.Fatalf("base workspace: %v", err)
	}

	// Mirror the composition root: mutating Fork branches get FULLY isolated forks.
	rf := &recordingForker{inner: forker.New(func(root string) (tool.Workspace, error) {
		return osfs.NewWorkspace(root)
	}, forker.WithForceCopy())}

	// The branch writes a file then commits it — all inside its fork.
	provider := &bashWriteProvider{
		command: "echo branchwork > branch.txt && git add -A && git commit -m 'branch commit' && git update-ref refs/heads/sneaky HEAD",
		marker:  "branch.txt",
	}
	childEngine := buildParallelChildEngine(cfg, provider, runner)

	// join=first PRESERVES the winner's fork so we can inspect it.
	fork := agent.NewParallelTool(childEngine, rf)

	call := session.NewToolCall("c1", "Parallel",
		json.RawMessage(`{"tasks":["commit the work"],"join":"first"}`))
	res, err := fork.Execute(context.Background(), call, baseWS)
	if err != nil {
		t.Fatalf("Fork.Execute: %v", err)
	}
	if res.IsError {
		t.Fatalf("Fork result is an error: %s", res.Content)
	}

	roots := rf.roots()
	if len(roots) != 1 {
		t.Fatalf("expected exactly 1 forked child workspace, got %d: %v", len(roots), roots)
	}
	forkRoot := roots[0]

	// The commit landed in the FORK's own repo (its HEAD advanced past the base's).
	if forkHead := gitOut(t, forkRoot, "rev-parse", "HEAD"); forkHead == baseHeadBefore {
		t.Errorf("fork HEAD did not advance; the branch's git commit may not have run (head=%q)", forkHead)
	}

	// The base repo's .git MUST be UNCHANGED: same HEAD, same refs, no sneaky ref,
	// no committed file in the working tree.
	if got := gitOut(t, base, "rev-parse", "HEAD"); got != baseHeadBefore {
		t.Errorf("base HEAD changed: before=%q after=%q (fork commit escaped into base)", baseHeadBefore, got)
	}
	if got := gitOut(t, base, "show-ref"); got != baseRefsBefore {
		t.Errorf("base refs changed:\nbefore=%q\nafter=%q (fork ref write escaped into base)", baseRefsBefore, got)
	}
	if _, err := os.Stat(filepath.Join(base, "branch.txt")); !os.IsNotExist(err) {
		t.Errorf("fork's working-tree file leaked into base (err=%v)", err)
	}
}

func initBaseGitRepo(t *testing.T, dir string) {
	t.Helper()
	gitRun(t, dir, "init")
	gitRun(t, dir, "config", "user.email", "test@example.com")
	gitRun(t, dir, "config", "user.name", "Test")
	if err := os.WriteFile(filepath.Join(dir, "seed.txt"), []byte("seed\n"), 0o644); err != nil {
		t.Fatalf("seed: %v", err)
	}
	gitRun(t, dir, "add", "-A")
	gitRun(t, dir, "commit", "-m", "init")
}

func gitRun(t *testing.T, dir string, args ...string) {
	t.Helper()
	cmd := exec.Command("git", append([]string{"-C", dir}, args...)...)
	if out, err := cmd.CombinedOutput(); err != nil {
		t.Fatalf("git %v: %v\n%s", args, err, out)
	}
}

func gitOut(t *testing.T, dir string, args ...string) string {
	t.Helper()
	cmd := exec.Command("git", append([]string{"-C", dir}, args...)...)
	out, err := cmd.Output()
	if err != nil {
		t.Fatalf("git %v: %v", args, err)
	}
	return strings.TrimSpace(string(out))
}
