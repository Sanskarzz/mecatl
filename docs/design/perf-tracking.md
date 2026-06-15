# Long-term performance & resource regression tracking

Status: **Phases 1 + 2 shipped; the trend store (Phase 3+) is research / proposal.**
Phase 0 (decide what we track) is settled below, Phase 1 (hot-path microbenchmarks
+ a `task bench` gate-feed) is built — see [Phase 1 — Status](#phase-1--status) — and
Phase 2 (the offline scenario harness behind `task perf:scenarios`) is built — see
[Phase 2 — Status](#phase-2--status). The trend store (Phase 3+) remains a design
sketch. Sibling to [`perf-observability.md`](perf-observability.md), which covers
the *introspection* half (pprof, flight recorder, OTel metrics, the perf MCP,
`--perf`, goleak).

## The gap this closes

`perf-observability.md` answers **"why is it slow/fat right now?"** — point-in-time
debugging. It does **not** answer **"did this commit make mecatl slower or fatter
than last week, and will CI tell me before it ships?"** That is a *regression
tracking* problem: baseline + time-series + a gate. This doc proposes how we close
it without standing up paid infrastructure.

Guiding calibration: mecatl is a library / CLI / embedded harness, not a fleet of
long-lived services. Its dominant resource cost is **tokens / $ and network
wall-clock**, not CPU. So the KPIs skew toward allocation discipline, memory growth
in long-running processes, goroutine hygiene, and token/cache efficiency — not CPU
flamegraphs (which the existing introspection tooling already covers on demand).

## Phase 0 — What we track (KPIs + baselines)

Gate **hard** on the deterministic/low-noise signals; treat wall-clock as advisory
until/unless we have a stable bench environment (see Phase 6 in the roadmap).

| KPI | Why it matters here | Noise | Gate | Current baseline |
|---|---|---|---|---|
| `allocs/op` on hot paths | Leading indicator of GC pressure; deterministic regardless of machine load | very low | **hard** | per-benchmark — see [Baseline snapshot](#baseline-snapshot) (micro) |
| Total allocations per offline scenario | Whole-loop allocation budget | very low | **hard** | per-scenario `allocs_per_op` — see [Baseline snapshot](#baseline-snapshot) (scenarios) |
| RSS-over-time on a fixed scenario | Long-session / scrollback growth (e.g. the `SetContent` O(scrollback) follow-up) | low | soft → hard | `rss_peak_bytes` / `rss_final_bytes` in the JSON (machine-specific; advisory) |
| End-of-run goroutine count | Delegation / team leak class we have hit before | deterministic | **hard** | **0** (delta) on every scenario — see [Baseline snapshot](#baseline-snapshot) |
| Tokens per scenario (input/output) | The dominant cost of an LLM harness | deterministic (offline) | **hard** | per-scenario — see [Baseline snapshot](#baseline-snapshot) |
| Prompt-cache-hit-rate per scenario | A prefix-stability regression silently doubles cost and no CPU/mem bench catches it | deterministic (offline) | **hard** | single_session **0.90**, team_fanout **0.75**; 0 by design on compaction/tui |
| `ns/op` on hot paths | Classic latency | high in shared CI | advisory | machine-specific; not pinned (advisory only) |
| Peak RSS per scenario | OOM headroom / `GOMEMLIMIT` budgeting | low | soft | `rss_peak_bytes` in the JSON (machine-specific) |

### Baseline snapshot

Captured `2026-06-14` on a Linux dev workstation (12 logical CPUs) at the Phase-1/2
landing. **`allocs/op` / `*_per_op` / cache-hit / goroutine-delta are deterministic and
portable — these are the gated baselines.** `ns/op` and `rss_*` are machine-specific and
recorded for shape only (advisory). The authoritative, commit-keyed baseline is owned by
the Phase 3 trend store once it lands; this block is the bootstrap reference. Re-capture
with `task bench` (micro) and `task perf:scenarios` (scenarios).

Micro hot paths (`task bench`, count=10 — allocs/op were identical across all 10 runs):

```
BenchmarkBuild                            31 allocs/op     6,532 B/op    ~2.1µs
BenchmarkBuildLargeCatalog               224 allocs/op    31,292 B/op   ~14µs
BenchmarkSplitCommands                    13 allocs/op       584 B/op   ~0.66µs
BenchmarkReadOnlyBash                     19 allocs/op       816 B/op   ~1.29µs
BenchmarkSubstitutionReadOnly             14 allocs/op       408 B/op   ~1.22µs
BenchmarkIsolationApprovable              32 allocs/op     1,272 B/op   ~2.0µs
BenchmarkEvaluatorEvaluate/simple-tool    13 allocs/op       856 B/op   ~0.94µs
BenchmarkEvaluatorEvaluate/plain-bash     18 allocs/op       920 B/op   ~1.39µs
BenchmarkEvaluatorEvaluate/compound-bash  29 allocs/op     1,544 B/op   ~2.8µs
BenchmarkBuildRequest                     77 allocs/op    12,648 B/op   ~4.86µs
BenchmarkHeuristicCompact                199 allocs/op    15,445 B/op   ~17.5µs
BenchmarkCascadeCompact                  249 allocs/op    29,551 B/op   ~24µs
BenchmarkRunReadOnlyTurn                 164 allocs/op    32,769 B/op   ~31µs
BenchmarkRunReadParallelTurn             228 allocs/op    38,671 B/op   ~45µs
BenchmarkRunMutatingTurn                 159 allocs/op    31,898 B/op   ~27.5µs
```

Scenarios (`task perf:scenarios`, BENCHCOUNT=6 — `allocs_per_op`, goroutine delta, tokens):

```
single_session_long      ~36,656 allocs/op   ~4.83 MB/op   goroutines Δ=0   cache-hit 0.90
team_fanout               ~2,690 allocs/op    ~599 KB/op    goroutines Δ=0   cache-hit 0.75
background_subagents      ~1,307 allocs/op    ~258 KB/op    goroutines Δ=0
compaction_cycle          ~4,204 allocs/op    ~570 KB/op    goroutines Δ=0   (39 compactions/40 turns; cache-hit 0 by design)
tui_scrollback_view        ~6,270 allocs/op   ~33.4 MB/op   (400 blocks; STREAMING worst case — live block mutates every op, join always rebuilds)
tui_scrollback_view_steady    ~51 allocs/op    ~137 KB/op   (400 blocks; UNCHANGED frame — join cache serves the memoized string; was ~94 allocs / ~32.5 MB/op pre-cache)
```

> **tui-scrollback hotspot (2026-06-15).** A heap profile of the render path pinned
> the per-frame cost to the JOIN in `renderConversation`, NOT `vp.SetContent`:
> `strings.Builder.WriteString` was ~91% of allocations — every flushed frame
> re-copied all 400 cached block strings into a fresh `Builder`, even when the
> per-block cache hit on every block. The fix caches the whole joined string
> (`renderer.joinCache`, keyed on a `blockRenders`/block-count/width/expand
> signature) and reuses it verbatim on any frame that re-rendered no block. The
> streaming bench (`tui_scrollback_view`) mutates the live block every op so the
> join always rebuilds — it is the unchanged worst-case floor and is flat across the
> change. The realistic win is the UNCHANGED frame (cursor move, scroll, the
> twice-per-message `renderInput`): `tui_scrollback_view_steady` falls from
> ~32.5 MB/op to ~137 KB/op (−99.6% B/op; the residual is `vp.SetContent`'s line
> split, the named follow-up).

`goroutines Δ=0` on every scenario means no leak across the run — the gated invariant
for the team / background-subagent leak class.

### Why allocs-first

Allocation counts are deterministic: they do not move with CPU load, so they survive
noisy shared CI runners where `ns/op` is useless. For a long-running harness they are
*also* the thing that actually drives GC pressure and memory growth. So allocs are
both the lowest-noise signal and a high-value one — gate hard on them, gate soft (or
not at all in shared CI) on wall-clock.

### The non-obvious KPI: prompt-cache-hit-rate

mecatl relies on a **byte-stable prompt prefix** for provider-side caching. A change
that perturbs the prefix tanks the cache-hit rate and silently doubles token cost —
and *no CPU or memory benchmark would ever catch it*. `session.Usage` already carries
`CacheReadTokens ⊂ InputTokens`, so the raw signal exists; this KPI just tracks it
per scenario over commits. Arguably higher value than any `ns/op` number.

## Regression-detection methodology

- **Sampling:** `-benchmem -count=10 -run='^$'` (skip unit tests to cut noise),
  consistent `GOMAXPROCS`.
- **Comparison:** `benchstat` (`golang.org/x/perf/cmd/benchstat`) — reports deltas
  with a confidence interval + p-value, distinguishing real change from noise. This
  is the single most important tool in the Go ecosystem for this.
- **Noise handling** (in order of cost): allocs-first gating (free); relative
  comparison (baseline + candidate on the *same* CI job, compare the ratio);
  change-point detection over a window of history (Phase 3, Bencher) for fewer false
  positives than threshold-on-last-value; dedicated bare-metal runner + `perflock`
  (Phase 6) for trustworthy wall-clock.

## Phase 1 — Status

**Shipped.** Hot-path `testing.B` microbenchmarks now live next to the code they
measure, each using the Go 1.25 `for b.Loop()` form with results assigned to
package-level sinks so dead-code elimination cannot delete the work:

- [`engine/prompt/bench_test.go`](../../engine/prompt/bench_test.go) — `BenchmarkBuild`,
  `BenchmarkBuildLargeCatalog` (per-turn prompt assembly, the hot path inside
  `Engine.buildRequest`).
- [`engine/governance/bench_test.go`](../../engine/governance/bench_test.go) —
  `BenchmarkSplitCommands`, `BenchmarkReadOnlyBash`, `BenchmarkSubstitutionReadOnly`,
  `BenchmarkIsolationApprovable`, and `BenchmarkEvaluatorEvaluate` (sub-benchmarks for
  simple-tool / plain-Bash / compound-Bash over a realistic deny+ask+allow rule set).
  These are the security-critical Bash gate + permission-fold paths.
- [`engine/agent/bench_test.go`](../../engine/agent/bench_test.go) —
  `BenchmarkRunReadOnlyTurn` (read-parallel dispatch), `BenchmarkRunMutatingTurn`
  (serial dispatch); full engine runs over `mockllm` + `memfs`, offline.
- [`engine/agent/bench_internal_test.go`](../../engine/agent/bench_internal_test.go) —
  `BenchmarkBuildRequest`, a white-box benchmark of the unexported per-turn request
  assembler.

All four are offline (mockllm + memfs, no network/live model) and import no
`internal/...` package.

Run them with `task bench` (see [`Taskfile.yml`](../../Taskfile.yml)) — overridable
sample count via `BENCHCOUNT` (default 10), `-run='^$'` to skip unit tests,
`-benchmem` for the allocs/op signal. This is the command that fills the `allocs/op`
baseline cells above: run it on `main`, paste the `allocs/op` numbers (or let the
Phase 3 trend store own them), and use `benchstat` to compare a candidate against the
baseline. `task bench` is deliberately NOT part of `task test` — same posture as
`task fuzz`.

## Phase 2 — Status

**Shipped.** The offline scenario harness now lives in two homes, both
`testing.B`-driven (no `cmd/` binary), behind
[`task perf:scenarios`](../../Taskfile.yml) (manual / CI target — NOT part of
`task test`, same posture as `task bench`):

- [`perf/kpi/`](../../perf/kpi/) — the stdlib-only KPI-capture support: the
  per-scenario metric shape (`ScenarioResult` + `WriteJSON`), the
  allocation/RSS/wall-clock capture bracket (`Capture`), the Linux
  `/proc/self/status` RSS sampler (`rss_linux.go`; a `rss_other.go` no-op
  off-Linux), and the settle-then-count goroutine probe
  (`GoroutinesAfterSettle`). It imports ONLY the standard library — never
  `engine/...` or `internal/...` — so the engine-shaped KPIs (tokens,
  cache-hit-rate) are passed IN by the scenario caller.
- [`perf/scenarios/`](../../perf/scenarios/) — the four whole-loop scenario
  benchmarks (external-test package, imports `engine/...` + the
  `engine/adapter/{mockllm,memfs,memstore,permpolicy}` reference adapters +
  `perf/kpi`, never `internal/...`): `BenchmarkSingleSessionLong` (~500-turn
  tool-using loop), `BenchmarkTeamFanout` (lead + K workers + synthesis),
  `BenchmarkBackgroundSubagents` (M detached children + drain), and
  `BenchmarkCompactionCycle` (history driven past a small `ContextWindowTokens`
  repeatedly). Each records a `kpi.ScenarioResult`; a `TestMain` flushes them to
  `$MECATL_PERF_JSON` when set.
- [`cmd/mecatui/ui/scrollback_bench_test.go`](../../cmd/mecatui/ui/scrollback_bench_test.go)
  — `BenchmarkScrollbackView` (streaming worst case) and
  `BenchmarkScrollbackViewSteady` (unchanged frame), the TUI scrollback render path
  (`refreshView` → `renderConversation`'s join, the profile-confirmed O(scrollback)
  hotspot — now memoized; see the tui-scrollback note above). It is an internal
  `_test` file so it can reach the unexported render path; `perf/kpi` is imported
  ONLY in the test file — the production `ui` package stays free of the perf
  dependency. Its rows MERGE into the same `$MECATL_PERF_JSON` (two metric families,
  one file).

All scenarios are deterministic and offline (`mockllm` + `memfs`/`memstore` +
`permpolicy`, fixed scripts, `time.Unix(0,0)` session epoch, `llm.Reset()` between
iterations) — no network, no live model, no `os/exec`. They are Benchmarks, so the
default `-run` skips them under `task test`; the only `Test*` in each home is a
cheap `TestMain` JSON flush.

### JSON KPI shape (what Phase 3 ingests)

Each row is a `kpi.ScenarioResult` (`schema_version` 1). The full per-field
normalization contract is the doc-comment on `ScenarioResult` in
[`perf/kpi/result.go`](../../perf/kpi/result.go); the points a trend gate must
know:

- **Grouping key:** `(name, git_sha)`. `task perf:scenarios` stamps `git_sha` from
  HEAD (overridable via `MECATL_PERF_SHA`), and `sample` is the per-`name` ordinal
  (0,1,2…) within one run, so `-count=N` emits N groupable rows per scenario.
- **`goroutines_end` is a DELTA**, not a raw count: live goroutines at end-of-run
  minus a baseline taken before the measured region, clamped at 0. **0 = no leak**;
  a positive value is the leak count. Gate it hard on the delegation scenarios
  (`team_fanout`, `background_subagents`) — that is the leak class it guards.
- **`compaction_cycle.cache_hit_rate == 0` is EXPECTED / by-design**: that
  scenario's script carries no cache-read tokens (it exercises the
  compact-and-replace cycle, not prefix caching). A Phase-3 cache-hit-rate gate
  must WHITELIST `compaction_cycle` (and any other no-cache scenario, e.g. the
  token-less `tui_scrollback_view`) rather than false-positive on the honest 0.
  The cache-hit signal is meaningful on `single_session_long` (~0.9) and
  `team_fanout` (~0.75), where the script scripts a cache-read fraction.

The original design sketch (kept for context):

The highest-leverage phase, because it catches the failure modes microbenchmarks
miss (leaks, memory growth, token/cache regressions) and it is **free and offline**:
it reuses the deterministic `mockllm` + `mecademo` path — no network, no cost.

### Scenarios (fixed, scripted, deterministic)

Each scenario is a scripted `mockllm` conversation driven through the real engine:

- **single-session-long** — N turns (e.g. 500) of tool-using loop; targets RSS
  growth + allocation budget over a long session.
- **team-fanout** — a supervisor with K members + synthesis; targets goroutine
  hygiene and per-round allocation under delegation.
- **background-subagents** — M detached children + drain; targets the registry /
  drain / seal path that has leaked before.
- **tui-scrollback** — render a large scrollback through `mecatui`'s `refreshView`
  path; targets the `renderConversation` join (the profile-confirmed O(scrollback)
  hotspot, now memoized — see the tui-scrollback note above). Two variants: a
  streaming worst case (live block mutates every frame) and a steady unchanged frame
  (the join-cache fast path).
- **compaction-cycle** — drive history past the compaction threshold repeatedly;
  targets allocation churn + correctness of the kept tail.

Scenarios live next to the offline driver (candidate home: `internal/app` test
support or a dedicated `perf/scenarios` package that imports only `mockllm` +
composition). They must stay deterministic — fixed mock scripts, injected `Clock`
(already supported), no `Date.now`/random in the path.

### Metrics captured per scenario

- `allocs` and `bytes` total (`runtime.MemStats` / `runtime/metrics` deltas around
  the run, with `runtime.GC()` bracketing).
- RSS sampled over time (peak + final; gopsutil already a transitive dep, or read
  `/proc/self/status` directly to avoid a new dep — decide in spec).
- goroutine count at end-of-run (promote goleak from boolean pass/fail to a *tracked
  number*).
- tokens (input/output) and cache-read tokens from the accumulated `session.Usage`.
- wall-clock (advisory).

### Output format

Emit two shapes from one run:

1. **benchstat-compatible** lines (so the same `benchstat` gate covers micro + macro).
2. **JSON** (so Phase 3's trend store can ingest scenario KPIs that aren't
   `testing.B`-shaped — tokens, cache-hit-rate, RSS-over-time).

### CI wiring (Phase 3 hooks)

Run on PR; compare against the `main` baseline; fail on a significant **allocs /
goroutine / token / cache-hit** regression; report **RSS / ns/op** as advisory
comment. Trend store: `benchmark-action/github-action-benchmark` (free, `gh-pages`)
to start; self-hosted Bencher if/when change-point detection earns its keep.

## Roadmap & the OSS boundary

| Phase | Goal | OSS tools | OSS gap |
|---|---|---|---|
| 0 — KPIs + baselines | Decide what we measure | (a doc) | none |
| 1 — Micro-gating ✅ | Fail a PR that regresses a hot path | `testing.B`, `benchstat` (`task bench`) | none for allocs; trustworthy `ns/op` needs Phase 6 |
| 2 — Scenario harness ✅ | Catch leaks / mem growth / token regressions offline | stdlib `runtime`, `mockllm` driver (`task perf:scenarios`) | none |
| 3 — Trend + history | See regressions over time, not just per-PR | github-action-benchmark (free) or self-hosted Bencher | Bencher's *hosted* analytics + same-bare-metal-local-and-CI is the paid delta |
| 4 — PGO | Free 2–14% CPU; give the profile archive a job | the Go toolchain | none — PGO is entirely OSS |
| 5 — Continuous profiling | Always-on queryable fleet profiles | Pyroscope / Parca + Grafana | *operational only* — see below; defer until `mecated`-as-a-service |
| 6 — Stable wall-clock gating | Make `ns/op` gating reliable in CI | self-hosted runner + `perflock` | the **hardware**, not the software |

### What OSS genuinely can't give us

Almost nothing here is a software-capability gap; everything that *catches a
regression* has a complete OSS path. The honest paid-only advantages are
operational:

1. **A stable bare-metal bench environment without owning/operating hardware.**
   (Bencher Cloud's signature feature: identical bare metal local + CI.) OSS gives
   `perflock` + a self-hosted runner, but we buy and babysit the box. The most
   material gap — and only relevant if `ns/op` gating becomes load-bearing, which our
   allocs/RSS/token KPIs are designed to avoid.
2. **Zero-ops hosting / retention / scaling for continuous profiling.**
   Pyroscope/Parca are fully capable self-hosted; Grafana Cloud Profiles / Polar
   Signals Cloud / Datadog just run the storage for you. Pure ops-offload, irrelevant
   at our scale.
3. **Polished managed regression analytics + PR UX** (Bencher Cloud / Datadog wrap
   change-point detection, alerting, dashboards as a product). OSS engines exist; we
   assemble them.
4. **Support / SLAs / SSO / multi-tenant team features.**

What money buys is **removing ops and hardware burden, not new abilities.**

### Suggested ordering (solo dev)

- **Do now** (free, offline, catches our real bugs): 0 → 2 → 1 → 3 (gh-pages route).
- **Do soon** (free win): 4 (PGO).
- **Defer until justified:** 5 (only when `mecated` is a real long-lived service),
  6 (only if `ns/op` gating becomes load-bearing).

## Open decisions

Resolved in the Phase 2 build:

- **RSS sampling — RESOLVED: direct `/proc/self/status` read.** No new dependency;
  Linux-only (CI + dev are Linux), with a no-op off-Linux fallback so the harness
  still builds and runs everywhere (RSS just reports 0). Lives in
  [`perf/kpi/rss_linux.go`](../../perf/kpi/rss_linux.go) /
  [`rss_other.go`](../../perf/kpi/rss_other.go).
- **Scenario-harness home — RESOLVED: `perf/scenarios` + a `cmd/mecatui/ui` bench,
  `testing.B`-driven, no `cmd/` binary.** The scenarios are external-test
  Benchmarks under [`perf/scenarios/`](../../perf/scenarios/) over a stdlib-only
  [`perf/kpi/`](../../perf/kpi/) support package; the TUI render bench lives next to
  the code it measures as an internal `_test` file. No test-support under
  `internal/app` (it would entangle the scenarios with composition and break the
  "no `internal/...`" rule the offline harness keeps).
- **Trend store — RESOLVED: same `gh-pages` store, two metric families.** The
  scenario JSON ($MECATL_PERF_JSON) and the TUI render bench MERGE into one file,
  ready to feed a single trend store as two families (Phase 3 wiring still TBD).

Still open:

- Trend tooling: start github-action-benchmark; revisit Bencher only when false
  positives from threshold gating become a real cost.
