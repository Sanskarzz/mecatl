# Guardrails — LLM-backed tool-content inspection (issue #27)

> **Design record.** Captured during the guardrails work; the rationale here is frozen.
> Current behaviour: [`docs/architecture.md`](../architecture.md) · shipped/deferred state: [PRODUCTION-READINESS.md](./PRODUCTION-READINESS.md). Evolve via a new [ADR](../adr/), not by editing this file.

Guardrails are an **operator-tier, LLM-backed content checker** that inspects the
data crossing the agent's tool boundary in both directions and enforces a verdict on
the call. It is the *dual-LLM quarantine* pattern (Willison's "CaMeL" framing, the
Codex sandbox lineage): a **separate** model judges tool content as data, never as
instructions, so a compromised tool result or a model bent on exfiltration is caught
by something the attacker cannot also prompt-inject in the same breath.

It is **OFF until a checker model is configured** and byte-identical to "no
guardrails" when no model is set. Configuring a model (`--guardrails-model` or the
`guardrails.model` YAML key) is the **opt-in to spend** — the only cost guardrails
incur is the per-tool checker call. Once a model is configured, guardrails are ON
even with no hand-authored rule list: they take the **default advisory rule set**
(below), honouring the headline default of "ON (advisory) for the network/MCP
surfaces, OFF for local tools".

## Default rule set

When a checker model is configured but the operator authors **no explicit rules**,
guardrails apply this built-in **advisory** (observe-only) set:

| Tool matcher | Phases | Mode |
| --- | --- | --- |
| `WebSearch` | pre + post | advisory |
| `WebFetch` | post | advisory |
| `mcp__*` (all MCP tools) | pre + post | advisory |

Local tools (`Read`/`Edit`/`Write`/`Bash`/`Grep`/`Glob`) are deliberately **not**
matched — their I/O stays on the machine. Advisory means observe-only: a finding is
an operator diagnostic, the call/result is byte-unchanged, so the operator measures
the false-positive rate before promoting any rule to `block`/`sanitize`. Authoring an
explicit `guardrails.rules` list **replaces** the default set entirely.

Because the default set matches `mcp__*` on both directions, a default-on,
MCP-heavy session could spend a checker call per matched call. So when the defaults
are in force and the operator did not pin `maxChecks`, a sane non-zero per-session
cap (200) is applied automatically — see *Cost model*.

## Threat model

A coding agent crosses two trust boundaries on every tool call:

- **OUTBOUND — `PreToolUse` (exfiltration).** The model chose the arguments. A
  compromised or coaxed model can put a secret where it does not belong: an env dump
  piped into an HTTP tool, a credential in the body of an MCP call, `/etc/passwd` or
  `.env` contents addressed to an external service. The guardrail inspects the
  **arguments** the model is about to run.
- **INBOUND — `PostToolUse` (prompt injection).** A tool *result* is attacker-
  influenced data: a fetched web page, a GitHub issue body, an MCP server's response.
  It can carry "ignore previous instructions", embedded system/developer-role
  directives, or hidden instructions aimed at the agent. The guardrail inspects the
  **result** the model is about to read.

The checker is a **dedicated, tool-less, one-turn** model. The content under review
is fenced with the **same `agent.UntrustedFence`** the team and ask-review prompts use
(one exported source of truth, issue #27) and run through `agent.NeutraliseFraming`,
so an injection cannot forge the fence or a section header to break out of the data
block. The verdict parse requires the **whole** checker output to be a single JSON
object (`modelhook.ParseVerdict`, the issue #31 `parseAskVerdict` discipline — **not**
`session.ValidateJSON`, which extracts-from-prose and is fail-open: wrong for
attacker-adjacent content), so a forged verdict-shaped object echoed *inside* the
fenced content can never be lifted out as the real verdict.

## The `PostToolUse`-Block-is-inert mechanism (the #1 constraint)

A `PreToolUse` Block is a **real veto** — the tool has not run, the loop never
executes it, and the model receives the block as an error tool result.

A `PostToolUse` Block is **inert** — the tool has already executed by the time the
hook fires, so the loop only emits a hook annotation; it neither undoes nor
suppresses the result. Therefore, to "block a bad inbound RESULT" in an enforcing
mode the guardrail rewrites the result via `HookOutcome.Mutated` to
`{content: "blocked by guardrail: <reason>", is_error: true}` — **not** Block.

The agent loop guarantees the recorded history, the client event stream, and the
model's recorded view all show the **effective (mutated)** payload (`dispatch.go`'s
`execute` emits `postHook`'s result), so the model sees the block and the client UI
agrees — no hidden divergence, and the raw injected result never reaches the client.

## Enforcement modes

Per rule (`mode:`):

- **`block`** — enforce. `PreToolUse` → veto; `PostToolUse` → rewrite-to-error.
- **`sanitize`** — enforce by **rewriting**. The checker returns `sanitized_content`;
  the runner rewrites the call args (`Pre`) or the result content (`Post`) to it. See
  *Bounding the sanitize path* below for the trust assumption and the fallbacks.
- **`advisory`** — observe only. An unsafe verdict emits an operator diagnostic; the
  call/result is byte-unchanged (the model never sees it). The diagnostic carries the
  session id, the tool-call id, and a stable `guardrail-finding` marker, so an
  operator can correlate a finding back to the exact conversation and call (it is an
  operator-log signal only — the client and model see nothing on an advisory finding).

A checker that **says safe always passes**, in every mode — that is its job.

## Bounding the sanitize path (the trust assumption)

Sanitize re-injects the checker's `sanitized_content` as content the main agent
trusts **more** (it is "sanitized"). That is a real trust transfer, so it is bounded:

- **Trust assumption (explicit):** a *compromised* checker can rewrite content;
  sanitize **trusts the checker's output**. Use `sanitize` only with a checker model
  you trust. For an untrusted/experimental checker, prefer `block` or `advisory`.
- A **nil** `sanitized_content` on an unsafe verdict ⇒ nothing safe to substitute ⇒
  fall back to a **block** (fail toward the safe outcome).
- An **oversized** `sanitized_content` (a compromised checker padding/laundering
  content back into the trusted stream) is **rejected** → fall back to a block.
- On **Pre**, a sanitized payload that is **not valid args JSON** ⇒ the loop would
  ignore it and run the **original unsafe args**, so it falls back to a **block**
  (the original unsafe call never runs).
- On **Post**, the sanitized result is prefixed with a visible
  `[guardrail: redacted unsafe content]` marker so the model knows it received edited
  content (it may otherwise confidently cite a removed hole). Pre arg-sanitizing stays
  silent (the model never sees the raw args anyway).

## Matcher precedence

A rule's `match:` keys **only on the tool NAME** (harness-controlled, never attacker
content): an exact name, a `prefix*` glob (e.g. `mcp__github__*`), or `*` (catch-all).
Resolution is **most-specific-wins**: exact > longest-prefix glob > `*`. A tie (two
equally-specific matchers, flagged by a load-time WARN) breaks toward the
earlier-configured rule, so resolution is deterministic regardless of map iteration.
A tool with no matching rule is **unchecked** — guardrails are opt-in per tool.

## Cost model & the per-session budget

Each check is one extra LLM call. Three cost/abuse guards:

- **`maxChecks` (per session).** The number of checker calls a session may incur is
  capped **separately** from the parent's `MaxRunTokens`: infrastructure spend must
  not starve the agent. A fresh budget is built per session (the composition
  constructs one `modelhook.Runner` per session), so the cap is naturally per-session
  and resets when a new session's Runner is built. Exhaustion is a one-time WARN, then
  a silent fail-open skip. **Omitting `maxChecks` means *no cap*** — EXCEPT when
  guardrails are on the default rule set with no explicit `maxChecks`, in which case a
  default cap of **200** is applied (so default-on guardrails cannot surprise-bill an
  MCP-heavy session). An explicit `maxChecks` (including a deliberate `0` for
  unbounded) or an explicit rule list keeps your value untouched.
- **`minContentBytes`.** Skip the checker for a trivially short **inbound (Post)**
  result that cannot carry a meaningful injection (omitting it checks every Post
  result). It applies to **Post only** — **outbound (Pre) args are always inspected
  regardless of size**, because secrets are short and a tiny exfiltration arg is
  exactly what the Pre check exists to catch.
- **`maxContentBytes` (built-in, 256 KiB).** Content over this is **not** inspected.
  In an enforcing mode it is **not silently passed**: it routes through the
  fail-open/closed policy (fail-closed blocks; fail-open WARNs), so an attacker cannot
  emit a huge tool result to induce a silent fail-open and slip past. Advisory passes
  but logs.

## Fail-open vs fail-closed

On a checker **error / timeout / unparseable verdict / oversized content**:

- **Fail-open (default).** Degrade to "no checker" with a WARN. The agent keeps
  working; an infra hiccup in the checker model does not brick the harness.
- **Fail-closed (per-rule `failClosed: true`).** Treat the content as unsafe — a
  Block on `Pre`, a Mutated-to-error on `Post`. For the rules where a missed check is
  worse than a halted call.

A checker **saying safe** always passes, regardless of mode or fail-closed.

**Guardrail-DOWN escalation.** Because fail-open means a broken checker = no
protection, a persistently-failing checker must be impossible to miss. After a run of
consecutive checker failures crosses a threshold, the runner emits a distinct,
one-time sticky WARN — *"guardrail checker DOWN — N consecutive failures; tool I/O is
currently UNGUARDED on fail-open rules"* — instead of a per-call WARN flood an operator
tunes out. A completed verdict (safe or unsafe) resets the streak and re-arms the
escalation for any future outage.

## Multi-runner merge

The guardrail `Runner` **decorates** an inner `port.HookRunner` (the
`hookexec` / `userModelReview` chain). The inner runs **first**, the checker
**second**. Merge (decision 5):

- **Block-dominant** — either side blocking blocks the merged outcome; messages
  concatenate inner-first.
- **Mutation conflict → checker wins** (security): the checker's `Mutated` replaces
  the inner's.
- **Non-tool phases delegate straight to inner** (the checker never fires).

## Operator-tier-only configuration (the trust inversion)

Guardrails config is **operator-tier ONLY**: it is read from the **user-global**
`settings.yaml` `guardrails:` subtree and the CLI, **never** from the project-tier
file. This **inverts** the usual permconfig tighten-only gate: a project repo is
allowed to *tighten* permissions, but a guardrail relaxation is a *loosening* — a
project repo disabling or weakening a security checker is a downgrade, so a
project-tier `guardrails:` block is **ignored with a loud WARN** (`permconfig.Resolver`
enforces this; `Resolver.OperatorGuardrails()` by construction never returns a
project-tier block). The subtree is parsed **strictly** (unknown sub-keys are an
error, like the `permissions:` subtree) so a typo cannot silently disable a guardrail.

The checker **model** and the **master kill-switch** are also CLI flags
(`--guardrails-model`, `--guardrails=off`); a flag out-ranks the YAML model. The rule
list lives only in YAML (a flag cannot express it).

## Recursion guard

The `Runner` is wired **only** into the **main** engine's hooks (`buildEngine`'s
`mainHooks` + the per-session factory's re-derivation), **never** into
`buildCatalog`'s child hooks. The checker engine is built via
`childEngineDepsForProvider`, which forces **inert Hooks + nil `ChildAskReviewer` +
`Interactive` false + a tool-less catalog** — so a checker call fires no hooks and can
never re-trigger the runner, and the checker cannot itself call tools.

## Where it lives (layering)

- **`internal/adapter/modelhook/`** — the adapter: the `Runner` (`port.HookRunner`),
  the `VerdictChecker` adapter-local port, `Verdict` + `ParseVerdict`, the matcher,
  the per-session budget, the built-in inspection prompts, the merge. It imports
  `engine/agent` only for the **exported** fence helpers
  (`agent.UntrustedFence` / `NeutraliseFraming` / `WriteUntrustedBlock`).
- **`engine/agent/guardrailcheck.go`** — `RunGuardrailCheck`, the engine-driving half
  (it needs the unexported `drainChild`): drives the tool-less one-turn checker engine
  over the assembled prompt and returns raw text. The composition parses it. (It is a
  free function, not an exported struct, matching the `engineAskReviewer`/`engineJudge`
  siblings — composition noise stays out of the importable public API.)
- **`internal/app/guardrails.go`** — composition: `buildGuardrailsHooks` (decorates
  the main hooks, OFF-until-a-model-is-set), `effectiveGuardrailSpecs` (explicit rules
  or the default advisory set), `engineGuardrailsChecker` (the `VerdictChecker` impl
  wrapping `RunGuardrailCheck` + `ParseVerdict`), `compileGuardrailRules`,
  `foldOperatorGuardrails` (the operator-tier YAML fold), and
  `normalizeGuardrailsModel` (fail-fast model validation + the build-once ACTIVE fact).
- **`internal/adapter/permconfig/`** — the operator-tier `guardrails:` YAML parse +
  the project-tier ignore-WARN.

## Example config

In the **user-global** `~/.config/mecatl/settings.yaml` (never a project file):

The minimal config is just a model — it takes the default advisory rule set:

```yaml
guardrails:
  model: gpt-5-mini          # configuring a model is the opt-in; default advisory rules apply
```

A full config overrides the defaults with an explicit rule list:

```yaml
guardrails:
  model: gpt-5-mini          # the checker model (or a --model-alias); --guardrails-model overrides
  maxChecks: 50              # per-session checker-call cap; OMITTING it = NO cap
                             # (a default cap of 200 applies ONLY to the default rule set)
  minContentBytes: 16        # skip a short INBOUND (post) result (omit = check every post).
                             # Never applies to outbound (pre) args — those are always inspected.
  rules:                     # an explicit list REPLACES the default advisory set
    - match: "WebFetch"      # inbound injection on fetched pages
      phases: ["post"]
      mode: block
    - match: "mcp__*"        # all MCP tools, both directions
      mode: advisory         # observe first, tune later
    - match: "Bash"          # outbound exfil in shell args
      phases: ["pre"]
      mode: sanitize         # trusts the checker's rewrite — use only with a trusted checker
      failClosed: true       # a checker outage must not let a shell exfil through
```

Run headless or interactive — guardrails fire on the **main loop's**
`PreToolUse`/`PostToolUse` regardless of whether the deployment surfaces *permission*
asks to a human (unlike the headless-only ask reviewer).


---

*Part of the [design docs](./README.md). Related: [ALLOW-ALL-POSTURE.md](./ALLOW-ALL-POSTURE.md), [WORKSPACE-TRUST-SPIKE.md](./WORKSPACE-TRUST-SPIKE.md).*
