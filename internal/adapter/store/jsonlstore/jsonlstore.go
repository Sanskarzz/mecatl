// Package jsonlstore implements an append-only, JSONL-backed port.SessionStore
// and port.ToolCallRecorder (the tool-call audit seam). It is the
// observability/replay seam: every Save appends a session snapshot as one JSON
// line to a per-session file, and every ToolCall appends a structured tool-call
// record to a per-session log. Nothing is ever overwritten, so the files form a
// replayable audit trail; Load reads the most recent snapshot line.
//
// Layout under the configured dir:
//
//	<dir>/<id>.session.jsonl   — one snapshot per Save (latest line wins)
//	<dir>/<id>.tools.jsonl     — one record per ToolCallRecorder.ToolCall
//
// Session ids are sanitized for use as filenames so an id can never escape dir.
package jsonlstore

import (
	"bufio"
	"context"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"time"

	"github.com/stacklok/mecatl/engine/adapter/sessnap"
	"github.com/stacklok/mecatl/engine/port"
	"github.com/stacklok/mecatl/engine/session"
)

// ErrNotFound is returned by Load when no snapshot file exists for the id. It wraps
// port.ErrSessionNotFound so a consumer that may not import this adapter can
// distinguish not-found from an infra failure via errors.Is.
var ErrNotFound = fmt.Errorf("jsonlstore: session not found: %w", port.ErrSessionNotFound)

// Store is an append-only JSONL SessionStore and ToolCallRecorder rooted at a directory.
type Store struct {
	dir string
	mu  sync.Mutex // serializes appends across files
}

// compile-time assertions that Store satisfies both ports plus the optional
// retention seam.
var (
	_ port.SessionStore     = (*Store)(nil)
	_ port.ToolCallRecorder = (*Store)(nil)
	_ port.PrunableStore    = (*Store)(nil)
)

// New constructs a Store writing under dir, creating dir if needed.
func New(dir string) (*Store, error) {
	if err := os.MkdirAll(dir, 0o755); err != nil {
		return nil, fmt.Errorf("jsonlstore: create dir: %w", err)
	}
	return &Store{dir: dir}, nil
}

// Save appends a snapshot of s as a single JSON line to the session file.
func (st *Store) Save(_ context.Context, s *session.Session) error {
	if s == nil {
		return sessnap.ErrNilSession
	}
	line, err := sessnap.Marshal(s)
	if err != nil {
		return err
	}
	st.mu.Lock()
	defer st.mu.Unlock()
	return appendLine(st.sessionPath(s.ID), line)
}

// Load reads the session file and reconstructs the latest snapshot line.
func (st *Store) Load(_ context.Context, id session.SessionID) (*session.Session, error) {
	path := st.sessionPath(id)
	f, err := os.Open(path) //nolint:gosec // path is sanitized via sessionPath
	if err != nil {
		if os.IsNotExist(err) {
			return nil, fmt.Errorf("%w: %q", ErrNotFound, id)
		}
		return nil, fmt.Errorf("jsonlstore: open session file: %w", err)
	}
	defer func() { _ = f.Close() }()

	var last []byte
	sc := bufio.NewScanner(f)
	sc.Buffer(make([]byte, 0, 64*1024), 16*1024*1024)
	for sc.Scan() {
		b := sc.Bytes()
		if len(strings.TrimSpace(string(b))) == 0 {
			continue
		}
		last = append(last[:0], b...) // copy: scanner reuses its buffer
	}
	if err := sc.Err(); err != nil {
		return nil, fmt.Errorf("jsonlstore: scan session file: %w", err)
	}
	if last == nil {
		return nil, fmt.Errorf("%w: %q (empty file)", ErrNotFound, id)
	}
	return sessnap.Unmarshal(last)
}

// sessionFileSuffix / toolsFileSuffix are the per-session file suffixes under
// dir (see the package doc layout).
const (
	sessionFileSuffix = ".session.jsonl"
	toolsFileSuffix   = ".tools.jsonl"
)

// List returns every stored session's id and last-modified time (the session
// file's mtime). It satisfies the optional port.PrunableStore retention seam.
//
// COST: safeName is NOT invertible (distinct ids can collide onto one
// filename, and a sanitized rune cannot be restored), so the REAL id is
// decoded from each session file's last snapshot line (the same latest-line
// the Load path trusts) rather than derived from the filename. That makes
// List O(total store bytes) in the worst case — acceptable for a
// retention sweep that runs on a startup/hourly cadence, not a hot path.
//
// List enumerates ONLY *.session.jsonl files: a .tools.jsonl sidecar without
// its session file (an orphan from a pre-fix partial Delete, or hand-pruning)
// is invisible here and is never swept — accepted as unreachable. Delete's
// tools-first removal order prevents this store from creating new ones.
func (st *Store) List(_ context.Context) ([]port.StoredSession, error) {
	st.mu.Lock()
	defer st.mu.Unlock()
	entries, err := os.ReadDir(st.dir)
	if err != nil {
		return nil, fmt.Errorf("jsonlstore: list store dir: %w", err)
	}
	var out []port.StoredSession
	for _, e := range entries {
		if e.IsDir() || !strings.HasSuffix(e.Name(), sessionFileSuffix) {
			continue
		}
		path := filepath.Join(st.dir, e.Name())
		id, err := decodeSessionID(path)
		if err != nil {
			// A truncated/empty/corrupt session file has no decodable id; skip
			// it rather than fail the whole inventory (Load of that id would
			// fail the same way). Best-effort listing, like the sweep itself.
			continue
		}
		info, err := e.Info()
		if err != nil {
			continue // raced with a concurrent delete; tolerate it
		}
		out = append(out, port.StoredSession{ID: id, ModifiedAt: info.ModTime()})
	}
	return out, nil
}

// Delete removes the session's snapshot file AND its tool-call log. It is
// idempotent: a missing file is success (port.PrunableStore contract), so
// concurrent List/Delete races are tolerated by construction.
//
// REMOVAL ORDER is load-bearing: the tools sidecar goes FIRST and the session
// file LAST, because the session file is what List enumerates. A partial
// failure then leaves the pair still VISIBLE (the session file survives, so
// the next retention sweep retries the whole Delete); the reverse order would
// leave an INVISIBLE orphaned .tools.jsonl that no future sweep can ever find
// (List ignores sidecars without a session file — a pre-existing orphan is
// accepted as unreachable; this ordering prevents us from ever creating one).
func (st *Store) Delete(_ context.Context, id session.SessionID) error {
	st.mu.Lock()
	defer st.mu.Unlock()
	for _, path := range []string{st.toolsPath(id), st.sessionPath(id)} {
		if err := os.Remove(path); err != nil && !os.IsNotExist(err) {
			return fmt.Errorf("jsonlstore: delete %q: %w", id, err)
		}
	}
	return nil
}

// decodeSessionID reads the REAL session id out of a session file's latest
// snapshot line (only the "id" field is decoded; the rest of the snapshot is
// skipped). It mirrors Load's latest-line-wins read.
func decodeSessionID(path string) (session.SessionID, error) {
	f, err := os.Open(path) //nolint:gosec // path is derived from the store dir listing
	if err != nil {
		return "", err
	}
	defer func() { _ = f.Close() }()
	var last []byte
	sc := bufio.NewScanner(f)
	sc.Buffer(make([]byte, 0, 64*1024), 16*1024*1024)
	for sc.Scan() {
		b := sc.Bytes()
		if len(strings.TrimSpace(string(b))) == 0 {
			continue
		}
		last = append(last[:0], b...) // copy: scanner reuses its buffer
	}
	if err := sc.Err(); err != nil {
		return "", err
	}
	if last == nil {
		return "", fmt.Errorf("jsonlstore: %s: empty session file", path)
	}
	var head struct {
		ID session.SessionID `json:"id"`
	}
	if err := json.Unmarshal(last, &head); err != nil {
		return "", fmt.Errorf("jsonlstore: %s: decode snapshot id: %w", path, err)
	}
	if head.ID == "" {
		return "", fmt.Errorf("jsonlstore: %s: snapshot carries no id", path)
	}
	return head.ID, nil
}

// toolCallRecord is the structured line written by ToolCall. It is a flat,
// self-describing record for offline replay/analysis.
type toolCallRecord struct {
	Type         string             `json:"type"` // always "tool_call"
	Time         time.Time          `json:"time"`
	SessionID    session.SessionID  `json:"session_id"`
	CallID       session.ToolCallID `json:"call_id"`
	Tool         string             `json:"tool"`
	Args         json.RawMessage    `json:"args,omitempty"`
	Result       string             `json:"result"`
	IsError      bool               `json:"is_error"`
	QueuedMicros int64              `json:"queued_micros"`
	TookMicros   int64              `json:"took_micros"`
}

// ToolCall appends a structured tool-call record to the per-session tool log,
// including both the dispatch queue time (queued) and the execution wall time
// (took) in microseconds. It satisfies port.ToolCallRecorder. Errors are intentionally
// swallowed (the port has no error return) but the record is best-effort durable.
func (st *Store) ToolCall(id session.SessionID, call session.ToolCall, result session.ToolResult, queued, took time.Duration) {
	rec := toolCallRecord{
		Type:         "tool_call",
		Time:         time.Now().UTC(),
		SessionID:    id,
		CallID:       call.ID,
		Tool:         call.Name,
		Args:         call.Args,
		Result:       result.Content,
		IsError:      result.IsError,
		QueuedMicros: queued.Microseconds(),
		TookMicros:   took.Microseconds(),
	}
	line, err := json.Marshal(rec)
	if err != nil {
		return
	}
	st.mu.Lock()
	defer st.mu.Unlock()
	_ = appendLine(st.toolsPath(id), line)
}

func (st *Store) sessionPath(id session.SessionID) string {
	return filepath.Join(st.dir, safeName(id)+sessionFileSuffix)
}

func (st *Store) toolsPath(id session.SessionID) string {
	return filepath.Join(st.dir, safeName(id)+toolsFileSuffix)
}

// appendLine appends b followed by a newline to the file at path, opening it
// for append (creating it if needed). Each line is a complete JSON record.
func appendLine(path string, b []byte) error {
	f, err := os.OpenFile(path, os.O_APPEND|os.O_CREATE|os.O_WRONLY, 0o644) //nolint:gosec // path is sanitized
	if err != nil {
		return fmt.Errorf("jsonlstore: open for append: %w", err)
	}
	if _, err := f.Write(append(b, '\n')); err != nil {
		_ = f.Close()
		return fmt.Errorf("jsonlstore: append: %w", err)
	}
	if err := f.Close(); err != nil {
		return fmt.Errorf("jsonlstore: close after append: %w", err)
	}
	return nil
}

// safeName maps a SessionID to a filename-safe token so it cannot traverse out
// of the store dir. Any rune that is not alphanumeric, '-', '_' or '.' becomes
// '_'. A leading '.' is also neutralized.
func safeName(id session.SessionID) string {
	s := string(id)
	if s == "" {
		return "_empty_"
	}
	var b strings.Builder
	b.Grow(len(s))
	for _, r := range s {
		switch {
		case r >= 'a' && r <= 'z', r >= 'A' && r <= 'Z', r >= '0' && r <= '9',
			r == '-', r == '_', r == '.':
			b.WriteRune(r)
		default:
			b.WriteRune('_')
		}
	}
	out := b.String()
	if strings.HasPrefix(out, ".") {
		out = "_" + out[1:]
	}
	return out
}
