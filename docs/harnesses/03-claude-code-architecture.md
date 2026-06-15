---
title: "Claude Code Architecture — Deep Dive"
doc_id: 03-claude-code-architecture
layer: reference
captured: 2026-05-18
status: stable
sourcing: "Mixes official Anthropic docs with community reverse-engineering; RE claims flagged inline as (community RE)."
keywords: [agent loop, query(), async generator, system prompt, two-layer prompt, tool catalog, Read, Edit, Write, Bash, Grep, Glob, Agent, Task, WebFetch, ToolSearch, plan mode, subagents, hooks, MCP, memory, CLAUDE.md, auto-memory, compaction, four-tier cascade, microcompact, autocompact, permissions, deny ask allow, sandbox, settings precedence, headless, Agent SDK]
answers:
  - "How is the Claude Code agent loop (query()) built?"
  - "What are the Edit tool's three invariants and the full tool catalog?"
  - "How do plan mode, subagents, hooks, and MCP integration work mechanically?"
  - "How does the four-tier compaction cascade work?"
  - "How do permissions, sandboxing, and settings precedence work?"
  - "What is load-bearing vs. cosmetic if I re-implement Claude Code?"
related: [02-twelve-patterns, 04-claude-code-recreations, 08-design-considerations]
---

# Claude Code Architecture — Deep Dive

> Captured: 2026-05-18
> Disclaimer: portions of this document rely on community reverse-engineering of the released npm bundle (in particular the March 2026 source-map exposure and the earlier February 2025 incident). Where a claim originates from leaked/deobfuscated source rather than official documentation, it is flagged inline as **(community RE)** and, where the reverse-engineer's confidence is itself uncertain, **(community RE, unverified)**.

## Overview

Claude Code is Anthropic's first-party agentic CLI for software engineering. It pairs a tightly-engineered tool harness with the Claude family of models (Opus 4.x and Sonnet 4.x by default in 2026, with Haiku used opportunistically for fast/cheap subagents). It ships as an npm package (`@anthropic-ai/claude-code`), exposes a `claude` binary, and is also available as a VS Code/JetBrains extension, a web app, a headless mode, and — most importantly for re-implementors — as the `@anthropic-ai/claude-agent-sdk` library that lets you embed exactly the same agent loop in your own code. The official documentation lives at [code.claude.com/docs](https://code.claude.com/docs) and the [Anthropic engineering blog](https://claude.com/blog/building-agents-with-the-claude-agent-sdk).

The architecture is studied closely for two reasons. First, Claude Code is the most-cited public reference implementation of an agentic coding harness; its conventions (the Read/Edit/Bash trio, the `CLAUDE.md` memory pattern, the `Task`/`Agent` subagent tool, plan mode, and hook lifecycle) have effectively become de-facto standards now mirrored by Codex, Cursor, OpenCode, Amp, and others. Second, the minified JavaScript bundle was twice shipped with source maps included: a partial leak in February 2025 and a complete one on March 31, 2026 (~512K lines of TypeScript across roughly 1,900 files) reported by [InfoQ](https://www.infoq.com/news/2026/04/claude-code-source-leak/) and analyzed in depth by [Ghuntley](https://github.com/ghuntley/claude-code-source-code-deobfuscation), [Karan Prasad](https://karanprasad.com/blog/how-claude-code-actually-works-reverse-engineering-512k-lines), and [bits-bytes-nn](https://bits-bytes-nn.github.io/insights/agentic-ai/2026/03/31/claude-code-architecture-analysis.html). These analyses agree on the broad strokes but disagree on details; treat any single number with skepticism.

## The agent loop

The single most important piece of the architecture is what Anthropic calls the **agentic loop** and what the leaked source names `query()` — an `async function*` (async generator) of roughly 1,729 lines in the deobfuscated bundle ([community RE](https://karanprasad.com/blog/how-claude-code-actually-works-reverse-engineering-512k-lines)). The function is documented at the conceptual level in the [Agent SDK loop reference](https://code.claude.com/docs/en/agent-sdk/agent-loop).

The shape of one turn, quoted from the SDK docs:

> 1. **Receive prompt.** Claude receives your prompt, along with the system prompt, tool definitions, and conversation history.
> 2. **Evaluate and respond.** Claude evaluates the current state and determines how to proceed.
> 3. **Execute tools.** The SDK runs each requested tool and collects the results.
> 4. **Repeat.** Steps 2 and 3 repeat as a cycle. Each full cycle is one turn.
> 5. **Return result.** The SDK yields a final `AssistantMessage` (no tool calls), followed by a `ResultMessage`.

In pseudo-code:

```ts
export async function* query(params: QueryParams): AsyncGenerator<Event> {
  yield SystemMessage({ subtype: "init", session_id });
  let history = buildInitialMessages(params);   // sys prompt + tool defs + CLAUDE.md
  while (true) {
    history = await compactIfNeeded(history);    // 4-tier compaction, see below
    const stream = callModelStreaming(history);  // streaming Anthropic API call
    const assistant = yield* relayAssistantStream(stream);
    history.push(assistant);
    if (!assistant.tool_calls.length) {
      yield ResultMessage({ subtype: "success", result: assistant.text });
      return;
    }
    const results = await executeTools(assistant.tool_calls); // parallel for read-only
    history.push(...results.map(asUserToolResultMessage));
    yield UserMessage(results);
    if (turn++ >= maxTurns || cost >= maxBudgetUsd) {
      yield ResultMessage({ subtype: "error_max_turns" });
      return;
    }
  }
}
```

A turn = one model call + the resulting tool invocations. A session may run dozens of turns; for "fix the failing tests in auth.ts" the official docs give an example of four turns. The generator design is load-bearing: it gives free pause/resume, lets the host process interleave hook callbacks and permission prompts between tool executions, and gives a single uniform stream of typed events (`SystemMessage` / `AssistantMessage` / `UserMessage` / `StreamEvent` / `ResultMessage`).

Parallelism is asymmetric: read-only tools (`Read`, `Glob`, `Grep`, MCP tools whose `readOnlyHint` is true) run concurrently inside a turn, while mutating tools (`Edit`, `Write`, `Bash`) run sequentially to avoid clobbering each other ([SDK docs](https://code.claude.com/docs/en/agent-sdk/agent-loop)). The community RE puts the concurrency cap at 10 ([Sid Bharath](https://sidbharath.com/blog/the-anatomy-of-claude-code/)).

Termination conditions: model emits no tool calls; `maxTurns` exceeded; `maxBudgetUsd` exceeded; structured-output validation retry budget exceeded; an unrecoverable API/execution error. Each yields a different `subtype` on the final `ResultMessage`, and the `result` text is only populated on `success`.

## System prompt

Anthropic has not published the verbatim Claude Code system prompt. What follows is assembled from the official [memory docs](https://code.claude.com/docs/en/memory), the [agent-loop docs](https://code.claude.com/docs/en/agent-sdk/agent-loop), and community reconstructions ([zep-us/claude-system-prompt](https://github.com/zep-us/claude-system-prompt), [bits-bytes-nn](https://bits-bytes-nn.github.io/insights/agentic-ai/2026/03/31/claude-code-architecture-analysis.html), [Karan Prasad](https://karanprasad.com/blog/how-claude-code-actually-works-reverse-engineering-512k-lines)).

The prompt is assembled in **two layers** for caching efficiency: a long static prefix that almost never changes (so it's prompt-cached), followed by a short volatile suffix.

**Static, cached prefix:**
- Role framing ("You are Claude Code, Anthropic's official CLI…").
- Tone & style block. Reverse-engineers consistently quote variations of *"be concise, direct, and to the point"* and explicit rules against preamble/postamble — optimized for terminal display ([Kir Shatrov](https://kirshatrov.com/posts/claude-code-internals)). One analysis enumerates around 20 prompt-engineering techniques used here, including `<good-example>` / `<bad-example>` few-shot blocks and immutable safety rules placed *before* any user-provided content to resist injection (community RE).
- Tool-use guidance (when to use Edit vs Write, when to use Grep vs Bash grep, "prefer the dedicated tool" boilerplate that you see leaking into Claude Code's own behavior in tool descriptions).
- Refusal and safety rules.
- Long enumerated tool descriptions (built-in tools only; MCP tool schemas are appended after a cache-bust boundary so adding/removing servers doesn't invalidate the prefix — community RE).

**Volatile suffix:**
- `<env>` block injected fresh each session: working directory, OS/platform, shell, model ID, today's date, and a git status snapshot (branch, uncommitted files, recent commits). The shape is visible in Claude Code's own behavior when it surfaces the env reminder; the [Agiflow analysis](https://agiflow.io/blog/claude-code-internals-reverse-engineering-prompt-augmentation/) and [Weaxs](https://weaxsey.org/en/articles/2025-10-12/) write-ups both reproduce it.
- `CLAUDE.md` content. This is **not** in the system prompt itself — official docs are explicit: *"CLAUDE.md content is delivered as a user message after the system prompt, not as part of the system prompt itself"* ([memory docs](https://code.claude.com/docs/en/memory)). This matters: CLAUDE.md is re-injected on every request (and so prompt-cached) but it does *not* benefit from the elevated trust the model gives the system role.
- Managed-policy CLAUDE.md (`/Library/Application Support/ClaudeCode/CLAUDE.md` on macOS, `/etc/claude-code/CLAUDE.md` on Linux) is concatenated first, then user `~/.claude/CLAUDE.md`, then project `CLAUDE.md` walking up the tree, then `CLAUDE.local.md`.
- Active task list (when `TaskList` has entries).
- The first 200 lines or 25KB of `~/.claude/projects/<repo>/memory/MEMORY.md` if auto-memory is enabled.
- `<system-reminder>` blocks. These are XML-tagged hint blocks the harness *injects at end-of-turn* to nudge the model — community RE pins these as a primary mechanism for behavioral steering between turns, and you can see them yourself: when you launch Claude Code today the harness injects reminders containing skills lists, MCP server instructions, and (for plan mode) restriction reminders.

Subagents get a *different* system prompt: each subagent's `description` is included for the parent's routing decision, but when invoked the subagent runs with its own system prompt assembled from its own definition's `prompt`/`tools`/`model` fields. The subagent does **not** see the parent's conversation history; it only sees the task description the parent passes ([sub-agents docs](https://code.claude.com/docs/en/sub-agents)).

## Tool catalog

Claude Code's full tool registry in 2026 has ~40 entries ([tools reference](https://code.claude.com/docs/en/tools-reference)). The agent never sees them all at once: built-ins are eagerly defined, MCP tool schemas are deferred behind `ToolSearch` (a retrieval index for tool descriptions) unless on Vertex/Bedrock/Foundry where deferral is unsupported.

Key tools, with their actual parameter shapes and the invariants a re-implementor must keep:

**`Read(file_path, offset?, limit?, pages?)`** — official ([tools reference](https://code.claude.com/docs/en/tools-reference)). Returns file content with `cat -n`-style line numbers prefixed. Claude is instructed to pass absolute paths. Large files return an error rather than partial content (the prompt nudges the model toward `offset`/`limit`). Images are downscaled and returned as multimodal blocks. PDFs with >10 pages require an explicit `pages: "1-5"` range, max 20 pages per call. `.ipynb` returns cells with outputs. Read does **not** list directories — `Bash ls` is used for that.

**`Edit(file_path, old_string, new_string, replace_all?)`** — official. The most heavily constrained tool:

> Three checks must pass for an edit to apply:
> - **Read-before-edit**: Claude must have read the file in the current conversation, and the file must not have changed on disk since that read.
> - **Match**: `old_string` must appear in the file exactly as written.
> - **Uniqueness**: `old_string` must appear exactly once (or use `replace_all`).

A `Bash cat file` or `Bash sed -n 'X,Yp' file` on a single file with no pipes counts as a "read" for the read-before-edit invariant; `head`, `tail`, and piped output do not. This is enforced by the harness, not the model.

**`Write(file_path, content)`** — official. Replaces an entire file. If the path already exists, the read-before-edit invariant also applies. New files have no such constraint.

**`Bash(command, timeout?, run_in_background?, description?)`** — official.
- Default timeout 120s, max 600s (10 minutes), configurable via `BASH_DEFAULT_TIMEOUT_MS` / `BASH_MAX_TIMEOUT_MS`.
- Output truncated at 30,000 characters by default (hard ceiling 150,000 via `BASH_MAX_OUTPUT_LENGTH`). Overflow is written to a session file and the path is returned with a head preview.
- Each command runs in a fresh process — env vars do **not** persist across invocations, but `cd` within the project directory *does* persist for the main session (not for subagents).
- `run_in_background: true` spawns a task; output is polled via the Task* tools or the `/tasks` slash.
- Process-wrapper canonicalization strips `timeout`, `time`, `nice`, `nohup`, `stdbuf`, and bare `xargs` before permission matching.
- Compound-command awareness: a rule like `Bash(safe-cmd *)` does *not* cover `safe-cmd && other-cmd`; recognized separators are `&&`, `||`, `;`, `|`, `|&`, `&`, and newlines. Each subcommand is matched independently.
- A built-in allowlist of read-only commands (`ls`, `cat`, `echo`, `pwd`, `head`, `tail`, `grep`, `find`, `wc`, `which`, `diff`, `stat`, `du`, `cd`, read-only `git`) runs without prompting in every mode.

**`Grep(pattern, path?, glob?, type?, output_mode?, multiline?, -A/-B/-C, head_limit?)`** — ripgrep-backed. `output_mode` is `files_with_matches` (default), `content`, or `count`. Respects `.gitignore` by default. Patterns use ripgrep regex syntax, not POSIX — `interface\{\}` not `interface{}`.

**`Glob(pattern, path?)`** — sorted by mtime, capped at 100 results with a truncation flag. Unlike Grep it does **not** respect `.gitignore` unless `CLAUDE_CODE_GLOB_NO_IGNORE=false`.

**`Agent(subagent_type, prompt, ...)`** / `Task` — spawns a subagent. The parent does not see the subagent's intermediate tool calls or outputs, only its final text result, which is folded into the parent's history as the tool result. See [subagents](#subagents-task-tool).

**`WebFetch(url, prompt)`** — fetches a URL, converts HTML to Markdown, runs the user-provided `prompt` against the content with a small fast model, and returns the small-model's *answer*. This is lossy by design and not configurable. Responses cached 15 minutes. Cross-host redirects are surfaced as text rather than auto-followed (the agent must explicitly re-fetch the new URL). HTTP→HTTPS auto-upgrade. `User-Agent` begins with `Claude-User`.

**`WebSearch(query, allowed_domains?, blocked_domains?)`** — uses Anthropic's web search backend. Returns titles and URLs only; pages are read via a follow-up `WebFetch`. Up to 8 internal sub-searches per call.

**`TodoWrite(todos[])`** / `TaskCreate` / `TaskGet` / `TaskList` / `TaskUpdate` — the task-list system. `TodoWrite` is now deprecated in interactive sessions in favor of the granular Task* tools (set `CLAUDE_CODE_ENABLE_TASKS=1` to switch in headless mode too). Tasks support dependencies and status transitions and are surfaced in the TUI.

**`NotebookEdit(notebook_path, cell_id, source, cell_type?, edit_mode?)`** — Jupyter-aware. `edit_mode` is `replace` (default), `insert`, or `delete`.

**`AskUserQuestion(question, options[])`** — multiple-choice prompt to disambiguate.

**`EnterPlanMode` / `ExitPlanMode(plan)`** — see [Plan mode](#plan-mode).

**`EnterWorktree(path?)` / `ExitWorktree`** — wraps `git worktree` for isolated parallel exploration. Not available to subagents.

**`Skill(skill_name, args?)`** — loads a skill's `SKILL.md` content into context and acts on it. See [Slash commands and skills](#slash-commands-and-skills).

**`ToolSearch(query, max_results?)`** — on-demand tool-schema retrieval. Lets a long catalog of MCP tools live behind a small "name + one-line description" index that the model can expand selectively. Critical for keeping context budget sane with many MCP servers.

**`Monitor(command)`** — runs a background command and feeds each stdout line back to the agent as it arrives. Powers tail-the-log and poll-for-status workflows; uses Bash's permission rules.

**`CronCreate/List/Delete`** — session-scoped scheduled prompts (restored on `--resume`).

**`SendMessage` / `TeamCreate` / `TeamDelete`** — agent-teams, guarded by `CLAUDE_CODE_EXPERIMENTAL_AGENT_TEAMS=1`.

**`PowerShell`** — Windows-first shell, opt-in elsewhere via `CLAUDE_CODE_USE_POWERSHELL_TOOL=1`.

**`LSP`** — language-server intelligence (definition, references, type errors). Inactive until a language-server plugin is installed.

**`ListMcpResourcesTool` / `ReadMcpResourceTool`** — for MCP resource URIs.

Notable design choices a re-implementor should keep:

1. **Read returns line-numbered output.** Without line numbers, the model can't reliably target Edit ranges by reference.
2. **Edit's read-before-edit invariant.** Removing this is the single most common way to corrupt files in re-implementations; it catches the "model edits a file someone else also modified" race.
3. **Bash + edit serialize, reads parallelize.** This is a correctness property, not a performance optimization.
4. **WebFetch is lossy on purpose.** It's a context-budget defense — large pages don't get to bloat history.
5. **Tool-schema deferral via ToolSearch.** A re-implementor with many MCP servers needs this or context will collapse.

## Plan mode

Plan mode is a permission mode (`permission_mode: "plan"`), not a separate model. It is triggered by `EnterPlanMode` (or `--permission-mode plan` at launch) and exited by the model calling `ExitPlanMode(plan)` ([tools reference](https://code.claude.com/docs/en/tools-reference)).

Mechanically:
- The harness restricts the active tool surface to read-only operations: `Read`, `Grep`, `Glob`, read-only `Bash` commands, `WebFetch`, `WebSearch`, and a few orchestration tools. Any attempt to call `Edit`, `Write`, `NotebookEdit`, or a non-read-only `Bash` returns a permission denial.
- The system prompt (via `<system-reminder>` injection — community RE) is augmented with guidance to *explore first, present a plan, then ask for approval*.
- `ExitPlanMode(plan)` is the only tool whose call triggers a UI gate: the host renders the plan, the user approves or rejects, and only on approval does the harness flip the permission mode back to `default`/`acceptEdits` and let the loop continue.

In practice the Plan subagent (a built-in subagent type) is often used to do the exploration in an isolated context; it then returns a plan summary to the parent. The [official docs](https://code.claude.com/docs/en/sub-agents) name Explore, Plan, and General-purpose as the three default subagents shipped by Claude Code.

## Subagents (Task tool)

A subagent is launched via the `Agent` tool (`Task` is the legacy name still surfaced in some docs and SDK code). The [subagents docs](https://code.claude.com/docs/en/sub-agents) define the file format: a markdown file with YAML frontmatter at `.claude/agents/<name>.md` (project) or `~/.claude/agents/<name>.md` (user).

The frontmatter fields ([sub-agents docs](https://code.claude.com/docs/en/sub-agents)):

```yaml
---
name: explorer
description: Read-only repo exploration. Use when the user asks "how does X work".
model: claude-haiku-4-5            # optional; defaults to inherited
tools: [Read, Grep, Glob, Bash]    # optional; whitelist
disallowedTools: [Bash]            # optional; blacklist (takes precedence over tools)
maxTurns: 20                       # optional; cap subagent turns
effort: low                        # low | medium | high | xhigh | max
---
You are a read-only code explorer. Return a concise summary of relevant files,
their roles, and how the requested feature is wired up. Do not edit anything.
```

Behavior:
- The subagent runs in **its own context window**, with its own system prompt assembled from its `prompt` body plus the project's CLAUDE.md (subagents *do* inherit CLAUDE.md, but not the parent's conversation).
- The parent's view: only a single tool result containing the subagent's final text. The parent doesn't see the subagent's intermediate calls.
- Tool gating: `tools` whitelists, `disallowedTools` blacklists, and `disallowedTools` wins if both list a tool.
- Foreground subagents surface permission prompts to the user as their tools run; background subagents auto-deny anything that would have prompted ([tools reference, Agent tool behavior](https://code.claude.com/docs/en/tools-reference)).
- Model selection lets you route exploration to Haiku for cheap parallel fan-out and reserve Opus for the orchestrator.

When to use one (from the official docs):

> Use one when a side task would flood your main conversation with search results, logs, or file contents you won't reference again: the subagent does that work in its own context and returns only the summary.

The community RE describes a `COORDINATOR_MODE` (feature-flagged, not yet GA) that promotes the orchestrator pattern to a first-class mode where the main loop only dispatches and synthesizes, never editing files itself, with workers communicating via a mailbox-style queue ([Sid Bharath](https://sidbharath.com/blog/the-anatomy-of-claude-code/), community RE, unverified).

## Hooks

Hooks are user-defined shell or LLM callbacks that run at specific lifecycle events. They are configured under the `hooks` key in any `settings.json` ([hooks guide](https://code.claude.com/docs/en/hooks-guide)).

The events (from the [SDK loop docs](https://code.claude.com/docs/en/agent-sdk/agent-loop) and [hooks guide](https://code.claude.com/docs/en/hooks-guide)):

| Event | Fires |
|---|---|
| `SessionStart` | Session boot, before first turn |
| `UserPromptSubmit` | A user message is about to be sent |
| `PreToolUse` | Before any tool executes; can deny, allow, or transform |
| `PostToolUse` | After a tool returns |
| `Notification` | OS-level notification dispatch |
| `Stop` | Loop is about to terminate |
| `SubagentStart` / `SubagentStop` | Subagent lifecycle |
| `PreCompact` | Before automatic compaction; receives `trigger: "manual" \| "auto"` |
| `InstructionsLoaded` | Diagnostic: emits which CLAUDE.md/rule files loaded |

Configuration shape:

```json
{
  "hooks": {
    "PreToolUse": [
      {
        "matcher": "Bash",
        "command": "scripts/audit-bash.sh",
        "if": "tool_input.command contains 'rm -rf'"
      }
    ],
    "PostToolUse": [{ "matcher": "Edit|Write", "command": "npm run lint --silent" }],
    "Stop": [{ "command": "scripts/save-transcript.sh" }]
  }
}
```

Hook commands receive a JSON payload on stdin (tool name, tool input, session id) and signal back via exit code: `0` = allow, `2` = block, and stdout-as-JSON for richer responses. A blocking `PreToolUse` hook returns the rejection to the model as the tool result, and the model typically picks another approach.

Hook variants (community RE corroborated by [hooks guide](https://code.claude.com/docs/en/hooks-guide)): plain `command` hooks (shell), prompt-based hooks (call a model), agent-based hooks (full subagent), and HTTP hooks for remote services.

Security implications a re-implementor must consider:
- Hook commands are **shell commands the user wrote**, run with the user's privileges. A compromised `settings.json` (project-level, untrusted clone) can run arbitrary code at `SessionStart`. Claude Code mitigates with a trust-dialog the first time a project's `.claude/` is encountered, and with `allowManagedHooksOnly` in managed settings to lock hooks to enterprise-deployed ones only.
- `PreToolUse` hooks are also the right place to harden against prompt injection that would otherwise weaponize an allowed tool (e.g. block `Bash(curl ... | sh)`).
- Hooks run in your application process, so they don't consume model context — but `PostToolUse` hooks that print to stdout *do* feed into the next turn's history.

## MCP integration

Claude Code is an MCP client. Servers are mounted via `.mcp.json` at the project root, via user `~/.claude/settings.json`, or with `claude mcp add` ([MCP docs](https://code.claude.com/docs/en/mcp)).

`.mcp.json` shape:

```json
{
  "mcpServers": {
    "postgres": {
      "command": "uvx",
      "args": ["mcp-server-postgres", "--url", "$DATABASE_URL"],
      "env": { "DATABASE_URL": "${DATABASE_URL}" }
    },
    "github": {
      "type": "http",
      "url": "https://mcp.github.com",
      "headers": { "Authorization": "Bearer ${GITHUB_TOKEN}" }
    }
  }
}
```

Transports: stdio (subprocess), SSE (legacy), and streamable HTTP. Stdio is the default for locally-run servers; HTTP for remote/hosted ones.

Tool namespacing: every tool from server `<name>` is exposed as `mcp__<name>__<tool>`. Permission rules use the same prefix: `mcp__github__create_pull_request` matches a single tool, `mcp__github__*` or just `mcp__github` matches the whole server.

Resources and prompts are exposed via `ListMcpResourcesTool` and `ReadMcpResourceTool` (resources have URIs and can be read on demand) and as slash-commands prefixed `/mcp__<server>__<prompt>` (prompts).

**MCP Tool Search** is the 2026 mechanism that made many-server setups practical: instead of injecting every tool's schema into the system prompt, Claude Code lists just names + one-line descriptions and lets the model call `ToolSearch("databases")` to load the full schemas for what it actually needs. This is the right answer to context-pollution and is one of the patterns most worth copying. Tool search defers MCP schemas by default but falls back to upfront loading on Vertex AI and on non-first-party `ANTHROPIC_BASE_URL`s ([agent-loop docs](https://code.claude.com/docs/en/agent-sdk/agent-loop)).

## Memory system

Claude Code's persistent memory is **plain markdown files on disk**, intentionally not a vector store. This is a deliberate design value: users must be able to inspect and correct what the agent remembers ([memory docs](https://code.claude.com/docs/en/memory)).

Two parallel systems:

### CLAUDE.md (user-authored)

Files at these locations, loaded in this order (broadest first):

| Scope | Location | Loaded into |
|---|---|---|
| Managed policy | `/Library/Application Support/ClaudeCode/CLAUDE.md` (macOS), `/etc/claude-code/CLAUDE.md` (Linux), `C:\Program Files\ClaudeCode\CLAUDE.md` | Every session, every repo, every user on the machine; cannot be excluded |
| User | `~/.claude/CLAUDE.md` | Every session for that user |
| Project | `./CLAUDE.md` or `./.claude/CLAUDE.md` (walking up the tree from cwd) | Sessions launched inside the repo |
| Local | `./CLAUDE.local.md` | That worktree only; gitignored |

Concatenation rules:
- Discovered files are **concatenated, not overridden** — broadest scope appears first, most specific last (so the project file effectively "wins" by recency-bias in the model's attention).
- Nested `CLAUDE.md` files in subdirectories are loaded **on demand** when Claude reads files in that subdir, not at session start.
- `@path/to/import` syntax inlines another file (max 5 hops deep). Relative paths resolve relative to the importing file.
- Block-level HTML comments (`<!-- ... -->`) are stripped before injection.

The official guidance: keep each CLAUDE.md under 200 lines. Beyond that, split into `.claude/rules/*.md` files, optionally path-scoped:

```markdown
---
paths:
  - "src/api/**/*.ts"
---

# API rules
...
```

Path-scoped rules only enter context when Claude reads a matching file.

CLAUDE.md is delivered as a **user message after the system prompt** ([memory docs](https://code.claude.com/docs/en/memory)), not as part of the system prompt itself. Re-implementors who try to put project rules into the system role will see different (sometimes worse) compliance behavior.

### Auto-memory (Claude-authored)

Lives at `~/.claude/projects/<repo>/memory/`:
- `MEMORY.md` — index file. First 200 lines or 25KB loaded at session start.
- Topic files (`debugging.md`, `api-conventions.md`, …) loaded on demand by Claude reading them.

Claude writes to these files itself when it learns something durable (a build command, a debugging insight, a user preference like "always pnpm, not npm"). Enabled by default; toggle with `autoMemoryEnabled: false` or `CLAUDE_CODE_DISABLE_AUTO_MEMORY=1`. The `/memory` slash command opens a UI to browse, edit, and toggle.

Auto-memory is machine-local, shared across worktrees of the same repo, never synced to cloud.

`AGENTS.md` compatibility: Claude Code reads `CLAUDE.md`, not `AGENTS.md`, but `@AGENTS.md` import or a symlink works fine.

## Context management

The context window does **not reset between turns**. System prompt + tool definitions + CLAUDE.md + conversation history + tool inputs/outputs all accumulate. The cached prefix (system prompt + tools + CLAUDE.md) is prompt-cached on every request so only the first turn pays full cost ([agent-loop docs](https://code.claude.com/docs/en/agent-sdk/agent-loop)).

When the window approaches its limit, automatic compaction kicks in. The leaked source describes a **four-tier strategy applied cheapest-first** ([bits-bytes-nn](https://bits-bytes-nn.github.io/insights/agentic-ai/2026/03/31/claude-code-architecture-analysis.html), community RE; this matches what users observe in practice):

| Tier | Cost | Loss | What it does |
|---|---|---|---|
| Snip / wholesale drop | free | high | Drop old messages outright |
| Microcompact | free | medium | Cache-aware selective clearing of old tool outputs |
| Context Collapse | low | medium | Re-project read-time state (e.g. replace a 5000-line file Read with "you read X; refer to file if needed") |
| Auto-compact | high | low | LLM-powered summarization of older history |

A buffer of ~13K tokens is reserved against the model's window limit to avoid hitting the wall during a turn (community RE, [Karan Prasad](https://karanprasad.com/blog/how-claude-code-actually-works-reverse-engineering-512k-lines)). Auto-compact fires when total context crosses ~95% (officially "approaches the limit"; community estimate ~167K on 200K models).

Compaction emits a `SystemMessage` with `subtype: "compact_boundary"` (or, in TS, a dedicated `SDKCompactBoundaryMessage`). A `PreCompact` hook fires first and can archive the pre-compaction transcript.

What survives compaction (from [memory docs](https://code.claude.com/docs/en/memory)):
- The system prompt and tool definitions (they were never in the conversation history).
- Project-root CLAUDE.md is **re-read from disk and re-injected** after compaction.
- Nested CLAUDE.md files in subdirs are **not** re-injected automatically; they reload on next subdir access.
- Auto-memory is re-read on every session boot but is not part of the per-turn replay.
- Conversation messages older than the boundary are replaced by the summary.

Manual controls: `/compact [optional instructions]`, `/clear` (full reset; new session preloaded with previous summary if `/compact` precedes it).

A few re-implementor tips fall straight out of this design:
- Put durable rules in CLAUDE.md, not in the first user message — the first user message is summarized away.
- Keep tool outputs short. A 20K-line `find` result wrecks your compaction budget.
- Use subagents for spike-and-summarize work; the parent only inherits the subagent's final text.

## Permissions and sandboxing

The permission system is the most operationally important guardrail. Rules are evaluated in the order **deny → ask → allow**, first match wins, so denies always dominate ([permissions docs](https://code.claude.com/docs/en/permissions)).

**Permission modes:**

| Mode | Behavior |
|---|---|
| `default` | First use of each non-read-only tool prompts |
| `acceptEdits` | Auto-approves file edits + common fs commands (`mkdir`, `touch`, `mv`, `cp`); other Bash still gated |
| `plan` | Read-only; agent explores and proposes a plan |
| `auto` | Per-call decision by a separate model classifier (research preview) |
| `dontAsk` | Anything not in an allow rule is denied silently |
| `bypassPermissions` | Skip prompts entirely. `rm -rf /` and `rm -rf ~` still prompt as a circuit breaker. Cannot run as root on Unix. Disablable via `permissions.disableBypassPermissionsMode` |

**Rule syntax**: `Tool` or `Tool(specifier)`. Specifiers vary by tool:
- `Bash(npm run *)` — command pattern with wildcards; word-boundary rules around trailing `*`; `:*` is equivalent to ` *` at end-of-pattern.
- `Read(./.env)` / `Edit(/src/**)` — gitignore-style globs with anchoring: `//absolute`, `~/home`, `/project-relative`, bare/`./cwd-relative`. Read deny rules also apply to recognized read-y Bash commands (`cat`, `head`, `tail`, `sed`). An `Edit(...)` allow grants implicit `Read(...)` on the same path.
- `WebFetch(domain:example.com)` — domain match.
- `Agent(Explore)` — gate by subagent name.
- `mcp__server__tool` — MCP namespacing.

**Compound Bash awareness**: Claude Code parses operators (`&&`, `||`, `;`, `|`, `|&`, `&`, newline) and checks each subcommand independently. "Yes, don't ask again" on `git status && npm test` writes a separate rule for `npm test`, not for the compound string.

**Process-wrapper canonicalization** strips `timeout`, `time`, `nice`, `nohup`, `stdbuf`, and bare `xargs` before matching — but **not** `direnv exec`, `devbox run`, `mise exec`, `npx`, `docker exec`, which are treated as opaque. This is a deliberate choice: `Bash(devbox run *)` would otherwise be a backdoor to anything inside the devbox.

**Sandboxing** is a separate, OS-level layer (Linux landlock/seccomp; macOS sandbox-exec) that restricts Bash commands' filesystem and network access regardless of what the model decides. With `autoAllowBashIfSandboxed: true` (default), the sandbox boundary substitutes for the per-command prompt — except for `rm`/`rmdir` targeting `/`, `~`, or critical system paths, which still prompt. Read/Edit deny rules merge into the sandbox boundary; `WebFetch(domain:...)` allow rules merge into the sandbox's `allowedDomains`.

The transcript-classifier path: in `auto` mode, a separate Claude API call evaluates each tool call for safety. Reverse-engineers describe this as a "transcript classifier" and report a `tengu_transcript_classifier_*` family of feature flags (community RE).

## Slash commands and skills

Two related-but-distinct extension points.

**Slash commands** historically lived under `.claude/commands/<name>.md` (project) and `~/.claude/commands/<name>.md` (user). The file is a markdown body; the filename becomes the command name; invoking `/<name> args` injects the body (with `$ARGUMENTS` substituted) as a user message.

**Skills** live under `.claude/skills/<name>/SKILL.md`, with YAML frontmatter that includes a `description` the agent uses to decide when to auto-invoke. Skills can have associated files (the directory can carry scripts and reference docs) and can declare `allowed-tools` to restrict what they can do.

In 2026 these have effectively merged: files in either location create the same `/slash-command` interface, but `.claude/skills/` is the recommended location going forward ([Alexop](https://alexop.dev/posts/claude-code-customization-guide-claudemd-skills-subagents/), [official slash-commands docs](https://code.claude.com/docs/en/agent-sdk/slash-commands)).

Built-in slash commands include `/clear`, `/compact`, `/help`, `/model`, `/cost`, `/permissions`, `/memory`, `/mcp`, `/init`, `/add-dir`, `/tasks`, `/schedule`, plus an experimental `/review` and `/security-review`. These are hardcoded — you cannot redefine them with a custom skill of the same name.

Discovery: skills under `.claude/skills/` are picked up at session start; their `description` field enters the system prompt (or a `<system-reminder>` block) so the model can decide when to invoke `Skill(name, args)`. Project skills + user skills + plugin skills all merge.

The `Skill` tool itself is a thin wrapper: it loads the SKILL.md body, opens any referenced files as context, and lets the loop proceed. Skills can spawn subagents and call any tool not blocked by their own `allowed-tools` frontmatter.

## Settings precedence

Settings live in `settings.json` files. From highest to lowest precedence ([permissions docs](https://code.claude.com/docs/en/permissions), [settings docs](https://code.claude.com/docs/en/settings)):

1. **Managed (policy)** — `/etc/claude-code/managed-settings.json` (Linux/WSL), `/Library/Application Support/ClaudeCode/managed-settings.json` (macOS), Windows registry + `C:\Program Files\ClaudeCode\managed-settings.json`. Deployed via MDM/Group Policy/Ansible. Cannot be overridden.
2. **Command-line arguments** — temporary session overrides (`--settings file.json`, `--add-dir`, `--allowedTools`, `--permission-mode`).
3. **Local project** — `.claude/settings.local.json`. Gitignored.
4. **Shared project** — `.claude/settings.json`. Committed.
5. **User** — `~/.claude/settings.json`.

Merging rules:
- Permission arrays *merge* across layers — they don't replace. A user `allow` and a project `allow` both apply.
- Denies from any layer beat allows from any other layer. A user-level deny blocks a project-level allow, and vice versa.
- Single-value keys (e.g. `defaultMode`) are overridden by the higher-precedence layer.
- A handful of keys are **managed-only**: `allowManagedHooksOnly`, `allowManagedMcpServersOnly`, `allowManagedPermissionRulesOnly`, `disableBypassPermissionsMode`, `forceLoginMethod`, etc. Placing them in user/project settings has no effect.

`/permissions` opens a UI that shows the merged rule set and which file each rule came from — invaluable for debugging "why was this not allowed".

## CLI surface and IDE integration

Claude Code ships in five form factors:

1. **Terminal CLI** (`claude`) — npm-installed, the canonical surface. Subcommands include `claude` (interactive), `claude -p "<prompt>"` (one-shot), `claude --resume <session-id>`, `claude --continue`, `claude mcp add/list/remove`, `claude config`.
2. **VS Code extension** — sidebar chat panel, inline diffs, checkpoint rewind (per-tool-call undo), `@file:line` mentions, multi-conversation tabs. The extension talks to a `claude` binary under the hood — it doesn't reimplement the loop.
3. **JetBrains plugin** — same model, slightly different UX.
4. **Web app and Desktop app** — claude.ai-hosted sessions ("web sessions") with cloud-backed file editing for users on Pro/Max/Team/Enterprise plans. Routines (scheduled remote agents) live here.
5. **Claude Agent SDK** — `@anthropic-ai/claude-agent-sdk` (TypeScript) and `claude_agent_sdk` (Python). The SDK is the agent loop without the TUI — you write `for await (const msg of query({...}))` and handle messages yourself. This is the entry point for re-implementors who want Claude Code's behavior but with their own UI or in their own service ([Anthropic engineering blog](https://claude.com/blog/building-agents-with-the-claude-agent-sdk)).

Headless mode (`claude -p` or the SDK with no TUI) is what powers GitHub Actions integrations, CI checks, scheduled routines, and the agent-team experimental mode.

The session state persists at `~/.claude/projects/<repo>/<session-id>.jsonl` — append-only JSON-lines transcripts including messages, tool calls, costs, and git context. This is how `--resume` works ([Sid Bharath](https://sidbharath.com/blog/the-anatomy-of-claude-code/)).

## What's load-bearing vs. cosmetic

If you're re-implementing Claude Code's behavior, these are the architectural choices that *must* be kept; everything else is variation.

**Load-bearing:**

1. **The streaming async-generator loop.** Yielding typed events instead of returning a buffered list is what makes hooks, permission prompts, cancellation, and resume sane.
2. **Read returns line-numbered text.** Without this, surgical edits don't work.
3. **Edit's read-before-edit + uniqueness + exact-match invariants.** Drop any one and you'll corrupt files in production within a week.
4. **Bash compound-command awareness for permissions.** Treating `safe-cmd && evil-cmd` as a single string is a privilege escalation.
5. **Process-wrapper canonicalization** with a closed, hardcoded list. Adding `docker exec` to the list would break permission boundaries.
6. **Read-only parallelism, mutating-serial.** This is correctness, not perf.
7. **Subagent isolation — own context, only final text returns.** Without it you cannot fan out exploration without blowing context budget.
8. **Tool-schema deferral (ToolSearch).** Required for many-MCP-server setups.
9. **Multi-tier compaction with cheapest-first.** Single-shot summarization is the obvious wrong answer; you want microcompact and context-collapse to fire long before you pay for an LLM summary.
10. **CLAUDE.md as user message, not system message.** This is subtle but the official docs are unambiguous; getting it wrong changes the model's deference.
11. **Permissions enforced by the harness, not the model.** The model is *advisory*; the rules are *authoritative*.
12. **Deny > ask > allow precedence, evaluated across merged scopes.** Anything else is a footgun.
13. **Sandbox as a separate OS-level layer.** Permission rules are a model-trust boundary; the sandbox is a process-trust boundary. You need both.

**Cosmetic (or at least: replaceable without changing essential behavior):**

- The exact slash commands (`/clear`, `/compact`, `/help`). Pick your own names.
- The terminal TUI's specific rendering.
- The `~/.claude/` directory layout. Any persistent on-disk store works.
- Auto-memory's specific filenames (`MEMORY.md`, topic files). Any append-only markdown store works.
- The choice of ripgrep for Grep (any fast searcher works), and the 100-result cap for Glob.
- Skill discovery via YAML frontmatter `description` — a manifest file or a registry endpoint would work equally well.
- The four-provider fallback chain (Anthropic → Bedrock → Vertex → Foundry). One model provider is fine for a re-impl.
- Hidden features like KAIROS/ULTRAPLAN/BUDDY ([MindStudio](https://www.mindstudio.ai/blog/claude-code-source-code-leak-hidden-features)) and the experimental agent-teams mode — these are product bets, not architecture.

## Reading list

### Official (Anthropic)
- [Tools reference](https://code.claude.com/docs/en/tools-reference) — authoritative tool list with permission shapes and per-tool behavior.
- [Agent loop docs](https://code.claude.com/docs/en/agent-sdk/agent-loop) — the loop, message types, compaction, hooks, model selection.
- [Permissions](https://code.claude.com/docs/en/permissions) — modes, rule syntax, precedence, sandbox interaction.
- [Memory](https://code.claude.com/docs/en/memory) — CLAUDE.md hierarchy, auto-memory, rules, AGENTS.md compat.
- [Subagents](https://code.claude.com/docs/en/sub-agents) — frontmatter fields, tool gating, fork mode.
- [Hooks guide](https://code.claude.com/docs/en/hooks-guide) — event list, JSON I/O contract.
- [MCP](https://code.claude.com/docs/en/mcp) — transports, `.mcp.json`, tool search.
- [Slash commands in the SDK](https://code.claude.com/docs/en/agent-sdk/slash-commands).
- [Anthropic engineering: Building agents with the Claude Agent SDK](https://claude.com/blog/building-agents-with-the-claude-agent-sdk) — official design principles ("give Claude a computer", gather→act→verify→repeat).
- [Anthropic engineering: Building effective agents](https://www.anthropic.com/research/building-effective-agents) — the workflow vs. agent distinction and pattern catalog.
- [Anthropic Cookbook agent patterns](https://github.com/anthropics/anthropic-cookbook/tree/main/patterns/agents).

### Community reverse-engineering (treat with verification)
- [ghuntley/claude-code-source-code-deobfuscation](https://github.com/ghuntley/claude-code-source-code-deobfuscation) — cleanroom deobfuscation of the npm bundle; the canonical primary source.
- [Yuyz0112/claude-code-reverse](https://github.com/Yuyz0112/claude-code-reverse) and [the July 2025 update](https://yuyz0112.github.io/claude-code-reverse/) — log parsing and visualization of LLM interactions.
- [zep-us/claude-system-prompt](https://github.com/zep-us/claude-system-prompt) — reconstructed system prompts, partially validated against Anthropic's published prompts.
- [InfoQ: Anthropic accidentally exposes Claude Code source via npm source map](https://www.infoq.com/news/2026/04/claude-code-source-leak/) — the March 2026 incident report.
- [afterpack.dev: Claude Code's Source Didn't Leak — It Was Already Public for Years](https://www.afterpack.dev/blog/claude-code-source-leak) — counter-narrative on the leak history.

### Blog analyses (varying depth and accuracy)
- [Karan Prasad: Reverse-engineering 512K lines of production AI agent](https://karanprasad.com/blog/how-claude-code-actually-works-reverse-engineering-512k-lines) — strongest single deep-dive on query loop, prompt structure, model fallback, IPC.
- [bits-bytes-nn: Claude Code architecture analysis](https://bits-bytes-nn.github.io/insights/agentic-ai/2026/03/31/claude-code-architecture-analysis.html) — six-stage per-turn pipeline, four-tier compaction, eight-layer security.
- [Sid Bharath: The Anatomy of Claude Code](https://sidbharath.com/blog/the-anatomy-of-claude-code/) — eight-step loop, permission layering, parallel tool execution, session recording.
- [Kir Shatrov: Reverse engineering Claude Code](https://kirshatrov.com/posts/claude-code-internals) — early (2025) walk-through, tool taxonomy, model selection.
- [Agiflow: Reverse-engineering prompt augmentation](https://agiflow.io/blog/claude-code-internals-reverse-engineering-prompt-augmentation/) — the four-layer prompt-injection mechanism (system, message, tool, conversation).
- [Weaxs: Brief analysis of Claude Code's execution and prompts](https://weaxsey.org/en/articles/2025-10-12/) — env-block dissection.
- [Alexop: Customization guide (CLAUDE.md, slash commands, skills, subagents)](https://alexop.dev/posts/claude-code-customization-guide-claudemd-skills-subagents/) — pragmatic survey current to 2026.
- [BrightCoding: Inside Claude Code RE report](https://www.blog.brightcoding.dev/2025/07/17/inside-claude-code-a-deep-dive-reverse-engineering-report/).
- [shareAI-lab/learn-claude-code](https://github.com/shareAI-lab/learn-claude-code) — "Bash is all you need" nano-clone for learning.
- [Context compaction research across Claude Code, Codex, OpenCode, Amp](https://gist.github.com/badlogic/cd2ef65b0697c4dbe2d13fbecb0a0a5f) — comparative deep-dive on compaction.
- [Justin3go: Shedding heavy memories — context compaction in Codex/Claude Code/OpenCode](https://justin3go.com/en/posts/2026/04/09-context-compaction-in-codex-claude-code-and-opencode).
- [Hidden features (MindStudio)](https://www.mindstudio.ai/blog/claude-code-source-code-leak-hidden-features) — KAIROS, ULTRAPLAN, BUDDY, Undercover Mode.
- [CVE-2025-54794/54795: InversePrompt](https://cymulate.com/blog/cve-2025-547954-54795-claude-inverseprompt/) — security analysis of prompt-injection paths in Claude Code.

### Gaps
- No publicly verified verbatim Claude Code system prompt as of 2026-05-18; community reconstructions agree on structure and most language but the exact text isn't authoritatively known.
- Internal feature-flag names (e.g. `COORDINATOR_MODE`, `KAIROS`, `tengu_*`) are sourced only from community RE and may be renamed/removed at any time.
- Exact compaction thresholds and per-tier behavior are inferred from logs and partial source; treat the four-tier model as directionally correct, not numerically precise.
- The exact line counts and file counts for the leak (1,729-line query loop; ~1,900 files; ~512K lines) differ slightly between sources and are best treated as order-of-magnitude.

---

**Previous:** [02 · Twelve Patterns](./02-twelve-patterns.md) · [↑ Index](./INDEX.md) · **Next:** [04 · Claude Code Recreations](./04-claude-code-recreations.md)
