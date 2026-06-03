package telemetry

import (
	"bytes"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"runtime"
	"runtime/trace"
	"slices"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/prometheus/client_golang/prometheus"
	otelprom "go.opentelemetry.io/otel/exporters/prometheus"
	"go.opentelemetry.io/otel/sdk/metric"
)

// --- pprof ---

func TestRegisterPprofNoPanicAndServes(t *testing.T) {
	mux := http.NewServeMux()
	RegisterPprof(mux) // must not panic on a fresh mux

	srv := httptest.NewServer(mux)
	defer srv.Close()

	// A named profile in text mode returns a goroutine dump.
	resp, err := http.Get(srv.URL + "/debug/pprof/goroutine?debug=1")
	if err != nil {
		t.Fatalf("GET goroutine: %v", err)
	}
	defer func() { _ = resp.Body.Close() }()
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("goroutine status = %d, want 200", resp.StatusCode)
	}
	body := readAll(t, resp)
	if !strings.Contains(body, "goroutine profile") && !strings.Contains(body, "goroutine ") {
		t.Fatalf("goroutine dump missing expected marker; got %.120q", body)
	}

	// The index lists the registered profiles.
	idxResp, err := http.Get(srv.URL + "/debug/pprof/")
	if err != nil {
		t.Fatalf("GET index: %v", err)
	}
	defer func() { _ = idxResp.Body.Close() }()
	if idxResp.StatusCode != http.StatusOK {
		t.Fatalf("index status = %d, want 200", idxResp.StatusCode)
	}
	idx := readAll(t, idxResp)
	for _, want := range []string{"heap", "goroutine", "allocs"} {
		if !strings.Contains(idx, want) {
			t.Errorf("index does not list %q profile; got %.200q", want, idx)
		}
	}
}

// --- Snapshot / expvar ---

func TestSnapshotPositiveGoroutinesAndRoundTrips(t *testing.T) {
	snap := Snapshot()
	if snap.Goroutines <= 0 {
		t.Errorf("Goroutines = %d, want > 0", snap.Goroutines)
	}
	if snap.NumCPU <= 0 {
		t.Errorf("NumCPU = %d, want > 0", snap.NumCPU)
	}
	if snap.GOMAXPROCS <= 0 {
		t.Errorf("GOMAXPROCS = %d, want > 0", snap.GOMAXPROCS)
	}
	if snap.UptimeSeconds < 0 {
		t.Errorf("UptimeSeconds = %f, want >= 0", snap.UptimeSeconds)
	}
	// The curated runtime/metrics names should populate at least total memory.
	if snap.TotalMemoryBytes == 0 {
		t.Errorf("TotalMemoryBytes = 0, expected the runtime/metrics sample to be present")
	}
	// Available makes presence explicit: the total-memory sample must be listed
	// (it was just read into TotalMemoryBytes), so absence carries the semantic
	// load rather than a zero value.
	if !slices.Contains(snap.Available, metricTotalBytes) {
		t.Errorf("Available = %v, want it to contain %q", snap.Available, metricTotalBytes)
	}

	b, err := json.Marshal(snap)
	if err != nil {
		t.Fatalf("json.Marshal(snapshot): %v", err)
	}
	var back RuntimeSnapshot
	if err := json.Unmarshal(b, &back); err != nil {
		t.Fatalf("json.Unmarshal round-trip: %v", err)
	}
	if back.Goroutines != snap.Goroutines {
		t.Errorf("round-trip Goroutines = %d, want %d", back.Goroutines, snap.Goroutines)
	}
}

// TestReadMetricsToleratesBogusName asserts the version-drift guard FORWARDS: a
// name absent from metrics.All (a removed/renamed metric on a future toolchain)
// is silently dropped without panicking, only the known sample is returned, and
// the bogus name never reaches Available.
func TestReadMetricsToleratesBogusName(t *testing.T) {
	const bogus = "/this/metric/never/existed:bytes"
	samples := readMetrics([]string{bogus, metricTotalBytes})

	if len(samples) != 1 {
		t.Fatalf("readMetrics returned %d samples, want 1 (only the known name); got %+v", len(samples), samples)
	}
	if samples[0].Name != metricTotalBytes {
		t.Errorf("sample name = %q, want %q", samples[0].Name, metricTotalBytes)
	}

	// Snapshot must likewise tolerate the toolchain set and never list a name it
	// did not actually read.
	snap := Snapshot()
	if slices.Contains(snap.Available, bogus) {
		t.Errorf("Available = %v, must not contain the bogus name %q", snap.Available, bogus)
	}
}

func TestExpvarHandlerServesCuratedKeys(t *testing.T) {
	srv := httptest.NewServer(ExpvarHandler())
	defer srv.Close()

	resp, err := http.Get(srv.URL + "/debug/vars")
	if err != nil {
		t.Fatalf("GET /debug/vars: %v", err)
	}
	defer func() { _ = resp.Body.Close() }()
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("status = %d, want 200", resp.StatusCode)
	}
	var vars map[string]json.RawMessage
	if err := json.NewDecoder(resp.Body).Decode(&vars); err != nil {
		t.Fatalf("decode /debug/vars JSON: %v", err)
	}
	raw, ok := vars["mecatl_runtime"]
	if !ok {
		t.Fatalf("/debug/vars missing curated mecatl_runtime key; got keys %v", keysOf(vars))
	}
	var snap RuntimeSnapshot
	if err := json.Unmarshal(raw, &snap); err != nil {
		t.Fatalf("mecatl_runtime is not a RuntimeSnapshot: %v", err)
	}
	if snap.Goroutines <= 0 {
		t.Errorf("mecatl_runtime.goroutines = %d, want > 0", snap.Goroutines)
	}
}

func TestExpvarHandlerIdempotentPublish(_ *testing.T) {
	// Calling twice must not panic on a duplicate expvar.Publish.
	_ = ExpvarHandler()
	_ = ExpvarHandler()
}

// --- FlightRecorder ---

func TestFlightRecorderSnapshotHasTraceMagic(t *testing.T) {
	fr := NewFlightRecorder(trace.FlightRecorderConfig{})
	if err := fr.Start(); err != nil {
		t.Fatalf("Start: %v", err)
	}
	defer fr.Stop()

	if !fr.Enabled() {
		t.Fatal("Enabled() = false after Start")
	}

	// Generate some scheduler/GC activity so the window is non-trivial.
	generateActivity()

	b, err := fr.SnapshotBytes()
	if err != nil {
		t.Fatalf("SnapshotBytes: %v", err)
	}
	if len(b) == 0 {
		t.Fatal("SnapshotBytes returned an empty buffer")
	}
	// Go 1.26 execution traces begin with the "go 1.<ver> trace" magic header.
	// Require the exact "go 1." prefix rather than a loose contains("trace") —
	// the prefix is the actual trace-format magic and a loose match would pass on
	// almost any bytes.
	if !bytes.HasPrefix(b, []byte("go 1.")) {
		t.Errorf("snapshot missing the \"go 1.\" trace magic prefix; first bytes: %q", b[:min(32, len(b))])
	}
}

func TestFlightRecorderSnapshotBeforeStartErrors(t *testing.T) {
	fr := NewFlightRecorder(trace.FlightRecorderConfig{})
	if _, err := fr.SnapshotBytes(); err == nil {
		t.Fatal("SnapshotBytes before Start should error")
	}
}

func TestFlightRecorderStartStopIdempotent(t *testing.T) {
	fr := NewFlightRecorder(trace.FlightRecorderConfig{})
	if err := fr.Start(); err != nil {
		t.Fatalf("Start: %v", err)
	}
	if err := fr.Start(); err != nil { // second Start is a no-op
		t.Fatalf("second Start: %v", err)
	}
	fr.Stop()
	fr.Stop() // second Stop is a no-op
	if fr.Enabled() {
		t.Fatal("Enabled() = true after Stop")
	}
}

func TestFlightRecorderDefaultsApplied(t *testing.T) {
	fr := NewFlightRecorder(trace.FlightRecorderConfig{})
	// We cannot read the private config back, but Start/Stop must work with the
	// bounded defaults and the constants must be sane.
	if DefaultFlightRecorderMaxBytes == 0 || DefaultFlightRecorderMinAge == 0 {
		t.Fatal("default flight-recorder bounds must be non-zero")
	}
	if err := fr.Start(); err != nil {
		t.Fatalf("Start with defaults: %v", err)
	}
	fr.Stop()
}

// TestProcessFlightRecorderCoalescesSecondArming asserts the process-singleton
// invariant: the first ProcessFlightRecorder call arms and returns the shared
// instance with a nil error; a second call is COALESCED — it returns the SAME
// instance plus ErrFlightRecorderAlreadyActive, never a second silently-dropped
// recorder. This is the guard for decision 7's second consumer (the embedded
// server) against the one-recorder-per-process stdlib constraint.
//
// It runs in its own process: the package-level sync.Once means the singleton is
// armed for the rest of this test binary, so it is isolated behind a build that
// only this test touches the accessor. It stops the recorder at the end so the
// runtime trace subscription does not leak into other tests.
func TestProcessFlightRecorderCoalescesSecondArming(t *testing.T) {
	first, err := ProcessFlightRecorder(trace.FlightRecorderConfig{})
	if err != nil {
		t.Fatalf("first ProcessFlightRecorder: %v", err)
	}
	if first == nil {
		t.Fatal("first ProcessFlightRecorder returned a nil recorder")
	}
	if !first.Enabled() {
		t.Fatal("the shared recorder should be started after the first call")
	}
	t.Cleanup(first.Stop)

	second, err := ProcessFlightRecorder(trace.FlightRecorderConfig{})
	if !errors.Is(err, ErrFlightRecorderAlreadyActive) {
		t.Fatalf("second ProcessFlightRecorder err = %v, want ErrFlightRecorderAlreadyActive", err)
	}
	if second != first {
		t.Errorf("second call returned a different instance (%p) than the first (%p); arming was not coalesced", second, first)
	}
}

// --- /debug/flightrecorder endpoint shape (mirrors what serve mounts) ---

func TestFlightRecorderEndpointServesSnapshot(t *testing.T) {
	fr := NewFlightRecorder(trace.FlightRecorderConfig{})
	if err := fr.Start(); err != nil {
		t.Fatalf("Start: %v", err)
	}
	defer fr.Stop()
	generateActivity()

	mux := http.NewServeMux()
	mux.HandleFunc("/debug/flightrecorder", func(w http.ResponseWriter, _ *http.Request) {
		if _, err := fr.Snapshot(w); err != nil {
			http.Error(w, err.Error(), http.StatusServiceUnavailable)
		}
	})
	srv := httptest.NewServer(mux)
	defer srv.Close()

	resp, err := http.Get(srv.URL + "/debug/flightrecorder")
	if err != nil {
		t.Fatalf("GET /debug/flightrecorder: %v", err)
	}
	defer func() { _ = resp.Body.Close() }()
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("status = %d, want 200", resp.StatusCode)
	}
	body := readAllBytes(t, resp)
	if len(body) == 0 {
		t.Fatal("flightrecorder endpoint returned an empty body")
	}
}

// --- RSS gauge (Linux only) ---

func TestProcessRSSGaugeOnMetricsScrape(t *testing.T) {
	if !rssSupported() {
		t.Skip("RSS gauge not supported on this platform")
	}
	if v := readRSS(); v == 0 {
		t.Fatalf("readRSS() = 0, want a positive RSS on a supported platform")
	}

	reg := prometheus.NewRegistry()
	exp, err := otelprom.New(otelprom.WithRegisterer(reg))
	if err != nil {
		t.Fatalf("prometheus exporter: %v", err)
	}
	mp := metric.NewMeterProvider(metric.WithReader(exp))
	if rerr := RegisterProcessGauges(mp); rerr != nil {
		t.Fatalf("RegisterProcessGauges: %v", rerr)
	}

	srv := httptest.NewServer(MetricsHandler(reg))
	defer srv.Close()
	resp, err := http.Get(srv.URL + "/metrics")
	if err != nil {
		t.Fatalf("GET /metrics: %v", err)
	}
	defer func() { _ = resp.Body.Close() }()
	scrape := readAll(t, resp)
	// Assert the actual SERIES line carries a POSITIVE value, not merely that the
	// substring (which also matches the # HELP/# TYPE lines) appears. The
	// otelprom exporter renders the gauge as "mecatl_process_rss_bytes <value>"
	// (no labels on this instrument).
	val := scrapeGaugeValue(t, scrape, "mecatl_process_rss_bytes")
	if val <= 0 {
		t.Errorf("mecatl_process_rss_bytes = %v, want a positive value; scrape:\n%.400s", val, scrape)
	}
}

// scrapeGaugeValue extracts the float value of a Prometheus sample line for the
// given metric name from a /metrics scrape, accepting both the unlabelled
// "<name> <value>" form and the labelled "<name>{...} <value>" form the
// otelprom exporter emits (it adds otel_scope_* labels). It fails the test if no
// such SAMPLE line (as opposed to a "# HELP"/"# TYPE" comment) is present.
func scrapeGaugeValue(t *testing.T, scrape, name string) float64 {
	t.Helper()
	for _, line := range strings.Split(scrape, "\n") {
		if strings.HasPrefix(line, "#") {
			continue
		}
		rest, ok := strings.CutPrefix(line, name)
		if !ok || rest == "" {
			continue
		}
		// The char immediately after the name must be a space (unlabelled) or '{'
		// (labelled) — otherwise this is a different series sharing the prefix.
		switch rest[0] {
		case '{':
			if i := strings.IndexByte(rest, '}'); i >= 0 {
				rest = rest[i+1:]
			} else {
				continue
			}
		case ' ':
		default:
			continue
		}
		v, err := strconv.ParseFloat(strings.TrimSpace(rest), 64)
		if err != nil {
			t.Fatalf("parse %q value from %q: %v", name, line, err)
		}
		return v
	}
	t.Fatalf("no sample line for %q in scrape:\n%.400s", name, scrape)
	return 0
}

// --- helpers ---

func generateActivity() {
	var wg sync.WaitGroup
	for range 8 {
		wg.Add(1)
		go func() {
			defer wg.Done()
			s := 0
			for i := range 10000 {
				s += i
			}
			runtime.Gosched()
			_ = s
		}()
	}
	wg.Wait()
	runtime.GC()
	time.Sleep(10 * time.Millisecond)
}

func keysOf(m map[string]json.RawMessage) []string {
	out := make([]string, 0, len(m))
	for k := range m {
		out = append(out, k)
	}
	return out
}

func readAll(t *testing.T, resp *http.Response) string {
	t.Helper()
	return string(readAllBytes(t, resp))
}

func readAllBytes(t *testing.T, resp *http.Response) []byte {
	t.Helper()
	var buf bytes.Buffer
	if _, err := buf.ReadFrom(resp.Body); err != nil {
		t.Fatalf("read body: %v", err)
	}
	return buf.Bytes()
}
