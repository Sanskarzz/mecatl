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
> `StateCancelled` (a failed/cancelled run is not resumable), matching the spirit
> of the existing terminal guards. A test asserting `Reopen` from each terminal
> state belongs with the change.

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

### 5.1 Members & the lead

The **lead** is an ordinary session whose catalog additionally has `SpawnTeammate`
+ the task/mailbox tools. **Teammates** are sessions spawned by the supervisor,
each with the task/mailbox tools (and `SendMessage`) always available even when a
referenced agent-definition restricts other tools (Claude Code does the same: team
tools bypass the allowlist). The lead is **fixed for the team's lifetime** and
**teams do not nest** (a teammate cannot spawn a team) — both match Claude Code's
limitations and our existing no-recursion guard.

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

### 5.3 Workspace contention (the agreed mutation stance)

Per the decision this spike was scoped under: **read-only teammates share the base
workspace; mutating teammates run in isolated forked workspaces** (`WorkspaceForker`,
reused from Fork). So:

- a `code-reviewer`/`researcher` teammate (read-only) shares the base — cheap, no
  copy;
- an `implementer` teammate (Edit/Write) gets its own fork — parallel writes
  are safe because isolated;
- **merge/selection stays manual in v1** (consistent with Fork's no-auto-merge):
  the team reports each mutating teammate's fork path; a later phase can add a
  judge/merge step (the "tournament" join).

This keeps the read-parallel/mutate-serial invariant intact: nothing mutates the
shared base concurrently.

> **Follow-up — workspace-aware Bash for forked children (fork-rooted CommandRunner).**
> A Mutating teammate (and a Fork branch) currently gets **Edit/Write but NOT Bash**,
> even when a shell is configured. The `BashTool`'s `CommandRunner` has its working
> directory baked to the **parent base** at construction
> (`internal/adapter/osfs` runner, `cmd.Dir = r.root`) and `BashTool.Execute` ignores
> the per-branch/per-member forked `Workspace` it is handed — so a forked child that
> ran Bash would mutate the **shared parent base**, escaping its fork and breaking the
> isolation guarantee (and `ForkTool.ReadOnly()==true`). The ship-now fix is to simply
> not register Bash for forked children (`app.buildForkChildEngine` and the Mutating
> branch / DEFINED member path of `app.buildMemberEngine`). Making Bash workspace-aware
> — a CommandRunner re-rooted at the forked child's `Workspace.Root()`, plumbed through
> the frozen `Tool.Execute(ctx, in, ws)` signature without weakening the bash gate —
> is a deliberate, separate follow-up; it touches the `CommandRunner` contract and the
> command-canonicalisation gate, so it is out of scope for the isolation fix.

### 5.4 Quiescence & deadlock

The team is **done** when every member is `Idle`/`Stopped`, no task is
`pending`/`in_progress`, and every inbox is empty (`Team.Quiescent()` in the
kernel). The same predicate distinguishes "done" from "deadlocked waiting on each
other": if members are idle but tasks remain blocked by an unsatisfiable
dependency cycle, the supervisor surfaces it to the lead rather than hanging. A
global wall-clock/turn budget bounds a runaway team.

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
  (that defeats context isolation). It sees only mailbox messages + idle/Completed
  notifications. Confirm the mailbox is the *only* lead-visible channel.
- **Token cost** — teams are linearly more expensive than one session (each member
  is a full loop). Worth a config cap on concurrent members + a global budget.

## Sources

- Claude Code — Create custom subagents: https://code.claude.com/docs/en/sub-agents
- Claude Code — Orchestrate teams of Claude Code sessions: https://code.claude.com/docs/en/agent-teams
- OpenAI Agents SDK — Orchestration & handoffs: https://developers.openai.com/api/docs/guides/agents/orchestration
- Goose — Sub-recipes / subagents: https://block.github.io/goose/docs/guides/recipes/sub-recipes/
- Internal: `docs/harnesses/02-twelve-patterns.md` (patterns 7, 8), `docs/harnesses/05-comparative-harnesses.md`
