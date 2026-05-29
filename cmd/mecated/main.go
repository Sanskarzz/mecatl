// Command mecated is the mecatl server binary and the composition root: the one
// place where concrete adapters are wired to the ports the agent loop consumes.
//
// It builds an LLM provider (OpenAI Responses, or a canned mock for smoke
// tests), the core tool catalog (with an OPTIONAL Bash tool, gated on a
// configured shell) plus a read-only Task subagent, the permission policy,
// lifecycle hooks, the session store, and the two-layer system prompt;
// assembles them into an agent.Engine; and serves the resulting HarnessService
// over gRPC and HTTP/SSE concurrently, with graceful shutdown on SIGINT/SIGTERM.
package main

import (
	"context"
	"crypto/tls"
	"crypto/x509"
	"errors"
	"flag"
	"fmt"
	"log/slog"
	"net"
	"net/http"
	"os"
	"os/signal"
	"runtime"
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
	"github.com/stacklok/mecatl/internal/adapter/dream"
	"github.com/stacklok/mecatl/internal/adapter/forker"
	"github.com/stacklok/mecatl/internal/adapter/hookexec"
	"github.com/stacklok/mecatl/internal/adapter/llmresilience"
	"github.com/stacklok/mecatl/internal/adapter/mcp"
	"github.com/stacklok/mecatl/internal/adapter/memory"
	"github.com/stacklok/mecatl/internal/adapter/mockllm"
	"github.com/stacklok/mecatl/internal/adapter/openai"
	"github.com/stacklok/mecatl/internal/adapter/osfs"
	"github.com/stacklok/mecatl/internal/adapter/permpolicy"
	"github.com/stacklok/mecatl/internal/adapter/repomap"
	"github.com/stacklok/mecatl/internal/adapter/server"
	"github.com/stacklok/mecatl/internal/adapter/store/jsonlstore"
	"github.com/stacklok/mecatl/internal/adapter/store/memstore"
	"github.com/stacklok/mecatl/internal/adapter/telemetry"
	"github.com/stacklok/mecatl/internal/adapter/tokenizer"
	"github.com/stacklok/mecatl/internal/adapter/tools"
	"github.com/stacklok/mecatl/internal/agent"
	"github.com/stacklok/mecatl/internal/governance"
	"github.com/stacklok/mecatl/internal/port"
	"github.com/stacklok/mecatl/internal/prompt"
	"github.com/stacklok/mecatl/internal/session"
	"github.com/stacklok/mecatl/internal/tool"
)

// defaultContextWindowTokens is the model context window the loop uses to decide
// when to compact. A conservative default that suits the common GPT-class models.
const defaultContextWindowTokens = 128_000

// defaultCompactionRatio is the agent loop's compaction TRIGGER fraction (0.8 of
// the context window).
const defaultCompactionRatio = 0.8

// defaultCompactionTargetRatio is the fraction the cascade compactor reduces the
// history TOWARD — deliberately below defaultCompactionRatio so there is
// hysteresis between the trigger and the target. Without this gap a head/tail-heavy
// history could re-trip the trigger (and a tier-4 LLM summary) on every turn.
const defaultCompactionTargetRatio = 0.6

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

// Default session stop limits. A zero Limits value disables every stop condition
// in package session, so the composition root supplies these non-zero defaults
// to ensure a session created without explicit limits is still bounded.
const (
	defaultMaxTurns               = 50
	defaultMaxToolCalls           = 200
	defaultMaxConsecutiveFailures = 5
)

// LLM resilience backoff bounds. These are not exposed as flags (the attempt
// count, per-attempt timeout, and breaker knobs are): exponential backoff
// between BaseBackoff and MaxBackoff is a sensible fixed envelope.
const (
	llmBaseBackoff = 200 * time.Millisecond
	llmMaxBackoff  = 10 * time.Second
)

// defaultMetricsAddr is the loopback listen address for the Prometheus /metrics
// endpoint. Unlike the harness API it is read-only and carries no secrets, but
// it is still bound to loopback by default. An empty --metrics-addr disables it.
const defaultMetricsAddr = "127.0.0.1:9090"

// config is the parsed command-line / environment configuration for mecated.
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

	// MCP: remote MCP servers to connect to and register tools from.
	mcpServers mcpServerList
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

func main() {
	if err := run(); err != nil {
		slog.Error("mecated exited with error", "err", err)
		os.Exit(1)
	}
}

// run parses flags, builds the engine and service, and serves until a termination
// signal arrives. It is separated from main so it can return errors cleanly.
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
	// picks up the installed provider. An empty --otlp-endpoint disables tracing
	// (Setup installs nothing and returns a no-op shutdown).
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

	provider, err := buildProvider(cfg)
	if err != nil {
		return err
	}

	store, err := buildStore(cfg)
	if err != nil {
		return err
	}

	// Observability: a private Prometheus registry feeds both the EventSink/
	// Logger adapter and the /metrics handler. Tracing uses the global OTel
	// TracerProvider installed by telemetry.Setup above (a no-op when tracing is
	// disabled), so the sink emits to the OTLP exporter when an endpoint is set.
	reg := prometheus.NewRegistry()
	metrics := telemetry.NewMetrics(reg)
	tracing := telemetry.NewTracing(otel.GetTracerProvider())
	sink := telemetry.NewSink(metrics, tracing)

	// MCP: connect to any configured remote servers and register their tools
	// into the parent catalog. A nil/failed manager is non-fatal; mcpClose is
	// always safe to call.
	engine, mcpClose := buildEngine(ctx, cfg, provider, sink, metrics, store)
	defer mcpClose()

	svc, err := server.NewService(server.Config{
		Engine:        engine,
		Store:         store,
		Workspaces:    osfsWorkspaceFactory(),
		DefaultLimits: defaultLimits(),
	})
	if err != nil {
		return fmt.Errorf("build service: %w", err)
	}

	return serve(ctx, cfg, svc, reg)
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

	fs.StringVar(&cfg.commandsDir, "commands-dir", "", "directory of slash-command templates (<name>.md); setting it enables command expansion. Empty + --enable-commands uses the defaults (.mecatl/commands, .claude/commands)")
	fs.BoolVar(&cfg.enableCommands, "enable-commands", false, "enable slash-command expansion using the default directories (.mecatl/commands, .claude/commands) when --commands-dir is empty")

	fs.BoolVar(&cfg.enableFork, "enable-fork", true, "register the Fork fan-out tool (parallel isolated child branches)")
	fs.BoolVar(&cfg.enableRepoMap, "enable-repomap", true, "register the Aider-style repo-map tool (CGO-free, tree-sitter via WebAssembly)")

	fs.Var(&cfg.mcpServers, "mcp-server", "remote MCP server as name=URL (repeatable); auth token read from MCP_<NAME>_TOKEN")

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

// buildProvider constructs the LLMProvider per config: OpenAI when requested or
// keyed, a canned mock when --mock is set, otherwise an error (the server needs a
// real LLM to be useful).
func buildProvider(cfg config) (port.LLMProvider, error) {
	switch {
	case cfg.useOpenAI:
		if cfg.openAIKey == "" {
			return nil, errors.New("--openai requires OPENAI_API_KEY to be set")
		}
		opts := []openai.Option{openai.WithAPIKey(cfg.openAIKey)}
		if cfg.openAIBaseURL != "" {
			opts = append(opts, openai.WithBaseURL(cfg.openAIBaseURL))
		}
		slog.Info("LLM provider: openai", "model", cfg.model, "base_url", cfg.openAIBaseURL)
		// Wrap the real provider with the resilience decorator (bounded retries,
		// per-attempt timeout, circuit breaker). Classifier/Clock are left nil so
		// the production defaults (DefaultClassifier / time.Now) apply. The mock
		// path below is intentionally left unwrapped: it never fails over the
		// network, so resilience would be inert.
		var llm port.LLMProvider = openai.New(opts...)
		llm = llmresilience.Wrap(llm, llmresilience.Config{
			MaxAttempts:       cfg.llmMaxAttempts,
			BaseBackoff:       llmBaseBackoff,
			MaxBackoff:        llmMaxBackoff,
			PerAttemptTimeout: cfg.llmPerAttemptTimeout,
			BreakerThreshold:  cfg.llmBreakerThreshold,
			BreakerCooldown:   cfg.llmBreakerCooldown,
		})
		slog.Info("LLM resilience enabled",
			"max_attempts", cfg.llmMaxAttempts,
			"per_attempt_timeout", cfg.llmPerAttemptTimeout,
			"breaker_threshold", cfg.llmBreakerThreshold,
			"breaker_cooldown", cfg.llmBreakerCooldown)
		return llm, nil
	case cfg.useMock:
		slog.Warn("LLM provider: mock (canned, offline) — for smoke tests only")
		return mockllm.New(
			mockllm.TextTurn("Mock provider: no real model is configured. Set --openai/OPENAI_API_KEY for live use."),
		), nil
	default:
		return nil, errors.New("no LLM provider configured: pass --openai (with OPENAI_API_KEY) or --mock")
	}
}

// buildStore constructs the SessionStore: a JSONL replay store under --store-dir,
// or the in-memory store when the dir is empty.
func buildStore(cfg config) (port.SessionStore, error) {
	if cfg.storeDir == "" {
		slog.Info("session store: in-memory")
		return memstore.New(), nil
	}
	st, err := jsonlstore.New(cfg.storeDir)
	if err != nil {
		return nil, fmt.Errorf("open jsonl store %q: %w", cfg.storeDir, err)
	}
	slog.Info("session store: jsonl", "dir", cfg.storeDir)
	return st, nil
}

// buildEngine assembles the parent agent.Engine: the core tool catalog (plus an
// optional Bash tool and a read-only Task subagent), the permission policy,
// hooks, prompt config, and the shared provider/store.
// It also wires the telemetry Sink (EventSink) and Logger, and connects any
// configured MCP servers, returning a close func that tears the MCP manager
// down on shutdown (a no-op when no servers are configured).
func buildEngine(ctx context.Context, cfg config, provider port.LLMProvider, sink port.EventSink, logger port.Logger, store port.SessionStore) (*agent.Engine, func()) {
	policy := permpolicy.NewPolicy(defaultRules())
	hooks := hookexec.New(nil) // no hooks by default; map is the injection seam

	cat, mcpClose := buildCatalog(ctx, cfg, provider, hooks)

	counter := buildTokenCounter(cfg)

	deps := agent.Deps{
		LLM:     provider,
		Catalog: cat,
		Policy:  policy,
		Hooks:   hooks,
		// Persist mid-run transitions (tool results, terminal state) so a durable
		// store (--store-dir) holds current state. The Service additionally
		// persists on entering awaiting and at run end; both share this store, so
		// the latest snapshot is always current for auto-resume after a restart.
		Store:               store,
		Sink:                sink,
		Logger:              logger,
		PromptConfig:        promptConfig(cfg),
		Model:               cfg.model,
		ContextWindowTokens: defaultContextWindowTokens,
		CompactionRatio:     defaultCompactionRatio,
		TokenCounter:        counter,
		Compactor:           buildCompactor(cfg, provider, counter),
		CommandExpander:     buildCommandExpander(cfg),
	}
	return agent.NewEngine(deps), mcpClose
}

// buildCommandExpander selects the slash-command expander for the agent Deps.
// Command expansion is OFF by default (the NoopExpander, leaving raw user text
// untouched). It is turned ON when EITHER --commands-dir is set (use that
// directory) OR --enable-commands is true (use the DirCommandExpander defaults
// of .mecatl/commands then .claude/commands). When --commands-dir is set it takes
// precedence over the defaults; --enable-commands without a dir uses the
// defaults. Templates are discovered through the session Workspace FS, so paths
// are workspace-relative.
func buildCommandExpander(cfg config) prompt.CommandExpander {
	if cfg.commandsDir == "" && !cfg.enableCommands {
		slog.Info("slash commands DISABLED (set --commands-dir or --enable-commands to enable)")
		return prompt.NoopExpander{}
	}
	if cfg.commandsDir != "" {
		slog.Info("slash commands ENABLED", "dir", cfg.commandsDir)
		return prompt.NewDirCommandExpander(cfg.commandsDir)
	}
	// --enable-commands with no explicit dir: use the package defaults.
	exp := prompt.NewDirCommandExpander()
	slog.Info("slash commands ENABLED (default dirs)", "dirs", ".mecatl/commands,.claude/commands")
	return exp
}

// buildTokenCounter selects the TokenCounter from --tokenizer. The default
// ("heuristic") returns the dependency-free heuristic counter — byte-identical
// behaviour to the pre-seam loop. "tiktoken" returns the offline tiktoken-backed
// counter from internal/adapter/tokenizer, chosen for the configured model; if it
// cannot be built it logs and falls back to the heuristic so startup never fails.
func buildTokenCounter(cfg config) agent.TokenCounter {
	switch cfg.tokenizer {
	case "tiktoken":
		tc, err := tokenizer.NewForModel(cfg.model)
		if err != nil {
			slog.Warn("tiktoken counter unavailable; falling back to heuristic", "model", cfg.model, "err", err)
			return agent.HeuristicTokenCounter{}
		}
		slog.Info("token counter: tiktoken (offline vocab)", "model", cfg.model)
		return tc
	default:
		slog.Info("token counter: heuristic (dependency-free)")
		return agent.HeuristicTokenCounter{}
	}
}

// buildCompactor selects the Compactor from --compaction. The default
// ("heuristic") returns the single-summary HeuristicCompactor — the exact v1
// behaviour. "cascade" returns the tiered CascadeCompactor (snip→strip→collapse,
// plus an LLM summarize tier wired with the shared provider). It reduces toward
// defaultCompactionTargetRatio, which is BELOW the loop's trigger ratio so there
// is hysteresis — compaction lands the history comfortably under the trigger
// rather than right at it (avoiding per-turn re-compaction).
func buildCompactor(cfg config, provider port.LLMProvider, counter agent.TokenCounter) agent.Compactor {
	switch cfg.compaction {
	case "cascade":
		slog.Info("compaction strategy: cascade (snip→strip→collapse→summarize)")
		return agent.CascadeCompactor{
			Counter:      counter,
			BudgetTokens: int(float64(defaultContextWindowTokens) * defaultCompactionTargetRatio),
			LLM:          provider,
			Model:        cfg.model,
		}
	default:
		slog.Info("compaction strategy: heuristic (single-summary)")
		return agent.HeuristicCompactor{}
	}
}

// buildCatalog registers the always-available core tools (Read, Edit, Write,
// Grep, Glob, WebFetch), a Task subagent wired per WP9 (a scoped explorer child
// Engine — Read/Grep/Glob only, allow-all read-only policy, the same
// provider/model — so a subagent never prompts a human and cannot recurse), and
// — only when a shell is configured — the optional Bash tool. With --no-bash or
// --shell="" the catalog has no Bash and the agent runs shell-less.
//
// After the built-in tools are registered it connects any --mcp-server entries
// and registers their (namespaced) tools. Connecting MCP is best-effort:
// unreachable servers are logged and skipped, and a failed manager never aborts
// startup. The returned close func tears down the MCP manager on shutdown.
func buildCatalog(ctx context.Context, cfg config, provider port.LLMProvider, hooks port.HookRunner) (*tool.Catalog, func()) {
	cat := tool.NewCatalog()
	for _, t := range tools.All() {
		cat.MustRegister(t)
	}
	if runner := buildCommandRunner(cfg); runner != nil {
		cat.MustRegister(tools.NewBashTool(runner))
		slog.Info("Bash tool ENABLED", "shell", cfg.shell, "cwd", cfg.workspace)
	} else {
		slog.Info("Bash tool DISABLED (shell-less mode): the agent has no command execution",
			"reason", bashDisabledReason(cfg))
	}
	cat.MustRegister(buildTaskTool(cfg, provider, hooks))

	// Fork fan-out tool (harness pattern 8): a scoped read-only child Engine (no
	// Fork/Task, so a branch cannot recurse) run against an isolated forked
	// workspace. Children write only to their own forks, so this is safe to enable
	// by default. Gated behind --enable-fork.
	if cfg.enableFork {
		fk := forker.New(func(root string) (tool.Workspace, error) { return osfs.NewWorkspace(root) })
		forkChild := buildChildEngine(cfg, provider)
		cat.MustRegister(agent.NewForkTool(forkChild, fk, agent.WithForkSubagentStopHook(hooks)))
		slog.Info("Fork tool ENABLED (parallel isolated child branches)")
	} else {
		slog.Info("Fork tool DISABLED (--enable-fork=false)")
	}

	// Memory tools (harness pattern 3): opt-in, registered only when a per-project
	// memory directory is configured via --memory-dir.
	if cfg.memoryDir != "" {
		store, err := memory.New(cfg.memoryDir)
		if err != nil {
			slog.Warn("could not open memory store; memory tools disabled", "dir", cfg.memoryDir, "err", err)
		} else if err := memory.Register(cat, store); err != nil {
			slog.Warn("registering memory tools failed; some tools may be missing", "err", err)
		} else {
			slog.Info("memory tools ENABLED (Remember/Recall)", "dir", cfg.memoryDir)
			// Dream consolidation (harness pattern 4): an opt-in background service
			// that distills the memory store on a ticker. It shares the run's ctx
			// (so it stops on shutdown) and the same LLM provider. Only started when
			// both --memory-dir and a positive --memory-consolidate-interval are set.
			startMemoryConsolidation(ctx, cfg, store, provider)
		}
	} else {
		slog.Info("memory tools DISABLED (--memory-dir empty)")
		if cfg.memoryConsolidateInterval > 0 {
			slog.Warn("--memory-consolidate-interval is a no-op without --memory-dir (memory is disabled)",
				"interval", cfg.memoryConsolidateInterval)
		}
	}

	// Repo-map tool (Aider-style ranked codebase overview). It parses source with
	// tree-sitter compiled to WebAssembly and run via wazero — pure Go, no CGO —
	// so it ships in the default static, CGO-free build (used by the ko image) with
	// NO build tag. Enabled by default; --enable-repomap=false turns it off.
	if cfg.enableRepoMap {
		cat.MustRegister(repomap.NewTool())
		slog.Info("repo map tool ENABLED (CGO-free tree-sitter via WebAssembly)")
	} else {
		slog.Info("repo map tool DISABLED (--enable-repomap=false)")
	}

	mcpClose := registerMCP(ctx, cfg, cat)
	return cat, mcpClose
}

// registerMCP connects the configured MCP servers and registers their tools into
// cat. It is non-fatal end to end: with no servers configured it does nothing;
// individual unreachable servers are logged and skipped via the onError hook; a
// manager that fails entirely is logged and skipped. It returns a close func
// that shuts the manager down (a no-op when there is nothing to close).
func registerMCP(ctx context.Context, cfg config, cat *tool.Catalog) func() {
	if len(cfg.mcpServers) == 0 {
		return func() {}
	}
	onError := func(sc mcp.ServerConfig, err error) {
		slog.Warn("MCP server unreachable; skipping", "name", sc.Name, "url", sc.URL, "err", err)
	}
	mgr, err := mcp.NewManager(ctx, cfg.mcpServers, onError)
	if err != nil {
		slog.Warn("MCP manager construction failed; continuing without MCP tools", "err", err)
		return func() {}
	}
	if err := mcp.Register(cat, mgr.Tools()); err != nil {
		slog.Warn("registering MCP tools failed; some tools may be missing", "err", err)
	}
	slog.Info("MCP tools registered", "servers", len(cfg.mcpServers), "tools", len(mgr.Tools()))
	return func() {
		if err := mgr.Close(); err != nil {
			slog.Warn("MCP manager close", "err", err)
		}
	}
}

// startMemoryConsolidation launches the dream consolidator on a background
// goroutine when --memory-consolidate-interval is positive. It shares the run's
// ctx (so the loop exits on shutdown) and the same LLM provider as the agent.
// A non-positive interval is a no-op. Per-run errors are logged at warn and do
// not stop the loop.
func startMemoryConsolidation(ctx context.Context, cfg config, store tool.MemoryStore, provider port.LLMProvider) {
	if cfg.memoryConsolidateInterval <= 0 {
		slog.Info("memory consolidation DISABLED (--memory-consolidate-interval=0)")
		return
	}
	cons := dream.New(store, provider, dream.Config{Model: cfg.model})
	slog.Info("memory consolidation ENABLED (dream)", "interval", cfg.memoryConsolidateInterval, "model", cfg.model)
	go func() {
		err := cons.RunPeriodically(ctx, cfg.memoryConsolidateInterval, func(err error) {
			slog.Warn("memory consolidation", "err", err)
		})
		// RunPeriodically returns ctx.Err() on shutdown; that is expected, not a fault.
		if err != nil && !errors.Is(err, context.Canceled) {
			slog.Warn("memory consolidation loop stopped", "err", err)
		}
	}()
}

// buildCommandRunner builds the local command runner the Bash tool executes
// against, rooted at the default session workspace. It returns nil when command
// execution is disabled (--no-bash, or an empty --shell), in which case the Bash
// tool is not registered and the harness runs without a shell. A runner that
// fails to construct also disables Bash rather than aborting startup.
func buildCommandRunner(cfg config) tool.CommandRunner {
	if cfg.noBash || cfg.shell == "" {
		return nil
	}
	runner, err := osfs.NewCommandRunnerShell(cfg.workspace, cfg.shell)
	if err != nil {
		slog.Warn("could not build command runner; Bash tool disabled", "workspace", cfg.workspace, "err", err)
		return nil
	}
	return runner
}

// bashDisabledReason returns a short human-readable reason Bash is disabled, for
// the startup log line.
func bashDisabledReason(cfg config) string {
	switch {
	case cfg.noBash:
		return "--no-bash"
	case cfg.shell == "":
		return "--shell is empty"
	default:
		return "command runner unavailable"
	}
}

// buildChildEngine constructs a child *Engine scoped to the read-only explorer
// toolset (Read/Grep/Glob ONLY — no Fork/Task, so a child can never recurse or
// fan out further) under an allow-all, non-interactive policy. Both the Task
// subagent and the Fork fan-out tool share this child shape: each runs one-shot
// and must never produce a permission ask.
func buildChildEngine(cfg config, provider port.LLMProvider) *agent.Engine {
	childCat := tool.NewCatalog()
	childCat.MustRegister(tools.ReadTool{})
	childCat.MustRegister(tools.GrepTool{})
	childCat.MustRegister(tools.GlobTool{})

	// Allow-all over the read-only explorer tools: the child is one-shot and
	// non-interactive, so it must never produce a permission ask.
	childPolicy := permpolicy.NewPolicy([]governance.Rule{{Effect: governance.Allow}})

	return agent.NewEngine(agent.Deps{
		LLM:                 provider,
		Catalog:             childCat,
		Policy:              childPolicy,
		Hooks:               hookexec.New(nil),
		PromptConfig:        promptConfig(cfg),
		Model:               cfg.model,
		ContextWindowTokens: defaultContextWindowTokens,
		CompactionRatio:     defaultCompactionRatio,
	})
}

// buildTaskTool constructs the Task subagent tool over a child Engine scoped to
// the read-only explorer toolset.
func buildTaskTool(cfg config, provider port.LLMProvider, hooks port.HookRunner) tool.Tool {
	return agent.NewTaskTool(buildChildEngine(cfg, provider), agent.WithSubagentStopHook(hooks))
}

// promptConfig builds the system-prompt configuration. The volatile Env values
// (cwd/os/model/date/mode) are computed HERE in the composition root so the
// domain stays infra-free; the loop fills in the per-turn Mode and Tools.
func promptConfig(cfg config) prompt.Config {
	return prompt.Config{
		Env: prompt.Env{
			Cwd:   cfg.workspace,
			OS:    runtime.GOOS,
			Model: cfg.model,
			Date:  time.Now().Format("2006-01-02"),
			Mode:  string(session.ModeDefault),
		},
	}
}

// defaultRules is the built-in permission ruleset: read-only tools (Read, Grep,
// Glob, the Task explorer) are allowed; mutating tools (Bash, Edit, Write) ask
// for approval. Anything unmatched defaults to ask via the evaluator.
func defaultRules() []governance.Rule {
	return []governance.Rule{
		{Scope: governance.ScopeManaged, Tool: "Read", Effect: governance.Allow},
		{Scope: governance.ScopeManaged, Tool: "Grep", Effect: governance.Allow},
		{Scope: governance.ScopeManaged, Tool: "Glob", Effect: governance.Allow},
		{Scope: governance.ScopeManaged, Tool: "WebFetch", Effect: governance.Allow},
		{Scope: governance.ScopeManaged, Tool: "Task", Effect: governance.Allow},
		{Scope: governance.ScopeManaged, Tool: "Bash", Effect: governance.Ask},
		{Scope: governance.ScopeManaged, Tool: "Edit", Effect: governance.Ask},
		{Scope: governance.ScopeManaged, Tool: "Write", Effect: governance.Ask},
	}
}

// defaultLimits returns the non-zero stop limits injected for sessions created
// without explicit limits, so a default session is always bounded (a zero
// Limits value disables every stop condition in package session).
func defaultLimits() session.Limits {
	return session.Limits{
		MaxTurns:               defaultMaxTurns,
		MaxToolCalls:           defaultMaxToolCalls,
		MaxConsecutiveFailures: defaultMaxConsecutiveFailures,
	}
}

// osfsWorkspaceFactory returns a server.WorkspaceFactory that builds an osfs
// Workspace rooted at the session's workspace dir. A root that cannot be opened
// (e.g. it does not exist) yields a nil Workspace; tool calls against it return
// errors the model can read.
func osfsWorkspaceFactory() server.WorkspaceFactory {
	return func(root string) tool.Workspace {
		ws, err := osfs.NewWorkspace(root)
		if err != nil {
			slog.Error("workspace factory: cannot open root", "root", root, "err", err)
			return nil
		}
		return ws
	}
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
