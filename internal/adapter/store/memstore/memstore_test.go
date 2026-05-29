package memstore_test

import (
	"context"
	"encoding/json"
	"errors"
	"reflect"
	"sync"
	"testing"
	"time"

	"github.com/stacklok/ozzharness/internal/adapter/store/memstore"
	"github.com/stacklok/ozzharness/internal/session"
)

func driven(t *testing.T) *session.Session {
	t.Helper()
	s := session.New("s1", session.ModeAccept, "/ws", session.Limits{
		MaxTurns: 5, MaxToolCalls: 9, MaxConsecutiveFailures: 2,
	}, time.Unix(1700000000, 0).UTC())
	_ = s.BeginTurn()
	_ = s.RecordAssistant(session.NewAssistantMessage("hi", "", []session.ToolCall{
		session.NewToolCall("c1", "Read", json.RawMessage(`{"path":"a"}`)),
	}))
	_ = s.RecordToolResults([]session.ToolResult{session.NewToolResult("c1", "data")})
	return s
}

func TestSaveLoadRoundTrip(t *testing.T) {
	ctx := context.Background()
	st := memstore.New()
	want := driven(t)
	if err := st.Save(ctx, want); err != nil {
		t.Fatalf("Save: %v", err)
	}
	got, err := st.Load(ctx, "s1")
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	if got.State != want.State || got.Mode != want.Mode || got.Limits != want.Limits ||
		got.Counters != want.Counters {
		t.Fatalf("scalar mismatch: got %+v want %+v", got, want)
	}
	if !reflect.DeepEqual(got.Conversation, want.Conversation) {
		t.Fatalf("conversation mismatch:\n got %+v\nwant %+v", got.Conversation, want.Conversation)
	}
}

func TestLoadNotFound(t *testing.T) {
	_, err := memstore.New().Load(context.Background(), "nope")
	if !errors.Is(err, memstore.ErrNotFound) {
		t.Fatalf("err = %v, want ErrNotFound", err)
	}
}

func TestSaveDeepCopyNoAliasing(t *testing.T) {
	ctx := context.Background()
	st := memstore.New()
	s := driven(t)
	if err := st.Save(ctx, s); err != nil {
		t.Fatalf("Save: %v", err)
	}
	// Mutate the original after saving; the stored copy must be unaffected.
	_ = s.RecordToolResults([]session.ToolResult{session.NewToolResult("c2", "more")})

	got, err := st.Load(ctx, "s1")
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	if got.Conversation.Len() != 2 {
		t.Fatalf("stored conversation len = %d, want 2 (no aliasing to mutated original)", got.Conversation.Len())
	}
}

func TestConcurrentSaveLoad(t *testing.T) {
	ctx := context.Background()
	st := memstore.New()
	var wg sync.WaitGroup
	for i := 0; i < 50; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			s := driven(t)
			if err := st.Save(ctx, s); err != nil {
				t.Errorf("Save: %v", err)
				return
			}
			if _, err := st.Load(ctx, "s1"); err != nil {
				t.Errorf("Load: %v", err)
			}
		}()
	}
	wg.Wait()
}
