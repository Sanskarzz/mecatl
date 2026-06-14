# Cloud-native arc: disposable process, externalized state, durable record

Status: **Phase 0 deliverable** (this doc IS the Phase 0 gate). Descriptive only, no
behavior change. Builds on the shipped driver-seams arc (`DRIVERS.md`) and the no-FS
session profile (issue #55, commit `9f8ba8c`); informed by the cloud-native kit
inventory (PR #54), whose subsystem #6 advice ("don't design resource lifetimes,
inventory them") this doc executes, and whose session-state gaps (§1.1 to §1.4) the
later phases close. Every file:line below was verified against main at the time of
writing; line numbers drift, symbols don't.

## What the arc is

Make the harness process genuinely disposable: kill it at any moment, start another
one over the same stores, and lose nothing the user cares about. That decomposes into
three properties:

1. **Externalized state**: nothing load-bearing lives only in process memory.
2. **Evict/rehydrate**: a session parked mid-turn (awaiting a human approval) can be
   resumed by a different process than the one that parked it.
3. **Durable record**: the rich history (events, approvals, pre-compaction turns) is
   persisted, not emitted-and-discarded.

This is the Tier-2 cloud arc that `DRIVERS.md` recorded as deferred (ask
externalization, outbox, leasing), now planned as concrete phases.

## What is already true

The harness is unusually close by construction:

- **Turn-boundary persistence.** The session aggregate round-trips through a stable
  snapshot (`engine/adapter/sessnap/sessnap.go:32-43`) saved at every turn boundary.
  Restore drives the state machine through its public transitions, including
  re-raising the pending ask via `PauseForApproval` for an awaiting session
  (`sessnap.go:113-167`). Kill the process between turns and a reload reconstructs
  the conversation exactly.
- **Stateless full-replay provider.** The LLM adapters keep no server-side
  conversation state (`store:false`, byte-stable prompt prefix); resume is "load the
  snapshot and replay". There is nothing provider-side to externalize.
- **The driver protocol.** Six ports already cross process boundaries over gRPC
  (`contracts/proto/mecatl/driver/v1`): sessions, memory, skills, soul, agent defs,
  slash commands, with conformance suites as the contract (`DRIVERS.md`). The session
  snapshot crosses as an opaque format-tagged blob
  (`grpcdriver.SnapshotFormat = "sessnap-json/1"`,
  `internal/adapter/grpcdriver/sessionstore.go:30`).
- **The no-FS profile and its rehydration seam** (commit `9f8ba8c`, issue #55). A
  session can run with no filesystem at all (`engine/adapter/nofs`), and the first
  run-entry rehydration seam exists: `Service.rehydrateNoFSSession`
  (`internal/adapter/server/service.go:1084`) rebuilds a restarted no-fs session's
  per-session engine through the same factory create used, double-defended by the
  empty-root chokepoint in the osfs workspace factory
  (`internal/app/build.go:3815`). That is the evict/rehydrate mechanism in miniature,
  verified e2e across two Builds over a shared store. Details in
  `IMPLEMENTATION-NOTES.md` ("Session profiles").

What is NOT yet true: a process death while a run is awaiting approval strands the
session (`ErrNoActiveRun`); the event stream is emitted and discarded; compaction
destructively rewrites the only durable record; several per-session facts (provider
selector, token usage, profile) are not in the snapshot; and two processes over one
store have no writer exclusion. Those gaps are exactly the phases below.

## Phase plan

Each phase is independently shippable, CI-green, and gated on a falsifiable
demonstration; the live e2e suite (`e2e/`) is the verification lane for the
cross-process gates.

### Phase 0: inventory + decisions (this doc)

Three lists: the resource inventory, the rehydrate-fidelity ledger, the recorded
decisions. Gate: the doc exists and the ledger has an explicit decision per row. No
behavior change.

### Phase 1: snapshot fidelity

Make the snapshot faithful enough that a restarted process is indistinguishable
mid-conversation. Per the ledger below:

- `profile` becomes a snapshot field (additive, omitempty; the same precedent as
  `ProviderPhase`/`Parts`; the empty-workspace inference stays as the second defense).
- The provider/model selector is persisted, so rehydration rebuilds the SAME
  per-session engine instead of falling to the default-provider floor (re-derive via
  the engine factory, never clone-and-swap; the existing discipline).
- `session.Usage` is persisted, so the `MaxRunTokens` budget brake survives restart.
- Anything else the ledger marks Phase 1.

Gate (extends the `9f8ba8c` drill): cross-Build e2e: create a no-fs session on a
selector model with a partially-consumed budget, kill the process, restart over the
shared store, verify same catalog + same model + budget continues. Mutation-verified
per field.

### Phase 2: awaiting-approval evict/rehydrate (the disposability completion)

The kit inventory's §1.4, built on the rehydration seam `9f8ba8c` created:

- A resume-from-awaiting entry at the run-entry funnel: `Approve`/`Deny` against a
  session whose process died while parked loads the snapshot (state=awaiting,
  `Pending` set), rebuilds the engine (Phase 1 makes this faithful), re-enters the
  loop AT the ask, and delivers the verdict. `ErrNoActiveRun` stops being terminal
  for awaiting sessions.
- The loop re-entry is the new piece: today resume re-enters at turn boundaries only
  (`loadAndReopen`, `internal/adapter/server/service.go:868`); this adds the mid-turn
  cursor. The three existing seams (Reopen/Interrupt/Recover) must NOT widen; this is
  a fourth, awaiting-only entry.
- Active eviction (a parked run voluntarily releasing its goroutine and heap after
  some idle period) is a follow-on knob, not this phase's gate. Approve-after-crash
  is the essence; eviction is then just choosing to crash on purpose.
- Known accepted wart: permstore rules are still in-memory, so a rehydrated session
  re-asks. Fail-safe; fixed by Phase 3b.

Gate: live e2e: raise an ask, SIGKILL the server, restart, approve over the shared
store, the run completes with the tool executed exactly once. Plus: child asks
(subagent-surfaced asks) explicitly documented as NOT rehydratable this round
(run-scoped by design), an honest note, not silence.

### Phase 3: durable event log / outbox (the new durable artifact)

The kit inventory's §1.1 to §1.3 taken incrementally, NOT full CQRS. The snapshot
stays the replay projection; the log is additive.

- **3a:** an append-only per-session event log behind a new port (EventLog/Outbox),
  local JSONL adapter first (the jsonlstore precedent), wired at the server relay:
  the stream the harness already emits, persisted instead of discarded. Include
  permission asks AND verdicts (the chronological approval record neither mecatl nor
  Claude Code has today).
- **3b:** durability consumers: permstore allow-always rules recoverable from
  verdict events (kills the Phase 2 wart); compaction archives the replaced span to
  the log before `ReplaceHistory` (the durable record stops being lossy; "what did
  the agent do in turn 12" stays answerable after compaction).
- **3c:** the driver service (`EventLogService` in driver/v1), same
  conformance-as-contract discipline as the other six. This is also the prerequisite
  issue #28 (session-scoped background detach) has been waiting on; #28 itself stays
  its own arc.

Gate: a session with one compaction and three verdicts can be fully reconstructed
(user-rich timeline including pre-compaction turns) from store + log alone; mecatui
or a test client can render it. Mutation-verify the no-leak guards (the log must
respect the same redaction the event stream already enforces, gauntlet #7
discipline).

### Phase 4: multi-replica readiness (defer until a real deployment wants it)

- Session leasing: single-writer enforcement (the inventory below says what else
  needs leases; sessions almost certainly first). Probably a driver-protocol concern
  (lease/renew on `SessionStoreService` or a sibling), Chubby / Kubernetes-Lease
  semantics.
- Until then the stated v1 constraint stands: session-affinity routing, one writer
  per session (decision (c) below).

### Sequencing rationale

0→1→2 is a strict dependency chain (rehydrate needs faithful snapshots). 3 is
independent of 2 and could swap, but 2-before-3 is recommended: it completes the
disposability thesis with the least code while the `9f8ba8c` rehydration seam is
fresh, and its one wart (re-asks) is exactly what 3b fixes, a clean handoff. 4 waits
for a deployment that needs it.

Explicitly deferred, unchanged from the PR #54 evaluation: FS-as-driver (the
`DRIVERS.md` workspace-driver sketch; the filesystem returns as an optional mounted
capability), fork merge-back, environment provisioning, the substrate/mount-table
model, ProcessHost, a memfs-scratch profile (needs a snapshot story, naturally
revisitable after Phase 1).

## List 1: resource inventory

Every resource the harness allocates whose lifecycle outlives a single tool call,
tagged with its de-facto scope in the nesting `call ⊂ run ⊂ session ⊂ team ⊂
process`. "Re-attach" answers: can a restarted process recover it?
**reconstructible** (rebuilt from config/disk on next Build), **persisted** (the
durable artifact survives and is reloaded), or **lost** (gone, possibly leaking).

| # | Resource | Owner | Scope | Cleanup today | Re-attach | Evidence |
|---|---|---|---|---|---|---|
| 1 | Global MCP manager (`globalMgr`) | `app.Build` | process | `mcpClose` in Build's `closeAll`; NEVER folded into per-session close (`build.go:980`) | reconstructible (reconnects from config at next Build) | `internal/app/build.go:1868` (`connectMCP`) |
| 2 | Per-session client MCP managers | `sessionEngineFactory` | session | per-session close func, invoked by `Service.CloseSession` (`service.go:743`) and shutdown | lost (client specs are not persisted; a client re-mounts via `LoadSessionWithMCP`, `service.go:968`) | `internal/app/build.go:962` |
| 3 | Preserved-fork LRU (`LRUForkReaper`) | `app.Build` (shared via `catalogAssets`) | process | LRU eviction runs each entry's cleanup (dir removal) outside the lock | registry lost; the preserved fork DIRS remain on disk un-tracked (a leak on crash) | `engine/agent/forkreaper.go:41,64`; built at `internal/app/build.go:1936` |
| 4 | Project memory store (flock pair: `memory.json` + `memory.lock`) | `app.Build` | process handle, per-directory data | flock held per-operation only; one `*Store` per dir per process (self-deadlock invariant, `memory/store.go:79-88`) | persisted (data on disk; handle rebuilt at next Build) | `internal/app/build.go:1895`; `internal/adapter/memory/store.go:89-92` |
| 5 | User-model store (same adapter, XDG dir) | `buildUserModelStore` | process handle, per-user data | as above | persisted | `internal/app/build.go:1337` (dir derivation `1328-1335`) |
| 6 | permstore learned allow-always rules | `app.Build` | session (data), process (store) | `Forget(sessionID)` via `OnCloseSession` (`service.go:748`); capped at 256/session | **lost** (in-memory by design; restart re-asks) | `engine/adapter/permstore/permstore.go:42,48` |
| 7 | Skill read-roots + driver asset cache (temp dir) | the skills seam in `buildCatalog` | process (build-scoped) | `os.RemoveAll` in the seam close, folded into Build's `closeAll` | reconstructible (fresh temp dir next Build; driver assets re-materialize lazily on first activation) | `internal/app/build.go:2228` (`MkdirTemp`) |
| 8 | jsonlstore session files + `.tools.jsonl` audit | jsonlstore | per-session files | none needed: files opened per call (`O_APPEND`), closed immediately, never held | persisted (`Load` reads the latest snapshot line) | `internal/adapter/store/jsonlstore/jsonlstore.go:264-265` |
| 9 | Live-run registry (`Service.runs`) | `server.Service` | run | `deregister` after the wire adapter drains `run.Events()` | lost (the run dies with the process; the session snapshot persists) | `internal/adapter/server/service.go:368` |
| 10 | Team registry (`Service.teams`) | `server.Service` | team | removed at team terminal | **lost** (see ledger row 10: the whole coordination state) | `internal/adapter/server/service.go:369` |
| 11 | Per-session engine registry (`Service.sessionEngines`) | `server.Service` | session | evicted + closed at `CloseSession` (`service.go:750-758`) and shutdown | lost; rehydrated ONLY for the no-fs profile (`rehydrateNoFSSession`, `service.go:1084`); selector/client-MCP sessions degrade to the default engine | `internal/adapter/server/service.go:379` |
| 12 | Per-session workspace overrides (`Service.sessionWorkspaces`: nofs, ACP buffers) | `server.Service` | session | evicted at `CloseSession` (`service.go:757`) | lost; the no-fs override is re-registered by rehydration | `internal/adapter/server/service.go:388` |
| 13 | Background children (`childRunRegistry`) | `agent.Run` | run | `drainChildren` at both terminate paths (cancel + join + seal) | registry lost; the child SESSIONS persist via `WithSubagentStore`/`WithMemberStore` and are individually resumable | `engine/agent/childregistry.go:128-131`; `engine/agent/subagent.go:599,1267` |
| 14 | askRegistry channel park + childAskRouter | `agent.Run` | run | unregistered on verdict/retract; dies with the run | lost; a post-death `Approve` returns `ErrNoActiveRun` (`service.go:1226-1232`), the Phase 2 target | `engine/agent/permission.go:30-32,161` |
| 15 | Hook subprocesses | hookexec, per invocation | call | spawn, wait (30s default timeout, process-group kill) | nothing to re-attach | `internal/adapter/hookexec/hookexec.go:116,130` |
| 16 | Child-session retention GC goroutine | `startChildGC` | process | exits on ctx done or sticky `ErrPruneUnsupported` | reconstructible (restarts with the process; the swept artifact is the store) | `internal/app/childgc.go:235` |
| 17 | Memory/user-model consolidation goroutines (dream) | `app.Build` | process | exit on ctx done | reconstructible | `internal/app/build.go:1893,1901,1920` |
| 18 | Live model-catalog refresh goroutine | `app.Build` | process | one-shot; `refreshClose` in `closeAll` | reconstructible (embedded catalog is the floor) | `internal/app/modellister.go:35`; `internal/app/build.go:763` |
| 19 | Driver connection cache (`driverConns`, one lazy `ClientConn` per URL) | `app.Build` | process | once-guarded closes folded into the per-seam closes | reconstructible (lazy redial next Build) | `internal/app/driverstore.go:54-90` |
| 20 | WebSearch `SearchProvider` (shared `*http.Client` + egress semaphore) | `app.Build` | process | none needed (stateless client; the semaphore is a per-process egress bound) | reconstructible (rebuilt from the backend-tier config at next Build) | `internal/app/build.go` (`buildSearchProvider`); `internal/adapter/search/httpsearch.go:79-80` (issue #26) |
| 21 | modelhook guardrail breakers (`checkBudget` call cap + `failureStreak`) | `modelhook.Runner` (composition) | session | dies with the session Runner | **lost** (in-memory; a rehydrated session gets a fresh check-budget and a closed breaker, fail-safe) | `internal/adapter/modelhook/breaker.go:15,53`; constructed `modelhook.go:124,128` (commit `a032412`) |
| 22 | `askReviewBreaker` (headless ask-reviewer circuit breaker) | `agent.Run` | run | dies with the run | lost (run-scoped by design) | `engine/agent/askadjudicator.go:144` (commit `1b774d4`) |

### Does resource-lifetime management earn a seam now?

The kit inventory's question, answered: **no, not yet; the hand-managed lifecycles
suffice until Phase 4.** The table sorts cleanly into three clusters, and none of
them wants a generic resource manager today:

- **Process-scoped infrastructure (rows 1, 3-5, 7, 16-20)** is reconstructible from
  config at the next Build. The only genuine restart liability in the cluster is row
  3's preserved-fork directories, which leak on crash because the LRU registry (the
  only thing that knows to delete them) is in-memory. That is small, bounded by the
  LRU cap per process lifetime, FS-profile-only (no forks exist under no-fs), and a
  startup sweep of the fork-dir naming convention would fix it without any
  abstraction. Row 20 (the WebSearch provider) is a process-scoped resource too, but
  with no crash-leak shape: a stateless `http.Client` plus an in-process semaphore,
  nothing on disk to orphan.
- **Session-scoped server state (rows 2, 6, 11, 12, 21)** is exactly the rehydration
  surface Phases 1-3 address one row at a time: row 11/12 via profile+selector in
  the snapshot (Phase 1), row 6 via verdict events (Phase 3b), row 2 stays
  client-owned by design. Row 21 (the modelhook guardrail breakers) resets fail-safe
  and is observability/spend-bounding only, not correctness-critical, so it stays
  reset-by-design.
- **Run-scoped ephemera (rows 9, 13, 14, 22)** dies with the run by design; Phase 2
  changes what "the run died" means for the one row that matters (14), without making
  the others durable.

The first thing that would force a real seam is cross-process exclusion (leasing),
which is a driver-protocol concern (Phase 4), not an in-process resource manager. The
trip-wire to revisit: if a future arc adds a fourth process-scoped resource with a
crash-leak shape like row 3, or if leasing lands and wants a uniform "what does this
process hold" enumeration, build the seam then, against this inventory. The three
post-`f1f4e31` additions (rows 20-22) do NOT trip it: row 20 is process-scoped but
leak-free, rows 21-22 are session/run-scoped, so the conclusion stands.

## List 2: rehydrate-fidelity ledger

Everything a live run or session holds that is NOT reconstructed by loading the
sessnap snapshot. The snapshot truth is `engine/adapter/sessnap/sessnap.go:32-43`:
`id, state, mode, limits, counters, workspace, created_at, messages[]` (each with
`role, text, tool_calls, tool_result, reasoning, phase, parts`), `pending`,
`stop_reason`. Nothing else is persisted.

Decisions: **persist-in-snapshot** (additive field), **derive** (recomputable from
what is persisted), **reset-by-design** (documented, acceptable),
**fix-via-event-log** (Phase 3 makes it durable).

| # | Item | Where it lives | On restart today | Decision | Phase |
|---|---|---|---|---|---|
| 1 | Session profile (no-fs vs default) | nowhere persisted; DERIVED from the empty-workspace pun (`service.go:1031-1041`) | correctly rehydrated, but only because "empty persisted workspace ⇒ no-fs" happens to be sound today; it breaks the day a second workspace-less profile exists | persist-in-snapshot (inference stays as second defense) | 1 |
| 2 | Provider/model selector | `Service.sessionEngines` (`service.go:379`), in-memory only | falls to the DEFAULT provider; the rehydration comment records this as the conscious sound floor (`service.go:1076-1078`); a posture change, not an escalation. The floor is now operator-configurable (`--default-provider`/`--default-model`, issue #21, commit `d1ac84b`), which strengthens it | persist-in-snapshot; rehydration re-derives the engine via the factory | 1 |
| 3 | `session.Usage` (cumulative run tokens) | a LOCAL variable in the loop (`var total session.Usage`, `engine/agent/loop.go:677`); not on the aggregate at all | resets to zero, so the `MaxRunTokens` brake (`budgetExhausted`, `loop.go:973-975`) grants a full fresh budget after every restart | persist-in-snapshot (additive `usage` field; the loop seeds `total` from it) | 1 |
| 4 | permstore allow-always rules | `permstore.Memory.bySession` (`engine/adapter/permstore/permstore.go:48`) | discarded; the user is re-asked. Fail-safe, annoying | fix-via-event-log (verdict events replayed into permstore) | 3b |
| 5 | The pending PARENT ask | the data IS in the snapshot (`Pending`, `sessnap.go:41`, restored via `PauseForApproval`); the LIVENESS is a parked channel (`askRegistry.await`, `engine/agent/permission.go:161`) | durable but stranded: `Approve` finds no run and returns `ErrNoActiveRun` (`service.go:1226-1232`) | the resume-from-awaiting loop entry (durability already correct; only liveness is missing) | 2 |
| 6 | Pending CHILD asks (childAskRouter) | run-scoped in-memory routing of child-namespaced askIDs | lost with the run | reset-by-design; Phase 2 re-enters at the PARENT ask only, child asks documented non-rehydratable | 2 (doc note) |
| 7 | Background children | `childRunRegistry` (`engine/agent/childregistry.go:131`), run-scoped; child sessions persist via `WithSubagentStore` (`subagent.go:599`), and parallel branches likewise via `WithParallelStore` (`parallel-<callID>-<i>`, commit `fe9ffe5`, forensically loadable through the `{subagent-, parallel-}` prefix gate) | the running children die un-drained; their persisted sessions remain individually loadable/resumable (`resume:` / `InspectSubagent`), but nothing reconnects them to the parent | reset-by-design for v1; session-scoped detach is issue #28, gated on the event log | 3c → #28 |
| 8 | Edit read-ledger | in-memory per-`osfs.Workspace` map of sha256 fingerprints (`internal/adapter/osfs/osfs.go:428,714,729`) | the factory builds a fresh Workspace with an empty ledger; the first Edit after restart is REFUSED ("not read this session") until the model re-Reads. Fail-safe, never silently wrong; costs one extra Read per touched file. N/A for no-fs sessions | reset-by-design now; becomes a snapshot candidate if the re-Read tax proves annoying (Phase-1-adjacent, FS sessions only; the kit inventory's §2.4 explicit-token shape is the eventual answer) | deferred |
| 9 | Pre-compaction history | nowhere: `maybeCompact` rewrites the conversation via `ReplaceHistory` and that is what the next Save persists | the durable record is already lossy BEFORE any restart; compaction itself is stateless given the (compacted) history, so restart adds no new loss | fix-via-event-log (archive the replaced span before `ReplaceHistory`) | 3b |
| 10 | Mid-round team state | the `team.Team` aggregate (roster, goal, tasks, mailbox, findings; `engine/team/team.go:179`) and `Supervisor.members` runtime (`engine/agent/teamsupervisor.go:298`) are in-memory only; `Service.teams` (`service.go:369`) likewise. ONLY member sessions persist (`persistMember`, `teamsupervisor.go:1187`, under `MemberSessionID`, `teamsupervisor.go:1509`) | a mid-round team is unrecoverable: member transcripts survive as orphan sessions, the coordination state (who was assigned what, the findings ledger, the round number, the goal) is gone; there is no resume-team seam | reset-by-design for v1 (teams are run-scoped work units); the event log is the prerequisite for anything better, and re-creating the team from scratch is the documented recovery | 3 (prereq), honest note now |
| 11 | The event stream | `Run.events`, a buffered channel (cap 64, `engine/agent/loop.go:575`), relayed by gRPC `Converse` / HTTP SSE and then discarded | gone; a reconnecting client sees only the replay history, never the rich timeline (live reasoning, ask/verdict pairs, delegation lifecycle) | fix-via-event-log (persist at the server relay) | 3a |
| 12 | Per-session client MCP mounts | session-supplied specs, never persisted; the manager is row 2 of the inventory | lost; the owning client re-mounts via `LoadSessionWithMCP` (`service.go:968`) | reset-by-design, client-owned (the client holds the specs; the server cannot reconstruct credentials it never stored) | n/a |
| 13 | The in-flight turn (LLM stream) | nowhere; no mid-stream checkpoint exists | a turn cut by process death is lost and replayed from the last turn boundary; this is the stateless-replay thesis working as designed | reset-by-design (turn-boundary granularity is the contract; Phase 2 adds the one finer-grained cursor that matters, the ask) | n/a |
| 14 | Run plumbing (diagnostics binding, askID serial, ctx) | minted fresh per `Run` | rebuilt trivially | derive | n/a |
| 15 | modelhook guardrail breakers (`checkBudget`, `failureStreak`) | per-session in-memory on `modelhook.Runner` (`internal/adapter/modelhook/breaker.go:15,53`, commit `a032412`) | reset to zero: a rehydrated session gets a fresh check-budget and a closed breaker | reset-by-design (fail-safe; bounds per-session guardrail spend, not correctness; mirrors the permstore shape of row 4) | n/a (3b-adjacent only if guardrail spend ever needs to survive restart) |
| 16 | `askReviewBreaker` (headless ask-reviewer breaker) | run-scoped on `agent.Run` (`engine/agent/askadjudicator.go:144`, commit `1b774d4`) | dies with the run | reset-by-design (run-scoped; same cluster as rows 13/14) | n/a |

Two ledger observations worth stating in prose:

- **The awaiting state is the one place where durability and liveness already
  diverge** (row 5): the snapshot faithfully holds the parked ask and restores it
  through the real state machine, yet the only consumer of that fidelity today is
  the test suite, because no code path re-enters a loaded awaiting session. Phase 2
  is small precisely because the hard half (durability) shipped with sessnap.
- **Teams are the largest honest gap** (row 10). The member-session persistence
  gives forensics, not resumption. Saying "mid-round teams do not survive restart"
  in the operator docs is part of this arc's v1 posture; pretending otherwise is
  not.

## List 3: decisions

Three decisions this arc must record now. Each carries a recommendation; all three
are **OPEN** until the maintainer confirms.

### (a) Memory scope key for FS-less sessions (OPEN)

Today memory is opened from `cfg.MemoryDir` once at build time
(`internal/app/build.go:1895`; the `--memory-dir` flag,
`cmd/mecated/main.go:820`) and shared by every session in the process. The
semantics are per-project-directory: facts written over one project dir are visible
to later sessions over the same dir. A no-FS cloud session has no directory, so
"which memory does this session see" becomes a real question.

What the driver protocol can already express: nothing scope-shaped. Every
`MemoryStoreService` RPC carries only the operation's own arguments; `RecallRequest`
is `{key}` (`contracts/proto/mecatl/driver/v1/memory_store.proto:99-102`),
`ListRequest` is `{prefix}`, `IndexRequest` is empty. There is no tenant, principal,
session, or namespace field anywhere in the service. The scope boundary is therefore
the **endpoint**: whatever backend `--memory-store-url` points at IS the scope, and
the process holds one shared connection per URL (`driverConns`,
`internal/app/driverstore.go:54-90`).

Options:

1. **Per-deployment (status quo).** The deployment's memory driver/dir is the
   scope; a hosting platform that wants per-tenant memory runs one harness (or one
   driver endpoint) per tenant, or implements scoping driver-side keyed on
   connection auth.
2. **Per-principal/tenant key threaded through the driver protocol.** Add a scope
   field to every memory RPC (or a per-stream header), plumbed from session
   creation. A protocol change plus a port change (`tool.MemoryStore` would need the
   key on every call or a scoped-store factory).
3. **Per-agent-identity.** Key memory on the soul/agent identity rather than the
   tenant; same plumbing cost as 2 with a different key choice. Note this is now
   *partially shipped for the read path*: per-agent persistent memory (issue #33,
   commit `31d716e`) keys a read-only `MEMORY.md` on the agent def name under an FS
   root (`<root>/agents-memory/<defName>/`, user or project tier,
   `internal/app/agentdefs.go`), injected as fenced untrusted data into the agent's
   prompt. It is FS-rooted and read-only, so it does not touch `tool.MemoryStore` or
   the driver protocol, and it fail-softs to empty under no-fs (no workspace/XDG base),
   so it is an instance of this option's *keying idea* without a no-fs story or a
   write path yet.

**Recommendation: 1 for this arc.** It is honest about what is built, requires no
protocol or port change, and composes with the existing posture (the driver endpoint
is already the trust and capability boundary, `DRIVERS.md` "Trust & security
posture"). Option 2 is the eventual multi-tenant answer, but threading a scope key
through six methods, the port, the conformance suite, and the driver protocol is
speculative until a real multi-tenant consumer exists; the no-speculative-widening
rule applies. Record the gap, defer the widening.

### (b) Profile in the snapshot, recommend YES in Phase 1 (OPEN)

The `9f8ba8c` rehydration derives the profile from "a persisted empty workspace can
only be no-fs" (`service.go:1031-1041`). The inference is sound today because every
other path requires a non-empty workspace, but it is a pun: it breaks the day a
second workspace-less profile exists (a memfs-scratch profile, a remote-FS profile),
and both are named candidates in the deferred list.

**Recommendation: add `profile` as an additive, omitempty snapshot field in Phase 1,
and keep the empty-workspace inference as the second defense.** The precedent is
exact: `ProviderPhase` (`sessnap.go:58`, json `"phase,omitempty"`) and `Parts`
(`sessnap.go:62`) were both added additively with no format-tag bump, and the
format-tag contract says the tag changes only if the encoding itself is replaced
(`internal/adapter/grpcdriver/sessionstore.go:17-30`); additive fields ride
`sessnap-json/1` unchanged, so remote drivers store and return the new field
opaquely with zero driver changes. The known downgrade edge is already recorded as
accepted (`DRIVERS.md` Deferred §5): an OLDER harness loading a newer snapshot
silently sheds the field; for the profile specifically, the retained
empty-workspace inference means even that downgrade path stays correct for no-fs,
which is exactly why the inference should not be deleted when the field lands.

### (c) v1 multi-replica stance: session affinity, single writer (OPEN)

Stated as a deployment requirement until Phase 4 leasing: **route every session to
exactly one harness process; never run two processes against the same session id
concurrently.** This is a constraint on the deployer, not a property the code
enforces, and the code today assumes it everywhere a writer exists:

- **jsonlstore is append-only with an in-process mutex only.** `Save` serializes
  through `st.mu` and appends a snapshot line via `O_APPEND` open-write-close
  (`internal/adapter/store/jsonlstore/jsonlstore.go:264-265`); there is no atomic
  rename, no file lock, no cross-process guard, and `Load` takes the last line.
  Two processes appending to one session file interleave at the mercy of OS append
  atomicity, last-write-wins at best.
- **The driver protocol is last-write-wins by contract.** `SaveRequest` carries
  `{session_id, snapshot}` and nothing else, no version, no CAS token, no lease
  (`contracts/proto/mecatl/driver/v1/session_store.proto:96-107`; "Save overwrites:
  Load returns the most recent snapshot saved under the id").
- **The server's run-exclusion is in-process only.** `Service.runs`
  (`service.go:368`) prevents two concurrent runs of one session within a process;
  `loadAndReopen` (`service.go:868`) consults nothing cross-process before
  reopening. Two replicas can each load, reopen, and run the same session, and each
  will happily persist over the other.
- **The GC liveness predicate is process-local**, already recorded: `Service.IsLive`
  sees only this process's runs (`internal/app/childgc.go:77-91`; `DRIVERS.md`
  Deferred §7), mitigated by age ordering and idempotent best-effort deletes, not by
  exclusion.

**Recommendation: state the constraint in `docs/usage.md`/deployment guidance when
the first shared-store deployment ships, and solve it in Phase 4 as a
driver-protocol lease** (lease/renew on `SessionStoreService` or a sibling service),
not as in-process locking; a process-local guard cannot enforce a cross-replica
property, and the flock precedent (memory store) is explicitly single-host. The
Phase 0 inventory confirms sessions are the first and, for now, only resource that
needs a lease; the GC liveness gap rides the same mechanism for free.

## Relationship to other docs

- `DRIVERS.md`: the shipped distribution layer this arc builds on; its "Deliberately
  deferred" list items (cross-process GC liveness, server-wrapper promotion) intersect
  Phases 3-4.
- `IMPLEMENTATION-NOTES.md` "Session profiles": the as-built no-FS profile and
  rehydration detail this doc's framing summarizes.
- The cloud-native kit inventory (PR #54): the upstream speculative subsystem
  inventory; this arc executes its session-state priorities (§1.1-§1.4) and its
  resource-lifetimes advice (#6), and consciously defers its filesystem, forking, and
  execution-environment subsystems behind the no-FS cut.
- `BACKGROUND-SUBAGENTS.md`: issue #28 (session-scoped detach) is gated on Phase 3's
  event log and stays its own arc.
