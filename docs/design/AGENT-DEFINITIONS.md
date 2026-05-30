# Agent definitions (Tier 1)

Named subagent specialists discovered from operator-controlled markdown files
(`<dir>/<name>.md`, YAML frontmatter + body), mirroring the skills Source seam. **One
definition, two consumers**: a `<name>.md` is reusable both as a Task delegate
(`Task(agent="<name>")`) and as a team-member role (`MemberSpec.AgentType` /
`SpawnTeammate(agent_type=...)`).

See `internal/adapter/agents/` (discovery + `Registry`) and
`internal/app/agentdefs.go` + `internal/app/build.go` (the registry→engine
translation, scoping, model/mode resolution — the layering rule keeps the `Registry`
out of `internal/agent`, which receives only `map[string]*Engine` / `MemberBuild`).

## Frontmatter

```yaml
---
name: code-reviewer            # REQUIRED — routing key / AgentType handle
description: Reviews a diff...  # REQUIRED — always-in-context routing metadata (capped)
tools: [Read, Grep, Glob]      # OPTIONAL — allowlist of CORE tool names (array or "a, b" string)
disallowedTools: [Write]       # OPTIONAL — subtractive filter applied after tools/default
model: sonnet                  # OPTIONAL — alias | full id | inherit/empty (=> parent)
permissionMode: plan           # OPTIONAL — default | plan | acceptEdits
color: blue                    # OPTIONAL — UX hint only; never affects execution
---
You are a meticulous code reviewer. <full body = the specialist's system-prompt instructions>
```

## What v1 supports

- **Prompt persona.** The body is composed into the engine's system prompt as the
  Role (a cache-stable StablePrefix layer; one byte-stable prefix per def).
- **Scoped CORE tools.** The def's `tools` allowlist (minus `disallowedTools`) is
  intersected with the call site's available **core** toolset (Read/Edit/Write/Grep/
  Glob/WebFetch/Bash). `Task`/`Fork`/`ToolSearch` are ALWAYS excluded (no nesting / no
  silent disclosure tool).
  - **Task delegates are unconditionally read-only** — `Task.ReadOnly()` stays `true`,
    so a def's mutating tools (Edit/Write/non-RO Bash) are dropped on the Task path
    (with a startup diagnostic). A mutating specialist is a **team member** (forks) or
    a **Fork** branch, not a Task.
  - **Team members** obey read-only-share / mutating-fork: a `Mutating` member (runs
    in an isolated fork) MAY keep Edit/Write/Bash; a read-only (base-sharing) member
    has them dropped (so the supervisor's `ErrReadOnlyMemberMutating` backstop never
    trips). Team coordination tools (`MemberTools`) are always appended and bypass the
    allowlist.
- **Per-def model.** `def.Model` > global `--subagent-model` > parent `--model`.
  Aliases (`--model-alias name=id`, plus built-in `sonnet`/`opus`/`haiku`→inherit)
  resolve only in the composition layer; the domain always gets a concrete id. An
  unknown alias warns and inherits.
- **Per-member permission mode.** A team member's `permissionMode` maps to its session
  mode via the `MemberEngine` factory returning `agent.MemberBuild{Engine, Mode}`; an
  empty Mode falls back to the team-wide default (`WithTeamMode`). `plan` hard-denies
  mutations (existing invariant), so a `plan` member is effectively read-only;
  `acceptEdits` is only meaningful for a Mutating member.
- **Forgiving resolution.** Conventional discovery is on by default and **inert** when
  no dir exists. An unknown member `AgentType` falls back to the default member
  catalog (warn, never fail the spawn). An unknown Task `agent` arg is a
  model-addressable error listing valid names (the model can retry).

## What v1 does NOT do (honest parity vs Claude Code)

- **No mutating Task delegates.** CC lets a `Task(subagent_type=...)` inherit Edit/
  Write and the project's tools. Our read-parallel/mutate-serial dispatcher makes a
  mutating Task a workspace-race hazard, so mutation is routed through the isolation
  mechanisms that already exist (member fork / Fork worktree). On the Task path the
  only thing a def adds over the anonymous explorer is a different prompt + model +
  read-only tool scope.
- **No per-agent MCP / skills / repo-map tools.** Scoped catalogs see only **core**
  tools. MCP/skills/repo-map tools are registered into the parent catalog at runtime
  and are not constructable into a per-def child catalog in this tier; a def listing
  one gets an "unknown tool" diagnostic (distinct from the read-only-drop diagnostic).
- **No per-agent hooks.** Child/member engines run the default (inert) hook runner.
- **No `--agents` inline JSON.** Definitions come only from `<name>.md` files under
  `--agents-dir` / the conventional dirs.
- **No Agent-as-tool nesting.** A def cannot re-add `Task`/`Fork`/`ToolSearch`; a child
  never recurses or fans out further.
- **No file-path scoping** (Roo's file allowlist) and **no maxTurns/maxToolCalls**
  per-def limit wiring yet.

## Flags

```
--agents-dir <dir>            repeatable; explicit dirs, highest precedence
--agents-conventional[=true]  also discover .mecatl/agents, .claude/agents, user dirs
                              (ON by default, inert when absent; like teams/fork)
--subagent-model <id|alias>   global model override for Task/member children
--model-alias name=id         repeatable alias→id map (composition layer only)
```

The TUI's embedded server (`cmd/mecatui`) enables `AgentsConventional` by default,
consistent with `EnableTeams`/`EnableFork`.
