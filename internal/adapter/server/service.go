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
// the shared engine, and keeps a registry of in-flight *agent.Run keyed by
// session id so out-of-band approve/cancel reach the right run. It is safe for
// concurrent use.
type Service struct {
	cfg Config

	mu   sync.Mutex
	runs map[session.SessionID]*agent.Run
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
	return &Service{cfg: cfg, runs: make(map[session.SessionID]*agent.Run)}, nil
}

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
	s.register(id, run)
	return run, nil
}

// LookupRun returns the in-flight run for a session and true, or false if no
// run is currently registered for it.
func (s *Service) LookupRun(id session.SessionID) (*agent.Run, bool) {
	s.mu.Lock()
	defer s.mu.Unlock()
	run, ok := s.runs[id]
	return run, ok
}

// register records run as the in-flight run for id.
func (s *Service) register(id session.SessionID, run *agent.Run) {
	s.mu.Lock()
	s.runs[id] = run
	s.mu.Unlock()
}

// deregister removes the in-flight run for id (only if it is still the one
// recorded, so a later run for the same session is never clobbered).
func (s *Service) deregister(id session.SessionID, run *agent.Run) {
	s.mu.Lock()
	if s.runs[id] == run {
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
