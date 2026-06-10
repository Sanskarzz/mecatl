package agent

import (
	"context"
	"encoding/json"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/stacklok/mecatl/internal/adapter/memfs"
	"github.com/stacklok/mecatl/internal/adapter/mockllm"
	"github.com/stacklok/mecatl/internal/adapter/permpolicy"
	"github.com/stacklok/mecatl/internal/governance"
	"github.com/stacklok/mecatl/internal/session"
	"github.com/stacklok/mecatl/internal/tool"
)

// TestChildRegistryReRegistrationOverwrites pins A5: re-registering an existing id
// (a `resume` of an already-run child within the SAME parent run) OVERWRITES the
// done entry with a FRESH doneCh — the old (closed) channel is never re-closed, the
// new one starts open, and the stale clientCancelled/done state is reset.
func TestChildRegistryReRegistrationOverwrites(t *testing.T) {
	reg := newChildRunRegistry()
	_, cancel1 := context.WithCancel(context.Background())
	defer cancel1()
	reg.register("c1", childFamilySubagent, "first run", cancel1, false)
	if _, _, ok := reg.requestCancel("c1"); !ok {
		t.Fatalf("a live entry must be cancellable")
	}
	reg.markDone("c1", session.StopCancelled)
	if !reg.clientCancelled("c1") {
		t.Fatalf("clientCancelled must be set after requestCancel")
	}
	oldDone := reg.entries["c1"].doneCh
	select {
	case <-oldDone:
	default:
		t.Fatalf("markDone must close the entry's doneCh")
	}

	// Re-register the SAME id (the resume-within-one-run case): fresh entry.
	_, cancel2 := context.WithCancel(context.Background())
	defer cancel2()
	reg.register("c1", childFamilySubagent, "resumed run", cancel2, false)
	e := reg.entries["c1"]
	if e.doneCh == oldDone {
		t.Fatalf("re-registration must mint a FRESH doneCh, not reuse the closed one")
	}
	select {
	case <-e.doneCh:
		t.Fatalf("the fresh doneCh must start open")
	default:
	}
	if e.clientCancelled || e.state == childDone {
		t.Fatalf("re-registration must reset clientCancelled/done state; got %+v", e)
	}
	// And the fresh entry terminates without a double-close panic.
	reg.markDone("c1", session.StopEndTurn)
	reg.markDone("c1", session.StopEndTurn) // idempotent second call: no panic
}

// TestChildRegistryRequestCancelEdges pins the cancel decision table: unknown id →
// false; done id → false; live id → true with the askID snapshot CLEARED (a second
// cancel returns true again — the child is still live — but re-retracts nothing).
func TestChildRegistryRequestCancelEdges(t *testing.T) {
	reg := newChildRunRegistry()
	if _, _, ok := reg.requestCancel("nope"); ok {
		t.Fatalf("unknown id must not be cancellable")
	}
	_, cancel := context.WithCancel(context.Background())
	defer cancel()
	reg.register("c1", childFamilySubagent, "g", cancel, false)
	reg.recordAsk("c1", "ask-1")
	reg.recordAsk("c1", "ask-2")

	_, asks, ok := reg.requestCancel("c1")
	if !ok || len(asks) != 2 {
		t.Fatalf("first cancel must snapshot both owned asks, got ok=%v asks=%v", ok, asks)
	}
	_, asks2, ok2 := reg.requestCancel("c1")
	if !ok2 {
		t.Fatalf("a second cancel of a still-live child is idempotent (true)")
	}
	if len(asks2) != 0 {
		t.Fatalf("the askID snapshot must be CLEARED on the first cancel; second got %v", asks2)
	}
	// recordAsk on a done entry is dropped (no retract for a terminal child).
	reg.markDone("c1", session.StopCancelled)
	reg.recordAsk("c1", "ask-late")
	if _, _, ok := reg.requestCancel("c1"); ok {
		t.Fatalf("a done id must not be cancellable")
	}
}

// TestChildRegistryEmitRetractSealed pins the seal contract the cancel path relies
// on: before seal, emitRetract publishes a permission.retract carrying ONLY the
// AskID; after seal it is a silent no-op (so a cancel racing run-teardown can never
// send on the closed events channel).
func TestChildRegistryEmitRetractSealed(t *testing.T) {
	reg := newChildRunRegistry()
	var got []session.Event
	reg.emit = func(ev session.Event) { got = append(got, ev) }

	reg.emitRetract("ask-1")
	if len(got) != 1 || got[0].Type != session.EvPermissionRetract {
		t.Fatalf("expected one permission.retract, got %+v", got)
	}
	if got[0].Ask == nil || got[0].Ask.AskID != "ask-1" {
		t.Fatalf("retract must carry the AskID, got %+v", got[0].Ask)
	}
	if got[0].Ask.Tool != "" || len(got[0].Ask.Args) != 0 || got[0].Ask.Reason != "" {
		t.Fatalf("retract must carry the AskID ONLY (server-authored), got %+v", got[0].Ask)
	}

	reg.seal()
	reg.emitRetract("ask-2")
	if len(got) != 1 {
		t.Fatalf("emitRetract after seal must be a no-op, got %+v", got)
	}
}

// TestChildRegistryConcurrentCancelAndCompletion is the -race brake: concurrent
// requestCancel+cancel() against the natural markDone never double-closes doneCh,
// never deadlocks, and leaves the entry done.
func TestChildRegistryConcurrentCancelAndCompletion(t *testing.T) {
	for i := 0; i < 200; i++ {
		reg := newChildRunRegistry()
		_, cancel := context.WithCancel(context.Background())
		reg.register("c", childFamilySubagent, "g", cancel, false)
		reg.recordAsk("c", "ask-1")
		var wg sync.WaitGroup
		wg.Add(2)
		go func() {
			defer wg.Done()
			if c, asks, ok := reg.requestCancel("c"); ok {
				if c != nil {
					c()
				}
				for range asks {
					reg.emitRetract("ask-1")
				}
			}
		}()
		go func() {
			defer wg.Done()
			reg.markDone("c", session.StopEndTurn)
		}()
		wg.Wait()
		select {
		case <-reg.entries["c"].doneCh:
		default:
			t.Fatalf("entry must be done after the race")
		}
	}
}

// TestRetractPrecedesDoneChClose pins the chokepoint's load-bearing ordering
// DETERMINISTICALLY (the e2e retract-before-EvResult index assertions can pass
// by scheduling accident even with the retract moved AFTER the close — the
// child goroutine outraces the drain's wakeup to the stream): AT RETRACT-EMIT
// TIME the entry's doneCh must NOT yet be closed. Flipping markDoneResult's
// retract/done-transition order fails this test; nothing else in the suite
// catches it reliably. Same pattern as TestCancelChildUnregistersBeforeRetract.
func TestRetractPrecedesDoneChClose(t *testing.T) {
	reg := newChildRunRegistry()
	router := newChildAskRouter()
	reg.unregisterAsk = router.unregister
	_, cancel := context.WithCancel(context.Background())
	defer cancel()
	reg.register("c", childFamilySubagent, "g", cancel, false)
	reg.recordAsk("c", "ask-1")
	router.registerChild("ask-1", &Run{asks: newAskRegistry()})
	reg.mu.Lock()
	doneCh := reg.entries["c"].doneCh
	reg.mu.Unlock()

	retracts := 0
	reg.emit = func(ev session.Event) {
		if ev.Type != session.EvPermissionRetract {
			return
		}
		retracts++
		// THE ordering pin: the done signal must not have fired yet.
		select {
		case <-doneCh:
			t.Errorf("doneCh already closed AT retract-emit time — the retract must precede the done transition")
		default:
		}
	}
	reg.markDoneResult("c", session.StopCancelled, nil)
	if retracts != 1 {
		t.Fatalf("expected exactly one retract emit, got %d", retracts)
	}
	select {
	case <-doneCh:
	default:
		t.Fatalf("markDoneResult must still close doneCh after the retract")
	}
}

// TestTakeAsksIncludesDoneEntries pins the DOCUMENTED robustness contract of
// takeAsks — a DONE entry is not skipped — rather than currently-observable
// behavior: today a done entry's ask set is always already empty (recordAsk
// drops asks for done entries; the first take/requestCancel cleared the rest),
// so a done-skip would be an equivalent mutant. It becomes load-bearing the
// moment a future markDoneResult reorder flips the done-transition BEFORE the
// take — this pin keeps that reorder from silently stranding a pending ask.
// The ask is seeded directly under mu, bypassing recordAsk's done-guard.
func TestTakeAsksIncludesDoneEntries(t *testing.T) {
	reg := newChildRunRegistry()
	_, cancel := context.WithCancel(context.Background())
	defer cancel()
	reg.register("c", childFamilySubagent, "g", cancel, false)
	reg.markDone("c", session.StopEndTurn)
	reg.mu.Lock()
	reg.entries["c"].askIDs["ask-late"] = struct{}{}
	reg.mu.Unlock()

	got := reg.takeAsks("c")
	if len(got) != 1 || got[0] != "ask-late" {
		t.Fatalf("takeAsks must NOT skip a done entry's asks, got %v", got)
	}
	if again := reg.takeAsks("c"); len(again) != 0 {
		t.Fatalf("the take must CLEAR the set (exactly-once), second take got %v", again)
	}
}

// TestVerdictRacesTerminalRetract is the -race brake for the answered-vs-pending
// gate: a routed verdict (route deletes the router entry, delivers to the child)
// races the child's registry terminal (markDoneResult takes the ask and
// retracts-if-unregistered). The router mutex serialises the two deletes, so
// EXACTLY ONE of {verdict routed, retract emitted} happens — never both (a
// spurious retract racing a just-delivered verdict), never neither (a stale
// modal), never a panic.
func TestVerdictRacesTerminalRetract(t *testing.T) {
	for i := 0; i < 200; i++ {
		reg := newChildRunRegistry()
		router := newChildAskRouter()
		reg.unregisterAsk = router.unregister
		var retracts atomic.Int32
		reg.emit = func(ev session.Event) {
			if ev.Type == session.EvPermissionRetract {
				retracts.Add(1)
			}
		}
		_, cancel := context.WithCancel(context.Background())
		reg.register("c", childFamilySubagent, "g", cancel, false)
		reg.recordAsk("c", "ask-1")
		child := &Run{asks: newAskRegistry()}
		verdictCh := child.asks.register("ask-1")
		router.registerChild("ask-1", child)

		var wg sync.WaitGroup
		wg.Add(2)
		go func() {
			defer wg.Done()
			router.route("ask-1", session.VerdictAllowOnce)
		}()
		go func() {
			defer wg.Done()
			reg.markDoneResult("c", session.StopEndTurn, nil)
		}()
		wg.Wait()
		cancel()

		routed := false
		select {
		case <-verdictCh:
			routed = true
		default:
		}
		retracted := retracts.Load() == 1
		if routed == retracted {
			t.Fatalf("iteration %d: exactly one of {verdict routed, retract emitted} must happen; routed=%v retracts=%d",
				i, routed, retracts.Load())
		}
	}
}

// TestRouterUnregisterAfterRouteNoRetract is the deterministic direction pin for
// the gate: when the verdict was ROUTED first (the entry is gone from the
// router), the child's later registry terminal must take the (stale) askID and
// emit NO retract — unregister reports false, the do-not-retract signal.
func TestRouterUnregisterAfterRouteNoRetract(t *testing.T) {
	reg := newChildRunRegistry()
	router := newChildAskRouter()
	reg.unregisterAsk = router.unregister
	retracts := 0
	reg.emit = func(ev session.Event) {
		if ev.Type == session.EvPermissionRetract {
			retracts++
		}
	}
	_, cancel := context.WithCancel(context.Background())
	defer cancel()
	reg.register("c", childFamilySubagent, "g", cancel, false)
	reg.recordAsk("c", "ask-1")
	child := &Run{asks: newAskRegistry()}
	verdictCh := child.asks.register("ask-1")
	router.registerChild("ask-1", child)

	if !router.route("ask-1", session.VerdictAllowOnce) {
		t.Fatalf("route must deliver the registered ask's verdict")
	}
	select {
	case <-verdictCh:
	default:
		t.Fatalf("the routed verdict must have reached the child's ask registry")
	}
	reg.markDoneResult("c", session.StopEndTurn, nil)
	if retracts != 0 {
		t.Fatalf("an ANSWERED ask must never draw a retract at the child terminal, got %d", retracts)
	}
}

// TestRouterRouteAfterUnregisterFalse pins the other direction plus unregister's
// bool semantics: a pending ask's first unregister reports removal (true), the
// second is an idempotent false, and a route AFTER unregister misses (false) —
// the caller then falls through to the parent's own registry where the stale
// verdict dies as an unknown-ask no-op.
func TestRouterRouteAfterUnregisterFalse(t *testing.T) {
	router := newChildAskRouter()
	router.registerChild("ask-1", &Run{asks: newAskRegistry()})
	if !router.unregister("ask-1") {
		t.Fatalf("first unregister of a pending ask must report removal (the retract-this signal)")
	}
	if router.unregister("ask-1") {
		t.Fatalf("second unregister must be false (idempotent; never a second retract)")
	}
	if router.route("ask-1", session.VerdictAllowOnce) {
		t.Fatalf("route after unregister must miss — the verdict falls through to the parent's own registry")
	}
	if router.unregister("ask-never-registered") {
		t.Fatalf("unknown ids must report false")
	}
}

// TestRouterEntryClearedOnChildExit pins the leak fix (bug 2): a never-answered
// surfaced ask's router entry used to linger until Run GC; now the child's
// registry terminal consumes it — after markDoneResult the router is EMPTY and
// the one retract was emitted.
func TestRouterEntryClearedOnChildExit(t *testing.T) {
	const askID = "subagent-p1:0:k1:r1"
	reg := newChildRunRegistry()
	router := newChildAskRouter()
	reg.unregisterAsk = router.unregister
	var retracts []string
	reg.emit = func(ev session.Event) {
		if ev.Type == session.EvPermissionRetract && ev.Ask != nil {
			retracts = append(retracts, ev.Ask.AskID)
		}
	}
	_, cancel := context.WithCancel(context.Background())
	defer cancel()
	reg.register("subagent-p1", childFamilySubagent, "g", cancel, false)
	reg.recordAsk("subagent-p1", askID)
	router.registerChild(askID, &Run{asks: newAskRegistry()})

	reg.markDoneResult("subagent-p1", session.StopCancelled, nil)

	router.mu.Lock()
	remaining := len(router.byAskID)
	router.mu.Unlock()
	if remaining != 0 {
		t.Fatalf("the never-answered ask's router entry must be cleared at the child terminal; %d remain", remaining)
	}
	if len(retracts) != 1 || retracts[0] != askID {
		t.Fatalf("expected exactly one retract for %q, got %v", askID, retracts)
	}
	// Idempotence: a second terminal takes an empty set — no re-retract.
	reg.markDoneResult("subagent-p1", session.StopCancelled, nil)
	if len(retracts) != 1 {
		t.Fatalf("a second markDoneResult must not re-retract, got %v", retracts)
	}
}

// TestCancelChildMidGateWait cancels a subagent QUEUED on a full concurrency gate:
// the per-call ctx is registered BEFORE acquireChildSlot, so the cancel unblocks the
// gate wait and the model-facing error names the CLIENT cancellation (not a generic
// "cancelled" that reads like the run collapsing).
func TestCancelChildMidGateWait(t *testing.T) {
	childEngine := NewEngine(Deps{
		LLM:     mockllm.New(mockllm.TextTurn("child: never runs")),
		Catalog: tool.NewCatalog(),
		Policy:  permpolicy.NewPolicy([]governance.Rule{{Effect: governance.Allow}}, nil),
		Model:   "child-model",
	})
	tl, ok := NewSubagentTool(childEngine, WithMaxConcurrentChildren(1)).(*SubagentTool)
	if !ok {
		t.Fatalf("NewSubagentTool did not return a *SubagentTool")
	}
	// Occupy the single slot so the call below genuinely queues on the gate.
	tl.childGate <- struct{}{}

	reg := newChildRunRegistry()
	caps := parentCaps{children: reg}

	resCh := make(chan session.ToolResult, 1)
	go func() {
		res, _ := tl.ExecuteWithParent(context.Background(),
			session.NewToolCall("p1", "Subagent", json.RawMessage(`{"prompt":"queued work"}`)),
			memfs.NewWorkspace("/ws"), nil, caps)
		resCh <- res
	}()

	// Registration happens synchronously inside run() BEFORE the gate wait, so
	// polling the registry is a faithful "queued and cancellable" signal.
	if !waitForEntry(reg, "subagent-p1", 5*time.Second) {
		t.Fatalf("child was not registered before the gate wait")
	}
	r := &Run{children: reg} // CancelChild needs only the registry (no router, no emit)
	if !r.CancelChild("subagent-p1") {
		t.Fatalf("CancelChild must reach a child queued on the gate")
	}

	select {
	case res := <-resCh:
		if !res.IsError {
			t.Fatalf("a cancel-while-queued must be a tool error (no child ran), got %q", res.Content)
		}
		if !strings.Contains(res.Content, "cancelled by the user while waiting for a concurrency slot") {
			t.Fatalf("the mid-gate-wait cancel must name the CLIENT cancellation, got %q", res.Content)
		}
	case <-time.After(5 * time.Second):
		t.Fatalf("cancel did not unblock the gate wait")
	}
	// The deferred terminal landed (doneCh closed) with the MEANINGFUL
	// StopCancelled — a cancel-while-queued is a real disposition, kept as a done
	// entry under the A5 vocabulary (unlike pre-start FAILURES, which are removed).
	reg.mu.Lock()
	e := reg.entries["subagent-p1"]
	reg.mu.Unlock()
	if e == nil {
		t.Fatalf("a cancelled-while-queued child must keep its registry entry (meaningful terminal)")
	}
	select {
	case <-e.doneCh:
	default:
		t.Fatalf("the terminal must land on the gate-wait exit path")
	}
	if e.stop != session.StopCancelled {
		t.Fatalf("gate-wait cancel must record StopCancelled, got %q", e.stop)
	}
}

// waitForEntry polls the registry until childID is registered or the deadline
// passes.
func waitForEntry(reg *childRunRegistry, childID string, timeout time.Duration) bool {
	deadline := time.Now().Add(timeout)
	for time.Now().Before(deadline) {
		reg.mu.Lock()
		_, ok := reg.entries[childID]
		reg.mu.Unlock()
		if ok {
			return true
		}
		time.Sleep(time.Millisecond)
	}
	return false
}

// TestCancelChildUnregistersBeforeRetract pins the FAIL-SAFE ORDERING of the
// parked-ask unwind deterministically: AT RETRACT-EMIT TIME the askID must
// already be gone from the parent router (childAsks.byAskID), so an approval
// racing the retraction can only ever hit the parent's own registry as an
// unknown-ask no-op. Flipping the unregister/emit order in Run.CancelChild fails
// this test (the e2e suite alone cannot see the ordering — QA verified the flip
// survives it).
func TestCancelChildUnregistersBeforeRetract(t *testing.T) {
	const askID = "subagent-p1:0:k1:r1"
	r := &Run{childAsks: newChildAskRouter(), children: newChildRunRegistry()}
	_, cancel := context.WithCancel(context.Background())
	defer cancel()
	r.children.register("subagent-p1", childFamilySubagent, "g", cancel, false)
	r.children.recordAsk("subagent-p1", askID)
	child := &Run{asks: newAskRegistry()}
	r.childAsks.registerChild(askID, child)

	retracts := 0
	r.children.emit = func(ev session.Event) {
		retracts++
		if ev.Type != session.EvPermissionRetract || ev.Ask == nil || ev.Ask.AskID != askID {
			t.Errorf("unexpected retract payload: %+v", ev)
		}
		// THE ordering pin: the router entry is gone BEFORE the retract is emitted.
		r.childAsks.mu.Lock()
		_, present := r.childAsks.byAskID[askID]
		r.childAsks.mu.Unlock()
		if present {
			t.Errorf("askID %q still registered in the router AT retract-emit time (unregister must precede the emit)", askID)
		}
	}
	if !r.CancelChild("subagent-p1") {
		t.Fatalf("CancelChild must succeed for a live child with a parked ask")
	}
	if retracts != 1 {
		t.Fatalf("expected exactly one retract emit, got %d", retracts)
	}
}

// TestSealUnblocksEmitParkedSend pins THE deadlock-prevention mechanic of the
// seal: emitAbort must close BEFORE seal acquires emitMu. A goroutine parked
// INSIDE safeEmit (emitMu held, send blocked on a full channel with NO consumer)
// can only release emitMu once its send aborts — if seal took emitMu first, it
// would deadlock forever behind that send. Reverting the abort-before-emitMu
// ordering fails this test on the watchdog; nothing else in the suite catches it.
func TestSealUnblocksEmitParkedSend(t *testing.T) {
	reg := newChildRunRegistry()
	events := make(chan session.Event, 1)
	events <- session.Event{} // buffer FULL; deliberately no consumer
	entered := make(chan struct{})
	reg.emit = func(ev session.Event) {
		// Once this closure runs, emitMu is already HELD by the caller (safeEmit
		// locks it before invoking the binding) — the entered signal is therefore a
		// deterministic "the lock is held and the send is about to park" anchor.
		close(entered)
		select {
		case events <- ev:
		case <-reg.emitAbort:
		}
	}
	go reg.safeEmit(session.Event{Type: session.EvSubagentEnd})
	<-entered

	done := make(chan struct{})
	go func() {
		reg.seal()
		close(done)
	}()
	select {
	case <-done:
	case <-time.After(5 * time.Second):
		t.Fatalf("seal deadlocked behind an emit-parked send — emitAbort must close BEFORE emitMu is acquired")
	}
}

// parkingToolInt is the internal-package twin of the external tests' parking
// tool: signals on start, parks until ctx cancel, returns a benign result.
type parkingToolInt struct {
	started chan struct{}
	once    sync.Once
}

func (*parkingToolInt) Spec() tool.ToolSpec {
	return tool.ToolSpec{Name: "Wait", Description: "parks until cancelled", Schema: json.RawMessage(`{"type":"object"}`)}
}
func (*parkingToolInt) ReadOnly() bool { return true }
func (p *parkingToolInt) Execute(ctx context.Context, in session.ToolCall, _ tool.Workspace) (session.ToolResult, error) {
	p.once.Do(func() { close(p.started) })
	<-ctx.Done()
	return session.NewToolResult(in.ID, "interrupted"), nil
}

// TestParentRunCancelKeepsUnNotedRendering is the D6 NEGATIVE: a PARENT-RUN
// cancel (the ctx the dispatcher hands the tool dies — esc / stream teardown)
// must keep the legacy UN-NOTED success rendering: no
// "[subagent cancelled by user]" text, and the registry's clientCancelled flag
// stays false (nothing called CancelChild). Hardcoding the clientCancelled read
// to true (the mutation QA found the suite blind to) fails this test.
func TestParentRunCancelKeepsUnNotedRendering(t *testing.T) {
	park := &parkingToolInt{started: make(chan struct{})}
	cat := tool.NewCatalog()
	cat.MustRegister(park)
	childEngine := NewEngine(Deps{
		LLM: mockllm.New(mockllm.ChunksTurn(
			mockllm.TextChunk("partial work"),
			mockllm.ToolCallChunk(session.NewToolCall("w1", "Wait", json.RawMessage(`{}`))),
			mockllm.DoneChunk(session.StopEndTurn),
		)),
		Catalog: cat,
		Policy:  permpolicy.NewPolicy([]governance.Rule{{Effect: governance.Allow}}, nil),
		Model:   "child-model",
	})
	tl, ok := NewSubagentTool(childEngine).(*SubagentTool)
	if !ok {
		t.Fatalf("NewSubagentTool did not return a *SubagentTool")
	}
	reg := newChildRunRegistry()
	caps := parentCaps{children: reg}

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	go func() {
		<-park.started
		cancel() // the PARENT-RUN cancel: the whole dispatch ctx dies, NOT CancelChild
	}()
	res, err := tl.ExecuteWithParent(ctx,
		session.NewToolCall("p1", "Subagent", json.RawMessage(`{"prompt":"x"}`)),
		memfs.NewWorkspace("/ws"), nil, caps)
	if err != nil {
		t.Fatalf("ExecuteWithParent transport error: %v", err)
	}
	if strings.Contains(res.Content, "[subagent cancelled by user]") {
		t.Fatalf("a PARENT-run cancel must NOT carry the client-cancel note (D6 disambiguation), got %q", res.Content)
	}
	if reg.clientCancelled("subagent-p1") {
		t.Fatalf("nothing called CancelChild; clientCancelled must be false")
	}
}

// TestChildRegistrySealVsEmitRace is the adversarial seal test (the design names
// the seal the riskiest piece): one goroutine hammers emitRetract through a REAL
// channel send while the main goroutine seals and then CLOSES that channel —
// exactly the run-teardown shape. Any send after close panics the test. ~200
// iterations under -race also exercise the emitMu/sealed memory ordering.
func TestChildRegistrySealVsEmitRace(t *testing.T) {
	for i := 0; i < 200; i++ {
		reg := newChildRunRegistry()
		events := make(chan session.Event, 1)
		// The production binding shape (Run.emitOrAbort): a blocking send that gives
		// up only when the registry's seal-abort channel closes.
		reg.emit = func(ev session.Event) {
			select {
			case events <- ev:
			case <-reg.emitAbort:
			}
		}
		var consumed sync.WaitGroup
		consumed.Add(1)
		go func() { // consumer: drains until close (the relay loop analogue)
			defer consumed.Done()
			for range events { //nolint:revive // draining
			}
		}()
		stop := make(chan struct{})
		var emitter sync.WaitGroup
		emitter.Add(1)
		go func() { // the racing CancelChild retract emitter
			defer emitter.Done()
			for {
				select {
				case <-stop:
					return
				default:
					reg.emitRetract("ask-1")
				}
			}
		}()
		// Teardown in production order: seal (closes the abort channel FIRST —
		// freeing a blocked send — then bars future sends + waits out an in-flight
		// one), close (must now be safe).
		reg.seal()
		close(events)
		// A post-seal emit against the CLOSED channel must be a silent no-op — if the
		// seal did not stick this panics (send on closed channel) and fails the test.
		reg.emitRetract("late-after-seal")
		reg.emitMu.Lock()
		sealed := reg.sealed
		reg.emitMu.Unlock()
		if !sealed {
			t.Fatalf("iteration %d: seal did not stick", i)
		}
		close(stop)
		emitter.Wait()
		consumed.Wait()
	}
}
