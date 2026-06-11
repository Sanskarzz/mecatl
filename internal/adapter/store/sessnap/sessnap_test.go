package sessnap_test

import (
	"encoding/json"
	"reflect"
	"testing"
	"time"

	"github.com/stacklok/mecatl/engine/session"
	"github.com/stacklok/mecatl/internal/adapter/store/sessnap"
)

// runningSession builds a session in StateRunning carrying conversation,
// counters and limits, used by several round-trip tests.
func runningSession(t *testing.T) *session.Session {
	t.Helper()
	s := session.New("s1", session.ModePlan, "/ws", session.Limits{
		MaxTurns: 10, MaxToolCalls: 20, MaxConsecutiveFailures: 3,
	}, time.Unix(1700000000, 0).UTC())
	if err := s.BeginTurn(); err != nil {
		t.Fatalf("BeginTurn: %v", err)
	}
	if err := s.RecordAssistant(session.NewAssistantMessage("thinking", "rsn", []session.ToolCall{
		session.NewToolCall("c1", "Edit", json.RawMessage(`{"path":"x"}`)),
	})); err != nil {
		t.Fatalf("RecordAssistant: %v", err)
	}
	if err := s.RecordToolResults([]session.ToolResult{
		session.NewToolResult("c1", "ok"),
		session.NewToolError("c2", "boom"),
	}); err != nil {
		t.Fatalf("RecordToolResults: %v", err)
	}
	return s
}

// assertEquivalent checks the fields the store must preserve.
func assertEquivalent(t *testing.T, got, want *session.Session) {
	t.Helper()
	if got.ID != want.ID {
		t.Errorf("ID = %q, want %q", got.ID, want.ID)
	}
	if got.State != want.State {
		t.Errorf("State = %q, want %q", got.State, want.State)
	}
	if got.Mode != want.Mode {
		t.Errorf("Mode = %q, want %q", got.Mode, want.Mode)
	}
	if got.Workspace != want.Workspace {
		t.Errorf("Workspace = %q, want %q", got.Workspace, want.Workspace)
	}
	if !got.CreatedAt.Equal(want.CreatedAt) {
		t.Errorf("CreatedAt = %v, want %v", got.CreatedAt, want.CreatedAt)
	}
	if got.Limits != want.Limits {
		t.Errorf("Limits = %+v, want %+v", got.Limits, want.Limits)
	}
	if got.Counters != want.Counters {
		t.Errorf("Counters = %+v, want %+v", got.Counters, want.Counters)
	}
	if !reflect.DeepEqual(got.Conversation, want.Conversation) {
		t.Errorf("Conversation mismatch:\n got = %+v\nwant = %+v", got.Conversation, want.Conversation)
	}
	gotR, gotOK := got.StopReason()
	wantR, wantOK := want.StopReason()
	if gotR != wantR || gotOK != wantOK {
		t.Errorf("StopReason = %q,%v; want %q,%v", gotR, gotOK, wantR, wantOK)
	}
}

func TestRoundTripRunning(t *testing.T) {
	want := runningSession(t)
	got, err := sessnap.Unmarshal(mustMarshal(t, want))
	if err != nil {
		t.Fatalf("Unmarshal: %v", err)
	}
	assertEquivalent(t, got, want)
}

func TestRoundTripAwaitingPreservesPendingAsk(t *testing.T) {
	want := runningSession(t)
	ask := session.PendingAsk{
		AskID:  "ask-1",
		Tool:   "Bash",
		Args:   json.RawMessage(`{"cmd":"rm -rf /"}`),
		Reason: "destructive",
	}
	if err := want.PauseForApproval(ask); err != nil {
		t.Fatalf("PauseForApproval: %v", err)
	}

	got, err := sessnap.Unmarshal(mustMarshal(t, want))
	if err != nil {
		t.Fatalf("Unmarshal: %v", err)
	}
	assertEquivalent(t, got, want)

	gotAsk, ok := got.PendingAsk()
	if !ok {
		t.Fatalf("restored session has no pending ask; want one")
	}
	if gotAsk.AskID != ask.AskID || gotAsk.Tool != ask.Tool ||
		gotAsk.Reason != ask.Reason || string(gotAsk.Args) != string(ask.Args) {
		t.Fatalf("pending ask = %+v, want %+v", gotAsk, ask)
	}

	// The restored session must be resumable to continue the loop.
	if got.State != session.StateAwaiting {
		t.Fatalf("restored state = %q, want awaiting", got.State)
	}
}

func TestRoundTripTerminalStates(t *testing.T) {
	cases := []struct {
		name     string
		drive    func(*session.Session)
		wantStop session.StopReason
	}{
		{"completed", func(s *session.Session) { _ = s.Complete() }, session.StopEndTurn},
		{"stopped_max_tool_calls", func(s *session.Session) { _ = s.Stop(session.StopMaxToolCalls) }, session.StopMaxToolCalls},
		{"cancelled", func(s *session.Session) { _ = s.Cancel() }, session.StopCancelled},
		{"failed", func(s *session.Session) { _ = s.Fail() }, session.StopError},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			want := runningSession(t)
			tc.drive(want)
			got, err := sessnap.Unmarshal(mustMarshal(t, want))
			if err != nil {
				t.Fatalf("Unmarshal: %v", err)
			}
			assertEquivalent(t, got, want)
			if r, _ := got.StopReason(); r != tc.wantStop {
				t.Fatalf("restored StopReason = %q, want %q", r, tc.wantStop)
			}
		})
	}
}

// TestRoundTripRecordedStopReason asserts the snapshot captures and restores the
// EXACT terminal reason explicitly recorded on the session (via Stop), without
// inferring it from the conflated StopReason()/limit derivation. The session is
// driven so its Limits would derive a DIFFERENT reason than the one recorded, so
// a faithful round-trip can only come from RecordedStopReason.
func TestRoundTripRecordedStopReason(t *testing.T) {
	want := session.New("rec1", session.ModeDefault, "/ws", session.Limits{
		// MaxTurns: 1 would derive StopMaxTurns once a turn begins...
		MaxTurns: 1,
	}, time.Unix(1700000000, 0).UTC())
	if err := want.BeginTurn(); err != nil {
		t.Fatalf("BeginTurn: %v", err)
	}
	// ...but we explicitly record a DIFFERENT reason.
	if err := want.Stop(session.StopMaxConsecutiveFailures); err != nil {
		t.Fatalf("Stop: %v", err)
	}
	wantRec, ok := want.RecordedStopReason()
	if !ok || wantRec != session.StopMaxConsecutiveFailures {
		t.Fatalf("precondition RecordedStopReason = %q,%v", wantRec, ok)
	}

	got, err := sessnap.Unmarshal(mustMarshal(t, want))
	if err != nil {
		t.Fatalf("Unmarshal: %v", err)
	}
	gotRec, gotOK := got.RecordedStopReason()
	if !gotOK || gotRec != session.StopMaxConsecutiveFailures {
		t.Fatalf("restored RecordedStopReason = %q,%v; want %q,true",
			gotRec, gotOK, session.StopMaxConsecutiveFailures)
	}
	if got.State != session.StateCompleted {
		t.Fatalf("restored state = %q, want completed", got.State)
	}
}

func TestRoundTripIdle(t *testing.T) {
	want := session.New("idle1", session.ModeDefault, "/w", session.Limits{}, time.Unix(0, 0).UTC())
	got, err := sessnap.Unmarshal(mustMarshal(t, want))
	if err != nil {
		t.Fatalf("Unmarshal: %v", err)
	}
	assertEquivalent(t, got, want)
}

func mustMarshal(t *testing.T, s *session.Session) []byte {
	t.Helper()
	b, err := sessnap.Marshal(s)
	if err != nil {
		t.Fatalf("Marshal: %v", err)
	}
	return b
}

// TestRoundTripWithParts asserts a user message carrying image+audio media parts
// survives a Marshal→Unmarshal round-trip (bytes are base64 in JSON).
func TestRoundTripWithParts(t *testing.T) {
	s := session.New("m1", session.ModeDefault, "/ws", session.Limits{}, time.Unix(1700000000, 0).UTC())
	parts := []session.Content{
		{Kind: session.MediaImage, MIMEType: "image/png", Data: []byte{0x89, 0x50, 0x4e, 0x47}},
		{Kind: session.MediaAudio, MIMEType: "audio/wav", URL: "https://example.com/a.wav"},
	}
	if err := s.RecordUserPromptWithParts("describe", parts, nil); err != nil {
		t.Fatalf("record: %v", err)
	}

	line, err := sessnap.Marshal(s)
	if err != nil {
		t.Fatalf("Marshal: %v", err)
	}
	got, err := sessnap.Unmarshal(line)
	if err != nil {
		t.Fatalf("Unmarshal: %v", err)
	}
	if !reflect.DeepEqual(got.Conversation, s.Conversation) {
		t.Fatalf("conversation mismatch:\n got=%+v\nwant=%+v", got.Conversation, s.Conversation)
	}
	gm := got.Conversation.Messages[0]
	if len(gm.Parts) != 2 || gm.Parts[0].Kind != session.MediaImage || gm.Parts[1].URL != "https://example.com/a.wav" {
		t.Fatalf("restored parts = %+v", gm.Parts)
	}
}

// TestLoadV1SnapshotNoPartsIsTextOnly asserts a v1 snapshot JSON with no "parts"
// key decodes to a text-only message (nil Parts) without error — the additive
// field is back-compatible.
func TestLoadV1SnapshotNoPartsIsTextOnly(t *testing.T) {
	v1 := `{"id":"old","state":"idle","mode":"default","limits":{},"counters":{},` +
		`"workspace":"/ws","created_at":"2023-11-14T22:13:20Z",` +
		`"messages":[{"role":"user","text":"hello there"}]}`
	got, err := sessnap.Unmarshal([]byte(v1))
	if err != nil {
		t.Fatalf("Unmarshal v1: %v", err)
	}
	if got.Conversation.Len() != 1 {
		t.Fatalf("messages = %d, want 1", got.Conversation.Len())
	}
	m := got.Conversation.Messages[0]
	if m.Text != "hello there" {
		t.Fatalf("text = %q", m.Text)
	}
	if m.Parts != nil {
		t.Fatalf("Parts = %v, want nil for a v1 (no parts) snapshot", m.Parts)
	}
}
