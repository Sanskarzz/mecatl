# Production Readiness — status & roadmap

> Consolidated tracker for everything previously deferred. Goal: a complete,
> production-ready harness with **no open deferrals** except items explicitly
> marked *Optional feature* (not a production blocker) with a rationale.
> Status legend: ✅ Done · 🔨 In progress · ⛔ Open (to close) · 🟦 Optional feature.

## Security

| Item | Status | Notes / definition of done |
|---|---|---|
| Permission gate (deny→ask→allow, scopes, compound-bash, plan mode) | ✅ | `governance` + `permpolicy`, tested |
| Bash gate substitution/newline-safe | ✅ | hardened post-review |
| Workspace path-escape containment | ✅ | `osfs` via `os.Root` |
| Model-based layer-2 risk classifier | ✅ | `permclassify` (opt-in, monotonic, fail-safe) |
| Hooks (full lifecycle fired, exit 0/2) | ✅ | all 6 phases fire |
| **API authentication + rate limiting** | ✅ | bearer (`--auth-token`/`OZZ_AUTH_TOKEN`, constant-time) + optional TLS/mTLS (`--tls-cert`/`--tls-key`/`--client-ca`) gRPC interceptors + HTTP middleware; per-client + global token-bucket rate limit (`--rate-limit`/`--rate-burst`, bounded/idle-evicting); off-loopback-no-auth WARNING (`server/authn.go`) |
| OS-level sandbox (process trust) | ⏸️ Deferred | Explicitly deferred (2026-05-29). The `CommandRunner` port is the seam; a Landlock(+seccomp) wrapper drops in later without touching the loop. Bash is also fully optional (shell-less deploys avoid the surface entirely), so this is not a blocker for those. |
| Secrets handling (no key logging) | ✅ | key via env, never logged |
| MCP transport restriction (no stdio) | ✅ | streaming-HTTP only |

## Reliability

| Item | Status | Notes |
|---|---|---|
| Stop conditions (turns/tool-calls/failures) | ✅ | enforced + default limits |
| Provider retry/backoff + circuit breaker | ✅ | `llmresilience` |
| Provider error surfaced to client | ✅ | `ResultPayload.Error` |
| **Auto-resume persisted sessions after restart** | ✅ | `GetSession`/`Approve`/`Cancel` fall back to `SessionStore.Load`; persist at create, on entering `awaiting`, and at run end (engine `Store` + `Service.Persist`). With `--store-dir` (jsonlstore) a session survives restart and is loadable. Boundary: an in-flight *stream* is NOT resumed across restart (the `*agent.Run` is in-memory); an `Approve`/`Cancel` against a runless-but-stored session returns `ErrNoActiveRun` (HTTP 409 / gRPC FailedPrecondition) |
| Graceful shutdown | ✅ | gRPC GracefulStop + HTTP Shutdown |

## Observability

| Item | Status | Notes |
|---|---|---|
| Prometheus metrics + `/metrics` | ✅ | `telemetry` |
| Per-tool logging + JSONL replay | ✅ | `Logger` + `jsonlstore` |
| OTel span model (run/turn/tool) | ✅ | `telemetry` |
| OTLP exporter wiring | ✅ | `telemetry.Setup` builds/installs an OTLP TracerProvider; wired in `ozzd` via `--otlp-endpoint`/`--otlp-protocol`/`--otlp-insecure` (no-op when empty) |
| **Health endpoints** (`/healthz`,`/readyz`, gRPC health) | ✅ | HTTP `/healthz` (liveness) + `/readyz` (readiness) mounted outside auth/rate-limit; standard `grpc_health_v1` SERVING (`server/health.go`). `deploy/` can switch TCP→httpGet probes |

## Context management

| Item | Status | Notes |
|---|---|---|
| Compaction seam (`Compactor`) | ✅ | pluggable |
| Single-summary heuristic compaction | ✅ | default |
| **Real tokenizer + tiered compaction cascade** | ⛔ | replace 4-chars/token estimate; snip→strip→collapse→summarize behind `Compactor` |

## Harness patterns (12) — pluggability

| Pattern | Status | Notes |
|---|---|---|
| 1 persistent instructions, 6 plan/act, 7 subagents, 11 single-purpose tools | ✅ | complete + pluggable |
| 2 scoped context assembly | ✅ | `InstructionAssembler` seam (default root) |
| 9 progressive tool disclosure | ✅ | `Disclosable`+`ToolSearch` seam (default off) |
| 10 command risk classification | ✅ | layer-1 rules + layer-2 `permclassify` |
| 12 lifecycle hooks | ✅ | all phases fire |
| 5 progressive compaction | 🔨 | seam ✅; cascade impl under Context management above |
| 8 fork-join parallelism | ✅ | `tool.WorkspaceForker` + `internal/adapter/forker` (git-worktree/copy isolation) + `agent.NewForkTool` (parallel isolated branches, join); wired in `ozzd` (`--enable-fork`) |
| **3 tiered memory** | ✅ | `tool.MemoryStore` seam + file-backed `internal/adapter/memory` (Remember/Recall tools, per-project, conservative descriptions); opt-in via `memory.Register` |
| 4 dream/sleep consolidation | 🟦 | depends on (3); optional background consolidation |

## Deployment

| Item | Status | Notes |
|---|---|---|
| ko build + PSS-restricted manifests | ✅ | `.ko.yaml`, `deploy/` |
| Health probes in manifests | 🔨 | `/healthz`+`/readyz`+gRPC health now exist; `deploy/` can switch TCP→httpGet (manifest edit pending) |
| Config file (vs flags only) | 🟦 | flags suffice for v1; add if operators ask |

## Other / future features (Optional — not production blockers)

| Item | Status | Rationale |
|---|---|---|
| Multi-vendor model routing | 🟦 | `LLMProvider` port already abstracts it; a router is a convenience adapter |
| Repo map (tree-sitter PageRank) | 🟦 | a future read-only tool; agentic grep works at current scale |
| Slash commands / skills packaging | 🟦 | prompt volatile-suffix seam exists |
| Live OpenAI validation | ✅ | validated against Sonnet 4.5 via OpenRouter (full tool-calling loop) |
| Fuzz tests (bash splitter, SSE decoder) | ✅ | native Go fuzzers + Taskfile `fuzz` target; security invariants asserted; no crashers found |

## Close-out plan (waves)

1. **Server hardening** — auth + rate limit + health endpoints + auto-resume.
2. **OS sandbox** — Landlock(+seccomp) `CommandRunner` adapter.
3. **Context** — tokenizer + compaction cascade.
4. **Observability** — OTLP exporter wiring; **fuzz** the parsers.
5. **Patterns** — fork-join (8); tiered memory (3) [+ optional dream (4)].
6. **Panel review** each wave; final gauntlet + live e2e.

Optional features (🟦) are left as documented seams unless requested.
