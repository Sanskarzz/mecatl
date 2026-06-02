package ui

import (
	"bytes"
	"context"
	"flag"
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"testing"
	"time"

	tea "charm.land/bubbletea/v2"
	"github.com/charmbracelet/x/exp/teatest/v2"

	"github.com/stacklok/mecatl/cmd/mecatui/client"
	"github.com/stacklok/mecatl/cmd/mecatui/theme"
	mecatlv1 "github.com/stacklok/mecatl/contracts/gen/go/mecatl/v1"
)

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
// screen is disabled so direct View() snapshots are clean.
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

// TestFullCycleProgram drives the whole three-act scenario through the real
// program loop (teatest): connect → prompt → stream Read+result → approve the
// Write ask → stream the tail → terminal result → quit. It asserts the live
// frames pass through the expected states and that the approval round-trip
// carried the exact ask_id on the SAME stream. This is the whole-program offline
// behavioural test; the pixel-exact lock lives in the View() goldens below.
func TestFullCycleProgram(t *testing.T) {
	m, _, send := newTestModel(t, theme.New("aztec", theme.AztecPalette()))
	tm := teatest.NewTestModel(t, m, teatest.WithInitialTermSize(100, 30))

	teatest.WaitFor(t, tm.Output(), func(b []byte) bool {
		return bytes.Contains(stripANSI(b), []byte("session sess-test"))
	}, teatest.WithDuration(3*time.Second))

	tm.Type("Read greeting.txt and save a note")
	tm.Send(tea.KeyPressMsg{Code: tea.KeyEnter})

	teatest.WaitFor(t, tm.Output(), func(b []byte) bool {
		return bytes.Contains(stripANSI(b), []byte("Permission required"))
	}, teatest.WithDuration(5*time.Second))

	// Allow is focused by default → enter approves.
	tm.Send(tea.KeyPressMsg{Code: tea.KeyEnter})

	teatest.WaitFor(t, tm.Output(), func(b []byte) bool {
		return bytes.Contains(stripANSI(b), []byte("done"))
	}, teatest.WithDuration(5*time.Second))

	tm.Send(tea.KeyPressMsg{Code: 'c', Mod: tea.ModCtrl})
	tm.WaitFinished(t, teatest.WithFinalTimeout(3*time.Second))

	var sawApproval bool
	for _, fr := range send.frames() {
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
	m, _, _ := newTestModel(t, theme.New("aztec", theme.AztecPalette()))
	tm := teatest.NewTestModel(t, m, teatest.WithInitialTermSize(100, 30))

	teatest.WaitFor(t, tm.Output(), func(b []byte) bool {
		return bytes.Contains(stripANSI(b), []byte("session sess-test"))
	}, teatest.WithDuration(5*time.Second))

	tm.Type("Read greeting.txt and save a note")
	tm.Send(tea.KeyPressMsg{Code: tea.KeyEnter})

	// Approve the Write so the run reaches its terminal result and returns to idle
	// (/clear is idle-only). Mirror TestFullCycleProgram: approve, then wait for
	// the footer stop label "done", which only renders after endRun lands the model
	// in phaseIdle — the reliable signal that the run is fully complete.
	teatest.WaitFor(t, tm.Output(), func(b []byte) bool {
		return bytes.Contains(stripANSI(b), []byte("Permission required"))
	}, teatest.WithDuration(5*time.Second))
	tm.Send(tea.KeyPressMsg{Code: tea.KeyEnter})

	teatest.WaitFor(t, tm.Output(), func(b []byte) bool {
		return bytes.Contains(stripANSI(b), []byte("done"))
	}, teatest.WithDuration(5*time.Second))

	// /clear + enter: the bare built-in line is intercepted and clears the conv.
	// Wait for the full "/clear" prefix to land (palette shows the built-in's
	// description) before enter — tm.Type is async, so an eager enter could submit
	// a partial line and fall through to a normal send.
	tm.Type("/clear")
	teatest.WaitFor(t, tm.Output(), func(b []byte) bool {
		return bytes.Contains(stripANSI(b), []byte("clear the conversation and scrollback"))
	}, teatest.WithDuration(5*time.Second))
	tm.Send(tea.KeyPressMsg{Code: tea.KeyEnter})

	// Wait until the zero-state welcome card is back on screen — it only renders
	// when the conversation is empty, so its return proves the clear took effect.
	teatest.WaitFor(t, tm.Output(), func(b []byte) bool {
		return bytes.Contains(stripANSI(b), []byte("Welcome to mecatui"))
	}, teatest.WithDuration(5*time.Second))

	tm.Send(tea.KeyPressMsg{Code: 'c', Mod: tea.ModCtrl})
	tm.WaitFinished(t, teatest.WithFinalTimeout(3*time.Second))

	// Final-model assertion (robust to cumulative output): the conversation is
	// empty and the assistant transcript text is gone from the live frame.
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
	m, _, _ := newTestModel(t, theme.New("aztec", theme.AztecPalette()))
	tm := teatest.NewTestModel(t, m, teatest.WithInitialTermSize(100, 30))

	teatest.WaitFor(t, tm.Output(), func(b []byte) bool {
		return bytes.Contains(stripANSI(b), []byte("session sess-test"))
	}, teatest.WithDuration(5*time.Second))

	tm.Type("/help")
	// Wait until the FULL "/help" prefix has landed and the palette shows the
	// built-in's description before pressing enter. tm.Type is asynchronous, so
	// sending enter eagerly can race ahead of the last rune and submit a partial
	// line ("/hel"), which would fall through to a normal send instead of running
	// the built-in. The palette description is distinctive to the open palette.
	teatest.WaitFor(t, tm.Output(), func(b []byte) bool {
		return bytes.Contains(stripANSI(b), []byte("show keys & features"))
	}, teatest.WithDuration(5*time.Second))
	tm.Send(tea.KeyPressMsg{Code: tea.KeyEnter})

	// The help overlay's distinctive body row only renders while the overlay is
	// open. (The title line is positioned with ANSI cursor moves the virtual
	// terminal splits, so we match a stable body string, not the title.)
	teatest.WaitFor(t, tm.Output(), func(b []byte) bool {
		return bytes.Contains(stripANSI(b), []byte("this help (on an empty prompt)"))
	}, teatest.WithDuration(5*time.Second))

	// esc closes the overlay.
	tm.Send(tea.KeyPressMsg{Code: tea.KeyEscape})
	tm.Send(tea.KeyPressMsg{Code: 'c', Mod: tea.ModCtrl})
	tm.WaitFinished(t, teatest.WithFinalTimeout(3*time.Second))

	// Final-model assertion (robust to cumulative output): the overlay opened then
	// closed, so the final frame no longer shows the help title.
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

// newScrambleModel wires a Model to the ungated scrambleScript (mirrors
// newTestModel but without the approval gate).
func newScrambleModel(t *testing.T, th theme.Theme) Model {
	t.Helper()
	recv := &fakeRecver{script: scrambleScript(), gate: make(chan struct{})}
	conv := &fakeConv{recv: recv, send: &fakeSender{}}
	return New(Deps{
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
	m := newScrambleModel(t, theme.New("aztec", theme.AztecPalette()))
	tm := teatest.NewTestModel(t, m, teatest.WithInitialTermSize(100, 30))

	teatest.WaitFor(t, tm.Output(), func(b []byte) bool {
		return bytes.Contains(stripANSI(b), []byte("session sess-test"))
	}, teatest.WithDuration(3*time.Second))

	tm.Type("show me the status")
	tm.Send(tea.KeyPressMsg{Code: tea.KeyEnter})

	// Gate the quit on a stable end-of-turn signal so the final frame is settled
	// before we snapshot it (avoids racing the repaint; same pattern as the other
	// teatest cases, which guards the known ~1/3 teatest flake under -race).
	teatest.WaitFor(t, tm.Output(), func(b []byte) bool {
		return bytes.Contains(stripANSI(b), []byte("second task is done too"))
	}, teatest.WithDuration(5*time.Second))

	tm.Send(tea.KeyPressMsg{Code: 'c', Mod: tea.ModCtrl})
	tm.WaitFinished(t, teatest.WithFinalTimeout(3*time.Second))

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
