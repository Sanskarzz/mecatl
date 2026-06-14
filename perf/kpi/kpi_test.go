package kpi

import (
	"encoding/json"
	"os"
	"path/filepath"
	"runtime"
	"testing"
	"time"
)

// TestWriteJSONRoundTrips writes a couple of results and reads them back,
// asserting the schema tags and values survive. It also covers the nil-slice
// path (a valid empty array, never a nil/"null").
func TestWriteJSONRoundTrips(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "out.json")

	in := []ScenarioResult{
		{SchemaVersion: SchemaVersion, Name: "alpha", Sample: 0, GitSHA: "abc123", Iterations: 3, AllocsPerOp: 100, BytesPerOp: 2048, CacheHitRate: 0.83},
		{SchemaVersion: SchemaVersion, Name: "alpha", Sample: 1, GitSHA: "abc123", GoroutinesEnd: 7, TokensInput: 1200, TokensCacheRead: 1000},
	}
	if err := WriteJSON(path, in); err != nil {
		t.Fatalf("WriteJSON: %v", err)
	}

	b, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("read back: %v", err)
	}
	var out []ScenarioResult
	if err := json.Unmarshal(b, &out); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}
	if len(out) != 2 || out[0].Name != "alpha" || out[1].TokensCacheRead != 1000 {
		t.Fatalf("round-trip mismatch: %+v", out)
	}
	// The sample ordinal + git_sha (the (name, git_sha) grouping key + discriminator)
	// must survive the round-trip — two same-named rows distinguished only by sample.
	if out[0].Sample != 0 || out[1].Sample != 1 || out[0].GitSHA != "abc123" {
		t.Fatalf("sample/git_sha discriminators lost: %+v", out)
	}

	// nil slice → empty array, not "null".
	nilPath := filepath.Join(dir, "empty.json")
	if err := WriteJSON(nilPath, nil); err != nil {
		t.Fatalf("WriteJSON nil: %v", err)
	}
	nb, _ := os.ReadFile(nilPath)
	var empty []ScenarioResult
	if err := json.Unmarshal(nb, &empty); err != nil {
		t.Fatalf("unmarshal empty: %v", err)
	}
	if len(empty) != 0 {
		t.Fatalf("expected empty array, got %d", len(empty))
	}
}

// TestGoroutineDeltaClampsAtZero asserts the leak-delta never goes negative (a
// baseline that caught a since-exited transient must report 0, not a spurious
// negative) and reports a positive count when the end exceeds the baseline.
func TestGoroutineDeltaClampsAtZero(t *testing.T) {
	// A baseline far above any plausible end count → clamped at 0.
	if d := GoroutineDelta(1_000_000, time.Millisecond); d != 0 {
		t.Errorf("expected clamp at 0 for an over-high baseline, got %d", d)
	}
	// A baseline of 0 → the delta is the (positive) live count.
	if d := GoroutineDelta(0, time.Millisecond); d <= 0 {
		t.Errorf("expected a positive delta against a zero baseline, got %d", d)
	}
}

// TestCaptureCountsAllocations brackets a region that allocates a known shape and
// asserts the capture reports a non-zero allocation delta and a sane wall-clock.
// It is deliberately tiny — it must not do heavy work under `task test`.
func TestCaptureCountsAllocations(t *testing.T) {
	c := NewCapture()
	c.Begin()
	sink := make([][]byte, 0, 64)
	for i := 0; i < 64; i++ {
		sink = append(sink, make([]byte, 256))
	}
	runtime.KeepAlive(sink)
	m := c.End()

	if m.Allocs == 0 {
		t.Error("expected a non-zero allocation count for 64 slice allocations")
	}
	if m.Bytes == 0 {
		t.Error("expected a non-zero byte count")
	}
	if m.WallNs < 0 {
		t.Errorf("wall-clock must be non-negative, got %d", m.WallNs)
	}
}

// TestRSSSamplerNonNegative checks the sampler runs and returns non-negative
// peak/final (0 off-linux). It does not assert a positive RSS because that is
// platform-dependent.
func TestRSSSamplerNonNegative(t *testing.T) {
	s := NewRSSSampler(time.Millisecond)
	s.Start()
	time.Sleep(5 * time.Millisecond)
	peak, final := s.Stop()
	if peak < final {
		t.Errorf("peak (%d) must be >= final (%d)", peak, final)
	}
}

// TestGoroutinesAfterSettle returns a positive count (at least this test's own
// goroutine) and does not hang.
func TestGoroutinesAfterSettle(t *testing.T) {
	if n := GoroutinesAfterSettle(2 * time.Millisecond); n < 1 {
		t.Errorf("expected at least one live goroutine, got %d", n)
	}
}
