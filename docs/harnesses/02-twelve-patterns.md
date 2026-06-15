---
title: "The 12 Agentic Harness Patterns From Claude Code"
doc_id: 02-twelve-patterns
layer: reference
captured: 2026-05-18
status: stable
keywords: [persistent instruction file, scoped context assembly, tiered memory, dream consolidation, autoDream, progressive compaction, microcompact, explore-plan-act, plan mode, context-isolated subagents, fork-join parallelism, worktree, progressive tool expansion, skills, command risk classification, permissions, single-purpose tools, lifecycle hooks, separation of read and write]
answers:
  - "What are the 12 reusable harness design patterns from Claude Code?"
  - "Which patterns are load-bearing vs. optional for a working coding agent?"
  - "What are the cross-cutting design instincts (context economy, read/write split, determinism, locality)?"
related: [03-claude-code-architecture, 06-architecture-patterns, 08-design-considerations]
---

# 12 Agentic Harness Patterns From Claude Code

> Source: [generativeprogrammer.com — 12 Agentic Harness Patterns From Claude Code](https://generativeprogrammer.com/p/12-agentic-harness-patterns-from)
> Companion piece: [Practical Lessons From the Claude Code Leak](https://generativeprogrammer.com/p/practical-lessons-from-the-claude)
> Captured: 2026-05-18

## TL;DR

The Generative Programmer article catalogues 12 reusable design patterns reverse-engineered from the 512K-line Claude Code source map that Anthropic accidentally shipped inside the `@anthropic-ai/claude-code` npm package in March 2026. The patterns cluster into four buckets: **memory and context** (persistent instructions, scoped assembly, tiered memory, dream consolidation, progressive compaction), **workflow and orchestration** (explore-plan-act, context-isolated subagents, fork-join parallelism), **tools and permissions** (progressive tool expansion, command risk classification, single-purpose tool design), and **automation** (deterministic lifecycle hooks). The throughline is separation: read from write, research context from edit context, sequential from parallel, deterministic from probabilistic. Every pattern exists to defend the model's finite context window and to make tool execution recoverable, reviewable, and bounded. Together they constitute a working spec for what a production coding harness must do — far more than "an LLM in a loop." A harness builder who implements even half of these patterns will outperform a naive ReAct loop on real-world tasks. The remaining half mostly matters once your harness handles multi-hour sessions or untrusted code.

## Context: why these patterns matter

A "harness" is everything wrapped around a model so it can actually finish a task: prompts, tools, context policies, memory, sandboxes, subagents, lifecycle hooks, permission gates, and recovery paths. Addy Osmani's framing is now standard — [a decent model with a great harness beats a great model with a bad harness](https://addyosmani.com/blog/agent-harness-engineering/). Claude Code is influential because it is (a) the highest-revenue coding agent on the market in 2026, (b) the harness whose source map briefly leaked in full, and (c) the reference implementation that Anthropic's own engineers ship against. The article's thesis is that the leak exposed not isolated tricks but a coherent set of design patterns — the cornerstones of agentic-harness design that will outlast any particular product.

The leak's specifics matter only as artifacts: 25+ named hook points, four-stage compaction cascade, ~20 default tools expandable to 60+, and the existence of an "autoDream" background consolidation mode. These names are evidence the patterns are real, but the patterns themselves transfer to any harness — Cursor, Aider, OpenAI Codex CLI, Cline, etc.

## The 12 patterns

### 1. Persistent Instruction File

**Problem:** Users repeat the same project setup ("use pnpm, not npm; tests live in `__tests__`; we use Vitest, not Jest") at the start of every session. Without persistence the model relitigates conventions on every turn and burns context.

**Mechanism:** A file at a known path (`CLAUDE.md`) is read into the system prompt at session start and treated as ground truth for the run. It encodes build commands, test procedures, architecture rules, naming conventions, and "do not touch" zones. It is loaded *before the first user prompt*, so it shapes every subsequent decision.

**Claude Code implementation:** `CLAUDE.md` lives at the repo root (or a parent directory). Anthropic's docs describe it as "how Claude remembers your project" ([code.claude.com/docs/en/memory](https://code.claude.com/docs/en/memory)). As of v2.1.59+ Claude Code also auto-writes a curated `auto memory` block capped at the first 200 lines and supports `@path` imports so a single file can compose smaller fragments.

**Seen elsewhere:** Cursor's `.cursorrules`, Aider's `CONVENTIONS.md`, OpenAI Codex CLI's `AGENTS.md` (which Claude Code now also reads for interoperability), Continue's `.continue/config.yaml` rules.

**Tradeoffs:** Becomes a junk drawer. Stale instructions silently corrupt later sessions. The file is invisible at runtime — users forget what's in it and blame the model for "weird" decisions. Mitigations: keep under ~200 lines, run periodic audits, prefer behavioral rules over factual claims that drift.

### 2. Scoped Context Assembly

**Problem:** A monorepo has one root style guide but each package needs its own conventions. A user has personal preferences (commit message style) that should apply across all projects. A single flat file does not express this.

**Mechanism:** The harness walks from CWD up to the repo root collecting every `CLAUDE.md` it finds, then concatenates them. There are typically four scopes:
- managed/org (`/etc/claude/CLAUDE.md` or similar)
- user (`~/.claude/CLAUDE.md`)
- project root (`<repo>/CLAUDE.md`)
- subdirectory (`<repo>/services/api/CLAUDE.md`)

Files higher in the tree load eagerly at launch; files in subdirectories load lazily when the agent first reads a file in that directory. Later-loaded instructions get soft priority on conflicts. An `@import` syntax allows inclusion without duplication.

**Claude Code implementation:** Documented at [code.claude.com/docs/en/memory](https://code.claude.com/docs/en/memory). The walk happens at session start, and subdirectory `CLAUDE.md` files trigger lazy load on first tool access in that subtree. `.claude/rules/` provides path-scoped rules (a per-glob equivalent).

**Seen elsewhere:** Git's hierarchical config (`/etc/gitconfig` → `~/.gitconfig` → `.git/config`) and ESLint's cascading `.eslintrc` files are direct intellectual ancestors. The pattern is older than agents; the novelty is applying it to natural-language instructions.

**Tradeoffs:** Implicit ordering means users cannot easily reason about which rule wins. Hard to debug ("why did the agent ignore my rule?"). Counter-pattern: a `/why-rule` introspection command that shows the resolved instruction set.

### 3. Tiered Memory

**Problem:** Project knowledge grows unboundedly across sessions ("we tried approach X last month and it failed"). Pasting all of it into context is wasteful; losing it is worse.

**Mechanism:** Memory is split into tiers with different load strategies:
- **Tier 0** — a compact index (`MEMORY.md`, capped at ~200 lines), always in context.
- **Tier 1** — topic-specific files (`memory/refactor-2025-q3.md`), loaded on demand when the index points to them.
- **Tier 2** — raw session transcripts on disk, retrieved only by explicit search.

The index is the routing table; the model decides when to descend.

**Claude Code implementation:** The leaked code reveals an "auto memory" feature (first 200 lines reserved, selective rather than exhaustive) and on-demand loading of topic files. See [Inside Claude Code's Leak: 8 Compaction Modes, 3 Memory Tiers, 44 Flags](https://medium.com/data-science-collective/inside-claude-codes-leak-8-compaction-modes-3-memory-tiers-44-flags-anthropic-never-talked-c9740c501e63).

**Seen elsewhere:** Letta/MemGPT's hierarchical-memory paper is the academic template. Cline's "memory bank" pattern is a manual variant. RAG over conversation history is the database-flavored cousin.

**Tradeoffs:** The index becomes a coordination bottleneck — if it lies about what's in deeper tiers the agent misses content. Requires consolidation (see pattern 4) or it goes stale.

### 4. Dream Consolidation

**Problem:** Memory naturally accumulates duplicates ("we use Vitest" appears in five files), contradictions ("ports are 8080" / "ports are 3000 now"), and rot. The agent cannot productively reason over this on the hot path.

**Mechanism:** A background process triggered during idle windows reads the memory store, deduplicates entries, prunes contradictions (preferring more recent), and rewrites the index. It is garbage collection for natural-language state.

**Claude Code implementation:** The leak surfaced a feature flag named `autoDream` that merges duplicates, prunes contradictions, and "keeps the index tight." Mentioned in the Generative Programmer article and corroborated by [WaveSpeedAI's leak analysis](https://wavespeed.ai/blog/posts/claude-code-architecture-leaked-source-deep-dive/). The name is the giveaway: a sleep-cycle analogy for offline memory work.

**Seen elsewhere:** MemGPT's "main context refresh" and Generative Agents' (Park et al. 2023) reflection step are the lineage. Rare in current OSS harnesses because most still have small memory footprints.

**Tradeoffs:** Risky — automated consolidation can drop information you wanted. Should keep an undo log. Best run with a smaller, cheaper model.

**Pseudocode:**
```pseudo
on_idle(threshold=5min):
  entries = load_all_memory()
  clusters = cluster_by_topic(entries)
  for cluster in clusters:
    merged = llm_summarize(cluster, instruction="dedup, prefer recent on conflict")
    write_entry(merged); archive_originals(cluster)
  rewrite_index()
```

### 5. Progressive Context Compaction

**Problem:** Context windows fill up. Naive "summarize everything when full" loses recent fidelity (where the model needs it most) and is expensive.

**Mechanism:** A cascade of compression stages with different aggressiveness applied to spans of different ages or types. Newer turns stay raw; older turns get light summarization; very old turns get aggressive collapse. The leaked Claude Code pipeline has four named stages:
- `HISTORY_SNIP` — drop entries below a token threshold (cheapest).
- `Microcompact` — strip noise from tool results without summarizing, run before any expensive call. Only tools in `COMPACTABLE_TOOLS` are eligible; MCP/Agent/custom tool results pass through unchanged.
- `CONTEXT_COLLAPSE` — structural summarization of a contiguous region.
- `Autocompact` — fires when conversation approaches the window ceiling; reserves a 13K-token buffer and emits up to a 20K-token structured summary of the session.

**Claude Code implementation:** Detailed in [Learn From Claude Code: Context Compaction](https://zane-portfolio.kiyo-n-zane.com/blog/by/developer/learn_from_claude_code_context_compaction/) and [Straiker's leak analysis](https://www.straiker.ai/blog/claude-code-source-leak-with-great-agency-comes-great-responsibility). The cascade is `tool result budgeting → microcompact → context collapse → autocompact`. Each stage has its own trigger and budget.

**Seen elsewhere:** Cursor's "context summarization" and Cline's compaction are simpler one-shot versions. LangGraph offers explicit `summarize_messages` nodes.

**Tradeoffs:** Compaction is lossy and the loss compounds. [Issue #13112](https://github.com/anthropics/claude-code/issues/13112) and [#18264](https://github.com/anthropics/claude-code/issues/18264) document user pain — silent context loss and a flag that doesn't honor itself. Security note from Straiker: payloads can be crafted to survive compaction, persisting prompt injection across arbitrarily long sessions.

### 6. Explore-Plan-Act Loop

**Problem:** A naive ReAct loop edits files before it understands the code. The result is a confidently wrong patch that the user has to revert.

**Mechanism:** Workflow is split into three (sometimes four: + Commit) phases with monotonically increasing tool permissions.
- **Explore** — read-only. The agent can `Read`, `Grep`, `Glob`, and `Bash` for non-destructive shell, but no `Edit`/`Write`. Goal: build a mental map.
- **Plan** — still read-only, but the agent must emit a written plan the user approves before any write.
- **Act** — full tool access; the plan becomes a checklist.

The user-facing UX is "Plan Mode": Claude reads and reasons but cannot mutate. On approval it transitions to act phase.

**Claude Code implementation:** Plan Mode is a first-class CLI feature ([codewithmukesh.com/blog/plan-mode-claude-code](https://codewithmukesh.com/blog/plan-mode-claude-code/), [datacamp.com/tutorial/claude-code-plan-mode](https://www.datacamp.com/tutorial/claude-code-plan-mode)). Anthropic recommends Explore → Plan → Implement → Commit as the canonical workflow.

**Seen elsewhere:** Aider's `--read` flag, Cursor's "Ask" vs "Agent" toggle, LangGraph's plan-and-execute templates, the broader "ReAct vs Plan-and-Execute" debate.

**Tradeoffs:** Slower per-task. Users in flow mode hate it. Worth it when the change touches >3 files or has irreversible side effects.

### 7. Context-Isolated Subagents

**Problem:** One agent doing research, planning, and editing simultaneously pollutes its own context with search results that have nothing to do with the final edit. The model's signal-to-noise ratio collapses.

**Mechanism:** Spawn a subagent — a separate Claude instance with its own system prompt, its own context window, its own tool allowlist, and its own permission mode. The parent invokes it with a task description, the subagent grinds through tool calls, and only the final summary returns to the parent. All intermediate noise stays inside the subagent.

**Claude Code implementation:** Defined as markdown files with YAML frontmatter ([code.claude.com/docs/en/sub-agents](https://code.claude.com/docs/en/sub-agents)). The frontmatter declares `tools`, `model`, and `description`. Two flavors: fresh-start (empty context) and forked (inherits parent transcript). Subagents cannot nest. Hooks fire on `SubagentStart` and `SubagentStop`.

**Seen elsewhere:** OpenAI's Assistants v2 with `assistant_id` per role, AutoGen's `GroupChat`, CrewAI's `Crew`, LangGraph's `subgraph` nodes. The pattern is older than LLMs — it's Erlang processes for prompts.

**Tradeoffs:** Subagent invocations cost a fresh prompt prefix (cache misses on the parent's prefix). Choosing what to delegate vs handle inline is a real skill — see Generative Programmer's [Distribution vs Escalation](https://generativeprogrammer.com/p/subagents-vs-advisor-distribution). Over-decomposition fragments reasoning across calls that can't see each other.

### 8. Fork-Join Parallelism

**Problem:** Some tasks decompose into independent units — implement three competing migration strategies, run the same refactor against three branches, generate three test files. Doing them sequentially wastes wall-clock time *and* token cache.

**Mechanism:** Fork N subagents, each working in an isolated `git worktree` of the repository. The parent's cached prompt prefix is reused by each fork (so the marginal cost of forking is near zero for the prefix). Each worktree is a full checkout but shares the same `.git` database — independent indexes, independent working directories, zero collision. When all branches finish, the parent merges or selects.

**Claude Code implementation:** Built-in `--worktree` flag (added in v2.1.50) creates a named worktree and branch. Subagents can set `isolation: worktree` in their YAML frontmatter to be auto-isolated. The pattern explicitly exploits LLM nondeterminism as a feature — running N parallel agents gives N candidate solutions. See [Claude Code Git Worktree Guide](https://thepromptshelf.dev/blog/claude-code-git-worktree-guide/) and Boris Cherny's [announcement](https://www.threads.com/@boris_cherny/post/DVAAnexgRUj).

**Seen elsewhere:** GitHub Copilot Workspace's parallel sessions, SpillwaveSolutions' [parallel-worktrees](https://github.com/spillwavesolutions/parallel-worktrees) skill, Aider's `--multiple` mode.

**Tradeoffs:** Merge conflicts and selection criteria (which fork's output wins?). N× the model spend if you don't dedupe. Best for "tournament" tasks where one of N is good enough.

**Pseudocode:**
```pseudo
prefix = build_shared_prompt_prefix(task)
worktrees = [git_worktree_add(f"wt-{i}") for i in range(N)]
results = parallel_map(worktrees, lambda wt:
  run_subagent(prefix, cwd=wt, prompt=variant_for(wt)))
best = score_and_pick(results)
merge_into_main(best.worktree)
```

### 9. Progressive Tool Expansion

**Problem:** Big tool catalogs are toxic. Every tool definition in the system prompt costs tokens and dilutes the model's attention; with 100 tools the model is materially worse at picking one.

**Mechanism:** Start with a deliberately small default tool set — fewer than 20 tools that cover the 90% case. Additional capabilities (MCP servers, custom skills, remote tools) are loaded on demand: surfaced by metadata at session start, hydrated into context only when the agent needs them. Skills are the progressive-disclosure unit — a 30-50-token metadata header is always present, and the full `SKILL.md` body loads when invoked.

**Claude Code implementation:** Default toolset per the leak: `AgentTool`, `BashTool`, `FileReadTool`, `FileEditTool`, `FileWriteTool`, `GrepTool`, `GlobTool`, `WebFetchTool`, `WebSearchTool`, `NotebookEditTool`, plus a handful more — expandable to 60+. Agent Skills documented at [platform.claude.com/docs/en/agents-and-tools/agent-skills/overview](https://platform.claude.com/docs/en/agents-and-tools/agent-skills/overview). Anthropic's [engineering post on skills](https://www.anthropic.com/engineering/equipping-agents-for-the-real-world-with-agent-skills) is the canonical reference.

**Seen elsewhere:** OpenAI's Function Calling "auto" with `tool_choice="required"` is the manual cousin. The [progressive disclosure pattern](https://www.mcpjam.com/blog/claude-agent-skills) is now widespread among MCP authors.

**Tradeoffs:** Discoverability suffers — the agent doesn't know what it doesn't know. Mitigated by a good metadata layer and a `list-skills` meta-tool.

### 10. Command Risk Classification

**Problem:** "Should this `rm -rf` run?" cannot be answered by the model alone — it needs deterministic gates *and* contextual judgment. Pure denylists are too rigid; pure model judgment is too risky.

**Mechanism:** Two layers stacked:
1. **Deterministic pre-parser** — parses the tool call (especially shell commands), matches against per-tool allow/ask/deny patterns from `settings.json`. Examples: allow `git status`, ask on `git push`, deny `rm -rf /`.
2. **Auto-mode classifier** — a separate model call (the leak shows Sonnet 4.6) that reads the transcript and the pending action, decides yes/no in two stages: a single-token fast filter, then chain-of-thought reasoning if the filter flags it. Checks scope escalation (is the agent going beyond what was asked?), untrusted infrastructure, and prompt injection signals.

Layer 1 runs first and resolves the deterministic cases. Layer 2 handles the rest.

**Claude Code implementation:** Documented at [code.claude.com/docs/en/permission-modes](https://code.claude.com/docs/en/permission-modes) and [Anthropic's auto-mode engineering post](https://www.anthropic.com/engineering/claude-code-auto-mode). [MindStudio's auto-mode write-up](https://www.mindstudio.ai/blog/what-is-claude-code-auto-mode-permission-classifier) and [Michael Bargury's analysis](https://www.mbgsec.com/archive/2026-03-29-claude-code-auto-mode-a-safer-way-to-skip-permissions/) cover the classifier internals. Leak data: users approve 93% of permission prompts — which is why auto-mode exists.

**Seen elsewhere:** Cursor's per-command allowlist, Aider's `--yes`/`--no` gates, Cline's command approval UI. The two-layer architecture is novel to Claude Code.

**Tradeoffs:** Classifier failures are scary in both directions. False positives erode trust; false negatives execute destructive commands. Regression history is real ([#57735](https://github.com/anthropics/claude-code/issues/57735)).

### 11. Single-Purpose Tool Design

**Problem:** Giving the model a generic `shell` tool means every operation is improvised, every input is unconstrained, and every permission is a blanket grant.

**Mechanism:** Replace general-purpose surfaces with narrow, typed, individually permissioned tools. `FileReadTool(path, offset, limit)` instead of `cat`. `FileEditTool(path, old, new)` instead of `sed`. `GrepTool(pattern, glob)` instead of `grep -r`. `GlobTool(pattern)` instead of `find`. Each tool has typed inputs, a constrained scope, and its own allow/ask/deny rules. The model spends less attention parsing flag syntax; the harness has fewer dangerous surfaces.

**Claude Code implementation:** `Read`, `Edit`, `Write`, `Grep`, `Glob`, `Bash`, `NotebookEdit` — each with strict schemas. `Bash` is intentionally last-resort for cases the typed tools don't cover. Documented in tool reference and reinforced by Anthropic's [Writing Effective Tools for AI Agents](https://www.anthropic.com/engineering/writing-tools-for-agents).

**Seen elsewhere:** Aider's separate `edit` and `apply` operations, Cursor's per-action tools, OpenAI Codex CLI's `apply_patch`. The principle generalizes: prefer N small tools to one big one — but Anthropic's guidance is "not so many that selection becomes hard" — see also pattern 9.

**Tradeoffs:** Combinatorial growth. Each new operation needs a new tool. Counter-pattern: use a small core plus skills (pattern 9) for the long tail.

### 12. Deterministic Lifecycle Hooks

**Problem:** Some behaviors must always happen — running `gofmt` after every Go file edit, blocking writes to `.env`, refreshing context when the working directory changes. Asking the model to remember is unreliable; the harness should enforce.

**Mechanism:** Named lifecycle events fire at specific points in the agent loop. Each event passes a JSON payload to user-configured shell commands. Exit code 0 means proceed; exit code 2 means block (with stderr fed back to the model as an error). The full hook catalog from the leak is 25+ events; the documented set includes:
- `SessionStart`, `SessionEnd`, `Resume`
- `UserPromptSubmit`
- `PreToolUse`, `PostToolUse` (the workhorses)
- `SubagentStart`, `SubagentStop`
- `CwdChanged`, `Notification`, `Stop`, `PreCompact`

`PreToolUse` is the only hook that can block.

**Claude Code implementation:** [Hooks reference](https://code.claude.com/docs/en/hooks); [Pixelmojo's CI/CD patterns post](https://www.pixelmojo.io/blogs/claude-code-hooks-production-quality-ci-cd-patterns); [SmartScope's guide](https://smartscope.blog/en/generative-ai/claude/claude-code-hooks-guide/). The hooks live in `settings.json` and accept any shell command. Mature use cases: secret scanning on `PreToolUse:Write`, format-on-save on `PostToolUse:Edit`, repo-state snapshot on `SessionStart`.

**Seen elsewhere:** Git hooks are the obvious ancestor. ESLint/pre-commit hooks. Cursor's "commands" are weaker — no programmatic blocking. Cline's auto-format is hardcoded.

**Tradeoffs:** Hooks run code with the user's privileges. A malicious hook config is a backdoor. The [ci-agent-hardening](https://docs.anthropic.com/) guidance applies. Also: hooks make sessions less reproducible across machines — bake them into the repo, not just `~/.claude`.

**Pseudocode:**
```pseudo
on_pre_tool_use(tool_name, tool_input):
  for hook in settings.hooks.PreToolUse:
    if hook.matcher.matches(tool_name):
      result = run_shell(hook.command, stdin=json(tool_input))
      if result.exit_code == 2: return BLOCK(stderr=result.stderr)
      if result.exit_code != 0: return WARN(result.stderr)
  return PROCEED
```

## Cross-cutting themes

Reading the 12 patterns together, four design instincts dominate:

**1. Context is the scarcest resource.** Patterns 1, 2, 3, 4, 5, 7, 9 are all context-economy moves. The harness fights entropy on the model's behalf — pre-loading what's needed, evicting what isn't, and isolating noisy work into separate windows. The article's implicit budget model is: every token in context should pay rent.

**2. Read and write are different operations.** Patterns 6 (explore-plan-act), 7 (subagents with read-only modes), 8 (worktrees), 10 (permissions), 11 (typed tools) all draw the same line. Reading is cheap and reversible; writing is expensive and irreversible. The harness should make that asymmetry visible in its phase structure, its agent boundaries, and its permission gates.

**3. Determinism is a feature, not a fallback.** Patterns 10, 11, 12 push critical behavior out of the prompt and into deterministic code. The hooks system is the cleanest expression: when something must happen, don't ask the model to remember — run a shell command.

**4. Locality of decision.** Patterns 2, 7, 11 all push decisions to the smallest scope that can make them — directory-local rules, subagent-local context, tool-local permissions. The harness avoids one giant global state that everything has to navigate.

A useful adjacent reading is Generative Programmer's [12 MCP Patterns Behind Production Agents](https://generativeprogrammer.com/p/12-mcp-patterns-behind-production), which extends the same separation-and-economy logic to the tool surface itself: thin surfaces over thick APIs, on-demand tool loading, programmatic tool calling in a sandbox.

## What the article gets right (and what's missing)

**Load-bearing claims that hold up.** The taxonomy is grounded in actual leaked artifacts — `autoDream`, `Microcompact`, the four-stage compaction cascade, the 25+ hook points, the <20-tool default — corroborated by independent leak analyses ([Straiker](https://www.straiker.ai/blog/claude-code-source-leak-with-great-agency-comes-great-responsibility), [WaveSpeedAI](https://wavespeed.ai/blog/posts/claude-code-architecture-leaked-source-deep-dive/), [DEV/stevengonsalvez](https://dev.to/stevengonsalvez/claude-code-source-code-leaked-512k-lines-of-typescript-and-what-actually-matters-4k06)). The patterns are not made up; they map to real code paths.

**What's underweighted:**
- **MCP as a first-class harness primitive.** The article folds MCP into "progressive tool expansion" but MCP is doing more than late-binding — it's a network protocol with auth flows, elicitation, and discoverability. A serious harness builder needs the MCP patterns post alongside this one.
- **Evaluation and self-grading.** Anthropic's own engineering writeups stress separating generation from evaluation. The article omits the planner/generator/evaluator split entirely, even though it's a well-known harness pattern.
- **The advisor pattern.** Generative Programmer's follow-up ([Distribution vs Escalation](https://generativeprogrammer.com/p/subagents-vs-advisor-distribution)) covers server-side sub-inference advisors that critique without acting. That's a 13th pattern that should have been in the original.
- **Prompt injection survivability.** Compaction is presented as a context-economy mechanism. Straiker's analysis shows it's also an attack surface — payloads survive across the cascade. A harness designer needs to treat compaction summaries as untrusted.
- **Sandboxing and execution isolation.** Claude Code in production runs in container/devcontainer modes. The article omits sandboxing entirely.
- **Hook security model.** The article treats hooks as helpful automation. The [ci-agent-hardening](https://www.anthropic.com/engineering) literature makes clear hooks are also the primary place where prompt injection escalates to RCE.

**What's overweighted:** "Dream Consolidation" (pattern 4) is interesting but not load-bearing — most working harnesses do fine without it. Putting it on equal footing with Compaction (5) or Hooks (12) overstates its current importance.

**Net assessment.** As a checklist, the 12 patterns are a strong starting set. As a curriculum, they need an evals chapter, a security chapter, and an MCP deep-dive bolted on. A harness builder who implements patterns 1, 5, 6, 7, 10, 11, 12 will have a working coding agent. The rest are refinements.

## Reading list

Primary:
- [12 Agentic Harness Patterns From Claude Code](https://generativeprogrammer.com/p/12-agentic-harness-patterns-from) — the article itself.
- [Practical Lessons From the Claude Code Leak](https://generativeprogrammer.com/p/practical-lessons-from-the-claude) — companion piece by the same author.
- [12 MCP Patterns Behind Production Agents](https://generativeprogrammer.com/p/12-mcp-patterns-behind-production) — the natural sequel.
- [Distribution vs Escalation: When to Use Subagents or Advisors](https://generativeprogrammer.com/p/subagents-vs-advisor-distribution).

Leak analyses:
- [Straiker: Claude Code Source Leak](https://www.straiker.ai/blog/claude-code-source-leak-with-great-agency-comes-great-responsibility).
- [WaveSpeedAI: Claude Code Architecture Deep Dive](https://wavespeed.ai/blog/posts/claude-code-architecture-leaked-source-deep-dive/).
- [Inside Claude Code's Leak: 8 Compaction Modes, 3 Memory Tiers, 44 Flags](https://medium.com/data-science-collective/inside-claude-codes-leak-8-compaction-modes-3-memory-tiers-44-flags-anthropic-never-talked-c9740c501e63).
- [VentureBeat: 5 Actions Enterprise Security Leaders Should Take](https://venturebeat.com/security/claude-code-512000-line-source-leak-attack-paths-audit-security-leaders).
- [Claude Code Source Code Leaked — 512K Lines](https://dev.to/stevengonsalvez/claude-code-source-code-leaked-512k-lines-of-typescript-and-what-actually-matters-4k06).
- [Learn From Claude Code: Context Compaction](https://zane-portfolio.kiyo-n-zane.com/blog/by/developer/learn_from_claude_code_context_compaction/).

Anthropic primary sources:
- [How Claude remembers your project (Memory)](https://code.claude.com/docs/en/memory).
- [Hooks reference](https://code.claude.com/docs/en/hooks).
- [Create custom subagents](https://code.claude.com/docs/en/sub-agents).
- [Choose a permission mode](https://code.claude.com/docs/en/permission-modes).
- [Common workflows (Explore-Plan-Implement-Commit)](https://code.claude.com/docs/en/common-workflows).
- [Agent Skills overview](https://platform.claude.com/docs/en/agents-and-tools/agent-skills/overview).
- [Equipping agents for the real world with Agent Skills](https://www.anthropic.com/engineering/equipping-agents-for-the-real-world-with-agent-skills).
- [Writing effective tools for AI agents](https://www.anthropic.com/engineering/writing-tools-for-agents).
- [Effective context engineering for AI agents](https://www.anthropic.com/engineering/effective-context-engineering-for-ai-agents).
- [Building Effective AI Agents](https://www.anthropic.com/research/building-effective-agents).
- [Claude Code auto mode: a safer way to skip permissions](https://www.anthropic.com/engineering/claude-code-auto-mode).

Independent harness-engineering writing:
- [Addy Osmani: Agent Harness Engineering](https://addyosmani.com/blog/agent-harness-engineering/).
- [O'Reilly Radar: Agent Harness Engineering](https://www.oreilly.com/radar/agent-harness-engineering/).
- [Awesome Harness Engineering](https://github.com/ai-boost/awesome-harness-engineering).
- [Plan-and-Execute Agents (LangChain)](https://www.langchain.com/blog/planning-agents).
- [Claude Code Hooks: Complete Guide to All 12 Lifecycle Events](https://claudefa.st/blog/tools/hooks/hooks-guide).
- [Plan Mode in Claude Code (codewithmukesh)](https://codewithmukesh.com/blog/plan-mode-claude-code/).
- [Claude Code Plan Mode (DataCamp)](https://www.datacamp.com/tutorial/claude-code-plan-mode).
- [Claude Code Git Worktree Guide](https://thepromptshelf.dev/blog/claude-code-git-worktree-guide/).
- [Auto Mode & AI Classifier (DeepWiki)](https://deepwiki.com/claude-code-best/claude-code/4.2-auto-mode-and-ai-classifier).
- [Michael Bargury: Auto Mode analysis](https://www.mbgsec.com/archive/2026-03-29-claude-code-auto-mode-a-safer-way-to-skip-permissions/).
- [Progressive Disclosure Might Replace MCP (MCPJam)](https://www.mcpjam.com/blog/claude-agent-skills).
- [The CLAUDE.md Configuration Hierarchy (AI Agent Factory)](https://agentfactory.panaversity.org/docs/General-Agents-Foundations/claude-code-teams-cicd/claude-md-configuration-hierarchy).
- [The Complete Guide to CLAUDE.md (Bijit Ghosh, Medium)](https://medium.com/@bijit211987/the-complete-guide-to-claude-md-memory-rules-loading-and-cross-tool-compression-97cc12ed037b).

---

**Previous:** [01 · Overview & Glossary](./01-overview.md) · [↑ Index](./INDEX.md) · **Next:** [03 · Claude Code Architecture](./03-claude-code-architecture.md)
