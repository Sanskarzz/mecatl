// Command mecatui is a flashy, themeable terminal UI for the mecatl headless
// agentic coding harness. It is a gRPC CLIENT of a running mecated server: it
// creates a session, opens the bidi Converse stream, renders the streamed Events
// (glamour markdown for assistant text, themed lipgloss for user/tool blocks),
// and resolves permission asks inline by sending ResumeApproval back on the same
// stream.
//
// Architectural boundary: this binary imports only contracts/gen (via the
// client package), grpc, the charm libraries, and the local theme/ui/client
// packages. It NEVER imports internal/agent, internal/session, internal/
// governance, internal/prompt, internal/port, or any other internal/... package.
// The UI renders purely from proto Events.
package main

import (
	"context"
	"fmt"
	"os"
	"os/signal"
	"path/filepath"
	"syscall"

	tea "charm.land/bubbletea/v2"

	"github.com/stacklok/mecatl/cmd/mecatui/client"
	"github.com/stacklok/mecatl/cmd/mecatui/theme"
	"github.com/stacklok/mecatl/cmd/mecatui/ui"
	mecatlv1 "github.com/stacklok/mecatl/contracts/gen/go/mecatl/v1"
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

	if cfg.insecure {
		fmt.Fprintln(os.Stderr, "mecatui: WARNING: --insecure skips TLS certificate verification (testing only)")
	}

	cl, err := client.Dial(client.DialConfig{
		Server:    cfg.server,
		AuthToken: cfg.authToken,
		UseTLS:    cfg.useTLS,
		TLSCAFile: cfg.tlsCA,
		Insecure:  cfg.insecure,
	})
	if err != nil {
		return err
	}
	defer func() { _ = cl.Close() }()

	ctx, stop := signal.NotifyContext(context.Background(), syscall.SIGINT, syscall.SIGTERM)
	defer stop()

	deps := ui.Deps{
		Session:   &sessionAdapter{cl: cl, workspace: cfg.workspace, mode: client.ModeFromString(cfg.mode)},
		Conv:      cl,
		Theme:     th,
		Server:    cfg.server,
		Workspace: cfg.workspace,
		Mode:      cfg.mode,
		Ctx:       ctx,
	}

	prog := tea.NewProgram(ui.New(deps), tea.WithContext(ctx))
	_, err = prog.Run()
	return err
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
	if base := os.Getenv("XDG_CONFIG_HOME"); base != "" {
		dirs = append(dirs, filepath.Join(base, "mecatui", "themes"))
	} else if home, err := os.UserHomeDir(); err == nil {
		dirs = append(dirs, filepath.Join(home, ".config", "mecatui", "themes"))
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
