package grpcdriver

import (
	"context"
	"errors"
	"strings"

	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"

	driverv1 "github.com/stacklok/mecatl/contracts/gen/go/mecatl/driver/v1"
	"github.com/stacklok/mecatl/engine/adapter/sessnap"
	"github.com/stacklok/mecatl/engine/port"
	"github.com/stacklok/mecatl/engine/session"
	"github.com/stacklok/mecatl/engine/tool"
)

// Server wrappers: mount an IN-PROCESS store as the generated driver server
// interfaces, so a Go driver process (or a bufconn test fixture) is the
// in-process store plus this file plus a grpc.Server. The session wrapper
// runs sessnap SERVER-side too, so a conformance run over wrapper+memstore
// exercises the full encode→wire→decode→state-machine→encode→wire→decode
// path. Exported from this internal adapter for the conformance fixtures;
// PROMOTING them to an importable location for external Go driver authors is
// a deliberate decision for a future DRIVERS.md, not implied here.
//
// CAPACITY: a driver mounting these wrappers MUST construct its grpc.Server
// with grpc.MaxRecvMsgSize(MaxSnapshotBytes) (the protocol's required minimum
// snapshot capacity; gRPC's default 4 MiB receive cap rejects a legitimate
// media-carrying snapshot). The harness client's send/receive limits are
// already raised to the same value by Dial; the bufconn conformance fixture
// does the same server-side.

// sessionStoreServer adapts a port.SessionStore to SessionStoreServiceServer.
type sessionStoreServer struct {
	driverv1.UnimplementedSessionStoreServiceServer
	store port.SessionStore
}

// NewSessionStoreServer wraps st as a SessionStoreService driver server.
func NewSessionStoreServer(st port.SessionStore) driverv1.SessionStoreServiceServer {
	return &sessionStoreServer{store: st}
}

// Save decodes the envelope (rejecting a non-SnapshotFormat payload, a
// malformed payload, or a top-level session_id that disagrees with the id
// inside the payload — all INVALID_ARGUMENT) and persists the restored
// session in the wrapped store.
func (s *sessionStoreServer) Save(ctx context.Context, req *driverv1.SaveRequest) (*driverv1.SaveResponse, error) {
	if req.GetSessionId() == "" {
		return nil, status.Error(codes.InvalidArgument, "session_id is required")
	}
	snap := req.GetSnapshot()
	if snap == nil {
		return nil, status.Error(codes.InvalidArgument, "snapshot is required")
	}
	if got := snap.GetFormat(); got != SnapshotFormat {
		return nil, status.Errorf(codes.InvalidArgument, "unknown snapshot format %q (this server speaks %q)", got, SnapshotFormat)
	}
	if len(snap.GetPayload()) == 0 {
		return nil, status.Error(codes.InvalidArgument, "snapshot payload is empty")
	}
	sess, err := sessnap.Unmarshal(snap.GetPayload())
	if err != nil {
		return nil, status.Errorf(codes.InvalidArgument, "decode snapshot: %v", err)
	}
	// Keying guard: the TOP-LEVEL session_id is the storage key (the proto
	// contract — a driver may key without decoding), so a payload carrying a
	// different id would store one session under another's key. Reject it.
	if string(sess.ID) != req.GetSessionId() {
		return nil, status.Errorf(codes.InvalidArgument,
			"session_id %q does not match the snapshot payload's session id %q (the top-level session_id is the storage key; the two must agree)",
			req.GetSessionId(), sess.ID)
	}
	if err := s.store.Save(ctx, sess); err != nil {
		return nil, storeStatus(err)
	}
	return &driverv1.SaveResponse{}, nil
}

// Load fetches the session from the wrapped store and re-encodes it into the
// envelope. A store not-found (port.ErrSessionNotFound) maps to NOT_FOUND.
func (s *sessionStoreServer) Load(ctx context.Context, req *driverv1.LoadRequest) (*driverv1.LoadResponse, error) {
	if req.GetSessionId() == "" {
		return nil, status.Error(codes.InvalidArgument, "session_id is required")
	}
	sess, err := s.store.Load(ctx, session.SessionID(req.GetSessionId()))
	if err != nil {
		return nil, storeStatus(err)
	}
	line, err := sessnap.Marshal(sess)
	if err != nil {
		return nil, status.Errorf(codes.Internal, "encode snapshot: %v", err)
	}
	return &driverv1.LoadResponse{
		Snapshot: &driverv1.SessionSnapshot{Format: SnapshotFormat, Payload: line},
	}, nil
}

// memoryStoreServer adapts a tool.MemoryStore to MemoryStoreServiceServer.
type memoryStoreServer struct {
	driverv1.UnimplementedMemoryStoreServiceServer
	store tool.MemoryStore
}

// NewMemoryStoreServer wraps st as a MemoryStoreService driver server.
func NewMemoryStoreServer(st tool.MemoryStore) driverv1.MemoryStoreServiceServer {
	return &memoryStoreServer{store: st}
}

// RememberEntry stores the entry; a blank/whitespace-only key is rejected
// with INVALID_ARGUMENT before the store is consulted (the wrapped store's
// own rejection remains conformance-tested in-process).
func (s *memoryStoreServer) RememberEntry(ctx context.Context, req *driverv1.RememberEntryRequest) (*driverv1.RememberEntryResponse, error) {
	e := req.GetEntry()
	if e == nil {
		return nil, status.Error(codes.InvalidArgument, "entry is required")
	}
	if strings.TrimSpace(e.GetKey()) == "" {
		return nil, status.Error(codes.InvalidArgument, "entry key must not be blank")
	}
	if err := s.store.RememberEntry(ctx, fromProtoEntry(e)); err != nil {
		return nil, storeStatus(err)
	}
	return &driverv1.RememberEntryResponse{}, nil
}

// Recall looks up the exact key; a miss is found=false, never NOT_FOUND.
func (s *memoryStoreServer) Recall(ctx context.Context, req *driverv1.RecallRequest) (*driverv1.RecallResponse, error) {
	e, found, err := s.store.Recall(ctx, req.GetKey())
	if err != nil {
		return nil, storeStatus(err)
	}
	resp := &driverv1.RecallResponse{Found: found}
	if found {
		resp.Entry = toProtoEntry(e)
	}
	return resp, nil
}

// List returns the prefix-filtered, key-sorted entries with values present.
func (s *memoryStoreServer) List(ctx context.Context, req *driverv1.ListRequest) (*driverv1.ListResponse, error) {
	entries, err := s.store.List(ctx, req.GetPrefix())
	if err != nil {
		return nil, storeStatus(err)
	}
	return &driverv1.ListResponse{Entries: toProtoServerEntries(entries)}, nil
}

// Forget deletes the key; missing keys succeed (idempotent).
func (s *memoryStoreServer) Forget(ctx context.Context, req *driverv1.ForgetRequest) (*driverv1.ForgetResponse, error) {
	if err := s.store.Forget(ctx, req.GetKey()); err != nil {
		return nil, storeStatus(err)
	}
	return &driverv1.ForgetResponse{}, nil
}

// Index returns the tier-0 routing table (values omitted by the store).
func (s *memoryStoreServer) Index(ctx context.Context, _ *driverv1.IndexRequest) (*driverv1.IndexResponse, error) {
	entries, err := s.store.Index(ctx)
	if err != nil {
		return nil, storeStatus(err)
	}
	return &driverv1.IndexResponse{Entries: toProtoServerEntries(entries)}, nil
}

// Search returns the store's best-first matches (values omitted by the store).
func (s *memoryStoreServer) Search(ctx context.Context, req *driverv1.SearchRequest) (*driverv1.SearchResponse, error) {
	entries, err := s.store.Search(ctx, req.GetQuery(), int(req.GetK()))
	if err != nil {
		return nil, storeStatus(err)
	}
	return &driverv1.SearchResponse{Entries: toProtoServerEntries(entries)}, nil
}

// storeStatus maps a wrapped store's error onto the driver protocol's status
// vocabulary (the §C table): the not-found sentinel → NOT_FOUND, context
// errors → CANCELLED / DEADLINE_EXCEEDED, everything else → INTERNAL.
func storeStatus(err error) error {
	switch {
	case errors.Is(err, port.ErrSessionNotFound):
		return status.Error(codes.NotFound, err.Error())
	case errors.Is(err, context.Canceled):
		return status.Error(codes.Canceled, err.Error())
	case errors.Is(err, context.DeadlineExceeded):
		return status.Error(codes.DeadlineExceeded, err.Error())
	default:
		return status.Error(codes.Internal, err.Error())
	}
}

// toProtoServerEntries projects store entries onto the wire form for
// responses, carrying the store's stamped UpdatedAt (toProtoEntry already
// guards the zero time → unset).
func toProtoServerEntries(entries []tool.MemoryEntry) []*driverv1.MemoryEntry {
	out := make([]*driverv1.MemoryEntry, len(entries))
	for i, e := range entries {
		out[i] = toProtoEntry(e)
	}
	return out
}
