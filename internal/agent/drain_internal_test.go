package agent

import (
	"context"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/stacklok/mecatl/internal/port"
	"github.com/stacklok/mecatl/internal/session"
)

// setDrainCaps shortens the run-end drain phases for a test and restores the
// real values on cleanup. The caps are vars ONLY as this test seam; the drain
// tests are not parallel, so the global write is safe.
func setDrainCaps(t *testing.T, phase1, grace time.Duration) {
	t.Helper()
	oldCap, oldGrace := childDrainCap, childDrainGrace
	childDrainCap, childDrainGrace = phase1, grace
	t.Cleanup(func() { childDrainCap, childDrainGrace = oldCap, oldGrace })
}

// newDrainRun builds the minimal Run shape drainChildren operates on: a real
// events channel, a registry with the PRODUCTION emit binding (Run.emitOrAbort
// over the registry's seal-abort channel), and a capturing diagnostics sink.
func newDrainRun(eventsCap int) (*Run, *internalCapturingDiag) {
	diag := newInternalCapturingDiag()
	r := &Run{
		events:   make(chan session.Event, eventsCap),
		ctx:      context.Background(),
		diag:     diag,
		children: newChildRunRegistry(),
	}
	r.children.emit = func(ev session.Event) { r.emitOrAbort(ev, r.children.emitAbort) }
	return r, diag
}

// warnsOf filters the captured records down to LevelWarn.
func warnsOf(diag *internalCapturingDiag) []internalDiagRecord {
	var warns []internalDiagRecord
	for _, rec := range diag.snapshot() {
		if rec.level == port.LevelWarn {
			warns = append(warns, rec)
		}
	}
	return warns
}

// TestDrainTwoPhaseJoinsEmitParkedChild is the architect's two-phase test: a
// child blocked in its own end-emit (full events channel, NON-draining consumer
// — the run's OWN backpressure) cannot reach markDone until emits abort. With a
// short phase-1 cap, drainChildren must NOT misattribute that backpressure as a
// wedged child: phase 2's abortEmits unblocks the send, the child reaches its
// terminal within the grace, it JOINS, and NO abandon WARN fires.
func TestDrainTwoPhaseJoinsEmitParkedChild(t *testing.T) {
	setDrainCaps(t, 100*time.Millisecond, 2*time.Second)
	r, diag := newDrainRun(1)
	r.events <- session.Event{} // buffer FULL; nobody drains — the wedged-consumer shape

	// Re-bind the emit with an entered-signal so the test can deterministically
	// wait until the child goroutine is genuinely parked INSIDE the guarded send
	// (emitMu held, send blocked) before draining.
	entered := make(chan struct{})
	var once sync.Once
	inner := r.children.emit
	r.children.emit = func(ev session.Event) {
		once.Do(func() { close(entered) })
		inner(ev)
	}

	_, cancel := context.WithCancel(context.Background())
	defer cancel()
	r.children.register("subagent-bg", childFamilySubagent, "g", cancel, true)
	r.children.markRunning("subagent-bg")

	go func() {
		// The emit-blocked child: its end event parks on the full channel; only
		// after the send aborts can it land its terminal.
		r.children.safeEmit(session.Event{Type: session.EvSubagentEnd,
			Subagent: &session.SubagentPayload{ChildID: "subagent-bg", Stop: session.StopCancelled}})
		r.children.markDone("subagent-bg", session.StopCancelled)
	}()
	<-entered

	done := make(chan struct{})
	go func() {
		(&Engine{}).drainChildren(context.Background(), r)
		close(done)
	}()
	select {
	case <-done:
	case <-time.After(10 * time.Second):
		t.Fatalf("drainChildren wedged")
	}

	if _, st, outcome := r.children.collect("subagent-bg"); outcome == collectUnknown || st.state != childDone {
		t.Fatalf("the emit-parked child must have JOINED once emits aborted, got %v (%+v)", outcome, st)
	}
	if warns := warnsOf(diag); len(warns) != 0 {
		t.Fatalf("consumer backpressure must NOT be misattributed as a wedged child; got WARNs %+v", warns)
	}
}

// TestDrainTwoPhaseJoinsRetractParkedChild pins the NEW park point the
// chokepoint introduced: markDoneResult's retract emit sits BEFORE the doneCh
// close, so against a non-draining consumer the child's terminal can park
// INSIDE the retract send — phase 2's abortEmits must unpark it so the child
// reaches its done-transition, JOINS within the grace, and is never flipped to
// abandoned (no WARN). Mirrors TestDrainTwoPhaseJoinsEmitParkedChild with the
// park moved from the end-emit to the recorded ask's retract.
func TestDrainTwoPhaseJoinsRetractParkedChild(t *testing.T) {
	setDrainCaps(t, 100*time.Millisecond, 2*time.Second)
	r, diag := newDrainRun(1)
	r.events <- session.Event{} // buffer FULL; nobody drains — the wedged-consumer shape
	const askID = "subagent-bg:0:k1:r1"
	router := newChildAskRouter()
	r.childAsks = router
	r.children.unregisterAsk = r.unregisterChildAsk

	// Entered-signal rebind so the test deterministically waits until the
	// terminal goroutine is parked INSIDE the guarded retract send.
	entered := make(chan struct{})
	var once sync.Once
	inner := r.children.emit
	r.children.emit = func(ev session.Event) {
		once.Do(func() { close(entered) })
		inner(ev)
	}

	_, cancel := context.WithCancel(context.Background())
	defer cancel()
	r.children.register("subagent-bg", childFamilySubagent, "g", cancel, true)
	r.children.markRunning("subagent-bg")
	r.children.recordAsk("subagent-bg", askID)
	router.registerChild(askID, &Run{asks: newAskRegistry()})

	go func() {
		// The retract-parked terminal: the ask's retract parks on the full
		// channel BEFORE doneCh closes; only after emits abort can the done
		// transition land.
		r.children.markDoneResult("subagent-bg", session.StopCancelled, nil)
	}()
	<-entered

	done := make(chan struct{})
	go func() {
		(&Engine{}).drainChildren(context.Background(), r)
		close(done)
	}()
	select {
	case <-done:
	case <-time.After(10 * time.Second):
		t.Fatalf("drainChildren wedged")
	}

	if _, st, outcome := r.children.collect("subagent-bg"); outcome == collectUnknown || st.state != childDone {
		t.Fatalf("the retract-parked child must have JOINED once emits aborted, got %v (%+v)", outcome, st)
	}
	if warns := warnsOf(diag); len(warns) != 0 {
		t.Fatalf("a retract parked on consumer backpressure must NOT be misattributed as a wedged child; got WARNs %+v", warns)
	}
	// The dropped retract leaves the router entry behind only if unregister never
	// ran — it DID run (unregister-before-emit), so the entry is gone even though
	// the event itself was aborted: the stale-modal residual is the consumer's
	// own wedge, not a routing leak.
	router.mu.Lock()
	_, present := router.byAskID[askID]
	router.mu.Unlock()
	if present {
		t.Fatalf("the parked retract's askID must still have been unregistered before the emit")
	}
}

// TestDrainAbandonsGenuinelyWedgedChildWithOneWarn pins the abandon path: a
// child whose doneCh NEVER closes (even after emits abort) is abandoned with
// exactly ONE LevelWarn whose attrs carry the ids ONLY (the child's goal label
// must not leak — A9), and a residual post-seal emit against the CLOSED events
// channel is a safe no-op, never a panic.
func TestDrainAbandonsGenuinelyWedgedChildWithOneWarn(t *testing.T) {
	setDrainCaps(t, 50*time.Millisecond, 50*time.Millisecond)
	r, diag := newDrainRun(8)

	_, cancel := context.WithCancel(context.Background())
	defer cancel()
	const secretGoal = "label-that-must-not-leak"
	r.children.register("subagent-wedged", childFamilySubagent, secretGoal, cancel, true)
	r.children.markRunning("subagent-wedged")
	// No goroutine: the doneCh genuinely never closes.

	(&Engine{}).drainChildren(context.Background(), r)

	warns := warnsOf(diag)
	if len(warns) != 1 {
		t.Fatalf("a genuinely wedged child must produce exactly ONE abandon WARN, got %d (%+v)", len(warns), warns)
	}
	if ids, _ := warns[0].attrs["ids"].(string); ids != "subagent-wedged" {
		t.Fatalf("the WARN must carry the abandoned ids, got attrs %+v", warns[0].attrs)
	}
	if strings.Contains(warns[0].msg, secretGoal) {
		t.Fatalf("the WARN must carry ids only — the goal label leaked into the message")
	}
	for k, v := range warns[0].attrs {
		if s, ok := v.(string); ok && strings.Contains(s, secretGoal) {
			t.Fatalf("the WARN must carry ids only — the goal label leaked into attr %q", k)
		}
	}

	// Run-teardown shape: the channel closes after seal; the abandoned child's
	// residual emit must be a silent no-op.
	close(r.events)
	r.children.safeEmit(session.Event{Type: session.EvSubagentEnd,
		Subagent: &session.SubagentPayload{ChildID: "subagent-wedged"}})
}

// drainEventsOf empties the run's buffered events channel non-blockingly and
// returns what was delivered (the drain tests have no live consumer).
func drainEventsOf(r *Run) []session.Event {
	var evs []session.Event
	for {
		select {
		case ev := <-r.events:
			evs = append(evs, ev)
		default:
			return evs
		}
	}
}

// TestDrainAbandonedChildAskRetractedPreSeal pins the drain's abandoned-only ask
// sweep: a genuinely WEDGED child (doneCh never closes) never reaches its
// registry terminal, so the chokepoint retraction cannot fire — drainChildren
// must sweep its still-pending surfaced ask itself, BEFORE the seal: the
// permission.retract is delivered (a post-seal emit would have been dropped),
// the router entry is gone, the abandon WARN stays exactly ONE line, and the
// abandoned child's eventual LATE markDoneResult emits nothing (its ask set was
// taken and the registry is sealed).
func TestDrainAbandonedChildAskRetractedPreSeal(t *testing.T) {
	setDrainCaps(t, 50*time.Millisecond, 50*time.Millisecond)
	r, diag := newDrainRun(8)
	const askID = "subagent-wedged:0:k1:r1"
	router := newChildAskRouter()
	r.childAsks = router
	r.children.unregisterAsk = r.unregisterChildAsk

	_, cancel := context.WithCancel(context.Background())
	defer cancel()
	r.children.register("subagent-wedged", childFamilySubagent, "g", cancel, true)
	r.children.markRunning("subagent-wedged")
	r.children.recordAsk("subagent-wedged", askID)
	router.registerChild(askID, &Run{asks: newAskRegistry()})
	// No goroutine: the doneCh genuinely never closes — the child is abandoned.

	(&Engine{}).drainChildren(context.Background(), r)

	if warns := warnsOf(diag); len(warns) != 1 {
		t.Fatalf("the abandoned child must still produce exactly ONE abandon WARN, got %d (%+v)", len(warns), warns)
	}
	var retracts []string
	for _, ev := range drainEventsOf(r) {
		if ev.Type == session.EvPermissionRetract && ev.Ask != nil {
			retracts = append(retracts, ev.Ask.AskID)
		}
	}
	if len(retracts) != 1 || retracts[0] != askID {
		t.Fatalf("the abandoned child's parked ask must be retracted pre-seal exactly once, got %v (want [%s])", retracts, askID)
	}
	router.mu.Lock()
	_, present := router.byAskID[askID]
	router.mu.Unlock()
	if present {
		t.Fatalf("the swept ask must be unregistered from the router")
	}

	// The abandoned child's LATE terminal: the ask set was already taken and the
	// registry is sealed — no retract, no panic, nothing on the channel.
	r.children.markDoneResult("subagent-wedged", session.StopCancelled, nil)
	if late := drainEventsOf(r); len(late) != 0 {
		t.Fatalf("the late markDoneResult must emit nothing post-seal, got %+v", late)
	}
}

// TestDrainCleanNoWarn pins the healthy path: children already at their terminal
// drain instantly — no WARN, registry sealed.
func TestDrainCleanNoWarn(t *testing.T) {
	r, diag := newDrainRun(8)
	_, cancel := context.WithCancel(context.Background())
	defer cancel()
	r.children.register("subagent-done", childFamilySubagent, "g", cancel, true)
	body := session.NewToolResult("p1", "agentId: subagent-done\n\nok")
	r.children.markDoneResult("subagent-done", session.StopEndTurn, &body)

	(&Engine{}).drainChildren(context.Background(), r)

	if warns := warnsOf(diag); len(warns) != 0 {
		t.Fatalf("a clean drain must emit NO warn, got %+v", warns)
	}
	r.children.emitMu.Lock()
	sealed := r.children.sealed
	r.children.emitMu.Unlock()
	if !sealed {
		t.Fatalf("drainChildren must seal the registry")
	}
}
