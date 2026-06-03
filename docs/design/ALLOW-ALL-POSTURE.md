# Unattended / allow-all posture (the "YOLO mode" question)

Status: **shipped** · supersedes the abandoned `ModeYolo` spike (see "What we
rejected"). Decision-support for issue: *"add a yolo mode."*

## The ask

People want a "don't prompt me for anything" posture so the harness can run
unattended — in CI, in a container, in a throwaway VM. The natural-seeming
implementation is a fourth `PermissionMode` (`yolo`) that bypasses the permission
gate, with a hardcoded "circuit breaker" denylist (`rm -rf /`, `rm -rf ~`) as a
last line of defence.

**That implementation is wrong on three counts, and we are not shipping it.** This
doc records why, and specifies the posture we ship instead.

## Decision

1. **No new `PermissionMode`.** "No prompts" is not a session state the engine
   (or the model) selects. It is a **server-wide operator posture** set at process
   start by whoever owns the blast radius, expressed as a single allow-all
   **rule** injected into the existing policy — *not* a code path that bypasses the
   evaluator.
2. **It stays subordinate to the governance invariants.** A `Deny` in any scope
   still wins (deny-dominant). A *deliberately configured* `Ask` (managed,
   project, or user) still asks. Allow-all only loosens the built-in mutate-ask
   floor (`Bash`/`Edit`/`Write`/`Team`/`SkillDraft` → `Ask`). This falls out of
   the existing merge fold with **zero evaluator changes**.
3. **No hardcoded command circuit breaker.** A substring denylist is security
   theatre (see below). The real boundary is deployment isolation. Anyone who
   wants "always confirm `rm -rf /` even here" expresses it as a configured
   `Deny`/`Ask` rule — which allow-all honours by construction.
4. **Loud, gated opt-in.** An explicit operator flag (`--yolo`), refused when
   running privileged outside a declared sandbox, logged at startup.
   Sandbox-first: the flag is for disposable, isolated environments only. (The
   name is deliberately memorable; the safety rests on the root/sandbox refusal,
   the loud startup warning, and the preserved deny-dominance — not on a
   scary-name deterrent.)

## Why not a `yolo` PermissionMode (what the spike got wrong)

The abandoned spike added `session.ModeYolo`, threaded it through the proto enum,
the gRPC mapper, the ACP mode picker, the `mecatui` flag, and short-circuited
`permpolicy.Evaluate` to `Allow` **before the governance evaluator was consulted**.
Three fatal problems:

### 1. It defeats `ScopeManaged` deny — the one thing governance must never allow
The core invariant (`internal/governance/evaluator.go`, CLAUDE.md) is *"a Deny in
ANY scope is absolute,"* with `ScopeManaged` as the admin/enterprise floor that no
lower scope — and **no CLI arg** — may override. The spike returned `Allow` in the
adapter, before any deny was evaluated, so a session flipping to `yolo` defeated a
`ScopeManaged` Deny. Its own test (`TestPolicyYoloModeIgnoresConfiguredRules`)
codified that as desired. This is precisely the failure Claude Code's
`disableBypassPermissionsMode` managed kill-switch exists to prevent. **A bypass
the session can reach is a bypass prompt-injection can reach.**

### 2. The "circuit breaker" was theatre — and broken
`isCircuitBreaker` did `strings.Contains(cmd, "rm -rf /")`. Trivially evaded
(`rm -fr /`, `rm  -rf /`, `find / -delete`, `dd`, `mkfs`, `chmod -R 000 /`,
`> /dev/sda`, fork bombs, `bash -c`, `/proc/self/root/...`), and it ignored the
repo's own substitution/newline-aware `governance.SplitCommands` machinery — the
exact regression CLAUDE.md forbids. It also **false-positived on every
absolute-path `rm`**: `rm -rf /tmp/foo` *contains* `"rm -rf /"`, so the breaker
fired on benign commands. The spike's own tests asserted those benign commands
return `Allow` while the code returned `Ask` — the suite was internally
contradictory and could not pass. (Industry consensus is unanimous here: a
denylist is a fumble-finger guard against *model error* at best, never a security
control. Documented agents have bypassed their own denylists via path indirection
and then disabled their own sandbox. The boundary must live *below* the agent.)

### 3. It wasn't needed — allow-all already exists
Every child/fork/team-member engine is already constructed allow-all:

```go
// internal/app/build.go — newChildEngineWithHooks
Policy: permpolicy.NewPolicy([]governance.Rule{{Effect: governance.Allow}}, nil),
```

An empty-`Tool`/`Pattern` `Allow` rule matches every call. We reuse that exact
mechanism for the main session instead of threading a fourth mode through nine
touch-points (session const, proto enum, mapper ×2, ACP advertise + parse,
agentdefs, evaluator, tests).

## How allow-all works as a rule (the mechanism)

The governance fold is deny → ask → allow, with the one narrow loosening: a
higher-scope `Allow` may relax **only** a `ScopeBuiltinDefault` `Ask` (the built-in
mutate-ask floor); it never suppresses a configured `Ask`, and a `Deny` in any
scope always wins (`internal/governance/evaluator.go`).

So injecting **one** rule at `ScopeCLI` —

```go
{Scope: governance.ScopeCLI, Tool: "", Pattern: "", Effect: governance.Allow}
```

— alongside `defaultRules()` yields exactly the safe semantics, with no evaluator
change:

| Against | Result | Why |
|---|---|---|
| Built-in `Ask` floor (`Bash`/`Edit`/`Write`/`Team`/`SkillDraft`) | **Allow** (no prompt) | CLI Allow loosens the `ScopeBuiltinDefault` Ask floor (the narrow exception) |
| A configured `Deny` (any scope, incl. `ScopeManaged`) | **Deny** | deny-dominance — absolute |
| A deliberately configured `Ask` (managed/project/user) | **Ask** | Allow never suppresses a configured Ask |
| An `--permission-config` `Ask` at `ScopeCLI` (same scope) | **Ask** | equal-scope ask/allow tie favours Ask |

`ScopeCLI` is the right semantic home: the operator, at invocation, declared the
posture. It sits above the built-in floor (so it loosens it) and below
`ScopeManaged` (so an admin still wins).

### The headless caveat (honest, documented)
Because allow-all honours *configured* `Ask` rules, a session whose resolved
config contains an `Ask` will still **pause for approval that may never come** in a
truly headless run. That is the operator misconfiguring two contradictory intents
(allow-all + an explicit Ask). We do **not** silently override the Ask — overriding
a deliberately configured (possibly admin) Ask is the unsafe behaviour. Instead, at
startup, **whenever allow-all is enabled we log a loud, GENERIC warning** that any
deliberately configured `Ask` (managed/project/user) still applies and may block an
unattended run. The warning is intentionally NOT a config-scan: the file-based
permission config is re-resolved *per session* against each session's workspace root
(issue #13), so there is no single "effective config" to scan at process start — a
startup scan would be both infeasible and misleading. The generic warning covers the
case honestly. (The common CI case configures no asks beyond the built-in floor, so
allow-all is fully unattended there.)

## Gating (mirrors Claude Code's posture)

- **Explicit flag, off by default.** `--yolo` on `mecated`; the embedded
  `mecatui` gets the same flag (it is a process-start flag, never a session
  `PermissionMode` — no `--mode yolo`). Refused by default.
- **Privilege + sandbox refusal.** At config validation in the composition root
  (`cmd/mecated`, `cmd/mecatui` — the only layers allowed to touch `os`): if
  allow-all is requested **and** `os.Geteuid() == 0` **and** no sandbox assertion
  env var is set (`MECATL_SANDBOX=1`, accepting `IS_SANDBOX=1` for parity with
  Claude Code), **refuse to start** with a clear error. Root + no prompts can
  modify anything on the host; the operator must affirm isolation.
- **Server-wide, single-tenant only.** The flag makes *every* session on that
  daemon allow-all. Documented as intended for ephemeral / single-tenant /
  sandboxed deployments; multi-tenant daemons must not enable it.
- **Startup log.** Emit a `WARN` at boot recording that allow-all is active and
  noting that any deliberately configured `Ask` still applies (the generic warning
  above — not a config scan).
- **Admins retain per-tool veto today.** A `ScopeManaged` `Deny` still wins, so an
  org can forbid specific tools/patterns even under allow-all without any new
  machinery.

## Sandbox-first (the real boundary)

The flag bypasses the *prompt*, never a *sandbox*. The defensible boundary for
unattended agentic execution is OS-level isolation — container / microVM,
network-off-by-default, ephemeral filesystem — not an in-process string match the
agent can reason around. The docs for this flag must say so plainly: enable
allow-all only where the harness cannot cause durable harm.

## Touch-points (implementation surface)

Far smaller than the rejected mode:

1. `internal/app/build.go` — add `AllowAllTools bool` to `Config`; when set, the
   `mainRules` helper prepends the `ScopeCLI` allow-all rule to `defaultRules()` for
   the **main** engine policy only (children are already allow-all). The startup
   warning lives in the composition roots, not here (`port.Logger` is ToolCall-only).
2. `cmd/mecated/main.go` — `--yolo` flag → `appConfig`;
   privilege/sandbox refusal in flag validation; startup `WARN`.
3. `cmd/mecatui/config.go` + `cmd/mecatui/main.go` — same flag for the embedded
   server; same refusal; thread into `app.Config`. (No `--mode yolo`.)
4. Tests: a governance/permpolicy test proving the injected `ScopeCLI` allow-all
   (a) loosens the built-in Ask floor, (b) still loses to a `ScopeManaged` Deny,
   (c) still defers to a configured `Ask`; a composition test that `AllowAllTools`
   injects the rule; a flag-validation test for the root/sandbox refusal. **No
   destructive command strings as test literals** — assertions are on the decision
   only, and we keep `rm -rf /`-style strings out of the suite entirely.

No proto change. No ACP change. No `session.PermissionMode` change.

## Future work (not v1)

- A managed-scope `disableAllowAll` kill-switch in `permconfig`, mirroring
  `disableBypassPermissionsMode`, so an org can forbid the posture globally rather
  than per-tool. (Deny-dominance already covers per-tool veto.)
- An "auto + reviewer" posture (out-of-band classifier over tool calls via the
  existing `HookRunner`/PreToolUse seam) as the *safe* unattended default, the way
  Anthropic's `auto` mode works — strictly better than skip-everything.
- Revisit `ModeAccept` (acceptEdits), which is currently **declared but not
  implemented** — metadata-only, never consulted in the decision logic. If we want
  a real "auto-approve edits but still ask for Bash" tier, that is its own design.

## References

External research backing this decision (Claude Code `bypassPermissions` +
`disableBypassPermissionsMode`, root refusal, Codex sandbox-by-default, the denylist
bypass corpus, Ona's self-escape) is summarised in the research brief that
accompanied this doc. Key primary source: Claude Code — Configure permissions,
https://code.claude.com/docs/en/permissions.
