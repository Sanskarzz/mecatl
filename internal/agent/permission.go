package agent

import (
	"context"
	"sync"
)

// approval is a client's resolution of a permission.ask: allow or deny.
type approval struct {
	allow bool
}

// askRegistry brokers the blocking-and-resume handshake between the loop
// goroutine (which pauses on a permission.ask and waits) and Run.Approve (which
// the API calls out-of-band to resolve it). Each pending ask owns a single
// buffered channel; Approve writes the verdict, await reads it. It is safe for
// concurrent use.
type askRegistry struct {
	mu      sync.Mutex
	pending map[string]chan approval
}

// newAskRegistry constructs an empty registry.
func newAskRegistry() *askRegistry {
	return &askRegistry{pending: make(map[string]chan approval)}
}

// register creates and stores a resolution channel for askID before the loop
// emits the permission.ask Event, so an Approve that races in immediately after
// the event is observed cannot be lost. It returns the channel the loop awaits.
func (r *askRegistry) register(askID string) <-chan approval {
	ch := make(chan approval, 1)
	r.mu.Lock()
	r.pending[askID] = ch
	r.mu.Unlock()
	return ch
}

// resolve delivers a verdict for askID if one is pending. It is non-blocking and
// idempotent: a second resolution (or one for an unknown ask) is dropped. It
// removes the ask from the registry so a stale Approve cannot resolve a later,
// distinct ask that happens to reuse an id.
func (r *askRegistry) resolve(askID string, allow bool) {
	r.mu.Lock()
	ch, ok := r.pending[askID]
	if ok {
		delete(r.pending, askID)
	}
	r.mu.Unlock()
	if !ok {
		return
	}
	// ch is buffered (cap 1) and only ever written once per ask, so this never
	// blocks.
	ch <- approval{allow: allow}
}

// discard drops a pending ask without resolving it. The loop calls this when an
// await is abandoned (e.g. ctx cancel) so the registry does not leak entries.
func (r *askRegistry) discard(askID string) {
	r.mu.Lock()
	delete(r.pending, askID)
	r.mu.Unlock()
}

// await blocks until the client resolves askID via resolve, or ctx is cancelled.
// It reports the allow verdict and ok=true on resolution; ok=false means the
// wait was abandoned (ctx cancelled), in which case the caller should end the
// run as cancelled. The ask is removed from the registry on either path.
func (r *askRegistry) await(ctx context.Context, askID string, ch <-chan approval) (allow bool, ok bool) {
	select {
	case a := <-ch:
		return a.allow, true
	case <-ctx.Done():
		r.discard(askID)
		return false, false
	}
}
