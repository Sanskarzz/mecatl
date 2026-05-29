---
name: ref-bash-permission-gate
description: How ozzharness's permission gate, compound-command splitting, and wrapper canonicalization work — and their known evasion gaps.
metadata:
  type: reference
---

The permission gate is the v1 security boundary for Bash (see [[project-ozzharness]]).

**Flow (correct):** `internal/agent/dispatch.go` calls `authorize()` →
`preHook()` → `execute()` in that order, for BOTH the serial mutating path
(`runOne`) and the read-only batch (`runReadBatch` phase 1 resolves
permission+hook before phase 2 executes). Gate is genuinely consulted before
execution. Unmatched tool/command defaults to **Ask**, never silent allow
(`evaluator.go resolveSimple`). Plan mode hard-denies Edit/Write and non-read-only
Bash (`planModeDecision`). Deny beats ask beats allow via `effectRank` folding
across compound subcommands.

**Known gaps (findings as of 2026-05-29 review):**
1. `governance.SplitCommands` (bash.go) only splits on `&& || ; |`. It does NOT
   split on: newlines (`\n`), background `&` (single), command-substitution
   `$(...)` / backticks, or subshell `(...)`. A deny rule on an inner command
   can be bypassed by smuggling it inside `$(...)`, a newline, or a subshell —
   e.g. `echo $(rm -rf x)` evaluates the gate against `echo $(rm -rf x)` as one
   token, never against `rm`. ReadOnlyBash inherits the same gap (plan-mode
   read-only classifier can be fooled by `cat $(rm x)`).
2. The DEFAULT ruleset (`cmd/ozzd/main.go defaultRules`) makes all of Bash =
   Ask with no command-specific deny rules, so gap #1 only escalates once an
   operator adds a pattern-based Bash deny (the documented use case). Severity
   is therefore conditional on configuration.
3. `cleanPath` (osfs.go) resolves symlinks only on the ROOT at construction
   (`resolveRoot`/`EvalSymlinks`), not on the joined target path. A symlink
   inside the workspace pointing outside it is followed on Read/Write (classic
   TOCTOU symlink escape). The `..`/absolute-path lexical checks are sound; the
   symlink case is the gap. v1 has no OS sandbox to backstop this.

**Where the sandbox seam should later wrap:** `Workspace.RunCommand`
(osfs.go:303) and the file ops — that's the process-trust boundary the v3
OS sandbox is meant to enclose.
