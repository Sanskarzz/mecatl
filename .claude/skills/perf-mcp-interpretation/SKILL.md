---
name: perf-mcp-interpretation
description: >-
  Interpret the mecatl perf MCP server's output to diagnose latency, goroutine
  leaks, GC pressure, allocation churn, and memory growth in a running mecatl
  harness. Use when connected to the mecatl perf MCP server (the perf:// resources
  or the query_metric / top_cpu_functions / capture_cpu_profile / top_allocations /
  list_slow_turns / capture_flight_recorder tools) and investigating why mecatl is
  slow, leaking, or growing. Covers tool routing/cost, reading pprof rankings and
  runtime metrics, and the leak/contention/GC signatures. NOT for generic Go
  profiling or non-mecatl MCP servers.
metadata:
  audience: an AI agent driving or debugging a mecatl harness over the perf MCP server
compatibility: Requires a connection to the mecatl perf MCP server (mecated/mecatui --perf-mcp, mounted at /mcp on the loopback admin listener).
---

# Interpreting mecatl perf MCP output

mecatl is a **streaming agentic loop**. Its time is dominated by *off-CPU* waiting
(on the model and on tool I/O), so the usual "run a CPU profile first" instinct
measures the wrong thing. Diagnose by reading cheap numeric state first, and reach
for the perturbing CPU tools only with a hypothesis to confirm.

## Cost discipline — what to call, in what order

1. **Start cheap, read state.** `perf://runtime/summary` (goroutines, heap, GC,
   RSS, uptime) and `perf://metrics/summary` (latency-histogram quantiles) are
   free, point-in-time reads. Read them first, and re-read `perf://runtime/summary`
   a few times to see *trends* — a single snapshot rarely diagnoses anything.
2. **Cheap tools next.** `query_metric` (one curated metric; omit `metric_name` to
   list names), `top_allocations` (heap rankings, no profiling window),
   `list_slow_turns` (per-turn timing) are all cheap and unlimited.
3. **Perturbing tools last.** `top_cpu_functions` and `capture_cpu_profile` start a
   live CPU profile that PERTURBS the process for `duration_seconds`, and are
   **rate limited to one capture per cooldown window** across both tools. Call them
   only to confirm a hypothesis, not to explore. A rate-limit hit comes back as an
   `isError` result saying to retry — wait, do not retry-spam.
4. **Never ingest raw blobs.** `capture_cpu_profile` (with `include_raw_link`) and
   `capture_flight_recorder` return a **user-audience `resource_link`** to a
   loopback `/debug/...` endpoint. That link is for a **human to download** (with
   `go tool pprof` / `go tool trace`) — the model receives only the reduced
   summary. Do NOT try to fetch or read the linked raw artifact; report the
   summary and tell the user the raw blob is available at the link.

## The MCP surface (what is actually there)

**Resources** (cheap, read-only, JSON):
- `perf://runtime/summary` — goroutines, num_cpu, gomaxprocs, heap_alloc_bytes,
  heap_objects, total_memory_bytes, heap_object_bytes, gc_pause_count,
  `gc_pause_p99_upper_bound_ns`, `rss_bytes`, uptime_seconds, `available[]`.
- `perf://runtime/memstats` — memory-focused projection (heap_alloc_bytes,
  heap_objects, heap_object_bytes, total_memory_bytes, rss_bytes, available[]).
- `perf://metrics/summary` — every curated metric reduced: histograms → count +
  p50/p90/p99 **bucket upper bounds (seconds)**; counters/gauges → a scalar value.
- `perf://pprof/{profile}` — template, `{profile}` ∈ `heap|goroutine|allocs|mutex|block`;
  reduced top-15 functions (function, file basename, flat/cum values).

**Tools** (all read-only):
- `query_metric{metric_name?, quantile?}` — one curated metric.
- `top_cpu_functions{duration_seconds?, limit?}` — **perturbs**, rate-limited.
- `capture_cpu_profile{duration_seconds?, limit?, include_raw_link?}` — **perturbs**, rate-limited.
- `top_allocations{limit?}` — heap top-N + total_heap_bytes.
- `list_slow_turns{threshold_ms?, limit?, cursor?}` — cursor-paginated, newest first.
- `capture_flight_recorder{}` — size + one-line summary + user link (needs `--flight-recorder`).

See [references/output-shapes.md](references/output-shapes.md) for the exact field
names of every tool's output (FuncStat, AllocStat, SlowTurn, MetricSummaryEntry).

## Honest-precision caveat — do not over-trust the numbers

Every histogram quantile this server returns (in `perf://metrics/summary`,
`query_metric`, and `gc_pause_p99_upper_bound_ns`) is the **upper bound of the
bucket the quantile rank falls in — NOT an interpolated exact quantile**. The
field is literally named `upper_bound` for this reason. Read p99 as "at worst this
bucket's ceiling," not an exact value. Report it as an upper bound.

## Interpreting the signatures

The full signature → diagnosis → next-step table is in
[references/signatures.md](references/signatures.md). Read it when you have a
symptom to match. The essentials:

- **Off-CPU workload.** This loop mostly waits on the model/IO, so high CPU in
  **JSON decode/marshal, markdown render, or chunk decode** is the real signal in
  a CPU profile — that is on-CPU work on the hot streaming path. Rank by `flat`
  (self time) for the hot leaf; use `cum` (includes callees) to find the
  responsible caller.
- **Goroutine leak.** `goroutines` rising monotonically across successive
  `perf://runtime/summary` reads (not just spiking during a run) = a leak. Read
  `perf://pprof/goroutine` to see which functions hold the stuck goroutines; this
  corroborates the live goroutine watchdog.
- **Off-heap growth (the WASM-leak signature).** `rss_bytes` climbing while
  `heap_alloc_bytes` / `heap_object_bytes` stay flat = growth **off the Go heap**,
  invisible to pprof/heap and runtime metrics. (RepoMap/tree-sitter WASM is the
  known cause; it is currently **disabled**, but the pattern still stands for any
  off-heap consumer.)
- **GC-driven jitter.** `gc_pause_p99_upper_bound_ns` spikes and a rising
  `gc_pause_count`, alongside high `top_allocations` on the streaming/chunk-decode
  path, explain inter-token jitter — GC pauses land between tokens.
- **User-felt latency = TTFT vs inter-token, split.** Read `ttft_seconds` and
  `inter_token_max_seconds` separately (the mean hides both). High `ttft` = slow
  first byte; high `inter_token_max` = stutter the user feels mid-stream.
- **Dispatch contention.** `mecatl_tool_queue_seconds` (short name `tool_queue_seconds`)
  rising **together with** `tool_duration_seconds` p99 = the read-parallel /
  mutate-serial dispatcher is queueing: a slow mutating tool serially blocks
  queued mutations. Queue time without duration is just load; both together is the
  contention signal.
- **Tail latency.** Averages lie. Use `list_slow_turns` to find the actual tail
  turns, then `capture_flight_recorder` **right after a slow turn** to hand the
  human a trace window covering it.

## Presence vs a real zero

`available[]` in the runtime/memstats snapshots lists which runtime/metrics fields
were actually published by this toolchain. A field absent from `available[]` was
**not measured**; a field present with value 0 is a **real zero**. `gc_pause_count`
is legitimately 0 before the first GC. `query_metric` returns an `isError`
"not present yet" for a metric with no observations — that means no relevant
activity has happened, not that the metric is broken.

## Workflow

1. Read `perf://runtime/summary` and `perf://metrics/summary`.
2. Match the symptom against the signatures above / `references/signatures.md`.
3. Confirm with the cheap tool for that signature (`top_allocations`, `query_metric`,
   `list_slow_turns`, `perf://pprof/goroutine`).
4. Only if you need function-level CPU attribution, spend the rate-limited
   `top_cpu_functions` / `capture_cpu_profile` once.
5. Report findings as numbers + an upper-bound caveat; point the user at any raw
   `resource_link` rather than ingesting it.
