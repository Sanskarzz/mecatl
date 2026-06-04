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

	// Embedded-server soul config (issue #14, Phase 1; used only when hosting an
	// in-process server). A user-scoped, agent-READ-ONLY persona fragment injected
	// as turn-0 context. ON by default reading the conventional
	// $XDG_CONFIG_HOME/mecatl/soul.md (fallback ~/.config/mecatl/soul.md) — a
	// missing file is fail-soft, so it costs nothing. soulFile overrides the path;
	// noSoul disables it entirely and wins (the resolved SoulPath/NoSoul map onto
	// app.Config in embeddedConfig). No tool can write the soul.
	//
	// Drift baseline (issue #14, Phase 3, Item 1): the harness records the soul's
	// content hash in a sidecar (<soulPath>.sha256) trust-on-first-use; a later run
	// whose hash differs logs a drift WARN and still loads. approveSoul (re)writes the
	// baseline to the current hash (accept the edit); soulStrict makes a DRIFTED soul
	// contribute no fragment.
	soulFile    string
	noSoul      bool
	approveSoul bool
	soulStrict  bool

	// Embedded-server user-model config (issue #14, Phase 2; used only when hosting
	// an in-process server). A user-scoped, CROSS-PROJECT memory of durable FACTS
	// about the operator (RememberUser/RecallUser/SearchUserModel + a turn-0
	// <user-model> block). ON by default at the conventional
	// $XDG_CONFIG_HOME/mecatl/usermodel (fallback ~/.config/mecatl/usermodel).
	// userModelDir overrides the dir; noUserModel disables it. userModelReview
	// enables the OPT-IN (off by default) Stop-triggered background reviewer;
	// userModelReviewInterval is its session-count debounce. Map onto app.Config in
	// embeddedConfig. The user model holds FACTS about the operator, never rules.
	userModelDir            string
	noUserModel             bool
	userModelReview         bool
	userModelReviewInterval int

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

	// Embedded-server perf observability (decision 7 in
	// docs/design/perf-observability.md; used only when hosting an in-process
	// server). OFF by default. perf arms the loopback runtime-introspection admin
	// surface (pprof/expvar/RSS/goroutines/flightrecorder + /metrics) plus the
	// domain-metrics EventSink in the embedded engine. perfAddr is the loopback
	// admin listen address (empty = an ephemeral loopback port, logged on start).
	// perfGoroutineWarnThreshold arms the live goroutine-leak watchdog (0 = off).
	perf                       bool
	perfAddr                   string
	perfGoroutineWarnThreshold int
	// perfMCP mounts the read-only perf MCP server at /mcp on the embedded admin
	// surface (only meaningful with --perf). The admin listener is loopback by
	// construction; embed FAILS CLOSED if --perf-addr is non-loopback with this set.
	perfMCP bool
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
	fs.BoolVar(&cfg.allowAllTools, "yolo", false,
		"embedded server only; OPERATOR POSTURE (dangerous): suppress permission prompts for the built-in mutate-ask floor, for ephemeral/sandboxed use only. Deny in any scope and configured Ask still apply. Refused as root unless MECATL_SANDBOX=1 (or IS_SANDBOX=1).")
	fs.StringVar(&cfg.memoryDir, "memory-dir", "", "embedded server only: per-project memory store directory (empty = a per-project default under $XDG_DATA_HOME/mecatui/memory)")
	fs.BoolVar(&cfg.noMemory, "no-memory", false, "embedded server only: disable cross-session memory (Remember/Recall) entirely")
	fs.StringVar(&cfg.soulFile, "soul-file", "", "embedded server only: path to a user-scoped, agent-READ-ONLY persona/\"soul\" file injected as turn-0 context (empty = the conventional $XDG_CONFIG_HOME/mecatl/soul.md, fallback ~/.config/mecatl/soul.md; fail-soft if absent)")
	fs.BoolVar(&cfg.noSoul, "no-soul", false, "embedded server only: disable the user-scoped persona/soul fragment entirely")
	fs.BoolVar(&cfg.approveSoul, "approve-soul", false, "embedded server only: (re)write the soul DRIFT BASELINE to the current soul's content hash, accepting the file as-is. The baseline is a harness-owned sidecar next to the soul (<soul-path>.sha256); a later run whose hash differs logs a drift WARN")
	fs.BoolVar(&cfg.soulStrict, "soul-strict", false, "embedded server only: refuse a DRIFTED soul — if its content hash differs from the recorded baseline, contribute NO soul fragment this run (instead of the default warn-and-load). Pair with --approve-soul to accept an edit")
	fs.StringVar(&cfg.userModelDir, "user-model-dir", "", "embedded server only: directory for the user-scoped, CROSS-PROJECT user-model store of durable FACTS about the operator (empty = the conventional $XDG_CONFIG_HOME/mecatl/usermodel, fallback ~/.config/mecatl/usermodel). Exposes RememberUser/RecallUser/SearchUserModel and a turn-0 <user-model> block")
	fs.BoolVar(&cfg.noUserModel, "no-user-model", false, "embedded server only: disable the user model entirely (the RememberUser/RecallUser/SearchUserModel tools and the <user-model> block)")
	fs.BoolVar(&cfg.userModelReview, "user-model-review", false, "embedded server only: enable the OPT-IN background user-model reviewer (off by default): after a session stops, a fresh single-shot child extracts durable operator FACTS from the transcript via RememberUser. NEVER reopens the user session")
	fs.IntVar(&cfg.userModelReviewInterval, "user-model-review-interval", 1, "embedded server only: session-count debounce for --user-model-review (1 = every session)")
	fs.StringVar(&cfg.commandsDir, "commands-dir", "", "embedded server only: directory of slash-command templates (<name>.md); empty = the conventional dirs (.mecatl/commands, .claude/commands)")
	fs.BoolVar(&cfg.noCommands, "no-commands", false, "embedded server only: disable slash-command expansion entirely")
	fs.StringVar(&cfg.skillsDir, "skills-dir", "", "embedded server only: directory of skill units (<name>/SKILL.md); empty = the conventional dirs (e.g. .claude/skills)")
	fs.BoolVar(&cfg.noSkills, "no-skills", false, "embedded server only: disable skill discovery (the Skill tool) entirely")

	fs.BoolVar(&cfg.perf, "perf", false, "embedded server only: expose the loopback perf-observability admin surface (/metrics, /debug/pprof, /debug/vars, /debug/flightrecorder) and wire domain metrics into the engine. OFF by default. The address is logged on start. SECURITY: loopback-bound, UNAUTHENTICATED — its output can embed prompt text/file paths/goroutine stacks, so it stays on 127.0.0.1 only (decision 6/7 of docs/design/perf-observability.md)")
	fs.StringVar(&cfg.perfAddr, "perf-addr", "", "embedded server only: loopback listen address for the --perf admin surface (empty = the fixed default 127.0.0.1:9099, predictable so an MCP-client config can hardcode the /mcp URL; distinct from mecated's :9090). Pass another host:port, or 127.0.0.1:0 for an ephemeral port. On a port clash, start FAILS with guidance. Only consulted with --perf")
	fs.IntVar(&cfg.perfGoroutineWarnThreshold, "perf-goroutine-warn-threshold", 0, "embedded server only: arm the live goroutine-leak watchdog — log a Warn whenever runtime.NumGoroutine() exceeds this count (decision 10). 0 (default) disables the alarm; the /metrics goroutine-count series is exported regardless. Only consulted with --perf")
	fs.BoolVar(&cfg.perfMCP, "perf-mcp", false, "embedded server only: mount the read-only perf MCP server at /mcp on the --perf admin surface, so an agent can introspect THIS process's runtime/latency/profile state over MCP (list_slow_turns, runtime/heap/CPU profiles, FlightRecorder). Only meaningful with --perf. SECURITY: loopback-bound, UNAUTHENTICATED (decision 6) — embed REFUSES a non-loopback --perf-addr with this set")

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
		return errors.New("--yolo refused: running as root (euid 0) without a declared sandbox; set MECATL_SANDBOX=1 (or IS_SANDBOX=1) to affirm an isolated, disposable environment")
	}
	return nil
}
