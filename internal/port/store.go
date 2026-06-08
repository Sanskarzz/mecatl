package port

import (
	"context"
	"errors"

	"github.com/stacklok/mecatl/internal/session"
)

// ErrSessionNotFound is the port-level sentinel a SessionStore.Load wraps (with %w)
// when no session is stored under the requested id — distinct from a genuine
// infrastructure failure (I/O error, decode failure). It lets a consumer in a layer
// that may NOT import the store adapters (e.g. internal/agent's InspectMemberTool)
// distinguish "no such session" from "the store is broken" via errors.Is, without
// reaching for an adapter's own not-found sentinel. Every SessionStore adapter MUST
// wrap this for the not-found case.
var ErrSessionNotFound = errors.New("port: session not found")

// SessionStore persists and retrieves server-side session state, enabling
// pause/resume and reload. Adapters provide an in-memory store (default) and an
// append-only JSONL replay log.
type SessionStore interface {
	// Save persists the current state of s.
	Save(ctx context.Context, s *session.Session) error
	// Load retrieves the session with the given id. The not-found case MUST wrap
	// port.ErrSessionNotFound; any other error is an infrastructure failure.
	Load(ctx context.Context, id session.SessionID) (*session.Session, error)
}
