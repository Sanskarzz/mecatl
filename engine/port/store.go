package port

import (
	"context"
	"errors"
	"time"

	"github.com/stacklok/mecatl/engine/session"
)

// ErrSessionNotFound is the port-level sentinel a SessionStore.Load wraps (with %w)
// when no session is stored under the requested id — distinct from a genuine
// infrastructure failure (I/O error, decode failure). It lets a consumer in a layer
// that may NOT import the store adapters (e.g. engine/agent's InspectMemberTool)
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

// StoredSession is one stored session's retention-relevant identity: its id
// plus when its snapshot was last modified (Save time, file mtime, or the
// store's nearest equivalent). It deliberately carries NO session content —
// listing is a retention/inventory concern, never a load.
type StoredSession struct {
	ID         session.SessionID
	ModifiedAt time.Time
}

// ErrPruneUnsupported is the port-level sentinel a PrunableStore's List or
// Delete wraps (with %w) when the store's BACKEND cannot enumerate/delete
// sessions at all — e.g. a remote driver answering UNIMPLEMENTED. It is the
// "this seam will never work here" signal, distinct from a transient
// infrastructure failure (I/O error, timeout): a consumer that sees it via
// errors.Is should stop consulting the seam (the composition layer's
// child-session GC logs one INFO and stickily disables further sweeps),
// whereas any other error is retried on the next sweep.
var ErrPruneUnsupported = errors.New("port: store does not support retention pruning")

// PrunableStore is the OPTIONAL retention seam a SessionStore adapter may
// additionally implement. It is a separate interface — SessionStore itself
// stays the minimal Save/Load pair — and consumers discover it by type
// assertion: a store that does not implement it is simply never swept (the
// composition-layer child-session GC degrades to a no-op).
//
// Contract:
//   - List returns ALL stored session ids (with their last-modified times).
//     It applies NO filtering — retention policy (which ids are prunable,
//     age thresholds, per-family caps) is entirely the CALLER's business.
//   - Delete removes the snapshot stored under id, plus any sidecar records
//     the adapter keeps for it (e.g. jsonlstore's tool-call log). It is
//     IDEMPOTENT: deleting an unknown id succeeds — callers tolerate
//     List/Delete races by construction.
type PrunableStore interface {
	// List returns every stored session's id and last-modified time, in no
	// guaranteed order.
	List(ctx context.Context) ([]StoredSession, error)
	// Delete removes the session stored under id. An unknown id is success
	// (idempotent); any returned error is an infrastructure failure.
	Delete(ctx context.Context, id session.SessionID) error
}
