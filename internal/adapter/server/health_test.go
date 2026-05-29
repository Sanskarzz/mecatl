package server_test

import (
	"context"
	"net"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"google.golang.org/grpc"
	"google.golang.org/grpc/credentials/insecure"
	"google.golang.org/grpc/health"
	healthpb "google.golang.org/grpc/health/grpc_health_v1"
	"google.golang.org/grpc/test/bufconn"

	"github.com/stacklok/mecatl/internal/adapter/server"
)

// TestHTTPHealthEndpoints checks /healthz is always 200 and /readyz reflects the
// injected ReadyFunc.
func TestHTTPHealthEndpoints(t *testing.T) {
	ready := false
	mux := http.NewServeMux()
	server.NewHealthHandler(func() bool { return ready }).RegisterHealth(mux)
	srv := httptest.NewServer(mux)
	defer srv.Close()

	// Liveness is always 200.
	resp, err := http.Get(srv.URL + "/healthz")
	if err != nil {
		t.Fatalf("GET /healthz: %v", err)
	}
	resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("/healthz status = %d, want 200", resp.StatusCode)
	}

	// Readiness is 503 while not ready.
	resp, err = http.Get(srv.URL + "/readyz")
	if err != nil {
		t.Fatalf("GET /readyz: %v", err)
	}
	resp.Body.Close()
	if resp.StatusCode != http.StatusServiceUnavailable {
		t.Fatalf("/readyz not-ready status = %d, want 503", resp.StatusCode)
	}

	// Flip to ready -> 200.
	ready = true
	resp, err = http.Get(srv.URL + "/readyz")
	if err != nil {
		t.Fatalf("GET /readyz: %v", err)
	}
	resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("/readyz ready status = %d, want 200", resp.StatusCode)
	}
}

// TestGRPCHealthService checks the standard grpc_health_v1 service reports
// SERVING once registered.
func TestGRPCHealthService(t *testing.T) {
	lis := bufconn.Listen(1 << 20)
	gs := grpc.NewServer()
	hs := health.NewServer()
	healthpb.RegisterHealthServer(gs, hs)
	hs.SetServingStatus("", healthpb.HealthCheckResponse_SERVING)
	go func() { _ = gs.Serve(lis) }()
	defer gs.Stop()

	conn, err := grpc.NewClient("passthrough:///bufnet",
		grpc.WithContextDialer(func(ctx context.Context, _ string) (net.Conn, error) {
			return lis.DialContext(ctx)
		}),
		grpc.WithTransportCredentials(insecure.NewCredentials()),
	)
	if err != nil {
		t.Fatalf("dial: %v", err)
	}
	defer conn.Close()

	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	resp, err := healthpb.NewHealthClient(conn).Check(ctx, &healthpb.HealthCheckRequest{})
	if err != nil {
		t.Fatalf("health Check: %v", err)
	}
	if resp.GetStatus() != healthpb.HealthCheckResponse_SERVING {
		t.Fatalf("status = %v, want SERVING", resp.GetStatus())
	}
}
