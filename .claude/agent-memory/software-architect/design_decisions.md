---
name: design-decisions
description: Locked architecture judgment calls for mecatl v1 (gRPC framework, state model, edit format, event model), plus as-built layering facts.
metadata:
  type: project
---

Judgment calls made in the v1 architecture. Each has a one-line rationale; revisit only with a stated forcing function.

- **gRPC framework: grpc-go + buf + protovalidate** (revised from connect-go; ARCHITECTURE §7). Why: mirrors a downstream consumer house style and its bidi `Converse` pattern — the approval frame returns on the same stream emitting events, no out-of-band correlation. Proto source of truth in `contracts/proto`, generated Go in `contracts/gen/go`. HTTP is a hand-rolled SSE adapter (grpc-gateway cannot map bidi), not connect auto-mapping.
- **Server-side conversation state.** Why: permission "ask" pause/resume and cancellation require the harness to own the loop across requests. SessionStore persists Sessions; client holds only a session_id.
- **Edit format: exact-match SEARCH/REPLACE (old_string/new_string + replace_all).** Why: doc 08 pragmatic default for v1; the three Edit invariants depend on exact match.
- **Event model is domain-owned, not provider-owned.** The loop yields domain `session.Event`; the LLMProvider port yields provider-neutral `port.Chunk` the loop translates. Verified: no OpenAI type escapes `adapter/openai`.
- **Read-only parallel / mutating serial enforced in the loop's tool-dispatch stage** (`agent/dispatch.go`), keyed off `tool.Tool.ReadOnly()`. Verified as-built: maximal RO batches run concurrent, mutating tools run alone.

As-built layering facts (verified by import audit, all green build+vet):
- `agent` imports only domain + `port` (no adapter, no api). Clean.
- `governance` imports NOTHING from internal — fully session-free. `permpolicy` adapter is the anti-corruption seam translating `session.ToolCall`→governance. Correct ACL.
- `port` imports `session`, `prompt`, `tool`, `governance` (return/param types). `FileSystem`/`Workspace` live in `internal/tool` (not `port`) to break a port↔tool cycle — documented at top of `port/doc.go` and `tool/tool.go`.
- `session` imports `governance` (for `ResumeWith(governance.PermissionDecision)` param) — the one domain→domain cross-context edge.
- Shared Engine/Run/Service seam: both gRPC `Converse` and HTTP/SSE wrap one `server.Service` over one `agent.Run`. Not reinvented per surface.

As-built beyond the v1 "production core" (verified 2026-05-29, contra the non-goals note in [[project-mecatl]]): an MCP client adapter EXISTS (`adapter/mcp`, Streamable-HTTP only, no stdio, tools namespaced `mcp__<server>__<tool>`, eager-registered into the catalog) and an LLM resilience decorator EXISTS (`adapter/llmresilience.Wrap` — retries/timeout/circuit-breaker over `port.LLMProvider`; the house decorator idiom). So "MCP client" is no longer a non-goal.

As-built seams (verified 2026-05-29, the audit's work packages are now IMPLEMENTED):
- **House decorator idiom**: `llmresilience.Wrap(port.LLMProvider, Config)` and `permclassify.Wrap(port.PermissionPolicy, Config)` both decorate-and-return the SAME port the loop consumes — composable, loop-untouched, opt-in at the composition root. `permclassify` enforces a documented MONOTONICITY contract (never relaxes an inner Deny). Correct direction.
- **`internal/tool` now hosts FOUR seams** (`CommandRunner`, `MemoryStore`, `WorkspaceForker`, plus the `Disclosable` capability iface) alongside `FileSystem`/`Workspace`. All justified by the same port↔tool cycle rule. NOT a junk drawer — each is consumed by a tool, not by the loop. But the rationale ("avoid the port↔tool cycle") is now copy-pasted across ~4 doc comments; the real reason is "Tooling-context-owned seam," worth stating once.
- **Hooks**: lifecycle firing split — per-tool phases (Pre/PostToolUse) in `dispatch.go`, run-lifecycle phases (SessionStart/UserPromptSubmit/Stop) in `agent/hooks.go`, SubagentStop duplicated in both `subagent.go` and `fork.go`. Cohesive by axis. UserPromptSubmit is the only run-level vetoing phase; hook-error there fails safe (rejects).
- **Telemetry**: `telemetry.{Metrics,Tracing}` implement port.EventSink + port.Logger, deriving spans/metrics from the event stream. Documented ctx limitation (EventSink.Emit has no ctx, Event has no session-id) → single "current run" span per Tracing instance; cannot correlate concurrent runs on one sink. Acceptable per-process; the right future seam is a per-run sink from the server interceptor.
- **fork vs task**: `NewForkTool` and `NewTaskTool` share `drainChild`, the child-`*Engine` injection pattern, and a near-identical `fireSubagentStop`. Fork adds workspace isolation (`WorkspaceForker`) so a mutating child is safe and `ReadOnly()==true` holds. Light duplication (options, fireSubagentStop, childSessionID) — at Rule-of-Two, not yet extract.

Known as-built defect: `session.EvSessionInit` is CONSUMED by both telemetry adapters (tracing keys its run span on it) but is NEVER emitted by the loop — the loop's first emit is `EvTurnStart`. Tracing's `ensureRun()` fallback masks it; metrics silently never counts it. Either emit it in `drive` or drop the dead constant + consumers.

Twelve-patterns pluggability audit lives at `docs/design/TWELVE-PATTERNS-AUDIT.md` (this agent, 2026-05-29). Key gaps: hook lifecycle UNDER-FIRED (only PreToolUse/PostToolUse/SubagentStop fire from the loop; SessionStart/UserPromptSubmit/Stop are declared `HookPhase` constants but never fired — seam exists, coverage doesn't); scoped instruction assembly is root-only (no parent walk / user / managed scope) even though governance HAS a `Scope` precedence ladder; no permission-classifier layer 2 (decorator over `port.PermissionPolicy` is the fix).

As-built context-management seams (verified 2026-05-29, the audit's compaction gap is now IMPLEMENTED):
- **`agent.TokenCounter` seam** (`agent/tokencount.go`): two-method contract `Count(string)` + `CountMessages([]session.Message)`; the latter owns per-message/per-tool-call wire-framing overhead so callers don't reconstruct it. Default `HeuristicTokenCounter` (4 chars/token, dependency-free) ships in `agent`; `adapter/tokenizer.Counter` is the opt-in offline tiktoken impl (vocab compiled in, no download). Direction correct: adapter depends inward on `agent.TokenCounter`, NO tiktoken type escapes into `agent`. Wired at composition root via `--tokenizer heuristic|tiktoken`. The framing-overhead constants are duplicated heuristic-vs-adapter ON PURPOSE (each is its own wire model).
- **`CascadeCompactor`** (`agent/cascade.go`) is a clean SECOND strategy behind the one `Compactor` seam — NOT a fork of `HeuristicCompactor`. It REUSES the heuristic's package-private primitives (`touchedPaths`/`buildSummary`/`truncateToolBody`); differs only in strategy (cheapest-first tiers snip→strip→collapse→summarize vs single-pass). Tier 4 (LLM summary) is opt-in via injected `port.LLMProvider`; nil→stops at tier 3, fully offline. Loop stays provider-agnostic (sees only `Compactor`). Summary re-enters as `NewUserMessage` (untrusted level, not system) — doc-08 attack-surface-safe.
- **`adapter/toolkit`** is a clean adapter-layer shared-kernel (NOT junk-drawer): `MaxOutputBytes`/`Truncate`/`ParseArgs`/`Schema`, consumed by `adapter/tools` + `adapter/memory`. Package doc names what does NOT belong (per-tool schemas/validation stay inline). Killed the duplicated `25_000` literal (latent drift bug).

KNOWN findings on the above (not yet fixed 2026-05-29):
- **Compaction trigger == cascade target** (`cmd/mecated/main.go:458` BudgetTokens == loop trigger `Window*Ratio`). No hysteresis → risk of per-turn re-compaction thrash on head/tail-dominated histories; masked today only because `snip` drops 50% of the middle. Fix: target < trigger (second ratio). The "target < trigger" relation is a real invariant of any trigger/compact pair.
- `toolkit.Truncate` marker hard-codes "25000 bytes" instead of using `maxBytes` param — message lies if cap changes (Low).
- `Deps` now carries 4 interdependent compaction fields (Compactor/TokenCounter/ContextWindowTokens/CompactionRatio); still coherent at 4, but a 5th knob is the threshold to extract a `CompactionConfig` value object that can validate target<trigger in one place.

As-built Wave-4 seams (verified 2026-05-29, build+import audit green):
- **`prompt.CommandExpander`** seam (`internal/prompt/command.go`): domain-owned iface `Expand(ctx, ws, input) (string, bool, error)`; default `NoopExpander` (off), `DirCommandExpander` reads `<name>.md` from `.mecatl/commands`/`.claude/commands` through `tool.Workspace` FS (never os, stays infra-free). Loop calls it in `recordPrompt` BEFORE recording/instructions. Correct altitude — it is prompt-assembly, a sibling of `InstructionAssembler`, both live in `prompt` and both are consumed in `recordPrompt`. NOT a leak.
- **`repomap` tool** (`internal/adapter/repomap`): a `tool.Tool` in its own adapter, tree-sitter (smacker/go-tree-sitter, CGO) fully contained — verified NO sitter type escapes the package, only `NewTool() tool.Tool` is exported. Gated by `//go:build repomap` / `!repomap` twin files `cmd/mecated/repomap_{enabled,disabled}.go` both defining `registerOptionalTools(*tool.Catalog)`. Default build is CGO-free/static (ko image); `-tags repomap` needs CGO. Both signatures match; default build verified green with CGO_ENABLED=0.
- **`dream.Consolidator`** (`internal/adapter/dream`): a SERVICE (not a tool, not in the loop) over `tool.MemoryStore`+`port.LLMProvider`. `Consolidate(ctx) (Report, error)` + `RunPeriodically`. Trigger policy correctly left to cmd (`startMemoryConsolidation` on a goroutine sharing run ctx). Conservative-by-construction: no invented keys, MaxForgets cap, fail-safe no-op on model/parse error. Adapter-local `planner` test seam. Imports inward only.
- `agent.Deps` now ~17 fields; CommandExpander is a coherent addition (a port like the rest). The compaction quartet (Compactor/TokenCounter/ContextWindowTokens/CompactionRatio) is still the one cluster worth extracting into a value object.

Known weak spots (see findings, not yet fixed as of 2026-05-29):
- `Session.StopReason()` conflates recorded terminal reason with limit-derived computation; `sessnap` round-trips it awkwardly via state-machine replay. WP6 flagged a `Snapshot()/Restore()` accessor pair as the fix.
- Aggregate boundary leaks: `agent/loop.go` and `sessnap` write `sess.Conversation.*` and `sess.Counters` directly, bypassing root methods.

See [[project-mecatl]] and [[domain-language]].
