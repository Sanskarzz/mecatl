// Package memstore implements an in-memory, concurrency-safe port.SessionStore.
// It is the default store: fast and offline, suitable for single-process use and
// tests. Sessions are keyed by session.SessionID in a mutex-guarded map.
//
// Save and Load deep-copy the session through a sessnap snapshot round-trip, so
// a stored session cannot be mutated through a reference the caller still holds
// (and vice versa). This keeps the store's copy authoritative and isolated.
package memstore

import (
	"context"
	"fmt"
	"sync"

	"github.com/stacklok/ozzharness/internal/adapter/store/sessnap"
	"github.com/stacklok/ozzharness/internal/port"
	"github.com/stacklok/ozzharness/internal/session"
)

// ErrNotFound is returned by Load when no session is stored under the given id.
var ErrNotFound = fmt.Errorf("memstore: session not found")

// Store is a concurrency-safe in-memory SessionStore.
type Store struct {
	mu       sync.RWMutex
	sessions map[session.SessionID]sessnap.Snapshot
}

// compile-time assertion that Store satisfies the port.
var _ port.SessionStore = (*Store)(nil)

// New constructs an empty in-memory Store.
func New() *Store {
	return &Store{sessions: make(map[session.SessionID]sessnap.Snapshot)}
}

// Save persists a deep copy of s under s.ID, overwriting any prior state.
func (st *Store) Save(_ context.Context, s *session.Session) error {
	if s == nil {
		return sessnap.ErrNilSession
	}
	snap, err := sessnap.Of(s)
	if err != nil {
		return err
	}
	st.mu.Lock()
	st.sessions[s.ID] = snap
	st.mu.Unlock()
	return nil
}

// Load returns a freshly reconstructed copy of the session stored under id. It
// returns ErrNotFound if no such session exists.
func (st *Store) Load(_ context.Context, id session.SessionID) (*session.Session, error) {
	st.mu.RLock()
	snap, ok := st.sessions[id]
	st.mu.RUnlock()
	if !ok {
		return nil, fmt.Errorf("%w: %q", ErrNotFound, id)
	}
	return snap.Restore()
}
