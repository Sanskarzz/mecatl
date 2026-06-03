package main

import (
	"errors"
	"flag"
	"fmt"
	"os"
	"path/filepath"
)

// config is the resolved CLI/env configuration for mecatui.
type config struct {
	// server is the external mecated gRPC address (host:port). Empty means AUTO:
	// probe the loopback default and, if nothing answers, host an embedded server
	// in-process over a UNIX socket (see cmd/mecatui/embed).
	server     string
	workspace  string
	mode       string
	theme      string
	themeDir   string
	authToken  string
	useTLS     bool
	tlsCA      string
	insecure   bool
	listThemes bool

	// noAltScreen renders mecatui INLINE in the terminal's normal buffer instead
	// of the alternate screen. Off by default (full-screen TUI on the alt screen);
	// the first-class opt-out for users who want the session streamed into native
	// scrollback so it stays searchable/scrollable after exit. Wired to
	// ui.Deps.NoAltScreen.
	noAltScreen bool

	// contextWindow is the model's context-window size in tokens, used as the
	// footer meter denominator. 0 = unknown (meter shows just the current size).
	// Honoured verbatim; never inferred from the model name.
	contextWindow int64

	// Embedded-server provider config (used only when no external server is
	// dialled). The OpenAI key is read from OPENAI_API_KEY; --mock selects the
	// canned offline provider instead (useful for a no-network smoke run).
	model         string
	openAIBaseURL string
	openAIKey     string
	mock          bool
	noBash        bool

	// allowAllTools is the operator allow-all posture for the EMBEDDED server only
	// (ignored when dialling an external --server). When set it injects a single
	// ScopeCLI allow-all rule that suppresses the built-in mutate-ask floor; a Deny
	// in any scope and any deliberately configured Ask still apply. Refused as root
	// outside a declared sandbox (see validate). See docs/design/ALLOW-ALL-POSTURE.md.
	allowAllTools bool

	// Embedded-server memory config (used only when hosting an in-process
	// server). An empty memoryDir means "compute the per-project default under
	// $XDG_DATA_HOME/mecatui/memory"; an explicit path overrides it. noMemory
	// disables cross-session memory (Remember/Recall) entirely and wins over
	// both (the resolved MemoryDir becomes ""). Precedence is applied in
	// embeddedConfig (resolveMemoryDir), not here.
	memoryDir string
	noMemory  bool

	// Embedded-server slash-command config (used only when hosting an in-process
	// server). Command expansion is ON by default, expanding "/<name>" inputs from
	// the conventional workspace dirs (.mecatl/commands, .claude/commands). An
	// explicit commandsDir overrides the directory; noCommands disables expansion
	// entirely and wins. Precedence is applied in embeddedConfig (resolveCommands),
	// not here.
	commandsDir string
	noCommands  bool

	// Embedded-server skills config (used only when hosting an in-process server).
	// Conventional skill discovery is ON by default: the progressive-disclosure
	// Skill tool activates SKILL.md units from the conventional dirs (e.g.
	// .claude/skills) when present — consistent with AgentsConventional. An explicit
	// skillsDir overrides with a single vetted directory; noSkills disables skill
	// discovery entirely and wins. Only read-only discovery is wired here, never the
	// writable SkillDraft quarantine. Precedence is applied in embeddedConfig
	// (resolveSkills), not here.
	skillsDir string
	noSkills  bool
}

// defaultProbeAddr is mecated's historical default loopback gRPC address. In AUTO
// mode (no --server) mecatui probes this; if a server is already serving there it
// connects, otherwise it hosts an embedded server instead.
const defaultProbeAddr = "127.0.0.1:8080"

// parseFlags parses argv into a config, applying env fallbacks. The workspace is
// resolved to an absolute path (the server requires absolute). args excludes the
// program name.
func parseFlags(args []string) (config, error) {
	var cfg config
	fs := flag.NewFlagSet("mecatui", flag.ContinueOnError)
	fs.StringVar(&cfg.server, "server", "", "external mecated gRPC address (host:port); empty = auto: reuse a server already running on "+defaultProbeAddr+", else host an embedded one over a UNIX socket")
	fs.StringVar(&cfg.workspace, "workspace", "", "absolute workspace root for the session (default: cwd)")
	fs.StringVar(&cfg.mode, "mode", "default", "permission mode: default | plan | accept-edits")
	fs.StringVar(&cfg.theme, "theme", "", "theme name (default: aztec)")
	fs.StringVar(&cfg.themeDir, "theme-dir", "", "extra directory of *.json themes to load")
	fs.StringVar(&cfg.authToken, "auth-token", "", "bearer token for an external server (or MECATL_AUTH_TOKEN)")
	fs.BoolVar(&cfg.useTLS, "tls", false, "use TLS transport when dialling an external server")
	fs.StringVar(&cfg.tlsCA, "tls-ca", "", "PEM CA bundle for external-server verification")
	fs.BoolVar(&cfg.insecure, "insecure", false, "skip TLS verification (testing only)")
	fs.BoolVar(&cfg.listThemes, "list-themes", false, "list available themes and exit")
	fs.BoolVar(&cfg.noAltScreen, "no-alt-screen", false, "render inline in the terminal's normal buffer instead of the alternate screen, preserving native scrollback/search")
	fs.BoolVar(&cfg.noAltScreen, "inline", false, "alias for --no-alt-screen: render inline in the normal buffer, preserving native scrollback/search")
	fs.Int64Var(&cfg.contextWindow, "context-window", 0, "model context-window size in tokens for the footer meter (0 = unknown; not inferred from the model name)")

	fs.StringVar(&cfg.model, "model", "gpt-5", "model identifier for the embedded server (ignored when dialling an external server)")
	fs.StringVar(&cfg.openAIBaseURL, "openai-base-url", "", "override the OpenAI API base URL for the embedded server (compatible endpoints)")
	fs.BoolVar(&cfg.mock, "mock", false, "embedded server only: use the canned offline mock provider instead of OpenAI (no network)")
	fs.BoolVar(&cfg.noBash, "no-bash", false, "embedded server only: disable the Bash tool (shell-less mode)")
	fs.BoolVar(&cfg.allowAllTools, "dangerously-allow-all-tools", false,
		"embedded server only; OPERATOR POSTURE (dangerous): suppress permission prompts for the built-in mutate-ask floor, for ephemeral/sandboxed use only. Deny in any scope and configured Ask still apply. Refused as root unless MECATL_SANDBOX=1 (or IS_SANDBOX=1).")
	fs.StringVar(&cfg.memoryDir, "memory-dir", "", "embedded server only: per-project memory store directory (empty = a per-project default under $XDG_DATA_HOME/mecatui/memory)")
	fs.BoolVar(&cfg.noMemory, "no-memory", false, "embedded server only: disable cross-session memory (Remember/Recall) entirely")
	fs.StringVar(&cfg.commandsDir, "commands-dir", "", "embedded server only: directory of slash-command templates (<name>.md); empty = the conventional dirs (.mecatl/commands, .claude/commands)")
	fs.BoolVar(&cfg.noCommands, "no-commands", false, "embedded server only: disable slash-command expansion entirely")
	fs.StringVar(&cfg.skillsDir, "skills-dir", "", "embedded server only: directory of skill units (<name>/SKILL.md); empty = the conventional dirs (e.g. .claude/skills)")
	fs.BoolVar(&cfg.noSkills, "no-skills", false, "embedded server only: disable skill discovery (the Skill tool) entirely")

	if err := fs.Parse(args); err != nil {
		return config{}, err
	}

	if cfg.authToken == "" {
		cfg.authToken = os.Getenv("MECATL_AUTH_TOKEN")
	}
	if cfg.theme == "" {
		cfg.theme = os.Getenv("MECATUI_THEME")
	}
	cfg.openAIKey = os.Getenv("OPENAI_API_KEY")

	if !cfg.listThemes {
		ws, err := resolveWorkspace(cfg.workspace)
		if err != nil {
			return config{}, err
		}
		cfg.workspace = ws
	}
	return cfg, nil
}

// resolveWorkspace defaults an empty workspace to the cwd and makes it absolute.
func resolveWorkspace(ws string) (string, error) {
	if ws == "" {
		cwd, err := os.Getwd()
		if err != nil {
			return "", fmt.Errorf("resolve cwd as workspace: %w", err)
		}
		return cwd, nil
	}
	if !filepath.IsAbs(ws) {
		abs, err := filepath.Abs(ws)
		if err != nil {
			return "", fmt.Errorf("resolve workspace %q: %w", ws, err)
		}
		return abs, nil
	}
	return ws, nil
}

// validate checks invariants the server also enforces, failing fast client-side.
func (c config) validate() error {
	if c.listThemes {
		return nil
	}
	if c.workspace == "" {
		return errors.New("workspace is required")
	}
	if !filepath.IsAbs(c.workspace) {
		return fmt.Errorf("workspace must be absolute: %q", c.workspace)
	}
	switch c.mode {
	case "default", "plan", "accept-edits":
	default:
		return fmt.Errorf("invalid --mode %q (want default|plan|accept-edits)", c.mode)
	}
	// When hosting an embedded server (no external --server) the provider must be
	// resolvable: either an OpenAI key in the environment or the offline mock.
	if c.server == "" && c.openAIKey == "" && !c.mock {
		return errors.New("no external --server given and no OPENAI_API_KEY set: " +
			"set OPENAI_API_KEY to host an embedded server, pass --mock for an offline run, " +
			"or point --server at a running mecated")
	}
	// Allow-all posture: only meaningful for the embedded server; refuse it when
	// running privileged outside a declared sandbox. Dialling an external server
	// never embeds, so it must not trip the refusal.
	if c.server == "" {
		sandbox := os.Getenv("MECATL_SANDBOX") == "1" || os.Getenv("IS_SANDBOX") == "1"
		if err := allowAllRefusalReason(c.allowAllTools, os.Geteuid(), sandbox); err != nil {
			return err
		}
	}
	return nil
}

// allowAllRefusalReason returns a non-nil error when an allow-all request must be
// refused: running privileged (euid 0) without a declared sandbox. Pure and
// table-testable; the os lookups live at the call site.
func allowAllRefusalReason(allowAll bool, euid int, sandbox bool) error {
	if allowAll && euid == 0 && !sandbox {
		return errors.New("--dangerously-allow-all-tools refused: running as root (euid 0) without a declared sandbox; set MECATL_SANDBOX=1 (or IS_SANDBOX=1) to affirm an isolated, disposable environment")
	}
	return nil
}
