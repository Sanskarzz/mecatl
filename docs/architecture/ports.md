# The ports (`engine/port`)

> Part of the [mecatl architecture guide](../architecture.md).

Small interfaces, `context.Context` first. Each has a fake adapter so the loop
runs with no network and no disk.

| Port | Responsibility | Signature (verbatim) |
|---|---|---|
| `LLMProvider` (`llm.go`) | provider-agnostic model call; streams neutral chunks | `Stream(ctx context.Context, req LLMRequest) (iter.Seq2[Chunk, error], error)` · `Capabilities() ProviderCapabilities` (multimodal-input flags; decorators must forward the inner provider's) |
| `SessionStore` (`store.go`) | persist/retrieve session state | `Save(ctx context.Context, s *session.Session) error` · `Load(ctx context.Context, id session.SessionID) (*session.Session, error)` — a store may additionally implement the optional `PrunableStore` (`List`/`Delete`) for retention ([observability & persistence](observability.md)) |
| `HookRunner` (`hookrunner.go`) | run a lifecycle hook → outcome (`hookexec` maps an external process exit code; `modelhook` maps a quarantined checker model's verdict — block/sanitize/advisory) | `Run(ctx context.Context, ev governance.HookEvent) (governance.HookOutcome, error)` |
| `PermissionPolicy` (`permission.go`) | deny→ask→allow across merged scopes; per-session learned allows | `Evaluate(ctx context.Context, sessionID session.SessionID, mode session.PermissionMode, c session.ToolCall, ws tool.WorkspaceReader) governance.PermissionDecision` (ws is the READ-ONLY discovery root for file-based permission config, issue #13; nil = no project config) · `Learn(sessionID session.SessionID, c session.ToolCall)` (the allow-**always** verdict; lowest scope, never overrides a deny or plan mode) |
| `EventSink` (`log.go`) | relay loop events to the API stream (mirrors live) | `Emit(ctx context.Context, ev session.Event)` |
| `EventLog` (`eventlog.go`) | DURABLE per-session event timeline, distinct from `EventSink` — a later consumer reads it back (cloud-native Phase 3). The loop NEVER calls it; persistence lives at the relay | `Append(ctx, id, ev) error` (must be durable before returning nil; at-most-once, no dedup) · `Read(ctx, id) iter.Seq2[session.Event, error]` (append order, streamable) |
| `ToolCallRecorder` (`log.go`) | structured per-tool AUDIT (distinct from `Diagnostics`) | `ToolCall(id session.SessionID, call session.ToolCall, result session.ToolResult, queued, took time.Duration)` |
| `PermissionStore` (`permission.go`) | persist/replay per-session learned allow-always verdicts | `Save`/`Load` of learned rules (powers the verdict-replay consumer, [observability & persistence](observability.md)) |
| `Diagnostics` (`diagnostics.go`) | injected operational-logging seam (NO global slog in `engine/` or `internal/`) | `Log(ctx, level Level, msg string, args ...any)` · `With(args ...any) Diagnostics` |
| `Clock` (`clock.go`) | abstract wall clock | `Now() time.Time` |

`SessionStore` may additionally implement `PrunableStore` (`List`/`Delete`) for child-session retention ([observability & persistence](observability.md)). That makes **11** port interfaces in `engine/port`; the loop consumes them through injection only.

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
    ChunkReasoning     // display-only reasoning summary delta
    ChunkReasoningItem // opaque reasoning REPLAY blob → Message.Reasoning
    ChunkToolCall
    ChunkUsage
    ChunkDone
    ChunkPhase         // opaque phase marker → Message.ProviderPhase (issue #46)
)

type Chunk struct {
    Kind     ChunkKind
    Text     string             // text / reasoning summary / replay blob / phase marker, per Kind
    ToolCall *session.ToolCall  // on ChunkToolCall
    Usage    *session.Usage     // on ChunkUsage
    Stop     session.StopReason // on ChunkDone
}
```

The `ChunkReasoning` (display summary) vs `ChunkReasoningItem` (replay blob)
split is deliberate and provider-neutral — Anthropic's thinking delta maps to
the former, its `(thinking,signature)` replay token to the latter; they must
never be conflated. `ChunkPhase` follows the same opaque-replay discipline.

### The tool contract and the FS seam (`engine/tool`)

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

---

[← Architecture guide](../architecture.md)
