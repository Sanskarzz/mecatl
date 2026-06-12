package mcpperf

import (
	"bytes"
	"context"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/google/pprof/profile"

	"github.com/stacklok/mecatl/internal/adapter/telemetry"
)

// --- rate limiter ---

func TestCPUGateCooldown(t *testing.T) {
	now := time.Unix(1000, 0)
	gate := newCPUGate(func() time.Time { return now })

	// First acquire succeeds and we release it (no profile in flight).
	first := gate.acquire()
	if !first.ok {
		t.Fatalf("first acquire should succeed: %q", first.reason)
	}
	first.release()

	// A second acquire within the cooldown is refused with a rate-limit message.
	now = now.Add(5 * time.Second)
	second := gate.acquire()
	if second.ok {
		t.Fatalf("second acquire within cooldown should be refused")
	}
	if !strings.Contains(second.reason, "rate limit") || !strings.Contains(second.reason, "retry in") {
		t.Errorf("rate-limit reason unexpected: %q", second.reason)
	}

	// After the cooldown elapses, acquire succeeds again.
	now = now.Add(defaultCPUCooldown)
	third := gate.acquire()
	if !third.ok {
		t.Errorf("acquire after cooldown should succeed: %q", third.reason)
	}
	third.release()
}

func TestCPUGateConcurrentInFlightRejected(t *testing.T) {
	gate := newCPUGate(func() time.Time { return time.Unix(2000, 0) })

	// Hold the first acquire in flight (do NOT release).
	first := gate.acquire()
	if !first.ok {
		t.Fatalf("first acquire should succeed")
	}

	// A concurrent acquire while one is in flight is rejected with the in-flight
	// message (NOT the cooldown message), regardless of the clock.
	second := gate.acquire()
	if second.ok {
		t.Fatalf("concurrent acquire should be rejected while one is in flight")
	}
	if !strings.Contains(second.reason, "already in progress") {
		t.Errorf("expected in-flight reason, got %q", second.reason)
	}
	first.release()
}

func TestClampCPUSeconds(t *testing.T) {
	cases := map[int]int{0: 5, -3: 5, 1: 1, 5: 5, 30: 30, 999: 30}
	for in, want := range cases {
		if got := clampCPUSeconds(in); got != want {
			t.Errorf("clampCPUSeconds(%d) = %d, want %d", in, got, want)
		}
	}
}

// --- redaction (end to end through the profiler seam) ---

// fakeProfiler serializes a prebuilt profile to real pprof bytes so the redaction
// path (parse → reduce) runs exactly as in production.
type fakeProfiler struct {
	named map[string]*profile.Profile
	cpu   *profile.Profile
}

func (f fakeProfiler) Lookup(name string) ([]byte, error) {
	p := f.named[name]
	if p == nil {
		return nil, errNoProfile
	}
	return marshalProfile(p)
}

func (f fakeProfiler) CPUProfile(time.Duration) ([]byte, error) {
	return marshalProfile(f.cpu)
}

var errNoProfile = &profileError{"no such profile"}

type profileError struct{ s string }

func (e *profileError) Error() string { return e.s }

func marshalProfile(p *profile.Profile) ([]byte, error) {
	var buf bytes.Buffer
	if err := p.Write(&buf); err != nil {
		return nil, err
	}
	return buf.Bytes(), nil
}

func TestRedactionThroughProfilerRoundTrip(t *testing.T) {
	// A profile whose function has an absolute path AND a sample label carrying
	// fake prompt text. After parse + reduce, NEITHER may appear in the output.
	prof := buildProfile("inuse_space", []funcSpec{
		{
			name:  "allocer",
			file:  "/home/victim/secret-workspace/internal/agent/loop.go",
			value: 2048,
			label: "SENSITIVE-PROMPT-TEXT-AND-TOKEN",
		},
	})
	raw, err := marshalProfile(prof)
	if err != nil {
		t.Fatalf("marshal: %v", err)
	}
	parsed, err := parseProfile(raw)
	if err != nil {
		t.Fatalf("parse: %v", err)
	}

	_, top := memTop(parsed, 10)
	if len(top) != 1 {
		t.Fatalf("expected 1 row, got %d", len(top))
	}
	row := top[0]
	if row.File != "loop.go" {
		t.Errorf("file not basenamed: %q", row.File)
	}
	// Render the full row to JSON-ish and assert no leak of path or label.
	blob := row.Function + "|" + row.File
	for _, banned := range []string{"/home", "victim", "secret-workspace", "SENSITIVE-PROMPT-TEXT-AND-TOKEN"} {
		if strings.Contains(blob, banned) {
			t.Errorf("redaction failed: %q leaked %q", blob, banned)
		}
	}
}

// --- transport posture (localhost / DNS-rebinding protection ON, no auth) ---

func TestPostureLocalhostProtectionRejectsForeignHost(t *testing.T) {
	srv := httptest.NewServer(Handler(minimalDeps()))
	defer srv.Close()

	// A request arriving on the loopback listener with a non-loopback Host header
	// is the DNS-rebinding shape; the SDK's default protection must 403 it.
	req, err := http.NewRequestWithContext(context.Background(), http.MethodPost, srv.URL, strings.NewReader("{}"))
	if err != nil {
		t.Fatalf("new request: %v", err)
	}
	req.Host = "evil.example.com"
	req.Header.Set("Content-Type", "application/json")
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatalf("do: %v", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusForbidden {
		t.Errorf("foreign Host status = %d, want 403 (localhost protection)", resp.StatusCode)
	}
}

func TestPostureTokenlessRequestAccepted(t *testing.T) {
	srv := httptest.NewServer(Handler(minimalDeps()))
	defer srv.Close()

	// No Authorization header: the surface is unauthenticated by design, so an
	// initialize (a valid MCP POST) is NOT rejected for lack of a token. We assert
	// it is not a 401/403; a full handshake is covered by the lifecycle test.
	body := `{"jsonrpc":"2.0","id":1,"method":"initialize","params":{"protocolVersion":"2025-06-18","capabilities":{},"clientInfo":{"name":"t","version":"v1"}}}`
	req, err := http.NewRequestWithContext(context.Background(), http.MethodPost, srv.URL, strings.NewReader(body))
	if err != nil {
		t.Fatalf("new request: %v", err)
	}
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Accept", "application/json, text/event-stream")
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatalf("do: %v", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode == http.StatusUnauthorized || resp.StatusCode == http.StatusForbidden {
		t.Errorf("tokenless request rejected with %d; the surface must be unauthenticated", resp.StatusCode)
	}
	// A well-formed initialize on the unauthenticated loopback surface succeeds:
	// the SDK answers the handshake with 200 OK (it is not gated on a token).
	if resp.StatusCode != http.StatusOK {
		t.Errorf("tokenless initialize status = %d, want 200", resp.StatusCode)
	}
}

// minimalDeps builds a Deps with just enough wired to construct the handler.
func minimalDeps() Deps {
	return Deps{
		Snapshot: fakeSnapshot,
		Gatherer: emptyGatherer{},
		Profiler: fakeProfiler{},
	}
}

// --- role-label cardinality posture (issue #47) ---

// TestRoleFamiliesAreTheClosedTelemetrySet pins this adapter's role allowlist
// against the telemetry adapter's exported Role* constants: the role dimension
// is a CLOSED family, and the two packages must agree on its members. A new
// family value must be added in BOTH places (and in internal/app's roleFamily)
// deliberately — never discovered from data.
func TestRoleFamiliesAreTheClosedTelemetrySet(t *testing.T) {
	want := []string{
		telemetry.RoleMain,
		telemetry.RoleSubagent,
		telemetry.RoleMember,
		telemetry.RoleParallel,
		telemetry.RoleUserModel,
		telemetry.RoleChild,
	}
	if len(roleFamilies) != len(want) {
		t.Fatalf("roleFamilies has %d entries, want %d: %v vs %v", len(roleFamilies), len(want), roleFamilies, want)
	}
	for i, w := range want {
		if roleFamilies[i] != w {
			t.Errorf("roleFamilies[%d] = %q, want telemetry constant %q", i, roleFamilies[i], w)
		}
	}
}

// TestRoleFilterRejectsNonFamilyValues asserts the role filter accepts ONLY
// the closed family set — a session id, def name, or free-text value is
// rejected on both role-filtered tools, so the surface never legitimises an
// unbounded role vocabulary.
func TestRoleFilterRejectsNonFamilyValues(t *testing.T) {
	d := fullDeps()
	sess := dialTestServer(t, d)

	for _, bad := range []string{"sess-7f3a1b9c", "task:code-reviewer", "member:alice", "Main", "ALL"} {
		q := callTool(t, sess, "query_metric", QueryMetricInput{MetricName: "tool_duration_seconds", Role: bad})
		if !q.IsError {
			t.Errorf("query_metric accepted non-family role %q; the closed set must be enforced", bad)
		}
		l := callTool(t, sess, "list_slow_turns", ListSlowTurnsInput{Role: bad})
		if !l.IsError {
			t.Errorf("list_slow_turns accepted non-family role %q; the closed set must be enforced", bad)
		}
	}
	// Every closed-family value IS accepted (no false rejection).
	for _, good := range roleFamilies {
		l := callTool(t, sess, "list_slow_turns", ListSlowTurnsInput{Role: good})
		if l.IsError && strings.Contains(resultText(l), "unknown role") {
			t.Errorf("list_slow_turns rejected closed-family role %q: %s", good, resultText(l))
		}
	}
}
