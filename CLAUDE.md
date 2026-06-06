# CLAUDE.md — mecatl

A **headless agentic coding harness** in Go 1.26 (hexagonal/DDD): the streaming agent
loop, ~7 tools, permissions, hooks, and subagents behind a provider-agnostic port.
Driven over gRPC + HTTP; an optional Bubble Tea TUI (`mecatui`) is a client.

> `AGENTS.md` and `docs/harnesses/` are the **research corpus** this was built from, not
> harness conventions. For the build itself read `docs/architecture.md` (how it works),
> `docs/usage.md` (how to run it), and `docs/design/*` (rationale per feature — multi-provider,
> workspace-trust, soul, memory, agent-teams each have a doc there).
> `docs/design/IMPLEMENTATION-NOTES.md` holds the dense per-subsystem implementation/status
> detail. **Prefer adding design detail to those docs, not here** — this file is a lean
> correction file, not documentation.

## Commands

**Always build through the Taskfile** — a bare `go build` in the repo root drops stray
binaries and is the wrong workflow.

```sh
task build              # → bin/mecated, bin/mecademo, bin/mecatui  (NEVER `go build` to repo root)
task test               # full suite, -race
task test:golden        # refresh mecatui View/teatest goldens (-update) then re-run
task lint               # golangci-lint v2 + go vet
task generate           # regenerate contracts/gen from contracts/proto via buf
go test ./internal/agent/ -run TestFullCycle   # a single test
go run ./cmd/mecademo    # end-to-end demo, fully offline (mock provider)
```

## Architecture (where things live)

Layered DDD; the package's layer is in CAPS. Design rationale lives in `docs/design/`.

- `internal/session/` — DOMAIN: the `Session` aggregate (state machine), Conversation, the `ToolCall`/`ToolResult`/`Usage` value objects, the `Event` taxonomy.
- `internal/governance/` — DOMAIN: permission `Effect`/`Scope`/`Rule` + `Evaluator`, bash splitting/canonicalization, hook event types. **Session-free** (`session` imports it, never the reverse).
- `internal/tool/` — DOMAIN: `Tool`/`ToolSpec`/`Catalog` **and** the `FileSystem`/`Workspace` interfaces (see gotcha).
- `internal/prompt/` — DOMAIN: two-layer prompt assembly + AGENTS.md/CLAUDE.md discovery; the turn-0 `InstructionAssembler` chain and its consumer-local ports (`MemoryIndexSource`, `SoulSource`, `UserModelSource`). Trust/provenance decisions live in composition, not here.
- `internal/port/` — the PORT interfaces the loop consumes (`LLMProvider`, `SessionStore`, `HookRunner`, `PermissionPolicy`, `Clock`, `Logger`, `EventSink`).
- `internal/agent/` — APPLICATION: the loop (`Engine`/`Run`), dispatch, permission pause/resume, compaction, the Task subagent, the agent-team `Supervisor`/`TeamTool`. Read-only subagents (Task + team members) run their Bash in an isolated git worktree via a sandboxed runner — see `docs/design/AGENT-TEAMS-SPIKE.md` and the subagent-shell gotcha.
- `internal/adapter/` — ADAPTERS: `openai`, `anthropic` (native Messages API), `mockllm`, `osfs`/`memfs`, `permpolicy`, `permconfig`, `workspacetrust`, `soul`, `memory`, `providercatalog` (embedded models.dev subset), `openrouter`, `gitenv`, `hashutil`, `xdgconfig`, `hookexec`, `store/*`, `tools`, `server`.
- `internal/app/` — COMPOSITION (not domain): the single shared assembly (`app.Build`) of provider registry + catalog + policy + engine into a `server.Service`. The ONLY package allowed to import adapters + `internal/agent`. Per-session provider/model routing, live model listing, capability intersection, trust/soul selection all live here. See `docs/design/MULTI-PROVIDER.md`.
- `contracts/proto/mecatl/v1/` — gRPC contract (source of truth); `contracts/gen/` is generated, **never hand-edit**.
- `cmd/mecated/` — standalone server (composition root): flags, TLS/auth/rate-limit, HTTP + metrics listeners. `cmd/mecademo/` — the offline demo.
- `cmd/mecatui/` — optional gRPC **client** TUI; by default hosts a `mecated` in-process over a UNIX socket. `ui`/`theme`/`client` import no `internal/...` and no proto directly — they render from relayed proto `Event`s. See `docs/tui.md`.

## The layering rule (the thing to get right)

Dependencies point **inward only** (verified by import review; not machine-enforced):

- Domain (`session`, `prompt`, `governance`, `tool`) and `internal/agent` must **never** import an adapter, `contracts/gen`, `os`, the OpenAI/Anthropic SDKs, or gRPC.
- `internal/port` imports only domain + stdlib. `internal/agent` imports only domain + `port` — adapters are injected.
- Concrete adapters meet ports **only** in the composition layer — `internal/app` and the `cmd/` mains. No DI framework; explicit constructors. Keep wiring in `internal/app`, not in domain/`port`/`agent`.

## Things That Will Bite You

- **`FileSystem`/`Workspace` live in `internal/tool`, NOT `internal/port`.** Moving them to `port` creates a `port↔tool` cycle (`port.LLMRequest` references `tool.ToolSpec`; `Tool.Execute` takes a `Workspace`). Leave them in `tool`.
- **`port.PermissionPolicy` is implemented in `internal/adapter/permpolicy`, not `governance`** — `governance` can't import `session` (cycle), so the session-aware policy is an adapter over the session-free `governance.Evaluator`.
- **Preserve these invariants — they have tests that fail if you regress them:**
  - Edit's three invariants: read-before-edit (+ unchanged-since), exact match, uniqueness-unless-`replace_all`.
  - Dispatch is **read-parallel / mutate-serial** (`tool.ReadOnly()` drives it). Two mutating tools must never run concurrently.
  - Permissions resolve **deny-dominant**: a deny in ANY scope is absolute. Among ask/allow the higher *configured* scope wins, with ONE narrow exception: a higher-scope config Allow may loosen **only** the built-in-default Ask (the `ScopeBuiltinDefault` floor) — it must NEVER suppress a *configured* Ask. Equal-scope ask/allow ties favor Ask. Plan mode hard-denies mutations first. The six memory tools + the synthetic `soul:apply` action are floor-scoped (`ScopeBuiltinDefault`) Allows — pre-approved but config-overridable; they loosen no other tool's Ask. (Scope order high→low: `Managed > CLI > LocalProject > SharedProject > User > ScopeBuiltinDefault`.)
  - The bash gate must stay substitution/newline-aware (`$()`, backticks, subshells, `\n` fail safe). Don't simplify `SplitCommands`/`ReadOnlyBash` to operator-only splitting.
- **No stdio MCP, ever.** Future MCP is **streaming-HTTP transport only**. Never spawn `os/exec` MCP servers. (mecatl itself never speaks stdio; ToolHive stdio backends are HTTP-proxied and fine.)
- **ACP prompt content is multimodal + capability-gated.** Every block becomes text, a `session.Content` Part, or a loud `codeInvalidParams` — never silent-drop. Reuses `session.NewContent`/`ValidateMediaParts` (no second validation path); image/audio gated on `Service.ProviderCapabilities()`. Audio is wired but dormant (OpenAI Responses has no audio input).
- **Capability truth is a SINGLE composition-computed intersection.** `internal/app/capability.go`'s `modelCapability` = (catalog per-model modalities) ∩ (adapter `Capabilities()`). The one neutral value feeds ListModels, the `CreateSessionResponse` echo, and the ACP gate — don't recompute it per sink or they'll disagree. Details: `docs/design/MULTI-PROVIDER.md`.
- **Tests are offline.** Use `mockllm` + `memfs`; never hit a live model/network. The OpenAI/Anthropic SSE→Chunk paths are tested from fixtures.
- **The LLM adapters are stateless** (`store:false`, full replay each turn, byte-stable prompt prefix for caching). Don't switch to server-side conversation state. `port.LLMRequest` is **provider-neutral and must stay so** — `Model` is a bare opaque string; provider-private knobs (reasoning-effort, thinking-budget, store/include) are adapter-construction Options, NOT new `LLMRequest` fields (a guard test in `port/llm_neutral_test.go` tripwires silent widening). `Message.Reasoning` carries one opaque replay blob per message — structure neutral, contents provider-private; don't widen it.
- **Provider is FIXED per session.** A per-session engine re-derives ALL provider-closing Deps via `engineDepsForProvider` (LLM, Compactor, Model, TokenCounter, prompt model, context window) — never clone-and-swap-LLM, or it compacts/counts through the wrong model.
- **`Session` is an aggregate** — mutate it through its methods (`RecordUserPrompt`, `RecordAssistant`, `ReplaceHistory`, …), not by poking `Conversation` directly.
- **Every run-entry path must reopen-if-completed OR interrupt-if-cancelled.** A turn drives the session to a terminal state within one `Engine.Run`; the engine does NOT recover it. Any path handing a *reused* session to `Run`/`RunContent` must recover the terminal state first or `RecordUserPrompt` rejects it. `completed → Reopen → idle`; `cancelled → Interrupt → idle` (Interrupt also history-repairs: it closes out tool calls orphaned by a mid-dispatch cancel with synthetic error results, so the replay has no dangling `tool_use`/`function_call`). `Reopen` stays completed-only; a `failed` session is NOT resumable and still surfaces the illegal transition. In the service layer: **`loadAndReopen`, never `GetSession`, before a run** — it drives the right seam per state (`GetSession` is read-only snapshots). All wire surfaces funnel through `Service.StartRunContent`, which routes through `loadAndReopen`. Guarded by `TestStartRunContentReopensCompletedSession` + `TestStartRunContentRecoversCancelledSession`.

- **Diagnostics flow through the injected `port.Diagnostics`, NEVER `slog.Default()`/package-level `slog.Info|Warn|Debug|Error` in `internal/`** (banned by `forbidigo` in `.golangci.yml`, `cmd/` mains exempt). The `slogdiag` adapter is the only slog bridge; composition picks the sink (mecated→stderr/journald via `slog.SetDefault`; mecatui→file `$XDG_STATE_HOME/mecatl/mecatui.log` AND a redirected slog default so ambient/third-party slog can't corrupt the alt-screen; `--quiet`→discard). Build-once composition facts log in `app.Build` ONLY — never in the per-engine deps builders (`engineDepsForProvider`/`childEngineDepsForProvider`) (re-emitting per derivation caused N× duplication). Per-RUN diagnostics are session-correlated: the loop binds `deps.Diagnostics.With("session", id)` (+`"agent", role` for children) ONCE per run inside `Engine.RunContent` — NOT at construction (the engine predates the session id and is shared). Child engines (Task/member/fork) DO emit correlated diagnostics (real `cfg.diag()` + a `Deps.Role`), but `Deps.Sink`/`Deps.ToolCallRecorder` stay OFF for them. The loop emits exactly TWO lines (compaction-failure WARN, policy-deny INFO); cancellation/tool-error/compaction-success/permission-ask are NOT emitted (the `session.Event` taxonomy owns them). `port.ToolCallRecorder` is the per-tool AUDIT seam, DISTINCT from diagnostics. See `docs/design/DIAGNOSTICS.md`.

## Verification

After changes: `task lint && task test` must be green, and `go run ./cmd/mecademo` must still
print a full offline session (turn → tool.call → permission.ask + approval → result). The 10
gauntlet items in `docs/harnesses/08-design-considerations.md` each have a passing test — keep them passing.

## Workflow

- Commit directly to `main`. End commit messages with the `Co-Authored-By` trailer.
- Never `git add -A` — stage explicit paths.
- For smoke tests / scratch files, use the repo-local `.scratch/` dir (gitignored) — **not** `/tmp` or `mktemp`.
