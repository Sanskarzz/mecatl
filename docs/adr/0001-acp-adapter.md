# ADR 0001 — Agent Client Protocol (ACP) adapter

- Status: Accepted
- Date: 2026-05-30
- Scope: Phase 1 (core loop). Phases 2–3 remain (see "Deferred").

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
   - **diff content blocks DEFERRED.** `tool_call_update` carries the tool result
     as a plain-text `content` block, not a structured `diff` for edits.
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

## Event projection (Phase 1)

| domain `session.Event`        | ACP                                                            |
|-------------------------------|----------------------------------------------------------------|
| `EvMessageDelta`              | `session/update` `agent_message_chunk{content:text}`           |
| `EvReasoningDelta`            | `session/update` `agent_thought_chunk{content:text}` (summary only — never the encrypted replay blob) |
| `EvToolCall`                  | `session/update` `tool_call{toolCallId,title,kind,rawInput,status:pending}` |
| `EvToolResult`                | `session/update` `tool_call_update{toolCallId,status:completed\|failed,content:[text]}` |
| `EvPermissionAsk`             | OUTBOUND `session/request_permission` (4 options); reply → `run.Approve` |
| `EvResult`                    | the prompt's return `{stopReason}` |

Stop-reason mapping: `StopEndTurn→end_turn`; `StopCancelled→cancelled`; the three
limit reasons (`StopMaxTurns`/`StopMaxToolCalls`/`StopMaxConsecutiveFailures`)→
`max_turn_requests`; `StopError→end_turn` (a clean terminal — the error text
still reaches the editor via the preceding message chunks).

**Dropped / folded this phase** (no clean ACP mapping yet; full fidelity is a
later phase): `session.init`, `turn.start`, `turn.end`, `compaction`, `hook`,
`subagent.*`, `team.*`. These are dropped (not surfaced) for now.

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
becomes a `Persist` call if Phase 2 adds `session/load`.

## Concurrency / serialization

One in-flight prompt per session is enforced (a second `session/prompt` for the
same session is rejected). Writes to the wire are mutex-serialized so concurrent
`session/update` notifications and the `request_permission` round-trip never
interleave a frame. Inbound requests are handled on their own goroutines so a
handler that issues an outbound `Call` (a `session/prompt` issuing
`request_permission`) cannot deadlock the single read loop.

## Deferred to later phases (Phases 2–3)

- fs/\* delegation (`readTextFile`/`writeTextFile`).
- `allow_always` rule persistence.
- `diff` content blocks for edits.
- Full-fidelity projection of `turn.*`/`hook`/`compaction`/`subagent.*`/`team.*`
  (ACP plan + nested-tool modelling).
- `session/load` (resume), session mode switching, slash commands / config
  options, client-provided MCP servers, image/audio prompt content.
