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
	"github.com/stacklok/mecatl/internal/adapter/mcp"
	"github.com/stacklok/mecatl/internal/adapter/server"
	"github.com/stacklok/mecatl/internal/adapter/skills"
	"github.com/stacklok/mecatl/internal/adapter/telemetry"
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

	// Memory: per-project memory store directory (empty disables memory tools).
	memoryDir string

	// Skills: explicit directories of progressive-disclosure skill units laid out
	// as <dir>/<name>/SKILL.md (repeatable; highest precedence). Empty + no
	// conventional set disables the Skill tool. skillsConventional adds the
	// built-in conventional project/user locations (lower precedence), default OFF
	// to keep skills strictly opt-in (a trust boundary — see resolve.go / usage.md).
	skillsDirs         stringList
	skillsConventional bool

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

	// ToolHive: discover MCP servers from the running ToolHive workloads (the
	// embedded ToolHive library lists already-running workloads and reads their
	// HTTP proxy URLs — mecatl never spawns a workload). Default ON; it fails soft
	// to zero servers when no container runtime is reachable.
	toolHiveEnabled bool
	// toolHiveGroup is the ToolHive group to discover from (empty -> "default").
	toolHiveGroup string
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

	ctx, stop := signal.NotifyContext(context.Background(), syscall.SIGINT, syscall.SIGTERM)
	defer stop()

	// Observability: install the OTLP trace exporter + global TracerProvider
	// BEFORE building the tracing sink below, so NewTracing(otel.GetTracerProvider())
	// picks up the installed provider. An empty --otlp-endpoint disables tracing.
	traceShutdown, err := telemetry.Setup(ctx, telemetry.OTLPConfig{
		Endpoint:    cfg.otlpEndpoint,
		Protocol:    cfg.otlpProtocol,
		Insecure:    cfg.otlpInsecure,
		ServiceName: "mecatl",
	})
	if err != nil {
		return fmt.Errorf("setup tracing: %w", err)
	}
	defer func() {
		shutdownCtx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		defer cancel()
		if serr := traceShutdown(shutdownCtx); serr != nil {
			slog.Warn("tracing shutdown", "err", serr)
		}
	}()
	if cfg.otlpEndpoint == "" {
		slog.Info("tracing disabled (--otlp-endpoint empty)")
	} else {
		slog.Info("tracing enabled (OTLP exporter installed)", "endpoint", cfg.otlpEndpoint, "protocol", cfg.otlpProtocol, "insecure", cfg.otlpInsecure)
	}

	// Observability: a private Prometheus registry feeds both the EventSink/Logger
	// adapter and the /metrics handler. Tracing uses the global OTel TracerProvider
	// installed by telemetry.Setup above (a no-op when tracing is disabled), so the
	// sink emits to the OTLP exporter when an endpoint is set.
	reg := prometheus.NewRegistry()
	metrics := telemetry.NewMetrics(reg)
	tracing := telemetry.NewTracing(otel.GetTracerProvider())
	sink := telemetry.NewSink(metrics, tracing)

	built, err := app.Build(ctx, appConfig(cfg, sink, metrics))
	if err != nil {
		return err
	}
	defer built.Close()

	return serve(ctx, cfg, built.Service, reg)
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
		CommandsDir:               cfg.commandsDir,
		EnableCommands:            cfg.enableCommands,
		EnableFork:                cfg.enableFork,
		EnableRepoMap:             cfg.enableRepoMap,
		EnableTeams:               cfg.enableTeams,
		MCPServers:                cfg.mcpServers,
		MCPResourceTools:          cfg.mcpResourceTools,
		MCPPrompts:                cfg.mcpPrompts,
		ToolHiveEnabled:           cfg.toolHiveEnabled,
		ToolHiveGroup:             cfg.toolHiveGroup,
		Sink:                      sink,
		Logger:                    logger,
	}
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

	fs.StringVar(&cfg.memoryDir, "memory-dir", "", "per-project memory store directory (empty disables the Remember/Recall tools)")
	fs.DurationVar(&cfg.memoryConsolidateInterval, "memory-consolidate-interval", 0, "interval for background memory consolidation (dream); 0 disables. Only meaningful with --memory-dir")

	fs.Var(&cfg.skillsDirs, "skills-dir", "directory to discover progressive-disclosure skills from, laid out as <name>/SKILL.md (repeatable; highest precedence); empty disables the Skill tool unless --skills-conventional is set. TRUST BOUNDARY: a SKILL.md steers the model like AGENTS.md/CLAUDE.md — point this only at directories you trust")
	fs.BoolVar(&cfg.skillsConventional, "skills-conventional", false, "also discover skills from the conventional locations: <workspace>/"+skills.ProjectDirMecatl+", <workspace>/"+skills.ProjectDirClaude+", $XDG_CONFIG_HOME/mecatl/skills (or ~/.config/mecatl/skills), and ~/.claude/skills (lower precedence than --skills-dir). Default OFF — opt in only for trusted locations (same trust class as AGENTS.md/CLAUDE.md)")

	fs.StringVar(&cfg.skillsDraftDir, "skills-draft-dir", "", "enable the writable SkillDraft tool and set the QUARANTINE directory for model-authored candidate skills. Empty disables the tool. TRUST BOUNDARY: must be OUTSIDE the workspace root (so the model's workspace-confined Write/Edit cannot reach it; fatal otherwise) and disjoint from every --skills-dir (fatal on overlap). Drafts are quarantined (never live); an operator reviews and promotes one with `mecated skills promote --skills-draft-dir <dir> --skills-dir <active> <name>`")
	fs.Float64Var(&cfg.skillsDraftThreshold, "skills-draft-similarity-threshold", skills.DefaultSimilarityThreshold, "2-gram Jaccard similarity above which a SkillDraft warns of a near-duplicate existing skill (warn-only, does not block)")

	fs.StringVar(&cfg.commandsDir, "commands-dir", "", "directory of slash-command templates (<name>.md); setting it enables command expansion. Empty + --enable-commands uses the defaults (.mecatl/commands, .claude/commands)")
	fs.BoolVar(&cfg.enableCommands, "enable-commands", false, "enable slash-command expansion using the default directories (.mecatl/commands, .claude/commands) when --commands-dir is empty")

	fs.BoolVar(&cfg.enableFork, "enable-fork", true, "register the Fork fan-out tool (parallel isolated child branches)")
	fs.BoolVar(&cfg.enableRepoMap, "enable-repomap", true, "register the Aider-style repo-map tool (CGO-free, tree-sitter via WebAssembly)")
	fs.BoolVar(&cfg.enableTeams, "enable-teams", true, "register the experimental agent-teams capability (CreateTeam/SpawnTeammate/RunTeam); on by default and inert until a client drives a team. Pass --enable-teams=false to disable")

	fs.Var(&cfg.mcpServers, "mcp-server", "remote MCP server as name=URL (repeatable); auth token read from MCP_<NAME>_TOKEN")
	fs.BoolVar(&cfg.mcpResourceTools, "mcp-resource-tools", true, "register the ListMcpResources/ReadMcpResource meta-tools when a connected MCP server exposes resources (no-op when none do). TRUST BOUNDARY: a remote resource's contents enter the model context like any other MCP output — enable only for servers you trust")
	fs.BoolVar(&cfg.mcpPrompts, "mcp-prompts", true, "expand \"/mcp__<server>__<prompt> key=value\" inputs into the server-rendered prompt (static snapshot taken at connect). TRUST BOUNDARY: an MCP prompt steers the model like a slash command — enable only for servers you trust")

	fs.BoolVar(&cfg.toolHiveEnabled, "toolhive", true, "discover MCP servers from the running ToolHive workloads (the embedded ToolHive library lists already-running workloads and reads their HTTP proxy URLs; mecatl NEVER starts or spawns a workload). Fails soft to zero servers when no container runtime is reachable. TRUST BOUNDARY: registering tools from running workloads is the same trust class as --mcp-server — every discovered workload's tools enter the model context")
	fs.StringVar(&cfg.toolHiveGroup, "toolhive-group", "", "ToolHive group to discover workloads from (empty -> the \"default\" group). Only consulted when --toolhive is set")

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
// Prometheus /metrics endpoint on its own listener) concurrently and blocks
// until ctx is cancelled (a signal) or a server fails, then shuts them all down
// gracefully.
//
// The harness API is protected by the server.Authenticator (bearer auth + rate
// limiting, both off by default) and optionally by TLS / mutual TLS. The
// liveness/readiness probes and the standard gRPC health service are mounted
// OUTSIDE the auth/rate-limit layer so orchestrators can probe without
// credentials.
func serve(ctx context.Context, cfg config, svc *server.Service, reg *prometheus.Registry) error {
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

	// The /metrics endpoint runs on a separate loopback listener: it is a
	// read-only, secret-free surface kept apart from the harness API. An empty
	// --metrics-addr disables it.
	var metricsSrv *http.Server
	if cfg.metricsAddr != "" {
		mux := http.NewServeMux()
		mux.Handle("/metrics", telemetry.MetricsHandler(reg))
		metricsSrv = &http.Server{
			Addr:              cfg.metricsAddr,
			Handler:           mux,
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
			slog.Info("metrics server listening", "addr", cfg.metricsAddr, "path", "/metrics")
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
