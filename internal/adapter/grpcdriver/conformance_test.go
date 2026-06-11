package grpcdriver

import (
	"testing"

	"google.golang.org/grpc"

	driverv1 "github.com/stacklok/mecatl/contracts/gen/go/mecatl/driver/v1"
	"github.com/stacklok/mecatl/engine/adapter/memconformance"
	"github.com/stacklok/mecatl/engine/adapter/memstore"
	"github.com/stacklok/mecatl/engine/adapter/storeconformance"
	"github.com/stacklok/mecatl/engine/port"
	"github.com/stacklok/mecatl/engine/tool"
	"github.com/stacklok/mecatl/internal/adapter/memory"
)

// The grpcdriver RETROFIT runs: the SAME shared conformance suites the
// in-process stores pass, run over the wire — client → bufconn → server
// wrapper → reference backend. Because the session server wrapper runs
// sessnap server-side, the session run exercises the FULL
// encode→wire→decode→state-machine→encode→wire→decode path.

// TestGRPCSessionStoreConformance runs the shared SessionStore conformance
// table over grpcdriver → bufconn → NewSessionStoreServer(memstore.New()).
func TestGRPCSessionStoreConformance(t *testing.T) {
	storeconformance.Run(t, func(t *testing.T) port.SessionStore {
		conn := dialBufconn(t, func(gs *grpc.Server) {
			driverv1.RegisterSessionStoreServiceServer(gs, NewSessionStoreServer(memstore.New()))
		})
		return NewSessionStore(conn)
	})
}

// TestGRPCMemoryStoreConformance runs the shared MemoryStore conformance
// table over grpcdriver → bufconn → NewMemoryStoreServer(memory.New(tmp)).
func TestGRPCMemoryStoreConformance(t *testing.T) {
	memconformance.Run(t, func(t *testing.T) tool.MemoryStore {
		backend, err := memory.New(t.TempDir())
		if err != nil {
			t.Fatalf("memory.New: %v", err)
		}
		conn := dialBufconn(t, func(gs *grpc.Server) {
			driverv1.RegisterMemoryStoreServiceServer(gs, NewMemoryStoreServer(backend))
		})
		return NewMemoryStore(conn)
	})
}
