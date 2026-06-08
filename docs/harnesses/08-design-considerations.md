---
title: "Design Considerations — Building Your Own Harness in 2026"
doc_id: 08-design-considerations
layer: practice
captured: 2026-05-18
status: stable
keywords: [load-bearing decisions, MVP roadmap, v0, v1, v2, v3, cost levers, security levers, sandbox layers, ideas to steal, anti-patterns, edit format, context strategy, multi-model, model pointers, headless, async PR, memory, sanity-check gauntlet, prompt caching, subagents]
answers:
  - "What should I actually build, and in what order (MVP through v3)?"
  - "What are the 13 load-bearing design decisions?"
  - "Which cost and security levers matter most early?"
  - "What ideas should I steal from which project?"
  - "What bets has the field NOT settled, and what are the pragmatic defaults?"
  - "How do I sanity-check my harness design before writing more code?"
related: [01-overview, 06-architecture-patterns, 07-context-and-mcp]
---

# Design Considerations: Building Your Own Harness in 2026

> Captured: 2026-05-18
> This file is the opinionated synthesis. Treat earlier files (02–07) as the
> evidence; this file is the argument.

If you've read the other files in this corpus, you've seen a coherent picture emerge. The agentic coding harness has converged on a small set of load-bearing ideas; everything else is variation. This document distills that convergence into a working argument for someone building one.

## TL;DR

Build the shape, not the feature list. The shape is: a single streaming agent loop, ~7 well-described core tools, a TodoWrite-style planning tool, an enforced plan/act gate, one-shot subagents with isolated context, deterministic hooks at every lifecycle event, a permission system that is *deny → ask → allow* across merged layers, prompt caching at every stable boundary, a four-tier compaction cascade, MCP as the extensibility surface, and an OS-level sandbox as a separate concern from the permission model. Everything else is taste.

If you remember only five things from the rest of this corpus, remember these:

1. **Prompt caching is table stakes, not optimization.** Without it, a long session costs ~10× more than it should. Anthropic 4-breakpoint, 5-minute TTL (1-hour at 2× write); OpenAI automatic above 1024 tokens. Design the system prompt and tool definitions to be cache-stable across turns from day one.
2. **Tool inventory is a context budget item, not a feature checklist.** Past ~30 tools you need the code-execution-with-MCP pattern: tools live as TypeScript modules on a sandboxed filesystem, the model writes code that imports them. Anthropic measured 98.7% token reduction on real workflows.
3. **Enforce constraints with hooks and sandboxes, not prompts.** "Don't write to /etc" in the system prompt fails one time in a thousand. A `PreToolUse` hook fails zero. Anything that matters for correctness or security must be enforced deterministically.
4. **Subagents (one-shot Task delegation) are the safe form of multi-agency.** Full multi-agent costs ~15× the tokens and introduces decision-conflict bugs (Cognition's reading). Subagents preserve the context-isolation benefits without the coordination cost — because there's no coordination, just a parent who waits for a child's final string.
5. **Read and write are different operations.** Plan/act, permission tiering, subagent gating, edit-tool invariants — all the harness's strongest patterns draw the same line. Read is cheap and reversible; write is expensive and irreversible. Make that asymmetry visible everywhere.

## The 13 load-bearing decisions

Pulled from the corpus, rewritten as decisions you'll have to make:

### 1. Use a streaming async-generator loop

A `for await (msg of query(...))` loop that yields typed events (`SystemMessage`, `AssistantMessage`, `UserMessage`, `StreamEvent`, `ResultMessage`) gives you free pause/resume, lets hooks and permission prompts interleave between tool calls, and is the only sane way to support cancellation. Don't return a buffered list; yield events. [03 §"The agent loop"]

### 2. Read returns line-numbered text

Without persistent line numbers in the Read tool's output, surgical edits don't work. Every credible Claude Code recreation kept this. [03 §"Tool catalog"]

### 3. Edit has three invariants: read-before-edit, exact-match, uniqueness

Edit cannot apply if the file hasn't been read in the current conversation (or has changed since), if `old_string` doesn't match exactly, or if `old_string` is non-unique without `replace_all`. Drop any one and you'll corrupt files in production within a week. [03 §"Tool catalog"]

### 4. Read-only tools parallelize; mutating tools serialize

Bash, Edit, Write run sequentially. Read, Grep, Glob, read-only MCP tools run concurrently. This is correctness, not performance — concurrent mutations clobber each other. [03 §"The agent loop"]

### 5. Two-layer system prompt: cached prefix + volatile suffix

A long static prefix (role, tone, tool inventory, safety rules, built-in tool descriptions) gets prompt-cached at a 1-hour TTL. A short volatile suffix (env block, CLAUDE.md, active task list, system reminders) re-injects per turn. CLAUDE.md belongs in the *user message* after the system prompt, not in the system role — Anthropic's docs are explicit. [03 §"System prompt"; 07 §3]

### 6. ~7 well-described core tools plus skills/MCP for the long tail

Read, Edit, Write, Bash, Grep, Glob, Task. Maybe WebFetch. That's the universal core across every recreation. The long tail — domain-specific operations, organizational tools, vendor integrations — should be MCP servers or skills, not bake into the core. The skill is the progressive-disclosure unit: 30–50-token metadata header always in context, full body loaded on activation. [04 §"Patterns that survived"; 06 §6; 07 §10]

### 7. Plan mode is a hook-enforced toolset, not a prompt instruction

When the user enters plan mode, the harness must *deny* calls to Edit, Write, and non-read-only Bash via a PreToolUse hook. "We told the model not to" doesn't work. ExitPlanMode is a UI gate the user must approve. [06 §3]

### 8. Subagents get a fresh context window and one-shot delegation

A subagent invocation looks like a single tool call to the parent. Internally it spins up a separate Claude instance with its own system prompt, its own tools, its own context, runs to completion, and returns a string. No back-and-forth. Cheaper than fan-out multi-agent, dramatically saves parent context. Best for "search → summarize," "read N files → report findings," "run tests → which failed." [03 §"Subagents"; 06 §5]

### 9. Hooks at every lifecycle event

SessionStart, UserPromptSubmit, PreToolUse, PostToolUse, Notification, Stop, SubagentStart, SubagentStop, PreCompact, InstructionsLoaded. JSON payload on stdin, exit-code semantics (0 allow, 2 block). Hooks run as the user's privileges — security implications are real, mitigate with project-trust dialog and `allowManagedHooksOnly` for enterprise. [03 §"Hooks"; 06 §8]

### 10. Permissions: deny → ask → allow, evaluated across merged scopes

Managed > CLI args > local project > shared project > user. Allowlists merge across layers; denies from any layer beat allows from any other. Compound-command awareness for Bash (`git status && rm -rf` is two rules, not one). Process-wrapper canonicalization (`timeout`, `time`, `nice`, etc. stripped before matching; `docker exec`, `npx`, `devbox run` *not* stripped — those would be backdoors). [03 §"Permissions"; 06 §9]

Command/process substitution and subshell grouping (`$(…)`, backticks, `<(…)`, `(…)`) fail safe — the inner command cannot be soundly extracted by the operator splitter, so the segment floors at Ask. mecatl refines this so a fully read-only substitution does not needlessly prompt: a SEPARATE classifier (`SubstitutionReadOnly`) recursively extracts every inner command and, when every inner is read-only AND the blanked outer is read-only, lets the ordinary rule fold decide instead of flooring (the `ReadOnlyBash`/plan-mode classifier and its fuzz invariants stay byte-for-byte unchanged). A SUBAGENT in an isolated worktree/fork additionally auto-approves a MINIMAL worktree-safe set (`IsolationApprovable` = read-only ∪ `go {test,build,vet,list}`, minus worktree-escape verbs `git push/config/remote/fetch/pull/clone/worktree/submodule`, minus a path-bearing `git -C`/`--git-dir`/`--work-tree` global flag, minus a `go` verb carrying `-exec`/`-toolexec`/`-overlay` arbitrary-external-program flags — the worktree isolates the FS, not the process); anything not auto-resolved is SURFACED to the human when one is attached (the child's ask is routed up to the parent's approval UI by its child-namespaced ask id), else auto-denied with an accurate message + operator diagnostic. `--yolo` loosens the substitution floor for the main agent too; a configured Deny/Ask in any scope still wins.

### 11. Sandbox is a separate, OS-level layer

Permission rules are a model-trust boundary. The sandbox (Seatbelt on macOS, Landlock + seccomp + bubblewrap on Linux) is a process-trust boundary. You need both. Codex CLI is the gold standard here; nobody else in OSS has caught up. [03 §"Permissions"; 05 §"Codex CLI"; 06 §9]

### 12. Four-tier compaction, cheapest-first

Snip (drop) → Microcompact (strip tool-output noise) → Context Collapse (replace large reads with pointers) → Auto-compact (LLM summary). Trigger at 70–80% of the window, not at the wall. Reserve a 13K-token buffer. The PreCompact hook archives the pre-compaction transcript. CLAUDE.md is re-read from disk after compaction; nested CLAUDE.md is not. [03 §"Context management"; 07 §4]

### 13. MCP is the extensibility surface; tool-schema deferral is mandatory

Past a handful of MCP servers, eager schema loading wrecks context. The solution: list names and one-line descriptions in the prompt, expose the rest behind a `ToolSearch` retrieval tool the model calls when it actually needs a schema. Better still for very large surfaces: the code-execution-with-MCP pattern (tools as TypeScript modules in a sandboxed filesystem). [03 §"MCP"; 07 §§7–8]

## MVP → v3 roadmap

A staged build that lets you ship something useful early and stays in budget at each milestone.

### v0 — the smallest thing that works (1–2 weeks)

- The streaming agent loop. One model provider. One tool: Bash.
- The agent gets a system prompt with env-block injection (cwd, OS, model, date).
- CLAUDE.md discovered at cwd, injected as user message.
- No permission gating beyond "always ask."
- TUI: rolling chat with markdown rendering and a `q` to quit.
- Headless mode: `agent -p "<prompt>"` reads stdin, writes stdout, exits.

This is enough to ship a real coding agent. `learn-claude-code/s01` is exactly this much code. It works because the model is doing 80% of the lifting.

### v1 — the production core (2–4 weeks)

- The 7-tool kit: Read (with line numbers), Edit (with three invariants), Write, Bash, Grep, Glob, Task. WebFetch optional.
- Read-only parallelism, mutating serialization.
- TodoWrite or equivalent planning tool. The model will use it heavily without prompting if the tool is well-described.
- Per-tool permission allowlists, prompt-on-unknown. settings.json with merge semantics.
- A `PreToolUse` hook surface, even if your only hook initially is a logger.
- Prompt caching: stable system prompt + tool defs at one breakpoint, CLAUDE.md at another.
- Compaction at 80% of window using a single LLM-summary stage. Don't bother with the four-tier cascade yet.
- Per-tool-call log to disk for replay debugging.

### v2 — the differentiators (4–8 weeks)

- Plan mode, enforced via PreToolUse denial of Edit/Write/non-read-only-Bash.
- Subagent / Task tool with isolated context, scoped tools, model choice. Two built-in subagent profiles: an explorer (read-only, cheap model) and a general-purpose (full tools, default model).
- The full hook lifecycle (SessionStart, UserPromptSubmit, PostToolUse, Stop, SubagentStop). Document JSON I/O contract.
- MCP client: stdio transport at minimum. Tool namespacing `mcp__<server>__<tool>`. `.mcp.json` discovery.
- Auto-memory or a memory tool (`recall(key)`, `remember(key, value)`). Smaller and more conservative than you think — saving facts the file system already knows is the dominant failure mode.
- Slash commands: scan `.claude/commands/` (or `.agent/commands/`), template substitution.
- Headless mode with `--output-format json` for CI use, taking cues from Gemini CLI.

### v3 — the optional advanced moves (8+ weeks, prioritize by need)

Pick what your users actually need. None of these is required.

- **OS-level sandbox.** Seatbelt on macOS, Landlock + seccomp on Linux. Heavy engineering; massive payoff for security. Codex CLI is the reference.
- **Tool-schema deferral via ToolSearch.** Only needed once you're hosting many MCP servers.
- **Four-tier compaction cascade.** Single-shot summary works fine for most sessions; the cascade matters for hours-long agents.
- **Worktree-based parallel subagents.** For "try three approaches in parallel" UX.
- **Fast-apply edit pipeline.** Owning or fine-tuning a small model is real work. Don't unless edit latency is your bottleneck.
- **Repo map (Aider-style tree-sitter PageRank).** Add when agentic grep visibly wastes turns. Strong, under-borrowed idea.
- **Persistent Jupyter kernel (CodeAct-style).** For data-heavy / notebook workflows specifically.
- **Client/server split via JSON-RPC.** Worth it when you're shipping more than one client surface (TUI + IDE plugin + CI). Otherwise YAGNI.
- **Skills as a packaging unit** with YAML frontmatter, full SKILL.md spec, and a registry. Skill is more sophisticated than slash commands; you mostly need it once a community wants to share.

## Cost levers

The harness has four ways to bring cost down by an order of magnitude. Pull all of them.

**Prompt caching.** A 50-turn session with a 20K system prompt costs ~7.25M tokens uncached, ~770K cached. Same answer for ~10× less money. The engineering work is small: stable prefix, careful breakpoint placement, no timestamps in the system prompt. Track cache-hit rate as a real metric (should be > 0.7 for a tuned coding agent). [07 §3]

**Tool inventory pruning.** Every tool definition in context is overhead per turn. Default to ~7 built-ins. Push MCP servers behind tool-schema deferral. Past 30 active tools, the code-execution-with-MCP pattern saves 98% of the schema tokens. [07 §8]

**Subagent isolation.** A subagent that does 50K of grep results internally and returns 1K to the parent saves 49K from *every subsequent turn* in the parent's session. Over a long session this compounds far past the cost of the subagent call. Spawn aggressively for spike-and-summarize work. [06 §5; 07 §2]

**Tool-output truncation and shaping.** Cap tool returns at ~25K tokens. Truncate with markers, paginate, filter at the source. Every byte you save in tool output is a byte you don't carry forward forever (or until compaction). [07 §10]

Smaller but worth doing:

- Route cheap turns to a cheap model (Haiku-class for subagent fan-out; Sonnet-class for orchestration; Opus only when reasoning depth matters). Most credible recreations expose four model pointers: `main`, `task`, `compact`, `quick`. Kode is the cleanest version.
- Prefer terseness in the system prompt's tone guidance. Output is 5× input on Anthropic; a model that writes a 10K explanation costs the same as one that reads 50K.
- Don't reformat the system prompt mid-session — even cosmetic edits invalidate the cache.

## Security levers

Two principles. *Determinism over prompts* for anything that matters. *Defense in depth*: permissions and sandboxes are separate boundaries, not redundant.

**The four layers, outer to inner:**

1. **OS-level sandbox.** Seatbelt / Landlock / seccomp / bubblewrap restricts what the process can do regardless of what the model decides. The first line because it can't be bypassed by clever prompting.
2. **Permission gates.** Per-tool allowlists with deny dominating. Compound-Bash aware. Process-wrapper canonicalization (with a *closed* list — never add `docker exec` or `devbox run`).
3. **PreToolUse hooks.** Domain-specific blocking: secret scanners before any write, network policies before any fetch, audit log of every shell call. This is also where plan mode is enforced.
4. **System prompt rules.** Last line of defense. Treat as a polite request, not a guarantee.

**The MCP supply chain is your problem now.** STDIO MCP transport runs configured commands as trusted with no sanitization. Treat every MCP server like a `curl | bash`: pin versions, source from registries you trust, sandbox the agent. Never let user-controlled input flow into MCP server configuration. [07 §8]

**Hook supply chain.** A project's `.claude/settings.json` can run arbitrary code at SessionStart. First-time trust dialog mitigates; `allowManagedHooksOnly` in enterprise managed settings locks to organization-deployed hooks. Bake hooks into the repo (committed), not just into `~/.claude/` (per-user).

**The prompt-injection survivability question.** Compaction summaries are an attack surface — payloads can be crafted to survive a compaction pass and persist across the cascade. Treat summaries as untrusted; consider running a sanitizer pass on `PreCompact`. CVE-2025-54794/54795 (InversePrompt) is the canonical case to study.

## Ideas worth stealing, by project

A condensed cross-reference of the most copy-able ideas in each major harness.

| Source | Idea worth stealing |
|---|---|
| **Claude Code** | The whole shape, especially: streaming async-generator loop, Edit invariants, four-tier compaction, hook lifecycle, plan mode as enforced toolset, subagent isolation, ToolSearch for MCP. |
| **sst/opencode** | Persist sessions to a database (not just a file). Client/server architecture for multi-surface support. Reserved-output + safety-buffer compaction math. |
| **charmbracelet/crush** | LSP-as-tool (the agent literally calls gopls). External community-curated model registry (Catwalk). |
| **musistudio/claude-code-router** | The router pattern: don't always own the UI; sometimes route an existing client. Per-task routing (background, think, longContext, webSearch). |
| **shareAI-lab/Kode** | Four-model pointers (main/task/compact/quick). `AskExpertModel` as a tool. YAML model-config export/import. |
| **shareAI-lab/learn-claude-code** | The 12-mechanism breakdown as a curriculum. The mantra "model is 80%, code is 20%." |
| **openai/codex** | OS-level sandboxing as a first-class concern, with `codex debug seatbelt` to test. App Server JSON-RPC for runtime/interface split. Two-axis safety: sandbox = capability, approval = gate. |
| **google-gemini/gemini-cli** | `--output-format stream-json` for headless / CI. Trusted-folders policy (different security postures per directory). 12 lifecycle hook points. |
| **Aider** | Tree-sitter + personalized PageRank repo map. Edit-format-per-model selection driven by a benchmark suite. Atomic git commit per turn. |
| **OpenHands** | Event-sourced architecture: typed Actions, typed Observations, immutable append-only EventLog as the *only* source of truth. Persistent Jupyter kernel across tool calls. Sandbox-as-choice (swap LocalWorkspace for DockerWorkspace without changing agent code). |
| **Cline** | Plan/Act as enforced tool gating, not prompt etiquette. Per-mode model: cheap for execution, expensive for planning. `replace_in_file` + `write_to_file` doctrine. |
| **Roo Code** | Mode = (system prompt + tool allowlist + file allowlist + model choice). The cleanest open-source "agent persona" primitive. |
| **Continue** | Config-as-content: shareable YAML blocks with semver-ish slugs. `create_rule_block` (let the agent codify rules). CI-enforceable AI rules. |
| **SWE-agent** | Reject the human interface: paginated viewer beats `cat`, line-range `edit` beats `sed`, summarized search beats raw `grep`. Linter-gated edits — discard invalid edits, return the error to the model. |
| **Cursor Composer** | Planner/applier split with a fast specialized model. Parallel agents via git worktrees. Train (or fine-tune) for the harness, not just prompt. |
| **Windsurf Cascade** | Continuously-running planner distinct from executor. User signals (clipboard, terminal, scroll) as first-class context. Persisted "Memories" beyond a session. |
| **Devin** | Skeptic-by-prompt design ("every claim must be backed by file-level evidence with line numbers"). Replayable event timeline as a product surface. Workspace as durable artifact, not ephemeral per-turn state. |
| **Goose** | Recipe = (instructions + extensions + parameters + provider + retry + response schema) as the composable unit. Sub-recipes as subagents in isolated context. |
| **GitHub Copilot Coding Agent** | Async-by-PR: long tasks belong in a queue with PRs as output. Platform's existing CI runner as the sandbox. Spaces as named persistent context bundles. |
| **Zed Agent (ACP)** | The editor is not the agent. Out-of-process, stable protocol; surfaces are clients. JetBrains co-adopting ACP shows the pattern generalizes. |
| **Augment** | Expose your context strategy as an MCP server if it's your differentiator. |

## Anti-patterns to avoid

A consolidated list (with details in 06 §13 and scattered across the rest).

- **Tool soup.** 30+ overlapping tools dilute the model's attention. Curate ruthlessly; push the long tail to MCP/skills.
- **Eager schema loading.** Loading 200 MCP tool schemas eats ~150K tokens. Use ToolSearch or code-execution-with-MCP.
- **Over-eager memory.** Saving facts the file system already knows is the dominant memory failure mode. Save user preferences and cross-cutting facts; let the code be the memory of the code.
- **Premature multi-agent.** Anthropic's published number: multi-agent costs ~15× chat. Use a single agent with plan mode for code, or sequence work with a workflow. Subagent (one-shot) is fine; full multi-agent is expensive and bug-prone.
- **No stop conditions.** Always cap `max_turns`, `max_tool_calls`, and `max_consecutive_failures`. Background-mode agents without caps have produced five-figure runaway sessions.
- **Gating reads.** Approval prompts on Read calls train users to click "approve" reflexively, defeating the gates that matter.
- **Schema-only tool descriptions.** A schema with `name: edit_file, params: {path: string, content: string}` and no description tells the model nothing. Treat tool descriptions like onboarding docs.
- **Trusting the model to enforce constraints.** Anything safety- or correctness-critical goes in hooks or the sandbox.
- **Compaction without signal preservation.** Naive `summarize-old-messages` loses tool IDs, file paths, error context. Preserve plan, decisions, unresolved questions, file paths touched; drop file contents, verbose grep output, old stack traces.
- **Reformatting the system prompt mid-session.** Invalidates cache. Cosmetic changes are not free.
- **CLAUDE.md in the system role.** Anthropic's docs are explicit it belongs in a user message after the system prompt; the model defers differently to each.
- **Plan-mode by prompt only.** The model will write a file from plan mode one time in ten. Enforce with a PreToolUse hook.
- **Hooks for model reasoning.** Hooks are deterministic interception. "Decide whether this edit is safe" is a model problem, not a hook problem.
- **Process-wrapper expansion list growth.** Adding `docker exec`, `devbox run`, or `npx` to the wrapper-canonicalization list silently breaks permission boundaries. The list should be closed and audited.

## Bets the field hasn't settled

Places where you'll have to make a judgment because the field hasn't converged.

**Edit format.** SEARCH/REPLACE (Aider, Cline) is robust when matches are exact, brittle when they aren't. Unified diff is terser but requires the model to count lines. Structured tool calls with line ranges + linter gate (SWE-agent, OpenHands) have the strongest reliability story. Fast-apply (Cursor) is best in class but requires owning a small model. **Pragmatic default:** SEARCH/REPLACE for v0/v1; add linter-gating as soon as you have a linter integration; revisit fast-apply only if edit latency dominates.

**Context strategy.** Agentic discovery (let the model grep) is the 2025–2026 default and the empirical winner at typical repo sizes. Aider's tree-sitter PageRank repo map is the strongest non-embedding alternative and is *under-borrowed*. Augment/Cursor/Windsurf bet on semantic embedding indices but pay infrastructure cost. **Pragmatic default:** start agentic; add a cached file-tree summary tool early (cheap, surprisingly powerful); evaluate adding a repo map if the agent visibly wastes turns hunting.

**Sandbox tier.** None (most IDE extensions, dangerous) → per-process OS sandboxing (Codex's gold standard) → full container/VM (OpenHands, Devin, Copilot Coding Agent). The middle tier is under-explored outside Codex; there's no good reason for that. **Pragmatic default:** ship with permission gates and a hook surface; add OS-level sandboxing as soon as you have any users running untrusted code (which is sooner than you'd think — README files in someone's cloned repo are untrusted code from a prompt-injection standpoint).

**Surface.** CLI / IDE extension / cloud web UI / GitHub-native / SDK-only. The CLI + SDK combination is most flexible (Aider, Codex, Gemini CLI, Goose). IDE-embedded gives the richest UX but couples you to one editor. Cloud is hardest to build and easiest to commercialize. **Pragmatic default:** CLI + SDK first. ACP-style protocol for the agent runtime lets you add IDE clients later without rewriting.

**Multi-model strategy.** Anthropic-only (Claude Code) trades flexibility for tight model coupling (and tighter agent training). BYO-multi-vendor (OpenCode, Kode, Crush) is the open-ecosystem default. **Pragmatic default:** ship multi-vendor from day one, expose four model pointers (main/task/compact/quick), default each to a sensible model. Most users want flexibility; the agent's quality is more dependent on the harness than the specific frontier model anyway.

**Headless / async UX.** PR-as-interface (GitHub Copilot Coding Agent) is the right shape for long tasks; chat is the right shape for short ones. Most harnesses pick one; few do both well. **Pragmatic default:** ship `agent -p "<prompt>"` headless mode as a feature, even if interactive is your primary UX. Async-PR is a separate product effort; treat it as v3+.

**Memory.** File-based memory is what Anthropic recommends; vector-store memory has the most failure modes (precision, staleness, infra overhead). **Pragmatic default:** start with no cross-session memory beyond CLAUDE.md and what's on disk. Add a memory tool only when you can identify a class of facts that (a) the model cannot rediscover in <5 tool calls and (b) is needed across sessions. Most "memory" requests in practice are wishes for "context that persists" — which is what a file is.

## A closing test

If you've designed your harness and want to sanity-check it before writing more code, run this gauntlet:

1. Can your agent loop pause, resume, and cancel? (Streaming generator yes; buffered list no.)
2. Does Edit error if the file hasn't been read this session? (If no, you will corrupt files.)
3. Can the user enter plan mode and have Edit / Write / Bash *deny* at the harness level? (If no, plan mode is decorative.)
4. Does compaction preserve file paths, decisions, and unresolved questions while dropping file contents and old grep output? (If no, your agent loses the plot.)
5. Do `PreToolUse` hooks actually fire and block when they return exit code 2? (If no, you have no security primitive.)
6. Can a session run for an hour without ballooning input cost? (Cache hit rate must be > 0.7. If it's not, your prompt isn't cache-stable.)
7. When a subagent finishes, does only its final string return to the parent — not its intermediate tool calls? (If no, you've reinvented multi-agent and inherited the bugs.)
8. Does your permission system deny across merged scopes? (If a user-level deny doesn't override a project-level allow, you've inverted the trust model.)
9. Is your sandbox a *separate* layer from your permission system? (If "the permission allowed it" is your only guarantee, an exploit in the permission classifier is RCE.)
10. Can a non-trivial bug in a tool description be diagnosed by reading the description? (If not, the model can't either.)

If your design fails any of these, you've found the v1 work that matters most.

---

The shorter version of this entire file: **build the shape, instrument heavily, and resist the urge to add features before the loop, the tools, the permissions, the hooks, and the cache are all working as designed.** The model is doing 80% of the work. Your job is to give it a clean place to do it.
