# ADR 0001 — Agent Client Protocol (ACP) adapter

- Status: Accepted
- Date: 2026-05-30
- Scope: Phase 1 (core loop) + Phase 2 (fidelity — diff blocks, subagent/team/hook
  projection) + Phase 3 bounded pieces (commands, set_mode, load — see "Phase 3
  (bounded)"). The Phase 3 long-tail (fs/\* delegation, governance-rule persistence,
  image/audio) remains (see "Deferred"). The hook-call-id veto (issue #6) is now
  DONE — a blocked PreToolUse veto keys a `failed` `tool_call_update` onto its card.

## Context

ACP editors (Zed, and others) drive an agent as a **subprocess** speaking
**JSON-RPC 2.0 over stdio**. We want such editors to drive the mecatl harness.

mecatl already exposes its agent loop through a surface-agnostic application
service — `internal/adapter/server.Service` — which the gRPC and HTTP/SSE adapters
both consume, and which `internal/app.Build(ctx, Config)` assembles (provider,
catalog, permission policy, store, MCP, skills). ACP is a third wire surface over
that same service.

ACP lifecycle (the subset Phase 1 implements):

- client→agent **requests**: `initialize`, `session/new {cwd}`, `session/prompt
  {prompt:[ContentBlock]}` (BLOCKS, returns `{stopReason}`).
- client→agent **notification**: `session/cancel`.
- agent→client **notification**: `session/update` (the streaming surface:
  `agent_message_chunk`, `agent_thought_chunk`, `tool_call`, `tool_call_update`).
- agent→client **request**: `session/request_permission` → `{outcome}`.

So the adapter is BOTH a JSON-RPC server (inbound) AND a client (it issues the
outbound `request_permission` and correlates the reply).

## Decision

1. **New adapter `internal/adapter/acp/`, entered via `mecated --acp`.** The flag
   makes `mecated` serve ACP over stdin/stdout instead of starting the TCP/HTTP
   listeners. It reuses `app.Build` → `*server.Service` unchanged; only the wire
   surface differs. No TLS/auth/rate-limit — stdio is a local, parent-process
   trust boundary (the editor spawned us). The non-`--acp` path is untouched.

2. **ACP is a THIRD wire format — its own JSON, NOT the proto.** The adapter
   never imports `contracts/gen`. It (de)serializes the ACP schema directly
   (`internal/adapter/acp/types.go`), decoupled from the gRPC contract, so the
   two wire formats evolve independently. A `projector.go` (parallel to the
   server adapter's `mapper.go`) translates domain `session.Event`s to ACP
   `session/update` payloads.

3. **Framing: Content-Length headers** (`Content-Length: N\r\n\r\n<N bytes>`),
   the LSP/ACP norm, NOT newline-delimited JSON. Chosen because it matches the
   reference ACP/LSP implementations and tolerates raw newlines in a JSON body.
   The codec (`conn.go`) is bidirectional: it reads inbound requests/
   notifications and routes them to a handler, AND issues outbound requests with
   response correlation (a pending-id → channel map) for `request_permission`.

4. **Four scoping decisions taken this phase:**
   - **fs/\* delegation DEFERRED.** We advertise `clientCapabilities` are
     unused and rely on mecatl's OWN `osfs` workspace rooted at the session
     `cwd`; the agent does not call the client's `fs/read_text_file` /
     `fs/write_text_file`. (We do not depend on the client filesystem at all.)
   - **`allow_always` behaves as `allow_once`.** There is no rule persistence
     yet, so every approval is one-shot. The four ACP option kinds are still
     offered; `allow_always`/`allow_once` both map to `run.Approve(askID, true)`.
   - **diff content blocks** (DONE in Phase 2 — see below). Phase 1 carried only
     plain-text `content`; Phase 2 attaches a structured `diff` block to Edit/Write
     tool calls.
   - **entry = `mecated --acp`.** (Not a separate binary.)

## Capabilities advertised (`initialize`)

- `protocolVersion: 1`.
- `promptCapabilities`: `image:false`, `audio:false`, `embeddedContext:false`
  (text content blocks only this phase).
- `mcpCapabilities`: `http:false`, `sse:false`. mecatl is **streaming-HTTP MCP
  only** (CLAUDE.md: "No stdio MCP, ever"); client-provided MCP servers in
  `session/new` are REJECTED — a stdio server (command, no URL) is hard-rejected,
  and even an HTTP one is rejected pending the deferred client-MCP work.
- `loadSession`: reflects whether a durable session store is configured. `mecated
  --acp` sets it `true` only when `--store-dir` is given (the in-memory store would
  lose snapshots across a restart); otherwise `false`. The flag is threaded into the
  adapter via `acp.WithResume(bool)` (Phase 3).
- `modes`: the session reflects mecatl's `default`/`plan`/`acceptEdits` modes; the
  `currentModeId` is the session's CURRENT mode (Phase 3 — was always `default`),
  and `session/set_mode` switches between them.

## Event projection (Phase 1 + Phase 2)

| domain `session.Event`        | ACP                                                            |
|-------------------------------|----------------------------------------------------------------|
| `EvMessageDelta`              | `session/update` `agent_message_chunk{content:text}`           |
| `EvReasoningDelta`            | `session/update` `agent_thought_chunk{content:text}` (summary only — never the encrypted replay blob) |
| `EvToolCall`                  | `session/update` `tool_call{toolCallId,title,kind,rawInput,status:pending}` — for Edit/Write a `diff` content block is attached (Phase 2) |
| `EvToolResult`                | `session/update` `tool_call_update{toolCallId,status:completed\|failed,content:[text]}` |
| `EvHook`                      | blocked **PreToolUse** w/ call id → `tool_call_update{toolCallId,status:failed,content:reason}` on the card opened before the gate (issue #6); PostToolUse blocks + all others → `agent_thought_chunk{content:reason}` |
| `EvSubagentTool`/`EvSubagentEnd` | `session/update` `tool_call_update` on the PARENT Task call id (Phase 2) |
| `EvTeamMember`/`EvTeamEnd`    | `session/update` `tool_call_update` on the PARENT Team call id (Phase 2) |
| `EvPermissionAsk`             | OUTBOUND `session/request_permission` (4 options); reply → `run.Approve` |
| `EvResult`                    | the prompt's return `{stopReason}` |

Stop-reason mapping: `StopEndTurn→end_turn`; `StopCancelled→cancelled`; the three
limit reasons (`StopMaxTurns`/`StopMaxToolCalls`/`StopMaxConsecutiveFailures`)→
`max_turn_requests`; `StopError→end_turn` (a clean terminal — the error text
still reaches the editor via the preceding message chunks).

**Dropped / folded**: `session.init`, `turn.start`, `turn.end`, `compaction`,
`subagent.start`, `team.start`. The two `.start` events are dropped because the
parent `tool_call` (Task / Team) already names the delegated work; the roster/goal
would add noise before the first progress line.

## Phase 2 fidelity

1. **Diff content blocks.** An Edit or Write `tool_call` carries an ACP `diff`
   content block `{type:"diff", path, oldText?, newText}` synthesized from the
   tool-call args, so editors render a native inline diff at the moment the call is
   shown (before it even runs). Mapping: **Edit** → `oldText=old_string`,
   `newText=new_string`, `path=path`; **Write** → `newText=content`, `oldText`
   OMITTED (ACP: an absent `oldText` means a new/overwritten file). The Edit/Write
   arg shapes are mirrored as tiny local structs in `projector.go` — the projector
   must not import the tools adapter. **Malformed args (bad JSON, or no `path`) →
   no diff (nil content)**, and the editor still gets the plain-text result on the
   later `tool_call_update` (graceful fallback). The tool-`kind` mapping
   (`toolKindFor`) covers Read/Grep/Glob→`read`, Edit/Write→`edit`, Bash→`execute`,
   WebFetch→`fetch`, Task/Team/Fork→`think`, everything else (incl. MCP)→`other`.

2. **Subagent + team fidelity.** Phase 1 DROPPED `subagent.*`/`team.*`. Phase 2
   projects them as `tool_call_update` **progress on the PARENT tool_call** keyed by
   `SubagentPayload.ParentCallID` / `TeamPayload.ParentCallID`: each `subagent.tool`
   / `team.member` appends an `in_progress` content line (the child tool name, or
   the bounded member activity — already redacted/bounded at source), and
   `subagent.end`/`team.end` finalize the parent call (`failed` if the child stopped
   on error, else `in_progress` — the parent's own `tool_call_update` carries the
   terminal `completed`). A `subagent.tool`/`team.member` with no parent id, or a
   `team.member` with nothing to show, is dropped.

3. **Hook events.** A blocked **PreToolUse** veto that carries the originating
   tool-call id (`HookPayload.CallID`, added for issue #6) projects to a `failed`
   `tool_call_update` keyed by that id, so the veto lands ON the tool card. This is
   safe because the loop now opens the `tool_call` card BEFORE the permission/hook
   gate (`openCard` in `internal/agent/dispatch.go`), so the id is always one the
   client has already seen — the earlier "phantom card" risk (an update for an
   unopened id) is gone. The PreToolUse block also emits a synthesized error
   `EvToolResult` on the same id, which projects to its own `failed` update; both
   settle the same already-open card harmlessly. A blocked **PostToolUse** hook
   stays an `agent_thought_chunk` annotation by domain semantics: the tool already
   ran and its (successful) `EvToolResult` settles the card, so failing the card
   would wrongly overwrite a completed result. Every other hook (a prompt/arg
   rewrite, a Stop notice, a hook with no call id) is likewise a thought chunk.
   Because the acp package must not import `internal/governance`, the phase guard
   compares `HookPayload.Phase` against the string `"PreToolUse"`.

4. **Modes + commands.** Phase 2 deferred these; the bounded pieces landed in
   Phase 3 below (`available_commands_update`, `session/set_mode` +
   `current_mode_update`, `session/load`).

## Phase 3 (bounded)

Three bounded pieces landed, each reusing an existing `*server.Service` seam — NO
proto change (ACP is its own JSON), confined to `internal/adapter/acp/`, a small
`server.Service` seam, one `session.Session` method, and the `mecated --acp`
wiring.

1. **`available_commands_update` on `session/new`.** After creating the session the
   adapter calls `Service.ListCommands(ctx, cwd)` (the SAME seam the gRPC
   `ListCommands` RPC uses — a composition-injected `CommandLister` that closes over
   the run-path command expander) and maps each `{name, description}` to an ACP
   `AvailableCommand`, sent as a single `available_commands_update` session/update.
   It is best-effort: a nil lister, an empty result, or a discovery fault yields NO
   update (the palette stays empty) — command discovery never breaks session
   creation. The `input.hint` field is omitted (mecatl commands take free-form text).

2. **`session/set_mode` + `current_mode_update`.** Inbound `session/set_mode
   {sessionId, modeId}` maps the modeId (mecatl's own `default`/`plan`/`acceptEdits`
   strings, advertised verbatim as the `availableModes` ids) to a `PermissionMode`
   and applies it via the new `Service.SetMode(ctx, id, mode)` seam, which mutates
   the session through a new guarded aggregate method `Session.SetMode` and
   persists. On success the adapter emits a `current_mode_update` so the editor's
   picker reflects the new selection; the result is the empty `SetSessionModeResponse`
   (`{}`). The `currentModeId` on `session/new`/`session/load` now reflects the
   session's actual mode.

   **Constraint — mid-turn switch scoped out.** `Session.SetMode` is legal only when
   the session is NOT actively progressing a turn (idle or terminal), NOT while
   `running`/`awaiting`: changing the posture mid-turn would race the loop's own
   permission evaluation (plan hard-denies mutations; acceptEdits auto-allows them)
   against dispatch already in flight. A mid-turn `set_mode` is therefore rejected
   (`ErrIllegalTransition`, surfaced as an `InvalidParams` error); a client must
   defer it to the next prompt. `Service.SetMode` applies to the LIVE session when a
   run is registered (so an idle-between-prompts session held in the registry is
   updated in place) and otherwise to the stored snapshot.

3. **`session/load` (resume).** Inbound `session/load {sessionId, cwd}` validates
   cwd and rejects client MCP exactly like `session/new`, then resumes the persisted
   session via the new `Service.LoadSession(ctx, id)` seam: it loads the latest
   snapshot from the store and, if the session had cleanly `completed`, `Reopen`s it
   to `idle` (preserving conversation history) and re-persists, so the next
   `session/prompt`'s `BeginTurn` is legal. A failed/cancelled session is NOT
   resumable (`Reopen` rejects it). `loadSession` is advertised `true` ONLY when a
   durable store is configured (`--store-dir`); without one it is `false` and
   `session/load` returns a method error. An unknown/never-persisted id (incl. the
   in-memory store after a restart) is an `InvalidParams` error.

   **Persistence wiring.** Phase 1/2's ACP adapter never persisted (the ADR noted
   it would become a `Persist` call when `session/load` landed). It now calls
   `Service.Persist` on `EvPermissionAsk` (awaiting snapshot, for re-attach) and at
   run end (the completed turn's history) — BEFORE the deferred `FinishRun`
   deregisters the run, since `Persist` is a no-op once the run is gone. Without the
   run-end `Persist`, a loaded session would have an empty conversation.

   **Transcript replay.** ACP's `session/load` optionally streams the prior
   conversation back as `session/update` notifications so a re-attaching editor
   rebuilds the transcript rather than seeing an empty session. mecatl now does
   this: on load it re-projects the persisted `Conversation` through the SAME
   `projectUpdate` path the live loop uses (`replay.go`'s pure `historyEvents`
   synthesizes the domain events; `replayHistory` drives them through
   `projectUpdate` → `notifyUpdate`), rebuilding the message chunks, `tool_call`
   cards, and their `tool_call_update`s. The **open-before-update** invariant holds
   naturally from history order — the assistant message (with its `ToolCalls`) is
   stored before the tool-role result, so each `tool_call` is replayed before its
   `tool_call_update`. The user's own prompts (no `user_message_chunk`; the editor
   renders those locally), the opaque reasoning replay blob (`Message.Reasoning` is
   `encrypted_content`, not display text), and historical `permission.ask`s (an
   out-of-band `request_permission`, never re-prompted on load) are deliberately
   NOT replayed. Replay is synchronous within the load handler, so the notifications
   are flushed before the load response returns, and it is idempotent — a repeated
   load simply re-streams the same transcript, keyed by tool-call id.

## Permission round-trip

On `EvPermissionAsk` the adapter issues an outbound `session/request_permission`
carrying the tool call (title/kind/rawInput) and four options
(`allow_once`/`allow_always`/`reject_once`/`reject_always`). It awaits the reply
on a goroutine (so draining the event channel never blocks), then resolves the
paused run: a `selected` `allow_*` → `run.Approve(askID, true)`; `reject_*` or a
`cancelled` outcome → `run.Approve(askID, false)`. A transport error or context
cancellation denies the ask (fail-safe), so the run never hangs.

Note: with Phase 3's `session/load`, the ACP adapter now calls `Service.Persist`
on `EvPermissionAsk` (awaiting snapshot, for re-attach) and at run end (the
completed turn's history), matching the gRPC/HTTP adapters. Phase 1/2 deliberately
skipped this because there was no re-attach surface; that is no longer true.

## Concurrency / serialization

One in-flight prompt per session is enforced (a second `session/prompt` for the
same session is rejected). Writes to the wire are mutex-serialized so concurrent
`session/update` notifications and the `request_permission` round-trip never
interleave a frame. Inbound requests are handled on their own goroutines so a
handler that issues an outbound `Call` (a `session/prompt` issuing
`request_permission`) cannot deadlock the single read loop.

## Done in Phase 3 (bounded)

- **Slash commands** (`available_commands_update` on `session/new`) — via
  `Service.ListCommands`.
- **Mode switching** (`session/set_mode` + `current_mode_update`, current mode in
  `session/new`/`session/load`) — via `Service.SetMode` + `Session.SetMode`;
  mid-turn switch scoped to "defer to next prompt".
- **`session/load` (resume)** — via `Service.LoadSession` (reopen-if-completed);
  `loadSession:true` only with a durable store.
- **`session/load` transcript replay** — on load the persisted `Conversation` is
  re-projected through the same `projectUpdate` path as the live loop (`replay.go`),
  so a re-attaching editor rebuilds the transcript. Open-before-update preserved by
  history order; user prompts / reasoning blob / permission-ask deliberately not
  replayed; idempotent re-stream keyed by tool-call id.

## Deferred (Phase 3 long-tail)

- **fs/\* delegation** (`readTextFile`/`writeTextFile`) — a client-backed
  `tool.Workspace`. mecatl still uses its OWN `osfs` rooted at the session `cwd`.
- **`allow_always` governance-rule persistence** (still maps to `allow_once` — no
  rule is recorded, so every approval is one-shot).
- **Image/audio prompt content** (`promptCapabilities` stays text-only).
- Client-provided MCP servers.
- Richer projection of `turn.*` / `compaction` (ACP plan modelling).
