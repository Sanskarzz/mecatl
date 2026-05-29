// Command ozzd is the ozzharness server binary and the composition root: the one
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

	ozzv1 "github.com/stacklok/ozzharness/contracts/gen/go/ozz/v1"
	"github.com/stacklok/ozzharness/internal/adapter/hookexec"
	"github.com/stacklok/ozzharness/internal/adapter/llmresilience"
	"github.com/stacklok/ozzharness/internal/adapter/mcp"
	"github.com/stacklok/ozzharness/internal/adapter/mockllm"
	"github.com/stacklok/ozzharness/internal/adapter/openai"
	"github.com/stacklok/ozzharness/internal/adapter/osfs"
	"github.com/stacklok/ozzharness/internal/adapter/permpolicy"
	"github.com/stacklok/ozzharness/internal/adapter/server"
	"github.com/stacklok/ozzharness/internal/adapter/store/jsonlstore"
	"github.com/stacklok/ozzharness/internal/adapter/store/memstore"
	"github.com/stacklok/ozzharness/internal/adapter/telemetry"
	"github.com/stacklok/ozzharness/internal/adapter/tools"
	"github.com/stacklok/ozzharness/internal/agent"
	"github.com/stacklok/ozzharness/internal/governance"
	"github.com/stacklok/ozzharness/internal/port"
	"github.com/stacklok/ozzharness/internal/prompt"
	"github.com/stacklok/ozzharness/internal/session"
	"github.com/stacklok/ozzharness/internal/tool"
)

// defaultContextWindowTokens is the model context window the loop uses to decide
// when to compact. A conservative default that suits the common GPT-class models.
const defaultContextWindowTokens = 128_000

// v1 TRUST ASSUMPTION (security): the ozzd API is UNAUTHENTICATED. It exposes
// command and file execution against the configured workspace with no caller
// identity check. It is intended for LOCALHOST, SINGLE-USER use only, which is
// why the default listen addresses below bind the loopback interface. Binding to
// a non-loopback address (e.g. ":8080" / "0.0.0.0") exposes unauthenticated
// command/file execution to the network and must not be done without an external
// trust boundary. Authentication / mTLS is future work.
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

// config is the parsed command-line / environment configuration for ozzd.
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

	// LLM resilience knobs (see package internal/adapter/llmresilience).
	llmMaxAttempts       int
	llmPerAttemptTimeout time.Duration
	llmBreakerThreshold  int
	llmBreakerCooldown   time.Duration

	// Observability: the Prometheus /metrics listen address (empty disables it).
	metricsAddr string

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
		slog.Error("ozzd exited with error", "err", err)
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
	// TracerProvider, which is a no-op until a provider is installed (no OTLP
	// exporter is wired in v1), so traces are inert by default.
	reg := prometheus.NewRegistry()
	metrics := telemetry.NewMetrics(reg)
	tracing := telemetry.NewTracing(otel.GetTracerProvider())
	sink := telemetry.NewSink(metrics, tracing)

	// MCP: connect to any configured remote servers and register their tools
	// into the parent catalog. A nil/failed manager is non-fatal; mcpClose is
	// always safe to call.
	engine, mcpClose := buildEngine(ctx, cfg, provider, sink, metrics)
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
	fs := flag.NewFlagSet("ozzd", flag.ContinueOnError)
	var cfg config

	cwd, _ := os.Getwd()

	fs.StringVar(&cfg.grpcAddr, "grpc-addr", defaultGRPCAddr,
		"gRPC listen address (defaults to loopback; the API is UNAUTHENTICATED — see package doc before binding non-loopback)")
	fs.StringVar(&cfg.httpAddr, "http-addr", defaultHTTPAddr,
		"HTTP/SSE listen address (defaults to loopback; the API is UNAUTHENTICATED — see package doc before binding non-loopback)")
	fs.StringVar(&cfg.workspace, "workspace", cwd, "default session workspace root")
	fs.StringVar(&cfg.model, "model", "gpt-5", "model identifier sent to the provider")
	fs.BoolVar(&cfg.useOpenAI, "openai", false, "use the OpenAI Responses provider (key from OPENAI_API_KEY)")
	fs.StringVar(&cfg.openAIBaseURL, "openai-base-url", "", "override the OpenAI API base URL (compatible endpoints)")
	fs.BoolVar(&cfg.useMock, "mock", false, "use a canned offline mock provider (no network; for smoke tests only)")
	fs.StringVar(&cfg.storeDir, "store-dir", "", "directory for the JSONL session store (empty -> in-memory store)")
	fs.StringVar(&cfg.shell, "shell", "/bin/sh", "shell used to execute Bash-tool commands; empty disables Bash (shell-less mode)")
	fs.BoolVar(&cfg.noBash, "no-bash", false, "disable the Bash tool entirely (shell-less mode); overrides --shell")

	fs.IntVar(&cfg.llmMaxAttempts, "llm-max-attempts", 3, "max LLM stream-establish attempts (initial call plus retries)")
	fs.DurationVar(&cfg.llmPerAttemptTimeout, "llm-per-attempt-timeout", 30*time.Second, "per-attempt timeout for establishing an LLM stream (0 disables)")
	fs.IntVar(&cfg.llmBreakerThreshold, "llm-breaker-threshold", 5, "consecutive LLM failures that open the circuit breaker (0 disables)")
	fs.DurationVar(&cfg.llmBreakerCooldown, "llm-breaker-cooldown", 30*time.Second, "how long the LLM circuit breaker stays open before half-opening")

	fs.StringVar(&cfg.metricsAddr, "metrics-addr", defaultMetricsAddr, "Prometheus /metrics listen address (empty disables the metrics endpoint)")

	fs.Var(&cfg.mcpServers, "mcp-server", "remote MCP server as name=URL (repeatable); auth token read from MCP_<NAME>_TOKEN")

	if err := fs.Parse(argv); err != nil {
		return config{}, err
	}

	cfg.openAIKey = os.Getenv("OPENAI_API_KEY")
	// An API key in the environment implies the user wants the real provider.
	if cfg.openAIKey != "" {
		cfg.useOpenAI = true
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
func buildEngine(ctx context.Context, cfg config, provider port.LLMProvider, sink port.EventSink, logger port.Logger) (*agent.Engine, func()) {
	policy := permpolicy.NewPolicy(defaultRules())
	hooks := hookexec.New(nil) // no hooks by default; map is the injection seam

	cat, mcpClose := buildCatalog(ctx, cfg, provider, hooks)

	deps := agent.Deps{
		LLM:                 provider,
		Catalog:             cat,
		Policy:              policy,
		Hooks:               hooks,
		Store:               nil, // store is held by the Service; the engine need not persist twice
		Sink:                sink,
		Logger:              logger,
		PromptConfig:        promptConfig(cfg),
		Model:               cfg.model,
		ContextWindowTokens: defaultContextWindowTokens,
	}
	return agent.NewEngine(deps), mcpClose
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

// buildTaskTool constructs the Task subagent tool over a child Engine scoped to
// the read-only explorer toolset.
func buildTaskTool(cfg config, provider port.LLMProvider, hooks port.HookRunner) tool.Tool {
	childCat := tool.NewCatalog()
	childCat.MustRegister(tools.ReadTool{})
	childCat.MustRegister(tools.GrepTool{})
	childCat.MustRegister(tools.GlobTool{})

	// Allow-all over the read-only explorer tools: the child is one-shot and
	// non-interactive, so it must never produce a permission ask.
	childPolicy := permpolicy.NewPolicy([]governance.Rule{{Effect: governance.Allow}})

	childEngine := agent.NewEngine(agent.Deps{
		LLM:                 provider,
		Catalog:             childCat,
		Policy:              childPolicy,
		Hooks:               hookexec.New(nil),
		PromptConfig:        promptConfig(cfg),
		Model:               cfg.model,
		ContextWindowTokens: defaultContextWindowTokens,
	})

	return agent.NewTaskTool(childEngine, agent.WithSubagentStopHook(hooks))
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
func serve(ctx context.Context, cfg config, svc *server.Service, reg *prometheus.Registry) error {
	grpcSrv := grpc.NewServer()
	ozzv1.RegisterHarnessServiceServer(grpcSrv, server.NewHarnessServer(svc))

	httpSrv := &http.Server{
		Addr:              cfg.httpAddr,
		Handler:           server.NewHTTPHandler(svc),
		ReadHeaderTimeout: 10 * time.Second,
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

	warnIfNonLoopback("grpc-addr", cfg.grpcAddr)
	warnIfNonLoopback("http-addr", cfg.httpAddr)

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
		slog.Info("HTTP/SSE server listening", "addr", cfg.httpAddr)
		if serveErr := httpSrv.ListenAndServe(); serveErr != nil && !errors.Is(serveErr, http.ErrServerClosed) {
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

// warnIfNonLoopback logs the v1 unauthenticated-API trust assumption: when an
// address binds something other than the loopback interface, it exposes
// unauthenticated command/file execution to the network. It logs an info line
// for the safe (loopback) case and a prominent warning otherwise.
func warnIfNonLoopback(flagName, addr string) {
	host, _, err := net.SplitHostPort(addr)
	if err != nil {
		host = addr
	}
	ip := net.ParseIP(host)
	loopback := host == "localhost" || (ip != nil && ip.IsLoopback())
	if loopback {
		slog.Info("API bound to loopback (unauthenticated, single-user localhost trust model)",
			"flag", flagName, "addr", addr)
		return
	}
	slog.Warn("API bound to a NON-loopback address: the ozzd API is UNAUTHENTICATED and exposes command/file execution; do not do this without an external trust boundary (auth/mTLS is future work)",
		"flag", flagName, "addr", addr)
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
