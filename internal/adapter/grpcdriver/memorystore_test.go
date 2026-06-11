package grpcdriver

import (
	"context"
	"testing"

	"google.golang.org/grpc"

	driverv1 "github.com/stacklok/mecatl/contracts/gen/go/mecatl/driver/v1"
	"github.com/stacklok/mecatl/engine/tool"
	"github.com/stacklok/mecatl/internal/adapter/memory"
)

// newWiredMemoryStore returns a grpcdriver MemoryStore client over a bufconn
// server wrapping a fresh flock reference store, PLUS the backend itself so
// tests can compare against direct (non-wire) reads.
func newWiredMemoryStore(t *testing.T) (*MemoryStore, *memory.Store) {
	t.Helper()
	backend, err := memory.New(t.TempDir())
	if err != nil {
		t.Fatalf("memory.New: %v", err)
	}
	conn := dialBufconn(t, func(gs *grpc.Server) {
		driverv1.RegisterMemoryStoreServiceServer(gs, NewMemoryStoreServer(backend))
	})
	return NewMemoryStore(conn), backend
}

// TestRecallMissOverWire pins the §C row: a driver Recall miss is
// (zero, false, nil) — never an error, never NOT_FOUND.
func TestRecallMissOverWire(t *testing.T) {
	st, _ := newWiredMemoryStore(t)
	got, found, err := st.Recall(context.Background(), "no/such/key")
	if err != nil {
		t.Fatalf("Recall miss: err = %v, want nil", err)
	}
	if found {
		t.Error("Recall miss: found = true, want false")
	}
	if got != (tool.MemoryEntry{}) {
		t.Errorf("Recall miss: entry = %+v, want the zero value", got)
	}
}

// TestTimestampRoundTrip pins the timestamppb translation: the driver-stamped
// UpdatedAt crosses the wire intact (equal to a direct backend read).
func TestTimestampRoundTrip(t *testing.T) {
	st, backend := newWiredMemoryStore(t)
	ctx := context.Background()
	if err := st.RememberEntry(ctx, tool.MemoryEntry{Key: "k", Value: "v"}); err != nil {
		t.Fatalf("RememberEntry: %v", err)
	}
	wire, found, err := st.Recall(ctx, "k")
	if err != nil || !found {
		t.Fatalf("wire Recall: found=%v err=%v", found, err)
	}
	direct, found, err := backend.Recall(ctx, "k")
	if err != nil || !found {
		t.Fatalf("direct Recall: found=%v err=%v", found, err)
	}
	if wire.UpdatedAt.IsZero() {
		t.Error("wire UpdatedAt is zero, want the driver-stamped write time")
	}
	if !wire.UpdatedAt.Equal(direct.UpdatedAt) {
		t.Errorf("wire UpdatedAt = %v, want the backend's %v", wire.UpdatedAt, direct.UpdatedAt)
	}
}

// TestBlankKeyRejectedOverWire pins the §C row: a blank/whitespace-only key
// on RememberEntry is rejected (the server wrapper pre-validates with
// INVALID_ARGUMENT).
func TestBlankKeyRejectedOverWire(t *testing.T) {
	st, _ := newWiredMemoryStore(t)
	for _, key := range []string{"", "   ", "\t\n"} {
		if err := st.RememberEntry(context.Background(), tool.MemoryEntry{Key: key, Value: "v"}); err == nil {
			t.Errorf("RememberEntry(key=%q) = nil error, want rejection", key)
		}
	}
}
