# Signature → diagnosis → next step

Match an observed symptom to its likely cause and the cheapest confirming step.
All quantiles are **bucket upper bounds**, not exact (see SKILL.md caveat).

| Symptom (what you read) | Likely diagnosis | Confirm with |
|---|---|---|
| `goroutines` rises monotonically across repeated `perf://runtime/summary` reads (between runs, not just during one) | **Goroutine leak** — a cancellation/drain path that doesn't unwind (per-run goroutine, SSE consumer + cancel, subagent/fork drain, server Run registry) | `perf://pprof/goroutine` top-N → which functions hold the stuck goroutines; cross-ref the live goroutine watchdog |
| `rss_bytes` climbs while `heap_alloc_bytes` / `heap_object_bytes` stay flat | **Off-heap growth** — invisible to Go heap/pprof. The WASM/tree-sitter leak signature (~23 MB RSS/call). RepoMap is currently disabled, but the pattern holds for any off-heap consumer | Re-read `perf://runtime/memstats` over time; correlate RSS slope with off-heap activity |
| `rss_bytes` and heap both climb together over a long session | Normal long-session growth (conversation history before compaction, jsonl store) | Watch whether compaction brings heap back down |
| `gc_pause_p99_upper_bound_ns` spikes; `gc_pause_count` rising fast | **GC pressure** → inter-token jitter (pauses land between tokens) | `top_allocations` — high flat_bytes on the streaming/chunk-decode/event-fanout path drives the churn |
| High `inter_token_max_seconds`, normal `ttft_seconds` | **Inter-token stutter** — user-felt jitter mid-stream; often GC, render-cadence, or a slow chunk path | `list_slow_turns` (high `inter_token_max_ms`); check GC signature above |
| High `ttft_seconds`, normal inter-token | **Slow first byte** — model/connection latency before the first token, not a mecatl hot path | `list_slow_turns` (high `ttft_ms`); usually nothing mecatl-side to fix |
| `tool_queue_seconds` p99 AND `tool_duration_seconds` p99 both high | **Dispatch contention** — read-parallel / mutate-serial: a slow mutating tool serially blocks queued mutations | `query_metric tool_calls_total` for which tool; correlate with the slow tool's duration |
| `tool_queue_seconds` high but `tool_duration_seconds` normal | Just load / many concurrent tools queued; not contention per se | Check `active_runs`, request volume |
| CPU profile dominated by JSON decode/marshal, markdown render, or chunk decode | **Real on-CPU signal** — this off-CPU workload spends little CPU, so on-CPU work on the hot streaming path is meaningful | Already confirmed; rank by `flat` for the hot leaf, `cum` to find the caller responsible |
| CPU profile dominated by runtime scheduler / netpoll / syscall wait | Expected — the loop is off-CPU waiting on the model/IO. Not actionable; don't chase it | Look at latency histograms + `list_slow_turns` instead |
| A few turns dominate p99 turn latency | **Tail latency** — averages lie for a streaming loop | `list_slow_turns`, then `capture_flight_recorder` immediately after a slow turn to give the human a trace window over it |
| `query_metric` returns `isError` "not present yet" | No relevant activity has occurred yet (real absence, not breakage) | Drive the harness to produce that activity, then re-query |
| A snapshot field is 0 and its name is absent from `available[]` | The field was **not measured** by this toolchain (presence vs real zero) | Don't infer a problem from it; test `available[]` membership |

## Reading pprof rankings (flat vs cum)

- **flat** = value attributed to this function alone (self time/space). The
  highest `flat` is the hottest single function — where the work actually happens.
- **cum** = this function plus everything it called. The highest `cum` is the
  responsible *caller* (e.g. a top-level handler whose callees are expensive).
- Diagnose by: find the hot leaf via `flat`, then walk up via `cum` to the caller
  worth fixing.
- CPU profile values are **nanoseconds**; alloc values are **bytes**.

## When to capture the flight recorder

Capture **right after observing a slow turn** (from `list_slow_turns`), because the
recorder is a bounded ring of the *recent past* (default ~8 MiB / 5s window) — wait
too long and the slow turn ages out of the window. The summary you get back is just
size + a one-line note; the raw trace goes to the human via the `resource_link` for
`go tool trace`.
