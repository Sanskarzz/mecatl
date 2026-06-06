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

The loop (`Engine`/`Run`), dispatch, permission pause/resume, compaction, the Task subagent,
and the agent-team `Supervisor`/`TeamTool`. (See `AGENT-TEAMS-SPIKE.md`.)

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

**The Task subagent (read-only explorer) gets the SAME treatment** (Phase 2): when Bash is
configured, `TaskTool` holds a worktree `childForker` (`WithChildForker`) and forks each child
run into a throwaway git worktree BEFORE running it (`buildChildEngine` registers Bash via the
SAME `buildSandboxedCommandRunner`; `buildTaskTool` wires the worktree forker iff a runner
exists; per-def Task engines keep Bash via `scopedToolNamesMode`'s `allowShell` and share the
one forker). So a `Task` to "investigate X" can now `git log`/`git show`/`cat`/build/test in an
isolated checkout — Edit/Write still dropped, no Task/Fork recursion. `TaskTool.ReadOnly()`
stays **true**: isolation (not catalog read-only-ness) is what keeps Task read-parallel — its
writes land in the worktree, never the shared base; a fork FAILURE is a tool error, NOT a
silent fallback to the shared ws. A `WithMaxConcurrentTaskShells` (default 4) semaphore bounds
concurrent worktree-bearing children (Task is read-parallel, so the model can fan out). Same
`gitenv` hardening + same untrusted-`.gitattributes` residual as team members; the
workspace-trust gate is the SHARED follow-up for both Team + Task.

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
Remember/query, ToolSearch, Fork, Team, Task, RepoMap, …); a strict-enforcing OpenAI-compatible
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

**Per-sub-agent provider (SHIPPED, both halves):** a Task agent def / team member may pin a
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
Task/Team) by building a per-session Task tool (+ in-catalog Team tool under `--enable-teams`)
inside `sessionEngineFactory` over the SAME `buildTaskTool`/`buildTeamWiring` builders (no
per-session-catalog drift), wired to the session provider as parent; the Task tool's inline-MCP
close folds into `SessionEngineResult.Close` (torn down by `CloseSession`/`Service.Close`),
bounded by `MaxSessionEngines`. The registry NEVER leaves composition — the child engine gets a
bare `port.LLMProvider`. **DEFERRED:** the standalone gRPC `CreateTeam` RPC stays on the default
provider (no per-CreateTeam selector); `ListAgents`/`AgentInfo` provider surfacing (no proto
change).

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
`meta.thinkingFor` (fed to the anthropic `WithThinkingResolver`). Precedence is PER FIELD: live
when present & `>0`/`Known`, else catalog floor, else the consumer's conservative default; a live
MISS for a model the catalog knows falls back WHOLESALE to the catalog row (live absence NEVER
erases the catalog); with NO lister the store is catalog-seeded ⇒ behaviour byte-identical.
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
