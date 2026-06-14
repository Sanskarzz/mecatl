# Long-term performance & resource regression tracking

Status: **Phase 1 shipped; the rest research / proposal.** Phase 0 (decide what we
track) is settled below, and Phase 1 (hot-path microbenchmarks + a `task bench`
gate-feed) is built — see [Phase 1 — Status](#phase-1--status). The scenario
harness (Phase 2) and trend store (Phase 3+) remain design sketches. Sibling to
[`perf-observability.md`](perf-observability.md), which covers the *introspection*
half (pprof, flight recorder, OTel metrics, the perf MCP, `--perf`, goleak).

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
| `allocs/op` on hot paths | Leading indicator of GC pressure; deterministic regardless of machine load | very low | **hard** | _TBD_ — fill via `task bench` (Phase 1) |
| Total allocations per offline scenario | Whole-loop allocation budget | very low | **hard** | _TBD_ — Phase 2 scenario harness |
| RSS-over-time on a fixed scenario | Long-session / scrollback growth (e.g. the `SetContent` O(scrollback) follow-up) | low | soft → hard | _TBD_ |
| End-of-run goroutine count | Delegation / team leak class we have hit before | deterministic | **hard** | _TBD_ |
| Tokens per scenario (input/output) | The dominant cost of an LLM harness | deterministic (offline) | **hard** | _TBD_ |
| Prompt-cache-hit-rate per scenario | A prefix-stability regression silently doubles cost and no CPU/mem bench catches it | deterministic (offline) | **hard** | _TBD_ |
| `ns/op` on hot paths | Classic latency | high in shared CI | advisory | _TBD_ |
| Peak RSS per scenario | OOM headroom / `GOMEMLIMIT` budgeting | low | soft | _TBD_ |

Baselines are filled by running Phase 1 + Phase 2 once on `main` and pasting the
numbers (or, better, by letting the trend store in Phase 3 own them). Until then the
cells stay `_TBD_` — do not invent numbers.

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

## Phase 2 — Offline scenario harness (design sketch)

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
- **tui-scrollback** — render a large scrollback through `mecatui`'s `View` path;
  targets the `SetContent` O(scrollback) class.
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
| 2 — Scenario harness | Catch leaks / mem growth / token regressions offline | stdlib `runtime`, goleak, `mockllm` driver | none |
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

- RSS sampling: reuse gopsutil (already transitive) vs read `/proc/self/status`
  directly (no new dep, Linux-only). Lean: direct read, since CI + dev are Linux.
- Scenario-harness home: `perf/scenarios` package vs test-support under `internal/app`.
- Whether to fold scenario JSON KPIs into the same `gh-pages` trend store or a second
  series. Lean: same store, two metric families.
- Trend tooling: start github-action-benchmark; revisit Bencher only when false
  positives from threshold gating become a real cost.
