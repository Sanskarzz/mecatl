# Spike: Headless Agent Teams for mecatl

> Status: **spike / design proposal** (not yet wired). Author pass: 2026-05-30.
> Companion prototype: `internal/team/` (coordination kernel + tests).
> Research basis: `docs/harnesses/02`, `05`; live survey of Claude Code subagents
> & agent teams, OpenAI Agents SDK, Goose recipes (see "Sources" at end).

## 1. What we're trying to add, and why

mecatl today has two delegation tools, both one-shot and context-isolated:

- **`Task`** (`internal/agent/subagent.go`) — one read-only explorer child, shared
  workspace, drained internally, returns only final text.
- **`Fork`** (`internal/agent/fork.go`) — N parallel children, each in an isolated
  forked workspace, drained internally, joined into one summary, no auto-merge.

Both are **agents-as-tools** (the OpenAI SDK term): the parent calls a child, the
child's noise stays inside, only the result folds back. There is no way for two
running agents to **see a shared work list**, **message each other**, or **self-
coordinate** over time. That is exactly the gap Claude Code's *agent teams*
(experimental, v2.1.32+) fills, and what this spike designs for mecatl.

The key difference between *subagents* and *teams*:

| | Subagents (Task/Fork) | Agent teams |
|---|---|---|
| Lifetime | one-shot | long-lived, multi-message |
| Visibility | drained internally | streamed to the client |
| Coordination | parent orchestrates everything | shared task list + peer mailbox |
| Communication | result only, child→parent | any member ↔ any member |

**Scope note — surface.** Claude Code's agent-teams *value* is largely its
interactive tmux/iTerm split-pane UX. mecatl is **headless** (gRPC + HTTP, no TUI
in core). So we are NOT porting the UX. We are designing the **orchestration
substrate** — shared task list, mailbox, lead/teammate lifecycle — exposed over
the gRPC surface so any client (including `cmd/mecatui` later) can drive and
observe a team. The substrate is the reusable, architecture-aligned part.

## 2. The reframing: a team is "Fork, but long-lived and talking"

The single most useful realisation from the spike: **`Fork` is already ~70% of the
plumbing.** It already:

- runs **N concurrent `Engine.Run` loops** (proven concurrency-safe; the Engine
  doc says one Engine backs the whole process and each Run owns its goroutine),
- gives each branch **its own fresh `session.Session`** and **its own workspace**
  via `tool.WorkspaceForker`,
- bounds fan-out and concurrency, fires `SubagentStop` per branch.

What a team adds on top of Fork:

1. branches are **long-lived** (they don't terminate after one turn),
2. branches **stream to the client** instead of being drained internally,
3. branches share a **task list** and a **mailbox** and can **self-coordinate**.

mecatl also gets a simplification Claude Code cannot have: **teammates are
goroutines in one `mecated` process, not separate OS processes.** Claude Code
needs on-disk task files with **file locking** to coordinate across processes
(`~/.claude/tasks/{team}/`). In mecatl the task list and mailbox are **in-memory,
mutex-guarded domain objects shared by reference** — no file locks, no IPC, no
serialization races. That is the whole reason the coordination kernel (Part 6) is
small and testable.

## 3. The blocking architectural finding: sessions are one-shot

`Engine.drive` (`internal/agent/loop.go`) **always** terminates the session in a
single `Run`: even a clean end-of-turn calls `terminateComplete → sess.Stop()`,
moving the aggregate to `StateCompleted`, which is terminal. `Service.StartRun`
just loads a session and runs it. **There is no in-place "continue this session
with another prompt" seam.** `session.RecordUserPrompt` explicitly rejects
terminal states.

A teammate that must **receive a message at T1, act, go idle, receive another
message at T2, act again** does not fit a one-shot session. The spike needs a
continuation seam. Two options:

- **(A) `session.Reopen()`** — a new intention-revealing method on the aggregate:
  legal only from a non-failed terminal state (`StateCompleted`), it transitions
  back to `StateIdle`, clears `stop`, and preserves `Conversation` + `Counters`.
  The supervisor then re-drives the same session via `Engine.Run` with the
  incoming message as the next user prompt. **Small, guarded, reuses everything**
  (the loop, compaction, permissions, events). It also incidentally unlocks
  ordinary multi-turn chat continuation, which the app layer currently lacks.
- (B) Carry the `Conversation` into a fresh `session.New` + `ReplaceHistory` each
  continuation. Rejected: `ReplaceHistory` is "running only", loses counter/limit
  continuity, and re-runs `SessionStart`/instruction assembly every message.

**Recommendation: (A).** It is the minimal domain change and is generally useful
beyond teams. The state machine becomes
`idle → running → awaiting → … → completed → (Reopen) → idle → …`.

> Invariant to preserve: `Reopen` must be illegal from `StateFailed`/
> `StateCancelled`, matching the spirit of the existing terminal guards. A test
> asserting `Reopen` from each terminal state belongs with the change.
>
> Scope update: a `cancelled` session **is** now recoverable in-process for
> interactive multi-turn — but via a SEPARATE seam, `session.Interrupt()`, not
> `Reopen`. Interrupt is legal only from `StateCancelled`, recovers to `idle`,
> and repairs the interrupted turn's history (closing out orphaned tool calls)
> so the replay stays provider-valid. `Reopen` stays `completed`-only. A
> `failed` session remains non-resumable from both seams.

## 4. Layering: where each piece lives

Dependencies point inward only (the project's load-bearing rule). The design slots
in without violating it:

- **`internal/team/` (NEW, DOMAIN leaf).** Pure coordination state + rules: the
  `Team` aggregate, `TaskList`, `Mailbox`, member lifecycle. Imports only
  `internal/session` (for `SessionID`) + stdlib. `session` never imports `team`,
  so no cycle. This is the **prototype delivered with this spike.**
- **`internal/session/`** — add `Reopen()` (Part 3). Domain.
- **`internal/governance/`** — add hook phases `TeammateIdle`, `TaskCreated`,
  `TaskCompleted` (mirror `PhaseSubagentStop`). Domain.
- **`internal/port/`** — optional `TeamStore` port if we want teams to survive a
  restart (mirrors `SessionStore`). v1 can run in-memory and skip this.
- **`internal/agent/` (APPLICATION).** A `TeamSupervisor`: owns the shared `*team.Team`,
  spawns one driver goroutine per member, re-drives each via `Engine.Run` +
  `Reopen` on message/task arrival, fans member event streams into one tagged
  stream, detects quiescence. Plus the member-facing **coordination tools**
  (`SendMessage`, `TaskCreate`, `TaskClaim`, `TaskComplete`, `SpawnTeammate`) as
  `tool.Tool`s backed by the shared `*team.Team`.
- **`internal/adapter/server/`** — new RPCs (Part 7); maps team events to proto.
- **`internal/app/`** — composition: build the supervisor, inject the team tools
  into the lead's / teammates' catalogs (read-only-share / mutating-fork per the
  agreed stance).

This mirrors exactly how `governance` (rules) and `permpolicy` (session-aware
adapter) are split, and how Fork's child Engine is composed in `internal/app`.

## 5. Mechanics

### 5.1 Members, the lead, and the synthesis deliverable

The **lead** is an ordinary session whose catalog additionally has `SpawnTeammate`
+ the task/mailbox tools. **Teammates** are sessions spawned by the supervisor,
each with the task/mailbox tools (and `SendMessage`) always available even when a
referenced agent-definition restricts other tools (Claude Code does the same: team
tools bypass the allowlist). The lead is **fixed for the team's lifetime** and
**teams do not nest** (a teammate cannot spawn a team) — both match Claude Code's
limitations and our existing no-recursion guard.

**Lead synthesis is the team's deliverable (implemented).** After the scheduling
loop reaches quiescence (or the round/budget cap), `Supervisor.Run` drives ONE final
**synthesis turn** on the lead (`synthesise` → the shared `driveOneTurn` helper). Its
output is `TeamOutcome.Report`, which the Team tool returns as its `ToolResult` and
the gRPC `RunTeam` rides back on the outcome — so both entry points get the
consolidated report for free. The team GOAL is rendered as the lead's (and every
member's) **TRUSTED top-level instruction** — NOT fenced — because its provenance is
the principal (the parent model's tool call from the user's prompt, or the gRPC request
the deployment owns) and no member-facing tool can mutate it (`WithTeamGoal` is the sole
writer). This is the instruction-hierarchy / spotlighting / CaMeL consensus: the
principal's task is trusted; only peer/retrieved data is untrusted. The goal is still
run through `neutraliseFraming` so it cannot forge a fence or a section header
(defang-but-don't-fence). A relay/multi-tenant deployment that interpolates untrusted
end-user text into the goal re-fences it via `agent.WithUntrustedGoal(true)` (the in-loop
Team tool stays always-trusted; the gRPC path flips it through `server.Config.TeamGoalUntrusted`).
Fencing the goal as UNTRUSTED was the original behaviour and caused spurious refusals
(the member was handed its own job inside a "do not obey" block). The synthesis prompt's
remaining source material is assembled in three layers (`buildSynthesisSources`), all
fenced UNTRUSTED:

1. **The findings ledger** (`team.Team.Findings()`) — the PRIMARY, deterministic
   channel: members record conclusions with the `RecordFinding` coordination tool as
   they reach them, so a finding survives even if the member is later cut off at its
   limits. Grouped by member in append order.
2. **A LastText/completed-task digest** for members that recorded NO finding — the
   fallback that rescues a member cut off mid-investigation (root cause: a `LastText`
   that was frequently empty on a limit cutoff).
3. **The lead's drained inbox** — peer messages addressed to the lead, appended last.

A lead stopped purely
by its lifetime turn budget is still resumable: the ONE synthesis turn runs even then
(the report is the deliverable).

**The deliverable resolves through a three-tier chain — never a bare refusal or empty
(`deliverable()` in `teamtool.go`).** `synthesise` is a pure PRODUCER; the QUALITY gate
lives in the Team tool. Tier **1** returns the lead's synthesis when it is usable —
non-empty AND `!isNonDeliverable(report, len(Findings))`. Tier **2** is the ledger-rich
structured fallback (`joinTeamFallback`): the findings ledger grouped by member FIRST,
then per-member disposition + `[STOPPED: reason]` + completed tasks + last text — reached
when the synthesis is empty OR a non-deliverable. Tier **3** is an honest floor ("ran N
rounds, did not converge, M stopped") when even the ledger is empty — structurally
non-empty. `isNonDeliverable` is deliberately CONSERVATIVE: it fires only on
empty/whitespace OR (short `≤ 280 runes` AND a PREFIX-anchored match against the tiny
`refusalPrefixes` set AND `ledgerLen > 0`) — all three together, so a legitimately terse
real report is never discarded and a refusal over an empty ledger is left alone. A
non-convergence header (`convergenceHeader`) is prepended to tiers 2/3 always and to tier
1 when `!Quiescent` (a runaway team's plausible-looking synthesis still carries the "did
NOT converge" banner). The fallback SKIPS the lead's `LastText` (it IS the rejected
synthesis). The data (`TeamOutcome.Findings`, `MemberOutcome.Completed`/`.Lead`) is
snapshotted in `outcome()`, so the gRPC path gets the same rich fallback. Headline guard:
`TestTeamToolRefusalSynthesisFallsBackToLedger`.

> **Deferred (4A) — team-wide token budget.** A config-only `WithTeamTokenBudget` that
> stops scheduling after the current round when the accumulated `session.Usage` crosses a
> ceiling, making `stop:max-tokens` trip this same fallback. The brake the 2.2M-token
> runaway needed; this bundle is the safety net. Orthogonal, not implemented here.

**On-demand member inspection (PULL).** Member sessions are persisted to the injected
`port.SessionStore` under collision-free, team-namespaced ids
(`MemberSessionID(teamID, member)` = `team-<teamID>-<member>`). The parent catalog's
`InspectMember` tool (read-only) loads ONE member's transcript by (team id, member)
and returns a bounded rendering — it does NOT auto-inject; the pulled transcript
enters the parent conversation only as that tool's own `ToolResult` (gauntlet #7's
no-auto-injection property holds).

**The Team ToolResult surfaces the team id (so `InspectMember` is reachable).** The
team id is the published Team call id; it rides `EvTeamStart` (a client-only event the
MODEL never sees). For the parent model to call `InspectMember` it must know that id —
so `renderTeamResult` prepends a `Team id: <id>` line to the Team tool's `ToolResult`
(BOTH the synthesis-report and the `joinTeamFallback` path), rendered VERBATIM so
`MemberSessionID(teamID, member)` reconstructs the saved member id byte-for-byte. Without
it the id was undiscoverable at runtime and `InspectMember` was unusable model-to-model
(the original defect; pinned by `TestParentDiscoversTeamIDFromResultAndInspects`). The
gRPC `RunTeam` consumer already holds the id from `CreateTeam`/`EvTeamStart`, so the
in-result header is the in-process-tool fix only.

**The lead's synthesis turn benefits from the loop's no-progress handler.** A lead whose
synthesis turn comes back empty (a reasoning-only / no-text turn) used to terminate at
once → `synthesise` returned `""` → `joinTeamFallback` skeleton. Because synthesis runs
through the SHARED `Engine.drive` (`driveOneTurn`), the no-progress handler now nudges the
lead up to `MaxNoProgressNudges` times INSIDE that one drive, so an empty first attempt is
driven to a real report before falling back. Pinned by
`TestLeadEmptySynthesisThenNudgedProducesReport`.

### 5.2 Message delivery: turn-boundary, not interrupt

A running `Engine.Run` is turn-based and we must not corrupt an in-flight turn.
So messages are delivered at **turn boundaries**, not as mid-turn interrupts:

```
member driver loop (per teammate, in the supervisor):
  for {
    inbox = team.Drain(me)                 // pending peer/lead messages
    task  = team.ClaimNext(me)             // next unblocked, unclaimed task
    if inbox empty and no task and team has open work elsewhere:
        team.SetMemberState(me, Idle); fire TeammateIdle; wait(signal)   // park
    if team.Quiescent(): break                                           // done
    prompt = render(inbox, task)           // synthesize the next user turn
    run = engine.Run(ctx, sess, ws, prompt)
    stream(run.Events(), tag=me)           // → client, NOT drained
    sess.Reopen()                          // ready for the next message/turn
  }
```

`wait(signal)` blocks on a per-member condition the `Mailbox`/`TaskList` signals
when a message is posted to `me` or a task `me` could claim becomes unblocked. No
busy-polling.

### 5.3 Workspace contention (the THREE-TIER model)

Workspace isolation is the security boundary; **capability flows down from the
parent** (which has Bash). A team member lands in one of three tiers (`AddMember`
picks the tier from `spec.Mutating` and the factory's `MemberBuild.IsolateReadOnly`):

- **base-share, no shell** (fallback when no read-only forker is wired): a read-only
  teammate (`code-reviewer`/`researcher`) shares the base — cheap, no copy — and gets
  Read/Grep/Glob only, NO Edit/Write/Bash, so it cannot corrupt the shared base;
- **read-only worktree, full shell**: a read-only teammate runs in a cheap git
  **worktree** (the forker's default mode — shares the base repo's `.git`, so it sees
  the **full commit history**) with Read/Grep/Glob **+ Bash**, but never Edit/Write.
  It can inspect with a real shell (`git log`/`git show`, `cat`, build, test) confined
  to a throwaway checkout. This is the common case once a shell is configured;
- **mutating copy, full shell**: an `implementer` teammate (Edit/Write) gets its own
  **force-copy** fork (own `.git`) with Edit/Write/Bash — parallel writes are safe
  because isolated.

- **merge/selection stays manual in v1** (consistent with Fork's no-auto-merge):
  the team reports each mutating teammate's fork path; a later phase can add a
  judge/merge step (the "tournament" join). A read-only worktree is throwaway — never
  merged.

This keeps the read-parallel/mutate-serial invariant intact: nothing mutates the
shared base concurrently. The Supervisor holds **two forkers** — `s.forker`
(force-copy, for mutating members) and `s.roForker` (worktree, for read-only-isolated
members) — and `AddMember`'s mutating-tool backstop gates on **base-sharing**
(`!needFork`), so an isolated member's mutating-classified Bash is exempt (it lands in
the member's own fork/worktree, never the shared base).

> **Workspace-aware Bash (done).** Both forked tiers get **Bash** when a shell is
> configured. `BashTool.Execute` passes the per-member forked `Workspace.Root()` to
> `CommandRunner.Run` as the working directory (the `CommandRunner` contract carries
> an explicit `workdir`; an empty one falls back to the runner's configured root), so a
> forked member's Bash runs in its OWN fork/worktree — its **default cwd is the fork,
> not the shared parent base** — and the bash gate (`SplitCommands`/`ReadOnlyBash`) is
> unchanged. Residual: unlike path-scoped Edit/Write, Bash can still escape its cwd via
> absolute paths or `cd` — the inherent Bash trust model, the same as the main session;
> isolation is the boundary.
>
> **Shared-`.git` hardening (read-only worktree).** A worktree shares the parent
> repo's `.git/config` + `.git/hooks`, so an untrusted repo could run code via
> `core.pager` / `core.hooksPath` / `core.fsmonitor` / an external diff driver — **and
> `git worktree add` fires the base repo's `post-checkout` hook at FORK time, before any
> member runner exists.** The hardening closes these fixed-key + hook vectors but does
> **not** close attacker-named `.gitattributes` driver configs (see *Residual /
> follow-up* below). The single neutralizing environment lives in
> `internal/adapter/gitenv` (a stdlib-only leaf) so two code paths share it and cannot
> drift:
>
> 1. **The forker's own git** (`runGit`/`gitRepoRoot`/`forkWorktree`) sets
>    `cmd.Env = gitenv.Scrub(os.Environ())`, so the fork-time `post-checkout` hook (and
>    any config-driven exec a probe might trigger) is neutralized before the worktree
>    even exists.
> 2. **A read-only member's Bash** runs through a **sandboxed command runner**
>    (`internal/app.buildSandboxedCommandRunner`) given the COMPLETE
>    `gitenv.Scrub(os.Environ())` environment.
>
> `gitenv.Scrub` **DROPS** every inherited `GIT_*` variable (so `GIT_EXTERNAL_DIFF`,
> `GIT_SSH_COMMAND`, `GIT_ALTERNATE_OBJECT_DIRECTORIES`, `GIT_PROXY_COMMAND` etc. cannot
> leak in — an append could not remove these) plus `PAGER`/`LESS`, while keeping
> PATH/HOME so git still works, then **APPENDS** the neutralizing set: `GIT_PAGER=cat`,
> `PAGER=cat`, `GIT_CONFIG_NOSYSTEM=1`, `GIT_CONFIG_GLOBAL=/dev/null`, and
> precedence-winning env-injected config (`GIT_CONFIG_COUNT`/`GIT_CONFIG_KEY_n`/
> `GIT_CONFIG_VALUE_n`) forcing `core.hooksPath=/dev/null`, `core.pager=cat`,
> `core.fsmonitor=false` and an empty `diff.external` over the shared `.git/config`. The
> MAIN session keeps its UNHARDENED runner (operator hooks/pager honoured). A Mutating
> (force-copy) member's fork has its OWN `.git`, so config-hardening is moot there, but
> it gets the hardened runner anyway. The per-command timeout (~30s) and the
> supervisor's concurrency cap (`defaultTeamConcurrency=4`) already bound a team's shell
> usage, so no extra per-member deadline/semaphore is added.
>
> **Residual / follow-up.** A fixed-key env override structurally cannot cover git config
> keys whose *driver name* is attacker-chosen in a tracked `.gitattributes`. Two such
> vectors remain reachable when the shared `.git` belongs to an **untrusted** repo:
> `filter.<drv>.smudge` (executes at `git worktree add` checkout — fork time) and
> `diff.<drv>.textconv` (executes on `git show` / `git log -p`); an `alias.<name>=!sh`
> also fires, but only if the member invokes that alias by name. Because the driver name
> is arbitrary, there is no fixed `GIT_CONFIG_KEY_n` that can pin it to an inert value.
> For a **trusted** repo (the operator's own) this execution is equivalent to the operator
> running git themselves — acceptable. The planned robust mitigation is to **gate
> read-only-member shell on workspace trust** (untrusted ⇒ no subagent shell, matching the
> harness's "untrusted degrades to ask-the-human" posture). This is a tracked follow-up,
> not yet implemented.

### 5.4 Quiescence & deadlock

The team is **done** when every member is `Idle`/`Stopped`, no task is
`pending`/`in_progress`, and every inbox is empty (`Team.Quiescent()` in the
kernel). The same predicate distinguishes "done" from "deadlocked waiting on each
other": if members are idle but tasks remain blocked by an unsatisfiable
dependency cycle, the supervisor surfaces it to the lead rather than hanging. A
global wall-clock/turn budget bounds a runaway team. A member's terminal
disposition — and, when it stopped, the closed-enum reason (`error`/`cancelled`/
`budget`) — now reaches the wire on `team.end` (a per-member snapshot parallel to
the terminal tasks/findings snapshots, bridged in `internal/agent`), so the ctrl+a
overlay renders `✗ stopped — <reason>` for a stopped member instead of flipping every
terminal lane to `✓ done`.

### 5.5 Permissions & non-interactivity

Teammates inherit the lead's permission mode at spawn (matches Claude Code:
"permissions set at spawn"). For headless operation, a teammate's permission asks
route to the **lead** (the lead arbitrates, as it does plan approval) or, if the
team runs unattended, fall back to the existing non-interactive auto-deny used by
Task/Fork. Plan-approval (teammate plans read-only, lead approves) maps cleanly
onto the existing `WithChildMode(session.ModePlan)` + the lead arbitration channel.

## 6. The coordination kernel (delivered prototype)

`internal/team/` implements and unit-tests the riskiest claim — that in-process,
shared-memory coordination is **correct under concurrency** and **testable
offline**. It is pure domain (no I/O, no LLM, no goroutines of its own), guarded
by a single mutex, and exercised under `-race`:

- `Team` aggregate: members (lifecycle states), a `TaskList`, a `Mailbox`.
- `CreateTask(desc, deps)` / `ClaimNext(member)` / `ClaimTask(id, member)` /
  `CompleteTask(id, member)` with **dependency gating** (a task is claimable only
  when all deps are completed) and **race-safe single-claim** (concurrent
  `ClaimNext` from N goroutines never double-assigns).
- `Send(from,to,body)` / `Drain(member)` mailbox with at-most-once delivery.
- `Quiescent()` done/deadlock predicate.

Tests cover: dependency gating, concurrent-claim safety (`-race`, N goroutines),
mailbox delivery semantics, unknown-member/þtask errors, and quiescence
transitions. Run: `go test ./internal/team/ -race`.

This kernel is deliberately **decoupled from the Engine** so it can be validated
before any supervisor/gRPC work exists — the essence of a spike.

## 7. Proposed gRPC surface (sketch, not in this spike)

New RPCs on the existing service (contract is `contracts/proto/mecatl/v1/harness.proto`):

```
CreateTeam(name, lead_session)            -> TeamID
SpawnTeammate(team, name, agent_type, prompt, isolation) -> Member
ListTeam(team)                            -> members, tasks, states
SendTeammateMessage(team, to, body)
StreamTeamEvents(team)                    -> stream of (member_name, Event)   // multiplexed
ShutdownTeammate(team, name)
CleanupTeam(team)
```

`StreamTeamEvents` is the multiplexed fan-in of every member's `Run.Events()`,
each event tagged with its member name — the headless analogue of split panes. The
existing per-session event mapping (`internal/adapter/server/mapper.go`) is reused
per member; only the member tag is new.

## 8. Phased implementation plan

1. **Kernel** *(done in this spike)* — `internal/team/` + tests.
2. **Continuation seam** — `session.Reopen()` + state-machine tests.
3. **Coordination tools** — `SendMessage`/`TaskCreate`/`TaskClaim`/`TaskComplete`
   as `tool.Tool`s over `*team.Team`; offline tests with `mockllm`.
4. **Supervisor** — `internal/agent` driver loop, event fan-in, quiescence,
   read-only-share/mutating-fork workspace policy; offline multi-member test
   (scripted `mockllm` per member) — the new gauntlet-style integration test.
5. **Hook phases** — `TeammateIdle`/`TaskCreated`/`TaskCompleted`.
6. **gRPC** — proto + server adapter + `task generate`.
7. **(Later) Join strategies** — judge/merge for mutating-teammate forks; optional
   `TeamStore` for restart durability.

Phases 1–4 are the substance and are all offline-testable. Phase 6 is the only one
touching the generated contract.

## 8a. Status of earlier gaps / remaining follow-ups

Two gaps flagged in the original spike have since been **closed**:

- **HTTP/SSE parity — DONE.** All six team RPCs now have HTTP routes
  (`http.go`): `POST /v1/teams` (create + optional roster), `POST
  /v1/teams/{id}/members`, `POST /v1/teams/{id}/messages`, `POST /v1/teams/{id}/run`
  (a `text/event-stream` of per-member-tagged `TeamEvent` frames, mirroring the
  `prompt` SSE handler), `GET /v1/teams/{id}`, `DELETE /v1/teams/{id}`. HTTP and gRPC
  share one JSON wire shape (the same `mecatlv1.*` messages); `ErrTeamsDisabled` /
  `ErrTeamRunning` → 412 and `ErrTooManyTeams` → 429 mirror the gRPC status codes.
- **Roster-in-CreateTeam — DONE.** `CreateTeamRequest` carries an optional
  `repeated TeammateSpec members`, enrolled **atomically** at creation (any member
  failure abandons the whole team — never registered, no `MaxTeams` slot consumed);
  `CreateTeamResponse` echoes the enrolled roster. The common path is now a single
  `CreateTeam` → `RunTeam`. `SpawnTeammate` remains for incremental pre-run adds (and
  is still rejected once the team is running, `ErrTeamRunning`).
- **Result aggregation — DONE.** The team's deliverable is now the lead's
  **consolidated synthesis** (§5.1), not a header-only concatenation of member
  `LastText`. Goal-to-lead threading (`WithTeamGoal` + `CreateTeamRequest.goal`), the
  `RecordFinding` ledger channel, the three-layer synthesis source, member-session
  persistence under `MemberSessionID` (`team-<teamID>-<member>`), the PULL
  `InspectMember` tool, and the `EvTeamFindings` event projection (wired through the
  proto + mecatui) all shipped together.

Still deferred (intentional, not oversights):

- **(See also §8.7)** join strategies for mutating-teammate forks and a `TeamStore`
  for restart durability remain deferred.
- Per-teammate model/agent-definition selection (the factory currently uses the
  session model for every member) is a natural next step but out of scope here.

## 9. Risks / open questions

- **Determinism in tests.** Free-running member goroutines + a shared mutex are
  race-safe but scheduling is nondeterministic. The supervisor test must assert on
  *outcomes* (all tasks completed, transcript per member) not interleavings, and
  drive `mockllm` so each member's tool calls are scripted. The kernel itself is
  fully deterministic (no goroutines of its own).
- **`Reopen` semantics** — *Decided & implemented:* legal only from
  `StateCompleted` (illegal from failed/cancelled/non-terminal), and it **resets**
  per-run `Counters` so `Limits` keep their single-run meaning (bound each prompt's
  work). A teammate's lifetime budget (total turns across its life) is the
  supervisor's responsibility, enforced separately. See `session.Reopen`.
- **Lead context growth** — the lead must NOT ingest teammates' full transcripts
  (that defeats context isolation). *Implemented:* the lead's synthesis turn reads a
  fenced DIGEST — the findings ledger + a per-member LastText/completed-task summary +
  its drained inbox (`buildSynthesisSources`), all bounded and fenced UNTRUSTED — never
  the members' full transcripts. The mailbox + the findings ledger are the only
  channels through which teammate work reaches the lead; an out-of-band reader uses the
  PULL `InspectMember` tool, which never auto-injects.
- **Token cost** — teams are linearly more expensive than one session (each member
  is a full loop). Worth a config cap on concurrent members + a global budget.

## Sources

- Claude Code — Create custom subagents: https://code.claude.com/docs/en/sub-agents
- Claude Code — Orchestrate teams of Claude Code sessions: https://code.claude.com/docs/en/agent-teams
- OpenAI Agents SDK — Orchestration & handoffs: https://developers.openai.com/api/docs/guides/agents/orchestration
- Goose — Sub-recipes / subagents: https://block.github.io/goose/docs/guides/recipes/sub-recipes/
- Internal: `docs/harnesses/02-twelve-patterns.md` (patterns 7, 8), `docs/harnesses/05-comparative-harnesses.md`
