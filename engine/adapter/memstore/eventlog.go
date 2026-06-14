package memstore

import (
	"context"
	"iter"
	"sync"

	"github.com/stacklok/mecatl/engine/port"
	"github.com/stacklok/mecatl/engine/session"
)

// EventLog is an in-memory, concurrency-safe port.EventLog — the durable-event
// sibling of the in-memory Store. It is the no-store-dir default the composition
// layer wires when SessionStore is memstore (so the event-log seam is never nil),
// and the mockable seam offline tests assert against: Append records each event
// per session id, Read replays them in append order.
//
// It does NOT round-trip through a serialization (the relay already hands it a
// value Event and the loop never mutates a past event), so the recorded events
// are stored by value directly.
type EventLog struct {
	mu     sync.Mutex
	events map[session.SessionID][]session.Event
}

// compile-time assertion that EventLog satisfies the port.
var _ port.EventLog = (*EventLog)(nil)

// NewEventLog constructs an empty in-memory event log.
func NewEventLog() *EventLog {
	return &EventLog{events: make(map[session.SessionID][]session.Event)}
}

// Append records ev under id in append order. It never fails (in-memory).
func (l *EventLog) Append(_ context.Context, id session.SessionID, ev session.Event) error {
	l.mu.Lock()
	l.events[id] = append(l.events[id], ev)
	l.mu.Unlock()
	return nil
}

// Read yields the events recorded under id in append order. A miss (no events)
// yields an empty sequence (absence is data). The slice is copied under the lock
// so a concurrent Append cannot race the iteration.
func (l *EventLog) Read(_ context.Context, id session.SessionID) iter.Seq2[session.Event, error] {
	l.mu.Lock()
	src := l.events[id]
	snapshot := make([]session.Event, len(src))
	copy(snapshot, src)
	l.mu.Unlock()
	return func(yield func(session.Event, error) bool) {
		for _, ev := range snapshot {
			if !yield(ev, nil) {
				return
			}
		}
	}
}
