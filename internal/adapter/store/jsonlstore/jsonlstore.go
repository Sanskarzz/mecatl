// Package jsonlstore implements an append-only, JSONL-backed port.SessionStore
// and port.Logger. It is the observability/replay seam: every Save appends a
// session snapshot as one JSON line to a per-session file, and every ToolCall
// appends a structured tool-call record to a per-session log. Nothing is ever
// overwritten, so the files form a replayable audit trail; Load reads the most
// recent snapshot line.
//
// Layout under the configured dir:
//
//	<dir>/<id>.session.jsonl   — one snapshot per Save (latest line wins)
//	<dir>/<id>.tools.jsonl     — one record per Logger.ToolCall
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

	"github.com/stacklok/mecatl/internal/adapter/store/sessnap"
	"github.com/stacklok/mecatl/internal/port"
	"github.com/stacklok/mecatl/internal/session"
)

// ErrNotFound is returned by Load when no snapshot file exists for the id.
var ErrNotFound = fmt.Errorf("jsonlstore: session not found")

// Store is an append-only JSONL SessionStore and Logger rooted at a directory.
type Store struct {
	dir string
	mu  sync.Mutex // serializes appends across files
}

// compile-time assertions that Store satisfies both ports.
var (
	_ port.SessionStore = (*Store)(nil)
	_ port.Logger       = (*Store)(nil)
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

// toolCallRecord is the structured line written by ToolCall. It is a flat,
// self-describing record for offline replay/analysis.
type toolCallRecord struct {
	Type       string             `json:"type"` // always "tool_call"
	Time       time.Time          `json:"time"`
	SessionID  session.SessionID  `json:"session_id"`
	CallID     session.ToolCallID `json:"call_id"`
	Tool       string             `json:"tool"`
	Args       json.RawMessage    `json:"args,omitempty"`
	Result     string             `json:"result"`
	IsError    bool               `json:"is_error"`
	TookMicros int64              `json:"took_micros"`
}

// ToolCall appends a structured tool-call record to the per-session tool log.
// It satisfies port.Logger. Errors are intentionally swallowed (the port has no
// error return) but the record is best-effort durable.
func (st *Store) ToolCall(id session.SessionID, call session.ToolCall, result session.ToolResult, took time.Duration) {
	rec := toolCallRecord{
		Type:       "tool_call",
		Time:       time.Now().UTC(),
		SessionID:  id,
		CallID:     call.ID,
		Tool:       call.Name,
		Args:       call.Args,
		Result:     result.Content,
		IsError:    result.IsError,
		TookMicros: took.Microseconds(),
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
	return filepath.Join(st.dir, safeName(id)+".session.jsonl")
}

func (st *Store) toolsPath(id session.SessionID) string {
	return filepath.Join(st.dir, safeName(id)+".tools.jsonl")
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
