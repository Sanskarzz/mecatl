package mcpperf

import (
	"context"
	"encoding/json"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/google/pprof/profile"
	mcpsdk "github.com/modelcontextprotocol/go-sdk/mcp"
	"github.com/prometheus/client_golang/prometheus"
	dto "github.com/prometheus/client_model/go"
	"google.golang.org/protobuf/proto"

	"github.com/stacklok/mecatl/internal/adapter/telemetry"
)

// --- shared test fakes ---

// fakeGatherer returns a hand-built set of metric families: one classic
// histogram (tool_duration) and one counter (events_total).
type fakeGatherer struct{}

func (fakeGatherer) Gather() ([]*dto.MetricFamily, error) {
	htype := dto.MetricType_HISTOGRAM
	ctype := dto.MetricType_COUNTER
	hist := &dto.MetricFamily{
		Name: proto.String("mecatl_tool_duration_seconds"),
		Type: &htype,
		Metric: []*dto.Metric{{Histogram: &dto.Histogram{
			SampleCount: proto.Uint64(4),
			Bucket: []*dto.Bucket{
				{UpperBound: proto.Float64(0.1), CumulativeCount: proto.Uint64(2)},
				{UpperBound: proto.Float64(0.5), CumulativeCount: proto.Uint64(3)},
				{UpperBound: proto.Float64(1.0), CumulativeCount: proto.Uint64(4)},
			},
		}}},
	}
	counter := &dto.MetricFamily{
		Name: proto.String("mecatl_events_total"),
		Type: &ctype,
		Metric: []*dto.Metric{
			{Counter: &dto.Counter{Value: proto.Float64(7)}},
			{Counter: &dto.Counter{Value: proto.Float64(3)}},
		},
	}
	return []*dto.MetricFamily{hist, counter}, nil
}

// emptyGatherer returns no families (used by posture tests where metric content
// does not matter).
type emptyGatherer struct{}

func (emptyGatherer) Gather() ([]*dto.MetricFamily, error) { return nil, nil }

var _ prometheus.Gatherer = fakeGatherer{}
var _ prometheus.Gatherer = emptyGatherer{}

// fakeRecorder is an armed flight recorder returning canned bytes.
type fakeRecorder struct {
	enabled bool
	bytes   []byte
}

func (f fakeRecorder) SnapshotBytes() ([]byte, error) { return f.bytes, nil }
func (f fakeRecorder) Enabled() bool                  { return f.enabled }

// fakeSlowTurns returns canned scalar-only slow turns.
type fakeSlowTurns struct{ turns []SlowTurn }

func (f fakeSlowTurns) Recent(_ int64) []SlowTurn { return f.turns }

// fakeSnapshot is a deterministic runtime snapshot for the resource round-trip.
func fakeSnapshot() telemetry.RuntimeSnapshot {
	return telemetry.RuntimeSnapshot{
		Goroutines:       42,
		NumCPU:           8,
		GOMAXPROCS:       8,
		HeapAllocBytes:   1 << 20,
		HeapObjects:      1234,
		TotalMemoryBytes: 4 << 20,
		HeapObjectBytes:  2 << 20,
		RSSBytes:         16 << 20,
		UptimeSeconds:    3.5,
		Available:        []string{"/sched/goroutines:goroutines"},
	}
}

// fullDeps wires every seam with fakes, including a CPU profile and named heap
// profile so the profiling tools succeed deterministically.
func fullDeps() Deps {
	heap := buildProfile("inuse_space", []funcSpec{
		{name: "allocHot", file: "/abs/secret/heap.go", value: 8192, label: "PROMPT-LEAK"},
		{name: "allocCold", file: "/abs/secret/cold.go", value: 256},
	})
	cpu := buildProfile("cpu", []funcSpec{
		{name: "cpuHot", file: "/abs/secret/cpu.go", value: 500, label: "PROMPT-LEAK"},
	})
	return Deps{
		Snapshot: fakeSnapshot,
		Gatherer: fakeGatherer{},
		Recorder: fakeRecorder{enabled: true, bytes: []byte("go 1.26 trace bytes")},
		Profiler: fakeProfiler{named: map[string]*profile.Profile{"heap": heap, "allocs": heap}, cpu: cpu},
		SlowTurns: fakeSlowTurns{turns: []SlowTurn{
			{TurnIndex: 1, DurationMs: 1200, TTFTMs: 300, InterTokenMaxMs: 90, EndedAt: time.Unix(100, 0).UTC()},
			{TurnIndex: 2, DurationMs: 900, TTFTMs: 250, InterTokenMaxMs: 70, EndedAt: time.Unix(200, 0).UTC()},
		}},
		Clock: func() time.Time { return time.Unix(5000, 0) },
	}
}

// dialTestServer stands up the perf handler over httptest and connects an SDK
// client via the Streamable HTTP transport (mirrors internal/adapter/mcp).
func dialTestServer(t *testing.T, d Deps) *mcpsdk.ClientSession {
	t.Helper()
	httpSrv := httptest.NewServer(Handler(d))
	t.Cleanup(httpSrv.Close)

	client := mcpsdk.NewClient(&mcpsdk.Implementation{Name: "test-client", Version: "v1"}, nil)
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	t.Cleanup(cancel)
	sess, err := client.Connect(ctx, &mcpsdk.StreamableClientTransport{
		Endpoint:             httpSrv.URL,
		DisableStandaloneSSE: true,
	}, nil)
	if err != nil {
		t.Fatalf("connect: %v", err)
	}
	t.Cleanup(func() { _ = sess.Close() })
	return sess
}

func callTool(t *testing.T, sess *mcpsdk.ClientSession, name string, args any) *mcpsdk.CallToolResult {
	t.Helper()
	raw, err := json.Marshal(args)
	if err != nil {
		t.Fatalf("marshal args: %v", err)
	}
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	res, err := sess.CallTool(ctx, &mcpsdk.CallToolParams{Name: name, Arguments: json.RawMessage(raw)})
	if err != nil {
		t.Fatalf("CallTool(%s): %v", name, err)
	}
	return res
}

func resultText(res *mcpsdk.CallToolResult) string {
	var sb strings.Builder
	for _, c := range res.Content {
		if tc, ok := c.(*mcpsdk.TextContent); ok {
			sb.WriteString(tc.Text)
		}
	}
	return sb.String()
}

// --- lifecycle ---

func TestLifecycleToolsList(t *testing.T) {
	sess := dialTestServer(t, fullDeps())

	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()

	got := map[string]*mcpsdk.Tool{}
	for tl, err := range sess.Tools(ctx, nil) {
		if err != nil {
			t.Fatalf("Tools: %v", err)
		}
		got[tl.Name] = tl
	}
	want := []string{
		"query_metric", "top_cpu_functions", "capture_cpu_profile",
		"top_allocations", "list_slow_turns", "capture_flight_recorder",
	}
	for _, name := range want {
		tl, ok := got[name]
		if !ok {
			t.Errorf("missing tool %q", name)
			continue
		}
		ann := tl.Annotations
		if ann == nil {
			t.Errorf("%s: nil annotations", name)
			continue
		}
		if !ann.ReadOnlyHint {
			t.Errorf("%s: readOnlyHint = false, want true", name)
		}
		if ann.DestructiveHint == nil || *ann.DestructiveHint {
			t.Errorf("%s: destructiveHint not false", name)
		}
		if !ann.IdempotentHint {
			t.Errorf("%s: idempotentHint = false, want true", name)
		}
		if ann.OpenWorldHint == nil || *ann.OpenWorldHint {
			t.Errorf("%s: openWorldHint not false", name)
		}
	}
}

func TestLifecycleResourcesListAndRead(t *testing.T) {
	sess := dialTestServer(t, fullDeps())
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()

	gotURIs := map[string]bool{}
	for r, err := range sess.Resources(ctx, nil) {
		if err != nil {
			t.Fatalf("Resources: %v", err)
		}
		gotURIs[r.URI] = true
	}
	for _, want := range []string{uriRuntimeSummary, uriRuntimeMemstats, uriMetricsSummary} {
		if !gotURIs[want] {
			t.Errorf("missing resource %q", want)
		}
	}

	// Read perf://runtime/summary and assert the fake snapshot round-trips.
	read, err := sess.ReadResource(ctx, &mcpsdk.ReadResourceParams{URI: uriRuntimeSummary})
	if err != nil {
		t.Fatalf("ReadResource: %v", err)
	}
	if len(read.Contents) != 1 {
		t.Fatalf("expected 1 content, got %d", len(read.Contents))
	}
	var snap telemetry.RuntimeSnapshot
	if err := json.Unmarshal([]byte(read.Contents[0].Text), &snap); err != nil {
		t.Fatalf("unmarshal snapshot: %v (%s)", err, read.Contents[0].Text)
	}
	if snap.Goroutines != 42 || snap.NumCPU != 8 {
		t.Errorf("snapshot round-trip wrong: %+v", snap)
	}
}

func TestLifecycleMetricsSummaryResource(t *testing.T) {
	sess := dialTestServer(t, fullDeps())
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()

	read, err := sess.ReadResource(ctx, &mcpsdk.ReadResourceParams{URI: uriMetricsSummary})
	if err != nil {
		t.Fatalf("ReadResource(metrics summary): %v", err)
	}
	if len(read.Contents) != 1 {
		t.Fatalf("expected 1 content, got %d", len(read.Contents))
	}
	var entries []MetricSummaryEntry
	if err := json.Unmarshal([]byte(read.Contents[0].Text), &entries); err != nil {
		t.Fatalf("unmarshal metrics summary: %v (%s)", err, read.Contents[0].Text)
	}
	byName := map[string]MetricSummaryEntry{}
	for _, e := range entries {
		byName[e.Name] = e
	}

	// Histogram family (fakeGatherer's mecatl_tool_duration_seconds): count + the
	// p50/p90/p99 bucket upper bounds. Buckets {0.1:2,0.5:3,1.0:4}, count 4 →
	// p50→0.1, p90→1.0, p99→1.0.
	hist, ok := byName["tool_duration_seconds"]
	if !ok || hist.Kind != "histogram" || hist.Count != 4 {
		t.Fatalf("tool_duration_seconds entry wrong: %+v (present=%v)", hist, ok)
	}
	wantBounds := map[float64]float64{0.5: 0.1, 0.9: 1.0, 0.99: 1.0}
	gotBounds := map[float64]float64{}
	for _, q := range hist.Quantiles {
		gotBounds[q.Quantile] = q.UpperBound
	}
	for q, w := range wantBounds {
		if g, ok := gotBounds[q]; !ok || g != w {
			t.Errorf("summary q%.2f upper bound = %v (present=%v), want %v", q, g, ok, w)
		}
	}

	// Counter family (events_total): scalarValue SUMS the per-label series (7+3=10).
	ev, ok := byName["events_total"]
	if !ok || ev.Kind != "scalar" || ev.Value != 10 {
		t.Errorf("events_total entry wrong: %+v (present=%v), want scalar value 10 (summed)", ev, ok)
	}

	// A curated family with no metric present is reported with Kind "absent".
	missing, ok := byName["active_runs"]
	if !ok || missing.Kind != "absent" {
		t.Errorf("active_runs entry wrong: %+v (present=%v), want Kind absent", missing, ok)
	}
}

func TestLifecycleMemstatsResource(t *testing.T) {
	sess := dialTestServer(t, fullDeps())
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()

	read, err := sess.ReadResource(ctx, &mcpsdk.ReadResourceParams{URI: uriRuntimeMemstats})
	if err != nil {
		t.Fatalf("ReadResource(memstats): %v", err)
	}
	if len(read.Contents) != 1 {
		t.Fatalf("expected 1 content, got %d", len(read.Contents))
	}
	var proj MemstatsProjection
	if err := json.Unmarshal([]byte(read.Contents[0].Text), &proj); err != nil {
		t.Fatalf("unmarshal memstats projection: %v (%s)", err, read.Contents[0].Text)
	}
	// MemstatsProjection shape, projected from fakeSnapshot.
	if proj.HeapAllocBytes != 1<<20 || proj.HeapObjects != 1234 ||
		proj.HeapObjectBytes != 2<<20 || proj.TotalMemoryBytes != 4<<20 || proj.RSSBytes != 16<<20 {
		t.Errorf("memstats projection wrong: %+v", proj)
	}
	if len(proj.Available) != 1 || proj.Available[0] != "/sched/goroutines:goroutines" {
		t.Errorf("memstats Available wrong: %+v", proj.Available)
	}
}

func TestLifecyclePprofResourceTemplate(t *testing.T) {
	sess := dialTestServer(t, fullDeps())
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()

	// Known profile → reduced top-N, redacted.
	read, err := sess.ReadResource(ctx, &mcpsdk.ReadResourceParams{URI: "perf://pprof/heap"})
	if err != nil {
		t.Fatalf("ReadResource(heap): %v", err)
	}
	body := read.Contents[0].Text
	if !strings.Contains(body, "allocHot") {
		t.Errorf("pprof summary missing allocHot: %s", body)
	}
	for _, banned := range []string{"/abs", "secret", "PROMPT-LEAK"} {
		if strings.Contains(body, banned) {
			t.Errorf("pprof resource leaked %q: %s", banned, body)
		}
	}

	// Unknown profile → resource-not-found protocol error.
	if _, err := sess.ReadResource(ctx, &mcpsdk.ReadResourceParams{URI: "perf://pprof/bogus"}); err == nil {
		t.Errorf("expected error for unknown profile, got nil")
	}
}

func TestLifecycleQueryMetric(t *testing.T) {
	sess := dialTestServer(t, fullDeps())

	// Discovery: no metric_name → list of names.
	disc := callTool(t, sess, "query_metric", QueryMetricInput{})
	if disc.IsError {
		t.Fatalf("discovery returned isError: %s", resultText(disc))
	}
	var discOut QueryMetricOutput
	decodeStructured(t, disc, &discOut)
	if len(discOut.AvailableMetrics) == 0 {
		t.Errorf("discovery returned no metric names")
	}

	// Histogram metric → count + quantile bounds.
	hist := callTool(t, sess, "query_metric", QueryMetricInput{MetricName: "tool_duration_seconds"})
	if hist.IsError {
		t.Fatalf("histogram query isError: %s", resultText(hist))
	}
	var histOut QueryMetricOutput
	decodeStructured(t, hist, &histOut)
	if histOut.Metric == nil || histOut.Metric.Kind != "histogram" || histOut.Metric.Count != 4 {
		t.Errorf("unexpected histogram metric: %+v", histOut.Metric)
	}
	// Assert the quantile UPPER-BOUND VALUES through the wire, not just the count.
	// fakeGatherer's buckets are {0.1:2, 0.5:3, 1.0:4}, count 4: p50→target ceil(0.5*4)=2
	// → first cum≥2 is bound 0.1; p90→ceil(0.9*4)=4 → 1.0; p99→ceil(0.99*4)=4 → 1.0.
	wantBounds := map[float64]float64{0.5: 0.1, 0.9: 1.0, 0.99: 1.0}
	if histOut.Metric != nil {
		gotBounds := map[float64]float64{}
		for _, q := range histOut.Metric.Quantiles {
			gotBounds[q.Quantile] = q.UpperBound
		}
		for q, want := range wantBounds {
			if got, ok := gotBounds[q]; !ok || got != want {
				t.Errorf("q%.2f upper bound = %v (present=%v), want %v", q, got, ok, want)
			}
		}
	}

	// Unknown metric → isError, NOT a protocol error.
	bad := callTool(t, sess, "query_metric", QueryMetricInput{MetricName: "nope"})
	if !bad.IsError {
		t.Errorf("unknown metric should be isError")
	}
	if !strings.Contains(resultText(bad), "no metric_name") {
		t.Errorf("unknown metric recovery text missing: %s", resultText(bad))
	}
}

func TestLifecycleTopAllocations(t *testing.T) {
	sess := dialTestServer(t, fullDeps())
	res := callTool(t, sess, "top_allocations", TopAllocationsInput{Limit: 5})
	if res.IsError {
		t.Fatalf("top_allocations isError: %s", resultText(res))
	}
	var out TopAllocationsOutput
	decodeStructured(t, res, &out)
	if out.TotalHeapBytes != 8192+256 {
		t.Errorf("total heap bytes = %d, want %d", out.TotalHeapBytes, 8192+256)
	}
	if len(out.Top) == 0 || out.Top[0].Function != "allocHot" {
		t.Errorf("unexpected top: %+v", out.Top)
	}
	// Redaction over the wire.
	body := resultText(res)
	for _, banned := range []string{"/abs", "secret", "PROMPT-LEAK"} {
		if strings.Contains(body, banned) {
			t.Errorf("top_allocations leaked %q: %s", banned, body)
		}
	}
}

func TestLifecycleTopCPUFunctions(t *testing.T) {
	sess := dialTestServer(t, fullDeps())
	res := callTool(t, sess, "top_cpu_functions", TopCPUFunctionsInput{DurationSeconds: 1, Limit: 5})
	if res.IsError {
		t.Fatalf("top_cpu_functions isError: %s", resultText(res))
	}
	var out TopCPUFunctionsOutput
	decodeStructured(t, res, &out)
	if out.DurationSeconds != 1 {
		t.Errorf("duration = %d, want 1 (clamped)", out.DurationSeconds)
	}
	if len(out.Top) == 0 || out.Top[0].Function != "cpuHot" {
		t.Errorf("unexpected cpu top: %+v", out.Top)
	}
	// Redaction over the wire: the fake CPU profile carries an absolute path
	// (/abs/secret/cpu.go) and a sample label ("PROMPT-LEAK"); neither may appear
	// in the CPU tool output. Same banned-substring scan top_allocations uses.
	body := resultText(res)
	for _, banned := range []string{"/abs", "secret", "PROMPT-LEAK"} {
		if strings.Contains(body, banned) {
			t.Errorf("top_cpu_functions leaked %q: %s", banned, body)
		}
	}
}

// TestCPURateLimitSpansBothCPUToolsOverWire proves the shared cpuGate is enforced
// THROUGH the SDK (not just the unit gate): with a fixed Clock, a second CPU
// capture within the cooldown is refused, and the gate is shared across BOTH CPU
// tools — capture_cpu_profile is gated by a prior top_cpu_functions capture. This
// fails if the gate were removed from either handler.
func TestCPURateLimitSpansBothCPUToolsOverWire(t *testing.T) {
	sess := dialTestServer(t, fullDeps()) // fixed Clock at Unix(5000): no cooldown elapses between calls.

	// First CPU capture succeeds.
	first := callTool(t, sess, "top_cpu_functions", TopCPUFunctionsInput{DurationSeconds: 1})
	if first.IsError {
		t.Fatalf("first top_cpu_functions should succeed: %s", resultText(first))
	}

	// Second top_cpu_functions within the cooldown is rate-limited.
	second := callTool(t, sess, "top_cpu_functions", TopCPUFunctionsInput{DurationSeconds: 1})
	if !second.IsError {
		t.Errorf("second top_cpu_functions within cooldown should be isError (rate limited)")
	}
	if !strings.Contains(resultText(second), "rate limit") || !strings.Contains(resultText(second), "retry") {
		t.Errorf("rate-limit recovery text missing: %s", resultText(second))
	}

	// capture_cpu_profile shares the SAME gate, so it is ALSO refused — the two CPU
	// tools cannot be alternated to bypass the limit.
	cross := callTool(t, sess, "capture_cpu_profile", CaptureCPUProfileInput{DurationSeconds: 1})
	if !cross.IsError {
		t.Errorf("capture_cpu_profile should be rate-limited by the shared gate after a top_cpu_functions capture")
	}
	if !strings.Contains(resultText(cross), "rate limit") {
		t.Errorf("shared-gate rate-limit text missing on capture_cpu_profile: %s", resultText(cross))
	}
}

func TestLifecycleCaptureCPUProfileRawLink(t *testing.T) {
	sess := dialTestServer(t, fullDeps())
	res := callTool(t, sess, "capture_cpu_profile", CaptureCPUProfileInput{DurationSeconds: 1, IncludeRawLink: true})
	if res.IsError {
		t.Fatalf("capture_cpu_profile isError: %s", resultText(res))
	}
	// Assert a user-audience resource_link to the loopback pprof endpoint.
	var link *mcpsdk.ResourceLink
	for _, c := range res.Content {
		if rl, ok := c.(*mcpsdk.ResourceLink); ok {
			link = rl
		}
	}
	if link == nil {
		t.Fatalf("expected a resource_link content item")
	}
	if link.URI != pprofProfilePath {
		t.Errorf("link URI = %q, want %q", link.URI, pprofProfilePath)
	}
	if link.Annotations == nil || len(link.Annotations.Audience) != 1 || link.Annotations.Audience[0] != "user" {
		t.Errorf("link audience not [user]: %+v", link.Annotations)
	}
	// Redaction over the wire on the SECOND CPU tool too (same fake CPU profile).
	body := resultText(res)
	for _, banned := range []string{"/abs", "secret", "PROMPT-LEAK"} {
		if strings.Contains(body, banned) {
			t.Errorf("capture_cpu_profile leaked %q: %s", banned, body)
		}
	}
}

// TestCaptureCPUProfileClampsDuration covers clampCPUSeconds wired into the SECOND
// CPU tool end-to-end: a request of 999s must clamp to the 30s server cap in the
// returned DurationSeconds (today only top_cpu_functions asserts the clamp).
func TestCaptureCPUProfileClampsDuration(t *testing.T) {
	sess := dialTestServer(t, fullDeps())
	res := callTool(t, sess, "capture_cpu_profile", CaptureCPUProfileInput{DurationSeconds: 999})
	if res.IsError {
		t.Fatalf("capture_cpu_profile isError: %s", resultText(res))
	}
	var out TopCPUFunctionsOutput
	decodeStructured(t, res, &out)
	if out.DurationSeconds != maxCPUProfileSeconds {
		t.Errorf("duration = %d, want %d (clamped from 999)", out.DurationSeconds, maxCPUProfileSeconds)
	}
}

func TestLifecycleListSlowTurns(t *testing.T) {
	sess := dialTestServer(t, fullDeps())
	res := callTool(t, sess, "list_slow_turns", ListSlowTurnsInput{Limit: 1})
	if res.IsError {
		t.Fatalf("list_slow_turns isError: %s", resultText(res))
	}
	var out ListSlowTurnsOutput
	decodeStructured(t, res, &out)
	if out.TotalCount != 2 {
		t.Errorf("totalCount = %d, want 2", out.TotalCount)
	}
	if len(out.Turns) != 1 {
		t.Fatalf("page size = %d, want 1", len(out.Turns))
	}
	if out.NextCursor == "" {
		t.Errorf("expected a nextCursor with more results pending")
	}
	// Second page via the cursor.
	res2 := callTool(t, sess, "list_slow_turns", ListSlowTurnsInput{Limit: 1, Cursor: out.NextCursor})
	var out2 ListSlowTurnsOutput
	decodeStructured(t, res2, &out2)
	if len(out2.Turns) != 1 || out2.Turns[0].TurnIndex != 2 {
		t.Errorf("second page wrong: %+v", out2.Turns)
	}
}

func TestLifecycleListSlowTurnsDisabled(t *testing.T) {
	d := fullDeps()
	d.SlowTurns = nil
	sess := dialTestServer(t, d)
	res := callTool(t, sess, "list_slow_turns", ListSlowTurnsInput{})
	if !res.IsError {
		t.Fatalf("expected isError when SlowTurns is nil")
	}
	if !strings.Contains(resultText(res), "not enabled") {
		t.Errorf("recovery text missing: %s", resultText(res))
	}
}

func TestLifecycleCaptureFlightRecorder(t *testing.T) {
	sess := dialTestServer(t, fullDeps())
	res := callTool(t, sess, "capture_flight_recorder", CaptureFlightRecorderInput{})
	if res.IsError {
		t.Fatalf("capture_flight_recorder isError: %s", resultText(res))
	}
	var out CaptureFlightRecorderOutput
	decodeStructured(t, res, &out)
	if out.CapturedBytes != len("go 1.26 trace bytes") {
		t.Errorf("captured bytes = %d", out.CapturedBytes)
	}
	var link *mcpsdk.ResourceLink
	for _, c := range res.Content {
		if rl, ok := c.(*mcpsdk.ResourceLink); ok {
			link = rl
		}
	}
	if link == nil || link.URI != flightRecorderURI {
		t.Fatalf("expected flight-recorder resource_link, got %+v", link)
	}
	if link.Annotations == nil || len(link.Annotations.Audience) != 1 || link.Annotations.Audience[0] != "user" {
		t.Errorf("flight recorder link audience not [user]: %+v", link.Annotations)
	}
}

func TestLifecycleCaptureFlightRecorderNotArmed(t *testing.T) {
	d := fullDeps()
	d.Recorder = fakeRecorder{enabled: false}
	sess := dialTestServer(t, d)
	res := callTool(t, sess, "capture_flight_recorder", CaptureFlightRecorderInput{})
	if !res.IsError {
		t.Fatalf("expected isError when recorder not armed")
	}
	if !strings.Contains(resultText(res), "not armed") {
		t.Errorf("recovery text missing: %s", resultText(res))
	}
}

// decodeStructured unmarshals a tool result's structuredContent into v by
// round-tripping through JSON.
func decodeStructured(t *testing.T, res *mcpsdk.CallToolResult, v any) {
	t.Helper()
	b, err := json.Marshal(res.StructuredContent)
	if err != nil {
		t.Fatalf("marshal structuredContent: %v", err)
	}
	if err := json.Unmarshal(b, v); err != nil {
		t.Fatalf("unmarshal structuredContent into %T: %v (%s)", v, err, b)
	}
}

// TestNewServerPanicsOnMissingRequiredDep asserts NewServer fails loud (panics
// naming the missing dep) when a REQUIRED dependency is nil, rather than deferring
// the failure to a nil-panic deep inside a handler. Recorder/SlowTurns stay
// nil-able and are NOT required.
func TestNewServerPanicsOnMissingRequiredDep(t *testing.T) {
	cases := map[string]func(*Deps){
		"Snapshot": func(d *Deps) { d.Snapshot = nil },
		"Gatherer": func(d *Deps) { d.Gatherer = nil },
		"Profiler": func(d *Deps) { d.Profiler = nil },
	}
	for name, mutate := range cases {
		t.Run(name, func(t *testing.T) {
			d := fullDeps()
			mutate(&d)
			defer func() {
				r := recover()
				if r == nil {
					t.Fatalf("NewServer with nil %s should panic", name)
				}
				msg, _ := r.(string)
				if !strings.Contains(msg, name) {
					t.Errorf("panic message %q should name the missing dep %q", msg, name)
				}
			}()
			NewServer(d)
		})
	}

	// All required deps present → no panic.
	NewServer(minimalDeps())
}

func TestServerIdentityAndInstructions(t *testing.T) {
	sess := dialTestServer(t, fullDeps())
	init := sess.InitializeResult()
	if init == nil {
		t.Fatal("nil initialize result")
	}
	if init.ServerInfo == nil || init.ServerInfo.Name != serverName {
		t.Errorf("server name = %+v, want %q", init.ServerInfo, serverName)
	}
	if !strings.Contains(init.Instructions, "reduced numeric summaries") {
		t.Errorf("instructions missing the reduced-summary statement: %q", init.Instructions)
	}
}
