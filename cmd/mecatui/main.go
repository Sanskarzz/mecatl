// Command mecatui is a flashy, themeable terminal UI for the mecatl headless
// agentic coding harness. It is a gRPC CLIENT of a mecated server: it creates a
// session, opens the bidi Converse stream, renders the streamed Events (glamour
// markdown for assistant text, themed lipgloss for user/tool blocks), and resolves
// permission asks inline by sending ResumeApproval back on the same stream.
//
// The server it talks to may be EXTERNAL (a separately-run mecated, via --server)
// or, by default, one this process HOSTS in-process over a UNIX socket (see
// cmd/mecatui/embed) — so a single `mecatui` binary "just works" with no daemon to
// start and no TCP port. In AUTO mode (no --server) it first probes the loopback
// default and reuses a server already running there; only if none answers does it
// embed.
//
// Architectural boundary: the render packages (ui, theme) and the client package
// import no internal/... package and no proto directly — they render purely from
// proto Events. Hosting the embedded server makes the cmd/mecatui MAIN (and its
// embed subpackage) a second composition root, alongside cmd/mecated; that import
// of internal/app + the server adapter is confined HERE and to cmd/mecatui/embed.
package main

import (
	"context"
	"fmt"
	"os"
	"os/signal"
	"path/filepath"
	"strings"
	"syscall"
	"time"

	tea "charm.land/bubbletea/v2"
	"github.com/adrg/xdg"

	"github.com/stacklok/mecatl/cmd/mecatui/client"
	"github.com/stacklok/mecatl/cmd/mecatui/embed"
	"github.com/stacklok/mecatl/cmd/mecatui/theme"
	"github.com/stacklok/mecatl/cmd/mecatui/ui"
	mecatlv1 "github.com/stacklok/mecatl/contracts/gen/go/mecatl/v1"
	"github.com/stacklok/mecatl/internal/app"
)

func main() {
	if err := run(os.Args[1:]); err != nil {
		fmt.Fprintln(os.Stderr, "mecatui:", err)
		os.Exit(1)
	}
}

func run(args []string) error {
	cfg, err := parseFlags(args)
	if err != nil {
		return err
	}
	if err := cfg.validate(); err != nil {
		return err
	}

	reg := buildRegistry(cfg.workspace, cfg.themeDir)
	if cfg.listThemes {
		for _, name := range reg.List() {
			fmt.Println(name)
		}
		return nil
	}
	th, ok := reg.Resolve(cfg.theme)
	if !ok {
		fmt.Fprintf(os.Stderr, "mecatui: unknown theme %q, using %q\n", cfg.theme, th.Name)
	}

	ctx, stop := signal.NotifyContext(context.Background(), syscall.SIGINT, syscall.SIGTERM)
	defer stop()

	// Resolve where to connect: an explicit external server, a server already
	// running on the loopback default, or an embedded server we host in-process.
	target, dial, cleanup, err := resolveTransport(ctx, cfg)
	if err != nil {
		return err
	}
	defer cleanup()

	if cfg.insecure {
		fmt.Fprintln(os.Stderr, "mecatui: WARNING: --insecure skips TLS certificate verification (testing only)")
	}

	cl, err := client.Dial(dial)
	if err != nil {
		return err
	}
	defer func() { _ = cl.Close() }()

	deps := ui.Deps{
		Session: &sessionAdapter{cl: cl, workspace: cfg.workspace, mode: client.ModeFromString(cfg.mode)},
		Conv:    cl,
		MCP:     cl,
		Cmds:    cl,
		Theme:   th,
		Server:  target,
		// Model is best-effort display only. For an EXTERNAL --server it reflects
		// the locally-configured --model flag and may NOT match the server's actual
		// model (the server owns provider config); for an embedded server it is
		// authoritative. The context window comes only from an explicit
		// --context-window flag (0 = unknown) — never inferred from the model name.
		Model:         cfg.model,
		ContextWindow: cfg.contextWindow,
		Workspace:     cfg.workspace,
		Mode:          cfg.mode,
		Ctx:           ctx,
		// First-class opt-out: render inline in the normal buffer (preserving
		// native scrollback) instead of the alternate screen. Default false.
		NoAltScreen: cfg.noAltScreen,
	}

	prog := tea.NewProgram(ui.New(deps), tea.WithContext(ctx))
	_, err = prog.Run()
	return err
}

// resolveTransport decides how mecatui reaches a server and returns the dial
// target (for display + connection), the client.DialConfig to dial it with, and a
// cleanup func to defer (a no-op unless an embedded server was started).
//
//   - --server set:     dial that external address with the TLS/auth flags.
//   - --server empty:   AUTO — if a server already answers on the loopback default,
//     reuse it (plaintext); otherwise host an embedded server over a UNIX socket.
func resolveTransport(ctx context.Context, cfg config) (target string, dial client.DialConfig, cleanup func(), err error) {
	noop := func() {}

	if cfg.server != "" {
		return cfg.server, client.DialConfig{
			Server:    cfg.server,
			AuthToken: cfg.authToken,
			UseTLS:    cfg.useTLS,
			TLSCAFile: cfg.tlsCA,
			Insecure:  cfg.insecure,
		}, noop, nil
	}

	if client.IsReachable(ctx, defaultProbeAddr) {
		fmt.Fprintf(os.Stderr, "mecatui: using mecated already running at %s\n", defaultProbeAddr)
		return defaultProbeAddr, client.DialConfig{Server: defaultProbeAddr}, noop, nil
	}

	srv, err := embed.Start(ctx, embeddedConfig(cfg))
	if err != nil {
		return "", client.DialConfig{}, noop, fmt.Errorf("start embedded server: %w", err)
	}
	fmt.Fprintf(os.Stderr, "mecatui: no server found; hosting an embedded mecated at %s\n", srv.Target())
	// The embedded server has no auth/TLS — it is a private UNIX socket dialled
	// plaintext, the same single-user loopback trust model mecated uses.
	return srv.Target(), client.DialConfig{Server: srv.Target()}, func() { _ = srv.Close() }, nil
}

// embeddedConfig maps the TUI config onto the shared app.Config build contract for
// the in-process server. It enables the standard default toolset (Bash unless
// --no-bash, Fork, repo map), the agent-teams capability (inert until a client
// drives a team), conventional agent-definition discovery (AgentsConventional:
// true, also inert until a <name>.md exists under a conventional dir), and
// cross-session memory (Remember/Recall) scoped per-project (see resolveMemoryDir;
// disable with --no-memory or relocate with --memory-dir). It leaves the heavier
// opt-ins (MCP, ToolHive, skills, slash commands, telemetry) off — a focused
// single-user default. The provider is OpenAI when OPENAI_API_KEY is set, else the
// offline mock (--mock).
func embeddedConfig(cfg config) app.Config {
	return app.Config{
		Workspace:            cfg.workspace,
		Model:                cfg.model,
		UseOpenAI:            cfg.openAIKey != "",
		OpenAIKey:            cfg.openAIKey,
		OpenAIBaseURL:        cfg.openAIBaseURL,
		UseMock:              cfg.mock,
		Shell:                "/bin/sh",
		NoBash:               cfg.noBash,
		Compaction:           "heuristic",
		Tokenizer:            "heuristic",
		LLMMaxAttempts:       3,
		LLMPerAttemptTimeout: 30 * time.Second,
		LLMBreakerThreshold:  5,
		LLMBreakerCooldown:   30 * time.Second,
		EnableFork:           true,
		// EnableRepoMap OFF: the WASM tree-sitter binding leaks and hangs after
		// ~160 files, freezing the in-process TUI. See docs/design/REPOMAP-TREE-SITTER.md.
		EnableRepoMap:      false,
		EnableTeams:        true,
		AgentsConventional: true,
		// Memory is ON by default, per-project. MemoryConsolidateInterval is left
		// at 0 (off) deliberately: the "dream" distiller spawns a goroutine that
		// calls the real provider on a timer, so a default-on interval would
		// silently spend tokens on an idle TUI. mecated defaults it to 0 too.
		MemoryDir: resolveMemoryDir(cfg),
	}
}

// resolveMemoryDir applies the embedded-server memory precedence: --no-memory
// disables it (""), an explicit --memory-dir overrides, otherwise a per-project
// default under XDG data (see defaultMemoryDir). The store owns creating the dir;
// main only computes a path string and treats the location as opaque.
func resolveMemoryDir(cfg config) string {
	if cfg.noMemory {
		return ""
	}
	if cfg.memoryDir != "" {
		return cfg.memoryDir
	}
	// The single place that reads the xdg.DataHome global — main is the
	// composition root, so the one global read lives here, and defaultMemoryDir
	// stays a pure function of its arguments (cheap to table-test).
	return defaultMemoryDir(xdg.DataHome, cfg.workspace)
}

// defaultMemoryDir derives a stable, per-project memory directory under the XDG
// data base dataHome ($XDG_DATA_HOME, else ~/.local/share — resolved by the caller
// via xdg.DataHome). The leaf is the resolved absolute workspace path with the OS
// separator replaced by '-' (preserving the leading separator as a leading '-'),
// e.g. "/var/home/ozz/dev/mecatl" → "-var-home-ozz-dev-mecatl". Encoding the FULL
// path keeps it deterministic, human-legible, and collision-free across same-named
// projects. Returns "" when dataHome or workspace is empty — in that degraded case
// memory stays off rather than anchoring a store at a bogus path. Pure in its
// arguments: it reads no globals.
func defaultMemoryDir(dataHome, workspace string) string {
	if dataHome == "" || workspace == "" {
		return ""
	}
	leaf := strings.ReplaceAll(workspace, string(filepath.Separator), "-")
	return filepath.Join(dataHome, "mecatui", "memory", leaf)
}

// buildRegistry seeds the theme registry with built-ins and loads user theme
// dirs in increasing precedence: XDG config → workspace .mecatui → cwd .mecatui
// → an explicit --theme-dir. Load errors are warnings, not fatal — a bad theme
// file should never stop the UI from launching.
func buildRegistry(workspace, extraDir string) *theme.Registry {
	reg := theme.NewRegistry()
	for _, dir := range themeDirs(workspace, extraDir) {
		if err := reg.LoadDir(dir); err != nil {
			fmt.Fprintln(os.Stderr, "mecatui: theme load:", err)
		}
	}
	return reg
}

// themeDirs returns the theme directory search path, lowest precedence first:
// XDG/home config, the workspace's .mecatui/themes, the cwd's .mecatui/themes,
// then any explicit --theme-dir (highest).
func themeDirs(workspace, extraDir string) []string {
	var dirs []string
	if xdg.ConfigHome != "" {
		dirs = append(dirs, filepath.Join(xdg.ConfigHome, "mecatui", "themes"))
	}
	if workspace != "" {
		dirs = append(dirs, filepath.Join(workspace, ".mecatui", "themes"))
	}
	if wd, err := os.Getwd(); err == nil && wd != workspace {
		dirs = append(dirs, filepath.Join(wd, ".mecatui", "themes"))
	}
	if extraDir != "" {
		dirs = append(dirs, extraDir)
	}
	return dirs
}

// sessionAdapter bridges the ui's parameterless SessionCreator to the client's
// CreateSession(ctx, workspace, mode). The workspace and mode are fixed at
// startup, so the ui only needs "create the session".
type sessionAdapter struct {
	cl        *client.Client
	workspace string
	mode      mecatlv1.PermissionMode
}

func (s *sessionAdapter) CreateSession(ctx context.Context) (string, error) {
	return s.cl.CreateSession(ctx, s.workspace, s.mode)
}
