package grpcdriver

import (
	"context"
	"fmt"

	"google.golang.org/grpc"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"

	driverv1 "github.com/stacklok/mecatl/contracts/gen/go/mecatl/driver/v1"
	"github.com/stacklok/mecatl/engine/adapter/sessnap"
	"github.com/stacklok/mecatl/engine/port"
	"github.com/stacklok/mecatl/engine/session"
)

// SnapshotFormat is the snapshot envelope format tag this client writes on
// Save and accepts on Load: the payload is exactly sessnap.Marshal output
// (one JSON line). The driver round-trips the tag verbatim; it changes ONLY
// if the encoding itself is replaced (sessnap's own schema evolution is
// additive and needs no bump). Load rejects any other tag with an
// infrastructure error — never ErrNotFound.
//
// SIGNPOST — a future format bump MUST be read-set-accept / write-newest:
// the readers (this client's Load, the server wrapper's Save decode) must
// keep ACCEPTING every previously-shipped format tag while Save WRITES only
// the newest. A bump that switches the write tag and rejects the old tag in
// the same step bricks every session already stored under the old format —
// the driver round-trips envelopes verbatim and cannot migrate them.
const SnapshotFormat = "sessnap-json/1"

// MaxSnapshotBytes is the protocol's REQUIRED MINIMUM message capacity for a
// snapshot envelope: 64 MiB. A session snapshot legitimately reaches multiple
// MiB (inline media parts alone may carry session.MaxPromptMediaBytes = 20 MiB
// in one prompt, base64-inflated by sessnap's JSON encoding), so gRPC's
// default 4 MiB receive cap would brick Save/Load mid-session. Dial raises
// the client's per-call send AND receive limits to this value; a conforming
// driver MUST accept payloads up to it on its server too
// (grpc.MaxRecvMsgSize(MaxSnapshotBytes) — see the server-wrapper notes in
// server.go and the SessionSnapshot doc in session_store.proto). Pinned by
// the storeconformance "large snapshot" subtest run over bufconn.
const MaxSnapshotBytes = 64 << 20

// ErrNotFound is returned by Load when the driver has no snapshot under the
// given id. It wraps port.ErrSessionNotFound so a consumer that may not
// import this adapter can distinguish not-found from an infra failure via
// errors.Is.
var ErrNotFound = fmt.Errorf("grpcdriver: session not found: %w", port.ErrSessionNotFound)

// SessionStore is a port.SessionStore over a remote SessionStoreService
// driver. Encode/decode happens HERE (sessnap), harness-side: the driver only
// ever sees the opaque envelope.
type SessionStore struct {
	client driverv1.SessionStoreServiceClient
}

// compile-time assertion that SessionStore satisfies the port.
var _ port.SessionStore = (*SessionStore)(nil)

// NewSessionStore wraps an established driver connection (see Dial) as a
// port.SessionStore.
func NewSessionStore(conn grpc.ClientConnInterface) *SessionStore {
	return &SessionStore{client: driverv1.NewSessionStoreServiceClient(conn)}
}

// Save encodes s via sessnap and persists it under s.ID on the driver,
// overwriting any prior snapshot. A nil session fails client-side with
// sessnap.ErrNilSession (no RPC), matching the local stores.
func (st *SessionStore) Save(ctx context.Context, s *session.Session) error {
	line, err := sessnap.Marshal(s)
	if err != nil {
		return err
	}
	if _, err := st.client.Save(ctx, &driverv1.SaveRequest{
		SessionId: string(s.ID),
		Snapshot:  &driverv1.SessionSnapshot{Format: SnapshotFormat, Payload: line},
	}); err != nil {
		return rpcErr(ctx, "save", err)
	}
	return nil
}

// Load fetches the most recent snapshot for id from the driver and restores
// it through sessnap. A driver NOT_FOUND maps to ErrNotFound (wrapping
// port.ErrSessionNotFound); an unknown envelope format, a payload that fails
// to decode, or a decoded session whose id is NOT the requested one (a
// mis-keyed driver) is an infrastructure error, never not-found.
func (st *SessionStore) Load(ctx context.Context, id session.SessionID) (*session.Session, error) {
	resp, err := st.client.Load(ctx, &driverv1.LoadRequest{SessionId: string(id)})
	if err != nil {
		if status.Code(err) == codes.NotFound {
			return nil, fmt.Errorf("%w: %q", ErrNotFound, id)
		}
		return nil, rpcErr(ctx, "load", err)
	}
	snap := resp.GetSnapshot()
	if got := snap.GetFormat(); got != SnapshotFormat {
		return nil, fmt.Errorf("grpcdriver: load %q: unknown snapshot format %q (this client speaks %q)", id, got, SnapshotFormat)
	}
	sess, err := sessnap.Unmarshal(snap.GetPayload())
	if err != nil {
		return nil, fmt.Errorf("grpcdriver: load %q: %w", id, err)
	}
	// Wrong-session guard: a driver that mis-keys its storage (or always
	// returns "the" session) must surface as a loud infra failure here, never
	// as a silently-adopted foreign session.
	if sess.ID != id {
		return nil, fmt.Errorf("grpcdriver: load %q: driver returned the snapshot of a DIFFERENT session %q (mis-keyed driver)", id, sess.ID)
	}
	return sess, nil
}

// rpcErr wraps a failed RPC's error. When the CALLER's ctx is already
// done, it wraps ctx.Err() instead, so errors.Is(err, context.Canceled /
// context.DeadlineExceeded) holds harness-side exactly as it does for the
// local stores. Everything else is an opaque infrastructure failure — NO
// transient/permanent classification (resilience, if ever needed, is a
// decorator; see the package doc).
func rpcErr(ctx context.Context, op string, err error) error {
	if ctxErr := ctx.Err(); ctxErr != nil {
		return fmt.Errorf("grpcdriver: %s: %w (rpc: %v)", op, ctxErr, err)
	}
	return fmt.Errorf("grpcdriver: %s: %w", op, err)
}
