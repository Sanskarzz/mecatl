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
mcpServers:                    # OPTIONAL — per-agent MCP (reference an existing server OR inline a new one)
  - github                     #   REFERENCE: a configured main server's name (gets its tools)
  - name: jira                 #   INLINE: a streamable-HTTP server only THIS def connects
    url: https://jira.example/mcp
    headers: { Authorization: "Bearer ${TOKEN}" }
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
- **Per-agent MCP servers (`mcpServers:`).** A def's `mcpServers:` scopes specific MCP
  servers' tools to THAT def's engine, in BOTH call sites (Task delegate and team
  member). Two forms, mixable in one list:
  - **REFERENCE** (a bare server name, or a mapping with only `name`): the def gets the
    tools of an ALREADY-configured main server, pulled from the process MCP manager. No
    new connection is opened, so there is nothing to tear down. An unknown reference is a
    non-fatal startup diagnostic (skipped).
  - **INLINE** (a mapping with `name` + `url` + optional `headers`): the def connects its
    OWN streamable-HTTP server, whose tools are added to the def's catalog but NEVER
    enter the main conversation's context. **Streamable-HTTP only** — a `command:`/stdio
    or any non-HTTP `type`/`transport` inline entry is REJECTED with a diagnostic and
    skipped (CLAUDE.md: no stdio MCP, ever).
  - The MCP tools are added to the def's catalog directly (a def opting into a server
    gets that server's tools); they do NOT go through the `tools:` core allowlist, and
    `Task`/`Fork`/`ToolSearch` exclusion + the def's core `tools`/`disallowedTools`
    semantics are unchanged.
  - **Read-only backstop interaction.** MCP tools report `ReadOnly()==false` but never
    touch the workspace, so they are EXEMPT from the supervisor's read-only-member
    workspace-mutating-tool backstop (`ErrReadOnlyMemberMutating`) — the same way the
    team coordination tools are exempt. A read-only member may therefore safely hold MCP
    tools; a genuine workspace-mutating tool (Edit/Write/non-RO Bash) is still rejected.
    The exemption is carried as `MemberBuild.MCPToolNames`, which the supervisor folds
    into its exemption set.
  - **Lifetime model.** A **Task-path** def engine is built once at composition
    (`buildAgentTaskEngines`); its inline managers' `Close` is aggregated into
    `Built.Close` (process-lifetime, torn down on shutdown). A **team-member** def engine
    is built per spawn (the `MemberEngine` factory); its inline-MCP `Close` rides on
    `MemberBuild.Close`, which the supervisor composes with the member's fork cleanup so
    it runs on every teardown path (`cleanupAll`, a failed/stopped member, a rejected
    enrolment). Reference entries open nothing, so they contribute no `Close`. See
    `defMCPTools` (the single reference/inline resolver shared by both call sites).
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
- **Per-agent MCP `tools:` allowlisting still core-only.** A def's `mcpServers:` adds the
  server's tools to the def's catalog directly (see "What v1 supports"), but listing an
  MCP tool name in the `tools:` allowlist still yields an "unknown tool" diagnostic —
  the `tools:`/`disallowedTools:` scope governs the **core** toolset only; MCP tools
  arrive via `mcpServers:`, not the core allowlist. **Repo-map tools** remain
  parent-catalog-only for the same scoped-catalog reason.
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
