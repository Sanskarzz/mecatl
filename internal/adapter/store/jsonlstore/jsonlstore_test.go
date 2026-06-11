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

	"github.com/stacklok/mecatl/engine/session"
	"github.com/stacklok/mecatl/internal/adapter/store/jsonlstore"
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
	st.ToolCall("sess-1", call, res, 800*time.Microsecond, 1500*time.Microsecond)
	st.ToolCall("sess-1", session.NewToolCall("call-8", "Read", nil),
		session.NewToolError("call-8", "nope"), 0, 42*time.Microsecond)

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
	if rec["queued_micros"].(float64) != 800 {
		t.Errorf("queued_micros = %v, want 800", rec["queued_micros"])
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

// TestListDecodesRealIDAndMtime pins that List returns the REAL session id
// decoded from the snapshot line — NOT a (non-invertible) reverse of the
// sanitized filename — and the session file's mtime as ModifiedAt. The id
// here contains a '/' that safeName flattens to '_', so a filename-derived id
// would come back mangled.
func TestListDecodesRealIDAndMtime(t *testing.T) {
	ctx := context.Background()
	st, dir := newStore(t)
	const id = session.SessionID("team-abc/lead") // sanitized on disk, real in the snapshot
	s := session.New(id, session.ModeDefault, "/ws", session.Limits{}, time.Unix(1700000000, 0).UTC())
	if err := st.Save(ctx, s); err != nil {
		t.Fatalf("Save: %v", err)
	}
	entries, err := st.List(ctx)
	if err != nil {
		t.Fatalf("List: %v", err)
	}
	if len(entries) != 1 {
		t.Fatalf("List returned %d entries, want 1: %+v", len(entries), entries)
	}
	if entries[0].ID != id {
		t.Errorf("List id = %q, want the REAL snapshot id %q (filename-derived ids are mangled)", entries[0].ID, id)
	}
	// ModifiedAt must be the session file's mtime.
	matches, err := filepath.Glob(filepath.Join(dir, "*.session.jsonl"))
	if err != nil || len(matches) != 1 {
		t.Fatalf("glob session file: %v (matches %v)", err, matches)
	}
	info, err := os.Stat(matches[0])
	if err != nil {
		t.Fatalf("Stat: %v", err)
	}
	if !entries[0].ModifiedAt.Equal(info.ModTime()) {
		t.Errorf("ModifiedAt = %v, want the file mtime %v", entries[0].ModifiedAt, info.ModTime())
	}
}

// TestListSkipsUndecodableFiles pins the best-effort posture: a corrupt or
// empty .session.jsonl (whose Load would fail identically) is skipped, not a
// List error.
func TestListSkipsUndecodableFiles(t *testing.T) {
	ctx := context.Background()
	st, dir := newStore(t)
	if err := st.Save(ctx, driven(t)); err != nil {
		t.Fatalf("Save: %v", err)
	}
	if err := os.WriteFile(filepath.Join(dir, "junk.session.jsonl"), []byte("{not json\n"), 0o644); err != nil {
		t.Fatalf("write junk: %v", err)
	}
	if err := os.WriteFile(filepath.Join(dir, "empty.session.jsonl"), nil, 0o644); err != nil {
		t.Fatalf("write empty: %v", err)
	}
	entries, err := st.List(ctx)
	if err != nil {
		t.Fatalf("List: %v", err)
	}
	if len(entries) != 1 || entries[0].ID != "sess-1" {
		t.Errorf("List = %+v, want exactly the one decodable session", entries)
	}
}

// TestDeleteRemovesBothFilesIdempotently pins that Delete removes the session
// snapshot AND the tool-call log, and that a second Delete (or a Delete of a
// never-saved id) succeeds.
func TestDeleteRemovesBothFilesIdempotently(t *testing.T) {
	ctx := context.Background()
	st, dir := newStore(t)
	s := driven(t)
	if err := st.Save(ctx, s); err != nil {
		t.Fatalf("Save: %v", err)
	}
	st.ToolCall(s.ID, session.NewToolCall("c9", "Read", json.RawMessage(`{"p":"x"}`)),
		session.NewToolResult("c9", "ok"), time.Millisecond, time.Millisecond)
	for _, suffix := range []string{".session.jsonl", ".tools.jsonl"} {
		if _, err := os.Stat(filepath.Join(dir, "sess-1"+suffix)); err != nil {
			t.Fatalf("precondition: %s missing: %v", suffix, err)
		}
	}
	if err := st.Delete(ctx, s.ID); err != nil {
		t.Fatalf("Delete: %v", err)
	}
	for _, suffix := range []string{".session.jsonl", ".tools.jsonl"} {
		if _, err := os.Stat(filepath.Join(dir, "sess-1"+suffix)); !os.IsNotExist(err) {
			t.Errorf("%s still present after Delete (stat err %v)", suffix, err)
		}
	}
	if _, err := st.Load(ctx, s.ID); !errors.Is(err, jsonlstore.ErrNotFound) {
		t.Errorf("Load after Delete = %v, want ErrNotFound", err)
	}
	if err := st.Delete(ctx, s.ID); err != nil {
		t.Errorf("second Delete = %v, want nil (idempotent)", err)
	}
	if err := st.Delete(ctx, "never-saved"); err != nil {
		t.Errorf("Delete(never-saved) = %v, want nil (idempotent)", err)
	}
}

// TestDeletePartialFailureLeavesSessionVisible pins Delete's removal ORDER:
// tools sidecar FIRST, session file LAST. When removing the tools file fails,
// the session file must SURVIVE — it is what List enumerates, so the pair
// stays visible and the next retention sweep retries the whole Delete. (The
// reverse order would permanently leak an invisible orphaned .tools.jsonl.)
// The unremovable tools file is simulated portably by replacing it with a
// NON-EMPTY directory, which os.Remove refuses on every platform.
func TestDeletePartialFailureLeavesSessionVisible(t *testing.T) {
	ctx := context.Background()
	st, dir := newStore(t)
	s := driven(t)
	if err := st.Save(ctx, s); err != nil {
		t.Fatalf("Save: %v", err)
	}
	sessionFile := filepath.Join(dir, "sess-1.session.jsonl")
	toolsPath := filepath.Join(dir, "sess-1.tools.jsonl")

	// Make the tools path unremovable: a non-empty directory under the sidecar's name.
	if err := os.MkdirAll(filepath.Join(toolsPath, "block"), 0o755); err != nil {
		t.Fatalf("mkdir blocking tools path: %v", err)
	}

	if err := st.Delete(ctx, s.ID); err == nil {
		t.Fatal("Delete with an unremovable tools file = nil error, want failure")
	}
	if _, err := os.Stat(sessionFile); err != nil {
		t.Fatalf("session file did not survive the partial Delete failure (stat: %v) — the List entry is gone and the orphan can never be retried", err)
	}
	entries, err := st.List(ctx)
	if err != nil {
		t.Fatalf("List after partial failure: %v", err)
	}
	if len(entries) != 1 || entries[0].ID != s.ID {
		t.Fatalf("List after partial failure = %+v, want the surviving session entry (the retry handle)", entries)
	}

	// Unblock and retry: the sweep's next Delete must complete the pair.
	if err := os.RemoveAll(toolsPath); err != nil {
		t.Fatalf("unblock tools path: %v", err)
	}
	if err := st.Delete(ctx, s.ID); err != nil {
		t.Fatalf("retry Delete after unblocking = %v, want nil", err)
	}
	if _, err := os.Stat(sessionFile); !os.IsNotExist(err) {
		t.Errorf("session file still present after the retry (stat err %v)", err)
	}
}
