# Production Readiness — status & roadmap

> The **single source of truth for mutable status** (per [ADR 0002](../adr/0002-documentation-lifecycle.md)).
> Design docs record *why* and are frozen; current behaviour lives in
> [`docs/architecture.md`](../architecture.md); shipped/deferred state lives here.
> Goal: a complete, production-ready harness with **no open deferrals** except items
> explicitly marked *Optional feature* (not a production blocker) with a rationale.
> Status legend: ✅ Done · 🔨 In progress · ⛔ Open (to close) · 🟦 Optional feature.

## Design records → status

One row per [design record](./README.md). Status is here; the *why* is in the linked
record; current behaviour is in [`docs/architecture.md`](../architecture.md) at the
noted section. Per-area production checklists follow below.

| Subsystem | Status | Design record | Arch § |
|---|---|---|---|
| Multi-provider / multi-model | ✅ P0+P1+live listing & metadata · ⛔ disk cache (P2) · ⛔ secrets/OAuth/per-client keys (P3) | [MULTI-PROVIDER.md](./MULTI-PROVIDER.md) | §18 |
| OpenAI Responses adapter | ✅ shipped (research brief frozen) | [OPENAI-RESPONSES-API.md](./OPENAI-RESPONSES-API.md) | §9 |
| Agent definitions (Tier-1 specialists) | ✅ shipped · ⛔ per-agent memory write path · ⛔ `local` tier | [AGENT-DEFINITIONS.md](./AGENT-DEFINITIONS.md) | §8 |
| Agent teams (kernel, supervisor, coordination) | ✅ shipped (substrate) · ⛔ mutating-fork join strategies · ⛔ `TeamStore` restart durability | [AGENT-TEAMS-SPIKE.md](./AGENT-TEAMS-SPIKE.md) | §8 |
| Background subagents + per-child cancel | ✅ shipped · ⛔ session-scoped detach (v2) | [BACKGROUND-SUBAGENTS.md](./BACKGROUND-SUBAGENTS.md) | §8 |
| Parallelism — fork-join | ✅ shipped | — | §15 |
| Memory defaults (on-by-default) | ✅ shipped | [MEMORY-DEFAULTS.md](./MEMORY-DEFAULTS.md) | §14 |
| Tiered memory (tier-0 index + BM25) | ✅ tier-0 index + BM25 `SearchMemory` · ⛔ semantic / embedding recall | [MEMORY-TIERING.md](./MEMORY-TIERING.md) · [MEMORY-TIER2.md](./MEMORY-TIER2.md) | §14 |
| Soul / persona + user-model | ✅ Phase 1 + 2a + 2b + Phase 3 items 1–3 | [SOUL-SPIKE.md](./SOUL-SPIKE.md) | — |
| Compaction (heuristic + cascade) | ✅ shipped | [COMPACTION.md](./COMPACTION.md) | §13 |
| System-prompt enhancement | ✅ §7a shipped (`agencyDelta`, tool-discipline hints, `<env>`) | [SYSTEM-PROMPT-RESEARCH.md](./SYSTEM-PROMPT-RESEARCH.md) | — |
| Guardrails (LLM-backed tool-content inspection) | ✅ shipped | [GUARDRAILS.md](./GUARDRAILS.md) | §7 |
| Allow-all / posture ladder | ✅ shipped · ⛔ managed-scope kill-switch · ⛔ `auto`+reviewer posture | [ALLOW-ALL-POSTURE.md](./ALLOW-ALL-POSTURE.md) | §17 |
| Workspace trust | ✅ Phases 0/1/2a/2b/2c · ⛔ Phase 3 (descoped) | [WORKSPACE-TRUST-SPIKE.md](./WORKSPACE-TRUST-SPIKE.md) | §17 |
| Driver seams (remote stores/sources) | ✅ Phases A–C2 · ⛔ workspace/FS driver (sketch only) | [DRIVERS.md](./DRIVERS.md) | §11 |
| Cloud-native arc | ✅ Phases 0–3 · ⛔ Phase 4 (writer exclusion / leasing) | [CLOUD-NATIVE.md](./CLOUD-NATIVE.md) | §11 |
| Diagnostics (injected `port.Diagnostics`) | ✅ shipped | [DIAGNOSTICS.md](./DIAGNOSTICS.md) | §11 |
| Perf observability (live admin/MCP) | ✅ Phases 1+2 · 🟦 Phase 3 (fleet/Pyroscope, optional) | [perf-observability.md](./perf-observability.md) | §11 |
| Perf tracking (offline regression gate) | ✅ Phases 0–4 · ⛔ Phases 5–6 (deferred-until-justified) | [perf-tracking.md](./perf-tracking.md) | §11 |
| UX discoverability (mecatui) | ✅ shipped | [UX-DISCOVERABILITY.md](./UX-DISCOVERABILITY.md) | — |
| Clipboard image paste (mecatui `ctrl+v`) | ✅ shipped | [CLIPBOARD-IMAGE-PASTE.md](./CLIPBOARD-IMAGE-PASTE.md) | — |
| mecatequi (single-shot GitHub Action) | ✅ shipped (v1 forge glue) | [MECATEQUI.md](./MECATEQUI.md) | §1 |
| _Historical / retired_ | — | [ARCHITECTURE.md](./ARCHITECTURE.md) · [STEP-CHAIN.md](./STEP-CHAIN.md) · [TWELVE-PATTERNS-AUDIT.md](./TWELVE-PATTERNS-AUDIT.md) · [REPOMAP-TREE-SITTER.md](./REPOMAP-TREE-SITTER.md) | — |

## Security

| Item | Status | Notes / definition of done |
|---|---|---|
| Permission gate (deny→ask→allow, scopes, compound-bash, plan mode) | ✅ | `governance` + `permpolicy`, tested |
| Bash gate substitution/newline-safe | ✅ | hardened post-review |
| Workspace path-escape containment | ✅ | `osfs` via `os.Root` |
| Model-based layer-2 risk classifier | ✅ | `permclassify` (opt-in, monotonic, fail-safe) |
| Hooks (full lifecycle fired, exit 0/2) | ✅ | all 6 phases fire |
| **API authentication + rate limiting** | ✅ | bearer (`--auth-token`/`MECATL_AUTH_TOKEN`, constant-time) + optional TLS/mTLS (`--tls-cert`/`--tls-key`/`--client-ca`) gRPC interceptors + HTTP middleware; per-client + global token-bucket rate limit (`--rate-limit`/`--rate-burst`, bounded/idle-evicting); off-loopback-no-auth WARNING (`internal/adapter/server/authn.go`) |
| OS-level sandbox (process trust) | ⏸️ Deferred | Explicitly deferred (2026-05-29). The `CommandRunner` port is the seam; a Landlock(+seccomp) wrapper drops in later without touching the loop. Bash is also fully optional (shell-less deploys avoid the surface entirely), so this is not a blocker for those. |
| Secrets handling (no key logging) | ✅ | key via env, never logged |
| MCP transport restriction (no stdio) | ✅ | streaming-HTTP only |

## Reliability

| Item | Status | Notes |
|---|---|---|
| Stop conditions (turns/tool-calls/failures) | ✅ | enforced + default limits |
| Provider retry/backoff + circuit breaker | ✅ | `llmresilience` |
| Provider error surfaced to client | ✅ | `ResultPayload.Error` |
| **Auto-resume persisted sessions after restart** | ✅ | `GetSession`/`Approve`/`Cancel` fall back to `SessionStore.Load`; persist at create, on entering `awaiting`, and at run end (engine `Store` + `Service.Persist`). With `--store-dir` (jsonlstore) a session survives restart and is loadable. Boundary: an in-flight *stream* is NOT resumed across restart (the `*agent.Run` is in-memory). Since cloud-native Phase 2, an `Approve` against a runless-but-stored session that died while `awaiting` **re-enters the loop at the ask** (`Service.resumeFromAwaiting`); `ErrNoActiveRun` (HTTP 409 / gRPC FailedPrecondition) is returned only for `Approve` against a non-awaiting state and for `Cancel` against any runless session. See `CLOUD-NATIVE.md` Phase 2 |
| Graceful shutdown | ✅ | gRPC GracefulStop + HTTP Shutdown |

## Observability

| Item | Status | Notes |
|---|---|---|
| Prometheus metrics + `/metrics` | ✅ | `telemetry` |
| Per-tool logging + JSONL replay | ✅ | `ToolCallRecorder` (the port formerly named `Logger`) + `jsonlstore` |
| OTel span model (run/turn/tool) | ✅ | `telemetry` |
| OTLP exporter wiring | ✅ | `telemetry.Setup` builds/installs an OTLP TracerProvider; wired in `mecated` via `--otlp-endpoint`/`--otlp-protocol`/`--otlp-insecure` (no-op when empty) |
| **Health endpoints** (`/healthz`,`/readyz`, gRPC health) | ✅ | HTTP `/healthz` (liveness) + `/readyz` (readiness) mounted outside auth/rate-limit; standard `grpc_health_v1` SERVING (`internal/adapter/server/health.go`). `deploy/` can switch TCP→httpGet probes |

## Context management

| Item | Status | Notes |
|---|---|---|
| Compaction seam (`Compactor`) | ✅ | pluggable |
| Single-summary heuristic compaction | ✅ | default |
| Real tokenizer + tiered compaction cascade | ✅ | `TokenCounter` seam (heuristic default + offline `tiktoken` adapter); `CascadeCompactor` snip→strip→collapse→summarize behind the `Compactor` seam, with trigger/target hysteresis (0.8/0.6). Opt-in via `--compaction=cascade`/`--tokenizer=tiktoken`; defaults unchanged |

## Harness patterns (12) — pluggability

| Pattern | Status | Notes |
|---|---|---|
| 1 persistent instructions, 6 plan/act, 7 subagents, 11 single-purpose tools | ✅ | complete + pluggable |
| 2 scoped context assembly | ✅ | `InstructionAssembler` seam (default root) |
| 9 progressive tool disclosure | ✅ | `Disclosable`+`ToolSearch` seam (default off) |
| 10 command risk classification | ✅ | layer-1 rules + layer-2 `permclassify` |
| 12 lifecycle hooks | ✅ | all phases fire |
| 5 progressive compaction | ✅ | `Compactor` seam + `HeuristicCompactor` (default) and `CascadeCompactor` (tiered) |
| 8 fork-join parallelism | ✅ | `tool.WorkspaceForker` + `internal/adapter/forker` (git-worktree/copy isolation) + `agent.NewParallelTool` (parallel isolated branches, join); wired in `mecated` (`--enable-parallel`) |
| **3 tiered memory** | ✅ | `tool.MemoryStore` seam + file-backed `internal/adapter/memory` (Remember/Recall/SearchMemory tools, per-project, conservative descriptions); ON by default per-project (`MEMORY-DEFAULTS.md`) — only consolidation stays opt-in |
| 4 dream/sleep consolidation | ✅ | `internal/adapter/dream` — conservative MemoryStore+LLM consolidator (merge dupes / drop stale, never invents keys, fail-safe), `RunPeriodically`; opt-in via `--memory-consolidate-interval` |

## Deployment

| Item | Status | Notes |
|---|---|---|
| ko build + PSS-restricted manifests | ✅ | `.ko.yaml`, `deploy/` |
| Health probes in manifests | ✅ | `deploy/deployment.yaml` uses `httpGet` probes against `/healthz` (liveness) and `/readyz` (readiness); see `deploy/README.md` |
| Config file (vs flags only) | ✅ | file-based PERMISSION config shipped (issue #13): `internal/adapter/permconfig` loads `.mecatl/settings.yaml` (+ imports Claude-Code `settings.json`), RE-RESOLVED PER SESSION against each session's workspace root via a `permpolicy.RuleResolver`. Tiered scopes (project < user), trust-gated project allows (`--trust-project`), conventional discovery (`--permissions-conventional`, ON), Claude import (`--import-claude-permissions`), explicit files (`--permission-config`). Broader (non-permission) config-file surface remains flags-only |

## Other / future features (Optional — not production blockers)

| Item | Status | Rationale |
|---|---|---|
| Multi-vendor model routing | ✅ | SHIPPED — server-side provider registry + native Anthropic Messages adapter + embedded models.dev catalog + OpenRouter, with per-session provider/model routing and capability intersection. See `docs/design/MULTI-PROVIDER.md` |
| Repo map (tree-sitter PageRank) | ❌ removed | The Aider-style repo-map tool was **retired and removed** — its WASM tree-sitter binding leaked (~23 MB/session) and hung after ~160 files. See `docs/design/REPOMAP-TREE-SITTER.md`. May return later from a clean design |
| Slash commands | ✅ | `prompt.CommandExpander` + `DirCommandExpander` (`.mecatl/commands`/`.claude/commands` templates); `--commands-dir`/`--enable-commands`. (Skills since shipped too: Skill/SkillDraft tools + the `engine/tool` `SkillSource` port + the `/skills` browser.) |
| Live OpenAI validation | ✅ | validated against Sonnet 4.5 via OpenRouter (full tool-calling loop) |
| Fuzz tests (bash splitter, SSE decoder) | ✅ | native Go fuzzers + Taskfile `fuzz` target; security invariants asserted; no crashers found |

## Close-out plan (waves)

All waves bar #2 are complete; #2 (OS sandbox) is the one deliberately-deferred item.

1. ✅ **Server hardening** — auth + rate limit + health endpoints + auto-resume.
2. ⏸️ **OS sandbox** — Landlock(+seccomp) `CommandRunner` adapter. *(deferred — the `CommandRunner` seam is in place; Bash is also fully optional.)*
3. ✅ **Context** — tokenizer + compaction cascade.
4. ✅ **Observability** — OTLP exporter wiring; **fuzz** the parsers.
5. ✅ **Patterns** — fork-join (8); tiered memory (3) [+ optional dream (4)].
6. ✅ **Panel review** each wave; final gauntlet + live e2e.

Optional features (🟦) are left as documented seams unless requested.


---

*Part of the [design docs](./README.md). Related: [TWELVE-PATTERNS-AUDIT.md](./TWELVE-PATTERNS-AUDIT.md), [ARCHITECTURE.md](./ARCHITECTURE.md).*
