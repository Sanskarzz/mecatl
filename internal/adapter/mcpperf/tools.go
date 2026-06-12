package mcpperf

import (
	"context"
	"encoding/json"
	"fmt"
	"strings"
	"time"

	mcpsdk "github.com/modelcontextprotocol/go-sdk/mcp"
)

// Admin endpoints for the user-audience raw-artifact links. These are the
// existing loopback perf-observability endpoints; this server adds no new
// artifact store, it just points the human at where the raw blob already lives.
const (
	pprofProfilePath  = "/debug/pprof/profile"
	flightRecorderURI = "/debug/flightrecorder"
)

// readOnlyAnnotations is the annotation set every tool on this server carries:
// read-only, non-destructive, idempotent, closed-world. The CPU-profiling tools
// perturb the process but do NOT modify state and are not "destructive" in the
// MCP sense, so they share these annotations; their cost is stated in their
// descriptions and enforced by the cpuGate, not signalled by destructiveHint.
func readOnlyAnnotations() *mcpsdk.ToolAnnotations {
	no := false
	return &mcpsdk.ToolAnnotations{
		ReadOnlyHint:    true,
		DestructiveHint: &no,
		IdempotentHint:  true,
		OpenWorldHint:   &no,
	}
}

// errorResult builds a tool-execution error result (isError:true) carrying a
// natural-language, actionable recovery message. Business failures use this, NOT
// a Go error (which the SDK would turn into a protocol error the model cannot
// see and self-correct from).
func errorResult(msg string) *mcpsdk.CallToolResult {
	return &mcpsdk.CallToolResult{
		IsError: true,
		Content: []mcpsdk.Content{&mcpsdk.TextContent{Text: msg}},
	}
}

// registerTools wires every read-only perf tool onto the server, sharing the one
// cpuGate across the two CPU-profiling tools.
func registerTools(srv *mcpsdk.Server, d Deps, gate *cpuGate) {
	registerQueryMetric(srv, d)
	registerTopCPUFunctions(srv, d, gate)
	registerCaptureCPUProfile(srv, d, gate)
	registerTopAllocations(srv, d)
	registerListSlowTurns(srv, d)
	registerCaptureFlightRecorder(srv, d)
}

// --- query_metric ---

// QueryMetricInput selects a curated metric to summarize. All fields optional:
// omitting metric_name lists the available names (discovery).
type QueryMetricInput struct {
	MetricName string  `json:"metric_name,omitempty" jsonschema:"the curated metric to query; omit to list available metric names"`
	Quantile   float64 `json:"quantile,omitempty" jsonschema:"for a histogram metric, a single quantile in (0,1] to report (e.g. 0.99); omit for p50/p90/p99"`
	Role       string  `json:"role,omitempty" jsonschema:"filter to one engine role family: main|subagent|member|parallel|usermodel|child; omit to aggregate across all roles"`
}

// QueryMetricOutput is the structured result of query_metric. Exactly one shape
// is populated per call: AvailableMetrics for discovery, or the metric summary.
type QueryMetricOutput struct {
	AvailableMetrics []string            `json:"available_metrics,omitempty"`
	Metric           *MetricSummaryEntry `json:"metric,omitempty"`
}

func registerQueryMetric(srv *mcpsdk.Server, d Deps) {
	mcpsdk.AddTool(srv, &mcpsdk.Tool{
		Name: "query_metric",
		Description: "Read one curated runtime metric. Call with no metric_name to list the available metric names (discovery). " +
			"With a metric_name, returns either a histogram summary (observation count + p50/p90/p99 bucket UPPER BOUNDS in seconds, " +
			"or a single quantile if 'quantile' is given) or a scalar value for a counter/gauge, aggregated across ALL engine roles by default; " +
			"set 'role' (main|subagent|member|parallel|usermodel|child) to read one role family's share. Cheap and unlimited. " +
			"Unknown names return an error listing how to discover valid names.",
		Annotations: readOnlyAnnotations(),
	}, func(_ context.Context, _ *mcpsdk.CallToolRequest, in QueryMetricInput) (*mcpsdk.CallToolResult, QueryMetricOutput, error) {
		if in.MetricName == "" {
			out := QueryMetricOutput{AvailableMetrics: availableMetricNames()}
			return resultWithText(out, "Available metrics: call query_metric with one of the listed metric_name values.")
		}
		c, ok := curatedByName[in.MetricName]
		if !ok {
			return errorResult(fmt.Sprintf(
				"unknown metric %q; call query_metric with no metric_name to list the available metric names",
				in.MetricName,
			)), QueryMetricOutput{}, nil
		}
		if in.Role != "" && !isRoleFamily(in.Role) {
			return errorResult(fmt.Sprintf(
				"unknown role %q; valid roles are %s (omit role to aggregate across all)",
				in.Role, strings.Join(roleFamilies, "|"),
			)), QueryMetricOutput{}, nil
		}
		byName, err := gatherByName(d)
		if err != nil && len(byName) == 0 {
			return errorResult("could not gather metrics right now; retry shortly"), QueryMetricOutput{}, nil
		}
		entry := MetricSummaryEntry{Name: c.name, Description: c.desc, Kind: "absent"}
		if mf := byName[c.family]; mf != nil {
			quantiles := defaultMetricQuantiles
			if in.Quantile > 0 {
				quantiles = []float64{in.Quantile}
			}
			fillEntry(&entry, c, mf, in.Role, quantiles)
			// The per-role breakdown rides only the UNFILTERED read (with a role
			// filter the whole entry already IS one role's share) and only for the
			// families that carry the role dimension meaningfully in the summary.
			if in.Role == "" && (entry.Kind == "histogram" || roleBreakdownMetrics[c.name]) {
				entry.ByRole = roleBreakdown(mf)
			}
		}
		if entry.Kind == "absent" || (in.Role != "" && entry.Kind == "histogram" && entry.Count == 0) {
			suffix := "it appears once the harness has produced the relevant activity"
			if in.Role != "" {
				suffix = fmt.Sprintf("no observations recorded for role %q yet", in.Role)
			}
			return errorResult(fmt.Sprintf(
				"metric %q is not present yet (no observations recorded); %s",
				in.MetricName, suffix,
			)), QueryMetricOutput{}, nil
		}
		out := QueryMetricOutput{Metric: &entry}
		return resultWithText(out, fmt.Sprintf("Metric %q (%s).", entry.Name, entry.Kind))
	})
}

// --- top_cpu_functions ---

// TopCPUFunctionsInput configures a short CPU profile + top-N reduction.
type TopCPUFunctionsInput struct {
	DurationSeconds int `json:"duration_seconds,omitempty" jsonschema:"CPU profiling window in seconds (1-30, default 5)"`
	Limit           int `json:"limit,omitempty" jsonschema:"number of top functions to return (1-50, default 15)"`
}

// TopCPUFunctionsOutput is the FuncStat-based result for the CPU tools.
type TopCPUFunctionsOutput struct {
	DurationSeconds int        `json:"duration_seconds"`
	SampleCount     int        `json:"sample_count"`
	Top             []FuncStat `json:"top"`
}

func registerTopCPUFunctions(srv *mcpsdk.Server, d Deps, gate *cpuGate) {
	mcpsdk.AddTool(srv, &mcpsdk.Tool{
		Name: "top_cpu_functions",
		Description: "Capture a short CPU profile of THIS process and return the top CPU-consuming functions (name, file basename, flat/cum nanoseconds). " +
			"COST: this PERTURBS the running process for duration_seconds and is rate limited to one CPU capture per cooldown window across all CPU tools. " +
			"Returns a reduced top-N only — never the raw profile. Use query_metric for cheap latency reads when you do not need function attribution.",
		Annotations: readOnlyAnnotations(),
	}, func(_ context.Context, _ *mcpsdk.CallToolRequest, in TopCPUFunctionsInput) (*mcpsdk.CallToolResult, TopCPUFunctionsOutput, error) {
		acq := gate.acquire()
		if !acq.ok {
			return errorResult(acq.reason), TopCPUFunctionsOutput{}, nil
		}
		defer acq.release()

		secs := clampCPUSeconds(in.DurationSeconds)
		limit := clampLimit(in.Limit, 15, 50)
		raw, err := d.Profiler.CPUProfile(time.Duration(secs) * time.Second)
		if err != nil {
			return errorResult("CPU profiling could not start (another profile may be active in the process); retry shortly"), TopCPUFunctionsOutput{}, nil
		}
		prof, err := parseProfile(raw)
		if err != nil {
			return errorResult("the captured CPU profile could not be parsed"), TopCPUFunctionsOutput{}, nil
		}
		top := topFunctions(prof, "cpu", limit)
		out := TopCPUFunctionsOutput{DurationSeconds: secs, SampleCount: len(prof.Sample), Top: top}
		return resultWithText(out, fmt.Sprintf("Top %d CPU functions over %ds.", len(top), secs))
	})
}

// --- capture_cpu_profile ---

// CaptureCPUProfileInput mirrors top_cpu_functions plus a raw-link flag.
type CaptureCPUProfileInput struct {
	DurationSeconds int  `json:"duration_seconds,omitempty" jsonschema:"CPU profiling window in seconds (1-30, default 5)"`
	Limit           int  `json:"limit,omitempty" jsonschema:"number of top functions to return (1-50, default 15)"`
	IncludeRawLink  bool `json:"include_raw_link,omitempty" jsonschema:"if true, also return a user-audience link to the loopback raw pprof endpoint for a human to download"`
}

func registerCaptureCPUProfile(srv *mcpsdk.Server, d Deps, gate *cpuGate) {
	mcpsdk.AddTool(srv, &mcpsdk.Tool{
		Name: "capture_cpu_profile",
		Description: "Capture a short CPU profile and return the reduced top-N function summary. COST: PERTURBS the process for duration_seconds; " +
			"rate limited to one CPU capture per cooldown window across all CPU tools. With include_raw_link=true it ALSO returns a user-audience " +
			"resource link to the loopback raw-pprof endpoint (for a human to download with go tool pprof) — the model receives only the summary, never the raw bytes.",
		Annotations: readOnlyAnnotations(),
	}, func(_ context.Context, _ *mcpsdk.CallToolRequest, in CaptureCPUProfileInput) (*mcpsdk.CallToolResult, TopCPUFunctionsOutput, error) {
		acq := gate.acquire()
		if !acq.ok {
			return errorResult(acq.reason), TopCPUFunctionsOutput{}, nil
		}
		defer acq.release()

		secs := clampCPUSeconds(in.DurationSeconds)
		limit := clampLimit(in.Limit, 15, 50)
		raw, err := d.Profiler.CPUProfile(time.Duration(secs) * time.Second)
		if err != nil {
			return errorResult("CPU profiling could not start (another profile may be active in the process); retry shortly"), TopCPUFunctionsOutput{}, nil
		}
		prof, err := parseProfile(raw)
		if err != nil {
			return errorResult("the captured CPU profile could not be parsed"), TopCPUFunctionsOutput{}, nil
		}
		top := topFunctions(prof, "cpu", limit)
		out := TopCPUFunctionsOutput{DurationSeconds: secs, SampleCount: len(prof.Sample), Top: top}

		content := []mcpsdk.Content{&mcpsdk.TextContent{Text: marshalText(out)}}
		if in.IncludeRawLink {
			content = append(content, userResourceLink(
				pprofProfilePath,
				"raw-cpu-profile",
				"Loopback raw pprof CPU profile endpoint (human download)",
			))
		}
		return &mcpsdk.CallToolResult{Content: content, StructuredContent: out}, out, nil
	})
}

// --- top_allocations ---

// TopAllocationsInput configures the heap top-N.
type TopAllocationsInput struct {
	Limit int `json:"limit,omitempty" jsonschema:"number of top allocating functions to return (1-50, default 15)"`
}

// TopAllocationsOutput is the heap allocation summary.
type TopAllocationsOutput struct {
	TotalHeapBytes int64       `json:"total_heap_bytes"`
	Top            []AllocStat `json:"top"`
}

func registerTopAllocations(srv *mcpsdk.Server, d Deps) {
	mcpsdk.AddTool(srv, &mcpsdk.Tool{
		Name: "top_allocations",
		Description: "Return the top heap-allocating functions of THIS process from the live heap/allocs profile (name, file basename, flat/cum bytes) plus total heap bytes. " +
			"Cheap (no profiling window) and unlimited. Returns a reduced top-N only — never the raw profile.",
		Annotations: readOnlyAnnotations(),
	}, func(_ context.Context, _ *mcpsdk.CallToolRequest, in TopAllocationsInput) (*mcpsdk.CallToolResult, TopAllocationsOutput, error) {
		limit := clampLimit(in.Limit, 15, 50)
		raw, err := d.Profiler.Lookup("allocs")
		if err != nil {
			// Fall back to the heap profile if allocs is unavailable.
			raw, err = d.Profiler.Lookup("heap")
		}
		if err != nil {
			return errorResult("the heap profile is not available on this runtime"), TopAllocationsOutput{}, nil
		}
		prof, err := parseProfile(raw)
		if err != nil {
			return errorResult("the heap profile could not be parsed"), TopAllocationsOutput{}, nil
		}
		total, top := memTop(prof, limit)
		out := TopAllocationsOutput{TotalHeapBytes: total, Top: top}
		return resultWithText(out, fmt.Sprintf("Top %d allocating functions.", len(top)))
	})
}

// --- list_slow_turns ---

// ListSlowTurnsInput pages the slow-turn history.
type ListSlowTurnsInput struct {
	ThresholdMs int64  `json:"threshold_ms,omitempty" jsonschema:"only turns at least this slow (ms); omit for the source default"`
	Limit       int    `json:"limit,omitempty" jsonschema:"max turns to return (1-20, default 10)"`
	Cursor      string `json:"cursor,omitempty" jsonschema:"opaque pagination cursor from a previous response's nextCursor"`
	Role        string `json:"role,omitempty" jsonschema:"filter to one engine role family: main|subagent|member|parallel|usermodel|child; omit for all roles"`
}

// ListSlowTurnsOutput is the paginated slow-turn page. Each turn carries numerics
// and timestamps only — never prompt text or session IDs.
type ListSlowTurnsOutput struct {
	Turns      []SlowTurn `json:"turns"`
	NextCursor string     `json:"nextCursor,omitempty"`
	TotalCount int        `json:"totalCount"`
}

func registerListSlowTurns(srv *mcpsdk.Server, d Deps) {
	mcpsdk.AddTool(srv, &mcpsdk.Tool{
		Name: "list_slow_turns",
		Description: "List recent slow turns (newest first) with per-turn timing only: turn index, duration, time-to-first-token, worst inter-token gap, end time, " +
			"and the bounded engine role family (main|subagent|member|parallel|usermodel|child) that produced the turn — filter with 'role'. " +
			"NO prompt text, tool arguments, or session IDs are ever included. Cursor-paginated; cheap and unlimited. " +
			"Returns an error if per-turn history is not enabled.",
		Annotations: readOnlyAnnotations(),
	}, func(_ context.Context, _ *mcpsdk.CallToolRequest, in ListSlowTurnsInput) (*mcpsdk.CallToolResult, ListSlowTurnsOutput, error) {
		if d.SlowTurns == nil {
			return errorResult("per-turn history is not enabled; start the harness with per-turn slow-turn tracking to use this tool"), ListSlowTurnsOutput{}, nil
		}
		if in.Role != "" && !isRoleFamily(in.Role) {
			return errorResult(fmt.Sprintf(
				"unknown role %q; valid roles are %s (omit role for all)",
				in.Role, strings.Join(roleFamilies, "|"),
			)), ListSlowTurnsOutput{}, nil
		}
		limit := clampLimit(in.Limit, 10, 20)
		offset := decodeCursor(in.Cursor)
		// The source returns its whole bounded, newest-first set; we paginate over
		// it in-memory so totalCount is accurate and cursors are stable.
		all := d.SlowTurns.Recent(in.ThresholdMs)
		if in.Role != "" {
			filtered := all[:0:0]
			for _, turn := range all {
				if turn.Role == in.Role {
					filtered = append(filtered, turn)
				}
			}
			all = filtered
		}
		total := len(all)
		var page []SlowTurn
		if offset < total {
			end := offset + limit
			if end > total {
				end = total
			}
			page = all[offset:end]
		}
		out := ListSlowTurnsOutput{Turns: page, TotalCount: total}
		if offset+len(page) < total {
			out.NextCursor = encodeCursor(offset + len(page))
		}
		return resultWithText(out, fmt.Sprintf("%d slow turn(s) returned.", len(page)))
	})
}

// --- capture_flight_recorder ---

// CaptureFlightRecorderInput takes no parameters.
type CaptureFlightRecorderInput struct{}

// CaptureFlightRecorderOutput summarizes a flight-recorder snapshot without
// returning the trace bytes.
type CaptureFlightRecorderOutput struct {
	CapturedBytes int    `json:"captured_bytes"`
	WindowSummary string `json:"window_summary"`
}

func registerCaptureFlightRecorder(srv *mcpsdk.Server, d Deps) {
	mcpsdk.AddTool(srv, &mcpsdk.Tool{
		Name: "capture_flight_recorder",
		Description: "Capture the execution-trace flight-recorder window and return only its size plus a one-line summary, along with a user-audience link " +
			"to the loopback /debug/flightrecorder endpoint for a human to download the raw trace. The model never receives the trace bytes. " +
			"Returns an error if the flight recorder is not armed.",
		Annotations: readOnlyAnnotations(),
	}, func(_ context.Context, _ *mcpsdk.CallToolRequest, _ CaptureFlightRecorderInput) (*mcpsdk.CallToolResult, CaptureFlightRecorderOutput, error) {
		if d.Recorder == nil || !d.Recorder.Enabled() {
			return errorResult("flight recorder not armed (start with --flight-recorder)"), CaptureFlightRecorderOutput{}, nil
		}
		b, err := d.Recorder.SnapshotBytes()
		if err != nil {
			return errorResult("the flight recorder is armed but a snapshot could not be taken right now; retry shortly"), CaptureFlightRecorderOutput{}, nil
		}
		out := CaptureFlightRecorderOutput{
			CapturedBytes: len(b),
			WindowSummary: fmt.Sprintf("execution-trace window of %d bytes captured; download the raw trace from the loopback endpoint to analyze with go tool trace", len(b)),
		}
		content := []mcpsdk.Content{
			&mcpsdk.TextContent{Text: marshalText(out)},
			userResourceLink(flightRecorderURI, "flight-recorder-trace", "Loopback execution-trace snapshot (human download)"),
		}
		return &mcpsdk.CallToolResult{Content: content, StructuredContent: out}, out, nil
	})
}

// --- helpers ---

// resultWithText returns a success result that carries BOTH the structured Out
// value (the SDK populates structuredContent from the returned Out) and a short
// human text line in Content, satisfying the structured-plus-text convention. The
// trailing nil error matches the ToolHandlerFor signature so call sites can
// `return resultWithText(...)` directly.
func resultWithText[T any](out T, text string) (*mcpsdk.CallToolResult, T, error) { //nolint:unparam // the trailing nil error is intentional: it matches the ToolHandlerFor signature so call sites return it directly
	res := &mcpsdk.CallToolResult{
		Content: []mcpsdk.Content{&mcpsdk.TextContent{Text: text}},
	}
	return res, out, nil
}

// marshalText renders a value as compact JSON text for a Content text block, used
// where a tool builds its CallToolResult by hand (to attach a resource_link).
func marshalText(v any) string {
	b, err := json.Marshal(v)
	if err != nil {
		return "{}"
	}
	return string(b)
}

// userResourceLink builds a resource_link content item annotated for the USER
// audience only, so the host shows the human a download link but does NOT feed
// the (potentially large/raw) target into the model's context.
func userResourceLink(uri, name, desc string) *mcpsdk.ResourceLink {
	return &mcpsdk.ResourceLink{
		URI:         uri,
		Name:        name,
		Description: desc,
		Annotations: &mcpsdk.Annotations{Audience: []mcpsdk.Role{"user"}},
	}
}

// clampLimit clamps a requested limit to [1, maxLimit], applying def when the
// request is zero/unset.
func clampLimit(requested, def, maxLimit int) int {
	if requested <= 0 {
		return def
	}
	if requested > maxLimit {
		return maxLimit
	}
	return requested
}

// encodeCursor / decodeCursor implement an opaque integer-offset cursor. The
// value is a decimal offset rendered as text; clients MUST treat it as opaque.
func encodeCursor(offset int) string { return fmt.Sprintf("o%d", offset) }

func decodeCursor(cursor string) int {
	if cursor == "" {
		return 0
	}
	var off int
	if _, err := fmt.Sscanf(cursor, "o%d", &off); err != nil || off < 0 {
		return 0
	}
	return off
}
