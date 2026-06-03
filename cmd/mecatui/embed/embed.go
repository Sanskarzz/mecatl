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
// contracts/gen, grpc, internal/app, internal/adapter/*, and the server adapter.
// The render packages (ui, theme) and the client package import none of it.
package embed

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"net"
	"net/http"
	"os"
	"path/filepath"
	"runtime"
	"runtime/trace"
	"time"

	"go.opentelemetry.io/otel"
	"google.golang.org/grpc"
	"google.golang.org/grpc/health"
	healthpb "google.golang.org/grpc/health/grpc_health_v1"

	mecatlv1 "github.com/stacklok/mecatl/contracts/gen/go/mecatl/v1"
	"github.com/stacklok/mecatl/internal/adapter/server"
	"github.com/stacklok/mecatl/internal/adapter/telemetry"
	"github.com/stacklok/mecatl/internal/app"
)

// socketName is the fixed socket filename inside the per-process temp directory.
// The directory is randomised (os.MkdirTemp), so the filename can be stable.
const socketName = "mecated.sock"

// DefaultPerfAddr is the loopback default for the opt-in perf admin listener when
// PerfConfig.Enabled is set but Addr is empty. It deliberately uses an ephemeral
// port (":0") so a mecatui run never clashes with a co-running mecated whose own
// admin listener defaults to 127.0.0.1:9090 — the chosen address is logged (and
// returned via Server.AdminAddr) so an operator can point a browser/pprof at it.
const DefaultPerfAddr = "127.0.0.1:0"

// PerfConfig is the opt-in perf-observability configuration for the embedded
// server (decision 7 in docs/design/perf-observability.md). It is OFF by default
// (the zero value): mecatui hosts a bare gRPC socket with no telemetry, exactly
// as before. When Enabled, Start arms the SAME runtime-introspection surface
// mecated exposes — pprof, expvar, the runtime/RSS snapshot, and the execution
// FlightRecorder — on a loopback HTTP listener, plus the domain-metrics EventSink
// wired into the embedded engine so turn/tool/latency series render at /metrics.
//
// The motivating incident (the WASM tree-sitter render-starvation + memory
// freeze) was a mecatui process freeze, so goroutines, RSS, pprof, and the flight
// recorder are exactly the instruments it needed — hence covering the embedded
// server, not just the standalone daemon.
type PerfConfig struct {
	// Enabled turns the whole perf surface on. The zero value (false) means no
	// telemetry, no admin listener, no flight recorder, no watchdog.
	Enabled bool
	// Addr is the loopback HTTP listen address for the admin mux. Empty defaults
	// to DefaultPerfAddr (an ephemeral loopback port). The bound address (with the
	// resolved port) is logged and exposed via Server.AdminAddr.
	Addr string
	// GoroutineWarnThreshold arms the live goroutine-leak watchdog (decision 10):
	// a background sampler logs slog.Warn whenever runtime.NumGoroutine() exceeds
	// this count. 0 (default) disables the alarm; the runtime collector still
	// exports the goroutine count as a /metrics series regardless.
	GoroutineWarnThreshold int
	// GoroutineWarnInterval is how often the watchdog samples NumGoroutine. <= 0
	// falls back to the watchdog's own 30s default.
	GoroutineWarnInterval time.Duration
	// Logger receives the perf-surface startup/teardown lines and the watchdog
	// alarms. Nil falls back to slog.Default().
	Logger *slog.Logger
}

// Server is a mecated server hosted in the current process, listening on a UNIX
// socket. Close it to stop serving and release the socket, temp dir, and any
// composition-owned resources (the MCP manager) plus, when perf is enabled, the
// admin listener, the watchdog, the flight recorder, and the telemetry providers.
// It is safe to call Close once.
type Server struct {
	target  string // gRPC dial target, e.g. "unix:///run/user/1000/mecatui-123/mecated.sock"
	dir     string // private temp dir holding the socket
	grpc    *grpc.Server
	appstop func() // app.Built.Close — tears down MCP etc.

	// Perf teardown (all nil/no-op when PerfConfig.Enabled is false). adminSrv is
	// the loopback admin HTTP server; perfStop cancels the watchdog ctx and stops
	// the flight recorder; perfShutdown flushes the telemetry providers.
	adminAddr    string
	adminSrv     *http.Server
	perfStop     func()
	perfShutdown func(context.Context) error

	// recorderArmed is true only when THIS server armed the process FlightRecorder
	// (ProcessFlightRecorder returned nil) — i.e. it owns it and will Stop it on
	// Close. False when perf is off, the recorder failed, or this server coalesced
	// onto an instance another owner armed. It exists so a test can tell whether
	// /debug/flightrecorder will serve a live snapshot (armed) versus a stopped
	// process-singleton (the sync.Once limitation; see ProcessFlightRecorder).
	recorderArmed bool
}

// Start builds the harness from cfg via internal/app and serves it over a fresh
// UNIX socket in a private temp directory. The returned Server's Target() is a
// gRPC dial string a client can connect to immediately (the listener is open
// before Start returns; serving runs on a background goroutine).
//
// When perf.Enabled, Start ALSO installs the perf-observability surface
// (decision 7): it builds a telemetry MeterProvider + prometheus registry via the
// same telemetry.Setup path mecated uses, wires the domain-metrics EventSink into
// the embedded engine, registers the runtime collector + process-RSS gauge, arms
// the process-singleton FlightRecorder, optionally arms the goroutine watchdog,
// and serves the admin mux (/metrics, /debug/pprof/*, /debug/vars,
// /debug/flightrecorder) on a loopback HTTP listener. All of it is torn down by
// Server.Close, so an enabled perf surface never leaks a listener, a watchdog
// goroutine, or the flight recorder's runtime-trace subscription.
//
// ctx governs the lifetime of composition-owned background work (MCP manager,
// memory consolidation) AND the perf watchdog; cancelling it does NOT stop the
// gRPC or admin servers — call Close for that. On any setup error Start cleans up
// everything it created before returning, so the caller never leaks a socket,
// temp dir, or telemetry resource.
func Start(ctx context.Context, cfg app.Config, perf PerfConfig) (*Server, error) {
	// Perf setup happens BEFORE app.Build so the domain-metrics EventSink can be
	// injected into the engine via cfg.Sink/cfg.Logger. perfState gathers the
	// teardown handles; on any later error we unwind it.
	ps, err := setupPerf(ctx, perf, &cfg)
	if err != nil {
		return nil, err
	}

	built, err := app.Build(ctx, cfg)
	if err != nil {
		ps.teardown(ctx)
		return nil, err
	}

	dir, err := os.MkdirTemp(runtimeDir(), "mecatui-")
	if err != nil {
		built.Close()
		ps.teardown(ctx)
		return nil, fmt.Errorf("create runtime dir: %w", err)
	}
	sock := filepath.Join(dir, socketName)

	lis, err := net.Listen("unix", sock)
	if err != nil {
		built.Close()
		_ = os.RemoveAll(dir)
		ps.teardown(ctx)
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

	srv := &Server{
		target:        "unix://" + sock,
		dir:           dir,
		grpc:          grpcSrv,
		appstop:       built.Close,
		adminAddr:     ps.adminAddr,
		adminSrv:      ps.adminSrv,
		perfStop:      ps.stop,
		perfShutdown:  ps.shutdown,
		recorderArmed: ps.recorderArmed,
	}
	return srv, nil
}

// RecorderArmed reports whether THIS server armed (and therefore owns + will stop)
// the process FlightRecorder. It is false when perf is disabled, when the recorder
// failed to start, or when this server coalesced onto a recorder another owner
// armed — including a second perf-enabled run in the same process after the first
// run stopped the process-singleton (the sync.Once cannot be re-armed). Tests use
// it to decide whether /debug/flightrecorder will serve a live snapshot.
func (s *Server) RecorderArmed() bool { return s.recorderArmed }

// Target returns the gRPC dial string for the hosted server (a "unix://" target).
func (s *Server) Target() string { return s.target }

// AdminAddr returns the bound loopback address of the perf admin listener
// (/metrics, /debug/pprof, /debug/vars, /debug/flightrecorder), or "" when perf
// is disabled. With an ephemeral DefaultPerfAddr it reflects the actual chosen
// port, so a caller can log or display where to point a browser/pprof.
func (s *Server) AdminAddr() string { return s.adminAddr }

// Close stops the gRPC server gracefully, tears down composition-owned resources
// and (when perf was enabled) the admin listener, the watchdog, the flight
// recorder, and the telemetry providers, then removes the socket and its temp
// directory. It is safe to call once.
func (s *Server) Close() error {
	s.grpc.GracefulStop()

	// Tear down the perf surface in reverse order of construction: stop the admin
	// listener, then the watchdog + flight recorder, then flush the providers.
	if s.adminSrv != nil {
		shutdownCtx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		_ = s.adminSrv.Shutdown(shutdownCtx)
		cancel()
	}
	if s.perfStop != nil {
		s.perfStop()
	}
	if s.perfShutdown != nil {
		shutdownCtx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		_ = s.perfShutdown(shutdownCtx)
		cancel()
	}

	if s.appstop != nil {
		s.appstop()
	}
	return os.RemoveAll(s.dir)
}

// perfState gathers the teardown handles for an armed perf surface so Start can
// unwind cleanly on a later error and Close can release everything in one place.
type perfState struct {
	adminAddr     string
	adminSrv      *http.Server
	stop          func()                      // cancels the watchdog ctx + stops the flight recorder
	shutdown      func(context.Context) error // flushes the telemetry providers
	recorderArmed bool                        // this server armed (and owns) the process FlightRecorder
}

// teardown releases everything a partially- or fully-built perfState holds. It is
// safe on a zero perfState (perf disabled), so Start's error paths can call it
// unconditionally.
func (p perfState) teardown(ctx context.Context) {
	if p.adminSrv != nil {
		shutdownCtx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		_ = p.adminSrv.Shutdown(shutdownCtx)
		cancel()
	}
	if p.stop != nil {
		p.stop()
	}
	if p.shutdown != nil {
		_ = p.shutdown(ctx)
	}
}

// setupPerf installs the perf-observability surface when perf.Enabled, mutating
// cfg to inject the domain-metrics EventSink/Logger into the engine BEFORE
// app.Build runs. It returns a perfState carrying the teardown handles (a zero
// perfState when perf is disabled). On any setup error it unwinds whatever it has
// already built and returns the error.
func setupPerf(ctx context.Context, perf PerfConfig, cfg *app.Config) (perfState, error) {
	if !perf.Enabled {
		return perfState{}, nil
	}
	logger := perf.Logger
	if logger == nil {
		logger = slog.Default()
	}

	// Telemetry providers: metrics ALWAYS on (no OTLP endpoint needed — the
	// embedded server only serves loopback Prometheus + introspection), the same
	// Setup path mecated uses. The runtime collector (go.goroutine.count, GC, heap)
	// is started against this MeterProvider by Setup.
	providers, err := telemetry.Setup(ctx, telemetry.OTLPConfig{ServiceName: "mecatui-embedded"})
	if err != nil {
		return perfState{}, fmt.Errorf("setup telemetry: %w", err)
	}
	var ps perfState
	ps.shutdown = providers.Shutdown

	// Process-RSS gauge (mecatl.process.rss): Linux-only, no-op elsewhere; rides
	// the same MeterProvider so it renders on /metrics (decision 9).
	if rerr := telemetry.RegisterProcessGauges(providers.Meter); rerr != nil {
		ps.teardown(ctx)
		return perfState{}, fmt.Errorf("setup process gauges: %w", rerr)
	}

	// Domain-metrics EventSink: wire turn/tool/latency instruments into the
	// embedded engine so the embedded server's domain series show up at /metrics
	// too (app.Config.Sink/Logger are optional injection — the engine nil-guards
	// both). app.Build already supports this seam, so we use it.
	metrics, err := telemetry.NewMetrics(providers.Meter)
	if err != nil {
		ps.teardown(ctx)
		return perfState{}, fmt.Errorf("setup metrics: %w", err)
	}
	tracing := telemetry.NewTracing(otel.GetTracerProvider())
	cfg.Sink = telemetry.NewSink(metrics, tracing)
	cfg.Logger = metrics

	// FlightRecorder: arm the bounded execution-trace ring buffer via the
	// process-singleton accessor (only one may be active process-wide). We may
	// either ARM it (rerr == nil ⇒ this call created+started the singleton) or
	// COALESCE onto an instance another owner already armed
	// (ErrFlightRecorderAlreadyActive ⇒ the recorder is still usable for snapshots,
	// but we did NOT start it). Only the owner may Stop it: ownsRecorder is true
	// solely in the arming case, mirroring mecated's owner-only-stops pattern (it
	// `defer recorder.Stop()`s only in its default arm branch). Stopping a recorder
	// we merely coalesced onto would yank the runtime-trace subscription out from
	// under its real owner (a co-running mecated, or a second embed in this
	// process). NOTE the sync.Once singleton: once ANY owner Stops the process
	// recorder it cannot be re-armed in the same process — fine for the
	// one-embed-per-process production wiring, but a hard constraint for a test
	// binary or any future multi-embed (see ProcessFlightRecorder's doc).
	var recorder *telemetry.FlightRecorder
	var ownsRecorder bool
	rec, rerr := telemetry.ProcessFlightRecorder(trace.FlightRecorderConfig{})
	switch {
	case errors.Is(rerr, telemetry.ErrFlightRecorderAlreadyActive):
		recorder = rec // usable for snapshots, but another owner armed it
		logger.Info("flight recorder already active process-wide; reusing the shared instance (will NOT stop it — not our recorder)")
	case rerr != nil:
		logger.Warn("flight recorder failed to start; continuing without it", "err", rerr)
	default:
		recorder = rec
		ownsRecorder = true // this call armed it ⇒ this server stops it on Close
	}
	ps.recorderArmed = ownsRecorder

	// Goroutine-leak watchdog (decision 10): bound to a child ctx we cancel on
	// teardown so the alarm itself never leaks.
	watchdogCtx, cancelWatchdog := context.WithCancel(ctx)
	if perf.GoroutineWarnThreshold > 0 {
		telemetry.StartGoroutineWatchdog(watchdogCtx, perf.GoroutineWarnThreshold, perf.GoroutineWarnInterval, runtime.NumGoroutine, logger)
		logger.Info("goroutine-leak watchdog armed (embedded server)",
			"threshold", perf.GoroutineWarnThreshold, "interval", perf.GoroutineWarnInterval)
	}

	// stop cancels the watchdog and — ONLY if this server armed it — stops the
	// flight recorder. A coalesced recorder belongs to another owner, so we must
	// not Stop it here (that would tear down the shared runtime-trace subscription
	// the real owner still relies on). The provider shutdown stays separate
	// (ps.shutdown) so Close can order it after the listener drain.
	ps.stop = func() {
		cancelWatchdog()
		if recorder != nil && ownsRecorder {
			recorder.Stop()
		}
	}

	// Admin listener: bind eagerly so the chosen (possibly ephemeral) port is known
	// before Start returns. SECURITY: pprof/expvar/flightrecorder output can embed
	// prompt text, file paths, and goroutine stacks — this MUST stay loopback-bound
	// (decision 6); it is never mounted on the gRPC service surface.
	addr := perf.Addr
	if addr == "" {
		addr = DefaultPerfAddr
	}
	lis, lerr := net.Listen("tcp", addr)
	if lerr != nil {
		ps.teardown(ctx)
		return perfState{}, fmt.Errorf("listen perf admin %q: %w", addr, lerr)
	}
	ps.adminAddr = lis.Addr().String()
	ps.adminSrv = &http.Server{
		Handler:           telemetry.NewAdminMux(providers.Registry, recorder),
		ReadHeaderTimeout: 10 * time.Second,
	}
	go func() {
		if serveErr := ps.adminSrv.Serve(lis); serveErr != nil && !errors.Is(serveErr, http.ErrServerClosed) {
			logger.Warn("perf admin server stopped", "err", serveErr)
		}
	}()
	logger.Info("perf admin server listening (loopback, UNAUTHENTICATED — single-user trust model)",
		"addr", ps.adminAddr, "paths", "/metrics /debug/pprof /debug/vars /debug/flightrecorder")

	return ps, nil
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
