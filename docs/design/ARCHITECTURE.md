# ozzharness — Architecture (v1)

> Status: design, pre-implementation.
> Module: `github.com/stacklok/ozzharness` · Go 1.26.3
> Primary source: `docs/harnesses/08-design-considerations.md` (the 13 load-bearing
> decisions and the 10-point gauntlet). Companions: `06-architecture-patterns.md`,
> `03-claude-code-architecture.md`, `07-context-and-mcp.md`.

ozzharness is a **headless agentic coding-harness**: a service/library that runs the
agent loop, executes coding tools, enforces permissions and hooks, and streams typed
events. There is no TUI. Clients drive it over gRPC or HTTP.

The whole design exists to satisfy one shape (doc 08 TL;DR): *a single streaming
agent loop, ~7 core tools, an enforced plan/act gate, one-shot subagents with isolated
context, deterministic hooks, a permission system that is deny → ask → allow across
merged scopes, and prompt caching at every stable boundary* — with the **LLM provider,
the servers, the filesystem, and the tools all behind ports** so the core is
provider-agnostic and unit-testable against fakes.

---

## 1. Architectural style

**Hexagonal (ports & adapters) with a DDD core.** Dependencies point inward only:

```
                         ┌─────────────────────────────────────────────┐
   ADAPTERS (driving)    │                  DOMAIN                      │   ADAPTERS (driven)
                         │  (no infra imports, no provider imports)     │
  ┌───────────────┐      │                                             │   ┌────────────────────┐
  │ gRPC server   │──┐   │   agent  (the loop / use-cases)             │   │ OpenAI Responses   │
  │ (connect-go)  │  │   │   ├─ orchestrates Session aggregate         │◀──│ adapter  (LLMPort) │
  └───────────────┘  │   │   ├─ depends on PORTS only:                 │   └────────────────────┘
  ┌───────────────┐  ├──▶│   │    LLMProvider, Tool, PermissionPolicy, │   ┌────────────────────┐
  │ HTTP/JSON+SSE │──┘   │   │    HookRunner, SessionStore, Clock,     │◀──│ tools adapter      │
  │ (connect-go)  │      │   │    Logger, EventSink                    │   │ (Read/Edit/Bash..) │
  └───────────────┘      │   │                                         │   └────────────────────┘
  ┌───────────────┐      │   session  (aggregate: Session, Conversation│   ┌────────────────────┐
  │ demo CLI      │─────▶│   │           Turn, Message, ToolCall,      │◀──│ filesystem adapter │
  │ (cmd/ozzdemo) │      │   │           Permission, Hook, Usage)      │   │ (OS fs / mem fake) │
  └───────────────┘      │                                             │   └────────────────────┘
                         │   prompt  (system-prompt assembly,          │   ┌────────────────────┐
                         │            cache-stable prefix + suffix)    │◀──│ session store      │
                         └─────────────────────────────────────────────┘   │ (mem / jsonl)      │
                                                                            └────────────────────┘
```

The **domain** (`session`, `prompt`) holds entities, value objects, and the *port
interfaces*. The **application** (`agent`) is the use-case layer: it is the agent loop
and knows only ports. **Adapters** implement ports and depend inward on the domain;
nothing in the domain imports an adapter, the OpenAI SDK, connect-go, or `os`.

Ports are defined **where they are consumed** (in `internal/port`, imported by `agent`),
per Go idiom "accept interfaces". Adapters return concrete structs.

---

## 2. Bounded contexts

This is a small system; over-contexting it would be layer hypertrophy. There are
**three** bounded contexts, with one ubiquitous language each.

| Context | Owns | Language |
|---|---|---|
| **Agent Session** | the run lifecycle, conversation, turns, the loop, stop conditions, compaction seam | Session, Conversation, Turn, Message, ToolCall, ToolResult, Usage, Event |
| **Governance** | permission evaluation, hooks, plan-mode gating | PermissionDecision (deny/ask/allow), Scope, Rule, HookEvent, HookOutcome |
| **Tooling** | the tool catalog and its execution invariants | Tool, ToolSpec, ReadOnly, Edit invariants, Workspace/FileSystem |

The LLM provider is **not** a bounded context — it is a port the Agent Session context
depends on. The two servers are driving adapters, also not contexts.

**Cross-context rule:** Governance and Tooling never import Agent Session internals;
they exchange the shared value objects (`ToolCall`, `ToolResult`, `HookEvent`) defined
in the domain `session`/`tool` packages. There is no `billing.User` / `auth.User`
style duplication risk here because all three contexts share one `ToolCall` type — that
is correct, because a ToolCall *is* the same concept across all three.

---

## 3. Go package layout

`internal/` for everything not meant as a stable public API. `pkg/` is **not** used in
v1 — there is no third-party-stable surface yet; the public surface is the proto/HTTP
API, not Go symbols. (Re-evaluate `pkg/sdk` once an external Go consumer exists. YAGNI.)

```
github.com/stacklok/ozzharness
├── api/
│   ├── proto/ozz/v1/ozz.proto         # gRPC service + messages (source of truth)
│   └── gen/ozz/v1/                     # generated Go (connect-go + protobuf)
├── cmd/
│   ├── ozzd/                           # the server binary (gRPC+HTTP on one port)
│   └── ozzdemo/                        # the demo driver (fake provider default)
├── internal/
│   ├── session/                        # DOMAIN: Session aggregate + value objects
│   │   ├── session.go                  #   Session (root), state machine
│   │   ├── conversation.go             #   Conversation, Message, Turn
│   │   ├── toolcall.go                 #   ToolCall, ToolResult (shared value objects)
│   │   ├── usage.go                    #   Usage (value object)
│   │   └── event.go                    #   Event taxonomy (domain-owned)
│   ├── prompt/                         # DOMAIN: two-layer system prompt assembly
│   │   ├── prompt.go                   #   StablePrefix / VolatileSuffix builder
│   │   └── env.go                      #   <env> block, AGENTS.md/CLAUDE.md discovery
│   ├── governance/                     # DOMAIN: permission + hook + plan-mode logic
│   │   ├── permission.go               #   PermissionDecision, Rule, Scope, merge/eval
│   │   ├── bash.go                     #   compound-command split + wrapper canonicalization
│   │   └── hookevent.go                #   HookEvent, HookOutcome, lifecycle enum
│   ├── tool/                           # DOMAIN: Tool port + ToolSpec + catalog contract
│   │   ├── tool.go                     #   Tool interface, ToolSpec, ReadOnly flag, FileSystem/Workspace
│   │   └── catalog.go                  #   Catalog (name→Tool), plan-mode filtering
│   ├── port/                           # PORTS: interfaces the agent consumes
│   │   ├── llm.go                      #   LLMProvider / Completer + stream chunk types
│   │   ├── store.go                    #   SessionStore
│   │   ├── hookrunner.go               #   HookRunner
│   │   ├── clock.go                    #   Clock
│   │   └── log.go                      #   Logger, EventSink
│   ├── agent/                          # APPLICATION: the loop (use-case layer)
│   │   ├── loop.go                     #   Run(ctx, Session) streaming the Event channel
│   │   ├── dispatch.go                 #   read-parallel / mutate-serial tool dispatch
│   │   ├── permission.go              #   ask-pause/resume wiring to EventSink
│   │   ├── compaction.go               #   ~80% window single-summary seam
│   │   └── subagent.go                 #   Task: fresh context, scoped tools, one-shot
│   └── adapter/                        # ADAPTERS: implement ports
│       ├── openai/                     #   LLMProvider over OpenAI Responses API (SSE)
│       ├── mockllm/                    #   scripted fake LLMProvider (no network)
│       ├── tools/                      #   Read, Edit, Write, Bash, Grep, Glob, Task, WebFetch(stub)
│       ├── osfs/                       #   FileSystem over the real OS
│       ├── memfs/                      #   in-memory FileSystem fake
│       ├── store/                      #   memstore (default) + jsonlstore (replay log)
│       ├── hookexec/                   #   shell-exec HookRunner (stdin JSON, exit-code)
│       └── server/                     #   connect-go service implementing api/gen
└── docs/design/                        # this file + STEP-CHAIN.md
```

**Allowed-imports matrix** (the contract; CI can enforce with `depguard`):

| Package | May import |
|---|---|
| `session`, `prompt`, `governance`, `tool` (domain) | stdlib, other domain packages. **Never** `adapter`, `agent`, `api`, `os`, OpenAI SDK, connect-go. |
| `port` | domain packages + stdlib (`context`, `io`, `time`). Nothing else. |
| `agent` (application) | domain + `port`. **Never** `adapter` or `api`. |
| `adapter/*` | domain + `port` + the specific external lib it adapts. Never `agent`. |
| `api/gen` | generated; protobuf + connect runtime only. |
| `cmd/*` | everything — this is the composition root where wiring happens. |

The only place concrete adapters meet ports is `cmd/` (dependency injection by hand;
no DI framework — explicit constructors, doc 03 "pass dependencies explicitly").

---

## 4. Domain model

### 4.1 Session aggregate

`Session` is the aggregate root. Outside code holds a `SessionID`, never an inner
entity — "reach through the root." All mutation of the Conversation goes through Session
methods so invariants (turn counting, stop conditions, state transitions) hold.

```go
// internal/session
type SessionID string

type State string
const (
    StateIdle      State = "idle"      // created, no turn running
    StateRunning   State = "running"   // a turn is in flight
    StateAwaiting  State = "awaiting"  // paused on a permission "ask"
    StateCompleted State = "completed"
    StateFailed    State = "failed"
    StateCancelled State = "cancelled"
)

type PermissionMode string
const (
    ModeDefault PermissionMode = "default"
    ModePlan    PermissionMode = "plan"        // read-only toolset enforced
    ModeAccept  PermissionMode = "acceptEdits"
)

type Session struct {
    ID            SessionID
    State         State
    Mode          PermissionMode
    Conversation  *Conversation
    Limits        Limits          // value object: stop conditions
    Counters      Counters        // turns, toolCalls, consecutiveFailures
    Workspace     string          // root dir for tools (cwd)
    CreatedAt     time.Time
    pending       *PendingAsk     // set iff State==Awaiting
}

type Limits struct {                  // doc 08: "no stop conditions" anti-pattern
    MaxTurns             int
    MaxToolCalls         int
    MaxConsecutiveFailures int
}
```

`Session` exposes intention-revealing methods, not setters:
`BeginTurn()`, `RecordAssistant(Message)`, `RecordToolResults([]ToolResult)`,
`PauseForApproval(PendingAsk)`, `ResumeWith(PermissionDecision)`, `Cancel()`,
`StopReason() (StopReason, bool)`.

### 4.2 Conversation / Turn / Message

```go
type Role string
const (RoleSystem Role="system"; RoleUser Role="user"; RoleAssistant Role="assistant"; RoleTool Role="tool")

type Message struct {
    Role      Role
    Text      string
    ToolCalls []ToolCall    // assistant messages requesting tools
    ToolResult *ToolResult  // tool-role messages
    Reasoning  string       // provider reasoning item, opaque, replayed verbatim
}

type Conversation struct { Messages []Message }       // model-visible history

type Turn struct {                                     // one model call + its tools
    Index     int
    Assistant Message
    Results   []ToolResult
    Usage     Usage
}
```

### 4.3 Shared value objects (cross-context)

```go
type ToolCallID string
type ToolCall struct {                 // immutable; produced by LLM, consumed by tool+gov
    ID   ToolCallID
    Name string
    Args json.RawMessage               // tool-specific, validated by the Tool
}

type ToolResult struct {               // immutable; paired to ToolCall by ID
    CallID  ToolCallID
    Content string                     // already token-shaped/truncated by the tool
    IsError bool
}

type Usage struct {                    // value object, doc 07 §12 accounting
    InputTokens, OutputTokens          int
    CacheReadTokens, CacheWriteTokens  int
}
func (u Usage) CacheHitRate() float64
```

### 4.4 Governance value objects

```go
// internal/governance
type Effect string
const (Deny Effect="deny"; Ask Effect="ask"; Allow Effect="allow")

type PermissionDecision struct {       // result of evaluating a ToolCall across scopes
    Effect Effect
    Reason string                      // teaches the model on deny (doc 07 §11)
}

type Scope int  // Managed > CLI > LocalProject > SharedProject > User  (doc 03 precedence)

// HookEvent / lifecycle  (doc 03 hook table; v1 implements PreToolUse/PostToolUse,
// designs in the rest)
type HookPhase string
const (
    PhaseSessionStart    HookPhase="SessionStart"
    PhaseUserPromptSubmit HookPhase="UserPromptSubmit"
    PhasePreToolUse      HookPhase="PreToolUse"
    PhasePostToolUse     HookPhase="PostToolUse"
    PhaseStop            HookPhase="Stop"
    PhaseSubagentStop    HookPhase="SubagentStop"
)
type HookEvent struct { Phase HookPhase; Tool string; Input json.RawMessage; SessionID string }
type HookOutcome struct { Block bool; Message string; Mutated json.RawMessage } // exit 0=allow,2=block
```

---

## 5. Ports

Small interfaces, `context.Context` first, `error` last (where applicable). Each port
has a fake in `internal/adapter/*` so the loop is unit-testable with no network and no
disk.

### 5.1 LLMProvider — the provider-agnostic seam

The loop must never see an OpenAI type. The provider yields a stream of **provider-
neutral chunks**; the loop assembles them into a domain `Message`. The OpenAI Responses
API specifics (function_call / function_call_output items, reasoning items, automatic
prompt caching, SSE framing) live entirely inside `adapter/openai`. A concurrent
research agent is filling that adapter in; this port is what they target.

```go
// internal/port
type LLMRequest struct {
    System    prompt.Layered        // stable prefix + volatile suffix (for cache breakpoints)
    Messages  []session.Message     // conversation history
    Tools     []tool.ToolSpec       // schemas; stable across turns for caching
    Model     string
}

type ChunkKind int
const (
    ChunkText ChunkKind = iota   // assistant text delta
    ChunkReasoning               // reasoning item delta (opaque, replayed back)
    ChunkToolCall                // a fully-formed tool call (emitted once assembled)
    ChunkUsage                   // terminal usage/cache accounting
    ChunkDone                    // end of stream; carries StopReason
)

type Chunk struct {
    Kind      ChunkKind
    Text      string
    ToolCall  *session.ToolCall
    Usage     *session.Usage
    Stop      session.StopReason
}

// LLMProvider streams chunks until ctx is cancelled or the model stops.
// Cancellation is via ctx — this is how the API "cancel" verb interrupts a turn.
type LLMProvider interface {
    Stream(ctx context.Context, req LLMRequest) (iter.Seq2[Chunk, error], error)
}
```

(Go 1.26 `iter.Seq2` gives us the async-generator shape doc 08 #1 demands, natively.)

### 5.2 Tool — the catalog contract

```go
// internal/tool
type ToolSpec struct {                 // what the model sees (doc 07 §9: descriptions are docs)
    Name        string
    Description string                 // when-to-use / when-not / example / limits
    Schema      json.RawMessage        // JSON schema for Args
}

type Tool interface {
    Spec() ToolSpec
    ReadOnly() bool                    // drives parallel-vs-serial dispatch (doc 08 #4)
    Execute(ctx context.Context, in ToolCall, ws Workspace) (ToolResult, error)
}
```

`Workspace` is the injected FS seam (real OS fs / mem fake), so every tool is testable:

```go
// internal/tool  (FileSystem/Workspace live here, scoped to a session root;
// kept in the Tooling context — not internal/port — to avoid a port↔tool import cycle)
type FileSystem interface {
    Read(ctx context.Context, path string) ([]byte, error)
    Write(ctx context.Context, path string, data []byte) error
    Stat(ctx context.Context, path string) (FileInfo, error)
    Glob(ctx context.Context, pattern string) ([]string, error)
    // Bash/Grep go through a Runner the FS exposes or a separate CommandRunner port.
}
```

Edit's three invariants (read-before-edit, exact-match, uniqueness — doc 08 #3) are
enforced **inside the Edit tool** against a per-session read-ledger the tool consults;
the ledger is part of session state passed via `Workspace`.

### 5.3 Remaining ports

```go
type SessionStore interface {                      // server-side state (decision: stateful)
    Save(ctx context.Context, s *session.Session) error
    Load(ctx context.Context, id session.SessionID) (*session.Session, error)
}

type HookRunner interface {                        // exit 0 allow / 2 block (doc 03)
    Run(ctx context.Context, ev governance.HookEvent) (governance.HookOutcome, error)
}

type PermissionPolicy interface {                  // deny→ask→allow across merged scopes
    Evaluate(ctx context.Context, mode session.PermissionMode, c session.ToolCall) governance.PermissionDecision
}

type EventSink interface { Emit(Event) }           // loop → API stream
type Clock interface { Now() time.Time }
type Logger interface { ToolCall(session.SessionID, session.ToolCall, session.ToolResult, time.Duration) }
```

---

## 6. The streaming event model

`Event` is **domain-owned** (`internal/session/event.go`) and shared by the loop and the
API. The API serializes it to proto; it is never an OpenAI type. This is the single
event taxonomy doc 08 #1 calls for.

```go
type EventType string
const (
    EvSessionInit  EventType = "session.init"
    EvTurnStart    EventType = "turn.start"
    EvMessageDelta EventType = "message.delta"   // streamed assistant text
    EvToolCall     EventType = "tool.call"       // a tool is about to run
    EvToolResult   EventType = "tool.result"
    EvPermissionAsk EventType = "permission.ask" // loop paused; client must approve/deny
    EvHook         EventType = "hook"            // hook fired (PreToolUse blocked, etc.)
    EvCompaction   EventType = "compaction"      // compaction boundary crossed
    EvResult       EventType = "result"          // terminal: success / max_turns / error / cancelled
)

type Event struct {
    Type     EventType
    Seq      int64
    Turn     int
    Text     string
    ToolCall *ToolCall
    ToolResult *ToolResult
    Ask      *PendingAsk        // on permission.ask: id, tool, args, reason
    Result   *ResultPayload     // on result: StopReason + final text + Usage
    Usage    *Usage
}
```

The loop runs as a producer goroutine writing `Event`s to a channel; the server adapter
relays them to the gRPC server-stream / HTTP SSE. On `permission.ask` the loop blocks in
`StateAwaiting` until the client calls `Approve`/`Deny`, which `ResumeWith` unblocks.
Cancellation is a `ctx` cancel propagated to `LLMProvider.Stream` and the running tool.

---

## 7. The API surface

**Decision (revised — the downstream consumer convention):** use **grpc-go + buf + protovalidate**,
mirroring `a downstream consumer`'s `contracts/proto` + `contracts/gen/go` layout and its
`AgentLoopService.Converse` bidi pattern. Rationale: house style, and the bidi stream is
the cleanest expression of pause/resume/cancel — the approval frame returns on the *same*
stream that is emitting events, so no out-of-band correlation is needed. (This supersedes
the earlier connect-go sketch.)

**Decision: server-side conversation state.** Permission "ask" pause/resume and mid-turn
cancellation require the harness to suspend the loop across frames; the client holds only
a `session_id` and drives the run over the stream.

**Two surfaces, one domain `Event`:**
- **gRPC (primary):** a bidi `Converse` stream (the downstream consumer's shape) carries the whole run.
- **HTTP (pragmatic):** grpc-gateway cannot map *bidi*, so HTTP is served by a thin hand-
  rolled SSE adapter over the same domain `Event` + the same application service — a
  server-streaming `Prompt` (SSE) plus unary `Approve`/`Cancel`. Both surfaces call the
  identical `agent` use-case; neither sees an OpenAI type.

### 7.1 Proto sketch (`contracts/proto/ozz/v1/harness.proto`, go_package → `contracts/gen/go`)

```proto
syntax = "proto3";
package ozz.v1;
import "buf/validate/validate.proto";

service HarnessService {
  // Unary setup/inspection.
  rpc CreateSession (CreateSessionRequest) returns (CreateSessionResponse);
  rpc GetSession    (GetSessionRequest)    returns (Session);

  // Converse — drive one run. First frame MUST be Prompt; then zero or more
  // ResumeApproval / Cancel frames. Server streams Events until result.
  rpc Converse (stream ConverseRequest) returns (stream ConverseResponse);
}

message ConverseRequest {
  oneof kind {
    Prompt          prompt          = 1;   // mandatory first frame
    ResumeApproval  resume_approval = 10;  // resolves a permission.ask
    Cancel          cancel          = 11;  // aborts the in-flight turn
  }
}
message ConverseResponse { Event event = 1; }

message Prompt {
  string session_id = 1 [(buf.validate.field).required = true];
  string text       = 2 [(buf.validate.field).required = true];
}
message ResumeApproval { string ask_id = 1 [(buf.validate.field).required = true]; bool allow = 2; }
message Cancel {}

message Event {
  string type = 1;            // mirrors session.EventType
  int64  seq  = 2;
  int32  turn = 3;
  string text = 4;
  ToolCall   tool_call   = 5;
  ToolResult tool_result = 6;
  PermissionAsk ask      = 7; // carries ask_id echoed back in ResumeApproval
  Result result          = 8;
  Usage  usage           = 9;
}
```

### 7.2 HTTP/SSE mapping (thin adapter, same domain Event)

| HTTP | Maps to | Notes |
|---|---|---|
| `POST /v1/sessions` | CreateSession | JSON body → session_id |
| `GET  /v1/sessions/{id}` | GetSession | JSON snapshot |
| `POST /v1/sessions/{id}/prompt` | Converse(Prompt …) | response is `text/event-stream` of Events |
| `POST /v1/sessions/{id}/approve` | Converse(ResumeApproval …) | resolves the paused ask out-of-band on the SSE run |
| `POST /v1/sessions/{id}/cancel` | Converse(Cancel) | cancels the in-flight turn (ctx cancel) |

The bidi gRPC stream and the HTTP-SSE-plus-unary surface are two adapters over the same
`agent` application service and the same `session.Event`; closing either stream cancels
the run `ctx`, which the loop observes.

---

## 8. Where each gauntlet item is enforced (doc 08 closing test)

| # | Gauntlet check | Enforced in |
|---|---|---|
| 1 | Loop can pause/resume/cancel | `agent/loop.go` (channel producer) + `agent/permission.go` (StateAwaiting) + `ctx` to `LLMProvider.Stream` |
| 2 | Edit errors if file not read this session | `adapter/tools` Edit tool against the per-session read-ledger in `Workspace` |
| 3 | Plan mode denies Edit/Write/non-RO Bash at harness level | `tool/catalog.go` plan-mode filter + `governance` PreToolUse, before dispatch |
| 4 | Compaction preserves paths/decisions, drops file bodies | `agent/compaction.go` (single-summary seam; prompt template preserves plan/paths) |
| 5 | PreToolUse hooks fire and block on exit 2 | `agent/dispatch.go` calls `HookRunner` before each tool; `adapter/hookexec` maps exit 2 → Block |
| 6 | Hour-long session stays cheap (cache hit > 0.7) | `prompt.Layered` stable prefix/volatile suffix + `adapter/openai` breakpoint placement; `Usage.CacheHitRate()` metric |
| 7 | Subagent returns only its final string | `agent/subagent.go` — child loop, only final text folded as one ToolResult |
| 8 | Permission denies across merged scopes | `governance/permission.go` `Evaluate` (deny→ask→allow, Scope precedence) |
| 9 | Sandbox is a separate layer | **Seam only in v1** — `port` boundary around Bash execution (`CommandRunner`) is where an OS-sandbox adapter slots later; documented non-goal |
| 10 | Tool-description bug is diagnosable by reading it | `tool.ToolSpec.Description` convention (doc 07 §9); descriptions reviewed as onboarding docs |

Also enforced: **read-parallel / mutate-serial** (doc 08 #4) in `agent/dispatch.go`,
keyed off `Tool.ReadOnly()`; **stop conditions** (`Limits`/`Counters` on Session);
**compound-Bash + wrapper canonicalization** in `governance/bash.go`.

---

## 9. Non-goals for v1 (designed-in seams, not built)

| Deferred | Seam left for it |
|---|---|
| OS-level sandbox (Landlock/seccomp/Seatbelt) | `CommandRunner` port around Bash; sandbox is a wrapping adapter |
| Four-tier compaction cascade | `agent/compaction.go` is a `Compactor` interface; v1 ships single-summary impl |
| MCP client (**streaming-HTTP transport ONLY — stdio MCP is explicitly NOT supported, ever**) | `tool.Catalog` is the registration seam; future MCP tools register as `Tool`s over a streaming-HTTP MCP client. No `os/exec`-spawned stdio servers. |
| Repo map / embeddings | a future read-only `Tool`; no core change |
| Multi-vendor model routing | `LLMProvider` port already abstracts this; v1 ships one adapter |
| Persistent cross-session memory | `SessionStore` + AGENTS.md/CLAUDE.md discovery already cover the file-as-memory case |
| Slash commands / skills | `prompt` volatile-suffix injection seam |

The guiding restraint (doc 08): build the *shape*, instrument it, and resist features
before the loop, tools, permissions, hooks, and cache all work.
