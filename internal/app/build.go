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
// It MAY import adapters, engine/agent, and (transitively, via the server
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

	"github.com/stacklok/mecatl/engine/adapter/memstore"
	"github.com/stacklok/mecatl/engine/adapter/permpolicy"
	"github.com/stacklok/mecatl/engine/adapter/permstore"
	"github.com/stacklok/mecatl/engine/adapter/wallclock"
	"github.com/stacklok/mecatl/engine/agent"
	"github.com/stacklok/mecatl/engine/governance"
	"github.com/stacklok/mecatl/engine/port"
	"github.com/stacklok/mecatl/engine/prompt"
	"github.com/stacklok/mecatl/engine/session"
	"github.com/stacklok/mecatl/engine/team"
	"github.com/stacklok/mecatl/engine/tool"
	"github.com/stacklok/mecatl/internal/adapter/agents"
	"github.com/stacklok/mecatl/internal/adapter/dream"
	"github.com/stacklok/mecatl/internal/adapter/forker"
	"github.com/stacklok/mecatl/internal/adapter/gitenv"
	"github.com/stacklok/mecatl/internal/adapter/grpcdriver"
	"github.com/stacklok/mecatl/internal/adapter/hookexec"
	"github.com/stacklok/mecatl/internal/adapter/mcp"
	mcpsource "github.com/stacklok/mecatl/internal/adapter/mcp/source"
	"github.com/stacklok/mecatl/internal/adapter/memory"
	"github.com/stacklok/mecatl/internal/adapter/osfs"
	"github.com/stacklok/mecatl/internal/adapter/permconfig"
	"github.com/stacklok/mecatl/internal/adapter/providercatalog"
	"github.com/stacklok/mecatl/internal/adapter/server"
	"github.com/stacklok/mecatl/internal/adapter/skills"
	"github.com/stacklok/mecatl/internal/adapter/store/jsonlstore"
	"github.com/stacklok/mecatl/internal/adapter/tokenizer"
	"github.com/stacklok/mecatl/internal/adapter/tools"
	"github.com/stacklok/mecatl/internal/adapter/xdgconfig"
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
	defaultMaxTurns               = 100
	defaultMaxToolCalls           = 400
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

	// MaxNoProgressNudges bounds how many continuation nudges the loop injects after
	// a completed turn that produced NEITHER a tool call NOR meaningful text (a
	// reasoning-only / empty turn that a reasoning model can emit). It is threaded
	// through engineDepsForProvider to agent.Deps.MaxNoProgressNudges. Semantics
	// (applied in agent.NewEngine): ZERO (the default; operators who never set it)
	// uses the safety-net default of 2; NEGATIVE disables nudging; positive overrides.
	// It is operator-tunable but defaults to the safe non-zero behaviour without any
	// flag. Child/member/lead engines inherit it via engineDepsForProvider.
	MaxNoProgressNudges int

	// MaxRunTokens is the loop-level cumulative token ceiling for a single run (the
	// shared runaway brake serving main + Subagent + Team + Fork). It is threaded through
	// engineDepsForProvider to agent.Deps.MaxRunTokens and INHERITED by every child/
	// member/lead engine (childEngineDepsForProvider keeps it). Semantics (in the loop):
	// 0 (the default; operators who never set it) DISABLES the budget, so existing
	// behaviour is byte-identical; a positive value is the ceiling and a run that crosses
	// it terminates cleanly with session.StopBudget (Reopen-recoverable). Operator-tunable
	// via --max-run-tokens.
	MaxRunTokens int

	// MaxTeamTokens is the TEAM-WIDE cumulative token budget threaded into every team
	// (agent.WithTeamToolTokenBudget for the in-catalog Team tool, server.Config.TeamTokenBudget
	// for the gRPC CreateTeam path). It is checked at the ROUND boundary: when crossed the
	// team stops scheduling new rounds while the in-flight round and the lead's synthesis
	// still complete. 0 (the default) disables it. It is ORTHOGONAL to MaxRunTokens, which
	// bounds ONE member drive and resets on Reopen each round — both compose. Operator-tunable
	// via --max-team-tokens.
	MaxTeamTokens int

	// Memory: per-project memory store directory (empty disables the tools), plus
	// the background consolidation (dream) interval (0 disables; only meaningful
	// with MemoryDir set).
	MemoryDir                 string
	MemoryConsolidateInterval time.Duration

	// Child-session retention/GC (issue #38): the delegation paths persist every
	// child snapshot (subagent-*/parallel-*/team-* ids) so InspectSubagent/
	// InspectMember/resume: work, but nothing ever deleted them — a durable store
	// grew without bound. startChildGC sweeps them through the OPTIONAL
	// port.PrunableStore seam: an age pass (delete child snapshots whose
	// last-modified time is older than ChildRetention; 0 disables) then a
	// per-family count cap (the newest ChildRetentionMaxPerFamily per prefix
	// family survive, oldest-first past it deleted; 0 disables), skipping ids
	// with an in-flight run. UNPREFIXED (operator/service) sessions are NEVER
	// touched. ChildGCInterval is the sweep cadence after the startup sweep
	// (0 = startup-only). Both knobs zero = fully disabled (the zero-config
	// default; mecated's flags default to 168h/500/1h). A non-prunable store
	// (e.g. a thin remote driver) is never swept — a no-op with one INFO.
	ChildRetention             time.Duration
	ChildRetentionMaxPerFamily int
	ChildGCInterval            time.Duration

	// Remote store drivers (Phase B): gRPC driver endpoints that replace the
	// LOCAL session/memory stores with internal/adapter/grpcdriver clients.
	// SessionStoreURL is mutually exclusive with StoreDir, MemoryStoreURL with
	// MemoryDir (validateDriverConfig, fatal at the top of Build). All-empty
	// keeps today's behaviour byte-identical. The Driver* auth/TLS fields apply
	// to EVERY driver connection (equal URLs share one ClientConn via the
	// build-scoped driverConns cache): DriverAuthToken is a bearer token
	// (loopback may ride plaintext; a non-loopback target demands DriverTLS or
	// the dial refuses), DriverTLS enables transport TLS with the optional
	// DriverTLSCA bundle and DriverTLSCert/DriverTLSKey mTLS client pair. The
	// user-model store stays LOCAL in Phase B (a deliberate deferral; see
	// docs/design/IMPLEMENTATION-NOTES.md).
	//
	// Phase C1 adds the content-source drivers: SkillSourceURL replaces the
	// LOCAL skills discovery (mutually exclusive with SkillsDirs/
	// SkillsConventional — one source per seam) with a
	// mecatl.driver.v1.SkillSourceService client; the driver's skill bundles
	// serve the same Skill tool, with auxiliary payloads materialized lazily
	// into a build-scoped asset cache on first activation. SoulSourceURL
	// replaces the LOCAL user-scoped soul file (mutually exclusive with
	// SoulPath; --no-soul still wins) with a mecatl.driver.v1.SoulSourceService
	// client occupying the USER slot of the soul selection precedence. Both are
	// probed at build (fatal on an unreachable driver — loud-misconfig); both
	// share the same Driver* auth/TLS posture and per-target connection cache.
	// Phase C2 adds the remaining content-source drivers: AgentSourceURL
	// replaces the LOCAL agent-definition discovery (mutually exclusive with
	// AgentsDirs; the default-true AgentsConventional is simply SUPERSEDED —
	// the driver branch constructs no conventional sources and narrates the
	// supersession) with a mecatl.driver.v1.AgentSourceService client whose
	// snapshot is taken ONCE at build (fatal if unreachable — defs bake
	// per-def child engines, the skills posture). CommandSourceURL COMPOSES
	// (no exclusivity): the driver's slash commands are layered AFTER the
	// file-backed commands and BEFORE MCP prompts (file commands shadow a
	// same-named driver command), consulted LIVE per expansion/listing;
	// probed once at build (fatal if unreachable), runtime faults fail soft.
	SessionStoreURL  string
	MemoryStoreURL   string
	SkillSourceURL   string
	SoulSourceURL    string
	AgentSourceURL   string
	CommandSourceURL string
	DriverAuthToken  string
	DriverTLS        bool
	DriverTLSCA      string
	DriverTLSCert    string
	DriverTLSKey     string

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
	// sources means Subagent keeps only the default explorer (no behaviour change).
	AgentsDirs         []string
	AgentsConventional bool

	// SubagentModel is the global default model for EVERY child engine that does
	// not pin its own model (the analogue of CLAUDE_CODE_SUBAGENT_MODEL): the
	// def-resolved Subagent specialists AND (issue #35) the default Subagent
	// explorer, undefined team members (lead included — lead-strong split
	// deferred), and Parallel BRANCH children. The Parallel JUDGE deliberately
	// stays on the session model. Resolution precedence is:
	// per-call/def model > SubagentModel > parent (session) Model. Same-provider
	// only: the id is resolved on the parent's provider (a def's `provider:` is
	// the cross-provider seam). Empty disables the override. It is resolved (with
	// ModelAliases) ONLY in this composition layer; Build normalizes it once
	// (normalizeSubagentModel) and FAILS FAST: a non-empty value that does not
	// resolve to a usable model id (unknown alias, or an alias meaning inherit —
	// the built-in sonnet/opus/haiku unless overridden) is a Build ERROR, never a
	// silent no-op. EXCLUSION: the user-model review engine
	// (buildUserModelReviewEngine) stays on cfg.Model — it is a Stop-REVIEW hook
	// engine, not a delegation child.
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
	EnableParallel bool

	// ForkPreservedCap bounds how many PRESERVED winner forks (join=first /
	// join=judge) survive at once across the process: a new winner beyond the cap
	// LRU-reaps the oldest preserved fork. Zero uses agent.DefaultPreservedForkCap.
	// Preserved forks remain the deliverable — they are inspectable/mergeable — but
	// are capped so many Parallel calls cannot grow disk without bound.
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
	// built-in mutate-ask floor AND the built-in substitution Ask floor (via
	// WithLooseSubstitution); a Deny in any scope and any CONFIGURED Ask still win
	// (see docs/design/ALLOW-ALL-POSTURE.md). Children are already allow-all.
	AllowAllTools bool

	// Interactive reports whether a HUMAN approver is attached to the main engine's
	// runs (a live Converse / HTTP-SSE client that can answer a permission ask). It is
	// threaded onto the MAIN engine's agent.Deps.Interactive (per session, via
	// engineDepsForProvider) so a SUBAGENT's permission ask that A1/A2 did not
	// auto-resolve can be SURFACED to the human (interactive) instead of auto-denied
	// (headless). DEFAULT false (fail-safe): a daemon launched without a known approver
	// auto-denies subagent asks rather than parking them forever. cmd/mecated sets it
	// true (the bidi/HTTP surfaces have a client); the offline demo leaves it false.
	Interactive bool

	// Observability relays, injected by the caller (mecated wires telemetry; the
	// embedded TUI server leaves both nil). The engine nil-guards each.
	Sink             port.EventSink
	ToolCallRecorder port.ToolCallRecorder

	// MetricsRoleScoper, when non-nil, supplies the role-scoped telemetry pair a
	// CHILD engine's Deps.Sink/Deps.ToolCallRecorder are wired to (issue #47). The
	// caller (cmd/mecated, the embedded TUI server) builds the closure over the
	// telemetry adapter's Metrics.WithRole — keeping internal/app free of the
	// telemetry import — and the child deps builders invoke it with the BOUNDED
	// family value from roleFamily (never the raw engine role), so every child
	// series carries a closed-set role label and no def/member name or session id
	// can leak into metric cardinality. Nil (the default, and the no-perf path)
	// keeps children unmetered: Sink/ToolCallRecorder stay nil, byte-identical to
	// the pre-feature child shape. The role-tagging is METRICS-ONLY — child
	// Diagnostics and the conversation event stream are unchanged.
	MetricsRoleScoper func(familyRole string) (port.EventSink, port.ToolCallRecorder)

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

	// driverConns is the build-scoped remote-driver connection cache (equal
	// *StoreURL targets share one lazy ClientConn). Build sets it once so the
	// session-store and memory-store dials in one composition share it;
	// helpers called directly by tests get a fresh cache via cfg.drivers().
	// Unexported: an internal composition detail, not an operator knob.
	driverConns *driverConns

	// commandSource is the build-once slash-command driver client, stashed by
	// Build after the one dial + Probe (the driverConns precedent):
	// buildCommandExpander runs PER SESSION, so it must compose the
	// already-probed source rather than re-dialling/re-probing per session.
	// nil when CommandSourceURL is unset. Unexported: an internal composition
	// detail, not an operator knob.
	commandSource prompt.CommandSource
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
	// Remote store drivers (Phase B): a local dir and a driver URL for the same
	// store are mutually exclusive — fatal here, before anything is constructed
	// (the validateSkillDraftConfig precedent).
	if err := validateDriverConfig(cfg); err != nil {
		return nil, err
	}
	// Build-scoped driver connection cache: set once so the session-store and
	// memory-store dials below share one ClientConn per distinct target.
	if cfg.driverConns == nil {
		cfg.driverConns = newDriverConns()
	}
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
	// SubagentModel (issue #35): validate + resolve the alias ONCE here — FAIL-FAST
	// on a value that doesn't resolve to a usable model id (the --agent-source-url
	// loud-misconfig posture; warn-and-inert would silently run the whole child
	// fleet on the expensive parent model) — and narrate the ACTIVE child-default
	// model as a build-once fact. A valid alias / literal id is kept verbatim
	// (per-child resolveModelFor re-resolves it cheaply and silently). cfg is a
	// local value, so the normalization propagates to every downstream consumer.
	subagentModel, err := normalizeSubagentModel(cfg)
	if err != nil {
		return nil, err
	}
	cfg.SubagentModel = subagentModel
	// Emit the build-once composition facts (token counter / compaction strategy /
	// slash commands) EXACTLY ONCE here, through the injected Diagnostics — keyed to
	// the resolved MAIN model. The per-derivation builders no longer log these (they
	// run per session AND per child engine); relocating the emit here makes operators
	// see each fact once instead of N times. Other slog sites in this file are not
	// yet relocated (iteration 2).
	logBuildConfigFacts(cfg)

	// Slash-command driver source (Phase C2): ONE dial + Probe at build time
	// (fatal on a fault — loud-misconfig posture), then the probed client is
	// STASHED on the unexported cfg.commandSource so buildCommandExpander —
	// which runs per session — composes it without re-dialling or re-probing
	// (the driverConns precedent). Runtime faults stay fail-soft inside the
	// client. The once-guarded conn close folds into closeAll below.
	commandConnClose := func() {}
	if cfg.CommandSourceURL != "" {
		conn, connClose, derr := cfg.drivers().dial(cfg, cfg.CommandSourceURL)
		if derr != nil {
			return nil, fmt.Errorf("dial command-source driver %q: %w", cfg.CommandSourceURL, derr)
		}
		cmdSrc := grpcdriver.NewCommandSource(conn, grpcdriver.CommandOptions{Diagnostics: cfg.diag()})
		if perr := cmdSrc.Probe(ctx); perr != nil {
			connClose()
			return nil, fmt.Errorf("probe command-source driver %q: %w", cfg.CommandSourceURL, perr)
		}
		cfg.commandSource = cmdSrc
		commandConnClose = connClose
	}

	store, storeClose, err := buildStore(cfg)
	if err != nil {
		commandConnClose()
		return nil, err
	}
	// Agent seam (Phase C2): resolve the agent-definition registry EXACTLY
	// ONCE for the whole composition — the build-time catalog's Subagent/Team
	// tools, the per-session engine factory, the ListAgents snapshot, and the
	// gRPC team wiring all consume THIS one registry (the C1 skillIdx hoist
	// pattern; previously three independent resolutions, the per-session-drift
	// class). The driver branch's once-guarded conn close folds into closeAll.
	agentReg, agentClose, err := resolveAgentSeam(ctx, cfg)
	if err != nil {
		storeClose()
		commandConnClose()
		return nil, err
	}
	if agentClose == nil {
		agentClose = func() {}
	}
	engine, mainMgr, mcpProvider, mcpInventory, sessFactory, learned, assets, mcpClose, err := buildEngine(ctx, cfg, reg, provider, store, agentReg)
	if err != nil {
		agentClose()
		storeClose()
		commandConnClose()
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
		Workspaces:    osfsWorkspaceFactory(cfg.diag(), assets.skillReadRoots),
		DefaultLimits: defaultLimits(),
		MCPProvider:   mcpProvider,
		MCPSources:    mcpInventory,
		// Live re-probe: ListMcpSources re-consults the resolved sources on each call
		// so a TUI panel refresh (ctrl+o → ctrl+r) reflects CURRENT source status,
		// not just this startup snapshot. nil when MCP is unconfigured (keeps the
		// empty snapshot). Resolution is idempotent + read-only, like the agent
		// registry re-resolution below.
		MCPSourceProber: mcpSourceProber(cfg),
		// ListAgents snapshot: project the ONE registry resolved by
		// resolveAgentSeam above (never a second resolution — the per-session
		// drift class) into the proto form; the snapshot stays a pure read at
		// request time.
		Agents: agentSnapshot(cfg, agentReg),
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
		// DefaultResolvedModel: the EFFECTIVE provider+model the DEFAULT/shared engine
		// resolved to (the registry default provider + the already-resolved cfg.Model +
		// the catalog-seed context window for that pair), computed ONCE here in
		// composition. Same single-source discipline as DefaultCapabilities above: the
		// server echoes it verbatim on resolved_model for a session that uses no
		// per-session engine, so nobody recomputes the resolution in a handler. cfg.Model
		// was resolved just above (reg.DefaultModel() when no --model); the catalog window
		// matches the ListModels-advertised context_limit. (multi-provider Phase 0.)
		DefaultResolvedModel: server.ResolvedModel{
			ProviderID:    reg.Default(),
			ModelID:       cfg.Model,
			ContextWindow: int64(catalogContextWindow(reg.Default(), cfg.Model)),
		},
		// ListSkills snapshot: the skills resolved once at build time (the skills
		// seam — FS or driver), projected into the proto form (metadata only).
		// Skills are immutable for the process lifetime, so this is a startup
		// snapshot (like Agents), not a live lister. nil/empty when skills are
		// disabled.
		Skills: skillSnapshot(skillValues(assets.skills, assets.skillIndex)),
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
		UserModel: userModelLister(assets.userModelStore),
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
	applyTeamConfig(&svcCfg, cfg, reg, provider, mainMgr, agentReg, assets.skillReadRoots, assets.skillIndex)

	svc, err := server.NewService(svcCfg)
	if err != nil {
		mcpClose()
		agentClose()
		storeClose()
		commandConnClose()
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

	// Child-session retention GC (issue #38): wired AFTER the Service exists
	// because the sweep's liveness predicate is the Service's in-flight run
	// registry. No-op (one INFO) when the policy is disabled or the store is
	// not prunable; otherwise a startup sweep + ticker sharing ctx (the
	// startMemoryConsolidation lifetime — the goroutine exits on shutdown).
	startChildGC(ctx, cfg, store, svc.IsLive)

	// Close tears down the main MCP manager AND any per-session client-MCP engines
	// still registered (svc.Close), so a process exit leaks neither. It also cancels
	// the live-model refresh goroutine and closes the session-store driver
	// connection (LAST — everything before it may still persist; a no-op for the
	// local stores, and once-guarded if the memory driver shares the conn).
	closeAll := func() {
		refreshClose()
		svc.Close()
		mcpClose()
		agentClose()
		storeClose()
		commandConnClose()
	}
	return &Built{Service: svc, Close: closeAll}, nil
}

// resolveAgentSeam resolves the agent-definition registry from cfg: the
// remote-driver branch when AgentSourceURL is set (fatal on an unreachable
// driver — an explicit operator config that cannot answer is a
// misconfiguration, the skill-driver posture; ONE ListAgentDefs snapshot, the
// build-once semantics per-def child engines depend on), else the filesystem
// branch (resolveAgentRegistry — fail-soft, narration unchanged). The
// returned close is the driver branch's once-guarded conn close (nil for the
// FS branch); Build folds it into closeAll.
func resolveAgentSeam(ctx context.Context, cfg Config) (*agents.Registry, func(), error) {
	if cfg.AgentSourceURL == "" {
		return resolveAgentRegistry(ctx, cfg), nil, nil
	}
	// Conventional discovery is default-ON (and inert without dirs), so the
	// driver branch is NOT an exclusivity fatal against it — the driver simply
	// supersedes it (no conventional source is constructed). Explicit
	// --agents-dir IS exclusive (validateDriverConfig, fatal before Build gets
	// here). Narrate the supersession so an operator with real conventional
	// dirs understands where their defs went.
	if cfg.AgentsConventional {
		cfg.diag().Log(ctx, port.LevelInfo, "agent definitions: conventional discovery superseded by --agent-source-url",
			"target", cfg.AgentSourceURL)
	}
	conn, connClose, err := cfg.drivers().dial(cfg, cfg.AgentSourceURL)
	if err != nil {
		return nil, nil, fmt.Errorf("dial agent-source driver %q: %w", cfg.AgentSourceURL, err)
	}
	src := grpcdriver.NewAgentSource(conn, grpcdriver.AgentOptions{Diagnostics: cfg.diag()})
	defs, err := src.ListAgentDefs(ctx)
	if err != nil {
		connClose()
		return nil, nil, fmt.Errorf("list agent definitions from driver %q: %w", cfg.AgentSourceURL, err)
	}
	if len(defs) == 0 {
		cfg.diag().Log(ctx, port.LevelInfo, "agent definitions DISABLED (agent-source driver serves no defs)",
			"target", cfg.AgentSourceURL)
		return agents.NewRegistry(nil), connClose, nil
	}
	discovered := make([]agents.Discovered, len(defs))
	names := make([]string, 0, len(defs))
	for i, d := range defs {
		// The adapter-private detail channel for a driver def names the driver
		// target, never a path (the driver's storage is its private business).
		discovered[i] = agents.Discovered{Def: d, Detail: "driver: " + cfg.AgentSourceURL}
		names = append(names, d.Name)
		// Make the SHELL capability visible once at build: a def's hooks run
		// through hookexec on the HARNESS host (every scoped lifecycle phase,
		// no permission ask), so a driver-sourced def carrying hooks is the
		// driver exercising harness-side shell. Names only — never hook values.
		if len(d.Hooks) > 0 {
			cfg.diag().Log(ctx, port.LevelInfo, "agent def carries lifecycle hooks (harness-side shell)",
				"agent", d.Name)
		}
	}
	reg := agents.NewRegistryDiscovered(discovered)
	cfg.diag().Log(ctx, port.LevelInfo, "agent definitions ENABLED",
		"target", cfg.AgentSourceURL, "count", reg.Len(), "agents", strings.Join(names, ","))
	return reg, connClose, nil
}

// sessionEngineFactory returns the server.SessionEngineFactory that builds a
// PER-SESSION engine over an optional non-default provider/model SELECTOR
// (multi-provider Phase 0, S3) AND/OR the client-provided streaming-HTTP MCP
// servers (the ACP session/new mcpServers). The two inputs are orthogonal: a
// session with BOTH a non-default model and client MCP gets ONE engine over ONE
// catalog from a single call. Each call connects a SCOPED mcp.NewManager for that
// one session when specs are present (best-effort: a down server is logged-and-
// skipped, never fatal), and assembles a fresh catalog through assembleCatalog —
// the SAME assembly the build-time shared catalog goes through (issue #42), over
// the SAME process-wide assets: the core tools, the SERVER-GLOBAL MCP tools (+
// resource meta-tools) reused from Build's already-connected shared manager (NOT
// reconnected, and NOT in the per-session closeFn), the client MCP tools, the
// Subagent/InspectSubagent/SubagentStatus trio, Parallel, Team/InspectMember, the
// six memory/user-model tools over the shared flocked stores, and Skill/SkillDraft.
// The ONLY sanctioned deltas vs the shared catalog are the client MCP tools and
// the unwrapped hooks (maybeWrapUserModelReview is main-engine-only). It builds an
// engine whose every NON-provider collaborator MATCHES the main engine via
// engineDepsForProvider (so a per-session engine compacts, expands commands,
// persists, and emits telemetry exactly like the shared one — only the catalog and
// the resolved provider/model differ). assets.globalMgr is also the
// `reference:`-resolution mainMgr for per-session Subagent/Team subagent defs
// (falling back to the client mgr when there is no global manager), parity with
// the build-time path.
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
// provider (the zero-selector fallback), and the build-once catalogAssets (the
// SHARED global MCP manager, agent registry, flocked memory/user-model stores,
// resolved skills, and process-wide fork reaper — all owned by Build), so the two
// engines cannot drift on their shared Deps or their toolset. It returns the
// engine and a Close that tears down ONLY this session's own MCP connections
// (client specs + per-def inline managers) — the shared assets.globalMgr is NEVER
// in that Close. Wired into server.Config.SessionEngine in Build, so neither the
// registry nor mcp/agent wiring leaks into the server or acp layers.
func sessionEngineFactory(
	cfg Config,
	reg *providerRegistry,
	provider port.LLMProvider,
	store port.SessionStore,
	policy port.PermissionPolicy,
	hooks port.HookRunner,
	mcpProvider mcp.Provider,
	instructions prompt.InstructionAssembler,
	assets catalogAssets,
) server.SessionEngineFactory {
	return func(ctx context.Context, sel server.ProviderSelector, specs []mcp.ServerConfig) (server.SessionEngineResult, error) {
		// Resolve the provider/model selector FIRST (before any MCP connect), so an
		// unknown provider fails fast without a wasted connection. The zero selector
		// keeps the default provider + cfg.Model (pre-S3 behaviour). resolvedProviderID
		// is threaded so the per-session capability intersection (modelCapability) keys
		// on the right provider — the zero selector uses the registry default.
		resolvedProvider, resolvedModel := provider, cfg.Model
		resolvedProviderID := reg.Default()
		// Seed the window from the catalog for the DEFAULT provider+model too, so a
		// zero-selector session that only needs a per-session engine because client MCP
		// specs are attached reports the SAME ResolvedModel.ContextWindow as
		// Config.DefaultResolvedModel (which carries catalogContextWindow(reg.Default(),
		// cfg.Model)) — the single-source value must not diverge on whether MCP is
		// present. A non-default selector overrides this below from the catalog for the
		// selected (provider, model). 0 still falls back to the 128k compaction default
		// in engineDepsForProvider regardless.
		contextWindow := catalogContextWindow(reg.Default(), cfg.Model)
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

		// Assemble the per-session catalog through the SAME assembleCatalog the
		// build-time shared catalog uses (issue #42 — the anti-drift seam): core +
		// server-global MCP (+ resource meta-tools) + client MCP + Subagent trio +
		// Parallel + Team + memory/user-model + Skill/SkillDraft, with THIS session's
		// resolved (provider, providerID, model) as the inherited sub-agent parent.
		// narrate=false keeps the build-once narration quiet on this per-session path.
		//
		// The returned close tears down ONLY this session's own connections (the
		// Subagent per-def inline managers + the client mgr); assets.globalMgr is
		// NEVER in it — Build owns its lifecycle (a per-session CloseSession must
		// never tear down MCP for every other session).
		cat, closeFn := assembleCatalog(ctx, cfg, reg, store, hooks, assets, catalogSession{
			provider:   resolvedProvider,
			providerID: resolvedProviderID,
			model:      resolvedModel,
			clientMgr:  mgr,
			narrate:    false,
		})

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
			// The EFFECTIVE provider+model the session resolved to, taken from the SAME
			// resolved locals that built the engine above (resolvedProviderID/resolvedModel/
			// contextWindow) — NOT recomputed. The server echoes these verbatim on
			// resolved_model, the SAME composition single-source rule as the capability
			// intersection (see internal/app/capability.go modelCapability). contextWindow
			// is seeded from the catalog even on the zero selector (above), so a default
			// MCP-only session reports the SAME window as Config.DefaultResolvedModel.
			ProviderID:    resolvedProviderID,
			ModelID:       resolvedModel,
			ContextWindow: int64(contextWindow),
			Close:         closeFn,
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

// buildStore constructs the SessionStore: a gRPC driver client when
// SessionStoreURL is set (validateDriverConfig has already rejected the
// URL+dir combination), a JSONL replay store under StoreDir, or the in-memory
// store when both are empty. The returned close func releases the driver
// connection (a no-op for the local stores) and chains into Build's closeAll;
// it is always non-nil on success.
func buildStore(cfg Config) (port.SessionStore, func(), error) {
	if cfg.SessionStoreURL != "" {
		conn, closeFn, err := cfg.drivers().dial(cfg, cfg.SessionStoreURL)
		if err != nil {
			return nil, nil, fmt.Errorf("dial session-store driver %q: %w", cfg.SessionStoreURL, err)
		}
		cfg.diag().Log(context.Background(), port.LevelInfo, "session store: grpc driver", "target", cfg.SessionStoreURL)
		return grpcdriver.NewSessionStore(conn), closeFn, nil
	}
	if cfg.StoreDir == "" {
		cfg.diag().Log(context.Background(), port.LevelInfo, "session store: in-memory")
		return memstore.New(), func() {}, nil
	}
	st, err := jsonlstore.New(cfg.StoreDir)
	if err != nil {
		return nil, nil, fmt.Errorf("open jsonl store %q: %w", cfg.StoreDir, err)
	}
	cfg.diag().Log(context.Background(), port.LevelInfo, "session store: jsonl", "dir", cfg.StoreDir)
	return st, func() {}, nil
}

// buildEngine assembles the parent agent.Engine: the core tool catalog (plus an
// optional Bash tool and a read-only Subagent tool), the permission policy, hooks,
// prompt config, and the shared provider/store. It also connects any configured
// MCP servers, returning a close func that tears the MCP manager down on shutdown
// (a no-op when no servers are configured), and the per-session client-MCP engine
// factory (built HERE because store/policy/hooks/counter/mcpProvider — the exact
// collaborators a per-session engine must share with the main one — are all in
// scope here, so the factory cannot drift from the main engine's Deps).
//
// agentReg is the ONE agent-definition registry Build resolved via
// resolveAgentSeam (FS or driver) — threaded in, never re-resolved here, so
// every consumer (catalog, per-session factory, snapshot, team wiring) shares
// the same registry.
func buildEngine(ctx context.Context, cfg Config, reg *providerRegistry, provider port.LLMProvider, store port.SessionStore, agentReg *agents.Registry) (*agent.Engine, *mcp.Manager, mcp.Provider, []mcpsource.SourceInfo, server.SessionEngineFactory, *permstore.Memory, catalogAssets, func(), error) {
	// SkillDraft trust boundary: when enabled, the quarantine dir must live OUTSIDE
	// the workspace root (so the model's workspace-confined Write/Edit cannot reach
	// it) and be disjoint from every active skills dir. Fatal on a misconfig.
	if err := validateSkillDraftConfig(cfg); err != nil {
		return nil, nil, nil, nil, nil, nil, catalogAssets{}, func() {}, err
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
		policy = permpolicy.NewPolicyWithResolver(mainRules(cfg), learned, resolver, mainEvaluatorOptions(cfg)...)
	} else {
		policy = permpolicy.NewPolicy(mainRules(cfg), learned, mainEvaluatorOptions(cfg)...)
	}
	hooks := hookexec.New(nil) // no hooks by default; map is the injection seam

	// agentReg (threaded from Build's single resolveAgentSeam) is shared with
	// BOTH the build-time catalog's Subagent/Team tools and the per-session
	// engine factory (Half B builds a per-session Subagent/Team tool over the
	// SAME registry, closed over below) — ONE resolution per process.
	cat, assets, mcpProvider, mcpInventory, mcpClose, err := buildCatalog(ctx, cfg, reg, provider, hooks, agentReg, store)
	if err != nil {
		return nil, nil, nil, nil, nil, nil, catalogAssets{}, func() {}, err
	}
	memStore, userModelStore := assets.memStore, assets.userModelStore

	// Soul driver probe (Phase C1): when --soul-source-url is set (and --no-soul
	// does not win), one LoadSoul round trip at BUILD time — fatal on a fault
	// (loud-misconfig posture: an explicitly configured driver that cannot
	// answer is a misconfiguration; RUNTIME faults stay fail-soft inside the
	// client). The once-guarded conn close folds into the engine teardown chain
	// (shared with any equal-URL driver conn).
	if cfg.SoulSourceURL != "" && !cfg.NoSoul {
		conn, connClose, derr := cfg.drivers().dial(cfg, cfg.SoulSourceURL)
		if derr != nil {
			mcpClose()
			return nil, nil, nil, nil, nil, nil, catalogAssets{}, func() {}, fmt.Errorf("dial soul-source driver %q: %w", cfg.SoulSourceURL, derr)
		}
		probe := grpcdriver.NewSoulSource(conn, grpcdriver.SoulOptions{Diagnostics: cfg.diag()})
		if perr := probe.Probe(ctx); perr != nil {
			connClose()
			mcpClose()
			return nil, nil, nil, nil, nil, nil, catalogAssets{}, func() {}, fmt.Errorf("probe soul-source driver %q: %w", cfg.SoulSourceURL, perr)
		}
		prevClose := mcpClose
		mcpClose = func() {
			prevClose()
			connClose()
		}
	}

	// Soul (issue #14, Phase 1): a user-scoped, agent-READ-ONLY persona source. ON
	// by default reading the conventional ~/.config/mecatl/soul.md; --no-soul leaves
	// it nil (no fragment), --soul-file overrides the path; --soul-source-url
	// (Phase C1) swaps the USER slot for the remote driver. The adapter
	// (*soul.Store or the grpcdriver client) meets the prompt-defined SoulSource
	// port HERE, in the composition layer — the one place the adapter binds the
	// port. A missing file is fail-soft (no-op).
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
	// The factory shares the build-once assets (global MCP manager, agent registry,
	// flocked memory/user-model stores, skills, fork reaper) so every per-session
	// catalog is assembled over the SAME collaborators as the shared one. It keeps
	// the UNWRAPPED hooks: the Phase-2b reviewer fires once per MAIN-engine Stop,
	// not per per-session stop (one of the two sanctioned per-session deltas).
	sessFactory := sessionEngineFactory(cfg, reg, provider, store, policy, hooks, mcpProvider, instructions, assets)
	// The build-once assets travel back to Build whole: it reads assets.skills
	// (ListSkills snapshot), assets.userModelStore (GetUserModel lister), and
	// assets.skillReadRoots (the workspace factory + team fork closures) off the
	// SAME value every catalog assembly shares.
	return agent.NewEngine(deps), assets.globalMgr, mcpProvider, mcpInventory, sessFactory, learned, assets, mcpClose, nil
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
func buildInstructionAssembler(soulSrc prompt.SoulSource, memStore, userModelStore tool.MemoryStore) prompt.InstructionAssembler {
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
// keeps drift + provenance a pure composition concern (engine/prompt stays
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
// an untyped nil tool.MemoryStore when user-model is disabled (--no-user-model),
// when no directory can be resolved, or when the store cannot be opened (all
// fail-soft: the user-model tools/block simply don't appear) — buildSoulSource
// precedent, so the callers' interface-nil checks hold (no typed-nil gotcha;
// guarded by TestBuildUserModelStoreDisabledReturnsNilInterface). It resolves
// UserModelDir when set, else the conventional <xdg>/mecatl/usermodel. It upholds
// the one-Store-per-dir invariant: this is the SOLE construction site for the
// user-model store, distinct from the per-project memory store (different dir),
// so the two never contend on a lock.
func buildUserModelStore(cfg Config) tool.MemoryStore {
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
		Store:            store,
		Sink:             cfg.Sink,
		ToolCallRecorder: cfg.ToolCallRecorder,
		// Clock: the production wall clock (issue #53). Before it was wired here the
		// field was left nil, which silently zeroed EVERY latency observation —
		// EvTurnEnd.DurationMs/TTFT/inter-token and tool queued/took. Children inherit
		// it (childEngineDepsForProvider does not clear it).
		Clock:               wallclock.Clock{},
		Diagnostics:         cfg.diag(),
		PromptConfig:        promptConfig(modelCfg, cfg.gitStatus),
		Model:               model,
		ContextWindowTokens: window,
		CompactionRatio:     defaultCompactionRatio,
		TokenCounter:        counter,
		Compactor:           buildCompactor(modelCfg, provider, counter),
		CommandExpander:     buildCommandExpander(cfg, mcpProvider),
		// No-progress nudge budget: operator-tunable (cfg), inherited by children
		// (childEngineDepsForProvider keeps this field). Zero → NewEngine applies the
		// safe default of 2; negative disables.
		MaxNoProgressNudges: cfg.MaxNoProgressNudges,
		// Token budget: the shared loop-level runaway brake, operator-tunable (cfg) and
		// INHERITED by children (childEngineDepsForProvider, which delegates here, keeps
		// it). 0 disables (behaviour byte-identical to pre-budget).
		MaxRunTokens: cfg.MaxRunTokens,
		// Interactivity: the MAIN engine surfaces a subagent's unresolved permission ask
		// to the human when a client is attached. childEngineDepsForProvider forces this
		// back to false (a child never surfaces further).
		Interactive: cfg.Interactive,
	}
}

// buildCommandExpander selects the slash-command expander for the agent Deps.
// Command expansion is OFF by default (the NoopExpander, leaving raw user text
// untouched). It is turned ON when CommandsDir is set, EnableCommands is true,
// or a slash-command driver source is stashed (cfg.commandSource — dialled and
// probed ONCE in Build, never here: this builder runs per session).
//
// COMPOSITION ORDER (first-that-expands-wins): file-backed commands (dirExp),
// then the driver source (sourceExp), then MCP prompts (mcpExp) — a local
// command file shadows a same-named driver command, and both shadow a
// same-named MCP prompt (the MCP prompt namespace is disjoint anyway, kept
// last as before).
func buildCommandExpander(cfg Config, mcpProvider mcp.Provider) prompt.CommandExpander {
	dirExp := buildDirCommandExpander(cfg)
	var sourceExp prompt.CommandExpander
	if cfg.commandSource != nil {
		sourceExp = prompt.NewSourceExpander(cfg.commandSource)
	}
	mcpExp := buildMCPPromptExpander(cfg, mcpProvider)

	expanders := make([]prompt.CommandExpander, 0, 3)
	for _, e := range []prompt.CommandExpander{dirExp, sourceExp, mcpExp} {
		if e != nil {
			expanders = append(expanders, e)
		}
	}
	switch len(expanders) {
	case 0:
		return prompt.NoopExpander{}
	case 1:
		return expanders[0]
	default:
		return prompt.NewMultiExpander(expanders...)
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
	if cfg.CommandSourceURL != "" {
		// The driver source COMPOSES with (never replaces) the file-backed
		// state slashCommandDecision narrates, so it is a separate fact.
		facts = append(facts, diagFact{
			level: port.LevelInfo,
			msg:   "slash-command driver source ENABLED",
			args:  []any{"target", cfg.CommandSourceURL},
		})
	}
	if subagentShellUntrustedReason(cfg) != "" {
		// The issue-#40 workspace-trust shell gate, narrated ONCE here (the gated
		// builder buildSandboxedCommandRunner runs per session AND per catalog
		// assembly, so it must not log). Emitted only when untrust is the OPERATIVE
		// cause — --no-bash / an empty shell already get their own narration via
		// registerCoreTools.
		facts = append(facts, diagFact{
			level: port.LevelInfo,
			msg: "read-only subagent/team-member shell DISABLED (untrusted workspace): " +
				"a worktree child's shell shares the repo's .git, and a tracked .gitattributes " +
				"in an untrusted repo can name filter/diff drivers that execute code; run with " +
				"--trust-project (or confirm trust in mecatui) to enable the subagent shell",
			args: []any{"workspace", cfg.Workspace},
		})
	}
	for _, f := range facts {
		cfg.diag().Log(context.Background(), f.level, f.msg, f.args...)
	}
}

// normalizeSubagentModel validates and resolves Config.SubagentModel EXACTLY ONCE
// at build time (called only from Build — the build-once composition-facts
// discipline) and emits the one INFO narrating the active child-default model.
// The posture is FAIL-FAST: a non-empty --subagent-model that does not resolve to
// a usable model id is a BUILD ERROR (the --agent-source-url loud-misconfig
// precedent), naming the flag, the value, and why it didn't resolve — never a
// warn-and-inert no-op, which would silently run the whole child fleet on the
// EXPENSIVE parent model (the opposite of the flag's purpose). The two
// dead-selector shapes (lookupModelAlias is the ONE grammar shared with the
// forgiving def path, resolveAlias):
//
//   - an unrecognised BARE token (not an alias, no separator ⇒ not a model id);
//   - an alias resolving to "" / inherit (the built-in sonnet/opus/haiku aliases
//     unless overridden in ModelAliases, or an operator alias mapped to an empty
//     id) — a child default that inherits the parent is a no-op.
//
// A VALID value is returned VERBATIM (not pre-resolved): per-child resolution
// (resolveModelFor) maps a known alias silently, and substituting the resolved id
// here could change behaviour for an alias whose target is itself a bare token.
func normalizeSubagentModel(cfg Config) (string, error) {
	sel := strings.TrimSpace(cfg.SubagentModel)
	if sel == "" {
		return "", nil
	}
	resolved, known := lookupModelAlias(cfg, sel)
	switch {
	case !known:
		return "", fmt.Errorf("--subagent-model %q: unknown model alias (not in --model-alias, not a built-in alias, and a bare token is not a concrete model id); every def-less child would silently run on the parent model — pass a concrete model id or define the alias", sel)
	case resolved == "":
		return "", fmt.Errorf("--subagent-model %q: the alias resolves to \"inherit\" (the built-in sonnet/opus/haiku aliases mean inherit unless overridden via --model-alias), which would make the child-default override a no-op — pass a concrete model id or map the alias to one", sel)
	}
	cfg.diag().Log(context.Background(), port.LevelInfo,
		"subagent default model ACTIVE: def-less Subagent explorer / Parallel-branch / undefined-team-member children run on it (the Parallel judge stays on the session model); a def `model:` or per-call override still wins",
		"model", resolved)
	return sel, nil
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

// buildCatalog is Phase A of catalog construction plus the ONE build-time
// assembly call. Phase A produces the process-wide catalogAssets — it connects
// the server-global MCP manager (connectMCP), opens the flocked memory and
// user-model stores (the SOLE construction sites — one Store per dir), starts
// the build-once consolidation goroutines, and resolves skills. It then runs
// assembleCatalog ONCE with the build-time inputs (default provider/model,
// no client MCP, narrate=true) to produce the shared engine's catalog.
//
// The returned catalogAssets are threaded (via buildEngine) into the per-session
// engine factory, so EVERY per-session catalog is assembled by the SAME
// assembleCatalog over the SAME assets — the issue-#42 anti-drift seam. The
// returned close func tears down the global MCP manager AND the build-time
// Subagent per-def inline managers on shutdown.
//
// The error return exists for the EXPLICITLY-CONFIGURED memory driver only
// (loud-misconfig posture): a --memory-store-url that fails to dial is FATAL,
// unlike the default-on local memory.New whose failure stays fail-soft
// (WARN + tools disabled). On error every connection already made here is
// torn down before returning.
func buildCatalog(ctx context.Context, cfg Config, reg *providerRegistry, provider port.LLMProvider, hooks port.HookRunner, agentReg *agents.Registry, store port.SessionStore) (*tool.Catalog, catalogAssets, mcp.Provider, []mcpsource.SourceInfo, func(), error) {
	// Connect the MAIN MCP servers FIRST, so the per-agent-def Subagent engines built by
	// buildSubagentTool can (a) pull a REFERENCED main server's tools out of this manager
	// and (b) connect their own INLINE servers. mainMgr is nil when no main servers are
	// configured (reference entries then resolve to a clear "unknown server" diagnostic).
	mainMgr, mcpProvider, mcpInventory, mcpClose := connectMCP(ctx, cfg)

	// Per-project memory store: opt-in via MemoryStoreURL (a remote gRPC driver;
	// Phase B) or MemoryDir (the flocked reference adapter, opened ONCE here —
	// one Store per dir) and shared by the build-time catalog, every per-session
	// catalog, the prompt tier-0 index source, and the consolidation goroutine.
	// Typed-nil discipline: memStore is assigned only on a successful
	// construction, so it is either a known-non-nil concrete store or nil. The
	// driver connection's close (once-guarded, possibly shared with the session
	// store) folds into this catalog's returned close func below.
	var memStore tool.MemoryStore
	var memDriverClose func()
	if cfg.MemoryStoreURL != "" {
		conn, closeFn, err := cfg.drivers().dial(cfg, cfg.MemoryStoreURL)
		if err != nil {
			// FATAL, not fail-soft: the driver URL is an EXPLICIT operator
			// config (unlike the default-on local store below) — silently
			// running without the memory backend the operator pointed at would
			// hide a misconfiguration.
			mcpClose()
			return nil, catalogAssets{}, nil, nil, nil, fmt.Errorf("dial memory-store driver %q: %w", cfg.MemoryStoreURL, err)
		}
		memStore = grpcdriver.NewMemoryStore(conn)
		memDriverClose = closeFn
		cfg.diag().Log(ctx, port.LevelInfo, "memory tools ENABLED (Remember/Recall/SearchMemory); permission: allow (built-in default, overridable to ask/deny via settings)", "target", cfg.MemoryStoreURL)
		startMemoryConsolidation(ctx, cfg, memStore, provider)
	} else if cfg.MemoryDir != "" {
		st, err := memory.New(cfg.MemoryDir)
		if err != nil {
			cfg.diag().Log(ctx, port.LevelWarn, "could not open memory store; memory tools disabled", "dir", cfg.MemoryDir, "err", err)
		} else {
			memStore = st
			cfg.diag().Log(ctx, port.LevelInfo, "memory tools ENABLED (Remember/Recall/SearchMemory); permission: allow (built-in default, overridable to ask/deny via settings)", "dir", cfg.MemoryDir)
			startMemoryConsolidation(ctx, cfg, st, provider)
		}
	} else {
		cfg.diag().Log(ctx, port.LevelInfo, "memory tools DISABLED (memory dir empty)")
		if cfg.MemoryConsolidateInterval > 0 {
			cfg.diag().Log(ctx, port.LevelWarn, "memory consolidation interval is a no-op without a memory dir (memory is disabled)",
				"interval", cfg.MemoryConsolidateInterval)
		}
	}

	// User-model store (issue #14, Phase 2a): a SECOND, USER-scoped memory store
	// (cross-project), exposed as RememberUser/RecallUser/SearchUserModel. ON by
	// default reading the conventional <xdg>/mecatl/usermodel; --no-user-model
	// disables it. Carried on the assets so the caller can bind it to the prompt
	// <user-model> source AND (for Phase 2b) to the background reviewer's write
	// tool. It stays nil when disabled or unopenable (fail-soft).
	userModelStore := buildUserModelStore(cfg)
	if userModelStore != nil {
		cfg.diag().Log(ctx, port.LevelInfo, "user-model tools ENABLED (RememberUser/RecallUser/SearchUserModel; cross-project); permission: allow (built-in default, overridable to ask/deny via settings)")
		startUserModelConsolidation(ctx, cfg, userModelStore, provider)
	}

	// The skills seam (Phase C1): FS snapshot or remote driver, resolved once.
	// A driver fault is FATAL (explicit operator config, the memory-driver
	// posture above); the FS branch stays fail-soft.
	seam, err := resolveSkillSeam(ctx, cfg, agentReg)
	if err != nil {
		mcpClose()
		return nil, catalogAssets{}, nil, nil, nil, err
	}

	// ONE process-wide preserved-fork LRU shared by every Parallel tool (build-time
	// AND per-session), so ForkPreservedCap stays a PROCESS bound.
	var forkReaper *agent.LRUForkReaper
	if cfg.EnableParallel {
		forkReaper = agent.NewLRUForkReaper(forkPreservedCap(cfg))
	}

	assets := catalogAssets{
		globalMgr:      mainMgr,
		agentReg:       agentReg,
		memStore:       memStore,
		userModelStore: userModelStore,
		skills:         seam.metas,
		skillActivator: seam.activator,
		skillIndex:     seam.index,
		// The ONE computation of the read-only allowed roots, now derived inside
		// the seam (FSSource.AssetDirs per-skill dirs, or the driver asset cache —
		// the project-tier trust gate is inherited by construction either way) and
		// threaded into every production osfs Workspace constructor via the
		// assets — no second list to drift.
		skillReadRoots: seam.readRoots,
		forkReaper:     forkReaper,
	}
	// The build-time assembly: default provider + model, no client MCP, narrating
	// the ENABLED/DISABLED composition facts exactly once.
	cat, assembledClose := assembleCatalog(ctx, cfg, reg, store, hooks, assets, catalogSession{
		provider:   provider,
		providerID: reg.Default(),
		model:      cfg.Model,
		narrate:    true,
	})
	// Aggregate the build-time Subagent per-def INLINE MCP managers' teardown into
	// the main MCP close, so Built.Close tears them ALL down on shutdown
	// (process-lifetime engines). The global manager itself stays in mcpClose only.
	mcpClose = composeClose(cfg.diag(), assembledClose, mcpClose)
	// Fold the memory-store driver connection's close (when one was dialled) into
	// the same teardown chain. It is once-guarded, so sharing the conn with the
	// session store (equal URLs) cannot double-close.
	if memDriverClose != nil {
		prev := mcpClose
		mcpClose = func() {
			prev()
			memDriverClose()
		}
	}
	// Fold the skill seam's teardown (driver branch only: asset-cache RemoveAll
	// + its once-guarded conn close) into the same chain.
	if seam.close != nil {
		prev := mcpClose
		mcpClose = func() {
			prev()
			seam.close()
		}
	}

	// Restore the pre-#42 advertises-implies-registered coupling: the old code set
	// memStore only AFTER a successful memory.Register, so the turn-0 prompt index
	// could never advertise a memory family the catalog lacked. Registration now
	// happens inside assembleCatalog (which WARNs on the failure), so on that
	// (name-collision, near-impossible) edge we DROP the store from the assets
	// before it reaches buildInstructionAssembler — and before the per-session
	// assemblies inherit it.
	if assets.memStore != nil {
		if _, ok := cat.Lookup(memory.RememberToolName); !ok {
			assets.memStore = nil
		}
	}
	if assets.userModelStore != nil {
		if _, ok := cat.Lookup(memory.RememberUserToolName); !ok {
			assets.userModelStore = nil
		}
	}

	return cat, assets, mcpProvider, mcpInventory, mcpClose, nil
}

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

// connectMCP RESOLVES the MCP server inventory from the pluggable source list
// (static MCPServers entries first, then the live ToolHive workload source when
// ToolHiveEnabled) and connects the merged set. It is non-fatal end to end:
// per-source SkipErrors and per-server connect failures are logged and skipped; a
// manager that fails entirely is logged and skipped.
//
// It only CONNECTS — mounting the manager's tools into a catalog is
// assembleCatalog's job (the same mount the per-session catalogs get). It returns
// the concrete *mcp.Manager (nil when no servers connect) so the per-agent-def
// wiring can pull a REFERENCED main server's tools out of it; the same value is
// the mcp.Provider used for resources/prompts.
func connectMCP(ctx context.Context, cfg Config) (*mcp.Manager, mcp.Provider, []mcpsource.SourceInfo, func()) {
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
	cfg.diag().Log(ctx, port.LevelInfo, "MCP servers connected", "servers", len(configs), "tools", len(mgr.Tools()))

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

// skillSeam is the resolved skills wiring (Phase C1): the build-once products
// every catalog assembly shares, produced by resolveSkillSeam from EITHER the
// filesystem branch (NewFSSource over the trust-gated resolved sources) or the
// remote-driver branch (a grpcdriver SkillSource + lazy asset materializer).
// The PORT (tool.SkillSource) carries logical bundles only; the path business
// an FS deployment still needs (readRoots) derives from the FS adapter's
// NON-PORT AssetDirs, and the driver branch's single read root is its asset
// cache.
type skillSeam struct {
	// metas is the name-sorted always-in-context metadata snapshot the Skill
	// tool enumerates and the ListSkills RPC projects.
	metas []tool.SkillMeta
	// activator loads a skill's body + base directory on activation (snapshot
	// in place for FS; lazy materialization for the driver).
	activator skills.Activator
	// index is the name → body preload map agent definitions' `skills:` lists
	// read (full for FS — bodies are snapshot-retained; LAZY for the driver —
	// only def-referenced names are fetched).
	index skillIndex
	// readRoots are the read-only allowed roots every production osfs Workspace
	// is constructed with: the FS per-skill dirs, or the driver's asset cache.
	readRoots []string
	// close is the driver branch's teardown (asset-cache RemoveAll + the
	// once-guarded conn close); nil for the FS/disabled branches.
	close func()
}

// resolveSkillSeam resolves the skills wiring from cfg: the remote-driver
// branch when SkillSourceURL is set (fatal on an unreachable driver — an
// explicit operator config that cannot answer is a misconfiguration, the
// memory-driver posture), else the filesystem branch (fail-soft, narration
// verbatim from the pre-seam resolveSkills). agentReg feeds the driver
// branch's LAZY preload index (only def-referenced skill bodies transfer).
func resolveSkillSeam(ctx context.Context, cfg Config, agentReg *agents.Registry) (skillSeam, error) {
	if cfg.SkillSourceURL != "" {
		return resolveDriverSkillSeam(ctx, cfg, agentReg)
	}
	return resolveFSSkillSeam(ctx, cfg), nil
}

// resolveFSSkillSeam is the filesystem branch: DISCOVER the
// progressive-disclosure skills from the resolved Source list (explicit dirs +
// conventional locations when enabled) into an FSSource snapshot. Skills stay
// OPT-IN: with no sources, nothing is discovered. Registration of the Skill
// tool over the seam is assembleCatalog's job (so per-session catalogs get it
// too); this is the build-once discovery + narration half. The zero seam means
// no skills (disabled, no sources, none valid, or a discovery fault — a fault
// drops the WHOLE inventory, no partials; the WARN names the failure).
func resolveFSSkillSeam(ctx context.Context, cfg Config) skillSeam {
	sources := skills.ResolveSources(skillResolveOptions(cfg))
	if len(sources) == 0 {
		cfg.diag().Log(ctx, port.LevelInfo, "skills DISABLED (no skills dirs configured)")
		return skillSeam{}
	}

	src, skips, err := skills.NewFSSource(ctx, sources...)
	for _, s := range skips {
		cfg.diag().Log(ctx, port.LevelWarn, "skill skipped", "path", s.Path, "reason", s.Reason)
	}
	if err != nil {
		cfg.diag().Log(ctx, port.LevelWarn, "discovering skills failed; Skill tool disabled",
			"dirs", strings.Join(cfg.SkillsDirs, ","), "conventional", cfg.SkillsConventional, "err", err)
		return skillSeam{}
	}
	discovered := src.Discovered()
	if len(discovered) == 0 {
		cfg.diag().Log(ctx, port.LevelInfo, "skills DISABLED (no valid SKILL.md found in any source)",
			"dirs", strings.Join(cfg.SkillsDirs, ","), "conventional", cfg.SkillsConventional)
		return skillSeam{}
	}
	names := make([]string, 0, len(discovered))
	idx := make(skillIndex, len(discovered))
	for _, s := range discovered {
		names = append(names, s.Name)
		idx[s.Name] = s.Body
	}
	cfg.diag().Log(ctx, port.LevelInfo, "Skill tool ENABLED",
		"dirs", strings.Join(cfg.SkillsDirs, ","), "conventional", cfg.SkillsConventional,
		"count", len(discovered), "skills", strings.Join(names, ","))
	metas, _ := src.ListSkills(ctx) // snapshot read; never errors
	return skillSeam{
		metas:     metas,
		activator: skills.NewSnapshotActivator(src),
		index:     idx,
		// The ONE computation of the per-skill read-only allowed roots, moved
		// home to the FS adapter (FSSource.AssetDirs): derived from the SAME
		// snapshot (so the project-tier trust gate is inherited by construction)
		// and threaded into every production osfs Workspace constructor via the
		// assets — no second list to drift.
		readRoots: src.AssetDirs(),
	}
}

// resolveDriverSkillSeam is the remote-driver branch: dial the
// SkillSourceService (fatal on a dial/snapshot fault), take ONE ListSkills
// snapshot, create the build-scoped asset cache EAGERLY (the osfs Workspace
// opens its read roots at construction and skips non-existent dirs — a
// late-born cache would be unreadable), and wire the lazy materializer behind
// a SourceActivator. The cache RemoveAll + the once-guarded conn close ride
// the seam's close, folded into the catalog teardown.
func resolveDriverSkillSeam(ctx context.Context, cfg Config, agentReg *agents.Registry) (skillSeam, error) {
	conn, connClose, err := cfg.drivers().dial(cfg, cfg.SkillSourceURL)
	if err != nil {
		return skillSeam{}, fmt.Errorf("dial skill-source driver %q: %w", cfg.SkillSourceURL, err)
	}
	src := grpcdriver.NewSkillSource(conn)
	metas, err := src.ListSkills(ctx)
	if err != nil {
		connClose()
		return skillSeam{}, fmt.Errorf("list skills from driver %q: %w", cfg.SkillSourceURL, err)
	}
	if len(metas) == 0 {
		cfg.diag().Log(ctx, port.LevelInfo, "skills DISABLED (skill-source driver serves no skills)",
			"target", cfg.SkillSourceURL)
		return skillSeam{close: connClose}, nil
	}

	rawCache, err := os.MkdirTemp("", "mecatl-skill-assets-")
	if err != nil {
		connClose()
		return skillSeam{}, fmt.Errorf("create skill asset cache: %w", err)
	}
	// Canonicalize the cache root through the SAME resolver the osfs read-root
	// allowlist is keyed on (a temp dir may live behind a symlinked prefix), so
	// the Base-directory paths the Skill tool advertises match the allowlist.
	cacheBase := rawCache
	if resolved, rerr := osfs.ResolveRoot(rawCache); rerr == nil {
		cacheBase = resolved
	}

	names := make([]string, 0, len(metas))
	for _, m := range metas {
		names = append(names, m.Name)
	}
	cfg.diag().Log(ctx, port.LevelInfo, "Skill tool ENABLED",
		"target", cfg.SkillSourceURL, "count", len(metas), "skills", strings.Join(names, ","),
		"asset_cache", cacheBase)

	return skillSeam{
		metas:     metas,
		activator: skills.NewSourceActivator(src, skills.NewAssetMaterializer(src, cacheBase)),
		index:     driverSkillIndex(ctx, cfg, src, metas, agentReg),
		readRoots: []string{cacheBase},
		close: func() {
			if rerr := os.RemoveAll(rawCache); rerr != nil {
				cfg.diag().Log(context.Background(), port.LevelWarn, "removing the skill asset cache failed", "dir", rawCache, "err", rerr)
			}
			connClose()
		},
	}, nil
}

// driverSkillIndex builds the agent-def preload index LAZILY over the driver:
// only the skill names some agent definition actually references are fetched
// (a def-less deployment transfers zero bodies). It is forgiving — a fetch
// fault drops that one name with a WARN (the def's preload then no-ops with a
// "missing" diagnostic), mirroring the pre-seam resolveSkillIndex posture.
func driverSkillIndex(ctx context.Context, cfg Config, src tool.SkillSource, metas []tool.SkillMeta, agentReg *agents.Registry) skillIndex {
	if agentReg == nil || agentReg.Len() == 0 {
		return nil
	}
	known := make(map[string]bool, len(metas))
	for _, m := range metas {
		known[m.Name] = true
	}
	idx := skillIndex{}
	for _, def := range agentReg.List() {
		for _, name := range def.Skills {
			name = strings.TrimSpace(name)
			if name == "" || !known[name] {
				continue
			}
			if _, done := idx[name]; done {
				continue
			}
			body, err := src.SkillBody(ctx, name)
			if err != nil {
				cfg.diag().Log(ctx, port.LevelWarn, "preloading a def-referenced skill body from the driver failed; the def's preload will report it missing",
					"skill", name, "err", err)
				continue
			}
			idx[name] = body
		}
	}
	if len(idx) == 0 {
		return nil
	}
	return idx
}

// skillValues projects the seam's metas + preload bodies back onto the adapter
// []skills.Skill value shape for the two legacy consumers that still take it:
// the ListSkills snapshot projection (skillSnapshot — metadata only) and the
// SkillDraft novelty input (NewDirDrafter — name/description/body). No path
// crosses: the projection carries none.
func skillValues(metas []tool.SkillMeta, idx skillIndex) []skills.Skill {
	if len(metas) == 0 {
		return nil
	}
	out := make([]skills.Skill, 0, len(metas))
	for _, m := range metas {
		out = append(out, skills.Skill{Name: m.Name, Description: m.Description, Body: idx[m.Name], Origin: m.Origin})
	}
	return out
}

// registerSkillDraft registers the writable SkillDraft tool when SkillsDraftDir is
// set, binding a DirDrafter to the quarantine dir and the snapshot of currently
// active skills (for the offline novelty check). The structural trust boundary is
// enforced by validateSkillDraftConfig at engine-build time. It narrates the
// ENABLED/DISABLED fact only when log is true (the build-time assembly), matching
// registerCoreTools' discipline — the per-session assembly stays quiet.
func registerSkillDraft(ctx context.Context, cfg Config, cat *tool.Catalog, existing []skills.Skill, log bool) {
	if cfg.SkillsDraftDir == "" {
		if log {
			cfg.diag().Log(ctx, port.LevelInfo, "SkillDraft tool DISABLED (no skills-draft dir)")
		}
		return
	}
	drafter := skills.NewDirDrafter(cfg.SkillsDraftDir, existing,
		skills.WithSimilarityThreshold(cfg.SkillsDraftThreshold))
	cat.MustRegister(skills.NewDraftTool(drafter))
	if log {
		cfg.diag().Log(ctx, port.LevelInfo, "SkillDraft tool ENABLED (model-authored skills -> quarantine -> operator promote)",
			"quarantine", cfg.SkillsDraftDir, "similarity_threshold", cfg.SkillsDraftThreshold,
			"snapshot_skills", len(existing))
	}
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
func maybeWrapUserModelReview(cfg Config, hooks port.HookRunner, store port.SessionStore, provider port.LLMProvider, userModelStore tool.MemoryStore) port.HookRunner {
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
// no Subagent/Fork). RememberUser carries the write-time injection scan, so a
// transcript-poisoning attempt cannot land in the user-model block.
//
// MODEL: deliberately cfg.Model, NOT Config.SubagentModel — this is a REVIEW hook
// engine (the Stop-triggered background reviewer of the finished session), not a
// delegation child, so the cheap child-default does not apply to it.
func buildUserModelReviewEngine(cfg Config, provider port.LLMProvider, store tool.MemoryStore) *agent.Engine {
	cat := tool.NewCatalog()
	for _, t := range memory.NewUserModelTools(store) {
		if t.Spec().Name == memory.RememberUserToolName {
			cat.MustRegister(t)
		}
	}
	return newChildEngine(cfg, "usermodel-review", provider, cat, cfg.Model, promptConfig(cfg, cfg.gitStatus))
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
// RESIDUAL — a fixed-key env override does NOT close git driver configs whose driver
// NAME is attacker-chosen in a tracked `.gitattributes`: filter.<drv>.smudge (fires at
// worktree checkout / fork time) and diff.<drv>.textconv (fires on `git show` /
// `git log -p`), plus alias.<name>=!sh if the member invokes that alias by name — an
// arbitrary driver name cannot be pinned to an inert value. These are reachable only
// when the shared `.git` is an UNTRUSTED repo; for a TRUSTED repo this is equivalent to
// the operator running git themselves. The robust mitigation is the WORKSPACE-TRUST
// GATE below — now implemented (issue #40): an untrusted workspace gets NO worktree
// subagent/member shell at all (the early nil return on !cfg.TrustProject), so the
// attacker-named driver vectors are unreachable; the env scrub remains the
// defence-in-depth layer for the trusted case.
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
	// WORKSPACE-TRUST GATE (issue #40): a worktree-isolated child's shell shares the
	// base repo's `.git`, and an UNTRUSTED repo's tracked `.gitattributes` can name
	// filter/diff drivers that execute code the moment the child runs git — a vector
	// the fixed-key env scrub structurally cannot close. So an untrusted workspace
	// gets NO read-only subagent/member shell (the loop degrades to Read/Grep/Glob —
	// "ask the human" posture, not "do nothing"). The decision is NOT logged here —
	// this builder runs per session/per assembly; the build-once INFO is emitted in
	// logBuildConfigFacts.
	if !cfg.TrustProject {
		return nil
	}
	return newHardenedCommandRunner(cfg)
}

// buildForceCopyRunner builds the command runner FORCE-COPY-fork children — MUTATING
// team members and Parallel branches — execute Bash against: the same hardened
// (env-scrubbed) construction as buildSandboxedCommandRunner, deliberately WITHOUT
// the workspace-trust gate.
//
// Why no trust gate: what makes the force-copy path safe at FORK time is that it
// performs NO git invocation at all (forker.WithForceCopy → copyTree, a pure FS
// copy — no checkout, so no smudge filter or hook can fire), unlike the read-only
// worktree path, where `git worktree add` performs a checkout that can execute an
// untrusted repo's attacker-named filter driver with nobody having run anything.
// It is emphatically NOT that the fork's `.git` is clean: copyTree copies the
// attacker's `.git` VERBATIM — config, hooks, and tracked `.gitattributes` all
// included.
//
// Why still hardened: at RUN time a member/branch running git inside the fork
// executes over that copied untrusted `.git`. gitenv.Scrub pins the FIXED keys
// (core.hooksPath/pager/fsmonitor, diff.external, GIT_* env), but attacker-NAMED
// drivers remain reachable — diff.<drv>.textconv on `git show`/`git log -p`,
// filter.<drv>.smudge on the fork's own checkouts, alias.<name>=!sh if invoked.
// That is the ACCEPTED residual, at MAIN-SESSION PARITY: the operator's own
// (ungated, even unhardened) main loop runs git in the same untrusted repo. The
// trust gate exists to close the FORK-TIME worktree-checkout RCE for read-only
// children, which would auto-fire without the model or operator running anything.
// nil when Bash is disabled.
func buildForceCopyRunner(cfg Config) tool.CommandRunner {
	if cfg.NoBash || cfg.Shell == "" {
		return nil
	}
	return newHardenedCommandRunner(cfg)
}

// newHardenedCommandRunner constructs the env-scrubbed runner shared by
// buildSandboxedCommandRunner and buildForceCopyRunner (see the former for the
// hardening rationale). It assumes the caller already applied the NoBash/empty-shell
// (and, where applicable, trust) gates.
func newHardenedCommandRunner(cfg Config) tool.CommandRunner {
	env := gitenv.Scrub(os.Environ())
	runner, err := osfs.NewCommandRunnerShell(cfg.Workspace, cfg.Shell, osfs.WithCommandEnvList(env))
	if err != nil {
		cfg.diag().Log(context.Background(), port.LevelWarn, "could not build sandboxed member command runner; team-member Bash disabled", "workspace", cfg.Workspace, "err", err)
		return nil
	}
	return runner
}

// subagentShellUntrustedReason returns the model/operator-facing reason the
// subagent/member shell is withheld when the WORKSPACE-TRUST gate is the OPERATIVE
// cause, and "" otherwise: --no-bash / an empty shell disable the shell regardless of
// trust (and must NOT read as an untrust problem), and a trusted workspace has no
// note. It is the single wording source for the Subagent Spec note
// (WithSubagentShellDisabledNote) so the model-facing text and the gate cannot drift.
func subagentShellUntrustedReason(cfg Config) string {
	if cfg.NoBash || cfg.Shell == "" || cfg.TrustProject {
		return ""
	}
	return "no shell on this workspace because it is untrusted (run with --trust-project " +
		"or confirm trust in mecatui to enable the subagent shell)"
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

// roleFamily maps an engine role (agent.Deps.Role — "", "task", "task:<def>",
// "member:<name>", "parallel", "parallel-judge", …) onto the CLOSED, bounded
// role-family label the telemetry plane is allowed to carry. This mapping is
// THE critical cardinality point of the role dimension (issue #47): def names,
// member names, model ids, and session ids must NEVER leak into the returned
// value — every input collapses onto one of exactly six family strings, with
// "child" as the fail-safe bucket for anything unrecognised. The judge is part
// of the Parallel fan-out's cost story, so "parallel-judge" lands in the
// parallel family, not the fallback. There is deliberately NO "fork" family:
// no engine carries a fork Deps.Role (fork/fork-judge exist only as session-id
// prefixes), and a family no series can ever carry would be a model trap in
// the perf tools' role filters. The values mirror the telemetry adapter's
// Role* constants (internal/app deliberately does not import the telemetry
// adapter; the integration tests pin the two sets against drift).
func roleFamily(role string) string {
	switch {
	case role == "":
		return "main"
	case role == "task" || strings.HasPrefix(role, "task:"):
		return "subagent"
	case strings.HasPrefix(role, "member:"):
		return "member"
	case role == "parallel" || role == "parallel-judge":
		return "parallel"
	case role == "usermodel-review":
		return "usermodel"
	default:
		return "child"
	}
}

// childTelemetryFor resolves the (Sink, ToolCallRecorder) pair for a child
// engine: the role-scoped pair from cfg.MetricsRoleScoper keyed on the BOUNDED
// roleFamily(role) when a scoper is wired, or (nil, nil) — the byte-identical
// pre-feature unmetered child shape — when it is not.
func childTelemetryFor(cfg Config, role string) (port.EventSink, port.ToolCallRecorder) {
	if cfg.MetricsRoleScoper == nil {
		return nil, nil
	}
	return cfg.MetricsRoleScoper(roleFamily(role))
}

// newChildEngine bakes in the shared shape every child/member engine assembles:
// an allow-all (non-interactive) permission policy, an inert hook runner, and the
// standard context-window / compaction-trigger settings. Call sites supply only
// what actually varies between them — the scoped catalog, the resolved model, and
// the prompt config. It is the single source of truth for that boilerplate so the
// five child-engine builders (Subagent explorer, Fork branch, Fork judge, per-def Subagent
// engine, team member) cannot drift apart.
func newChildEngine(cfg Config, role string, provider port.LLMProvider, cat *tool.Catalog, model string, pc prompt.Config) *agent.Engine {
	return newChildEngineWithHooks(cfg, role, provider, cat, model, pc, hookexec.New(nil))
}

// newChildEngineWithHooks is newChildEngine with an explicit HookRunner, so a
// per-def Subagent/member engine can scope its own lifecycle hooks (from a def's
// `hooks:` map) instead of the inert default. A nil hooks runner falls back to an
// inert one, preserving the no-hooks contract.
func newChildEngineWithHooks(cfg Config, role string, provider port.LLMProvider, cat *tool.Catalog, model string, pc prompt.Config, hooks port.HookRunner) *agent.Engine {
	return agent.NewEngine(childEngineDeps(cfg, role, provider, cat, model, pc, hooks))
}

// childEngineDeps builds the agent.Deps for the DEFAULT-provider child shape
// (newChildEngineWithHooks). It is split out from newChildEngineWithHooks for
// the same reason childEngineDepsForProvider is split from
// newChildEngineForProvider: the engine's deps are private, so a test can only
// assert this literal's fields (e.g. that Clock is wired — issue #53) against
// the helper, never through the constructed *agent.Engine.
func childEngineDeps(cfg Config, role string, provider port.LLMProvider, cat *tool.Catalog, model string, pc prompt.Config, hooks port.HookRunner) agent.Deps {
	if hooks == nil {
		hooks = hookexec.New(nil)
	}
	// Role-scoped telemetry (issue #47): when the composition wires a
	// MetricsRoleScoper, this child's metrics flow into the shared instruments
	// under the BOUNDED roleFamily(role) label; with no scoper both stay nil
	// (the byte-identical unmetered child shape). Same posture as
	// childEngineDepsForProvider — the two child deps builders must not drift.
	sink, recorder := childTelemetryFor(cfg, role)
	return agent.Deps{
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
		// which are role-scoped via the scoper above (or OFF without one). The role
		// tags every line the child emits with "agent"=<role>.
		Diagnostics:      cfg.diag(),
		Role:             role,
		Sink:             sink,
		ToolCallRecorder: recorder,
		// Clock: the production wall clock (issue #53) — children time their tool
		// calls/turns regardless of whether the role-scoped telemetry pair is wired.
		Clock:               wallclock.Clock{},
		ContextWindowTokens: defaultContextWindowTokens,
		CompactionRatio:     defaultCompactionRatio,
	}
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
// child Deps directly (Sink/ToolCallRecorder role-scoped-or-nil, Compactor/TokenCounter
// keyed on the CHILD's model) — the engine's deps are otherwise private.
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
	// Child telemetry is ROLE-SCOPED, never the main pair (issue #47).
	// engineDepsForProvider set Sink/ToolCallRecorder from cfg (the main engine's
	// role="main" pair); reusing those for a child would double-count a
	// sub-agent's turns/tool-calls against the operator-facing main series — the
	// reason the old child constructor forced both nil. With a MetricsRoleScoper
	// wired, the child instead gets its OWN pair tagged with the BOUNDED
	// roleFamily(role) label ("subagent"/"member"/"parallel"/…), so child activity
	// lands on role-distinct series of the SAME instruments: visible on the perf
	// plane, never folded into role="main". The mapping is the cardinality
	// guarantee — the raw role (which can embed a def name or model id) never
	// reaches a label. Without a scoper (no-perf composition) both stay nil,
	// byte-identical to the pre-feature child shape. Diagnostics is a SEPARATE
	// seam: it is intentionally LIVE for children, bound to cfg.diag() and tagged
	// with the child's agent role (Deps.Role), so interleaved child diagnostics
	// (compaction degradation, policy denies) stay readable and correlated on the
	// operator channel. The conversation event stream (Run.Events()) is untouched
	// either way — this is metrics/audit only.
	deps.Sink, deps.ToolCallRecorder = childTelemetryFor(cfg, role)
	deps.Diagnostics = cfg.diag()
	deps.Role = role
	// A child engine never surfaces a further-nested subagent ask (subagents cannot
	// recurse), so it installs no child-ask router: force Interactive false regardless
	// of the parent's cfg.Interactive. The child's OWN asks resolve via the per-child
	// posture (isolation auto-approve → surface-via-parent → headless auto-deny), driven
	// by the PARENT run's caps, not by the child engine's interactivity.
	deps.Interactive = false
	return deps
}

// buildChildEngine constructs the default Subagent explorer child *Engine: the read-only
// explorer toolset (Read/Grep/Glob — never Fork/Subagent/ToolSearch, so a child can never
// recurse or fan out further, and never Edit/Write, so it cannot edit the project)
// under an allow-all, non-interactive policy.
//
// Bash IS registered when a runner is configured (runner != nil), using the SANDBOXED
// runner: a Subagent child now runs in an isolated git WORKTREE (wired via the Subagent tool's
// child forker — see buildSubagentTool) that SHARES the parent repo's `.git`, so its shell
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
//
// MODEL (issue #35): the explorer resolves its model through the SAME def-less
// chain every child family uses — `SubagentModel (alias-resolved) > parentModel`
// (resolveDefaultChildModel) — and is built through newChildEngineForProvider so
// a SubagentModel-overridden explorer compacts/counts/prompts on the OVERRIDE
// model with its own re-derived context window (never a clone-and-swap). With no
// override the resolved model IS parentModel and window 0 keeps the engine
// byte-identical to the historical shape.
func buildChildEngine(cfg Config, provReg *providerRegistry, provider port.LLMProvider, parentProviderID, parentModel string, runner tool.CommandRunner) *agent.Engine {
	return agent.NewEngine(childExplorerDeps(cfg, provReg, provider, parentProviderID, parentModel, runner))
}

// childExplorerDeps builds the default Subagent explorer's agent.Deps — split out
// from buildChildEngine (the childEngineDepsForProvider precedent) so a test can
// assert the resolved Deps directly (Model / PromptConfig.Env.Model /
// ContextWindowTokens are private once inside the engine).
func childExplorerDeps(cfg Config, provReg *providerRegistry, provider port.LLMProvider, parentProviderID, parentModel string, runner tool.CommandRunner) agent.Deps {
	model, childWindow := resolveDefaultChildModel(cfg, provReg, parentProviderID, parentModel)
	return childEngineDepsForProvider(cfg, "task", provider, model, childWindow,
		readOnlyExplorerCatalog(runner), explorerPromptConfig(modelCfgFor(cfg, model)), nil)
}

// readOnlyExplorerCatalog builds the canonical read-only explorer tool surface a Subagent
// child (and a read-only Fork/member base) is scoped to: Read/Grep/Glob, PLUS the
// SANDBOXED Bash tool when a runner is wired (runner != nil). It NEVER includes
// Edit/Write (the explorer inspects, it does not edit the project) nor Subagent/Parallel/
// ToolSearch (no recursion/fan-out). It is the ONE definition of that surface, shared by
// buildChildEngine (default Subagent explorer), buildSubagentEngineFactory (per-call model
// override — byte-identical to the default), and buildParallelChildEngine's read-only base
// (which then layers Edit/Write on top). The team-member catalog is DELIBERATELY NOT
// built from here: its Bash gating differs (spec.Mutating || roIsolationAvailable, with
// the isolateReadOnly side-effect), so it keeps its own tiering.
func readOnlyExplorerCatalog(runner tool.CommandRunner) *tool.Catalog {
	cat := tool.NewCatalog()
	cat.MustRegister(tools.ReadTool{})
	cat.MustRegister(tools.GrepTool{})
	cat.MustRegister(tools.GlobTool{})
	if runner != nil {
		cat.MustRegister(tools.NewBashTool(runner))
	}
	return cat
}

// explorerPromptConfig is promptConfig for the DEFAULT Subagent explorer child: it appends
// the References convention (D5b) to the explorer's Role so the child ENDS its summary
// with a `References:` block listing the relevant file paths (path or path:line). This
// makes the most common Subagent deliverable navigable without re-searching, and it lands in
// the model-visible RESULT by construction (it shapes the child's output). It augments
// the explorer Role specifically — NOT the shared defaultTone "Cite code as
// file_path:line" sentence (that already exists and is a different, inline-citation
// instruction). A per-def Subagent engine builds its Role via agentPromptConfig instead, so
// this applies to the anonymous explorer (the no-`agent` path) where it is most useful.
func explorerPromptConfig(cfg Config) prompt.Config {
	pc := promptConfig(cfg, cfg.gitStatus)
	pc.Role += "\n\n" + explorerReferencesInstruction
	return pc
}

// explorerReferencesInstruction is the References-convention directive appended to the
// default explorer child's Role (D5b). The substring "References:" is a stable test key
// (TestExplorerPromptInstructsReferences) — do not change it.
const explorerReferencesInstruction = "When you finish, END your summary with a " +
	"\"References:\" section listing the file paths (as path or path:line) most relevant " +
	"to the task, so the caller can navigate directly to them without searching again. " +
	"List concrete paths, not prose."

// buildParallelChildEngine constructs the child *Engine each Parallel branch runs. Unlike
// buildChildEngine (the read-only Subagent explorer), a Parallel branch child MAY MUTATE
// its OWN fork: it gets Read/Grep/Glob/Edit/Write, still EXCLUDING Subagent/Parallel/
// ToolSearch (a branch must not recurse or fan out further).
//
// Bash IS registered when a runner is configured (runner != nil). The runner is
// now workspace-aware: BashTool.Execute passes the per-branch forked
// Workspace.Root() to CommandRunner.Run as the working directory, so a branch's
// Bash runs in its OWN fork — its DEFAULT cwd is the isolated fork, never the
// shared parent base. (A shell-less deployment passes a nil runner and the branch
// simply runs without Bash, exactly like the main session.)
//
// This is the behavioural shift Tier 3 enables: Parallel branches can now IMPLEMENT
// (via Edit/Write AND Bash), not merely explore. It is safe — and
// ParallelTool.ReadOnly() stays true — because every branch runs in its OWN isolated
// forked workspace, so a branch's Edit/Write/Bash land in its fork and (for
// relative-path operations) never touch the parent base. Bash can still escape
// its cwd via absolute paths / `cd` — that is the inherent Bash trust model, the
// same as the main session; what the fix guarantees is that the DEFAULT cwd is
// the fork, removing the accidental shared-base mutation a parent-rooted runner
// caused. The mutating winner's fork is what winner-preservation
// (join=first/judge) keeps.
//
// MODEL (issue #35): a branch resolves its model through the SAME def-less chain
// as the default Subagent explorer and undefined team members — `SubagentModel
// (alias-resolved) > parentModel` (resolveDefaultChildModel) — built through
// newChildEngineForProvider so an overridden branch compacts/counts/prompts on
// the OVERRIDE model with its re-derived window. The Parallel JUDGE is the
// deliberate asymmetry: it stays on the SESSION model (see registerParallelTool).
func buildParallelChildEngine(cfg Config, provReg *providerRegistry, provider port.LLMProvider, parentProviderID, parentModel string, runner tool.CommandRunner) *agent.Engine {
	return agent.NewEngine(parallelChildDeps(cfg, provReg, provider, parentProviderID, parentModel, runner))
}

// parallelChildDeps builds the Parallel branch child's agent.Deps — split out from
// buildParallelChildEngine (the childExplorerDeps precedent) so a test can assert
// the resolved Deps directly.
func parallelChildDeps(cfg Config, provReg *providerRegistry, provider port.LLMProvider, parentProviderID, parentModel string, runner tool.CommandRunner) agent.Deps {
	model, childWindow := resolveDefaultChildModel(cfg, provReg, parentProviderID, parentModel)
	// Start from the read-only explorer surface (Read/Grep/Glob + sandboxed Bash) then
	// LAYER Edit/Write on top — a Parallel branch MAY mutate its OWN fork. Bash is
	// workspace-aware (BashTool.Execute passes the per-branch forked Workspace.Root() as
	// workdir), so a branch's Bash runs in its OWN fork. (Subagent/Parallel/ToolSearch stay
	// excluded — readOnlyExplorerCatalog never adds them — so a branch can't recurse.)
	childCat := readOnlyExplorerCatalog(runner)
	childCat.MustRegister(tools.EditTool{})
	childCat.MustRegister(tools.WriteTool{})

	return childEngineDepsForProvider(cfg, "parallel", provider, model, childWindow,
		childCat, promptConfig(modelCfgFor(cfg, model), cfg.gitStatus), nil)
}

// buildParallelJudgeEngine constructs the minimal, tool-less read-only child *Engine
// the Parallel join=judge/best strategy runs to SELECT a winner. It scores text only,
// so it gets an EMPTY catalog (no tools) under an allow-all policy. It is a DISTINCT
// Engine instance from the branch child so, with the mockllm shared-cursor provider
// in tests, the judge's LLM calls never interleave with the branches'; with the
// stateless OpenAI adapter this separation is naturally harmless.
//
// MODEL ASYMMETRY (issue #35, deliberate): the judge KEEPS the session model and
// never consults Config.SubagentModel — selecting a winner is a judgement call the
// operator implicitly trusts to the model they chose for the session, while the
// branches are the bulk-token workers the cheap child default exists for. Pinned
// by TestParallelJudgeStaysOnParentModel; documented in MULTI-PROVIDER.md.
func buildParallelJudgeEngine(cfg Config, provider port.LLMProvider) *agent.Engine {
	return newChildEngine(cfg, "parallel-judge", provider, tool.NewCatalog(), cfg.Model, promptConfig(cfg, cfg.gitStatus))
}

// buildSubagentTool constructs the Subagent tool tool over a default child Engine
// scoped to the read-only explorer toolset, PLUS the per-definition read-only
// child engines resolved from the agent registry (Tier 1). When the registry is
// empty the per-def map is nil and Subagent behaves exactly as before (default
// explorer only); otherwise the model can route to a named specialist via the
// Subagent `agent` arg, and the specialist names+descriptions are surfaced in the
// Subagent spec for progressive disclosure.
// It also threads the MAIN MCP manager so a def's mcpServers can REFERENCE a
// configured server's tools, and connects each def's INLINE servers; the returned
// close func tears those inline managers down (it is aggregated into Built.Close —
// these are process-lifetime engines). The close is nil when no def opens an inline
// server.
//
// Workspace isolation (Phase 2): when Bash is configured, the Subagent tool is wired with
// a SANDBOXED command runner AND a worktree forker (the forker DEFAULT mode — no
// WithForceCopy — so the child shares the parent repo's `.git` for full history). The
// Subagent tool then forks each child run into a throwaway git worktree before running it,
// so a read-only explorer's shell (git log/show, build, test) is confined to that
// worktree and never touches the shared parent base — which is what keeps Subagent
// read-parallel-safe (see agent.SubagentTool.ReadOnly). The sandboxed runner neutralises
// the git config-driven code-execution vectors in the shared `.git` (same rationale
// and residual as team members — see buildSandboxedCommandRunner). When Bash is
// disabled (nil runner) no forker is wired and the child stays a base-sharing
// read-only explorer with no shell, exactly as before.
//
// PER-SUB-AGENT PROVIDER: provReg + parentProviderID + parentModel are the
// inheritance point threaded down to buildAgentSubagentEngines so a def's `provider:`
// can route its child to a different provider (Half A) and a def that pins none
// inherits whatever the call site supplies — the build-time default (buildCatalog)
// or a session-selected provider (Half B). The registry never reaches the Subagent
// tool itself; it is consumed only inside buildAgentSubagentEngines' resolution loop.
func buildSubagentTool(ctx context.Context, cfg Config, provReg *providerRegistry, provider port.LLMProvider, parentProviderID, parentModel string, hooks port.HookRunner, reg *agents.Registry, mainMgr *mcp.Manager, store port.SessionStore, skillReadRoots []string, skillIdx skillIndex) (tool.Tool, func() error) {
	// skillIdx is the build-once name→body preload index (the SAME
	// operator-controlled skill set the Skill tool serves, threaded from the
	// skills seam via the catalog assets) so a def's `skills:` can preload
	// skill bodies into its engine prompt. `hooks` is the inert default each
	// def adopts unless its own `hooks:` map scopes lifecycle hooks to its
	// engine.
	//
	// The Subagent child's Bash runs over a worktree that SHARES the parent `.git`, so it
	// gets the HARDENED runner (the main session keeps its own unhardened runner). nil
	// when Bash is disabled — then no shell, no forker.
	sandboxedRunner := buildSandboxedCommandRunner(cfg)
	engines, meta, mcpClose := buildAgentSubagentEngines(ctx, cfg, provider, provReg, parentProviderID, parentModel, reg, skillIdx, hooks, sandboxedRunner, mainMgr)
	opts := []agent.SubagentOption{
		agent.WithSubagentStopHook(hooks),
		agent.WithAgentEngines(engines, meta),
		// Best-effort persist each child session to the SHARED session store so the
		// InspectSubagent tool can load its transcript by the agentId trailer (ids verbatim;
		// the namespace stays disjoint by prefix convention, not engineering).
		agent.WithSubagentStore(store),
	}
	// Issue #40: when the WORKSPACE-TRUST gate (not --no-bash / an empty shell) is what
	// nil'd the runner, tell the model honestly via the Spec — otherwise the description
	// keeps promising the isolated-worktree shell and the model delegates build/test/git
	// work the child cannot perform. The other disable causes keep the historical
	// description (subagentShellUntrustedReason returns "" for them).
	if reason := subagentShellUntrustedReason(cfg); reason != "" {
		opts = append(opts, agent.WithSubagentShellDisabledNote(reason))
	}
	// Wire the worktree forker ONLY when Bash is available: the child catalog has Bash
	// iff sandboxedRunner != nil, and the forker is what isolates that shell. The two
	// must move together — a Bash child without isolation would run its shell in the
	// shared base (the exact hazard); a forker without Bash would fork for nothing.
	if sandboxedRunner != nil {
		// Worktree default (no WithForceCopy): shares the base repo's `.git` ⇒ full
		// history for git log/show, with its own throwaway working tree.
		taskForker := forker.New(newForkWorkspace(skillReadRoots))
		opts = append(opts, agent.WithChildForker(taskForker))
	}
	// Per-call model override factory: mint an explorer child engine for a requested
	// model through the SAME contamination-safe per-provider path (newChildEngineFor
	// Provider re-derives Compactor/TokenCounter/Env.Model/ContextWindow for the
	// override model) — never a clone-and-swap of the LLM on an existing engine. The
	// closure hands engine/agent only func(string)(*Engine,bool); the registry never
	// crosses (same shape/spirit as WithAgentEngines).
	opts = append(opts, agent.WithSubagentEngineFactory(
		buildSubagentEngineFactory(cfg, provReg, provider, parentProviderID, parentModel, sandboxedRunner)))
	return agent.NewSubagentTool(
		buildChildEngine(cfg, provReg, provider, parentProviderID, parentModel, sandboxedRunner),
		opts...,
	), mcpClose
}

// buildSubagentEngineFactory returns the per-call model-override factory the Subagent tool
// invokes when a call sets `model`. Given an opaque model id it builds a fresh
// read-only explorer child engine pinned to that model on the parent's provider,
// re-deriving the provider-closing Deps (Compactor/TokenCounter/Env.Model/ContextWindow)
// via newChildEngineForProvider so the override child compacts and counts on the
// OVERRIDE model — the contamination-safe path, NEVER a clone-and-swap of an existing
// engine's LLM. The explorer catalog mirrors buildChildEngine exactly (Read/Grep/Glob +
// Bash when a sandboxed runner is wired), so a model-override child has the same tool
// surface and worktree isolation as the default explorer.
//
// Routability: a blank model is unroutable (ok=false → Subagent surfaces a model-addressable
// error). Any non-blank model is routed on the parent provider (the provider validates
// the exact id at request time); its context window is re-derived through the shared
// childWindowFor rule (live-first via the registry meta) so the override child compacts
// on the right window — an override naming the parent's own model stays on the
// inherited-default path (window 0 ⇒ 128k), like every other unchanged-model child.
// Cross-provider routing by a bare model id is intentionally out of scope this round
// (the registry is keyed by provider, not model) — a def's `provider:` remains the
// cross-provider seam.
func buildSubagentEngineFactory(cfg Config, provReg *providerRegistry, provider port.LLMProvider, parentProviderID, parentModel string, runner tool.CommandRunner) func(model string) (*agent.Engine, bool) {
	return func(model string) (*agent.Engine, bool) {
		model = strings.TrimSpace(model)
		if model == "" {
			return nil, false
		}
		// Same read-only explorer surface + References convention as the default explorer
		// (buildChildEngine) — a model-override child is still the explorer, just on a
		// different model.
		childCat := readOnlyExplorerCatalog(runner)
		childWindow := childWindowFor(provReg, parentProviderID, model, parentProviderID, parentModel)
		eng := newChildEngineForProvider(cfg, "task:model="+model, provider, model, childWindow,
			childCat, explorerPromptConfig(modelCfgFor(cfg, model)), nil)
		return eng, true
	}
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
// agentReg is the ONE registry Build resolved via resolveAgentSeam, threaded
// in by both callers (registerTeamTools passes the catalog assets' copy;
// applyTeamConfig passes Build's) — never re-resolved here. The two forkers:
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
func buildTeamWiring(_ context.Context, cfg Config, provReg *providerRegistry, provider port.LLMProvider, parentProviderID, parentModel string, mainMgr *mcp.Manager, agentReg *agents.Registry, skillReadRoots []string, skillIdx skillIndex) (server.MemberEngineFactory, tool.WorkspaceForker, tool.WorkspaceForker, port.HookRunner) {
	// A single hooks runner shared by the supervisor and the member coordination
	// tools. hookexec.New(nil) matches buildEngine's default: the configured-hook map
	// is not yet wired from cfg anywhere, so this is an inert (no-op) runner today,
	// but it is the injection seam once it is.
	teamHooks := hookexec.New(nil)
	// agentReg is the SHARED registry (Build's single resolveAgentSeam): a
	// member whose spec.AgentType names a def adopts that def's scoped
	// catalog/model/prompt/permissionMode. skillIdx is the build-once preload
	// index threaded from the skills seam (catalog assets / Build).
	//
	// fk (force-copy) for mutating members; roFk (worktree default) for read-only
	// members the factory grants a shell. Read-only members run git in the shared
	// .git of a worktree, so they get the SANDBOXED runner — which is also
	// TRUST-GATED (issue #40): on an untrusted workspace memberRunner is nil and no
	// read-only member gets a shell. A MUTATING member runs in a force-copy fork —
	// created by a pure FS copy with NO fork-time git invocation, so the
	// worktree-checkout RCE the trust gate closes cannot fire there — and its
	// RUN-time git executes over the COPIED (possibly untrusted) .git, the accepted
	// main-session-parity residual; its runner is therefore hardened but
	// deliberately NOT trust-gated (buildForceCopyRunner) — the asymmetry
	// TestUntrustedMutatingMemberKeepsBash pins. The main session keeps its own
	// unhardened runner elsewhere.
	fk := forker.New(newForkWorkspace(skillReadRoots), forker.WithForceCopy())
	roFk := forker.New(newForkWorkspace(skillReadRoots))
	memberRunner := buildSandboxedCommandRunner(cfg)
	mutatingRunner := buildForceCopyRunner(cfg)
	roIsolationAvailable := memberRunner != nil && roFk != nil
	factory := buildMemberEngine(cfg, provReg, provider, parentProviderID, parentModel, teamHooks, agentReg, skillIdx, memberRunner, mutatingRunner, roIsolationAvailable, mainMgr)
	return factory, fk, roFk, teamHooks
}

// applyTeamConfig wires the opt-in agent-teams capability into the server.Config.
// When cfg.EnableTeams is false it leaves MemberEngine nil (CreateTeam stays
// ErrTeamsDisabled). When enabled it installs the per-member engine factory, the
// workspace forker, and the shared team hooks runner — all from buildTeamWiring, the
// SAME wiring the Team tool uses (buildCatalog) — so the gRPC CreateTeam path and the
// Team tool cannot drift. MaxTeams is left at zero so the server applies its own
// default.
func applyTeamConfig(svcCfg *server.Config, cfg Config, reg *providerRegistry, provider port.LLMProvider, mainMgr *mcp.Manager, agentReg *agents.Registry, skillReadRoots []string, skillIdx skillIndex) {
	if !cfg.EnableTeams {
		cfg.diag().Log(context.Background(), port.LevelInfo, "agent teams DISABLED (set --enable-teams to enable; experimental)")
		return
	}
	// The gRPC CreateTeam path's MemberEngine is wired ONCE here with the build-time
	// DEFAULT provider as the inherited parent (reg.Default()/cfg.Model). Per-session
	// provider propagation to the standalone CreateTeam RPC is DEFERRED (CreateTeam
	// carries no selector today); the in-catalog Team tool IS covered in Half B.
	// agentReg is the ONE registry Build resolved (resolveAgentSeam).
	factory, fk, roFk, teamHooks := buildTeamWiring(context.Background(), cfg, reg, provider, reg.Default(), cfg.Model, mainMgr, agentReg, skillReadRoots, skillIdx)
	svcCfg.MemberEngine = factory
	svcCfg.Forker = fk
	svcCfg.ReadOnlyForker = roFk
	svcCfg.TeamHooks = teamHooks
	svcCfg.TeamTokenBudget = cfg.MaxTeamTokens
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
//     base toolset, minus disallowedTools, ALWAYS excluding Subagent/Fork/ToolSearch.
//     The available base differs by spec.Mutating: a Mutating member (isolated fork)
//     may keep Edit/Write/Bash, so the def MAY scope them in; a read-only
//     (base-sharing) member has mutating tools DROPPED with a diagnostic, so the
//     supervisor's AddMember backstop (ErrReadOnlyMemberMutating) is never tripped.
//   - The member's model resolves def.Model > SubagentModel > parent — and since
//     issue #35 the DEFAULT (undefined) member resolves through the SAME chain
//     minus the def tier (SubagentModel > parent, via resolveDefaultChildModel),
//     so a configured cheap child default reaches undefined members too (lead
//     included; lead-strong split deferred). The def body composes into the system
//     prompt as the Role; the def's permissionMode maps to a per-member session
//     mode returned in the MemberBuild.
//
// In BOTH cases the team coordination tools (MemberTools) are ALWAYS appended after
// scoping — they bypass the def allowlist — and Subagent/Fork are NEVER included (a
// member must not recurse or fan out further).
//
// Unknown AgentType is FORGIVING (the skills/teams philosophy): it logs a warning
// and falls back to the DEFAULT member catalog/model/mode rather than failing the
// spawn, so a stale roster reference never wedges a team. (An unknown Subagent `agent`
// arg, by contrast, is a model-addressable error — the model can retry; an operator
// roster entry cannot.)
//
// PER-SUB-AGENT PROVIDER: provReg + parentProviderID + parentModel are the
// inheritance point. A DEFINED member resolves its def's (provider, model) via
// resolveProviderModel; a def that pins a known provider routes the member engine
// to THAT provider (built through newChildEngineForProvider so it compacts/counts
// on the right model). A member pinning none — or the DEFAULT (undefined) member —
// inherits the parent provider unchanged.
// SHELL RUNNERS (issue #40): `runner` is the TRUST-GATED sandboxed runner a read-only
// member's worktree Bash uses (nil on an untrusted workspace ⇒ no read-only shell);
// `mutatingRunner` is the hardened-but-UNGATED runner a Mutating member's force-copy
// Bash uses (no fork-time git invocation, and its run-time git over the COPIED
// untrusted .git is the accepted main-session-parity residual — see
// buildForceCopyRunner), so untrust withholds ONLY the worktree shell — the
// asymmetry the trust-gate tests pin.
func buildMemberEngine(cfg Config, provReg *providerRegistry, provider port.LLMProvider, parentProviderID, parentModel string, teamHooks port.HookRunner, reg *agents.Registry, skillIdx skillIndex, runner, mutatingRunner tool.CommandRunner, roIsolationAvailable bool, mainMgr *mcp.Manager) server.MemberEngineFactory {
	return func(t *team.Team, spec agent.MemberSpec) agent.MemberBuild {
		cat := tool.NewCatalog()
		// Default (undefined) member model (issue #35): the SAME def-less chain as
		// the default Subagent explorer and Parallel branches — `SubagentModel
		// (alias-resolved) > parentModel` — with the window re-derived when the
		// override changes the model. v1 applies it to EVERY undefined member,
		// LEAD INCLUDED (a lead-strong/member-cheap split is deferred — a lead that
		// must stay on the strong model can pin it via an agent def today). A
		// DEFINED member overrides all of this via resolveChildProvider below.
		defaultModel, defaultWindow := resolveDefaultChildModel(cfg, provReg, parentProviderID, parentModel)
		var (
			// Default (undefined) member: inherit the parent provider + the resolved
			// def-less model the call site supplied (the build-time default, or a
			// session-selected provider in Half B). A DEFINED member overrides these
			// via resolveProviderModel below.
			model         = defaultModel
			pc            = promptConfig(modelCfgFor(cfg, defaultModel), cfg.gitStatus)
			childProvider = provider
			childWindow   = defaultWindow
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
			base := baseSubagentTools(cfg)
			// A MUTATING member's shell is NOT trust-gated (force-copy fork: no
			// fork-time git, run-time git is main-session parity — see
			// buildForceCopyRunner), so when the trust-gated base excludes Bash
			// (untrusted workspace) the mutating member's base gets it back from the
			// ungated runner: a def allow-listing Bash for a Mutating member keeps it
			// under untrust, consistent with the default-member tier.
			if spec.Mutating && mutatingRunner != nil {
				bt := tools.NewBashTool(mutatingRunner)
				base[bt.Spec().Name] = bt
			}
			// allowShell: a non-mutating member may keep Bash ONLY when a runner is
			// wired AND a read-only forker is available to isolate it in a worktree.
			allowShell := !spec.Mutating && runner != nil && roIsolationAvailable
			names, diags := scopedToolNamesMode(def, base, spec.Mutating, allowShell, bashScopeMissReason(cfg))
			for _, d := range diags {
				cfg.diag().Log(context.Background(), port.LevelWarn, "team member agent def tool scoping",
					"member", spec.Name, "agent", def.Name, "tool", d.tool, "reason", d.reason, "source", reg.Detail(def.Name))
			}
			for _, name := range names {
				// Bash registers with the HARDENED member runner (passed in), not the
				// baseSubagentTools one used purely to compute the name set — so a
				// member's shell over the shared `.git` cannot be hijacked via git config
				// (core.pager/hooksPath/fsmonitor/external-diff). Every other tool registers
				// as-is. Note: there is NO unhardened member-Bash fall-through — a
				// read-only member gets the trust-gated `runner` (the only path that
				// kept Bash for it, via allowShell ⇒ runner != nil), a Mutating member
				// the ungated-but-hardened `mutatingRunner` (its force-copy fork
				// COPIED the base `.git` verbatim — possibly an untrusted repo's — so
				// the env scrub is load-bearing there too, not moot).
				if name == tools.BashToolName {
					if memberBash := memberBashRunner(spec.Mutating, runner, mutatingRunner); memberBash != nil {
						cat.MustRegister(tools.NewBashTool(memberBash))
					}
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
					"member", spec.Name, "agent", def.Name, "skill", name, "source", reg.Detail(def.Name))
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
				"preloaded_skills", len(bodies), "source", reg.Detail(def.Name))
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
			isolateReadOnly = registerDefaultMemberTools(cat, spec, runner, mutatingRunner, roIsolationAvailable)
		}

		// Team coordination tools ALWAYS, in both branches: they bypass the def
		// allowlist and are exempt from the read-only-member mutating-tool backstop.
		for _, mt := range agent.MemberTools(t, spec.Name, teamHooks) {
			cat.MustRegister(mt)
		}

		pc = applyUntrustedMemberShellNote(cfg, spec, pc)

		// Built through newChildEngineForProvider so a provider-switched member
		// compacts/counts on its own model (contamination fix); childWindow=0 for the
		// inherited-default member keeps it byte-identical.
		eng := newChildEngineForProvider(cfg, "member:"+spec.Name, childProvider, model, childWindow, cat, pc, memberHooks)
		return agent.MemberBuild{Engine: eng, Mode: mode, Limits: memberLimits, Close: mcpClose, MCPToolNames: mcpNames, IsolateReadOnly: isolateReadOnly}
	}
}

// registerDefaultMemberTools registers the DEFAULT (no-def) member catalog tiers:
// Read/Grep/Glob always; Edit/Write for a Mutating member; and Bash per the
// issue-#40 runner split — a Mutating member's Bash rides the ungated
// mutatingRunner (force-copy fork: no fork-time git, run-time git over the copied
// .git is main-session parity — see buildForceCopyRunner) while a read-only
// member's rides the TRUST-GATED runner (nil on an untrusted workspace), so an
// untrusted read-only member stays shell-less while a Mutating one keeps Bash (the
// pinned asymmetry).
// It reports whether the member ended up read-only-ISOLATED (Bash granted to a
// non-mutating member ⇒ the supervisor must worktree-isolate it).
func registerDefaultMemberTools(cat *tool.Catalog, spec agent.MemberSpec, runner, mutatingRunner tool.CommandRunner, roIsolationAvailable bool) (isolateReadOnly bool) {
	cat.MustRegister(tools.ReadTool{})
	cat.MustRegister(tools.GrepTool{})
	cat.MustRegister(tools.GlobTool{})
	if spec.Mutating {
		cat.MustRegister(tools.EditTool{})
		cat.MustRegister(tools.WriteTool{})
	}
	if memberBash := memberBashRunner(spec.Mutating, runner, mutatingRunner); memberBash != nil && (spec.Mutating || roIsolationAvailable) {
		cat.MustRegister(tools.NewBashTool(memberBash))
		isolateReadOnly = !spec.Mutating && roIsolationAvailable
	}
	return isolateReadOnly
}

// applyUntrustedMemberShellNote appends the issue-#40 honesty line to a READ-ONLY
// member's Role on an UNTRUSTED workspace (the trust gate withheld its worktree
// shell), so the member plans around Read/Grep/Glob instead of burning turns
// attempting Bash. A Mutating member keeps its force-copy-fork shell, and a trusted
// workspace keeps its shell, so both pass through unchanged.
func applyUntrustedMemberShellNote(cfg Config, spec agent.MemberSpec, pc prompt.Config) prompt.Config {
	if cfg.TrustProject || spec.Mutating {
		return pc
	}
	if pc.Role == "" {
		pc.Role = prompt.DefaultRole()
	}
	pc.Role += "\n\n" + untrustedMemberShellNote
	return pc
}

// memberBashRunner selects which hardened runner a member's Bash registers with:
// the ungated mutatingRunner for a Mutating (force-copy, own-.git) member, the
// TRUST-GATED roRunner for a read-only (worktree, shared-.git) member — the single
// selection point for the issue-#40 asymmetry, used by both the def and default
// member catalog tiers so they cannot drift.
func memberBashRunner(mutating bool, roRunner, mutatingRunner tool.CommandRunner) tool.CommandRunner {
	if mutating {
		return mutatingRunner
	}
	return roRunner
}

// untrustedMemberShellNote is the one-line system-prompt suffix a READ-ONLY team
// member receives on an untrusted workspace (issue #40), so it knows up front it has
// no shell rather than discovering it via unknown-tool errors.
const untrustedMemberShellNote = "This workspace is untrusted: you have no shell; use Read/Grep/Glob."

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

// agencyDelta returns the emphatic task-persistence contract appended to the
// role framing. It is supplied for ALL model families, Claude included: the
// earlier assumption that Claude persists without it (and that the extra wording
// over-steers it) was disproven in the field — Claude Opus repeatedly ANNOUNCED a
// tool action as plain text ("launching all six subagents now") and then ended the
// turn WITHOUT emitting the tool calls, an "intent without action" agency failure
// (issue #49). So the contract now applies uniformly, with a same-turn-action
// clause to close that gap and a strong ambiguity hedge so it does not over-steer
// (push through genuine ambiguity or refuse to ever yield). The prompt package is
// model-neutral by design; this composition layer owns the wording.
func agencyDelta(model string) string {
	_ = model // the contract is now uniform across families
	return "Keep going until the task is actually resolved before ending your " +
		"turn — implement the change rather than describing it, and when you say " +
		"you are going to call a tool or take an action, emit those tool calls in " +
		"the SAME turn instead of ending on an announcement of intent. Do not stop " +
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
// Glob, WebFetch, the Subagent explorer) are allowed; mutating tools (Bash, Edit, Write)
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
//
// CHILD-OBSERVABILITY pre-approval (issue #37): the three read-only
// child-observability tools (InspectSubagent/InspectMember/SubagentStatus) follow
// the SAME pattern — explicit ScopeBuiltinDefault ALLOWs, pre-approved but
// config-overridable, loosening no other tool's Ask. Rationale inline at the
// entries below; guarded by internal/app/inspect_perm_test.go.
func defaultRules() []governance.Rule {
	return []governance.Rule{
		{Scope: governance.ScopeBuiltinDefault, Tool: "Read", Effect: governance.Allow},
		{Scope: governance.ScopeBuiltinDefault, Tool: "Grep", Effect: governance.Allow},
		{Scope: governance.ScopeBuiltinDefault, Tool: "Glob", Effect: governance.Allow},
		{Scope: governance.ScopeBuiltinDefault, Tool: "WebFetch", Effect: governance.Allow},
		{Scope: governance.ScopeBuiltinDefault, Tool: "Subagent", Effect: governance.Allow},
		{Scope: governance.ScopeBuiltinDefault, Tool: "Bash", Effect: governance.Ask},
		{Scope: governance.ScopeBuiltinDefault, Tool: "Edit", Effect: governance.Ask},
		{Scope: governance.ScopeBuiltinDefault, Tool: "Write", Effect: governance.Ask},
		{Scope: governance.ScopeBuiltinDefault, Tool: skills.DraftToolName, Effect: governance.Ask},
		// Team spawns coordinating subagents that may mutate the workspace (Mutating
		// members), so it ASKS — unlike the read-only Subagent explorer, which is allowed.
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
		// Child observability (issue #37, decided for all three together): the inspect
		// tools (InspectSubagent/InspectMember) and the background-collection tool
		// (SubagentStatus) are read-only PULLs of harness-owned data — persisted child/
		// member transcripts and the run-local child registry — bounded-rendered, with
		// the InspectSubagent prefix gate closing the cross-tool bypass. The children
		// themselves were already permission-gated when they ran; re-prompting to READ
		// their output adds friction without a boundary (SubagentStatus is the SOLE
		// collection channel for background results — an Ask there stalls every
		// background flow on a human). Floor-scoped + tool-name-exact like the memory
		// allows: overridable to ask/deny by any config scope, loosening no other
		// tool's Ask.
		{Scope: governance.ScopeBuiltinDefault, Tool: "InspectSubagent", Effect: governance.Allow},
		{Scope: governance.ScopeBuiltinDefault, Tool: "InspectMember", Effect: governance.Allow},
		{Scope: governance.ScopeBuiltinDefault, Tool: "SubagentStatus", Effect: governance.Allow},
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

// mainEvaluatorOptions returns the governance.Evaluator construction options for the
// MAIN engine's policy. Under --yolo (cfg.AllowAllTools) it loosens the built-in
// substitution Ask floor (WithLooseSubstitution) — consistent with the mutate-ask floor
// the ScopeCLI allow-all rule already loosens, so a substitution command no longer
// prompts under yolo. A configured Deny/Ask in any scope still wins (deny-dominance and
// the configured-ask floor are unaffected). Without --yolo it returns no options (the
// floor stands). Child/member policies are built with their own allow-all rules
// elsewhere; the substitution loosening rides this main-policy seam, and subagents'
// surface/isolation posture handles their substitutions independently.
func mainEvaluatorOptions(cfg Config) []governance.EvaluatorOption {
	if cfg.AllowAllTools {
		return []governance.EvaluatorOption{governance.WithLooseSubstitution(true)}
	}
	return nil
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
// Workspace rooted at the session's workspace dir, carrying the per-skill
// read-only allowed roots (catalogAssets.skillReadRoots) so Read/Stat can serve
// an activated skill's files by absolute path. A root that cannot be opened
// yields a nil Workspace; tool calls against it return errors the model can read.
func osfsWorkspaceFactory(d port.Diagnostics, skillReadRoots []string) server.WorkspaceFactory {
	return func(root string) tool.Workspace {
		ws, err := osfs.NewWorkspace(root, osfs.WithReadRoots(skillReadRoots...))
		if err != nil {
			d.Log(context.Background(), port.LevelError, "workspace factory: cannot open root", "root", root, "err", err)
			return nil
		}
		return ws
	}
}

// newForkWorkspace returns the ONE workspace constructor every fork family
// (Subagent worktree, team member force-copy/worktree, Parallel branch) hands its
// forker, so the per-skill read-only allowed roots reach ISOLATED worktrees too —
// a single helper, not four closures that could drift on the allowlist.
func newForkWorkspace(skillReadRoots []string) func(string) (tool.Workspace, error) {
	return func(root string) (tool.Workspace, error) {
		return osfs.NewWorkspace(root, osfs.WithReadRoots(skillReadRoots...))
	}
}
