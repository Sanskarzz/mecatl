## 13. Hooks

Lifecycle hooks let an external command observe or veto agent actions. A hook is
a phase → shell-command map; each command is run as `<shell> -c <command>`
(default shell `/bin/sh`), with the JSON `HookEvent` written to its **stdin**.

### Phases

| Phase | Fires |
| --- | --- |
| `SessionStart` | once when a session begins |
| `UserPromptSubmit` | when the user submits a prompt |
| `PreToolUse` | before a tool runs — a block aborts the call |
| `PostToolUse` | after a tool runs |
| `Stop` | when the main loop stops |
| `SubagentStop` | when a subagent loop stops |

> v1 fully implements `PreToolUse` and `PostToolUse`; the others are defined and wired as the injection seam. **`mecated` ships with no global hooks configured by default**, so every event is allowed. Global hooks are configured in Go in the shared composition layer. Separately, an **agent definition** may carry a per-def `hooks:` map executed through the same runner for that child's lifecycle phases — note this is **ungated shell on the harness host** (no permission ask), which is why agent-def sources are a trust boundary (see the `--agents-dir` / `--agent-source-url` notes in §3 and §11).

### The stdin contract

The hook receives a JSON `HookEvent` on stdin:

```json
{
  "Phase": "PreToolUse",
  "Tool": "Bash",
  "Input": { "command": "rm -rf build" },
  "SessionID": "8867bdea940108c1dd82d13d3fb7fc61"
}
```

### The exit-code contract

| Exit code | Outcome |
| --- | --- |
| `0` | **allow** — the message (if any) is read from stdout |
| `2` | **block** — the action is vetoed; the reason is read from stdout (preferred) or stderr |
| any other | **error** — the hook itself failed; surfaced to the caller |

A single invocation is bounded by a timeout (default 30 s).

### Example hook script

A `PreToolUse` hook that blocks any `Bash` command containing `rm -rf`:

```sh
#!/bin/sh
# pretooluse-guard.sh — exit 2 to block, 0 to allow.
event="$(cat)"                       # the HookEvent JSON arrives on stdin
if printf '%s' "$event" | grep -q 'rm -rf'; then
  echo "blocked: 'rm -rf' is not permitted by policy"   # reason -> client/model
  exit 2
fi
exit 0
```

> Wiring a global hook requires editing the shared composition layer and rebuilding — there is no config-file path for global hooks in v1. An agent definition's per-def `hooks:` map is the config-driven path.

---

See also: [permissions configuration](permissions-config.md) (the other
governance surface), or the [operator guide index](../usage.md).

