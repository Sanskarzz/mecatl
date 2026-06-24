package main

import (
	"context"
	"errors"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"github.com/alicebob/miniredis/v2"

	"github.com/stacklok/mecatl/engine/port"
	"github.com/stacklok/mecatl/engine/session"
	"github.com/stacklok/mecatl/internal/adapter/redisstore"
	"github.com/stacklok/mecatl/internal/adapter/server"
	"github.com/stacklok/mecatl/internal/app"
)

// TestParseFlagsK8sDefaults asserts the k8s-native defaults parse: --headless
// defaults true, --posture defaults "auto", --session-lease-k8s-namespace
// defaults "mecatl", --grpc-addr/--http-addr bind 0.0.0.0, and --redis-url is
// empty by default (storage-free is opt-in via the flag, not forced).
func TestParseFlagsK8sDefaults(t *testing.T) {
	def, err := parseFlags(nil)
	if err != nil {
		t.Fatalf("parseFlags(nil): %v", err)
	}
	if !def.headless {
		t.Errorf("headless default = false, want true (mecak8s is a headless daemon)")
	}
	if def.posture != "auto" {
		t.Errorf("posture default = %q, want auto (recommended UNATTENDED tier)", def.posture)
	}
	if def.sessionLeaseK8sNamespace != defaultK8sLeaseNamespace {
		t.Errorf("sessionLeaseK8sNamespace default = %q, want %q", def.sessionLeaseK8sNamespace, defaultK8sLeaseNamespace)
	}
	if def.grpcAddr != defaultGRPCAddr {
		t.Errorf("grpcAddr default = %q, want %q (a pod binds 0.0.0.0)", def.grpcAddr, defaultGRPCAddr)
	}
	if def.httpAddr != defaultHTTPAddr {
		t.Errorf("httpAddr default = %q, want %q", def.httpAddr, defaultHTTPAddr)
	}
	if def.redisURL != "" {
		t.Errorf("redisURL default = %q, want empty (storage-free is opt-in)", def.redisURL)
	}
	if def.postureFlagSet {
		t.Error("postureFlagSet default = true, want false (flag not given)")
	}
}

// TestAppConfigMapsK8sFields asserts appConfig threads the k8s-native fields
// onto the shared app.Config: RedisURL, SessionLeaseK8sNamespace, the headless
// inversion (Interactive=!headless), and the posture.
func TestAppConfigMapsK8sFields(t *testing.T) {
	cfg, err := parseFlags([]string{
		"--redis-url", "redis:6379",
		"--session-lease-k8s-namespace", "myns",
		"--headless=false",
		"--posture", "trusted",
	})
	if err != nil {
		t.Fatalf("parseFlags: %v", err)
	}
	ac := appConfig(cfg, port.NopDiagnostics{})
	if ac.RedisURL != "redis:6379" {
		t.Errorf("app.Config RedisURL = %q, want redis:6379", ac.RedisURL)
	}
	if ac.SessionLeaseK8sNamespace != "myns" {
		t.Errorf("app.Config SessionLeaseK8sNamespace = %q, want myns", ac.SessionLeaseK8sNamespace)
	}
	if !ac.Interactive {
		t.Error("app.Config Interactive = false with --headless=false, want true (Interactive=!headless)")
	}
	if ac.Posture != app.PostureTrusted {
		t.Errorf("app.Config Posture = %v, want PostureTrusted", ac.Posture)
	}
	if !cfg.postureFlagSet {
		t.Error("postureFlagSet = false after --posture, want true")
	}
}

// TestAppConfigHeadlessDefault asserts the headless DEFAULT (true) maps to
// Interactive=false so a child's unresolved ask engages the auto-deny path.
func TestAppConfigHeadlessDefault(t *testing.T) {
	cfg, err := parseFlags(nil)
	if err != nil {
		t.Fatalf("parseFlags: %v", err)
	}
	ac := appConfig(cfg, port.NopDiagnostics{})
	if ac.Interactive {
		t.Error("default mecak8s must be Interactive=false (headless=true default) so the auto-deny/reviewer path engages")
	}
}

// TestBuildOverRedisDrivesRunToCompletion is the composition e2e: a full
// app.Build with --mock + a real (miniredis) Redis store, then a run driven to
// completion through the Service. It proves the k8s-native composition wiring
// (Redis session store + durable event log, headless, auto posture) assembles
// and serves a run end-to-end. Fully offline: mockllm + miniredis, no API key,
// no network.
func TestBuildOverRedisDrivesRunToCompletion(t *testing.T) {
	mr, err := miniredis.Run()
	if err != nil {
		t.Fatalf("miniredis: %v", err)
	}
	defer mr.Close()

	// A k8s lease namespace would require a real apiserver; leave it empty so
	// no lease is wired (the drain gate + run path are exercised regardless).
	built, err := app.Build(context.Background(), app.Config{
		Workspace:               t.TempDir(),
		UseMock:                 true,
		NoSoul:                  true,
		NoUserModel:             true,
		RedisURL:                mr.Addr(),
		Interactive:             false, // mecak8s headless default (Interactive=!headless)
		Posture:                 app.PostureAuto,
		PermissionsConventional: false,
		AgentsConventional:      false,
		Diagnostics:             port.NopDiagnostics{},
	})
	if err != nil {
		t.Fatalf("app.Build over Redis: %v", err)
	}
	defer built.Close()

	sess, err := built.Service.CreateSession(context.Background(), t.TempDir(), session.ModeDefault, session.Limits{})
	if err != nil {
		t.Fatalf("CreateSession: %v", err)
	}
	run, err := built.Service.StartRun(context.Background(), sess.ID, "hello from mecak8s")
	if err != nil {
		t.Fatalf("StartRun: %v", err)
	}
	var stop session.StopReason
	for ev := range run.Events() {
		if ev.Type == session.EvResult && ev.Result != nil {
			stop = ev.Result.Stop
		}
	}
	built.Service.FinishRun(sess.ID, run)
	if stop != session.StopEndTurn {
		t.Fatalf("run stop = %q, want end_turn (the mock produces a clean text turn)", stop)
	}

	// The session must be persisted to Redis (storage-free): a fresh Load
	// recovers it, proving the store is wired through composition.
	loaded, lerr := built.Service.GetSession(context.Background(), sess.ID)
	if lerr != nil {
		t.Fatalf("GetSession after run (Redis persistence): %v", lerr)
	}
	if loaded.ID != sess.ID {
		t.Errorf("loaded session ID = %q, want %q", loaded.ID, sess.ID)
	}
}

// TestDrainRejectsNewRunsViaComposition asserts the drain gate (ADR 0048)
// works through the composition-built Service: after Drain, StartRun returns
// ErrUnavailable and IsDraining reports true. It exercises the IsDraining
// method the /readyz ReadyFunc closes over.
func TestDrainRejectsNewRunsViaComposition(t *testing.T) {
	built, err := app.Build(context.Background(), app.Config{
		Workspace:               t.TempDir(),
		UseMock:                 true,
		NoSoul:                  true,
		NoUserModel:             true,
		PermissionsConventional: false,
		AgentsConventional:      false,
		Diagnostics:             port.NopDiagnostics{},
	})
	if err != nil {
		t.Fatalf("app.Build: %v", err)
	}
	defer built.Close()

	if built.Service.IsDraining() {
		t.Fatal("IsDraining = true on a fresh service, want false")
	}
	sess, err := built.Service.CreateSession(context.Background(), t.TempDir(), session.ModeDefault, session.Limits{})
	if err != nil {
		t.Fatalf("CreateSession: %v", err)
	}
	built.Service.Drain()
	if !built.Service.IsDraining() {
		t.Fatal("IsDraining = false after Drain, want true")
	}
	_, err = built.Service.StartRun(context.Background(), sess.ID, "after drain")
	if !errors.Is(err, server.ErrUnavailable) {
		t.Fatalf("StartRun after Drain = %v, want ErrUnavailable (503 via the drain gate)", err)
	}
}

// TestDrainHTTPFlipsReadyz asserts the /drain endpoint arms the drain gate and
// the dynamic ReadyFunc flips to not-ready. It builds a real HealthHandler over
// the composition Service's IsDraining + a nil-Redis (drain-gated-only)
// readiness, drives /drain, and checks /readyz transitions 200→503.
func TestDrainHTTPFlipsReadyz(t *testing.T) {
	built, err := app.Build(context.Background(), app.Config{
		Workspace:               t.TempDir(),
		UseMock:                 true,
		NoSoul:                  true,
		NoUserModel:             true,
		PermissionsConventional: false,
		AgentsConventional:      false,
		Diagnostics:             port.NopDiagnostics{},
	})
	if err != nil {
		t.Fatalf("app.Build: %v", err)
	}
	defer built.Close()

	// The nil-Redis readiness branch (drain-gated only), as serve.go wires it.
	ready := server.ReadyFunc(func() bool { return !built.Service.IsDraining() })
	hh := server.NewHealthHandler(ready)
	mux := http.NewServeMux()
	hh.RegisterHealth(mux)
	mux.HandleFunc("GET /drain", func(w http.ResponseWriter, _ *http.Request) {
		built.Service.Drain()
		// Skip the propagation sleep in the test (it's a fixed 3s window).
		w.WriteHeader(http.StatusOK)
	})

	srv := httptest.NewServer(mux)
	defer srv.Close()

	// Before drain: ready (200).
	if code := probe(t, srv.URL+"/readyz"); code != http.StatusOK {
		t.Fatalf("readyz before drain = %d, want 200", code)
	}
	// Hit /drain (arms the gate).
	if code := probe(t, srv.URL+"/drain"); code != http.StatusOK {
		t.Fatalf("/drain = %d, want 200", code)
	}
	// After drain: not ready (503) — the endpoint controller would remove the pod.
	if code := probe(t, srv.URL+"/readyz"); code != http.StatusServiceUnavailable {
		t.Fatalf("readyz after drain = %d, want 503 (drain gate flips readiness)", code)
	}
}

// TestRedisPinger asserts redisPinger: a live miniredis pings true; a closed
// miniredis pings false (so /readyz flips not-ready on a Redis outage); a nil
// store yields a nil pinger (drain-gated-only readiness).
func TestRedisPinger(t *testing.T) {
	if redisPinger(nil) != nil {
		t.Fatal("redisPinger(nil) != nil, want nil (no Redis → drain-gated readiness)")
	}
	mr, err := miniredis.Run()
	if err != nil {
		t.Fatalf("miniredis: %v", err)
	}
	st, err := redisstore.New(mr.Addr())
	if err != nil {
		t.Fatalf("redisstore.New: %v", err)
	}
	defer st.Close()
	ping := redisPinger(st)
	if ping == nil {
		t.Fatal("redisPinger over a live store = nil, want a func")
	}
	if !ping() {
		t.Error("ping() on a live miniredis = false, want true")
	}
	mr.Close()
	// After the broker closes, the ping must fail (a short-timeout ctx keeps it
	// from wedging). Allow a brief window for the client to notice.
	deadline := time.Now().Add(3 * time.Second)
	for time.Now().Before(deadline) {
		if !ping() {
			return
		}
		time.Sleep(50 * time.Millisecond)
	}
	t.Error("ping() on a closed miniredis stayed true, want false (Redis outage → /readyz not-ready)")
}

// TestParseFlagsHasNoStoreDir asserts mecak8s wires NO --store-dir flag at all
// (storage-free): the flag is simply absent from the FlagSet.
func TestParseFlagsHasNoStoreDir(t *testing.T) {
	if _, err := parseFlags([]string{"--store-dir", "/tmp/x"}); err == nil {
		t.Fatal("parseFlags(--store-dir) = nil, want an error (mecak8s is storage-free; --store-dir is not a flag)")
	}
}

// probe issues a GET and returns the status code.
func probe(t *testing.T, url string) int {
	t.Helper()
	resp, err := http.Get(url)
	if err != nil {
		t.Fatalf("GET %s: %v", url, err)
	}
	defer func() { _ = resp.Body.Close() }()
	return resp.StatusCode
}
