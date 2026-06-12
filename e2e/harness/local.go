//go:build e2e

package harness

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"errors"
	"fmt"
	"net"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"syscall"
	"time"

	"github.com/stacklok/mecatl/cmd/mecatui/client"
)

// Local spawns ./bin/mecated as a subprocess with fully ephemeral state dirs
// under <repo>/.scratch/e2e-<timestamp>-<rand>/ (HOME, XDG_*, workspace, store, memory,
// user-model, soul, skills fixtures) and loopback TCP listeners on
// harness-picked free ports. Its combined stdout+stderr is captured to the
// artifact dir (mecated.log) — the suite's failure reports and the
// soul-diagnostic spec read it.
//
// LINUX-ONLY: the spawn sets SysProcAttr.Pdeathsig (SIGTERM on parent death),
// so a hard-killed test runner (go test panic, SIGKILL, OOM) takes mecated
// with it instead of orphaning it. Pdeathsig exists only on Linux; the live
// e2e harness is deliberately Linux-only because of it.
//
// mecated has no UNIX-socket listen mode (the TUI's embedded server wires the
// in-process service over a UDS itself; the BINARY listens TCP only — verified
// in cmd/mecated/main.go serve()), so the harness mirrors the daemon reality:
// loopback TCP on free ports. The gRPC/HTTP/metrics ports are picked by binding
// :0, reading the port, and closing — racy in principle, loopback-private in
// practice.
type Local struct {
	Root string // .scratch/e2e-<timestamp>-<rand>

	grpcAddr    string
	metricsAddr string

	cmd     *exec.Cmd
	logPath string // combined stdout+stderr capture (mecated.log)
	logFile *os.File

	// exited is closed by the Wait goroutine started at spawn; after the close,
	// cmd.ProcessState is set. It is the single Wait owner — waitReady's
	// exited-early fast-fail and Close's bounded shutdown both select on it.
	exited chan struct{}

	cli *client.Client
}

// NewLocal builds the fixture tree, spawns mecated, waits for readiness, and
// returns the connected target.
func NewLocal() (*Local, error) {
	root, err := newScratchRoot()
	if err != nil {
		return nil, err
	}
	l := &Local{Root: root}
	if err := l.start(); err != nil {
		_ = l.Close()
		return nil, err
	}
	return l, nil
}

// newScratchRoot creates <repo>/.scratch/e2e-<timestamp>-<rand> (the repo-local scratch
// area; never /tmp — house rule).
func newScratchRoot() (string, error) {
	repo, err := RepoRoot()
	if err != nil {
		return "", err
	}
	var b [4]byte
	if _, err := rand.Read(b[:]); err != nil {
		return "", err
	}
	root := filepath.Join(repo, ".scratch", "e2e-"+time.Now().Format("20060102-150405")+"-"+hex.EncodeToString(b[:]))
	if err := os.MkdirAll(root, 0o755); err != nil {
		return "", err
	}
	return root, nil
}

// RepoRoot walks up from the working directory to the directory containing
// go.mod (go test runs with cwd = the package dir, e2e/).
func RepoRoot() (string, error) {
	dir, err := os.Getwd()
	if err != nil {
		return "", err
	}
	for {
		if _, err := os.Stat(filepath.Join(dir, "go.mod")); err == nil {
			return dir, nil
		}
		parent := filepath.Dir(dir)
		if parent == dir {
			return "", errors.New("could not locate the repo root (no go.mod above the test working dir)")
		}
		dir = parent
	}
}

func (l *Local) dir(parts ...string) string {
	return filepath.Join(append([]string{l.Root}, parts...)...)
}

// start writes fixtures, spawns the daemon, and waits for readiness.
func (l *Local) start() error {
	repo, err := RepoRoot()
	if err != nil {
		return err
	}
	bin := filepath.Join(repo, "bin", "mecated")
	if _, err := os.Stat(bin); err != nil {
		return fmt.Errorf("bin/mecated not found (%v): run `task build` first (task e2e depends on it)", err)
	}

	for _, d := range []string{"home", "xdg-config", "xdg-state", "xdg-cache", "workspace", "store", "memory", "usermodel", "soul", "artifacts"} {
		if err := os.MkdirAll(l.dir(d), 0o755); err != nil {
			return err
		}
	}
	if err := WriteFixtures(repo, l.Root); err != nil {
		return fmt.Errorf("write fixtures: %w", err)
	}
	if err := initWorkspaceGit(l.dir("workspace")); err != nil {
		return fmt.Errorf("git-init workspace: %w", err)
	}

	grpcPort, err := freePort()
	if err != nil {
		return err
	}
	httpPort, err := freePort()
	if err != nil {
		return err
	}
	metricsPort, err := freePort()
	if err != nil {
		return err
	}
	l.grpcAddr = "127.0.0.1:" + strconv.Itoa(grpcPort)
	l.metricsAddr = "127.0.0.1:" + strconv.Itoa(metricsPort)

	args := []string{
		"--grpc-addr", l.grpcAddr,
		"--http-addr", "127.0.0.1:" + strconv.Itoa(httpPort),
		"--metrics-addr", l.metricsAddr,
		"--workspace", l.dir("workspace"),
		"--model", DefaultModel(),
		"--store-dir", l.dir("store"),
		"--memory-dir", l.dir("memory"),
		"--user-model-dir", l.dir("usermodel"),
		"--soul-file", l.dir("soul", "soul.md"),
		// Skills come from the conventional locations under the FAKE HOME /
		// workspace (fixtures.go laid them out): ~/.claude/skills (user-global
		// lane) and <workspace>/.claude/skills (project lane, trust-gated).
		"--skills-conventional",
		// Honour the project tier (project skills + any project allows) — the
		// workspace is harness-authored, so it is trusted by construction.
		"--trust-project",
		// CLI-scope permission config: allows for the ask-floor tools the
		// scenarios exercise (Skill/Parallel/Team). Everything else keeps the
		// production posture; the driver auto-DENIES unexpected asks.
		"--permission-config", l.dir("permissions.yaml"),
		// Budgets: the shared runaway brakes. Reality check (run 1): a single
		// turn's input with the full catalog is ~5-6k tokens, so the originally
		// planned 4000 tripped at the FIRST turn boundary and forced every
		// multi-turn scenario to stop=budget. 20k/60k keep the brake while
		// letting the 2-3-turn scenarios end naturally.
		"--max-run-tokens", envOr("MECATL_E2E_MAX_RUN_TOKENS", "20000"),
		"--max-team-tokens", envOr("MECATL_E2E_MAX_TEAM_TOKENS", "60000"),
		// Hermeticity: never adopt MCP servers from the developer's running
		// ToolHive workloads.
		"--toolhive=false",
	}

	// One combined stdout+stderr capture — mecated logs to stderr, but stdout
	// is folded in too so nothing the daemon prints is lost.
	l.logPath = l.dir("artifacts", "mecated.log")
	l.logFile, err = os.Create(l.logPath)
	if err != nil {
		return err
	}

	cmd := exec.Command(bin, args...)
	cmd.Stdout = l.logFile
	cmd.Stderr = l.logFile
	// Orphan prevention (Linux-only — see the type doc): if the test runner is
	// hard-killed (go test -timeout panic, SIGKILL), the kernel SIGTERMs mecated.
	cmd.SysProcAttr = &syscall.SysProcAttr{Pdeathsig: syscall.SIGTERM}
	// A minimal, explicit environment: the fake HOME/XDG tree, PATH (git, shell),
	// and the OpenRouter key. Deliberately NOT os.Environ(): OPENAI_API_KEY /
	// ANTHROPIC_API_KEY in the developer's shell would flip provider detection.
	cmd.Env = []string{
		"PATH=" + os.Getenv("PATH"),
		"HOME=" + l.dir("home"),
		"XDG_CONFIG_HOME=" + l.dir("xdg-config"),
		"XDG_STATE_HOME=" + l.dir("xdg-state"),
		"XDG_CACHE_HOME=" + l.dir("xdg-cache"),
		"OPENROUTER_API_KEY=" + os.Getenv("OPENROUTER_API_KEY"),
	}
	if err := cmd.Start(); err != nil {
		return fmt.Errorf("spawn mecated: %w", err)
	}
	l.cmd = cmd
	// The ONE Wait call, started at spawn: without it cmd.ProcessState is never
	// populated, so any "did it crash?" check is dead code. waitReady and Close
	// both observe the exit through this channel.
	l.exited = make(chan struct{})
	go func() {
		_ = cmd.Wait()
		close(l.exited)
	}()

	cli, err := client.Dial(client.DialConfig{Server: l.grpcAddr})
	if err != nil {
		return err
	}
	l.cli = cli
	if err := l.waitReady(60 * time.Second); err != nil {
		return fmt.Errorf("mecated did not become ready: %w\n--- mecated log tail ---\n%s", err, l.LogTail(4096))
	}
	return nil
}

// waitReady retries CreateSession (the first real RPC; it also exercises the
// provider registry) until it succeeds, then closes the probe session. A
// mecated that exits during readiness fails IMMEDIATELY (the spawn-time Wait
// goroutine closes l.exited and sets ProcessState) instead of burning the
// full timeout.
func (l *Local) waitReady(timeout time.Duration) error {
	deadline := time.Now().Add(timeout)
	var lastErr error
	for time.Now().Before(deadline) {
		select {
		case <-l.exited:
			return fmt.Errorf("mecated exited during readiness: %v", l.cmd.ProcessState)
		default:
		}
		ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		id, _, _, err := l.cli.CreateSession(ctx, l.Workspace(), client.ModeFromString("default"), client.ModelSelection{})
		if err == nil {
			_ = l.cli.CloseSession(ctx, id)
			cancel()
			return nil
		}
		cancel()
		lastErr = err
		time.Sleep(250 * time.Millisecond)
	}
	return lastErr
}

// freePort binds :0 on loopback, reads the assigned port, and releases it.
func freePort() (int, error) {
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		return 0, err
	}
	defer ln.Close()
	return ln.Addr().(*net.TCPAddr).Port, nil
}

// initWorkspaceGit makes the workspace a real git repo with one commit, so the
// subagent/team/parallel worktree-fork machinery has a base to fork.
func initWorkspaceGit(ws string) error {
	cmds := [][]string{
		{"git", "init", "-q"},
		{"git", "add", "README.md", "FRUIT.txt", ".claude"},
		{"git", "-c", "user.name=mecatl-e2e", "-c", "user.email=e2e@mecatl.invalid", "commit", "-q", "-m", "e2e fixture workspace"},
	}
	for _, c := range cmds {
		cmd := exec.Command(c[0], c[1:]...)
		cmd.Dir = ws
		cmd.Env = []string{"PATH=" + os.Getenv("PATH"), "HOME=" + ws, "GIT_CONFIG_NOSYSTEM=1"}
		if out, err := cmd.CombinedOutput(); err != nil {
			return fmt.Errorf("%v: %v (%s)", c, err, out)
		}
	}
	return nil
}

func envOr(key, def string) string {
	if v := os.Getenv(key); v != "" {
		return v
	}
	return def
}

// --- Target interface ---

func (l *Local) Client() *client.Client { return l.cli }
func (l *Local) Workspace() string      { return l.dir("workspace") }
func (l *Local) MetricsURL() string     { return "http://" + l.metricsAddr + "/metrics" }
func (l *Local) IsLocal() bool          { return true }

func (l *Local) StateDir(kind StateKind) string {
	switch kind {
	case StateWorkspace:
		return l.dir("workspace")
	case StateMemory:
		return l.dir("memory")
	case StateUserModel:
		return l.dir("usermodel")
	case StateStore:
		return l.dir("store")
	case StateArtifacts:
		return l.dir("artifacts")
	}
	return ""
}

// LogTail returns up to the last n bytes of the captured mecated combined
// stdout+stderr log (mecated.log).
func (l *Local) LogTail(n int) string {
	data, err := os.ReadFile(l.logPath)
	if err != nil {
		return ""
	}
	if len(data) > n {
		data = data[len(data)-n:]
	}
	return string(data)
}

// Close SIGTERMs the daemon, waits (bounded), then SIGKILLs. The scratch tree
// is left in place — the artifacts ARE the deliverable of a failed run. The
// exit is observed via the spawn-time Wait goroutine (the single Wait owner).
func (l *Local) Close() error {
	var errs []error
	if l.cli != nil {
		errs = append(errs, l.cli.Close())
	}
	if l.cmd != nil && l.cmd.Process != nil {
		_ = l.cmd.Process.Signal(syscall.SIGTERM)
		select {
		case <-l.exited:
		case <-time.After(10 * time.Second):
			_ = l.cmd.Process.Kill()
			<-l.exited
		}
	}
	if l.logFile != nil {
		errs = append(errs, l.logFile.Close())
	}
	return errors.Join(errs...)
}
