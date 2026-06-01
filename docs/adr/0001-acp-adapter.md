# ADR 0001 — Agent Client Protocol (ACP) adapter

- Status: Accepted
- Date: 2026-05-30
- Scope: Phase 1 (core loop) + Phase 2 (fidelity — diff blocks, subagent/team/hook
  projection) + Phase 3 bounded pieces (commands, set_mode, load, **fs/\* file-I/O
  delegation** — see "Phase 3 (bounded)"). **Multimodal prompt content (issue #5)
  is now DONE** — image/audio prompt blocks are parsed, validated, and gated on the
  configured provider's `ProviderCapabilities` (see "Multimodal prompt content").
  The Phase 3 long-tail (governance-rule persistence, grep-over-buffers, fs/\* on
  resume) remains (see "Deferred"). The hook-call-id veto (issue #6) is now DONE — a blocked
  PreToolUse veto keys a `failed` `tool_call_update` onto its card. **fs/\* delegation
  (issue #2) is now DONE (bounded hybrid)** — file Read/Write flow through the editor's
  buffers when the client advertises the capability.

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
   - **fs/\* delegation** — DEFERRED in Phase 1; **now DONE as a bounded hybrid in
     Phase 3** (issue #2 — see "Phase 3 (bounded)" §4). The client's
     `clientCapabilities` are now DECODED and consulted: when the client advertises
     BOTH `fs.readTextFile` && `fs.writeTextFile`, a per-session workspace routes file
     Read/Write through the editor's buffers (`fs/read_text_file` /
     `fs/write_text_file`); otherwise the OWN `osfs` workspace rooted at the session
     `cwd` is used (the pre-delegation behavior, still the fallback).
   - **`allow_always` LEARNS a per-session rule** (issue #3 — see "Learned
     permissions" below). It maps to `VerdictAllowAlways`, distinct from
     `allow_once` (`VerdictAllowOnce`); the harness records a narrow
     tool+exact-pattern allow scoped to the session so the same call does not ask
     again. (Earlier phases mapped both to a bare `allow`; that is superseded.)
   - **diff content blocks** (DONE in Phase 2 — see below). Phase 1 carried only
     plain-text `content`; Phase 2 attaches a structured `diff` block to Edit/Write
     tool calls.
   - **entry = `mecated --acp`.** (Not a separate binary.)

## Capabilities advertised (`initialize`)

- `protocolVersion: 1`.
- `promptCapabilities`: now REFLECT THE CONFIGURED PROVIDER, via the
  `port.ProviderCapabilities` seam read once at `NewAgent` from
  `Service.ProviderCapabilities()`. `image`/`audio` mirror what the provider can
  consume; `embeddedContext` mirrors the provider's `EmbeddedContext` (the adapter
  accepts inline-text `resource` blocks by flattening them into the prompt text).
  The OpenAI Responses provider declares `image:true`, `embeddedContext:true`,
  `audio:false` (its input content union has NO audio member — see "Multimodal
  prompt content"), so a typical `mecated --acp` advertises image but not audio; a
  text-only provider (e.g. the `mockllm` default) advertises all three `false`.
- `mcpCapabilities`: `http:true`, `sse:false`. mecatl is **streaming-HTTP MCP
  only** (CLAUDE.md: "No stdio MCP, ever"). A client may supply **streaming-HTTP**
  MCP servers in `session/new`; they are validated and mounted **per-session** (see
  "Client streaming-HTTP MCP" below). A **stdio** entry (`type:"stdio"`, or a
  command-shaped entry) and an **sse** entry are hard-rejected — mecatl never spawns
  an MCP server process and does not speak the SSE transport. **Asymmetry:**
  `session/load` still rejects ANY client MCP (re-mount on resume is a tracked
  follow-up), so a resumed session does not silently lose, or silently re-mount, the
  servers the client passed.
- `loadSession`: reflects whether a durable session store is configured. `mecated
  --acp` sets it `true` only when `--store-dir` is given (the in-memory store would
  lose snapshots across a restart); otherwise `false`. The flag is threaded into the
  adapter via `acp.WithResume(bool)` (Phase 3).
- `modes`: the session reflects mecatl's `default`/`plan`/`acceptEdits` modes; the
  `currentModeId` is the session's CURRENT mode (Phase 3 — was always `default`),
  and `session/set_mode` switches between them.
- **CLIENT capabilities are now decoded + consulted** (Phase 3 — `clientCapabilities`
  was previously an unused `json.RawMessage`). The agent reads
  `clientCapabilities.fs.readTextFile` and `.writeTextFile` from the `initialize`
  REQUEST to decide whether to delegate file I/O. This is a property of the request,
  NOT something the agent advertises back, so it does NOT appear in the response's
  `agentCapabilities`. BOTH must be true to delegate (see "Phase 3 (bounded)" §4).

## Multimodal prompt content (provider-capability-gated)

Inbound `session/prompt` content is no longer text-only. `buildPromptContent`
(replacing `flattenPrompt`) translates the ACP ContentBlock list into mecatl's
multimodal prompt shape — the flattened text PLUS `[]session.Content` media parts —
and `Service.StartRunContent(ctx, id, text, parts)` carries them into the run.

- **Block → content mapping.** `text` appends to the flattened text. A `resource`
  with inline **text** contents flattens into the text (an embedded-text resource —
  no Part; this is why `embeddedContext` is advertised when the provider supports
  it). A `resource` with a **blob** + image/audio mime becomes that media Part.
  `image`/`audio` blocks become media Parts (inline base64 `data`, or a `uri`).
  `resource_link` is **rejected loudly** (a URI mecatl cannot fetch).
- **No silent drops — the honesty fix.** Every non-text block becomes prompt text,
  a media Part, or a LOUD `codeInvalidParams` error. An unknown/unsupported block
  type, a `resource` with neither text nor blob, a blob with a non-media mime, or a
  base64-decode failure are all rejected — never silently dropped (the previous
  `flattenPrompt` silently discarded every non-text block).
- **Validator reuse — one validation path.** The ACP boundary uses the SAME
  validating constructors (`session.NewImageContent` / `NewImageURLContent` /
  `NewAudioContent` / `NewAudioURLContent` via `session.NewContent`) and the SAME
  per-prompt size caps (`session.ValidateMediaParts`) as the gRPC/HTTP surfaces, so
  the URL-SSRF (CWE-918), mime-consistency, exactly-one-of(data,url), and size
  (CWE-770) guarantees hold for ACP-supplied media exactly as elsewhere. There is
  NO second validation path in the adapter.
- **Capability-gated loud reject.** After building the parts and BEFORE
  `StartRunContent`, an image Part with `!caps.Image` (or audio with `!caps.Audio`)
  is rejected (`"image/audio content not supported by the configured provider"`),
  so `StartRunContent` is never reached for content the provider cannot consume. An
  image-only prompt against a text-only provider therefore errors and starts no run.
  This is defense-in-depth: `initialize` already advertised the caps, but a
  non-conformant client gets a clear error rather than a silent drop.
- **Audio is wired-but-dormant.** The full audio path (ACP block parsing,
  `session.MediaAudio`) is built end-to-end, but the OpenAI Responses input content
  union has no audio member, so the OpenAI provider declares `Audio:false`,
  `initialize` advertises `audio:false`, and an audio prompt is loud-rejected. Audio
  lights up with no further ACP code the day a provider declares `Audio:true`.

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
   cwd and rejects ANY client MCP (unlike `session/new`, which now accepts
   streaming-HTTP servers — re-mounting them on resume is a tracked follow-up), then
   resumes the persisted
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

4. **fs/\* file-I/O delegation (issue #2) — a bounded HYBRID.** When the client
   advertises BOTH `fs.readTextFile` && `fs.writeTextFile` at `initialize`, file
   Read/Write for that connection's sessions flow through the editor's buffers
   (`fs/read_text_file` / `fs/write_text_file`) instead of touching disk directly — so
   a model edit lands in the editor's in-memory buffer (including unsaved changes) and
   the agent's view and the editor's view never diverge **on the mutation path**.

   - **Why a hybrid, not "everything through fs/\*".** ACP's filesystem surface is
     ONLY `fs/read_text_file` / `fs/write_text_file` — there is no `fs/list`,
     `fs/stat`, or `fs/grep`. So the per-session `fsWorkspace` (in
     `internal/adapter/acp/fsworkspace.go`, implementing `tool.Workspace`) is a hybrid:
     **Read/Write are DELEGATED** through fs/\*; **Root/Stat/Glob/Grep are COMPOSED**
     from an `osfs.Workspace` rooted at the SAME session cwd (they read the local
     on-disk tree); the **Edit read-ledger** (`RecordRead`/`WasReadUnchanged`) is
     SYNTHESIZED locally over the delegated reads — the fingerprint is the sha256 of
     the `fs/read` content, so Edit's read-before-edit-and-unchanged invariant tracks
     the editor's BUFFER (strictly better than osfs for an editor session). The
     ledger has its OWN mutex (read-only dispatch fires concurrent fs/read Calls; the
     ACP `Conn` is already concurrency-safe, as `request_permission` proves).

   - **Require BOTH read AND write.** A read-only-delegating workspace would read the
     editor's buffers but still write to disk — re-introducing exactly the buffer/disk
     divergence delegation exists to remove. Both-or-neither → otherwise `osfs`.

   - **Path confinement BEFORE delegation (stated honestly — NOT os.Root-grade).**
     The model is UNTRUSTED even though the editor is trusted, so a session-relative
     tool path is confined BEFORE it is joined onto the EvalSymlinks-resolved `Root()`
     and handed to the editor. Two layers: (1) LEXICAL — reject absolute paths and any
     `..` that climbs out of root after `Clean` (the same lexical rule osfs applies);
     (2) SYMLINK (best-effort, on the on-disk tree) — `EvalSymlinks` the deepest
     EXISTING ancestor of the joined target (resolving the existing parent for a
     buffer-only/new leaf) and re-verify the resolved real path is still within
     `Root()`, rejecting if it escapes. This defends against a model creating an
     in-workspace symlink (`ln -s /etc/passwd evil` via Bash) and then reading/writing
     `evil`. It is NOT os.Root-grade and does NOT "mirror osfs": osfs routes every op
     through `*os.Root`, which refuses symlink traversal at the kernel level on a real
     path; this is a best-effort filesystem-side re-confinement (the fs/\* target may be
     a buffer-only path with no real leaf to open as an os.Root). The editor is a
     trusted-local process and owns final filesystem policy; this layer rejects the
     obviously-escaping shapes the untrusted model can construct. Each fs/\* `conn.Call`
     (read AND write) is also bounded by a per-call timeout (`fsCallTimeout`, 30s) so a
     wedged editor cannot hang the turn indefinitely (CWE-400).

   - **Write integrity — Stat is buffer-aware for EXISTENCE.** The Write tool uses a
     not-exist `Stat` to mean "new file, no read-before-overwrite required". A file may
     exist ONLY as an unsaved editor buffer (never written to disk), so a disk-only Stat
     would report it not-exist and Write would CLOBBER the unsaved buffer through
     `fs/write_text_file` with NO unchanged-since check — the exact divergence this
     feature exists to prevent. So `fsWorkspace.Stat` is disk-primary but, when disk
     reports not-exist, probes the editor via `fs/read_text_file`: a successful read →
     report EXISTS (Write's read-before-overwrite gate engages); a CLEAN editor
     not-found → `ErrNotExist` (genuinely new, Write allowed); an AMBIGUOUS fs/read
     fault → FAIL SAFE to EXISTS (force the gate rather than allow an unguarded write).
     ACP defines no canonical not-found code for fs/read, so the clean-not-found
     classification matches conservative message markers (`no such file`, `not found`,
     `does not exist`, `enoent`, `cannot find`) and treats everything else — including a
     timeout — as ambiguous.

   - **The wiring seam — a per-session Workspace OVERRIDE registry.** The shared
     `server.WorkspaceFactory func(root) tool.Workspace` has no per-connection/-session
     context (and is consumed by gRPC/HTTP too), so it was NOT widened. Instead the
     `Service` gained `SetSessionWorkspace(id, ws)` + a `sessionWorkspaces` map that
     `StartRun` PREFERS over the factory, evicted by `CloseSession`/`Close` — mirroring
     the existing per-session-engine registry EXACTLY. The ACP adapter registers an
     `fsWorkspace` (closing over the ACP `Conn` + `sessionId`) at `session/new` when the
     capability is present, and tracks the session for teardown on disconnect. gRPC/HTTP
     never call `SetSessionWorkspace`, so their behavior is unchanged.

   - **Residual divergence (ACCEPTED + documented).** Grep/Glob read DISK, not unsaved
     buffers (Stat's EXISTENCE check is buffer-aware — see above — but its metadata is
     still disk). So a model can grep stale (on-disk) text. This is acceptable because
     the Edit invariant forces a re-read-THROUGH-fs/\* before any edit, so a stale grep
     hit can never become a stale EDIT — the divergence is confined to search/discovery
     and never reaches the mutation path. This matches reference ACP agents (which grep
     the filesystem and delegate read/write to buffers); eliminating it would need an
     ACP file-list capability that does not exist, or an O(files) `fs/read` storm per
     Grep. Grep-over-buffers is a tracked follow-up.

   - **Asymmetry: `session/load` uses osfs, NOT fs/\*.** The fs delegation override is
     per-session connection state established at `session/new`; a loaded session does
     not (yet) re-establish it, so a resumed session uses the `osfs` fallback. This
     mirrors the existing client-MCP-on-load asymmetry (load rejects client MCP).
     Only `session/new` delegates; fs/\* on resume is a tracked follow-up.

## Client streaming-HTTP MCP

A client may declare **streaming-HTTP** MCP servers in `session/new {mcpServers}`.
They are accepted and **mounted per-session**, never globally.

- **Per-session, not global — the seam.** Each accepted session gets its OWN engine,
  built by a `server.SessionEngineFactory` (`func(ctx, []mcp.ServerConfig) (*agent.Engine,
  func() error, error)`) the composition root (`internal/app`) supplies via
  `server.Config.SessionEngine`. This **mirrors `MemberEngineFactory`** (the team
  seam): the `server.Service` and the ACP adapter reference `mcp.ServerConfig` /
  `*agent.Engine` in signatures but **build no managers themselves** — `internal/app`
  is the only layer that wires `mcp` + `agent` into an engine. The factory connects a
  scoped `mcp.NewManager` for that one session, registers the SAME core tools the main
  engine gets (`registerCoreTools`, factored out so its TWO call sites — the main build
  and the per-session factory — cannot drift; the narrower agent-def/team/fork child
  catalogs deliberately do not use it) plus the session's MCP tools, and builds its
  `agent.Deps` through the SHARED `baseEngineDeps` helper so EVERY collaborator matches
  the main engine — same provider, policy (interactive `defaultRules`), hooks, store,
  sink, logger, token counter, **compactor**, and **command expander**. (The per-session
  engine is exactly the long-running kind, so silently dropping the compactor/token
  counter — it would never compact — or the command expander — slash commands would
  stop expanding — was the [High] review finding; `baseEngineDeps` is the single
  source that prevents it.) `StartRun` routes a session with a registered per-session
  engine to it; every other session uses the shared engine with zero overhead (no
  specs → no factory call → no entry).
- **WHY not global-mount.** Registering a client's servers into the one shared catalog
  would leak that editor's tools — and its **auth headers** — into every other
  session/run on the process. A per-session engine confines the tools and the
  credentials to the session that supplied them.
- **SSRF stance + editor-trust model.** The client URL is validated by
  `mcp.ValidateClientURL` (a scheme/host-shape **allowlist**): `https` always, `http`
  ONLY for an explicit loopback host (`127.0.0.1`/`::1`/`localhost`);
  `file`/`ftp`/`gopher`/relative/hostless are rejected. This is applied ONLY to the
  untrusted client path — the operator-configured `Connect`/`NewManager` path is NOT
  gated (an operator may legitimately target an internal host). We deliberately do
  **NOT** do metadata-IP / link-local filtering: the editor is a local-trusted process
  (it spawned us over stdio), so we block the obviously dangerous URL shapes rather
  than resolving and filtering IPs. Header VALUES are never logged.
- **Cross-origin redirect header stripping.** `ValidateClientURL` only vets the
  INITIAL URL, and the HTTP client follows redirects — so the per-server header
  injector (`headerRoundTripper`) is **origin-scoped**: it applies the auth (and any
  other) header ONLY to requests whose `scheme://host` matches the configured
  endpoint. A server that `302`s to a different origin therefore never re-receives
  the Bearer token (CWE-918/601). This hardening covers the operator path too.
- **Connect-time DoS bound (CWE-400).** `mcp.NewManager` connects servers SERIALLY,
  each bounded by a timeout, so `session/new` caps the client at
  `maxClientMCPServers` (8; rejected loudly if exceeded) and sets each spec's
  `ServerConfig.Timeout` to a shorter client-path value (`clientMCPConnectTimeout`,
  10s, vs the operator path's 30s) — an unreachable client server can stall
  `session/new` by at most that bound, not the operator budget × count.
- **Best-effort mount lifecycle.** A down/unreachable client server is logged-and-
  skipped (like `defMCPTools`), never fatal — the session still gets a usable engine
  (core tools, plus whatever MCP servers did connect). The per-session MCP manager is
  torn down when the editor disconnects (the ACP `Serve` loop ends → `Service.CloseSession`
  for each tracked session) and, as a backstop, by `Service.Close` on process exit.
  `CloseSession` is SAFE under an in-flight run with no run-cancel guard: `mgr.Close`
  is the go-sdk's GRACEFUL session close, which prevents new requests and WAITS for
  ongoing ones to return before terminating the connection (idempotent +
  concurrency-safe), so a disconnect racing a live MCP tool call cannot yank the
  connection mid-call. The per-session registry is bounded by connection lifetime,
  not a count cap.
- **Follow-up (Slice B).** `session/load` re-mount of client MCP, and mid-session
  teardown (a single session ending before the connection closes), are tracked
  separately.

## Permission round-trip

On `EvPermissionAsk` the adapter issues an outbound `session/request_permission`
carrying the tool call (title/kind/rawInput) and four options
(`allow_once`/`allow_always`/`reject_once`/`reject_always`). It awaits the reply
on a goroutine (so draining the event channel never blocks), then resolves the
paused run with a three-way `session.ApprovalVerdict` (`approvalFor`):
`allow_once` → `VerdictAllowOnce`; `allow_always` → `VerdictAllowAlways` (which
additionally LEARNS — see below); `reject_*`, a `cancelled` outcome, a transport
error, or context cancellation → `VerdictDeny` (fail-safe), so the run never hangs.

Note: with Phase 3's `session/load`, the ACP adapter now calls `Service.Persist`
on `EvPermissionAsk` (awaiting snapshot, for re-attach) and at run end (the
completed turn's history), matching the gRPC/HTTP adapters. Phase 1/2 deliberately
skipped this because there was no re-attach surface; that is no longer true.

## Learned permissions (`allow_always`, issue #3)

`allow_always` now records a per-session permission rule so an approved call is
not re-asked. The slice is deliberately CONSERVATIVE; the invariants below are the
whole point and have tests that fail on regression.

**Verdict enum, end to end.** The old allow/deny bool is widened to a three-way
verdict — `session.ApprovalVerdict` (`VerdictDeny` = zero value / fail-safe,
`VerdictAllowOnce`, `VerdictAllowAlways`), mirrored in proto as the
`ApprovalVerdict` enum on `ResumeApproval.verdict`. It threads: ACP `approvalFor`
/ gRPC `verdictFromResumeApproval` / HTTP `verdictFromHTTP` →
`Service.Approve(…, verdict)` → `Run.Approve(askID, verdict)` →
`askRegistry`/`await` → `dispatch.authorize`. The loop owns the askID→tool+args
correlation, so it is the loop — via the policy PORT — that records the rule, not
the adapter.

**Proto back-compat.** `ResumeApproval` keeps `bool allow = 2` and adds
`ApprovalVerdict verdict = 3`. When `verdict != UNSPECIFIED` it wins; otherwise the
legacy bool is honoured (`true` → `ALLOW_ONCE`, `false` → `DENY`). `task generate`
stays idempotent.

**Granularity: tool + EXACT canonical pattern, never broader.** On
`VerdictAllowAlways`, `dispatch.authorize` calls `Policy.Learn(sess.ID, c)` as a
SIDE EFFECT (it governs future calls only; the current call proceeds one-shot via
the Allow it already resolved, never re-evaluated). `governance.LearnableRule`
derives the rule using the SAME pattern derivation the evaluator matches against
(`nonBashPattern` for non-Bash; `SplitCommands`+`Canonicalize` for Bash) and
returns `{Tool, Pattern, Effect: Allow, Exact: true, Scope: ScopeUser}` (lowest
scope). `Rule.Exact` forces LITERAL matching — a learned pattern is never
glob-expanded, so a stray `*` cannot escalate.

**Refuse-to-learn.** `LearnableRule` returns "do not learn" when: a Bash command
splits into ≠1 segment (compound `a && b` / `a; b`), or contains
substitution/grouping (`$(…)`, backticks, `(…)`), or is empty; or the derived
pattern is empty for ANY tool (no targetable field). So `git status` is learnable
but `git status; rm -rf /` is not, and we NEVER learn a tool-wide allow.

**The seam.** `governance.Evaluator` stays immutable and session-free: it gains
`EvaluateWith(tool, args, planMode, extra []Rule)` (the plan-mode gate runs FIRST,
THEN the deny→ask→allow fold over `static ++ extra`) and `LearnableRule`, but never
stores anything. The mutable, per-session, in-memory, NON-durable
`adapter/permstore.Memory` (implementing the new `port.PermissionStore`) keys
learned rules by session; `adapter/permpolicy.NewPolicy(rules, store)` merges this
session's learned rules in on every `Evaluate`. Rules are dropped on
`Service.CloseSession` (`OnCloseSession` → `Memory.Forget`) and on process restart.

**Security invariants preserved.** A learned allow can NEVER override a deny (the
fold is deny-dominant across the WHOLE merged set, learned rules sit at the lowest
scope), NEVER beat an ask (same reason), and NEVER bypass plan-mode mutation
denial (the plan gate runs before any learned rule is consulted). Cross-session
isolation holds: session A's learned rule is invisible to session B.

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
- **fs/\* file-I/O delegation (issue #2)** — a bounded hybrid: Read/Write delegated
  through `fs/read_text_file`/`fs/write_text_file` (gated on the client advertising
  BOTH caps), the Edit read-ledger synthesized buffer-keyed over the delegated reads,
  Stat/Glob/Grep composed from an osfs view at the same root; per-session
  `Service.SetSessionWorkspace` override (no `WorkspaceFactory` signature change);
  osfs fallback when caps absent or on `session/load`. See "Phase 3 (bounded)" §4.

## Deferred (Phase 3 long-tail)

- **Grep/Glob over editor buffers** — the fs/\* hybrid (issue #2, DONE) delegates
  Read/Write but searches DISK (ACP has no `fs/list`/`fs/grep`). Search-over-unsaved-
  buffers is a tracked follow-up; the mutation path is already buffer-consistent.
- **fs/\* delegation on `session/load`** — a resumed session uses the osfs fallback;
  re-establishing the per-session fs override on resume is a tracked follow-up
  (parallels the client-MCP-on-load asymmetry).
- **DURABLE learned permissions** (cross-restart) — the `allow_always` rule store
  is in-memory and per-session today (issue #3, see "Learned permissions"); a
  durable store that survives process restart is a tracked follow-up.
- **Broader learned-rule granularity** (glob / prefix / tool-wide grants) — today
  a learned rule is tool + EXACT canonical pattern only, by design; richer
  granularity (e.g. "allow always for `git *`") is a tracked follow-up.
- **Learned-rule eviction over gRPC/HTTP** — DONE (issue #10). `Forget` runs from
  `Service.CloseSession`, now reachable over all three surfaces: the ACP adapter (on
  editor disconnect), the gRPC `CloseSession` RPC, and HTTP `DELETE /v1/sessions/{id}`
  (both via `Service.EndSession`, which verifies the session exists then runs the same
  idempotent teardown — NotFound only for a never-created id; close ≠ delete-snapshot ≠
  cancel-run). Option 1 (symmetric session-end across transports) + Option 2 (a
  client-independent per-session cap, `permstore.maxRulesPerSession`, fail-safe toward
  asking) shipped. Option 3 (TTL / idle-based eviction) is deliberately DEFERRED pending
  telemetry on real long-lived-session rule accumulation.
- **Client MCP on `session/load`** (re-mount the client's streaming-HTTP servers on
  resume) + **mid-session teardown** — tracked follow-up (Slice B). `session/new`
  client streaming-HTTP MCP is now DONE (see "Client streaming-HTTP MCP").
- Richer projection of `turn.*` / `compaction` (ACP plan modelling).
