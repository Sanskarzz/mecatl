---
name: perf-optimization
description: >-
  Profile-driven performance optimization of mecatl using the offline benchmark +
  scenario harness. Use when optimizing allocations or latency, reducing allocs/op
  or memory, profiling a Go benchmark, pinpointing a hotspot with pprof, proving a
  win with benchstat, adding a regression benchmark, or investigating "why is this
  slow / allocating" or a suspected perf regression. Covers task bench,
  task perf:scenarios, memprofile -> pprof, benchstat A/B, allocs-first gating, and
  the discipline (follow the profile not the hypothesis; keep pure-perf changes
  byte-identical; mutation-test cache guards; skip the wrong abstraction).
  NOT for the live perf MCP server (use perf-mcp-interpretation) or non-mecatl Go
  profiling.
metadata:
  author: mecatl
  tags: [performance, profiling, benchmarks, pprof, benchstat, allocations]
---

# Perf optimization (mecatl offline harness)

The companion to the regression-tracking design in
[`docs/design/perf-tracking.md`](../../../docs/design/perf-tracking.md). This skill
is the **offline benchmark/scenario** workflow: measure → profile → optimize →
prove → guard. For diagnosing a **running** harness via the perf MCP server, use
the `perf-mcp-interpretation` skill instead — different tool, different signals.

## The harness (where the numbers come from)

- `task bench` — hot-path microbenchmarks (`engine/prompt`, `engine/governance`,
  `engine/agent`). `BENCHCOUNT` default 10. Offline (mockllm + memfs). Not part of
  `task test`.
- `task perf:scenarios` — five whole-loop scenarios. Four live in
  `perf/scenarios/` (single-session-long, team-fanout, background-subagents,
  compaction-cycle); the fifth (tui-scrollback) lives in `cmd/mecatui/ui/`, and the
  task runs **both** packages — so `go test ./perf/scenarios/` alone gives only four.
  `BENCHCOUNT` default 6. `MECATL_PERF_JSON=.scratch/x.json` writes the KPI JSON.
- Both are deterministic and offline — never reach for a live model/network.

## What to gate on (allocs-first)

`allocs/op`, `bytes/op`, goroutine-delta, and cache-hit-rate are **deterministic and
portable** — these are the real signal. `ns/op` and `rss_*` are machine-specific —
treat as advisory shape, never the headline. A perf change is **done only when
allocs/op moves** in benchstat; a wall-clock-only "win" on a shared machine is noise.

## Workflow

### 1. Measure — pick the benchmark that actually exercises the path

Run the relevant benchmark and confirm it reflects the workload you care about. A
benchmark whose per-iteration **setup** dwarfs the code under test, or that is
**all-miss by construction**, will hide a real win. If no benchmark covers the path,
add one first (match the existing `bench_test.go` style: `for b.Loop()`, results to a
package-level sink, offline).

### 2. Profile — pinpoint the site, do NOT guess

Capture an allocation profile on the benchmark and follow it to a `file:line`:

```sh
go test -run='^$' -bench=BenchmarkX -benchmem -memprofile=.scratch/x.mprof -count=3 ./pkg/
go tool pprof -alloc_space   -top -nodecount=25 .scratch/x.mprof   # bytes
go tool pprof -alloc_objects -top -nodecount=25 .scratch/x.mprof   # object count
go tool pprof -list=FuncName .scratch/x.mprof                      # line-level
```

Save profiles under `.scratch/` (repo rule — never `/tmp`). **Follow the profile to
the real site.** The hypothesis is often wrong (see the playbook: the TUI hotspot was
the string join, not `SetContent` as assumed). Let `-list` show you the exact lines.

### 3. Design — the smallest change at the proven hotspot

Optimize only what the profile proves is hot. Weigh the win against the real cost: a
microsecond saved once per turn is invisible next to an LLM round-trip. **The wrong
abstraction is worse than the allocation** — if the clean seam doesn't exist or the
fix adds stateful invalidation surface for a marginal gain, it is a legitimate NO-GO.
Say so and skip it rather than forcing it.

### 4. Prove — benchstat before/after, count >= 10

```sh
go test -run='^$' -bench=BenchmarkX -benchmem -count=10 ./pkg/ > .scratch/before.txt
# ... apply the change ...
go test -run='^$' -bench=BenchmarkX -benchmem -count=10 ./pkg/ > .scratch/after.txt
benchstat .scratch/before.txt .scratch/after.txt   # go install golang.org/x/perf/cmd/benchstat@latest
```

`allocs/op` / `B/op` must drop with a statistically significant delta. Re-run
`task perf:scenarios` and confirm the scenario KPI moved in the expected direction.
Update the baseline snapshot in `docs/design/perf-tracking.md`.

### 5. Guard — keep behaviour identical, prove the guard isn't vacuous

- **Pure-perf changes must be byte-identical.** For the TUI, `task test:golden` must
  stay green with **zero golden diffs** — you change HOW, never WHAT. Caveat: golden
  refresh runs `-update` first, so a plain `go test ./...` (no `-update`) against the
  committed goldens is what actually catches a regression; don't rely on the refresh
  step to catch a stale serve.
- **Any cache/oracle you add must be mutation-tested.** Temporarily break it (force
  the stale/wrong path), confirm a test goes red, then restore (back up with `cp`,
  restore with `cp` — never `git checkout`, it wipes uncommitted work). A guard that
  stays green when the behaviour is broken is worse than none.

## Common pitfalls

- **All-miss benchmark hides the win.** A streaming bench that mutates state every
  iteration never hits the cache you added — it's the worst-case floor, flat by
  design. Add a *steady-state* bench for the cache-hit path to show the real win.
- **Setup swamps the signal.** Build fixtures outside `b.Loop()`; confirm with a
  quick profile that setup isn't the dominant allocator.
- **Chasing `ns/op` on a shared machine.** Gate on allocs; ns is advisory.
- **Memoizing across the byte-stable prompt prefix.** Any prompt-inventory cache must
  produce byte-identical output or it tanks the provider cache-hit rate — the exact
  thing perf-tracking exists to protect. Usually not worth it (see playbook).

## See Also

- [`references/playbook.md`](references/playbook.md) — pprof flag cookbook + two
  worked examples (a real win and a real NO-GO) showing the discipline end to end.
- [`docs/design/perf-tracking.md`](../../../docs/design/perf-tracking.md) — the KPI
  design, gating posture, baselines, and the full roadmap.
- `perf-mcp-interpretation` skill — the **live** counterpart (running-harness
  diagnosis via the perf MCP server).
