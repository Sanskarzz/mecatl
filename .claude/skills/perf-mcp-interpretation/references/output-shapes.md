# Exact output shapes

The reduced JSON shapes the mecatl perf MCP server returns. Field names are
verbatim from the server; trust these, not invented ones.

## Resources

### `perf://runtime/summary` (RuntimeSnapshot)
```jsonc
{
  "goroutines": 0,                      // live count; rising across reads = leak
  "num_cpu": 0,
  "gomaxprocs": 0,
  "heap_alloc_bytes": 0,                // cumulative bytes allocated to heap
  "heap_objects": 0,
  "total_memory_bytes": 0,              // all memory mapped by the runtime
  "heap_object_bytes": 0,               // live heap-object memory
  "gc_pause_count": 0,                  // 0 before first GC is a real zero
  "gc_pause_p99_upper_bound_ns": 0,     // BUCKET UPPER BOUND, not exact p99
  "rss_bytes": 0,                       // process RSS (Linux; 0 elsewhere)
  "uptime_seconds": 0,
  "available": ["..."]                  // which runtime/metrics fields were present
}
```
`goroutines`, `num_cpu`, `gomaxprocs`, `uptime_seconds`, `rss_bytes` are always
populated and are NOT listed in `available[]`; `available[]` tracks only the
runtime/metrics-derived fields.

### `perf://runtime/memstats` (MemstatsProjection)
```jsonc
{
  "heap_alloc_bytes": 0,
  "heap_objects": 0,
  "heap_object_bytes": 0,
  "total_memory_bytes": 0,
  "rss_bytes": 0,
  "available": ["..."]
}
```
A projection of the snapshot, NOT a stop-the-world `runtime.ReadMemStats`.

### `perf://metrics/summary` ([]MetricSummaryEntry)
One entry per curated metric. `kind` ∈ `"histogram" | "scalar" | "absent"`.
```jsonc
[
  {
    "name": "ttft_seconds",
    "description": "Time to first content token per turn (histogram, seconds).",
    "kind": "histogram",
    "count": 0,
    "quantiles": [ { "quantile": 0.99, "upper_bound": 0.0 } ]  // upper_bound = bucket ceiling, seconds
  },
  {
    "name": "active_runs",
    "description": "Runs currently in flight (gauge).",
    "kind": "scalar",
    "value": 0.0
  }
]
```

### `perf://pprof/{profile}`  ({profile} ∈ heap|goroutine|allocs|mutex|block)
```jsonc
{
  "profile": "goroutine",
  "top": [ { "function": "...", "file": "loop.go", "flat_value": 0, "cum_value": 0 } ]
}
```
`file` is the **basename only** (absolute paths dropped); pprof labels are never
included (redaction).

## Tools

### `query_metric{metric_name?, quantile?}`
- No `metric_name` → `{ "available_metrics": ["..."] }` (discovery).
- With a name → `{ "metric": MetricSummaryEntry }` (same shape as above).
- Unknown name or no-observations → `isError` result with a recovery message.

Curated metric short names (pass to `metric_name`):
`tool_duration_seconds`, `turn_duration_seconds`, `ttft_seconds`,
`inter_token_seconds`, `inter_token_max_seconds`, `tool_queue_seconds`,
`events_total`, `runs_total`, `tool_calls_total`, `tokens`,
`permission_asks_total`, `active_runs`, `cache_hit_ratio`, `process_rss_bytes`.
(Underlying Prometheus family names are `mecatl_<name>`.) Histogram quantiles
default to p50/p90/p99 unless `quantile` is given.

### `top_cpu_functions{duration_seconds?, limit?}` — PERTURBS, rate-limited
```jsonc
{
  "duration_seconds": 5,                // clamped 1-30, default 5
  "sample_count": 0,
  "top": [ { "function": "...", "file": "stream.go", "flat_value": 0, "cum_value": 0 } ]  // limit 1-50, default 15
}
```
Values are **nanoseconds** (the CPU profile's unit). Ranked by `flat` desc.

### `capture_cpu_profile{duration_seconds?, limit?, include_raw_link?}` — PERTURBS, rate-limited
Same output as `top_cpu_functions`. With `include_raw_link:true` ALSO returns a
user-audience `resource_link` to the loopback raw-pprof endpoint (human download).

### `top_allocations{limit?}` — cheap
```jsonc
{
  "total_heap_bytes": 0,
  "top": [ { "function": "...", "file": "chunk.go", "flat_bytes": 0, "cum_bytes": 0 } ]  // limit 1-50, default 15
}
```
From the live `allocs` profile (falls back to `heap`). Bytes, ranked by flat desc.

### `list_slow_turns{threshold_ms?, limit?, cursor?}` — cheap, paginated
```jsonc
{
  "turns": [
    {
      "turn_index": 0,
      "duration_ms": 0,
      "ttft_ms": 0,
      "inter_token_max_ms": 0,
      "ended_at": "2026-06-03T00:00:00Z"
    }
  ],
  "nextCursor": "",     // opaque; pass back as `cursor` for the next page
  "totalCount": 0
}
```
Newest first. **No** prompt text, tool args, or session IDs ever. `limit` 1-20
(default 10). Returns `isError` "per-turn history is not enabled" if the slow-turn
ring is not wired.

### `capture_flight_recorder{}` — needs `--flight-recorder`
```jsonc
{ "captured_bytes": 0, "window_summary": "execution-trace window of N bytes ..." }
```
Plus a user-audience `resource_link` to `/debug/flightrecorder` (human downloads,
analyzes with `go tool trace`). Returns `isError` "flight recorder not armed" if
the recorder is off.
