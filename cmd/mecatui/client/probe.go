package client

import (
	"context"
	"time"

	"google.golang.org/grpc"
	"google.golang.org/grpc/credentials/insecure"
	healthpb "google.golang.org/grpc/health/grpc_health_v1"
)

// probeTimeout bounds a single reachability check. A running mecated answers the
// health RPC over loopback in well under this; a closed port fails fast (connection
// refused) without waiting for it.
const probeTimeout = 500 * time.Millisecond

// IsReachable reports whether a mecated server is already serving at addr. It dials
// the standard gRPC health service (which mecated mounts OUTSIDE auth, so no token
// is needed) and returns true only when the server reports SERVING within
// probeTimeout. Any dial/RPC error — including connection-refused when nothing is
// listening — returns false.
//
// It is plaintext-only: the auto-detect path probes the loopback default, where
// mecated serves plaintext. A user pointing mecatui at a TLS/remote server passes
// --server explicitly and skips this probe.
func IsReachable(ctx context.Context, addr string) bool {
	conn, err := grpc.NewClient(addr, grpc.WithTransportCredentials(insecure.NewCredentials()))
	if err != nil {
		return false
	}
	defer func() { _ = conn.Close() }()

	ctx, cancel := context.WithTimeout(ctx, probeTimeout)
	defer cancel()

	resp, err := healthpb.NewHealthClient(conn).Check(ctx, &healthpb.HealthCheckRequest{})
	if err != nil {
		return false
	}
	return resp.GetStatus() == healthpb.HealthCheckResponse_SERVING
}
