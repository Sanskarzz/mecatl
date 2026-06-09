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

## Domain — `internal/session/` (lifecycle recovery)

A turn always drives the `Session` aggregate to a terminal state within one
`Engine.Run`; the engine never recovers it. Two intention-revealing seams
re-enter the loop on a reused session (a `failed` session is never resumable
from either):

- **`Reopen()`** — `completed → idle` only. Clears stop + pending, resets per-run
  `Counters`, preserves history. The clean-end-of-run continuation seam.
- **`Interrupt()`** — `cancelled → idle` only. Mirrors Reopen's reset but is a
  SEPARATE method because its precondition (`StateCancelled`) and its
  history-repair invariant differ. A turn cancelled mid-dispatch (ctx cancel
  AFTER `RecordAssistant` but before `RecordToolResults`) leaves the trailing
  assistant message with `ToolCall`s that never got a result — a dangling
  `tool_use` (Anthropic) / `function_call` (OpenAI) that both providers 400 on at
  replay. The private `closeOutInterruptedTurn()` finds the LAST assistant
  message, collects the `CallID`s already answered by the `RoleTool` messages
  that follow it, and appends one `NewToolError(call.ID, "tool call interrupted
  by cancellation")` per unanswered `ToolCall.ID` (in `ToolCalls` order) via the
  same `Conversation.Append(NewToolMessage(...))` path `RecordToolResults` uses.
  `IsError` here means *cancellation*, not a real tool failure. It is idempotent
  and a NO-OP for clean shapes: an assistant with no tool calls, the
  dangling-trailing-user shape (cancel before the first token — LEFT AS-IS,
  benign for both providers), and a turn whose results all landed before the
  cancel. Partial results are honoured (only the missing `CallID`s are
  closed out).

The service's `loadAndReopen` (shared by `LoadSession`/`LoadSessionWithMCP`, and
upstream of every `StartRunContent` run-entry) branches on state: `completed →
Reopen`, `cancelled → Interrupt`, then re-persists the recovered snapshot;
`failed` is returned as-is so the next run surfaces the illegal transition. This
is what fixes the wedge where an interactive session that was cancelled mid-turn
rejected every later prompt with `RecordUserPrompt from "cancelled"`. Regression:
`TestStartRunContentRecoversCancelledSession`; adapter-level orphan guards:
`anthropic.TestRequestNoOrphanedToolUseAfterInterrupt`,
`openai.TestRequestNoOrphanedFunctionCallAfterInterrupt`.

---

## Domain — `internal/governance/`

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
**Ask⇒withhold** — no interactive build-time gate).

## Domain — `internal/prompt/`

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

## Port — `internal/port/`

The PORT interfaces the loop consumes (`LLMProvider`, `SessionStore`, `HookRunner`,
`PermissionPolicy`, `Clock`, `Logger`, `EventSink`). `PermissionPolicy.Evaluate` carries the
session as a READ-ONLY `tool.WorkspaceReader` (Root+Read+Stat — issue #13) so file-based
config resolves per-session against that root without a mutate-capable handle; `ws` may be nil
(child/member engines with no resolver).

## Application — `internal/agent/` (subagent workspace policy)

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
exactly TWO diagnostic lines" invariant holds); the final/extractive attempt carries a DISTINCT
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
provider-agnostic route is the bounded nudge + the per-model persistence prompt (`agencyDelta`,
composition layer). `EvNoProgress`/`StopNoProgress` are STRING passthroughs on the wire (proto
`type`/`stop` are strings, not enums), so no proto regen was needed; mecatui renders
`EvNoProgress` as a muted notice and `StopNoProgress` as a `stopped · no progress` footer label.

**Token budget — the shared loop-level ceiling (`StopBudget`).** `agent.Deps.MaxRunTokens`
(0 = disabled) is a per-RUN cumulative token ceiling checked at the turn BOUNDARY in
`Engine.drive` (Step 2, after the existing `sess.StopReason()` and `ctx.Err()` checks, before
`BeginTurn`) against the run's accumulated `session.Usage` via `Usage.TotalTokens()`
(input+output; cache tokens excluded — `CacheReadTokens` is a subset of `InputTokens`,
`CacheWriteTokens` is a side cost). When `total.TotalTokens() >= MaxRunTokens` the loop ends via
`terminateComplete(…, session.StopBudget, …)` — a NON-error CLEAN terminal (completed path,
Reopen-recoverable), so it mirrors `StopNoProgress` exactly. The boundary check means an
in-flight turn always COMPLETES (no mid-stream abort → no-replay-after-first-chunk holds); a turn
whose usage massively overshoots still finishes, then the budget trips before the next turn. It
is NOT a `port.LLMRequest` field (the request stays provider-neutral) — it is composition-tunable
(`app.Config.MaxRunTokens` → `--max-run-tokens`) and INHERITED by every engine via
`engineDepsForProvider`; `childEngineDepsForProvider` delegates there and does NOT clear it, so
Subagent/team-member/lead/Parallel children inherit the same ceiling. `StopBudget` is the
AGENT-TEAMS-SPIKE's named "Deferred 4A" brake, now landed once for every delegation path. It is a
STRING passthrough on the wire (`session.StopBudget = "budget"`, no proto enum). Guards:
`agent.TestBudget*`, `session.TestStopBudgetIsCleanReopenableTerminal`,
`server.TestServiceBudgetSurfacesAndReopens`, `app.TestMaxRunTokensPropagatesToParentAndChild`.

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

**Subagent structured output (`output_schema` + `SubmitResult` + bounded validation-retry).** When
`taskArgs.OutputSchema` (a model-authored JSON schema) is present, the child is given a synthetic
`SubmitResult` tool (`internal/agent/structuredoutput.go`) whose PARAMETERS ARE that schema,
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
…) → success. On every non-error terminal the result text carries an `agentId: <childID>` trailer
(mirroring `renderTeamResult`'s Team-id line) so the parent MODEL can DISCOVER the deterministic
child id (`subagent-<callID>`) — the runtime-discoverability axis: the id must be where the model
reads it, not only on the client-only `subagent.*` events. It is INFORMATIONAL this round (no
`InspectSubagent` tool yet — R2), pre-positioning the seam. Guard:
`agent.TestSubagentAgentIdTrailerInResultText` (+ the existing subagent tests updated from exact-equality
to substring assertions for the trailer).

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
catalog's read-only **`InspectMember`** tool (`internal/agent/teaminspect.go`) loads ONE member's
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
`session.TeamMemberDisposition` (domain, plain strings) bridged in `internal/agent`
(`projectTeamDispositions`) exactly like the tasks/findings bridges (session never imports
`internal/team`), maps to the proto `TeamMemberDisposition` (`bool stopped` + closed-enum
`TeamMemberStopReason` — `(done, error)` non-representable) via `toProtoTeam` /
`toProtoTeamMemberStopReason`, and the mecatui client decodes it to `client.TeamMemberDisposition`
→ the lane's `stopped`/`stopReason`. It is CONSUMED BY THE CLIENT OVERLAY, NOT the model (the
model already gets `[STOPPED]` in the lead's report via `joinTeamFallback`), so the overlay renders
`✗ stopped — <reason>` distinctly from `✓ done` instead of recomputing "done" and contradicting the
supervisor. It is a supervisor verdict (closed enums, `Name` already on the roster), kept OFF the
`EvTeamMember` redaction channel exactly like the tasks/findings discipline.

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
  the "loop emits exactly TWO diagnostics lines" invariant holds), and runs a defensive
  `ValidateToolPairing(compacted)` between `Compact` and `ReplaceHistory`. `Session.ReplaceHistory`
  itself now rejects an unpaired slice (aggregate-level guard), so the existing ReplaceHistory-reject
  branch catches pairing failures for free.

Coverage: unit tests on both compactors (orphan-at-tail-head, tail-all-tool-results,
multiple-consecutive-leading-orphans, cascade-orphan), `ValidateToolPairing` table test, and an
END-TO-END `TestCompactionThroughLoopNeverOrphans` that drives the real loop + real
`HeuristicCompactor` with a mockllm tool-call script and a tiny `ContextWindowTokens`, asserting
the final history is pairing-valid and the session did NOT reach `StateFailed`.

**Deferred follow-up: `failed` is still not recoverable.** This PR does NOT make `StateFailed`
resumable (`Reopen` stays completed-only; `Fail()` carries a cross-reference comment). Rationale:
the pairing fix REMOVES the trigger that bricked sessions here, so automatic failed-recovery is no
longer urgent; making `failed` recoverable is a larger lifecycle change tracked separately.

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
`internal/app`; the three skills callers — `registerSkills`/`resolveSkillIndex`/`activeSkillDirs`
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
`internal/agent`, and (via the `server` adapter) `contracts/gen`; nothing imports it except the
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
sub-agent tools (the build-time per-session catalog is core-tools-only and could not spawn
Subagent/Team) by building a per-session Subagent tool (+ in-catalog Team tool under `--enable-teams`)
inside `sessionEngineFactory` over the SAME `buildSubagentTool`/`buildTeamWiring` builders (no
per-session-catalog drift), wired to the session provider as parent; the Subagent tool's inline-MCP
close folds into `SessionEngineResult.Close` (torn down by `CloseSession`/`Service.Close`),
bounded by `MaxSessionEngines`. The registry NEVER leaves composition — the child engine gets a
bare `port.LLMProvider`. **DEFERRED:** the standalone gRPC `CreateTeam` RPC stays on the default
provider (no per-CreateTeam selector); `ListAgents`/`AgentInfo` provider surfacing (no proto
change).

**Server-global MCP on every session (bug #3 fix, `sessionEngineFactory`):** the
per-session catalog mounts the SERVER-GLOBAL MCP tools (`cfg.MCPServers` + ToolHive — the
same tools `buildCatalog→registerMCP` mounts on the main engine), NOT just core + client
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
build-time `registerMCP` GLOBAL mount → *"server-global MCP: skipped duplicate tool
name(s) (a server advertised a name already registered): <names>"* (a within-/across-global
defective-server condition). These are composition logs, so the loop's two-line
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

## TUI — `cmd/mecatui/` (see `docs/tui.md`)

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
a persisted selection whose provider is no longer available is cleared to the server default for
that run (loud notice, state file untouched) BEFORE the create carries it — so a removed key never
hard-fails connect with `InvalidArgument`. `/models` is gated on `caps.ModelSelection &&
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
redacted per-tool child lifecycle) is ALREADY shared in `agent.drainChildObserved` (the single
redaction chokepoint), so consolidation would couple two shipped contracts to absorb a third
for near-zero saving. The three families share a LIFECYCLE (parent call id, child/branch/member
identity, tool name/error/count, usage, stop, duration) but differ in AGGREGATION shape:
subagent = flat fleet, parallel = fan-out GROUP (join + winner + preserved fork paths), team =
coordinating roster (tasks + findings + mailbox). **TRIP-WIRE: a 4th delegation family is the
point to extract a shared `ChildActivity` value object — not before** (recorded in the
`session.event.go` doc-comment above the three payloads).

- **Server** (`internal/session/event.go`): `EvParallelStart` / `EvParallelBranch` /
  `EvParallelEnd` + `session.ParallelPayload` (string-passthrough like `subagent.*`; a
  `ParallelEventKind` discriminates the per-branch `branch_start`/`branch_tool`/`branch_end`
  transitions). It is METADATA ONLY — no branch message text, tool args, or result bodies; it
  carries fork-root PATHS (handles already in the result text, not branch content). `Event.Parallel`
  mirrors `Event.Subagent`/`Event.Team`.
- **Emission** (`internal/agent/parallel.go`): `ExecuteWithParent` (the `childCapableTool` seam the
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
