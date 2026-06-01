package server

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"errors"
	"fmt"
	"sort"
	"sync"
	"time"

	mecatlv1 "github.com/stacklok/mecatl/contracts/gen/go/mecatl/v1"
	"github.com/stacklok/mecatl/internal/adapter/mcp"
	"github.com/stacklok/mecatl/internal/adapter/mcp/source"
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

// SessionEngineFactory builds a PER-SESSION agent engine over the client-provided
// streaming-HTTP MCP servers (specs), returning the engine, a close func that
// tears down that session's MCP manager, and an error. It is the seam the ACP
// adapter uses to mount an editor's session/new mcpServers for the lifetime of
// one session, WITHOUT leaking those tools (or their auth) into the shared engine
// every other session uses. The composition root (internal/app) supplies it via
// Config.SessionEngine; when nil, CreateSessionWithMCP rejects any non-empty
// specs with ErrInvalidArgument. It mirrors MemberEngineFactory: the Service
// references the type in its signatures but never builds managers itself — the
// app layer is the only place mcp + agent are wired together.
type SessionEngineFactory func(ctx context.Context, specs []mcp.ServerConfig) (*agent.Engine, func() error, error)

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

	// SessionEngine builds a PER-SESSION engine over client-provided streaming-HTTP
	// MCP servers (the ACP session/new mcpServers). It is the seam that lets a
	// session mount its OWN MCP tools without leaking them into the shared Engine
	// every other session uses. When nil, CreateSessionWithMCP rejects any non-empty
	// MCP specs with ErrInvalidArgument; a session with no client MCP always uses the
	// shared Engine (zero overhead). The composition root (internal/app) supplies it.
	SessionEngine SessionEngineFactory

	// MemberEngine builds a team member's Engine from the shared team and the
	// member spec (see internal/agent.MemberEngine). It is the seam that wires
	// the agent-team RPCs: when nil, those RPCs return ErrTeamsDisabled. The
	// composition root supplies it (internal/app), capturing the per-member
	// catalog (read-only base + MemberTools, plus mutating tools only for a
	// Mutating member) and the provider/model.
	MemberEngine MemberEngineFactory
	// Forker isolates a Mutating team member's workspace. Optional; required only
	// if a Mutating member is spawned.
	Forker tool.WorkspaceForker
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
}

// defaultMaxTeams is the live-team registry cap applied when Config.MaxTeams is
// zero. It bounds memory growth from teams that are created but never cleaned up.
const defaultMaxTeams = 64

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
	// sessionEngines holds the per-session client-MCP engines. Unlike teams (capped
	// by MaxTeams), it is bounded by CONNECTION LIFETIME, not a count: the ACP
	// adapter calls CloseSession for each tracked session when the editor
	// disconnects (closeTrackedSessions), and Service.Close drains the rest on
	// shutdown — so no speculative cap is warranted.
	sessionEngines map[session.SessionID]*sessionEngine
}

// sessionEngine couples a per-session engine (built over that session's
// client-provided MCP servers) with the close func that tears down its MCP
// manager. It is registered by CreateSessionWithMCP and released by CloseSession
// (and by the Service's own Close). StartRun prefers it over the shared engine
// for the owning session id.
type sessionEngine struct {
	engine *agent.Engine
	close  func() error
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
	return &Service{
		cfg:            cfg,
		runs:           make(map[session.SessionID]*runState),
		teams:          make(map[string]*teamState),
		sessionEngines: make(map[session.SessionID]*sessionEngine),
	}, nil
}

// ErrNoActiveRun is returned by Approve/Cancel when the session exists (possibly
// loaded from the store after a restart) but has no in-flight run in this
// process to deliver the control to. The session state is still loadable via
// GetSession; the lost stream simply cannot be resumed in place.
var ErrNoActiveRun = errors.New("server: no active run for session")

// CreateSession allocates a new idle session, persists it, and returns it.
// workspace must be non-empty. An unspecified mode falls back to DefaultMode.
func (s *Service) CreateSession(ctx context.Context, workspace string, mode session.PermissionMode, limits session.Limits) (*session.Session, error) {
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
	sess := session.New(s.cfg.NewID(), mode, workspace, limits, s.cfg.Now())
	if err := s.cfg.Store.Save(ctx, sess); err != nil {
		return nil, fmt.Errorf("server: persist session: %w", err)
	}
	return sess, nil
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
	if len(specs) == 0 {
		return s.CreateSession(ctx, workspace, mode, limits)
	}
	if s.cfg.SessionEngine == nil {
		return nil, fmt.Errorf("%w: client MCP not supported (no per-session engine configured)", ErrInvalidArgument)
	}
	if workspace == "" {
		return nil, fmt.Errorf("%w: workspace is required", ErrInvalidArgument)
	}
	eng, closeFn, err := s.cfg.SessionEngine(ctx, specs)
	if err != nil {
		return nil, err
	}
	if mode == "" {
		mode = s.cfg.DefaultMode
	}
	if limits == (session.Limits{}) {
		limits = s.cfg.DefaultLimits
	}
	sess := session.New(s.cfg.NewID(), mode, workspace, limits, s.cfg.Now())
	if serr := s.cfg.Store.Save(ctx, sess); serr != nil {
		// The engine was built but the session could not be persisted: tear the
		// per-session MCP manager down so a failed create never leaks it.
		if closeFn != nil {
			_ = closeFn()
		}
		return nil, fmt.Errorf("server: persist session: %w", serr)
	}
	s.mu.Lock()
	s.sessionEngines[sess.ID] = &sessionEngine{engine: eng, close: closeFn}
	s.mu.Unlock()
	return sess, nil
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
	s.mu.Lock()
	se, ok := s.sessionEngines[id]
	if ok {
		delete(s.sessionEngines, id)
	}
	s.mu.Unlock()
	if ok && se.close != nil {
		_ = se.close()
	}
}

// Close tears down all per-session engines' MCP managers. It is the Service's
// shutdown hook so a process exit does not leak any per-session MCP connection.
// It is safe to call multiple times.
func (s *Service) Close() {
	s.mu.Lock()
	engines := s.sessionEngines
	s.sessionEngines = make(map[session.SessionID]*sessionEngine)
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

// StartRun loads the session, builds its workspace, starts a run on the shared
// engine and registers the *agent.Run so Approve/Cancel can reach it. The
// caller is responsible for draining run.Events() AND, once the channel closes,
// for calling FinishRun(id, run) to remove the run from the registry (each wire
// adapter `defer`s FinishRun after the drain — see grpc.go/http.go/the ACP
// adapter). It returns ErrNotFound if the session does not exist.
func (s *Service) StartRun(ctx context.Context, id session.SessionID, text string) (*agent.Run, error) {
	if text == "" {
		return nil, fmt.Errorf("%w: prompt text is required", ErrInvalidArgument)
	}
	sess, err := s.GetSession(ctx, id)
	if err != nil {
		return nil, err
	}
	ws := s.cfg.Workspaces(sess.Workspace)
	// Prefer a per-session engine (built over the session's client-provided MCP
	// servers) when one is registered; otherwise drive the shared engine.
	engine := s.cfg.Engine
	s.mu.Lock()
	if se, ok := s.sessionEngines[id]; ok {
		engine = se.engine
	}
	s.mu.Unlock()
	run := engine.Run(ctx, sess, ws, text)
	s.register(id, run, sess)
	return run, nil
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

// Approve resolves the paused permission ask on the session's in-flight run. If
// no run is registered in this process it consults the store: a missing session
// yields ErrNotFound; an existing-but-runless session yields ErrNoActiveRun
// (the stream was not resumable, e.g. across a restart).
func (s *Service) Approve(ctx context.Context, id session.SessionID, askID string, allow bool) error {
	run, ok := s.LookupRun(id)
	if ok {
		run.Approve(askID, allow)
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
