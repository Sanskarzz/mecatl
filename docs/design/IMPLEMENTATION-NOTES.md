# Implementation Notes

Detailed, per-subsystem implementation and status narrative — the "how it was built and
why it's shaped this way" detail that used to live inline in `CLAUDE.md`. It was moved here
when `CLAUDE.md` was trimmed back to a lean correction file (~1k words).

**This is reference, not a contract.** It captures decisions, invariants, and the
SHIPPED/DEFERRED state of each subsystem as of the trim. When a subsystem has a dedicated
spike/design doc (`MULTI-PROVIDER.md`, `WORKSPACE-TRUST-SPIKE.md`, `SOUL-SPIKE.md`,
`MEMORY-*.md`, `AGENT-TEAMS-SPIKE.md`, `ALLOW-ALL-POSTURE.md`), that doc is the deeper
source; this file is the one-stop index of the dense detail that was crammed into CLAUDE.md.
Prefer updating the relevant design doc + this file over re-growing CLAUDE.md.

---

## Domain — `engine/session/` (lifecycle recovery)

A turn always drives the `Session` aggregate to a terminal state within one
`Engine.Run`; the engine never recovers it. Three intention-revealing seams —
one per terminal state, each legal ONLY from its own state — re-enter the loop
on a reused session at the run-entry funnel (`loadAndReopen`):

- **`Reopen()`** — `completed → idle` only. Clears stop + pending, resets per-run
  `Counters`, preserves history. The clean-end-of-run continuation seam.
- **`Interrupt()`** — `cancelled → idle` only. Mirrors Reopen's reset but is a
  SEPARATE method because its precondition (`StateCancelled`) and its
  history-repair invariant differ. A turn cancelled mid-dispatch (ctx cancel
  AFTER `RecordAssistant` but before `RecordToolResults`) leaves the trailing
  assistant message with `ToolCall`s that never got a result — a dangling
  `tool_use` (Anthropic) / `function_call` (OpenAI) that both providers 400 on at
  replay. The private `closeOutInterruptedTurn(message)` finds the LAST assistant
  message, collects the `CallID`s already answered by the `RoleTool` messages
  that follow it, and appends one `NewToolError(call.ID, message)` per
  unanswered `ToolCall.ID` (in `ToolCalls` order) via the same
  `Conversation.Append(NewToolMessage(...))` path `RecordToolResults` uses. The
  message is SEAM-ACCURATE, durable model-facing history (the
  childAutoDenyMessage discipline — never claim a user action that didn't
  happen): Interrupt passes "tool call interrupted by cancellation"
  (byte-identical to the pre-#51 text); Recover passes "tool call aborted: the
  run failed before this call's result was recorded". `IsError` here means the
  call never ran to a result, not a real tool failure. It is idempotent and a
  NO-OP for clean shapes: an assistant with no tool calls, the
  dangling-trailing-user shape (cancel before the first token — LEFT AS-IS,
  benign for both providers), and a turn whose results all landed before the
  cancel. Partial results are honoured (only the missing `CallID`s are
  closed out).
- **`Recover()`** — `failed → idle` only (issue #51). The third sibling: same
  reset as Reopen/Interrupt (the shared private `resetToIdle()` — one reset
  body, three per-method state guards, so the reset fields cannot drift across
  seams) and the SAME `closeOutInterruptedTurn()` history repair with the
  failure-accurate message (a turn that failed mid-stream/mid-dispatch can
  orphan trailing tool calls exactly like a cancel), so a transient provider
  failure (an upstream 5xx that exhausted the resilience retries) degrades to
  "retryable" instead of permanently bricking the session. Recovery makes retry
  POSSIBLE, not guaranteed — a permanent-cause failure (auth/config) simply
  fails again with the conversation context intact, and the user can clear. The
  subagent `resume:` policy is deliberately NOT changed (see the Subagent
  resume note below): a failed child is still not resumable — re-delegate
  instead.

The service's `loadAndReopen` (shared by `LoadSession`/`LoadSessionWithMCP`, and
upstream of every `StartRunContent` run-entry) branches on state: `completed →
Reopen`, `cancelled → Interrupt`, `failed → Recover`, then re-persists the
recovered snapshot. This is what fixes the wedge where an interactive session
that was cancelled mid-turn rejected every later prompt with `RecordUserPrompt
from "cancelled"` (and, since issue #51, the same wedge from `"failed"` after a
transient provider failure). Regression:
`TestStartRunContentRecoversCancelledSession` +
`TestStartRunContentRecoversFailedSession`; adapter-level orphan guards:
`anthropic.TestRequestNoOrphanedToolUseAfterInterrupt`,
`openai.TestRequestNoOrphanedFunctionCallAfterInterrupt`.

---

## Domain — `engine/governance/`

Permission `Effect`/`Scope`/`Rule` + `Evaluator`, bash splitting/canonicalization, hook
event types. **Session-free** (`session` imports `governance`, never the reverse).

Scopes run highest→lowest `Managed > CLI > LocalProject > SharedProject > User > ScopeBuiltinDefault`
(the last, added for issue #13, is the built-in floor's scope — BELOW every config scope). A
higher-scope config Allow may loosen **only** the `ScopeBuiltinDefault` Ask floor; it can
never suppress a configured Ask. Deny-dominant + plan-mode gating still hold.

The allow-all operator posture (`--yolo` → `app.Config.AllowAllTools`, injected by `mainRules`
in `internal/app/build.go`) is a **rule** (a single `ScopeCLI` allow-all), NOT a
`PermissionMode` and NOT an evaluator bypass — it loosens only the `ScopeBuiltinDefault`
floor, so both invariants above are unchanged. (See `ALLOW-ALL-POSTURE.md`.)

Memory + soul pre-approval (issue #14): the six memory tools (`Remember`/`Recall`/`SearchMemory`
+ cross-project `RememberUser`/`RecallUser`/`SearchUserModel`) and the synthetic `soul:apply`
action are explicit `ScopeBuiltinDefault` Allows in `defaultRules()` — pre-approved at the
floor so they don't prompt by default, but auditable (in source + ENABLED logs) and
**overridable** (a higher-scope `settings.yaml` Ask/Deny still wins). Being floor-scoped +
tool-name-exact they can only LOSE to a higher scope and loosen no other tool's Ask, so the
invariants above are structurally untouched. `soul:apply` is consulted at **soul-load (build
time)** in `selectSoulSource` via the same evaluator/resolver (Allow⇒apply, Deny⇒withhold,
**Ask⇒withhold** — no interactive build-time gate). The three read-only child-observability
tools (`InspectSubagent`/`InspectMember`/`SubagentStatus`) carry the same floor-scoped Allows
(issue #37 — see the Subagent-inspection section below for the rationale; guarded by
`internal/app/inspect_perm_test.go`).

## Domain — `engine/prompt/`

Two-layer prompt assembly + AGENTS.md/CLAUDE.md discovery; the turn-0 `InstructionAssembler`
chain and its consumer-local ports (`MemoryIndexSource`, `SoulSource` — issue #14 Phase 1's
read-only persona seam, `UserModelSource` — issue #14 Phase 2's cross-project operator-FACTS
seam). Turn-0 ORDER is soul → memory index → user model (identity → saved project facts →
operator model), all on the volatile turn-0 user-message seam (never `StablePrefix`).

**`SoulSource` stays trust-/provenance-UNAWARE** (issue #14 Phase 3 Item 2): the soul's
USER-vs-PROJECT provenance + `--trust-project` gate + USER-WINS precedence are decided in
`internal/app/soulselect.go` (composition), which hands the assembler the single winning
source — `prompt` neither knows nor cares which provenance won (the `SoulSource` interface is
unchanged).

## Port — `engine/port/`

The PORT interfaces the loop consumes (`LLMProvider`, `SessionStore`, `HookRunner`,
`PermissionPolicy`, `Clock`, `Logger`, `EventSink`). `PermissionPolicy.Evaluate` carries the
session as a READ-ONLY `tool.WorkspaceReader` (Root+Read+Stat — issue #13) so file-based
config resolves per-session against that root without a mutate-capable handle; `ws` may be nil
(child/member engines with no resolver). `Clock`'s production implementation is
`engine/adapter/wallclock`, wired in `engineDepsForProvider`/`newChildEngineWithHooks`
(issue #53 — previously never injected, leaving all latency observations zero).

## Application — `engine/agent/` (subagent workspace policy)

The loop (`Engine`/`Run`), dispatch, permission pause/resume, compaction, the Subagent delegation tool,
and the agent-team `Supervisor`/`TeamTool`. (See `AGENT-TEAMS-SPIKE.md`.)

**No-progress handler (loop Step 5, `finishTurnNoTools`).** A reasoning model can complete a
turn producing NEITHER a tool call NOR meaningful text — only a reasoning blob (a
"reasoning-only"/empty turn). The pre-fix loop terminated on zero tool calls regardless of
text, silently completing with `StopEndTurn` and an empty deliverable. Now: a turn with
meaningful text still ends the run (honouring `streamStop`); a no-progress turn is RECORDED first
(its reasoning blob is preserved on history for replay — decision D-4), then the loop injects a
BOUNDED continuation
user message (`noProgressNudgeText`, a *new* user message, not a re-send and not a forced text
block) up to `Deps.MaxNoProgressNudges` times (default `defaultNoProgressNudges`=2 applied in
`NewEngine`; `Config.MaxNoProgressNudges` threads an operator override through
`engineDepsForProvider`; `<0` disables). The nudge is **GRADUATED** by attempt, selected on the
PRE-INCREMENT counter: the early attempt(s) get the gentle `noProgressNudgeText`; the FINAL
attempt before give-up (where `*noProgressNudges == nudgeCap-1` at entry to the nudge branch)
gets the forceful `noProgressExtractiveNudgeText`, which tells the model to stop investigating
and emit its best-effort final answer NOW from information already gathered. Because the give-up
branch and the increment both read the pre-increment value, the extractive nudge is structurally
guaranteed one more model turn before give-up — a model that answers on that turn completes
`StopEndTurn` (the rescue path), not `StopNoProgress`. With `nudgeCap==1` the single nudge IS the
final one → extractive only, no gentle attempt (no `tool_choice` forcing on either nudge; both are
plain `RoleUser` messages). Each nudge emits a visible `EvNoProgress` event (transient, advisory,
NOT recorded to history, NOT a diagnostics line — the event taxonomy owns it, so the "loop emits
exactly THREE diagnostic lines" invariant holds); the final/extractive attempt carries a DISTINCT
advisory text ("final attempt: requesting a best-effort answer") so clients can render it as a
last-ditch notice, while the gentle advisory and terminal give-up texts are unchanged. On budget exhaustion the
run terminates CLEANLY via `terminateComplete` with `StopNoProgress` — a NON-error terminal, so
the session ends `completed` and stays Reopen-recoverable (never `StopError`, never an infinite
loop). The nudge loop is ALSO independently bounded by `Limits.MaxTurns` (each nudged turn goes
through `BeginTurn`), and a stop-condition/cancellation trips at Step 2 before a nudged turn.

**`streamStop` discipline — the masking guard (load-bearing predicate).** The no-progress nudge
applies ONLY when the empty turn ended on a BENIGN end: `streamStop ∈ {StopEndTurn, StopNone}`
(the model simply finished). Both adapters' `mapStop` relay a REAL terminal condition on the
ChunkDone stop, NOT as a Go error: `max_tokens`/`refusal`/`incomplete`/`failed` → `StopError`,
`cancelled` → `StopCancelled` (and any limit reason). Such a turn can ALSO come back empty (a
truncated/refused response), and nudging "please continue" + relabeling it `StopNoProgress` would
MASK the real reason — the very silent-mislabel disease the handler fixes. So `finishTurnNoTools`
surfaces a non-benign `streamStop` BEFORE the no-progress branch: `StopError` → `terminate` as a
failure (carrying a diagnostic cause); any other non-benign stop → `terminateComplete` carrying
that reason verbatim — never nudged, never relabeled. This composes with the meaningful-text path
above, which already honours `streamStop`. Pinned by `TestEmptyTurnWithTerminalStopNotNudged`
(StopError) and `TestEmptyTurnWithNonErrorTerminalStopSurfaced` (a non-error non-benign stop),
both of which FAIL on the pre-fix code (the masking bug).
Because the fix lives in the SHARED `Engine.drive`, it covers main + Subagent children + fork
branches + every team member + the team lead's synthesis turn (a no-progress synthesis is driven
to a real report, not an empty `joinTeamFallback` skeleton). **No `tool_choice` forcing**: forcing
tool use is incompatible with Anthropic extended thinking and the OpenAI reasoning path — the
provider-agnostic route is the bounded nudge + the persistence prompt (`agencyDelta`,
composition layer — supplied for ALL model families incl. Claude since issue #49, after Claude
was observed announcing actions without emitting the tool calls). `EvNoProgress`/`StopNoProgress` are STRING passthroughs on the wire (proto
`type`/`stop` are strings, not enums), so no proto regen was needed; mecatui renders
`EvNoProgress` as a muted notice and `StopNoProgress` as a `stopped · no progress` footer label.

**Token budget — the shared loop-level ceiling (`StopBudget`).** `agent.Deps.MaxRunTokens`
(0 = disabled) is a per-RUN cumulative token ceiling checked at the turn BOUNDARY in
`Engine.drive` (Step 2, after the existing `sess.StopReason()` and `ctx.Err()` checks, before
`BeginTurn`) against the run's accumulated `session.Usage` via `Usage.TotalTokens()`
(input+output; cache tokens excluded — `CacheReadTokens` is a subset of `InputTokens`,
`CacheWriteTokens` is a side cost). The subset invariant holds CROSS-PROVIDER because the
adapters normalize to it: OpenAI's `input_tokens` already includes cached tokens; Anthropic's
raw `input_tokens` EXCLUDES cache reads/writes, so its adapter folds `cache_read_input_tokens`
+ `cache_creation_input_tokens` into `InputTokens` at the single `session.Usage` mapping site
(`anthropic/stream.go` `translateMessageStop`) — before that fix `--max-run-tokens`
UNDERCOUNTED Anthropic runs (cache-served prompt tokens never hit the budget).
When `total.TotalTokens() >= MaxRunTokens` the loop ends via
`terminateComplete(…, session.StopBudget, …)` — a NON-error CLEAN terminal (completed path,
Reopen-recoverable), so it mirrors `StopNoProgress` exactly. The boundary check means an
in-flight turn always COMPLETES (no mid-stream abort → no-replay-after-first-chunk holds); a turn
whose usage massively overshoots still finishes, then the budget trips before the next turn. It
is NOT a `port.LLMRequest` field (the request stays provider-neutral) — it is composition-tunable
(`app.Config.MaxRunTokens` → `--max-run-tokens`) and INHERITED by every engine via
`engineDepsForProvider`; `childEngineDepsForProvider` delegates there and does NOT clear it, so
Subagent/team-member/lead/Parallel children inherit the same ceiling. `StopBudget` is the
PER-ENGINE half of the AGENT-TEAMS-SPIKE's named "Deferred 4A" brake — landed once for every
delegation path, but it bounds ONE run and `Reopen` resets its accumulator each round; the
team-AGGREGATE half is the separate `WithTeamTokenBudget` below. It is a
STRING passthrough on the wire (`session.StopBudget = "budget"`, no proto enum). Guards:
`agent.TestBudget*`, `session.TestStopBudgetIsCleanReopenableTerminal`,
`server.TestServiceBudgetSurfacesAndReopens`, `app.TestMaxRunTokensPropagatesToParentAndChild`.

**Team-aggregate token budget — the supervisor-level ceiling (`WithTeamTokenBudget`).** The
team-AGGREGATE companion to the per-engine `MaxRunTokens` closes the residual 4A item.
`Supervisor.WithTeamTokenBudget(n)` (0 = disabled, no nonzero default) is a TEAM-WIDE cumulative
token ceiling summed across ALL members and ALL rounds (the lead's synthesis included in the final
accounting). The accumulator discipline is load-bearing: each member's per-drive `EvResult.Usage`
(the run-CUMULATIVE figure — `driveOneTurn` returns it as a third value) is folded onto
`memberRT.tokensUsed` in the SAME single-goroutine capture block as `turnsUsed`, BEFORE `Reopen` —
NEVER also summed from `turn.end` (that double-counts; see `memberEventUsage`'s warning).
`teamTokensUsed()` sums `tokensUsed` over the roster on the single Run goroutine between rounds. The
trip sits in `Run`'s loop AFTER the `ctx` check and BEFORE `planRound` (whose `Drain`/`ClaimNext`
side effects must not fire for a round that never runs): `if s.tokenBudget > 0 &&
s.teamTokensUsed().TotalTokens() >= s.tokenBudget { s.budgetTripped = true; break }` (`>=` mirrors
`Engine.budgetExhausted`). The in-flight round always completes and the lead's synthesis turn STILL
runs after the loop — its drive usage folds into the OUTCOME (`TeamOutcome.Usage`) but never the
GATE (it runs after the loop, structurally cannot trip). Members are NOT individually stopped (no
new `MemberStopReason`); `TeamOutcome` gains `BudgetExhausted` + `Usage`, and `teamStop` returns
`session.StopBudget` when `!Quiescent && BudgetExhausted` (a client distinguishes a budget-stop from
a round-cap). The deliverable header (`convergenceHeader`) and the trusted synthesis-prompt "Team
status:" line both state the budget stop, so BOTH entry points surface it. It is composition-tunable
(`app.Config.MaxTeamTokens` → `--max-team-tokens`; `server.Config.TeamTokenBudget` for the gRPC
CreateTeam path; `agent.WithTeamToolTokenBudget` for the in-catalog Team tool) and a per-call Team
`max_team_tokens` may only TIGHTEN it (`tightenLimit`). It is ORTHOGONAL to the per-engine
`MaxRunTokens` (which bounds ONE member drive and resets on `Reopen`) — both compose. The Supervisor
sum (`TeamOutcome.Usage`) is authoritative for the budget gate; the TeamTool sink's `turn.end` sum
(`memberEventUsage`) stays authoritative for the `EvTeamEnd` payload — they are equal by
construction, documented not reconciled. Guards: `agent.TestTeamTokenBudget*` /
`TestTeamToolTokenBudget*` / `TestConvergenceHeaderBudgetMatrix` /
`TestDeliverableTier1ForcedByBudget`, `server.TestRunTeamBudgetExhaustedOutcome`,
`app.TestMaxTeamTokensPropagates`, `cmd/mecated.TestAppConfigMapsMaxTeamTokens`. DEFERRED: the gRPC
wire handlers still discard the `TeamOutcome`, so a `CreateTeamRequest.max_team_tokens` /
`RunTeamResponse` outcome field stays deferred proto work.

**Subagent per-call limits + wall-clock deadline (domain-only).** `subagentArgs` gains three OPTIONAL
pointer fields — `MaxTurns`/`MaxToolCalls` (TIGHTEN-ONLY via `tightenLimit`: a present positive
override applies only if it LOWERS the inherited `session.Limits`, so the model can make its child
stricter than the operator's bound but never looser; nil / non-positive ignored) and `TimeoutMs`
(a `context.WithTimeout` wrapping the child ctx). A deadline hit is detected by holding the
timeout ctx separately and checking `timeoutCtx.Err() == context.DeadlineExceeded` AFTER the
drain, rendered as a model-addressable time-budget tool error (distinct from a parent
cancellation). No wire/proto change (`session.Limits` semantics unchanged). Guards:
`agent.TestSubagentPerCall*`.

**Subagent child concurrency cap.** `SubagentTool.childGate` (a counting-semaphore channel, default
`defaultMaxConcurrentChildren = 4`, override `WithMaxConcurrentChildren`; the old
`WithMaxConcurrentSubagentShells` is a deprecated alias) is now acquired at the TOP of `run()` for
ALL Subagent children — forking AND forker-less — not only the worktree-forking path. Subagent is
read-only so the dispatcher fans out N concurrent Subagent calls in one turn; each consumes a child
session + an LLM slot (and, when shell-bearing, a forked worktree), so the gate is the single
fan-out brake bounding how many children run at once. This closes the previously-unbounded
forker-LESS fan-out. The team supervisor's round was ALREADY bounded
(`errgroup.SetLimit(s.concurrency)`, `WithTeamConcurrency`, default 4) — a code comment + the
`TestSupervisorRoundConcurrencyBounded` guard keep a future refactor from silently dropping it.
read-parallel/mutate-serial is UNCHANGED (the gate bounds child START, not dispatch ordering).
Guards: `agent.TestSubagentChildGateCapsForkerlessConcurrency`,
`agent.TestSubagentShellGateCapsConcurrentForks`, `agent.TestSupervisorRoundConcurrencyBounded`.

**Subagent per-call model override (`WithSubagentEngineFactory`).** `subagentArgs.Model` (optional opaque
string) pins THIS child to a specific provider model. The SubagentTool cannot build engines
(composition layer's job), so the composition root injects a closure
`func(model string) (*agent.Engine, bool)` via `WithSubagentEngineFactory`; `buildSubagentEngineFactory`
(internal/app) builds the override child through `newChildEngineForProvider` — the SAME
contamination-safe per-provider path the named-agent engines use — so Compactor/TokenCounter/
`Env.Model`/ContextWindow are RE-DERIVED for the override model, NEVER a clone-and-swap of an
existing engine's LLM (the "Provider is FIXED per session" invariant). The factory routes the
model on the parent's provider (the registry is keyed by provider, not model — cross-provider
routing by a bare model id stays a def's `provider:` concern), re-derives the window live-first via
`provReg.meta.contextWindowFor`, and returns `(nil,false)` for a blank/unroutable model (Subagent then
surfaces a model-addressable error). `agent` + `model` together is REJECTED (R9): a specialist
already pins its own engine/model. Reasoning-effort stays an adapter-construction Option (the
factory owns adapter construction), never a `taskArgs`/`port.LLMRequest` field. Guards:
`agent.TestSubagentPerCallModelRoutesToFactory`, `agent.TestSubagentPerCallModelUnknownErrors`,
`agent.TestSubagentAgentAndModelTogetherRejected`, `app.TestBuildSubagentEngineFactoryReDerivesForOverrideModel`.

**Def-less child default model (`Config.SubagentModel` everywhere — issue #35).** `SubagentModel`
(`--subagent-model`, mecated AND mecatui) used to reach only the def-RESOLVED child paths
(`resolveModelFor` via `resolveChildProvider`); four mint sites ignored it. The four are now
broadened — all through ONE helper, `resolveDefaultChildModel` (internal/app/agentdefs.go), which
delegates to `resolveModelFor(cfg, agents.AgentDef{}, parentModel)` (the zero-def chain — NOT a
parallel resolver) and re-derives the context window live-first when the model actually changes
(`childWindowFor` — the ONE window rule shared by `resolveChildProvider`, `resolveDefaultChildModel`,
and the per-call factory: it keys on the MODEL changing, not only a provider switch, so a
same-provider def `model:` compacts on ITS catalogued window too):
(1) `buildChildEngine` — the default Subagent explorer (split into the testable `childExplorerDeps`);
(2) `buildMemberEngine`'s DEFAULT (undefined-member) branch — LEAD INCLUDED in v1, the
lead-strong/members-cheap split is DEFERRED (a strong lead pins via an agent def today);
(3) `buildParallelChildEngine` — Parallel BRANCH children (split into `parallelChildDeps`);
(4) `buildParallelJudgeEngine` — NO change, the deliberate asymmetry: the judge stays on the
SESSION model (winner selection is a session-model judgement call; branches are the bulk-token
workers). All built through `newChildEngineForProvider` (never clone-and-swap). Same-provider
only (a def's `provider:` stays the cross-provider seam). Alias resolution happens ONCE at build
(`normalizeSubagentModel` in `app.Build`) and is FAIL-FAST: a non-empty value that does not
resolve to a usable model id (unknown bare alias, or an alias meaning inherit — the built-in
sonnet/opus/haiku unless overridden) is a BUILD ERROR naming the flag/value/reason, never a
warn-and-inert no-op; a valid override ⇒ one INFO fact (build-once-facts discipline). The
user-model REVIEW engine deliberately stays on the session model (a Stop-review hook engine,
not a delegation child). IMAGE-CAPABILITY FAILURE MODE (documented,
no gate in v1): a cheap child model lacking image input fails on the provider-400 path when a
child request carries an image — fail-safe, surfaced, recoverable; a capability-aware gate is
deferred until it bites. Guards: `app.TestDefaultExplorerUsesSubagentModel` (+InheritsParentWhenUnset),
`app.TestDefaultMemberUsesSubagentModel` (+InheritsParentWhenUnset),
`app.TestParallelBranchUsesSubagentModel`, `app.TestParallelJudgeStaysOnParentModel` (the asymmetry pin),
`app.TestPerCallModelOverridesSubagentModel`, `app.TestPerDefModelOverridesSubagentModel`,
`app.TestExplorerPromptKeysDeltaOnSubagentModel`,
`app.TestResolveChildProviderSameProviderModelWindow` (+ `TestSubproviderChildContextWindow`
case (c) — the same-provider window rule), `app.TestNormalizeSubagentModelUnresolvableIsError` +
`app.TestBuildFailsOnUnresolvableSubagentModel` (the fail-fast posture),
`app.TestBuildNarratesSubagentModelExactlyOnce` + the verbatim-keep pins,
`app.TestRegisterParallelToolThreadsSubagentModel` (the registration seam), and the composition e2e
`app.TestSubagentModelRoutesChildToCheapModel` (mutation-verified: reverting the explorer wiring
fails the deps test, the prompt test, AND the e2e).

**Subagent structured output (`output_schema` + `SubmitResult` + bounded validation-retry).** When
`taskArgs.OutputSchema` (a model-authored JSON schema) is present, the child is given a synthetic
`SubmitResult` tool (`engine/agent/structuredoutput.go`) whose PARAMETERS ARE that schema,
injected run-scoped via the new `RunOptions.ExtraTools` (never registered into the shared catalog,
so concurrent runs of the same engine never see it). The child prompt is augmented to "call
SubmitResult to deliver" — NO `tool_choice` forcing (incompatible with Anthropic thinking + the
OpenAI reasoning path). `SubmitResult.Execute` validates the submitted payload against the schema
via `session.ValidateJSON` (a JSON-schema SUBSET validator — object/array/string/number/integer/
boolean/null/properties/required/items/enum, FAIL-OPEN on any unsupported keyword; a domain helper,
the SINGLE structured-output validation choke point, DISTINCT from `ValidateMediaParts`) and records
the payload + validity onto the per-run tool struct. On a validation miss (or a child that never
called SubmitResult) the Subagent tool re-injects a model-visible correction prompt and re-drives the
SAME child session (`session.Reopen` + `Engine.RunContentWith`) up to `defaultStructuredOutputRetries`
(2), then gives up with the new `session.StopStructuredOutput` CLEAN terminal. The retry is a
SEPARATE bounded loop owned by `driveChild` — NOT a change to the hot shared `finishTurnNoTools`
(decision D2). The validated payload becomes the result text; exhaustion is rendered as a
MODEL-VISIBLE tool error carrying the last validation failure (never only a log line). Default (no
schema) = today's free-text behaviour. PER-ATTEMPT vs CROSS-ATTEMPT limits: each correction
re-drive `Reopen()`s the child, RESETTING its `Counters`, so per-call `MaxTurns`/`MaxToolCalls`
(and `WithChildLimits`) bound EACH attempt — up to `(1+defaultStructuredOutputRetries)×` across the
call (bounded, not a runaway); the cross-attempt brake is the TOKEN budget, which `driveChild` SUMS
across drives and re-passes (the `runOpts` override) to each `RunContentWith`. Guards:
`session.TestValidateJSONSubset`, `agent.TestSubagentStructuredOutputHappyPath/RetryCorrects/
ExhaustionFails/FreeTextUnchanged`, `agent.TestDriveChildStructuredPlainTextExhaustsToCleanTerminal`
(plain-text-never-SubmitResult exhaustion → StopStructuredOutput, child COMPLETED + Reopen-recoverable),
`agent.TestSubmitResultOverlayWinsAndIsAdvertised` (RunOptions overlay-first + advertised once).

**References convention (D5b) + read-only explorer catalog extraction.** The DEFAULT explorer
child's Role appends `explorerReferencesInstruction` (via `explorerPromptConfig`, the single site)
so the child ENDS its summary with a `References:` block listing relevant file paths (path or
path:line) — model-visible by construction (it shapes the child's output → the RESULT text). It is
DISTINCT from the shared `defaultTone` "Cite code as file_path:line" inline-citation sentence. The
read-only explorer tool surface `{Read, Grep, Glob, +sandboxed Bash when runner != nil}` is now
`readOnlyExplorerCatalog(runner)` — ONE definition shared by `buildChildEngine`,
`buildSubagentEngineFactory` (byte-identical), and `buildForkChildEngine`'s read-only base (which then
layers Edit/Write). The team-member catalog is DELIBERATELY NOT built from it (its Bash gating
differs: `spec.Mutating || roIsolationAvailable` + the `isolateReadOnly` side-effect). Guard:
`app.TestExplorerPromptInstructsReferences`.

**Subagent typed result taxonomy + agentId trailer (`renderSubagentResult`/`renderSubagentTrailer`).** The Subagent
RESULT is now LABELLED by terminal stop reason: `StopError` → tool error; `StopStructuredOutput` →
tool error carrying the last validation failure; `StopMaxTurns`/`StopMaxToolCalls`/`StopBudget` →
success-with-note (`[subagent stopped: …]` prefix); everything else (`StopEndTurn`/`StopNoProgress`/
…) → success. On EVERY terminal — including the error/timeout terminals (`StopError`,
`StopStructuredOutput`, the `timeout_ms` deadline) — the result text carries an `agentId: <childID>`
trailer (mirroring `renderTeamResult`'s Team-id line; TRAILING on errors so the error headline stays
first) so the parent MODEL can DISCOVER the deterministic child id (`subagent-<callID>`) — the
runtime-discoverability axis: the id must be where the model reads it, not only on the client-only
`subagent.*` events. The trailer is now the model's REAL handle: each child session is best-effort
persisted via `WithSubagentStore` (the shared session store, `persistMember` discipline — nil
disables, failures swallowed; persisted on ALL terminals after any structured-output re-drives), and
the read-only `InspectSubagent` PULL tool (`subagentinspect.go`, sibling of `InspectMember`) loads
that transcript by the id VERBATIM (the `agent_id` IS the session id — no derivation), rendering it
through the SHARED `renderInspectTranscript(header, sess)` (extracted from `renderMemberTranscript`,
byte-identical bounds 40/1000/8000). A PREFIX GATE rejects any `agent_id` not starting with the
subagent child prefix (`requiredPrefix`, default `"subagent-"`) BEFORE the store is touched, so the
model cannot read team-member (`team-<teamID>-<member>`) or service-session transcripts through this
tool, bypassing `InspectMember`'s team_id+member framing — the same gate the planned `resume` path
(B2.1) specifies. `InspectSubagent` is registered UNCONDITIONALLY wherever the
Subagent tool is (NOT gated on `EnableTeams`, unlike `InspectMember`); both inspect tools share the
one store, ids kept disjoint by prefix convention (`subagent-…` / `team-…`), the gate enforcing the
subagent side. Floor-scoped (`ScopeBuiltinDefault`) ALLOW in `defaultRules()` (issue #37, decided for
`InspectSubagent` + `InspectMember` + `SubagentStatus` together): all three are read-only pulls of
harness-owned data (persisted child/member transcripts; the run-local child registry), bounded-rendered,
prefix-gated — and the children were already permission-gated when they ran. Overridable to ask/deny by
any config scope, loosening no other tool's Ask (the memory-tools pattern; guarded by
`internal/app/inspect_perm_test.go`). Guards:
`agent.TestParentDiscoversAgentIDFromResultAndInspects` (model-facing e2e), `TestSubagentPersistsChildAfterRun`,
`TestSubagentPersistsFinalStateAfterStructuredRetry`, `TestSubagentPersistsAfterErrorTerminal`,
`TestSubagentPersistsAfterTimeoutTerminal`, `TestSubagentNilStoreSkipsPersist`, `TestInspectSubagentHappyPath`,
`TestInspectSubagentUnknownIDErrors`, `TestInspectSubagentStoreFailureDistinct`,
`TestInspectSubagentForgedIDCleanError` (+ the existing subagent tests' substring assertions for the trailer).

**Subagent resume (`subagentArgs.Resume`).** A Subagent call carrying `resume: <agentId>` CONTINUES a
previously-run child by its persisted session id (the `agentId:` trailer value, verbatim) instead of
starting fresh. v1 scope: it runs on the DEFAULT explorer engine only — `resume` is REJECTED together
with `agent` or `model` (those pin their own engine; a resumed child cannot also be re-routed), with a
clear model-visible error. A no-store deployment rejects `resume` (`no session store wired`). A PREFIX
GATE (`!strings.HasPrefix(args.Resume, t.idPrefix+"-")`, NOT a literal) rejects non-subagent ids so a
team-member transcript (`team-<teamID>-<member>`) cannot be resumed through Subagent (it is read-only
via `InspectMember`) — the same gate `InspectSubagent` uses. The child is reloaded and its terminal
state recovered at the AGENT layer (the `loadAndReopen` discipline): `StateCompleted` → `Reopen()`,
`StateCancelled` → `Interrupt()` (history-repair, no dangling tool_use), `StateIdle` → run as-is,
`StateFailed` → NOT resumable (start fresh), any other state → not-in-a-resumable-state. The load +
recovery + limits-tighten run BEFORE the workspace fork, so the common error cases (unknown id, failed
state, broken store) FAIL FAST without paying a fork/unfork round-trip; AFTER the fork the recovered
session is re-homed onto the fresh root via the new domain method `session.Session.Rehome` (legal only
from `StateIdle`) — a FIELD-CONSISTENCY repair: it keeps the persisted session's recorded workspace
consistent with where the resumed run actually executes (the original worktree is torn down; without
it the re-persisted snapshot would record a dead path). NOTE: the child's prompt cwd is independently
sourced from the engine's `PromptConfig` and is NOT affected by this field (the loop's
`sess.Workspace` fallback only fires when the configured prompt `Env.Cwd` is empty, and composition
pre-populates it). The effective prompt is prefixed
with the verbatim `resumeStalenessNote` (the conversation survives but file changes/build state/running
processes do NOT — re-run/re-read before trusting earlier observations), computed BEFORE the
structured-output wrap so a resumed structured-output child sees the note inside the wrap. The LOADED
session keeps its STORED Limits; the per-call `max_turns`/`max_tool_calls` only TIGHTEN them (Reopen/
Interrupt already reset Counters, so each bound applies afresh); `max_tokens` rides the same
`RunOptions.MaxRunTokensOverride`. An IN-FLIGHT GUARD (`tryAcquireChildID`/`releaseChildID` over a
mutex-guarded `inFlight` set) registers EVERY child id (fresh AND resume) BEFORE acquiring the
concurrency slot and rejects a SECOND concurrent run on the SAME id with a model-visible "already
running" error — NOT a wait: two runs over one unlocked `Session` aggregate is a data race (correctness),
and waiting would park a dispatcher goroutine + a gate slot (liveness). Everything downstream is
unchanged: the persist re-saves the SAME id (the grown conversation), the trailer carries the SAME id,
the structured-output retry and `driveChild` work identically, and no-nesting holds by construction.
Guards: `agent.TestParentResumesSubagentByTrailerID` (model-facing e2e), `TestSubagentResumeContinuesPriorConversation`,
`TestSubagentResumeAfterMaxTurns`, `TestSubagentResumeCancelledInterrupts`, `TestSubagentResumeFailedRejected`,
`TestSubagentResumeWithAgentRejected`/`TestSubagentResumeWithModelRejected`, `TestSubagentResumeUnknownIDErrors`,
`TestSubagentResumeNoStoreRejected`, `TestSubagentConcurrentResumeGuard`, `TestSubagentResumeBudgetTightenOnly`,
`TestSubagentResumeStructuredOutput`, `TestSubagentResumeTeamMemberIDRejected` (adversarial),
`TestSubagentResumePreservesStoredLimits`, and `session.TestRehomeFromIdleRepointsWorkspace`/
`TestRehomeIllegalFromNonIdleStates`.

**Per-child cancel — the child-run registry + `CancelChild` (BACKGROUND-SUBAGENTS I1).** The parent
`agent.Run` now owns a `childRunRegistry` (`childregistry.go`) alongside `childAsks`, created
UNCONDITIONALLY in `RunContentWith` (cancel arrives only on interactive surfaces, but the registry's
bookkeeping must work headless too) — one flat map keyed by the child SESSION id (the `agentId:`
trailer / overlay ChildID / store key: the single handle convention; family prefixes disjoint by the
existing convention). The registry is handed to spawning tools DIRECTLY as `parentCaps.children`
(an agent-package handle — zero layering cost; the panel replaced the original three pass-through
closures; `surfaceAsk` stays a closure because it genuinely composes router registration + redaction
+ the parent emit) with nil-safe wrappers `registerChildRun`/`finishChildRun`/
`childWasClientCancelled`: a per-CALL `context.WithCancel` is minted after the in-flight guard and
registered BEFORE `acquireChildSlot`, so a child QUEUED on the concurrency gate is already
cancellable (the gate's ctx select unblocks; the error then names the CLIENT cancellation, not a
generic cancel). Re-registration of a resumed id within one run OVERWRITES the done entry with a
fresh `doneCh` (never double-closed — A5); `markDone` is idempotent. The
`sealed`/`background`/`result`/`delivered`/`doneCh` fields are present per the registry design but
only the cancel-relevant paths are exercised. LOCKING is split per concern: `mu` guards the entries
map (short sections, never across a send) and a separate `emitMu` guards `sealed` + every guarded
send as ONE locked section (A4a holds; a send waiting on the events channel can never wedge markDone /
sibling registration behind it). (I3a generalised the emit semantics: the bound send is now
`Run.emitOrAbort` — blocking until delivered, giving up when the registry's seal-abort channel
`emitAbort` closes at seal-intent OR when the run's `hardAbort` fires (`Run.Cancel`'s explicit
unwedge — a `hardAbortGrace`=1s timer armed BEFORE the ctx cancel; BACKGROUND-SUBAGENTS.md §2.3
step 4) — so a cancelled run's in-flight child events still reach the draining consumer (the
try-send-first shape delivers deterministically while the buffer has room, and the grace lets a
backlogged-but-draining consumer absorb the tail; only a send still parked past the grace gives
up) and seal can never deadlock behind a blocked send; the original ctx-select `tryEmit` is gone.) `Run.CancelChild(childID) bool` is the Approve
mirror: idempotent, unknown/done → false; on a live child it sets `clientCancelled`, snapshots+clears
the child's surfaced askIDs, invokes `cancel()` OUTSIDE the registry lock, then per askID
`childAskRouter.unregister` (a locked delete) BEFORE emitting the new `permission.retract` event
(string-passthrough EventType; payload rides the existing `Event.Ask` carrying the AskID ONLY) — a
racing late approval falls through to the parent's own registry and dies as an unknown-ask no-op.
askIDs additionally carry a per-RUN ":r<runSerial>" SUFFIX (`newAskID`; the leading "<sessionID>:"
prefix isChildAsk consumes is untouched): without it, cancel-a-parked-ask → `resume` the same child
id in the same run (Counters reset) → the provider re-mints the same call id → the new ask would
COLLIDE with the retracted one and a stale queued ResumeApproval could resolve it (CWE-863).
Ask OWNERSHIP is recorded at the single surfacing seam: `childPosture` gains an explicit `childID`
field (set at ALL THREE construction sites — subagent: childID; team: `m.sess.ID`; parallel:
`childSess.ID` — because `role` does NOT universally carry the session id), passed through
`surfaceAsk` to `recordAsk` (team/parallel cancel itself is the next iteration; the seam is uniform
now). Terminal rendering: `renderSubagentResult` gains the `StopCancelled + clientCancelled` arm —
success-with-note `[subagent cancelled by user]` + partial text + the resumable trailer (an error
would teach the model the delegation mechanism failed); the `timeoutCtx` deadline check stays first
and a PARENT-run cancel keeps the legacy un-noted rendering. A client-cancelled child persists
(state cancelled) and resumes via the existing cancelled→Interrupt recovery. Wire: proto
`ConverseRequest` oneof `CancelChild cancel_child = 12` (`{string child_id = 1}`);
`readControl` dispatches it (false ignored by design on the stream — the finished-as-you-pressed
race is benign); `Service.CancelChild` (Approve-mirror: LookupRun → `ErrChildNotFound` on false —
worded FAMILY-NEUTRALLY ("child agent …") so it stays truthful when team/parallel cancel lands;
store fallback ErrNotFound/ErrNoActiveRun) backs HTTP `POST /v1/sessions/{id}/cancel-child`. The
SAME regen landed the three DORMANT fields for the next iterations (A10): `Subagent.background=10`
(now written by the mapper since I3a), `Parallel.child_id=18`, `Team.member_session_id=18` (17 is
taken by dispositions — A1; both written since I2). mecatui: the ctrl+a Subagents tab gains an `x` cancel key (roster + focus pane,
non-terminal lanes only, confirm-less — recoverable) sending `client.Stream.SendCancelChild`;
`permission.retract` maps to `PermissionRetractMsg`; mecatui keeps a FIFO ask queue behind the
visible modal (`m.ask` is always the head — concurrent subagents surface asks concurrently), so a
retract matching the VISIBLE ask dismisses the modal and advances the queue, a retract matching a
QUEUED ask removes it in place (with a notice), and an unknown/stale id is idempotently dropped. The Subagents-tab roster hint is deliberately SHORTER than the team/parallel ones (the
"x cancel" segment would otherwise push the centred card past a 100-col terminal — the hint is the
card's widest line and centerCard does not wrap), and the two subagent golden tests carry an
`assertFitsViewport` width guard so a future overflow cannot be silently absorbed by a golden
refresh. Guards: `agent.TestCancelChildMidDrive`/`TestCancelChildWhileParkedOnAsk` (full unwind
e2e: retract + late-approval-no-op + command-never-ran)/`TestCancelChildAfterDoneAndUnknownNoOp`/
`TestCancelChildPersistResumeRoundTrip`/`TestCancelChildNaturalCompletionRace` (legal-renderings +
trailer, half the iterations synced on EvSubagentStart)/`TestCancelChildMidGateWait`/
`TestParentRunCancelNoClientNoteOnStream` + the internal `TestParentRunCancelKeepsUnNotedRendering`
(the D6 negatives — a hardcoded clientCancelled mutation fails both)/
`TestResumeWithinRunReRegistersAndIsCancellable` (the reachable-path A5 e2e)/
`TestStaleVerdictAfterCancelResumeDoesNotResolveNewAsk` + `TestNewAskIDRunSerialDisjoint` (the
CWE-863 regression pair), the `TestChildRegistry*` unit+race suite incl.
`TestChildRegistrySealVsEmitRace` (adversarial seal-vs-emit, real channel close) and
`TestCancelChildUnregistersBeforeRetract` (deterministic ordering pin — the flipped
unregister/retract order fails it), `server.TestGRPCConverseCancelChild` (real-stream wire e2e)
/`TestServiceCancelChildFallbacks`/`TestHTTPCancelChild`, `client.TestSendCancelChildFrame` + the
`permission.retract` EventToMsg case, and `ui.TestSubagent*CancelKey*`/`TestPermissionRetract*`.
*Post-arc fix (ask retraction at the child terminal):* retraction no longer lives ONLY in
`Run.CancelChild` — `markDoneResult` is now the CHOKEPOINT: every child that ran lands exactly one
registry terminal there (deferred by all spawning tools), and by then an unanswered surfaced ask is
dead by definition, so the terminal takes (snapshot+clears, `takeAsks`/`takeAsksLocked` — the locked
core `requestCancel` now shares) the child's still-pending askIDs and retracts each via the shared
`retractAsks`, strictly BEFORE the done-transition closes `doneCh` (⇒ before the drain's join ⇒
before seal ⇒ before the terminal `EvResult`). Exactly-once is two atomic gates: the locked
snapshot+clear, and `childAskRouter.unregister` now returning a BOOL (locked check-and-delete — the
answered-vs-pending gate: route() already removed an answered ask's entry, so false means
do-not-retract; a stale already-answered id emits nothing). The gate is bound onto the registry by
`RunContentWith` (`unregisterAsk` = `Run.unregisterChildAsk`; nil on unbound unit-test registries ⇒
retracts skipped, never a panic; a headless/child run's router is nil ⇒ gate false ⇒ no retract —
nothing was ever surfaced). `Run.CancelChild` shares the loop via `retractAsksVia` (explicit gate, so
the pinned unregister-BEFORE-emit ordering holds on manually-built Runs too), and a just-answered ask
no longer draws a spurious retract. This covers every ctx-driven unwind (run-end drain, per-call
`timeout_ms`, parallel join=first losers, whole-run cancel, team teardown) AND fixes the router-entry
leak (a never-answered ask's entry lingered until Run GC). Guards:
`TestRunEndDrainRetractsParkedAsk` (clean-end drain: one retract, before EvResult, late approval
no-op, persisted+resumable), `TestChildTimeoutRetractsSurfacedAskMidRun` (the mid-run ghost-ask pin:
retract precedes the Subagent ToolResult), `TestVerdictRacesTerminalRetract` (-race: exactly one of
{verdict routed, retract emitted}), `TestRouterUnregisterAfterRouteNoRetract` /
`TestRouterRouteAfterUnregisterFalse` (the deterministic direction pins),
`TestRouterEntryClearedOnChildExit` (the leak fix), and
`TestDrainAbandonedChildAskRetractedPreSeal` (the drain's abandoned-only sweep).

**Per-child cancel for parallel branches + team members (BACKGROUND-SUBAGENTS I2).** The registry now
covers ALL THREE delegation families. **Parallel** (`parallel.go`): the shared per-branch goroutine
body is `launchBranch` — it mints each branch's OWN `context.WithCancel` (ALL join modes; previously
only join=first had a shared cancelable ctx) and registers it (`childFamilyParallelBranch`, the
branch label as the display goal, key = the deterministic `childSessionID` "parallel-<callID>-<i>")
BEFORE the worker-semaphore wait, so a QUEUED branch is already cancellable (the slot select waits on
the branch ctx; a client-cancelled queued branch reads "cancelled by user before start").
`runBranch` now also returns the terminal stop, which `launchBranch` lands via `finishChildRun`. A
client-cancelled mid-drive branch keeps the EXISTING `StopCancelled` arm but flips its failReason
"cancelled" → "cancelled by user" (`childWasClientCancelled` — A8). Join-mode semantics fall out
of failed=true with ZERO new join-path code: `all` → branch `[FAILED]`; `first` → a cancelled branch
can never win (the winner test is `!failed`); `judge` → excluded from candidates, and cancelling the
only success degrades to the all-failed report (judge never called). The JUDGE's own run stays
UNREGISTERED (short, tool-less; whole-run cancel covers it) — documented v1 limitation. Bracketing
`branch_start`/`branch_end` still fire for cancelled branches (incl. cancelled-before-start).
**Team** (`teamsupervisor.go`): `AddMember` mints a DETACHED per-member `context.WithCancel`
(`context.Background()`, NOT the enrolment ctx — on the gRPC path AddMember runs under the
CreateTeam REQUEST ctx, which dies before RunTeam; deriving from it would insta-cancel every member)
and registers it under `MemberSessionID` via `s.caps` (nil-safe: the RunTeam path registers nothing
— D4's whole-stream cancel covers it). `driveOneTurn` MERGES the member ctx into each drive's ctx
via `context.AfterFunc` (neither parent subsumes the other), so a mid-drive cancel rides the
EXISTING `StopCancelled` classification in `runTurn` — which stays ordered BEFORE the reopenErr
fold (don't disturb `warnUnexpectedReopen`'s suppression) — de-scheduling the member + `ReleaseTasks`.
Idle-between-rounds: `planRound` gains an up-front `m.ctx.Err()` check → stopped +
`StopReasonCancelled` + `SetMemberState(MemberStopped)` + `ReleaseTasks` + registry markDone BEFORE
planning; the session stays resumable (D5 de-schedule, deliberate). `Supervisor.CancelMember(name)
bool` is the shared seam (the registry-registered cancel IS the member cancel; a future RunTeam-path
`CancelTeammate` unary — deferred, D4 — would call it directly). A supervisor-stopped member's
registry entry is marked done WITHOUT cancelling its ctx (a budget-stopped lead must stay drivable
for the one synthesis turn); `cleanupAll` cancels every member ctx + markDones the rest at team end.
**Wire (D16, fields landed dormant in I1):** the mapper now sets `Parallel.child_id` (= 18; on
branch_start/branch_end, from the new `session.ParallelPayload.ChildID` — added to the
gauntlet-#7 structural allow-list as a harness-derived addressing handle, never branch content) and
`Team.member_session_id` (= 18; on team.member events, threaded `driveOneTurn` →
`TeamEvent.MemberSessionID` → `projectTeamEvent` → `session.TeamPayload.MemberSessionID`).
**mecatui:** the `x` cancel key now covers parallel-branch lanes (the focused group gains a branch
SELECTION cursor — `parallelState.branchCursor`, `↑/↓`) and team-member lanes (roster + focus pane;
gated on a live team + a learned `teamLane.sessionID`); the shared sender is `cancelChildByID`
(empty handle from an older server → no-op). The Teams-tab roster hint was SHORTENED (the same
width-clamp discipline as the Subagents tab — the old 121-col hint actually overflowed the 100-col
viewport) and the changed-hint goldens now carry `assertFitsViewport` guards. Guards:
`agent.TestCancelParallelBranchJoinAll`/`...JoinFirstWinnerNeverCancelled`/`...JudgeExcluded`/
`...JudgeOnlySuccessCancelled`/`...WhileQueued` (all driven via `Run.CancelChild` with ids read off
the D16 events), `TestCancelMemberMidDrive` (disposition + task-release + the synthesis prompt's
"Team status: worker (cancelled)" digest)/`TestCancelMemberIdleBetweenRounds` (provider never
driven)/`TestCancelMemberUnknownFalse`/`TestCancelChildReachesTeamMember` (registry route, in-process),
`server.TestGRPCConverseCancelTeamMember` (real-stream wire e2e: id learned from
`team.member.member_session_id`, member disposed stopped/cancelled, team still delivers) + the
mapper child_id/member_session_id cases, `client` EventToMsg cases, and
`ui.TestParallelBranchCancel*`/`TestTeam*Cancel*` (send + done-lane/handle-less no-ops).

**Background subagents — mechanics + SubagentStatus + seal/drain + gate fail-fast
(BACKGROUND-SUBAGENTS I3a; I3b — the notice injection + background-pending nudge — is the
following entry).**
`subagentArgs.Background` detaches a Subagent child, RUN-scoped (D8): after validation/in-flight
guard/registration, `startBackground` does a FAIL-FAST gate acquisition (`tryAcquireChildSlot`,
D12 — a background child holds its slot ACROSS turns, so blocking could deadlock the model against
itself; the error lists the live background ids, ids ONLY per A9, + the recoverable action — with
abort() ORDERED BEFORE the ids read, so the failing call's own pre-gate registration is never
listed as "currently running"), a
fail-fast SYNCHRONOUS resume load (an unknown/non-resumable id errors inline, never as a collectible
surprise), emits `EvSubagentStart{Background:true}` SYNCHRONOUSLY before the spawn (A5 deterministic
start-before-started-result; proto `Subagent.background=10` now mapped), then spawns
`driveBackground` — the goroutine owning fork → drive → persist → `markDoneResult(rendered result +
stop)` plus the transferred lifecycle handles (per-call cancel, timeout cancel, gate release,
in-flight id). The immediate started-result rides `renderSubagentTrailer` (agentId FIRST line, the
existing convention) with the amended D7 body ("collect with SubagentStatus … wait_ms … cancelled if
still running when this run ends"). A POST-spawn failure (fork/session-build) is COLLECTIBLE
(done+StopError, the error text as the stored body, a closing subagent.end) — the model already holds
"started". Background composes with resume/output_schema (retry loop runs inside the goroutine)/
tighten-only limits/timeout_ms/agent/model; the in-flight guard still rejects resuming a
still-running background id. **`SubagentStatus`** (`subagentstatus.go`, read-only, registered at both
build.go sites alongside InspectSubagent — never in child catalogs) is the SOLE body channel (A2):
no args → an id-sorted roster (id/family/background/state/stop — ids + enum labels only, A9, no goal);
`agent_id` → state, or the stored rendered body for a done background child (collect marks
`delivered`; a second collect reports "already delivered" — exactly-once, never re-bloats context;
a done FOREGROUND child reports result-was-inline; error bodies re-key to the status call preserving
IsError); `wait_ms` (capped `maxSubagentStatusWaitMs` 120000, documented dispatch-slot note — A3)
parks ctx-aware on the target's `doneCh`, or — for any-child — on the registry's terminal-GENERATION
channel taken via `liveGeneration()`, a SINGLE locked snapshot of (liveness, generation): separate
anyLive/generationCh reads had a TOCTOU window where a terminal landing between them parked the
waiter on the fresh generation for its full capped wait. State/collection wording is FAMILY-AWARE
(`childKindLabel`): a done team-member/parallel-branch id points at the Team report (InspectMember) /
the Parallel result, never at "its own Subagent call". **Run-end drain + seal (A4):**
`drainChildren` sits at the TOP of BOTH `terminate` and `terminateComplete` — cancel every live
background entry (`cancelLiveBackground`, cancels invoked outside the lock), then a TWO-PHASE join:
phase 1 joins each `doneCh` under `childDrainCap` (10s; a var only as a test seam, operationally a
constant — ctx-cancel kills stream+shell promptly and osfs `cmd.WaitDelay` bounds the
grandchild-pipe residual per A7); on expiry, the cap may have been burned by the run's OWN emit
backpressure (a child parked in its end-send on a full events channel cannot reach markDone until
the send aborts), so phase 2 calls `registry.abortEmits()` (the same sealOnce'd emitAbort close seal
performs) and re-joins under `childDrainGrace` (1s); only children STILL unjoined after both phases
are ABANDONED with ONE operator WARN (ids only — the consciously-amended THIRD loop diagnostics
line; CLAUDE.md + DIAGNOSTICS.md updated — never misattributing consumer backpressure as a wedged
child), then `seal()` — so on the healthy path a drained child's subagent.end PRECEDES the terminal
EvResult (A4b). ALL child-originated emits (subagent.start/tool/end, the surfaced-ask
EvPermissionAsk in `parentCaps`, permission.retract) route through `registry.safeEmit` (A4c): a
locked sealed-check+send (A4a), bound to `Run.emitOrAbort` — BLOCKING (a cancelled run's in-flight
child events still reach the draining consumer; the bracketing parallel.branch events of a
cancelled-before-start branch must be represented) and aborted by `emitAbort`, which `seal`
closes BEFORE taking `emitMu` so seal can never deadlock behind a blocked send (the single
seal-side deadlock-prevention mechanic, pinned by `TestSealUnblocksEmitParkedSend`), OR by the
run's `hardAbort` (a `hardAbortGrace` timer armed by `Run.Cancel` BEFORE the ctx cancel — the
explicit unwedge for a consumer that stopped draining mid-run, when the terminate paths that seal
are themselves blocked, while the grace still lets a backlogged-but-draining consumer collect the
post-cancel tail; threaded to the team supervisor's member forward as `parentCaps.hardAbort`; both
relays additionally drain-to-discard `run.Events()` after their first Send/Write error — pinned by
`TestCancelUnwedgesStalledTeamRun`, `TestCancelAbortNoChildLeakAfterSeal`,
`TestEmitDeliversWithBufferRoomAfterHardAbort` (the try-send-first delivery shape),
`TestCancelMemberUnparksEvChSend` (the member-forward's driveCtx arm),
`TestRelaySendErrorDrainsBusyRun`, `TestSSEWriteErrorDrainsBusyRun`). **A5 state
vocabulary** (documented in childregistry.go): `childQueued → childRunning → childDone`
(`markRunning` at slot acquisition / branch start / member enrolment); a PRE-START failure whose
error returned inline (fork/resume-load/session-build, the background gate-full path) REMOVES the
entry (`remove` — closes doneCh so waiters wake, no done+StopNone phantom); a cancel-while-queued
keeps a MEANINGFUL done+StopCancelled entry; a never-driven, never-cancelled team member is removed
at `cleanupAll` (`memberRT.ran`, set in `driveOneTurn`) instead of fabricated done+StopEndTurn;
`remove` is a no-op for done entries (never erase a real terminal). Register-over-done (a `resume`
of an already-run id) STASHES the displaced terminal entry (`childEntry.displaced`); a pre-start
abort of the resume attempt REINSTATES it — a failed resume can never erase the prior child's
undelivered background result — and `markRunning`/`markDone` drop the stash (the overwrite is then
permanent). **osfs (A7):** `cmd.WaitDelay`
(default 5s, `WithCommandWaitDelay` test seam) bounds the post-exit/post-cancel pipe wait a
grandchild's inherited fds caused; `exec.ErrWaitDelay` on a zero-exit shell is treated as success
with the captured output. Guards: `agent.TestBackgroundSubagentHappyPath` (REAL-loop e2e: immediate
started-result + same-turn second tool + wait_ms collection),
`TestBackgroundSubagentAlreadyDelivered`, `TestSubagentStatusPollBeforeDone`,
`TestBackgroundChildCancelledAtRunEnd` (drain + end-before-EvResult ordering + persisted-cancelled +
resume in a NEW run), `TestBackgroundChildSurfacedAskAnsweredMidRun` (D11/A8),
`TestBackgroundChildCancelledWhileParkedOnAsk` (retract + collectible cancel note),
`TestBackgroundGateFullFailFast` (ids listed, self-id ABSENT, phantom removed) +
`TestBackgroundGateFullAbortsPhantomAndListsIDs`, `TestBackgroundStructuredOutput`,
`TestCompactionDuringLiveBackgroundChild`, `TestSubagentStatusWaitRespectsRunCancel`,
`TestBackgroundComposesWithResume`, `TestSubagentBackgroundWithoutRegistryErrors`,
`TestEffectiveStatusWaitClamp`, `TestChildRegistryCollectOutcomes`/`TestChildRegistryRemoveSemantics`,
`TestSubagentForegroundForkFailureAbortsEntry`/`TestBackgroundForkFailureIsCollectible`,
`TestChildRegistrySafeEmitSealedFullSurface` (+ the I1 seal-vs-emit race retuned to the
emitOrAbort binding), the updated `TestCancelChildMidGateWait` (StopCancelled pinned) and
`TestCleanupAllAttributesIdleClientCancel` (never-ran member removed), and
`osfs.TestCommandRunnerWaitDelayUnblocksGrandchildPipeWait`. The post-panel hardening pass added:
`TestDrainTwoPhaseJoinsEmitParkedChild` (consumer backpressure joins, no WARN) /
`TestDrainAbandonsGenuinelyWedgedChildWithOneWarn` (exactly one ids-only WARN + post-seal no-op) /
`TestDrainCleanNoWarn`, `TestSealUnblocksEmitParkedSend` (the abort-before-emitMu deadlock pin),
`TestChildRegistryRemoveReinstatesDisplacedEntry` + `TestFailedResumeAttemptPreservesUndeliveredResult`
(failed-resume erase), `TestLiveGenerationSnapshotConsistent` +
`TestWaitForChildAnyReturnsPromptlyOnConcurrentTerminal` (the any-wait TOCTOU), and the A5
sync-start ordering pin inside the happy path. The goleak gate (leakmain) covers the
drain: a leaked background goroutine fails the whole agent package. *Post-arc fix:* the drain now
participates in ask RETRACTION (the child-terminal chokepoint — see the per-child-cancel entry): a
JOINED child retracts its own still-pending surfaced asks at `markDoneResult` (pre-doneCh-close, so
pre-seal and pre-EvResult); only an ABANDONED child (still unjoined after both phases) gets a
`drainChildren`-side sweep — `retractAsks(takeAsks(id))` per abandoned join, AFTER the abandon WARN
and BEFORE `seal()` — so no client holds a stale modal for a child the run will never answer for.
No diagnostics change (the retract is an EVENT; the abandon WARN stays the only line); the abandoned
child's late `markDoneResult` then takes an empty ask set against a sealed registry and emits
nothing (`TestDrainAbandonedChildAskRetractedPreSeal`).

**Background subagents — completion NOTICE injection + background-pending nudge
(BACKGROUND-SUBAGENTS I3b; A2/A9/D10-as-amended).** Two loop-side additions complete the
delivery story. **(1) Turn-boundary notice (Step 2a of `Engine.drive`, BEFORE `preTurnTerminal`
— the design's injection seam, provider-legal because history at that boundary always ends on
the user prompt / tool results / a nudge message):** `injectBackgroundNotice` asks the registry
for newly-finished background children (`noticeFinishedBackground` — done ∧ background ∧
¬noticed, candidates flipped to `noticed` under the lock, id-sorted) and records ONE
harness-framed user message (`backgroundNoticeText`): ids + `session.StopReason` labels ONLY,
nothing child-authored (A2 — no goal labels, no result text; the body's SOLE channel stays
`SubagentStatus`). `noticed` is a flag SEPARATE from `delivered`: a noticed result is never
re-noticed but remains collectible exactly once. A done child whose result was ALREADY
collected (a same-turn `wait_ms` collection) is marked noticed SILENTLY and not listed —
announcing "collect it" for a body the model holds would only provoke an "already delivered"
round-trip (a deliberate I3b decision, pinned by `TestBackgroundNoticeSkipsDeliveredResult`).
`e.save` runs immediately after the record, so the notice is durable in the replayed history
independent of how the run later ends (the placement is witnessed by a RUNNING-state-save spy
in `TestBackgroundNoticeDurableAcrossSave`). The injection emits NO event (not an
`EvNoProgress` — it is ordinary history), consumes no no-progress nudge, and does not itself
consume a turn (the following `BeginTurn` does, so `Limits.MaxTurns` semantics are unchanged);
sitting before the terminal checks means a child finishing right at a terminal boundary may be
noticed on a non-clean terminal too — durable for the resumed run. A child landing its terminal
between the scan and the turn is simply noticed at the NEXT boundary. **(2) Background-pending
nudge (`finishTurnNoTools`, the real-clean-end branch):** when the model produces a meaningful-
text turn on a benign stop (the would-be `StopEndTurn` terminal) while background children are
still LIVE, the loop — ONCE per run (`bgPendingNudged`, mirroring the `noProgressNudges`
accounting) — records `backgroundPendingNudgeText` (ids ONLY — A9) and re-drives one more turn
instead of terminating. It CANNOT live in `terminate`/`terminateComplete`: `drainChildren` at
their top has already cancelled the children and sealed the registry (the I3a placement note on
`drainChildren`). Pinned semantics: it fires ONLY on that branch — the no-progress machinery
owns the empty turn first and even the `StopNoProgress` give-up is not background-nudged
(`TestNoProgressPrecedesBackgroundPendingNudge` / `TestNoProgressGiveUpDoesNotBackgroundNudge`);
every non-clean terminal (error/cancel/limits/budget) skips it and the eventual stop reason is
never relabelled; it is EVENT-silent (no new event type, no `EvNoProgress` — that taxonomy
means "the model stalled"); on the SECOND clean end the normal terminate path runs and the
drain cancels + persists what is still live; the nudged continuation re-enters Step 2, so
`MaxTurns` still bounds it. A finished-but-never-collected child at run end is the accepted
disposition: the drain has nothing to cancel, the uncollected result dies with the run's
registry, the persisted child stays inspectable/resumable. **mecademo** gained a third offline
act (`RunBackgroundScenario`): background start → started-result → `wait_ms` roster wait → the
injected notice (printed from recorded HISTORY — notices are not events) → `SubagentStatus`
collection → clean end. Guards: `agent.TestBackgroundNoticeInjectedAtNextBoundary` (exact
notice text + notice-precedes-collection + replay-valid pairing),
`TestBackgroundNoticeBatchesTwoFinishedChildren` (ONE message for two children, id-sorted,
internal — joins both registry doneChs as the deterministic anchor),
`TestNoticeFinishedBackgroundSemantics` (registry unit: candidates/sorting/silent-delivered/
never-renotice/still-collectible), `TestBackgroundNoticeSkipsDeliveredResult`,
`TestBackgroundNoticeDurableAcrossSave`, `TestBackgroundPendingNudgeOneMoreTurn` (exact nudge
text, event-silent, model collects on the granted turn),
`TestBackgroundPendingNudgeIgnoredThenCancelledAtRunEnd` (adversarial: nudge once → second
clean end → drain-cancel + persisted-resumable), `TestBackgroundPendingNudgeAbsentWithoutLiveChildren`
(byte-identical clean end), `TestBackgroundPendingNudgeSkippedOnBudget`/`...SkippedOnCancel`,
`TestBackgroundNudgeRespectsMaxTurns`, and `mecademo.TestRunBackgroundScenarioOffline`. The
pre-existing I3a tests `TestBackgroundChildCancelledAtRunEnd` and `TestBackgroundGateFullFailFast`
now script a second clean end (their first one legitimately draws the nudge). No diagnostics
change: the loop still emits exactly THREE operator lines.

**TUI queue budget stop.** The type-while-running queue treats `StopBudget` like the other healthy
size-bound stops (`max_turns` / `max_tool_calls`): the current run produced a usable partial and a
queued follow-up should reopen the session with a fresh budget instead of pausing like error/cancel.
`shouldDrain` therefore includes `budget`, and the footer renders it as `stopped · token budget`.
Pinned by `ui.TestDrainOnSizeLimit` and `ui.TestStopReasonLabel`.

**Background subagents — TUI surfaces + final description pass (BACKGROUND-SUBAGENTS I4, the
arc's final iteration).** The client now decodes proto `Subagent.background` (field 10, mapped
by the server since I3a) onto `client.SubagentMsg.Background` — set on subagent.start only, an
older server yields false. The fleet lane (`subagentLane.background`, recorded by `fleetStart`)
drives three surfaces: (1) a **`⇢ bg` marker** (`subagentBackgroundMarker`, glyph-plus-text so
it survives ANSI stripping) after the roster row's `#hash` — the focus header reuses the roster
line so it inherits the marker; (2) a **transient footer notice** on a background child's
subagent.end ("background subagent #hash done — result ready for the agent" — the
team-done/no-progress advisory channel, never a durable scrollback block; a FOREGROUND end stays
silent, its result already landed on its own card); (3) an **honest delivery line** on the focus
pane (*running detached* vs *done — result ready for the agent (SubagentStatus)*) that renders
only what the events carry — background + done; the registry's `delivered` state is deliberately
NOT on the wire, so the pane never claims a collected/uncollected state. The footer fleet count
needed NO change (verified + pinned): the fleet is session-scoped and keyed on subagent.end, so a
cross-turn background child keeps counting as ◐ running. The description pass tightened the
Subagent Spec to one coherent id workflow — the trailer line now points at SubagentStatus (live
state / background collection) AND InspectSubagent AND `resume`, the background mention rides the
FINAL-MESSAGE sentence, and the duplicated report-format/fresh-context guidance (already in the
`prompt` arg description) was trimmed so token cost stays ~flat; `backgroundStartedBody` and the
`background` arg description now both name the turn-boundary note ("a note will tell you when it
finishes"). The Spec guard test grew to require SubagentStatus + background. Docs finale: the
design promoted to `docs/design/BACKGROUND-SUBAGENTS.md` (as-built, amendments folded, I1-I4
hashes), architecture §8 gained the background/SubagentStatus/cancel paragraph (+ the stale
forking-only-gate and blanket-auto-deny bullets corrected to the childGate/4-step reality),
docs/tui.md gained the marker/notice/footer-count notes, docs/usage.md's delegation note gained
background + per-child cancel (and the child-concurrency default corrected 10→4). Guards:
`client.TestEventToMsg` (background decode), `ui.TestSubagentRosterLineBackgroundMarker` /
`TestSubagentRosterBackgroundMarkerEndToEnd` / `TestSubagentFocusBackgroundNote` /
`TestBackgroundSubagentEndTransientNotice` / `TestForegroundSubagentEndNoTransientNotice` /
`TestFooterCountsCrossTurnBackgroundChild`, `agent.TestSubagentSpecEnumeratesAgents` (the
widened description guard). Goldens: unchanged (no golden covers a background lane).

**Subagent per-call token ceiling (`max_tokens`, Run-scoped budget override — R4).** `subagentArgs.MaxTokens`
rides the new `RunOptions.MaxRunTokensOverride` carried into `Engine.RunContentWith`, so a per-call
token ceiling bounds the SHARED child engine WITHOUT minting a fresh engine. `effectiveMaxRunTokens`
folds it TIGHTEN-ONLY with `Deps.MaxRunTokens` (the lower non-zero value wins), so a per-call ceiling
can make the child stricter than the operator default, never looser. A budget-stopped child ends
`StopBudget` (clean terminal) → a success-with-note Subagent result, not an error. The `RunOptions`
override is the cleaner of the two R4 options (it generalises and works on the shared engine);
`RunContent`/`Run` delegate to `RunContentWith` with a zero `RunOptions` (legacy run, unchanged).
Guards: `agent.TestSubagentPerCallMaxTokensHitsBudgetTerminal`, `agent.TestSubagentPerCallMaxTokensTightenOnly`.

**Unknown-tool card (dispatch `runOne`).** An unknown/unresolved tool-call name now opens an
`EvToolCall` card BEFORE its `EvToolResult` error (`unknown tool %q`), preserving the
card-before-the-gate ordering so a client (ACP/mecatui) keys the failure to a card it already
opened rather than dropping a result for a `tool_call` it never saw. The read-batch path cannot
carry an unknown tool (the batcher only groups resolved read-only tools), so `runOne` is the
single fix site.

**Reasoning replay is verified, not buggy.** The OpenAI adapter replays the REAL
`encrypted_content` blob (not the human-readable summary) and requests it via
`Include=[reasoning.encrypted_content]` + `Store=false`; Anthropic packs the thinking
*signature* across pack→unpack. Pinning tests (`TestReasoningReplayUsesRealBlobNotSummary`,
`TestReasoningEnvelopeRoundTripsSignature`) tripwire any regression that swaps the blob for the
summary or drops the load-bearing `Include` flag. See `OPENAI-RESPONSES-API.md`.

**Team-member workspace policy is THREE-TIER** (isolation is the security boundary; capability
flows down from the parent, which has Bash): a base-sharing read-only member (no forker wired)
gets NO shell; a read-only member the factory marks `MemberBuild.IsolateReadOnly` runs in a
cheap git **worktree** (shares the base repo's `.git` ⇒ full history) with Read/Grep/Glob +
**Bash** but never Edit/Write — so it can `git log`/`git show`/build/test, confined to a
throwaway checkout; a Mutating member runs in a **force-copy** fork (own `.git`) with
Edit/Write/Bash. The Supervisor holds TWO forkers (`s.forker` force-copy, `s.roForker`
worktree via `WithReadOnlyForker`); `AddMember`'s mutating-tool backstop gates on
**base-sharing** (`!needFork`), so an isolated member's Bash is exempt.

A read-only member's Bash runs through a **sandboxed** command runner
(`buildSandboxedCommandRunner`) that neutralises the **fixed-key** git config-driven
code-execution vectors in the shared `.git` (`core.pager`/`hooksPath`/`fsmonitor`/external
diff + scrubbed `GIT_*`/`PAGER`); it does **NOT** close attacker-named `.gitattributes` driver
configs (`filter.<drv>.smudge` at fork-time checkout, `diff.<drv>.textconv` on `git show`/`log
-p`, `alias.<name>=!sh` if invoked) — reachable only in an UNTRUSTED shared repo, with a
tracked follow-up to gate read-only-member shell on workspace trust.

The single neutralizing env lives in the stdlib-only leaf `internal/adapter/gitenv`
(`Scrub`/`NeutralizingVars`) so two paths share it and can't drift: (1) the **forker's own
git** (`runGit`/`gitRepoRoot`) sets `cmd.Env = gitenv.Scrub(os.Environ())` — critical because
`git worktree add` FIRES the base repo's `post-checkout` hook at FORK time, before any member
runner exists; (2) the member runner gets the COMPLETE scrubbed env (via osfs
`WithCommandEnvList`, which REPLACES `os.Environ()` so inherited danger can be removed, not
merely overridden). `Scrub` DROPS inherited `GIT_*` (so
`GIT_EXTERNAL_DIFF`/`GIT_SSH_COMMAND`/`GIT_ALTERNATE_OBJECT_DIRECTORIES`/`GIT_PROXY_COMMAND`
can't leak — an append can't remove these) + `PAGER`/`LESS`, keeps PATH/HOME, then appends
`GIT_PAGER=cat`, `PAGER=cat`, `GIT_CONFIG_NOSYSTEM`, `GIT_CONFIG_GLOBAL=/dev/null` and
precedence-winning env-injected
`core.hooksPath=/dev/null`+`core.pager=cat`+`core.fsmonitor=false`+empty `diff.external`. osfs
stays git-agnostic (the git knowledge is in `gitenv`/composition). The MAIN session keeps its
unhardened runner; a Mutating force-copy member's own `.git` makes config-hardening moot but it
gets the hardened runner anyway.

**Subagent Bash permission asks resolve in a 4-step model (NOT a blanket auto-deny).** The
old contract auto-DENIED every subagent permission ask, so a team member / Subagent child could
NEVER run a command containing substitution/subshell — the `$(go list ./...)`-per-package
coverage loop hard-failed with a misleading "denied by user". The fix (`handleChildEvent` →
`resolveChildAsk`, threaded by a per-child `childPosture` through `drainChildObserved` /
`drainChild` / `driveOneTurn`):

- **Step 1 — A1 read-only substitution (GLOBAL).** `governance.SubstitutionReadOnly(seg)`
  extracts every recursively-nested inner command (`extractSubstitutions`) and blanks the outer
  (`outerWithSubstitutionsBlanked` → an inert `MECATL_SUBST` placeholder); if every inner is
  `ReadOnlyBash` and the blanked outer is `simpleReadOnly`, `resolveBash` does NOT floor at Ask
  (the ordinary fold stands). It is a SEPARATE classifier — `ReadOnlyBash`/`simpleReadOnly`/
  plan-mode and the `FuzzReadOnlyBash`/`FuzzSplitCommands` Inv-5 are byte-for-byte unchanged. A
  bare `(subshell)` is safe grouping; `$(...)`/backticks in command position are NOT (the output
  is executed), so a lone-placeholder outer is accepted only for `isPureSubshell`. Shell
  control-flow keywords (`for … in …; do …; done`) are stripped (`stripShellKeywords`) so the
  real command is classified.
- **Step 2 — A2 isolation auto-approve.** `governance.IsolationApprovable(cmd)` clears, for an
  ISOLATED subagent only (`childPosture.isolated`: a worktree/force-copy fork), read-only ∪ a
  MINIMAL worktree-safe verb set `{go test,build,vet,list}`, minus worktree-escape verbs
  (`git push/config/remote/fetch/pull/clone/worktree/submodule`). Two hardening points the
  flag-agnostic first cut missed (panel iter-1 S1/S2): the git subcommand is resolved PAST
  leading global flags (`gitSubcommand`) so `git -C /outside push` can't slip the escape check
  by shifting the subcommand right, and a PATH-bearing global flag (`-C`/`--git-dir`/`--work-tree`)
  is itself disqualifying (it points git outside the worktree, even for a read-only subcommand);
  and a worktree-safe `go test`/`go build` is REJECTED if it carries `-exec`/`-toolexec`/`-overlay`
  (`goArgsRunExternalProgram`) — those run an arbitrary external program, which the worktree's
  FILESYSTEM isolation does not contain. Fail-safe false on any ambiguity, any
  unrecognised/destructive command, or any escape verb. `FuzzIsolationApprovable` asserts POSITIVE
  soundness (every cleared segment, outer-blanked + inner, is scaffolding/read-only/worktree-safe-go
  with no `>`/exec flag and no git escape), mirroring `FuzzSubstitutionReadOnly`.
- **Step 3 — surface to human.** An interactive parent run installs `Run.childAsks`
  (`childAskRouter`) when `Deps.Interactive`. The dispatcher passes a `parentCaps`
  (interactivity + a register-then-emit `surfaceAsk`) to a `childCapableTool`
  (Subagent/Team/Parallel's `ExecuteWithParent`). `resolveChildAsk` registers the child Run in the
  parent router and emits a REDACTED parent `EvPermissionAsk` (command `clampPreview`'d, framed
  "subagent requests approval to run Bash: …", raw `Args` dropped — gauntlet #7), then returns
  WITHOUT resolving; the child parks in its own goroutine. The parent's `Run.Approve` routes the
  verdict to the child by the child-namespaced askID (`childAskRouter.route` → `child.Approve`);
  NO proto change (the child session id IS the namespace). A parked team member blocks only its
  own errgroup goroutine; peers keep running.
- **Step 4 — headless auto-deny.** No router → `run.autoDenyChildAsk(askID, childAutoDenyMessage(reason))`,
  which carries the ACCURATE model-facing message ("not permitted in a non-interactive subagent
  shell: …; rephrase to avoid substitution or use an auto-approved tool") via
  `approval.denyReason` — NOT "denied by user". It ALSO emits a correlated operator diagnostic
  (`caps.diag.Log(LevelInfo, …, "agent", role)`), never a parent-stream content event.

**`--yolo` loosens the substitution floor for the MAIN agent.** `Config.AllowAllTools` now also
threads `governance.WithLooseSubstitution(true)` (`mainEvaluatorOptions`) into the main policy's
Evaluator, so a substitution command resolves by the allow-all fold instead of the Ask floor —
consistent with the mutate-ask floor the ScopeCLI allow-all rule already loosens. A configured
Deny/Ask in any scope still wins (deny-dominance unaffected). The default (no `--yolo`) keeps the
floor. `Deps.Interactive` is set on the MAIN engine from `Config.Interactive` (mecated → true, the
bidi/HTTP surfaces have a client; the offline demo → false); child engines force it false
(subagents cannot recurse, so they install no router — their OWN asks resolve via the parent's
caps). The gRPC `RunTeam` direct path leaves `parentCaps` zero (headless auto-deny) — the in-loop
Team tool is the surfacing path.

**Team result aggregation is a LEAD SYNTHESIS, not a `LastText` concatenation.** After the
scheduling loop, `Supervisor.Run` drives ONE final synthesis turn on the lead (`synthesise` →
the shared `driveOneTurn` helper, factored OUT of `runTurn` so the auto-deny / event-forward /
terminal-text-capture logic lives in one place). Its output is `TeamOutcome.Report`, which the
Team tool resolves through the **three-tier `deliverable()` chain** (`teamtool.go`) before
returning it as the `ToolResult`; the gRPC `RunTeam` rides the report back on
the outcome. Synthesis lives inside `Run`, so BOTH entry points share it. **The team GOAL is
rendered as the lead's (and every member's) TRUSTED top-level instruction**, not fenced — its
provenance is the principal (the parent model's tool call from the user's own prompt, or the gRPC
request the deployment owns), and `WithTeamGoal` is the SOLE writer with no member-facing tool able
to mutate it, so trusting it is safe (instruction-hierarchy / spotlighting / CaMeL consensus: the
principal's task is trusted, only peer/retrieved data is untrusted). It is STILL run through
`neutraliseFraming` on render so it cannot forge a fence or a section header (defang-but-don't-fence).
A deployment that interpolates untrusted end-user text into the goal opts BACK into fencing via
`agent.WithUntrustedGoal(true)` (the Team-tool path is always trusted; the gRPC path flips it through
`server.Config.TeamGoalUntrusted`). `buildSynthesisSources`
assembles the rest of the prompt in three layers, ALL fenced UNTRUSTED via `writeUntrustedBlock`: (1) the
**findings ledger** (`team.Team.Findings()`, the PRIMARY channel — members append with the
`RecordFinding` coordination tool, a sixth member tool auto-exempted from the read-only-member
mutating-tool backstop because `MemberToolNames()` derives from `MemberTools`); (2) a
**LastText/completed-task digest** for members that recorded NO finding (rescues a limit-cut-off
member whose `LastText` is otherwise the only trace); (3) the **lead's drained inbox**.
`neutraliseFraming`'s header list is extended for every new synthesis/round-0 section header so
an injected body cannot forge one. A lead stopped purely by its lifetime turn budget is still
*resumable* (`memberRT.nonResumable` is set ONLY on `StopError`/`Reopen`-fail, NOT on budget), so
the ONE synthesis turn runs even then (§5 special-case); a genuinely non-resumable lead yields an
empty `Report` → the structured fallback.

**The deliverable chain — never a bare refusal or empty (`teamtool.go`).** `synthesise` is a
pure PRODUCER; the QUALITY gate lives in `deliverable(TeamOutcome)`, three tiers: **(1)** the
lead's synthesis when it is a usable report — non-empty AND `!isNonDeliverable(report, len(Findings))`;
**(2)** a ledger-rich structured fallback (`joinTeamFallback`) leading with the findings ledger
grouped by member, then per-member disposition + `[STOPPED: reason]` + completed tasks + last text;
**(3)** an honest floor ("ran N rounds, did not converge, M stopped") when even the ledger is empty —
always non-empty because round count + dispositions always exist. `isNonDeliverable` is CONSERVATIVE:
it fires only on empty/whitespace OR (short `≤ nonDeliverableMaxLen` = **280 runes** AND a lower-cased
PREFIX-anchored match against the tiny `refusalPrefixes` set AND `ledgerLen > 0`) — all three required,
so a legitimately terse real report is never discarded and a refusal over an empty ledger is left
alone (nothing better to show). A non-convergence header (`convergenceHeader`) is prepended to tiers 2
and 3 always, and to tier 1 only when `!Quiescent` (so even a plausible-looking synthesis on a runaway
team carries the "did NOT converge (stop: max-turns)" banner). The fallback SKIPS the lead's `LastText`
(`MemberOutcome.Lead`) — after synthesis it IS the rejected report, so echoing it would re-surface the
discarded refusal. `TeamOutcome` carries `Findings []session.TeamFindingSnapshot` and `MemberOutcome`
carries `Completed []string` + `Lead bool`, all populated in `outcome()` (clamped via
`projectTeamFindingsSnapshot`), so BOTH the Team-tool and gRPC paths get the rich fallback without
reaching into the live `*team.Team`. The headline regression guard is
`TestTeamToolRefusalSynthesisFallsBackToLedger`: a refusal synthesis over a populated ledger must never
reach the parent.

> **PARTIAL — 4A: per-engine token ceiling SHIPPED, team-AGGREGATE budget DEFERRED.** The
> per-RUN ceiling ships as the SHARED `agent.Deps.MaxRunTokens` loop ceiling (see the
> Token-budget note above), NOT a team-only `WithTeamTokenBudget`: a runaway team member crosses
> the per-run `MaxRunTokens` ceiling and ends with `session.StopBudget`, which the supervisor
> handles exactly like any other stopped member (the resilient-deliverable safety-net fallback
> still applies; a budget-stopped lead stays RESUMABLE so its one synthesis turn still runs —
> guarded by `TestBudgetStoppedLeadStillSynthesises`). This bundle remains the *safety net*
> (never return junk); the per-engine budget is the *per-member brake*. **Still open:** there is
> NO team-aggregate ceiling — the budget is per-run and `session.Reopen` resets the accumulator
> each round, so an N-member team can still spend ~N×`MaxRunTokens` across a round (cross-round
> lifetime is bounded only by `WithMemberTurnBudget`, a TURN count, not tokens). A supervisor-level
> summed-`session.Usage` budget that stops scheduling after the current round remains the residual
> 4A item.

**Member sessions persist for out-of-band inspection.** The supervisor saves each member session
to the injected `port.SessionStore` (`WithMemberStore`, never a concrete adapter — layering
holds) after every turn and after synthesis, under collision-free ids namespaced by the team id:
`MemberSessionID(teamID, member)` = `team-<teamID>-<member>` (the SINGLE source of truth both the
supervisor's `sessionID` prefix and the `InspectMember` tool's id derivation route through, so
they can't drift). The team id is the parent call id (Team tool) or the server-assigned
`team-<NewID()>` (gRPC, computed BEFORE `NewSupervisor` so the prefix can carry it). The parent
catalog's read-only **`InspectMember`** tool (`engine/agent/teaminspect.go`) loads ONE member's
transcript by (team id, member) and returns a BOUNDED rendering — it is PULL, never auto-injects
(gauntlet #7's no-auto-injection property holds: the transcript enters the parent conversation
only as that tool's own `ToolResult`).

**`EvTeamFindings` is fully wired to the wire.** A `team.findings` event mirrors `team.tasks`:
the Team-tool sink emits it on ledger change (de-duped via `findingsEqual`, clamped via
`clampPreview`) and on `EvTeamEnd` (terminal snapshot). It rides `session.TeamFindingSnapshot`
(domain), maps to the proto `TeamFinding` (`toProtoTeam`), and the mecatui client decodes it to
`client.TeamFinding` → the conversation block's `teamFindings`, consistent with the task path.

**`EvTeamEnd` also carries a per-member terminal disposition snapshot.** The supervisor already
knows each member's terminal verdict (`MemberOutcome`); `runTurn`'s stop branch classifies the
cause into a closed `MemberStopReason` (`error`/`cancelled`/`budget`) and `outcome` derives a
closed `MemberDisposition` (`done`/`stopped`). The cause→reason classification tests
`stop == StopCancelled` BEFORE `reopenErr` (a cancelled member's `Reopen` also fails, so without
this ordering a genuine cancellation would collapse into `error`); a failed `Reopen` folds into
`error`; `budget` is the residual lifetime-cap cause; `StopNoProgress` and a clean idle stay
`done` (no special handling — `runTurn` never marks them stopped). The snapshot rides
`session.TeamMemberDisposition` (domain, plain strings) bridged in `engine/agent`
(`projectTeamDispositions`) exactly like the tasks/findings bridges (session never imports
`engine/team`), maps to the proto `TeamMemberDisposition` (`bool stopped` + closed-enum
`TeamMemberStopReason` — `(done, error)` non-representable) via `toProtoTeam` /
`toProtoTeamMemberStopReason`, and the mecatui client decodes it to `client.TeamMemberDisposition`
→ the lane's `stopped`/`stopReason`. It is CONSUMED BY THE CLIENT OVERLAY, NOT the model (the
model already gets `[STOPPED]` in the lead's report via `joinTeamFallback`), so the overlay renders
`✗ stopped — <reason>` distinctly from `✓ done` instead of recomputing "done" and contradicting the
supervisor. It is a supervisor verdict (closed enums, `Name` already on the roster), kept OFF the
`EvTeamMember` redaction channel exactly like the tasks/findings discipline.

**Team observability structural guard.** Because `TeamPayload` is deliberately
fuller-than-metadata (bounded member message/tool previews, task descriptions, findings, and terminal
dispositions), it now has the same review gate Parallel already had: `engine/session/team_payload_test.go`
allow-lists every top-level and nested team projection field and rejects unreviewed content-shaped
additions (`Args`, `Content`, `Message`, `Prompt`, `Transcript`, `PermissionAsk`, `Ask`). This pins the
redaction contract in code: `team.member` may be watchable, but permission asks are never forwarded,
every content-shaped field is explicitly reviewed as bounded, and member transcripts still reach the
parent only through the Team result or an explicit `InspectMember` pull.

**The Subagent delegation tool (read-only explorer) gets the SAME treatment** (Phase 2): when Bash is
configured, `SubagentTool` holds a worktree `childForker` (`WithChildForker`) and forks each child
run into a throwaway git worktree BEFORE running it (`buildChildEngine` registers Bash via the
SAME `buildSandboxedCommandRunner`; `buildSubagentTool` wires the worktree forker iff a runner
exists; per-def Subagent engines keep Bash via `scopedToolNamesMode`'s `allowShell` and share the
one forker). So a `Subagent` to "investigate X" can now `git log`/`git show`/`cat`/build/test in an
isolated checkout — Edit/Write still dropped, no Subagent/Parallel recursion. `SubagentTool.ReadOnly()`
stays **true**: isolation (not catalog read-only-ness) is what keeps Subagent read-parallel — its
writes land in the worktree, never the shared base; a fork FAILURE is a tool error, NOT a
silent fallback to the shared ws. A `WithMaxConcurrentChildren` (default 4; old
`WithMaxConcurrentSubagentShells` is a deprecated alias) semaphore bounds concurrent children —
ALL of them now, forking and forker-less (Subagent is read-parallel, so the model can fan out; see
the Subagent-concurrency-cap note above). Same
`gitenv` hardening + same untrusted-`.gitattributes` residual as team members; the
workspace-trust gate is the SHARED follow-up for both Team + Subagent.

**Compaction never emits unpaired history (tool-pairing invariant).** Both compactors
(`HeuristicCompactor` in `compaction.go`, `CascadeCompactor` in `cascade.go`) slice a kept
tail by message COUNT. The bug: when the count cut landed ON a `RoleTool` message whose
matching assistant `ToolCall` was dropped into the summarised head, the replayed history
opened on an ORPHANED tool result → provider HTTP 400 ("No tool call found for function call
output with call_id X") → run stop=error → `Session.Fail()` → every subsequent prompt rejected
with `illegal state transition: RecordUserPrompt from "failed"` (permanently bricked).

The fix has three layers, all pure/offline:
- **Cut-snapping.** The shared unexported `snapCutToTurnBoundary(msgs, cut)` clamps `cut` to
  `[0,len]` then advances it past any leading `RoleTool` messages (`for cut<len && msgs[cut].Role==RoleTool { cut++ }`),
  so the tail never STARTS on a tool result. Used by `HeuristicCompactor.Compact` (replaces the
  bare `len-keep` clamp) AND `CascadeCompactor.Compact` (applied to its head-floored cut before
  deriving tail/middle). Edge cases: a tail that is entirely tool results snaps to `len` (empty
  tail — system+goal+summary still emitted, output non-empty); multiple consecutive leading
  orphans are all consumed by the while-loop.
- **Self-validation + abort-to-original.** `session.ValidateToolPairing(out)` is BIDIRECTIONAL
  (errors on an orphaned tool result OR a dangling assistant call). Each compactor runs it on the
  assembled slice and, on failure, returns the ORIGINAL `conv.Messages` wrapped in the exported
  sentinel `agent.ErrCompactionWouldOrphan`. The cascade funnels EVERY successful return through a
  small `finish(conv,head,middle,tail,paths,notes)` helper so all five tier return points validate.
- **Loop + aggregate backstop.** `maybeCompact` treats `ErrCompactionWouldOrphan` like the existing
  Compact-error branch (WARN + return false, no compaction Event, REUSING the existing WARN line so
  the "loop emits exactly THREE diagnostics lines" invariant holds), and runs a defensive
  `ValidateToolPairing(compacted)` between `Compact` and `ReplaceHistory`. `Session.ReplaceHistory`
  itself now rejects an unpaired slice (aggregate-level guard), so the existing ReplaceHistory-reject
  branch catches pairing failures for free.

Coverage: unit tests on both compactors (orphan-at-tail-head, tail-all-tool-results,
multiple-consecutive-leading-orphans, cascade-orphan), `ValidateToolPairing` table test, and an
END-TO-END `TestCompactionThroughLoopNeverOrphans` that drives the real loop + real
`HeuristicCompactor` with a mockllm tool-call script and a tiny `ContextWindowTokens`, asserting
the final history is pairing-valid and the session did NOT reach `StateFailed`.

**Shipped (issue #51): `failed` now recovers via `Session.Recover`.** The deferred follow-up
landed as a third terminal-recovery seam (`failed → Recover → idle`, history-repaired via the
same `closeOutInterruptedTurn` as Interrupt; `Reopen` stays completed-only — no widening), wired
into the service layer's `loadAndReopen` so every wire surface gets it for free. The compaction
pairing fix above REMAINS the trigger-removal defense; Recover is the degrade-gracefully defense
for any other transient provider failure. Both are kept.

## Adapters — `internal/adapter/`

### `anthropic` (multi-provider P1)

The native Anthropic **Messages**-API `port.LLMProvider` on the official MIT `anthropic-sdk-go`;
STATELESS full-replay like openai — no server-side conversation id, full `messages` array
resent each turn; meets the port ONLY in `internal/app`'s registry via `newAnthropicEntry`, NO
domain/agent/server/acp/proto edit — the abstraction-held proof. The genuine wire-divergences
are adapter-construction Options/seams, NOT `LLMRequest` fields, and the adapter stays
CATALOG-FREE: `max_tokens` is REQUIRED by Anthropic and resolved **PER REQUEST MODEL** via
`WithMaxTokensResolver` (composition injects `anthropicOutputLimit` from the catalog;
`buildParams` computes it from `req.Model`, conservative 4096 fallback for uncatalogued) —
critical so a per-session/sub-agent route to a smaller-ceiling model (e.g. claude-3-5-haiku=8192)
never sends the default model's larger value and 400s.

Extended thinking is **ON + MODEL-AWARE** via `thinkingConfigFor` with THREE classes —
`{type:adaptive}` for Opus 4.8/4.7/4.6 + Sonnet 4.6 + Mythos (manual `{type:enabled,budget_tokens}`
**400s** on Opus 4.8/4.7), `{type:enabled,budget_tokens:N}` (`WithThinkingBudget`, clamped
≥1024 & <max_tokens) for older thinking-CAPABLE families (Claude 4 + 3.7 Sonnet), and
**NONE/omit** for thinking-INCAPABLE models (Claude 3.5 and earlier — `{type:enabled}` 400s
there); `display:summarized` set so display deltas stream. `New` passes
`option.WithoutEnvironmentDefaults()` FIRST (no ambient
`ANTHROPIC_BASE_URL`/`ANTHROPIC_AUTH_TOKEN`/WIF autoload — single-knob custody). Tool
`input_schema` passes the WHOLE schema (`ToolInputSchemaParam.ExtraFields` carries
`$defs`/`additionalProperties`/enums). Per-block stream buffers (tool-args/thinking/signature)
capped at 8 MiB. `cache_control` is a SINGLE ephemeral breakpoint at the `prompt.Layered`
StablePrefix boundary.

**Usage is NORMALIZED to the engine contract at `translateMessageStop`:** Anthropic reports
`input_tokens` EXCLUDING cache reads/writes (OpenAI's includes them), so the adapter folds
`cache_read_input_tokens` + `cache_creation_input_tokens` into `session.Usage.InputTokens`
(cache writes are part of the prompt and billed) — `InputTokens` is the FULL prompt and
`CacheReadTokens ⊂ InputTokens` holds cross-provider. This made `--max-run-tokens` bill
Anthropic runs correctly (previously undercounted by the cache-served portion) and populated
`CacheWriteTokens` (never set before — the mecatui footer's ⊕ facet now renders for Anthropic).
Guards: `anthropic.TestTranslateCacheWriteTurn` + the `TestUsageCacheReadSubsetOfInput` parity
pair (one per adapter package).

**Reasoning replay is PACKED INTO `Message.Reasoning` (which STAYS A STRING)**: Anthropic's
replay unit is a LIST of `thinking{thinking,signature}` + `redacted_thinking{data}` blocks
(interleaved thinking ⇒ several per turn, redacted must round-trip too), packed as a versioned
JSON envelope `{"v":1,"blocks":[{"t":"thinking","x":..,"s":..},{"t":"redacted","d":..}]}`
(`reasoning.go`) and unpacked to reconstruct the thinking blocks BEFORE `tool_use` on the next
turn (mis/omit ⇒ 400 — the load-bearing item); one `ChunkReasoningItem` emitted at
`message_stop`, display deltas stream separately as `ChunkReasoning`.

`engineDepsForProvider`/per-session routing/capability-intersection/per-sub-agent-provider
switch ALL work for anthropic with zero new code — it's data. Capabilities
Image:true/Audio:false/EmbeddedContext:true; default model `claude-sonnet-4-6`. **LIVE model
listing SHIPPED** — a keyed `Lister` (`lister.go`, `client.Models.ListAutoPaging`) maps the rich
`ModelInfo` → its OWN neutral `anthropic.Model` (`MaxTokens`→OutputLimit,
`MaxInputTokens`→ContextLimit, `ImageInput`→image,
`Capabilities.Thinking.Types.{adaptive,enabled}`→a `ThinkingDescriptor`); UNLIKE the keyless
openrouter lister this endpoint is AUTHENTICATED — the lister carries the key for a READ-ONLY
metadata GET, used ONLY to read + NEVER logged (CWE-200), availability-gated by construction;
offline-tested via `option.WithHTTPClient` mock transport + a `testdata/models.json` fixture.
**Thinking-from-live:** a new `WithThinkingResolver` Option lets `thinkingConfigFor` read the
LIVE descriptor when `known=true` (adaptive ⇒ adaptive, else enabled ⇒ manual, else NONE) and
FALL BACK to the hardcoded prefix matrix
(`adaptiveThinkingPrefixes`/`thinkingIncapablePrefixes`, NOT deleted) as the OFFLINE FLOOR; the
`max_tokens` resolver is likewise live-first via the `liveMetaStore`.

### `permconfig` (file-based permission config — issue #13)

A `permpolicy.RuleResolver` that re-resolves per workspace-root the shared
`.mecatl/settings.yaml`→`ScopeSharedProject`, the gitignored
`.mecatl/settings.local.yaml`→`ScopeLocalProject`, the matching Claude `settings{,.local}.json`
imports, explicit `--permission-config` files→`ScopeCLI`; trust-gates project allows; caches
per root with **mtime/size revalidation** so a mid-process edit takes effect; byte+rule caps.

### `workspacetrust` (WORKSPACE-TRUST — see `WORKSPACE-TRUST-SPIKE.md`)

**Phase 1:** a stdlib+`xdgconfig` leaf reading an operator-authored, **read-only**
`trustedWorkspaces: [<abs path>]` list from the user-global `settings.yaml` — config DATA,
never a governance `Rule`; realpath-keyed (`filepath.Abs`+`EvalSymlinks`) so a moved/symlinked
path can't forge trust; fail-safe (missing/malformed ⇒ untrusted). The fold is **composition**:
`internal/app/trust.go`'s `resolveTrust(cfg) TrustDecision` combines `--trust-project`
(`TrustFlag`) > a declared match (`TrustDeclared`) > none, then `Build` collapses `.Trusted`
onto `cfg.TrustProject` so permconfig AND the soul gate honour declared trust through the
**same** monotonic-positive admission path — it only GRANTS, never overrides a Deny/Ask.

**Phase 2a** widened the gate beyond allows+soul to the full **project authority set**: when
untrusted, composition also withholds the **PROJECT TIER ONLY** of agent definitions, slash
commands, and skills (`<workspace>/.mecatl/*`, `<workspace>/.claude/*`) — via the additive
`agents`/`skills` `ResolveOptions.IncludeProjectTier` (set to `cfg.TrustProject` in
`internal/app`; the three skills callers — `resolveSkills`/`resolveSkillIndex`/`activeSkillDirs`
— all pass it) and a `buildDirCommandExpander` branch (gated on `cfg.Workspace!="" &&
!cfg.TrustProject`) that drops the default project-tier command dirs (an explicit
`--commands-dir`/`--agents-dir`/`--skills-dir` is operator-supplied and stays). User-tier
config, built-in tools, the base prompt, every Deny/Ask, and the permission prompt are NEVER
gated — an untrusted repo degrades to "ask the human", not "do nothing".

**Phase 2b** added the machine-written `<xdg>/mecatl/trust.yaml` registry (a SIBLING of, never
inside, the human `settings.yaml` — settings-vs-state split): `registry.go`'s
`Remembered`(read)/`Remember`(write — `O_NOFOLLOW`+`0o600`+temp-rename, realpath-keyed,
INJECTED `trustedAt` so the adapter never calls `time.Now()`, fail-to-untrusted) + `anchor.go`'s
`AnchorHash` (the **identity anchor** = project soul ⊕ project-tier agent/command/skill defs,
deterministic sorted fold; **`settings.yaml` EXCLUDED** — permission edits re-resolve live,
never nag). `resolveTrust` now folds `flag > declared > REMEMBERED > none`; a remembered entry
grants only while its stored anchor MATCHES the live anchor — a mismatch ⇒ `Drifted` and
`Trusted=false` (fail-safe; `narrateTrust` Warns). `mecated` reads the registry declaratively
but NEVER prompts/writes (the write API's only production caller — the mecatui first-encounter
prompt — is Phase 2c). The `SHA256Hex` primitive is the shared `internal/adapter/hashutil`
leaf, used by BOTH the soul adapter and the anchor; **soulguard's soul-only `.sha256` sidecar
anchor stays PARALLEL** (different surface/store/re-bless gesture — share only the primitive,
don't merge the two drift mechanisms).

### `soul` (issue #14 Phase 1 — see `SOUL-SPIKE.md`)

A user-scoped, **agent-read-only** persona over `~/.config/mecatl/soul.md`, satisfying
`prompt.SoulSource`; env-injected resolution — NOT the WorkspaceReader, the file is outside any
session root — injection-scanned via `skills.ScanForInjection` + 20 KiB cap, fail-soft, **no
write path** — every method is read-only: `Load`, `LoadWithMeta` (issue #14 Phase 3: the same
clean body + its sha256 computed in one read, no second read), `ResolvedPath`, plus `NewWithEnv`
(an env-injectable READ constructor — composition uses it to resolve the user soul against a
faked XDG in tests; still no write).

The drift BASELINE write lives in `internal/app/soulguard.go` (composition), NEVER the adapter:
a harness-owned `<soulPath>.sha256` sidecar, trust-on-first-use, `slog.Warn` on mismatch,
`--approve-soul` re-baselines, `--soul-strict` drops a drifted soul; drift is NOT a governance
gate, the soul stays fenced DATA.

**Provenance + trust (issue #14 Phase 3 Item 2)** is decided in `internal/app/soulselect.go`,
NOT the adapter: a USER soul (`<xdg>/mecatl/soul.md` or `--soul-file`) is always trusted; a
PROJECT soul (`<workspace>/.mecatl/soul.md`) is untrusted-by-default and honoured only with
`--trust-project` (the SAME issue-#13 gate — not a new flag, not via governance); USER-WINS
precedence; an untrusted project soul is dropped + `slog.Warn`-logged. The adapter stays a pure
loader — it never sees provenance/trust.

**Read-only TUI inspection (issue #14 Phase 3 Item 3)**: `internal/app/soulsnapshot.go`
projects the winning soul's content + `soulMeta` into the proto `SoulInfo` for the `GetSoul`
RPC (a build-time snapshot) and wraps the user-model store's read-only `Index` into the
`GetUserModel` LIVE lister; the new `soul`/`user_model` caps gate the read-only `/soul`
(scrollable) + `/usermodel` mecatui panels — trust/drift computed in composition, only
displayed in the ui.

### `memory` (issue #14 Phase 2 — see `MEMORY-*.md`)

Per-project Remember/Recall/SearchMemory store — AND issue #14 Phase 2's SECOND, user-scoped,
**cross-project** user-model store: a separate `memory.New(<xdg>/mecatl/usermodel)` exposing
the parameterized RememberUser/RecallUser/SearchUserModel family under an enforced `user/`
prefix, satisfying `prompt.UserModelSource`, with a write-time `skills.ScanForInjection` over
BOTH the value AND the effective description — the `<user-model>` block renders key+description,
so a value-only scan would miss a payload in `description` — plus a `</user-model>`
fence-close-tag reject (mirrors soul) — an adapter→adapter edge like `soul`; the WRITABLE
user-model is FACTS not rules, never a governance scope.

### `providercatalog` (multi-provider Phase 0 S2)

A pinned `go:embed`-vendored CURATED SUBSET of the models.dev catalog — DATA leaf,
stdlib+`embed`+`encoding/json` ONLY, no domain/port/app/adapter import, no live refresh; typed
read-only `Catalog`/`Provider`/`Model` value types parsed once in `Default()` with a
**panic-on-parse** posture — compiled-in data ⇒ a parse failure is a build bug, not a runtime
fail-safe; composition reads per-provider `EnvVars()` for availability + exposes
`Models()`/`ContextLimit()`/modalities/`SupportsImageInput`/`SupportsReasoning` for S3
ListModels + S5 cap-intersection. Curation is documented + count-guard-tested — all openai (52)
+ all anthropic (24) + a hand-pinned 19-id openrouter flagship allowlist, with the deterministic
`jq -S` regen recipe + MIT attribution (`MODELS_DEV_LICENSE`) vendored alongside; it is now the
**FALLBACK FLOOR**, not the only source — a provider with a live `modelLister` (openrouter) has
its real catalog fetched and REPLACES the curated subset, with the embedded subset shown on any
live error/empty/offline.

### `openrouter` (LIVE model listing leaf)

GETs the FIXED-host const `https://openrouter.ai/api/v1/models` over an INJECTED `*http.Client`
— KEYLESS (no `Authorization`; CWE-200), fixed-host (no SSRF; CWE-918), `io.LimitReader` 4 MiB
cap (CWE-770), ctx+client timeout; stdlib-ONLY, no domain/port/app/other-adapter import; returns
its OWN `openrouter.Model` (composition maps it to `modelEntry` — no import cycle); maps
`id`/`name`/`context_length`/`top_provider.max_completion_tokens`→OutputLimit (the output
ceiling, captured for the resolvers)/`architecture.input_modalities`/`supported_parameters∋{reasoning,tools}`.

### `openai` tool schemas — NON-STRICT (shared by openai + openrouter)

`openai.buildTools` sends function tools **non-strict** (`FunctionToolParam.Strict` left unset
→ the SDK omits it → upstream default applies). Strict mode would require every tool schema's
`required` to list ALL of its `properties`, but many built-in tools carry genuinely optional
params (Bash `timeout_ms`, Edit `replace_all`, Read `offset`/`limit`, Grep `path`, memory
Remember/query, ToolSearch, Parallel, Team, Subagent, …); a strict-enforcing OpenAI-compatible
upstream (Azure reached via OpenRouter) `400`s those. We don't need the guarantee: **argument
validation lives at the execution edge** — every tool re-parses/validates via
`session.ParseArgs` / `NewToolError` before acting. The openai adapter is shared by the
`openai` and `openrouter` provider ids, so non-strict here covers both.

### `llmresilience` — cleanup-cancel vs genuine deadline/cancel

`establish` applies the per-attempt timeout via a child context it must `cancel()` to release.
The TRUE error cause is captured **before** that cleanup `cancel()` (`cause := attemptCtx.Err()`
then `cancel()` then `attemptError(cause, err)`); reading `attemptCtx.Err()` AFTER cancel would
report `context.Canceled` and mask every real establishment error (e.g. a 400) — which the loop
then mis-classifies as a caller cancel and terminates as "cancelled" with the real message
discarded. `attemptError(cause, err)`: `cause==nil` ⇒ return `err` verbatim (real error
surfaces → `StopError`); `cause!=nil` ⇒ wrap `cause: err` (genuine per-attempt deadline stays
retryable; genuine caller-cancel stays a cancel / wedge-recovery).

**Breaker counts only TRANSIENT failures.** The per-provider circuit breaker is for
provider-health signals, not every establish failure. `Stream` calls `recordFailure` ONLY when
`isTransientForBreaker(err)` is true — a breaker-specific predicate (mirroring the classifier's
`errors.As` chain) DISTINCT from `cfg.Classifier`/`retryableStatus`: true for HTTP 408/429/5xx,
`net.Error`, and a bare `context.DeadlineExceeded` (per-attempt timeout); false for all other 4xx
(400/401/403/404…), `context.Canceled`, unknown, nil. It deliberately **diverges on HTTP 409**:
409 is retryable per-request (so `retryableStatus` returns true) but a request conflict is NOT a
sign the provider is unhealthy, so it must not trip a shared breaker — `isTransientForBreaker`
returns false for 409. The breaker must NOT be coupled to the caller-injectable `cfg.Classifier`,
hence its own inline status switch (not `retryableStatus` minus 409). In `Stream`, the order is:
caller-cancel check first (breaker-neutral, never retried) → `recordFailure` only if transient →
permanent errors surfaced verbatim → backoff. A permanent error and a caller-cancel leave the
breaker counters UNTOUCHED (neither `recordFailure` nor `recordSuccess`); a half-open trial that
fails with a PERMANENT error leaves the breaker in `open&halfOpen` so the next `allow` re-admits a
trial after cooldown. **Motivating incident:** a burst of permanent 404s (OpenRouter
policy-blocked / unavailable models) was counting toward the shared breaker via an unconditional
`recordFailure`, tripping it and then blocking unrelated WORKING models for the cooldown.

**Post-first-chunk idle bound (`StreamIdleTimeout`).** `PerAttemptTimeout` bounds only
establishment and the FIRST chunk; once streaming proper begins the per-attempt context is left
live and there is no per-chunk deadline. A real upstream SSE connection can stall mid-stream —
the openai/anthropic adapters' `stream.Next()` then blocks forever, the loop never sees
`ChunkDone`, and the turn hangs in "thinking" permanently. `StreamIdleTimeout` (default 120s; 0
disables) closes this: in `restSeq`'s continuation loop each `next()` runs on a helper goroutine
and a `time.NewTimer` (real time, NOT `cfg.Clock` — that drives breaker math only) is reset to the
idle budget per iteration. On a timeout the wrapper `cancel()`s the per-attempt context (to
unblock the inner `stream.Next()`), DRAINS the in-flight helper (so neither it nor the pull
coroutine leaks — `next()`/`stop()` may not run concurrently, so the helper must finish first),
then yields a synthesized terminal **`*StreamIdleError`**.

The synthesis is load-bearing: both adapters **swallow the ctx error on cancel** (they yield
NOTHING once the context is done), so cancelling unblocks `stream.Next()` but surfaces no error —
the wrapper must produce one itself. `StreamIdleError.Unwrap()` returns `context.DeadlineExceeded`
so `errors.Is(err, context.DeadlineExceeded)` holds (classified as a deadline, not a caller
cancel). It is **TERMINAL and never retried** — no-replay-after-first-chunk holds, so a mid-stream
idle stall ends the turn as `StopError` rather than replaying a partially-observed turn. The
breaker is untouched (a mid-stream error structurally never reaches the establishment seam where
`recordFailure` lives). When `StreamIdleTimeout <= 0` the loop is the plain pull (no goroutine, no
behaviour change). A package-level `goleak` gate (`leakmain_test.go`) proves the watchdog goroutine
unwinds on every path.

## Composition — `internal/app/` (multi-provider — see `MULTI-PROVIDER.md`)

The single shared assembly of provider + catalog + policy + engine into a `server.Service`
(`app.Build(ctx, Config)`). Both composition roots consume it — `cmd/mecated` (serves it over
TCP) and `cmd/mecatui` (hosts it embedded over a UNIX socket). It MAY import adapters,
`engine/agent`, and (via the `server` adapter) `contracts/gen`; nothing imports it except the
`cmd/` mains.

**Provider registry (Phase 0, S1, `registry.go`):** `buildProviderRegistry(cfg, detect
envDetector)` is a COMPOSITION-ONLY (not a port — single consumer) `providerRegistry` of N
configured providers (`openai`, `openrouter`), keyed by a WIRE-STABLE id matching models.dev.
AVAILABILITY is env-auto-detected via the injectable `envDetector` seam (defaults to
`os.Getenv`, set in `Build`; tests inject a fake map so registry construction is offline)
against the **`providercatalog` catalog's per-provider `env[]`** (S2 replaced S1's inline
`builtinProviderEnv` map): `providerEnvVars(id)` reads
`providercatalog.Default().Provider(id).EnvVars()` (`openai`→`OPENAI_API_KEY`,
`openrouter`→`OPENROUTER_API_KEY`). The openrouter `OPENAI_API_KEY` fallback is a mecatl
CONVENTION, so it is a COMPOSITION augmentation appended in `providerEnvVars` (the vendored
catalog stays HONEST to upstream — openrouter's `env[]` is `["OPENROUTER_API_KEY"]` only); the
catalog never carries mecatl policy. OpenRouter rides the SAME stateless `openai` adapter with
`WithBaseURL("https://openrouter.ai/api/v1")` + the OpenRouter key — no separate wire adapter in
P0. Only AVAILABLE providers are constructed (each `llmresilience.Wrap`-ped); `UseMock`
short-circuits to a single `mock` entry; zero available + `!UseMock` ⇒ the named `errNoProvider`
(names both env vars + `--openai`/`--mock`). Startup logging emits provider id + base URL only,
NEVER the key (CWE-200). `buildProvider` is a thin shim returning the registry's DEFAULT
provider so the `Build` call site is unchanged.

**`engineDepsForProvider` (`build.go`, designed S1 / consumed S3):** the SINGLE enumeration of
provider/model-closing `agent.Deps` fields — `LLM`, `Compactor` (binds provider+model by value),
`Model`, `TokenCounter` (model-keyed), `PromptConfig.Env.Model` (+ the agency-delta `Role`),
and **`ContextWindowTokens`** (the S1-deferred "6th field": its `contextWindow` param, `<=0` ⇒
the 128k default; the per-session factory passes the catalog `ContextLimit()` for the selected
model so the compaction trigger AGREES with the `ListModels`-advertised `context_limit`, and
`baseEngineDeps` passes 0 so the DEFAULT path stays byte-identical at 128k). `baseEngineDeps`
delegates for the default provider+model, so a per-session engine bound to a non-default provider
re-derives EVERY provider-closing field rather than shallow-cloning + swapping only `LLM` (which
would compact/count through the wrong model — cross-provider contamination).

**Per-session provider/model routing (S3, `sessionEngineFactory` + `modelsnapshot.go`):**
`buildProvider` returns the registry ALONGSIDE the default provider so composition threads it
into the factory + `modelSnapshot`. The widened `server.SessionEngineFactory func(ctx, sel
server.ProviderSelector, specs)` is the ONE per-session-engine seam, serving a non-default
provider/model selector AND/OR client MCP (orthogonal → ONE engine over ONE catalog). The factory
resolves `sel` against the registry (unknown/unavailable id ⇒ error wrapping
`server.ErrInvalidArgument`, never a silent fallback; `model_id` flows VERBATIM — catalog gates
nothing), then builds Deps via `engineDepsForProvider`. The neutral `ProviderSelector` keeps the
server adapter free of registry/catalog imports; `session.Session` is NOT widened (selector →
ENGINE at create-time, registered in the SAME `sessionEngines` map + selected by
`StartRunContent` like the MCP path; `loadAndReopen` untouched). That map is CAPPED at
`server.Config.MaxSessionEngines` (default 1024) — the gRPC/HTTP surfaces have no teardown drain,
so an uncapped map is a CWE-770 DoS; past the cap `createSession` returns
`ErrTooManySessionEngines` (gRPC `ResourceExhausted` / HTTP 429), `CloseSession`/`EndSession`
frees a slot. `modelSnapshot(reg)` projects registry.Available() × catalog →
`[]*mecatlv1.ModelInfo` (public metadata only, mock-skipped, sorted), injected into
`server.Config.Models` (the ListAgents idiom).

**Per-sub-agent provider (SHIPPED, both halves):** a Subagent agent def / team member may pin a
`provider:` frontmatter field (`agents.AgentDef.Provider` — PURE DATA, the adapter never imports
the registry) to route its CHILD engine to a different provider than the parent; resolution is
composition-only via `resolveProviderModel(cfg, reg, def, parentProviderID, parentModel)`
(extends `resolveModel`) — provider precedence `def.Provider > session-selected provider >
build-time default`, realised by what the call site threads as `parentProviderID` (build-time =
`reg.Default()`/`cfg.Model`; a SELECTED session = its resolved provider/model). On a provider
SWITCH the model rebases off `def.Model`-or-`builtinDefaultModel[pid]` (NEVER the inherited
parent model — a bare `gpt-5` is invalid on openrouter); same-provider keeps the existing
`resolveModel` chain (full back-compat). Every child routes through the new
`newChildEngineForProvider` → `engineDepsForProvider` (contamination fix — child on X
compacts/counts/prompts through X+model's window; a non-switching child keeps window=0 ⇒ 128k,
byte-identical). Unknown/unavailable `provider:` ⇒ loud `slog.Warn` + parent fallback (mirrors
every other forgiving def-error handler). **Half B** gives a provider-SELECTED session its
sub-agent tools (pre-Half-B the per-session catalog was core-tools-only and could not spawn
Subagent/Team; since issue #42 the per-session catalog is the FULL shared formula — see the
assembleCatalog paragraph below) by building a per-session Subagent tool (+ in-catalog Team tool under `--enable-teams`)
inside `sessionEngineFactory` over the SAME `buildSubagentTool`/`buildTeamWiring` builders (no
per-session-catalog drift), wired to the session provider as parent; the Subagent tool's inline-MCP
close folds into `SessionEngineResult.Close` (torn down by `CloseSession`/`Service.Close`),
bounded by `MaxSessionEngines`. The registry NEVER leaves composition — the child engine gets a
bare `port.LLMProvider`. **DEFERRED:** the standalone gRPC `CreateTeam` RPC stays on the default
provider (no per-CreateTeam selector); `ListAgents`/`AgentInfo` provider surfacing (no proto
change).

**Server-global MCP on every session (bug #3 fix, `sessionEngineFactory`):** the
per-session catalog mounts the SERVER-GLOBAL MCP tools (`cfg.MCPServers` + ToolHive — the
same tools the build-time `buildCatalog`→`connectMCP`+`assembleCatalog` path mounts on the main engine), NOT just core + client
MCP. `Build` threads the shared, already-connected `mainMgr` into the factory as
`globalMgr`; the factory calls `mcp.Register(cat, globalMgr.Tools())` right after
`registerCoreTools` and BEFORE the client specs. Before this, a selector session (any
`provider_id`/`model_id` — what the mecatui `/models` picker always sends) got a fresh
core-only catalog and silently dropped all ~100 server-global MCP tools. Mount order
**core → global MCP → client MCP → per-session Subagent/Team** yields **global-wins**
collision precedence: `mcp.Register` is **first-wins + skip-and-continue** — a colliding
client tool is skipped (the global one stays) while EVERY other non-colliding client tool
is still registered, the skipped names accumulating into one aggregated `ErrDuplicateTool`
(`errors.Join`, still `errors.Is`-matchable). (A return-on-first-dup would silently drop
every tool ordered after the collider; the skip-and-continue form also fixes two global
servers clashing.) `Register` ALSO returns the skipped tool NAMES (`(skipped []string, err
error)`), so each of the three mount sites logs ONE **provenance-bearing** WARN listing
the dropped tools: the per-session CLIENT mount → *"client MCP: tool(s) shadowed by an
existing server-global tool of the same name (the global tool wins): <names>"* (the
client↔global tier, the line an end-user reads); the per-session GLOBAL mount and
build-time GLOBAL mount (both now in `assembleCatalog`) → *"server-global MCP: skipped duplicate tool
name(s) (a server advertised a name already registered): <names>"* (a within-/across-global
defective-server condition). These are composition logs, so the loop's three-line
diagnostics invariant does not apply. (Identity-dedup, a `CreateSessionResponse` skipped
field, and TUI rendering are a deferred follow-up — out of scope.) **Lifecycle:**
`globalMgr` is owned by `Build` — it is reused (NOT reconnected) and its `Close` is NEVER
folded into `SessionEngineResult.Close` (a per-session `CloseSession` closing the shared
manager would kill MCP for all other sessions). **Subagent/Team parity:** the factory threads
`globalMgr` (falling back to the per-session client `mgr` only when nil) as the
`reference:`-resolution mainMgr into `buildSubagentTool`/`buildTeamWiring`, so a selector
session's subagent `reference: <name>` resolves against the global servers like the
build-time path; managers are NOT merged (no entangled lifecycles) and per-def INLINE MCP
entries connect independently. Guards: `TestSessionEngineFactoryMountsGlobalMCPToolsForSelector`
(catalog + dispatch proof), `TestSessionEngineFactorySelectorCloseKeepsGlobalMCP` (DELETE-counter
proof the shared manager survives a per-session close), `TestSessionEngineFactorySelectorNilGlobalMCP`
(nil-globalMgr: core present, no global tool), `TestSelectorSubagentRefResolvesGlobalMCP` /
`TestSelectorSubagentRefResolvesClientMCPWhenNoGlobal` (Subagent reference parity, both refMgr branches
driven through the factory), `TestSelectorClientToolCollisionGlobalWins` (skip-and-continue: global
wins, the other client tool survives), and `mcp.TestRegisterSkipAndContinueOnCollision` (the
Register-level contract — asserts the returned `skipped []string` names exactly the collider
and the joined error still matches `errors.Is(_, tool.ErrDuplicateTool)`).

**One catalog assembly for every engine (issue #42, `internal/app/catalog.go`):** the
THIRD firing of the per-session-catalog drift class (first server-global MCP under bug
#3, then Subagent/Team under Half B, then memory/user-model/Parallel/skills/
resource-meta-tools — a selector session's turn-0 prompt advertised the
`<memory-index>` while its catalog carried none of the six memory tools), so the fix is
STRUCTURAL rather than another hand-synced list: `buildCatalog` is now Phase A only
(connect the global MCP manager via `connectMCP`, open the flocked memory/user-model
stores — still the sole construction sites — start the consolidation goroutines,
discover skills via `resolveSkills`) and produces the process-wide `catalogAssets`
(global manager, agent registry, the two `tool.MemoryStore` seams — interface-typed,
under the typed-nil discipline: every assignment is a known-non-nil concrete store or
an untyped nil (`buildUserModelStore` returns the interface with untyped-nil returns,
guarded by `TestBuildUserModelStoreDisabledReturnsNilInterface`), so the typed-nil
interface trap cannot arise — the skills slice, the per-skill
read-root allowlist `skillReadRoots` — computed ONCE from that same discovered slice
(`internal/app.skillReadRoots`: unique `osfs.ResolveRoot(filepath.Dir(sk.Path))` per
skill, so the trust gate is inherited by construction and there is no second list to
drift) and threaded into EVERY production osfs Workspace constructor
(`osfsWorkspaceFactory` + the shared `newForkWorkspace` fork closure) as
`osfs.WithReadRoots`, making an activated skill's out-of-workspace files Read/Stat-able
by the absolute path the Skill tool's "Base directory" header advertises — and ONE
process-wide `agent.LRUForkReaper` so `ForkPreservedCap` stays a process bound). `assembleCatalog`
is the single registration path both the build-time shared catalog and every
`sessionEngineFactory` catalog run through, in the canonical order core → global MCP
(+ `MCPResourceTools` meta-tools) → client MCP → Subagent trio → Parallel → Team →
memory → user-model → Skill/SkillDraft (global-wins MCP precedence preserved). The
per-catalog inputs ride `catalogSession` (resolved provider/providerID/model, the
session's client manager, and `narrate` — the build-once-facts discipline: ENABLED/
DISABLED narration fires only on the build-time call; WARNs are ungated). The returned
close aggregates ONLY the Subagent inline-MCP close + the client manager's Close —
never `globalMgr`. Permissions needed zero changes: the six memory floor-Allows key on
tool NAMES in `defaultRules`, and the factory already shares the policy instance. The
only sanctioned per-session deltas remain the client MCP tools and the unwrapped hooks
(`maybeWrapUserModelReview` is main-engine-only). Guards: the kill-switch
`TestPerSessionCatalogMatchesSharedCatalog` — its shared baseline is the catalog the
REAL `buildCatalog` returns (not a direct `assembleCatalog` call), so a post-assembly
`MustRegister` snuck into `buildCatalog`/`buildEngine` (the historical bug shape)
shifts the baseline and fails; it then (a) pins a `requiredFamilyTools` list
(Subagent trio, Parallel, Team/InspectMember, Skill/SkillDraft,
ListMcpResources/ReadMcpResource, the global MCP tool — the QA-mutant-M2 fix: pure
equality is blind to a family dropped from BOTH paths) and (b) asserts exact
tool-name-set equality between that baseline and a selector assembly under a
fully-loaded config, modulo an explicit `mcp__<client>__*` allowlist when client
specs are attached, plus a factory-level superset check. Companions:
`TestSessionEngineFactoryRegistersMemoryToolsForSelector` / `...ForClientMCP` /
`TestSessionEngineFactoryOmitsMemoryToolsWhenUnconfigured`,
`TestSelectorSessionMemoryPromptHasMatchingTools` (the wire-level symptom: the captured
`port.LLMRequest` carries BOTH the `<memory-index>` message and the Recall tool spec),
`TestBuildNarratesFamilyFactsExactlyOnceAcrossSessions` (the narrate gate stays
build-once PAST the factory — a selector session adds zero family narration lines),
and the now-BEHAVIORAL `TestSelectorClientToolCollisionGlobalWins` (the global and
colliding client "globe" servers answer with distinct prefixes and the surviving tool
is EXECUTED — a precedence flip changes the output, so the global-wins assertion is
no longer tautological). The five bug-#3 guards above are unchanged and still green.

**LIVE model listing (`modellister.go`):** an OPTIONAL composition-local `modelLister` interface
(`ListModels(ctx) ([]modelEntry, error)` — NOT a port, single consumer; same reasoning as
`providerRegistry`) carried on an OPTIONAL `providerEntry.lister` field (set per-provider at
registry build for openrouter — NOT a type-assert on `entry.provider`, because openrouter+openai
share the SAME `openai.Provider` and only openrouter opts in). A future provider opts in =
implement the interface + set `entry.lister` (zero merge/snapshot plumbing change).
`liveModelSnapshot(ctx, reg)` is the live analogue of `modelSnapshot`: per available provider, a
SUCCESSFUL+non-empty live result REPLACES the embedded subset, ANY error/empty/timeout (or no
lister) falls back to `embeddedModels(pid)` (the floor) with one `slog.Warn` — never blanked,
never crashes; availability-gated to `reg.Available()` (no key ⇒ no entry ⇒ no fetch). The seed
(`modelSnapshot`) and the live refresh-floor share ONE source-agnostic type (`modelEntry`, used
by BOTH `embeddedModels` and the live listers) + ONE projection (`projectModelEntry`) + ONE sort
(`sortModelInfos`), so the floor and the seed cannot hand-sync-drift. Image is the SHARED
single-source predicate `adapterCaps.Image && hasImageModality(model.InputModalities)`
(extracted in `capability.go`, used by BOTH the live and embedded paths) — a live text-only
model advertises image=false even though the openai-adapter-backed provider reports Image:true;
scope is the PICKER only (a session bound to a live-only/uncatalogued model keeps the adapter
passthrough caps — the closer is deferred P2).

**Async swap:** `Build` seeds `Config.Models` with the EMBEDDED snapshot synchronously (Build
NEVER touches the network; `ModelSelection` honest from t=0), then `startLiveModelRefresh` kicks
ONE background goroutine that fetches live + atomically swaps via `Service.SetModels` (an
`atomic.Pointer[[]*ModelInfo]` inside the Service; `ListModels`/`ModelSelection` read it
lock-free, race-free); cancelled by `Close` (no leak); a `liveModelRefreshSync` test seam runs it
inline for deterministic offline e2e; a no-op when no available provider has a lister.

**LIVE METADATA → RESOLVERS (`livemeta.go`):** the live record feeds the request-path resolvers,
not just the picker. `modelEntry` gains `OutputLimit` + a `thinkingDescriptor{Known,Adaptive,Enabled}`
(the neutral, source-agnostic thinking projection — zero ⇒ unknown ⇒ adapter prefix floor; only
the live anthropic source sets `Known=true`). A composition-owned `liveMetaStore`
(`map[providerID]map[modelID]modelEntry` behind an `atomic.Pointer`, rides on
`providerRegistry.meta`) is **SEEDED FROM THE CATALOG at Build BEFORE any network**
(`seedFromCatalog` over `reg.Available()`) and atomically swapped by the SAME one-shot refresh
(`liveModelSnapshot` returns BOTH the proto slice AND a per-provider `[]modelEntry` map — the
picker + the store project from the ONE list, can't drift). The catalog read sites become
live-first-then-catalog-floor helpers: `meta.outputLimitFor` (replaces the bare
`anthropicOutputLimit` in `WithMaxTokensResolver`), `reg.meta.contextWindowFor` (replaces
`catalogContextWindow` at `build.go`/`agentdefs.go` — per-session children pick it up FOR FREE),
`meta.thinkingFor` (fed to the anthropic `WithThinkingResolver`), and `meta.modalitiesFor` (the
input-modality list, consumed by `modelCapability` — see below). Precedence is PER FIELD: live
when present & `>0`/`Known` (the SCALAR fields), else catalog floor, else the consumer's
conservative default; a live MISS for a model the catalog knows falls back WHOLESALE to the
catalog row (live absence NEVER erases the catalog); with NO lister the store is catalog-seeded ⇒
behaviour byte-identical. **`modalitiesFor` is the exception: PRESENCE-keyed, not value-keyed** — a
present live entry is authoritative EVEN with an empty modality list (treated text-only, matching
the picker), so only a true miss falls through to the catalog (the regression the picker≠echo
Medium pinned).

| field | helper | live source | catalog floor | default |
|-------|--------|-------------|---------------|---------|
| output ceiling | `outputLimitFor` | `modelEntry.OutputLimit>0` (clamped) | `anthropicOutputLimit` | adapter `defaultMaxTokens` |
| context window | `contextWindowFor` | `modelEntry.ContextLimit>0` (clamped) | `catalogContextWindow` | `engineDeps` 128k |
| thinking | `thinkingFor` | `modelEntry.Thinking.Known` | — (catalog has none) | adapter prefix matrix |
| modalities | `modalitiesFor` | `modelEntry.InputModalities` (PRESENT entry, even if empty) | `catalogModalities` | adapter-only passthrough caps |

**`modelCapability` modality input is LIVE-FIRST** (`capability.go`): the per-(provider,model)
capability intersection now reads `reg.meta.modalitiesFor` FIRST — `Image = adapter.Image AND
hasImageModality(live)`, `Audio = adapter.Audio AND hasAudioModality(live)` — falling through to
the `catalogModalities` floor, then to the adapter-only passthrough (uncatalogued + no live entry,
nil-guarded). It reads the SAME `modelEntry.InputModalities` the picker (`projectModelEntry`)
reads, restoring the single-source guarantee for the SESSION ECHO / ACP gate (not just the
picker). Fixes the OpenRouter text-only model reporting `image:true` (shared openai adapter, never
read its live `["text"]`). OpenRouter-scoped; openai-direct/anthropic passthrough semantics
unchanged.
**Anthropic + OpenRouter listers SHIPPED** (anthropic keyed `client.Models.List` +
thinking-from-live; openrouter captures `top_provider.max_completion_tokens`); **OpenAI stays
CATALOG-ONLY** (its `/v1/models` is sparse — no lister). **DEFERRED (live listing):** disk cache,
periodic/interval refresh, OpenAI lister (catalog-only by design), the per-session
live-capability closer, surfacing the output ceiling/thinking in the picker proto (Slice D), the
OpenRouter `reasoning_details` replay fix (a separate request-path bug, not bundled).

## Proto — `contracts/proto/mecatl/v1/` (multi-provider Phase 0 S3 wire surface)

`CreateSessionRequest` carries an OPTIONAL `provider_id`(4)+`model_id`(5) selector (two distinct
fields, NEVER slash-joined); `ListModels(ListModelsRequest)→ListModelsResponse` returns the
`ModelInfo` inventory (id/provider_id/display_name/image/reasoning/context_limit) for AVAILABLE
providers only, secret-free; `ServerCapabilities.model_selection`(12) is true iff ≥1 provider is
available (gates the client picker like `agents` gates `/agents`). Additive + old-client-safe (an
old client sends no selector ⇒ server default; reads an old server's
`model_selection=false`/`Unimplemented` ListModels ⇒ hides the picker).

## Store drivers — `contracts/proto/mecatl/driver/v1/` + `internal/adapter/grpcdriver/` + `engine/adapter/storeconformance/` (Phase B)

The remote-store seam: `SessionStoreService` (behind `port.SessionStore`) and
`MemoryStoreService` (behind `tool.MemoryStore`), selected ONLY in composition
(`--session-store-url` ⟂ `--store-dir`, `--memory-store-url` ⟂ `--memory-dir`;
`validateDriverConfig` is fatal on both-set AND on a `--driver-tls-*` file without
`--driver-tls` — silently-ignored config is a misconfig; all-empty is byte-identical to the
local stores). A `--memory-store-url` that fails to DIAL is FATAL too (explicit config =
loud-misconfig posture; only the default-on local `memory.New` stays fail-soft).
The settled decisions, condensed:

- **A — Wire encoding: opaque sessnap blob in a format-tagged envelope.** `bytes payload` +
  `string format` (`grpcdriver.SnapshotFormat = "sessnap-json/1"`); the payload is exactly
  `sessnap.Marshal` output and the driver NEVER decodes it (stores/returns verbatim;
  `session_id` is duplicated top-level on Save so a driver keys without decoding). sessnap owns
  schema evolution (additive JSON); the envelope owns format identification — the harness
  rejects an unknown format on Load with an INFRA error, never `ErrSessionNotFound`. Decode is
  harness-side (`sessnap.Unmarshal` → state-machine restore); tool-pairing is NOT revalidated on
  Restore (identical to memstore/jsonlstore — don't add `ValidateToolPairing` to this path). The
  driver sits at the SAME trust tier as the JSONL file on disk. KEYING: the top-level
  `session_id` is the AUTHORITATIVE storage key — the server wrapper rejects a Save whose
  payload carries a different id (`INVALID_ARGUMENT`), and the client's Load rejects a decoded
  session whose id is not the requested one (infra error, never not-found). CAPACITY:
  `grpcdriver.MaxSnapshotBytes` (64 MiB) is the protocol's required minimum message capacity —
  `Dial` raises the client send/recv call options to it and a conforming driver mounts
  `grpc.MaxRecvMsgSize(MaxSnapshotBytes)` (pinned by the storeconformance "large snapshot"
  subtest, which FAILS over default 4 MiB gRPC limits). FORMAT BUMP signpost (on
  `SnapshotFormat`): read-set-accept / write-newest, or the bump bricks stored sessions.
- **B — Server wrappers live in the adapter.** `NewSessionStoreServer(port.SessionStore)` /
  `NewMemoryStoreServer(tool.MemoryStore)` (embedding `Unimplemented*Server`) exist for the
  bufconn conformance fixtures; the session wrapper runs sessnap SERVER-side, so the wire
  conformance run exercises encode→wire→decode→state-machine→encode→wire→decode.
- **C — Error mapping.** Load miss: `NOT_FOUND` → `grpcdriver.ErrNotFound` wrapping
  `port.ErrSessionNotFound` (id in message). Save(nil): client-side `sessnap.ErrNilSession`,
  zero RPCs. Recall miss: `found=false`, NEVER `NOT_FOUND`. Forget(missing): OK (idempotent).
  Blank RememberEntry key: `INVALID_ARGUMENT` (server wrapper pre-validates; the in-process
  store's own rejection stays conformance-tested). Failed RPC with a done caller ctx: rewrap
  `ctx.Err()` so `errors.Is(_, context.Canceled/DeadlineExceeded)` holds harness-side.
  Everything else: `"grpcdriver: <op>: %w"` — NO transient/permanent classification. Server
  wrapper: `ErrSessionNotFound`→`NotFound`, ctx errors→`Canceled`/`DeadlineExceeded`, else
  `Internal`.
- **D — Lint/arch: minimal.** grpcdriver has NO depguard rule (mirrors
  `internal/adapter/server`); the DAG test is unchanged (proto types never enter `engine/`).
  Only the new ENGINE package `storeconformance` gets the strict treatment ($gostd +
  `engine/port` + `engine/session`; deny `os`) plus the test-helper lint relaxation.
- **E — Resilience: deadline passthrough only.** No retries, no default deadline, lazy
  `grpc.NewClient` (fail-fast, no WaitForReady). If drivers ever need retries/breakers, the
  answer is a `driverresilience` DECORATOR (the llmresilience precedent), not knobs here.
- **Dial posture** mirrors `cmd/mecatui/client/client.go` (~60 lines DUPLICATED with a
  cross-reference comment — an internal adapter cannot import `cmd/`): loopback plaintext
  default, `RequireTransportSecurity()=!loopback`, pre-dial refusal of
  token+cleartext+non-loopback, CA pinning; grpcdriver adds mTLS (client cert). Composition
  knobs: `--driver-auth-token` (env `MECATL_DRIVER_AUTH_TOKEN`), `--driver-tls{,-ca,-cert,-key}`.
  Equal URLs share ONE lazy ClientConn (`internal/app`'s build-scoped `driverConns` cache;
  once-guarded close, so the session-store and memory-store teardown chains can both fold it).
- **Conformance as contract.** `engine/adapter/storeconformance.Run(t, newStore)` is the shared
  `port.SessionStore` suite (round trip via the public aggregate API incl. tool pairs /
  reasoning / media parts, lifecycle fidelity incl. awaiting+PendingAsk and a non-default stop,
  a ~5 MiB media-part snapshot — the size-contract probe, multi-session keying,
  miss-wraps-sentinel, overwrite, nil-save, isolation in BOTH directions: post-Save mutation of
  the original AND mutation of the loaded copy). Run matrix: memstore (the in-engine
  validation — deliberately NO separate self-test fake), jsonlstore, and grpcdriver-over-bufconn;
  `memconformance` (unchanged) additionally runs over the grpcdriver memory client.

**Phase D (DRIVERS.md) needs:** the driver pattern statement; the format-versioning rule; the
error table; the auth/trust posture; conformance-as-contract for third-party drivers; the
server-wrapper PROMOTION question (exporting them beyond `internal/` is a public-API commitment
made there, not implied by the current placement); a user-model driver flag (the user-model
store stays LOCAL in Phase B — deliberate deferral); a workspace/FS driver sketch.

### Child-session retention GC (issue #38 — `port.PrunableStore` + `internal/app/childgc.go`)

The delegation paths persist every child snapshot (`subagent-<callID>`,
`parallel-<callID>-<i>`, `team-<teamID>-<member>`) so InspectSubagent/InspectMember/`resume:`
work — but nothing ever deleted them, so a durable store grew without bound. Split mechanism
from policy:

- **Port delta — a SEPARATE OPTIONAL interface, never a widened SessionStore.**
  `port.PrunableStore` (`engine/port/store.go`): `List(ctx) []StoredSession` (ALL ids +
  last-modified times, unfiltered — policy is the caller's) and `Delete(ctx, id)`
  (IDEMPOTENT: unknown id = success, so List/Delete races are tolerated by construction).
  Discovered by type assertion; a Save/Load-only store is simply never swept.
- **Proto delta.** `SessionStoreService` gains `List(ListSessionsRequest) →
  ListSessionsResponse{repeated StoredSessionEntry{session_id, modified_at}}` and
  `Delete(DeleteSessionRequest) → DeleteSessionResponse` (message names are
  `ListSessions*`/`DeleteSession*` because the memory-store service in the same proto
  package already owns `ListRequest`/`ListResponse`). A driver that cannot enumerate
  answers UNIMPLEMENTED — the harness client maps it to the port sentinel
  `port.ErrPruneUnsupported` (wrapped), on which the sweeper logs ONE INFO ("store does
  not support retention; disabling child GC") and STICKILY disables further sweeps —
  graceful degradation without a recurring WARN, verified by test on both sides. The
  harness client maps a Delete NOT_FOUND to success.
- **Adapters.** memstore: `savedAt` map + injectable `WithNow` clock. jsonlstore: List
  decodes the REAL id from each `*.session.jsonl`'s latest snapshot line (`safeName` is
  NOT invertible — a filename-derived id would be mangled; cost is O(store bytes), fine
  for a startup/hourly sweep), `ModifiedAt` = file mtime; Delete removes BOTH files —
  tools sidecar FIRST, session file LAST, so a partial failure leaves the List entry
  (the session file) and the next sweep retries the pair instead of leaking an
  invisible orphaned `.tools.jsonl` (a PRE-EXISTING orphan sidecar is invisible to List
  and never swept — accepted). grpcdriver client+server wrapper round trip the seam;
  the wrapper type-asserts its backend (UNIMPLEMENTED for plain stores). Conformance:
  `storeconformance.RunPrunable` (mechanism only — including ModifiedAt STABILITY
  across reads, killing a stamp-Now()-at-List adapter that would neuter the age pass),
  run at all three sites.
- **Policy — composition only (`internal/app/childgc.go`).** An AGE pass (delete
  child-prefixed snapshots STRICTLY older than `ChildRetention`; exactly-at-cutoff is
  retained — pinned) then a per-family COUNT CAP (newest `ChildRetentionMaxPerFamily`
  survive, oldest-first past it deleted, equal `ModifiedAt` tie-broken by ID for
  deterministic eviction), both skipping ids with an in-flight run (`Service.IsLive` —
  the runs registry; pure read). HONESTY: `IsLive` knows TOP-LEVEL run ids only —
  engine-spawned children are never registered there (pinned by
  `TestServiceIsLiveDoesNotKnowEngineChildren`); their real protection is age horizon +
  snapshot freshness (children persist at their terminal, and a `resume:`d subagent
  RE-PERSISTS AT RESUME START so a long resumed run never goes stale mid-flight). The
  child prefixes are consumed from the engine's EXPORTED id-minting constants
  (`agent.SubagentSessionPrefix`/`ParallelSessionPrefix`/`TeamSessionPrefix`,
  `engine/agent/childregistry.go` — the same constants the minting sites derive from;
  drift-guard test pins the wiring; a `WithChildSessionPrefix`-style override DE-SCOPES
  those ids from GC). UNPREFIXED ids are NEVER touched — the load-bearing safety test
  (`TestChildGCMainSessionsNeverDeleted`) is mutation-verified (prefix gate removed →
  test fails). Best-effort: transient List failure = one WARN + skip;
  `port.ErrPruneUnsupported` = one INFO + sticky disable; Delete failures = one tallied
  WARN; one INFO summary only when something was deleted.
- **Placement + defaults.** `startChildGC` runs after Service construction (it needs the
  liveness predicate): startup sweep + ticker on one ctx-bound goroutine
  (`--child-gc-interval`, default 1h, 0 = startup-only). `--child-retention` default
  168h, `--child-retention-max-per-family` default 500; both zero = fully disabled (the
  zero-config/app.Config default, so embedded/test Builds are byte-identical unless
  opted in — mecatui's embeddedConfig passes the mecated defaults so a long-lived TUI's
  in-memory store stays bounded too). Durable-store-only in effect: the in-memory
  default never accumulates across restarts.

## Source drivers — skill + soul (Phase C1: `engine/tool/skillsource.go` + `engine/adapter/sourceconformance/` + `skills.FSSource`/`Activator`/`AssetMaterializer` + grpcdriver clients)

HARD REQUIREMENT honoured throughout: the `tool.SkillSource` port carries **NO path/dir/root/
file concept** — a skill crosses as a LOGICAL BUNDLE (identity + trigger metadata, instruction
body, payloads addressed by LOGICAL name). The FS adapter's path business
(`FSSource.AssetDir/AssetDirs`) is adapter-public NON-PORT API consumed only by composition.
The settled decisions, condensed:

- **A — Port home: `engine/tool` (skills); soul stays on `prompt.SoulSource`.** New types are
  stdlib-only (zero depguard/DAG churn; Phase-A MemoryStore precedent). No new Go port for soul —
  the existing consumer-local `prompt.SoulSource` is already file-agnostic; a gRPC soul source
  implements it directly.
- **B — The port.** `SkillMeta{Name,Description,Origin,HasAssets}` (Origin = a CLOSED admission-
  tier label set `explicit|project|user|driver`, NEVER a location; trust is enforced at source
  CONSTRUCTION in composition), `SkillAsset{Name,Size,Executable}`, sentinels
  `ErrSkillNotFound`/`ErrSkillAssetNotFound`, and `ValidSkillAssetName` — THE one shared
  logical-name validator (slash-separated, relative, no empty/`.`/`..` segments, no backslash,
  no NUL). SNAPSHOT semantics: ListSkills is stable for the source's life — **no watch/reload
  seam, deliberately** (the build-once trust-gate-completeness invariant depends on it).
- **C — Aux assets: real disk behind the existing Read/read-roots contract.** FS skills serve
  IN PLACE (zero copy; `FSSource.AssetDirs` = the old per-skill `skillReadRoots`). Driver skills
  materialize LAZILY (`skills.AssetMaterializer`, over the PORT only) into
  `<cacheBase>/<skill>/<logical-name>` on FIRST activation (per-skill once; never-activated =
  zero bytes; executable→0o755 else 0o644; caps 16 MiB/asset + 64 MiB/bundle on the ACTUAL
  bytes; name validation + post-Clean containment; failure = model-addressable activation error,
  NEVER a partial bundle). cacheBase via eager `os.MkdirTemp` at build (osfs opens read roots at
  workspace construction — a late-born root would be unreadable), canonicalized through
  `osfs.ResolveRoot`, RemoveAll folded into the catalog close. A pure-virtual overlay was
  REJECTED on a hard fact: Bash executes real OS processes — a virtual file can't be executed.
  A dedicated asset tool was REJECTED: it orphans every SKILL.md's relative-Read/script
  instructions (model-facing regression for zero interface gain).
- **K — Skill tool seam.** `skills.NewTool(metas []tool.SkillMeta, act Activator)`;
  `Activator.Activate(ctx,name) → Activation{Body, BaseDir}` (BaseDir "" omits the
  Base-directory header block). `NewSnapshotActivator(*FSSource)` (FS, byte-identical — the
  golden `TestFSSkillActivationByteIdentical` pins Execute output AND Spec().Description
  byte-for-byte against the pre-seam rendering) and `NewSourceActivator(tool.SkillSource,
  *AssetMaterializer)` (driver; caches body+BaseDir after first success; failures NOT cached —
  the materializer's once caches deterministic rejections). descriptionPreamble / header strings
  / truncation are UNCHANGED — editing them is a defect against the C1 plan.
- **H — Wire + client discipline.** `SkillSourceService{ListSkills,GetSkillBody,
  ListSkillAssets,ReadSkillAsset}` (unary; rides the 64 MiB ceiling; origin is a string
  passthrough, no proto enum) and `SoulSourceService{LoadSoul}`. Server wrappers
  (`NewSkillSourceServer(tool.SkillSource)` / `NewSoulSourceServer(prompt.SoulSource)`)
  pre-validate blank names and logical names (`ValidSkillAssetName` → `INVALID_ARGUMENT`,
  never content); unknown skill/asset → `NOT_FOUND` → the client wraps the sentinels (name in
  message); ctx rewrap as Phase B. Client ListSkills is DEFENSIVE: drop blank names, de-dup
  first-wins, name-sort, re-truncate descriptions to `skills.MaxDescriptionBytes` (exported),
  normalize unknown origins → `SkillOriginDriver`. The soul client RE-VALIDATES via the
  extracted `soul.ValidateBody` (the single body discipline: raw byte cap, trim, injection
  scan, fence integrity) — a driver is never trusted to sanitize; runtime fault = ("", nil) +
  WARN (fail-soft contract); `Probe` is the build-time FATAL reachability check.
- **J — Soul selection.** `--soul-source-url` ⟂ `--soul-file` (validateDriverConfig);
  `--no-soul` wins; the driver OCCUPIES the user slot (`selectDriverSoul` — shadows a project
  soul exactly like a present user soul; an empty/rejected driver body falls through to the
  project soul); provenance `soulDriver` (proto `SOUL_PROVENANCE_DRIVER`, additive), Trusted
  true; the `soul:apply` gate runs unchanged BEFORE the driver branch; drift baseline SKIPPED
  (one INFO line; `--soul-strict`/`--approve-soul` are documented no-ops for this provenance).
  Build-time probe failure FATAL (in `buildEngine`, conn close folded into the teardown chain);
  per-session turn-0 Load fail-soft.
- **Composition reshape.** `resolveSkillSeam(ctx,cfg,agentReg)` replaces `resolveSkills` (FS
  branch: `NewFSSource` over `ResolveSources(skillResolveOptions(cfg))`, narration verbatim;
  driver branch: dial + ONE ListSkills snapshot, both FATAL on fault — explicit config =
  loud-misconfig). `catalogAssets` now carries `skills []tool.SkillMeta` + `skillActivator` +
  `skillIndex` (name→body preload: full for FS, LAZY def-referenced-only for the driver) +
  `skillReadRoots` (name + ALL workspace-constructor threading KEPT; only the derivation moved
  into the seam). `resolveSkillIndex` and skilldraft's `skillReadRoots()` are DELETED;
  `buildSubagentTool`/`buildTeamWiring`/`applyTeamConfig` take the index as a param.
  `skillValues(metas, idx)` projects back to `[]skills.Skill{Name,Description,Body}` for the
  two legacy consumers (skillSnapshot, NewDirDrafter novelty input — signature kept).
  `activeSkillDirs` stays CONCRETE (quarantine-overlap validation is inherently FS business;
  driver source ⇒ empty active dirs ⇒ the check trivially passes, documented).
- **Conformance as contract.** `sourceconformance.RunSkillSource` (driven by the exported
  canonical `Fixture`: text+executable assets / asset-less / multi-segment logical name;
  subtests: list-matches-fixture incl. sorted/unique/HasAssets/Origin-non-empty,
  list-deterministic, body round-trip, sentinel misses, asset name/size/executable + content
  round-trips, asset-less, invalid-name-never-content) runs over the in-memory
  `NewFixtureSource` self-test, `skills.FSSource` over a written-out TempDir tree, and
  grpcdriver→bufconn→server-wrapper. `RunSoulSource` (round-trip, trim, empty/whitespace/
  fence-breakout fail-soft) runs over `soul.Store` (temp file) and the wire client (verbatim
  fake server — proving the CLIENT's re-validation).

**Phase D notes (additions to the Phase-B list):** the construction-time-trust rule (Origin is
observability; admission is gated where sources are CONSTRUCTED — an untrusted workspace's
project tier is never built); the logical-name grammar (verbatim from `ValidSkillAssetName`);
the no-watch/snapshot decision and its trust-gate rationale; the memfs/virtual-overlay
rejection rationale (Bash executes real processes — drivers must materialize).

## Source drivers — agent defs + commands (Phase C2: `engine/tool/agentsource.go` + `engine/prompt/commandsource.go` + `agents.FSSource` + grpcdriver clients)

- **AgentDef moved engine-side WHOLESALE, minus the locator.** `tool.AgentDef` is the old
  adapter value object minus `Path`, plus `Origin tool.AgentOrigin` (a CLOSED tier label set
  mirroring `SkillOrigin` — deliberately NOT a shared type: a THIRD origin-bearing seam is the
  extraction point, not before). The agents adapter keeps `type AgentDef = tool.AgentDef` /
  `type AgentMCPServer = tool.AgentMCPServer` aliases so every literal/signature compiles
  unmodified (no test sets `.Path`, so the alias strategy is invariance-clean). The port is
  `tool.AgentDefSource{ListAgentDefs}` — SNAPSHOT semantics (per-def child engines are baked
  once; the trust-gate-completeness invariant depends on resolve-once).
- **The locator became a NON-PORT detail channel.** Sources return `agents.Discovered{Def,
  Detail}` (`"<label>: <path>"` FS, `"driver: <target>"` driver); `Registry` retains the map
  (`NewRegistryDiscovered`; the one-arg `NewRegistry` is KEPT, detail-less, for tests) and
  exposes `Detail(name)`. The old 14 `def.Path` diagnostics sites became: pure-def helpers →
  `"origin", string(def.Origin)`; registry-loop sites (buildAgentSubagentEngines ×3,
  buildMemberEngine ×3) → `"source", reg.Detail(def.Name)`. Full paths remain ONLY in the
  one-time discovery `SkipError` lines (adapter-internal). NOTE: the C2 plan tabled 12 sites;
  reality had 14 (buildMemberEngine's skill-preload + adopts-def lines mirror the
  subagent-engine pair) — the same rule was applied to all.
- **HEADERS DECISION (§0.5).** `AgentMCPServer.Headers` is SECRET-SHAPED (`Authorization`
  etc.) and CROSSES the port + wire anyway: dropping it would functionally regress vs file
  defs (which carry plaintext auth headers on disk today); the wire is protected (driver
  dials refuse ALL non-local cleartext); and it never leaks downstream — `defMCPTools` logs
  names/urls/counts, never headers, `AgentInfo` carries no mcpServers. Guarded by
  `TestDefMCPHeadersNeverLogged` (an arg-scanning diag sink + a sentinel header value across
  the def-engine build — it exercises the WARN/failure branches; a successful inline
  connect needs a live MCP server, out of scope offline).
- **HOOKS = HARNESS-SIDE SHELL (trust framing).** A def's `hooks:` map executes through
  `hookexec` as UNGATED shell on the HARNESS HOST (every scoped lifecycle phase, no
  permission ask) — strictly stronger than the skill driver, whose payloads still ride the
  permission-gated Bash path. **A compromised agent-source driver executes arbitrary shell
  on the harness host via def hooks; treat it as harness-equivalent infrastructure** (echoed
  in docs/usage.md and the proto `AgentDef.hooks` comment). `resolveAgentSeam`'s driver
  branch narrates every driver def carrying hooks once at build ("agent def carries
  lifecycle hooks (harness-side shell)", names only — never hook values).
- **Driver client discipline** (`grpcdriver.NewAgentSource(conn, AgentOptions{Diagnostics})`):
  TRIM names before every use (dedup key AND stored name — the FS parser trims; the wire
  must not be weaker), drop blank names, de-dup first-wins, sort,
  `singleLine`+`TruncateRunes(desc, tool.MaxAgentDescriptionBytes)`,
  `TruncateRunes(body, tool.MaxAgentBodyBytes)` (the canonical caps moved engine-side next
  to the port; the adapter keeps lowercase internal aliases — the
  maxDescriptionLen=MaxCommandDescriptionRunes pattern; the proto comments reference them BY
  NAME and a RunAgentSource subtest asserts every listed def respects both), COUNT CAPS
  mirroring C1's asset caps (maxAgentDefs=1024, per-def hooks 32, tools/disallowed/skills
  256 each, mcp_servers 64 — over-cap drops THE DEF with a WARN naming it, fail-soft per
  def because defs are independent, never a fatal snapshot error), hooks/headers
  re-normalized via the exported `agents.NormalizeHooks/NormalizeHeaders` (the same helpers
  the frontmatter parser uses), `Origin` stamped `AgentOriginDriver` UNCONDITIONALLY (wire
  origin is driver-side observability only).
- **ONE resolution per build (the drift-class guard).** The registry used to resolve THREE
  times (buildEngine→catalog, the ListAgents snapshot, buildTeamWiring) — the third firing of
  the per-session-drift class. `resolveAgentSeam` (driver branch: dial fatal, ONE
  `ListAgentDefs` fatal, `NewRegistryDiscovered` with `driver: <target>` details; FS branch:
  `resolveAgentRegistry`, now `NewFSSource`+`NewRegistryDiscovered`, narration byte-identical
  incl. the untrusted-workspace WARN) runs ONCE in Build; buildEngine/buildCatalog, the
  snapshot, and buildTeamWiring/applyTeamConfig all take the one registry. Guarded by
  `TestBuildResolvesAgentRegistryExactlyOnce` (a counting bufconn-style server asserting
  exactly ONE `ListAgentDefs` RPC across a full teams+parallel Build).
- **AGENTSCONVENTIONAL ASYMMETRY (§1.F).** `--agent-source-url` is FATAL with `--agents-dir`
  (explicit local source vs driver — one source per seam) but NOT with
  `--agents-conventional`: that flag is ON by default and inert, so the driver branch simply
  does not construct conventional sources and narrates "conventional discovery superseded by
  --agent-source-url" (failing every default deployment would be wrong). Deliberately
  asymmetric vs skills, whose conventional discovery is opt-in and therefore exclusivity-
  checked.
- **Commands: consumer-local LIVE port, deliberately NO latching.** `prompt.CommandSource`
  (`ListCommands` metadata-only + `CommandBody` returning the RAW template, `found=false` for
  unknown — normal, never an error) lives in `engine/prompt` (the SoulSource precedent);
  `prompt.SourceExpander` implements `CommandExpander`+`CommandLister` reusing the SAME
  `parseCommand`/`stripFrontmatter`/`substitute` internals as `DirCommandExpander` (zero
  change to those types — byte-parity pinned by `TestSourceExpanderByteParityWithDirExpander`).
  `ValidCommandName` is the ONE invocation-grammar validator; `MaxCommandDescriptionRunes`
  (80) is exported and `maxDescriptionLen` aliases it. The driver client
  (`grpcdriver.NewCommandSource`) is consulted LIVE per call: runtime faults FAIL SOFT (WARN
  via injected Diagnostics + `(nil,nil)`/`("",false,nil)` — `MultiExpander.List` aborts the
  whole palette walk on a child error, so a transient blip must not propagate, and a fault
  must never latch a command "missing"); `NOT_FOUND` → normal pass-through; ctx
  cancel/deadline and the server's blank-name `INVALID_ARGUMENT` still surface as errors;
  build-time `Probe` (one ListCommands) is FATAL. The probed client is STASHED on the
  unexported `Config.commandSource` (buildCommandExpander runs per session — it must not
  re-dial/probe); composition order is `dirExp, sourceExp, mcpExp` (file shadows driver;
  COMPOSES, no exclusivity rule — `validateDriverConfig` has the agent rule only). One
  build-fact INFO ("slash-command driver source ENABLED, target=…") rides
  `logBuildConfigFacts`.
- **Conformance as contract.** `RunAgentSource` (canonical `AgentFixture`: minimal /
  fully-loaded incl. hooks+skills+limits+model+provider+permissionMode+color / MCP-bearing
  with one reference + one inline-with-headers; authored to round-trip the frontmatter
  parser; subtests: list-matches-fixture deep-equal-minus-Origin + sorted/unique/
  Origin-non-empty, list-deterministic) runs over the in-memory `NewAgentFixtureSource`
  self-test, `agents.FSSource` over a written-out TempDir tree, and grpcdriver→bufconn.
  `RunCommandSource` (canonical `CommandFixture`; list grammar-valid/sorted/unique/
  descriptions, RAW body round-trip with frontmatter intact, unknown→`("",false,nil)`,
  list-stable-over-fixed-backend — the PORT is live, the fixture fixed) runs over
  `NewCommandFixtureSource` and grpcdriver→bufconn. Deliberately NO FS row for commands:
  `DirCommandExpander` is the workspace-tier surface, not a `CommandSource` implementation.
- **Model-facing invariance.** `agentSnapshot`'s field set/output is pinned against literals
  (`TestAgentSnapshotLiteralPin`) and the Subagent roster tail is pinned byte-for-byte against
  the pre-change rendering (`TestSubagentRosterByteIdentical`); `engine/prompt/command_test.go`
  and the server `agents_test.go`/`capabilities_test.go` are untouched and green.

## TUI — `cmd/mecatui/` (see `docs/tui.md`)

**Upstream textarea word-backward hang workaround** (`cmd/mecatui/ui/textarea_guard.go` + the two
`default:` guards in `update.go`'s `onRunningKey`/`onIdleKey`): bubbles v2.1.0's
`textarea.wordLeft()` has an unbounded loop — alt+left/alt+b with only whitespace strictly before
the cursor (the subtle case: `" foo"` at the origin) spins forever, wedging the update goroutine
at 100% CPU; this froze a live session. The guard predicts the hang (`wordLeftWouldHang`) and
swallows the keypress (a semantic no-op: upstream's intended "no word to the left" is don't-move).
Upstream refs: charmbracelet/bubbletea#1652; bubbles PRs #948 (incomplete) / #959 / #987
(unmerged). Removal condition: once on a bubbles release whose `wordLeft` has an in-loop boundary
guard, delete `textarea_guard.go`, the two call sites, and this paragraph. The textinput overlays
need nothing (textinput is upstream-guarded), but any FUTURE textarea-bearing overlay needs the
same one-line guard in front of its `textarea.Update`.

**First-encounter workspace-trust prompt** (WORKSPACE-TRUST Phase 2c, `cmd/mecatui/trust.go`) is
a **pre-TUI** prompt in this composition root — NOT in `ui/` (it imports `internal/app`'s
`ResolveTrust`/`HasProjectAuthority`/`RememberTrust`, allowed here); it gates the embedded server
only, non-TTY fails safe to untrusted, and the prompt outcome feeds `cfg.trustProject` so
`app.Build` does not re-resolve. The workspace-trust feature (Phases 0–2c) is complete; `mecated`
stays declarative (never prompts/writes `trust.yaml`).

**`/models` picker** (multi-provider Phase 0, S4) is the FIRST *selecting* overlay (cursor +
enter-to-select, mirroring the mcp.go resource picker — every other inventory overlay is
read-only/esc-only): it lists `ListModels` grouped by provider and, on select, **persists** the
choice + applies it to the **NEXT** `CreateSession` (apply-on-next-create — it does NOT re-route
the live session). The model selection threads **proto-free** as
`client.ModelSelection{ProviderID,ModelID}` through `ui` → `sessionAdapter` →
`client.CreateSession` (the SINGLE proto-build point — `ui` never sees the proto request);
`client.ModelInfo`/`ModelsMsg`/`ModelLister` are the proto-free picker surface (mirror
`usermodel`), so `client` stays proto-only (NO `internal/` import). Persistence lives in the
**`cmd/mecatui` MAIN** (`state.go`, mirroring `trust.go`), NOT `client`/`ui`: a machine-written
client-side state file `$XDG_STATE_HOME/mecatui/models.yaml` (state, not config — added
`xdgconfig.UserStateDir`; the settings-vs-state split, like `trust.yaml`) — a per-workspace map
(realpath-keyed) + a global `default` (a new repo inherits the last choice), atomic `0o600`
temp-rename write, fail-soft read. Connect SEQUENCES `ListModels` → reconcile → `CreateSession`:
a persisted selection whose PROVIDER is no longer available is cleared to the server default for
that run (loud notice, state file untouched) BEFORE the create carries it — so a removed key never
hard-fails connect with `InvalidArgument`. The reconcile rule is **PROVIDER-level only** (issue
#41): a saved model absent from the snapshot is KEPT and sent verbatim — the boot snapshot is the
EMBEDDED catalog floor until the async live refresh lands (which the connect race always wins),
and the server is the model-string authority (an unknown provider errors loudly; a non-empty
`model_id` on a known provider is passthrough). The exact-row clear was an implementation drift
that silently downgraded boot sessions to the server default. The companion FALLBACK leg
(`createSessionCmd` + `connectFallbackMsg`): a connect-time create whose non-zero selection the
server REJECTS retries ONCE with the zero selection — success applies the session like
SessionReadyMsg, clears the bad selection for the run, and sets a loud warning naming the
rejected model + the error (state file untouched); both failing keeps the unchanged fatal path
with the ORIGINAL error. `/models` is gated on `caps.ModelSelection &&
m.deps.Models != nil` (same mechanism as `/soul`/`/usermodel`); palette-only open (NO `ctrl+m` —
it collides with enter); fixed builtin order now `clear, help, mcp, agents, team, skills, soul,
usermodel, models`.

**Unified `ctrl+a` agents overlay + fleet footer** (Subagent delegation-tool watchability, Package C) is
CLIENT-ONLY — built purely from the relayed `subagent.*`/`team.*` projection, NO new server
event/field (the F2 finding: the three `subagent.*` events already carry ChildID/goal/tool
name/error/count/usage/stop/duration). Two pieces:
- **Fleet state** (`conversation.subagentFleet`, keyed by `ChildID`): a flat `[]subagentLane`
  fed by `applySubagent` ALONGSIDE the inline Subagent-card routing (the inline card keys on
  `ParentCallID`, the fleet on `ChildID` — so two children of one Subagent call are distinct rows).
  Part of the conversation, so `/clear` drops it. `subagentFleetCounts`/`hasSubagents` drive the
  footer + the Subagents tab.
- **Fleet footer segment** (`footer.go` `subagentFooter{Full,Medium,Compact}`, mirroring the team
  segment): `⛭ subagents N◐ M✓ · ctrl+a`, shown once ≥1 subagent started; `view.go` `fitFooter`
  composes it into an "agents prefix" (team segment + fleet segment via `joinSeg`) that sheds
  before the ctx meter. No-subagent footer is byte-identical to before.
- **Unified overlay** (`agents_overlay.go`): ONE `ctrl+a` surface with two tabs (`agentsTab`
  Subagents|Teams). The container open flag + Teams-tab state STILL live on `m.team` (teamState) —
  the existing team overlay became the Teams tab verbatim (`renderTeamsTab` dispatches to the
  unchanged `renderTeamRoster`/`Focus`/`Tasks`/`Findings`; the standalone `renderTeamOverlay` is
  gone, the container owns the `centerCard` framing now). The Subagents tab (`subagentState`,
  `renderSubagentRoster`/`Focus`) reuses the team roster's window/clamp/focus patterns; rows are
  metadata-only with a `#<hash>` ChildID disambiguator and the latest child tool as the liveness
  signal. `tab` (`keys.NextTab`) switches tabs (only from a roster); `enter` focuses; `esc` steps
  back then closes. **Default tab is context-sensitive** (`preferredAgentsTab`, tested in isolation):
  Teams when a team is LIVE, else Subagents when subagents ran, else the available tab. The newer
  Subagent/Team terminal stop reasons (`budget`/`structured_output`/`no_progress`/`max_*`) ride the
  string `stop` field and map to compact labels in `subagentStopLabel` (+ a ✓/✗ glyph split in
  `subagentLaneGlyph`: cap-family ✓, error/cancel-family ✗). Help/zero-state `ctrl+a` row is no
  longer teams-gated (subagents are always available via Subagent). Gauntlet #7 holds: the focus pane
  shows redacted chips only, never child content.

**The `parallel.*` observability family** (`Parallel` fork-join tool watchability) is the
THIRD delegation family alongside `subagent.*` and `team.*`. It was chosen as a DEDICATED
family (Option A) over consolidating into the subagent/team families — see
`.scratch/task-research/REVIEW-event-consolidation.md`: the only genuinely-shared part (the
child lifecycle shape) is shared where safe, while Team remains intentionally
fuller-than-metadata because a crew is meant to be watched. The three families share a
LIFECYCLE (parent call id, child/branch/member identity, tool name/error/count, usage,
stop, duration) but differ in AGGREGATION shape: subagent = flat fleet, parallel = fan-out
GROUP (join + winner + preserved fork paths), team = coordinating roster (bounded member
previews + tasks + findings + mailbox). **TRIP-WIRE: a 4th delegation family is the point
to extract a shared `ChildActivity` value object — not before** (recorded in the
`session.event.go` doc-comment above the three payloads).

- **Server** (`engine/session/event.go`): `EvParallelStart` / `EvParallelBranch` /
  `EvParallelEnd` + `session.ParallelPayload` (string-passthrough like `subagent.*`; a
  `ParallelEventKind` discriminates the per-branch `branch_start`/`branch_tool`/`branch_end`
  transitions). It is METADATA ONLY — no branch message text, tool args, or result bodies; it
  carries fork-root PATHS (handles already in the result text, not branch content). `Event.Parallel`
  mirrors `Event.Subagent`/`Event.Team`.
- **Emission** (`engine/agent/parallel.go`): `ExecuteWithParent` (the `childCapableTool` seam the
  dispatcher prefers — emit was already plumbed) brackets the run with `parallel.start`/`parallel.end`
  and each branch with `branch_start`/`branch_end` via a small `branchEmitter` carrier (the plan's
  Q1 carrier: `emit` + `parentCallID`, nil-safe so the plain `Execute` path is byte-identical).
  Per-branch tool activity REUSES `drainChildObserved` (was `drainChild`) with a per-branch
  TRANSLATION closure (`branchEmitter.branchTool`) that RE-TAGS its redacted `subagent.tool` emit
  into a `parallel.branch{branch_tool}` — copying only already-redacted fields, opening NO new
  content path. `parallel.end.Winner` carries the REAL `branchResult.index` (-1 for join=all /
  none-succeeded); run Stop is the winner's stop for first/judge and omitted (zero) for all (plan
  Q3). `branchResult.usage` was added so `sumBranchUsage` can carry the run-total.
- **Proto + mapper**: `Event.parallel = 14` + a new `Parallel` message (additive, non-breaking) +
  `toProtoParallel` (mirrors `toProtoSubagent`).
- **Gauntlet #7**: enforced by a STRUCTURAL test (`TestParallelPayloadHasNoContentFields` — an
  allow-list of metadata field names; trips if a content-shaped field appears) AND a BEHAVIORAL
  sentinel test (`TestParallelNoContentLeakBehavioral` — a branch whose args/result/message carry a
  canary; the canary never appears in any emitted `parallel.*` field). Model-facing e2e:
  `TestParallelEmitsObservabilityStreamAll` / `TestParallelEmitsWinnerJudge` (winner at a non-zero
  index — off-by-one guard) / `TestParallelEmitsWinnerFirst` / `TestParallelBranchErrorRepresented`
  (a fork-failed branch still emits a coherent `branch_end` with `Failed=true`).
- **Client/UI** (`cmd/mecatui`): `client.ParallelMsg`/`ParallelKind` + `applyParallel` build GROUPED
  `parallelGroup`/`parallelBranch` state (deterministic, insertion-ordered, no map-iteration flake);
  a third `Parallel` tab in the unified `ctrl+a` overlay (`Subagents | Parallel | Teams`) renders the
  grouped roster (join + branch counts + winner) → ONE-level group focus (branches inline with chip
  traces, the winner highlighted, the preserved fork path, an honesty note). It folds into the fleet
  footer (a `⑂` segment). Default-tab precedence (plan Q5): `teamLive > parallelLive > haveSubagents >
  haveParallel > haveTeam > Subagents`. Rendered from relayed Events ONLY (no internal/proto import).
