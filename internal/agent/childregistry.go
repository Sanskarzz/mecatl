package agent

import (
	"context"
	"sort"
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
	// childFamilyParallelBranch is one Parallel fork-join branch
	// ("parallel-<callID>-<i>").
	childFamilyParallelBranch childFamily = "parallel-branch"
	// childFamilyTeamMember is one long-lived team member ("team-<teamID>-<member>",
	// MemberSessionID). Unlike the one-shot families its entry stays live ACROSS
	// rounds (idle-between-rounds is still cancellable) and is marked done when the
	// supervisor stops it or the team ends.
	//
	// CAUTION — for THIS family, done/doneCh means DE-SCHEDULED, not quiescent: a
	// budget-stopped LEAD is marked done by the supervisor's stop path yet is
	// deliberately driven ONE more time for the synthesis turn (its per-member ctx
	// is not cancelled on stop for exactly that reason — see Supervisor.runTurn /
	// cleanupAll). Any reader of done state (the SubagentStatus tool, a
	// background-team future) must NOT treat a team-member done entry as
	// fully-terminal; only the TEAM's own end (cleanupAll) is.
	childFamilyTeamMember childFamily = "team-member"
)

// childState is a registered child's lifecycle position: queued (registered,
// possibly waiting on the concurrency gate), running (its drive has started; a
// team member stays running across rounds), or done (terminal; doneCh closed).
type childState int

const (
	childQueued childState = iota
	childRunning
	childDone
)

// String renders the state as the enum-ish label the SubagentStatus roster shows
// (ids and labels only — A9).
func (s childState) String() string {
	switch s {
	case childRunning:
		return "running"
	case childDone:
		return "done"
	default:
		return "queued"
	}
}

// childRunRegistry tracks every child run spawned under one parent Run, keyed by
// the child session id. It is the shared foundation for per-child cancel (cancel
// consumes cancel/clientCancelled/askIDs) and background subagents (background
// consumes state/result/delivered/doneCh and the seal). It is owned by the parent
// Run exactly as childAsks is, but created UNCONDITIONALLY (cancel arrives only on
// interactive surfaces, but background bookkeeping must work headless too, and
// the registry is a mutex + map — negligible).
//
// LOCKING: two mutexes for two concerns, never held together by the registry.
// `mu` guards the entries map (registration/markDone/recordAsk/requestCancel —
// always short, never across a channel send). `emitMu` guards the seal flag AND
// every guarded send (safeEmit) as ONE locked section (A4a: no TOCTOU between
// the sealed check and the send). The split means a guarded send that waits on
// the events channel can never wedge entry bookkeeping (markDone, a sibling's
// registration, a later drain) behind it.
//
// STATE VOCABULARY (A5 — the ghost-entry resolution). An entry describes a child
// that RAN, is running, or is queued-and-cancellable. Two deliberate rules keep
// the roster honest:
//
//   - PRE-START ABORT (remove): a child that NEVER started driving — a
//     post-registration validation/fork/session-build failure whose error already
//     returned inline to the model, the background fail-fast gate-full path, or a
//     team member torn down by an enrolment failure before any round drove it —
//     is REMOVED from the registry (the one exception to "entries are never
//     removed"). There is nothing to observe, collect, resume, or inspect, so a
//     done+StopNone (or fabricated StopEndTurn) entry would be a phantom in the
//     SubagentStatus roster. remove closes the entry's doneCh (a parked waiter
//     wakes) and only ever applies to a NOT-done entry — a pre-start abort by
//     definition precedes any real terminal.
//
//   - MEANINGFUL PRE-START TERMINAL (markDone): a child cancelled while QUEUED
//     (CancelChild / parent-ctx death during the gate wait) keeps a done entry
//     with StopCancelled — the cancellation is a real, attributable disposition,
//     not a phantom.
//
// Otherwise entries are never removed during the run — done entries are what
// SubagentStatus and background delivery read — and the whole map dies with the
// Run.
type childRunRegistry struct {
	// mu guards entries (and the per-entry fields) plus gen. Never held across emit.
	mu      sync.Mutex
	entries map[string]*childEntry
	// gen is the terminal-generation broadcast channel: closed and replaced (under
	// mu) on every markDone/remove, so an "any child" waiter (SubagentStatus with
	// wait_ms and no agent_id) can park on the CURRENT generation and wake when the
	// next terminal lands.
	gen chan struct{}

	// emitMu guards sealed + every guarded send in one locked section (A4a), so a
	// late emit can never race seal/close into a send-on-closed-channel panic.
	// seal() therefore also serialises against an in-flight emit. Distinct from
	// mu so a send waiting on the events channel never blocks entry bookkeeping.
	emitMu sync.Mutex
	// sealed is set (via seal) after the run-end drain and before the run's events
	// channel closes. Every later safeEmit is a silent no-op.
	sealed bool
	// sealOnce guards the emitAbort close (seal may be called twice: the loop's
	// pre-terminal drain hook AND the run goroutine's deferred belt).
	sealOnce sync.Once
	// emitAbort is closed at seal-INTENT, BEFORE emitMu is acquired, so a guarded
	// send blocked on a full events channel (a consumer that stopped draining)
	// aborts and releases emitMu rather than deadlocking seal. Run.emitOrAbort
	// selects on it alongside the send.
	emitAbort chan struct{}
	// emit publishes an event toward the parent Run's stream. RunContentWith binds
	// it to Run.emitOrAbort (+ the sink mirror): a BLOCKING send (loop-style
	// backpressure; a cancelled run's in-flight child events still reach the
	// draining consumer) that gives up only when emitAbort closes (seal), so a
	// wedged consumer cannot park a child goroutine (or readControl inside
	// CancelChild) past the run's own teardown. It is set once before the run
	// goroutine starts and only read after; emitMu guards each call. nil (tests)
	// silently drops.
	emit func(session.Event)
}

// childEntry is one registered child's control block.
type childEntry struct {
	family childFamily
	// goal is the clamped, plain-text label registered for overlay/status
	// rendering (never child content). The SubagentStatus ROSTER deliberately does
	// not render it (ids and enum labels only — A9).
	goal string
	// cancel is the per-CHILD context cancel covering the whole call (gate wait,
	// fork, drive, structured-output re-drives). Invoked OUTSIDE the registry lock.
	cancel context.CancelFunc
	// background marks a detached-delivery child: its tool call returned an
	// immediate "started" result and its rendered result is stored here at terminal
	// for SubagentStatus collection.
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
	// displaced is the DONE entry this registration OVERWROTE (a `resume` of an
	// already-run id within the same run — register stashes it). If the new
	// attempt ABORTS pre-start (gate full, resume-load/fork failure), remove
	// REINSTATES it, so the prior terminal — and its possibly-undelivered
	// background result — survives a failed resume attempt instead of being
	// erased. It is cleared the moment the new attempt genuinely proceeds
	// (markRunning) or lands its own terminal (markDone): from then on the
	// overwrite is real and permanent.
	displaced *childEntry
	// result is a BACKGROUND child's rendered terminal ToolResult (the exact text
	// the foreground call would have returned, stored by markDoneResult), awaiting
	// collection via SubagentStatus — the SOLE body channel (A2). nil for
	// foreground children (their result returned inline).
	result *session.ToolResult
	// delivered marks a background result as collected via SubagentStatus
	// (exactly-once: a second collect reports "already delivered"). The separate
	// `noticed` injection flag is the notice-injection iteration's (I3b).
	delivered bool
	// doneCh is closed exactly once at the child's terminal (markDone); the
	// run-end drain and SubagentStatus wait_ms park on it.
	doneCh chan struct{}
}

// childStatus is the read-only snapshot of one entry the SubagentStatus tool
// renders (plain values; no channels, no cancel funcs).
type childStatus struct {
	id         string
	family     childFamily
	goal       string
	background bool
	state      childState
	stop       session.StopReason
	delivered  bool
}

// backgroundJoin is one live background child the run-end drain cancels and joins.
type backgroundJoin struct {
	id     string
	doneCh <-chan struct{}
}

// newChildRunRegistry constructs an empty registry.
func newChildRunRegistry() *childRunRegistry {
	return &childRunRegistry{
		entries:   make(map[string]*childEntry),
		gen:       make(chan struct{}),
		emitAbort: make(chan struct{}),
	}
}

// register records a child under its session id BEFORE the child acquires its
// concurrency slot, so a child queued on the gate is already cancellable.
// Re-registration of an existing id (a `resume` of an already-run child within
// the SAME parent run) OVERWRITES the old entry with a fresh doneCh — the old
// entry's doneCh was closed at its terminal and is never touched again, so a
// double-close is structurally impossible. The displaced DONE entry is STASHED
// on the new one so a pre-start abort of the resume attempt can reinstate it
// (see childEntry.displaced); it is dropped for good once the new attempt
// genuinely starts (markRunning) or terminates (markDone).
func (g *childRunRegistry) register(childID string, family childFamily, goal string, cancel context.CancelFunc, background bool) {
	g.mu.Lock()
	defer g.mu.Unlock()
	e := &childEntry{
		family:     family,
		goal:       goal,
		cancel:     cancel,
		background: background,
		askIDs:     make(map[string]struct{}),
		state:      childQueued,
		doneCh:     make(chan struct{}),
	}
	if old, ok := g.entries[childID]; ok && old.state == childDone {
		e.displaced = old
	}
	g.entries[childID] = e
}

// markRunning advances a queued entry to running (the child's drive has actually
// started — its concurrency slot is held / its first round is being driven).
// Unknown or already-done ids are ignored. From here the registration's
// overwrite of a displaced prior entry is permanent (the stash is dropped).
func (g *childRunRegistry) markRunning(childID string) {
	g.mu.Lock()
	defer g.mu.Unlock()
	e, ok := g.entries[childID]
	if !ok || e.state == childDone {
		return
	}
	e.state = childRunning
	e.displaced = nil
}

// markDone records the child's terminal stop and closes its doneCh. It is
// idempotent: a second markDone for the same entry (or one for an unknown id)
// is a no-op, so doneCh is never double-closed.
func (g *childRunRegistry) markDone(childID string, stop session.StopReason) {
	g.markDoneResult(childID, stop, nil)
}

// markDoneResult is markDone plus the BACKGROUND child's rendered terminal
// ToolResult, stored for SubagentStatus collection (A2: the result body's sole
// channel). Same idempotence as markDone.
func (g *childRunRegistry) markDoneResult(childID string, stop session.StopReason, result *session.ToolResult) {
	g.mu.Lock()
	defer g.mu.Unlock()
	e, ok := g.entries[childID]
	if !ok || e.state == childDone {
		return
	}
	e.state = childDone
	e.stop = stop
	e.result = result
	e.displaced = nil
	close(e.doneCh)
	g.bumpGenLocked()
}

// remove deletes a NEVER-STARTED entry — the pre-start abort of the A5 state
// vocabulary (see the type comment): the child's failure already surfaced inline
// (or its member never ran a round), so a lingering done+StopNone entry would be
// a roster phantom. It closes the doneCh so a parked waiter wakes, and is a
// deliberate no-op for a done entry (a pre-start abort precedes any real
// terminal; never erase a meaningful one). If the aborted registration had
// DISPLACED a prior done entry (a failed `resume` attempt on an already-run id),
// that entry is REINSTATED — the prior terminal and its possibly-undelivered
// background result must survive the failed attempt.
func (g *childRunRegistry) remove(childID string) {
	g.mu.Lock()
	defer g.mu.Unlock()
	e, ok := g.entries[childID]
	if !ok || e.state == childDone {
		return
	}
	close(e.doneCh)
	if e.displaced != nil {
		g.entries[childID] = e.displaced
	} else {
		delete(g.entries, childID)
	}
	g.bumpGenLocked()
}

// bumpGenLocked wakes every "any child" waiter parked on the current terminal
// generation. Callers hold mu.
func (g *childRunRegistry) bumpGenLocked() {
	close(g.gen)
	g.gen = make(chan struct{})
}

// liveGeneration returns, under ONE lock hold, whether any registered child is
// still live AND the terminal-generation channel CONSISTENT with that liveness
// snapshot (closed when the next markDone/remove lands). The single hold is
// load-bearing: separate anyLive()/generationCh() calls had a TOCTOU window — a
// terminal landing between them handed the waiter the FRESH generation channel,
// parking an "any child" wait for its full capped duration even though the
// terminal it was waiting for had already happened.
func (g *childRunRegistry) liveGeneration() (gen <-chan struct{}, live bool) {
	g.mu.Lock()
	defer g.mu.Unlock()
	for _, e := range g.entries {
		if e.state != childDone {
			live = true
			break
		}
	}
	return g.gen, live
}

// doneChFor returns the entry's terminal channel (closed iff the child is done),
// or ok=false for an unknown id. The channel survives a later remove (remove
// closes it first), so a parked waiter never hangs.
func (g *childRunRegistry) doneChFor(childID string) (<-chan struct{}, bool) {
	g.mu.Lock()
	defer g.mu.Unlock()
	e, ok := g.entries[childID]
	if !ok {
		return nil, false
	}
	return e.doneCh, true
}

// statusSnapshot returns a stable (id-sorted) value snapshot of every entry for
// the SubagentStatus roster.
func (g *childRunRegistry) statusSnapshot() []childStatus {
	g.mu.Lock()
	defer g.mu.Unlock()
	out := make([]childStatus, 0, len(g.entries))
	for id, e := range g.entries {
		out = append(out, childStatus{
			id: id, family: e.family, goal: e.goal, background: e.background,
			state: e.state, stop: e.stop, delivered: e.delivered,
		})
	}
	sort.Slice(out, func(i, j int) bool { return out[i].id < out[j].id })
	return out
}

// collectOutcome classifies one SubagentStatus collection attempt.
type collectOutcome int

const (
	// collectUnknown — no such child in this run.
	collectUnknown collectOutcome = iota
	// collectRunning — the child has not reached its terminal yet (no body).
	collectRunning
	// collectForeground — done, but a FOREGROUND child: its result already
	// returned inline on its own tool call; there is no stored body.
	collectForeground
	// collectAlready — a background result that was already delivered (never
	// re-bloat the context with a second copy).
	collectAlready
	// collectOK — a background result delivered NOW (marked delivered by this
	// call; exactly-once).
	collectOK
)

// collect attempts to deliver one background child's stored result body,
// marking it delivered on success (exactly-once). The returned status is valid
// for every outcome except collectUnknown.
func (g *childRunRegistry) collect(childID string) (res *session.ToolResult, st childStatus, outcome collectOutcome) {
	g.mu.Lock()
	defer g.mu.Unlock()
	e, ok := g.entries[childID]
	if !ok {
		return nil, childStatus{}, collectUnknown
	}
	st = childStatus{id: childID, family: e.family, goal: e.goal, background: e.background,
		state: e.state, stop: e.stop, delivered: e.delivered}
	switch {
	case e.state != childDone:
		return nil, st, collectRunning
	case !e.background:
		return nil, st, collectForeground
	case e.delivered:
		return nil, st, collectAlready
	}
	e.delivered = true
	st.delivered = true
	return e.result, st, collectOK
}

// liveBackgroundIDs returns the sorted ids of every background child that has not
// reached its terminal — the gate-full fail-fast error's "currently running" list
// (ids ONLY — A9).
func (g *childRunRegistry) liveBackgroundIDs() []string {
	g.mu.Lock()
	defer g.mu.Unlock()
	var ids []string
	for id, e := range g.entries {
		if e.background && e.state != childDone {
			ids = append(ids, id)
		}
	}
	sort.Strings(ids)
	return ids
}

// cancelLiveBackground cancels every live BACKGROUND entry's per-call context
// (outside the lock) and returns their (id, doneCh) join handles, id-sorted, for
// the run-end drain. Foreground/team/parallel children cannot be live at a run
// terminal (their tool calls are synchronous), so only background entries are
// touched.
func (g *childRunRegistry) cancelLiveBackground() []backgroundJoin {
	g.mu.Lock()
	var joins []backgroundJoin
	var cancels []context.CancelFunc
	for id, e := range g.entries {
		if e.background && e.state != childDone {
			joins = append(joins, backgroundJoin{id: id, doneCh: e.doneCh})
			if e.cancel != nil {
				cancels = append(cancels, e.cancel)
			}
		}
	}
	g.mu.Unlock()
	for _, c := range cancels {
		c()
	}
	sort.Slice(joins, func(i, j int) bool { return joins[i].id < joins[j].id })
	return joins
}

// recordAsk records that the (live) child owns a surfaced ask, so a later
// CancelChild can retract it. Unknown or already-done ids are ignored (a child
// driven without parent caps, or a terminal race).
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
// spawning tools read it (via parentCaps.children) to attribute the kill: the
// Subagent tool renders the "[subagent cancelled by user]" terminal note and a
// Parallel branch flips its failReason to "cancelled by user", disambiguating a
// client cancel from a parent-run cancel or a per-call timeout.
func (g *childRunRegistry) clientCancelled(childID string) bool {
	g.mu.Lock()
	defer g.mu.Unlock()
	e, ok := g.entries[childID]
	return ok && e.clientCancelled
}

// abortEmits closes the emit-abort channel (idempotent — it shares sealOnce with
// seal), unblocking every guarded send parked on a full events channel WITHOUT
// yet barring future emits. The run-end drain calls it between its two join
// phases: a child blocked inside safeEmit (consumer stopped draining) cannot
// reach markDone until its send aborts, so aborting emits BEFORE the grace
// re-join lets such a child unwind and join — instead of burning the whole cap
// and being misreported as wedged.
func (g *childRunRegistry) abortEmits() {
	g.sealOnce.Do(func() { close(g.emitAbort) })
}

// seal marks the registry closed for emission. It is called twice per run —
// once by the loop's pre-terminal drain hook (so an ABANDONED background child's
// residual emits are safe no-ops before the terminal EvResult goes out) and once
// by the run goroutine's deferred belt just before the events channel closes.
//
// Ordering (deadlock-free by construction): seal first closes emitAbort (via
// abortEmits; the drain may already have closed it), which unblocks any guarded
// send parked on a full events channel — even with a LIVE run ctx (a consumer
// that stopped draining) — releasing emitMu; only then does it take emitMu and
// set sealed. Because the emit path holds emitMu across its sealed-check+send
// (A4a), an in-flight emit completes (or aborts) before seal returns, and every
// later emit is a safe no-op. This abort-before-emitMu ordering is THE deadlock
// prevention mechanic; TestSealUnblocksEmitParkedSend pins it.
func (g *childRunRegistry) seal() {
	g.abortEmits()
	g.emitMu.Lock()
	g.sealed = true
	g.emitMu.Unlock()
}

// safeEmit publishes one event on the parent stream through the seal guard: the
// sealed check AND the send happen inside one emitMu section (no TOCTOU against
// seal/close — A4a); the entries mutex is NOT held here, so a send waiting on
// the events channel never wedges entry bookkeeping. EVERY child-originated emit
// (subagent.start/tool/end, the surfaced-ask EvPermissionAsk, permission.retract)
// routes through here (A4c), so a post-seal emit from an abandoned background
// goroutine is a silent no-op, never a send-on-closed-channel panic. The bound
// emit (Run.emitOrAbort) blocks until delivered and gives up when seal aborts
// it — child observability is a UX courtesy; dropping an event against a
// wedged/teardown consumer is acceptable.
func (g *childRunRegistry) safeEmit(ev session.Event) {
	g.emitMu.Lock()
	defer g.emitMu.Unlock()
	if g.sealed || g.emit == nil {
		return
	}
	g.emit(ev)
}

// emitRetract publishes a permission.retract event for one withdrawn askID on
// the parent stream, through the same seal guard as every child emit. The
// payload is server-authored and carries the AskID only.
func (g *childRunRegistry) emitRetract(askID string) {
	g.safeEmit(session.Event{Type: session.EvPermissionRetract, Ask: &session.PendingAsk{AskID: askID}})
}
