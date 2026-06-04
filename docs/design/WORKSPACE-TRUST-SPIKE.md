# Workspace Trust — implementation plan (Phases 0+1+2)

> **STATUS: Phases 0 + 1 SHIPPED; Phase 2 is implementation-ready plan.**
> Phase 0 (unify the default — mecatui resolves `--trust-project`, default
> false, no longer hardcoding trust ON) is wired and tested. Phase 1
> (declarative `trustedWorkspaces:` list + the composition `TrustDecision`
> resolver) is wired and tested (`internal/adapter/workspacetrust`,
> `internal/app/trust.go`). Phase 2 (prompt-and-remember + complete gate +
> drift) is not yet built. Phase 3 (in-TUI trust modal, per-scope trust,
> speculative schema reservations) is **explicitly out of scope** — see §11.
>
> File:line citations are to the tree as of this spike; verify before
> implementing.

## 1. Problem (recap)

"Trust" of a workspace is a **per-invocation boolean** (`Config.TrustProject`,
`internal/app/build.go:237`) from `--trust-project` (mecated:
`cmd/mecated/main.go:649,760`, default **OFF**) and **hardcoded TRUE** in
mecatui embedded (`cmd/mecatui/main.go:267`). It is **not persisted**.

The user wants: detect a project's steering files on startup, prompt for trust
if not already trusted, and **remember** the decision (no re-prompt every
launch). The remembered decision must be **machine-written state**, not jammed
into the human-authored `~/.config/mecatl/settings.yaml` (which `permconfig`
only ever reads — `resolve.go:316-325`). The settings-vs-state split is the
panel-validated foundation: machine state goes in `<xdg>/mecatl/trust.yaml`,
sibling to but never inside `settings.yaml`.

---

## 2. Terminology — and what "untrusted" does NOT take away

The single most important framing correction (vs an earlier draft): **an
untrusted workspace must still be a fully usable coding agent.** Trust is NOT
"the agent refuses to function on a repo you haven't blessed" — that would make
the harness unusable on every freshly-cloned repo, which no real agent (Claude
Code included) does. Claude Code's folder-trust prompt gates *auto-execution
and the repo's ability to pre-authorize/re-persona the agent*; it does **not**
disable the agent. We match that.

So we draw the line between the agent's **own capability** (never gated) and the
repo's **injected steering/authority** (gated):

- **Always available, on ANY repo, trusted or not — NEVER gated:**
  - the built-in tools (Read, Edit, Bash, etc.) and the whole agent loop;
  - the base system prompt;
  - the **operator's OWN** user-tier config under `~/.config/mecatl` /
    `~/.claude` — the user soul, user agent defs, user commands, user skills,
    user-global permission rules. None of this is repo-sourced; an untrusted
    repo cannot touch it (each adapter already separates a USER tier from a
    PROJECT tier — e.g. `skills/resolve.go:76-90`);
  - every **DENY / ASK** rule from any scope (they only tighten);
  - the permission prompt itself. On an untrusted repo the agent still runs;
    it just **asks** for the tool calls the repo would have auto-allowed.

  An untrusted repo therefore degrades to **"ask the human" mode**, not **"do
  nothing" mode.** Read/edit/run-with-a-prompt all work.

- **Gated by trust — the PROJECT-INJECTED steering/authority set** (the thing
  the trust gate withholds when untrusted):
  - project permission **ALLOW** rules (auto-approval the repo grants itself);
  - the project **soul** (a repo rewriting the agent's persona);
  - **project-tier** agent definitions, slash commands, and skills — i.e. the
    `<workspace>/.mecatl/*` and `<workspace>/.claude/*` tiers ONLY. The
    user-tier equivalents stay active.

  This is the "project authority set." Withholding it makes the agent *more
  cautious on an unknown repo*, not *broken*.

- **Identity anchor** (the drift sub-concept) — *the high-signal, rarely-edited
  subset of the project authority set whose change should re-prompt.* Members:
  the project **soul**, project **agent-definition** bodies, **command**
  definitions, and **skill** definitions. **Explicitly NOT `settings.yaml`**
  (edited every commit → drift nag → blind-click trust → defeats the premise).

The trust gate withholds the **project authority set**. The drift re-prompt is
anchored on the **identity anchor** only. (MUST-FIX 1, resolved in §4.)

---

## 3. Recommendation (lead)

Build **(d) hybrid, phased 0→1→2**:

- **Phase 0** — flip mecatui's hardcoded `TrustProject: true` to resolve from
  `--trust-project` (default false), unifying it with mecated. A **standalone
  first commit** that removes the blanket-trust regression with zero new
  machinery.
- **Phase 1** — declarative trust: a `trustedWorkspaces:` list read from
  `settings.yaml`, plus the composition-level `TrustDecision` resolver
  (MUST-FIX 2). No prompt, no writes. Serves CI / mecated / power-users and
  gives durable no-re-prompt trust.
- **Phase 2** — the interactive prompt-and-remember: a machine-written
  `<xdg>/mecatl/trust.yaml` registry keyed by absolute path, the **identity
  anchor** drift check, completion of the admission gate to cover the
  **project tier** of agents/commands/skills, and the mecatui **pre-TUI**
  prompt that writes it. mecated stays declarative (reads the registry; never
  prompts/writes).

The trust resolution composes one answer consumed by every member of the
project authority set.

---

## 4. MUST-FIX 1 — admission vs drift, and the agents/commands/skills gap

### 4.1 Two separate concerns, resolved

| Concern | Covers | Mechanism |
|---|---|---|
| **Admission** (the trust gate) | the **project authority set**: project ALLOW rules + project soul + **project-tier** agents/commands/skills | when untrusted, the repo's INJECTED steering is withheld — the agent's own tools, the base prompt, and the operator's USER-tier config stay fully active |
| **Drift re-prompt** | the **identity anchor** only: soul + agent/command/skill **definitions** (NOT settings.yaml) | a changed anchor hash ⇒ re-prompt (mecatui) / re-gate-to-safe (mecated) |

**What untrusted does NOT remove** (see §2): built-in tools, the loop, the base
prompt, all user-tier config, every Deny/Ask, and the permission prompt itself.
An untrusted repo gives a working agent in "ask the human" mode — it just
doesn't auto-approve the repo's own grants or adopt the repo's persona.

Rationale (the panel's tension, resolved):
- **Admission must be complete WITHIN the project authority set** or the gate is
  security theatre: a trusted repo could ship a malicious agent persona or slash
  command today and trip no alarm because project-tier agents/commands/skills are
  admitted regardless of trust (confirmed below). So the gate must cover the
  project tier of all three — but ONLY the project tier; the user tier and the
  built-in capability are never gated.
- **Drift must be tuned to signal**: `settings.yaml` changes on nearly every
  commit; hashing it for drift = constant re-prompt = nag-fatigue = users
  blind-clicking "trust" = the premise defeated. So `settings.yaml` is **in the
  admission set but NOT in the drift anchor.** The soul + agent/command/skill
  *definitions* are rarely-edited identity/executable content — high-signal for
  drift. A re-bless gesture (§7) accepts an intended anchor edit.

Net: trusting a repo admits its full project authority set; editing its **permissions**
does not re-prompt (those re-resolve live via permconfig's own mtime cache —
`resolve.go:174-192` — and were already trusted); editing its **identity
anchor** (persona/commands/skills/soul) does re-prompt.

### 4.2 The gap is REAL — current state (verified)

`--trust-project` today gates **only** permission ALLOW rules
(`permconfig.applyTrustGate`, `resolve.go:269-282`) and the project soul
(`soulselect.go:262-266`). It does **NOT** gate:

- **agent definitions** — `agentdefs`/`agents` discovery
  (`agents/agentdef.go:22-26` calls them "operator-controlled" and gates only
  on the conventional **on/off** toggle, not on trust);
- **slash commands** — `prompt.NewDirCommandExpander` (`build.go:775-781`),
  on/off only;
- **skills** — `skills.ResolveSources` (`build.go:1115-1147`,
  `skills/resolve.go:42-58`) mixes **project** and **user** conventional paths
  in one ordered list with **no trust split** — the project tier is admitted
  whenever `Conventional` is on.

So a malicious cloned repo with `.claude/agents/evil.md` or
`.mecatl/commands/x.md` steers the model on first run with no gate. **This is
the same class of risk `--trust-project` was created to close for allows/soul,
left open for the other three surfaces.**

### 4.3 CRITICAL DECISION — completing the gate is **IN this feature (Phase 2)**

Completing the gate to cover agents/commands/skills is **part of this
feature**, done in **Phase 2**, because:

- Leaving it out ships a `trust.yaml` whose admission story is a half-truth —
  exactly the "incomplete hash" the panel rejected. The doc would be claiming
  "trusted = the repo can steer you" while three steering channels ignore it.
- The cost is **bounded and localized to composition** (`internal/app`): the
  gate is a *withhold-project-tier-when-untrusted* decision at the point each
  source list is built. It does **not** require changing the
  `agents`/`commands` adapters' signatures — composition decides whether to
  *include the project-tier sources at all*.

**Sizing (so scope doesn't balloon):**
- **commands** — small: `buildCommandExpander` (`build.go:775-781`) already
  takes a dir; when untrusted, skip the **project** command dir (keep user
  dirs). One branch.
- **agents** — small–medium: project-tier conventional dirs are added in the
  agentdefs source resolution; when untrusted, omit the project tier (keep
  user/explicit). A source-list filter in composition, mirroring permconfig's
  `applyTrustGate` shape but applied at discovery rather than post-load.
- **skills** — medium: `skills.ResolveSources` (`resolve.go:56-58`) returns one
  flat ordered list folding project+user. To gate, composition must
  **distinguish the project-tier sources** so it can drop them when untrusted.
  Cleanest: add `ResolveOptions.IncludeProjectTier bool` (default true; set
  false when untrusted). **This is the one real adapter touch** — a narrow,
  additive option, not a signature break.

If skills-gating proves larger than estimated during implementation, the
fallback (documented, not silent) is: ship Phase 2 with allows+soul+agents+
commands gated, and file skills-gating as the single follow-up
(`#TRUST-SKILLS-GATE`), with the gap stated in §11. But the **default plan is
all five gated in Phase 2.**

---

## 5. MUST-FIX 2 — TrustDecision (not a bare bool)

Produced once per workspace in **composition** (`internal/app`); its `.Trusted`
field feeds `permconfig.Options.TrustProject` and the agents/commands/skills
gates (so adapters still take a `bool` — no churn); its `Source`/`Drifted`
fields drive **narration** mirroring `soulMeta` (`soulselect.go:88-110`).

```go
// internal/app (composition), NOT a domain or adapter type.
type TrustSource int
const (
    TrustNone      TrustSource = iota // not trusted
    TrustFlag                         // --trust-project one-shot override
    TrustDeclared                     // settings.yaml trustedWorkspaces
    TrustRemembered                   // trust.yaml registry entry (hash matched)
)

type TrustDecision struct {
    Trusted bool        // the effective bool fed to consumers
    Source  TrustSource // why (for the log narration)
    Drifted bool        // a registry entry existed but its anchor hash mismatched
}
```

- **Produced by** a new `internal/app` helper `resolveTrust(cfg) TrustDecision`
  (Phase 1 introduces it folding flag+declared; Phase 2 adds registry+drift).
- **Consumed by**: `buildEngine`/`buildCatalog` (`build.go:491,879`) feed
  `decision.Trusted` into `permconfig.Options.TrustProject` (`build.go:518`),
  into the soul gate (`soulselect.go:151`), and into the new agents/commands/
  skills project-tier gates.
- **Narrated**: composition logs `slog.Info("workspace trust", "trusted",
  d.Trusted, "source", d.Source, "drifted", d.Drifted, "root", root)` — so the
  why-trusted/drift story is visible, mirroring the soul narration. A `Drifted`
  decision logs `slog.Warn` in the `applyTrustGate` drop-report style.

---

## 6. MUST-FIX 3 — soul:apply double-gate, reconciled

The project soul is admitted only if **BOTH** gates pass; they compose as a
logical AND, in this order:

1. **Trust gate (admission)** — `TrustDecision.Trusted` must be true, else the
   project soul is withheld before the permission gate is even consulted. This
   is the *provenance* gate: "is this repo allowed to set a persona at all?"
   (`selectSoulSource`, `soulselect.go:262-266`, with `cfg.TrustProject`
   replaced by `decision.Trusted`).
2. **`soul:apply` permission gate (policy)** — even on a trusted repo, the
   synthetic `soul:apply` action (`buildSoulGate` → `EvaluateWith`,
   `soulselect.go:145-175`) can Deny/Ask the soul. Allow ⇒ apply; Deny/Ask ⇒
   withhold (`soulselect.go:201-213`).

**Why AND, in this order:** trust answers *provenance* (may this repo
contribute at all), `soul:apply` answers *policy* (does the operator's
permission config permit applying a soul this run). An untrusted repo's soul
never reaches the policy gate; a trusted repo's soul still obeys an explicit
`soul:apply: deny`. The two are independent and both must pass — this is the
existing behaviour made explicit, with `decision.Trusted` substituted for the
raw bool. No change to `buildSoulGate`'s internals.

Note: `buildSoulGate` *also* resolves project permission rules via permconfig,
which are themselves trust-gated. With `decision.Trusted` feeding
`permconfig.Options.TrustProject`, a `soul:apply` ALLOW *in the project's*
`settings.yaml` is honoured only when trusted — consistent and non-circular
(the trust decision is computed before the gate is built).

---

## 7. MUST-FIX 4 — share the hash helper, keep the anchors parallel

Two concerns, two mechanisms, **one** shared primitive:

- **Extract** `internal/adapter/hashutil` (or a tiny `cleanhash` leaf):
  `func SHA256Hex(b []byte) string` returning lowercase-hex SHA-256, the exact
  discipline `soul.LoadWithMeta` / `soulguard` already compute. Both subsystems
  call it.
- **soulguard stays as-is** in shape: a **sidecar** `soul.md.sha256` next to
  the soul file, soul-only anchor, re-blessed by `--approve-soul`
  (`soulguard.go:163-166`). Do **not** route it through the trust registry.
- **workspacetrust** has its **own** anchor: the **identity-anchor hash** (soul
  ⊕ agent defs ⊕ command defs ⊕ skill defs), stored **in the registry entry**
  (not a sidecar), re-blessed by re-answering the prompt (or a `--approve-trust`
  one-shot, §11). Different surface, different store, different gesture.

They share only `SHA256Hex`. We are NOT generalizing soulguard into a trust
subsystem.

> **Anchor composition.** The identity-anchor hash folds the `SHA256Hex` of
> each member file's clean bytes, over the files in a **fixed sorted order**,
> with absent files contributing a stable `"absent:<relpath>"` marker (so
> *adding* a persona later registers as drift). This is computed in
> `workspacetrust`; the exact member list is the §2 identity anchor.

---

## 8. MUST-FIX 5 — security specifics

1. **Path keying (symlink/alias).** Registry keyed by `filepath.Clean` +
   `filepath.EvalSymlinks` **realpath**. Store BOTH the cleaned path the
   operator saw and the resolved realpath; a lookup matches only if **both**
   agree with the live resolution. This defeats a moved-symlink swap (point an
   alias at a trusted realpath to inherit trust) — a path cannot *forge* trust
   it wasn't granted, and a realpath whose symlink alias changed no longer
   matches. EvalSymlinks failure (broken link) ⇒ **untrusted** (fail-safe).
2. **TOCTOU — single coherent read.** The trust decision and the config read
   must be coherent: compute the identity-anchor hash from the **same bytes**
   used to admit. Concretely, `workspacetrust` reads each anchor file **once**,
   computes the hash, AND that read is the one composition uses to admit the
   project authority set — no second open between check and use where the file
   could be swapped. Where a single read isn't structurally possible (permconfig reads
   `settings.yaml` separately), the residual window is the same narrow
   single-user-desktop window `soulguard` already accepts; documented, not
   over-engineered. (settings.yaml is NOT in the drift anchor, so its TOCTOU
   surface is only the admit-or-not bool, which fails safe.)
3. **Prompt-path sanitization (CWE-150).** The pre-TUI prompt echoes the repo
   path and the discovered filenames. These are attacker-influencable (a repo
   dir/file name can embed terminal escapes to spoof the prompt). **Strip
   terminal escapes** before echoing — reuse the existing `sanitizeTerminal`
   discipline the permission modal already applies (`ui/permission.go:36-39`).
   The pre-TUI prompt is composition-side (not `ui/`), so it needs its own small
   sanitizer or a shared `cmd/mecatui` helper; spec it as a reused function, not
   a reinvented one.
4. **Fail-direction.** An unreadable / missing / corrupt / wrong-version
   `trust.yaml` ⇒ resolve to **untrusted** (`TrustNone`), logged at
   `slog.Warn`, never an error that aborts startup (fail-soft + fail-safe,
   matching permconfig/soulguard). A malformed `trustedWorkspaces` entry ⇒ that
   entry ignored, rest honoured. A corrupt registry never *grants* trust.
5. **No lower-priority override of a higher-priority untrust/deny.** Trust is
   **monotonic-positive only**: the three positive inputs (flag / declared /
   remembered) can only *grant* trust; none can *revoke* a Deny or downgrade
   another. There is no "untrust" input that a lower input could override —
   absence of a grant IS untrust. And trust grants only **admission**; it never
   touches the permission evaluator's deny-dominance (`applyTrustGate` already
   only adds back ALLOW rules, never suppresses Deny/Ask — `resolve.go:269-282`,
   unchanged). So no path lets trust override a permission Deny or a configured
   Ask. **Test asserts this explicitly** (§9 per-phase tests).

---

## 9. Per-phase implementation plan

### Phase 0 — unify the default (standalone commit, no new machinery) — **SHIPPED**

**Goal:** remove mecatui's blanket-trust regression; both composition roots
default to untrusted, honour `--trust-project`.

**Status:** SHIPPED. mecatui gained a `--trust-project` flag (default false) in
`cmd/mecatui/config.go`, mapped onto `app.Config.TrustProject` in
`embeddedConfig` (replacing the hardcoded `true`). Tests:
`TestParseFlagsTrustProject`, `TestEmbeddedConfigMapsTrustProject`,
`TestEmbeddedConfigPermissionPosture` (the former `TestEmbeddedConfigTrustsPermissions`,
inverted to assert default-false). Docs updated: `docs/tui.md`, `docs/usage.md`.

**Requirements (testable):**
- **R0.1** mecatui's embedded config no longer hardcodes `TrustProject: true`;
  it resolves from a `--trust-project` flag (new in mecatui), default **false**.
- **R0.2** mecated behaviour unchanged (already default-false flag).
- **R0.3** No persistence, no prompt, no registry yet.

**File-level changes:**
- `cmd/mecatui/main.go:267` — remove `TrustProject: true`; set from a new
  `cfg.trustProject` flag (default false).
- `cmd/mecatui/config.go` + flag parsing — add `--trust-project` flag.
- `cmd/mecatui/config_test.go:36-37` — invert the assertion (default false).

**Tests:** mecatui default config has `TrustProject==false`; `--trust-project`
flips it true; mecated unchanged.

**Docs:** `docs/tui.md`, `docs/usage.md` — note the default flip + the new flag.

---

### Phase 1 — declarative trust + TrustDecision resolver — **SHIPPED**

**Goal:** durable, no-prompt trust via `settings.yaml` `trustedWorkspaces:`,
behind a real `TrustDecision`.

**Status:** SHIPPED. The `internal/adapter/workspacetrust` leaf reads
`trustedWorkspaces: []string` from `<xdg>/mecatl/settings.yaml` (realpath-keyed,
fail-safe). `internal/app/trust.go` introduces `TrustSource`/`TrustDecision` and
`resolveTrust(cfg)`, folding `--trust-project` (`TrustFlag`) > a declared match
(`TrustDeclared`) > none. `Build` collapses the decision onto `cfg.TrustProject`
before the downstream build, so permconfig's `Options.TrustProject` and the soul
provenance gate both honour declared trust through the EXACT same monotonic-positive
admission path (no new bypass; deny/ask unchanged). Composition narrates the
decision (`slog.Info "workspace trust"`). Tests:
`internal/adapter/workspacetrust/trust_test.go` (parse, match/no-match, missing
key, malformed fail-safe, blank-entry skip, symlink-alias match, alias-cannot-forge);
`internal/app/trust_test.go` (`TestResolveTrustFlagWins`, `TestResolveTrustDeclared`,
`TestResolveTrustNone`, `TestResolveTrustFlagWinsSourceOverDeclared`,
`TestResolveTrustMalformedEntryFailSafe`, `TestResolveTrustEmptyWorkspaceNoDeclared`,
`TestDeclaredTrustFeedsSoulGate`, `TestNonDeclaredDropsSoul`,
`TestResolveTrustMonotonicPositiveDenyHonoured`). Docs updated: `docs/usage.md`,
`docs/architecture.md`, CLAUDE.md. `Drifted` stays false/unused until Phase 2.

**Requirements (testable):**
- **R1.1** A new `internal/adapter/workspacetrust` leaf reads
  `trustedWorkspaces: []string` from `<xdg>/mecatl/settings.yaml` via the shared
  `xdgconfig.ResolveEnv` (offline-testable). Trust parsing lives **with trust**,
  not in the permission YAML parser.
- **R1.2** `internal/app.resolveTrust(cfg) TrustDecision` folds, highest first:
  `--trust-project` (⇒ `TrustFlag`) > a `trustedWorkspaces` match on the
  realpath-keyed workspace (⇒ `TrustDeclared`) > none (⇒ `TrustNone`).
- **R1.3** `decision.Trusted` feeds `permconfig.Options.TrustProject`
  (`build.go:518`) and the soul gate (`soulselect.go:151`) — replacing the raw
  `cfg.TrustProject`.
- **R1.4** Composition narrates `slog.Info("workspace trust", source, trusted)`.
- **R1.5** Path keying is realpath+cleaned (MUST-FIX 5.1); EvalSymlinks failure
  ⇒ untrusted.
- **R1.6** A malformed `trustedWorkspaces` entry is ignored fail-safe; a
  missing key ⇒ untrusted.

**File-level changes:**
- **new** `internal/adapter/workspacetrust/{trust.go,trust_test.go}` —
  `trustedWorkspaces` reader; realpath keying; `xdgconfig` env; the
  `SHA256Hex` helper consumer (helper itself extracted here or in `hashutil`).
- **new** `internal/app/trust.go` — `TrustDecision`, `TrustSource`,
  `resolveTrust` (flag + declared only this phase).
- `internal/app/build.go:518` — feed `decision.Trusted`.
- `internal/app/soulselect.go:151,262` — consume `decision.Trusted`.
- `internal/adapter/permconfig/` — no change (still takes a bool).

**Tests:**
- declared workspace ⇒ `TrustDeclared`, allows honoured + project soul loaded;
- non-declared ⇒ `TrustNone`, allows dropped + soul withheld;
- `--trust-project` ⇒ `TrustFlag` regardless of declaration;
- realpath aliasing: a symlinked alias of a declared realpath resolves to
  trusted; a declared path whose symlink was repointed ⇒ untrusted;
- malformed `trustedWorkspaces` entry ignored, rest honoured;
- **security:** assert trust never adds back a Deny/Ask (MUST-FIX 5.5) — a repo
  with a project Deny stays denied even when declared-trusted.

**Docs:** `docs/usage.md` — document `trustedWorkspaces:` (read-only,
operator-authored, CI/daemon use). `docs/architecture.md` — the
settings-vs-state split + the `TrustDecision` resolver.

---

### Phase 2 — interactive prompt-and-remember + complete the gate + drift

**Goal:** the requested UX — detect, prompt, remember — with a complete
admission gate and identity-anchor drift.

**Requirements (testable):**

*Registry (remembered trust):*
- **R2.1** `workspacetrust` reads/writes `<xdg>/mecatl/trust.yaml`: a map keyed
  by `{cleaned, realpath}` ⇒ `{anchorSHA256, trustedAt}`. Written via an
  `O_NOFOLLOW`, 0o600, temp-then-rename seam (the `soulguard` discipline,
  `soulguard.go:69-80`), injectable for offline tests.
- **R2.2** `resolveTrust` gains the `TrustRemembered` tier (below `TrustFlag`
  and `TrustDeclared`): a registry entry whose `anchorSHA256` **matches** the
  live identity-anchor hash ⇒ `Trusted, TrustRemembered`. A present entry whose
  hash **mismatches** ⇒ `Trusted=false, Drifted=true`.
- **R2.3** Corrupt/unreadable/wrong-version `trust.yaml` ⇒ `TrustNone`, never a
  grant (MUST-FIX 5.4).

*Complete admission gate (MUST-FIX 1):*
- **R2.4** When `!decision.Trusted`, ONLY the **project-tier** sources are
  withheld for: agent definitions, slash commands, AND skills — in addition to
  the existing project-allows + project-soul gating. **The built-in tools, the
  base prompt, every user-tier source (user soul/agents/commands/skills,
  user-global permission rules), every Deny/Ask, and the permission prompt
  itself stay fully active** (§2). The untrusted agent works in "ask the human"
  mode; it is never disabled.
- **R2.5** Skills gating uses an additive `ResolveOptions.IncludeProjectTier`
  (default true) so composition can drop the project tier when untrusted — the
  one narrow adapter touch; no signature break.
- **R2.6** Each withheld project-tier source logs a `slog.Warn` drop-report
  line (mirroring `applyTrustGate`, `resolve.go:276`).

*Drift (identity anchor only):*
- **R2.7** The identity-anchor hash = `SHA256Hex`-fold of soul ⊕ project agent
  defs ⊕ project command defs ⊕ project skill defs, fixed sorted order, absent
  marked. **`settings.yaml` is NOT in the anchor** (MUST-FIX 1).
- **R2.8** A drift (entry exists, anchor mismatches) ⇒ mecatui **re-prompts**;
  mecated **re-gates to safe** (untrusted) + `slog.Warn`.

*mecatui pre-TUI prompt:*
- **R2.9** Before the TUI starts (the pre-alt-screen window, near
  `cmd/mecatui/main.go:58-64`), if the workspace has any project authority-set
  member present AND `resolveTrust` returns `TrustNone` or `Drifted`, prompt on
  the terminal: **[t]rust / [o]nce / [n]o** (default no).
  - `t` ⇒ write the registry entry (realpath + current anchor hash) ⇒ trusted
    this and future runs.
  - `o` ⇒ trusted this run only; write nothing.
  - `n` ⇒ untrusted (the safe default).
- **R2.10** The prompt echoes repo path + discovered filenames **sanitized**
  (terminal escapes stripped, MUST-FIX 5.3).
- **R2.11** The decision + the registry write are **composition-side**
  (`cmd/mecatui` main); `ui/theme/client` are untouched (render-layer rule).
- **R2.12** mecated **never prompts and never writes** `trust.yaml`; it reads
  it (declarative). A repo trusted in mecatui is honoured by mecated for the
  **same user** (shared `$XDG_CONFIG_HOME`); cross-user is not shared (correct).

**File-level changes:**
- `internal/adapter/workspacetrust/` — add `trust.yaml` read/write
  (`O_NOFOLLOW`/0o600/temp-rename), the identity-anchor hash computation, the
  registry schema; injectable IO seam.
- `internal/app/trust.go` — extend `resolveTrust` with the `TrustRemembered`
  tier + drift; expose anchor-hash + authority-set detection helpers.
- `internal/app/build.go` — gate project-tier agents (agentdefs source
  resolution), commands (`buildCommandExpander`, `:775-781`), skills
  (`registerSkills`, `:1115-1147`) on `decision.Trusted`.
- `internal/adapter/skills/resolve.go` — add `ResolveOptions.IncludeProjectTier`
  (default true).
- `cmd/mecatui/main.go` — add the pre-TUI detect+prompt+write in `run()`; a
  sanitizing echo helper.
- `cmd/mecated/main.go` — wire the registry read into declarative trust
  resolution (no prompt).

**Tests (incl. security):**
- registry entry with matching anchor ⇒ `TrustRemembered`, full project
  authority set admitted; mismatched anchor ⇒ `Drifted`, re-gated to safe;
- untrusted repo: PROJECT-tier agents/commands/skills/soul/allows withheld, but
  **user-tier agents/commands/skills/soul + user-global rules + built-in tools
  stay active and the loop still runs** (the "still usable" assertion) — one
  test per surface, both the withheld-project and the active-user side;
- editing `settings.yaml` does NOT cause drift (not in the anchor); editing the
  soul/an agent def DOES;
- mecated never writes `trust.yaml` (write-seam spy asserts zero writes on the
  daemon path);
- **CWE-150:** a repo path/filename containing an ANSI escape is stripped
  before the prompt echoes it;
- **CWE-59:** the registry write refuses a symlinked `trust.yaml` path
  (`O_NOFOLLOW` ⇒ ELOOP, fail-soft);
- **fail-direction:** corrupt/wrong-version `trust.yaml` ⇒ untrusted, no grant;
- **symlink keying:** a moved alias does not inherit a trusted realpath;
- **monotonic:** no positive trust input revokes a Deny/Ask (MUST-FIX 5.5);
- offline throughout (faked `xdgconfig.ResolveEnv` + injected IO seam — never
  the developer's real `~/.config`).

**Docs:** `docs/usage.md` (the prompt + `trust / once / no`), `docs/tui.md`
(the pre-TUI gate), `docs/architecture.md` (registry, identity-anchor drift,
the complete admission gate), CLAUDE.md (the trust gate now covers
agents/commands/skills, not just allows+soul — a one-line invariant update).

---

## 10. Layering check

- **New adapter `internal/adapter/workspacetrust`** — leaf: stdlib +
  `xdgconfig` + the `SHA256Hex` helper. Imports no domain package; no domain
  package imports it. Mirrors `permconfig`/`soul`.
- **`SHA256Hex` helper** — a stdlib-only leaf (`internal/adapter/hashutil` or
  inline in `workspacetrust`), shared by soulguard + workspacetrust. No new
  dependency direction.
- **`TrustDecision` + `resolveTrust`** — composition only (`internal/app`).
  Not a domain or adapter type. Feeds adapters a plain `bool`.
- **Trust is NOT a governance scope** — preserves the soul precedent
  (`soulselect.go:24-29`); `governance`/`session`/`prompt`/`tool` stay
  trust-unaware. Trust is a composition gate that *feeds* admission, exactly as
  `--trust-project` does today.
- **Render-layer rule** — the prompt decision + registry write are
  composition-side (`cmd/mecatui` main). `ui/theme/client` import no
  `workspacetrust`, no new proto. (No proto event is added — Phase 3 only.)
- **Skills adapter touch** — `IncludeProjectTier` is an additive option on an
  existing `ResolveOptions`; no inward dependency change, no signature break.

---

## 11. Out of scope / known limitations / follow-ups

**Cut (Phase 3, not built):**
- **In-TUI trust modal** + a proto `trust.ask` event — the pre-TUI prompt
  (composition-side) delivers the identical decision for the embedded server
  with zero proto churn. An external-server / remote-workspace trust modal is a
  later issue.
- **Per-scope trust** (trust the soul but not the allows) — admission is
  all-or-nothing for the project authority set. No reserved schema field (the
  panel and we agree: don't reserve speculative schema).
- **`--approve-trust` one-shot re-bless** — Phase 2 re-blesses by re-answering
  the prompt; a non-interactive `--approve-trust` flag (parallel to
  `--approve-soul`) is a small follow-up if mecated operators need it.

**Known limitations (documented, accepted):**
- **Cross-user daemon** does not inherit a desktop user's `trust.yaml` (different
  `$XDG_CONFIG_HOME`). Correct posture; document it. Cross-user trust ⇒ use that
  user's `trustedWorkspaces`.
- **settings.yaml TOCTOU window** — permconfig reads it separately from the
  trust check; the residual window is the single-user-desktop window soulguard
  already accepts. settings.yaml is admission-only (no drift anchor), fail-safe.
- **AGENTS.md / CLAUDE.md** — these project files steer the model (system-prompt
  discovery in `internal/prompt`). They are operator-context, not an
  admission-gated steering channel today. **Decision:** out of the trust gate
  for this feature (they are read as ambient project context like the code
  itself); noted as a candidate follow-up (`#TRUST-AGENTSMD`) if the threat model
  later demands gating ambient project markdown. Stated, not silently ignored.

**Follow-up issues to file:**
- `#TRUST-SKILLS-GATE` — only if skills-gating (R2.5) balloons beyond the
  additive option; otherwise it lands in Phase 2.
- `#TRUST-APPROVE` — non-interactive `--approve-trust` re-bless for mecated.
- `#TRUST-AGENTSMD` — whether ambient AGENTS.md/CLAUDE.md should be
  admission-gated.
- `#TRUST-MULTIWORKSPACE` — if per-session workspaces ever land, trust must
  become a per-root `TrustResolver` (today single-workspace per process makes a
  per-construction `TrustDecision` correct).

**What NOT to build (hard rules):**
- Never write the human `settings.yaml`; all machine writes go to `trust.yaml`.
- Never route trust through `internal/governance`.
- Never let trust override a permission Deny or a configured Ask.
- Never block mecated startup on a prompt.
- Never let `ui/` import `workspacetrust` or write the registry.
- Never hash `settings.yaml` into the **drift** anchor (nag-fatigue).
- Don't generalize soulguard into a trust subsystem — share only `SHA256Hex`.
