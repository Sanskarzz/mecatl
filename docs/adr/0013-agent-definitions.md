# ADR 0013 — Agent definitions (Tier 1)

- Status: Accepted
- Date: 2026
- Scope: named specialist agent definitions discovered from operator-controlled markdown files, consumed by the Subagent and team-member delegation paths

## Context

mecatl's Subagent and Parallel tools delegated to anonymous explorers with no persona, fixed tooling, and the parent's model. Operators needed reusable named specialists — a code reviewer, an implementer, a researcher — with their own system-prompt body, tool allowlist, model, run limits, per-agent MCP servers, hooks, and persistent memory, without leaking registry or filesystem concerns into the domain.

## Decision

Agent definitions are operator-controlled markdown files with YAML frontmatter that are discovered once at build time via a snapshot port (`AgentDefSource`). The `AgentDef` value object is pure data — no path, no locator — and the composition layer is the only consumer of the discovery adapter. Both the Subagent delegate path and the team-member spawn path share one definition format and one resolution chain.

## Consequences

Named specialists work across both delegation surfaces without duplicating the format. The domain stays free of filesystem and registry concerns; per-def MCP, hooks, and memory are adapter-construction options, never domain fields. Mutating specialists must use the team-member fork path — the Subagent path stays read-only. Per-agent memory write and a `local` memory tier are deferred to a future ADR.

---

Named subagent specialists discovered from operator-controlled markdown files
(`<dir>/<name>.md`, YAML frontmatter + body), mirroring the skills Source seam. **One
definition, two consumers**: a `<name>.md` is reusable both as a Subagent delegate
(`Subagent(agent="<name>")`) and as a team-member role (`MemberSpec.AgentType` /
`SpawnTeammate(agent_type=...)`).

See `internal/adapter/agents/` (discovery + `Registry`) and
`internal/app/agentdefs.go` + `internal/app/build.go` (the registry→engine
translation, scoping, model/mode resolution — the layering rule keeps the `Registry`
out of `engine/agent`, which receives only `map[string]*Engine` / `MemberBuild`).

The port seam is `engine/tool/agentsource.go`: `AgentDef` (a pure value object — no
path/locator concept; `Origin` is a closed admission-tier label, never a location;
`AgentMCPServer.Headers` is SECRET-SHAPED — never logged or projected into any
inventory/snapshot surface) and `AgentDefSource` (SNAPSHOT semantics —
`ListAgentDefs` is stable for the source's life; the harness resolves once at
build). Where a def came from is the adapter's NON-PORT detail channel
(`agents.Discovered.Detail` / `Registry.Detail`), never a port field.

## Frontmatter

```yaml
---
name: code-reviewer            # REQUIRED — routing key / AgentType handle
description: Reviews a diff...  # REQUIRED — always-in-context routing metadata (capped)
tools: [Read, Grep, Glob]      # OPTIONAL — allowlist of CORE tool names (array or "a, b" string)
disallowedTools: [Write]       # OPTIONAL — subtractive filter applied after tools/default
model: sonnet                  # OPTIONAL — alias | full id | inherit/empty (=> parent)
provider: openrouter           # OPTIONAL — provider id (openai|openrouter|…). Empty => inherit
                               #   the session's provider, or the build-time default for a
                               #   non-selected session. Orthogonal to model:; when it SWITCHES
                               #   provider the model rebases off model: (or the provider default),
                               #   never the parent model. Unknown provider => loud parent fallback.
permissionMode: plan           # OPTIONAL — default | plan | acceptEdits
maxTurns: 9                    # OPTIONAL — per-run turn cap (0/absent => call-site default)
maxToolCalls: 25               # OPTIONAL — per-run tool-call cap (0/absent => call-site default)
color: blue                    # OPTIONAL — UX hint only; never affects execution
skills: [refactoring, testing] # OPTIONAL — skill names PRELOADED into this def's prompt (array or "a, b" string)
hooks:                         # OPTIONAL — phase → shell-command map scoped to this def's engine
  PreToolUse: ./scripts/gate.sh
mcpServers:                    # OPTIONAL — per-agent MCP (reference an existing server OR inline a new one)
  - github                     #   REFERENCE: a configured main server's name (gets its tools)
  - name: jira                 #   INLINE: a streamable-HTTP server only THIS def connects
    url: https://jira.example/mcp
    headers: { Authorization: "Bearer ${TOKEN}" }
memory: project                # OPTIONAL — persistent per-agent memory tier: user | project
                               #   (READ-ONLY in v1). user => a cross-project dir under the XDG
                               #   config base; project => <workspace>/.mecatl/agents-memory/<name>/,
                               #   trust-gated (only read when the workspace is --trust-project'd).
                               #   The dir's MEMORY.md head is injected as fenced DATA into the def's
                               #   cache-stable system prompt at startup. "local" is deferred.
---
You are a meticulous code reviewer. <full body = the specialist's system-prompt instructions>
```

## What v1 supports

- **Prompt persona.** The body is composed into the engine's system prompt as the
  Role (a cache-stable StablePrefix layer; one byte-stable prefix per def).
- **Scoped CORE tools.** The def's `tools` allowlist (minus `disallowedTools`) is
  intersected with the call site's available **core** toolset (Read/Edit/Write/Grep/
  Glob/WebFetch/Bash). `Subagent`/`Parallel`/`ToolSearch` are ALWAYS excluded (no nesting / no
  silent disclosure tool).
  - **Subagent delegates are read-only EXPLORERS WITH A SHELL** — `Subagent.ReadOnly()` stays
    `true`, but a Subagent child now runs in an isolated git **worktree** (when Bash is
    configured AND the workspace is trusted — issue #40: an untrusted workspace yields
    no read-only-child shell at all), so it KEEPS Bash for inspection (git log/show, cat, build, test) while
    **Edit/Write are still dropped** on the Subagent path (with a startup diagnostic) — a
    def's Bash survives via `scopedToolNamesMode`'s `allowShell`. Its writes land in the
    throwaway worktree, never the shared base, which is why `ReadOnly()` stays true. A
    truly **mutating** specialist (edits the project's files) is a **team member**
    (force-copy fork) or a **Parallel** branch, not a Subagent.
  - **Team members** obey read-only-share / mutating-fork: a `Mutating` member (runs
    in an isolated fork) MAY keep Edit/Write/Bash; a read-only (base-sharing) member
    has them dropped (so the supervisor's `ErrReadOnlyMemberMutating` backstop never
    trips). Team coordination tools (`MemberTools`) are always appended and bypass the
    allowlist.
- **Per-def model.** `def.Model` > global `--subagent-model` > parent `--model`.
  Aliases (`--model-alias name=id`, plus built-in `sonnet`/`opus`/`haiku`→inherit)
  resolve only in the composition layer; the domain always gets a concrete id. An
  unknown alias warns and inherits.
- **Per-def run limits (`maxTurns`/`maxToolCalls`).** A def's `maxTurns`/`maxToolCalls`
  map to the child session's `session.Limits`, **per-field** over the call site's
  default (the Subagent tool's `agent.DefaultChildLimits()` / the team's `WithTeamLimits`):
  a def that pins only `maxTurns` keeps the default tool-call/failure caps, and a def
  that pins neither runs on the default unchanged. Wired on BOTH paths — the Subagent path
  carries the limits on `agent.AgentMeta.Limits` (the Subagent tool builds the routed
  child session under them), and the member path carries them on
  `agent.MemberBuild.Limits` (the supervisor's `AddMember` per-field merges them onto
  the team default `s.limits`). The int→`Limits` mapping (`defLimits`) lives in the
  composition layer; the domain stays free of agent-def types.
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
  diagnostic (not preloaded). Wired in BOTH the per-def Subagent engine
  (`buildAgentSubagentEngines`) and the team-member engine (`buildMemberEngine`) via
  `resolveSkillIndex` + `preloadedSkillBodies`.
- **Per-agent MCP servers (`mcpServers:`).** A def's `mcpServers:` scopes specific MCP
  servers' tools to THAT def's engine, in BOTH call sites (Subagent delegate and team
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
    `Subagent`/`Parallel`/`ToolSearch` exclusion + the def's core `tools`/`disallowedTools`
    semantics are unchanged.
  - **Read-only backstop interaction.** MCP tools report `ReadOnly()==false` but never
    touch the workspace, so they are EXEMPT from the supervisor's read-only-member
    workspace-mutating-tool backstop (`ErrReadOnlyMemberMutating`) — the same way the
    team coordination tools are exempt. A read-only member may therefore safely hold MCP
    tools; a genuine workspace-mutating tool (Edit/Write/non-RO Bash) is still rejected.
    The exemption is carried as `MemberBuild.MCPToolNames`, which the supervisor folds
    into its exemption set.
  - **Lifetime model.** A **Subagent-path** def engine is built once at composition
    (`buildAgentSubagentEngines`); its inline managers' `Close` is aggregated into
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
- **Per-agent persistent memory (`memory:`, issue #33).** A def's `memory: user|project`
  binds a per-agent dir (`<root>/agents-memory/<sanitized-name>/MEMORY.md`); its head is
  read once at build time, bounded (~8 KiB, head-first with a truncation marker),
  injection-scanned, fenced as UNTRUSTED **DATA**, and injected into the def's
  **cache-stable** system-prompt prefix via the shared `agentPromptConfig` seam (so the
  Subagent-routed AND team-member paths get it identically; it rides the StablePrefix,
  never a per-turn message, so prompt-prefix caching is preserved). The **user** tier
  resolves under the XDG config base (cross-project); the **project** tier resolves under
  `<workspace>/.mecatl/agents-memory/` and is **`--trust-project`-gated** — the workspace
  is attacker-controllable, so an untrusted repo's project memory is withheld regardless
  of the def's own `Origin`. `memory: project` resolves against `cfg.Workspace/.mecatl/`
  INDEPENDENTLY of where the def file itself lives: a **user-tier def can point its memory
  at the workspace**. This is an intentional decoupling of "where the def lives" from
  "where its memory lives" — and exactly why the trust gate keys on the resolved tier, not
  on `Origin` (so a user-tier def still can't read an untrusted workspace). The def name is
  path-sanitized (allowlist token + post-join containment check) so a hostile name can't
  traverse; the resolved `MEMORY.md` is additionally symlink-contained (CWE-59 — a symlink
  to an out-of-tree secret is refused) and the def name in the prompt header is
  framing-neutralised (CWE-117). A token collision (two names → one dir) is benign for
  read-only v1 but the deferred WRITE path must key on a collision-free identity.
  **READ-ONLY in v1** (injection only): a memory-bearing def gains **no** write tools. See
  `resolveAgentMemoryHead` + `safeAgentMemoryDir` + `memoryPathContained`.
- **Forgiving resolution.** Conventional discovery is on by default and **inert** when
  no dir exists. An unknown member `AgentType` falls back to the default member
  catalog (warn, never fail the spawn). An unknown Subagent `agent` arg is a
  model-addressable error listing valid names (the model can retry).

## What v1 does NOT do (honest parity vs Claude Code)

- **No mutating Subagent delegates.** CC lets a `Task(subagent_type=...)` inherit Edit/
  Write and the project's tools. Our read-parallel/mutate-serial dispatcher makes a
  mutating Subagent a workspace-race hazard, so mutation is routed through the isolation
  mechanisms that already exist (member fork / Parallel worktree). On the Subagent path the
  only thing a def adds over the anonymous explorer is a different prompt + model +
  read-only tool scope.
- **Per-agent MCP `tools:` allowlisting still core-only.** A def's `mcpServers:` adds the
  server's tools to the def's catalog directly (see "What v1 supports"), but listing an
  MCP tool name in the `tools:` allowlist still yields an "unknown tool" diagnostic —
  the `tools:`/`disallowedTools:` scope governs the **core** toolset only; MCP tools
  arrive via `mcpServers:`, not the core allowlist.
- **No `--agents` inline JSON.** Definitions come from `<name>.md` files under
  `--agents-dir` / the conventional dirs, or from a remote gRPC driver via
  `--agent-source-url` (`mecatl.driver.v1.AgentSourceService`; snapshotted at
  startup, mutually exclusive with `--agents-dir`, supersedes conventional
  discovery).
- **No Agent-as-tool nesting.** A def cannot re-add `Subagent`/`Parallel`/`ToolSearch`; a child
  never recurses or fans out further.
- **No file-path scoping** (Roo's file allowlist) yet.
- **No per-agent memory WRITE path (deferred).** `memory:` is READ-ONLY in v1: the def's
  MEMORY.md is injected but the agent cannot update it. The scoped write path (the six
  memory tools scoped to the per-agent dir, one flock'd `Store` per dir) and a `local`
  tier are deferred; the `agents-memory/<name>/` directory scheme is forward-compatible
  with adding them. A remote agent-source DRIVER def's `Memory` is also deferred (no proto
  change in v1 — a driver def stays cold-start).

## Flags

```
--agents-dir <dir>            repeatable; explicit dirs, highest precedence
--agents-conventional[=true]  also discover .mecatl/agents, .claude/agents, user dirs
                              (ON by default, inert when absent; like teams/fork)
--agent-source-url <addr>     host:port of a remote agent-definition gRPC driver
                              (mecatl.driver.v1.AgentSourceService); snapshotted at
                              startup, mutually exclusive with --agents-dir,
                              supersedes conventional discovery
--subagent-model <id|alias>   global model override for Subagent/member children
--model-alias name=id         repeatable alias→id map (composition layer only)
```

The TUI's embedded server (`cmd/mecatui`) enables `AgentsConventional` by default,
consistent with `EnableTeams`/`EnableParallel`.


---

*Part of the [design docs](../design/README.md). Related: [Spike: Headless Agent Teams for mecatl](0014-agent-teams.md), [BACKGROUND-SUBAGENTS.md — Background Subagents + Per-Child Cancel over a Shared Child-Run Registry](0015-background-subagents.md).*
