# Spike: A "soul" for mecatl — persistent identity + cross-session user-model

> Status: **Phase 1 SHIPPED** (issue #14). The user-scoped, agent-read-only persona
> fragment is wired (`internal/prompt/soul.go`, `internal/adapter/soul/`, bound in
> `internal/app/build.go`); Phase 2 (the learning loop) remains a proposal.
> Author pass: 2026-06-04.
>
> **Ground-truth corrections applied during implementation** (the as-built wins over
> the sketch below where they conflict):
> - **(A)** The soul is NOT read through the per-session `WorkspaceReader`. That handle
>   is rooted at the SESSION workspace; `~/.config/mecatl/soul.md` lives OUTSIDE any
>   session root. The adapter resolves it against the process environment via an
>   injectable `env` (getenv/userHomeDir/readFile), mirroring `permconfig`/`skills` —
>   exactly as `MemoryIndexAssembler` ignores its `ws` argument.
> - **(C)** No trust-gate in Phase 1. `--trust-project` gates project-sourced ALLOW
>   rules only; user-scoped config is always trusted, and `~/.config/mecatl/soul.md` is
>   user-authored on the user's own box. Trust-gating an *imported* soul is a Phase-2
>   concern, not wired here.
> - **(D)** The content hash is DEFERRED to Phase 2: Phase 1 has no consumer for it (no
>   cache, no `/soul` view, no drift baseline) — the soul is re-read each build. (The
>   adapter optionally logs at Debug for observability; no hash is computed.)
> - The consumer-local port is named **`SoulSource`** (not `SoulReader`).
> Research basis: NousResearch/hermes-agent source read (`~/Development/hermes-dir`),
> the `SOUL.md` community ecosystem, and `docs/harnesses/02` (Twelve Patterns) +
> `08` (Design Considerations). Sources at end.

## 1. The framing correction (read this first)

Issue #14 asks us to study how Hermes implements its "soul" and whether a
**self-modifying soul** fits mecatl. The first finding rewrites the question:

**Hermes's soul is _not_ self-modifying. The agent has no write path to it.**

Nous states the design intent directly: *"the separation of who the agent **is**
(`SOUL.md`) from what the agent **knows** (`MEMORY.md`) is the key architectural
insight."* There are two distinct subsystems, and conflating them is the central
trap:

| | **Who the agent IS** | **What the agent KNOWS** |
|---|---|---|
| Artifact | `SOUL.md` | `MEMORY.md`, `USER.md`, skills |
| Author | **User only** — no agent tool can write it | **Agent** (`memory()` / `skill_manage`) |
| Lifecycle | Static anchor, edited by hand | The "learning loop": nudges → background review → consolidation |
| Prompt slot | Stable tier, **slot #1**, verbatim | Volatile tier |
| Survives compaction by | being re-read from disk each build | being re-read from disk each build |

The "self-improving agent" / "deepening model of who you are across sessions"
marketing is the **right column**. The "soul" is the **left column** — a stable
identity anchor deliberately kept _outside_ the learning loop, precisely so the
loop can't corrupt it.

This matters for mecatl because the two halves have opposite security postures
(§4) and mecatl already owns most of the right column (§5).

## 2. How Hermes implements it (concretely)

### 2.1 The soul (`SOUL.md`) — identity, read-only to the agent

- Lives at `$HERMES_HOME/SOUL.md` (default `~/.hermes/SOUL.md`). Freeform Markdown,
  no enforced schema.
- Loaded by `agent/prompt_builder.py:load_soul_md()`: read → **prompt-injection
  scan** (`tools/threat_patterns.py`, scope `context`) → **truncate** (20k, head/tail)
  → return string or `None`.
- Injected at `stable_parts[0]` in `agent/system_prompt.py:build_system_prompt_parts()`
  — first fragment, verbatim, before tool guidance / memory / context files.
- Bootstrapped from `hermes_cli/default_soul.py:DEFAULT_SOUL_MD` on first run
  (`hermes_cli/config.py:_ensure_default_soul_md`). **Existing files are never
  overwritten.**
- **No tool writes it.** Only the user (text editor) or a profile-distribution
  `git` update can change it. The full system prompt is cached on
  `agent._cached_system_prompt` and rebuilt only after compaction.

### 2.2 The learning loop — user-model + skills, agent-writable

- **`MEMORY.md`** (env/project facts) and **`USER.md`** (model of the user), both
  under `$HERMES_HOME/memories/`, `§`-delimited, char-capped (2200 / 1375). Managed
  by `tools/memory_tool.py:MemoryStore`; injected into the **volatile** tier.
- **Nudge triggers** (`agent/conversation_loop.py`): a turn counter
  (`memory.nudge_interval`, default 10) and a tool-iteration counter
  (`skills.creation_nudge_interval`) set "review" flags.
- **Background review fork** (`agent/background_review.py`): on a nudge, Hermes
  forks an `AIAgent` in a daemon thread that **inherits the parent's cached system
  prompt verbatim** (for prefix-cache reuse), is handed the transcript, and is
  prompted to extract user persona/preferences/corrections → writes them back via
  the memory tool. Recursion disabled on the fork.
- **Session search** (`tools/session_search_tool.py`): FTS5 over `~/.hermes/state.db`
  for cross-session recall.
- **Optional Honcho plugin** (`agent/memory_provider.py`): external _dialectic_
  user modeling, recalled per-turn and injected into the volatile tier.

## 3. Convention status — `SOUL.md` is not a standard

Unlike `SKILL.md` (formal spec at agentskills.io, Linux-Foundation-governed,
adopted by Anthropic + OpenAI), `SOUL.md` is a **de facto community convention**
with no ratified schema. Two reference shapes exist: a slim Hermes-canonical one
(identity one-liner → style → avoid → posture) and a richer community template
(worldview, opinions-by-domain, tensions/contradictions, boundaries). Adopting it
buys ecosystem familiarity, not interoperability guarantees.

## 4. Security & governance — the part that matters most for mecatl

The community (hermes-soul-governance, prompt-security/clawsec `soul-guardian`,
relic, SoulTavern) independently converged on one lesson:

> **A _writable_ identity anchor is broken.**

Three threats, all directly in mecatl's `internal/governance` wheelhouse:

1. **Prompt injection _into_ identity.** Untrusted content (a fetched page, a
   cloned repo's file, an imported persona) rewrites who the agent is —
   _persistently, across all future sessions_. This is strictly worse than a
   single-turn injection. Hermes's mitigation: the soul has no write path + a
   load-time injection scan. The scan is necessary-not-sufficient.
2. **Drift via legitimate writes.** Behavioral _rules_ mistakenly stored in the
   writable memory degrade under compression. This is the entire motivation for
   the `hermes-soul-governance` project. Conclusion: rules belong in the
   read-only anchor, never in agent-curated memory.
3. **Portability / supply-chain.** `relic`'s "one soul, many agents, bidirectional
   sync" means write access to one agent's workspace compromises every connected
   agent's identity. `SoulTavern` treats _all imported persona content as untrusted_
   and wraps it in an operator-level identity directive + trust banner + sanitiser.

**How this maps onto mecatl (the encouraging part).** mecatl already has the exact
primitives this threat model demands:

- **Deny-dominant scope hierarchy** + `--trust-project` gate → a soul imported from
  a project/external source is _untrusted by default_, same as project permission
  allows.
- **Read-only `WorkspaceReader`** (issue #13) → the soul file is loaded through a
  handle that _cannot mutate_, by construction.
- **`ScopeUser`** already exists (`~/.config/mecatl/settings.yaml`) as the one
  user-global, non-project scope → the natural home for a user-scoped soul.

Design rule we adopt from this: **the soul is read-only to the agent loop, lives in
a user scope, is injection-scanned + content-hashed at load, and an externally
imported soul is trust-gated exactly like a project allow.**

## 5. What mecatl already has

mecatl already owns most of the **right column** (what-it-knows):

- **Two-layer prompt** (`internal/prompt/builder.go`): `StablePrefix` (cache-stable,
  `Config.Role`/`Tone`/`Safety`) + `VolatileSuffix` (`<env>`).
- **`InstructionAssembler` / `MultiAssembler`** (`internal/prompt/instructions.go`):
  the turn-0 context-injection chain. `RootAssembler` (AGENTS.md > CLAUDE.md) and
  `MemoryIndexAssembler` already ride it. **This is the seam.**
- **`MemoryIndexSource`** (`internal/prompt/memoryindex.go:19`): the existing
  _consumer-local port_ pattern — `prompt` declares a minimal interface the adapter
  satisfies structurally, no import cycle. The template for a soul source.
- **Memory store** (`internal/adapter/memory/store.go`): BM25 search, flock-safe,
  but **project-scoped**, not per-user.
- **Dream consolidation** (`internal/adapter/dream/dream.go`): Pattern 4 — conservative
  merge/forget on a ticker. The GC half of a learning loop already exists.
- **Hooks** (`port.HookRunner`): `Stop` / `SessionStart` attachment points.
- **Agent definitions** (`internal/adapter/agents/`): could define a soul-reflection
  subagent.

**Gaps** (what's missing for both halves):

- **G1** Memory is project-scoped; a soul needs **user scope** (`~/.config/mecatl/`).
- **G6/G8** No user-identity concept; `ScopeUser` holds only permission rules, not
  persona/preferences.
- No agent-read-only persona fragment in the prompt today (`Config.Role` is one
  static string, baked into the cache-stable prefix — wrong place for per-user
  identity).
- No transcript-as-corpus access for cross-session inference; no nudge/background
  review trigger that _generates_ new entries (dream only _compacts_ existing ones).

## 6. Proposal — two phases, persona first

### Phase 1 — the persona/soul (low-risk, high-fit). RECOMMENDED.

A user-scoped, **agent-read-only** identity fragment, injected as a turn-0 user
message (not the cache-stable prefix), with governance-grade load discipline.

```
internal/prompt/soul.go        SoulSource (consumer-local port, mirrors MemoryIndexSource)
                               SoulAssembler implements InstructionAssembler
internal/adapter/soul/         Store over ~/.config/mecatl/soul.md (env-injected,
                               NOT the WorkspaceReader — the file is outside any session root)
                                 - Load: resolve → read → trim → byte-cap → injection-scan
                                   (reuses skills.ScanForInjection); fail-soft to "" at each branch
                                 - no Write/Create/WriteFragment anywhere (no agent path)
internal/app/build.go          wire SoulAssembler into buildInstructionAssembler,
                               after RootAssembler and BEFORE MemoryIndexAssembler,
                               fenced in <soul>…</soul>
```

> As-built note: the hash step in the original sketch is DEFERRED to Phase 2 (no Phase-1
> consumer); there is no trust-gate (user-scoped config is always trusted). On by
> default reading the conventional path; `--soul-file` overrides it, `--no-soul` disables.

Properties, each tracing to §4:

- **Read-only to the agent** — no tool, no `WriteFragment`. (threat 1, 2)
- **User-scoped** — `~/.config/mecatl/soul.md`, applies across all projects. (G1)
- **Turn-0 user message, data-fenced** — never the stable prefix (keeps cache
  byte-stability; matches how `MemoryIndexAssembler` injects). Survives compaction
  by being re-read from disk each build.
- **Injection-scanned + byte-capped at load** (reusing `skills.ScanForInjection`); a
  hit/oversize degrades to no fragment. The content hash and trust-gating of an
  *imported* soul are deferred to Phase 2 (Phase 1 reads only the user-authored,
  always-trusted user-scoped path). (threats 1, 3)
- Fail-soft: a missing/empty/oversized/flagged soul degrades to no fragment, never
  an error.

Phase 1 is small, violates no layering rule, reuses an established seam, and lands
squarely inside mecatl's existing governance story. It is the spike's recommended
deliverable.

### Phase 2 — the user-model learning loop (bigger, optional, gated)

The "what it knows about _you_" half: a user-scoped `USER.md`-equivalent the agent
_does_ curate, plus a background review pass.

- **User-scoped memory partition** — either a `user/` key namespace in the existing
  store, or a sibling `memory.Store` rooted at `~/.config/mecatl/`. Visible to the
  existing `Recall`/`SearchMemory` tools.
- **Background review on `Stop`** — a hook (or a forked single-shot engine, like
  `Task`) that reads the just-finished transcript and proposes new user-model
  entries. Reuses the fork machinery mecatl already has.
- **Consolidation** — point a `dream.Consolidator` (with a `user/` prefix) at the
  partition. The GC half already exists.

Phase 2 carries the harder questions (over-eager memory — the dominant failure mode
per `docs/harnesses/08`; cost of an extra model call per turn; whether the user-model
is _rules_ (→ read-only, belongs in the soul) or _facts_ (→ writable memory)). It
should ship behind a flag and only after Phase 1.

## 7. Feasibility & recommendation

- **Persona (Phase 1): feasible and well-fitted.** Clean seam, no new domain types
  beyond a consumer-local port, no layering violation, and it _strengthens_ the
  governance story rather than straining it. Recommend proceeding to a design pass.
- **Learning loop (Phase 2): feasible but with real risk surface.** mecatl owns the
  GC and fork primitives; the open risk is over-eager/low-signal user-modeling and
  per-turn cost. Recommend deferring behind Phase 1 and a flag.
- **Naming:** adopt `soul.md`/`SoulAssembler` for ecosystem familiarity, but treat
  `SOUL.md` as convention, not contract — no interop promise (§3).

### Open questions

1. Is the persona a **scope** in the governance sense, or just an instruction
   fragment? (If a scope, where does it sit relative to `ScopeManaged`? It must
   **not** be able to loosen a configured Ask — same invariant as issue #13.)
2. Multi-user: the embedded/gateway surfaces are effectively single-user today.
   Does a soul need a user-identity key, or is "the operator" implicitly singular?
3. Do we want a `/soul` TUI affordance (view current soul + its hash + trust state),
   mirroring `/agents` and `/skills`?

## Sources

- NousResearch/hermes-agent (source read): `agent/prompt_builder.py`,
  `agent/system_prompt.py`, `agent/background_review.py`, `agent/conversation_loop.py`,
  `tools/memory_tool.py`, `hermes_cli/default_soul.py`, `agent/memory_provider.py`.
- Hermes docs: `website/docs/user-guide/features/personality.md`;
  hermes-agent.nousresearch.com/docs.
- Ecosystem: jangyuxue/hermes-soul-governance, prompt-security/clawsec
  (`soul-guardian`), LucioLiu/relic, imphillip/SoulTavern, aaronjmars/soul.md.
- mecatl: `internal/prompt/{builder,instructions,memoryindex}.go`,
  `internal/adapter/{memory,dream}`, `internal/governance/permission.go`,
  `docs/harnesses/02-twelve-patterns.md`, `docs/harnesses/08-design-considerations.md`.
