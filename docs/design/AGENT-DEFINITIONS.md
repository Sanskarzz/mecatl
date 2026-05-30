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
skills: [refactoring, testing] # OPTIONAL — skill names PRELOADED into this def's prompt (array or "a, b" string)
hooks:                         # OPTIONAL — phase → shell-command map scoped to this def's engine
  PreToolUse: ./scripts/gate.sh
mcpServers: [github]           # OPTIONAL — PARSED + CARRIED, but NOT yet wired (see "deferred" below)
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
- **Preloaded skills (`skills:`).** A def's `skills:` names are resolved against the
  active skills registry (the same operator-controlled set the `Skill` tool serves) and
  the matched skill BODIES are injected into the def's system-prompt Role — Claude-Code-
  style skill preloading, so the specialist starts with those playbooks in context
  rather than having to activate them. An unknown skill name is a non-fatal startup
  diagnostic (not preloaded). Wired in BOTH the per-def Task engine
  (`buildAgentTaskEngines`) and the team-member engine (`buildMemberEngine`) via
  `resolveSkillIndex` + `preloadedSkillBodies`.
- **Per-def hooks (`hooks:`).** A def's `hooks:` phase→command map scopes lifecycle
  hooks to that def's engine: the engine's `HookRunner` is built from the def's map
  (via the `hookexec` adapter) instead of the default inert runner, so a specialist can
  enforce its own `PreToolUse`/`PostToolUse`/etc. gates. Unknown phases are dropped with
  a diagnostic; a def with no (valid) hooks keeps the inert default (no behaviour
  change). See `defHookRunner` + `newChildEngineWithHooks`.
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
- **No per-agent MCP tools (DEFERRED — parsed, not wired).** A def's `mcpServers:` is
  PARSED and carried on `AgentDef.MCPServers`, but is NOT yet wired into a per-def MCP
  connection lifecycle, and listing an MCP tool in `tools:` still yields an "unknown
  tool" diagnostic (scoped catalogs see only **core** tools). **Why deferred, honestly:**
  per-def MCP requires a per-engine connection lifecycle (connect on build → add the
  server's tools to that def's catalog → disconnect on teardown). The current MCP
  manager (`internal/adapter/mcp`) is **process-scoped**: it is built once in
  `buildCatalog`, registered into the single parent catalog, and torn down only via the
  top-level `Built.Close`. The per-def Task engines and the per-member engines have **no
  teardown seam** today (they are built eagerly and live for the process / are rebuilt
  per spawn with no Close hook), so wiring connect/disconnect cleanly would mean adding
  an engine-lifetime/teardown abstraction and a per-def MCP sub-manager — substantial,
  correctness-sensitive work (leaked connections, double-close, group scoping) that does
  not fit the existing manager seam. Rather than half-implement a leaky version, the
  field is parsed + carried (so the contract is stable and a future slice has the data)
  and this is the dedicated follow-up. **Repo-map tools** remain parent-catalog-only for
  the same scoped-catalog reason.
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
