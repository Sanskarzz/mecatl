package agent

import (
	"context"
	"sync"

	"github.com/stacklok/mecatl/internal/session"
)

// childFamily discriminates which delegation family a registered child belongs
// to. The registry indexes all three families in ONE flat map keyed by the child
// SESSION id (the agentId trailer / overlay ChildID / store key — the single
// handle convention); the families' id prefixes ("subagent-"/"parallel-"/
// "team-") are disjoint by the existing convention, which the registry inherits
// rather than re-engineers.
type childFamily string

const (
	// childFamilySubagent is a flat-fleet Subagent child ("subagent-<callID>").
	childFamilySubagent childFamily = "subagent"
)

// childState is a registered child's lifecycle position: queued (registered,
// possibly waiting on the concurrency gate), or done (terminal; doneCh closed).
// A "running" refinement (for the SubagentStatus tool) is a later iteration;
// cancel semantics only distinguish done from not-done.
type childState int

const (
	childQueued childState = iota
	childDone
)

// childRunRegistry tracks every child run spawned under one parent Run, keyed by
// the child session id. It is the shared foundation for per-child cancel (this
// iteration) and background subagents (a later one): cancel consumes
// cancel/clientCancelled/askIDs; background will consume state/result/delivered/
// doneCh and the seal. It is owned by the parent Run exactly as childAsks is,
// but created UNCONDITIONALLY (cancel arrives only on interactive surfaces, but
// background bookkeeping must work headless too, and the registry is a mutex +
// map — negligible).
//
// LOCKING: two mutexes for two concerns, never held together by the registry.
// `mu` guards the entries map (registration/markDone/recordAsk/requestCancel —
// always short, never across a channel send). `emitMu` guards the seal flag AND
// the retract send as ONE locked section (A4a: no TOCTOU between the sealed
// check and the send). The split means a retract send that waits on the events
// channel can never wedge entry bookkeeping (markDone, a sibling's
// registration, a later drain) behind it.
//
// Entries are never removed during the run — done entries are what a later
// status/delivery reader consumes — and the whole map dies with the Run.
type childRunRegistry struct {
	// mu guards entries (and the per-entry fields). Never held across emit.
	mu      sync.Mutex
	entries map[string]*childEntry

	// emitMu guards sealed + the retract send in one locked section (A4a), so a
	// late emit can never race seal/close into a send-on-closed-channel panic.
	// seal() therefore also serialises against an in-flight emit. Distinct from
	// mu so a send waiting on the events channel never blocks entry bookkeeping.
	emitMu sync.Mutex
	// sealed is set (via seal) just before the run's events channel closes. Full
	// run-end drain logic (joining background children) is a later iteration; the
	// seal alone is what cancel's retract emission needs to be panic-free.
	sealed bool
	// emit publishes an event toward the parent Run's stream. RunContentWith binds
	// it to Run.tryEmit (+ the sink mirror): a NON-BLOCKING-on-teardown send that
	// gives up when the run context ends, so a wedged consumer cannot park the
	// readControl goroutine inside CancelChild forever. It is set once before the
	// run goroutine starts and only read after; emitMu guards each call. nil
	// (tests) silently drops.
	emit func(session.Event)
}

// childEntry is one registered child's control block.
type childEntry struct {
	family childFamily
	// goal is the clamped, plain-text label registered for overlay/status
	// rendering (a later iteration's SubagentStatus reads it; never child content).
	goal string
	// cancel is the per-CHILD context cancel covering the whole call (gate wait,
	// fork, drive, structured-output re-drives). Invoked OUTSIDE the registry lock.
	cancel context.CancelFunc
	// background marks a detached child (dormant this iteration; the Subagent tool
	// always registers false until the background flag lands).
	background bool
	// clientCancelled is set by Run.CancelChild — it disambiguates the child's
	// StopCancelled terminal (cancelled BY THE USER vs parent-run cancel/timeout).
	clientCancelled bool
	// askIDs are the surfaced permission asks currently owned by this child,
	// recorded at the single surfacing seam (surfaceAsk → recordAsk) and snapshot+
	// cleared by requestCancel so each gets a permission.retract. Verdict routing
	// leaves the set slightly stale (an answered ask is not removed); harmless —
	// retraction tolerates already-resolved ids (the router delete and the client
	// dismiss are both idempotent).
	askIDs map[string]struct{}
	state  childState
	// stop is the child's terminal stop reason (set by markDone).
	stop session.StopReason
	// result / delivered are the background-delivery fields (rendered result
	// awaiting delivery; exactly-once accounting). Present per the registry design
	// but dormant until the background iteration wires them.
	result    *session.ToolResult //nolint:unused // background delivery (later iteration) consumes it
	delivered bool                //nolint:unused // background delivery (later iteration) consumes it
	// doneCh is closed exactly once at the child's terminal (markDone); the
	// run-end drain (later iteration) and tests join on it.
	doneCh chan struct{}
}

// newChildRunRegistry constructs an empty registry.
func newChildRunRegistry() *childRunRegistry {
	return &childRunRegistry{entries: make(map[string]*childEntry)}
}

// register records a child under its session id BEFORE the child acquires its
// concurrency slot, so a child queued on the gate is already cancellable.
// Re-registration of an existing id (a `resume` of an already-run child within
// the SAME parent run) OVERWRITES the old entry with a fresh doneCh — the old
// entry's doneCh was closed at its terminal and is never touched again, so a
// double-close is structurally impossible.
//
//nolint:unparam // background is the background-subagents iteration's knob; every I1 caller passes false.
func (g *childRunRegistry) register(childID string, family childFamily, goal string, cancel context.CancelFunc, background bool) {
	g.mu.Lock()
	defer g.mu.Unlock()
	g.entries[childID] = &childEntry{
		family:     family,
		goal:       goal,
		cancel:     cancel,
		background: background,
		askIDs:     make(map[string]struct{}),
		state:      childQueued,
		doneCh:     make(chan struct{}),
	}
}

// markDone records the child's terminal stop and closes its doneCh. It is
// idempotent: a second markDone for the same entry (or one for an unknown id)
// is a no-op, so doneCh is never double-closed.
func (g *childRunRegistry) markDone(childID string, stop session.StopReason) {
	g.mu.Lock()
	defer g.mu.Unlock()
	e, ok := g.entries[childID]
	if !ok || e.state == childDone {
		return
	}
	e.state = childDone
	e.stop = stop
	close(e.doneCh)
}

// recordAsk records that the (live) child owns a surfaced ask, so a later
// CancelChild can retract it. Unknown or already-done ids are ignored (a
// team-member/parallel-branch ask in this iteration — those families register
// in a later one — or a terminal race).
func (g *childRunRegistry) recordAsk(childID, askID string) {
	g.mu.Lock()
	defer g.mu.Unlock()
	e, ok := g.entries[childID]
	if !ok || e.state == childDone {
		return
	}
	e.askIDs[askID] = struct{}{}
}

// requestCancel marks the child client-cancelled and returns its cancel func
// plus a snapshot of its owned askIDs (cleared from the entry, so a second
// cancel never re-retracts). It returns ok=false for an unknown or already-done
// id — CancelChild is a no-op then. The returned cancel MUST be invoked outside
// the registry lock (it may synchronously unwind code paths that re-enter the
// registry, e.g. markDone).
func (g *childRunRegistry) requestCancel(childID string) (cancel context.CancelFunc, askIDs []string, ok bool) {
	g.mu.Lock()
	defer g.mu.Unlock()
	e, found := g.entries[childID]
	if !found || e.state == childDone {
		return nil, nil, false
	}
	e.clientCancelled = true
	for id := range e.askIDs {
		askIDs = append(askIDs, id)
	}
	clear(e.askIDs)
	return e.cancel, askIDs, true
}

// clientCancelled reports whether CancelChild was requested for the child. The
// Subagent tool reads it (via parentCaps.children) to render the
// "[subagent cancelled by user]" terminal note, disambiguating a client cancel
// from a parent-run cancel or a per-call timeout.
func (g *childRunRegistry) clientCancelled(childID string) bool {
	g.mu.Lock()
	defer g.mu.Unlock()
	e, ok := g.entries[childID]
	return ok && e.clientCancelled
}

// seal marks the registry closed for emission. It is called by the run goroutine
// just before the events channel closes (AFTER the run context is cancelled, so
// an in-flight retract send that is waiting on a full channel aborts via its
// ctx select rather than holding emitMu); because seal takes the same emitMu the
// emit path holds across its send, an in-flight emitRetract completes (or
// aborts) before the channel close proceeds, and every later emit is a safe
// no-op.
func (g *childRunRegistry) seal() {
	g.emitMu.Lock()
	g.sealed = true
	g.emitMu.Unlock()
}

// emitRetract publishes a permission.retract event for one withdrawn askID on
// the parent stream. The sealed check AND the send happen inside one emitMu
// section (no TOCTOU against seal/close — A4a); the entries mutex is NOT held
// here, so a send waiting on the events channel never wedges entry bookkeeping.
// The bound emit (Run.tryEmit) gives up when the run context ends: retraction
// is a UX courtesy — the router unregister already happened — so dropping it
// against a wedged/teardown consumer is acceptable. The payload is
// server-authored and carries the AskID only.
func (g *childRunRegistry) emitRetract(askID string) {
	g.emitMu.Lock()
	defer g.emitMu.Unlock()
	if g.sealed || g.emit == nil {
		return
	}
	g.emit(session.Event{Type: session.EvPermissionRetract, Ask: &session.PendingAsk{AskID: askID}})
}
