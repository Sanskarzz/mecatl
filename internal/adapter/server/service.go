package server

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"errors"
	"fmt"
	"sync"
	"time"

	"github.com/stacklok/ozzharness/internal/agent"
	"github.com/stacklok/ozzharness/internal/port"
	"github.com/stacklok/ozzharness/internal/session"
	"github.com/stacklok/ozzharness/internal/tool"
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
}

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

	mu   sync.Mutex
	runs map[session.SessionID]*runState
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
	return &Service{cfg: cfg, runs: make(map[session.SessionID]*runState)}, nil
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

// GetSession returns the persisted session under id, or ErrNotFound.
func (s *Service) GetSession(ctx context.Context, id session.SessionID) (*session.Session, error) {
	sess, err := s.cfg.Store.Load(ctx, id)
	if err != nil {
		return nil, fmt.Errorf("%w: %q", ErrNotFound, id)
	}
	return sess, nil
}

// StartRun loads the session, builds its workspace, starts a run on the shared
// engine and registers the *agent.Run so Approve/Cancel can reach it. The
// caller is responsible for draining run.Events(); the run is automatically
// de-registered when its event channel closes (see relay). It returns
// ErrNotFound if the session does not exist.
func (s *Service) StartRun(ctx context.Context, id session.SessionID, text string) (*agent.Run, error) {
	if text == "" {
		return nil, fmt.Errorf("%w: prompt text is required", ErrInvalidArgument)
	}
	sess, err := s.GetSession(ctx, id)
	if err != nil {
		return nil, err
	}
	ws := s.cfg.Workspaces(sess.Workspace)
	run := s.cfg.Engine.Run(ctx, sess, ws, text)
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

// randomID returns a 128-bit random hex session id.
func randomID() session.SessionID {
	var b [16]byte
	_, _ = rand.Read(b[:])
	return session.SessionID(hex.EncodeToString(b[:]))
}
