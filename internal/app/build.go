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
	"github.com/stacklok/mecatl/internal/adapter/permstore"
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

	// ForkPreservedCap bounds how many PRESERVED winner forks (join=first /
	// join=judge) survive at once across the process: a new winner beyond the cap
	// LRU-reaps the oldest preserved fork. Zero uses agent.DefaultPreservedForkCap.
	// Preserved forks remain the deliverable — they are inspectable/mergeable — but
	// are capped so many Fork calls cannot grow disk without bound.
	ForkPreservedCap int

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
	engine, mainMgr, mcpProvider, mcpInventory, sessFactory, learned, mcpClose, err := buildEngine(ctx, cfg, provider, store)
	if err != nil {
		return nil, err
	}
	logMCPInventory(mcpInventory)

	// The command lister backs ListCommands (the TUI palette). It reuses the SAME
	// expander build the engine consumes, so the palette offers exactly the
	// commands a "/<cmd>" prompt would expand. nil when commands are disabled.
	commandLister := buildCommandLister(cfg, mcpProvider)

	svcCfg := server.Config{
		Engine:        engine,
		Store:         store,
		Workspaces:    osfsWorkspaceFactory(),
		DefaultLimits: defaultLimits(),
		MCPProvider:   mcpProvider,
		MCPSources:    mcpInventory,
		// Live re-probe: ListMcpSources re-consults the resolved sources on each call
		// so a TUI panel refresh (ctrl+o → ctrl+r) reflects CURRENT source status,
		// not just this startup snapshot. nil when MCP is unconfigured (keeps the
		// empty snapshot). Resolution is idempotent + read-only, like the agent
		// registry re-resolution below.
		MCPSourceProber: mcpSourceProber(cfg),
		// ListAgents snapshot: resolve the agent registry once here and project it
		// into the proto form. Discovery is idempotent file scanning (buildCatalog
		// resolves the same registry for the Task tool), so this re-resolution is
		// cheap and keeps the snapshot a pure read at request time.
		Agents: agentSnapshot(cfg, resolveAgentRegistry(ctx, cfg)),
		// ListCommands palette discovery: a workspace-aware lister over the same
		// command expander build the engine uses. nil disables the RPC (empty list).
		Commands: commandLister,
		// Per-session client MCP (ACP session/new mcpServers): builds a scoped engine
		// over the client's streaming-HTTP servers, mounted for that session only. Built
		// in buildEngine so it shares the main engine's exact collaborators.
		SessionEngine: sessFactory,
		// Evict a session's LEARNED permission rules when the session is closed
		// (issue #3): the rules are per-session and non-durable, so they must not
		// outlive the session that learned them.
		//
		// CloseSession is now reachable over all three surfaces (issue #10): the ACP
		// adapter (on editor disconnect), the gRPC CloseSession RPC, and HTTP DELETE
		// /v1/sessions/{id}. So a well-behaved client evicts a session's learned rules
		// at session end across every transport. As a client-independent backstop, the
		// learned-rule slice is also capped (permstore.maxRulesPerSession) so a
		// pathological long-lived session that never signals end cannot grow it without
		// bound (each rule still requires a human allow-always approval). TTL/idle
		// eviction remains a follow-up; see docs/adr/0001-acp-adapter.md.
		OnCloseSession: learned.Forget,
	}
	applyTeamConfig(&svcCfg, cfg, provider, mainMgr)

	svc, err := server.NewService(svcCfg)
	if err != nil {
		mcpClose()
		return nil, fmt.Errorf("build service: %w", err)
	}
	// Close tears down the main MCP manager AND any per-session client-MCP engines
	// still registered (svc.Close), so a process exit leaks neither.
	closeAll := func() {
		svc.Close()
		mcpClose()
	}
	return &Built{Service: svc, Close: closeAll}, nil
}

// sessionEngineFactory returns the server.SessionEngineFactory that builds a
// PER-SESSION engine over the client-provided streaming-HTTP MCP servers (the ACP
// session/new mcpServers). Each call connects a SCOPED mcp.NewManager for that one
// session (best-effort, exactly like defMCPTools: a down server is logged-and-
// skipped, never fatal), registers the CORE tools (registerCoreTools — the same
// core toolset the main engine gets) PLUS those MCP tools into a fresh catalog, and
// builds an engine whose every collaborator MATCHES the main engine via
// baseEngineDeps (so a per-session engine compacts, expands commands, persists, and
// emits telemetry exactly like the shared one — only the catalog differs).
//
// It captures the SAME store/policy/hooks/token-counter the main engine was built
// with (threaded from Build, where they are already in scope), so the two engines
// cannot drift on their Deps. The mcpProvider passed to baseEngineDeps is the MAIN
// provider, so a client-MCP session's CommandExpander mirrors main's command source
// (file commands + main MCP prompts); per-session MCP prompts-as-commands is a
// future refinement, not required here.
//
// It returns the engine and the manager's Close so the Service can tear that
// session's MCP connections down on disconnect. The factory is wired into
// server.Config.SessionEngine in Build, so the ACP adapter can mount client MCP
// without app having to leak mcp/agent wiring into the server or acp layers.
func sessionEngineFactory(
	cfg Config,
	provider port.LLMProvider,
	store port.SessionStore,
	policy port.PermissionPolicy,
	hooks port.HookRunner,
	counter agent.TokenCounter,
	mcpProvider mcp.Provider,
	instructions prompt.InstructionAssembler,
) server.SessionEngineFactory {
	return func(ctx context.Context, specs []mcp.ServerConfig) (*agent.Engine, func() error, error) {
		onError := func(sc mcp.ServerConfig, err error) {
			slog.Warn("client MCP server unreachable; skipping for this session",
				"server", sc.Name, "url", sc.URL, "err", err)
		}
		mgr, err := mcp.NewManager(ctx, specs, onError)
		if err != nil {
			// Best-effort: every server failed. The session still gets a usable engine
			// (core tools only) rather than failing session creation outright.
			slog.Warn("client MCP: no servers connected for this session; mounting core tools only", "err", err)
		}

		cat := tool.NewCatalog()
		registerCoreTools(cfg, cat, false)
		closeFn := func() error { return nil }
		if mgr != nil {
			if rerr := mcp.Register(cat, mgr.Tools()); rerr != nil {
				slog.Warn("client MCP: registering tools failed; some may be missing", "err", rerr)
			}
			closeFn = mgr.Close
			slog.Info("client MCP mounted for session",
				"servers", len(mgr.Servers()), "tools", len(mgr.Tools()))
		}

		// Identical to the main engine in every Deps field except the catalog (which
		// carries the extra client MCP tools): baseEngineDeps is the single source of
		// that shared wiring, so no collaborator is silently dropped.
		deps := baseEngineDeps(cfg, provider, store, policy, hooks, counter, mcpProvider, instructions)
		deps.Catalog = cat
		return agent.NewEngine(deps), closeFn, nil
	}
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
// (a no-op when no servers are configured), and the per-session client-MCP engine
// factory (built HERE because store/policy/hooks/counter/mcpProvider — the exact
// collaborators a per-session engine must share with the main one — are all in
// scope here, so the factory cannot drift from the main engine's Deps).
func buildEngine(ctx context.Context, cfg Config, provider port.LLMProvider, store port.SessionStore) (*agent.Engine, *mcp.Manager, mcp.Provider, []mcpsource.SourceInfo, server.SessionEngineFactory, *permstore.Memory, func(), error) {
	// SkillDraft trust boundary: when enabled, the quarantine dir must live OUTSIDE
	// the workspace root (so the model's workspace-confined Write/Edit cannot reach
	// it) and be disjoint from every active skills dir. Fatal on a misconfig.
	if err := validateSkillDraftConfig(cfg); err != nil {
		return nil, nil, nil, nil, nil, nil, func() {}, err
	}
	warnSkillDraftResiduals(cfg)

	// Per-session learned-rule store (issue #3): an ACP "allow always" verdict
	// records a narrow tool+exact-pattern allow here, scoped to the session; the
	// policy merges it in at the lowest scope on every evaluation. In-memory and
	// non-durable by design — rules are dropped on CloseSession (Forget) and on
	// process restart. Returned to Build so it can wire Forget into CloseSession.
	// The SAME policy (and thus store) is shared with every per-session client-MCP
	// engine via sessionEngineFactory, so an MCP-mounted session learns identically.
	learned := permstore.New()
	policy := permpolicy.NewPolicy(defaultRules(), learned)
	hooks := hookexec.New(nil) // no hooks by default; map is the injection seam

	cat, mainMgr, mcpProvider, mcpInventory, memStore, mcpClose := buildCatalog(ctx, cfg, provider, hooks)

	counter := buildTokenCounter(cfg)

	// Instructions seam: RootAssembler (AGENTS.md/CLAUDE.md) always; when a memory
	// store is wired, also the tier-0 MemoryIndexAssembler so the model sees its
	// saved-memory index at turn 0. The adapter (*memory.Store) meets the
	// prompt-defined MemoryIndexSource port HERE, in the composition layer — prompt
	// never imports the memory adapter. The index rides as a turn-0 user message
	// (after the cache breakpoint), so it never enters prompt.Build's StablePrefix.
	instructions := buildInstructionAssembler(memStore)

	deps := baseEngineDeps(cfg, provider, store, policy, hooks, counter, mcpProvider, instructions)
	deps.Catalog = cat
	sessFactory := sessionEngineFactory(cfg, provider, store, policy, hooks, counter, mcpProvider, instructions)
	return agent.NewEngine(deps), mainMgr, mcpProvider, mcpInventory, sessFactory, learned, mcpClose, nil
}

// buildInstructionAssembler composes the turn-0 instruction assembler: always the
// RootAssembler (project instruction files), plus the tier-0 MemoryIndexAssembler
// when a memory store is wired (memStore non-nil). When memStore is nil the
// MemoryIndexAssembler is omitted entirely, so a memory-disabled deployment adds
// no index machinery. The store satisfies prompt.MemoryIndexSource structurally;
// this is the one place the adapter meets the port.
func buildInstructionAssembler(memStore *memory.Store) prompt.InstructionAssembler {
	if memStore == nil {
		return prompt.RootAssembler{}
	}
	return prompt.NewMultiAssembler(
		prompt.RootAssembler{},
		prompt.MemoryIndexAssembler{Src: memStore},
	)
}

// baseEngineDeps assembles the agent.Deps SHARED by the main engine (buildEngine)
// and every per-session client-MCP engine (sessionEngineFactory): identical
// provider/store/sink/logger/policy/hooks/prompt/model/context-window/token-counter/
// compactor/command-expander wiring. ONLY the Catalog differs between the two sites
// (the per-session engine adds the client's MCP tools), so the caller sets Catalog
// after this returns. Centralising every other field here is the drift guard the
// [High] review called for: adding a new Deps field updates THIS one helper, so a
// per-session engine can never silently lose a collaborator (compactor, token
// counter, command expander, store, sink, logger) the main engine has.
//
// Policy is shared deliberately: a client-MCP session must resolve permissions
// through the SAME interactive defaultRules the main engine uses (NOT the allow-all
// child-engine policy), so an MCP-mounted session is governed identically.
func baseEngineDeps(
	cfg Config,
	provider port.LLMProvider,
	store port.SessionStore,
	policy port.PermissionPolicy,
	hooks port.HookRunner,
	counter agent.TokenCounter,
	mcpProvider mcp.Provider,
	instructions prompt.InstructionAssembler,
) agent.Deps {
	return agent.Deps{
		LLM:          provider,
		Policy:       policy,
		Hooks:        hooks,
		Instructions: instructions,
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

// buildCommandLister builds the server.CommandLister backing the ListCommands
// RPC (the TUI palette). It reuses buildCommandExpander — the SAME expander the
// engine consumes on the run path — so the palette enumerates exactly the
// commands a "/<cmd>" prompt would expand. It returns nil (RPC yields an empty
// list) when the expander cannot enumerate, i.e. it is the NoopExpander (commands
// disabled) or does not implement prompt.CommandLister. The lister opens a fresh
// osfs Workspace per request rooted at the requested workspace, so discovery
// reflects the CURRENT command files on disk (not a startup snapshot).
func buildCommandLister(cfg Config, mcpProvider mcp.Provider) server.CommandLister {
	exp := buildCommandExpander(cfg, mcpProvider)
	lister, ok := exp.(prompt.CommandLister)
	if !ok {
		return nil
	}
	if _, isNoop := exp.(prompt.NoopExpander); isNoop {
		// The NoopExpander lists nothing; skip the RPC wiring entirely so the
		// palette stays empty without a per-request workspace open.
		return nil
	}
	return commandListerFunc(func(ctx context.Context, root string) ([]server.Command, error) {
		ws, err := osfs.NewWorkspace(root)
		if err != nil {
			return nil, fmt.Errorf("open workspace %q: %w", root, err)
		}
		cmds, err := lister.List(ctx, ws)
		if err != nil {
			return nil, err
		}
		out := make([]server.Command, 0, len(cmds))
		for _, c := range cmds {
			out = append(out, server.Command{Name: c.Name, Description: c.Description})
		}
		return out, nil
	})
}

// commandListerFunc adapts a function to the server.CommandLister interface, the
// same lightweight-adapter idiom mcpSourceProber uses for its prober closure.
type commandListerFunc func(ctx context.Context, root string) ([]server.Command, error)

// List implements server.CommandLister.
func (f commandListerFunc) List(ctx context.Context, root string) ([]server.Command, error) {
	return f(ctx, root)
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

// registerCoreTools registers the always-available core tools (Read, Edit, Write,
// Grep, Glob, WebFetch) plus the optional Bash tool when a shell is configured,
// into cat. It is the single source of truth for the CORE toolset shared by its TWO
// call sites — the main catalog (buildCatalog) and the per-session MCP engine
// (sessionEngineFactory) — so the two cannot drift on which core tools a session
// gets. (The agent-def / team / fork child catalogs deliberately register a
// NARROWER toolset and do NOT call this, so they are not call sites.) It logs the
// Bash enable/disable decision only when log is true, so the per-session path (which
// runs per session/new) stays quiet while the once-at-startup main path narrates.
func registerCoreTools(cfg Config, cat *tool.Catalog, log bool) {
	for _, t := range tools.All() {
		cat.MustRegister(t)
	}
	if runner := buildCommandRunner(cfg); runner != nil {
		cat.MustRegister(tools.NewBashTool(runner))
		if log {
			slog.Info("Bash tool ENABLED", "shell", cfg.Shell, "cwd", cfg.Workspace)
		}
	} else if log {
		slog.Info("Bash tool DISABLED (shell-less mode): the agent has no command execution",
			"reason", bashDisabledReason(cfg))
	}
}

// buildCatalog registers the always-available core tools (Read, Edit, Write, Grep,
// Glob, WebFetch), a read-only Task subagent, and — only when a shell is configured
// — the optional Bash tool. It then optionally registers Fork, memory, skills, the
// repo map, and connects any MCP servers. The returned close func tears down the
// MCP manager on shutdown.
func buildCatalog(ctx context.Context, cfg Config, provider port.LLMProvider, hooks port.HookRunner) (*tool.Catalog, *mcp.Manager, mcp.Provider, []mcpsource.SourceInfo, *memory.Store, func()) {
	cat := tool.NewCatalog()
	registerCoreTools(cfg, cat, true)
	// memStore is the per-project memory store, returned so the caller can bind it
	// to the prompt tier-0 index source. It stays nil when memory is disabled.
	var memStore *memory.Store
	// Connect the MAIN MCP servers FIRST, so the per-agent-def Task engines built by
	// buildTaskTool can (a) pull a REFERENCED main server's tools out of this manager
	// and (b) connect their own INLINE servers. The main manager is registered into
	// the parent catalog here; the def engines get only the servers their mcpServers
	// opts into. mainMgr is nil when no main servers are configured (reference
	// entries then resolve to a clear "unknown server" diagnostic).
	mainMgr, mcpProvider, mcpInventory, mcpClose := registerMCP(ctx, cfg, cat)

	agentReg := resolveAgentRegistry(ctx, cfg)
	taskTool, taskMCPClose := buildTaskTool(ctx, cfg, provider, hooks, agentReg, mainMgr)
	cat.MustRegister(taskTool)
	// Aggregate the per-def INLINE MCP managers' teardown into the main MCP close, so
	// Built.Close tears them ALL down on shutdown (process-lifetime engines).
	mcpClose = composeClose(taskMCPClose, mcpClose)

	// Fork fan-out tool: a scoped child Engine (no Fork/Task/ToolSearch, so a branch
	// cannot recurse) run against an ISOLATED forked workspace. Unlike the Task
	// subagent, the Fork branch child MAY mutate (Edit/Write/Bash-if-configured):
	// that is safe because every branch writes only to its own fork, never the
	// parent base, so ForkTool.ReadOnly() stays true. The judge is a SEPARATE,
	// tool-less read-only Engine built from a distinct provider concern so its LLM
	// calls never interleave with the branches' (matters for the mockllm cursor in
	// tests; harmless for the stateless OpenAI adapter).
	if cfg.EnableFork {
		// WithForceCopy: Fork branches MUTATE and run Bash (incl. git), so they get
		// FULLY isolated forks (a full copy incl. .git — own object DB/refs) rather
		// than a worktree that shares the base repo's .git. This stops a branch's
		// git commit/push/update-ref from escaping into the base repo.
		fk := forker.New(func(root string) (tool.Workspace, error) { return osfs.NewWorkspace(root) }, forker.WithForceCopy())
		forkChild := buildForkChildEngine(cfg, provider, buildCommandRunner(cfg))
		judge := agent.NewEngineJudge(buildForkJudgeEngine(cfg, provider))
		// Bound the PRESERVED winner forks (join=first/judge): an LRU reaper keeps the
		// most-recent N and tears down the oldest beyond the cap, so a long-lived
		// process running many Fork calls cannot leak winner forks unboundedly. The
		// winner stays inspectable until it falls off the LRU tail.
		preservedCap := cfg.ForkPreservedCap
		if preservedCap <= 0 {
			preservedCap = agent.DefaultPreservedForkCap
		}
		cat.MustRegister(agent.NewForkTool(forkChild, fk,
			agent.WithForkSubagentStopHook(hooks),
			agent.WithForkJudge(judge),
			agent.WithWinnerReaper(agent.NewLRUForkReaper(preservedCap))))
		slog.Info("Fork tool ENABLED (parallel isolated MUTATING child branches; judge selection wired)",
			"preserved_fork_cap", preservedCap)
	} else {
		slog.Info("Fork tool DISABLED")
	}

	// Team tool: forms a team of coordinating subagents in-process, driving a
	// Supervisor over the SAME member-engine wiring the gRPC CreateTeam path uses
	// (buildTeamWiring is the single source of that wiring truth, so the two paths
	// cannot drift). Registered ONLY when teams are enabled. It is mutate-serial
	// (unlike the read-parallel Task/Fork) and defaults to ASK (see defaultRules).
	if cfg.EnableTeams {
		// buildTeamWiring always returns a non-nil forker and hooks runner under
		// EnableTeams, so they are wired unconditionally (no nil guards).
		factory, fk, teamHooks := buildTeamWiring(ctx, cfg, provider, mainMgr)
		// factory is a server.MemberEngineFactory; NewTeamTool wants the
		// agent.TeamMemberEngineFactory of identical underlying shape — an explicit
		// conversion bridges the two named types (both func(*team.Team, MemberSpec)
		// MemberBuild), so a single wiring serves both team paths.
		cat.MustRegister(agent.NewTeamTool(
			agent.TeamMemberEngineFactory(factory),
			agent.WithTeamToolForker(fk),
			agent.WithTeamToolHooks(teamHooks),
		))
		slog.Info("Team tool ENABLED (in-process coordinating subagents; mutate-serial, ASK)")
	} else {
		slog.Info("Team tool DISABLED")
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
			memStore = store
			slog.Info("memory tools ENABLED (Remember/Recall/SearchMemory)", "dir", cfg.MemoryDir)
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

	return cat, mainMgr, mcpProvider, mcpInventory, memStore, mcpClose
}

// registerMCP RESOLVES the MCP server inventory from the pluggable source list
// (static MCPServers entries first, then the live ToolHive workload source when
// ToolHiveEnabled), connects the merged set, and registers their tools into cat.
// It is non-fatal end to end: per-source SkipErrors and per-server connect failures
// are logged and skipped; a manager that fails entirely is logged and skipped.
//
// It returns the concrete *mcp.Manager (nil when no servers connect) so the
// per-agent-def wiring can pull a REFERENCED main server's tools out of it; the same
// value is the mcp.Provider used for resources/prompts.
// mcpResolveOptions derives the source-resolver options purely from cfg, so the
// startup wiring (registerMCP) and the live re-probe (mcpSourceProber) resolve
// the SAME ordered source list. Keeping this in one place stops the two paths
// from drifting on which sources exist.
func mcpResolveOptions(cfg Config) mcpsource.ResolveOptions {
	return mcpsource.ResolveOptions{
		StaticServers:   cfg.MCPServers,
		ToolHiveEnabled: cfg.ToolHiveEnabled,
		ToolHiveGroup:   cfg.ToolHiveGroup,
	}
}

// mcpSourceProber builds the live-inventory prober wired into the server.Service.
// It re-runs source.InspectSources over the SAME resolved sources on every call,
// so a client refresh reflects CURRENT source status/diagnostics (a ToolHive
// workload that crashed or appeared after startup), not the startup snapshot.
// Resolution is read-only (the ToolHive source queries the container runtime; the
// static source is in-memory) and fail-soft, matching the rest of MCP wiring. It
// returns nil when MCP is not configured (no static servers, ToolHive off) so the
// Service simply keeps using the (empty) startup snapshot.
func mcpSourceProber(cfg Config) func(ctx context.Context) []mcpsource.SourceInfo {
	opts := mcpResolveOptions(cfg)
	if len(opts.StaticServers) == 0 && !opts.ToolHiveEnabled {
		return nil
	}
	sources := mcpsource.ResolveSources(opts)
	return func(ctx context.Context) []mcpsource.SourceInfo {
		return mcpsource.InspectSources(ctx, sources, opts)
	}
}

func registerMCP(ctx context.Context, cfg Config, cat *tool.Catalog) (*mcp.Manager, mcp.Provider, []mcpsource.SourceInfo, func()) {
	opts := mcpResolveOptions(cfg)
	sources := mcpsource.ResolveSources(opts)

	configs, inventory, skips := mcpsource.Resolve(ctx, sources)
	for _, s := range skips {
		slog.Warn("MCP server skipped", "name", s.Server, "reason", s.Reason)
	}
	if len(configs) == 0 {
		slog.Info("MCP DISABLED (no servers resolved from any source)",
			"toolhive", cfg.ToolHiveEnabled, "static", len(cfg.MCPServers))
		return nil, nil, inventory, func() {}
	}

	onError := func(sc mcp.ServerConfig, err error) {
		slog.Warn("MCP server unreachable; skipping", "name", sc.Name, "url", sc.URL, "err", err)
	}
	mgr, err := mcp.NewManager(ctx, configs, onError)
	if err != nil {
		slog.Warn("MCP manager construction failed; continuing without MCP tools", "err", err)
		return nil, nil, inventory, func() {}
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

	return mgr, mgr, inventory, func() {
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

// newChildEngine bakes in the shared shape every child/member engine assembles:
// an allow-all (non-interactive) permission policy, an inert hook runner, and the
// standard context-window / compaction-trigger settings. Call sites supply only
// what actually varies between them — the scoped catalog, the resolved model, and
// the prompt config. It is the single source of truth for that boilerplate so the
// five child-engine builders (Task explorer, Fork branch, Fork judge, per-def Task
// engine, team member) cannot drift apart.
func newChildEngine(provider port.LLMProvider, cat *tool.Catalog, model string, pc prompt.Config) *agent.Engine {
	return newChildEngineWithHooks(provider, cat, model, pc, hookexec.New(nil))
}

// newChildEngineWithHooks is newChildEngine with an explicit HookRunner, so a
// per-def Task/member engine can scope its own lifecycle hooks (from a def's
// `hooks:` map) instead of the inert default. A nil hooks runner falls back to an
// inert one, preserving the no-hooks contract.
func newChildEngineWithHooks(provider port.LLMProvider, cat *tool.Catalog, model string, pc prompt.Config, hooks port.HookRunner) *agent.Engine {
	if hooks == nil {
		hooks = hookexec.New(nil)
	}
	return agent.NewEngine(agent.Deps{
		LLM:     provider,
		Catalog: cat,
		// Child/member engines are non-interactive (allow-all) and never learn:
		// nil store disables Learn entirely for them.
		Policy:              permpolicy.NewPolicy([]governance.Rule{{Effect: governance.Allow}}, nil),
		Hooks:               hooks,
		PromptConfig:        pc,
		Model:               model,
		ContextWindowTokens: defaultContextWindowTokens,
		CompactionRatio:     defaultCompactionRatio,
	})
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

	return newChildEngine(provider, childCat, cfg.Model, promptConfig(cfg))
}

// buildForkChildEngine constructs the child *Engine each Fork branch runs. Unlike
// buildChildEngine (the read-only Task explorer), a Fork branch child MAY MUTATE
// its OWN fork: it gets Read/Grep/Glob/Edit/Write, still EXCLUDING Task/Fork/
// ToolSearch (a branch must not recurse or fan out further).
//
// Bash IS registered when a runner is configured (runner != nil). The runner is
// now workspace-aware: BashTool.Execute passes the per-branch forked
// Workspace.Root() to CommandRunner.Run as the working directory, so a branch's
// Bash runs in its OWN fork — its DEFAULT cwd is the isolated fork, never the
// shared parent base. (A shell-less deployment passes a nil runner and the branch
// simply runs without Bash, exactly like the main session.)
//
// This is the behavioural shift Tier 3 enables: Fork branches can now IMPLEMENT
// (via Edit/Write AND Bash), not merely explore. It is safe — and
// ForkTool.ReadOnly() stays true — because every branch runs in its OWN isolated
// forked workspace, so a branch's Edit/Write/Bash land in its fork and (for
// relative-path operations) never touch the parent base. Bash can still escape
// its cwd via absolute paths / `cd` — that is the inherent Bash trust model, the
// same as the main session; what the fix guarantees is that the DEFAULT cwd is
// the fork, removing the accidental shared-base mutation a parent-rooted runner
// caused. The mutating winner's fork is what winner-preservation
// (join=first/judge) keeps.
func buildForkChildEngine(cfg Config, provider port.LLMProvider, runner tool.CommandRunner) *agent.Engine {
	childCat := tool.NewCatalog()
	childCat.MustRegister(tools.ReadTool{})
	childCat.MustRegister(tools.GrepTool{})
	childCat.MustRegister(tools.GlobTool{})
	childCat.MustRegister(tools.EditTool{})
	childCat.MustRegister(tools.WriteTool{})
	if runner != nil {
		// Workspace-aware: the runner honors the per-branch forked Workspace.Root()
		// the BashTool passes as workdir, so a branch's Bash runs in its OWN fork.
		childCat.MustRegister(tools.NewBashTool(runner))
	}

	return newChildEngine(provider, childCat, cfg.Model, promptConfig(cfg))
}

// buildForkJudgeEngine constructs the minimal, tool-less read-only child *Engine
// the Fork join=judge/best strategy runs to SELECT a winner. It scores text only,
// so it gets an EMPTY catalog (no tools) under an allow-all policy. It is a DISTINCT
// Engine instance from the branch child so, with the mockllm shared-cursor provider
// in tests, the judge's LLM calls never interleave with the branches'; with the
// stateless OpenAI adapter this separation is naturally harmless.
func buildForkJudgeEngine(cfg Config, provider port.LLMProvider) *agent.Engine {
	return newChildEngine(provider, tool.NewCatalog(), cfg.Model, promptConfig(cfg))
}

// buildTaskTool constructs the Task subagent tool over a default child Engine
// scoped to the read-only explorer toolset, PLUS the per-definition read-only
// child engines resolved from the agent registry (Tier 1). When the registry is
// empty the per-def map is nil and Task behaves exactly as before (default
// explorer only); otherwise the model can route to a named specialist via the
// Task `agent` arg, and the specialist names+descriptions are surfaced in the
// Task spec for progressive disclosure.
// It also threads the MAIN MCP manager so a def's mcpServers can REFERENCE a
// configured server's tools, and connects each def's INLINE servers; the returned
// close func tears those inline managers down (it is aggregated into Built.Close —
// these are process-lifetime engines). The close is nil when no def opens an inline
// server.
func buildTaskTool(ctx context.Context, cfg Config, provider port.LLMProvider, hooks port.HookRunner, reg *agents.Registry, mainMgr *mcp.Manager) (tool.Tool, func() error) {
	// Resolve the active skills once so a def's `skills:` can preload skill bodies
	// into its engine prompt. The same index is the operator-controlled skill set
	// the Skill tool serves. `hooks` is the inert default each def adopts unless its
	// own `hooks:` map scopes lifecycle hooks to its engine.
	skillIdx := resolveSkillIndex(ctx, cfg)
	engines, meta, mcpClose := buildAgentTaskEngines(ctx, cfg, provider, reg, skillIdx, hooks, mainMgr)
	return agent.NewTaskTool(
		buildChildEngine(cfg, provider),
		agent.WithSubagentStopHook(hooks),
		agent.WithAgentEngines(engines, meta),
	), mcpClose
}

// buildTeamWiring constructs the three agent-team dependencies — the unified
// per-member engine factory, the workspace forker, and the shared team hooks runner
// — that BOTH team entry points consume: the parent catalog's Team tool
// (buildCatalog) and the gRPC CreateTeam path (applyTeamConfig → server.Config). It
// is the single source of that wiring truth, so the two paths cannot drift; each
// caller invokes it and gets a functionally identical factory. It is only ever
// called under cfg.EnableTeams.
//
// The factory is server.MemberEngineFactory, which is the SAME shape as
// agent.TeamMemberEngineFactory (both `func(*team.Team, MemberSpec) MemberBuild`), so
// one factory value satisfies both the gRPC Config.MemberEngine and NewTeamTool.
//
// It resolves the agent-definition registry and skill index ONCE (exactly as
// buildCatalog shares the registry with the Task tool), and uses
// forker.WithForceCopy so a Mutating member runs in a FULLY isolated fork (own .git
// object DB/refs), matching buildCatalog's Fork branch wiring. The single teamHooks
// runner is threaded through both the supervisor (TeammateIdle) and the member
// coordination tools (TaskCreated / TaskCompleted gates). mainMgr supplies the
// per-agent MCP base manager so a member's agent definition can scope its MCP servers.
func buildTeamWiring(ctx context.Context, cfg Config, provider port.LLMProvider, mainMgr *mcp.Manager) (server.MemberEngineFactory, tool.WorkspaceForker, port.HookRunner) {
	// A single hooks runner shared by the supervisor and the member coordination
	// tools. hookexec.New(nil) matches buildEngine's default: the configured-hook map
	// is not yet wired from cfg anywhere, so this is an inert (no-op) runner today,
	// but it is the injection seam once it is.
	teamHooks := hookexec.New(nil)
	// Resolve the agent-definition registry ONCE and share it with the member
	// factory, exactly as buildCatalog shares it with the Task tool — ONE registry,
	// TWO consumers. A member whose spec.AgentType names a def adopts that def's
	// scoped catalog/model/prompt/permissionMode.
	agentReg := resolveAgentRegistry(ctx, cfg)
	skillIdx := resolveSkillIndex(ctx, cfg)
	factory := buildMemberEngine(cfg, provider, teamHooks, agentReg, skillIdx, buildCommandRunner(cfg), mainMgr)
	// WithForceCopy: Mutating team members run Bash (incl. git) in their forks, so
	// they get FULLY isolated forks (a full copy incl. .git — own object DB/refs)
	// rather than a worktree that shares the base repo's .git, keeping a member's
	// git commit/push/update-ref from escaping into the base repo. (Read-only
	// members share the base directly and never fork, so they're unaffected.)
	fk := forker.New(func(root string) (tool.Workspace, error) { return osfs.NewWorkspace(root) }, forker.WithForceCopy())
	return factory, fk, teamHooks
}

// applyTeamConfig wires the opt-in agent-teams capability into the server.Config.
// When cfg.EnableTeams is false it leaves MemberEngine nil (CreateTeam stays
// ErrTeamsDisabled). When enabled it installs the per-member engine factory, the
// workspace forker, and the shared team hooks runner — all from buildTeamWiring, the
// SAME wiring the Team tool uses (buildCatalog) — so the gRPC CreateTeam path and the
// Team tool cannot drift. MaxTeams is left at zero so the server applies its own
// default.
func applyTeamConfig(svcCfg *server.Config, cfg Config, provider port.LLMProvider, mainMgr *mcp.Manager) {
	if !cfg.EnableTeams {
		slog.Info("agent teams DISABLED (set --enable-teams to enable; experimental)")
		return
	}
	factory, fk, teamHooks := buildTeamWiring(context.Background(), cfg, provider, mainMgr)
	svcCfg.MemberEngine = factory
	svcCfg.Forker = fk
	svcCfg.TeamHooks = teamHooks
	slog.Info("agent teams ENABLED (experimental; CreateTeam/SpawnTeammate/RunTeam + Team tool)")
}

// buildMemberEngine returns the per-member engine factory the server uses to build
// each team member's engine (and its optional per-member permission mode). It
// mirrors buildChildEngine's allow-all, non-interactive shape but shapes the catalog
// from the member spec AND — Tier 1b — from the member's agent definition when
// spec.AgentType names one in the shared registry.
//
// Catalog shaping:
//
//   - DEFAULT (no/unknown AgentType): the historical member catalog — Read, Grep,
//     Glob always; plus Edit, Write, and the Bash tool (when a runner is available)
//     for a Mutating member only. Bash is workspace-aware (it runs in the member's
//     forked Workspace.Root()), so a Mutating member's Bash is fork-confined.
//   - DEFINED (known AgentType): the def's tools allowlist ∩ the member's AVAILABLE
//     base toolset, minus disallowedTools, ALWAYS excluding Task/Fork/ToolSearch.
//     The available base differs by spec.Mutating: a Mutating member (isolated fork)
//     may keep Edit/Write/Bash, so the def MAY scope them in; a read-only
//     (base-sharing) member has mutating tools DROPPED with a diagnostic, so the
//     supervisor's AddMember backstop (ErrReadOnlyMemberMutating) is never tripped.
//   - The member's model resolves def.Model > SubagentModel > parent; the def body
//     composes into the system prompt as the Role; the def's permissionMode maps to
//     a per-member session mode returned in the MemberBuild.
//
// In BOTH cases the team coordination tools (MemberTools) are ALWAYS appended after
// scoping — they bypass the def allowlist — and Task/Fork are NEVER included (a
// member must not recurse or fan out further).
//
// Unknown AgentType is FORGIVING (the skills/teams philosophy): it logs a warning
// and falls back to the DEFAULT member catalog/model/mode rather than failing the
// spawn, so a stale roster reference never wedges a team. (An unknown Task `agent`
// arg, by contrast, is a model-addressable error — the model can retry; an operator
// roster entry cannot.)
func buildMemberEngine(cfg Config, provider port.LLMProvider, teamHooks port.HookRunner, reg *agents.Registry, skillIdx skillIndex, runner tool.CommandRunner, mainMgr *mcp.Manager) server.MemberEngineFactory {
	return func(t *team.Team, spec agent.MemberSpec) agent.MemberBuild {
		cat := tool.NewCatalog()
		var (
			model = cfg.Model
			pc    = promptConfig(cfg)
			mode  session.PermissionMode
			// memberLimits carries ONLY the def-set per-round stop conditions (zero =
			// unset); AddMember per-field merges them onto the team default (s.limits).
			memberLimits session.Limits
			// mcpClose tears down any INLINE MCP managers this member connected (nil for a
			// reference-only or MCP-less member); mcpNames are the def's MCP tool names the
			// supervisor exempts from the read-only-member backstop (MCP tools report
			// ReadOnly()==false but never touch the workspace).
			mcpClose func() error
			mcpNames []string
			// memberHooks is the per-member engine HookRunner. It stays inert (the
			// historical default-member shape) UNLESS the member adopts a def whose
			// `hooks:` map scopes lifecycle hooks to its engine. teamHooks remains the
			// separate runner the coordination tools use; it is the per-def fallback so a
			// defined member with no scoped hooks behaves as before.
			memberHooks port.HookRunner = hookexec.New(nil)
		)

		def, defined := lookupMemberDef(reg, spec)
		if defined {
			// Scope the def over the member's AVAILABLE base, allowing mutating tools
			// (Edit/Write/Bash) only for a Mutating member — it runs in an isolated
			// fork, and Bash is now workspace-aware (BashTool passes the member's
			// forked Workspace.Root() to the runner as workdir), so a def MAY scope
			// Bash in for a Mutating member and it runs in the member's fork, not the
			// shared parent base. For a read-only (base-sharing) member,
			// scopedToolNamesMode drops Bash (and Edit/Write) since allowMutating is
			// false — Bash.ReadOnly()==false — so the read-only-share guarantee holds.
			base := baseTaskTools(cfg)
			names, diags := scopedToolNamesMode(def, base, spec.Mutating)
			for _, d := range diags {
				slog.Warn("team member agent def tool scoping",
					"member", spec.Name, "agent", def.Name, "tool", d.tool, "reason", d.reason, "path", def.Path)
			}
			for _, name := range names {
				cat.MustRegister(base[name])
			}
			// Per-agent MCP: add the def's referenced/inline servers' tools to THIS
			// member's catalog. The inline managers' Close rides on the MemberBuild so the
			// supervisor tears them down on member teardown; the MCP tool names are handed
			// to the supervisor so the read-only-member backstop exempts them (they report
			// ReadOnly()==false but never touch the workspace).
			mcpTools, names2, cl := defMCPTools(context.Background(), def, mainMgr)
			for _, mt := range mcpTools {
				if err := cat.Register(mt); err != nil {
					slog.Warn("team member agent def MCP tool registration failed; skipped",
						"member", spec.Name, "agent", def.Name, "tool", mt.Spec().Name, "err", err)
				}
			}
			mcpClose, mcpNames = cl, names2
			memberLimits = defLimits(def, session.Limits{}) // only def-set fields; AddMember merges with the team default
			model = resolveModel(cfg, def)                  // resolve ONCE; thread the id into agentPromptConfig
			bodies, missing := preloadedSkillBodies(def, skillIdx)
			for _, name := range missing {
				slog.Warn("team member agent def references an unknown skill; not preloaded",
					"member", spec.Name, "agent", def.Name, "skill", name, "path", def.Path)
			}
			pc = agentPromptConfig(cfg, def, model, bodies...)
			mode = resolvePermissionMode(def)
			// A def's `hooks:` scope lifecycle hooks to this member's engine. A def that
			// scopes none keeps the inert default (memberHooks unchanged), preserving the
			// historical defined-member engine shape.
			memberHooks = defHookRunner(cfg, def, memberHooks)
			slog.Info("team member adopts agent def",
				"member", spec.Name, "agent", def.Name, "tools", strings.Join(names, ","),
				"model", model, "mode", mode, "mutating", spec.Mutating,
				"preloaded_skills", len(bodies), "path", def.Path)
		} else {
			// Default member catalog: read-only base, plus Edit/Write (and Bash, when a
			// runner is configured) for a Mutating member only. Bash is now
			// workspace-aware — BashTool.Execute passes the member's forked
			// Workspace.Root() to the runner as the working directory — so a Mutating
			// member's Bash runs in its OWN isolated fork, not the shared parent base.
			// A read-only (base-sharing) member gets NO mutating tools, so the
			// read-only-share / mutating-fork isolation guarantee holds. (Bash can still
			// escape its cwd via absolute paths / `cd`, the inherent Bash trust model;
			// the fix removes the accidental shared-base mutation a parent-rooted runner
			// caused.)
			cat.MustRegister(tools.ReadTool{})
			cat.MustRegister(tools.GrepTool{})
			cat.MustRegister(tools.GlobTool{})
			if spec.Mutating {
				cat.MustRegister(tools.EditTool{})
				cat.MustRegister(tools.WriteTool{})
				if runner != nil {
					cat.MustRegister(tools.NewBashTool(runner))
				}
			}
		}

		// Team coordination tools ALWAYS, in both branches: they bypass the def
		// allowlist and are exempt from the read-only-member mutating-tool backstop.
		for _, mt := range agent.MemberTools(t, spec.Name, teamHooks) {
			cat.MustRegister(mt)
		}

		eng := newChildEngineWithHooks(provider, cat, model, pc, memberHooks)
		return agent.MemberBuild{Engine: eng, Mode: mode, Limits: memberLimits, Close: mcpClose, MCPToolNames: mcpNames}
	}
}

// lookupMemberDef resolves spec.AgentType against the registry, returning the def
// and true on a hit. An empty AgentType or a miss returns false (the caller falls
// back to the default member catalog); a miss on a NON-empty AgentType also warns,
// so a stale roster reference is observable without failing the spawn.
func lookupMemberDef(reg *agents.Registry, spec agent.MemberSpec) (agents.AgentDef, bool) {
	name := strings.TrimSpace(spec.AgentType)
	if name == "" || reg == nil {
		return agents.AgentDef{}, false
	}
	def, ok := reg.Get(name)
	if !ok {
		slog.Warn("team member references an unknown agent def; using the default member catalog",
			"member", spec.Name, "agent", name)
		return agents.AgentDef{}, false
	}
	return def, true
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
		// Team spawns coordinating subagents that may mutate the workspace (Mutating
		// members), so it ASKS — unlike the read-only Task explorer, which is allowed.
		{Scope: governance.ScopeManaged, Tool: "Team", Effect: governance.Ask},
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
