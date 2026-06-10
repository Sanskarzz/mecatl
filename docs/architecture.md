# mecatl — Architecture

> Reader-facing architecture guide. This describes the **code as it exists** in
> `internal/`, `cmd/`, and `contracts/`. Where the design notes in
> `docs/design/` differ from the implementation, this document follows the
> implementation.

## 1. What it is

mecatl is a **headless agentic coding harness**: a service (and library)
that runs the agent loop — call the model, stream its output, execute coding
tools, enforce permissions and hooks, and emit a single typed event stream.
There is no TUI. Clients drive it over **gRPC** (a bidirectional `Converse`
stream) or **HTTP/SSE**. Both surfaces speak one provider-neutral domain
`session.Event` and call the same application service; neither ever sees an
OpenAI type.

The system is built **hexagonally (ports & adapters) with a DDD core**.
Dependencies point inward only: a domain of pure value objects and aggregates
(`session`, `governance`, `tool`, `prompt`), a set of port interfaces the
application consumes (`port`), the application use-case layer that is the agent
loop (`agent`), and adapters that implement the ports (`adapter/*`). The LLM
provider sits behind the `port.LLMProvider` seam, with the OpenAI Responses API
isolated entirely inside `internal/adapter/openai`, so the core is
provider-agnostic and unit-testable against fakes (`mockllm`, `memfs`,
`memstore`).

Around that core, every capability beyond the minimal loop is a **seam with a
default and a swap-in adapter**, so the production build stays static and
network-free unless you wire something in. The current adapters cover, grouped:
**reliability** (`llmresilience` retry/breaker decorator), **observability**
(`telemetry`: OTel metrics + runtime collector via a Prometheus exporter, OTel
spans over OTLP), **security** (server auth/mTLS, rate
limiting, the `permclassify` model-based risk classifier), **context management**
(`tokenizer` + the `CascadeCompactor`), **memory** (`memory` + `dream`),
**parallelism** (`forker` fork-join), and **extensibility** (the `mcp`
streaming-HTTP client). Each is detailed below.

## 2. The big picture

```mermaid
flowchart LR
  subgraph CMD["composition — internal/app + cmd/"]
    app["internal/app (app.Build: shared engine+service assembly)"]
    mecated["cmd/mecated/main.go (flags, serve, TLS/auth)"]
    demo["cmd/mecademo"]
    tui["cmd/mecatui (gRPC client TUI; embeds app.Build when no --server)"]
    mecated --> app
    tui --> app
  end

  subgraph DRIVING["driving adapters — internal/adapter/server"]
    grpc["HarnessService (gRPC, bidi Converse)"]
    http["HTTP/SSE handler"]
    svc["Service (lifecycle + Run registry)\nauth/mTLS · rate limit · health"]
  end

  subgraph APP["application — internal/agent"]
    engine["Engine / Run\nloop · dispatch · permission · hooks\ncompaction · cascade · tokencount\nsubagent (Subagent) · parallel (Parallel)"]
  end

  subgraph PORTS["ports — internal/port"]
    p["LLMProvider · SessionStore\nPermissionPolicy · HookRunner\nEventSink · Clock · Logger"]
  end

  subgraph DOMAIN["domain (no infra imports)"]
    sess["internal/session\nSession · Conversation · Event\nToolCall · ToolResult · Usage"]
    gov["internal/governance\nEffect · Decision · Rule · Scope\nHookEvent · Evaluator · bash.go"]
    tl["internal/tool\nTool · ToolSpec · Catalog · Disclosable\nFileSystem · Workspace · CommandRunner\nMemoryStore · WorkspaceForker · ToolSearch"]
    pr["internal/prompt\nLayered · Build · Env · toolDisciplineHints\nInstructionAssembler · SoulSource · CommandExpander\n(model-neutral; per-model agencyDelta lives in internal/app)"]
  end

  subgraph DECOR["decorators (port → same port)"]
    res["llmresilience (retry + breaker)"]
    pc["permclassify (layer-2 classifier)"]
  end

  subgraph DRIVEN["driven adapters — internal/adapter"]
    oai["openai · mockllm"]
    fs["osfs (+CommandRunner) · memfs"]
    st["store/memstore · jsonlstore · sessnap"]
    tools["tools (Read/Edit/Write/Grep/Glob/WebFetch + optional Bash)"]
    pp["permpolicy · hookexec"]
    tel["telemetry (OTel metrics+spans · Prometheus exporter · OTLP)"]
    ext["mcp (streaming-HTTP)\nmemory · dream · soul · forker · tokenizer"]
  end

  mecated --> svc --> engine
  demo --> engine
  tui -. "gRPC Converse (client)" .-> grpc
  grpc --> svc
  http --> svc
  engine --> PORTS
  engine --> DOMAIN
  PORTS --> DOMAIN
  res -.wraps.-> oai
  res -.implements.-> p
  pc -.wraps.-> pp
  pc -.implements.-> p
  oai -.implements.-> p
  st -.implements.-> p
  pp -.implements.-> p
  tel -.implements.-> p
  tools -.implements.-> tl
  fs -.implements.-> tl
  ext -.implements.-> tl
  mecated -. wires .-> DRIVEN
  mecated -. wires .-> DECOR
```

**Dependency direction is inward only.** The allowed-imports rule, stated by the
per-package `doc.go` files and honoured by the code:

| Package | May import |
|---|---|
| `session`, `governance`, `tool`, `prompt` (domain) | stdlib + other domain packages. Never `adapter`, `agent`, `contracts`, `os`, or any third-party library. |
| `port` | domain packages + stdlib (`context`, `io`, `iter`, `time`). |
| `agent` (application) | domain + `port` + stdlib only. Never an adapter or `contracts`. (Tests may import adapters.) |
| `adapter/*` | domain + `port` + the one external lib it adapts. Never `agent`. (Deliberate adapter→adapter carve-outs: (1) `adapter/mcpperf` may import `adapter/telemetry` solely for the `RuntimeSnapshot` data DTO it projects into tool output — a plain JSON struct with no OTel/SDK types, not a behavioural dependency; the DTO stays in `telemetry` by design. (2) `adapter/soul` AND `adapter/memory` import `adapter/skills` for `ScanForInjection` — the conservative role-override deny-list is shared so the soul (load-time) and the user-model RememberUser write path (write-time) reuse the same injection gate rather than copying the regexes. (3) `adapter/{permconfig,skills,agents,soul,memory}` import the leaf `adapter/xdgconfig` for the shared `ResolveEnv`/`UserConfigDir` XDG path-resolution seam — a stdlib-only adapter leaf, extracted to de-duplicate the copies (the user-model store resolves `<xdg>/mecatl/usermodel` through it). (4) `adapter/soul` and `adapter/memory` import the DOMAIN `internal/prompt` for a single compile-time assertion only — `var _ prompt.SoulSource = (*Store)(nil)` (soul→prompt) and `var _ prompt.UserModelSource = (*Store)(nil)` (memory→prompt) — pinning that each adapter satisfies the consumer-local prompt port it is bound to at composition. These are assertion-only edges (no prompt value is constructed or called); the adapters meet the ports structurally, and `internal/prompt` never imports them. |
| `contracts/gen` | generated; protobuf + gRPC runtime. |
| `app` (composition) | the shared engine/service assembly (`app.Build`). MAY import adapters + `agent` + (via `server`) `contracts/gen`. Nothing imports it but the `cmd/` mains. |
| `cmd/*` | flags + serving; consumes `internal/app`. With `app`, the only places concrete adapters meet ports. |
| `cmd/mecatui/{client,ui,theme}` | a gRPC **client**. `contracts/gen` + grpc + `internal/app` appear only in `client`, `embed`, and the `cmd/mecatui` main; `ui` and `theme` import none of them and **never** any `internal/...` package. |

**mecatui — the terminal UI (`cmd/mecatui`).** An optional gRPC *client*. It dials
the `HarnessService`, creates a session, opens the bidi `Converse` stream, and
renders the streamed `Event` envelopes (glamour markdown for assistant text,
themed lipgloss cards for user prompts and tool I/O), resolving permission asks
inline by sending `ResumeApproval` on the same stream. The server it talks to is
either an external `mecated` (`--server`) or one it **hosts in-process** over a
UNIX socket (`cmd/mecatui/embed` → `app.Build`) when none is given — so a single
binary works with no daemon. The render packages stay pure: they render **purely
from proto `Event`s** and are bound by the inward-only layering rule. The
`contracts/gen` + grpc + `internal/app` surface lives only in `cmd/mecatui/client`,
`cmd/mecatui/embed`, and the `cmd/mecatui` main; the `ui` (Bubble Tea
model/update/view) and `theme` (pure styling) packages import no `internal/...`
package and no proto directly. Usage and theming are documented in `docs/tui.md`.

Two deliberate cycle-breaks worth noting, documented in code:
- `port` imports `tool` and `prompt` (because `LLMRequest` carries
  `[]tool.ToolSpec` and `prompt.Layered`) — see the package note at the top of
  `internal/port/llm.go`.
- `FileSystem`/`Workspace` live in `internal/tool`, **not** `internal/port`,
  because `port` already imports `tool` while `tool.Tool.Execute` takes a
  `Workspace`; defining them in `port` would form a `port↔tool` cycle. See the
  package note in `internal/tool/tool.go`.
- `governance` does **not** import `session` (so `session` can import
  `governance` without a cycle); the `Evaluator` works on primitive args, and
  the `permpolicy` adapter bridges `session` types into it.

## 3. The domain model

### Session aggregate (`internal/session/session.go`)

`Session` is the aggregate root. Outside code holds a `SessionID` and reaches
inner entities only through intention-revealing methods, so the state machine
and stop conditions always hold. There are no public setters for lifecycle
state; mutation flows through methods such as `BeginTurn`, `RecordAssistant`,
`RecordToolResults`, `RecordUserPrompt`, `ReplaceHistory`, `PauseForApproval`,
`ResumeWith`, `Complete`, `Stop`, `Cancel`, `Fail`.

States (`session.State`): `idle`, `running`, `awaiting`, `completed`, `failed`,
`cancelled`. The last three are terminal (`State.IsTerminal()`).

```mermaid
stateDiagram-v2
  [*] --> idle: New
  idle --> running: BeginTurn
  running --> running: BeginTurn (next turn)
  running --> awaiting: PauseForApproval
  awaiting --> running: ResumeWith
  running --> completed: Complete / Stop(reason)
  running --> cancelled: Cancel
  running --> failed: Fail
  idle --> cancelled: Cancel
  idle --> failed: Fail
  awaiting --> cancelled: Cancel
  completed --> idle: Reopen
  cancelled --> idle: Interrupt (history-repairing)
  completed --> [*]
  cancelled --> [*]
  failed --> [*]
```

A terminal session can re-enter the loop through one of two intention-revealing
seams (a `failed` session stays terminal — never resumable):
- `completed → (Reopen) → idle` — a clean end-of-run is reopened to accept the
  next prompt, preserving history and resetting per-run `Counters`.
- `cancelled → (Interrupt) → idle` — an interrupted turn is recovered the same
  way, but Interrupt **also repairs the history**: a turn cancelled mid-dispatch
  can leave the trailing assistant message with tool calls that never received a
  result, so `closeOutInterruptedTurn` appends one synthetic error tool result
  per orphaned `ToolCall.ID` before going idle, keeping the replayed history
  provider-valid (no dangling `tool_use`/`function_call`). The service's
  `loadAndReopen` drives the right seam per state; both persist the recovered
  snapshot.

Notable, code-accurate details:
- `BeginTurn` is legal from `idle` **or** `running` (a follow-up model call in
  the same run) and increments `Counters.Turns`. It does **not** itself enforce
  limits; the loop consults `StopReason()` as a pre-turn guard.
- `Cancel`/`Fail`/`Stop`/`Complete` are legal from any non-terminal state and
  clear any pending ask.
- `ResumeWith()` takes **no** governance argument and returns the resolved
  `PendingAsk`. The aggregate only reconciles its own lifecycle; the loop owns
  acting on the decision. This keeps `session` a clean domain leaf with no
  `governance` dependency in its method signatures.

`Limits` (`MaxTurns`, `MaxToolCalls`, `MaxConsecutiveFailures`) and `Counters`
(`Turns`, `ToolCalls`, `ConsecutiveFailures`) drive stop conditions. A **zero
limit disables** that condition — which is why the composition root injects
non-zero defaults. Three derived predicates:
- `LimitTripped()` — pure; reports the limit-implied stop reason.
- `RecordedStopReason()` — pure; the faithful terminal reason recorded via
  `Stop/Cancel/Fail/Complete` (used by persistence; performs no derivation).
- `StopReason()` — `RecordedStopReason()` first, else `LimitTripped()`; the
  loop's pre-turn guard. Its godoc explicitly warns it conflates the two and is
  not a faithful witness — persistence must use `RecordedStopReason()`.

`PermissionMode`: `default`, `plan` (read-only toolset enforced), `acceptEdits`.

### Conversation / Message / Turn (`internal/session/conversation.go`)

`Conversation` holds the ordered, model-visible `[]Message`. `Message` is an
immutable value object built via `NewUserMessage`, `NewSystemMessage`,
`NewAssistantMessage(text, reasoning, calls)`, `NewToolMessage(result)`. Roles:
`system`, `user`, `assistant`, `tool`. `Message.Reasoning` carries the
provider's opaque reasoning item, replayed back verbatim and never interpreted.
`Turn` is a value object summarizing one model call plus its results and usage
(defined but not the primary carrier — the loop appends messages directly).

### Value objects

- `ToolCall{ID, Name, Args json.RawMessage}` (`toolcall.go`) — produced by the
  LLM, consumed by tool + governance; `NewToolCall`.
- `ToolResult{CallID, Content, IsError}` — `NewToolResult` / `NewToolError`;
  an error result is still fed back to the model so it can recover.
- `Usage{InputTokens, OutputTokens, CacheReadTokens, CacheWriteTokens}`
  (`usage.go`) with `CacheHitRate()` and an immutable `Add(other) Usage`.

### Event taxonomy (`internal/session/event.go`)

`EventType` is the single, provider-neutral taxonomy shared by the loop and the
API. The real constants:

| EventType value | Const | Emitted when |
|---|---|---|
| `session.init` | `EvSessionInit` | run starts (declared; loop emits `turn.start` first) |
| `turn.start` | `EvTurnStart` | beginning of each turn |
| `message.delta` | `EvMessageDelta` | streamed assistant text delta |
| `tool.call` | `EvToolCall` | a tool is about to run |
| `tool.result` | `EvToolResult` | a tool result (incl. denies / hook blocks) |
| `permission.ask` | `EvPermissionAsk` | loop paused for client approval |
| `hook` | `EvHook` | a hook fired (e.g. PreToolUse block) |
| `compaction` | `EvCompaction` | a compaction boundary crossed |
| `no_progress` | `EvNoProgress` | a completed turn produced no tool call and no meaningful text; the loop is nudging (gentle, then a final best-effort extraction) or giving up |
| `result` | `EvResult` | terminal: carries `ResultPayload{Stop, Text, Usage}` |
| `subagent.start/tool/end` | `EvSubagent*` | a `Subagent` child run's REDACTED, metadata-only projection (flat fleet) |
| `team.start/member/tasks/findings/end` | `EvTeam*` | an in-process `Team` run's BOUNDED projection (coordinating roster) |
| `parallel.start/branch/end` | `EvParallel*` | a `Parallel` fork-join run's REDACTED, metadata-only GROUP projection (join + winner + fork paths) |

`Event` carries `Type, Seq, Turn, Text` plus optional pointers `ToolCall`,
`ToolResult`, `Ask *PendingAsk`, `Result *ResultPayload`, `Usage *Usage`,
`Subagent`, `Team`, `Parallel`.

The three DELEGATION families (`subagent.*` / `team.*` / `parallel.*`) project the SAME
redacted child-loop lifecycle (the per-tool redaction is shared in
`agent.drainChildObserved`, the single chokepoint) and differ only in AGGREGATION shape —
flat fleet vs coordinating roster vs fan-out group. They are deliberately NOT merged; a
4th family is the trip-wire to extract a shared lifecycle value object (see
`docs/design/IMPLEMENTATION-NOTES.md`). Gauntlet #7: none carries branch/child content
into the parent conversation — only metadata (and, for Parallel, fork-root path handles).

## 4. The ports (`internal/port`)

Small interfaces, `context.Context` first. Each has a fake adapter so the loop
runs with no network and no disk.

| Port | Responsibility | Signature (verbatim) |
|---|---|---|
| `LLMProvider` (`llm.go`) | provider-agnostic model call; streams neutral chunks | `Stream(ctx context.Context, req LLMRequest) (iter.Seq2[Chunk, error], error)` |
| `SessionStore` (`store.go`) | persist/retrieve session state | `Save(ctx context.Context, s *session.Session) error` · `Load(ctx context.Context, id session.SessionID) (*session.Session, error)` |
| `HookRunner` (`hookrunner.go`) | run a lifecycle hook, map exit code to outcome | `Run(ctx context.Context, ev governance.HookEvent) (governance.HookOutcome, error)` |
| `PermissionPolicy` (`permission.go`) | deny→ask→allow across merged scopes | `Evaluate(ctx context.Context, mode session.PermissionMode, c session.ToolCall) governance.PermissionDecision` |
| `EventSink` (`log.go`) | relay loop events to the API stream | `Emit(ev session.Event)` |
| `ToolCallRecorder` (`log.go`) | structured per-tool AUDIT (distinct from `Diagnostics`) | `ToolCall(id session.SessionID, call session.ToolCall, result session.ToolResult, took time.Duration)` |
| `Diagnostics` (`diagnostics.go`) | injected operational-logging seam (NO global slog in `internal/`) | `Log(ctx, level Level, msg string, args ...any)` · `With(args ...any) Diagnostics` |
| `Clock` (`clock.go`) | abstract wall clock | `Now() time.Time` |

The model-call request and stream types (`llm.go`):

```go
type LLMRequest struct {
    System   prompt.Layered    // stable prefix + volatile suffix
    Messages []session.Message
    Tools    []tool.ToolSpec
    Model    string
}

type ChunkKind int
const (
    ChunkText ChunkKind = iota
    ChunkReasoning
    ChunkToolCall
    ChunkUsage
    ChunkDone
)

type Chunk struct {
    Kind     ChunkKind
    Text     string             // text on ChunkText, reasoning on ChunkReasoning
    ToolCall *session.ToolCall  // on ChunkToolCall
    Usage    *session.Usage     // on ChunkUsage
    Stop     session.StopReason // on ChunkDone
}
```

### The tool contract and the FS seam (`internal/tool`)

```go
type Tool interface {
    Spec() ToolSpec
    ReadOnly() bool
    Execute(ctx context.Context, in session.ToolCall, ws Workspace) (session.ToolResult, error)
}
```

`ToolSpec{Name, Description, Schema json.RawMessage}` is what the model sees;
descriptions are documentation (gauntlet #10). `Catalog` (`catalog.go`) is a
name→Tool registry with `Register`/`MustRegister`/`Lookup`/`Tools`. Its
`Specs(mode)` and `Available(mode)` apply **plan-mode filtering at the catalog
level**: in `ModePlan` only `ReadOnly()` tools are exposed, ordered by name.

`FileSystem` and `Workspace` live here (not in `port`) to break the
`port↔tool` cycle. `Workspace` is the session-scoped seam every tool executes
against: it scopes all paths to one root (rejecting `../` escapes), exposes
`Root/Read/Write/Stat/Glob/Grep`, and carries the Edit **read-ledger**
via `RecordRead(path, version)` / `WasReadUnchanged(ctx, path)`. The
read-before-edit invariant is enforced inside the Edit tool against this ledger
(`internal/adapter/tools/edit.go`: invariant #1 via `WasReadUnchanged`, #2
exact match, #3 uniqueness unless `replace_all`).

**Command execution is a separate seam, not part of `Workspace`.**
`tool.CommandRunner` (`Run(ctx, command) (CommandResult, error)`) is the only
chokepoint for shell execution; the agent loop never references it, and only the
Bash tool depends on it. That makes Bash — and therefore *all* command
execution — optional in the catalog: `NewBashTool(runner)` is registered only
when a runner is configured (it panics on a nil runner), and `tools.Register`
deliberately excludes it. The `osfs` adapter ships a local `/bin/sh`
`CommandRunner` (output-capped, context-bounded); a runner may also execute
remotely or refuse with `tool.ErrNoShell`. A shell-less deployment simply omits
Bash, and an OS sandbox would wrap this seam. `tool.MemoryStore` and
`tool.WorkspaceForker` live alongside it for the same layering reason (the tools
that need them depend on the interface, not a `port`).

## 5. The agent loop (`internal/agent`)

`Engine` is built from `Deps` (all ports + the application seams + config) via
`NewEngine`, which supplies network-free defaults for every optional seam:
`Compactor`→`HeuristicCompactor{}`, `CompactionRatio`→`0.8`,
`TokenCounter`→`HeuristicTokenCounter{}`, `Instructions`→`prompt.RootAssembler{}`,
`CommandExpander`→`prompt.NoopExpander{}`. `Engine.Run(ctx, sess, ws, userText)`
returns a `*Run` handle immediately and drives the loop in a background
goroutine; the `Run` exposes:
- `Events() <-chan session.Event` — the primary surface, closed exactly once
  when the run terminates.
- `Approve(askID string, allow bool)` — resolves a `permission.ask`
  out-of-band.
- `Cancel()` — cancels the run's context.

`drive` (in `loop.go`) is the algorithm:

1. **SessionStart gate** (`fireSessionStart`, first turn only): fire the blocking
   `SessionStart` hook before anything else; a block (or hook error) aborts the
   run before the prompt is even recorded.
2. **Record the prompt** (`recordPrompt`): expand the raw input through
   `CommandExpander.Expand` (slash commands; the `NoopExpander` default leaves it
   unchanged), fire the blocking `UserPromptSubmit` hook **on the expanded text**
   (a block ends the run; a `Mutated` payload replaces the effective prompt), and
   on the first turn assemble project instructions via `Instructions.Assemble`
   (the `RootAssembler` default reads AGENTS.md/CLAUDE.md), recording them + the
   final user text through the aggregate root.
3. **Pre-turn stop guard**: if `sess.StopReason()` trips or `ctx` is cancelled,
   terminate.
4. `BeginTurn`, emit `turn.start`.
5. **Maybe compact** (`maybeCompact`).
6. **Run the turn** (`runTurn`): build the `LLMRequest`, call `LLM.Stream`,
   consume chunks, emit `message.delta` for text, accumulate reasoning, collect
   tool calls and usage, capture the stop reason; assemble one assistant
   `Message`. `ctx` cancellation mid-stream surfaces as a cancellation.
7. `RecordAssistant`. If there are **no tool calls**, the model is done →
   complete the run.
8. **Dispatch** the tool calls, `RecordToolResults`, `save`, loop back to (3).

The loop terminates the session in exactly one of `Complete`/`Stop`/`Cancel`/
`Fail` and emits exactly one terminal `result` event carrying cumulative usage.

```mermaid
sequenceDiagram
  participant C as Client
  participant E as Engine.drive
  participant L as LLMProvider
  participant D as dispatch
  participant T as Tool
  C->>E: Run(ctx, sess, ws, "fix the bug")
  E->>E: BeginTurn → emit turn.start
  E->>L: Stream(LLMRequest)
  L-->>E: ChunkText / ChunkReasoning ...
  E-->>C: message.delta
  L-->>E: ChunkToolCall (Read)
  L-->>E: ChunkUsage, ChunkDone
  E->>E: RecordAssistant
  E->>D: dispatch([Read])
  D->>D: Policy.Evaluate → Allow
  D->>D: PreToolUse hook
  D-->>C: tool.call
  D->>T: Execute(call, ws)
  T-->>D: ToolResult
  D-->>C: tool.result
  D->>D: PostToolUse hook
  D->>E: results
  E->>E: RecordToolResults → loop (next turn)
  E-->>C: result (StopEndTurn)
```

### Read-parallel / mutate-serial dispatch (`dispatch.go`)

Enforced in `Engine.dispatch`, keyed off `Tool.ReadOnly()`:
- Calls are processed **in original order**, batched into maximal runs of
  consecutive read-only tools.
- A read-only batch (`runReadBatch`) authorizes + runs PreToolUse hooks for
  every call first (permission **asks are sequenced one at a time**, never two
  at once), then executes the cleared calls **concurrently**, one goroutine per
  call, results merged under a mutex.
- A mutating or **unknown** tool (`runOne`) runs **alone, serially**, never
  overlapping anything.
- Results are keyed by `CallID` and re-assembled in input order.

A `cancelled` flag propagates from `dispatch` so the loop terminates as
`StopCancelled` if `ctx` was cancelled mid-await or mid-execution. A
harness-level tool error becomes an error `ToolResult` (the loop never aborts on
one tool failure); a genuinely unknown tool yields an error result too.

## 6. Permission pause / resume

When `Policy.Evaluate` returns `Ask`, `dispatch.go`'s `authorize` pauses the
loop. The handshake is brokered by `askRegistry` (`permission.go`): the loop
**registers the resolution channel before** pausing and emitting, so an
`Approve` that races in cannot be lost. `PauseForApproval` moves the session to
`awaiting`; the loop emits `permission.ask`; `askRegistry.await` blocks on the
buffered channel until `Run.Approve` resolves it or `ctx` is cancelled.

```mermaid
sequenceDiagram
  participant Cl as Client
  participant Sv as server (gRPC / HTTP)
  participant R as Run
  participant A as authorize (dispatch.go)
  participant Reg as askRegistry
  participant P as PermissionPolicy
  A->>P: Evaluate(mode, call)
  P-->>A: {Effect: Ask, Reason}
  A->>Reg: register(askID)
  A->>A: sess.PauseForApproval → state=awaiting
  A-->>Sv: emit permission.ask {askID, tool, args, reason}
  Sv-->>Cl: Event permission.ask
  Note over A,Reg: loop blocked in askRegistry.await
  Cl->>Sv: Converse ResumeApproval{ask_id, allow}  /  POST /approve
  Sv->>R: run.Approve(askID, allow)
  R->>Reg: resolve(askID, allow)
  Reg-->>A: verdict
  A->>A: sess.ResumeWith → state=running
  alt allow
    A->>A: execute the tool
  else deny
    A->>A: denyResult → error ToolResult fed to model
  end
```

- **Allow** → the call executes normally.
- **Deny** → `denyResult` synthesizes a `permission denied: <reason>` error
  `ToolResult`, fed back so the model can adapt.
- **Cancel while awaiting** → `await` returns `ok=false`; the loop ends as
  `StopCancelled`.

This ties directly to the API: the gRPC `Converse` stream carries the verdict in
a `ResumeApproval` frame on the **same** stream emitting events (no out-of-band
correlation), and the HTTP surface uses `POST /v1/sessions/{id}/approve`. Both
land on `Run.Approve`. The `Service` keeps a registry of in-flight `*agent.Run`
keyed by session id so the verdict reaches the right run
(`server/service.go`: `LookupRun`).

## 7. Hooks

Hook lifecycle phases (`governance/hookevent.go`): `SessionStart`,
`UserPromptSubmit`, `PreToolUse`, `PostToolUse`, `Stop`, `SubagentStop`. **All
six now fire** — the per-tool pair from `dispatch.go`, the run-level trio from
`agent/hooks.go`, and `SubagentStop` from the Subagent tool.

Per-tool placement (`dispatch.go`):
- **PreToolUse** (`preHook`) runs after permission clears, before execution, for
  every call (read-batch and serial). On a hook **block** it emits a `hook`
  event and substitutes an error `ToolResult` (the tool does not run). A hook
  execution error is surfaced to the model as a block annotation rather than
  aborting the run; a cancelled context is the one case that ends the run. A
  non-empty `HookOutcome.Mutated` payload on an allow — interpreted symmetrically
  with the PreToolUse `HookEvent.Input`, i.e. the tool's raw **arguments JSON** —
  **rewrites the call's args before execution**: the loop builds a fresh
  `session.ToolCall` (same CallID and tool Name, new Args) and executes that. A
  malformed (non-JSON) mutation is ignored (original args stand) and a notice
  event is emitted. **Permission ordering (trust):** the policy is evaluated on
  the **original, pre-mutation** args; the mutated args are **not**
  re-permission-checked. This is deliberate — a PreToolUse hook is
  operator-deployed and more trusted than the model, so it may rewrite a call
  past the policy that gated the model's request (matching Claude Code).
- **PostToolUse** (`postHook`) runs best-effort after execution and **before** the
  `tool.result` event is emitted (the emit was moved after `postHook` for this
  reason). A block there only annotates (the tool already ran; a block neither
  undoes nor suppresses the result), and a hook execution error is ignored —
  neither aborts. A non-empty `HookOutcome.Mutated` payload — interpreted
  symmetrically with the PostToolUse `HookEvent.Input`, i.e. the result object
  `{"content", "is_error"}` — **rewrites the result**: the loop builds a fresh
  `session.ToolResult` (same CallID; `NewToolError` when `is_error`, else
  `NewToolResult`). A malformed (non-JSON) mutation is ignored (original result
  stands) with a notice event. Because the **effective** (rewritten) result is what
  is both emitted and returned/recorded, the client stream and the model's history
  agree — there is no hidden divergence. **Trust:** a PostToolUse hook is
  operator-deployed and trusted, so it may rewrite what the model sees the tool
  returned (e.g. redact secrets).

Run-level placement (`agent/hooks.go`). Both pre-prompt phases are **blocking
run-level gates** that fail safe — a block, or a hook **execution error**, ends
the run before any model call with `StopError`, emits a `hook` event, and still
fires `Stop`:
- **SessionStart** (`fireSessionStart`) runs once at the very start of a run,
  before the prompt is recorded; a block (or hook error) **aborts** the run.
- **UserPromptSubmit** (`fireUserPromptSubmit`) runs on the (command-expanded)
  prompt text **before** `RecordUserPrompt`; a block (or hook error) ends the run.
  A non-empty `HookOutcome.Mutated` payload — interpreted symmetrically with the
  `{"prompt": ...}` `HookEvent.Input` — **replaces the effective prompt**: the
  mutated text is what is recorded into the session and sent to the model (a
  malformed mutation is ignored, original text stands). Ordering is preserved:
  command expansion runs first, then the hook, then recording; first-turn
  instruction assembly is unchanged.
- **Stop** (`fireStop`) runs exactly once at the terminal end of any run path,
  even if `ctx` is already cancelled (it is a terminal notification). The Subagent
  subagent mirrors this with **SubagentStop**.

Exit-code semantics live in the `hookexec` adapter
(`internal/adapter/hookexec/hookexec.go`): the `HookEvent` is JSON-serialized to
the hook process's **stdin**; **exit 0 = allow**, **exit 2 = block**
(`HookOutcome.Block = true`, message from stdout/stderr). On an **allow**, if the
hook's stdout is a JSON **object** it is parsed as a control envelope —
`{"mutated": <raw payload>, "message": "..."}` — and `mutated` becomes
`HookOutcome.Mutated` (the rewritten prompt/args payload the loop applies). Plain
(non-JSON-object) stdout is treated as a message exactly as before, so existing
hooks are unaffected. This is what lets **real shell hooks** emit mutations (not
just custom Go `HookRunner` adapters).

## 8. Subagents (`internal/agent/subagent.go`)

`SubagentTool` is a `tool.Tool` (catalog name `Subagent`) that delegates a focused,
self-contained task (multi-step investigation or build/test/git work) to a **child agent loop**. Its `Execute`:
1. **Workspace selection.** When a child forker is wired (`WithChildForker` — the
   composition root wires it **iff** the child catalog includes Bash) it forks the
   incoming `ws` into an **isolated git worktree** (the forker DEFAULT mode — shares
   the base repo's `.git`, so the child sees full history) and runs the child there;
   the worktree is torn down after the child drains. Without a forker the child has
   no Bash and runs against the **parent** `ws`, exactly as before. A fork **failure**
   on the wired path is a tool **error**, never a silent fallback to the shared `ws`
   (running the child's Bash in the shared base is the exact hazard isolation exists
   to prevent).
2. Builds a **fresh** child `session.New(...)` — own conversation, own (tighter)
   `Limits` (`defaultChildLimits`: 12 turns / 40 tool calls / 3 failures),
   scoped to the **run** workspace root (the worktree when forked, else the parent).
   **On `resume`** (a Subagent call carrying `resume: <agentId>`) it instead RELOADS the
   persisted child by that id and recovers its terminal state — `completed` → `Reopen()`,
   `cancelled` → `Interrupt()` (history-repair), `failed` is **not** resumable — then
   re-homes it onto the fresh fork (`session.Session.Rehome`) and prepends an honest
   staleness note (the conversation survives, the workspace does NOT). Resume runs on the
   **default explorer engine only** (rejected with `agent`/`model`); an in-flight guard
   rejects a concurrent run on the same id.
3. Runs the child via the injected `childEngine.Run(ctx, child, runWS, prompt)`.
4. **Drains the child's entire Event stream inside `Execute`** (`drainChild`),
   discarding every intermediate `turn.start`/`message.delta`/`tool.call`/
   `tool.result`/`hook`/`compaction` event, and **returns only the final
   summary string** as one `ToolResult` (gauntlet #7).

**Per-call knobs (`subagentArgs`).** Beyond `prompt`/`description`/`agent`, a Subagent call may
supply: `max_turns`/`max_tool_calls`/`max_tokens` (TIGHTEN-ONLY caps — the model can
make its child stricter than the operator's bound, never looser); `timeout_ms` (a
wall-clock deadline → a time-budget tool error); `model` (pin THIS child to a specific
provider model — minted via the composition-supplied `WithSubagentEngineFactory` closure
through the contamination-safe `newChildEngineForProvider` path, NEVER a clone-and-swap;
mutually exclusive with `agent`); and `output_schema` (a model-authored JSON schema —
the child is given a synthetic `SubmitResult` tool whose params ARE the schema, must
call it to deliver, and the submitted payload is validated by `session.ValidateJSON`
with a bounded correction-retry, NO `tool_choice` forcing). The Subagent RESULT is labelled
by terminal reason (success / `[subagent stopped: …]` note / structured-output
validation error / error) and carries an `agentId: <childID>` trailer on every terminal
(model-visible, mirroring the Team-id line) so the parent can discover the child id and
read its persisted transcript via the read-only `InspectSubagent` tool (the id is used
verbatim), or pass it as `resume` to CONTINUE that subagent with a follow-up prompt
(default engine only, fresh fork + staleness note; `failed` is not resumable). None of
these widen `port.LLMRequest` — they are `subagentArgs`/`RunOptions`/factory concerns.

**Background, SubagentStatus & per-child cancel (`docs/design/BACKGROUND-SUBAGENTS.md`).**
`background: true` DETACHES the child, RUN-scoped: the call returns an immediate
started-result (agentId trailer first) and a goroutine owns fork → drive → persist →
result-stash in the parent `Run`'s **child-run registry** (`childRunRegistry` — every
run registers ALL children of all three families under their child session ids). The
result body's sole channel is the read-only **`SubagentStatus`** tool (no args → this
run's roster; `agent_id` → state + the stored body, delivered exactly once; `wait_ms`
parks up to 120s) — a turn-boundary harness NOTICE (ids + stop labels only, nothing
child-authored) tells the model when a background child finishes, and ONE
background-pending nudge defers a would-be clean end so results aren't silently lost.
At run end live background children are cancelled, joined (bounded two-phase drain),
sealed (`safeEmit` makes post-seal child emits no-ops), and persisted — resumable
next run. The registry also powers **per-child cancel**: `Run.CancelChild(childID)`
(gRPC `ConverseRequest.cancel_child`, HTTP `POST /v1/sessions/{id}/cancel-child`,
mecatui's `x` key) cancels ONE subagent / parallel branch / team member without
touching the run, retracting any permission ask the child had parked
(`permission.retract`); the child persists and stays resumable.

The child is a **read-only explorer with a shell** — capability flows down from the
parent (which has Bash); isolation, not catalog read-only-ness, is the security
boundary:
- The composition layer wires `childEngine` with **Read/Grep/Glob plus Bash**
  (`buildChildEngine` registers Bash via the **sandboxed** runner —
  `buildSandboxedCommandRunner`, the SAME hardening team members get, since the
  worktree shares the parent `.git`), **never `Subagent`/`Parallel`/`ToolSearch`** (no
  recursion / fan-out) and **never Edit/Write** (it inspects, it does not edit the
  project). Per-def Subagent engines keep Bash via `scopedToolNamesMode`'s `allowShell`
  and share the one `SubagentTool` forker. With no runner (`--no-bash`) the child is a
  Bash-less read-only explorer and no forker is wired — the original behaviour. The
  policy is **allow-all** so the child never prompts a human (`internal/app`:
  `buildSubagentTool` / `buildChildEngine` / `buildAgentSubagentEngines`).
- `SubagentTool.ReadOnly()` stays **`true`**, letting the parent run `Subagent` concurrently
  with other read-only tools. This is safe because the child's (mutating-classified)
  Bash writes land in the **isolated worktree**, never the shared base the parent's
  other read-only calls race over; the only shared surface is the `.git` object
  DB/refs (git-locked; config-driven code-exec vectors neutralised via `gitenv`).
- `WithMaxConcurrentChildren` (default 4; `WithMaxConcurrentSubagentShells` is a
  deprecated alias) sizes the **child concurrency gate**, acquired at the top of
  `run()` for ALL children (forking and forker-less) — Subagent is read-parallel, so
  the model can fan many out; each child consumes a session + an LLM slot (and, when
  forked, a worktree). Foreground acquisition blocks; a **background** child's
  acquisition is **fail-fast** (a full gate is a model-addressable error listing the
  live background ids — a background child holds its slot across turns, so blocking
  could deadlock the model against itself).
- A child's permission ask resolves through the **4-step model** (see CLAUDE.md's
  subagent-shell gotcha): read-only-substitution and isolation auto-approve resolve
  most asks in `governance`; what remains is **surfaced to the human** when the parent
  run is interactive (the child parks; `Run.Approve` routes the verdict by the
  child-namespaced askID) or **auto-denied with an accurate model-facing message**
  when headless — never a blanket deny.
- The child run is bounded by the parent `ctx`; `SubagentStop` fires
  best-effort (on a detached short-lived context if the parent is already
  cancelled). `NewSubagentTool` panics on a nil child Engine.

This mirrors the **team-member** worktree treatment (§ below): same `gitenv`
hardening, same untrusted-`.gitattributes` residual, and the workspace-trust gate is
the **shared follow-up** for both Team and Subagent.

## 9. The OpenAI Responses adapter (`internal/adapter/openai`)

`Provider` implements `port.LLMProvider` over `POST /v1/responses` using
`github.com/openai/openai-go/v3`. It owns its own conversation state
("strategy B"): every request is **stateless** — `Store: false`, no
`previous_response_id` — and resends the full input item slice.

**Request translation** (`request.go`, `buildParams`):
- `LLMRequest.System` (the `prompt.Layered`) is `Render()`-ed into
  `Instructions`.
- `Tools` → function tools, each spec's JSON `Schema` unmarshalled into the
  SDK's parameter map (`Strict: true`; empty schema → empty object).
- `Messages` → the input item array via `buildInput`: system/user/assistant text
  become message items; tool messages become `function_call_output` items keyed
  by `call_id`. An assistant turn expands (`assistantItems`) in the order
  **reasoning item → function_call item(s) → text message**.
- `Store: false` plus `Include: [reasoning.encrypted_content]` so reasoning
  survives across turns statelessly. `Message.Reasoning` is carried verbatim as
  the reasoning item's `EncryptedContent`.

**SSE → Chunk translation** (`stream.go`, `translate` — a pure function driven
directly from recorded fixtures by `decodeSSE` in tests):
- `response.output_text.delta` → `ChunkText`
- `response.reasoning_summary_text.delta` → `ChunkReasoning`
- `response.output_item.done` (function_call) → `ChunkToolCall` (acts on the
  assembled `.done` payload, not concatenated deltas)
- `response.completed` → `ChunkUsage` then `ChunkDone` (cached tokens map into
  `Usage.CacheReadTokens`)
- `response.failed` / `response.incomplete` / `error` → `ChunkDone(StopError)`

**Cancellation**: `Stream` (`openai.go`) selects on `ctx.Done()` each iteration
and abandons the underlying stream; a deliberate `ctx` cancel is **not** reported
as a stream error.

**The provider-neutral seam**: the loop only ever sees `port.Chunk`; no OpenAI
type crosses the boundary. The fake `mockllm.Provider` (`adapter/mockllm`,
`New(turns...)`, `TextTurn`) implements the same port for offline loop testing.

**Compatible endpoints**: `WithBaseURL(url)` overrides the host (vLLM, LiteLLM,
a local proxy); the SDK appends `/responses`. `WithAPIKey` and
`WithRequestOption` round out the options. `cmd/mecated` plumbs
`--openai-base-url` through to it.

## 10. The API surface (`internal/adapter/server`, `contracts/proto/mecatl/v1/harness.proto`)

One `Service` (`service.go`) backs two surfaces, both relaying the same domain
`session.Event` mapped to one proto `Event` by `toProto` (`mapper.go`). The
service owns session lifecycle (`CreateSession`, `GetSession`), starts runs
(`StartRun`), and registers/deregisters in-flight `*agent.Run` so approve/cancel
reach the right run.

**gRPC (`harness.proto`, `grpc.go`)** — `HarnessService`:
- `CreateSession(CreateSessionRequest) → CreateSessionResponse` — carries an
  OPTIONAL per-session `provider_id` / `model_id` selector (multi-provider Phase 0; see §18).
- `GetSession(GetSessionRequest) → GetSessionResponse`
- `ListModels(ListModelsRequest) → ListModelsResponse` — the selectable-model
  inventory: every AVAILABLE provider's catalog models projected to public metadata
  (`ModelInfo{id, provider_id, display_name, image, reasoning, context_limit}`), no
  secrets, (provider_id, id)-sorted. Gated by `ServerCapabilities.model_selection`
  (true iff ≥1 provider is available). See §18.
- `Converse(stream ConverseRequest) → stream ConverseResponse)` — bidirectional.
  The first frame **must** be `prompt`; then zero or more `resume_approval` /
  `cancel` control frames. `ConverseRequest` is a `oneof kind { Prompt prompt=1;
  ResumeApproval resume_approval=10; Cancel cancel=11 }`. The server starts the
  run, reads control frames on a side goroutine (`readControl`), and relays
  `Event`s on the main goroutine until the channel closes.

`ConverseResponse` wraps one `Event`. The proto `Event` mirrors `session.Event`
one-for-one: string `type` plus `ToolCall`, `ToolResult`, `PermissionAsk`,
`Result`, `Usage` submessages. `PermissionAsk.ask_id` is echoed back in
`ResumeApproval.ask_id`. Required-field annotations use `buf.validate.field`;
v1 enforces required checks in the Go server (protovalidate runtime is deferred).

**HTTP/SSE (`http.go`)** — the thin mirror, since grpc-gateway cannot map bidi:

| HTTP | Maps to | Notes |
|---|---|---|
| `POST /v1/sessions` | `CreateSession` | JSON body → `session_id`; optional `provider_id`/`model_id` selector |
| `GET /v1/sessions/{id}` | `GetSession` | JSON snapshot |
| `GET /v1/models` | `ListModels` | JSON selectable-model inventory (available providers only, secret-free) |
| `POST /v1/sessions/{id}/prompt` | start a run | `text/event-stream`; each event is `data: <proto Event as JSON>` |
| `POST /v1/sessions/{id}/approve` | `Run.Approve` | resolves the paused ask |
| `POST /v1/sessions/{id}/cancel` | `Run.Cancel` | cancels the in-flight run |

Closing either stream cancels the run: the SSE handler watches
`r.Context().Done()` and calls `run.Cancel()`; the gRPC relay cancels on a send
error. `Run.Cancel` first arms the run's sticky `hardAbort` signal (a short
grace timer, armed before the ctx cancel), so every guarded send — the loop's
`emit`, the child registry's `emitOrAbort`, the team supervisor's member
forward — gives up instead of parking forever behind a consumer that stopped
draining (the explicit unwedge; a cancelled run with a *draining* consumer
still delivers its in-flight events — the grace covers even a backlogged one).
Both relays are drain-to-discard after the FIRST Send/Write error: they record
the error, cancel the run, and keep ranging `run.Events()` (discarding, no
further writes) until the channel closes — a busy run never wedges in its own
emits behind a dead client.

## 11. Observability & persistence

- **EventSink** (`port.EventSink`) — an optional secondary relay.
  `Emit(ctx, ev)` carries the run's `context.Context` so telemetry can parent a
  run span to an inbound request span (the ctx is a **trace/baggage carrier
  only** — sinks must not derive cancellation/deadlines from it, since terminal
  emits deliberately pass an already-cancelled ctx). `Run.emit` always writes to
  the `Events()` channel (the primary surface) and then mirrors the sequenced
  event to `Deps.Sink` when configured (`dispatch.go`'s `Engine.emit`, which
  forwards the per-run `Run.ctx`). `Seq` is a monotonic per-run counter
  (`atomic.Int64`).
- **ToolCallRecorder** (`port.ToolCallRecorder`) — `ToolCall(id, call, result,
  took)` records tool-execution timing, the per-tool AUDIT seam, distinct from the
  model-visible conversation. The loop times execution via the injected `Clock`
  (`timeExecute`).
- **Diagnostics** (`port.Diagnostics`) — the injected operational-logging seam
  (composition decisions, degraded-mode warnings, lifecycle notes), DISTINCT from
  the audit (`ToolCallRecorder`) and the event stream (`EventSink`). `internal/`
  takes this port and NEVER touches global slog; the `slogdiag` adapter is the only
  slog bridge and composition picks the sink per binary. The ban is `forbidigo`-
  guarded. See `docs/design/DIAGNOSTICS.md`.
- **Telemetry** (`internal/adapter/telemetry`) — one adapter that implements
  **both** `port.EventSink` (deriving counters/gauges from the event stream) and
  `port.ToolCallRecorder` (per-tool counters + a latency histogram). It is built on the
  **OTel metrics SDK**: `telemetry.NewMetrics(metric.MeterProvider)` creates the
  domain instruments from an injected MeterProvider. `telemetry.NewSink` fans one
  Engine `EventSink` out to several sinks; OTel spans model the run/turn/tool
  hierarchy. `telemetry.Setup` returns a `Providers{Tracer, Meter, Registry,
  Shutdown}` struct: **metrics are always on** — Setup always builds an SDK
  MeterProvider with a Prometheus-exporter reader on a fresh `Registry`, installs
  the tool-duration base-2 exponential-histogram view (`telemetry.ToolDurationView`,
  the single source of truth any MeterProvider feeding `NewMetrics` must install),
  and starts the contrib **runtime collector** (goroutines, GC, heap) against that
  provider. `/metrics` is served via the OTel Prometheus exporter using
  `telemetry.MetricsHandler(providers.Registry)`. **Tracing installs only when an
  OTLP endpoint is set**: with `cfg.Endpoint` non-empty Setup builds an OTLP span
  exporter + SDK TracerProvider and installs it globally (wired in `mecated` via
  `--otlp-endpoint` / `--otlp-protocol` / `--otlp-insecure`); with an empty
  endpoint `Providers.Tracer` is the current (no-op) global and only metrics run.
  > A broader performance-observability effort lands incrementally on a
  > **loopback-only, unauthenticated admin listener** (`--metrics-addr`, default
  > `127.0.0.1:9090`): `/metrics`, `/debug/pprof/*`, `/debug/vars` (a curated
  > `runtime/metrics` snapshot), `/debug/flightrecorder` (an execution-trace ring),
  > and — opt-in via `--perf-mcp` — `/mcp`, the read-only **perf MCP server**
  > (`internal/adapter/mcpperf`) that lets an agent introspect this process's
  > runtime/latency/profile state as reduced numeric summaries (slow-turns,
  > profile rankings, FlightRecorder summaries). All of it is **live**. The MCP
  > surface is fail-closed to loopback (it is unauthenticated and can embed
  > goroutine-derived names/timing). See `docs/design/perf-observability.md` (the
  > decided direction) and `docs/perf-measurement-survey.md` (the Go-perf
  > technique reference).
- **SessionStore** — `memstore` (default, in-memory) and `jsonlstore`
  (append-only JSONL replay log: `<dir>/<id>.session.jsonl` snapshots +
  `<dir>/<id>.tools.jsonl` tool records; `jsonlstore` also implements `Logger`).
  Both serialize via **`sessnap`** (`adapter/store/sessnap`): a `Snapshot` DTO
  that round-trips a `Session` by driving the public state machine on restore
  (so a session saved mid-`awaiting` reloads with its pending ask intact). It
  captures the terminal reason via `RecordedStopReason()` for exact round-trips.

## 12. Reliability — provider resilience

The `port.LLMProvider` seam is wrapped by a **decorator**,
`llmresilience.Wrap(inner, Config) port.LLMProvider`, so the loop is unchanged.
It adds retry with exponential backoff and a circuit breaker, configured in
`mecated` via `--llm-max-attempts` / `--llm-per-attempt-timeout` /
`--llm-breaker-threshold` / `--llm-breaker-cooldown`.

Its load-bearing invariant is **no replay after the first chunk**: retries happen
only while *establishing* the stream (connect + first chunk). Once the first
`Chunk` has been yielded, the decorator never re-issues the call, so the model
never re-sees a half-streamed turn. The breaker opens after N consecutive
**transient** establishment failures (rate-limits, timeouts, 5xx, network);
permanent client errors (4xx other than 408/429) and caller cancellations don't
count. It short-circuits with a `BreakerError` until its
cooldown half-opens it; exhausted retries surface as an `ExhaustedError`. Both
flow back to the client as a terminal `result` event — `session.ResultPayload`
now carries an **`Error`** field, so a provider failure is reported to the caller
rather than swallowed.

Auto-resume complements this: `GetSession`/`Approve`/`Cancel` fall back to
`SessionStore.Load`, and the service persists at create, on entering `awaiting`,
and at run end. With `--store-dir` (jsonlstore) a session survives a restart and
is loadable; the in-flight *stream* itself is not resumed (the `*agent.Run` is
in-memory), and an approve/cancel against a stored-but-runless session returns
`ErrNoActiveRun` (HTTP 409 / gRPC `FailedPrecondition`).

## 13. Context management — tokens & the compaction cascade

Two seams keep a long run inside the model's context window:

- **`TokenCounter`** (`agent/tokencount.go`) estimates message-slice token cost.
  The default `HeuristicTokenCounter` (≈chars/4) needs no dependencies; the
  offline **`tokenizer.Counter`** (`internal/adapter/tokenizer`, tiktoken BPE
  tables embedded — no network, no CGO) is the accurate swap-in, selected with
  `--tokenizer=tiktoken`.
- **`Compactor`** (`agent/compaction.go`) compresses the conversation once it
  crosses the trigger ratio. The default `HeuristicCompactor` is single-summary:
  it preserves the goal + touched file paths, truncates large tool bodies, and
  keeps the last N messages. The swap-in `CascadeCompactor` (`agent/cascade.go`,
  `--compaction=cascade`) runs a **cheapest-first tiered cascade** —
  snip → strip tool bodies → collapse large file bodies → summarize — stopping as
  soon as the slice fits the token budget, with trigger/target **hysteresis** so
  it does not thrash near the threshold.

  Both compactors **snap the kept-tail boundary past leading tool results** (the
  shared `snapCutToTurnBoundary` helper) so the preserved tail never STARTS on a
  `RoleTool` message whose matching assistant tool call was dropped into the head —
  an orphaned tool result draws a provider HTTP 400 on replay. As a final guard
  each compactor **self-validates** the assembled slice with
  `session.ValidateToolPairing` (bidirectional: no orphaned results, no dangling
  calls) and, on failure, **aborts to the original history** with the
  `agent.ErrCompactionWouldOrphan` sentinel; the loop treats it like any other
  compaction failure (keep the uncompacted history, WARN, continue). The aggregate
  itself backstops this: `Session.ReplaceHistory` rejects an unpaired slice.

## 14. Memory — cross-session recall & consolidation (pattern 3 / 4)

`tool.MemoryStore` (`Remember`/`Recall`/`List`/`Forget`) is the seam for
conservative, **per-project** memory. The file-backed `internal/adapter/memory`
implementation persists entries scoped to a project directory and exposes them to
the model as the **Remember** and **Recall** tools (opt-in via `memory.Register`,
`--memory-dir`). On top of it, `internal/adapter/dream` is an opt-in background
**consolidation** ("sleep") service: `dream.Consolidator` distills the stored
memory with an LLM call — merging duplicates and dropping stale entries — but is
deliberately conservative (it never invents keys and is fail-safe on error), run
once or on a ticker via `RunPeriodically` (`--memory-consolidate-interval`).

**User model (issue #14 Phase 2).** A SECOND `memory.Store` — user-scoped and
**cross-project** (`<xdg>/mecatl/usermodel`, distinct from the per-project store) —
holds durable FACTS about the operator. It is exposed (2a, default-on) as the
**RememberUser/RecallUser/SearchUserModel** tool family (the parameterized memory tool
structs, not duplicates) under an enforced `user/` key prefix, plus a turn-0
`<user-model>` block (`prompt.UserModelAssembler`, injected LAST — soul → memory index →
user model). RememberUser injection-scans the value AND the effective description at write time (`skills.ScanForInjection`) and rejects the `</user-model>` fence close-tag in either,
guarding the block against transcript-sourced poisoning. An OPT-IN (off by default,
`--user-model-review`) Stop-triggered background reviewer (`agent.UserModelReviewer`,
wired via a composition-layer Stop-hook decorator) re-reads a finished session's
transcript and extracts operator facts via a FRESH single-shot child — it **never
reopens the user's terminal session** (R10). A `--user-model-consolidate-interval`
drives a separate `dream.Consolidator{Prefix:"user/"}`. The user-model is a writable
instruction FRAGMENT of FACTS, NEVER a governance scope; behaviour comes from the soul +
system rules, not this block.

## 15. Parallelism — fork-join (pattern 8)

`tool.WorkspaceForker` (`tool/isolation.go`) is the workspace-isolation seam:
`Fork(ctx, base, label)` returns an isolated child `Workspace` plus a cleanup
func. The default `internal/adapter/forker` picks its strategy per base —
a **git worktree** (`git worktree add --detach … HEAD`) when the root is inside a
repo, else a **recursive copy** — so a child can never write back into the
parent's tree. `agent.NewParallelTool(childEngine, forker, …)` is the fan-out tool
(catalog name `Parallel`): it runs several isolated child loops on independent
branches and joins their results. It is opt-in via `--enable-parallel`; like Subagent,
the children's intermediate events are drained internally.

The same seam serves **agent teams** (`agent.Supervisor`/`TeamTool`) with a
**three-tier** member workspace policy. A Mutating member forks **force-copy** (own
`.git`, via `forker.WithForceCopy`) and gets Edit/Write/Bash; a read-only member the
factory marks `MemberBuild.IsolateReadOnly` forks **worktree** (the forker DEFAULT —
shares the base repo's `.git`, so it sees full history) and gets Read/Grep/Glob +
Bash but never Edit/Write, so it can `git log`/`git show`/build/test confined to a
throwaway checkout; a base-sharing read-only member (no forker wired) gets NO shell.
The Supervisor holds two forkers (`s.forker` force-copy, `s.roForker` worktree); the
mutating-tool backstop gates on base-sharing, exempting any isolated member. A
read-only member's Bash runs through a **sandboxed** runner
(`buildSandboxedCommandRunner`) that neutralises git config-driven code execution in
the shared `.git`. The single neutralizing env lives in the stdlib-only leaf
`internal/adapter/gitenv` (`Scrub`) so the **forker's own git** (whose `git worktree
add` would otherwise fire the base repo's `post-checkout` hook at fork time) and the
member runner share it and can't drift: `Scrub` drops inherited `GIT_*` danger
(`GIT_EXTERNAL_DIFF`/`GIT_SSH_COMMAND`/…) and forces `core.hooksPath=/dev/null`,
`core.pager=cat`, `core.fsmonitor=false`, empty `diff.external`, `GIT_PAGER`/`PAGER=cat`,
`GIT_CONFIG_NOSYSTEM`. The main session keeps its unhardened runner. **The Subagent
subagent (§8) shares this exact treatment**: when Bash is configured `SubagentTool` holds
its own worktree forker (`WithChildForker`) and forks each child into a throwaway
worktree, with the SAME `buildSandboxedCommandRunner` + `gitenv` hardening and the
SAME untrusted-`.gitattributes` residual; the workspace-trust gate is the shared
follow-up for both.

A team's **returned deliverable** is the **lead's consolidated synthesis**, not a
concatenation of member `LastText`: after the scheduling loop, `Supervisor.Run` drives
ONE final synthesis turn on the lead whose output is `TeamOutcome.Report` (the Team
tool's `ToolResult`; the gRPC `RunTeam` carries it on the outcome). The synthesis
prompt reads three fenced-UNTRUSTED layers — the **findings ledger** (members append
with the `RecordFinding` tool, the primary channel), a **LastText/completed-task
digest** for non-recording members, and the **lead's inbox** — never the members' full
transcripts (context isolation holds). Member sessions persist to the `port.SessionStore`
under `MemberSessionID(teamID, member)` (`team-<teamID>-<member>`, collision-free across
concurrent teams); the parent catalog's read-only **`InspectMember`** tool pulls ONE
member's bounded transcript on demand (PULL — never auto-injected). A `team.findings`
event projects the ledger onto the stream, mirroring `team.tasks`.

## 16. Extensibility — MCP, tools & progressive disclosure

The `tool.Catalog` is the single registration seam, so every tool — core, remote,
or generated — is one uniform `tool.Tool`.

**MCP client** (`internal/adapter/mcp`) — remote tools register here. The
transport is **streaming-HTTP only** (the project's hard constraint): the
stdio/command transport is never used, so no MCP server is ever `os/exec`-spawned.
`mcp.Connect` / `mcp.NewManager` dial the configured servers, and the discovered
tools are registered into the catalog **namespaced** `mcp__<server>__<tool>` so a
remote tool can never collide with or shadow a built-in.

**Progressive tool disclosure** (pattern 9) — a tool may optionally implement
`tool.Disclosable`; the built-in `tool.Search` tool (catalog name `ToolSearch`,
`tool.NewToolSearch`) hydrates hidden tools on demand by searching the catalog. A
tool that does not implement `Disclosable` is always listed, so this is opt-in and
backwards-compatible (gated by the `ProgressiveTools` flag on the Engine `Deps`).

**Skills** (`internal/adapter/skills`) — pattern 9 applied to *instructions*
instead of tool schemas. A skill is a progressive-disclosure instruction unit: a
`SKILL.md` file with YAML frontmatter (`name` + `description`) and a markdown
body, laid out as `<skills-dir>/<name>/SKILL.md` (matching the Agent Skills
ecosystem; see the format references under `docs/examples/skills/`).
`skills.Discover` scans the directory and parses each file into a pure
`skills.Skill` value object; discovery is forgiving — a malformed or
frontmatter-less file is **skipped and reported** (`skills.SkipError`), never
fatal, and a kept skill whose **always-in-context description** exceeds a cap
(`maxDescriptionBytes`) is rune-safe truncated with a warning (so one oversized
description cannot bloat every request and break the byte-stable prompt prefix);
an oversized body is flagged too (it is truncated on activation). A single
read-only `Skill` tool (`skills.NewTool`, catalog name `Skill`,
`ReadOnly()==true`) exposes them: its `Spec().Description` **enumerates every
discovered skill's name + one-line description** — the cheap, always-in-context,
cache-stable metadata layer — while `Execute({name})` returns that skill's full
**body** only when the model activates it (the load-on-activation layer). Because
the tool is read-only it is also available in plan mode. The tool is registered
**only when at least one valid skill is discovered** — an empty inventory
advertises nothing.

The discovered set is *also* projected into a server-side inventory snapshot
(`internal/app.skillSnapshot`, name-sorted, name+description only — no body),
carried on `server.Config.Skills` and served read-only by the **`ListSkills`
RPC** (`HarnessService.ListSkills` / `GET /v1/skills`). It mirrors `ListAgents`
rather than `ListCommands`: skills are discovered once at build time and
immutable for the process lifetime, so the snapshot is a pure read, never a live
re-scan. The mecatui TUI consumes it for the `/skills` browser panel (gated on
`caps.Skills` plus a wired `client.SkillLister`); activation stays the model's
concern, so the panel is discovery only.

*Where skills come from* is itself a seam: `skills.Source`
(`Skills(ctx) ([]Skill, []SkipError, error)`) is the **pluggable extensibility
point**. `skills.DirSource{Dir, Label}` is the default local-filesystem
implementation (the `<dir>/<name>/SKILL.md` layout); `skills.MultiSource`
composes an **ordered** list of sources with a defined precedence — **earlier
source wins** on name collisions, the loser dropped with a "shadowed by a
higher-precedence source" `SkipError`. A future embedded-defaults or remote
registry source just implements `Source` and slots into the `MultiSource`; the
consumer (`skills.RegisterSource`) is unchanged. The Source seam lives in the
**adapter** package, NOT the domain: nothing in the domain or the agent loop
consumes skills (they are packaged into a `tool.Tool` at composition time), so a
domain port would be the wrong home — the seam is scoped to where it is consumed,
mirroring MCP and the TUI theme search-path.

**The self-improving skill loop** (`skills.Drafter`, opt-in) closes the loop so
durable skills can *come into being from the agent's own experience*. A single
writable tool, **`SkillDraft`** (`skills.NewDraftTool`, catalog name `SkillDraft`,
`ReadOnly()==false`), lets the model PROPOSE a skill; its `skills.Drafter` write
seam (mirroring `Source`, in the adapter package — nothing in domain/agent consumes
or produces skills) validates and sanitizes the untrusted candidate and writes it
to a **quarantine directory that is NEVER registered as a catalog `Source`**. The
default `DirDrafter` is fully offline: it reuses `parseSkill`/`validateName`, an
exported injection scan (`ScanForInjection`, run on both the always-in-context
description and the body), a name regex (lowercase Agent-Skills style, blocking
traversal), the existing size caps, a path-containment assert, an atomic
temp+rename write, and an offline **2-gram Jaccard** novelty check
(`Jaccard2Gram`) that *warns* (never blocks) on near-duplicate descriptions. Every
quarantined `SKILL.md` is provenance-stamped (`origin: model`, `drafted_at`) for the
reviewer; `parseSkill` ignores those keys so they never reach context. **The trust
boundary** (stated in `skills/doc.go`): the model can author a candidate but can
never activate its own proposal in any session. It rests on two invariants, both
enforced in `cmd/mecated` (`validateSkillDraftConfig`, fatal on a misconfig):
(1) the quarantine dir must live **outside the workspace root**, so the model's
workspace-confined `Write`/`Edit` structurally cannot reach it — a candidate only
ever enters quarantine via the `Drafter`; and (2) the dir must be **disjoint from
every active skills dir**. Promotion from quarantine to an active `--skills-dir` is
an **operator** action (`skills.Promote`, the `mecated skills promote` subcommand),
which **shows the full candidate, requires confirmation** (`--yes` for scripted use),
**verifies `origin: model` provenance**, and re-runs structural validation + the
injection scan before moving it (refusing to overwrite an existing name). The
convention is **author in session N → operator promotes → active in N+1**: drafts
never enter the live catalog or perturb the byte-stable prompt prefix (it is built
once at startup from operator-trusted sources only). `SkillDraft` is opt-in via
`--skills-draft-dir` (empty ⇒ tool not registered, like `--memory-dir` gating
Remember). **Residual** (documented, not hidden): absent the deferred OS sandbox the
`Bash` tool can write to any path, so the structural boundary covers `Write`/`Edit`
only — `mecated` warns when `SkillDraft` and `Bash` are enabled together; the fully
structural deployment is shell-less or sandboxed. `SkillDraft` itself defaults to **ask** so a
human reviews authorship, and being mutating it is filtered out of plan mode.

It stays **opt-in**: `mecated` wires it via a repeatable `--skills-dir`
(highest precedence) and an opt-in `--skills-conventional` that adds Claude-Code-
style **known paths** (`skills.ResolveSources`): project-level
`<workspace>/.mecatl/skills` and `<workspace>/.claude/skills`, then user-level
`$XDG_CONFIG_HOME/mecatl/skills` (or `~/.config/mecatl/skills`) and `~/.claude/skills`,
with precedence **explicit > project > user**. With neither flag set, the resolver
yields no sources and nothing is read. Discovery (reading files, YAML parsing via
`go.yaml.in/yaml/v3`) is an adapter concern; nothing in this package is imported
by a domain package — it merely implements the domain `tool.Tool` interface.

### Seam summary

Every capability above is a default-on (or opt-in) interface; the core never
changes when one is swapped:

| Seam | Where | Default → swap-in |
|---|---|---|
| `port.LLMProvider` | `internal/port/llm.go` | `openai`/`mockllm`; decorated by `llmresilience`; other vendors slot in unchanged |
| `port.PermissionPolicy` | `internal/port/permission.go` | `permpolicy` (layer-1 rules), optionally decorated by `permclassify` (layer-2 model classifier) |
| `Compactor` | `agent/compaction.go` | `HeuristicCompactor` → `CascadeCompactor` |
| `TokenCounter` | `agent/tokencount.go` | `HeuristicTokenCounter` → `tokenizer.Counter` |
| `InstructionAssembler` | `prompt/instructions.go` | `RootAssembler` (AGENTS.md/CLAUDE.md) → `MultiAssembler` composing `RootAssembler` → `SoulAssembler` (persona) → `MemoryIndexAssembler` (saved project facts) → `UserModelAssembler` (operator FACTS), all as turn-0 user messages |
| `prompt.SoulSource` | `prompt/soul.go` (impl `internal/adapter/soul`) | nil (off) → `*soul.Store`; agent-READ-ONLY (no write path), env-injected (not the WorkspaceReader — the file is outside any session root), injection-scanned + byte-capped, fail-soft; on by default, `--soul-file`/`--no-soul`. **Two provenances + trust gate (issue #14, Phase 3, Item 2):** a USER soul (`<xdg>/mecatl/soul.md` or `--soul-file PATH`) is always trusted; a PROJECT soul (a discovered `<workspace>/.mecatl/soul.md`, parallel to `.mecatl/settings.yaml`) is **untrusted by default** and honoured only with `--trust-project` (the SAME issue-#13 gesture — not a new flag, not routed through governance: the soul is fenced DATA). **USER-WINS precedence** (single identity anchor, not a merge): a present user soul is used and the project soul is ignored; an untrusted project soul is dropped + `slog.Warn`-logged. The selection (provenance/trusted/drift metadata) lives in `internal/app/soulselect.go`; `internal/prompt` stays trust-unaware. **Drift baseline (Item 1):** `soul.LoadWithMeta` computes the sha256 of the clean body in the same read; `internal/app/soulguard` records it as a harness-owned sidecar `<soulPath>.sha256` trust-on-first-use (against WHICHEVER soul wins), `slog.Warn`s on a later mismatch, and (with `--soul-strict`) drops a drifted soul. `--approve-soul` re-baselines. The WRITE lives ONLY in the composition layer — the adapter stays write-free. |
| `prompt.UserModelSource` | `prompt/usermodel.go` (impl `internal/adapter/memory`) | nil (off) → a SECOND, user-scoped, **cross-project** `*memory.Store` over `<xdg>/mecatl/usermodel`; durable operator FACTS exposed as RememberUser/RecallUser/SearchUserModel (enforced `user/` prefix; write-time injection scan) + the turn-0 `<user-model>` block; on by default, `--user-model-dir`/`--no-user-model`. Writable FACTS, not a governance scope. OPT-IN Stop-triggered reviewer via `--user-model-review` (never reopens the user session) |
| `CommandExpander` | `prompt/command.go` | `NoopExpander` → `DirCommandExpander` (slash commands) |
| `tool.Disclosable` + `ToolSearch` | `internal/tool` | always-listed → progressive disclosure |
| `Skill` tool (skills) | `internal/adapter/skills` (impl) | off → opt-in `--skills-dir`; progressive disclosure of *instructions* (metadata always in context, body on activation) |
| `skills.Source` | `internal/adapter/skills/source.go` | `DirSource` (one dir) → `MultiSource` (ordered, earlier-wins); known-path resolver (`--skills-conventional`: project `.mecatl`/`.claude`, user XDG/`~/.claude`); future embedded/remote sources slot in |
| `skills.Drafter` (self-improving loop) | `internal/adapter/skills/drafter.go` | off → opt-in `--skills-draft-dir`; default `DirDrafter` (offline: validate/sanitize/2-gram-Jaccard novelty → out-of-workspace quarantine, NEVER a catalog Source). WRITE side is pluggable (a future LLM-vetting decorator slots in); promotion is filesystem-only in the MVP — operator `mecated skills promote` is the gate (shows content, confirms, verifies provenance; author N → promote → active N+1) |
| `tool.CommandRunner` | `internal/tool/tool.go` (impl `osfs`) | the command-execution chokepoint; an OS sandbox wraps here |
| `tool.MemoryStore` | `internal/tool/tool.go` (impl `memory`) | cross-session memory + `dream` consolidation |
| `tool.WorkspaceForker` | `tool/isolation.go` (impl `forker`) | fork-join isolated branches |
| `tool.Catalog` | `internal/tool/catalog.go` | core tools + MCP (streaming-HTTP) |
| `mcpperf.Deps` (perf MCP server) | `internal/adapter/mcpperf` | opt-in `--perf-mcp`; a read-only MCP `http.Handler` mounted at `/mcp` on the loopback admin listener (both composition roots: `cmd/mecated` and `cmd/mecatui/embed`). Built by DI — `Snapshot`/`Gatherer`/`Profiler` from `telemetry`, a slow-turn ring buffer (`telemetry.SlowTurnBuffer`) bridged at the cmd boundary to the `mcpperf.SlowTurnSource` seam (telemetry never imports mcpperf — the dependency points inward). Fail-closed to loopback (unauthenticated) |
| `SessionStore` + AGENTS.md/CLAUDE.md discovery | `port` + `prompt/builder.go` | file-as-memory; AGENTS.md wins over CLAUDE.md, injected as a **user** message, never system |

**Remaining non-goals / deliberate deferrals**: an **OS-level sandbox**
(Landlock/seccomp/Seatbelt) is the one explicitly-deferred item — the
`CommandRunner` seam is the place it wraps, and shell-less deploys avoid the
surface entirely. **stdio MCP is never supported**. Embeddings and multi-vendor model
routing (the `LLMProvider` port already abstracts it) remain unbuilt; **skills**
exist as progressive-disclosure instruction units (see above), though richer
*packaging* (bundled scripts/resources alongside `SKILL.md`) is not yet built. The guiding restraint still holds: build the shape, instrument it,
and resist features before the loop, tools, permissions, hooks, and cache all work.

## 17. Deployment & server hardening

The server (`internal/adapter/server`) is hardened for off-loopback operation,
and `cmd/mecated` wires the knobs:

- **Authentication** — optional bearer token (`--auth-token` / `MECATL_AUTH_TOKEN`,
  constant-time compared) enforced by a gRPC interceptor + HTTP middleware
  (`server/authn.go`); optional **TLS / mTLS** (`--tls-cert` / `--tls-key` /
  `--client-ca`). The server still **warns loudly** if it binds a non-loopback
  address with no auth configured.
- **Rate limiting** — per-client + global token-bucket (`--rate-limit` /
  `--rate-burst`), bounded and idle-evicting.
- **Health** — HTTP `/healthz` (liveness) + `/readyz` (readiness) mounted outside
  auth/rate-limit, plus standard `grpc_health_v1` `SERVING` (`server/health.go`).
- **Graceful shutdown** — gRPC `GracefulStop` + HTTP `Shutdown`.

Deployment artifacts: a hardened **GitHub Actions** CI plus a **ko**-based release
that signs images with **cosign** and emits an **SBOM** and **SLSA provenance**
(`.github/workflows`, `.ko.yaml`); `deploy/` carries PSS-restricted manifests
(health probes can switch TCP→httpGet against the endpoints above).

### Permission & bash governance details (`internal/governance`)

The `permpolicy` adapter wraps `governance.Evaluator`. Resolution
(`evaluator.go`): a **Deny in any scope beats Ask beats Allow**; among rules of
the same effect the highest-precedence `Scope` wins (`Managed > CLI >
LocalProject > SharedProject > User`); **no matching rule defaults to Ask** (the
harness never silently allows an unconfigured call). Plan mode (`ModePlan`)
denies mutating tools (`Edit`, `Write`) and non-read-only `Bash` up front.

The allow-all operator posture (`--yolo`, `app.Config.AllowAllTools`)
is **not** an evaluator bypass: it injects a single `ScopeCLI` allow-all rule into the
**main** engine's static ruleset (`mainRules` in `internal/app/build.go`), which loosens
only the `ScopeBuiltinDefault` mutate-ask floor — deny-dominance and the honouring of any
deliberately configured `Ask` are preserved exactly (see `docs/design/ALLOW-ALL-POSTURE.md`).

For Bash, `bash.go` splits compound lines (`SplitCommands`, honouring quotes and
splitting on `&&`, `||`, `;`, `|`, a bare `&`, and newlines) and evaluates
**every** sub-command, taking the worst outcome — so a deny on `rm` blocks
`git status && rm -rf /`. `Canonicalize` strips a **closed, audited** set of
transparent wrappers (`timeout`, `time`, `nice`, `env`, `stdbuf`, `ionice`) but
deliberately **never** strips re-entrant launchers (`docker exec`, `npx`,
`sudo`, `devbox run`). `HasSubstitutionOrGrouping` flags `$(...)`, backticks,
`<(...)`, and `(`/`{` grouping and floors such segments at Ask (fail-safe).
`ReadOnlyBash` classifies a command line as read-only for plan-mode gating.

### Workspace trust (`internal/app/trust.go`, `internal/adapter/workspacetrust`)

Whether a *project's* contributions are admitted — the **project authority set** —
is a **composition** decision, not a governance scope
(`governance`/`session`/`prompt`/`tool` stay trust-unaware). The project authority
set is: the project permission **ALLOW** rules, the project **soul**, and (Phase 2a)
the **project tier** of agent definitions, slash commands, and skills
(`<workspace>/.mecatl/*`, `<workspace>/.claude/*`). The decision is
produced once per process by `resolveTrust(cfg) TrustDecision` (MUST-FIX 2 of the
workspace-trust design), which folds, highest first:

1. `--trust-project` — the per-invocation operator flag (`TrustFlag`);
2. a **declarative** `trustedWorkspaces:` match (`TrustDeclared`) — Phase 1: the
   `internal/adapter/workspacetrust` leaf reads an operator-authored, **read-only**
   list of absolute workspace paths from the user-global `settings.yaml` (via the
   shared `xdgconfig` env seam) and answers "is this workspace declared-trusted?";
3. a **remembered** `trust.yaml` entry (`TrustRemembered`) — Phase 2b: a
   machine-written registry entry whose stored **identity-anchor hash** still
   matches the workspace's live identity surface. A present entry whose anchor
   **mismatches** ⇒ `Drifted` (and `Trusted=false` — fail-safe);
4. otherwise `TrustNone`.

`Build` collapses `decision.Trusted` back onto `cfg.TrustProject` before the
downstream build, so the existing consumers — `permconfig.Options.TrustProject`
and the soul provenance gate (`soulselect.go`) — honour declared trust through the
**exact same admission path** as the flag, with no adapter signature churn and no
bypass. The composition narrates the decision (`slog.Info "workspace trust"
trusted=… source=… drifted=…`), mirroring the soul-selection narration.

**Phase 2a — the project-tier authority gate.** When the folded decision is
**untrusted**, composition also withholds the **PROJECT TIER ONLY** of
agents/commands/skills, mirroring how the project ALLOWs and project soul are gated:
the `agents`/`skills` adapters gained an additive `ResolveOptions.IncludeProjectTier`
(set to `cfg.TrustProject`; the three `internal/app` skills callers —
`registerSkills`, the agent-def skill-preload `resolveSkillIndex`, and the
draft-overlap `activeSkillDirs` — all pass it), and `buildDirCommandExpander` drops
the default project-tier command dirs (`.mecatl/commands`, `.claude/commands`) when
`cfg.Workspace != "" && !cfg.TrustProject`, degrading to the `NoopExpander` so raw
text still passes through. The **user tier** (`$XDG_CONFIG_HOME/mecatl/*`,
`~/.claude/*`), the built-in tools, the base prompt, every Deny/Ask, the permission
prompt, and any **explicit** `--commands-dir`/`--agents-dir`/`--skills-dir`
(operator-supplied, not repo-injected) stay active — an untrusted repo degrades to
**"ask the human"**, never **"do nothing"**. The project soul carries a **double
gate** (trust provenance AND the `soul:apply` policy — a logical AND, reconciled in
`selectSoulSource`'s doc comment).

**Settings-vs-state split.** `trustedWorkspaces:` is config **DATA**, not a
governance `Rule`, and lives in the **human-authored** `settings.yaml` that mecatl
only ever *reads*. The **machine-written** trust registry (Phase 2b) is a
**separate** file — `<xdg>/mecatl/trust.yaml`, a sibling of but never inside
`settings.yaml`. `workspacetrust` reads it (`Remembered`) and writes it
(`Remember`, the only write path); `mecated` reads it declaratively and **never**
writes or prompts. Each entry is keyed by `realpath` and stores the
**identity-anchor hash** captured at trust time plus a `trustedAt` timestamp (the
timestamp is injected by composition — the adapter never calls `time.Now()`, so the
write is deterministic in tests). The **identity anchor** (`anchor.go`) is a
deterministic fold of the project **soul** ⊕ project-tier **agent** ⊕ **command** ⊕
**skill** definitions (sorted file set, per-file content hashes), and **explicitly
excludes `settings.yaml`** — permissions change every commit, so anchoring drift on
them would nag-fatigue the operator (they re-resolve live via permconfig's mtime
cache instead). A drift (entry present, anchor mismatched) re-gates to untrusted +
`slog.Warn`; the interactive re-prompt is the `mecatui` first-encounter prompt
(Phase 2c, still to come). The registry **write** uses `O_NOFOLLOW` + `0o600` +
temp-then-rename (mirroring `soulguard`'s sidecar write, CWE-59), and a corrupt /
oversized / wrong-version registry fails safe to untrusted. The SHA-256 primitive is
shared with `soulguard` via the `internal/adapter/hashutil` leaf (`SHA256Hex`) —
**only** the primitive is shared; the soul drift baseline (a soul-only `.sha256`
sidecar, re-blessed by `--approve-soul`) and the trust identity anchor (the
registry-stored fold, re-blessed by re-answering the prompt) stay **parallel**
mechanisms. **Path keying** is cleaned + absolute +
symlink-resolved (`filepath.Abs` then `EvalSymlinks`) on both sides, so a
moved/symlinked path cannot forge or inherit trust; an unresolvable entry is
skipped. Trust is **monotonic-positive**: it only ever *grants* admission of a
project's ALLOWs/soul — it never overrides a Deny or a configured Ask (those remain
deny-dominant in the evaluator). A missing/malformed/unparseable `settings.yaml`
**or** `trust.yaml` fails safe to untrusted (a corrupt config never grants trust).
See `docs/design/WORKSPACE-TRUST-SPIKE.md`.

## 18. Multi-provider — registry, per-session routing & model inventory

mecatl can serve more than one LLM provider in one process and bind a **provider +
model per session**. The wiring lives entirely in the composition layer
(`internal/app`); the domain/agent/server never see a registry — they receive a bare
`port.LLMProvider`.

**The registry (`internal/app/registry.go`).** `buildProviderRegistry` constructs,
once at `Build`, the set of AVAILABLE providers — a provider is available iff one of
its credential env vars resolves (the var NAMES come from the embedded models.dev
catalog, `internal/adapter/providercatalog`; `OPENAI_API_KEY`/`OPENROUTER_API_KEY`/`ANTHROPIC_API_KEY`).
Only available providers are held (an unkeyed provider is omitted — its availability
is itself sensitive, CWE-200). OpenRouter rides the SAME stateless openai adapter with
the OpenRouter base URL substituted. **Anthropic (P1) is the first native non-OpenAI
wire adapter** (`internal/adapter/anthropic`, on the official MIT `anthropic-sdk-go`):
the native Messages API, also STATELESS full-replay, wired via `newAnthropicEntry`. It
validated the provider abstraction — it shipped with NO domain/agent/server/acp/proto
edit; `engineDepsForProvider`, per-session routing, the capability intersection, and
per-sub-agent-provider switching all treat it as data. Its wire-divergences (the
REQUIRED `max_tokens`, the model-class-dependent extended-thinking config which is ON
and model-aware, and the `(thinking,signature[],redacted)` reasoning-replay list packed
into the opaque `Message.Reasoning` STRING) are absorbed at adapter-construction, not in
the DTO. `UseMock` short-circuits to a single synthetic
`mock` entry (offline). The zero-keys case is the named, actionable `errNoProvider`.
`buildProvider` returns the registry **and** its default provider so the shared engine
+ every child/fork/team engine keep receiving the single default provider exactly as
before (the default path is byte-identical). A composition-only `providerConstructor`
seam (mirroring `envDetector`) lets the offline e2e back two real provider ids with
mocks; production leaves it nil.

**Per-session routing (`sessionEngineFactory`).** `CreateSession` carries an OPTIONAL
`provider_id`/`model_id` selector, expressed at the server boundary as the NEUTRAL
`server.ProviderSelector` (the server adapter imports neither the registry nor the
catalog). The widened `SessionEngineFactory func(ctx, sel, specs)` is the ONE seam for
a per-session engine — it serves BOTH a non-default provider/model AND client-provided
MCP servers (orthogonal inputs → ONE engine over ONE catalog). The composition factory
resolves the selector against the registry and builds Deps via
**`engineDepsForProvider`**, which re-derives EVERY provider/model-closing field
(LLM, Compactor, Model, model-keyed TokenCounter, `PromptConfig.Env.Model`, and the
**ContextWindowTokens** — looked up from the catalog's `ContextLimit()` for the
selected model so the compaction trigger AGREES with the `ListModels`-advertised
`context_limit`; an uncatalogued passthrough model or the default provider falls back
to the 128k default). This is the contamination fix: a shallow clone swapping only the
LLM would compact/count through the wrong model. The resolution table:

| `provider_id` | `model_id` | Outcome |
|---|---|---|
| `""` | `""` | **Shared engine** (default provider, no per-session build) — today's path |
| `""` | set | **InvalidArgument** — a bare model on the env-derived default provider is ambiguous |
| known+available | `""` | per-session engine on that provider's default model |
| known+available | catalogued | per-session engine bound to (provider, model) |
| known+available | NOT catalogued | **passthrough** — the model string reaches the provider verbatim (catalog gates nothing) |
| unknown/unavailable | any | **InvalidArgument** — `"unknown or unavailable provider"`, never a silent fallback |

The provider is **fixed for the session lifetime** (reasoning-replay + the byte-stable
prefix are provider-private; "switch provider" = new session). `session.Session` is
NOT widened — the selector resolves to an ENGINE at create time, registered in the same
`sessionEngines` map (and selected the same way by `StartRunContent`) the client-MCP
path uses; `loadAndReopen` is untouched. That map is **capped** at
`Config.MaxSessionEngines` (default 1024): the gRPC/HTTP surfaces have no
connection-teardown drain, so without a cap a client creating selector sessions and
never calling `CloseSession`/`EndSession` could grow it unbounded (CWE-770). Past the
cap, `createSession` returns `ErrTooManySessionEngines` (gRPC `ResourceExhausted` /
HTTP 429); `CloseSession`/`EndSession` frees a slot. (Keys are NEVER on the wire — only
the provider id.)

**Model inventory (`ListModels` / `internal/app/modelsnapshot.go`).** `modelSnapshot`
joins the registry's AVAILABLE providers to the embedded catalog and projects each
model into the proto `ModelInfo` (public metadata only — id, provider_id, display_name,
image/reasoning flags, context_limit — never a key/env/base-URL). The composition root
injects the snapshot into `server.Config.Models`; the server adapter holds only the
proto slice (mirroring the `ListAgents` idiom). The `mock` provider advertises no
selectable models. `ServerCapabilities.model_selection` is true iff the snapshot is
non-empty, gating the client's model picker the way `agents` gates `/agents`. Provider
key/base-URL flags landed in `cmd/mecated` earlier; the picker UX is a client concern.

**Capability single-source (`internal/app/capability.go`).** A model's true input
capability is the INTERSECTION `catalog-per-model-modalities ∩ adapter-Capabilities()`,
computed by `modelCapability` in composition (the only layer holding both inputs). That
ONE neutral `port.ProviderCapabilities` feeds three sinks so they cannot disagree:
`ModelInfo.image` (ListModels), the `CreateSessionResponse.session_capabilities` echo
(per-session), and the ACP gate (`Service.ProviderCapabilities()`, the default caps).
The server/acp adapters receive only the computed value — no catalog/registry type
crosses inward. Keys are never on the wire — only the provider id.

**Per-sub-agent provider (shipped).** A Subagent agent def or team member may pin a
`provider:` (orthogonal to `model:`) to run its child engine on a DIFFERENT provider
than the parent, and a provider-selected session propagates its provider to the
sub-agents it spawns (which it now CAN — Half B builds it a per-session Subagent/Team
tool). Precedence: `def.Provider > session-selected provider > build-time default`;
every child routes through `engineDepsForProvider` so it never contaminates the
parent's compactor/counter. Composition-only — the registry never reaches the child
engine (a bare `port.LLMProvider` is handed down).

**Full design: see `docs/design/MULTI-PROVIDER.md`** (registry, catalog-as-data, DTO
neutrality, selection primitive, per-session engine, capability intersection,
disclosure posture + per-client key custody, per-sub-agent provider, and the P0→P3
phasing).
