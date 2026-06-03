// Command mecated is the standalone mecatl server binary and a composition root:
// it parses the CLI/env configuration, builds the telemetry sink, delegates the
// engine + service assembly to internal/app (the SHARED composition layer also
// used by the embedded server in cmd/mecatui), and serves the resulting
// HarnessService over gRPC and HTTP/SSE concurrently, with graceful shutdown on
// SIGINT/SIGTERM.
//
// The agent loop, tool catalog, permission policy, provider, store, MCP and skills
// wiring all live in internal/app so the TUI can host the same server in-process;
// mecated owns only the things specific to a network daemon: flag parsing,
// TLS/auth/rate-limit, the HTTP + metrics listeners, and the `skills promote`
// operator subcommand.
package main

import (
	"bufio"
	"context"
	"crypto/tls"
	"crypto/x509"
	"errors"
	"flag"
	"fmt"
	"io"
	"log/slog"
	"net"
	"net/http"
	"os"
	"os/signal"
	"runtime"
	"runtime/trace"
	"sort"
	"strings"
	"syscall"
	"time"

	"github.com/prometheus/client_golang/prometheus"
	"go.opentelemetry.io/otel"
	"google.golang.org/grpc"
	"google.golang.org/grpc/credentials"
	"google.golang.org/grpc/health"
	healthpb "google.golang.org/grpc/health/grpc_health_v1"

	mecatlv1 "github.com/stacklok/mecatl/contracts/gen/go/mecatl/v1"
	"github.com/stacklok/mecatl/internal/adapter/agents"
	"github.com/stacklok/mecatl/internal/adapter/mcp"
	"github.com/stacklok/mecatl/internal/adapter/server"
	"github.com/stacklok/mecatl/internal/adapter/skills"
	"github.com/stacklok/mecatl/internal/adapter/telemetry"
	"github.com/stacklok/mecatl/internal/agent"
	"github.com/stacklok/mecatl/internal/app"
	"github.com/stacklok/mecatl/internal/port"
)

// TRUST MODEL (security): the mecated API exposes command and file execution
// against the configured workspace. The default listen addresses below bind the
// loopback interface (single-user localhost). Authentication is OPTIONAL and
// OFF by default for that loopback case: enable a bearer token (--auth-token /
// MECATL_AUTH_TOKEN) and/or TLS/mTLS (--tls-cert/--tls-key/--client-ca) before
// binding a non-loopback address. Binding non-loopback with NO authentication
// is permitted (an operator may front it with a mesh) but logs a prominent
// WARNING, since it exposes command/file execution to the network.
const (
	defaultGRPCAddr = "127.0.0.1:8080"
	defaultHTTPAddr = "127.0.0.1:8081"
)

// defaultMetricsAddr is the loopback listen address for the Prometheus /metrics
// endpoint. Unlike the harness API it is read-only and carries no secrets, but
// it is still bound to loopback by default. An empty --metrics-addr disables it.
const defaultMetricsAddr = "127.0.0.1:9090"

// config is the parsed command-line / environment configuration for mecated. The
// engine-build subset is mapped onto app.Config by appConfig; the rest (listen
// addresses, TLS, auth, rate limiting, metrics, tracing) is serve-time state
// owned by this binary.
type config struct {
	grpcAddr      string
	httpAddr      string
	workspace     string
	model         string
	useOpenAI     bool
	openAIBaseURL string
	openAIKey     string
	useMock       bool
	storeDir      string
	shell         string
	noBash        bool

	// Context management: the compaction strategy and the token counter. Both
	// default to the current behaviour exactly (heuristic compactor + heuristic
	// counter); "cascade"/"tiktoken" opt into the tiered cascade / real tokenizer.
	compaction string
	tokenizer  string

	// Security: API authentication, transport security, and rate limiting.
	authToken string  // bearer token required on every RPC/request (empty disables auth)
	tlsCert   string  // PEM server certificate; enables TLS on gRPC + HTTP when set with tlsKey
	tlsKey    string  // PEM server private key (paired with tlsCert)
	clientCA  string  // PEM client CA bundle; enables mutual TLS (require+verify client certs)
	rateLimit float64 // sustained per-client request rate (req/s); 0 disables rate limiting
	rateBurst int     // token-bucket burst size; 0 -> derived from rateLimit

	// LLM resilience knobs (see package internal/adapter/llmresilience).
	llmMaxAttempts       int
	llmPerAttemptTimeout time.Duration
	llmBreakerThreshold  int
	llmBreakerCooldown   time.Duration

	// Observability: the Prometheus /metrics listen address (empty disables it),
	// plus the OTLP trace exporter knobs (empty endpoint disables tracing).
	metricsAddr  string
	otlpEndpoint string // OTLP collector endpoint (empty disables tracing)
	otlpProtocol string // OTLP transport: "grpc" (default) or "http"
	otlpInsecure bool   // skip TLS when dialing the OTLP collector (dev only)

	// Runtime-introspection admin surface (loopback only, on the --metrics-addr
	// listener): pprof + expvar + a runtime/metrics snapshot + a FlightRecorder.
	// mutexProfileFraction arms runtime.SetMutexProfileFraction (0 = off);
	// blockProfileRate arms runtime.SetBlockProfileRate in ns (0 = off); both add
	// runtime overhead when > 0. flightRecorder arms the bounded execution-trace
	// ring buffer (default ON — low, bounded overhead).
	mutexProfileFraction int
	blockProfileRate     int
	flightRecorder       bool

	// Memory: per-project memory store directory (empty disables memory tools).
	memoryDir string

	// Skills: explicit directories of progressive-disclosure skill units laid out
	// as <dir>/<name>/SKILL.md (repeatable; highest precedence). Empty + no
	// conventional set disables the Skill tool. skillsConventional adds the
	// built-in conventional project/user locations (lower precedence), default OFF
	// to keep skills strictly opt-in (a trust boundary — see resolve.go / usage.md).
	skillsDirs         stringList
	skillsConventional bool

	// Agent definitions (Tier 1): named subagent specialists discovered from
	// <dir>/<name>.md files. agentsDirs are explicit dirs (repeatable; highest
	// precedence); agentsConventional adds the conventional project/user locations
	// (.mecatl/agents, .claude/agents, XDG/user) — ON by default and INERT when no
	// such dir exists, mirroring the teams/fork "on-but-inert" philosophy. subagentModel
	// globally overrides the model of every Task/member child that does not pin its
	// own; modelAliases maps short aliases (sonnet/opus/fast/...) to concrete ids.
	agentsDirs         stringList
	agentsConventional bool
	subagentModel      string
	modelAliases       keyValueList

	// Skills self-improvement loop (opt-in): when skillsDraftDir is non-empty the
	// writable SkillDraft tool is registered, writing model-authored candidate
	// SKILL.md files into this QUARANTINE directory (NEVER a catalog Source). An
	// operator promotes a candidate into an active --skills-dir with the
	// `mecated skills promote` subcommand. Empty disables the tool. The dir must be
	// disjoint from every active skills dir (fatal config error on overlap).
	skillsDraftDir       string
	skillsDraftThreshold float64

	// Memory consolidation (dream): background distillation interval. 0 disables.
	// Only meaningful when memoryDir is set; a positive value with an empty
	// memoryDir is a no-op (logged as a warning).
	memoryConsolidateInterval time.Duration

	// Slash commands: directory of <name>.md command templates, and an explicit
	// enable switch. commandsDir set OR enableCommands true wires the
	// DirCommandExpander; otherwise the default NoopExpander is left in place.
	commandsDir    string
	enableCommands bool

	// Fork: enable the Fork fan-out tool (parallel isolated child branches).
	enableFork bool
	// forkPreservedCap bounds how many PRESERVED winner forks (join=first/judge)
	// survive at once; the oldest beyond the cap is LRU-reaped. 0 => the default.
	forkPreservedCap int

	// RepoMap: enable the Aider-style repo-map tool. It is CGO-free (tree-sitter
	// runs as WebAssembly via wazero), so it is registered unconditionally by
	// default; this flag lets operators turn it off without a rebuild.
	enableRepoMap bool

	// Teams: enable the experimental agent-teams capability (CreateTeam /
	// SpawnTeammate / RunTeam). Opt-in, default off.
	enableTeams bool

	// MCP: remote MCP servers to connect to and register tools from.
	mcpServers mcpServerList

	// MCP resources: register the ListMcpResources/ReadMcpResource meta-tools when
	// a connected server exposes resources. Default ON — the tools are registered
	// only when there is at least one resource to expose.
	mcpResourceTools bool
	// MCP prompts: compose the MCP prompt expander so "/mcp__<server>__<prompt>"
	// inputs expand to the server-rendered prompt. Default ON; expansion only fires
	// when the MCP prompt namespace is actually used.
	mcpPrompts bool

	// ACP: serve the Agent Client Protocol over stdio instead of the TCP/HTTP
	// listeners. When set, mecated speaks JSON-RPC 2.0 to an ACP editor (Zed, etc.)
	// that spawned it as a subprocess; the normal network daemon path is skipped.
	acp bool

	// ToolHive: discover MCP servers from the running ToolHive workloads (the
	// embedded ToolHive library lists already-running workloads and reads their
	// HTTP proxy URLs — mecatl never spawns a workload). Default ON; it fails soft
	// to zero servers when no container runtime is reachable.
	toolHiveEnabled bool
	// toolHiveGroup is the ToolHive group to discover from (empty -> "default").
	toolHiveGroup string

	// File-based permission config (issue #13). permissionConfigs are explicit
	// operator-pointed YAML files (repeatable, fully trusted). permissionsConventional
	// auto-discovers the per-project .mecatl/settings.yaml (and the user-global
	// file), re-resolved per session against each session's workspace root — ON by
	// default. importClaudePermissions also imports Claude-Code settings.json (with
	// the lossy fail-safe table). trustProject honours a project's ALLOW rules; a
	// project's deny/ask is always honoured regardless. Default OFF (the safe
	// stance): an untrusted repo's allows are ignored.
	permissionConfigs       stringList
	permissionsConventional bool
	importClaudePermissions bool
	trustProject            bool
	allowAllTools           bool
}

// mcpServerList is a repeatable flag.Value collecting --mcp-server name=URL
// entries into a slice of mcp.ServerConfig.
type mcpServerList []mcp.ServerConfig

func (l *mcpServerList) String() string {
	names := make([]string, 0, len(*l))
	for _, c := range *l {
		names = append(names, c.Name)
	}
	return strings.Join(names, ",")
}

// Set parses a single "name=URL" entry. A per-server bearer token is read from
// the environment variable MCP_<NAME>_TOKEN (name upper-cased) when present.
func (l *mcpServerList) Set(v string) error {
	name, url, ok := strings.Cut(v, "=")
	if !ok || name == "" || url == "" {
		return fmt.Errorf("invalid --mcp-server %q: want name=URL", v)
	}
	cfg := mcp.ServerConfig{Name: name, URL: url}
	if tok := os.Getenv("MCP_" + strings.ToUpper(name) + "_TOKEN"); tok != "" {
		cfg.Headers = map[string]string{"Authorization": "Bearer " + tok}
	}
	*l = append(*l, cfg)
	return nil
}

// stringList is a repeatable string flag.Value, preserving order across multiple
// occurrences. A single occurrence behaves exactly like a plain StringVar, so a
// flag using it stays backward-compatible with single-value invocations.
type stringList []string

func (l *stringList) String() string { return strings.Join(*l, ",") }

func (l *stringList) Set(v string) error {
	*l = append(*l, v)
	return nil
}

// keyValueList is a repeatable "key=value" flag.Value collecting into an ordered
// map. A later occurrence of the same key overrides an earlier one. It backs
// --model-alias (e.g. --model-alias fast=gpt-4o-mini --model-alias smart=gpt-5).
type keyValueList map[string]string

func (m keyValueList) String() string {
	if len(m) == 0 {
		return ""
	}
	parts := make([]string, 0, len(m))
	for k, v := range m {
		parts = append(parts, k+"="+v)
	}
	sort.Strings(parts)
	return strings.Join(parts, ",")
}

func (m *keyValueList) Set(v string) error {
	k, val, ok := strings.Cut(v, "=")
	k = strings.TrimSpace(k)
	if !ok || k == "" {
		return fmt.Errorf("model alias must be key=value, got %q", v)
	}
	if *m == nil {
		*m = keyValueList{}
	}
	(*m)[k] = strings.TrimSpace(val)
	return nil
}

func main() {
	// Subcommand dispatch: `mecated skills promote ...` is the OPERATOR gate that
	// moves a model-authored candidate skill out of quarantine into an active
	// skills dir. It is a one-shot offline CLI action (no daemon), kept here so it
	// shares the binary and the skills adapter.
	if len(os.Args) >= 3 && os.Args[1] == "skills" && os.Args[2] == "promote" {
		if err := runSkillsPromote(os.Args[3:], os.Stdin, os.Stderr); err != nil {
			slog.Error("skills promote failed", "err", err)
			os.Exit(1)
		}
		return
	}
	if err := run(); err != nil {
		slog.Error("mecated exited with error", "err", err)
		os.Exit(1)
	}
}

// runSkillsPromote implements `mecated skills promote --skills-draft-dir
// <quarantine> --skills-dir <active> [--yes] <name>`: the operator-trust action that
// shows the full candidate, asks for confirmation (unless --yes), re-validates it
// (provenance + structure + injection scan), and moves it into the active skills
// tree. It requires filesystem access the model does not have, so it is the only
// path from model-authored quarantine to the trusted, live skill catalog. Flags
// precede the positional <name> (Go's flag parser stops at the first positional).
func runSkillsPromote(argv []string, in io.Reader, out io.Writer) error {
	fs := flag.NewFlagSet("mecated skills promote", flag.ContinueOnError)
	fs.SetOutput(out)
	var quarantine, active string
	var assumeYes bool
	fs.StringVar(&quarantine, "skills-draft-dir", "", "the QUARANTINE directory the candidate was drafted into")
	fs.StringVar(&active, "skills-dir", "", "the ACTIVE skills directory to promote the candidate into")
	fs.BoolVar(&assumeYes, "yes", false, "skip the interactive content review and promote without confirmation (scripted/CI use only)")
	if err := fs.Parse(argv); err != nil {
		return err
	}
	name := fs.Arg(0)
	if name == "" || quarantine == "" || active == "" {
		return fmt.Errorf("usage: mecated skills promote --skills-draft-dir <quarantine> --skills-dir <active> [--yes] <name>")
	}

	// Show the operator the FULL untrusted candidate (frontmatter + body) before
	// promoting: promotion is what makes model-authored text TRUSTED, so a human
	// must actually read it. The injection scan in skills.Promote is a backstop,
	// not a review.
	raw, err := skills.ReadCandidate(quarantine, name)
	if err != nil {
		return err
	}
	_, _ = fmt.Fprintf(out, "\n--- candidate skill %q (model-authored, UNTRUSTED until promoted) ---\n%s\n--- end candidate ---\n\n", name, raw)

	if !assumeYes {
		_, _ = fmt.Fprintf(out, "Promote %q into %s? This makes the above content TRUSTED and loadable by every future session. [y/N]: ", name, active)
		line, _ := bufio.NewReader(in).ReadString('\n')
		if ans := strings.ToLower(strings.TrimSpace(line)); ans != "y" && ans != "yes" {
			return fmt.Errorf("promotion of %q aborted by operator", name)
		}
	}

	if err := skills.Promote(quarantine, active, name); err != nil {
		return err
	}
	slog.Info("skill promoted to the active catalog (takes effect on next server start)",
		"name", name, "from", quarantine, "to", active)
	return nil
}

// run parses flags, builds the engine/service via internal/app, and serves until a
// termination signal arrives. It is separated from main so it can return errors
// cleanly.
func run() error {
	cfg, err := parseFlags(os.Args[1:])
	if err != nil {
		return err
	}

	logger := slog.New(slog.NewTextHandler(os.Stderr, &slog.HandlerOptions{Level: slog.LevelInfo}))
	slog.SetDefault(logger)

	// Allow-all posture: refuse the dangerous flag when running privileged outside a
	// declared sandbox (root + no prompts can modify anything on the host). Checked
	// AFTER the slog handler is installed and BEFORE app.Build, so it covers both the
	// ACP and network serving modes.
	if err := validateAllowAll(cfg); err != nil {
		return err
	}
	if cfg.allowAllTools {
		slog.Warn("ALLOW-ALL POSTURE ACTIVE (--yolo): permission prompts for the built-in mutate-ask floor are SUPPRESSED for EVERY session on this daemon. A Deny in any scope and any deliberately configured Ask (managed/project/user) still apply — a configured Ask may block an unattended run. Intended for ephemeral, isolated, single-tenant deployments only.")
	}

	ctx, stop := signal.NotifyContext(context.Background(), syscall.SIGINT, syscall.SIGTERM)
	defer stop()

	// Observability: install the OTel metrics pipeline (always on) + the OTLP
	// trace exporter/global TracerProvider (when --otlp-endpoint is set) BEFORE
	// building the sinks below. Setup returns the meter provider feeding the
	// domain instruments, the prometheus registry to serve at /metrics, the
	// tracer provider, and a combined shutdown that flushes both.
	providers, err := telemetry.Setup(ctx, telemetry.OTLPConfig{
		Endpoint:    cfg.otlpEndpoint,
		Protocol:    cfg.otlpProtocol,
		Insecure:    cfg.otlpInsecure,
		ServiceName: "mecatl",
	})
	if err != nil {
		return fmt.Errorf("setup telemetry: %w", err)
	}
	defer func() {
		shutdownCtx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		defer cancel()
		if serr := providers.Shutdown(shutdownCtx); serr != nil {
			slog.Warn("telemetry shutdown", "err", serr)
		}
	}()
	if cfg.otlpEndpoint == "" {
		slog.Info("tracing disabled (--otlp-endpoint empty); metrics + runtime collector active")
	} else {
		slog.Info("tracing enabled (OTLP exporter installed)", "endpoint", cfg.otlpEndpoint, "protocol", cfg.otlpProtocol, "insecure", cfg.otlpInsecure)
	}

	// Observability: the OTel meter provider feeds the EventSink/Logger adapter;
	// its prometheus exporter renders those series on providers.Registry, served
	// by the /metrics handler. Tracing uses the global OTel TracerProvider
	// installed by telemetry.Setup above (a no-op when tracing is disabled).
	metrics, err := telemetry.NewMetrics(providers.Meter)
	if err != nil {
		return fmt.Errorf("setup metrics: %w", err)
	}
	// Process-RSS gauge (mecatl.process.rss): Linux-only, no-op elsewhere. It
	// rides the same MeterProvider so it renders on /metrics (decision 9).
	if rerr := telemetry.RegisterProcessGauges(providers.Meter); rerr != nil {
		return fmt.Errorf("setup process gauges: %w", rerr)
	}

	// Runtime profiling knobs: arm mutex/block sampling only when explicitly
	// requested (both add overhead; default 0 = off). pprof exposes the resulting
	// profiles at /debug/pprof/{mutex,block} on the loopback admin mux.
	if cfg.mutexProfileFraction > 0 {
		runtime.SetMutexProfileFraction(cfg.mutexProfileFraction)
		slog.Info("mutex profiling enabled", "fraction", cfg.mutexProfileFraction)
	}
	if cfg.blockProfileRate > 0 {
		runtime.SetBlockProfileRate(cfg.blockProfileRate)
		slog.Info("block profiling enabled", "rate_ns", cfg.blockProfileRate)
	}

	// FlightRecorder: arm the bounded execution-trace ring buffer (default ON) so
	// /debug/flightrecorder can snapshot recent activity. Stopped on shutdown so
	// the runtime trace subscription is torn down (goleak-clean).
	var recorder *telemetry.FlightRecorder
	if cfg.flightRecorder {
		// Use the process-singleton accessor: only one flight recorder may be
		// active process-wide (a stdlib constraint), and a second consumer (the
		// embedded server) must coalesce onto it rather than silently lose a Start.
		rec, rerr := telemetry.ProcessFlightRecorder(trace.FlightRecorderConfig{})
		switch {
		case errors.Is(rerr, telemetry.ErrFlightRecorderAlreadyActive):
			// Already armed elsewhere in this process — reuse the shared instance.
			recorder = rec
			slog.Info("flight recorder already active process-wide; reusing the shared instance (loopback /debug/flightrecorder)")
		case rerr != nil:
			slog.Warn("flight recorder failed to start; continuing without it", "err", rerr)
		default:
			recorder = rec
			slog.Info("flight recorder armed (loopback /debug/flightrecorder)")
			defer recorder.Stop()
		}
	}

	tracing := telemetry.NewTracing(otel.GetTracerProvider())
	sink := telemetry.NewSink(metrics, tracing)

	built, err := app.Build(ctx, appConfig(cfg, sink, metrics))
	if err != nil {
		return err
	}
	defer built.Close()

	// ACP mode: serve the Agent Client Protocol over stdio instead of the network
	// daemon. The same engine/service assembly (app.Build) backs it; only the wire
	// surface differs. No TLS/auth/rate-limit — stdio is a local parent-process
	// boundary. Logs still go to stderr (set above), keeping stdout pure JSON-RPC.
	if cfg.acp {
		// session/load (resume) is offered only when a durable session store is
		// configured: the in-memory store would lose snapshots across a restart, so
		// loadSession stays false there.
		return serveACP(ctx, built.Service, cfg.storeDir != "")
	}

	return serve(ctx, cfg, built.Service, providers.Registry, recorder)
}

// appConfig maps the CLI/env config onto the shared app.Config build contract,
// threading the telemetry sink (EventSink) and metrics (Logger) into the engine.
func appConfig(cfg config, sink port.EventSink, logger port.Logger) app.Config {
	return app.Config{
		Workspace:                 cfg.workspace,
		Model:                     cfg.model,
		UseOpenAI:                 cfg.useOpenAI,
		OpenAIBaseURL:             cfg.openAIBaseURL,
		OpenAIKey:                 cfg.openAIKey,
		UseMock:                   cfg.useMock,
		StoreDir:                  cfg.storeDir,
		Shell:                     cfg.shell,
		NoBash:                    cfg.noBash,
		Compaction:                cfg.compaction,
		Tokenizer:                 cfg.tokenizer,
		LLMMaxAttempts:            cfg.llmMaxAttempts,
		LLMPerAttemptTimeout:      cfg.llmPerAttemptTimeout,
		LLMBreakerThreshold:       cfg.llmBreakerThreshold,
		LLMBreakerCooldown:        cfg.llmBreakerCooldown,
		MemoryDir:                 cfg.memoryDir,
		MemoryConsolidateInterval: cfg.memoryConsolidateInterval,
		SkillsDirs:                cfg.skillsDirs,
		SkillsConventional:        cfg.skillsConventional,
		SkillsDraftDir:            cfg.skillsDraftDir,
		SkillsDraftThreshold:      cfg.skillsDraftThreshold,
		AgentsDirs:                cfg.agentsDirs,
		AgentsConventional:        cfg.agentsConventional,
		SubagentModel:             cfg.subagentModel,
		ModelAliases:              cfg.modelAliases,
		CommandsDir:               cfg.commandsDir,
		EnableCommands:            cfg.enableCommands,
		EnableFork:                cfg.enableFork,
		ForkPreservedCap:          cfg.forkPreservedCap,
		EnableRepoMap:             cfg.enableRepoMap,
		EnableTeams:               cfg.enableTeams,
		MCPServers:                cfg.mcpServers,
		MCPResourceTools:          cfg.mcpResourceTools,
		MCPPrompts:                cfg.mcpPrompts,
		ToolHiveEnabled:           cfg.toolHiveEnabled,
		ToolHiveGroup:             cfg.toolHiveGroup,
		PermissionsConventional:   cfg.permissionsConventional,
		ImportClaudePermissions:   cfg.importClaudePermissions,
		TrustProject:              cfg.trustProject,
		PermissionConfigs:         cfg.permissionConfigs,
		AllowAllTools:             cfg.allowAllTools,
		Sink:                      sink,
		Logger:                    logger,
	}
}

// allowAllRefusalReason returns a non-nil error when an allow-all request must
// be refused: running privileged (euid 0) without a declared sandbox.
func allowAllRefusalReason(allowAll bool, euid int, sandbox bool) error {
	if allowAll && euid == 0 && !sandbox {
		return errors.New("--yolo refused: running as root (euid 0) without a declared sandbox; set MECATL_SANDBOX=1 (or IS_SANDBOX=1) to affirm an isolated, disposable environment")
	}
	return nil
}

func sandboxDeclared() bool {
	return os.Getenv("MECATL_SANDBOX") == "1" || os.Getenv("IS_SANDBOX") == "1"
}

func validateAllowAll(cfg config) error {
	return allowAllRefusalReason(cfg.allowAllTools, os.Geteuid(), sandboxDeclared())
}

// parseFlags turns argv into a config, resolving env-derived defaults.
func parseFlags(argv []string) (config, error) {
	fs := flag.NewFlagSet("mecated", flag.ContinueOnError)
	var cfg config

	cwd, _ := os.Getwd()

	fs.StringVar(&cfg.grpcAddr, "grpc-addr", defaultGRPCAddr,
		"gRPC listen address (defaults to loopback; set --auth-token and/or --tls-cert before binding non-loopback)")
	fs.StringVar(&cfg.httpAddr, "http-addr", defaultHTTPAddr,
		"HTTP/SSE listen address (defaults to loopback; set --auth-token and/or --tls-cert before binding non-loopback)")
	fs.StringVar(&cfg.workspace, "workspace", cwd, "default session workspace root")
	fs.StringVar(&cfg.model, "model", "gpt-5", "model identifier sent to the provider")
	fs.BoolVar(&cfg.useOpenAI, "openai", false, "use the OpenAI Responses provider (key from OPENAI_API_KEY)")
	fs.StringVar(&cfg.openAIBaseURL, "openai-base-url", "", "override the OpenAI API base URL (compatible endpoints)")
	fs.BoolVar(&cfg.useMock, "mock", false, "use a canned offline mock provider (no network; for smoke tests only)")
	fs.StringVar(&cfg.storeDir, "store-dir", "", "directory for the JSONL session store (empty -> in-memory store)")
	fs.StringVar(&cfg.shell, "shell", "/bin/sh", "shell used to execute Bash-tool commands; empty disables Bash (shell-less mode)")
	fs.BoolVar(&cfg.noBash, "no-bash", false, "disable the Bash tool entirely (shell-less mode); overrides --shell")

	fs.StringVar(&cfg.compaction, "compaction", "heuristic", "compaction strategy: \"heuristic\" (default, single-summary) or \"cascade\" (tiered snip→strip→collapse→summarize)")
	fs.StringVar(&cfg.tokenizer, "tokenizer", "heuristic", "token counter for the compaction trigger: \"heuristic\" (default, dependency-free) or \"tiktoken\" (offline tiktoken vocab)")

	fs.IntVar(&cfg.llmMaxAttempts, "llm-max-attempts", 3, "max LLM stream-establish attempts (initial call plus retries)")
	fs.DurationVar(&cfg.llmPerAttemptTimeout, "llm-per-attempt-timeout", 30*time.Second, "per-attempt timeout for establishing an LLM stream (0 disables)")
	fs.IntVar(&cfg.llmBreakerThreshold, "llm-breaker-threshold", 5, "consecutive LLM failures that open the circuit breaker (0 disables)")
	fs.DurationVar(&cfg.llmBreakerCooldown, "llm-breaker-cooldown", 30*time.Second, "how long the LLM circuit breaker stays open before half-opening")

	fs.StringVar(&cfg.metricsAddr, "metrics-addr", defaultMetricsAddr, "Prometheus /metrics listen address (empty disables the metrics endpoint)")

	fs.StringVar(&cfg.otlpEndpoint, "otlp-endpoint", "", "OTLP trace collector endpoint, e.g. localhost:4317 (empty disables tracing)")
	fs.StringVar(&cfg.otlpProtocol, "otlp-protocol", telemetry.ProtocolGRPC, "OTLP transport: \"grpc\" (default) or \"http\"")
	fs.BoolVar(&cfg.otlpInsecure, "otlp-insecure", false, "skip TLS when dialing the OTLP collector (development only)")

	fs.IntVar(&cfg.mutexProfileFraction, "mutex-profile-fraction", 0, "runtime.SetMutexProfileFraction: report 1/N mutex contention events for /debug/pprof/mutex. 0 (default) disables it. Adds per-contention sampling overhead; enable only when investigating lock contention")
	fs.IntVar(&cfg.blockProfileRate, "block-profile-rate", 0, "runtime.SetBlockProfileRate in nanoseconds: sample one blocking event per N ns blocked for /debug/pprof/block. 0 (default) disables it. Adds per-block-event overhead; enable only when investigating blocking")
	fs.BoolVar(&cfg.flightRecorder, "flight-recorder", true, "arm the execution-trace FlightRecorder (bounded in-memory ring buffer) so /debug/flightrecorder can snapshot recent activity. ON by default (low, bounded overhead). Pass --flight-recorder=false to disable")

	fs.StringVar(&cfg.memoryDir, "memory-dir", "", "per-project memory store directory (empty disables the Remember/Recall tools)")
	fs.DurationVar(&cfg.memoryConsolidateInterval, "memory-consolidate-interval", 0, "interval for background memory consolidation (dream); 0 disables. Only meaningful with --memory-dir")

	fs.Var(&cfg.skillsDirs, "skills-dir", "directory to discover progressive-disclosure skills from, laid out as <name>/SKILL.md (repeatable; highest precedence); empty disables the Skill tool unless --skills-conventional is set. TRUST BOUNDARY: a SKILL.md steers the model like AGENTS.md/CLAUDE.md — point this only at directories you trust")
	fs.BoolVar(&cfg.skillsConventional, "skills-conventional", false, "also discover skills from the conventional locations: <workspace>/"+skills.ProjectDirMecatl+", <workspace>/"+skills.ProjectDirClaude+", $XDG_CONFIG_HOME/mecatl/skills (or ~/.config/mecatl/skills), and ~/.claude/skills (lower precedence than --skills-dir). Default OFF — opt in only for trusted locations (same trust class as AGENTS.md/CLAUDE.md)")

	fs.StringVar(&cfg.skillsDraftDir, "skills-draft-dir", "", "enable the writable SkillDraft tool and set the QUARANTINE directory for model-authored candidate skills. Empty disables the tool. TRUST BOUNDARY: must be OUTSIDE the workspace root (so the model's workspace-confined Write/Edit cannot reach it; fatal otherwise) and disjoint from every --skills-dir (fatal on overlap). Drafts are quarantined (never live); an operator reviews and promotes one with `mecated skills promote --skills-draft-dir <dir> --skills-dir <active> <name>`")
	fs.Float64Var(&cfg.skillsDraftThreshold, "skills-draft-similarity-threshold", skills.DefaultSimilarityThreshold, "2-gram Jaccard similarity above which a SkillDraft warns of a near-duplicate existing skill (warn-only, does not block)")

	fs.Var(&cfg.agentsDirs, "agents-dir", "directory to discover named agent definitions (subagent specialists) from, laid out as <name>.md with YAML frontmatter (repeatable; highest precedence). A def is reusable as a Task delegate (Task(agent=<name>)) and as a team-member role (AgentType). TRUST BOUNDARY: a def body steers the model like AGENTS.md/CLAUDE.md — point this only at directories you trust")
	fs.BoolVar(&cfg.agentsConventional, "agents-conventional", true, "also discover agent definitions from the conventional locations: <workspace>/"+agents.ProjectDirMecatl+", <workspace>/"+agents.ProjectDirClaude+", $XDG_CONFIG_HOME/mecatl/agents (or ~/.config/mecatl/agents), and ~/.claude/agents (lower precedence than --agents-dir). ON by default and INERT when no such dir exists (like teams/fork). Pass --agents-conventional=false to disable. TRUST BOUNDARY: same trust class as AGENTS.md/CLAUDE.md")
	fs.StringVar(&cfg.subagentModel, "subagent-model", "", "global model override applied to every Task/team-member child that does not pin its own model in its definition (the analogue of CLAUDE_CODE_SUBAGENT_MODEL). May be a concrete id or an alias from --model-alias. Empty inherits the parent --model")
	fs.Var(&cfg.modelAliases, "model-alias", "model alias mapping as name=model-id (repeatable), e.g. --model-alias fast=gpt-4o-mini. Aliases are resolved only in the composition layer; an agent def's `model: <alias>` resolves through this map (then the built-in sonnet/opus/haiku aliases)")

	fs.StringVar(&cfg.commandsDir, "commands-dir", "", "directory of slash-command templates (<name>.md); setting it enables command expansion. Empty + --enable-commands uses the defaults (.mecatl/commands, .claude/commands)")
	fs.BoolVar(&cfg.enableCommands, "enable-commands", false, "enable slash-command expansion using the default directories (.mecatl/commands, .claude/commands) when --commands-dir is empty")

	fs.BoolVar(&cfg.enableFork, "enable-fork", true, "register the Fork fan-out tool (parallel isolated child branches)")
	fs.IntVar(&cfg.forkPreservedCap, "fork-preserved-cap", agent.DefaultPreservedForkCap, "max PRESERVED winner forks (join=first/judge) kept on disk at once; the oldest beyond this is LRU-reaped. Preserved forks stay inspectable until reaped")
	fs.BoolVar(&cfg.enableRepoMap, "enable-repomap", false, "register the Aider-style repo-map tool. OFF by default: the WASM tree-sitter binding leaks and hangs after ~160 files, freezing the in-process TUI (see docs/design/REPOMAP-TREE-SITTER.md). Pass --enable-repomap to opt in until the extraction is reworked")
	fs.BoolVar(&cfg.enableTeams, "enable-teams", true, "register the experimental agent-teams capability (CreateTeam/SpawnTeammate/RunTeam); on by default and inert until a client drives a team. Pass --enable-teams=false to disable")

	fs.Var(&cfg.mcpServers, "mcp-server", "remote MCP server as name=URL (repeatable); auth token read from MCP_<NAME>_TOKEN")
	fs.BoolVar(&cfg.mcpResourceTools, "mcp-resource-tools", true, "register the ListMcpResources/ReadMcpResource meta-tools when a connected MCP server exposes resources (no-op when none do). TRUST BOUNDARY: a remote resource's contents enter the model context like any other MCP output — enable only for servers you trust")
	fs.BoolVar(&cfg.mcpPrompts, "mcp-prompts", true, "expand \"/mcp__<server>__<prompt> key=value\" inputs into the server-rendered prompt (static snapshot taken at connect). TRUST BOUNDARY: an MCP prompt steers the model like a slash command — enable only for servers you trust")

	fs.BoolVar(&cfg.toolHiveEnabled, "toolhive", true, "discover MCP servers from the running ToolHive workloads (the embedded ToolHive library lists already-running workloads and reads their HTTP proxy URLs; mecatl NEVER starts or spawns a workload). Fails soft to zero servers when no container runtime is reachable. TRUST BOUNDARY: registering tools from running workloads is the same trust class as --mcp-server — every discovered workload's tools enter the model context")
	fs.StringVar(&cfg.toolHiveGroup, "toolhive-group", "", "ToolHive group to discover workloads from (empty -> the \"default\" group). Only consulted when --toolhive is set")

	fs.Var(&cfg.permissionConfigs, "permission-config", "path to a YAML permission-config file (.mecatl/settings.yaml schema: a permissions.{allow,ask,deny} list of \"Tool(pattern)\" specs) to load at the CLI scope — the HIGHEST config precedence, fully trusted (repeatable). Always loaded regardless of --permissions-conventional. A CLI rule out-ranks a project/user rule of the same effect; a config allow can LOOSEN ONLY the built-in Bash/Edit/Write ask, but a deny/ask in ANY scope still wins and a config allow never suppresses a configured ask")
	fs.BoolVar(&cfg.permissionsConventional, "permissions-conventional", true, "auto-discover the per-project permission config: <workspace>/.mecatl/settings.local.yaml (gitignored, personal — higher precedence) and <workspace>/.mecatl/settings.yaml (checked-in, shared), plus — with --import-claude-permissions — the matching .claude/settings.local.json and .claude/settings.json, plus the user-global file ($XDG_CONFIG_HOME/mecatl/settings.yaml). RE-RESOLVED PER SESSION against each session's workspace root (and revalidated on file mtime change), so two sessions in different repos get different decisions. ON by default and INERT when no such file exists. TRUST BOUNDARY: a project's ALLOW rules are honoured ONLY with --trust-project; its deny/ask rules are ALWAYS honoured")
	fs.BoolVar(&cfg.importClaudePermissions, "import-claude-permissions", false, "also import Claude-Code settings.json permissions (project <workspace>/.claude/settings{,.local}.json and user ~/.claude/settings.json) when --permissions-conventional is set. LOSSY (fail-safe): a WebFetch(domain:...) ALLOW is DEMOTED to ask, a Read(~/...) rule is left INERT (\"~\" unexpanded), an unparseable spec is DROPPED — every case is logged")
	fs.BoolVar(&cfg.trustProject, "trust-project", false, "honour a discovered PROJECT's ALLOW rules (its deny/ask rules are always honoured regardless). Default OFF (the safe stance): an untrusted repo's permission grants are ignored. TRUST BOUNDARY: enabling this lets a checked-in .mecatl/settings.yaml auto-approve tool calls — only pass it for a repo you trust")
	fs.BoolVar(&cfg.allowAllTools, "yolo", false,
		"OPERATOR POSTURE (dangerous): suppress permission prompts for the built-in mutate-ask floor server-wide, for ephemeral/sandboxed single-tenant use only. A Deny in ANY scope and any DELIBERATELY configured Ask still apply (see docs/design/ALLOW-ALL-POSTURE.md). Refused when running as root (euid 0) unless MECATL_SANDBOX=1 (or IS_SANDBOX=1) declares an isolated environment.")

	fs.BoolVar(&cfg.acp, "acp", false, "serve the Agent Client Protocol (ACP) over stdio for an editor that spawned mecated as a subprocess (JSON-RPC 2.0 on stdin/stdout). Skips the TCP/HTTP listeners; the single session workspace is the editor-provided cwd. No TLS/auth/rate-limit (stdio is a local, parent-process trust boundary)")

	fs.StringVar(&cfg.authToken, "auth-token", "", "bearer token required on every gRPC/HTTP request (or MECATL_AUTH_TOKEN; empty disables auth)")
	fs.StringVar(&cfg.tlsCert, "tls-cert", "", "PEM server certificate; with --tls-key enables TLS on the gRPC + HTTP servers")
	fs.StringVar(&cfg.tlsKey, "tls-key", "", "PEM server private key (paired with --tls-cert)")
	fs.StringVar(&cfg.clientCA, "client-ca", "", "PEM client-CA bundle; enables mutual TLS (require + verify client certs)")
	fs.Float64Var(&cfg.rateLimit, "rate-limit", 0, "sustained per-client request rate in req/s (0 disables rate limiting)")
	fs.IntVar(&cfg.rateBurst, "rate-burst", 0, "rate-limit token-bucket burst size (0 derives a sane default from --rate-limit)")

	if err := fs.Parse(argv); err != nil {
		return config{}, err
	}

	cfg.openAIKey = os.Getenv("OPENAI_API_KEY")
	// An API key in the environment implies the user wants the real provider.
	if cfg.openAIKey != "" {
		cfg.useOpenAI = true
	}
	// An auth token from the environment is honored when the flag is unset, so a
	// secret need not appear in the process argv.
	if cfg.authToken == "" {
		cfg.authToken = os.Getenv("MECATL_AUTH_TOKEN")
	}
	return cfg, nil
}

// serve starts the gRPC and HTTP servers (and, when --metrics-addr is set, the
// loopback admin endpoint — /metrics plus the pprof/expvar/FlightRecorder
// runtime-introspection surface — on its own listener) concurrently and blocks
// until ctx is cancelled (a signal) or a server fails, then shuts them all down
// gracefully. recorder may be nil (FlightRecorder disabled), in which case
// /debug/flightrecorder is not mounted.
//
// The harness API is protected by the server.Authenticator (bearer auth + rate
// limiting, both off by default) and optionally by TLS / mutual TLS. The
// liveness/readiness probes and the standard gRPC health service are mounted
// OUTSIDE the auth/rate-limit layer so orchestrators can probe without
// credentials.
func serve(ctx context.Context, cfg config, svc *server.Service, reg *prometheus.Registry, recorder *telemetry.FlightRecorder) error {
	tlsCfg, err := buildTLSConfig(cfg)
	if err != nil {
		return err
	}

	auth := server.NewAuthenticator(server.SecurityConfig{
		AuthToken: cfg.authToken,
		RateLimit: cfg.rateLimit,
		RateBurst: cfg.rateBurst,
	})
	logSecurityPosture(cfg, tlsCfg)

	// --- gRPC: auth+rate interceptors, standard health service ---
	grpcOpts := []grpc.ServerOption{
		grpc.UnaryInterceptor(auth.UnaryInterceptor()),
		grpc.StreamInterceptor(auth.StreamInterceptor()),
	}
	if tlsCfg != nil {
		grpcOpts = append(grpcOpts, grpc.Creds(credentials.NewTLS(tlsCfg)))
	}
	grpcSrv := grpc.NewServer(grpcOpts...)
	mecatlv1.RegisterHarnessServiceServer(grpcSrv, server.NewHarnessServer(svc))
	healthSrv := health.NewServer()
	healthpb.RegisterHealthServer(grpcSrv, healthSrv)
	healthSrv.SetServingStatus("", healthpb.HealthCheckResponse_SERVING)
	healthSrv.SetServingStatus("mecatl.v1.HarnessService", healthpb.HealthCheckResponse_SERVING)

	// --- HTTP: health endpoints mounted OUTSIDE auth/rate-limit; the API mux
	// wrapped in the auth middleware. The readiness probe reports ready as soon
	// as the engine/service are wired (they are, by the time serve runs). ---
	httpMux := http.NewServeMux()
	server.NewHealthHandler(func() bool { return true }).RegisterHealth(httpMux)
	httpMux.Handle("/", auth.Middleware(server.NewHTTPHandler(svc)))
	httpSrv := &http.Server{
		Addr:              cfg.httpAddr,
		Handler:           httpMux,
		ReadHeaderTimeout: 10 * time.Second,
		TLSConfig:         tlsCfg,
	}

	// The admin endpoint runs on a separate loopback listener: it carries
	// /metrics (read-only, secret-free) PLUS the runtime-introspection surface —
	// pprof, expvar (/debug/vars), and the FlightRecorder snapshot
	// (/debug/flightrecorder). An empty --metrics-addr disables the whole mux.
	//
	// SECURITY: pprof/FlightRecorder/expvar output can embed prompt text, file
	// paths, and goroutine stacks. This listener is loopback-bound by default and
	// MUST stay loopback — these endpoints are never mounted on the public
	// gRPC/HTTP service surface (decision 6 in docs/design/perf-observability.md).
	var metricsSrv *http.Server
	if cfg.metricsAddr != "" {
		metricsSrv = &http.Server{
			Addr:              cfg.metricsAddr,
			Handler:           newAdminMux(reg, recorder),
			ReadHeaderTimeout: 10 * time.Second,
		}
	}

	authed := auth != nil && (cfg.authToken != "" || tlsCfg != nil)
	warnIfNonLoopback("grpc-addr", cfg.grpcAddr, authed)
	warnIfNonLoopback("http-addr", cfg.httpAddr, authed)

	grpcLis, err := net.Listen("tcp", cfg.grpcAddr)
	if err != nil {
		return fmt.Errorf("listen grpc %q: %w", cfg.grpcAddr, err)
	}

	errCh := make(chan error, 3)

	go func() {
		slog.Info("gRPC server listening", "addr", grpcLis.Addr().String())
		if serveErr := grpcSrv.Serve(grpcLis); serveErr != nil && !errors.Is(serveErr, grpc.ErrServerStopped) {
			errCh <- fmt.Errorf("grpc serve: %w", serveErr)
		}
	}()

	go func() {
		slog.Info("HTTP/SSE server listening", "addr", cfg.httpAddr, "tls", tlsCfg != nil)
		// ListenAndServeTLS with empty cert/key paths uses the certificate already
		// loaded into TLSConfig.Certificates by buildTLSConfig.
		var serveErr error
		if tlsCfg != nil {
			serveErr = httpSrv.ListenAndServeTLS("", "")
		} else {
			serveErr = httpSrv.ListenAndServe()
		}
		if serveErr != nil && !errors.Is(serveErr, http.ErrServerClosed) {
			errCh <- fmt.Errorf("http serve: %w", serveErr)
		}
	}()

	if metricsSrv != nil {
		go func() {
			slog.Info("admin server listening (loopback)", "addr", cfg.metricsAddr, "paths", "/metrics /debug/pprof /debug/vars /debug/flightrecorder")
			if serveErr := metricsSrv.ListenAndServe(); serveErr != nil && !errors.Is(serveErr, http.ErrServerClosed) {
				errCh <- fmt.Errorf("metrics serve: %w", serveErr)
			}
		}()
	} else {
		slog.Info("metrics endpoint DISABLED (--metrics-addr empty)")
	}

	select {
	case <-ctx.Done():
		slog.Info("shutdown signal received; stopping servers")
	case err := <-errCh:
		slog.Error("server failed; shutting down", "err", err)
		shutdown(grpcSrv, httpSrv, metricsSrv)
		return err
	}

	shutdown(grpcSrv, httpSrv, metricsSrv)
	return nil
}

// newAdminMux builds the loopback admin mux: /metrics (read-only, secret-free)
// plus the runtime-introspection surface — pprof, expvar (/debug/vars), and,
// when recorder is non-nil, the FlightRecorder snapshot (/debug/flightrecorder).
// It is extracted from serve so a test can drive the REAL mux construction (a
// parallel hand-rolled mux in a test would not catch a deleted Handle here).
//
// recorder may be nil (FlightRecorder disabled), in which case
// /debug/flightrecorder is NOT mounted and a GET returns 404.
//
// SECURITY: pprof/FlightRecorder/expvar output can embed prompt text, file
// paths, and goroutine stacks. The returned mux MUST be served only on a
// loopback-bound listener — never on the public gRPC/HTTP service surface
// (decision 6 in docs/design/perf-observability.md).
func newAdminMux(reg *prometheus.Registry, recorder *telemetry.FlightRecorder) *http.ServeMux {
	mux := http.NewServeMux()
	mux.Handle("/metrics", telemetry.MetricsHandler(reg))
	telemetry.RegisterPprof(mux)
	mux.Handle("/debug/vars", telemetry.ExpvarHandler())
	if recorder != nil {
		mux.HandleFunc("/debug/flightrecorder", func(w http.ResponseWriter, _ *http.Request) {
			w.Header().Set("Content-Type", "application/octet-stream")
			if _, err := recorder.Snapshot(w); err != nil {
				http.Error(w, "flight recorder snapshot: "+err.Error(), http.StatusServiceUnavailable)
			}
		})
	}
	return mux
}

// warnIfNonLoopback logs the API trust assumption for the given bind address.
// Loopback binds are logged at info. A non-loopback bind WITH authentication
// (bearer token and/or TLS, indicated by authed) is logged at info; a
// non-loopback bind with NO authentication is logged as a prominent WARNING,
// since it exposes command/file execution to the network. It never hard-fails:
// an operator may legitimately front the server with a service mesh.
func warnIfNonLoopback(flagName, addr string, authed bool) {
	host, _, err := net.SplitHostPort(addr)
	if err != nil {
		host = addr
	}
	ip := net.ParseIP(host)
	loopback := host == "localhost" || (ip != nil && ip.IsLoopback())
	if loopback {
		slog.Info("API bound to loopback (single-user localhost trust model)",
			"flag", flagName, "addr", addr, "authenticated", authed)
		return
	}
	if authed {
		slog.Info("API bound to a non-loopback address WITH authentication (bearer token and/or TLS)",
			"flag", flagName, "addr", addr)
		return
	}
	slog.Warn("API bound to a NON-loopback address with NO authentication: it exposes UNAUTHENTICATED command/file execution to the network — set --auth-token / --tls-cert (or front it with a trusted mesh) before doing this",
		"flag", flagName, "addr", addr)
}

// buildTLSConfig assembles the *tls.Config for the gRPC + HTTP servers from the
// TLS flags. It returns nil (plaintext) when neither --tls-cert nor --tls-key is
// set. --tls-cert and --tls-key must be supplied together. When --client-ca is
// set it enables mutual TLS: the server requires and verifies a client
// certificate signed by the given CA bundle.
func buildTLSConfig(cfg config) (*tls.Config, error) {
	if cfg.tlsCert == "" && cfg.tlsKey == "" {
		if cfg.clientCA != "" {
			return nil, errors.New("--client-ca requires --tls-cert/--tls-key (mTLS needs server TLS)")
		}
		return nil, nil
	}
	if cfg.tlsCert == "" || cfg.tlsKey == "" {
		return nil, errors.New("--tls-cert and --tls-key must be supplied together")
	}
	cert, err := tls.LoadX509KeyPair(cfg.tlsCert, cfg.tlsKey)
	if err != nil {
		return nil, fmt.Errorf("load TLS keypair: %w", err)
	}
	tlsCfg := &tls.Config{
		Certificates: []tls.Certificate{cert},
		MinVersion:   tls.VersionTLS12,
	}
	if cfg.clientCA != "" {
		caPEM, err := os.ReadFile(cfg.clientCA)
		if err != nil {
			return nil, fmt.Errorf("read client CA: %w", err)
		}
		pool := x509.NewCertPool()
		if !pool.AppendCertsFromPEM(caPEM) {
			return nil, fmt.Errorf("client CA %q: no certificates parsed", cfg.clientCA)
		}
		tlsCfg.ClientCAs = pool
		tlsCfg.ClientAuth = tls.RequireAndVerifyClientCert
	}
	return tlsCfg, nil
}

// logSecurityPosture logs the effective authentication / transport / rate-limit
// posture once at startup so an operator can confirm what is enabled.
func logSecurityPosture(cfg config, tlsCfg *tls.Config) {
	mtls := tlsCfg != nil && tlsCfg.ClientAuth == tls.RequireAndVerifyClientCert
	slog.Info("API security posture",
		"bearer_auth", cfg.authToken != "",
		"tls", tlsCfg != nil,
		"mutual_tls", mtls,
		"rate_limit_rps", cfg.rateLimit,
		"rate_burst", cfg.rateBurst,
		"store", storeKind(cfg))
}

// storeKind returns a short label for the configured session store, noting the
// auto-resume implication of an in-memory store.
func storeKind(cfg config) string {
	if cfg.storeDir == "" {
		return "memory (no resume across restart)"
	}
	return "jsonl (resumable across restart)"
}

// shutdown gracefully stops the servers, bounding the HTTP drains with a
// timeout. metricsSrv may be nil when the /metrics endpoint is disabled.
func shutdown(grpcSrv *grpc.Server, httpSrv, metricsSrv *http.Server) {
	shutdownCtx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	if err := httpSrv.Shutdown(shutdownCtx); err != nil {
		slog.Warn("http graceful shutdown", "err", err)
	}
	if metricsSrv != nil {
		if err := metricsSrv.Shutdown(shutdownCtx); err != nil {
			slog.Warn("metrics graceful shutdown", "err", err)
		}
	}
	grpcSrv.GracefulStop()
}
