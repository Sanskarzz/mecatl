package embed_test

import (
	"context"
	"errors"
	"io"
	"net"
	"net/http"
	"runtime/trace"
	"strings"
	"testing"
	"time"

	"go.uber.org/goleak"
	"google.golang.org/grpc"
	"google.golang.org/grpc/credentials/insecure"

	"github.com/stacklok/mecatl/cmd/mecatui/client"
	"github.com/stacklok/mecatl/cmd/mecatui/embed"
	mecatlv1 "github.com/stacklok/mecatl/contracts/gen/go/mecatl/v1"
	"github.com/stacklok/mecatl/internal/adapter/telemetry"
	"github.com/stacklok/mecatl/internal/app"
)

// driveTurn dials the embedded server's UNIX socket as a raw gRPC client, creates
// a session, and pushes ONE real prompt through a Converse stream, draining it to
// completion (the terminal "result" event, then EOF). It exists so a perf test
// can run a turn through the embedded engine — the only thing that makes the
// domain-metrics EventSink/Logger injection emit a mecatl_ series — rather than
// relying on the runtime collector alone. It fails the test on any wire error so
// a broken Converse path surfaces here, not as a confusing empty-metrics
// assertion downstream. It uses the generated proto client directly (this package
// is one of the few allowed to import contracts/gen) because the higher-level
// client.Stream exposes no synchronous Recv for a test to drain.
func driveTurn(ctx context.Context, t *testing.T, target, workspace string) {
	t.Helper()
	conn, err := grpc.NewClient(target, grpc.WithTransportCredentials(insecure.NewCredentials()))
	if err != nil {
		t.Fatalf("dial embedded server for turn: %v", err)
	}
	defer func() { _ = conn.Close() }()
	svc := mecatlv1.NewHarnessServiceClient(conn)

	cs, err := svc.CreateSession(ctx, &mecatlv1.CreateSessionRequest{Workspace: workspace})
	if err != nil {
		t.Fatalf("CreateSession for turn: %v", err)
	}
	stream, err := svc.Converse(ctx)
	if err != nil {
		t.Fatalf("open Converse: %v", err)
	}
	if err := stream.Send(&mecatlv1.ConverseRequest{
		Kind: &mecatlv1.ConverseRequest_Prompt{
			Prompt: &mecatlv1.Prompt{SessionId: cs.GetSessionId(), Text: "hello"},
		},
	}); err != nil {
		t.Fatalf("send prompt: %v", err)
	}
	// Drain to EOF. The terminal "result" event arrives just before the server
	// closes the stream; reaching EOF means the run completed and the engine
	// emitted its run/turn events through the injected Sink.
	for {
		_, rerr := stream.Recv()
		if errors.Is(rerr, io.EOF) {
			return
		}
		if rerr != nil {
			t.Fatalf("Converse Recv: %v", rerr)
		}
	}
}

// TestStartServesOverSocket is the end-to-end proof of the embedded path: Start
// builds the harness from a (mock-provider) app.Config, serves it over a private
// UNIX socket, and the ordinary TUI client can both health-probe it and drive a
// real unary RPC (CreateSession) across that socket — no TCP port, no daemon.
func TestStartServesOverSocket(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	workspace := t.TempDir()
	srv, err := embed.Start(ctx, app.Config{
		Workspace:  workspace,
		Model:      "mock-model",
		UseMock:    true, // offline: no network, no OPENAI_API_KEY needed
		Shell:      "/bin/sh",
		Compaction: "heuristic",
		Tokenizer:  "heuristic",
	}, embed.PerfConfig{})
	if err != nil {
		t.Fatalf("embed.Start: %v", err)
	}
	defer func() { _ = srv.Close() }()

	target := srv.Target()
	if target == "" {
		t.Fatal("Target() is empty")
	}

	// The standard gRPC health service must report SERVING over the socket.
	if !client.IsReachable(ctx, target) {
		t.Fatalf("embedded server not reachable at %q", target)
	}

	// A real HarnessService RPC must succeed over the socket, against the workspace.
	cl, err := client.Dial(client.DialConfig{Server: target})
	if err != nil {
		t.Fatalf("dial embedded server: %v", err)
	}
	defer func() { _ = cl.Close() }()

	sessID, _, err := cl.CreateSession(ctx, workspace, client.ModeFromString("default"))
	if err != nil {
		t.Fatalf("CreateSession over embedded socket: %v", err)
	}
	if sessID == "" {
		t.Fatal("CreateSession returned an empty session id")
	}
}

// TestStartWithMemoryDirServes asserts the embedded server builds and serves when
// a memory directory is configured — exercising app.Build's memory-registration
// gate (build.go: MemoryDir != "" ⇒ memory.New + memory.Register) end to end
// without network. A bad/empty dir would surface as a build error or a CreateSession
// failure; a clean session id proves the gate ran and registered without blowing up.
// It also exercises the slash-command gate (EnableCommands ⇒ a command lister ⇒
// caps.SlashCommands) over the same wire, since the embedded server now enables both
// opt-ins by default.
func TestStartWithMemoryDirServes(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	workspace := t.TempDir()
	srv, err := embed.Start(ctx, app.Config{
		Workspace:      workspace,
		Model:          "mock-model",
		UseMock:        true, // offline: no network, no OPENAI_API_KEY needed
		Shell:          "/bin/sh",
		Compaction:     "heuristic",
		Tokenizer:      "heuristic",
		MemoryDir:      t.TempDir(), // turns on the Remember/Recall registration gate
		EnableCommands: true,        // turns on the slash-command lister (caps.SlashCommands)
	}, embed.PerfConfig{})
	if err != nil {
		t.Fatalf("embed.Start with MemoryDir: %v", err)
	}
	defer func() { _ = srv.Close() }()

	cl, err := client.Dial(client.DialConfig{Server: srv.Target()})
	if err != nil {
		t.Fatalf("dial embedded server: %v", err)
	}
	defer func() { _ = cl.Close() }()

	sessID, caps, err := cl.CreateSession(ctx, workspace, client.ModeFromString("default"))
	if err != nil {
		t.Fatalf("CreateSession over embedded socket (memory enabled): %v", err)
	}
	if sessID == "" {
		t.Fatal("CreateSession returned an empty session id")
	}
	// The capabilities ride the create response over the embedded socket: with a
	// MemoryDir configured, app.Build registers the Remember tool, so the server
	// must report memory=true. This is the end-to-end proof that caps flow from
	// the BUILT catalog through the wire to the client (not a static guess).
	if !caps.Memory {
		t.Errorf("caps.Memory = false, want true (MemoryDir configured ⇒ Remember registered)")
	}
	if !caps.SlashCommands {
		t.Errorf("caps.SlashCommands = false, want true (EnableCommands ⇒ command lister wired)")
	}
}

// TestStartProviderError asserts Start surfaces app.Build's provider error (and
// leaks nothing) when neither OpenAI nor the mock is configured.
func TestStartProviderError(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	_, err := embed.Start(ctx, app.Config{Workspace: t.TempDir(), Model: "x"}, embed.PerfConfig{})
	if err == nil {
		t.Fatal("expected an error when no LLM provider is configured")
	}
}

// mockAppConfig is the offline (mock-provider) app.Config shared by the perf
// tests — no network, no OPENAI_API_KEY.
func mockAppConfig(workspace string) app.Config {
	return app.Config{
		Workspace:  workspace,
		Model:      "mock-model",
		UseMock:    true,
		Shell:      "/bin/sh",
		Compaction: "heuristic",
		Tokenizer:  "heuristic",
	}
}

// TestStartPerfDisabledStartsNoAdminListener asserts that WITHOUT --perf the
// embedded server hosts only the gRPC socket: AdminAddr() is empty and nothing
// extra is bound. This is the default posture (perf off).
func TestStartPerfDisabledStartsNoAdminListener(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	srv, err := embed.Start(ctx, mockAppConfig(t.TempDir()), embed.PerfConfig{})
	if err != nil {
		t.Fatalf("embed.Start (perf off): %v", err)
	}
	defer func() { _ = srv.Close() }()

	if addr := srv.AdminAddr(); addr != "" {
		t.Fatalf("AdminAddr() = %q, want empty when perf is disabled", addr)
	}
}

// TestStartPerfServesAdminSurface is the end-to-end proof of decision 7: with
// --perf the embedded server brings up a loopback admin listener on an ephemeral
// port (AdminAddr reports it) serving the runtime-introspection surface —
// /metrics, /debug/vars, /debug/pprof/ — and tears it ALL down on Close with no
// leaked listener or goroutine (the trailing goleak gate). The gRPC socket still
// works alongside it.
//
// ONLY ONE test in this binary may enable perf: ProcessFlightRecorder is a
// process-lifetime sync.Once, so once this test's Close stops the recorder it
// cannot be re-armed in the same process (see flightrecorder.go). Adding a second
// perf-enabled test here would coalesce onto a recorder this test already stopped.
func TestStartPerfServesAdminSurface(t *testing.T) {
	// Per-test goleak: the perf surface arms a watchdog ctx, a flight-recorder
	// runtime-trace subscription, an admin HTTP server, and telemetry providers —
	// Close must tear EVERY one down. We deliberately do NOT ignore
	// runtime/trace.Start.func1 or runtime.ReadTrace: those are the goroutines a
	// live flight recorder runs, so after an owner-Stop they MUST be gone. Leaving
	// them un-ignored is what makes this gate FAIL if Close ever forgets to stop
	// the recorder (which would also mask the ownsRecorder fix). The goleak defer
	// is registered FIRST so it runs LAST; Close is registered via t.Cleanup below
	// so it always runs BEFORE the gate, even on an early t.Fatalf (otherwise an
	// early failure would run goleak without Close and false-fail).
	defer goleak.VerifyNone(t)

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	workspace := t.TempDir()
	srv, err := embed.Start(ctx, mockAppConfig(workspace), embed.PerfConfig{
		Enabled:                true,
		Addr:                   "127.0.0.1:0", // ephemeral loopback
		GoroutineWarnThreshold: 1 << 30,       // armed but never fires
		GoroutineWarnInterval:  time.Millisecond,
	})
	if err != nil {
		t.Fatalf("embed.Start (perf on): %v", err)
	}
	// Register Close via t.Cleanup so it runs before the goleak gate (LIFO: the
	// defer above was registered first, so it fires last) even if a later
	// assertion calls t.Fatalf before the explicit Close.
	closed := false
	t.Cleanup(func() {
		if !closed {
			_ = srv.Close()
		}
	})

	addr := srv.AdminAddr()
	if addr == "" {
		t.Fatal("AdminAddr() is empty with perf enabled")
	}
	if host, _, _ := net.SplitHostPort(addr); host != "127.0.0.1" {
		t.Errorf("perf admin bound to %q, want a loopback (127.0.0.1) address", addr)
	}

	// The gRPC socket must still serve alongside the admin surface.
	if !client.IsReachable(ctx, srv.Target()) {
		t.Fatalf("embedded gRPC server not reachable at %q with perf enabled", srv.Target())
	}

	// Drive one real turn through the embedded engine BEFORE scraping /metrics and
	// /debug/flightrecorder: it (a) makes the domain-metrics Sink emit a mecatl_
	// series (the headline injection — runtime-collector series alone would pass a
	// mere non-empty check), and (b) fills the flight-recorder trace window so the
	// snapshot is non-empty.
	driveTurn(ctx, t, srv.Target(), workspace)

	base := "http://" + addr
	cases := []struct {
		path       string
		wantSubstr string // marker that must appear in the body (empty = any non-empty body)
	}{
		// /metrics must carry a DOMAIN series, not just runtime-collector output —
		// this is the actual proof the EventSink/Logger were injected into the
		// embedded engine. mecatl_events_total is emitted by every run.
		{"/metrics", "mecatl_"},
		{"/debug/vars", "mecatl_runtime"},      // the curated expvar key
		{"/debug/pprof/", "Types of profiles"}, // the pprof index page
	}
	for _, tc := range cases {
		t.Run(tc.path, func(t *testing.T) {
			resp, gerr := http.Get(base + tc.path)
			if gerr != nil {
				t.Fatalf("GET %s: %v", tc.path, gerr)
			}
			defer func() { _ = resp.Body.Close() }()
			if resp.StatusCode != http.StatusOK {
				t.Fatalf("GET %s status = %d, want 200", tc.path, resp.StatusCode)
			}
			body, _ := io.ReadAll(resp.Body)
			if len(body) == 0 {
				t.Fatalf("GET %s returned an empty body", tc.path)
			}
			if tc.wantSubstr != "" && !strings.Contains(string(body), tc.wantSubstr) {
				t.Errorf("GET %s body missing %q; got %.160q", tc.path, tc.wantSubstr, body)
			}
		})
	}

	// /debug/flightrecorder is the endpoint UNIQUE to the perf wiring (it is only
	// mounted when a recorder is non-nil). After a turn the trace window is
	// non-empty, so when THIS run armed the recorder the snapshot must be 200 + a
	// non-empty octet-stream beginning with the Go execution-trace magic ("go 1."
	// in the v2 trace header).
	//
	// Under `go test -count=N` this whole test re-runs in the SAME process, but
	// ProcessFlightRecorder is a process-lifetime sync.Once: run 1 arms+stops the
	// singleton, so runs 2..N coalesce onto a now-STOPPED recorder
	// (RecorderArmed()==false) and the endpoint correctly serves 503 (nothing to
	// snapshot). We assert the live-snapshot contract on the arming run and the
	// documented stopped-singleton behaviour on the coalesced runs — so -count=N
	// stays green without weakening the real assertion.
	t.Run("/debug/flightrecorder", func(t *testing.T) {
		resp, gerr := http.Get(base + "/debug/flightrecorder")
		if gerr != nil {
			t.Fatalf("GET /debug/flightrecorder: %v", gerr)
		}
		defer func() { _ = resp.Body.Close() }()
		if !srv.RecorderArmed() {
			// Coalesced onto an already-stopped process singleton (a re-run, or a
			// co-running owner): the documented limitation — no live window to snapshot.
			if resp.StatusCode != http.StatusServiceUnavailable {
				t.Fatalf("GET /debug/flightrecorder status = %d, want 503 when this run did not arm the recorder", resp.StatusCode)
			}
			return
		}
		if resp.StatusCode != http.StatusOK {
			t.Fatalf("GET /debug/flightrecorder status = %d, want 200 (this run armed the recorder)", resp.StatusCode)
		}
		if ct := resp.Header.Get("Content-Type"); ct != "application/octet-stream" {
			t.Errorf("GET /debug/flightrecorder Content-Type = %q, want application/octet-stream", ct)
		}
		body, _ := io.ReadAll(resp.Body)
		if len(body) == 0 {
			t.Fatal("GET /debug/flightrecorder returned an empty trace body")
		}
		if !strings.HasPrefix(string(body), "go 1.") {
			t.Errorf("flight-recorder body missing the trace magic; got %.16q", body)
		}
	})

	// Snapshot whether this run armed the recorder BEFORE Close (Close is the act
	// that stops it); the post-Close assertion below depends on it.
	armed := srv.RecorderArmed()

	// Close must tear down the admin listener: a follow-up GET must fail to connect.
	if err := srv.Close(); err != nil {
		t.Fatalf("Close: %v", err)
	}
	closed = true
	if resp, gerr := http.Get(base + "/metrics"); gerr == nil {
		_ = resp.Body.Close()
		t.Fatalf("admin listener still serving %s after Close — it leaked", base)
	}

	// DIRECT recorder-stop assertion (the reliable half of QA must-add #5): a
	// forgotten recorder Stop does NOT reliably leave a goroutine goleak can see
	// (runtime.ReadTrace is transient — it only runs during a WriteTo), so the
	// goleak gate ABOVE — now run WITHOUT the trace ignores — is necessary but not
	// sufficient on its own. We additionally assert the process FlightRecorder is
	// DISABLED after Close: when this run armed it, Close's owner-Stop must have
	// disabled it. ProcessFlightRecorder returns the same singleton (coalesced); its
	// Enabled() must report false. If Close ever stops the watchdog but forgets the
	// recorder (the ownsRecorder regression), Enabled() stays true and this FAILS —
	// which is the guarantee the goleak gate alone could not give.
	if armed {
		rec, _ := telemetry.ProcessFlightRecorder(trace.FlightRecorderConfig{})
		if rec == nil {
			t.Fatal("ProcessFlightRecorder returned nil after Close; expected the stopped singleton")
		}
		if rec.Enabled() {
			t.Fatal("flight recorder still Enabled() after Close — owner-Stop was skipped (ownsRecorder regression)")
		}
	}
	// The goleak gate (deferred above, runs last) is now run WITHOUT the
	// runtime/trace.Start.func1 + runtime.ReadTrace ignores, so any persistent
	// trace goroutine left by a forgotten Stop also fails it; the direct Enabled()
	// assertion above closes the gap for the transient-goroutine case.
}
