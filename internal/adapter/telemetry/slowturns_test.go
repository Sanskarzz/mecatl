package telemetry

import (
	"context"
	"reflect"
	"sync"
	"testing"
	"time"

	"github.com/stacklok/mecatl/internal/session"
)

// emitTurn feeds one synthetic EvTurnEnd into the buffer with the given turn
// index and duration; ttft/interMax are derived deterministically so the stored
// scalars are distinguishable in assertions.
func emitTurn(b *SlowTurnBuffer, turn int, durationMs int64) {
	b.Emit(context.Background(), session.Event{
		Type: session.EvTurnEnd,
		Turn: turn,
		TurnEnd: &session.TurnEndPayload{
			DurationMs:      durationMs,
			TTFTMs:          durationMs / 10,
			InterTokenMaxMs: durationMs / 5,
		},
	})
}

// TestSlowTurnBufferRecordsScalarsFromTurnEnd asserts the buffer maps the
// TurnEndPayload scalars (and Event.Turn) into a SlowTurn, and ignores non-turn
// events and a nil payload.
func TestSlowTurnBufferRecordsScalarsFromTurnEnd(t *testing.T) {
	b := NewSlowTurnBuffer(8, func() time.Time { return time.Unix(1000, 0) })

	emitTurn(b, 3, 500)
	// Non-turn events and a nil payload must be ignored (no panic, no entry).
	b.Emit(context.Background(), session.Event{Type: session.EvMessageDelta, Text: "hello"})
	b.Emit(context.Background(), session.Event{Type: session.EvTurnEnd, Turn: 9, TurnEnd: nil})

	got := b.Recent(0)
	if len(got) != 1 {
		t.Fatalf("want 1 recorded turn, got %d", len(got))
	}
	w := got[0]
	if w.TurnIndex != 3 || w.DurationMs != 500 || w.TTFTMs != 50 || w.InterTokenMaxMs != 100 {
		t.Fatalf("scalars not copied from payload/event: %+v", w)
	}
	if !w.EndedAt.Equal(time.Unix(1000, 0)) {
		t.Fatalf("EndedAt should come from the injected clock, got %v", w.EndedAt)
	}
}

// TestSlowTurnBufferEvictsAtCapacity proves the ring is bounded: once full it
// overwrites the oldest, keeping only the most recent `capacity` turns.
func TestSlowTurnBufferEvictsAtCapacity(t *testing.T) {
	const capacity = 4
	b := NewSlowTurnBuffer(capacity, nil)
	for i := 0; i < 10; i++ {
		emitTurn(b, i, int64(100+i))
	}
	got := b.Recent(0)
	if len(got) != capacity {
		t.Fatalf("ring should hold exactly %d, got %d", capacity, len(got))
	}
	// Newest first: the last four turns are 9,8,7,6.
	wantIdx := []int{9, 8, 7, 6}
	for i, w := range wantIdx {
		if got[i].TurnIndex != w {
			t.Fatalf("position %d: want turn %d, got %d (full set %+v)", i, w, got[i].TurnIndex, got)
		}
	}
}

// TestSlowTurnBufferNewestFirstStableOrder asserts Recent returns newest-first
// and that the order is stable across repeated calls on an unchanged buffer — the
// contract the MCP tool's in-memory cursor pagination + totalCount depend on.
func TestSlowTurnBufferNewestFirstStableOrder(t *testing.T) {
	b := NewSlowTurnBuffer(16, nil)
	for i := 0; i < 5; i++ {
		emitTurn(b, i, int64(200+i))
	}
	first := b.Recent(0)
	second := b.Recent(0)
	if !reflect.DeepEqual(first, second) {
		t.Fatalf("Recent not stable across calls:\n first=%+v\nsecond=%+v", first, second)
	}
	// Newest first: turn 4 (most recent) leads, turn 0 trails.
	if first[0].TurnIndex != 4 || first[len(first)-1].TurnIndex != 0 {
		t.Fatalf("not newest-first: %+v", first)
	}
}

// TestSlowTurnBufferThresholdFilter asserts thresholdMs filters to turns at least
// that slow, and that 0 (or negative) returns the whole set.
func TestSlowTurnBufferThresholdFilter(t *testing.T) {
	b := NewSlowTurnBuffer(16, nil)
	emitTurn(b, 0, 100)
	emitTurn(b, 1, 500)
	emitTurn(b, 2, 1000)

	if all := b.Recent(0); len(all) != 3 {
		t.Fatalf("threshold 0 should return all 3, got %d", len(all))
	}
	if neg := b.Recent(-5); len(neg) != 3 {
		t.Fatalf("negative threshold should return all 3, got %d", len(neg))
	}
	slow := b.Recent(500)
	if len(slow) != 2 {
		t.Fatalf("threshold 500 should return 2 (500,1000), got %d: %+v", len(slow), slow)
	}
	for _, s := range slow {
		if s.DurationMs < 500 {
			t.Fatalf("threshold leaked a faster turn: %+v", s)
		}
	}
}

// TestSlowTurnHasNoTextFields is the redaction-by-shape guard: the stored struct
// must carry NO string/text field, so the buffer physically cannot hold prompt
// text, tool args, or session IDs. If a future change adds a string field, this
// fails loudly — exactly the architect's deferred MEDIUM.
func TestSlowTurnHasNoTextFields(t *testing.T) {
	ty := reflect.TypeOf(SlowTurn{})
	for i := 0; i < ty.NumField(); i++ {
		f := ty.Field(i)
		if f.Type.Kind() == reflect.String {
			t.Fatalf("SlowTurn.%s is a string: the buffer must store SCALARS ONLY (redaction by shape) — no text fields permitted", f.Name)
		}
	}
}

// TestSlowTurnBufferConcurrentEmitRecent proves the mutex actually guards the
// ring: N goroutines hammer Emit while another loops Recent(0), all in flight at
// once. Run under -race, dropping the lock (or returning the backing slice
// directly) trips the detector. The buffer's whole reason to exist is this
// concurrent access (the engine Emits from a run goroutine while an MCP tool
// reads via Recent), so the suite must exercise it concurrently.
func TestSlowTurnBufferConcurrentEmitRecent(t *testing.T) {
	const (
		writers        = 8
		emitsPerWriter = 500
		readerLoops    = 2000
	)
	b := NewSlowTurnBuffer(64, func() time.Time { return time.Unix(2000, 0) })

	var wg sync.WaitGroup
	wg.Add(writers + 1)

	for w := 0; w < writers; w++ {
		go func(base int) {
			defer wg.Done()
			for i := 0; i < emitsPerWriter; i++ {
				emitTurn(b, base+i, int64(100+i))
			}
		}(w * emitsPerWriter)
	}

	// Concurrent reader: loop Recent while the writers churn the ring. We only
	// assert it never panics / never races; the values are nondeterministic under
	// concurrency, so we don't check them here (that's the single-threaded tests).
	go func() {
		defer wg.Done()
		for i := 0; i < readerLoops; i++ {
			_ = b.Recent(0)
		}
	}()

	wg.Wait()

	// After the dust settles the ring is full and internally consistent.
	if got := b.Recent(0); len(got) != 64 {
		t.Fatalf("after %d emits the ring should be full (64), got %d", writers*emitsPerWriter, len(got))
	}
}

// TestSlowTurnBufferRecentReturnsDecoupledCopy asserts Recent returns a slice
// decoupled from the buffer's backing array: a slice captured before further
// Emits is unchanged by them. Catches a future "return the ring directly"
// aliasing regression that the stable-order test (which re-reads) would miss.
func TestSlowTurnBufferRecentReturnsDecoupledCopy(t *testing.T) {
	b := NewSlowTurnBuffer(4, func() time.Time { return time.Unix(3000, 0) })
	emitTurn(b, 0, 100)
	emitTurn(b, 1, 200)

	r := b.Recent(0)
	before := make([]SlowTurn, len(r))
	copy(before, r)

	// Emit enough to wrap the ring, overwriting the backing array slots the first
	// snapshot may have aliased.
	for i := 2; i < 10; i++ {
		emitTurn(b, i, int64(100+i))
	}

	if !reflect.DeepEqual(r, before) {
		t.Fatalf("Recent's result mutated after later Emits — it aliases the backing array:\n got=%+v\nwant=%+v", r, before)
	}
}

// TestSlowTurnBufferEmptyAndDefaults exercises the zero-state and the capacity/
// clock fallbacks.
func TestSlowTurnBufferEmptyAndDefaults(t *testing.T) {
	b := NewSlowTurnBuffer(0, nil) // <= 0 -> DefaultSlowTurnCapacity; nil clock -> time.Now
	if len(b.ring) != DefaultSlowTurnCapacity {
		t.Fatalf("zero capacity should fall back to %d, got %d", DefaultSlowTurnCapacity, len(b.ring))
	}
	if got := b.Recent(0); len(got) != 0 {
		t.Fatalf("empty buffer should return no turns, got %d", len(got))
	}
	emitTurn(b, 0, 100)
	if got := b.Recent(0); len(got) != 1 || got[0].EndedAt.IsZero() {
		t.Fatalf("default clock should stamp a non-zero EndedAt, got %+v", got)
	}
}
