package redisstore_test

import (
	"context"
	"errors"
	"strings"
	"testing"

	"github.com/alicebob/miniredis/v2"

	"github.com/stacklok/mecatl/engine/port"
	"github.com/stacklok/mecatl/engine/session"
	"github.com/stacklok/mecatl/internal/adapter/redisstore"
)

// TestNewRejectsEmptyAddr guards the constructor's fail-fast on an empty
// address (a nil-deref or a silent localhost default would be a worse failure
// mode than a clear error).
func TestNewRejectsEmptyAddr(t *testing.T) {
	if _, err := redisstore.New(""); err == nil {
		t.Fatal("New(\"\") = nil error, want rejection")
	}
}

// TestNewPingsAndFailsFastOnUnreachableBroker guards that New pings the broker
// so a misconfigured address surfaces at construction, not on the first Save.
func TestNewPingsAndFailsFastOnUnreachableBroker(t *testing.T) {
	// An address that refuses connections: miniredis closed before New pings.
	mr, err := miniredis.Run()
	if err != nil {
		t.Fatalf("miniredis: %v", err)
	}
	addr := mr.Addr()
	mr.Close()
	if _, err := redisstore.New(addr); err == nil {
		t.Fatal("New(closed broker) = nil error, want ping failure")
	}
}

// TestLoadNotFoundWrapsSentinelAndNamesId is the redis-specific not-found
// guarantee: redis.Nil on a missing key maps to an error wrapping
// port.ErrSessionNotFound AND naming the id (the conformance suite asserts the
// same, but this pins it against the raw Redis path without the suite's
// scaffolding).
func TestLoadNotFoundWrapsSentinelAndNamesId(t *testing.T) {
	st := newTestStore(t)
	const id session.SessionID = "redis-no-such"
	_, err := st.Load(context.Background(), id)
	if !errors.Is(err, port.ErrSessionNotFound) {
		t.Errorf("Load(missing) = %v, want errors.Is(_, ErrSessionNotFound)", err)
	}
	if !strings.Contains(err.Error(), string(id)) {
		t.Errorf("Load(missing) error %q does not name the id %q", err, id)
	}
}

// TestReadRejectsUnknownFormatTag guards the forward-incompatibility contract:
// a record carrying an unknown format tag must surface as an error on Read,
// never a silent skip.
func TestReadRejectsUnknownFormatTag(t *testing.T) {
	mr, err := miniredis.Run()
	if err != nil {
		t.Fatalf("miniredis: %v", err)
	}
	t.Cleanup(mr.Close)
	st, err := redisstore.New(mr.Addr())
	if err != nil {
		t.Fatalf("redisstore.New: %v", err)
	}
	t.Cleanup(func() { _ = st.Close() })

	const id session.SessionID = "redis-bad-tag"
	// Inject a record with a bogus format tag directly into the event list,
	// bypassing Append (which always stamps the correct tag).
	mr.RPush("mecatl:events:"+string(id), `{"v":"bogus-format/9","ev":{}}`)

	log := port.EventLog(st)
	var sawEvent bool
	var sawErr error
	for ev, err := range log.Read(context.Background(), id) {
		if err != nil {
			sawErr = err
			break
		}
		sawEvent = true
		_ = ev
	}
	if sawEvent {
		t.Fatal("Read yielded an event for a bogus-format record, want an error first")
	}
	if sawErr == nil {
		t.Fatal("Read yielded no error for a bogus-format record")
	}
}

// TestOverwriteReplacesSnapshot guards the HSET-overwrite path: a second Save
// replaces the blob (not appends), so Load returns the latest snapshot.
func TestOverwriteReplacesSnapshot(t *testing.T) {
	st := newTestStore(t)
	ctx := context.Background()
	s := newTestSession(t, "redis-overwrite")
	if err := s.RecordUserPrompt("first", nil); err != nil {
		t.Fatalf("RecordUserPrompt: %v", err)
	}
	if err := st.Save(ctx, s); err != nil {
		t.Fatalf("Save #1: %v", err)
	}
	if err := s.RecordUserPrompt("second", nil); err != nil {
		t.Fatalf("RecordUserPrompt: %v", err)
	}
	if err := st.Save(ctx, s); err != nil {
		t.Fatalf("Save #2: %v", err)
	}
	got, err := st.Load(ctx, s.ID)
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	if n := len(got.Conversation.Messages); n != 2 {
		t.Errorf("Load after overwrite = %d messages, want 2 (the latest snapshot)", n)
	}
}
