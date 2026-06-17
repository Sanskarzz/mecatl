# Design docs — lifecycle & conventions

This folder holds mecatl's **design records**: the *why* behind each feature, captured
at a point in time. The lifecycle model below says what each doc is (and is not); the
citation convention after it is what the `docs/lint` gate enforces. The full rationale
is [ADR 0002 — Documentation lifecycle](../adr/0002-documentation-lifecycle.md).

## What these docs are (lifecycle)

Every doc has **one** lifecycle, and one source of truth per fact:

- **Living truth** — how the system works/operates **now**: [`docs/architecture.md`](../architecture.md),
  [`docs/usage.md`](../usage.md), [`docs/tui.md`](../tui.md). Kept current.
- **Status tracker** — what is shipped / in-progress / deferred: [PRODUCTION-READINESS.md](./PRODUCTION-READINESS.md).
  The **only** place mutable status lives.
- **Decision records** — the *why*, frozen at a point in time: the `docs/design/*.md`
  files here, and [`docs/adr/`](../adr/) for new decisions.
- **Research** — point-in-time studies (`*RESEARCH*.md`, plus [`docs/harnesses/`](../harnesses/README.md)).

**The rules** (ADR 0002):

1. **Design records are frozen.** A spike captures the rationale when it was written.
   When the feature ships you do **not** rewrite the spike to match the new code — you
   update `architecture.md` (current behaviour) and `PRODUCTION-READINESS.md` (status).
2. **Status lives only in the tracker.** No design doc here carries a `Status:` line
   (the `docs/lint` lifecycle gate fails the build if one does). Status is in
   [PRODUCTION-READINESS.md](./PRODUCTION-READINESS.md).
3. **New design is a new record** — add a [`docs/adr/`](../adr/) entry (copy
   [`template.md`](../adr/template.md)). Supersede an old decision with a new ADR plus a
   `Superseded by:` pointer; never rewrite a frozen record in place.

**The lifecycle banner.** Every design record opens with a one-line banner declaring its
kind. Use exactly one of:

```
> **Design record.** Captured during the <feature> work; the rationale here is frozen.
> Current behaviour: [`docs/architecture.md`](../architecture.md) · shipped/deferred state: [Production Readiness — status & roadmap](./PRODUCTION-READINESS.md). Evolve via a new [ADR](../adr/), not by editing this file.

> **Historical.** <superseded by X / retired on DATE>. Preserved for rationale; not maintained.

> **Research note.** Captured <date>. A point-in-time study, not a description of current code. Frozen.
```

The lifecycle gate (`docs/lint`) requires one of the three bold lead tokens — `Design
record` / `Historical` / `Research note` — near the top of every design doc, and forbids
a `Status:` line anywhere in them (status lives in the tracker, not here).

## The citation convention

These docs cite code a lot. A citation is a load-bearing claim that a file (and
sometimes a symbol) exists where the doc says it does. The engine carve (core
moved from `internal/` to `engine/`) showed how that drifts silently: a doc kept
citing `internal/agent/loop.go` long after the file became `engine/agent/loop.go`, <!-- lint:not-a-citation: illustrative stale path in narrative -->
and nothing complained. `docs/lint` (`CheckCitations`) turns a dead citation into
a CI failure so the docs stay honest. This file is the convention the test
enforces.

## How to cite

- Cite a file as a repo-root-relative path inside backticks: `` `engine/agent/loop.go` ``.
  The path must contain at least one slash and must be a path that resolves from
  the repo root. (In practice that means it starts at a real top-level directory
  such as `engine/`, `internal/`, `contracts/`, `cmd/`, or `docs/`, but the test
  checks resolution, not the directory name: an abbreviated path like
  `grpcdriver/server.go` does NOT resolve from the root, so it is flagged on
  purpose, not exempted.) The slash plus the resolution is what makes it a locator
  the test can verify.
- To cite a symbol, use the explicit pairing `` `pkg/file.go` (`SymbolName`) ``:
  a verifiable file span, a single space, then the symbol in its own backticks
  wrapped in parentheses. The test verifies the symbol is present in the file
  (word-boundary match, so `Save` does not match `SaveRequest`). Nothing else is
  read as a symbol claim; arbitrary prose like "the recordPrompt helper" is never
  parsed.
- Line numbers are non-load-bearing. Write `` `engine/agent/loop.go:677` `` if it
  helps a reader, but the test strips the `:NN` (and `:NN-MM`, `:NN,MM`) suffix
  and never verifies it. Lines drift constantly; the file path and the symbol are
  the durable anchors.

## What the test does and does not check

- It verifies file existence for slash-bearing repo-root-relative paths, and
  symbol presence for the `(Symbol)` form. That is all.
- A bare basename is a back-reference, not a citation, and is left alone: spans
  like `` `Save` ``, `` `service.go:868` ``, `` `task test` ``, or `` `omitempty` ``
  have no slash, so the test ignores them. Use the short form freely in prose once
  the full path has been cited nearby; it reads better and the test will not chase
  it.
- A known-extension path is one ending in `.go`, `.proto`, `.yaml`/`.yml`, or
  `.md`. Other extensions (`.sh`, `.json`, ...) are not treated as code citations.
- When a citation goes missing, the failure names the doc and the dead span, and
  for a moved file it suggests the new location if exactly one file with the same
  basename exists elsewhere (the engine-carve catch).
- The test verifies that the citations a doc DOES make resolve; it does NOT
  verify completeness. A resource that exists in code but has no inventory row in
  `CLOUD-NATIVE.md` (or any other claim a doc simply fails to make) is invisible
  to this test. Catching a missing row stays the job of the per-phase re-audit
  (`CLOUD-NATIVE.md`, Phase plan), not of `CheckCitations`. Don't let the green
  docs test lull the re-audit into lapsing.

## Illustrative paths that are not citations

Sometimes a doc quotes a slash-path that is not a repo file: a skill logical
asset name (`references/api.md`), an example value, a path in another project.
Those should not read as code citations, but the test cannot guess intent. Mark
the line with an inline HTML comment so the exemption is explicit and local. The
canonical form carries a REQUIRED reason:

```
... in the skill namespace (`references/api.md`). <!-- lint:not-a-citation: skill asset name, not a repo file -->
```

Always write the reason: an unexplained exemption is exactly the thing that rots,
because the next reader cannot tell whether it is deliberate or a forgotten dodge.
The bare form (`<!-- lint:not-a-citation -->`, no reason) still technically
matches, so a legacy marker keeps working, but do not write new ones. The token
match is anchored, so a near-miss typo (for example a pluralized
`lint:not-a-citations`) does NOT suppress the check: the citation is still flagged
(fail-safe), and you will see the red test rather than a silently-widened hatch.

The marker exempts every citation on its line. Reach for it only when a span is
genuinely not a repo file. An abbreviated-but-real path (for example
`grpcdriver/server.go` for `internal/adapter/grpcdriver/server.go`) is NOT an
illustrative path: expand it to the full repo-root-relative form rather than
hiding it behind the marker. The test flags the abbreviation on purpose, and the
basename suggestion points the way.

## Scope and widening it

The live guard runs over `docs/design/*.md` **and** the architecture guide
(`docs/architecture.md` + `docs/architecture/*.md`, the living citation-heavy
reference), globbed at run time so a new file in either is covered automatically.
`CLAUDE.md`, the top-level `README`, and the rest of `docs/*.md` are out of scope for
now. To widen it, add a glob to the `patterns` slice in `TestRealDesignDocsCitations`
(`docs/lint/citations_test.go`); the checker itself is path-agnostic.

## The design docs

The index to everything under `docs/design/`, grouped by subsystem. Each entry links
the doc and summarises it in a line. **Status is not shown here** — it lives in
[PRODUCTION-READINESS.md](./PRODUCTION-READINESS.md); each doc's own lifecycle banner
declares whether it is a frozen design record, historical, or research.

### Architecture & implementation

- [ARCHITECTURE.md](./ARCHITECTURE.md) — **HISTORICAL / superseded by the live
  [`docs/architecture.md`](../architecture.md).** The v1 core shape, layering, and
  design rationale; predates multi-provider, the TUI, and the composition layer.
  Preserved for rationale.
- [DRIVERS.md](./DRIVERS.md) — driver seams: ports, the gRPC driver protocol, and
  conformance.
- [IMPLEMENTATION-NOTES.md](./IMPLEMENTATION-NOTES.md) — detailed, per-subsystem
  implementation and status narrative — the "how it was built" reference.
- [STEP-CHAIN.md](./STEP-CHAIN.md) — the v1 implementation step-chain.
- [PRODUCTION-READINESS.md](./PRODUCTION-READINESS.md) — consolidated production-
  readiness status & roadmap tracker.
- [TWELVE-PATTERNS-AUDIT.md](./TWELVE-PATTERNS-AUDIT.md) — twelve agentic-harness
  patterns, audited for pluggability.

### Memory

- [MEMORY-DEFAULTS.md](./MEMORY-DEFAULTS.md) — memory enabled by default on the
  embedded mecatui server (Task 1).
- [MEMORY-TIERING.md](./MEMORY-TIERING.md) — genuine tiered memory, closing the
  tier-0 gap (Task 2).
- [MEMORY-TIER2.md](./MEMORY-TIER2.md) — tier-2 / semantic memory recall:
  assessment + buildable design.
- [SOUL-SPIKE.md](./SOUL-SPIKE.md) — a "soul" for mecatl: persistent identity +
  cross-session user-model.
- [COMPACTION.md](./COMPACTION.md) — conversation compaction: how mecatl
  compresses a conversation.

### Agents & teams

- [AGENT-DEFINITIONS.md](./AGENT-DEFINITIONS.md) — Tier-1 named subagent
  specialists discovered from operator-controlled markdown.
- [AGENT-TEAMS-SPIKE.md](./AGENT-TEAMS-SPIKE.md) — headless agent teams (kernel,
  supervisor, coordination tools).
- [BACKGROUND-SUBAGENTS.md](./BACKGROUND-SUBAGENTS.md) — background subagents +
  per-child cancel over a shared child-run registry.

### Providers & APIs

- [MULTI-PROVIDER.md](./MULTI-PROVIDER.md) — multi-provider / multi-model: one
  process, a provider + model bound per session (Phase 0).
- [OPENAI-RESPONSES-API.md](./OPENAI-RESPONSES-API.md) — OpenAI Responses API for
  a Go agentic coding harness — 2026 implementation brief.

### Performance & diagnostics

- [perf-observability.md](./perf-observability.md) — performance observability:
  problem, approaches, and the decided direction.
- [perf-tracking.md](./perf-tracking.md) — long-term performance & resource
  regression tracking.
- [DIAGNOSTICS.md](./DIAGNOSTICS.md) — diagnostics, audit, and the global-slog
  ban.
- [../perf-measurement-survey.md](../perf-measurement-survey.md) — survey of
  performance-measurement approaches that informed the perf harness.

### Governance & trust

- [GUARDRAILS.md](./GUARDRAILS.md) — operator-tier, LLM-backed tool-content
  inspection (issue #27).
- [ALLOW-ALL-POSTURE.md](./ALLOW-ALL-POSTURE.md) — unattended / allow-all posture
  (the "YOLO mode" question).
- [WORKSPACE-TRUST-SPIKE.md](./WORKSPACE-TRUST-SPIKE.md) — workspace trust
  implementation plan (Phases 0+1+2).
- [SYSTEM-PROMPT-RESEARCH.md](./SYSTEM-PROMPT-RESEARCH.md) — system-prompt research
  & enhancement (issue #19).

### UX

- [UX-DISCOVERABILITY.md](./UX-DISCOVERABILITY.md) — mecatui UX discoverability
  design (Option C: wire capabilities).
- [CLIPBOARD-IMAGE-PASTE.md](./CLIPBOARD-IMAGE-PASTE.md) — clipboard image paste
  (`ctrl+v`) in the mecatui prompt.

### Cloud-native

- [CLOUD-NATIVE.md](./CLOUD-NATIVE.md) — the cloud-native arc: disposable process,
  externalized state, durable record.

### Forge integration

- [MECATEQUI.md](./MECATEQUI.md) — running mecatequi as a single-shot GitHub Action: the
  split-privilege workflow, the token boundary, and the trust model for untrusted issue
  text.

### Decisions (ADRs)

- [../adr/0001-acp-adapter.md](../adr/0001-acp-adapter.md) — the Agent Client Protocol
  (ACP) adapter: why ACP runs over stdio as a driving adapter, and the trust boundary.

### Historical / retired

- [REPOMAP-TREE-SITTER.md](./REPOMAP-TREE-SITTER.md) — repo-map tree-sitter: freeze
  root cause + binding evaluation.

---

*See also: [`docs/architecture.md`](../architecture.md) — the live implementation
reference · [`docs/usage.md`](../usage.md) — the operator guide · [`docs/adr/`](../adr/)
— architecture decision records · [`CLAUDE.md`](../../CLAUDE.md) — the coding-agent
contract.*
