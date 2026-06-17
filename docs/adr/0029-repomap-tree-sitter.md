# ADR 0029 — Repo-map Tree-sitter Binding

- Status: Superseded
- Date: 2026-06-06
- Scope: the (removed) Aider-style repo-map tool and its WASM tree-sitter extraction mechanism

## Context

The repo-map tool relied on a WASM tree-sitter binding running on wazero. The binding leaked unrecoverably (~23 MB per session) and hung after roughly 160 files due to per-file handles that were never freed. Because mecatui hosts the engine in-process, the hang starved the Bubble Tea render loop and froze the whole program. The tool was already gated off by default.

## Decision

Rather than rework the extraction mechanism (options ranged from forking the dormant binding, to subprocess isolation, to a CGO binding, to a pure-Go redesign), the RepoMap tool and its tree-sitter dependency were removed entirely. The cost of carrying a broken, default-off tool plus its heavy dependency outweighed its value. A repo-map capability may return from a clean design but not by re-enabling this code.

## Consequences

Removed in commit a6a9229 on 2026-06-06. The entire repomap adapter package, the enable-repomap flag, the app.Config field, and the catalog registration were deleted. The github.com/malivvan/tree-sitter dependency was removed; github.com/tetratelabs/wazero was retained because it has another live consumer. This ADR is superseded in the sense that the feature no longer exists; the investigation below is the historical record that informed the removal decision.

---

> ## RESOLUTION — RETIRED / REMOVED (2026-06-06)
>
> **The RepoMap tool and the `github.com/malivvan/tree-sitter` dependency have been
> removed entirely.** Rather than rework the extraction, we retired the feature.
>
> **Rationale.** The Aider-style repo-map tool relied on a WASM tree-sitter binding
> (`github.com/malivvan/tree-sitter`, running on `wazero`) that leaks unrecoverably
> (~23 MB/session) and hangs after ~160 files — fatal for the in-process `mecatui`
> host. It was already gated OFF by default behind `--enable-repomap`. We decided
> the cost of carrying a broken, default-off tool plus its heavy dependency
> outweighed its value, so it was removed instead of reworked.
>
> **What was removed (commit `a6a9229`):**
> - the entire `internal/adapter/repomap/` package;
> - the `--enable-repomap` flag (`cmd/mecated`), the `EnableRepoMap` `app.Config`
>   field, and its catalog registration in `internal/app/build.go` /
>   `internal/app/agentdefs.go`;
> - the `github.com/malivvan/tree-sitter` direct dependency (and its now-orphaned
>   transitive deps `andybalholm/brotli`, `xyproto/randomstring`) via `go mod tidy`.
>   NOTE: `github.com/tetratelabs/wazero` was **NOT** dropped — contrary to an
>   earlier assumption it has another live consumer (ToolHive's secrets path →
>   `1password/onepassword-sdk-go` → `extism/go-sdk`), so it remains an indirect dep.
>
> **Reintroduction.** A repo-map capability may return later, but only from a clean
> design (a non-leaking extractor, e.g. a CGO-free pure-Go parser or a reworked
> tree-sitter integration) — not by re-enabling this code. The investigation below
> is preserved as the historical record that informed the removal decision.

---

**State:** RETIRED (removed 2026-06-06). Investigation complete; original mechanism decision was pending when the feature was removed.
**Date:** 2026-06-02 (investigation); 2026-06-06 (retired).
**Scope:** `internal/adapter/repomap/` — the (now removed) Aider-style repo-map tool.

## Summary

The repo-map tool froze the in-process `mecatui` TUI hard — including
keyboard input — when run against this repo. The cause is **not** file size
or over-scanning (an earlier fix, commit `f27c7bf`, addressed those and an
absent-progress UX gap, but did not stop the freeze). The real cause is a
**memory-management defect in the WASM tree-sitter binding**
(`github.com/malivvan/tree-sitter`), which leaks unrecoverably and eventually
hangs. Because `mecatui` hosts the engine **in-process**, the hang — an
uninterruptible, non-preemptible WASM call — starves the UI goroutine and
freezes the whole program.

This is a binding-quality problem, not a repo-map-design problem. The
graph/PageRank/render half of the tool is sound.

## Root cause (reproduced + measured)

The tool parses every discovered source file through a single reused
tree-sitter WASM module (`parseSession`). Three compounding facts:

1. **Per-file handles are never freed.** `parseFile` allocates a `Tree`, a
   `Query`, and a `QueryCursor` per file, but the binding exposes **only**
   `Parser.Close()` — there are no `ts_tree_delete` / `ts_query_delete` /
   `ts_query_cursor_delete` exports in the bundled `ts.wasm`. Those handles
   accumulate in the module's linear memory.
2. **The module hangs after ~160 files.** Once enough leaked handles pile up,
   the next `ts_parser_parse_string` spins forever (uninterruptible — the call
   takes no timeout and no cancellation flag; `ctx` is only checked *between*
   files). Reproduced deterministically: the scan sailed through file 128,
   then hung between 128 and 192. The file it hung on (`internal/adapter/server/team_test.go`,
   22 KB of ordinary Go) **parses in 0.02 s on a fresh session** — i.e. it is
   not the file, it is the accumulated module state. Any file at that index
   hangs.
3. **The module cannot be freed.** The binding exposes no module/runtime
   `Close`, so the leaked memory is never reclaimed. Measured: **~23 MB RSS
   per session, unbounded** (200 sessions → 4.8 GB). Note this means the
   *current* code already leaks ~23 MB on **every** RepoMap call, because its
   one session is never closed either.

Plus the amplifier: **`mecatui` runs the engine in-process** (`embed.Start` →
`app.Build`). wazero-compiled WASM is not async-preemptible by the Go
scheduler, so a spinning parse pins its OS thread and starves the Bubble Tea
render/input loop → total UI freeze, not just a stuck spinner.

### Why the obvious fix (recycle the session every N files) is a trap

Recycling **does** stop the hang — verified, 300/300 files in 1.97 s. But
since the binding cannot `Close` a module, each recycle leaks another ~23 MB.
Recycling every 40 files would leak ~8× per scan, and on a long-lived
`mecated` every RepoMap call would leak ~185 MB permanently. Trading a hang
for an unbounded daemon leak is not acceptable.

## The binding: `github.com/malivvan/tree-sitter`

- Version pinned (`v0.0.2-0.20250125152656-46b39a70b658`) **is** the latest —
  3 commits, all 2025-01-25, dormant ~16 months. 3 stars, single maintainer,
  self-described "pre-release software, expect bugs." License: **MIT**.
- Bundles **22 grammars** (bash, c, cpp, c#, css, cue, dockerfile, elixir,
  elm, go, groovy, hcl, html, java, javascript, kotlin, lua, python, ruby,
  rust, sql, php). `repomap` currently wires up only **3** (go, python,
  javascript — and parses TypeScript/TSX with the *JavaScript* grammar, a
  fidelity hack). So cross-language reach was always available; the leak/hang
  make it moot.
- Surfaces no per-handle free, no module/runtime `Close`, and no parse
  timeout/cancellation.

## Options evaluated

| # | Option | Leak-free | Parse timeout | CGO-free | Cross-language | Governance | Effort |
|---|--------|-----------|---------------|----------|----------------|------------|--------|
| A | **Fork malivvan**, add module `Close()` + recycle | Yes (via close) | No (per-tree leak remains; recycle masks it) | Yes | 22 grammars bundled | Poor — own a fork of a dormant 3★ hobby project | Low (1-line Close) / Moderate (zig WASM rebuild to add `ts_*_delete` exports) |
| B | **Subprocess-isolate** the existing binding (child parses, prints, exits) | Yes (child exit reclaims all) | Yes (kill child) | Yes | same 3 (or 22) | inherits malivvan | Moderate (IPC + child binary) |
| C | **Official CGO binding** `tree-sitter/go-tree-sitter` | Yes (`Tree/Query/Cursor.Close`) | Yes (`ParseWithOptions` + `ProgressCallback`, ctx) | **No** — reverses `d9f44bb` | go/python/ts/**real TSX**/js/rust/java/c as MIT modules | Strong (official org, MIT, maintained) | High — CGO toolchain in every build + CI, cross-compile pain, binary growth |
| D | **`odvcencio/gotreesitter`** — pure-Go (no WASM, no CGO) | Yes (`Tree.Release` + bounded grammar LRU) | Partial (`SetCancellationFlag`/`SetTimeoutMicros`, no `ctx`; watchdog-poll) | Yes | 206 grammars incl real TSX | Weak — single author, young (Feb 2026), but 497★ and active (faster than CGO on their bench) | Moderate (swap binding) |
| E | **Redesign extraction** — Go via stdlib `go/parser`+`go/ast`; other langs via lightweight regex/heuristic extractor; keep `graph.go`/`render.go` | Yes (no native runtime) | Yes (pure Go, ctx-cancellable) | Yes | Go full-fidelity; others crude-but-adequate; trivially extensible | **Strongest** — stdlib for Go, zero new deps | Moderate (rewrite `parse.go`/`lang.go` behind the `parseFile → *fileNode` seam) |
| F | **Disable** RepoMap by default (`--enable-repomap=false`) | n/a (stopgap) | n/a | — | — | — | Trivial |

Key insight (from the redesign analysis): the repo-map's value is the
**PageRank centrality ranking** — "which files the rest of the repo depends on
most" — which Glob/Grep/Read cannot cheaply give a model. The def/ref model is
**name-based string matching**, not scope/type resolution, so it does **not
need a full AST**: it needs per-file top-level symbol signatures + a bag of
referenced identifiers. Tree-sitter is overpowered for that, and is the source
of every problem here.

## Recommendation

**Stopgap now:** flip `EnableRepoMap` to **off by default** (Option F) so the
freeze stops being reachable while the mechanism is reworked.

**Direction:** Option **E** (redesign the extraction). It is the only
CGO-free, leak-free, governance-clean path: Go gets full fidelity from the
**stdlib** (`go/parser`), other languages get a lightweight heuristic
extractor that is sufficient for a damped PageRank ranking, the
`github.com/malivvan/tree-sitter` dependency is **removed entirely**, and the
graph/PageRank/render code is reused verbatim behind the existing
`parseFile → *fileNode` seam. The cost is reduced fidelity for non-Go
languages (no nested defs, some false-positive ref edges) — acceptable for a
"orient me / rank central files" tool.

**If full-AST fidelity for non-Go is required:** the CGO-free contenders both
fail the maturity/governance screen (malivvan: dormant; gotreesitter:
single-author, 4 months old) the same way the ACP Go SDK did. The only
governance-clean tree-sitter is the official **CGO** binding (Option C), whose
price is reversing the deliberate CGO-free decision. Option **D**
(`gotreesitter`) is the best CGO-free tree-sitter if its single-vendor youth
is acceptable. Option **B** (subprocess) keeps today's behavior safely without
new deps but adds an IPC/child-binary path to preserve a mechanism Option E
would otherwise delete.

## Decision

> Superseded 2026-06-06 by the RESOLUTION at the top — E was never executed; the
> feature was removed outright (commit `a6a9229`).

**2026-06-02: F now + E as the rework.**

- **F (stopgap, done):** `EnableRepoMap` flipped to **off by default** in both
  composition roots (`cmd/mecated` flag default, `cmd/mecatui` config). RepoMap
  remains opt-in via `--enable-repomap`. This makes the freeze unreachable in
  the default configuration.
- **E (rework, planned):** replace the tree-sitter extraction. Go via stdlib
  `go/parser`+`go/ast`; other languages via a lightweight heuristic extractor.
  Remove the `github.com/malivvan/tree-sitter` dependency. Reuse `graph.go` /
  `render.go` unchanged behind the `parseFile → *fileNode` seam. Re-enable by
  default once landed and verified.


---

*Part of the [design docs](../design/README.md). Related: [mecatl — Architecture](0004-v1-architecture.md).*
