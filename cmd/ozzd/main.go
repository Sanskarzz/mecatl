// Command ozzd is the ozzharness server binary and the composition root: the one
// place where concrete adapters are wired to the ports the agent loop consumes.
//
// It builds an LLM provider (OpenAI Responses, or a canned mock for smoke
// tests), the seven-tool catalog plus a read-only Task subagent, the permission
// policy, lifecycle hooks, the session store, and the two-layer system prompt;
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
	"syscall"
	"time"

	"google.golang.org/grpc"

	ozzv1 "github.com/stacklok/ozzharness/contracts/gen/go/ozz/v1"
	"github.com/stacklok/ozzharness/internal/adapter/hookexec"
	"github.com/stacklok/ozzharness/internal/adapter/mockllm"
	"github.com/stacklok/ozzharness/internal/adapter/openai"
	"github.com/stacklok/ozzharness/internal/adapter/osfs"
	"github.com/stacklok/ozzharness/internal/adapter/permpolicy"
	"github.com/stacklok/ozzharness/internal/adapter/server"
	"github.com/stacklok/ozzharness/internal/adapter/store/jsonlstore"
	"github.com/stacklok/ozzharness/internal/adapter/store/memstore"
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

	provider, err := buildProvider(cfg)
	if err != nil {
		return err
	}

	store, err := buildStore(cfg)
	if err != nil {
		return err
	}

	engine := buildEngine(cfg, provider)

	svc, err := server.NewService(server.Config{
		Engine:        engine,
		Store:         store,
		Workspaces:    osfsWorkspaceFactory(),
		DefaultLimits: defaultLimits(),
	})
	if err != nil {
		return fmt.Errorf("build service: %w", err)
	}

	ctx, stop := signal.NotifyContext(context.Background(), syscall.SIGINT, syscall.SIGTERM)
	defer stop()

	return serve(ctx, cfg, svc)
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
		return openai.New(opts...), nil
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

// buildEngine assembles the parent agent.Engine: the full tool catalog (seven
// tools plus a read-only Task subagent), the permission policy, hooks, prompt
// config, and the shared provider/store.
func buildEngine(cfg config, provider port.LLMProvider) *agent.Engine {
	policy := permpolicy.NewPolicy(defaultRules())
	hooks := hookexec.New(nil) // no hooks by default; map is the injection seam

	cat := buildCatalog(cfg, provider, hooks)

	deps := agent.Deps{
		LLM:                 provider,
		Catalog:             cat,
		Policy:              policy,
		Hooks:               hooks,
		Store:               nil, // store is held by the Service; the engine need not persist twice
		PromptConfig:        promptConfig(cfg),
		Model:               cfg.model,
		ContextWindowTokens: defaultContextWindowTokens,
	}
	return agent.NewEngine(deps)
}

// buildCatalog registers the seven core tools and a Task subagent wired per WP9:
// a scoped explorer child Engine (Read/Grep/Glob only, allow-all read-only
// policy, the same provider/model) so a subagent never prompts a human and cannot
// recurse.
func buildCatalog(cfg config, provider port.LLMProvider, hooks port.HookRunner) *tool.Catalog {
	cat := tool.NewCatalog()
	for _, t := range tools.All() {
		cat.MustRegister(t)
	}
	cat.MustRegister(buildTaskTool(cfg, provider, hooks))
	return cat
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

// serve starts the gRPC and HTTP servers concurrently and blocks until ctx is
// cancelled (a signal) or a server fails, then shuts both down gracefully.
func serve(ctx context.Context, cfg config, svc *server.Service) error {
	grpcSrv := grpc.NewServer()
	ozzv1.RegisterHarnessServiceServer(grpcSrv, server.NewHarnessServer(svc))

	httpSrv := &http.Server{
		Addr:              cfg.httpAddr,
		Handler:           server.NewHTTPHandler(svc),
		ReadHeaderTimeout: 10 * time.Second,
	}

	warnIfNonLoopback("grpc-addr", cfg.grpcAddr)
	warnIfNonLoopback("http-addr", cfg.httpAddr)

	grpcLis, err := net.Listen("tcp", cfg.grpcAddr)
	if err != nil {
		return fmt.Errorf("listen grpc %q: %w", cfg.grpcAddr, err)
	}

	errCh := make(chan error, 2)

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

	select {
	case <-ctx.Done():
		slog.Info("shutdown signal received; stopping servers")
	case err := <-errCh:
		slog.Error("server failed; shutting down", "err", err)
		shutdown(grpcSrv, httpSrv)
		return err
	}

	shutdown(grpcSrv, httpSrv)
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

// shutdown gracefully stops both servers, bounding the HTTP drain with a timeout.
func shutdown(grpcSrv *grpc.Server, httpSrv *http.Server) {
	shutdownCtx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	if err := httpSrv.Shutdown(shutdownCtx); err != nil {
		slog.Warn("http graceful shutdown", "err", err)
	}
	grpcSrv.GracefulStop()
}
