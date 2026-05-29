package port

import (
	"context"

	"github.com/stacklok/mecatl/internal/session"
)

// SessionStore persists and retrieves server-side session state, enabling
// pause/resume and reload. Adapters provide an in-memory store (default) and an
// append-only JSONL replay log.
type SessionStore interface {
	// Save persists the current state of s.
	Save(ctx context.Context, s *session.Session) error
	// Load retrieves the session with the given id.
	Load(ctx context.Context, id session.SessionID) (*session.Session, error)
}
