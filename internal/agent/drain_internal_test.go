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
