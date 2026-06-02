package ui

import (
	"bytes"
	"context"
	"flag"
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"sync"
	"testing"
	"time"

	tea "charm.land/bubbletea/v2"
	"github.com/charmbracelet/x/exp/teatest/v2"

	"github.com/stacklok/mecatl/cmd/mecatui/client"
	"github.com/stacklok/mecatl/cmd/mecatui/theme"
	mecatlv1 "github.com/stacklok/mecatl/contracts/gen/go/mecatl/v1"
)

// raceWaitScale multiplies the teatest WaitFinished deadline (and the few
// remaining content waits) when the binary is built with the race detector. The
// detector adds a documented 2–20× slowdown, and `task test` runs `go test -race
// ./...` with every package's tests in parallel; non-race builds keep the tight
// deadlines (raceEnabled == false → scale 1).
const raceWaitScale = 6

// scaleWait applies raceWaitScale under -race and is identity otherwise.
func scaleWait(d time.Duration) time.Duration {
	if raceEnabled {
		return d * raceWaitScale
	}
	return d
}

// progress records the phases the reducer has passed through, fed by Deps.onPhase
// on the update goroutine. The *Program teatest cases sequence input on it instead
// of on rendered output: under `task test`'s parallel -race load Bubble Tea's 60fps
// flush ticker is CPU-starved and the captured output stalls for seconds (so a
// WaitFor(tm.Output()) deadline fires before any frame is flushed), but the reducer
// goroutine keeps getting scheduled — making phase-observation starvation-robust.
type progress struct {
	mu   sync.Mutex
	cond *sync.Cond
	seen map[phase]bool
	last phase // most recently observed phase
	// runDone counts completed runs: a phaseRunning→…→phaseIdle return. It lets a
	// case wait for a run to FINISH even though phaseIdle was already seen at connect
	// time (so a plain seen[phaseIdle] can't distinguish "connected" from "run done").
	runDone   int
	wasActive bool // true once phaseRunning/awaitingApproval seen since last idle
}

func newProgress() *progress {
	p := &progress{seen: map[phase]bool{}, last: phaseConnecting}
	p.cond = sync.NewCond(&p.mu)
	return p
}

// record is the Deps.onPhase callback: it marks a phase seen, advances the
// run-completion counter on an active→idle return, and wakes any waiter.
func (p *progress) record(ph phase) {
	p.mu.Lock()
	p.seen[ph] = true
	p.last = ph
	switch ph {
	case phaseRunning, phaseAwaitingApproval:
		p.wasActive = true
	case phaseIdle:
		if p.wasActive {
			p.runDone++
			p.wasActive = false
		}
	}
	p.mu.Unlock()
	p.cond.Broadcast()
}

// wait blocks until the reducer has been observed in phase target, or fails after
// a (race-scaled) deadline. A background goroutine broadcasts on the deadline so a
// genuinely-stuck reducer surfaces as a clear failure rather than a hang.
func (p *progress) wait(t *testing.T, target phase, d time.Duration) {
	t.Helper()
	p.waitFunc(t, d, func() bool { return p.seen[target] },
		func() string { return "reach phase " + phaseName(target) })
}

// waitRunComplete blocks until at least n runs have completed (phaseRunning →
// phaseIdle), the deterministic "the run fully streamed and the reducer settled
// back to idle, force-flushing the tail at the result boundary" signal.
func (p *progress) waitRunComplete(t *testing.T, n int, d time.Duration) {
	t.Helper()
	p.waitFunc(t, d, func() bool { return p.runDone >= n },
		func() string { return "complete a run (idle after running)" })
}

// waitFunc is the shared wait body: block until ok() under the lock, failing after
// the race-scaled deadline. desc names the awaited condition for the failure.
func (p *progress) waitFunc(t *testing.T, d time.Duration, ok func() bool, desc func() string) {
	t.Helper()
	deadline := time.Now().Add(scaleWait(d))
	timer := time.AfterFunc(time.Until(deadline), func() { p.cond.Broadcast() })
	defer timer.Stop()

	p.mu.Lock()
	defer p.mu.Unlock()
	for !ok() {
		if time.Now().After(deadline) {
			t.Fatalf("reducer did not %s within %s (last=%s seen=%v runDone=%d)",
				desc(), scaleWait(d), phaseName(p.last), p.seen, p.runDone)
		}
		p.cond.Wait()
	}
}

// phaseName renders a phase for failure messages.
func phaseName(p phase) string {
	switch p {
	case phaseConnecting:
		return "connecting"
	case phaseIdle:
		return "idle"
	case phaseRunning:
		return "running"
	case phaseAwaitingApproval:
		return "awaitingApproval"
	case phaseFatal:
		return "fatal"
	default:
		return "unknown"
	}
}

// waitClosed blocks until ch is closed or the (race-scaled) deadline elapses,
// failing on timeout. Used to gate on the fake's reader-goroutine signals
// (reachedGate / drained / sessionReady), which are output-flush-independent.
func waitClosed(t *testing.T, what string, ch <-chan struct{}, d time.Duration) {
	t.Helper()
	select {
	case <-ch:
	case <-time.After(scaleWait(d)):
		t.Fatalf("timed out after %s waiting for %s", scaleWait(d), what)
	}
}

// updateGolden reports whether goldens should be refreshed. It reuses the
// -update flag that teatest already registers (defining our own would collide),
// resolved lazily at test time.
func updateGolden() bool {
	if f := flag.Lookup("update"); f != nil {
		return f.Value.String() == "true"
	}
	return false
}

// ansiRE strips ANSI escape sequences so stripped goldens are stable across
// terminal/colour-profile differences. One ANSI-on golden keeps the Aztec
// escapes locked separately.
var ansiRE = regexp.MustCompile(`\x1b\[[0-9;?]*[a-zA-Z]|\x1b\][^\x07]*\x07`)

func stripANSI(b []byte) []byte { return ansiRE.ReplaceAll(b, nil) }

// ev wraps an Event for a script.
func ev(e *mecatlv1.Event) *mecatlv1.ConverseResponse {
	return &mecatlv1.ConverseResponse{Event: e}
}

// preApprovalScript is the three-act scenario (mirrors cmd/mecademo) as proto
// events: Read (auto-allowed) → permission.ask(Write) → tail → result. The
// fakeRecver gates after the ask so the run pauses for the test to approve.
func preApprovalScript() []*mecatlv1.ConverseResponse {
	return []*mecatlv1.ConverseResponse{
		ev(&mecatlv1.Event{Type: "session.init", Seq: 1}),
		ev(&mecatlv1.Event{Type: "turn.start", Seq: 2, Turn: 1}),
		ev(&mecatlv1.Event{Type: "message.delta", Seq: 3, Turn: 1, Text: "Reading the greeting file."}),
		ev(&mecatlv1.Event{Type: "tool.call", Seq: 4, Turn: 1, ToolCall: &mecatlv1.ToolCall{
			Id: "call-read-1", Name: "Read", Args: `{"path":"greeting.txt"}`,
		}}),
		ev(&mecatlv1.Event{Type: "tool.result", Seq: 5, Turn: 1, ToolResult: &mecatlv1.ToolResult{
			CallId: "call-read-1", Content: "hello from the mecatl demo workspace",
		}}),
		ev(&mecatlv1.Event{Type: "turn.start", Seq: 6, Turn: 2}),
		ev(&mecatlv1.Event{Type: "message.delta", Seq: 7, Turn: 2, Text: "Now saving a note, which needs approval."}),
		ev(&mecatlv1.Event{Type: "permission.ask", Seq: 8, Turn: 2, Ask: &mecatlv1.PermissionAsk{
			AskId: "ask-write-1", Tool: "Write", Args: `{"path":"note.txt","content":"reviewed"}`, Reason: "Write requires approval",
		}}),
		ev(&mecatlv1.Event{Type: "tool.result", Seq: 9, Turn: 2, ToolResult: &mecatlv1.ToolResult{
			CallId: "call-write-1", Content: "wrote note.txt",
		}}),
		ev(&mecatlv1.Event{Type: "message.delta", Seq: 10, Turn: 3, Text: "Done."}),
		ev(&mecatlv1.Event{Type: "result", Seq: 11, Turn: 3, Result: &mecatlv1.Result{
			Stop: "end_turn", Text: "Done.", Usage: &mecatlv1.Usage{InputTokens: 1500, OutputTokens: 30, CacheReadTokens: 1400},
		}}),
	}
}

// newTestModel builds a Model wired to a gated fake stream. The recver gates
// after the permission.ask; the sender releases the gate when a ResumeApproval is
// sent, so the approval drives the post-approval tail (mirrors mecademo). Alt
// screen is disabled so direct View() snapshots are clean. Used by the synchronous
// golden/render tests (driveTo, TestRenderStripsServerEscapes).
func newTestModel(t *testing.T, th theme.Theme) (Model, *fakeRecver, *fakeSender) {
	t.Helper()
	recv := &fakeRecver{script: preApprovalScript(), gateType: "permission.ask", gate: make(chan struct{})}
	send := &fakeSender{}
	send.onSend = func(req *mecatlv1.ConverseRequest) {
		if req.GetResumeApproval() != nil {
			recv.release()
		}
	}
	conv := &fakeConv{recv: recv, send: send}
	m := New(Deps{
		Session:     conv,
		Conv:        conv,
		Theme:       th,
		Server:      "127.0.0.1:8080",
		Workspace:   "/workspace",
		Mode:        "default",
		Model:       "mock-model",
		Ctx:         context.Background(),
		NoAltScreen: true,
	})
	return m, recv, send
}

// programDeps bundles the fakes + the deterministic progress observer a *Program
// teatest case drives the real program loop with. The fake exposes reader-goroutine
// signals (sessionReady / reachedGate / drained) and the model an onPhase observer,
// all of which are independent of Bubble Tea's CPU-starved output flush — so the
// cases sequence input on real reducer/stream progress, not on rendered bytes.
type programDeps struct {
	recv  *fakeRecver
	send  *fakeSender
	conv  *fakeConv
	prog  *progress
	model Model
}

// newProgramModel builds the gated fake stream PLUS the deterministic signals and
// the onPhase observer the *Program teatest cases sequence on. The script is
// configurable so the scramble case can supply its own ungated stream.
func newProgramModel(t *testing.T, th theme.Theme, script []*mecatlv1.ConverseResponse, gateType string) programDeps {
	t.Helper()
	recv := &fakeRecver{
		script:      script,
		gateType:    gateType,
		gate:        make(chan struct{}),
		reachedGate: make(chan struct{}),
	}
	send := &fakeSender{}
	send.onSend = func(req *mecatlv1.ConverseRequest) {
		if req.GetResumeApproval() != nil {
			recv.release()
		}
	}
	conv := &fakeConv{recv: recv, send: send, sessionReady: make(chan struct{})}
	prog := newProgress()
	m := New(Deps{
		Session:     conv,
		Conv:        conv,
		Theme:       th,
		Server:      "127.0.0.1:8080",
		Workspace:   "/workspace",
		Mode:        "default",
		Model:       "mock-model",
		Ctx:         context.Background(),
		NoAltScreen: true,
		onPhase:     prog.record,
	})
	return programDeps{recv: recv, send: send, conv: conv, prog: prog, model: m}
}

// TestFullCycleProgram drives the whole three-act scenario through the real
// program loop (teatest): connect → prompt → stream Read+result → approve the
// Write ask → stream the tail → terminal result → quit. It asserts the live
// frames pass through the expected states and that the approval round-trip
// carried the exact ask_id on the SAME stream. This is the whole-program offline
// behavioural test; the pixel-exact lock lives in the View() goldens below.
func TestFullCycleProgram(t *testing.T) {
	pd := newProgramModel(t, theme.New("aztec", theme.AztecPalette()), preApprovalScript(), "permission.ask")
	tm := teatest.NewTestModel(t, pd.model, teatest.WithInitialTermSize(100, 30))

	// Connect: wait until the reducer has actually processed SessionReadyMsg and is
	// idle, so the follow-up prompt is not dropped by submitPrompt's sessionID guard.
	// (Gating on reducer phase, not rendered output — see progress's doc.)
	pd.prog.wait(t, phaseIdle, 3*time.Second)

	// Prompt: every key is delivered via program.Send → the eventLoop's FIFO msg
	// channel (teatest.Type sends KeyPressMsgs, it does not touch the input buffer),
	// so these are reduced strictly after the SessionReadyMsg above.
	tm.Type("Read greeting.txt and save a note")
	tm.Send(tea.KeyPressMsg{Code: tea.KeyEnter})

	// The run streams up to the gated permission.ask: the reducer reaches
	// phaseAwaitingApproval and the fake confirms the ask was yielded to the loop.
	waitClosed(t, "permission.ask delivered", pd.recv.reachedGate, 5*time.Second)
	pd.prog.wait(t, phaseAwaitingApproval, 5*time.Second)

	// Allow is focused by default → enter approves; the sender releases the gate so
	// the post-approval tail streams to the terminal result.
	tm.Send(tea.KeyPressMsg{Code: tea.KeyEnter})

	// The run completes: the reducer streams the tail, processes the terminal result
	// (force-flushing the conversation at the result boundary), and settles back to
	// idle — the deterministic completion signal.
	pd.prog.waitRunComplete(t, 1, 5*time.Second)

	tm.Send(tea.KeyPressMsg{Code: 'c', Mod: tea.ModCtrl})
	tm.WaitFinished(t, teatest.WithFinalTimeout(scaleWait(3*time.Second)))

	// The approval round-trip carried the exact ask_id on the SAME stream.
	var sawApproval bool
	for _, fr := range pd.send.frames() {
		if ra := fr.GetResumeApproval(); ra != nil {
			sawApproval = true
			if ra.GetAskId() != "ask-write-1" || !ra.GetAllow() {
				t.Errorf("ResumeApproval = %#v, want {ask-write-1, allow}", ra)
			}
		}
	}
	if !sawApproval {
		t.Error("no ResumeApproval frame sent")
	}

	// Whole-program final-state proof on the deterministic FinalModel (the live
	// frames assertion is the pixel-exact View() golden below; here we confirm the
	// run reached its terminal result and settled to idle).
	fm := tm.FinalModel(t).(Model)
	if fm.phase != phaseIdle {
		t.Errorf("final phase = %d, want phaseIdle (run settled)", fm.phase)
	}
	frame := stripANSIstr(fm.View().Content)
	if !strings.Contains(frame, "Done.") {
		t.Errorf("final frame missing the streamed tail 'Done.':\n%s", frame)
	}
}

// driveTo synchronously drives a Model to the awaiting-approval state with a
// fully rendered conversation, bypassing the program loop so View() yields one
// deterministic frame for golden comparison. It sizes the model, marks it
// connected, seeds the user block, then feeds the scripted client msgs up to the
// permission ask. Using the real client msg types + real Update keeps the golden
// faithful to production rendering.
func driveTo(t *testing.T, th theme.Theme) Model {
	t.Helper()
	m, _, _ := newTestModel(t, th)
	m = applyAll(m,
		tea.WindowSizeMsg{Width: 100, Height: 30},
		client.SessionReadyMsg{SessionID: "sess-test-0001"},
	)
	// Seed the user prompt block + running phase directly (submit path needs the
	// transport; we want a pure render snapshot).
	m.conv.addUser("Read greeting.txt and save a note")
	m.phase = phaseRunning
	m.refreshView()

	for _, e := range askFrameMsgs() {
		mm, _ := m.Update(e)
		m = mm.(Model)
	}
	return m
}

// applyAll feeds a sequence of msgs to a Model, discarding commands.
func applyAll(m Model, msgs ...tea.Msg) Model {
	for _, msg := range msgs {
		mm, _ := m.Update(msg)
		m = mm.(Model)
	}
	return m
}

// askFrameMsgs is the scripted client-msg sequence up to and including the
// permission ask (transport-independent), driving the conversation render.
func askFrameMsgs() []tea.Msg {
	return []tea.Msg{
		client.SessionInitMsg{Seq: 1},
		client.TurnStartMsg{Turn: 1},
		client.ReasoningDeltaMsg{Turn: 1, Text: "I should read the greeting first\nthen decide what to save"},
		client.AssistantDeltaMsg{Turn: 1, Text: "Reading the greeting file."},
		client.ToolCallMsg{ID: "call-read-1", Name: "Read", Args: `{"path":"greeting.txt"}`},
		client.ToolResultMsg{CallID: "call-read-1", Content: "hello from the mecatl demo workspace"},
		client.TurnEndMsg{Turn: 1, Usage: client.Usage{InputTokens: 1200, OutputTokens: 340}, DurationMs: 4100},
		client.TurnStartMsg{Turn: 2},
		client.AssistantDeltaMsg{Turn: 2, Text: "Now saving a note, which needs approval."},
		client.PermissionAskMsg{AskID: "ask-write-1", Tool: "Write", Args: `{"path":"note.txt","content":"reviewed"}`, Reason: "Write requires approval"},
	}
}

// TestViewGoldenStripped locks the awaiting-approval frame (ANSI stripped) — the
// readable structural golden a reviewer can eyeball.
func TestViewGoldenStripped(t *testing.T) {
	m := driveTo(t, theme.New("aztec", theme.AztecPalette()))
	got := stripANSI([]byte(m.View().Content))
	compareGolden(t, "view_stripped.golden", got)
}

// TestViewGoldenAztecANSI locks the same frame WITH ANSI so an Aztec palette
// regression (a changed escape) is caught.
func TestViewGoldenAztecANSI(t *testing.T) {
	m := driveTo(t, theme.New("aztec", theme.AztecPalette()))
	got := []byte(m.View().Content)
	compareGolden(t, "view_aztec_ansi.golden", got)
}

// escapePayload is a hostile string a malicious tool result/ask could carry: an
// OSC window-title set plus a clear-screen — used to prove the renderer never
// passes server escapes to the terminal (CWE-150).
const escapePayload = "\x1b]0;pwned\x07\x1b[2J"

// TestRenderStripsServerEscapes feeds the escape payload through a tool result
// and a permission ask, then asserts no ESC byte from the payload survives in
// View().Content. The theme's own legitimate escapes are removed first via the
// shared ansiRE, so any remaining 0x1b would be attacker-controlled.
func TestRenderStripsServerEscapes(t *testing.T) {
	m, _, _ := newTestModel(t, theme.New("aztec", theme.AztecPalette()))
	m = applyAll(m,
		tea.WindowSizeMsg{Width: 100, Height: 30},
		client.SessionReadyMsg{SessionID: "sess-test-0001"},
	)
	m.phase = phaseRunning

	// A tool call + a hostile result.
	m = applyAll(m,
		client.ToolCallMsg{ID: "c1", Name: "Read" + escapePayload, Args: `{"p":"` + escapePayload + `"}`},
		client.ToolResultMsg{CallID: "c1", Content: "line1" + escapePayload + "\nline2"},
		// A hostile permission ask — the modal must not be spoofable.
		client.PermissionAskMsg{
			AskID:  "a1",
			Tool:   "Write" + escapePayload,
			Args:   `{"x":"` + escapePayload + `"}`,
			Reason: "please " + escapePayload + " allow",
		},
	)

	raw := []byte(m.View().Content)
	// Strip the theme's own legitimate ANSI; whatever ESC remains is the payload.
	residual := ansiRE.ReplaceAll(raw, nil)
	if bytes.IndexByte(residual, 0x1b) >= 0 {
		t.Fatalf("server escape survived into View().Content:\n%q", residual)
	}
	// Belt and braces: the exact payload escape sequences must be absent.
	if bytes.Contains(raw, []byte("\x1b]0;pwned")) || bytes.Contains(raw, []byte("\x1b[2J")) {
		t.Fatal("payload OSC/clear-screen escape present in rendered output")
	}
}

func compareGolden(t *testing.T, name string, got []byte) {
	t.Helper()
	path := filepath.Join("testdata", name)
	got = normalizeTrailing(got)
	if updateGolden() {
		if err := os.MkdirAll("testdata", 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(path, got, 0o644); err != nil { //nolint:gosec // test golden
			t.Fatal(err)
		}
		return
	}
	want, err := os.ReadFile(path) //nolint:gosec // test golden
	if err != nil {
		t.Fatalf("read golden %s (run with -update): %v", name, err)
	}
	if !bytes.Equal(got, normalizeTrailing(want)) {
		t.Errorf("golden %s mismatch (run with -update to refresh)\n--- got ---\n%s\n--- want ---\n%s", name, got, want)
	}
}

// normalizeTrailing trims trailing whitespace per line and trailing blank lines
// so incidental padding differences don't churn goldens.
func normalizeTrailing(b []byte) []byte {
	lines := bytes.Split(b, []byte("\n"))
	for i := range lines {
		lines[i] = bytes.TrimRight(lines[i], " \t\r")
	}
	out := bytes.Join(lines, []byte("\n"))
	return bytes.TrimRight(out, "\n")
}

// TestClearBuiltinProgram drives the /clear built-in through the real program
// loop: run a turn to completion, see assistant text in the scrollback, then
// "/clear"+enter and assert the transcript text is GONE and the zero-state
// welcome card is back — the whole-program behavioural proof of the built-in.
func TestClearBuiltinProgram(t *testing.T) {
	pd := newProgramModel(t, theme.New("aztec", theme.AztecPalette()), preApprovalScript(), "permission.ask")
	tm := teatest.NewTestModel(t, pd.model, teatest.WithInitialTermSize(100, 30))

	pd.prog.wait(t, phaseIdle, 5*time.Second)

	tm.Type("Read greeting.txt and save a note")
	tm.Send(tea.KeyPressMsg{Code: tea.KeyEnter})

	// Approve the Write so the run reaches its terminal result and returns to idle
	// (/clear is idle-only). Gate on the reducer reaching awaiting-approval and the
	// fake yielding the ask, then approve and wait for the whole script to drain —
	// all output-flush-independent signals (see progress's doc).
	waitClosed(t, "permission.ask delivered", pd.recv.reachedGate, 5*time.Second)
	pd.prog.wait(t, phaseAwaitingApproval, 5*time.Second)
	tm.Send(tea.KeyPressMsg{Code: tea.KeyEnter})
	pd.prog.waitRunComplete(t, 1, 5*time.Second)

	// /clear + enter: every key is delivered FIFO via program.Send, so enter is
	// reduced strictly after the full "/clear" line lands — no need to poll output
	// for the palette description. The bare built-in line is intercepted and clears
	// the conversation.
	tm.Type("/clear")
	tm.Send(tea.KeyPressMsg{Code: tea.KeyEnter})

	tm.Send(tea.KeyPressMsg{Code: 'c', Mod: tea.ModCtrl})
	tm.WaitFinished(t, teatest.WithFinalTimeout(scaleWait(3*time.Second)))

	// Final-model assertion (deterministic, output-flush-independent): the
	// conversation is empty and the assistant transcript text is gone from the live
	// frame, with the zero-state welcome card back.
	fm := tm.FinalModel(t).(Model)
	if !fm.conv.isEmpty() {
		t.Error("/clear should have emptied the conversation")
	}
	if strings.Contains(stripANSIstr(fm.View().Content), "Reading the greeting file.") {
		t.Error("assistant transcript text should be gone from the final frame after /clear")
	}
	if !strings.Contains(stripANSIstr(fm.View().Content), "Welcome to mecatui") {
		t.Error("zero-state welcome card should be back after /clear")
	}
}

// TestHelpBuiltinProgram drives the /help built-in through the real program
// loop: "/help"+enter opens the keys-&-features overlay; esc closes it.
func TestHelpBuiltinProgram(t *testing.T) {
	pd := newProgramModel(t, theme.New("aztec", theme.AztecPalette()), preApprovalScript(), "permission.ask")
	tm := teatest.NewTestModel(t, pd.model, teatest.WithInitialTermSize(100, 30))

	pd.prog.wait(t, phaseIdle, 5*time.Second)

	// "/help"+enter opens the overlay; esc closes it. Every key is delivered FIFO via
	// program.Send (teatest.Type sends KeyPressMsgs, not buffered bytes), so enter is
	// reduced strictly after the full "/help" line lands and esc strictly after the
	// overlay opens — no need to poll the (CPU-starved) output for the intermediate
	// frames; the deterministic final-model assertion proves the open→close cycle.
	tm.Type("/help")
	tm.Send(tea.KeyPressMsg{Code: tea.KeyEnter})
	tm.Send(tea.KeyPressMsg{Code: tea.KeyEscape})
	tm.Send(tea.KeyPressMsg{Code: 'c', Mod: tea.ModCtrl})
	tm.WaitFinished(t, teatest.WithFinalTimeout(scaleWait(3*time.Second)))

	// Final-model assertion (deterministic): the overlay opened then closed, so the
	// final frame no longer shows the help body.
	fm := tm.FinalModel(t).(Model)
	if fm.showHelp {
		t.Error("/help overlay should be closed after esc")
	}
	// The overlay's distinctive body row (not shared with the zero-state card) is
	// gone once the overlay is closed.
	if strings.Contains(stripANSIstr(fm.View().Content), "this help (on an empty prompt)") {
		t.Error("help overlay body should be gone from the final frame after esc")
	}
}

// simpleRunScript is a minimal ungated run: a turn that streams a one-line tail
// then a clean end_turn result. label distinguishes the two runs the queue-drain
// e2e serves so the final frame can assert both prompts' turns rendered.
func simpleRunScript(label string) []*mecatlv1.ConverseResponse {
	return []*mecatlv1.ConverseResponse{
		ev(&mecatlv1.Event{Type: "session.init", Seq: 1}),
		ev(&mecatlv1.Event{Type: "turn.start", Seq: 2, Turn: 1}),
		ev(&mecatlv1.Event{Type: "message.delta", Seq: 3, Turn: 1, Text: "handled " + label}),
		ev(&mecatlv1.Event{Type: "result", Seq: 4, Turn: 1, Result: &mecatlv1.Result{
			Stop: "end_turn", Text: "handled " + label,
			Usage: &mecatlv1.Usage{InputTokens: 100, OutputTokens: 10},
		}}),
	}
}

// TestQueuedPromptAutoSendsProgram drives the type-while-running + queued-message
// feature through the real program loop: prompt "first" (run 1), then type "second"
// + enter MID-RUN to enqueue it, let run 1 complete cleanly, and assert run 2
// auto-fires from the queue. Each submitPrompt opens a NEW stream (one stream per
// prompt, as in production), so the fake serves TWO scripted recvers round-robin —
// without that, run 2 would replay run 1's script. Sequencing is on the
// run-completion counter (reducer-side, starvation-robust), never tm.Output().
func TestQueuedPromptAutoSendsProgram(t *testing.T) {
	th := theme.New("aztec", theme.AztecPalette())
	// Two recvers, one per run; the fakeConv hands a fresh one per OpenConverse
	// (round-robin) so each prompt streams its own script. Run 1 is GATED at its
	// "result": its terminal event is physically held in the fake's Recv until the
	// test releases it, so the enqueue keypress is guaranteed to be reduced WHILE run
	// 1 is still streaming (phaseRunning) — deterministically winning the race the
	// plan warns about, without polling output. Run 2 is ungated (auto-streams to its
	// own result once the drain fires it).
	run1 := &fakeRecver{script: simpleRunScript("first"), gateType: "message.delta", gate: make(chan struct{}), reachedGate: make(chan struct{})}
	run2 := &fakeRecver{script: simpleRunScript("second")}
	send := &fakeSender{}
	conv := &fakeConv{recv: run1, send: send, recvers: []*fakeRecver{run1, run2}, sessionReady: make(chan struct{})}
	prog := newProgress()
	model := New(Deps{
		Session:     conv,
		Conv:        conv,
		Theme:       th,
		Server:      "127.0.0.1:8080",
		Workspace:   "/workspace",
		Mode:        "default",
		Model:       "mock-model",
		Ctx:         context.Background(),
		NoAltScreen: true,
		onPhase:     prog.record,
	})
	tm := teatest.NewTestModel(t, model, teatest.WithInitialTermSize(100, 30))

	// Connect: wait for the reducer to settle idle (so the prompt isn't dropped by
	// submitPrompt's sessionID guard).
	prog.wait(t, phaseIdle, 3*time.Second)

	// Run 1: prompt "first". It streams its delta then BLOCKS before the result (the
	// gate is after the delta), so the result is physically held in the fake.
	tm.Type("first")
	tm.Send(tea.KeyPressMsg{Code: tea.KeyEnter})
	prog.wait(t, phaseRunning, 5*time.Second)
	waitClosed(t, "run 1 streamed its delta (result gated)", run1.reachedGate, 5*time.Second)

	// Mid-run (run 1 is provably still streaming — its result is gated): type "second"
	// + enter to ENQUEUE it. Keys are delivered FIFO via program.Send and reduced
	// strictly after the prompt above; run 1's result cannot have been processed yet
	// (it's blocked in the fake's Recv), so the reducer is in phaseRunning and enter
	// enqueues.
	tm.Type("second")
	tm.Send(tea.KeyPressMsg{Code: tea.KeyEnter})

	// Release run 1's result → endRun + drain. The drain coalesces run 1's completion
	// with run 2's submit in ONE reducer step (so onPhase never surfaces the
	// intervening idle); thus run 1's end is NOT a separate runDone. Run 2 then streams
	// ungated to its own result, which IS an observable completion: waitRunComplete(1)
	// fires only after the WHOLE chain (run 1 done → drained → run 2 done) has settled.
	run1.release()
	prog.waitRunComplete(t, 1, 5*time.Second)

	tm.Send(tea.KeyPressMsg{Code: 'c', Mod: tea.ModCtrl})
	tm.WaitFinished(t, teatest.WithFinalTimeout(scaleWait(3*time.Second)))

	// Deterministic final-model assertions (never tm.Output): the queue drained, two
	// Prompt frames were sent in order, and both prompts' turns rendered.
	fm := tm.FinalModel(t).(Model)
	if len(fm.queued) != 0 {
		t.Errorf("queue should be empty after the drain, got %v", fm.queued)
	}
	var prompts []string
	for _, fr := range send.frames() {
		if p := fr.GetPrompt(); p != nil {
			prompts = append(prompts, p.GetText())
		}
	}
	if len(prompts) != 2 || prompts[0] != "first" || prompts[1] != "second" {
		t.Fatalf("prompt frames = %v, want [first second] in order", prompts)
	}
	frame := stripANSIstr(fm.View().Content)
	if !strings.Contains(frame, "first") {
		t.Errorf("final frame missing the first prompt:\n%s", frame)
	}
	if !strings.Contains(frame, "second") {
		t.Errorf("final frame missing the auto-sent second prompt:\n%s", frame)
	}
}

// scrambleScript streams an assistant turn whose markdown carries the exact shapes
// that scrambled in the bug report: an "## mecatl" heading and a numbered list with
// ✅ markers (one bare, one VS16-decorated). No permission gate — it runs straight
// to result so the end-of-turn repaint settles. The deltas arrive in fragments so
// the live block reflows mid-cluster, the condition under which the width-method
// disagreement used to scramble the layout.
func scrambleScript() []*mecatlv1.ConverseResponse {
	const heading = "## mecatl\n\n"
	const item1 = "1. ✅ first task is done\n"
	const item2 = "2. ✅️ second task is done too\n" // VS16-decorated check
	return []*mecatlv1.ConverseResponse{
		ev(&mecatlv1.Event{Type: "session.init", Seq: 1}),
		ev(&mecatlv1.Event{Type: "turn.start", Seq: 2, Turn: 1}),
		ev(&mecatlv1.Event{Type: "message.delta", Seq: 3, Turn: 1, Text: "## mec"}),
		ev(&mecatlv1.Event{Type: "message.delta", Seq: 4, Turn: 1, Text: "atl\n\n1. "}),
		ev(&mecatlv1.Event{Type: "message.delta", Seq: 5, Turn: 1, Text: "✅ first task is done\n2. "}),
		ev(&mecatlv1.Event{Type: "message.delta", Seq: 6, Turn: 1, Text: "✅️ second task is done too\n"}),
		ev(&mecatlv1.Event{Type: "result", Seq: 7, Turn: 1, Result: &mecatlv1.Result{
			Stop: "end_turn", Text: heading + item1 + item2,
			Usage: &mecatlv1.Usage{InputTokens: 100, OutputTokens: 20},
		}}),
	}
}

// TestStreamedEmojiMarkdownNotScrambled is the e2e regression guard for the
// streaming scramble. It streams an assistant turn whose markdown is the bug's
// shape — an "## mecatl" heading and a "1. ✅ … 2. ✅️ …" numbered list — in
// fragments, then asserts the FINAL frame (FinalModel().View(), since the
// cumulative tm.Output() byte stream is append-only and so unsound for
// absence-matching) renders the heading text un-mangled ("mecatl", never the
// "mec##atl" scramble) and keeps each list item's prose intact and in order.
//
// Note on the list markers: glamour reformats an ordered-list marker (the literal
// "1. " becomes a styled "1" gutter), so this asserts on the ITEM PROSE, not the
// literal "1." — the scramble symptom is mangled prose / interleaved heading, not
// glamour's own marker styling.
//
// Honest caveat: teatest's emulator may measure cell width with the same method as
// glamour's wrap, so a passing emulator frame does not by itself prove the fix on a
// WcWidth terminal. This is a content/regression guard; the AUTHORITATIVE assertion
// is the per-line width-agreement unit test (TestMarkdownWidthMethodAgreement),
// which fails on the un-normalised code and passes after normalizeEmojiWidth.
func TestStreamedEmojiMarkdownNotScrambled(t *testing.T) {
	// Ungated stream (no permission.ask): it runs straight to the terminal result.
	pd := newProgramModel(t, theme.New("aztec", theme.AztecPalette()), scrambleScript(), "")
	tm := teatest.NewTestModel(t, pd.model, teatest.WithInitialTermSize(100, 30))

	pd.prog.wait(t, phaseIdle, 3*time.Second)

	tm.Type("show me the status")
	tm.Send(tea.KeyPressMsg{Code: tea.KeyEnter})

	// Gate the quit on the run completing: the reducer streams the fragments,
	// processes the terminal result (force-flushing the conversation at the result
	// boundary), and settles back to idle — so the final frame is settled before we
	// snapshot it. This is the deterministic, output-flush-independent replacement
	// for polling the rendered tail, which is what flaked under -race CPU starvation.
	pd.prog.waitRunComplete(t, 1, 5*time.Second)

	tm.Send(tea.KeyPressMsg{Code: 'c', Mod: tea.ModCtrl})
	tm.WaitFinished(t, teatest.WithFinalTimeout(scaleWait(3*time.Second)))

	frame := stripANSIstr(tm.FinalModel(t).(Model).View().Content)
	if !strings.Contains(frame, "mecatl") {
		t.Errorf("final frame missing the heading text 'mecatl':\n%s", frame)
	}
	if strings.Contains(frame, "mec##atl") || strings.Contains(frame, "mec ##atl") {
		t.Errorf("final frame shows the scrambled heading 'mec##atl':\n%s", frame)
	}
	first := strings.Index(frame, "first task is done")
	second := strings.Index(frame, "second task is done too")
	if first < 0 {
		t.Errorf("final frame missing the first list item prose:\n%s", frame)
	}
	if second < 0 {
		t.Errorf("final frame missing the second list item prose:\n%s", frame)
	}
	if first >= 0 && second >= 0 && first > second {
		t.Errorf("list items rendered out of order (first=%d second=%d):\n%s", first, second, frame)
	}
}
