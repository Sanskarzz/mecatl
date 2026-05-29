package jsonlstore_test

import (
	"bufio"
	"context"
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"reflect"
	"testing"
	"time"

	"github.com/stacklok/ozzharness/internal/adapter/store/jsonlstore"
	"github.com/stacklok/ozzharness/internal/session"
)

func newStore(t *testing.T) (*jsonlstore.Store, string) {
	t.Helper()
	dir := t.TempDir()
	st, err := jsonlstore.New(dir)
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	return st, dir
}

func driven(t *testing.T) *session.Session {
	t.Helper()
	s := session.New("sess-1", session.ModePlan, "/ws", session.Limits{
		MaxTurns: 7, MaxToolCalls: 11, MaxConsecutiveFailures: 4,
	}, time.Unix(1700000000, 0).UTC())
	_ = s.BeginTurn()
	_ = s.RecordAssistant(session.NewAssistantMessage("plan", "rsn", []session.ToolCall{
		session.NewToolCall("c1", "Grep", json.RawMessage(`{"q":"foo"}`)),
	}))
	_ = s.RecordToolResults([]session.ToolResult{session.NewToolResult("c1", "hit")})
	return s
}

func TestSaveLoadRoundTrip(t *testing.T) {
	ctx := context.Background()
	st, _ := newStore(t)
	want := driven(t)
	if err := st.Save(ctx, want); err != nil {
		t.Fatalf("Save: %v", err)
	}
	got, err := st.Load(ctx, "sess-1")
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	if got.State != want.State || got.Mode != want.Mode ||
		got.Limits != want.Limits || got.Counters != want.Counters {
		t.Fatalf("scalar mismatch: got %+v want %+v", got, want)
	}
	if !reflect.DeepEqual(got.Conversation, want.Conversation) {
		t.Fatalf("conversation mismatch:\n got %+v\nwant %+v", got.Conversation, want.Conversation)
	}
}

func TestAppendOnlyLatestWins(t *testing.T) {
	ctx := context.Background()
	st, dir := newStore(t)
	s := driven(t)
	if err := st.Save(ctx, s); err != nil {
		t.Fatalf("Save#1: %v", err)
	}
	// Advance the session and save again.
	_ = s.PauseForApproval(session.PendingAsk{AskID: "a1", Tool: "Bash", Reason: "approve"})
	if err := st.Save(ctx, s); err != nil {
		t.Fatalf("Save#2: %v", err)
	}

	// Two saves -> two lines.
	path := filepath.Join(dir, "sess-1.session.jsonl")
	if n := countLines(t, path); n != 2 {
		t.Fatalf("session file has %d lines, want 2 (append-only)", n)
	}

	// Load returns the latest snapshot (awaiting, with pending ask).
	got, err := st.Load(ctx, "sess-1")
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	if got.State != session.StateAwaiting {
		t.Fatalf("loaded state = %q, want awaiting (latest)", got.State)
	}
	ask, ok := got.PendingAsk()
	if !ok || ask.AskID != "a1" {
		t.Fatalf("pending ask = %+v,%v; want a1", ask, ok)
	}
}

func TestResumePausedAwaitingSession(t *testing.T) {
	ctx := context.Background()
	st, _ := newStore(t)
	s := driven(t)
	ask := session.PendingAsk{AskID: "ask-9", Tool: "Edit", Args: json.RawMessage(`{"p":"f"}`), Reason: "write"}
	if err := s.PauseForApproval(ask); err != nil {
		t.Fatalf("PauseForApproval: %v", err)
	}
	if err := st.Save(ctx, s); err != nil {
		t.Fatalf("Save: %v", err)
	}

	got, err := st.Load(ctx, "sess-1")
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	gotAsk, ok := got.PendingAsk()
	if !ok {
		t.Fatalf("reloaded session has no pending ask; cannot resume")
	}
	if gotAsk.AskID != ask.AskID || gotAsk.Tool != ask.Tool ||
		gotAsk.Reason != ask.Reason || string(gotAsk.Args) != string(ask.Args) {
		t.Fatalf("pending ask = %+v, want %+v", gotAsk, ask)
	}
}

func TestLoadNotFound(t *testing.T) {
	st, _ := newStore(t)
	_, err := st.Load(context.Background(), "ghost")
	if !errors.Is(err, jsonlstore.ErrNotFound) {
		t.Fatalf("err = %v, want ErrNotFound", err)
	}
}

func TestToolCallLogParseable(t *testing.T) {
	st, dir := newStore(t)
	call := session.NewToolCall("call-7", "Bash", json.RawMessage(`{"cmd":"ls"}`))
	res := session.NewToolResult("call-7", "file.txt")
	st.ToolCall("sess-1", call, res, 1500*time.Microsecond)
	st.ToolCall("sess-1", session.NewToolCall("call-8", "Read", nil),
		session.NewToolError("call-8", "nope"), 42*time.Microsecond)

	path := filepath.Join(dir, "sess-1.tools.jsonl")
	if n := countLines(t, path); n != 2 {
		t.Fatalf("tools file has %d lines, want 2", n)
	}

	lines := readLines(t, path)
	var rec map[string]any
	if err := json.Unmarshal([]byte(lines[0]), &rec); err != nil {
		t.Fatalf("first record not parseable: %v", err)
	}
	if rec["type"] != "tool_call" {
		t.Errorf("type = %v, want tool_call", rec["type"])
	}
	if rec["tool"] != "Bash" {
		t.Errorf("tool = %v, want Bash", rec["tool"])
	}
	if rec["call_id"] != "call-7" {
		t.Errorf("call_id = %v, want call-7", rec["call_id"])
	}
	if rec["result"] != "file.txt" {
		t.Errorf("result = %v, want file.txt", rec["result"])
	}
	if rec["took_micros"].(float64) != 1500 {
		t.Errorf("took_micros = %v, want 1500", rec["took_micros"])
	}

	var rec2 map[string]any
	if err := json.Unmarshal([]byte(lines[1]), &rec2); err != nil {
		t.Fatalf("second record not parseable: %v", err)
	}
	if rec2["is_error"] != true {
		t.Errorf("is_error = %v, want true", rec2["is_error"])
	}
}

func TestSessionIDSanitizedToSafeFilename(t *testing.T) {
	ctx := context.Background()
	st, dir := newStore(t)
	s := session.New("../escape/../x", session.ModeDefault, "/w", session.Limits{}, time.Unix(0, 0).UTC())
	if err := st.Save(ctx, s); err != nil {
		t.Fatalf("Save: %v", err)
	}
	// File must live directly under dir, not escape it.
	entries, err := os.ReadDir(dir)
	if err != nil {
		t.Fatalf("ReadDir: %v", err)
	}
	for _, e := range entries {
		// The sanitized name must stay a single path element (no traversal).
		if filepath.Base(e.Name()) != e.Name() {
			t.Errorf("store entry %q escaped the store dir", e.Name())
		}
	}
	// Round-trip still works via the original id.
	if _, err := st.Load(ctx, "../escape/../x"); err != nil {
		t.Fatalf("Load after sanitize: %v", err)
	}
}

func countLines(t *testing.T, path string) int {
	t.Helper()
	return len(readLines(t, path))
}

func readLines(t *testing.T, path string) []string {
	t.Helper()
	f, err := os.Open(path)
	if err != nil {
		t.Fatalf("open %s: %v", path, err)
	}
	defer f.Close()
	var lines []string
	sc := bufio.NewScanner(f)
	for sc.Scan() {
		if len(sc.Bytes()) == 0 {
			continue
		}
		lines = append(lines, sc.Text())
	}
	if err := sc.Err(); err != nil {
		t.Fatalf("scan: %v", err)
	}
	return lines
}
