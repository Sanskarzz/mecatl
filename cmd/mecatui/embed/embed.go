// Package embed lets mecatui host its OWN mecated server in-process when no
// external one is running, so a single `mecatui` binary "just works" with no
// separately-spawned daemon and no TCP port.
//
// It assembles the harness via the SHARED composition layer (internal/app) — the
// exact same engine, tools, permission policy, and service the standalone mecated
// binary builds — and serves it over a per-process UNIX socket in a private temp
// directory. The TUI then dials that socket as an ordinary gRPC client, so the
// ui/theme/client packages stay pure: they never learn the server is in-process.
//
// Architectural boundary: this package — like cmd/mecatui/client and the
// cmd/mecatui main — is the ONLY place in the TUI tree allowed to import
// contracts/gen, grpc, internal/app, and the server adapter. The render packages
// (ui, theme) and the client package import none of it.
package embed

import (
	"context"
	"fmt"
	"net"
	"os"
	"path/filepath"

	"google.golang.org/grpc"
	"google.golang.org/grpc/health"
	healthpb "google.golang.org/grpc/health/grpc_health_v1"

	mecatlv1 "github.com/stacklok/mecatl/contracts/gen/go/mecatl/v1"
	"github.com/stacklok/mecatl/internal/adapter/server"
	"github.com/stacklok/mecatl/internal/app"
)

// socketName is the fixed socket filename inside the per-process temp directory.
// The directory is randomised (os.MkdirTemp), so the filename can be stable.
const socketName = "mecated.sock"

// Server is a mecated server hosted in the current process, listening on a UNIX
// socket. Close it to stop serving and release the socket, temp dir, and any
// composition-owned resources (the MCP manager). It is safe to call Close once.
type Server struct {
	target  string // gRPC dial target, e.g. "unix:///run/user/1000/mecatui-123/mecated.sock"
	dir     string // private temp dir holding the socket
	grpc    *grpc.Server
	appstop func() // app.Built.Close — tears down MCP etc.
}

// Start builds the harness from cfg via internal/app and serves it over a fresh
// UNIX socket in a private temp directory. The returned Server's Target() is a
// gRPC dial string a client can connect to immediately (the listener is open
// before Start returns; serving runs on a background goroutine).
//
// ctx governs the lifetime of composition-owned background work (MCP manager,
// memory consolidation); cancelling it does NOT stop the gRPC server — call Close
// for that. On any setup error Start cleans up everything it created before
// returning, so the caller never leaks a socket or temp dir.
func Start(ctx context.Context, cfg app.Config) (*Server, error) {
	built, err := app.Build(ctx, cfg)
	if err != nil {
		return nil, err
	}

	dir, err := os.MkdirTemp(runtimeDir(), "mecatui-")
	if err != nil {
		built.Close()
		return nil, fmt.Errorf("create runtime dir: %w", err)
	}
	sock := filepath.Join(dir, socketName)

	lis, err := net.Listen("unix", sock)
	if err != nil {
		built.Close()
		_ = os.RemoveAll(dir)
		return nil, fmt.Errorf("listen unix %q: %w", sock, err)
	}

	// No auth/TLS interceptors: the socket lives in a private, user-owned temp dir
	// (0700 via MkdirTemp) and only this process knows its path — the same
	// single-user loopback trust model mecated uses for 127.0.0.1, with a tighter
	// blast radius (filesystem perms, no network surface at all).
	grpcSrv := grpc.NewServer()
	mecatlv1.RegisterHarnessServiceServer(grpcSrv, server.NewHarnessServer(built.Service))

	// Mount the standard gRPC health service so client.IsReachable-style probes
	// (and orchestration tooling) can confirm readiness over the same socket.
	healthSrv := health.NewServer()
	healthpb.RegisterHealthServer(grpcSrv, healthSrv)
	healthSrv.SetServingStatus("", healthpb.HealthCheckResponse_SERVING)
	healthSrv.SetServingStatus("mecatl.v1.HarnessService", healthpb.HealthCheckResponse_SERVING)

	go func() { _ = grpcSrv.Serve(lis) }() // returns when GracefulStop is called

	return &Server{
		target:  "unix://" + sock,
		dir:     dir,
		grpc:    grpcSrv,
		appstop: built.Close,
	}, nil
}

// Target returns the gRPC dial string for the hosted server (a "unix://" target).
func (s *Server) Target() string { return s.target }

// Close stops the gRPC server gracefully, tears down composition-owned resources,
// and removes the socket and its temp directory. It is safe to call once.
func (s *Server) Close() error {
	s.grpc.GracefulStop()
	if s.appstop != nil {
		s.appstop()
	}
	return os.RemoveAll(s.dir)
}

// runtimeDir picks the base directory for the per-process socket dir: the
// XDG_RUNTIME_DIR (a user-private tmpfs on Linux desktops) when set, else the OS
// temp dir. An empty return makes os.MkdirTemp fall back to os.TempDir itself.
func runtimeDir() string {
	if d := os.Getenv("XDG_RUNTIME_DIR"); d != "" {
		return d
	}
	return ""
}
