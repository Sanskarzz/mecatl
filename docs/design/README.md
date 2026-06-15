# Design docs: the citation convention

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

The live guard runs over `docs/design/*.md` only (globbed at run time, so a new
design doc is covered automatically). `CLAUDE.md`, the top-level `README`, and the
rest of `docs/*.md` are out of scope for now. To widen it, add a glob to the
`patterns` slice in `TestRealDesignDocsCitations` (`docs/lint/citations_test.go`);
the checker itself is path-agnostic.

## The design docs

The index to everything under `docs/design/`, grouped by subsystem. Each entry
links the doc, summarises it in a line, and notes its status where the doc records
one.

### Architecture & implementation

- [ARCHITECTURE.md](./ARCHITECTURE.md) — mecatl's architecture: the v1 core
  shape, layering, and design rationale. *Status: historical design + rationale.*
- [DRIVERS.md](./DRIVERS.md) — driver seams: ports, the gRPC driver protocol, and
  conformance. *Status: shipped (driver-seams arc, Phases A–C2).*
- [IMPLEMENTATION-NOTES.md](./IMPLEMENTATION-NOTES.md) — detailed, per-subsystem
  implementation and status narrative — the "how it was built" reference.
- [STEP-CHAIN.md](./STEP-CHAIN.md) — the v1 implementation step-chain.
  *Status: historical — the v1 implementation plan, fully executed.*
- [PRODUCTION-READINESS.md](./PRODUCTION-READINESS.md) — consolidated production-
  readiness status & roadmap tracker.
- [TWELVE-PATTERNS-AUDIT.md](./TWELVE-PATTERNS-AUDIT.md) — twelve agentic-harness
  patterns, audited for pluggability.

### Memory

- [MEMORY-DEFAULTS.md](./MEMORY-DEFAULTS.md) — memory enabled by default on the
  embedded mecatui server (Task 1).
- [MEMORY-TIERING.md](./MEMORY-TIERING.md) — genuine tiered memory, closing the
  tier-0 gap (Task 2). *Status: design only.*
- [MEMORY-TIER2.md](./MEMORY-TIER2.md) — tier-2 / semantic memory recall:
  assessment + buildable design. *Status: BM25 lexical search shipped.*
- [SOUL-SPIKE.md](./SOUL-SPIKE.md) — a "soul" for mecatl: persistent identity +
  cross-session user-model. *Status: Phase 1 + Phase 2 (2a + 2b) + Phase 3 (Items
  1–3) shipped.*
- [COMPACTION.md](./COMPACTION.md) — conversation compaction: how mecatl
  compresses a conversation. *Status: shipped.*

### Agents & teams

- [AGENT-DEFINITIONS.md](./AGENT-DEFINITIONS.md) — Tier-1 named subagent
  specialists discovered from operator-controlled markdown.
- [AGENT-TEAMS-SPIKE.md](./AGENT-TEAMS-SPIKE.md) — headless agent teams (kernel,
  supervisor, coordination tools). *Status: shipped (the substrate).*
- [BACKGROUND-SUBAGENTS.md](./BACKGROUND-SUBAGENTS.md) — background subagents +
  per-child cancel over a shared child-run registry. *Status: shipped.*

### Providers & APIs

- [MULTI-PROVIDER.md](./MULTI-PROVIDER.md) — multi-provider / multi-model: one
  process, a provider + model bound per session (Phase 0).
- [OPENAI-RESPONSES-API.md](./OPENAI-RESPONSES-API.md) — OpenAI Responses API for
  a Go agentic coding harness — 2026 implementation brief.

### Performance & diagnostics

- [perf-observability.md](./perf-observability.md) — performance observability:
  problem, approaches, and the decided direction. *Status: Approach D — shipped
  (Phase 1 + Phase 2 complete).*
- [perf-tracking.md](./perf-tracking.md) — long-term performance & resource
  regression tracking. *Status: Phases 1–4 shipped; Phases 5–6 deferred.*
- [DIAGNOSTICS.md](./DIAGNOSTICS.md) — diagnostics, audit, and the global-slog
  ban. *Status: shipped (logging-architecture refactor, iterations 1–3).*

### Governance & trust

- [GUARDRAILS.md](./GUARDRAILS.md) — operator-tier, LLM-backed tool-content
  inspection (issue #27).
- [ALLOW-ALL-POSTURE.md](./ALLOW-ALL-POSTURE.md) — unattended / allow-all posture
  (the "YOLO mode" question). *Status: shipped.*
- [WORKSPACE-TRUST-SPIKE.md](./WORKSPACE-TRUST-SPIKE.md) — workspace trust
  implementation plan (Phases 0+1+2). *Status: feature complete.*
- [SYSTEM-PROMPT-RESEARCH.md](./SYSTEM-PROMPT-RESEARCH.md) — system-prompt research
  & enhancement (issue #19). *Status: §7a enhancement plan implemented.*

### UX

- [UX-DISCOVERABILITY.md](./UX-DISCOVERABILITY.md) — mecatui UX discoverability
  design (Option C: wire capabilities). *Status: implemented — Phases A+B shipped.*
- [CLIPBOARD-IMAGE-PASTE.md](./CLIPBOARD-IMAGE-PASTE.md) — clipboard image paste
  (`ctrl+v`) in the mecatui prompt.

### Cloud-native

- [CLOUD-NATIVE.md](./CLOUD-NATIVE.md) — the cloud-native arc: disposable process,
  externalized state, durable record. *Status: Phase 0 deliverable.*

### Forge integration

- [MECATEQUI.md](./MECATEQUI.md) — running mecatequi as a single-shot GitHub Action: the
  split-privilege workflow, the token boundary, and the trust model for untrusted issue
  text. *Status: v1 forge glue (composite action + template workflow + docs).*

### Historical / retired

- [REPOMAP-TREE-SITTER.md](./REPOMAP-TREE-SITTER.md) — repo-map tree-sitter: freeze
  root cause + binding evaluation. *Status: retired (removed 2026-06-06).*
