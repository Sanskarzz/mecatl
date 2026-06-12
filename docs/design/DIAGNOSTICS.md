# Diagnostics, audit, and the global-slog ban

Status: **shipped** (logging-architecture refactor, iterations 1–3). This is a
correction/reference doc, not an essay — it records the seams, the sink-per-binary
policy, the build-once rule, and the enforcement guard.

## Two seams, deliberately separate

The harness has two distinct logging concerns, and they are NOT the same port:

- **`port.Diagnostics`** (`engine/port/diagnostics.go`) — general-purpose
  operational logging: composition decisions, degraded-mode warnings, lifecycle
  notes. Low-volume, human-readable. The agent loop and the composition layer write
  to it; domain packages stay silent. Contract is tiny and slog-shaped
  (`Log(ctx, level, msg, args...)` + `With`), but the port imports only `context`
  (a tripwire test, `diagnostics_imports_test.go`, keeps it backend-free). Callers
  that inject nothing get `NopDiagnostics` (`app.Build` defaults to it), so every
  consumer is nil-safe.
- **`port.ToolCallRecorder`** (`engine/port/log.go`) — the per-tool AUDIT seam:
  `ToolCall(id, call, result, took)`, one structured record per tool execution.

They are separate because they answer different questions. Diagnostics is "what is
the harness doing / why did it degrade"; the recorder is "what did each tool call
do, with what result, how long" — an audit trail with a fixed shape, consumed by
telemetry as counters + a latency histogram. Folding them into one port would force
either the audit into free-form strings or the diagnostics into a tool-call shape.

## Three observability channels

| Channel | Port | Adapter | Carries |
|---|---|---|---|
| Operational logging | `port.Diagnostics` | `slogdiag` (over `log/slog`) | composition/lifecycle/degraded-mode lines |
| Per-tool audit | `port.ToolCallRecorder` | `telemetry` (counters + histogram); `jsonlstore` audit | one record per tool call (role-tagged: `role="main"` for the main engine; bounded family per child via `MetricsRoleScoper`) |
| Metrics / traces | `port.EventSink` | `telemetry` (OTel SDK) | counters/gauges/spans from the event stream (every series carries the bounded `role` label) |

All three are injected at composition; none reaches for a global.

## Sink-per-binary policy

`slogdiag` owns the `port.Level → slog.Level` map and the text-vs-JSON handler
choice; the composition layer picks the destination:

- **mecated** (daemon): `slog.SetDefault` installs a stderr/text/Info logger and
  `slogdiag.NewFromLogger(logger)` wraps the SAME logger. stderr/journald is the
  correct sink for a server, so the `slog.SetDefault` here is the DELIBERATE,
  PERMANENT third-party-slog bridge — any ambient `slog.Default()` use (a transitive
  dependency, the perf surface's nil-Logger fallback) is correctly routed there.
- **mecatui** (TUI): the global slog default is redirected on EVERY startup path, in
  two layers. (1) A UNIVERSAL baseline — `installBaselineSlog` at the top of `run()`,
  before any transport resolution or the Bubble Tea program — floors the default to
  `io.Discard`. The TUI owns the alt-screen and has no in-process diagnostics of its
  own, so for the client-only transports (external `--server`, or reuse of an
  already-running mecated — both of which return early from `resolveTransport`) the
  floor is the whole story: ambient/third-party slog is silently dropped rather than
  corrupting the render. (2) A REFINEMENT — in the host-embedded branch only,
  `resolveTransport` opens a FILE at `$XDG_STATE_HOME/mecatl/mecatui.log` (fallback
  `~/.local/state/...`) once and installs a second `slog.SetDefault` onto that file
  writer (the same writer backs the `slogdiag` sink AND the perf surface's
  `*slog.Logger`). That second call wins for the embedded path, so the embedded
  server's ambient slog is captured and operator-recoverable instead of discarded.
  Under `--quiet` every writer is `io.Discard`. Either way, no transport mode leaks a
  stray slog line to the alt-screen.

`cmd/` mains are the ONLY layer allowed to call `slog.SetDefault` — they own the
third-party-slog bridge. `slogdiag.NewFromLogger(nil)` falls back to a DISCARD
logger (not `slog.Default()`), so even the adapter's own fallback can't reach the
global default.

## Build-once facts

Composition facts (token counter, compaction strategy, session-store kind, feature
enable/disable lines) are emitted ONCE, in `app.Build`. They are NEVER emitted in
the per-engine deps builders (`engineDepsForProvider` / `childEngineDepsForProvider`)
— a per-session engine is re-derived on every provider/model change, and re-emitting
the facts there caused N× duplication of the same lines. The build-once FACTS stay a
one-shot main-engine concern; child engines never re-emit them.

(Child engines DO emit live per-run diagnostics — see "Session correlation" below —
but only the run-scoped lines from the two emitters, never these build-once facts.)

## Session correlation

Diagnostics lines are correlated to the originating run at the point they are
emitted, via `port.Diagnostics.With`:

- **Per-run, not at construction.** The engine is built BEFORE the session id
  exists and is often SHARED across sessions (`service.go` builds the engine before
  `session.New`). So the run-scoped sink is bound ONCE per run, inside
  `Engine.RunContent`, where the live `*session.Session` is in scope:
  `runDiag := deps.Diagnostics.With("session", id)` — plus `"agent", role` when the
  engine carries a `Deps.Role` (a child/subagent engine). It is stored on the `Run`
  (`Run.diag`) and the two emitters log through it. Binding at construction would
  cross-tag every session that shares the engine; binding per-run is what makes
  `TestRunDiagnosticsNoCrossTag` hold. It is Nop-safe: `deps.Diagnostics` is never
  nil post-`NewEngine`, and `With` on `NopDiagnostics` returns `NopDiagnostics`.
- **Main engine vs children.** The MAIN engine has `Deps.Role == ""` → lines carry
  only the `"session"` key. A CHILD engine (Subagent explorer `"task"` / per-def
  `"task:<name>"` / model-override `"task:model=<model>"`, team member
  `"member:<name>"`, Parallel branch `"parallel"`, Parallel judge
  `"parallel-judge"`, user-model reviewer `"usermodel-review"`) sets `Deps.Role` →
  lines carry `"session"` + `"agent"=<role>`, so interleaved child diagnostics are
  readable.
- **Children emit Diagnostics, plus ROLE-SCOPED metrics (issue #47).** Child
  engines carry the REAL `cfg.diag()` (not `NopDiagnostics`) so their
  degraded-mode warnings and policy denies reach the operator channel, correlated.
  Telemetry/audit is now role-scoped rather than off: when the composition wires
  `app.Config.MetricsRoleScoper` (mecated always; the embedded TUI server only
  with `--perf` on), each child's `Deps.Sink`/`Deps.ToolCallRecorder` become a
  role-tagged view over the SHARED telemetry instruments — keyed on the BOUNDED
  family `roleFamily(role)` (`main|subagent|member|parallel|usermodel|child`;
  the raw role, which can embed a def name or model id, never reaches a label) —
  so child turns/tool-calls land on role-distinct series and CANNOT double-count
  against `role="main"`. Without a scoper (the no-perf path) both stay nil,
  byte-identical to the old unmetered child shape. The conversation event stream
  is still `Run.Events()` only; nothing about child event delivery changed.

### The three emitters (and what is deliberately NOT emitted)

The agent loop emits exactly THREE run-scoped diagnostics lines, all through the
run-scoped sink (the third was a CONSCIOUS amendment by the background-subagents
design — see its invariant audit):

1. **Compaction failure** (`maybeCompact`, `LevelWarn`, key `error`) — the two
   branches that previously SWALLOWED a compaction error (the `Compact` error and
   the `ReplaceHistory` rejection) now log a degraded-mode warning before
   continuing uncompacted. Behaviour is unchanged (the run still continues); the
   warning just stops the silent swallow.
2. **Policy deny** (`authorize`, `LevelInfo`, keys `tool`, `reason`) — at the one
   site where the permission POLICY resolves a call to `Deny`, the deny reason
   (which otherwise reaches only the client event via `denyResult`) is surfaced on
   the operator channel.
3. **Background drain abandon** (`drainChildren`, `LevelWarn`, keys `ids`, `cap`)
   — at run end every live BACKGROUND subagent is cancelled and joined in TWO
   phases: a hard cap (`childDrainCap`, 10s), then — because the cap can be
   burned by the run's OWN emit backpressure, not a wedged child — an
   `abortEmits()` that unblocks any child parked in its own end-emit, plus a
   short grace re-join (`childDrainGrace`, 1s). Only a child still unjoined
   after BOTH phases is ABANDONED (the registry seal makes its residual emits
   safe no-ops) and this one warning names the abandoned ids (ids only) — so a
   line here means a genuinely wedged child, never a slow consumer. Rare by
   construction: ctx-cancel kills the child's stream and shell promptly, and
   the osfs runner's `cmd.WaitDelay` bounds the grandchild-pipe residual.

Deliberately NOT emitted, because the `session.Event` taxonomy already owns them
(emitting here would be double-logging):

- **Cancellation** — `EvResult{Stop: StopCancelled}` carries it.
- **Tool errors** — `EvToolResult{IsError}` + the `ToolCallRecorder` audit seam.
- **Compaction success** — `EvCompaction` carries it.
- **Permission ask / allow** — `EvPermissionAsk` / `EvToolCall` carry them; only
  the policy DENY is logged, never allow or ask.

Two further emissions ride the PARENT run's diagnostics (`parentCaps.diag`) at the
supervisor/child-posture level, OUTSIDE the agent loop's three-line contract (they are
not loop lines and do not count against it): the headless subagent auto-deny INFO
(`resolveChildAsk`, when a non-interactive child's permission ask cannot be surfaced)
and the team member-reopen-failure WARN (`warnUnexpectedReopen`, when a member's
`Reopen` fails for a reason other than the expected cancelled case).

## The ban + guard

Global slog is banned in `engine/` and `internal/`: no `slog.Default()`, no `slog.SetDefault()`,
no package-level `slog.Info|Warn|Debug|Error(Context)?`. Diagnostics flow through
the injected `port.Diagnostics` seam instead. The ban is enforced by **`forbidigo`**
in `.golangci.yml` with precise patterns, scoped so `engine/` and `internal/` are covered and
`cmd/` mains are exempt (issues exclude-rule on `^cmd/`; `_test.go` also exempt).
The patterns are call-shape-precise: `slog.New*` / `slog.Handler` / `HandlerOptions`
/ `Level` / `DiscardHandler` (slogdiag's legitimate internals) and the `*slog.Logger`
type (the perf surface) do NOT match. Each forbidden hit emits a message pointing
the contributor back at this doc and the `port.Diagnostics` seam.
