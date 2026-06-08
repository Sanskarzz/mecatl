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
	"net/http"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"time"
	"unicode/utf8"

	"github.com/stacklok/mecatl/internal/adapter/agents"
	"github.com/stacklok/mecatl/internal/adapter/dream"
	"github.com/stacklok/mecatl/internal/adapter/forker"
	"github.com/stacklok/mecatl/internal/adapter/gitenv"
	"github.com/stacklok/mecatl/internal/adapter/hookexec"
	"github.com/stacklok/mecatl/internal/adapter/mcp"
	mcpsource "github.com/stacklok/mecatl/internal/adapter/mcp/source"
	"github.com/stacklok/mecatl/internal/adapter/memory"
	"github.com/stacklok/mecatl/internal/adapter/osfs"
	"github.com/stacklok/mecatl/internal/adapter/permconfig"
	"github.com/stacklok/mecatl/internal/adapter/permpolicy"
	"github.com/stacklok/mecatl/internal/adapter/permstore"
	"github.com/stacklok/mecatl/internal/adapter/providercatalog"
	"github.com/stacklok/mecatl/internal/adapter/server"
	"github.com/stacklok/mecatl/internal/adapter/skills"
	"github.com/stacklok/mecatl/internal/adapter/store/jsonlstore"
	"github.com/stacklok/mecatl/internal/adapter/store/memstore"
	"github.com/stacklok/mecatl/internal/adapter/tokenizer"
	"github.com/stacklok/mecatl/internal/adapter/tools"
	"github.com/stacklok/mecatl/internal/adapter/xdgconfig"
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
// the fields they need. Sink and ToolCallRecorder are optional (nil installs no
// telemetry — the engine nil-guards both).
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

	// OpenRouter (multi-provider Phase 0, S1): the OpenRouter provider rides the
	// SAME stateless openai adapter (it speaks the Responses API) with the
	// OpenRouter base URL substituted. OpenRouterKey is the credential (the cmd
	// layer reads it from OPENROUTER_API_KEY); when empty the registry falls back
	// to the OPENROUTER_API_KEY / OPENAI_API_KEY env vars via its envDetector.
	// OpenRouterBaseURL overrides the default https://openrouter.ai/api/v1. These
	// are ADDITIVE — the OpenAI fields above are unchanged (S1 is non-breaking).
	OpenRouterKey     string
	OpenRouterBaseURL string

	// Anthropic (multi-provider P1): the native Anthropic Messages-API provider.
	// AnthropicKey is the credential (the cmd layer reads it from ANTHROPIC_API_KEY);
	// when empty the registry falls back to the ANTHROPIC_API_KEY env var via its
	// envDetector. AnthropicBaseURL overrides the API host for a compatible/proxy
	// endpoint. These are ADDITIVE — the OpenAI/OpenRouter fields are unchanged.
	AnthropicKey     string
	AnthropicBaseURL string

	// Context management: the compaction strategy ("heuristic"|"cascade") and the
	// token counter ("heuristic"|"tiktoken"). Empty means "heuristic".
	Compaction string
	Tokenizer  string

	// LLM resilience knobs (see internal/adapter/llmresilience).
	LLMMaxAttempts       int
	LLMPerAttemptTimeout time.Duration
	LLMStreamIdleTimeout time.Duration
	LLMBreakerThreshold  int
	LLMBreakerCooldown   time.Duration

	// Memory: per-project memory store directory (empty disables the tools), plus
	// the background consolidation (dream) interval (0 disables; only meaningful
	// with MemoryDir set).
	MemoryDir                 string
	MemoryConsolidateInterval time.Duration

	// Soul (issue #14, Phase 1): a user-scoped, agent-READ-ONLY persona fragment
	// injected as a turn-0 user message. ON by default reading the conventional
	// $XDG_CONFIG_HOME/mecatl/soul.md (fallback ~/.config/mecatl/soul.md) — a
	// missing file is fail-soft, so it costs nothing. SoulPath overrides the path
	// (--soul-file); NoSoul disables it entirely (--no-soul), in which case the
	// SoulAssembler is not wired (nil source → no-op). The adapter is read-only by
	// construction: no tool can write the soul.
	//
	// Soul DRIFT BASELINE (issue #14, Phase 3, Item 1): on load the harness records
	// the soul's content hash in a sidecar (<soulPath>.sha256) trust-on-first-use; a
	// later run whose hash differs logs a drift WARN and still loads (the persona is
	// the operator's own). ApproveSoul (--approve-soul) (re)writes the baseline to the
	// current hash, accepting an edit. SoulStrict (--soul-strict) makes a DRIFTED soul
	// contribute NO fragment. Both default false. The hash is computed in the adapter;
	// the baseline WRITE lives only in the composition layer (soulguard) — the soul
	// adapter stays write-free.
	SoulPath    string
	NoSoul      bool
	ApproveSoul bool
	SoulStrict  bool

	// User model (issue #14, Phase 2): a user-scoped, cross-PROJECT memory of
	// durable FACTS about the operator, exposed to the agent as RememberUser /
	// RecallUser / SearchUserModel tools (2a, default-on) and injected as a turn-0
	// <user-model> block (LAST, after the soul + project memory index). It is a
	// SECOND memory.Store under UserModelDir (or the conventional
	// <xdg>/mecatl/usermodel). NoUserModel disables it entirely (--no-user-model).
	//
	// UserModelReview enables the OPT-IN Phase-2b background reviewer (OFF by
	// default): after a session stops, a fresh single-shot child extracts operator
	// facts from the transcript and writes them via RememberUser. It NEVER reopens
	// the user session (R10). UserModelReviewInterval is a session-count debounce
	// (0/1 = review every session when enabled). UserModelConsolidateInterval drives
	// a separate dream.Consolidator scoped to the "user/" namespace (0 = off).
	UserModelDir                 string
	NoUserModel                  bool
	UserModelReview              bool
	UserModelReviewInterval      int
	UserModelConsolidateInterval time.Duration

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
	EnableFork bool

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

	// File-based permission config (issue #13). PermissionsConventional turns on
	// auto-discovery of the conventional per-project config (<ws>/.mecatl/settings.yaml
	// and, with ImportClaudePermissions, <ws>/.claude/settings.json) plus the
	// user-global files; it is re-resolved PER SESSION against each session's
	// workspace root. ImportClaudePermissions additionally imports Claude-Code
	// settings.json (with the lossy fail-safe table). TrustProject honours a
	// project's ALLOW rules (a project's deny/ask is always honoured regardless);
	// leave it off to ignore an untrusted repo's grants. PermissionConfigs are
	// explicit operator-pointed YAML files, loaded at the user (fully-trusted)
	// scope regardless of the conventional toggle. When none of these select any
	// source the resolver is nil and the policy behaves exactly as before
	// (built-ins + learned rules only).
	PermissionsConventional bool
	ImportClaudePermissions bool
	TrustProject            bool
	PermissionConfigs       []string

	// AllowAllTools, when set, injects a single ScopeCLI allow-all rule into the
	// MAIN engine's static ruleset alongside defaultRules(). It loosens ONLY the
	// built-in mutate-ask floor; a Deny in any scope and any CONFIGURED Ask still
	// win (see docs/design/ALLOW-ALL-POSTURE.md). Children are already allow-all.
	AllowAllTools bool

	// Observability relays, injected by the caller (mecated wires telemetry; the
	// embedded TUI server leaves both nil). The engine nil-guards each.
	Sink             port.EventSink
	ToolCallRecorder port.ToolCallRecorder

	// Diagnostics is the general-purpose operational logging seam, injected by the
	// caller (mecated wires a slogdiag sink to stderr; the embedded TUI passes its
	// own). It is the sink the build-once composition facts (token counter /
	// compaction strategy / slash-command state) are emitted through EXACTLY ONCE in
	// Build. Nil is tolerated: Build defaults it to port.NopDiagnostics so the
	// composition stays silent rather than nil-panicking.
	Diagnostics port.Diagnostics

	// gitStatus is the start-of-session git snapshot (branch + short status + recent
	// commits) rendered into the volatile <git-status> sub-block. It is computed ONCE
	// in Build (against cfg.Workspace, with the hardened/scrubbed git env, and only for
	// a TRUSTED workspace) and carried here so promptConfig/agentPromptConfig thread the
	// single precomputed value into every child/member engine — never re-running git per
	// child build or per team-member spawn on the hot path. Unexported: it is an
	// internal composition detail, not an operator knob.
	gitStatus string

	// envDetector is the injectable environment-lookup seam the provider registry
	// uses for credential-availability detection (multi-provider S1). It defaults
	// to os.Getenv (set in Build); tests inject a fake map-backed lookup so registry
	// construction runs OFFLINE. Unexported: an internal composition detail mirroring
	// xdgconfig.OSEnv's env-injection idiom, not an operator knob.
	envDetector envDetector

	// providerConstructor is the injectable seam (multi-provider Phase 0, S3 e2e)
	// for the concrete port.LLMProvider built per AVAILABLE provider id. It defaults
	// to the real (resilience-wrapped) openai-adapter constructor (set in
	// buildProviderRegistry); tests inject a fake that returns a distinct mockllm per
	// id, so the offline multi-provider e2e can build a registry with TWO real
	// provider ids backed by mocks WITHOUT a single mock short-circuit collapsing
	// them. It does NOT replace credential detection — a provider is still AVAILABLE
	// iff a (fake) key resolves via envDetector; this seam only swaps WHAT the
	// available entry's provider is. Unexported: a composition-only test seam
	// mirroring envDetector, not an operator knob. Production path unchanged.
	providerConstructor providerConstructor

	// liveModelHTTPClient is the composition-only test seam for the LIVE model
	// listers' HTTP transport (mirroring envDetector/providerConstructor). Production
	// leaves it nil — each lister then builds a default client with a sane timeout.
	// Tests inject a mock transport (a RoundTripper) so the live-listing path runs
	// OFFLINE and never contacts the real provider endpoint. Unexported: an internal
	// composition detail, not an operator knob.
	liveModelHTTPClient *http.Client

	// liveModelRefreshSync makes the live-model refresh run SYNCHRONOUSLY inside
	// Build (before it returns) instead of in a background goroutine. It is a
	// composition-only test seam so an offline e2e can assert the post-refresh
	// snapshot deterministically without sleeps/polling (the repo's anti-flake rule).
	// Production leaves it false — the refresh is fully background, so Build never
	// blocks on the network. Unexported: not an operator knob.
	liveModelRefreshSync bool
}

// providerConstructor builds the port.LLMProvider for an available provider id,
// given its resolved key and base URL. The production implementation
// (newOpenAIEntry's body) constructs the resilience-wrapped openai adapter; the
// S3 e2e injects a mock-returning fake. It NEVER receives the key on any wire — it
// is a pure in-process construction seam.
type providerConstructor func(cfg Config, id, key, baseURL string) port.LLMProvider

// osGetenv is the production environment lookup the provider registry uses when no
// envDetector is injected. It is the single place internal/app reads the process
// environment for provider credentials; tests override it via Config.envDetector.
func osGetenv(name string) string { return os.Getenv(name) }

// diag returns c.Diagnostics, or port.NopDiagnostics when it is nil. Build
// defaults c.Diagnostics to a non-nil sink for the whole production path, but the
// composition helpers are also exercised DIRECTLY by unit tests that construct a
// bare Config (no Diagnostics). Routing every helper's Log call through cfg.diag()
// makes those direct-call sites nil-safe by construction without forcing every test
// Config to set the field — and never silently nil-panics on a forgotten sink.
//
// DISCIPLINE (keeps the nil-safety invariant from regressing): every composition
// helper MUST log via cfg.diag(), NEVER cfg.Diagnostics directly — the field is nil
// on the direct-call test path, so a bare cfg.Diagnostics.Log would nil-panic there.
// Passing cfg.diag() — not cfg.Diagnostics — into a callee's Diagnostics argument is
// the same rule; the one exception is a callee that itself defaults nil→Nop (e.g.
// the adapter constructors), where forwarding cfg.Diagnostics is harmless.
func (c Config) diag() port.Diagnostics {
	if c.Diagnostics == nil {
		return port.NopDiagnostics{}
	}
	return c.Diagnostics
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
	// Workspace trust (MUST-FIX 2): fold the --trust-project flag and the
	// declarative settings.yaml `trustedWorkspaces:` list into ONE decision,
	// produced once here, then collapse it back onto cfg.TrustProject — the SAME
	// bool both downstream admission consumers already read (permconfig's
	// Options.TrustProject and the soul provenance gate). This keeps the fold a
	// composition concern with zero adapter signature churn: buildEngine and the
	// soul build see only the effective trust bool. Trust is monotonic-positive —
	// it grants admission only and never suppresses a Deny/Ask (deny-dominance is
	// unchanged in the evaluator).
	// Default the provider registry's environment-lookup seam to the real process
	// environment (tests inject a fake before calling Build). Set once here so every
	// downstream registry construction shares it.
	if cfg.envDetector == nil {
		cfg.envDetector = osGetenv
	}
	// Default the general-purpose diagnostics sink so the build-once composition
	// facts (and every other Diagnostics consumer) are nil-safe: an injected nil
	// means "stay silent", not "panic". Set once here so every downstream
	// engineDepsForProvider closes over the same sink.
	if cfg.Diagnostics == nil {
		cfg.Diagnostics = port.NopDiagnostics{}
	}

	trust := resolveTrust(cfg)
	narrateTrust(cfg.diag(), trust, cfg.Workspace)
	cfg.TrustProject = trust.Trusted

	// Start-of-session git snapshot, computed ONCE here (FIX 2): gitSnapshot runs git
	// against cfg.Workspace through a HARDENED/scrubbed env and only for a TRUSTED
	// workspace (FIX 1). The single value is carried on cfg.gitStatus so every
	// child/member promptConfig threads it in rather than re-running git per build or
	// per team-member spawn. Computed AFTER the trust fold so the gate sees effective
	// trust (declared/remembered/flag all collapse onto cfg.TrustProject above).
	cfg.gitStatus = gitSnapshot(cfg.Workspace, cfg.Shell, cfg.TrustProject)

	reg, provider, err := buildProvider(cfg)
	if err != nil {
		return nil, err
	}
	// Per-provider default model (multi-provider): when the operator passed no
	// explicit --model (cfg.Model == ""), adopt the registry's resolved per-provider
	// default (e.g. openai => "gpt-5", openrouter => "openai/gpt-5") so EVERY
	// downstream consumer below — buildEngine, buildCompactor, buildTokenCounter,
	// modelSnapshot, and DefaultCapabilities — uses the provider-appropriate model
	// rather than one valid only for OpenAI. cfg is a local value here, so this single
	// assignment propagates to all of them. An explicit --model is untouched
	// (resolveDefaultModel returns it verbatim, so reg.DefaultModel() == cfg.Model).
	if cfg.Model == "" {
		cfg.Model = reg.DefaultModel()
	}
	// Emit the build-once composition facts (token counter / compaction strategy /
	// slash commands) EXACTLY ONCE here, through the injected Diagnostics — keyed to
	// the resolved MAIN model. The per-derivation builders no longer log these (they
	// run per session AND per child engine); relocating the emit here makes operators
	// see each fact once instead of N times. Other slog sites in this file are not
	// yet relocated (iteration 2).
	logBuildConfigFacts(cfg)

	store, err := buildStore(cfg)
	if err != nil {
		return nil, err
	}
	engine, mainMgr, mcpProvider, mcpInventory, sessFactory, learned, discoveredSkills, userModelStore, mcpClose, err := buildEngine(ctx, cfg, reg, provider, store)
	if err != nil {
		return nil, err
	}
	logMCPInventory(ctx, cfg.diag(), mcpInventory)

	// The command lister backs ListCommands (the TUI palette). It reuses the SAME
	// expander build the engine consumes, so the palette offers exactly the
	// commands a "/<cmd>" prompt would expand. nil when commands are disabled.
	commandLister := buildCommandLister(cfg, mcpProvider)

	svcCfg := server.Config{
		Engine:        engine,
		Store:         store,
		Workspaces:    osfsWorkspaceFactory(cfg.diag()),
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
		// ListModels snapshot: join the provider registry's AVAILABLE providers to the
		// embedded catalog and project each model into the proto form. The registry and
		// catalog are both fixed for the process lifetime, so this is a startup snapshot
		// (like Agents/Skills), not a live lister. Empty when zero providers are
		// available (the zero-keys / mock case). Secret-free (modelSnapshot projects no
		// key/env/base-URL); the projection lives in modelsnapshot.go so the server
		// adapter never imports providercatalog or the registry.
		Models: modelSnapshot(reg),
		// DefaultCapabilities: the catalog ∩ adapter INTERSECTION for the DEFAULT
		// provider + cfg.Model, computed ONCE here in composition (the single source).
		// It backs BOTH the shared-engine session_capabilities echo (when a session
		// uses no per-session engine) AND Service.ProviderCapabilities() (the ACP gate),
		// so the wire echo, the server-wide caps, and the ACP gate cannot disagree. A
		// neutral port.ProviderCapabilities — the registry/catalog never reach the
		// server adapter. (multi-provider Phase 0, S5.)
		//
		// NOTE: this runs in Build, BEFORE the background live Swap, so its modality
		// input is the CATALOG SEED, not the live feed — fine for the default/ACP path,
		// which has no per-session selector in P0. A per-session SELECTOR session (see
		// the modelCapability call below, evaluated post-Swap) DOES get the live value.
		DefaultCapabilities: modelCapability(reg, reg.Default(), cfg.Model),
		// ListSkills snapshot: the skills discovered once at build time (registerSkills),
		// projected into the proto form. Skills are immutable for the process lifetime,
		// so this is a startup snapshot (like Agents), not a live lister. nil/empty when
		// skills are disabled.
		Skills: skillSnapshot(discoveredSkills),
		// GetSoul snapshot: re-run the same selection policy (selectSoulSource) once
		// here and project the WINNING soul's content + meta into the proto form. The
		// soul is selected deterministically at build time (USER-wins precedence, trust
		// gate, drift check), so this re-read of one tiny capped file is idempotent and
		// keeps the snapshot a pure read at request time — exactly the Agents idiom. nil
		// when no soul source is wired (capabilities().Soul then false).
		Soul: soulSnapshot(cfg),
		// GetUserModel live lister: wrap the SAME user-model store the engine writes to
		// (threaded out of buildEngine — never a second store on the same dir, which
		// would violate the one-Store-per-dir lock invariant) so a fetch reflects the
		// CURRENT entries. nil when user model is disabled (capabilities().UserModel
		// then false).
		UserModel: userModelLister(userModelStore),
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
	applyTeamConfig(&svcCfg, cfg, reg, provider, mainMgr)

	svc, err := server.NewService(svcCfg)
	if err != nil {
		mcpClose()
		return nil, fmt.Errorf("build service: %w", err)
	}

	// LIVE model listing: Build seeded svcCfg.Models with the EMBEDDED snapshot
	// synchronously above (so the ModelSelection cap is honest from t=0 and Build
	// NEVER touches the network). Now kick a SINGLE background refresh that fetches
	// each available provider's live catalog (only providers WITH a lister actually
	// fetch — openrouter today) and atomically SWAPS the merged result into the
	// service via SetModels. The refresh is owned by COMPOSITION (it holds the
	// registry + listers); the service just stores the projected proto slice. It is
	// cancelled by Close so a shutdown mid-fetch does not leak the goroutine (the
	// goleak suite catches a leak). startLiveModelRefresh is a no-op when no provider
	// has a lister (e.g. mock/openai-only), so the goroutine + ctx are skipped.
	refreshClose := startLiveModelRefresh(cfg.diag(), reg, svc, cfg.liveModelRefreshSync)

	// Close tears down the main MCP manager AND any per-session client-MCP engines
	// still registered (svc.Close), so a process exit leaks neither. It also cancels
	// the live-model refresh goroutine.
	closeAll := func() {
		refreshClose()
		svc.Close()
		mcpClose()
	}
	return &Built{Service: svc, Close: closeAll}, nil
}

// sessionEngineFactory returns the server.SessionEngineFactory that builds a
// PER-SESSION engine over an optional non-default provider/model SELECTOR
// (multi-provider Phase 0, S3) AND/OR the client-provided streaming-HTTP MCP
// servers (the ACP session/new mcpServers). The two inputs are orthogonal: a
// session with BOTH a non-default model and client MCP gets ONE engine over ONE
// catalog from a single call. Each call connects a SCOPED mcp.NewManager for that
// one session when specs are present (best-effort: a down server is logged-and-
// skipped, never fatal), and assembles a fresh catalog in this order: the CORE
// tools (registerCoreTools — the same core toolset the main engine gets), then the
// SERVER-GLOBAL MCP tools (globalMgr.Tools() — cfg.MCPServers + ToolHive, the same
// tools buildCatalog→registerMCP mounts on the main engine, reused from Build's
// already-connected shared manager — NOT reconnected, and NOT in the per-session
// closeFn), then the client MCP tools, then a per-session Task/Team. It builds an
// engine whose every NON-provider collaborator MATCHES the main engine via
// engineDepsForProvider (so a per-session engine compacts, expands commands,
// persists, and emits telemetry exactly like the shared one — only the catalog and
// the resolved provider/model differ). globalMgr is also the `reference:`-resolution
// mainMgr for per-session Task/Team subagent defs (falling back to the client mgr
// when there is no global manager), parity with the build-time path.
//
// PROVIDER/MODEL RESOLUTION (the §0.2 resolution table): the zero selector keeps
// the DEFAULT provider + cfg.Model (the pre-S3 MCP path, byte-identical). A
// non-empty sel.ProviderID is looked up in the registry — a miss (unknown id, or
// an available-only registry that omits an unkeyed provider) is a loud error
// wrapping server.ErrInvalidArgument, NEVER a silent fallback. sel.ModelID is
// handed VERBATIM to engineDepsForProvider (empty => provider/adapter default; an
// id the catalog doesn't know flows through to the provider unchanged — the
// catalog never gates the model string). engineDepsForProvider re-derives EVERY
// provider-closing Deps field (LLM/Compactor/Model/TokenCounter/PromptConfig.Env)
// against the resolved (provider, model), so a per-session engine bound to a
// non-default provider compacts and counts through THAT provider — the
// contamination fix the S1 seam was designed for.
//
// It captures the SAME store/policy/hooks the main engine was built with (threaded
// from Build), plus the registry (so it can resolve the selector), the DEFAULT
// provider (the zero-selector fallback), and the SHARED global MCP manager
// (globalMgr — owned by Build), so the two engines cannot drift on their shared
// Deps or their MCP toolset. It returns the engine and a Close that tears down ONLY
// this session's own MCP connections (client specs + per-def inline managers) — the
// shared globalMgr is NEVER in that Close. Wired into server.Config.SessionEngine in Build, so neither the
// registry nor mcp/agent wiring leaks into the server or acp layers.
func sessionEngineFactory(
	cfg Config,
	reg *providerRegistry,
	provider port.LLMProvider,
	store port.SessionStore,
	policy port.PermissionPolicy,
	hooks port.HookRunner,
	mcpProvider mcp.Provider,
	globalMgr *mcp.Manager,
	instructions prompt.InstructionAssembler,
	agentReg *agents.Registry,
) server.SessionEngineFactory {
	return func(ctx context.Context, sel server.ProviderSelector, specs []mcp.ServerConfig) (server.SessionEngineResult, error) {
		// Resolve the provider/model selector FIRST (before any MCP connect), so an
		// unknown provider fails fast without a wasted connection. The zero selector
		// keeps the default provider + cfg.Model (pre-S3 behaviour) and window=0 (⇒ the
		// 128k default, byte-identical). resolvedProviderID is threaded so the
		// per-session capability intersection (modelCapability) keys on the right
		// provider — the zero selector uses the registry default.
		resolvedProvider, resolvedModel := provider, cfg.Model
		resolvedProviderID := reg.Default()
		contextWindow := 0
		if sel.ProviderID != "" {
			entry, ok := reg.Lookup(sel.ProviderID)
			if !ok {
				return server.SessionEngineResult{}, fmt.Errorf("%w: unknown or unavailable provider %q", server.ErrInvalidArgument, sel.ProviderID)
			}
			resolvedProvider = entry.provider
			resolvedProviderID = sel.ProviderID
			// "" => provider/adapter default; a non-empty unknown model => verbatim
			// passthrough (the catalog is NOT consulted to GATE the model string).
			resolvedModel = sel.ModelID
			// Derive the compaction window from the catalog for (provider, model) so the
			// trigger AGREES with the ListModels-advertised context_limit (Medium #2). A
			// passthrough/uncatalogued model or a zero/missing catalog limit yields 0,
			// which engineDepsForProvider falls back to the 128k default — the model
			// string still flows through verbatim regardless. LIVE-FIRST: the live
			// context window (when present) beats the catalog, falling back to the catalog
			// floor — so the live ListModels picker and the compaction trigger still agree
			// (both project from the one refreshed modelEntry list).
			contextWindow = reg.meta.contextWindowFor(sel.ProviderID, sel.ModelID)
		}
		// The per-session input capability is the catalog ∩ adapter INTERSECTION for
		// the resolved (provider, model), computed HERE in composition — the single
		// source the server echoes verbatim on session_capabilities. It is a NEUTRAL
		// port.ProviderCapabilities; neither the catalog nor the registry crosses into
		// the server adapter.
		sessionCaps := modelCapability(reg, resolvedProviderID, resolvedModel)

		onError := func(sc mcp.ServerConfig, err error) {
			cfg.diag().Log(ctx, port.LevelWarn, "client MCP server unreachable; skipping for this session",
				"server", sc.Name, "url", sc.URL, "err", err)
		}
		var mgr *mcp.Manager
		if len(specs) > 0 {
			m, err := mcp.NewManager(ctx, specs, onError, cfg.diag())
			if err != nil {
				// Best-effort: every server failed. The session still gets a usable engine
				// (core tools only) rather than failing session creation outright.
				cfg.diag().Log(ctx, port.LevelWarn, "client MCP: no servers connected for this session; mounting core tools only", "err", err)
			}
			mgr = m
		}

		cat := tool.NewCatalog()
		registerCoreTools(cfg, cat, false)
		closeFn := func() error { return nil }
		// Mount the SERVER-GLOBAL MCP tools (cfg.MCPServers + ToolHive) that the main
		// engine got via buildCatalog→registerMCP. globalMgr is the SHARED manager Build
		// owns; we reuse its already-connected Tools() — we do NOT reconnect and we MUST
		// NOT fold globalMgr.Close into closeFn (a per-session CloseSession must never
		// tear down MCP for every other session). Mounted BEFORE the client specs so a
		// client tool colliding with a global one loses: mcp.Register is FIRST-wins +
		// skip-and-continue, so the global tool stays and every OTHER (non-colliding)
		// client tool is still registered. Each mount logs ONE provenance-bearing WARN
		// naming the dropped tools (Register returns the skipped names): the global mount
		// is a within-/across-global clash (a defective server advertising a duplicate),
		// the client mount is the client↔global tier (the global tool shadows the client
		// one — the WARN an end-user reads to self-diagnose a vanished tool).
		if globalMgr != nil {
			if skipped, rerr := mcp.Register(cat, globalMgr.Tools()); rerr != nil {
				cfg.diag().Log(ctx, port.LevelWarn,
					"server-global MCP: skipped duplicate tool name(s) (a server advertised a name already registered): "+strings.Join(skipped, ", "),
					"tools", strings.Join(skipped, ", "), "err", rerr)
			}
		}
		if mgr != nil {
			if skipped, rerr := mcp.Register(cat, mgr.Tools()); rerr != nil {
				cfg.diag().Log(ctx, port.LevelWarn,
					"client MCP: tool(s) shadowed by an existing server-global tool of the same name (the global tool wins): "+strings.Join(skipped, ", "),
					"tools", strings.Join(skipped, ", "), "err", rerr)
			}
			closeFn = mgr.Close
			cfg.diag().Log(ctx, port.LevelInfo, "client MCP mounted for session",
				"servers", len(mgr.Servers()), "tools", len(mgr.Tools()))
		}

		// Half B — per-session sub-agent tools. The per-session catalog already carries
		// core + server-global MCP + client MCP (above); here we add a per-session Task
		// tool (and, under EnableTeams, an in-catalog Team tool) wired to THIS session's
		// resolved (provider, providerID, model) as the inherited parent — reusing the
		// SAME builders the build-time path uses so the two catalogs cannot drift. A
		// def's own `provider:` still overrides per-def.
		//
		// refMgr is the mainMgr for Task/member defs' MCP `reference:` resolution: prefer
		// the SHARED globalMgr (parity with the build-time path, which passes the global
		// mainMgr — so a per-session subagent's `reference: <name>` resolves against the
		// SERVER-global servers), falling back to the per-session client mgr when there is
		// no global manager. We do NOT merge the two into a synthetic manager (that would
		// entangle their lifecycles); per-def INLINE MCP entries connect independently of
		// refMgr and are unaffected.
		//
		// The Task tool's inline-MCP close is FOLDED into the returned Close so a def's
		// inline MCP managers are torn down with the session (CloseSession/Service.Close).
		// globalMgr is NEVER in that Close — Build owns its lifecycle.
		refMgr := globalMgr
		if refMgr == nil {
			refMgr = mgr
		}
		taskTool, taskClose := buildTaskTool(ctx, cfg, reg, resolvedProvider, resolvedProviderID, resolvedModel, hooks, agentReg, refMgr)
		cat.MustRegister(taskTool)
		closeFn = composeCloseErr(taskClose, closeFn)
		if cfg.EnableTeams {
			// In-catalog Team tool over a per-session member factory wired to the session
			// provider as parent. (The standalone gRPC CreateTeam RPC stays on the default
			// provider — deferred; CreateTeam carries no per-session selector today.)
			factory, fk, roFk, teamHooks := buildTeamWiring(ctx, cfg, reg, resolvedProvider, resolvedProviderID, resolvedModel, refMgr)
			cat.MustRegister(agent.NewTeamTool(
				agent.TeamMemberEngineFactory(factory),
				agent.WithTeamToolForker(fk),
				agent.WithTeamToolReadOnlyForker(roFk),
				agent.WithTeamToolHooks(teamHooks),
				agent.WithTeamToolStore(store),
			))
			// The PULL member-transcript inspect tool reads the SAME shared store.
			cat.MustRegister(agent.NewInspectMemberTool(store))
		}

		// Identical to the main engine in every NON-provider Deps field except the
		// catalog (which carries the extra client MCP + per-session sub-agent tools):
		// engineDepsForProvider is the single source of the provider-closing wiring AND
		// the shared wiring, so no collaborator is silently dropped and a non-default
		// provider never contaminates compaction/counting.
		deps := engineDepsForProvider(cfg, resolvedProvider, resolvedModel, contextWindow, store, policy, hooks, mcpProvider, instructions)
		deps.Catalog = cat
		return server.SessionEngineResult{
			Engine:       agent.NewEngine(deps),
			Capabilities: sessionCaps,
			Close:        closeFn,
		}, nil
	}
}

// catalogContextWindow returns the embedded models.dev catalog's total context
// window (limit.context) for (providerID, modelID), or 0 when the provider/model is
// not catalogued (a power-user passthrough model) or carries no/zero window. The
// caller (the per-session factory) threads it into engineDepsForProvider, where 0
// falls back to the conservative 128k default. Reading the catalog HERE keeps the
// compaction-trigger window in agreement with the ListModels-advertised
// context_limit (both projected from the same catalog), so a large-context model is
// not compacted at 128k. It NEVER gates the model string — an uncatalogued model
// still reaches the provider verbatim; only its trigger window falls back.
func catalogContextWindow(providerID, modelID string) int {
	if providerID == "" || modelID == "" {
		return 0
	}
	p, ok := providercatalog.Default().Provider(providerID)
	if !ok {
		return 0
	}
	for _, m := range p.Models() {
		if m.ID() == modelID {
			return m.ContextLimit()
		}
	}
	return 0
}

// buildProvider builds the N-provider registry (multi-provider S1) and returns the
// DEFAULT provider so the Build call site is unchanged: every downstream consumer
// (buildEngine, buildCompactor, child/fork/team/dream/reviewer engines) keeps
// receiving the single default provider exactly as before. The registry itself —
// env-based availability detection across the configured providers (openai,
// openrouter), the resilience wrapping, and the per-provider startup logging — lives
// in registry.go. Per-session multi-provider ROUTING is S3, not S1; S1 only settles
// the registry shape and the default-provider seam.
//
// A canned mock (UseMock) short-circuits to a single offline entry; the zero-keys
// case returns the named, actionable errNoProvider.
//
// S3 returns the registry ALONGSIDE the default provider (it was discarded in S1)
// so the composition can thread it into the per-session engine factory (for
// per-session provider/model routing) and into modelSnapshot (for the ListModels
// projection). The default provider is still returned so every other downstream
// consumer (buildEngine's shared engine, child/fork/team/dream/reviewer engines)
// keeps receiving the single default provider exactly as before — one construction,
// no second env probe.
func buildProvider(cfg Config) (*providerRegistry, port.LLMProvider, error) {
	reg, err := buildProviderRegistry(cfg, cfg.envDetector)
	if err != nil {
		return nil, nil, err
	}
	entry, ok := reg.Lookup(reg.Default())
	if !ok {
		// Defensive: buildProviderRegistry never returns a non-nil registry with an
		// empty/absent default (it errors on zero providers), so this is unreachable.
		return nil, nil, errNoProvider
	}
	return reg, entry.provider, nil
}

// buildStore constructs the SessionStore: a JSONL replay store under StoreDir, or
// the in-memory store when the dir is empty.
func buildStore(cfg Config) (port.SessionStore, error) {
	if cfg.StoreDir == "" {
		cfg.diag().Log(context.Background(), port.LevelInfo, "session store: in-memory")
		return memstore.New(), nil
	}
	st, err := jsonlstore.New(cfg.StoreDir)
	if err != nil {
		return nil, fmt.Errorf("open jsonl store %q: %w", cfg.StoreDir, err)
	}
	cfg.diag().Log(context.Background(), port.LevelInfo, "session store: jsonl", "dir", cfg.StoreDir)
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
func buildEngine(ctx context.Context, cfg Config, reg *providerRegistry, provider port.LLMProvider, store port.SessionStore) (*agent.Engine, *mcp.Manager, mcp.Provider, []mcpsource.SourceInfo, server.SessionEngineFactory, *permstore.Memory, []skills.Skill, *memory.Store, func(), error) {
	// SkillDraft trust boundary: when enabled, the quarantine dir must live OUTSIDE
	// the workspace root (so the model's workspace-confined Write/Edit cannot reach
	// it) and be disjoint from every active skills dir. Fatal on a misconfig.
	if err := validateSkillDraftConfig(cfg); err != nil {
		return nil, nil, nil, nil, nil, nil, nil, nil, func() {}, err
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
	// File-based permission config (issue #13): the resolver re-resolves the
	// per-project `.mecatl/settings.yaml` (and Claude-imported settings.json)
	// against each session's workspace root, gating project ALLOW rules behind
	// TrustProject and caching per root. It rides the SAME lowest-scope extra
	// channel as the learned rules. permconfig.New returns nil when no source is
	// configured, in which case NewPolicyWithResolver behaves exactly like
	// NewPolicy (built-ins + learned only).
	resolver := permconfig.New(permconfig.Options{
		Conventional:  cfg.PermissionsConventional,
		ImportClaude:  cfg.ImportClaudePermissions,
		TrustProject:  cfg.TrustProject,
		ExplicitFiles: cfg.PermissionConfigs,
		Diagnostics:   cfg.diag(),
	})
	// permconfig.New returns a TYPED-nil (*permconfig.Resolver)(nil) when no config
	// source is wired; passing that into NewPolicyWithResolver would store a non-nil
	// INTERFACE wrapping a nil pointer, so the policy's `resolver != nil` guard stays
	// true and Resolve panics on the first tool-permission evaluation. Pass a real
	// untyped nil so the documented "nil resolver behaves like NewPolicy" contract
	// holds (the no-config default — built-ins + learned only).
	var policy *permpolicy.Policy
	if resolver != nil {
		policy = permpolicy.NewPolicyWithResolver(mainRules(cfg), learned, resolver)
	} else {
		policy = permpolicy.NewPolicy(mainRules(cfg), learned)
	}
	hooks := hookexec.New(nil) // no hooks by default; map is the injection seam

	// Resolve the agent-definition registry ONCE here and share it with BOTH the
	// build-time catalog's Task/Team tools and the per-session engine factory (Half B
	// builds a per-session Task/Team tool over the SAME registry, closed over below).
	// Discovery is idempotent file scanning; resolving once avoids per-session
	// re-discovery (the registry does not vary per session).
	agentReg := resolveAgentRegistry(ctx, cfg)

	cat, mainMgr, mcpProvider, mcpInventory, memStore, userModelStore, discoveredSkills, mcpClose := buildCatalog(ctx, cfg, reg, provider, hooks, agentReg, store)

	// Soul (issue #14, Phase 1): a user-scoped, agent-READ-ONLY persona source. ON
	// by default reading the conventional ~/.config/mecatl/soul.md; --no-soul leaves
	// it nil (no fragment), --soul-file overrides the path. The adapter (*soul.Store)
	// meets the prompt-defined SoulSource port HERE, in the composition layer — the
	// one place the adapter binds the port. A missing file is fail-soft (no-op).
	soulSrc := buildSoulSource(cfg)

	// Instructions seam: RootAssembler (AGENTS.md/CLAUDE.md) always; then the soul
	// (identity, when wired), then the tier-0 MemoryIndexAssembler (saved facts, when
	// a memory store is wired) — identity BEFORE saved-facts (issue #14 ordering). The
	// adapters (*soul.Store, *memory.Store) meet their prompt-defined ports HERE, in
	// the composition layer — prompt never imports them. Both ride as turn-0 user
	// messages (after the cache breakpoint), so neither enters prompt.Build's
	// StablePrefix.
	instructions := buildInstructionAssembler(soulSrc, memStore, userModelStore)

	// Phase 2b (OPT-IN, OFF by default): when UserModelReview is set AND a user-model
	// store is wired, wrap the MAIN engine's HookRunner with a composition-layer
	// Stop-trigger decorator that, on PhaseStop, fires the background reviewer in a
	// DETACHED goroutine (debounced by UserModelReviewInterval). The reviewer spawns a
	// FRESH single-shot child scoped to ONLY the RememberUser tool over the user-model
	// store — it NEVER reopens the user's terminal session (R10). The decorator is a
	// composition-layer wrapper around port.HookRunner, NOT a domain port. The
	// per-session client-MCP engines keep the UNWRAPPED hooks: the reviewer fires once
	// per MAIN-engine Stop, not per client-MCP session stop.
	mainHooks := maybeWrapUserModelReview(cfg, hooks, store, provider, userModelStore)

	deps := baseEngineDeps(cfg, provider, store, policy, mainHooks, mcpProvider, instructions)
	deps.Catalog = cat
	sessFactory := sessionEngineFactory(cfg, reg, provider, store, policy, hooks, mcpProvider, mainMgr, instructions, agentReg)
	return agent.NewEngine(deps), mainMgr, mcpProvider, mcpInventory, sessFactory, learned, discoveredSkills, userModelStore, mcpClose, nil
}

// buildInstructionAssembler composes the turn-0 instruction assembler in order:
// always the RootAssembler (project instruction files); then the SoulAssembler
// (user-scoped persona/identity) when a soul source is wired (soulSrc non-nil);
// then the tier-0 MemoryIndexAssembler (saved project facts) when a project memory
// store is wired (memStore non-nil); then the UserModelAssembler (durable FACTS
// about the operator) when a user-model store is wired (userModelStore non-nil).
// Order is identity → saved project facts → operator model (issue #14 ordering).
// Each nil collaborator is OMITTED entirely, so a soul/memory/user-model-disabled
// deployment adds no machinery. The sources satisfy prompt.SoulSource /
// prompt.MemoryIndexSource / prompt.UserModelSource structurally; this is the one
// place those adapters meet their ports. When nothing but the root is wired, the
// bare RootAssembler is returned (no Multi).
func buildInstructionAssembler(soulSrc prompt.SoulSource, memStore, userModelStore *memory.Store) prompt.InstructionAssembler {
	if soulSrc == nil && memStore == nil && userModelStore == nil {
		return prompt.RootAssembler{}
	}
	assemblers := []prompt.InstructionAssembler{prompt.RootAssembler{}}
	if soulSrc != nil {
		assemblers = append(assemblers, prompt.SoulAssembler{Src: soulSrc})
	}
	if memStore != nil {
		assemblers = append(assemblers, prompt.MemoryIndexAssembler{Src: memStore})
	}
	if userModelStore != nil {
		// LAST in the seam (issue #14 ordering): soul (identity) → memory index
		// (saved project facts) → user model (who the operator is).
		assemblers = append(assemblers, prompt.UserModelAssembler{Src: userModelStore})
	}
	return prompt.NewMultiAssembler(assemblers...)
}

// buildSoulSource constructs the user-scoped, agent-read-only soul source (issue
// #14, Phase 1). It returns an untyped nil prompt.SoulSource when soul is disabled
// (--no-soul) so buildInstructionAssembler's nil check holds (no typed-nil
// gotcha). Otherwise it builds a *soul.Store reading SoulPath (when set) or the
// conventional ~/.config/mecatl/soul.md. A missing file is fail-soft, so leaving
// soul on costs nothing. The adapter is read-only by construction — no write path.
func buildSoulSource(cfg Config) prompt.SoulSource {
	return buildSoulSourceWith(cfg, osBaselineIO)
}

// buildSoulSourceWith is buildSoulSource with an injectable baseline-IO seam, so the
// drift-baseline read/write can be exercised offline (no real ~/.config). It
// delegates the full selection policy to selectSoulSource (soulselect.go), which
// owns: USER-wins precedence between a user-scoped and a project-sourced soul (Item
// 2), the project-soul trust gate (--trust-project, the issue-#13 mechanism), and
// the Item-1 drift check applied to WHICHEVER soul wins. It discards the soulMeta
// snapshot here (the prompt assembler only needs the source); selectSoulSource's
// second return value (the provenance/trusted/drift metadata) is the read-only seam
// Item 3's TUI inspector will consume — there is no proto/RPC/TUI for it yet.
//
// NOTE: the selected soul file is read TWICE per build — once here for the startup
// hash + selection, then again by the assembler's Load at run time (which
// re-validates the body). That is an accepted cost: it is one small file (capped at
// 20 KiB), read at most twice, and keeping the hash/selection out of the assembler
// keeps drift + provenance a pure composition concern (internal/prompt stays
// drift- and trust-unaware). No caching seam is warranted for two reads of a tiny file.
func buildSoulSourceWith(cfg Config, io baselineIO) prompt.SoulSource {
	src, _ := selectSoulSource(cfg, io, buildSoulGate(cfg))
	return src
}

// userModelSubdir is the conventional user-model store directory relative to the
// XDG config base, i.e. <config>/mecatl/usermodel (fallback
// ~/.config/mecatl/usermodel). Mirrors soul's soulSubpath path convention.
const userModelSubdir = "mecatl/usermodel"

// buildUserModelStore constructs the SECOND, USER-scoped memory store (issue #14,
// Phase 2) — a cross-project store of durable FACTS about the operator. It returns
// nil when user-model is disabled (--no-user-model), when no directory can be
// resolved, or when the store cannot be opened (all fail-soft: the user-model
// tools/block simply don't appear). It resolves UserModelDir when set, else the
// conventional <xdg>/mecatl/usermodel. It upholds the one-Store-per-dir invariant:
// this is the SOLE construction site for the user-model store, distinct from the
// per-project memory store (different dir), so the two never contend on a lock.
func buildUserModelStore(cfg Config) *memory.Store {
	if cfg.NoUserModel {
		cfg.diag().Log(context.Background(), port.LevelInfo, "user model DISABLED (--no-user-model)")
		return nil
	}
	dir := cfg.UserModelDir
	if dir == "" {
		base := xdgconfig.UserConfigDir(xdgconfig.OSEnv)
		if base == "" {
			cfg.diag().Log(context.Background(), port.LevelInfo, "user model DISABLED (no --user-model-dir and no XDG/home to resolve the conventional location)")
			return nil
		}
		dir = filepath.Join(base, userModelSubdir)
	}
	store, err := memory.New(dir)
	if err != nil {
		cfg.diag().Log(context.Background(), port.LevelWarn, "could not open user-model store; user-model tools disabled", "dir", dir, "err", err)
		return nil
	}
	cfg.diag().Log(context.Background(), port.LevelInfo, "user model ENABLED (cross-project operator FACTS)", "dir", dir)
	return store
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
	mcpProvider mcp.Provider,
	instructions prompt.InstructionAssembler,
) agent.Deps {
	// baseEngineDeps is the "default provider + default model" specialization of
	// engineDepsForProvider: it delegates so there is a SINGLE enumeration of the
	// provider-closing fields (LLM/Compactor/Model/TokenCounter/PromptConfig). The
	// model-keyed TokenCounter is derived INSIDE engineDepsForProvider against
	// cfg.Model, so the default path stays semantically identical to pre-S1
	// (buildTokenCounter(cfg) for the default model). It passes contextWindow=0 so the
	// default model keeps the conservative 128k window (no behaviour change) — the
	// catalog-derived per-model window applies ONLY to a non-default selector.
	return engineDepsForProvider(cfg, provider, cfg.Model, 0, store, policy, hooks, mcpProvider, instructions)
}

// engineDepsForProvider re-derives the COMPLETE set of provider-closing agent.Deps
// for a specific provider+model, so a per-session engine bound to a non-default
// provider compacts and replays reasoning through THAT provider — never the default.
// It is the multi-provider analogue of baseEngineDeps: baseEngineDeps closes over the
// single default provider; this takes the provider+model explicitly and rebuilds
// EVERY field that binds them by value. The provider/model-dependent fields are EXACTLY:
//   - Deps.LLM                    (the provider itself)
//   - Deps.Compactor              (buildCompactor binds provider+model BY VALUE)
//   - Deps.Model                  (the model string)
//   - Deps.TokenCounter           (tiktoken is model-keyed)
//   - Deps.PromptConfig.Env.Model (the agency-delta + env model are model-keyed)
//   - Deps.ContextWindowTokens    (the compaction trigger window — model-keyed; see
//     contextWindow below. This is the S1-deferred "6th field": ListModels now
//     advertises the real per-model window, so the trigger MUST agree with it or a
//     1M-context model would still compact at 128k.)
//
// Every NON-provider field (Policy/Hooks/Store/Sink/ToolCallRecorder/Instructions/
// CompactionRatio/CommandExpander) is shared and threaded in. Catalog is
// deliberately left unset — the caller sets it AFTER this returns (the per-session
// engine adds the client's MCP tools), matching baseEngineDeps' contract.
//
// contextWindow is the model's total context window in tokens. The CALLER resolves
// it (the per-session factory looks it up in the catalog for the selected
// provider+model); a value <= 0 means "unknown / not catalogued / default provider"
// and falls back to defaultContextWindowTokens (128k). The DEFAULT path
// (baseEngineDeps) passes 0 so it stays byte-identical to pre-S1 (128k) — a behaviour
// change to the default model is deliberately avoided; only an explicit non-default
// selector whose catalog entry carries a known window overrides it.
//
// CRITICAL (design): a shallow clone of baseEngineDeps with only LLM swapped would
// compact and COUNT through the wrong provider/model, because buildCompactor and
// buildTokenCounter BOTH bind model by value — cross-provider reasoning
// contamination. This explicit re-derivation is the fix. DESIGNED in S1, CONSUMED in
// S3 (per-session routing); S1 exercises only the default-provider path via
// baseEngineDeps, which MUST stay byte-identical to the pre-S1 Deps.
//
// The model-keyed TokenCounter is DERIVED INTERNALLY (buildTokenCounter against the
// model-overridden cfg), NOT taken as a parameter: this makes counter/model
// contamination impossible BY CONSTRUCTION — an S3 caller cannot thread in a counter
// built for a different model — and guarantees the Compactor and the compaction
// trigger share ONE counter keyed to THIS model. (Panel finding #1.)
func engineDepsForProvider(
	cfg Config,
	provider port.LLMProvider,
	model string,
	contextWindow int,
	store port.SessionStore,
	policy port.PermissionPolicy,
	hooks port.HookRunner,
	mcpProvider mcp.Provider,
	instructions prompt.InstructionAssembler,
) agent.Deps {
	// modelCfg is cfg with the provider-closing Model overridden, so promptConfig
	// (Env.Model + agencyDelta), buildTokenCounter (tiktoken vocab), and buildCompactor
	// (Model field) all close over the REQUESTED model rather than cfg.Model. Every
	// other cfg field is shared.
	modelCfg := cfg
	modelCfg.Model = model
	counter := buildTokenCounter(modelCfg)
	// Resolve the compaction window: an unknown/uncatalogued/default (<=0) window
	// falls back to the conservative 128k default, so the trigger never compacts a
	// large-context model prematurely yet the default path stays byte-identical.
	window := defaultContextWindowTokens
	if contextWindow > 0 {
		window = contextWindow
	}
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
		ToolCallRecorder:    cfg.ToolCallRecorder,
		Diagnostics:         cfg.diag(),
		PromptConfig:        promptConfig(modelCfg, cfg.gitStatus),
		Model:               model,
		ContextWindowTokens: window,
		CompactionRatio:     defaultCompactionRatio,
		TokenCounter:        counter,
		Compactor:           buildCompactor(modelCfg, provider, counter),
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
//
// It does NOT log — the build-once slash-command fact is emitted ONCE in Build
// via the injected Diagnostics (see slashCommandDecision / logBuildConfigFacts).
// It is reached per session (buildCommandExpander) and via buildCommandLister, so
// logging here would fire repeatedly.
func buildDirCommandExpander(cfg Config) prompt.CommandExpander {
	if cfg.CommandsDir == "" && !cfg.EnableCommands {
		return nil
	}
	if cfg.CommandsDir != "" {
		// An explicit --commands-dir is OPERATOR-supplied (not repo-injected), so it is
		// trusted regardless of workspace trust — no project-tier gate applies.
		return prompt.NewDirCommandExpander(cfg.CommandsDir)
	}
	// EnableCommands with no explicit dir: the package defaults are the PROJECT-tier
	// dirs (workspace-relative .mecatl/commands, .claude/commands). They are repo-
	// injected steering, so they are withheld when there IS a workspace to distrust
	// AND it is untrusted (Phase 2a). cfg.TrustProject carries the folded
	// TrustDecision (Build). With no workspace there is no project to gate (the
	// expander resolves per-session against each session's root). An untrusted repo's
	// slash commands cannot run before the operator trusts it; the agent still works
	// in "ask the human" mode (raw text passes through the NoopExpander).
	if cfg.Workspace != "" && !cfg.TrustProject {
		return nil
	}
	return prompt.NewDirCommandExpander()
}

// slashCommandDecision mirrors buildDirCommandExpander's branch logic to produce
// the human-readable slash-command fact (same messages and key-values as before),
// so Build can log it ONCE rather than the builder logging it per derivation. It
// depends only on cfg, matching the builder's branches exactly.
func slashCommandDecision(cfg Config) diagFact {
	if cfg.CommandsDir == "" && !cfg.EnableCommands {
		return diagFact{level: port.LevelInfo, msg: "slash commands DISABLED (set a commands dir or enable commands to enable)"}
	}
	if cfg.CommandsDir != "" {
		return diagFact{level: port.LevelInfo, msg: "slash commands ENABLED", args: []any{"dir", cfg.CommandsDir}}
	}
	if cfg.Workspace != "" && !cfg.TrustProject {
		return diagFact{
			level: port.LevelWarn,
			msg:   "slash commands: project-tier command dirs WITHHELD (untrusted workspace); raw text passes through. Trust this repo (--trust-project or trustedWorkspaces) or pass --commands-dir to enable project slash commands",
			args:  []any{"dirs", ".mecatl/commands,.claude/commands"},
		}
	}
	return diagFact{level: port.LevelInfo, msg: "slash commands ENABLED (default dirs)", args: []any{"dirs", ".mecatl/commands,.claude/commands"}}
}

// buildMCPPromptExpander returns the MCP prompt expander, or nil when MCP prompts
// are disabled or no connected server exposes a prompt.
func buildMCPPromptExpander(cfg Config, p mcp.Provider) prompt.CommandExpander {
	if !cfg.MCPPrompts || p == nil {
		if !cfg.MCPPrompts {
			cfg.diag().Log(context.Background(), port.LevelInfo, "MCP prompts DISABLED")
		}
		return nil
	}
	prompts, err := p.ListPrompts(context.Background(), "")
	if err != nil {
		cfg.diag().Log(context.Background(), port.LevelWarn, "MCP prompt listing failed; prompt expansion disabled", "err", err)
		return nil
	}
	if len(prompts) == 0 {
		cfg.diag().Log(context.Background(), port.LevelInfo, "MCP prompts DISABLED (no connected server exposes a prompt)")
		return nil
	}
	cfg.diag().Log(context.Background(), port.LevelInfo, "MCP prompt expansion ENABLED", "count", len(prompts))
	return mcp.NewPromptExpander(p)
}

// diagFact is one build-once composition decision rendered for the operator: a
// severity level, a human-readable message, and slog-style alternating key/value
// args. The three build-once fact families (token counter, compaction strategy,
// slash commands) each produce one, and logBuildConfigFacts emits them ONCE at
// composition through the injected Diagnostics — instead of the per-derivation
// builders logging them N times (once per session AND per child engine).
type diagFact struct {
	level port.Level
	msg   string
	args  []any
}

// logBuildConfigFacts emits the build-once composition facts EXACTLY ONCE through
// cfg.diag(). It is called a single time from Build (after cfg.Model is
// resolved), NOT from engineDepsForProvider — which is re-invoked per session and
// per child engine. The facts are keyed to the MAIN engine's model (cfg.Model);
// the build-once FACTS are emitted only here, once. (Child engines DO emit live
// per-run diagnostics — correlated by session+agent role — but not these
// build-once composition facts, which are a one-shot main-engine concern.)
//
// The token-counter fact is captured from an actual build attempt
// (buildTokenCounterWithDecision) so the tiktoken-unavailable fallback warning is
// faithful; the other two are derived purely from cfg.
func logBuildConfigFacts(cfg Config) {
	_, tokenFact := buildTokenCounterWithDecision(cfg)
	facts := []diagFact{
		tokenFact,
		compactionDecision(cfg),
		slashCommandDecision(cfg),
	}
	for _, f := range facts {
		cfg.diag().Log(context.Background(), f.level, f.msg, f.args...)
	}
}

// buildTokenCounter selects the TokenCounter from cfg.Tokenizer. The default
// ("heuristic"/empty) returns the dependency-free heuristic counter. "tiktoken"
// returns the offline tiktoken-backed counter for the configured model; if it
// cannot be built it falls back to the heuristic so startup never fails.
//
// It does NOT log — the build-once composition fact is emitted ONCE in Build via
// the injected Diagnostics (see logBuildConfigFacts). This builder is re-invoked
// per session AND per child engine, so logging here would fire N times.
func buildTokenCounter(cfg Config) agent.TokenCounter {
	counter, _ := buildTokenCounterWithDecision(cfg)
	return counter
}

// buildTokenCounterWithDecision is buildTokenCounter plus the human-readable
// decision (level/msg/kv) describing the selection, so Build can log it ONCE.
// The fallback case is only knowable by actually attempting tokenizer
// construction, so the decision is captured here rather than re-derived from cfg.
func buildTokenCounterWithDecision(cfg Config) (agent.TokenCounter, diagFact) {
	switch cfg.Tokenizer {
	case "tiktoken":
		tc, err := tokenizer.NewForModel(cfg.Model)
		if err != nil {
			return agent.HeuristicTokenCounter{}, diagFact{
				level: port.LevelWarn,
				msg:   "tiktoken counter unavailable; falling back to heuristic",
				args:  []any{"model", cfg.Model, "err", err},
			}
		}
		return tc, diagFact{
			level: port.LevelInfo,
			msg:   "token counter: tiktoken (offline vocab)",
			args:  []any{"model", cfg.Model},
		}
	default:
		return agent.HeuristicTokenCounter{}, diagFact{
			level: port.LevelInfo,
			msg:   "token counter: heuristic (dependency-free)",
		}
	}
}

// buildCompactor selects the Compactor from cfg.Compaction. The default
// ("heuristic"/empty) returns the single-summary HeuristicCompactor. "cascade"
// returns the tiered CascadeCompactor reducing toward defaultCompactionTargetRatio
// (below the trigger ratio, for hysteresis).
//
// It does NOT log — the build-once composition fact is emitted ONCE in Build via
// the injected Diagnostics (see logBuildConfigFacts); this builder runs per
// session AND per child engine.
func buildCompactor(cfg Config, provider port.LLMProvider, counter agent.TokenCounter) agent.Compactor {
	switch cfg.Compaction {
	case "cascade":
		return agent.CascadeCompactor{
			Counter:      counter,
			BudgetTokens: int(float64(defaultContextWindowTokens) * defaultCompactionTargetRatio),
			LLM:          provider,
			Model:        cfg.Model,
		}
	default:
		return agent.HeuristicCompactor{}
	}
}

// compactionDecision describes the compaction-strategy selection from cfg, so
// Build can log it ONCE. It mirrors buildCompactor's switch but takes no provider
// (the human-readable fact depends only on cfg.Compaction).
func compactionDecision(cfg Config) diagFact {
	switch cfg.Compaction {
	case "cascade":
		return diagFact{level: port.LevelInfo, msg: "compaction strategy: cascade (snip→strip→collapse→summarize)"}
	default:
		return diagFact{level: port.LevelInfo, msg: "compaction strategy: heuristic (single-summary)"}
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
			cfg.diag().Log(context.Background(), port.LevelInfo, "Bash tool ENABLED", "shell", cfg.Shell, "cwd", cfg.Workspace)
		}
	} else if log {
		cfg.diag().Log(context.Background(), port.LevelInfo, "Bash tool DISABLED (shell-less mode): the agent has no command execution",
			"reason", bashDisabledReason(cfg))
	}
}

// buildCatalog registers the always-available core tools (Read, Edit, Write, Grep,
// Glob, WebFetch), a read-only Task subagent, and — only when a shell is configured
// — the optional Bash tool. It then optionally registers Fork, memory, skills, the
// repo map, and connects any MCP servers. The returned close func tears down the
// MCP manager on shutdown.
func buildCatalog(ctx context.Context, cfg Config, reg *providerRegistry, provider port.LLMProvider, hooks port.HookRunner, agentReg *agents.Registry, store port.SessionStore) (*tool.Catalog, *mcp.Manager, mcp.Provider, []mcpsource.SourceInfo, *memory.Store, *memory.Store, []skills.Skill, func()) {
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

	// Build-time Task tool: the inherited parent is the build-time DEFAULT provider +
	// model (reg.Default()/cfg.Model), so a def that pins no provider runs on the
	// default exactly as before. A def's own `provider:` overrides per-def. agentReg
	// is resolved ONCE in buildEngine and shared with the per-session factory (Half B).
	taskTool, taskMCPClose := buildTaskTool(ctx, cfg, reg, provider, reg.Default(), cfg.Model, hooks, agentReg, mainMgr)
	cat.MustRegister(taskTool)
	// Aggregate the per-def INLINE MCP managers' teardown into the main MCP close, so
	// Built.Close tears them ALL down on shutdown (process-lifetime engines).
	mcpClose = composeClose(cfg.diag(), taskMCPClose, mcpClose)

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
		cfg.diag().Log(ctx, port.LevelInfo, "Fork tool ENABLED (parallel isolated MUTATING child branches; judge selection wired)",
			"preserved_fork_cap", preservedCap)
	} else {
		cfg.diag().Log(ctx, port.LevelInfo, "Fork tool DISABLED")
	}

	// Team tool: forms a team of coordinating subagents in-process, driving a
	// Supervisor over the SAME member-engine wiring the gRPC CreateTeam path uses
	// (buildTeamWiring is the single source of that wiring truth, so the two paths
	// cannot drift). Registered ONLY when teams are enabled. It is mutate-serial
	// (unlike the read-parallel Task/Fork) and defaults to ASK (see defaultRules).
	if cfg.EnableTeams {
		// buildTeamWiring always returns a non-nil forker and hooks runner under
		// EnableTeams, so they are wired unconditionally (no nil guards).
		factory, fk, roFk, teamHooks := buildTeamWiring(ctx, cfg, reg, provider, reg.Default(), cfg.Model, mainMgr)
		// factory is a server.MemberEngineFactory; NewTeamTool wants the
		// agent.TeamMemberEngineFactory of identical underlying shape — an explicit
		// conversion bridges the two named types (both func(*team.Team, MemberSpec)
		// MemberBuild), so a single wiring serves both team paths.
		cat.MustRegister(agent.NewTeamTool(
			agent.TeamMemberEngineFactory(factory),
			agent.WithTeamToolForker(fk),
			agent.WithTeamToolReadOnlyForker(roFk),
			agent.WithTeamToolHooks(teamHooks),
			agent.WithTeamToolStore(store),
		))
		// The PULL member-transcript inspect tool: read-only, reads the SAME session
		// store the Team tool persists members to (collision-free MemberSessionID ids).
		// Gated on EnableTeams (no teams → no transcripts to inspect).
		cat.MustRegister(agent.NewInspectMemberTool(store))
		cfg.diag().Log(ctx, port.LevelInfo, "Team tool ENABLED (in-process coordinating subagents; mutate-serial, ASK)")
	} else {
		cfg.diag().Log(ctx, port.LevelInfo, "Team tool DISABLED")
	}

	// Memory tools: opt-in, registered only when a per-project memory directory is
	// configured via MemoryDir.
	if cfg.MemoryDir != "" {
		store, err := memory.New(cfg.MemoryDir)
		if err != nil {
			cfg.diag().Log(ctx, port.LevelWarn, "could not open memory store; memory tools disabled", "dir", cfg.MemoryDir, "err", err)
		} else if err := memory.Register(cat, store); err != nil {
			cfg.diag().Log(ctx, port.LevelWarn, "registering memory tools failed; some tools may be missing", "err", err)
		} else {
			memStore = store
			cfg.diag().Log(ctx, port.LevelInfo, "memory tools ENABLED (Remember/Recall/SearchMemory); permission: allow (built-in default, overridable to ask/deny via settings)", "dir", cfg.MemoryDir)
			startMemoryConsolidation(ctx, cfg, store, provider)
		}
	} else {
		cfg.diag().Log(ctx, port.LevelInfo, "memory tools DISABLED (memory dir empty)")
		if cfg.MemoryConsolidateInterval > 0 {
			cfg.diag().Log(ctx, port.LevelWarn, "memory consolidation interval is a no-op without a memory dir (memory is disabled)",
				"interval", cfg.MemoryConsolidateInterval)
		}
	}

	// User-model tools (issue #14, Phase 2a): a SECOND, USER-scoped memory store
	// (cross-project), exposed as RememberUser/RecallUser/SearchUserModel. ON by
	// default reading the conventional <xdg>/mecatl/usermodel; --no-user-model
	// disables it. userModelStore is returned so the caller can bind it to the
	// prompt <user-model> source AND (for Phase 2b) to the background reviewer's
	// write tool. It stays nil when disabled or unopenable (fail-soft).
	userModelStore := buildUserModelStore(cfg)
	if userModelStore != nil {
		if err := memory.RegisterUserModel(cat, userModelStore); err != nil {
			cfg.diag().Log(ctx, port.LevelWarn, "registering user-model tools failed; some tools may be missing", "err", err)
		} else {
			cfg.diag().Log(ctx, port.LevelInfo, "user-model tools ENABLED (RememberUser/RecallUser/SearchUserModel; cross-project); permission: allow (built-in default, overridable to ask/deny via settings)")
			startUserModelConsolidation(ctx, cfg, userModelStore, provider)
		}
	}

	discoveredSkills := registerSkills(ctx, cfg, cat)

	return cat, mainMgr, mcpProvider, mcpInventory, memStore, userModelStore, discoveredSkills, mcpClose
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
		cfg.diag().Log(ctx, port.LevelWarn, "MCP server skipped", "name", s.Server, "reason", s.Reason)
	}
	if len(configs) == 0 {
		cfg.diag().Log(ctx, port.LevelInfo, "MCP DISABLED (no servers resolved from any source)",
			"toolhive", cfg.ToolHiveEnabled, "static", len(cfg.MCPServers))
		return nil, nil, inventory, func() {}
	}

	onError := func(sc mcp.ServerConfig, err error) {
		cfg.diag().Log(ctx, port.LevelWarn, "MCP server unreachable; skipping", "name", sc.Name, "url", sc.URL, "err", err)
	}
	mgr, err := mcp.NewManager(ctx, configs, onError, cfg.diag())
	if err != nil {
		cfg.diag().Log(ctx, port.LevelWarn, "MCP manager construction failed; continuing without MCP tools", "err", err)
		return nil, nil, inventory, func() {}
	}
	if skipped, err := mcp.Register(cat, mgr.Tools()); err != nil {
		// Within-/across-global clash at build time: a defective server advertised a
		// name another already-registered server (or the same server) exposes. The
		// namespaced names (mcp__<server>__<tool>) carry the server provenance.
		cfg.diag().Log(ctx, port.LevelWarn,
			"server-global MCP: skipped duplicate tool name(s) (a server advertised a name already registered): "+strings.Join(skipped, ", "),
			"tools", strings.Join(skipped, ", "), "err", err)
	}
	cfg.diag().Log(ctx, port.LevelInfo, "MCP tools registered", "servers", len(configs), "tools", len(mgr.Tools()))

	if cfg.MCPResourceTools {
		registered, rerr := mcp.RegisterResourceTools(cat, mgr)
		switch {
		case rerr != nil:
			cfg.diag().Log(ctx, port.LevelWarn, "registering MCP resource tools failed", "err", rerr)
		case registered:
			cfg.diag().Log(ctx, port.LevelInfo, "MCP resource tools ENABLED (ListMcpResources/ReadMcpResource)")
		default:
			cfg.diag().Log(ctx, port.LevelInfo, "MCP resource tools DISABLED (no connected server exposes a resource)")
		}
	} else {
		cfg.diag().Log(ctx, port.LevelInfo, "MCP resource tools DISABLED")
	}

	return mgr, mgr, inventory, func() {
		if err := mgr.Close(); err != nil {
			cfg.diag().Log(ctx, port.LevelWarn, "MCP manager close", "err", err)
		}
	}
}

// logMCPInventory logs a one-line-per-source summary of the resolved MCP source
// inventory, keeping the resolution observable.
func logMCPInventory(ctx context.Context, d port.Diagnostics, inventory []mcpsource.SourceInfo) {
	for _, src := range inventory {
		d.Log(ctx, port.LevelInfo, "MCP source resolved",
			"source", src.Name, "kind", src.Kind, "group", src.Group,
			"servers", len(src.Servers), "diagnostics", len(src.Diagnostics))
	}
}

// skillResolveOptions is the SINGLE source of truth for how this package resolves
// skills sources from cfg. Every skills consumer (registerSkills, resolveSkillIndex,
// activeSkillDirs) MUST build its skills.ResolveOptions through here so the
// Workspace-Trust Phase-2a project-tier gate (IncludeProjectTier: cfg.TrustProject —
// cfg.TrustProject carries the folded TrustDecision from Build) is applied by
// CONSTRUCTION. A future consumer that calls this helper inherits the gate
// automatically; a future consumer that hand-rolls a skills.ResolveOptions would
// silently reopen the project-tier injection gap — so don't. Behaviour for the three
// existing callers is identical to the prior hand-synced options.
func skillResolveOptions(cfg Config) skills.ResolveOptions {
	return skills.ResolveOptions{
		Explicit:     cfg.SkillsDirs,
		Conventional: cfg.SkillsConventional,
		Workspace:    cfg.Workspace,
		// Project-tier skills are withheld when the workspace is untrusted (Phase 2a /
		// R2.5). cfg.TrustProject already carries the folded TrustDecision (Build).
		IncludeProjectTier: cfg.TrustProject,
	}
}

// registerSkills wires the progressive-disclosure Skill tool from the resolved
// Source list (explicit dirs + conventional locations when enabled). Skills stay
// OPT-IN: with no sources, nothing is registered. The SkillDraft tool is registered
// when SkillsDraftDir is set, even with no active sources (to close the
// author→promote→active loop).
//
// It returns the discovered skills so the composition root can project them into
// the ListSkills snapshot (skillSnapshot). The slice is nil when no skills are
// discovered (disabled, no sources, or none valid).
func registerSkills(ctx context.Context, cfg Config, cat *tool.Catalog) []skills.Skill {
	sources := skills.ResolveSources(skillResolveOptions(cfg))
	if len(sources) == 0 && cfg.SkillsDraftDir != "" {
		registerSkillDraft(ctx, cfg, cat, nil)
		return nil
	}
	if len(sources) == 0 {
		cfg.diag().Log(ctx, port.LevelInfo, "skills DISABLED (no skills dirs configured)")
		return nil
	}

	discovered, skips, err := skills.RegisterSource(ctx, cat, skills.NewMultiSource(sources...))
	for _, s := range skips {
		cfg.diag().Log(ctx, port.LevelWarn, "skill skipped", "path", s.Path, "reason", s.Reason)
	}
	switch {
	case err != nil:
		cfg.diag().Log(ctx, port.LevelWarn, "registering skills failed; Skill tool disabled",
			"dirs", strings.Join(cfg.SkillsDirs, ","), "conventional", cfg.SkillsConventional, "err", err)
	case len(discovered) == 0:
		cfg.diag().Log(ctx, port.LevelInfo, "skills DISABLED (no valid SKILL.md found in any source)",
			"dirs", strings.Join(cfg.SkillsDirs, ","), "conventional", cfg.SkillsConventional)
	default:
		names := make([]string, 0, len(discovered))
		for _, s := range discovered {
			names = append(names, s.Name)
		}
		cfg.diag().Log(ctx, port.LevelInfo, "Skill tool ENABLED",
			"dirs", strings.Join(cfg.SkillsDirs, ","), "conventional", cfg.SkillsConventional,
			"count", len(discovered), "skills", strings.Join(names, ","))
	}

	registerSkillDraft(ctx, cfg, cat, discovered)
	return discovered
}

// registerSkillDraft registers the writable SkillDraft tool when SkillsDraftDir is
// set, binding a DirDrafter to the quarantine dir and the snapshot of currently
// active skills (for the offline novelty check). The structural trust boundary is
// enforced by validateSkillDraftConfig at engine-build time.
func registerSkillDraft(ctx context.Context, cfg Config, cat *tool.Catalog, existing []skills.Skill) {
	if cfg.SkillsDraftDir == "" {
		cfg.diag().Log(ctx, port.LevelInfo, "SkillDraft tool DISABLED (no skills-draft dir)")
		return
	}
	drafter := skills.NewDirDrafter(cfg.SkillsDraftDir, existing,
		skills.WithSimilarityThreshold(cfg.SkillsDraftThreshold))
	cat.MustRegister(skills.NewDraftTool(drafter))
	cfg.diag().Log(ctx, port.LevelInfo, "SkillDraft tool ENABLED (model-authored skills -> quarantine -> operator promote)",
		"quarantine", cfg.SkillsDraftDir, "similarity_threshold", cfg.SkillsDraftThreshold,
		"snapshot_skills", len(existing))
}

// startMemoryConsolidation launches the dream consolidator on a background
// goroutine when MemoryConsolidateInterval is positive. It shares ctx (so the loop
// exits on shutdown) and the same LLM provider as the agent.
func startMemoryConsolidation(ctx context.Context, cfg Config, store tool.MemoryStore, provider port.LLMProvider) {
	if cfg.MemoryConsolidateInterval <= 0 {
		cfg.diag().Log(ctx, port.LevelInfo, "memory consolidation DISABLED")
		return
	}
	cons := dream.New(store, provider, dream.Config{Model: cfg.Model})
	cfg.diag().Log(ctx, port.LevelInfo, "memory consolidation ENABLED (dream)", "interval", cfg.MemoryConsolidateInterval, "model", cfg.Model)
	go func() {
		err := cons.RunPeriodically(ctx, cfg.MemoryConsolidateInterval, func(err error) {
			cfg.diag().Log(ctx, port.LevelWarn, "memory consolidation", "err", err)
		})
		if err != nil && !errors.Is(err, context.Canceled) {
			cfg.diag().Log(ctx, port.LevelWarn, "memory consolidation loop stopped", "err", err)
		}
	}()
}

// startUserModelConsolidation launches a SEPARATE dream consolidator on the
// USER-model store, scoped to the "user/" key namespace, when
// UserModelConsolidateInterval is positive (default 0 = off). It mirrors
// startMemoryConsolidation but with dream.Config{Prefix: "user/"} so it only ever
// touches user-model entries, never project memory. It shares ctx and the agent's
// provider. It returns true when a consolidator was started (a positive interval),
// false otherwise — a small testability seam so a test can assert the OFF-by-default
// posture (interval 0 ⇒ no goroutine) without observing the background loop.
func startUserModelConsolidation(ctx context.Context, cfg Config, store tool.MemoryStore, provider port.LLMProvider) bool {
	if cfg.UserModelConsolidateInterval <= 0 {
		cfg.diag().Log(ctx, port.LevelInfo, "user-model consolidation DISABLED")
		return false
	}
	cons := dream.New(store, provider, dream.Config{Model: cfg.Model, Prefix: "user/"})
	cfg.diag().Log(ctx, port.LevelInfo, "user-model consolidation ENABLED (dream; user/ namespace)",
		"interval", cfg.UserModelConsolidateInterval, "model", cfg.Model)
	go func() {
		err := cons.RunPeriodically(ctx, cfg.UserModelConsolidateInterval, func(err error) {
			cfg.diag().Log(ctx, port.LevelWarn, "user-model consolidation", "err", err)
		})
		if err != nil && !errors.Is(err, context.Canceled) {
			cfg.diag().Log(ctx, port.LevelWarn, "user-model consolidation loop stopped", "err", err)
		}
	}()
	return true
}

// maybeWrapUserModelReview returns hooks wrapped with the Phase-2b Stop-trigger
// decorator when UserModelReview is enabled AND a user-model store is wired;
// otherwise it returns hooks unchanged (the feature is OFF by default, so the
// common path is a passthrough). The decorator builds a child engine scoped to
// ONLY the RememberUser tool over the user-model store, constructs an
// agent.UserModelReviewer, and on PhaseStop fires reviewer.Review in a DETACHED
// goroutine (debounced by UserModelReviewInterval). The reviewer reads the
// finished session's transcript via the SessionStore and spawns a FRESH child
// session — it NEVER reopens the user's terminal session (R10).
func maybeWrapUserModelReview(cfg Config, hooks port.HookRunner, store port.SessionStore, provider port.LLMProvider, userModelStore *memory.Store) port.HookRunner {
	if !cfg.UserModelReview {
		cfg.diag().Log(context.Background(), port.LevelInfo, "user-model background review DISABLED")
		return hooks
	}
	if userModelStore == nil {
		cfg.diag().Log(context.Background(), port.LevelWarn, "user-model background review requested but the user-model store is disabled; review is a no-op")
		return hooks
	}
	reviewer := agent.NewUserModelReviewer(store, buildUserModelReviewEngine(cfg, provider, userModelStore))
	cfg.diag().Log(context.Background(), port.LevelInfo, "user-model background review ENABLED (Stop-triggered, detached fork; never reopens the user session)",
		"review_interval", cfg.UserModelReviewInterval)
	return newUserModelReviewHooks(hooks, reviewer, cfg.UserModelReviewInterval, cfg.diag())
}

// buildUserModelReviewEngine constructs the child *Engine the Phase-2b reviewer
// runs: a catalog containing ONLY the RememberUser tool bound to the user-model
// store, under the standard allow-all, non-interactive child policy. So the
// reviewer can WRITE the user model but has no other capability (no Read/Edit/Bash,
// no Task/Fork). RememberUser carries the write-time injection scan, so a
// transcript-poisoning attempt cannot land in the user-model block.
func buildUserModelReviewEngine(cfg Config, provider port.LLMProvider, store *memory.Store) *agent.Engine {
	cat := tool.NewCatalog()
	for _, t := range memory.NewUserModelTools(store) {
		if t.Spec().Name == memory.RememberUserToolName {
			cat.MustRegister(t)
		}
	}
	return newChildEngine(cfg.diag(), "usermodel-review", provider, cat, cfg.Model, promptConfig(cfg, cfg.gitStatus))
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
		cfg.diag().Log(context.Background(), port.LevelWarn, "could not build command runner; Bash tool disabled", "workspace", cfg.Workspace, "err", err)
		return nil
	}
	return runner
}

// buildSandboxedCommandRunner builds the command runner team MEMBERS' Bash executes
// against. It mirrors buildCommandRunner (returns nil when Bash is disabled) but
// HARDENS the runner against several git config-driven code-execution vectors in a
// SHARED `.git`: a read-only member runs in a git worktree (the forker default) that
// shares the parent repo's `.git/config` and `.git/hooks`, so without this an untrusted
// base repo could run code via core.pager / core.hooksPath / core.fsmonitor / an
// external diff driver the instant the member runs git.
//
// The runner is given a COMPLETE, scrubbed environment from gitenv.Scrub(os.Environ())
// — the SAME helper the forker uses for its own fork-time git, so the two cannot drift.
// Scrub:
//   - DROPS every inherited GIT_* variable (so GIT_EXTERNAL_DIFF, GIT_SSH_COMMAND,
//     GIT_ALTERNATE_OBJECT_DIRECTORIES, GIT_PROXY_COMMAND etc. cannot leak in — an
//     append-only env could not remove these) plus inherited PAGER/LESS, while
//     keeping PATH/HOME/etc. so git still functions;
//   - APPENDS the neutralizing set: GIT_CONFIG_NOSYSTEM=1,
//     GIT_CONFIG_GLOBAL=/dev/null, GIT_PAGER=cat, PAGER=cat, plus env-injected git
//     config (GIT_CONFIG_COUNT + KEY/VALUE pairs) that takes PRECEDENCE over the
//     shared repo-local .git/config, force-overriding core.hooksPath=/dev/null (kills
//     ALL repo hooks, including the fork-time post-checkout), core.pager=cat,
//     core.fsmonitor=false and an empty diff.external (no external diff driver).
//
// RESIDUAL — this does NOT close git driver configs whose driver NAME is attacker-chosen
// in a tracked `.gitattributes`: filter.<drv>.smudge (fires at worktree checkout / fork
// time) and diff.<drv>.textconv (fires on `git show` / `git log -p`), plus
// alias.<name>=!sh if the member invokes that alias by name. A fixed-key env override
// cannot pin an arbitrary driver name to an inert value. These are reachable only when
// the shared `.git` is an UNTRUSTED repo; for a TRUSTED repo this is equivalent to the
// operator running git themselves. The planned robust mitigation is to gate
// read-only-member shell on workspace trust (untrusted ⇒ no subagent shell) — a tracked
// follow-up, not yet implemented.
//
// The MAIN session keeps its own UNHARDENED runner (buildCommandRunner) so operator
// hooks/pager are honoured there; only team-member shells are sandboxed. Per-command
// timeout (~30s, applied by the runner) and the supervisor's concurrency cap
// (defaultTeamConcurrency=4) already bound how much shell a team can run, so no extra
// per-subagent deadline/semaphore is added here.
func buildSandboxedCommandRunner(cfg Config) tool.CommandRunner {
	if cfg.NoBash || cfg.Shell == "" {
		return nil
	}
	env := gitenv.Scrub(os.Environ())
	runner, err := osfs.NewCommandRunnerShell(cfg.Workspace, cfg.Shell, osfs.WithCommandEnvList(env))
	if err != nil {
		cfg.diag().Log(context.Background(), port.LevelWarn, "could not build sandboxed member command runner; team-member Bash disabled", "workspace", cfg.Workspace, "err", err)
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
func newChildEngine(diag port.Diagnostics, role string, provider port.LLMProvider, cat *tool.Catalog, model string, pc prompt.Config) *agent.Engine {
	return newChildEngineWithHooks(diag, role, provider, cat, model, pc, hookexec.New(nil))
}

// newChildEngineWithHooks is newChildEngine with an explicit HookRunner, so a
// per-def Task/member engine can scope its own lifecycle hooks (from a def's
// `hooks:` map) instead of the inert default. A nil hooks runner falls back to an
// inert one, preserving the no-hooks contract.
func newChildEngineWithHooks(diag port.Diagnostics, role string, provider port.LLMProvider, cat *tool.Catalog, model string, pc prompt.Config, hooks port.HookRunner) *agent.Engine {
	if hooks == nil {
		hooks = hookexec.New(nil)
	}
	if diag == nil {
		diag = port.NopDiagnostics{}
	}
	return agent.NewEngine(agent.Deps{
		LLM:     provider,
		Catalog: cat,
		// Child/member engines are non-interactive (allow-all) and never learn:
		// nil store disables Learn entirely for them.
		Policy:       permpolicy.NewPolicy([]governance.Rule{{Effect: governance.Allow}}, nil),
		Hooks:        hooks,
		PromptConfig: pc,
		Model:        model,
		// Diagnostics is LIVE for child engines (correlated by session + the agent
		// role below) so interleaved child diagnostics are readable on the operator
		// channel — this is DISTINCT from Sink/ToolCallRecorder (telemetry/audit),
		// which stay OFF for children (never set here). The role tags every line the
		// child emits with "agent"=<role>.
		Diagnostics:         diag,
		Role:                role,
		ContextWindowTokens: defaultContextWindowTokens,
		CompactionRatio:     defaultCompactionRatio,
	})
}

// newChildEngineForProvider is newChildEngineWithHooks BUT it RE-DERIVES the
// provider-closing Deps (Compactor/TokenCounter/PromptConfig.Env.Model/
// ContextWindow) for the supplied provider+model via engineDepsForProvider — so a
// child bound to a NON-default provider compacts and counts through THAT provider,
// never the default (the cross-provider contamination fix the Phase-0 panel
// flagged). It then OVERRIDES the non-provider fields back to the child's shape:
// an allow-all non-learning policy (nil store), the supplied catalog + hooks, and
// no instructions/sink/store (child engines are internal sub-agents, not
// persisted sessions). The provider is FIXED for this child's lifetime.
//
// For the INHERITED-DEFAULT case (a def that pins no provider on a default session)
// the caller passes contextWindow=0, so engineDepsForProvider falls back to the
// 128k default and the child stays byte-identical to the old newChildEngineWithHooks
// path. A provider-SWITCHED child gets its real catalog window (catalogContextWindow).
func newChildEngineForProvider(cfg Config, role string, provider port.LLMProvider, model string, contextWindow int, cat *tool.Catalog, pc prompt.Config, hooks port.HookRunner) *agent.Engine {
	return agent.NewEngine(childEngineDepsForProvider(cfg, role, provider, model, contextWindow, cat, pc, hooks))
}

// childEngineDepsForProvider builds the agent.Deps for a child/member engine bound
// to provider+model, re-deriving the provider-closing fields via
// engineDepsForProvider then OVERRIDING the non-provider fields back to the child's
// shape. It is split out from newChildEngineForProvider so a test can assert the
// child Deps directly (Sink/ToolCallRecorder nil, Compactor/TokenCounter keyed on the CHILD's
// model) — the engine's deps are otherwise private.
func childEngineDepsForProvider(cfg Config, role string, provider port.LLMProvider, model string, contextWindow int, cat *tool.Catalog, pc prompt.Config, hooks port.HookRunner) agent.Deps {
	if hooks == nil {
		hooks = hookexec.New(nil)
	}
	// engineDepsForProvider re-derives every provider-closing field against the
	// requested provider+model. We pass the child's allow-all policy and nil
	// store/mcpProvider/instructions directly so it does not adopt the main engine's
	// interactive policy or persistence. The PromptConfig it builds (Env.Model +
	// agency delta keyed on `model`) is then REPLACED with the caller's pc, which the
	// per-def path composes with the def body — but the Model/TokenCounter/Compactor/
	// ContextWindow it derived are kept (those are the contamination-sensitive fields).
	deps := engineDepsForProvider(cfg, provider, model, contextWindow,
		nil, // store: child engines never persist (disables Learn entirely)
		permpolicy.NewPolicy([]governance.Rule{{Effect: governance.Allow}}, nil),
		hooks,
		nil, // mcpProvider: child command expansion does not consult MCP prompts
		nil, // instructions: child engines carry no turn-0 instruction assembler
	)
	deps.Catalog = cat
	deps.PromptConfig = pc
	// Child engines do NOT expand slash commands (the old newChildEngineWithHooks
	// path left CommandExpander nil — a sub-agent receives literal instructions, not
	// user "/cmd" text). engineDepsForProvider built one from cfg; clear it so the
	// child's non-provider shape is unchanged from the pre-feature constructor.
	deps.CommandExpander = nil
	// Telemetry stays OFF for child engines: engineDepsForProvider set Sink/ToolCallRecorder
	// from cfg, but the OLD child constructor (newChildEngineWithHooks) left BOTH nil,
	// so a sub-agent's turns/tool-calls were invisible to the operator-facing
	// TTFT/turn-duration histograms. Restoring nil keeps byte-identity with the
	// pre-feature child shape — without it every def-pinned / team-member / Half-B
	// session child would double-count against the shared Sink. Distinct sub-agent
	// telemetry tagging is a SEPARATE decision; nil is the conservative choice here.
	deps.Sink = nil
	deps.ToolCallRecorder = nil
	// Telemetry (Sink) and the per-tool audit seam (ToolCallRecorder) stay OFF for
	// child engines (set nil just above) — a sub-agent's turns/tool-calls must not
	// double through the operator's metrics/audit. Diagnostics is DIFFERENT: it is
	// intentionally LIVE for children, bound to cfg.diag() and tagged with the
	// child's agent role (Deps.Role), so interleaved child diagnostics (compaction
	// degradation, policy denies) are readable and correlated on the operator
	// channel. This is the one operator-facing seam children speak on; Sink and
	// ToolCallRecorder remain silent.
	deps.Diagnostics = cfg.diag()
	deps.Role = role
	return deps
}

// buildChildEngine constructs the default Task explorer child *Engine: the read-only
// explorer toolset (Read/Grep/Glob — never Fork/Task/ToolSearch, so a child can never
// recurse or fan out further, and never Edit/Write, so it cannot edit the project)
// under an allow-all, non-interactive policy.
//
// Bash IS registered when a runner is configured (runner != nil), using the SANDBOXED
// runner: a Task child now runs in an isolated git WORKTREE (wired via the Task tool's
// child forker — see buildTaskTool) that SHARES the parent repo's `.git`, so its shell
// can inspect history (git log/show), build, and test confined to a throwaway
// checkout. Because the worktree shares `.git/config` and `.git/hooks`, the runner
// must be the hardened buildSandboxedCommandRunner (same rationale as team members —
// see that func) so an untrusted base repo cannot run code via
// core.pager/hooksPath/fsmonitor/external-diff the instant the child runs git. A
// shell-less deployment passes a nil runner and the child runs Bash-less (and the
// caller wires no forker), exactly like the original read-only explorer.
//
// BashTool.Execute is workspace-aware: it passes the child's forked Workspace.Root()
// to the runner as the working directory, so the child's Bash defaults to its OWN
// worktree, not the shared parent base.
func buildChildEngine(cfg Config, provider port.LLMProvider, runner tool.CommandRunner) *agent.Engine {
	childCat := tool.NewCatalog()
	childCat.MustRegister(tools.ReadTool{})
	childCat.MustRegister(tools.GrepTool{})
	childCat.MustRegister(tools.GlobTool{})
	if runner != nil {
		childCat.MustRegister(tools.NewBashTool(runner))
	}

	return newChildEngine(cfg.diag(), "task", provider, childCat, cfg.Model, promptConfig(cfg, cfg.gitStatus))
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

	return newChildEngine(cfg.diag(), "fork", provider, childCat, cfg.Model, promptConfig(cfg, cfg.gitStatus))
}

// buildForkJudgeEngine constructs the minimal, tool-less read-only child *Engine
// the Fork join=judge/best strategy runs to SELECT a winner. It scores text only,
// so it gets an EMPTY catalog (no tools) under an allow-all policy. It is a DISTINCT
// Engine instance from the branch child so, with the mockllm shared-cursor provider
// in tests, the judge's LLM calls never interleave with the branches'; with the
// stateless OpenAI adapter this separation is naturally harmless.
func buildForkJudgeEngine(cfg Config, provider port.LLMProvider) *agent.Engine {
	return newChildEngine(cfg.diag(), "fork-judge", provider, tool.NewCatalog(), cfg.Model, promptConfig(cfg, cfg.gitStatus))
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
//
// Workspace isolation (Phase 2): when Bash is configured, the Task tool is wired with
// a SANDBOXED command runner AND a worktree forker (the forker DEFAULT mode — no
// WithForceCopy — so the child shares the parent repo's `.git` for full history). The
// Task tool then forks each child run into a throwaway git worktree before running it,
// so a read-only explorer's shell (git log/show, build, test) is confined to that
// worktree and never touches the shared parent base — which is what keeps Task
// read-parallel-safe (see agent.TaskTool.ReadOnly). The sandboxed runner neutralises
// the git config-driven code-execution vectors in the shared `.git` (same rationale
// and residual as team members — see buildSandboxedCommandRunner). When Bash is
// disabled (nil runner) no forker is wired and the child stays a base-sharing
// read-only explorer with no shell, exactly as before.
//
// PER-SUB-AGENT PROVIDER: provReg + parentProviderID + parentModel are the
// inheritance point threaded down to buildAgentTaskEngines so a def's `provider:`
// can route its child to a different provider (Half A) and a def that pins none
// inherits whatever the call site supplies — the build-time default (buildCatalog)
// or a session-selected provider (Half B). The registry never reaches the Task
// tool itself; it is consumed only inside buildAgentTaskEngines' resolution loop.
func buildTaskTool(ctx context.Context, cfg Config, provReg *providerRegistry, provider port.LLMProvider, parentProviderID, parentModel string, hooks port.HookRunner, reg *agents.Registry, mainMgr *mcp.Manager) (tool.Tool, func() error) {
	// Resolve the active skills once so a def's `skills:` can preload skill bodies
	// into its engine prompt. The same index is the operator-controlled skill set
	// the Skill tool serves. `hooks` is the inert default each def adopts unless its
	// own `hooks:` map scopes lifecycle hooks to its engine.
	skillIdx := resolveSkillIndex(ctx, cfg)
	// The Task child's Bash runs over a worktree that SHARES the parent `.git`, so it
	// gets the HARDENED runner (the main session keeps its own unhardened runner). nil
	// when Bash is disabled — then no shell, no forker.
	sandboxedRunner := buildSandboxedCommandRunner(cfg)
	engines, meta, mcpClose := buildAgentTaskEngines(ctx, cfg, provider, provReg, parentProviderID, parentModel, reg, skillIdx, hooks, sandboxedRunner, mainMgr)
	opts := []agent.TaskOption{
		agent.WithSubagentStopHook(hooks),
		agent.WithAgentEngines(engines, meta),
	}
	// Wire the worktree forker ONLY when Bash is available: the child catalog has Bash
	// iff sandboxedRunner != nil, and the forker is what isolates that shell. The two
	// must move together — a Bash child without isolation would run its shell in the
	// shared base (the exact hazard); a forker without Bash would fork for nothing.
	if sandboxedRunner != nil {
		// Worktree default (no WithForceCopy): shares the base repo's `.git` ⇒ full
		// history for git log/show, with its own throwaway working tree.
		taskForker := forker.New(func(root string) (tool.Workspace, error) { return osfs.NewWorkspace(root) })
		opts = append(opts, agent.WithChildForker(taskForker))
	}
	return agent.NewTaskTool(
		buildChildEngine(cfg, provider, sandboxedRunner),
		opts...,
	), mcpClose
}

// buildTeamWiring constructs the agent-team dependencies — the unified per-member
// engine factory, the TWO workspace forkers (force-copy for mutating members,
// worktree for read-only-isolated members), and the shared team hooks runner — that
// BOTH team entry points consume: the parent catalog's Team tool (buildCatalog) and
// the gRPC CreateTeam path (applyTeamConfig → server.Config). It is the single source
// of that wiring truth, so the two paths cannot drift; each caller invokes it and
// gets a functionally identical factory. It is only ever called under cfg.EnableTeams.
//
// The factory is server.MemberEngineFactory, which is the SAME shape as
// agent.TeamMemberEngineFactory (both `func(*team.Team, MemberSpec) MemberBuild`), so
// one factory value satisfies both the gRPC Config.MemberEngine and NewTeamTool.
//
// It resolves the agent-definition registry and skill index ONCE (exactly as
// buildCatalog shares the registry with the Task tool). The two forkers:
//   - fk (force-copy, WithForceCopy): a Mutating member runs in a FULLY isolated fork
//     (own .git object DB/refs), matching buildCatalog's Fork branch wiring, so its
//     git commit/push/update-ref cannot escape into the base repo.
//   - roFk (worktree, the forker DEFAULT — no WithForceCopy): a read-only-isolated
//     member runs in a cheap git worktree that SHARES the base repo's .git (⇒ full
//     history for git log/show) but has its own working tree; it never edits, only
//     inspects.
//
// The member Bash runs through a SANDBOXED command runner
// (buildSandboxedCommandRunner) that neutralises the fixed-key git config-driven
// code-execution vectors in the shared .git (core.pager/hooksPath/fsmonitor/external
// diff); a residual remains for attacker-named `.gitattributes` filter/diff drivers in
// an untrusted repo (see buildSandboxedCommandRunner). roIsolationAvailable (runner
// wired AND roFk non-nil) tells the factory it may grant a read-only member Bash and
// mark it IsolateReadOnly. The single teamHooks runner is threaded through both the
// supervisor (TeammateIdle) and the member coordination tools (TaskCreated /
// TaskCompleted gates). mainMgr supplies the per-agent MCP base manager so a member's
// agent definition can scope its MCP servers.
//
// PER-SUB-AGENT PROVIDER: provReg + parentProviderID + parentModel thread the
// inheritance point into buildMemberEngine so a member's agent def `provider:` can
// route its engine to a different provider, and a member that pins none inherits
// whatever the caller supplies (the build-time default in buildCatalog/
// applyTeamConfig, or a session-selected provider in Half B's in-catalog Team tool).
func buildTeamWiring(ctx context.Context, cfg Config, provReg *providerRegistry, provider port.LLMProvider, parentProviderID, parentModel string, mainMgr *mcp.Manager) (server.MemberEngineFactory, tool.WorkspaceForker, tool.WorkspaceForker, port.HookRunner) {
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
	// fk (force-copy) for mutating members; roFk (worktree default) for read-only
	// members the factory grants a shell. Read-only members run git in the shared
	// .git of a worktree, so they get the SANDBOXED runner; the main session keeps its
	// own unhardened runner elsewhere.
	fk := forker.New(func(root string) (tool.Workspace, error) { return osfs.NewWorkspace(root) }, forker.WithForceCopy())
	roFk := forker.New(func(root string) (tool.Workspace, error) { return osfs.NewWorkspace(root) })
	memberRunner := buildSandboxedCommandRunner(cfg)
	roIsolationAvailable := memberRunner != nil && roFk != nil
	factory := buildMemberEngine(cfg, provReg, provider, parentProviderID, parentModel, teamHooks, agentReg, skillIdx, memberRunner, roIsolationAvailable, mainMgr)
	return factory, fk, roFk, teamHooks
}

// applyTeamConfig wires the opt-in agent-teams capability into the server.Config.
// When cfg.EnableTeams is false it leaves MemberEngine nil (CreateTeam stays
// ErrTeamsDisabled). When enabled it installs the per-member engine factory, the
// workspace forker, and the shared team hooks runner — all from buildTeamWiring, the
// SAME wiring the Team tool uses (buildCatalog) — so the gRPC CreateTeam path and the
// Team tool cannot drift. MaxTeams is left at zero so the server applies its own
// default.
func applyTeamConfig(svcCfg *server.Config, cfg Config, reg *providerRegistry, provider port.LLMProvider, mainMgr *mcp.Manager) {
	if !cfg.EnableTeams {
		cfg.diag().Log(context.Background(), port.LevelInfo, "agent teams DISABLED (set --enable-teams to enable; experimental)")
		return
	}
	// The gRPC CreateTeam path's MemberEngine is wired ONCE here with the build-time
	// DEFAULT provider as the inherited parent (reg.Default()/cfg.Model). Per-session
	// provider propagation to the standalone CreateTeam RPC is DEFERRED (CreateTeam
	// carries no selector today); the in-catalog Team tool IS covered in Half B.
	factory, fk, roFk, teamHooks := buildTeamWiring(context.Background(), cfg, reg, provider, reg.Default(), cfg.Model, mainMgr)
	svcCfg.MemberEngine = factory
	svcCfg.Forker = fk
	svcCfg.ReadOnlyForker = roFk
	svcCfg.TeamHooks = teamHooks
	cfg.diag().Log(context.Background(), port.LevelInfo, "agent teams ENABLED (experimental; CreateTeam/SpawnTeammate/RunTeam + Team tool)")
}

// modelCfgFor returns a copy of cfg with the Model field overridden, so a
// promptConfig built for a child/member keys its Env.Model + agency delta on the
// INHERITED (parent/session) model rather than cfg.Model. It mirrors the
// modelCfg-clone idiom inside engineDepsForProvider. An empty override leaves
// cfg.Model unchanged (the build-time default path).
func modelCfgFor(cfg Config, model string) Config {
	if model == "" {
		return cfg
	}
	cfg.Model = model
	return cfg
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
//
// PER-SUB-AGENT PROVIDER: provReg + parentProviderID + parentModel are the
// inheritance point. A DEFINED member resolves its def's (provider, model) via
// resolveProviderModel; a def that pins a known provider routes the member engine
// to THAT provider (built through newChildEngineForProvider so it compacts/counts
// on the right model). A member pinning none — or the DEFAULT (undefined) member —
// inherits the parent provider unchanged.
func buildMemberEngine(cfg Config, provReg *providerRegistry, provider port.LLMProvider, parentProviderID, parentModel string, teamHooks port.HookRunner, reg *agents.Registry, skillIdx skillIndex, runner tool.CommandRunner, roIsolationAvailable bool, mainMgr *mcp.Manager) server.MemberEngineFactory {
	return func(t *team.Team, spec agent.MemberSpec) agent.MemberBuild {
		cat := tool.NewCatalog()
		var (
			// Default (undefined) member: inherit the parent provider + model the call
			// site supplied (the build-time default, or a session-selected provider in
			// Half B). A DEFINED member overrides these via resolveProviderModel below.
			model         = parentModel
			pc            = promptConfig(modelCfgFor(cfg, parentModel), cfg.gitStatus)
			childProvider = provider
			childWindow   = 0
			mode          session.PermissionMode
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
			// isolateReadOnly is set true iff this is a NON-mutating member that we
			// nonetheless gave Bash (runner wired AND a read-only forker available). It
			// tells the supervisor to run the member in a throwaway git worktree (where
			// its Bash is confined) and to exempt it from the base-sharing
			// mutating-tool backstop. It stays false for a Mutating member (its flag
			// already drives the force-copy fork) and for a base-sharing read-only
			// member (no shell).
			isolateReadOnly bool
		)

		def, defined := lookupMemberDef(cfg.diag(), reg, spec)
		if defined {
			// Scope the def over the member's AVAILABLE base, allowing mutating tools
			// (Edit/Write/Bash) only for a Mutating member — it runs in an isolated
			// fork, and Bash is now workspace-aware (BashTool passes the member's
			// forked Workspace.Root() to the runner as workdir), so a def MAY scope
			// Bash in for a Mutating member and it runs in the member's fork, not the
			// shared parent base. For a read-only member that we can isolate in a
			// worktree (allowShell), scopedToolNamesMode keeps Bash but still drops
			// Edit/Write; for a base-sharing read-only member it drops all three.
			base := baseTaskTools(cfg)
			// allowShell: a non-mutating member may keep Bash ONLY when a runner is
			// wired AND a read-only forker is available to isolate it in a worktree.
			allowShell := !spec.Mutating && runner != nil && roIsolationAvailable
			names, diags := scopedToolNamesMode(def, base, spec.Mutating, allowShell)
			for _, d := range diags {
				cfg.diag().Log(context.Background(), port.LevelWarn, "team member agent def tool scoping",
					"member", spec.Name, "agent", def.Name, "tool", d.tool, "reason", d.reason, "path", def.Path)
			}
			for _, name := range names {
				// Bash registers with the HARDENED member runner (passed in), not the
				// unhardened baseTaskTools one used purely to compute the name set — so a
				// member's shell over the shared `.git` cannot be hijacked via git config
				// (core.pager/hooksPath/fsmonitor/external-diff). Every other tool registers
				// as-is. Note: there is NO unhardened member-Bash fall-through — `runner` is
				// always the sandboxed memberRunner; a force-copy (Mutating) member would not
				// even need it (its fork has its OWN `.git`, so config-hardening is moot
				// there), but it gets the hardened runner anyway.
				if name == tools.BashToolName && runner != nil {
					cat.MustRegister(tools.NewBashTool(runner))
					continue
				}
				cat.MustRegister(base[name])
			}
			// A read-only def-member that ended up with Bash is worktree-isolated.
			if allowShell {
				for _, name := range names {
					if name == tools.BashToolName {
						isolateReadOnly = true
						break
					}
				}
			}
			// Per-agent MCP: add the def's referenced/inline servers' tools to THIS
			// member's catalog. The inline managers' Close rides on the MemberBuild so the
			// supervisor tears them down on member teardown; the MCP tool names are handed
			// to the supervisor so the read-only-member backstop exempts them (they report
			// ReadOnly()==false but never touch the workspace).
			mcpTools, names2, cl := defMCPTools(context.Background(), cfg.diag(), def, mainMgr)
			for _, mt := range mcpTools {
				if err := cat.Register(mt); err != nil {
					cfg.diag().Log(context.Background(), port.LevelWarn, "team member agent def MCP tool registration failed; skipped",
						"member", spec.Name, "agent", def.Name, "tool", mt.Spec().Name, "err", err)
				}
			}
			mcpClose, mcpNames = cl, names2
			memberLimits = defLimits(def, session.Limits{}) // only def-set fields; AddMember merges with the team default
			// Resolve the def's (provider, model, window) via the SHARED helper: a
			// pinned-and-known provider switches the member engine; a def pinning none
			// inherits the parent. resolve ONCE; thread the model into agentPromptConfig.
			childProvider, _, model, childWindow = resolveChildProvider(cfg, provReg, def, provider, parentProviderID, parentModel)
			bodies, missing := preloadedSkillBodies(def, skillIdx)
			for _, name := range missing {
				cfg.diag().Log(context.Background(), port.LevelWarn, "team member agent def references an unknown skill; not preloaded",
					"member", spec.Name, "agent", def.Name, "skill", name, "path", def.Path)
			}
			pc = agentPromptConfig(cfg, def, model, bodies...)
			mode = resolvePermissionMode(cfg.diag(), def)
			// A def's `hooks:` scope lifecycle hooks to this member's engine. A def that
			// scopes none keeps the inert default (memberHooks unchanged), preserving the
			// historical defined-member engine shape.
			memberHooks = defHookRunner(cfg, def, memberHooks)
			cfg.diag().Log(context.Background(), port.LevelInfo, "team member adopts agent def",
				"member", spec.Name, "agent", def.Name, "tools", strings.Join(names, ","),
				"model", model, "mode", mode, "mutating", spec.Mutating,
				"isolate_read_only", isolateReadOnly,
				"preloaded_skills", len(bodies), "path", def.Path)
		} else {
			// Default member catalog (three tiers). Read/Grep/Glob always. Edit/Write
			// only for a Mutating member. Bash when a runner is wired AND the member is
			// either Mutating (own force-copy fork) OR read-only-isolated (own worktree,
			// roIsolationAvailable) — so a read-only member now gets a shell for
			// inspection (git log/show, build, test) confined to its throwaway worktree,
			// while a base-sharing read-only member (no forker) still gets NO shell, so
			// the read-only-share isolation guarantee holds. Bash is workspace-aware
			// (BashTool.Execute passes the member's forked Workspace.Root() to the runner
			// as workdir), so an isolated member's Bash runs in its OWN fork/worktree,
			// not the shared parent base. (Bash can still escape its cwd via absolute
			// paths / `cd`, the inherent Bash trust model; isolation is the boundary.)
			cat.MustRegister(tools.ReadTool{})
			cat.MustRegister(tools.GrepTool{})
			cat.MustRegister(tools.GlobTool{})
			if spec.Mutating {
				cat.MustRegister(tools.EditTool{})
				cat.MustRegister(tools.WriteTool{})
			}
			if runner != nil && (spec.Mutating || roIsolationAvailable) {
				cat.MustRegister(tools.NewBashTool(runner))
				isolateReadOnly = !spec.Mutating && roIsolationAvailable
			}
		}

		// Team coordination tools ALWAYS, in both branches: they bypass the def
		// allowlist and are exempt from the read-only-member mutating-tool backstop.
		for _, mt := range agent.MemberTools(t, spec.Name, teamHooks) {
			cat.MustRegister(mt)
		}

		// Built through newChildEngineForProvider so a provider-switched member
		// compacts/counts on its own model (contamination fix); childWindow=0 for the
		// inherited-default member keeps it byte-identical.
		eng := newChildEngineForProvider(cfg, "member:"+spec.Name, childProvider, model, childWindow, cat, pc, memberHooks)
		return agent.MemberBuild{Engine: eng, Mode: mode, Limits: memberLimits, Close: mcpClose, MCPToolNames: mcpNames, IsolateReadOnly: isolateReadOnly}
	}
}

// lookupMemberDef resolves spec.AgentType against the registry, returning the def
// and true on a hit. An empty AgentType or a miss returns false (the caller falls
// back to the default member catalog); a miss on a NON-empty AgentType also warns,
// so a stale roster reference is observable without failing the spawn.
func lookupMemberDef(d port.Diagnostics, reg *agents.Registry, spec agent.MemberSpec) (agents.AgentDef, bool) {
	name := strings.TrimSpace(spec.AgentType)
	if name == "" || reg == nil {
		return agents.AgentDef{}, false
	}
	def, ok := reg.Get(name)
	if !ok {
		d.Log(context.Background(), port.LevelWarn, "team member references an unknown agent def; using the default member catalog",
			"member", spec.Name, "agent", name)
		return agents.AgentDef{}, false
	}
	return def, true
}

// promptConfig builds the system-prompt configuration. The volatile Env values
// (cwd/os/model/date/mode) are computed HERE in the composition layer so the domain
// stays infra-free; the loop fills in the per-turn Mode and Tools. The git snapshot
// is NOT recomputed here — it is the single value Build computed once (hardened +
// trust-gated) and threaded in as gitStatus, so a child-engine build or a team-member
// spawn never re-runs git on the hot path (FIX 2).
func promptConfig(cfg Config, gitStatus string) prompt.Config {
	pc := prompt.Config{
		Env: prompt.Env{
			Cwd:       cfg.Workspace,
			OS:        runtime.GOOS,
			Model:     cfg.Model,
			Date:      time.Now().Format("2006-01-02"),
			Mode:      string(session.ModeDefault),
			Shell:     cfg.Shell,
			GitStatus: gitStatus,
		},
	}
	// The emphatic task-persistence "agency" contract is supplied per-model HERE
	// (the prompt package stays model-neutral); fold it onto the default role.
	if d := agencyDelta(cfg.Model); d != "" {
		pc.Role = prompt.DefaultRole() + "\n\n" + d
	}
	return pc
}

// agencyDelta returns the per-model emphatic task-persistence contract appended
// to the role framing. Claude-family models already persist on a task without
// it (and the extra wording can over-steer them), so it is OMITTED for Claude
// and supplied for every other model family. The prompt package is model-neutral
// by design; this model-family decision lives in the composition layer.
func agencyDelta(model string) string {
	if strings.Contains(strings.ToLower(model), "claude") {
		return ""
	}
	return "Keep going until the task is actually resolved before ending your " +
		"turn — implement the change rather than describing it, and do not stop " +
		"at analysis or a partial fix. But when you are genuinely blocked or the " +
		"request is ambiguous, stop and ask rather than guessing."
}

// gitSnapshot returns a bounded, fail-soft start-of-session git snapshot for the
// workspace (branch + short status + recent commits), rendered into the volatile
// <git-status> sub-block. It runs git best-effort through an osfs command runner and
// returns "" on any failure — a missing shell, a non-git directory (rev-parse fails),
// or a timeout. It never registers a tool.
//
// SECURITY (FIX 1): reading an untrusted `.git` is exactly the operation that needs
// neutralizing — `git status`/`git log` would otherwise execute repo-local
// core.fsmonitor / core.pager / core.hooksPath / an external diff driver, an RCE on a
// malicious clone the instant the session starts. So this runner is HARDENED with the
// SAME scrubbed env as buildSandboxedCommandRunner — gitenv.Scrub(os.Environ()) via
// osfs.WithCommandEnvList — which drops inherited GIT_*/PAGER and force-overrides
// core.hooksPath=/dev/null, core.pager=cat, core.fsmonitor=false and an empty
// diff.external, neutralizing the fixed-key git-config code-exec vectors. AND it is
// TRUST-GATED: it runs ONLY for a trusted workspace (trustProject) — when untrusted it
// returns "" so no <git-status> block renders at all and no git ever executes against
// the untrusted repo. The residual attacker-named .gitattributes driver vectors are
// the same as buildSandboxedCommandRunner and moot here given the trust gate.
func gitSnapshot(workspace, shell string, trustProject bool) string {
	// Trust gate: never run git against an untrusted workspace.
	if !trustProject {
		return ""
	}
	// Harden the runner with the scrubbed git env (same pattern as
	// buildSandboxedCommandRunner) so a repo-local git config cannot run code.
	env := gitenv.Scrub(os.Environ())
	runner, err := osfs.NewCommandRunnerShell(workspace, shellOr(shell), osfs.WithCommandEnvList(env))
	if err != nil {
		return ""
	}
	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
	defer cancel()

	run := func(command string) string {
		res, err := runner.Run(ctx, command, workspace)
		if err != nil || res.ExitCode != 0 {
			return ""
		}
		return strings.TrimSpace(res.Stdout)
	}

	branch := run("git rev-parse --abbrev-ref HEAD")
	if branch == "" {
		// Not a git repo (or git unavailable) — emit nothing.
		return ""
	}

	status := run("git status --short")
	if status == "" {
		status = "(clean)"
	} else {
		// Bound the status to ~20 lines so a noisy tree cannot blow up the prompt.
		lines := strings.Split(status, "\n")
		if len(lines) > 20 {
			lines = append(lines[:20], "... (truncated)")
			status = strings.Join(lines, "\n")
		}
	}

	commits := run("git log --oneline -n 5")

	snapshot := "branch: " + branch + "\nstatus:\n" + status
	if commits != "" {
		snapshot += "\ncommits:\n" + commits
	}
	// Final guard: cap the whole snapshot to ~2KB. Truncate on a rune boundary
	// (FIX 3) so the byte cap cannot slice a multibyte UTF-8 rune mid-sequence:
	// back off to the start of the last valid rune.
	const maxSnapshot = 2048
	if len(snapshot) > maxSnapshot {
		snapshot = snapshot[:maxSnapshot]
		for len(snapshot) > 0 && !utf8.ValidString(snapshot) {
			snapshot = snapshot[:len(snapshot)-1]
		}
	}
	return snapshot
}

// shellOr returns shell, or a sane default when it is empty, so gitSnapshot can
// run even if no shell was configured for the Bash tool.
func shellOr(shell string) string {
	if shell == "" {
		return "/bin/sh"
	}
	return shell
}

// SoulApplyAction is the SYNTHETIC governance action key the soul load-gate
// evaluates (issue #14). It is NOT a real tool — the soul is fenced DATA applied at
// build time, not a tool the model invokes — but governance.Rule.Tool is a free
// string matched verbatim by the evaluator, so a synthetic colon-namespaced key
// rides the SAME deny→ask→allow machinery as a real tool. The colon namespace
// guarantees it can never collide with a real tool name (tool names are
// identifier-like, never colon-bearing). The soul is pre-approved at the built-in
// floor (see defaultRules), so it does not prompt by default but is explicit,
// auditable in source + logs, and overridable to ask/deny via operator config.
const SoulApplyAction = "soul:apply"

// defaultRules is the built-in permission ruleset: read-only tools (Read, Grep,
// Glob, WebFetch, the Task explorer) are allowed; mutating tools (Bash, Edit, Write)
// and the writable SkillDraft tool ask for approval. Anything unmatched defaults to
// ask via the evaluator.
//
// These rules carry ScopeBuiltinDefault — the LOWEST precedence scope, below every
// config scope (issue #13). That lets a higher-scope config Allow LOOSEN a built-in
// Ask (e.g. a project `.mecatl/settings.yaml` that allows `Bash(go test:*)` relaxes
// the built-in Bash→Ask). Deny/ask in any scope still beats allow, so a config can
// only loosen a built-in ASK, never a built-in DENY (there are none here) — and a
// config deny/ask still wins over anything.
//
// MEMORY + SOUL pre-approval (issue #14): the six memory tools (per-project
// Remember/Recall/SearchMemory + cross-project RememberUser/RecallUser/
// SearchUserModel) and the synthetic soul-application action ("soul:apply") are
// pre-approved here as explicit ScopeBuiltinDefault ALLOWs. They are the agent's
// memory capability + the soul-application gesture — pre-approved at the built-in
// floor (the LOWEST scope) so they do NOT prompt by default, yet they are EXPLICIT
// (visible in source + ENABLED logs) and OVERRIDABLE: because the floor is the
// lowest scope, a higher-scope config Ask/Deny (a user/project/managed
// settings.yaml) still WINS — a user can flip any of them to ask/deny. Being
// floor-scoped + tool-name-exact, these allows can only LOSE to a higher-scope
// ask/deny; they never loosen any OTHER tool's Ask, so the deny-dominant +
// loosen-only-the-floor invariants are structurally untouched.
func defaultRules() []governance.Rule {
	return []governance.Rule{
		{Scope: governance.ScopeBuiltinDefault, Tool: "Read", Effect: governance.Allow},
		{Scope: governance.ScopeBuiltinDefault, Tool: "Grep", Effect: governance.Allow},
		{Scope: governance.ScopeBuiltinDefault, Tool: "Glob", Effect: governance.Allow},
		{Scope: governance.ScopeBuiltinDefault, Tool: "WebFetch", Effect: governance.Allow},
		{Scope: governance.ScopeBuiltinDefault, Tool: "Task", Effect: governance.Allow},
		{Scope: governance.ScopeBuiltinDefault, Tool: "Bash", Effect: governance.Ask},
		{Scope: governance.ScopeBuiltinDefault, Tool: "Edit", Effect: governance.Ask},
		{Scope: governance.ScopeBuiltinDefault, Tool: "Write", Effect: governance.Ask},
		{Scope: governance.ScopeBuiltinDefault, Tool: skills.DraftToolName, Effect: governance.Ask},
		// Team spawns coordinating subagents that may mutate the workspace (Mutating
		// members), so it ASKS — unlike the read-only Task explorer, which is allowed.
		{Scope: governance.ScopeBuiltinDefault, Tool: "Team", Effect: governance.Ask},
		// Memory capability (per-project + cross-project), pre-approved at the floor:
		// reading/writing the agent's own saved facts is part of "having a memory", not
		// a workspace mutation, so it does not prompt by default. Overridable to ask/deny.
		{Scope: governance.ScopeBuiltinDefault, Tool: memory.RememberToolName, Effect: governance.Allow},
		{Scope: governance.ScopeBuiltinDefault, Tool: memory.RecallToolName, Effect: governance.Allow},
		{Scope: governance.ScopeBuiltinDefault, Tool: memory.SearchMemoryToolName, Effect: governance.Allow},
		{Scope: governance.ScopeBuiltinDefault, Tool: memory.RememberUserToolName, Effect: governance.Allow},
		{Scope: governance.ScopeBuiltinDefault, Tool: memory.RecallUserToolName, Effect: governance.Allow},
		{Scope: governance.ScopeBuiltinDefault, Tool: memory.SearchUserModelToolName, Effect: governance.Allow},
		// Soul application: the synthetic "soul:apply" action the soul load-gate
		// consults at build time (selectSoulSource). Pre-approved here so the soul is
		// applied by default; an operator config Ask/Deny still wins (Ask ⇒ withheld,
		// since the soul is applied at build time with no interactive gate).
		{Scope: governance.ScopeBuiltinDefault, Tool: SoulApplyAction, Effect: governance.Allow},
	}
}

// mainRules returns the main engine's static ruleset: the built-in floor, plus
// — when cfg.AllowAllTools — a single ScopeCLI allow-all rule that loosens that
// floor (a Deny in any scope and any configured Ask still win).
func mainRules(cfg Config) []governance.Rule {
	rules := defaultRules()
	if cfg.AllowAllTools {
		rules = append([]governance.Rule{{Scope: governance.ScopeCLI, Effect: governance.Allow}}, rules...)
	}
	return rules
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
func osfsWorkspaceFactory(d port.Diagnostics) server.WorkspaceFactory {
	return func(root string) tool.Workspace {
		ws, err := osfs.NewWorkspace(root)
		if err != nil {
			d.Log(context.Background(), port.LevelError, "workspace factory: cannot open root", "root", root, "err", err)
			return nil
		}
		return ws
	}
}
