package mcpperf

import (
	"context"
	"encoding/json"
	"fmt"
	"strings"

	mcpsdk "github.com/modelcontextprotocol/go-sdk/mcp"

	"github.com/stacklok/mecatl/internal/adapter/telemetry"
)

// Resource URIs. The perf:// scheme namespaces this server's read-only views;
// each is application-controlled context the host may include, not a tool the
// model invokes.
const (
	uriRuntimeSummary  = "perf://runtime/summary"
	uriRuntimeMemstats = "perf://runtime/memstats"
	uriMetricsSummary  = "perf://metrics/summary"
	// uriPprofTemplate is the parameterized profile-summary resource; {profile}
	// is one of heap|goroutine|allocs|mutex|block.
	uriPprofTemplate = "perf://pprof/{profile}"
	// pprofURIPrefix is the literal prefix of an expanded pprof resource URI, used
	// to extract the {profile} segment from a read request.
	pprofURIPrefix = "perf://pprof/"
)

const (
	mimeJSON = "application/json"
)

// pprofSummaryTopN is the fixed top-N used by the pprof resource template. The
// resource is application-controlled and parameterless beyond {profile}, so the
// reduction depth is a server constant rather than a client knob.
const pprofSummaryTopN = 15

// registerResources wires the runtime, memstats, metrics-summary, and pprof
// template resources onto the server. Each handler reduces server-side and
// returns JSON text; none ever returns a raw profile or the raw /metrics dump.
func registerResources(srv *mcpsdk.Server, d Deps) {
	srv.AddResource(&mcpsdk.Resource{
		URI:         uriRuntimeSummary,
		Name:        "runtime-summary",
		Description: "Current runtime snapshot: goroutines, CPU count, heap/total memory bytes, GC pause count and ~p99 pause upper bound, RSS, and uptime. Small JSON DTO.",
		MIMEType:    mimeJSON,
	}, resourceJSON(func(context.Context) (any, error) {
		return d.Snapshot(), nil
	}))

	srv.AddResource(&mcpsdk.Resource{
		URI:         uriRuntimeMemstats,
		Name:        "runtime-memstats",
		Description: "Memory-focused projection of the runtime snapshot (heap alloc/objects, heap-object bytes, total mapped bytes, RSS). Derived from runtime/metrics, NOT a stop-the-world ReadMemStats.",
		MIMEType:    mimeJSON,
	}, resourceJSON(func(context.Context) (any, error) {
		return memstatsProjection(d.Snapshot()), nil
	}))

	srv.AddResource(&mcpsdk.Resource{
		URI:         uriMetricsSummary,
		Name:        "metrics-summary",
		Description: "Curated latency-histogram summary: per instrument the observation count and p50/p90/p99 bucket UPPER BOUNDS (seconds). Reduced from the gathered metric families — never the raw /metrics exposition.",
		MIMEType:    mimeJSON,
	}, resourceJSON(func(context.Context) (any, error) {
		return metricsSummary(d)
	}))

	srv.AddResourceTemplate(&mcpsdk.ResourceTemplate{
		Name:        "pprof-summary",
		URITemplate: uriPprofTemplate,
		Description: "Reduced top-15 function summary of a named profile (profile ∈ heap|goroutine|allocs|mutex|block): function name, file basename, and flat/cum values. Never the raw profile bytes — paths are basenamed and pprof labels are dropped.",
		MIMEType:    mimeJSON,
	}, pprofResourceHandler(d))
}

// resourceJSON adapts a value-producing func into a ResourceHandler that marshals
// the value to indented JSON text under the requested URI. A producer error is
// returned as a Go error (the SDK maps it to a protocol error), which is correct
// for resources: a read either yields content or fails.
func resourceJSON(produce func(context.Context) (any, error)) mcpsdk.ResourceHandler {
	return func(ctx context.Context, req *mcpsdk.ReadResourceRequest) (*mcpsdk.ReadResourceResult, error) {
		v, err := produce(ctx)
		if err != nil {
			return nil, err
		}
		b, err := json.MarshalIndent(v, "", "  ")
		if err != nil {
			return nil, fmt.Errorf("mcpperf: marshal resource: %w", err)
		}
		uri := uriRuntimeSummary
		if req != nil && req.Params != nil {
			uri = req.Params.URI
		}
		return &mcpsdk.ReadResourceResult{
			Contents: []*mcpsdk.ResourceContents{
				{URI: uri, MIMEType: mimeJSON, Text: string(b)},
			},
		}, nil
	}
}

// pprofResourceHandler returns the handler for the perf://pprof/{profile}
// template. It extracts the {profile} segment, rejects an unknown profile with a
// resource-not-found PROTOCOL error (the URI named a resource that does not
// exist), looks up and parses the profile, and returns the reduced top-N JSON.
func pprofResourceHandler(d Deps) mcpsdk.ResourceHandler {
	return func(_ context.Context, req *mcpsdk.ReadResourceRequest) (*mcpsdk.ReadResourceResult, error) {
		uri := ""
		if req != nil && req.Params != nil {
			uri = req.Params.URI
		}
		profileName := strings.TrimPrefix(uri, pprofURIPrefix)
		if profileName == uri || !isKnownNamedProfile(profileName) {
			// Unknown profile: the URI does not name a real resource. This is a
			// genuine not-found, so a resource-not-found protocol error is correct.
			return nil, mcpsdk.ResourceNotFoundError(uri)
		}

		raw, err := d.Profiler.Lookup(profileName)
		if err != nil {
			return nil, fmt.Errorf("mcpperf: lookup profile %q: %w", profileName, err)
		}
		prof, err := parseProfile(raw)
		if err != nil {
			return nil, fmt.Errorf("mcpperf: parse profile %q: %w", profileName, err)
		}
		top := topFunctions(prof, "", pprofSummaryTopN)
		payload := map[string]any{
			"profile": profileName,
			"top":     top,
		}
		b, err := json.MarshalIndent(payload, "", "  ")
		if err != nil {
			return nil, fmt.Errorf("mcpperf: marshal pprof summary: %w", err)
		}
		return &mcpsdk.ReadResourceResult{
			Contents: []*mcpsdk.ResourceContents{
				{URI: uri, MIMEType: mimeJSON, Text: string(b)},
			},
		}, nil
	}
}

// MemstatsProjection is the memory-focused superset view of the runtime snapshot
// served by perf://runtime/memstats. It is a projection of RuntimeSnapshot (NOT a
// runtime.ReadMemStats), so it inherits the snapshot's lock-free, no-STW
// guarantee. Byte counts are bytes.
type MemstatsProjection struct {
	HeapAllocBytes   uint64 `json:"heap_alloc_bytes"`
	HeapObjects      uint64 `json:"heap_objects"`
	HeapObjectBytes  uint64 `json:"heap_object_bytes"`
	TotalMemoryBytes uint64 `json:"total_memory_bytes"`
	RSSBytes         uint64 `json:"rss_bytes"`
	// Available echoes the snapshot's Available set so a consumer can tell which
	// runtime/metrics-derived fields were actually present this read.
	Available []string `json:"available"`
}

// memstatsProjection builds the memory view from a runtime snapshot.
func memstatsProjection(s telemetry.RuntimeSnapshot) MemstatsProjection {
	return MemstatsProjection{
		HeapAllocBytes:   s.HeapAllocBytes,
		HeapObjects:      s.HeapObjects,
		HeapObjectBytes:  s.HeapObjectBytes,
		TotalMemoryBytes: s.TotalMemoryBytes,
		RSSBytes:         s.RSSBytes,
		Available:        s.Available,
	}
}
