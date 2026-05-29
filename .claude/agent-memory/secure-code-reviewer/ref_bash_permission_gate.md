---
name: ref-bash-permission-gate
description: How mecatl's permission gate, compound-command splitting, and wrapper canonicalization work — and their known evasion gaps.
metadata:
  type: reference
---

The permission gate is the v1 security boundary for Bash (see [[project-mecatl]]).

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
2. The DEFAULT ruleset (`cmd/mecated/main.go defaultRules`) makes all of Bash =
   Ask with no command-specific deny rules, so gap #1 only escalates once an
   operator adds a pattern-based Bash deny (the documented use case). Severity
   is therefore conditional on configuration.
3. ~~`cleanPath` (osfs.go) symlink-escape TOCTOU~~ CLOSED as of the
   da3dd2b..HEAD refactor. osfs.FileSystem now does all file ops through an
   `*os.Root` (Go 1.24+) opened on the resolved root: Read/Write/Stat/Glob/
   fingerprint route through it, and Glob/walkAll skip symlinks. os.Root refuses
   both `..` and symlink traversal leaving the root. The model can still create a
   symlink via Bash but it can no longer be followed out of root. The
   CommandRunner was extracted to its own type but keeps `cmd.Dir = root` — cwd
   rooting preserved (NB: a shell command can still `cat /etc/passwd` by absolute
   path; os.Root only guards the FileSystem tool seam, not arbitrary shell).

**Where the sandbox seam should later wrap:** `Workspace.RunCommand`
(osfs.go:303) and the file ops — that's the process-trust boundary the v3
OS sandbox is meant to enclose.
