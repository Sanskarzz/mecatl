package server

import (
	"context"
	"crypto/rand"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"sort"
	"strings"
	"sync"
	"time"

	mecatlv1 "github.com/stacklok/mecatl/contracts/gen/go/mecatl/v1"
	"github.com/stacklok/mecatl/internal/adapter/mcp"
	"github.com/stacklok/mecatl/internal/adapter/mcp/source"
	"github.com/stacklok/mecatl/internal/adapter/memory"
	"github.com/stacklok/mecatl/internal/adapter/skills"
	"github.com/stacklok/mecatl/internal/adapter/tools"
	"github.com/stacklok/mecatl/internal/agent"
	"github.com/stacklok/mecatl/internal/port"
	"github.com/stacklok/mecatl/internal/session"
	"github.com/stacklok/mecatl/internal/tool"
)

// WorkspaceFactory builds the session-scoped tool.Workspace for a session root.
// The server is workspace-agnostic: the composition root injects memfs (tests)
// or osfs (production) via this seam.
type WorkspaceFactory func(root string) tool.Workspace

// Clock returns the current wall time. It defaults to time.Now when nil so the
// server can stamp session creation timestamps deterministically in tests.
type Clock func() time.Time

// IDGenerator returns a fresh, unique session id. It defaults to a random hex
// id when nil; tests may inject a deterministic generator.
type IDGenerator func() session.SessionID

// ProviderSelector names a per-session provider+model (multi-provider Phase 0,
// S3). The zero value (both empty) means "server default" — the shared engine,
// no per-session build. It is a NEUTRAL value object owned by the server adapter:
// the composition root (internal/app) resolves it against the registry/catalog;
// the adapter never imports either. Setting ModelID with an empty ProviderID is a
// client error (a bare model on an env-derived default provider is ambiguous) —
// rejected at the create boundary before the factory is consulted.
type ProviderSelector struct {
	// ProviderID is the registry id ("" => server default).
	ProviderID string
	// ModelID is the model selector ("" => provider default; a non-empty id the
	// catalog doesn't know is passed through to the provider verbatim).
	ModelID string
}

// SessionEngineResult is what a SessionEngineFactory returns: the built
// per-session engine, the per-session resolved input Capabilities (a NEUTRAL
// port.ProviderCapabilities computed in composition as the catalog ∩ adapter
// intersection for the session's resolved provider+model — see internal/app
// modelCapability), and the Close func that tears down that session's MCP manager
// (a no-op when no specs). A struct (not a 4-tuple) keeps the two interface-typed
// members readable and leaves room for future per-session metadata without another
// signature churn. The Service echoes Capabilities back on CreateSessionResponse
// (session_capabilities) and never recomputes it — the composition is the single
// source so the wire echo and the ListModels view cannot disagree.
type SessionEngineResult struct {
	// Engine is the built per-session engine. Required (non-nil on a nil error).
	Engine *agent.Engine
	// Capabilities is the session's resolved input capability (catalog ∩ adapter),
	// computed in composition. The server echoes it verbatim; it never recomputes.
	Capabilities port.ProviderCapabilities
	// Close tears down the session's MCP manager. Never nil (a no-op when no specs).
	Close func() error
}

// SessionEngineFactory builds a PER-SESSION agent engine over a non-default
// provider/model selector AND/OR the client-provided streaming-HTTP MCP servers
// (specs), returning a SessionEngineResult (engine + per-session capabilities +
// close func) and an error. It is the seam the ACP adapter uses to mount an
// editor's session/new mcpServers AND the seam the gRPC/HTTP CreateSession path
// uses to bind a per-session provider/model — both WITHOUT leaking those tools (or
// the registry/catalog) into the shared engine every other session uses. A session
// needing BOTH a non-default model and client MCP gets ONE engine over ONE catalog
// from a single call (sel + specs are orthogonal inputs). The factory returns an
// error wrapping ErrInvalidArgument for an unknown/unavailable provider id. The
// composition root (internal/app) supplies it via Config.SessionEngine; when nil, a
// non-default selector or non-empty specs are rejected with ErrInvalidArgument. It
// mirrors MemberEngineFactory: the Service references the type in its signatures
// but never builds managers itself.
type SessionEngineFactory func(ctx context.Context, sel ProviderSelector, specs []mcp.ServerConfig) (SessionEngineResult, error)

// Config wires the server adapter to the WP8 engine and its collaborators.
type Config struct {
	// Engine is the shared agent engine that drives every run. Required.
	Engine *agent.Engine
	// Store persists and looks up sessions. Required.
	Store port.SessionStore
	// Workspaces builds a Workspace for a session root. Required.
	Workspaces WorkspaceFactory
	// DefaultMode is applied when a CreateSession request leaves mode
	// unspecified. Defaults to session.ModeDefault when empty.
	DefaultMode session.PermissionMode
	// DefaultLimits are the stop limits applied to a session created without
	// explicit limits. Because a zero Limits value DISABLES every stop condition
	// by design in package session, the composition root injects non-zero
	// defaults here so a default session is always bounded. Per-field: a request
	// that supplies any non-zero limit field is taken as explicit and used as-is.
	DefaultLimits session.Limits
	// Now supplies the creation timestamp; defaults to time.Now.
	Now Clock
	// NewID allocates session ids; defaults to a crypto-random hex generator.
	NewID IDGenerator
	// MCPProvider exposes the connected MCP servers' resources/prompts to the
	// catalog-level inspection RPCs. Optional and nil-safe: when nil, the list
	// RPCs return empty and the read/get RPCs return ErrNoMCPProvider.
	MCPProvider mcp.Provider
	// MCPSources is the resolved MCP source inventory snapshot taken at startup.
	// It backs ListMcpSources and ListToolHiveGroups when MCPSourceProber is nil;
	// in that case both derive purely from this snapshot and perform no live
	// discovery. May be empty.
	MCPSources []source.SourceInfo
	// MCPSourceProber, when non-nil, re-consults the resolved MCP sources on each
	// ListMcpSources/ListToolHiveGroups call and returns a FRESH inventory — so a
	// client refresh reflects CURRENT source status/diagnostics (e.g. a ToolHive
	// workload that crashed or appeared after startup), not the startup snapshot.
	// It is the live-discovery seam: the composition root supplies a prober that
	// closes over the resolved []source.Source and re-runs source.InspectSources.
	// When nil, ListMcpSources falls back to the cached MCPSources snapshot. The
	// prober is read-only (streaming-HTTP / container queries only; never spawns a
	// process) and fail-soft: on any failure the Service falls back to the cached
	// snapshot so the panel always renders.
	MCPSourceProber func(ctx context.Context) []source.SourceInfo
	// Commands lists the available slash commands for a workspace, backing the
	// ListCommands RPC (the client's in-input command palette). It is the
	// composition-injected discovery seam: the composition root (internal/app)
	// closes over the SAME command expander it builds for the run path and the
	// workspace factory, so the palette offers exactly the commands a "/<cmd>"
	// prompt would expand. Optional and nil-safe: when nil (command expansion
	// disabled, or no expander enumerates), ListCommands returns an empty list.
	// It is read-only and called per request (discovery is cheap file scanning).
	Commands CommandLister

	// Agents is the resolved agent-definition snapshot taken at startup. It backs
	// ListAgents and is a pure read of this snapshot (no live discovery). The
	// composition root (internal/app) resolves the registry once and projects each
	// def into the proto form (name/description/resolved model/effective read-only
	// tool scope/permission mode/color) so the server adapter never imports the
	// agents adapter. May be empty (agent definitions disabled or none found).
	Agents []*mecatlv1.AgentInfo

	// Models is the resolved selectable-model inventory snapshot taken at startup
	// (multi-provider Phase 0, S3). It backs ListModels and is a pure read of this
	// snapshot (no live discovery — the registry's available providers + the
	// embedded catalog are both fixed for the process lifetime). The composition
	// root (internal/app) joins the registry's AVAILABLE providers to the catalog
	// and projects each model into the proto form (modelSnapshot) so the server
	// adapter never imports providercatalog or the registry. May be empty (zero
	// providers available). NO secret material (no key, env var name, or base URL).
	Models []*mecatlv1.ModelInfo

	// DefaultCapabilities is the NEUTRAL per-(default provider+default model) input
	// capability — the catalog ∩ adapter INTERSECTION computed once in composition
	// (internal/app modelCapability for the registry default + cfg.Model). It is the
	// single source for BOTH the shared/default-engine session_capabilities echo
	// (when a session uses no per-session engine) AND ProviderCapabilities() (the ACP
	// gate). The server adapter holds only this neutral value — it never imports the
	// catalog or registry. The zero value (text-only) is the safe default for a
	// child/member service with no provider. (multi-provider Phase 0, S5.)
	DefaultCapabilities port.ProviderCapabilities

	// Skills is the resolved skills-inventory snapshot taken at startup. It backs
	// ListSkills and is a pure read of this snapshot (no live discovery — skills
	// are discovered once at build time and immutable for the process lifetime).
	// The composition root (internal/app) discovers the skills once and projects
	// each into the proto form (name + description); that PROJECTION (skillSnapshot)
	// lives in internal/app, not here, so the server adapter holds only the proto
	// snapshot and never reaches into the skills adapter's discovery types. May be
	// empty (skills disabled or none found).
	Skills []*mecatlv1.SkillInfo

	// Soul is the resolved soul (persona) BUILD-TIME SNAPSHOT taken at startup. It
	// backs GetSoul and is a pure read of this snapshot — the soul is selected once
	// (USER-wins precedence, project trust gate, drift check) and is immutable for the
	// process lifetime, so no live re-read is warranted. The composition root
	// (internal/app) projects the winning soul's content + soulMeta into the proto form
	// (soulSnapshot) so the server adapter never reaches into the soul adapter or the
	// composition-layer soulMeta type. nil when no soul source is wired (--no-soul or
	// none present); a nil Soul makes capabilities().Soul false and GetSoul return an
	// empty (present=false) snapshot.
	Soul *mecatlv1.SoulInfo

	// UserModel lists the CURRENT user-model entries, backing GetUserModel. Unlike
	// Soul (a startup snapshot) it is a LIVE lister: the composition root closes over
	// the user-model store's Index so a refresh reflects entries saved since startup.
	// It is the same seam idiom as Commands. Optional and nil-safe: when nil (user
	// model disabled) capabilities().UserModel is false and GetUserModel returns empty.
	UserModel UserModelLister

	// SessionEngine builds a PER-SESSION engine over a non-default provider/model
	// selector AND/OR client-provided streaming-HTTP MCP servers (the ACP
	// session/new mcpServers). It is the seam that lets a session bind its OWN
	// provider/model or mount its OWN MCP tools without leaking them (or the
	// provider registry) into the shared Engine every other session uses. When nil,
	// a non-default selector or non-empty MCP specs are rejected with
	// ErrInvalidArgument; a session with the zero selector and no client MCP always
	// uses the shared Engine (zero overhead). The composition root (internal/app)
	// supplies it.
	SessionEngine SessionEngineFactory

	// MemberEngine builds a team member's Engine from the shared team and the
	// member spec (see internal/agent.MemberEngine). It is the seam that wires
	// the agent-team RPCs: when nil, those RPCs return ErrTeamsDisabled. The
	// composition root supplies it (internal/app), capturing the per-member
	// catalog (read-only base + MemberTools, plus mutating tools only for a
	// Mutating member) and the provider/model.
	MemberEngine MemberEngineFactory
	// Forker isolates a Mutating team member's workspace (force-copy: own `.git`).
	// Optional; required only if a Mutating member is spawned.
	Forker tool.WorkspaceForker
	// ReadOnlyForker isolates a read-only-isolated team member's workspace as a cheap
	// git worktree (shares the base repo's `.git` ⇒ full history) so an inspect-only
	// member can run a shell (git log/show, build, test) confined to a throwaway
	// checkout. Optional; required only if the member factory marks any read-only
	// member IsolateReadOnly (which the composition root does only when this is
	// wired). When nil, read-only members base-share with no shell.
	ReadOnlyForker tool.WorkspaceForker
	// TeamHooks fires the team lifecycle hooks (TeammateIdle) and is passed to
	// member coordination tools for the TaskCreated / TaskCompleted gates.
	// Optional.
	TeamHooks port.HookRunner
	// MaxTeams caps the number of live (un-cleaned) teams the registry holds at
	// once, bounding the leak when clients create teams but never CleanupTeam.
	// CreateTeam returns ErrTooManyTeams (ResourceExhausted) when the cap is
	// reached; cleaning up a created/done team frees a slot. Defaults to
	// defaultMaxTeams when zero.
	MaxTeams int

	// MaxSessionEngines caps the number of live (un-released) PER-SESSION engines
	// the registry holds at once (CWE-770). A per-session engine is registered when
	// a session needs a non-default provider/model selector OR client-provided MCP
	// servers. The ACP surface drains them on editor disconnect, but the gRPC/HTTP
	// surfaces have no teardown signal, so without a cap a hostile authed client
	// could call CreateSession with a valid provider_id repeatedly (never closing)
	// and grow the map unbounded. createSession returns ErrTooManySessionEngines
	// (ResourceExhausted) when the cap is reached; CloseSession / EndSession frees a
	// slot. It is a generous count (a session is multi-turn and its engine MUST
	// persist across turns, so this is NOT terminal-state eviction). Defaults to
	// defaultMaxSessionEngines when zero.
	MaxSessionEngines int

	// OnCloseSession, when non-nil, is invoked by CloseSession with the closing
	// session id BEFORE the per-session engine teardown. It is the composition
	// seam for releasing session-scoped state the Service does not own — currently
	// the per-session LEARNED permission rules (issue #3), evicted via
	// permstore.Memory.Forget so they do not outlive the session. Optional and
	// nil-safe.
	OnCloseSession func(session.SessionID)
}

// defaultMaxTeams is the live-team registry cap applied when Config.MaxTeams is
// zero. It bounds memory growth from teams that are created but never cleaned up.
const defaultMaxTeams = 64

// defaultMaxSessionEngines is the per-session engine registry cap applied when
// Config.MaxSessionEngines is zero. It is generous (a per-session engine is a
// legitimate per-conversation resource) but finite, so a client that never
// releases its selector/MCP sessions cannot grow the map without bound (CWE-770).
const defaultMaxSessionEngines = 1024

// ErrConfig is returned by NewService when a required dependency is missing.
var ErrConfig = errors.New("server: invalid config")

// Service is the surface-agnostic application service shared by the gRPC and
// HTTP/SSE adapters. It owns session lifecycle (create/lookup), starts runs on
// the shared engine, and keeps a registry of in-flight runs keyed by session id
// so out-of-band approve/cancel reach the right run. It is safe for concurrent
// use.
//
// # Persistence and auto-resume
//
// The engine mutates the session in place over a run and the Service persists
// it to the SessionStore at meaningful transitions: at create, when the run
// pauses awaiting approval (see Persist), and at run end. With a durable store
// (jsonlstore via --store-dir) the latest snapshot therefore survives a process
// restart.
//
// A registered run is removed by the wire adapter that owns the stream: each
// adapter `defer`s FinishRun(id, run) after it finishes draining run.Events()
// (the channel closes when the run terminates). The Service does not deregister
// runs on its own — there is no internal relay goroutine that does so.
//
// GetSession, Approve and Cancel for a session id NOT in the in-memory run
// registry fall back to SessionStore.Load, so a session created (or last
// persisted) before a restart is still observable and its terminal/awaiting
// state is loadable. Resuming an in-flight STREAM across a restart is out of
// scope: the *agent.Run and its event channel live only in process memory, so
// after a restart there is no run to deliver an approval to. A persisted
// awaiting session remains loadable (a client can GET it and re-attach), but an
// Approve/Cancel that finds the session only in the store — with no live run —
// returns ErrNoActiveRun rather than silently succeeding.
type Service struct {
	cfg Config

	mu    sync.Mutex
	runs  map[session.SessionID]*runState
	teams map[string]*teamState
	// sessionEngines holds the per-session engines — built for a session that needs
	// a non-default provider/model selector (gRPC/HTTP CreateSession) OR
	// client-provided MCP servers (ACP session/new). The ACP surface drains them on
	// editor disconnect (closeTrackedSessions) and Service.Close drains the rest on
	// shutdown, but the gRPC/HTTP surfaces have NO connection-teardown signal — a
	// client that creates selector sessions and never calls CloseSession/EndSession
	// would otherwise grow this map unbounded (CWE-770). So it is also CAPPED at
	// Config.MaxSessionEngines (mirroring MaxTeams): createSession returns
	// ErrTooManySessionEngines once the cap is reached, and CloseSession frees a slot.
	sessionEngines map[session.SessionID]*sessionEngine
	// sessionWorkspaces holds per-session Workspace OVERRIDES. When an entry is
	// present for a session id, StartRun uses it instead of building one from the
	// shared Workspaces factory. It mirrors sessionEngines exactly: registered by a
	// surface adapter (the ACP adapter, to route file I/O through the editor's
	// fs/* buffers), preferred by StartRun, and evicted by CloseSession (editor
	// disconnect) / drained by Close (shutdown). It is bounded by connection
	// lifetime, not a count — same rationale as sessionEngines. The gRPC/HTTP
	// surfaces never register an override, so their behavior is unchanged.
	sessionWorkspaces map[session.SessionID]tool.Workspace
}

// sessionEngine couples a per-session engine (built over that session's
// client-provided MCP servers) with the close func that tears down its MCP
// manager. It is registered by CreateSessionWithMCP and released by CloseSession
// (and by the Service's own Close). StartRun prefers it over the shared engine
// for the owning session id.
type sessionEngine struct {
	engine *agent.Engine
	// caps is the session's resolved input capability (catalog ∩ adapter), computed
	// in composition and echoed verbatim on CreateSessionResponse.session_capabilities
	// via SessionCapabilities. Never recomputed here — the composition is the single
	// source so the per-session echo cannot drift from the ListModels view.
	caps  port.ProviderCapabilities
	close func() error
}

// runState couples an in-flight *agent.Run with the live *session.Session the
// engine mutates in place, so the Service can persist the current session state
// (e.g. on entering awaiting, or at run end) without re-loading from the store.
type runState struct {
	run  *agent.Run
	sess *session.Session
}

// NewService validates cfg and constructs a Service. It returns ErrConfig if
// Engine, Store or Workspaces is nil.
func NewService(cfg Config) (*Service, error) {
	if cfg.Engine == nil {
		return nil, fmt.Errorf("%w: Engine is required", ErrConfig)
	}
	if cfg.Store == nil {
		return nil, fmt.Errorf("%w: Store is required", ErrConfig)
	}
	if cfg.Workspaces == nil {
		return nil, fmt.Errorf("%w: Workspaces is required", ErrConfig)
	}
	if cfg.DefaultMode == "" {
		cfg.DefaultMode = session.ModeDefault
	}
	if cfg.Now == nil {
		cfg.Now = time.Now
	}
	if cfg.NewID == nil {
		cfg.NewID = randomID
	}
	if cfg.MaxTeams <= 0 {
		cfg.MaxTeams = defaultMaxTeams
	}
	if cfg.MaxSessionEngines <= 0 {
		cfg.MaxSessionEngines = defaultMaxSessionEngines
	}
	return &Service{
		cfg:               cfg,
		runs:              make(map[session.SessionID]*runState),
		teams:             make(map[string]*teamState),
		sessionEngines:    make(map[session.SessionID]*sessionEngine),
		sessionWorkspaces: make(map[session.SessionID]tool.Workspace),
	}, nil
}

// ErrNoActiveRun is returned by Approve/Cancel when the session exists (possibly
// loaded from the store after a restart) but has no in-flight run in this
// process to deliver the control to. The session state is still loadable via
// GetSession; the lost stream simply cannot be resumed in place.
var ErrNoActiveRun = errors.New("server: no active run for session")

// CreateSession allocates a new idle session on the SHARED engine, persists it,
// and returns it. workspace must be non-empty. An unspecified mode falls back to
// DefaultMode. It is the no-selector, no-MCP fast path: it delegates to the
// generalized createSession with the zero selector and nil specs.
func (s *Service) CreateSession(ctx context.Context, workspace string, mode session.PermissionMode, limits session.Limits) (*session.Session, error) {
	return s.createSession(ctx, workspace, mode, limits, ProviderSelector{}, nil)
}

// CreateSessionWithProvider creates a session bound to a non-default
// provider/model selector (multi-provider Phase 0, S3) via a PER-SESSION engine,
// with no client MCP. It is the gRPC/HTTP entry for a CreateSession request that
// carries provider_id/model_id. The zero selector delegates to the shared-engine
// fast path; a non-zero selector REQUIRES Config.SessionEngine (else
// ErrInvalidArgument) and resolves through the factory (an unknown/unavailable
// provider id surfaces as ErrInvalidArgument). Setting ModelID with an empty
// ProviderID is rejected (a bare model on the env-derived default provider is
// ambiguous).
func (s *Service) CreateSessionWithProvider(ctx context.Context, workspace string, mode session.PermissionMode, limits session.Limits, sel ProviderSelector) (*session.Session, error) {
	if sel.ProviderID == "" && sel.ModelID != "" {
		return nil, fmt.Errorf("%w: model_id requires provider_id (a bare model on the default provider is ambiguous)", ErrInvalidArgument)
	}
	return s.createSession(ctx, workspace, mode, limits, sel, nil)
}

// createSession is the single create path generalizing the shared-engine fast
// path, the per-session provider/model selector, and the per-session client MCP
// servers. A session needs a PER-SESSION engine when the selector is non-zero OR
// specs are non-empty; otherwise it uses the shared engine (zero overhead, no
// registry entry — today's byte-identical path). The factory takes both inputs so
// a session with BOTH a non-default model and client MCP gets ONE engine over ONE
// catalog. On a persist failure after the engine was built, the per-session MCP
// manager is torn down so a failed create never leaks it.
func (s *Service) createSession(ctx context.Context, workspace string, mode session.PermissionMode, limits session.Limits, sel ProviderSelector, specs []mcp.ServerConfig) (*session.Session, error) {
	if workspace == "" {
		return nil, fmt.Errorf("%w: workspace is required", ErrInvalidArgument)
	}
	if mode == "" {
		mode = s.cfg.DefaultMode
	}
	if limits == (session.Limits{}) {
		// An all-zero Limits disables every stop condition; substitute the
		// injected defaults so a default session cannot run unbounded.
		limits = s.cfg.DefaultLimits
	}

	needPerSession := sel != (ProviderSelector{}) || len(specs) > 0
	if !needPerSession {
		// Shared-engine fast path (today's behaviour, byte-identical).
		sess := session.New(s.cfg.NewID(), mode, workspace, limits, s.cfg.Now())
		if err := s.cfg.Store.Save(ctx, sess); err != nil {
			return nil, fmt.Errorf("server: persist session: %w", err)
		}
		return sess, nil
	}

	if s.cfg.SessionEngine == nil {
		return nil, fmt.Errorf("%w: per-session engine not supported (no session-engine factory configured)", ErrInvalidArgument)
	}
	// Cheap cap pre-check (CWE-770): reject BEFORE the factory connects MCP /
	// allocates an engine when the registry is already full, so a hostile client
	// that never releases its sessions cannot even drive the (more expensive) build
	// path. The authoritative re-check under lock at registration below closes the
	// TOCTOU window (two concurrent creates racing the last slot).
	s.mu.Lock()
	full := len(s.sessionEngines) >= s.cfg.MaxSessionEngines
	s.mu.Unlock()
	if full {
		return nil, fmt.Errorf("%w: %d", ErrTooManySessionEngines, s.cfg.MaxSessionEngines)
	}

	res, err := s.cfg.SessionEngine(ctx, sel, specs)
	if err != nil {
		// Factory maps an unknown/unavailable provider to ErrInvalidArgument; any
		// error is propagated as-is for the caller to map to a status.
		return nil, err
	}
	eng, closeFn := res.Engine, res.Close
	sess := session.New(s.cfg.NewID(), mode, workspace, limits, s.cfg.Now())

	// Authoritative cap check under the SAME lock as the insert (TOCTOU-safe): if
	// the registry filled between the pre-check and here, tear the freshly-built
	// engine down rather than exceed the cap. This is BEFORE the Store.Save, so a
	// cap rejection leaves NO orphan session in the store.
	s.mu.Lock()
	if len(s.sessionEngines) >= s.cfg.MaxSessionEngines {
		s.mu.Unlock()
		if closeFn != nil {
			_ = closeFn()
		}
		return nil, fmt.Errorf("%w: %d", ErrTooManySessionEngines, s.cfg.MaxSessionEngines)
	}
	// Reserve the slot under the lock so a concurrent create cannot also claim it,
	// then persist OUTSIDE the lock (no I/O under the mutex). If the persist fails,
	// evict the reservation and tear the engine down.
	s.sessionEngines[sess.ID] = &sessionEngine{engine: eng, caps: res.Capabilities, close: closeFn}
	s.mu.Unlock()

	if serr := s.cfg.Store.Save(ctx, sess); serr != nil {
		// The engine was built and the slot reserved but the session could not be
		// persisted: evict the reservation and tear the per-session MCP manager down
		// so a failed create leaks neither a slot nor a connection.
		s.mu.Lock()
		delete(s.sessionEngines, sess.ID)
		s.mu.Unlock()
		if closeFn != nil {
			_ = closeFn()
		}
		return nil, fmt.Errorf("server: persist session: %w", serr)
	}
	return sess, nil
}

// capabilities reports which optional features THIS service has actually built,
// for the CreateSession response. It is the single source of truth for the
// client's honest-UI affordances; it reads the wired Config seams and the engine
// catalog (NOT a static list) so it can never claim a feature the server did not
// register. Tool presence is checked by the tools' registered names via
// Engine.HasTool, which is nil-safe (a nil engine/catalog yields the tool caps as
// false). The names are referenced from each owning package's exported constant
// — memory.RememberToolName (internal/adapter/memory.NewRememberTool),
// skills.ToolName (internal/adapter/skills.NewTool), tools.BashToolName
// (internal/adapter/tools.NewBashTool) — so the cap links to the registered name
// at COMPILE time and cannot drift on a rename.
func (s *Service) capabilities() *mecatlv1.ServerCapabilities {
	has := func(name string) bool {
		return s.cfg.Engine != nil && s.cfg.Engine.HasTool(name)
	}
	// Multimodal prompt-input caps come from the composition-computed DEFAULT
	// intersection (catalog ∩ adapter for the default provider+model), NOT the bare
	// engine.Capabilities() (which is adapter-only and would re-introduce the catalog
	// gap). The zero value (text-only) is the safe default for a child/member service
	// with no provider. This is the SAME DefaultCapabilities ProviderCapabilities()
	// returns, so the server-wide caps echo and the ACP gate share ONE source.
	pcaps := s.cfg.DefaultCapabilities
	return &mecatlv1.ServerCapabilities{
		Mcp:            s.cfg.MCPProvider != nil,
		SlashCommands:  s.cfg.Commands != nil,
		Teams:          s.cfg.MemberEngine != nil,
		Agents:         len(s.cfg.Agents) > 0,
		Soul:           s.cfg.Soul != nil,
		UserModel:      s.cfg.UserModel != nil,
		ModelSelection: len(s.cfg.Models) > 0,
		Memory:         has(memory.RememberToolName),
		Skills:         has(skills.ToolName),
		Bash:           has(tools.BashToolName),
		Image:          pcaps.Image,
		Audio:          pcaps.Audio,
	}
}

// CreateSessionWithMCP creates a session that mounts the client-provided
// streaming-HTTP MCP servers (specs) for the lifetime of that session, via a
// PER-SESSION engine. It is the ACP session/new entry for an editor that supplies
// mcpServers.
//
//   - With NO specs it delegates to CreateSession: the session uses the SHARED
//     engine, with zero per-session overhead and no registry entry.
//   - With specs it REQUIRES Config.SessionEngine (else ErrInvalidArgument: "client
//     MCP not supported"); it builds the per-session engine via that factory, and
//     on success registers it under the new session id so StartRun routes the
//     session's runs to it. A factory error is returned as-is (the caller maps it).
//
// The per-session engine's MCP manager is torn down by CloseSession (editor
// disconnect) or by the Service's Close.
func (s *Service) CreateSessionWithMCP(ctx context.Context, workspace string, mode session.PermissionMode, limits session.Limits, specs []mcp.ServerConfig) (*session.Session, error) {
	// Thin wrapper over the generalized create path with the ZERO provider
	// selector: no specs uses the shared engine (today's behaviour), specs build a
	// per-session engine. The factory now takes (sel, specs); the zero selector
	// leaves the per-session engine bound to the DEFAULT provider, matching the
	// pre-S3 MCP path exactly.
	return s.createSession(ctx, workspace, mode, limits, ProviderSelector{}, specs)
}

// SetSessionWorkspace registers a per-session Workspace OVERRIDE for id, so a
// subsequent StartRun uses ws instead of building one from the shared Workspaces
// factory. It is the seam the ACP adapter uses to route a session's file I/O
// through the editor's fs/* buffers. A second call for the same id replaces the
// override. The override is evicted by CloseSession (and drained by Close), so
// the caller MUST pair it with CloseSession on the owning connection's teardown
// (the ACP adapter tracks the session and does this on disconnect). The
// gRPC/HTTP surfaces never call this, so their workspace path is unchanged.
func (s *Service) SetSessionWorkspace(id session.SessionID, ws tool.Workspace) {
	s.mu.Lock()
	s.sessionWorkspaces[id] = ws
	s.mu.Unlock()
}

// CloseSession tears down the per-session engine registered for id (if any) and
// removes it from the registry. It is idempotent: an id with no per-session
// engine is a no-op. The ACP adapter calls it when an editor disconnects so a
// session's client-provided MCP manager does not outlive the session.
//
// SAFE under an in-flight run (so it needs no run-aware guard like the team path):
// the close func is the MCP manager's Close, a GRACEFUL shutdown — the underlying
// go-sdk ClientSession.Close "prevents new requests from being handled, and WAITS
// for ongoing requests to return" before terminating the connection, and is
// documented idempotent + concurrency-safe. A disconnect that races a live
// engine.Run dispatching an MCP tool call therefore does NOT yank the connection
// mid-call: Close blocks until that CallTool returns (or the jsonrpc2 layer retires
// it with an error response the remoteTool maps to a model-facing error). The
// editor disconnect already implies the run is being abandoned, so blocking briefly
// for the in-flight call to unwind is the correct, leak-free behaviour.
func (s *Service) CloseSession(id session.SessionID) {
	// Release composition-owned session-scoped state first (e.g. the per-session
	// learned permission rules) so it never outlives the session, even if the
	// per-session engine teardown below is a no-op for this id.
	if s.cfg.OnCloseSession != nil {
		s.cfg.OnCloseSession(id)
	}
	s.mu.Lock()
	se, ok := s.sessionEngines[id]
	if ok {
		delete(s.sessionEngines, id)
	}
	// Drop any per-session workspace override too: it closes over the (now
	// disconnecting) connection, so it must not outlive the session.
	delete(s.sessionWorkspaces, id)
	s.mu.Unlock()
	if ok && se.close != nil {
		_ = se.close()
	}
}

// EndSession is the precondition-checked sibling of CloseSession: the
// surface-facing session-end entry for the gRPC/HTTP transports (the ACP adapter
// calls the void CloseSession directly on disconnect). It verifies the session
// exists, then runs the same teardown as CloseSession (OnCloseSession ->
// learned-rule Forget, per-session engine + workspace eviction). It returns
// ErrNotFound for a never-created id; teardown is idempotent, so closing an
// already-released (but still persisted) session succeeds. It does NOT delete the
// persisted snapshot and does NOT cancel an in-flight run (orthogonal to Cancel).
func (s *Service) EndSession(ctx context.Context, id session.SessionID) error {
	if _, err := s.GetSession(ctx, id); err != nil {
		return err
	}
	s.CloseSession(id)
	return nil
}

// Close tears down all per-session engines' MCP managers. It is the Service's
// shutdown hook so a process exit does not leak any per-session MCP connection.
// It is safe to call multiple times.
func (s *Service) Close() {
	s.mu.Lock()
	engines := s.sessionEngines
	s.sessionEngines = make(map[session.SessionID]*sessionEngine)
	// Drop all per-session workspace overrides on shutdown; they hold no resources
	// of their own (the underlying connection is closed separately) but must not
	// linger past the Service.
	s.sessionWorkspaces = make(map[session.SessionID]tool.Workspace)
	s.mu.Unlock()
	for _, se := range engines {
		if se.close != nil {
			_ = se.close()
		}
	}
}

// GetSession returns the persisted session under id, or ErrNotFound.
func (s *Service) GetSession(ctx context.Context, id session.SessionID) (*session.Session, error) {
	sess, err := s.cfg.Store.Load(ctx, id)
	if err != nil {
		return nil, fmt.Errorf("%w: %q", ErrNotFound, id)
	}
	return sess, nil
}

// SetMode changes the permission posture of the session under id and persists
// the change, returning the updated session. It is the out-of-band mode-switch
// seam (ACP session/set_mode). It applies to the LIVE session when a run is
// registered (so the change takes effect immediately for an idle-between-prompts
// session held in the registry) and otherwise to the stored snapshot.
//
// It returns ErrNotFound for an unknown session, ErrInvalidArgument for an empty
// mode, and propagates session.ErrIllegalTransition (wrapped as ErrInvalidArgument)
// when the session is mid-turn (running/awaiting) — the aggregate refuses a mode
// change while a turn is in flight, so the caller must defer it to the next
// prompt. A change to the mode the session already has is a no-op success.
func (s *Service) SetMode(ctx context.Context, id session.SessionID, mode session.PermissionMode) (*session.Session, error) {
	if mode == "" {
		return nil, fmt.Errorf("%w: mode is required", ErrInvalidArgument)
	}
	// Prefer the live session the engine drives (if registered) so the change is
	// observed by the same object; otherwise operate on the stored snapshot.
	s.mu.Lock()
	st, live := s.runs[id]
	s.mu.Unlock()

	var sess *session.Session
	if live {
		sess = st.sess
	} else {
		loaded, err := s.GetSession(ctx, id)
		if err != nil {
			return nil, err
		}
		sess = loaded
	}

	if err := sess.SetMode(mode); err != nil {
		// A mid-turn refusal from the aggregate is a client-sequencing error.
		return nil, fmt.Errorf("%w: %v", ErrInvalidArgument, err)
	}
	if err := s.cfg.Store.Save(ctx, sess); err != nil {
		return nil, fmt.Errorf("server: persist session: %w", err)
	}
	return sess, nil
}

// LoadSession resumes a previously-persisted session so a subsequent StartRun
// continues it. It loads the latest snapshot from the store and, if the session
// is in a terminal-but-resumable state (StateCompleted — a clean end-of-run),
// REOPENS it to StateIdle (preserving the conversation history) and re-persists,
// so the next prompt's BeginTurn is legal. A session already idle is returned
// unchanged; a failed/cancelled session is NOT resumable (Reopen rejects it) and
// the prior state is returned as-is so the next StartRun surfaces the illegal
// transition rather than silently continuing a broken session.
//
// It returns ErrNotFound when the store has no snapshot for id (including the
// in-memory store after a process restart, or when no store-dir is configured
// and the id was never created in this process).
func (s *Service) LoadSession(ctx context.Context, id session.SessionID) (*session.Session, error) {
	return s.loadAndReopen(ctx, id)
}

// loadAndReopen is the shared load + reopen-if-completed body of LoadSession and
// LoadSessionWithMCP, factored out so the two cannot drift: it loads the latest
// snapshot, and if the session cleanly completed REOPENS it to idle (preserving
// history) and re-persists. ErrNotFound propagates from GetSession; Reopen rejects
// a failed/cancelled session and that prior state is returned as-is.
func (s *Service) loadAndReopen(ctx context.Context, id session.SessionID) (*session.Session, error) {
	sess, err := s.GetSession(ctx, id)
	if err != nil {
		return nil, err
	}
	if sess.State == session.StateCompleted {
		if rerr := sess.Reopen(); rerr != nil {
			return nil, fmt.Errorf("server: reopen session: %w", rerr)
		}
		if serr := s.cfg.Store.Save(ctx, sess); serr != nil {
			return nil, fmt.Errorf("server: persist reopened session: %w", serr)
		}
	}
	return sess, nil
}

// LoadSessionWithMCP resumes a previously-persisted session AND re-mounts the
// client-provided streaming-HTTP MCP servers (specs) for the lifetime of that
// session, via a PER-SESSION engine. It is the ACP session/load entry for an editor
// that re-supplies mcpServers on resume — the symmetric sibling of
// CreateSessionWithMCP (session/new).
//
//   - With NO specs it delegates to LoadSession: the resumed session uses the SHARED
//     engine, with zero per-session overhead and no registry entry.
//   - With specs it REQUIRES Config.SessionEngine (else ErrInvalidArgument: "client
//     MCP not supported"); it loads + reopens-if-completed FIRST (so an unknown id
//     fails fast — ErrNotFound — without a wasted MCP connect), then builds the
//     per-session engine via that factory and, on success, registers it under the
//     session id so StartRun routes the session's runs to it. A factory error is
//     returned as-is (the caller maps it).
//
// The per-session engine's MCP manager is torn down by CloseSession (editor
// disconnect) or by the Service's Close.
func (s *Service) LoadSessionWithMCP(ctx context.Context, id session.SessionID, specs []mcp.ServerConfig) (*session.Session, error) {
	if len(specs) == 0 {
		return s.LoadSession(ctx, id)
	}
	if s.cfg.SessionEngine == nil {
		return nil, fmt.Errorf("%w: client MCP not supported (no per-session engine configured)", ErrInvalidArgument)
	}
	// Load + reopen-if-completed BEFORE building the engine, so an unknown id fails
	// fast (ErrNotFound) without a wasted MCP connect. The reopen's Store.Save runs
	// here too.
	sess, err := s.loadAndReopen(ctx, id)
	if err != nil {
		return nil, err
	}
	// Re-mount client MCP on resume with the ZERO provider selector: a resumed
	// session keeps the DEFAULT provider (per-session provider/model binding on
	// resume is out of scope — the wire CreateSession selector is for new sessions).
	res, err := s.cfg.SessionEngine(ctx, ProviderSelector{}, specs)
	if err != nil {
		// The session was loaded + (if needed) reopened and re-persisted, but the
		// per-session engine could not be built. We deliberately do NOT roll that
		// back: the session is now just an idle session with no per-session engine —
		// exactly a no-MCP load — which is acceptable, so there is no closeFn cleanup
		// branch here (the asymmetry from CreateSessionWithMCP, where a persist
		// failure tears the freshly-built engine down).
		return nil, err
	}
	s.mu.Lock()
	// Re-load leak guard: if a per-session engine is already registered for this id
	// (e.g. a re-load of the same session on the same connection), close the prior
	// one before replacing it so its MCP manager is not orphaned.
	if prior, ok := s.sessionEngines[id]; ok && prior.close != nil {
		_ = prior.close()
	}
	s.sessionEngines[id] = &sessionEngine{engine: res.Engine, caps: res.Capabilities, close: res.Close}
	s.mu.Unlock()
	return sess, nil
}

// StartRun loads the session, builds its workspace, starts a run on the shared
// engine and registers the *agent.Run so Approve/Cancel can reach it. The
// caller is responsible for draining run.Events() AND, once the channel closes,
// for calling FinishRun(id, run) to remove the run from the registry (each wire
// adapter `defer`s FinishRun after the drain — see grpc.go/http.go/the ACP
// adapter). It returns ErrNotFound if the session does not exist.
func (s *Service) StartRun(ctx context.Context, id session.SessionID, text string) (*agent.Run, error) {
	return s.StartRunContent(ctx, id, text, nil)
}

// StartRunContent is the multimodal sibling of StartRun: it starts a run with a
// prompt carrying flattened text PLUS non-text media parts (image/audio). text
// may be "" when parts carries the content; at least one of text/parts must be
// non-empty (else ErrInvalidArgument). StartRun delegates here with nil parts.
// The media passes through to the engine untouched — command expansion and the
// UserPromptSubmit hook operate on the TEXT only (see Engine.RunContent). All
// other behaviour (workspace/engine selection, registration, drain contract) is
// identical to StartRun.
//
// It reopens-if-completed (via loadAndReopen) so a follow-up prompt on a session
// that cleanly finished a prior turn continues it — the in-process multi-turn
// counterpart to the cross-process LoadSession resume path.
func (s *Service) StartRunContent(ctx context.Context, id session.SessionID, text string, parts []session.Content) (*agent.Run, error) {
	if text == "" && len(parts) == 0 {
		return nil, fmt.Errorf("%w: prompt text or parts is required", ErrInvalidArgument)
	}
	// loadAndReopen (not GetSession): a session that cleanly completed a prior turn
	// is in StateCompleted, and the engine's RecordUserPrompt rejects a terminal
	// state — so an in-process follow-up prompt (interactive multi-turn chat, a
	// long-lived teammate) must reopen-if-completed FIRST, exactly as the
	// cross-process LoadSession resume path does. A freshly-created idle session is
	// returned unchanged; a failed/cancelled session is NOT reopened, so its illegal
	// transition still surfaces rather than silently continuing a broken session.
	sess, err := s.loadAndReopen(ctx, id)
	if err != nil {
		return nil, err
	}
	// Prefer a per-session workspace override (e.g. the ACP fs/* buffer workspace)
	// when one is registered; otherwise build one from the shared factory. AND
	// prefer a per-session engine (built over the session's client-provided MCP
	// servers) when one is registered; otherwise drive the shared engine.
	engine := s.cfg.Engine
	s.mu.Lock()
	if se, ok := s.sessionEngines[id]; ok {
		engine = se.engine
	}
	ws := s.sessionWorkspaces[id]
	s.mu.Unlock()
	if ws == nil {
		ws = s.cfg.Workspaces(sess.Workspace)
	}
	run := engine.RunContent(ctx, sess, ws, text, parts)
	s.register(id, run, sess)
	return run, nil
}

// ProviderCapabilities reports the DEFAULT provider+model's multimodal input
// support, so a surface adapter can advertise it (e.g. ACP promptCapabilities) and
// loud-reject unsupported prompt content. It returns the composition-computed
// DefaultCapabilities — the catalog ∩ adapter INTERSECTION for the default
// provider+cfg.Model — NOT the bare engine.Capabilities() (adapter-only, which
// would over-advertise a model the adapter can transmit to but the catalog says
// cannot take image). This is the SAME value the CreateSessionResponse echoes for a
// default-engine session, so the ACP gate and the wire echo cannot disagree.
//
// ACP carries NO per-session provider/model selector in P0 (session/new passes only
// mcpServers, never a selector), so every ACP session rides the DEFAULT engine and
// the Agent's capture-once a.caps = svc.ProviderCapabilities() is correct for every
// ACP session. A per-session ACP capability gate lands only when an ACP selector
// lands (P1+) — see docs/design/MULTI-PROVIDER.md.
func (s *Service) ProviderCapabilities() port.ProviderCapabilities {
	return s.cfg.DefaultCapabilities
}

// SessionCapabilities reports the resolved input capability (catalog ∩ adapter)
// for the session under id: the per-session engine's precomputed neutral caps when
// a per-session engine is registered (a non-default provider/model selector or
// client MCP), else the composition-computed DefaultCapabilities (the shared-engine
// path). The value was computed ONCE in composition (modelCapability) and stored;
// SessionCapabilities never recomputes it, so the wire echo cannot drift from the
// ListModels view. It backs the CreateSessionResponse.session_capabilities echo.
func (s *Service) SessionCapabilities(id session.SessionID) port.ProviderCapabilities {
	s.mu.Lock()
	se, ok := s.sessionEngines[id]
	s.mu.Unlock()
	if ok {
		return se.caps
	}
	return s.cfg.DefaultCapabilities
}

// LookupRun returns the in-flight run for a session and true, or false if no
// run is currently registered for it.
func (s *Service) LookupRun(id session.SessionID) (*agent.Run, bool) {
	s.mu.Lock()
	defer s.mu.Unlock()
	st, ok := s.runs[id]
	if !ok {
		return nil, false
	}
	return st.run, true
}

// Approve resolves the paused permission ask on the session's in-flight run with
// the client's three-way verdict (deny / allow-once / allow-always). If no run is
// registered in this process it consults the store: a missing session yields
// ErrNotFound; an existing-but-runless session yields ErrNoActiveRun (the stream
// was not resumable, e.g. across a restart).
func (s *Service) Approve(ctx context.Context, id session.SessionID, askID string, verdict session.ApprovalVerdict) error {
	run, ok := s.LookupRun(id)
	if ok {
		run.Approve(askID, verdict)
		return nil
	}
	return s.noActiveRun(ctx, id)
}

// Cancel cancels the session's in-flight run. The store-fallback semantics match
// Approve: ErrNotFound when the session is unknown, ErrNoActiveRun when it
// exists only in the store with no live run.
func (s *Service) Cancel(ctx context.Context, id session.SessionID) error {
	run, ok := s.LookupRun(id)
	if ok {
		run.Cancel()
		return nil
	}
	return s.noActiveRun(ctx, id)
}

// noActiveRun distinguishes "unknown session" (ErrNotFound) from "known session,
// no in-flight run" (ErrNoActiveRun) by loading from the store.
func (s *Service) noActiveRun(ctx context.Context, id session.SessionID) error {
	if _, err := s.GetSession(ctx, id); err != nil {
		return err
	}
	return ErrNoActiveRun
}

// Persist saves the current state of the session backing id, if a run is
// registered for it. It is the seam the adapters call when a run enters the
// awaiting state (so a persisted awaiting session is loadable for re-attach
// after a restart) and at run end (so the terminal state is durable). The engine
// mutates the session in place, so this captures whatever state it is in now. A
// best-effort no-op when no run is registered.
func (s *Service) Persist(ctx context.Context, id session.SessionID) {
	s.mu.Lock()
	st, ok := s.runs[id]
	s.mu.Unlock()
	if !ok {
		return
	}
	if err := s.cfg.Store.Save(ctx, st.sess); err != nil {
		// Persistence is best-effort: a Save failure must not break the live
		// stream. The run continues from in-memory state; only resume-across-
		// restart is affected.
		_ = err
	}
}

// register records run (and the live session it drives) as the in-flight run
// for id.
func (s *Service) register(id session.SessionID, run *agent.Run, sess *session.Session) {
	s.mu.Lock()
	s.runs[id] = &runState{run: run, sess: sess}
	s.mu.Unlock()
}

// deregister removes the in-flight run for id (only if it is still the one
// recorded, so a later run for the same session is never clobbered).
func (s *Service) deregister(id session.SessionID, run *agent.Run) {
	s.mu.Lock()
	if st, ok := s.runs[id]; ok && st.run == run {
		delete(s.runs, id)
	}
	s.mu.Unlock()
}

// FinishRun removes run from the in-flight registry for id. It is the EXPORTED
// counterpart of register that every wire adapter must call (typically via
// `defer`) once it has finished draining run.Events(), so a completed run does
// not leak in the registry. It is idempotent and only removes the entry if run
// is still the one recorded (a later run for the same session is never
// clobbered), so it is safe to call unconditionally after a drain.
func (s *Service) FinishRun(id session.SessionID, run *agent.Run) {
	s.deregister(id, run)
}

// randomID returns a 128-bit random hex session id.
func randomID() session.SessionID {
	var b [16]byte
	_, _ = rand.Read(b[:])
	return session.SessionID(hex.EncodeToString(b[:]))
}

// --- MCP inspection ----------------------------------------------------------
//
// These operations expose the connected MCP servers' resources/prompts and the
// resolved source inventory over the network surface. They are catalog-level
// (independent of any session/run). The provider is nil-safe: list operations
// degrade to empty, and the two operations that genuinely require a live
// provider (ReadMcpResource / GetMcpPrompt) return ErrNoMCPProvider.

// ListMcpResources returns the resource snapshots for server (empty = union of
// all servers). Returns nil with no provider configured.
func (s *Service) ListMcpResources(ctx context.Context, server string) ([]mcp.Resource, error) {
	if s.cfg.MCPProvider == nil {
		return nil, nil
	}
	res, err := s.cfg.MCPProvider.ListResources(ctx, server)
	if err != nil {
		return nil, classifyMCPError(err)
	}
	return res, nil
}

// ReadMcpResource reads a single resource by URI from the named server. server
// and uri must be non-empty; a nil provider yields ErrNoMCPProvider; an unknown
// server name yields ErrInvalidArgument; a read/transport fault on a known
// server yields ErrInternal.
func (s *Service) ReadMcpResource(ctx context.Context, server, uri string) (mcp.ResourceContents, error) {
	if server == "" || uri == "" {
		return mcp.ResourceContents{}, fmt.Errorf("%w: server and uri are required", ErrInvalidArgument)
	}
	if s.cfg.MCPProvider == nil {
		return mcp.ResourceContents{}, ErrNoMCPProvider
	}
	c, err := s.cfg.MCPProvider.ReadResource(ctx, server, uri)
	if err != nil {
		return mcp.ResourceContents{}, classifyMCPError(err)
	}
	return c, nil
}

// ListMcpPrompts returns the prompt snapshots for server (empty = union of all
// servers). Returns nil with no provider configured.
func (s *Service) ListMcpPrompts(ctx context.Context, server string) ([]mcp.Prompt, error) {
	if s.cfg.MCPProvider == nil {
		return nil, nil
	}
	ps, err := s.cfg.MCPProvider.ListPrompts(ctx, server)
	if err != nil {
		return nil, classifyMCPError(err)
	}
	return ps, nil
}

// GetMcpPrompt expands a named prompt with args on the named server. server and
// name must be non-empty; a nil provider yields ErrNoMCPProvider; an unknown
// server name yields ErrInvalidArgument; an unknown prompt, missing required
// arg, or other expansion fault on a known server yields ErrInternal.
func (s *Service) GetMcpPrompt(ctx context.Context, server, name string, args map[string]string) (mcp.PromptResult, error) {
	if server == "" || name == "" {
		return mcp.PromptResult{}, fmt.Errorf("%w: server and name are required", ErrInvalidArgument)
	}
	if s.cfg.MCPProvider == nil {
		return mcp.PromptResult{}, ErrNoMCPProvider
	}
	res, err := s.cfg.MCPProvider.GetPrompt(ctx, server, name, args)
	if err != nil {
		return mcp.PromptResult{}, classifyMCPError(err)
	}
	return res, nil
}

// classifyMCPError maps a provider error to the right service sentinel so the
// network surfaces can distinguish a client mistake from a downstream fault. An
// unknown-server name (mcp.ErrUnknownServer) is genuinely a client error →
// ErrInvalidArgument (InvalidArgument / HTTP 400). Anything else is a
// transport/protocol fault on a connected server → ErrInternal (Internal /
// HTTP 500). Empty-required-field validation is handled by the callers before
// the provider is consulted and stays InvalidArgument.
func classifyMCPError(err error) error {
	if errors.Is(err, mcp.ErrUnknownServer) {
		return fmt.Errorf("%w: %v", ErrInvalidArgument, err)
	}
	return fmt.Errorf("%w: %v", ErrInternal, err)
}

// ListMcpSources returns the MCP source inventory (possibly empty). When a
// MCPSourceProber is configured it RE-CONSULTS the resolved sources for live
// status/diagnostics on every call (so a client refresh reflects current state,
// not the startup snapshot); on a prober that returns nil it falls back to the
// cached startup snapshot so the panel always renders. With no prober it is a
// pure read of the injected snapshot (no live discovery).
func (s *Service) ListMcpSources(ctx context.Context) []source.SourceInfo {
	return s.liveSources(ctx)
}

// liveSources returns the freshest inventory available: the prober's result when
// it is configured and yields anything, otherwise the cached startup snapshot.
// Centralising this keeps ListMcpSources and ListToolHiveGroups consistent — a
// refresh that re-probes sources is reflected in both the panel and the groups.
func (s *Service) liveSources(ctx context.Context) []source.SourceInfo {
	if s.cfg.MCPSourceProber != nil {
		if probed := s.cfg.MCPSourceProber(ctx); probed != nil {
			return probed
		}
	}
	return s.cfg.MCPSources
}

// ListToolHiveGroups derives the distinct, non-empty ToolHive groups from the
// inventory (the live re-probe when a MCPSourceProber is set, else the startup
// snapshot — see liveSources). It considers only sources whose Kind is
// "toolhive". Output is sorted for deterministic results.
func (s *Service) ListToolHiveGroups(ctx context.Context) []string {
	seen := make(map[string]struct{})
	var groups []string
	for _, src := range s.liveSources(ctx) {
		if src.Kind != "toolhive" || src.Group == "" {
			continue
		}
		if _, ok := seen[src.Group]; ok {
			continue
		}
		seen[src.Group] = struct{}{}
		groups = append(groups, src.Group)
	}
	sort.Strings(groups)
	return groups
}

// ListAgents returns the resolved agent-definition snapshot (possibly empty).
// It is a pure read of the injected snapshot; no live discovery.
func (s *Service) ListAgents(_ context.Context) []*mecatlv1.AgentInfo {
	return s.cfg.Agents
}

// ListSkills returns the resolved skills-inventory snapshot (possibly empty).
// It is a pure read of the injected snapshot; no live discovery.
func (s *Service) ListSkills(_ context.Context) []*mecatlv1.SkillInfo {
	return s.cfg.Skills
}

// ListModels returns the resolved selectable-model inventory snapshot (possibly
// empty) — every available provider's catalog models, secret-free. It is a pure
// read of the injected snapshot; no live discovery (multi-provider Phase 0, S3).
func (s *Service) ListModels(_ context.Context) []*mecatlv1.ModelInfo {
	return s.cfg.Models
}

// --- Soul + user-model inspection --------------------------------------------

// GetSoul returns the resolved soul (persona) snapshot (the build-time
// projection injected via Config.Soul). It is a pure read of that snapshot; no
// live re-read. When no soul source is wired it returns an empty snapshot
// (present=false), never nil, so the wire adapters always have a SoulInfo to
// serialize.
func (s *Service) GetSoul(_ context.Context) *mecatlv1.SoulInfo {
	if s.cfg.Soul == nil {
		return &mecatlv1.SoulInfo{}
	}
	return s.cfg.Soul
}

// UserModelEntry is the surface-agnostic listing metadata for one user-model
// fact (key + description, value omitted), mirroring tool.MemoryEntry's index
// shape. The Service exposes its own type so the wire adapters and the
// composition seam (UserModelLister) need not import the memory adapter.
type UserModelEntry struct {
	// Key is the entry's stable key.
	Key string
	// Description is the entry's one-line description.
	Description string
}

// UserModelLister enumerates the CURRENT user-model entries (key + description,
// value omitted). It is the composition-injected seam backing GetUserModel: the
// composition root closes over the user-model store's Index so a fetch reflects
// the live store state. It is read-only.
type UserModelLister interface {
	// List returns the user-model entries, key-sorted, or an error on a genuine
	// store fault.
	List(ctx context.Context) ([]UserModelEntry, error)
}

// GetUserModel returns the CURRENT user-model entries (a live read of the wired
// lister) plus aggregate size + hash over the rendered "key — description" rows.
// A nil lister (user model disabled) yields an empty response. A store fault is
// returned as ErrInternal so the wire adapters surface it distinctly.
func (s *Service) GetUserModel(ctx context.Context) (*mecatlv1.GetUserModelResponse, error) {
	if s.cfg.UserModel == nil {
		return &mecatlv1.GetUserModelResponse{}, nil
	}
	entries, err := s.cfg.UserModel.List(ctx)
	if err != nil {
		return nil, fmt.Errorf("%w: list user model: %v", ErrInternal, err)
	}
	out := make([]*mecatlv1.UserModelEntry, 0, len(entries))
	var agg strings.Builder
	for _, e := range entries {
		out = append(out, &mecatlv1.UserModelEntry{Key: e.Key, Description: e.Description})
		agg.WriteString(e.Key)
		agg.WriteByte('\t')
		agg.WriteString(e.Description)
		agg.WriteByte('\n')
	}
	sum := sha256.Sum256([]byte(agg.String()))
	return &mecatlv1.GetUserModelResponse{
		Entries:   out,
		SizeBytes: int64(agg.Len()),
		Sha256:    hex.EncodeToString(sum[:]),
	}, nil
}

// --- Slash command discovery -------------------------------------------------

// Command is the surface-agnostic listing metadata for one slash command (name +
// short description), mirroring prompt.Command. The Service exposes its own type
// so the wire adapters and the composition seam (CommandLister) need not import
// the prompt domain package directly.
type Command struct {
	// Name is the command's invocation name (without the leading "/").
	Name string
	// Description is a short, capped one-line summary for the palette.
	Description string
}

// CommandLister enumerates the slash commands available under a workspace root.
// It is the composition-injected discovery seam backing ListCommands: the
// composition root supplies an implementation that closes over the run-path
// command expander and the workspace factory, so the palette and the run path
// agree on which commands exist. It is read-only.
type CommandLister interface {
	// List returns the commands discovered under root, de-duplicated by name and
	// name-sorted, or an error on a genuine discovery fault.
	List(ctx context.Context, root string) ([]Command, error)
}

// ListCommands returns the available slash commands for the given workspace
// root. An empty root, a nil lister (command expansion disabled), or a lister
// that enumerates nothing all yield an empty slice. A discovery fault from the
// lister is returned as ErrInternal so the wire adapters surface it distinctly.
func (s *Service) ListCommands(ctx context.Context, workspace string) ([]Command, error) {
	if s.cfg.Commands == nil || workspace == "" {
		return nil, nil
	}
	cmds, err := s.cfg.Commands.List(ctx, workspace)
	if err != nil {
		return nil, fmt.Errorf("%w: list commands: %v", ErrInternal, err)
	}
	return cmds, nil
}
