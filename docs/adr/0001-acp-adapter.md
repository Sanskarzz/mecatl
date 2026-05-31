# ADR 0001 — Agent Client Protocol (ACP) adapter

- Status: Accepted
- Date: 2026-05-30
- Scope: Phase 1 (core loop) + Phase 2 (fidelity — diff blocks, subagent/team/hook
  projection). Phase 3 remains (see "Deferred to Phase 3").

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
- `loadSession: false`.
- `modes`: the session reflects mecatl's `default`/`plan`/`acceptEdits` modes;
  the current mode is always `default` on a fresh session this phase (switching
  is deferred).

## Event projection (Phase 1 + Phase 2)

| domain `session.Event`        | ACP                                                            |
|-------------------------------|----------------------------------------------------------------|
| `EvMessageDelta`              | `session/update` `agent_message_chunk{content:text}`           |
| `EvReasoningDelta`            | `session/update` `agent_thought_chunk{content:text}` (summary only — never the encrypted replay blob) |
| `EvToolCall`                  | `session/update` `tool_call{toolCallId,title,kind,rawInput,status:pending}` — for Edit/Write a `diff` content block is attached (Phase 2) |
| `EvToolResult`                | `session/update` `tool_call_update{toolCallId,status:completed\|failed,content:[text]}` |
| `EvHook`                      | `session/update` `agent_thought_chunk{content:reason}` (Phase 2) |
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

3. **Hook events.** `EvHook` projects to an `agent_thought_chunk` carrying the
   hook's reason (a blocked PreToolUse veto, a prompt/arg rewrite, a Stop notice).
   It is deliberately NOT a `failed` `tool_call_update` on the related tool: ACP
   keys a `tool_call_update` by `toolCallId`, but `HookPayload` carries only the
   tool NAME, never the originating tool-call id — so this package cannot address
   the real tool_call without fabricating a phantom card. The blocked call's own
   `EvToolResult` (which DOES carry the real id) still projects to a `failed`
   `tool_call_update`. Surfacing the veto ON the tool card needs the call id added
   to `HookPayload` (an event-taxonomy change) — deferred to Phase 3.

4. **Modes + commands DEFERRED to Phase 3.** `current_mode_update` /
   `session/set_mode` need a Service seam to change a session's `PermissionMode`
   mid-session; `*server.Service` exposes none today (only Create/Get/Start/Approve/
   Cancel), and adding one is outside this adapter's package. `available_commands_update`
   needs a wired slash-command registry, which mecatl does not expose. Both were
   scoped out to keep Phase 2 confined to `internal/adapter/acp/`.

## Permission round-trip

On `EvPermissionAsk` the adapter issues an outbound `session/request_permission`
carrying the tool call (title/kind/rawInput) and four options
(`allow_once`/`allow_always`/`reject_once`/`reject_always`). It awaits the reply
on a goroutine (so draining the event channel never blocks), then resolves the
paused run: a `selected` `allow_*` → `run.Approve(askID, true)`; `reject_*` or a
`cancelled` outcome → `run.Approve(askID, false)`. A transport error or context
cancellation denies the ask (fail-safe), so the run never hangs.

Note: the gRPC/HTTP adapters call `Service.Persist` on `EvPermissionAsk` so an
awaiting session is loadable for re-attach after a restart. The ACP adapter
deliberately SKIPS that this phase — there is no re-attach surface yet
(`loadSession:false`, no `session/load`), and the prompt blocks for the whole
turn on the same connection, so a persisted awaiting snapshot would be unused. It
becomes a `Persist` call when a later phase adds `session/load`.

## Concurrency / serialization

One in-flight prompt per session is enforced (a second `session/prompt` for the
same session is rejected). Writes to the wire are mutex-serialized so concurrent
`session/update` notifications and the `request_permission` round-trip never
interleave a frame. Inbound requests are handled on their own goroutines so a
handler that issues an outbound `Call` (a `session/prompt` issuing
`request_permission`) cannot deadlock the single read loop.

## Deferred to Phase 3

- fs/\* delegation (`readTextFile`/`writeTextFile`).
- `allow_always` rule persistence (still maps to `allow_once`).
- `session/load` (resume).
- Session **mode switching** (`current_mode_update` / `session/set_mode`) — needs
  a Service seam to mutate a session's `PermissionMode` mid-session.
- **Slash commands** (`available_commands_update`) — needs a wired command registry.
- Keying a hook veto onto the related tool card — needs the tool-call id added to
  `HookPayload` (an event-taxonomy change).
- Client-provided MCP servers, image/audio prompt content.
- Richer projection of `turn.*` / `compaction` (ACP plan modelling).
