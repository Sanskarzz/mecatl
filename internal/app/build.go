// Package app is the SHARED composition layer for the mecatl server: the one
// place that wires concrete adapters (LLM provider, tool catalog, permission
// policy, hooks, session store, MCP, skills) into an agent.Engine and exposes it
// as a server.Service. It is consumed by two composition roots:
//
//   - cmd/mecated  — the standalone server binary (parses flags, builds the
//     telemetry sink, calls Build, then serves the Service over gRPC + HTTP).
//   - cmd/mecatui  — the TUI client, which when no external server is running
//     calls Build to host its OWN server in-process over a UNIX socket, so a
//     single binary "just works" with no separately-spawned daemon.
//
// Layering: app sits at the SAME level as cmd/ — it is composition, not domain.
// It MAY import adapters, internal/agent, and (transitively, via the server
// adapter) contracts/gen; the domain/port/agent packages must never import it.
// Nothing imports app except the cmd/ mains.
package app

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"runtime"
	"strings"
	"time"

	"github.com/stacklok/mecatl/internal/adapter/agents"
	"github.com/stacklok/mecatl/internal/adapter/dream"
	"github.com/stacklok/mecatl/internal/adapter/forker"
	"github.com/stacklok/mecatl/internal/adapter/hookexec"
	"github.com/stacklok/mecatl/internal/adapter/llmresilience"
	"github.com/stacklok/mecatl/internal/adapter/mcp"
	mcpsource "github.com/stacklok/mecatl/internal/adapter/mcp/source"
	"github.com/stacklok/mecatl/internal/adapter/memory"
	"github.com/stacklok/mecatl/internal/adapter/mockllm"
	"github.com/stacklok/mecatl/internal/adapter/openai"
	"github.com/stacklok/mecatl/internal/adapter/osfs"
	"github.com/stacklok/mecatl/internal/adapter/permpolicy"
	"github.com/stacklok/mecatl/internal/adapter/repomap"
	"github.com/stacklok/mecatl/internal/adapter/server"
	"github.com/stacklok/mecatl/internal/adapter/skills"
	"github.com/stacklok/mecatl/internal/adapter/store/jsonlstore"
	"github.com/stacklok/mecatl/internal/adapter/store/memstore"
	"github.com/stacklok/mecatl/internal/adapter/tokenizer"
	"github.com/stacklok/mecatl/internal/adapter/tools"
	"github.com/stacklok/mecatl/internal/agent"
	"github.com/stacklok/mecatl/internal/governance"
	"github.com/stacklok/mecatl/internal/port"
	"github.com/stacklok/mecatl/internal/prompt"
	"github.com/stacklok/mecatl/internal/session"
	"github.com/stacklok/mecatl/internal/team"
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

// Default session stop limits. A zero Limits value disables every stop condition
// in package session, so the composition layer supplies these non-zero defaults
// to ensure a session created without explicit limits is still bounded.
const (
	defaultMaxTurns               = 50
	defaultMaxToolCalls           = 200
	defaultMaxConsecutiveFailures = 5
)

// LLM resilience backoff bounds. Exponential backoff between BaseBackoff and
// MaxBackoff is a sensible fixed envelope (the attempt count, per-attempt
// timeout, and breaker knobs are configurable via Config).
const (
	llmBaseBackoff = 200 * time.Millisecond
	llmMaxBackoff  = 10 * time.Second
)

// Config is the build contract for the server composition: everything Build
// needs to assemble the engine and service, independent of HOW the resulting
// service is served (TCP, UNIX socket, TLS, auth — all serve-time concerns owned
// by the caller). Each cmd/ main maps its own CLI/env surface onto this struct.
//
// The zero value is a usable shell-less, provider-less configuration; callers set
// the fields they need. Sink and Logger are optional (nil installs no telemetry —
// the engine nil-guards both).
type Config struct {
	Workspace     string
	Model         string
	UseOpenAI     bool
	OpenAIBaseURL string
	OpenAIKey     string
	UseMock       bool
	StoreDir      string
	Shell         string
	NoBash        bool

	// Context management: the compaction strategy ("heuristic"|"cascade") and the
	// token counter ("heuristic"|"tiktoken"). Empty means "heuristic".
	Compaction string
	Tokenizer  string

	// LLM resilience knobs (see internal/adapter/llmresilience).
	LLMMaxAttempts       int
	LLMPerAttemptTimeout time.Duration
	LLMBreakerThreshold  int
	LLMBreakerCooldown   time.Duration

	// Memory: per-project memory store directory (empty disables the tools), plus
	// the background consolidation (dream) interval (0 disables; only meaningful
	// with MemoryDir set).
	MemoryDir                 string
	MemoryConsolidateInterval time.Duration

	// Skills: explicit directories (highest precedence) plus the conventional
	// project/user locations when SkillsConventional is set. SkillsDraftDir enables
	// the writable SkillDraft tool quarantine (see validateSkillDraftConfig).
	SkillsDirs           []string
	SkillsConventional   bool
	SkillsDraftDir       string
	SkillsDraftThreshold float64

	// Agent definitions (Tier 1): named subagent specialists (prompt + scoped
	// read-only catalog + per-def model) discovered from <name>.md files. Mirrors
	// the Skills fields: explicit dirs (highest precedence) plus the conventional
	// project/user locations when AgentsConventional is set. Strict opt-in — zero
	// sources means Task keeps only the default explorer (no behaviour change).
	AgentsDirs         []string
	AgentsConventional bool

	// SubagentModel is the global override applied to every Task/member child
	// engine that does not pin its own model (the analogue of
	// CLAUDE_CODE_SUBAGENT_MODEL). Resolution precedence per def is:
	// def.Model > SubagentModel > parent Model. Empty disables the override. It is
	// resolved (with ModelAliases) ONLY in this composition layer.
	SubagentModel string
	// ModelAliases maps a short alias (e.g. "sonnet"/"opus"/"haiku"/"fast") to a
	// concrete provider model id. Resolved only here; the domain/agent always
	// receives a concrete model string.
	ModelAliases map[string]string

	// Slash commands: directory of <name>.md templates; EnableCommands turns on the
	// default directories when CommandsDir is empty.
	CommandsDir    string
	EnableCommands bool

	// Optional tools, on by default in the standalone server.
	EnableFork    bool
	EnableRepoMap bool

	// EnableTeams turns on the agent-teams capability (the CreateTeam /
	// SpawnTeammate / RunTeam RPCs). It is OPT-IN and EXPERIMENTAL: default off.
	// When false, server.Config.MemberEngine stays nil and the team RPCs return
	// ErrTeamsDisabled.
	EnableTeams bool

	// MCP: static servers, the resource meta-tools toggle, the prompt-expander
	// toggle, and the live ToolHive workload source.
	MCPServers       []mcp.ServerConfig
	MCPResourceTools bool
	MCPPrompts       bool
	ToolHiveEnabled  bool
	ToolHiveGroup    string

	// Observability relays, injected by the caller (mecated wires telemetry; the
	// embedded TUI server leaves both nil). The engine nil-guards each.
	Sink   port.EventSink
	Logger port.Logger
}

// Built is the result of Build: the assembled server.Service plus a Close func
// that tears down composition-owned resources (the MCP manager). Close is always
// safe to call, even when nothing needs closing.
type Built struct {
	Service *server.Service
	Close   func()
}

// Build assembles the LLM provider, session store, tool catalog, agent engine,
// and server.Service from cfg. It returns ErrNoProvider-class errors from the
// provider step and a fatal error if the SkillDraft trust boundary is misconfigured.
//
// The returned Built.Close must be deferred by the caller to release the MCP
// manager on shutdown. Build itself starts no listeners — serving is the caller's
// responsibility (see cmd/mecated/serve and cmd/mecatui/embed).
func Build(ctx context.Context, cfg Config) (*Built, error) {
	provider, err := buildProvider(cfg)
	if err != nil {
		return nil, err
	}
	store, err := buildStore(cfg)
	if err != nil {
		return nil, err
	}
	engine, mcpProvider, mcpInventory, mcpClose, err := buildEngine(ctx, cfg, provider, store)
	if err != nil {
		return nil, err
	}
	logMCPInventory(mcpInventory)

	svcCfg := server.Config{
		Engine:        engine,
		Store:         store,
		Workspaces:    osfsWorkspaceFactory(),
		DefaultLimits: defaultLimits(),
		MCPProvider:   mcpProvider,
		MCPSources:    mcpInventory,
	}
	applyTeamConfig(&svcCfg, cfg, provider)

	svc, err := server.NewService(svcCfg)
	if err != nil {
		mcpClose()
		return nil, fmt.Errorf("build service: %w", err)
	}
	return &Built{Service: svc, Close: mcpClose}, nil
}

// buildProvider constructs the LLMProvider per config: OpenAI when requested or
// keyed, a canned mock when UseMock is set, otherwise an error (the server needs a
// real LLM to be useful).
func buildProvider(cfg Config) (port.LLMProvider, error) {
	switch {
	case cfg.UseOpenAI:
		if cfg.OpenAIKey == "" {
			return nil, errors.New("OpenAI provider requires an API key (OPENAI_API_KEY)")
		}
		opts := []openai.Option{openai.WithAPIKey(cfg.OpenAIKey)}
		if cfg.OpenAIBaseURL != "" {
			opts = append(opts, openai.WithBaseURL(cfg.OpenAIBaseURL))
		}
		slog.Info("LLM provider: openai", "model", cfg.Model, "base_url", cfg.OpenAIBaseURL)
		// Wrap the real provider with the resilience decorator (bounded retries,
		// per-attempt timeout, circuit breaker). Classifier/Clock are left nil so
		// the production defaults (DefaultClassifier / time.Now) apply. The mock
		// path below is intentionally left unwrapped: it never fails over the
		// network, so resilience would be inert.
		var llm port.LLMProvider = openai.New(opts...)
		llm = llmresilience.Wrap(llm, llmresilience.Config{
			MaxAttempts:       cfg.LLMMaxAttempts,
			BaseBackoff:       llmBaseBackoff,
			MaxBackoff:        llmMaxBackoff,
			PerAttemptTimeout: cfg.LLMPerAttemptTimeout,
			BreakerThreshold:  cfg.LLMBreakerThreshold,
			BreakerCooldown:   cfg.LLMBreakerCooldown,
		})
		slog.Info("LLM resilience enabled",
			"max_attempts", cfg.LLMMaxAttempts,
			"per_attempt_timeout", cfg.LLMPerAttemptTimeout,
			"breaker_threshold", cfg.LLMBreakerThreshold,
			"breaker_cooldown", cfg.LLMBreakerCooldown)
		return llm, nil
	case cfg.UseMock:
		slog.Warn("LLM provider: mock (canned, offline) — for smoke tests only")
		return mockllm.New(
			mockllm.TextTurn("Mock provider: no real model is configured. Set OPENAI_API_KEY for live use."),
		), nil
	default:
		return nil, errors.New("no LLM provider configured: set OPENAI_API_KEY (OpenAI) or enable the mock")
	}
}

// buildStore constructs the SessionStore: a JSONL replay store under StoreDir, or
// the in-memory store when the dir is empty.
func buildStore(cfg Config) (port.SessionStore, error) {
	if cfg.StoreDir == "" {
		slog.Info("session store: in-memory")
		return memstore.New(), nil
	}
	st, err := jsonlstore.New(cfg.StoreDir)
	if err != nil {
		return nil, fmt.Errorf("open jsonl store %q: %w", cfg.StoreDir, err)
	}
	slog.Info("session store: jsonl", "dir", cfg.StoreDir)
	return st, nil
}

// buildEngine assembles the parent agent.Engine: the core tool catalog (plus an
// optional Bash tool and a read-only Task subagent), the permission policy, hooks,
// prompt config, and the shared provider/store. It also connects any configured
// MCP servers, returning a close func that tears the MCP manager down on shutdown
// (a no-op when no servers are configured).
func buildEngine(ctx context.Context, cfg Config, provider port.LLMProvider, store port.SessionStore) (*agent.Engine, mcp.Provider, []mcpsource.SourceInfo, func(), error) {
	// SkillDraft trust boundary: when enabled, the quarantine dir must live OUTSIDE
	// the workspace root (so the model's workspace-confined Write/Edit cannot reach
	// it) and be disjoint from every active skills dir. Fatal on a misconfig.
	if err := validateSkillDraftConfig(cfg); err != nil {
		return nil, nil, nil, func() {}, err
	}
	warnSkillDraftResiduals(cfg)

	policy := permpolicy.NewPolicy(defaultRules())
	hooks := hookexec.New(nil) // no hooks by default; map is the injection seam

	cat, mcpProvider, mcpInventory, mcpClose := buildCatalog(ctx, cfg, provider, hooks)

	counter := buildTokenCounter(cfg)

	deps := agent.Deps{
		LLM:     provider,
		Catalog: cat,
		Policy:  policy,
		Hooks:   hooks,
		// Persist mid-run transitions (tool results, terminal state) so a durable
		// store (StoreDir) holds current state. The Service additionally persists on
		// entering awaiting and at run end; both share this store, so the latest
		// snapshot is always current for auto-resume after a restart.
		Store:               store,
		Sink:                cfg.Sink,
		Logger:              cfg.Logger,
		PromptConfig:        promptConfig(cfg),
		Model:               cfg.Model,
		ContextWindowTokens: defaultContextWindowTokens,
		CompactionRatio:     defaultCompactionRatio,
		TokenCounter:        counter,
		Compactor:           buildCompactor(cfg, provider, counter),
		CommandExpander:     buildCommandExpander(cfg, mcpProvider),
	}
	return agent.NewEngine(deps), mcpProvider, mcpInventory, mcpClose, nil
}

// buildCommandExpander selects the slash-command expander for the agent Deps.
// Command expansion is OFF by default (the NoopExpander, leaving raw user text
// untouched). It is turned ON when EITHER CommandsDir is set OR EnableCommands is
// true. It also composes an MCP prompt expander (the file-backed DirCommandExpander
// has higher precedence, so a local command file shadows a same-named MCP prompt)
// when MCPPrompts is set and at least one connected server exposes a prompt.
func buildCommandExpander(cfg Config, mcpProvider mcp.Provider) prompt.CommandExpander {
	dirExp := buildDirCommandExpander(cfg)
	mcpExp := buildMCPPromptExpander(cfg, mcpProvider)

	switch {
	case dirExp == nil && mcpExp == nil:
		return prompt.NoopExpander{}
	case mcpExp == nil:
		return dirExp
	case dirExp == nil:
		return mcpExp
	default:
		// File-backed commands win on a name collision (listed first).
		return prompt.NewMultiExpander(dirExp, mcpExp)
	}
}

// buildDirCommandExpander returns the file-backed slash-command expander, or nil
// when command expansion is not enabled (so the caller can compose conditionally).
func buildDirCommandExpander(cfg Config) prompt.CommandExpander {
	if cfg.CommandsDir == "" && !cfg.EnableCommands {
		slog.Info("slash commands DISABLED (set a commands dir or enable commands to enable)")
		return nil
	}
	if cfg.CommandsDir != "" {
		slog.Info("slash commands ENABLED", "dir", cfg.CommandsDir)
		return prompt.NewDirCommandExpander(cfg.CommandsDir)
	}
	// EnableCommands with no explicit dir: use the package defaults.
	slog.Info("slash commands ENABLED (default dirs)", "dirs", ".mecatl/commands,.claude/commands")
	return prompt.NewDirCommandExpander()
}

// buildMCPPromptExpander returns the MCP prompt expander, or nil when MCP prompts
// are disabled or no connected server exposes a prompt.
func buildMCPPromptExpander(cfg Config, p mcp.Provider) prompt.CommandExpander {
	if !cfg.MCPPrompts || p == nil {
		if !cfg.MCPPrompts {
			slog.Info("MCP prompts DISABLED")
		}
		return nil
	}
	prompts, err := p.ListPrompts(context.Background(), "")
	if err != nil {
		slog.Warn("MCP prompt listing failed; prompt expansion disabled", "err", err)
		return nil
	}
	if len(prompts) == 0 {
		slog.Info("MCP prompts DISABLED (no connected server exposes a prompt)")
		return nil
	}
	slog.Info("MCP prompt expansion ENABLED", "count", len(prompts))
	return mcp.NewPromptExpander(p)
}

// buildTokenCounter selects the TokenCounter from cfg.Tokenizer. The default
// ("heuristic"/empty) returns the dependency-free heuristic counter. "tiktoken"
// returns the offline tiktoken-backed counter for the configured model; if it
// cannot be built it logs and falls back to the heuristic so startup never fails.
func buildTokenCounter(cfg Config) agent.TokenCounter {
	switch cfg.Tokenizer {
	case "tiktoken":
		tc, err := tokenizer.NewForModel(cfg.Model)
		if err != nil {
			slog.Warn("tiktoken counter unavailable; falling back to heuristic", "model", cfg.Model, "err", err)
			return agent.HeuristicTokenCounter{}
		}
		slog.Info("token counter: tiktoken (offline vocab)", "model", cfg.Model)
		return tc
	default:
		slog.Info("token counter: heuristic (dependency-free)")
		return agent.HeuristicTokenCounter{}
	}
}

// buildCompactor selects the Compactor from cfg.Compaction. The default
// ("heuristic"/empty) returns the single-summary HeuristicCompactor. "cascade"
// returns the tiered CascadeCompactor reducing toward defaultCompactionTargetRatio
// (below the trigger ratio, for hysteresis).
func buildCompactor(cfg Config, provider port.LLMProvider, counter agent.TokenCounter) agent.Compactor {
	switch cfg.Compaction {
	case "cascade":
		slog.Info("compaction strategy: cascade (snip→strip→collapse→summarize)")
		return agent.CascadeCompactor{
			Counter:      counter,
			BudgetTokens: int(float64(defaultContextWindowTokens) * defaultCompactionTargetRatio),
			LLM:          provider,
			Model:        cfg.Model,
		}
	default:
		slog.Info("compaction strategy: heuristic (single-summary)")
		return agent.HeuristicCompactor{}
	}
}

// buildCatalog registers the always-available core tools (Read, Edit, Write, Grep,
// Glob, WebFetch), a read-only Task subagent, and — only when a shell is configured
// — the optional Bash tool. It then optionally registers Fork, memory, skills, the
// repo map, and connects any MCP servers. The returned close func tears down the
// MCP manager on shutdown.
func buildCatalog(ctx context.Context, cfg Config, provider port.LLMProvider, hooks port.HookRunner) (*tool.Catalog, mcp.Provider, []mcpsource.SourceInfo, func()) {
	cat := tool.NewCatalog()
	for _, t := range tools.All() {
		cat.MustRegister(t)
	}
	if runner := buildCommandRunner(cfg); runner != nil {
		cat.MustRegister(tools.NewBashTool(runner))
		slog.Info("Bash tool ENABLED", "shell", cfg.Shell, "cwd", cfg.Workspace)
	} else {
		slog.Info("Bash tool DISABLED (shell-less mode): the agent has no command execution",
			"reason", bashDisabledReason(cfg))
	}
	agentReg := resolveAgentRegistry(ctx, cfg)
	cat.MustRegister(buildTaskTool(cfg, provider, hooks, agentReg))

	// Fork fan-out tool: a scoped read-only child Engine (no Fork/Task, so a branch
	// cannot recurse) run against an isolated forked workspace.
	if cfg.EnableFork {
		fk := forker.New(func(root string) (tool.Workspace, error) { return osfs.NewWorkspace(root) })
		forkChild := buildChildEngine(cfg, provider)
		cat.MustRegister(agent.NewForkTool(forkChild, fk, agent.WithForkSubagentStopHook(hooks)))
		slog.Info("Fork tool ENABLED (parallel isolated child branches)")
	} else {
		slog.Info("Fork tool DISABLED")
	}

	// Memory tools: opt-in, registered only when a per-project memory directory is
	// configured via MemoryDir.
	if cfg.MemoryDir != "" {
		store, err := memory.New(cfg.MemoryDir)
		if err != nil {
			slog.Warn("could not open memory store; memory tools disabled", "dir", cfg.MemoryDir, "err", err)
		} else if err := memory.Register(cat, store); err != nil {
			slog.Warn("registering memory tools failed; some tools may be missing", "err", err)
		} else {
			slog.Info("memory tools ENABLED (Remember/Recall)", "dir", cfg.MemoryDir)
			startMemoryConsolidation(ctx, cfg, store, provider)
		}
	} else {
		slog.Info("memory tools DISABLED (memory dir empty)")
		if cfg.MemoryConsolidateInterval > 0 {
			slog.Warn("memory consolidation interval is a no-op without a memory dir (memory is disabled)",
				"interval", cfg.MemoryConsolidateInterval)
		}
	}

	registerSkills(ctx, cfg, cat)

	// Repo-map tool (Aider-style ranked codebase overview). CGO-free (tree-sitter via
	// WebAssembly), so it ships in the default static build with no build tag.
	if cfg.EnableRepoMap {
		cat.MustRegister(repomap.NewTool())
		slog.Info("repo map tool ENABLED (CGO-free tree-sitter via WebAssembly)")
	} else {
		slog.Info("repo map tool DISABLED")
	}

	mcpProvider, mcpInventory, mcpClose := registerMCP(ctx, cfg, cat)
	return cat, mcpProvider, mcpInventory, mcpClose
}

// registerMCP RESOLVES the MCP server inventory from the pluggable source list
// (static MCPServers entries first, then the live ToolHive workload source when
// ToolHiveEnabled), connects the merged set, and registers their tools into cat.
// It is non-fatal end to end: per-source SkipErrors and per-server connect failures
// are logged and skipped; a manager that fails entirely is logged and skipped.
func registerMCP(ctx context.Context, cfg Config, cat *tool.Catalog) (mcp.Provider, []mcpsource.SourceInfo, func()) {
	opts := mcpsource.ResolveOptions{
		StaticServers:   cfg.MCPServers,
		ToolHiveEnabled: cfg.ToolHiveEnabled,
		ToolHiveGroup:   cfg.ToolHiveGroup,
	}
	sources := mcpsource.ResolveSources(opts)

	configs, inventory, skips := mcpsource.Resolve(ctx, sources)
	for _, s := range skips {
		slog.Warn("MCP server skipped", "name", s.Server, "reason", s.Reason)
	}
	if len(configs) == 0 {
		slog.Info("MCP DISABLED (no servers resolved from any source)",
			"toolhive", cfg.ToolHiveEnabled, "static", len(cfg.MCPServers))
		return nil, inventory, func() {}
	}

	onError := func(sc mcp.ServerConfig, err error) {
		slog.Warn("MCP server unreachable; skipping", "name", sc.Name, "url", sc.URL, "err", err)
	}
	mgr, err := mcp.NewManager(ctx, configs, onError)
	if err != nil {
		slog.Warn("MCP manager construction failed; continuing without MCP tools", "err", err)
		return nil, inventory, func() {}
	}
	if err := mcp.Register(cat, mgr.Tools()); err != nil {
		slog.Warn("registering MCP tools failed; some tools may be missing", "err", err)
	}
	slog.Info("MCP tools registered", "servers", len(configs), "tools", len(mgr.Tools()))

	if cfg.MCPResourceTools {
		registered, rerr := mcp.RegisterResourceTools(cat, mgr)
		switch {
		case rerr != nil:
			slog.Warn("registering MCP resource tools failed", "err", rerr)
		case registered:
			slog.Info("MCP resource tools ENABLED (ListMcpResources/ReadMcpResource)")
		default:
			slog.Info("MCP resource tools DISABLED (no connected server exposes a resource)")
		}
	} else {
		slog.Info("MCP resource tools DISABLED")
	}

	return mgr, inventory, func() {
		if err := mgr.Close(); err != nil {
			slog.Warn("MCP manager close", "err", err)
		}
	}
}

// logMCPInventory logs a one-line-per-source summary of the resolved MCP source
// inventory, keeping the resolution observable.
func logMCPInventory(inventory []mcpsource.SourceInfo) {
	for _, src := range inventory {
		slog.Info("MCP source resolved",
			"source", src.Name, "kind", src.Kind, "group", src.Group,
			"servers", len(src.Servers), "diagnostics", len(src.Diagnostics))
	}
}

// registerSkills wires the progressive-disclosure Skill tool from the resolved
// Source list (explicit dirs + conventional locations when enabled). Skills stay
// OPT-IN: with no sources, nothing is registered. The SkillDraft tool is registered
// when SkillsDraftDir is set, even with no active sources (to close the
// author→promote→active loop).
func registerSkills(ctx context.Context, cfg Config, cat *tool.Catalog) {
	sources := skills.ResolveSources(skills.ResolveOptions{
		Explicit:     cfg.SkillsDirs,
		Conventional: cfg.SkillsConventional,
		Workspace:    cfg.Workspace,
	})
	if len(sources) == 0 && cfg.SkillsDraftDir != "" {
		registerSkillDraft(cfg, cat, nil)
		return
	}
	if len(sources) == 0 {
		slog.Info("skills DISABLED (no skills dirs configured)")
		return
	}

	discovered, skips, err := skills.RegisterSource(ctx, cat, skills.NewMultiSource(sources...))
	for _, s := range skips {
		slog.Warn("skill skipped", "path", s.Path, "reason", s.Reason)
	}
	switch {
	case err != nil:
		slog.Warn("registering skills failed; Skill tool disabled",
			"dirs", strings.Join(cfg.SkillsDirs, ","), "conventional", cfg.SkillsConventional, "err", err)
	case len(discovered) == 0:
		slog.Info("skills DISABLED (no valid SKILL.md found in any source)",
			"dirs", strings.Join(cfg.SkillsDirs, ","), "conventional", cfg.SkillsConventional)
	default:
		names := make([]string, 0, len(discovered))
		for _, s := range discovered {
			names = append(names, s.Name)
		}
		slog.Info("Skill tool ENABLED",
			"dirs", strings.Join(cfg.SkillsDirs, ","), "conventional", cfg.SkillsConventional,
			"count", len(discovered), "skills", strings.Join(names, ","))
	}

	registerSkillDraft(cfg, cat, discovered)
}

// registerSkillDraft registers the writable SkillDraft tool when SkillsDraftDir is
// set, binding a DirDrafter to the quarantine dir and the snapshot of currently
// active skills (for the offline novelty check). The structural trust boundary is
// enforced by validateSkillDraftConfig at engine-build time.
func registerSkillDraft(cfg Config, cat *tool.Catalog, existing []skills.Skill) {
	if cfg.SkillsDraftDir == "" {
		slog.Info("SkillDraft tool DISABLED (no skills-draft dir)")
		return
	}
	drafter := skills.NewDirDrafter(cfg.SkillsDraftDir, existing,
		skills.WithSimilarityThreshold(cfg.SkillsDraftThreshold))
	cat.MustRegister(skills.NewDraftTool(drafter))
	slog.Info("SkillDraft tool ENABLED (model-authored skills -> quarantine -> operator promote)",
		"quarantine", cfg.SkillsDraftDir, "similarity_threshold", cfg.SkillsDraftThreshold,
		"snapshot_skills", len(existing))
}

// startMemoryConsolidation launches the dream consolidator on a background
// goroutine when MemoryConsolidateInterval is positive. It shares ctx (so the loop
// exits on shutdown) and the same LLM provider as the agent.
func startMemoryConsolidation(ctx context.Context, cfg Config, store tool.MemoryStore, provider port.LLMProvider) {
	if cfg.MemoryConsolidateInterval <= 0 {
		slog.Info("memory consolidation DISABLED")
		return
	}
	cons := dream.New(store, provider, dream.Config{Model: cfg.Model})
	slog.Info("memory consolidation ENABLED (dream)", "interval", cfg.MemoryConsolidateInterval, "model", cfg.Model)
	go func() {
		err := cons.RunPeriodically(ctx, cfg.MemoryConsolidateInterval, func(err error) {
			slog.Warn("memory consolidation", "err", err)
		})
		if err != nil && !errors.Is(err, context.Canceled) {
			slog.Warn("memory consolidation loop stopped", "err", err)
		}
	}()
}

// buildCommandRunner builds the local command runner the Bash tool executes
// against, rooted at the workspace. It returns nil when command execution is
// disabled (NoBash, or an empty Shell), in which case Bash is not registered.
func buildCommandRunner(cfg Config) tool.CommandRunner {
	if cfg.NoBash || cfg.Shell == "" {
		return nil
	}
	runner, err := osfs.NewCommandRunnerShell(cfg.Workspace, cfg.Shell)
	if err != nil {
		slog.Warn("could not build command runner; Bash tool disabled", "workspace", cfg.Workspace, "err", err)
		return nil
	}
	return runner
}

// bashDisabledReason returns a short human-readable reason Bash is disabled.
func bashDisabledReason(cfg Config) string {
	switch {
	case cfg.NoBash:
		return "bash disabled"
	case cfg.Shell == "":
		return "shell is empty"
	default:
		return "command runner unavailable"
	}
}

// buildChildEngine constructs a child *Engine scoped to the read-only explorer
// toolset (Read/Grep/Glob ONLY — no Fork/Task, so a child can never recurse or fan
// out further) under an allow-all, non-interactive policy. Both the Task subagent
// and the Fork fan-out tool share this child shape.
func buildChildEngine(cfg Config, provider port.LLMProvider) *agent.Engine {
	childCat := tool.NewCatalog()
	childCat.MustRegister(tools.ReadTool{})
	childCat.MustRegister(tools.GrepTool{})
	childCat.MustRegister(tools.GlobTool{})

	childPolicy := permpolicy.NewPolicy([]governance.Rule{{Effect: governance.Allow}})

	return agent.NewEngine(agent.Deps{
		LLM:                 provider,
		Catalog:             childCat,
		Policy:              childPolicy,
		Hooks:               hookexec.New(nil),
		PromptConfig:        promptConfig(cfg),
		Model:               cfg.Model,
		ContextWindowTokens: defaultContextWindowTokens,
		CompactionRatio:     defaultCompactionRatio,
	})
}

// buildTaskTool constructs the Task subagent tool over a default child Engine
// scoped to the read-only explorer toolset, PLUS the per-definition read-only
// child engines resolved from the agent registry (Tier 1). When the registry is
// empty the per-def map is nil and Task behaves exactly as before (default
// explorer only); otherwise the model can route to a named specialist via the
// Task `agent` arg, and the specialist names+descriptions are surfaced in the
// Task spec for progressive disclosure.
func buildTaskTool(cfg Config, provider port.LLMProvider, hooks port.HookRunner, reg *agents.Registry) tool.Tool {
	engines, meta := buildAgentTaskEngines(cfg, provider, reg)
	return agent.NewTaskTool(
		buildChildEngine(cfg, provider),
		agent.WithSubagentStopHook(hooks),
		agent.WithAgentEngines(engines, meta),
	)
}

// applyTeamConfig wires the opt-in agent-teams capability into the server.Config.
// When cfg.EnableTeams is false it leaves MemberEngine nil (CreateTeam stays
// ErrTeamsDisabled). When enabled it installs the per-member engine factory, the
// workspace forker (so a Mutating member runs in an isolated fork — same wiring as
// buildCatalog's Fork branch), and ONE shared team hooks runner threaded through
// BOTH the supervisor (TeammateIdle) and the member coordination tools (the
// TaskCreated / TaskCompleted gates), so a team's lifecycle hooks all flow through
// a single runner. MaxTeams is left at zero so the server applies its own default.
func applyTeamConfig(svcCfg *server.Config, cfg Config, provider port.LLMProvider) {
	if !cfg.EnableTeams {
		slog.Info("agent teams DISABLED (set --enable-teams to enable; experimental)")
		return
	}
	// A single hooks runner shared by the supervisor and the member coordination
	// tools. hookexec.New(nil) matches buildEngine's default: the configured-hook map
	// is not yet wired from cfg anywhere, so this is an inert (no-op) runner today,
	// but it is the injection seam once it is.
	teamHooks := hookexec.New(nil)
	svcCfg.MemberEngine = buildMemberEngine(cfg, provider, teamHooks)
	svcCfg.Forker = forker.New(func(root string) (tool.Workspace, error) { return osfs.NewWorkspace(root) })
	svcCfg.TeamHooks = teamHooks
	slog.Info("agent teams ENABLED (experimental; CreateTeam/SpawnTeammate/RunTeam)")
}

// buildMemberEngine returns the per-member engine factory the server uses to build
// each team member's Engine. It mirrors buildChildEngine's allow-all, non-interactive
// shape but shapes the catalog from the member spec:
//
//   - Base (always, read-only): Read, Grep, Glob.
//   - Mutating member only: Edit, Write, and the Bash tool when a command runner is
//     available. A read-only (base-sharing) member gets NONE of these — the
//     supervisor's AddMember REJECTS a non-Mutating member whose catalog holds a
//     workspace-mutating tool, so handing a read-only member Edit/Write/Bash would
//     fail Spawn.
//   - Always: the team coordination tools (MemberTools) bound to the shared team and
//     this member's name, sharing teamHooks with the supervisor.
//
// It NEVER includes Task or Fork: a member must not recurse or fan out further.
func buildMemberEngine(cfg Config, provider port.LLMProvider, teamHooks port.HookRunner) server.MemberEngineFactory {
	return func(t *team.Team, spec agent.MemberSpec) *agent.Engine {
		cat := tool.NewCatalog()
		cat.MustRegister(tools.ReadTool{})
		cat.MustRegister(tools.GrepTool{})
		cat.MustRegister(tools.GlobTool{})
		if spec.Mutating {
			cat.MustRegister(tools.EditTool{})
			cat.MustRegister(tools.WriteTool{})
			if runner := buildCommandRunner(cfg); runner != nil {
				cat.MustRegister(tools.NewBashTool(runner))
			}
		}
		for _, mt := range agent.MemberTools(t, spec.Name, teamHooks) {
			cat.MustRegister(mt)
		}

		return agent.NewEngine(agent.Deps{
			LLM:                 provider,
			Catalog:             cat,
			Policy:              permpolicy.NewPolicy([]governance.Rule{{Effect: governance.Allow}}),
			Hooks:               hookexec.New(nil),
			PromptConfig:        promptConfig(cfg),
			Model:               cfg.Model,
			ContextWindowTokens: defaultContextWindowTokens,
			CompactionRatio:     defaultCompactionRatio,
		})
	}
}

// promptConfig builds the system-prompt configuration. The volatile Env values
// (cwd/os/model/date/mode) are computed HERE in the composition layer so the domain
// stays infra-free; the loop fills in the per-turn Mode and Tools.
func promptConfig(cfg Config) prompt.Config {
	return prompt.Config{
		Env: prompt.Env{
			Cwd:   cfg.Workspace,
			OS:    runtime.GOOS,
			Model: cfg.Model,
			Date:  time.Now().Format("2006-01-02"),
			Mode:  string(session.ModeDefault),
		},
	}
}

// defaultRules is the built-in permission ruleset: read-only tools (Read, Grep,
// Glob, WebFetch, the Task explorer) are allowed; mutating tools (Bash, Edit, Write)
// and the writable SkillDraft tool ask for approval. Anything unmatched defaults to
// ask via the evaluator.
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
		{Scope: governance.ScopeManaged, Tool: skills.DraftToolName, Effect: governance.Ask},
	}
}

// defaultLimits returns the non-zero stop limits injected for sessions created
// without explicit limits, so a default session is always bounded (a zero Limits
// value disables every stop condition in package session).
func defaultLimits() session.Limits {
	return session.Limits{
		MaxTurns:               defaultMaxTurns,
		MaxToolCalls:           defaultMaxToolCalls,
		MaxConsecutiveFailures: defaultMaxConsecutiveFailures,
	}
}

// osfsWorkspaceFactory returns a server.WorkspaceFactory that builds an osfs
// Workspace rooted at the session's workspace dir. A root that cannot be opened
// yields a nil Workspace; tool calls against it return errors the model can read.
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
