---
title: "Agent Harness Engineering — Master Index"
doc_id: INDEX
layer: meta
captured: 2026-05-18
status: stable
purpose: "Tier-0 routing index for the harness-engineering corpus. An agent reads THIS file first, then loads only the specific file and section it needs."
keywords: [harness, agent loop, tools, plan mode, subagents, hooks, permissions, sandbox, compaction, prompt caching, MCP, memory, context engineering, edit format, repo map, Claude Code, opencode, codex, aider, cursor, cline, devin, goose]
---

# Agent Harness Engineering — Master Index

> **You are an agent. Read this file, not the whole corpus.**
> This is a Tier-0 routing index (the "compact index always in context" pattern
> from [02 §3 Tiered Memory](./02-twelve-patterns.md#3-tiered-memory)). Find the
> row that matches your need, open **only** the file and section it names, and
> stop. Every content file is independently readable. Do not load files you
> were not routed to.
>
> Contributing to this corpus? The conventions (frontmatter, INDEX upkeep) are in
> [the corpus conventions](./README.md#contributing-to-this-corpus).

**Corpus:** 9 files, ~37K words, on the design of agentic *coding* harnesses as
of **2026-05-18**. Topic: everything wrapped around a model so it can finish a
task — system prompt, tools, context policy, memory, sandboxes, permissions,
subagents, hooks, plan/act gating, observability.

**How files are tagged.** Every file opens with YAML frontmatter
(`title`, `layer`, `keywords`, `answers`, `related`). If you reached a file by
grep, read its frontmatter first to confirm relevance before reading the body.

---

## 1. The corpus at a glance

| # | File | Layer | What it is |
|---|------|-------|------------|
| 01 | [Overview & glossary](./01-overview.md) | meta | Framing, audience, corpus map, and the **glossary** of every recurring term. |
| 02 | [02-twelve-patterns.md](./02-twelve-patterns.md) | reference | The **12 reusable harness patterns** distilled from Claude Code, with tradeoffs and cross-references. |
| 03 | [03-claude-code-architecture.md](./03-claude-code-architecture.md) | reference | **Deep dive on Claude Code**: loop, tools, plan mode, subagents, hooks, MCP, memory, compaction, permissions, settings. |
| 04 | [04-claude-code-recreations.md](./04-claude-code-recreations.md) | survey | The **clones and parallel designs** (opencode, crush, Kode, codex, gemini-cli, claw-code…): what they kept, what diverged. |
| 05 | [05-comparative-harnesses.md](./05-comparative-harnesses.md) | survey | The **wider field** (Aider, Cline, Roo, Continue, OpenHands, SWE-agent, Cursor, Windsurf, Devin, Goose, Copilot, Augment, Zed). |
| 06 | [06-architecture-patterns.md](./06-architecture-patterns.md) | practice | **Cross-cutting design discipline**: loop, reasoning patterns, multi-agent, tool/prompt/hook/permission design, anti-patterns. |
| 07 | [07-context-and-mcp.md](./07-context-and-mcp.md) | practice | **The plumbing**: context economics, prompt caching, compaction, discovery vs. indices, memory, MCP, tool result shaping, observability. |
| 08 | [08-design-considerations.md](./08-design-considerations.md) | practice | **Opinionated synthesis**: 13 load-bearing decisions, MVP→v3 roadmap, cost/security levers, ideas to steal, unsettled bets, a sanity-check gauntlet. |
| 09 | [09-agent-primitives-evaluation.md](./09-agent-primitives-evaluation.md) | evaluation | **mecatl vs the field (June 2026)**: delegation-primitives comparison vs Claude Code/opencode/Gemini/Codex/Amp/Goose/Cursor/Copilot/Factory/Crush; ahead/behind tables; the tiered roadmap that drove the 3b982bb..0bdcc21 work; remaining items = issues #28-#40. |

**Layer meaning:** `meta` = orientation · `reference` = how a specific thing is
built · `survey` = landscape · `practice` = what to do when building your own.

---

## 2. Route by goal — "I am trying to…"

| If your goal is… | Read, in order |
|---|---|
| **Get oriented fast (≈1 hour).** | 01 → [08 §TL;DR](./08-design-considerations.md#tldr) → [08 §The 13 load-bearing decisions](./08-design-considerations.md#the-13-load-bearing-decisions) |
| **Build a harness from scratch.** | [08 §MVP → v3 roadmap](./08-design-considerations.md#mvp--v3-roadmap) → [06](./06-architecture-patterns.md) → [07](./07-context-and-mcp.md) → 02 |
| **Re-implement Claude Code specifically.** | [03](./03-claude-code-architecture.md) → [03 §What's load-bearing vs. cosmetic](./03-claude-code-architecture.md#whats-load-bearing-vs-cosmetic) → 04 |
| **Evaluate / compare the field.** | 05 → 04 → [05 §Comparison matrix](./05-comparative-harnesses.md#comparison-matrix) |
| **Design just the tools.** | [06 §6 Tool design principles](./06-architecture-patterns.md#6-tool-design-principles) → [07 §9 Tool descriptions](./07-context-and-mcp.md#9-tool-descriptions-that-actually-work) → [07 §10 Tool result shaping](./07-context-and-mcp.md#10-tool-result-shaping) |
| **Design context / cost strategy.** | [07 §1–4](./07-context-and-mcp.md#1-context-as-a-finite-resource) → [08 §Cost levers](./08-design-considerations.md#cost-levers) |
| **Design MCP integration / an MCP server.** | [07 §7 MCP overview](./07-context-and-mcp.md#7-mcp-overview) → [07 §8 MCP best practices](./07-context-and-mcp.md#8-mcp-best-practices) → [03 §MCP integration](./03-claude-code-architecture.md#mcp-integration) |
| **Get security / sandboxing right.** | [08 §Security levers](./08-design-considerations.md#security-levers) → [03 §Permissions and sandboxing](./03-claude-code-architecture.md#permissions-and-sandboxing) → [06 §9 Permissioning models](./06-architecture-patterns.md#9-permissioning-models) |
| **Decide plan/act, subagents, multi-agent.** | [06 §3 Plan / Act mode](./06-architecture-patterns.md#3-plan--act-mode) → [06 §4 Multi-agent](./06-architecture-patterns.md#4-multi-agent-patterns) → [06 §5 Subagent / Task](./06-architecture-patterns.md#5-the-subagent--task-pattern) |
| **Sanity-check a design I already have.** | [08 §A closing test](./08-design-considerations.md#a-closing-test) (10-point gauntlet) |
| **Just learn the vocabulary.** | [01 §Glossary](./01-overview.md#glossary) |

---

## 3. Route by question

Concrete questions an agent is likely to carry, mapped straight to an answer location.

| Question | Where |
|---|---|
| What is a "harness"? | [01 §What is a harness?](./01-overview.md#what-is-a-harness) |
| What are the 12 patterns? | [02 §The 12 patterns](./02-twelve-patterns.md#the-12-patterns) |
| How is the agent loop built? | [03 §The agent loop](./03-claude-code-architecture.md#the-agent-loop) · [06 §1](./06-architecture-patterns.md#1-the-core-agent-loop) |
| What are the Edit tool's invariants? | [03 §Tool catalog](./03-claude-code-architecture.md#tool-catalog) (read-before-edit, exact-match, uniqueness) |
| What is the core tool set every harness keeps? | [04 §Patterns that survived the recreation](./04-claude-code-recreations.md#patterns-that-survived-the-recreation) · [08 §load-bearing #6](./08-design-considerations.md#the-13-load-bearing-decisions) |
| How does compaction work? | [03 §Context management](./03-claude-code-architecture.md#context-management) (4-tier) · [07 §4 Compaction strategies](./07-context-and-mcp.md#4-compaction-strategies) |
| How does prompt caching work? | [07 §3 Prompt caching](./07-context-and-mcp.md#3-prompt-caching) (Anthropic vs OpenAI) |
| Why does my long session cost so much? | [07 §2 Token economics](./07-context-and-mcp.md#2-token-economics-may-2026) · [08 §Cost levers](./08-design-considerations.md#cost-levers) |
| How do I enforce plan mode? | [06 §3](./06-architecture-patterns.md#3-plan--act-mode) (hook-enforced, not prompt) · [03 §Plan mode](./03-claude-code-architecture.md#plan-mode) |
| When should I use a subagent vs do it inline? | [06 §5 The Subagent / Task pattern](./06-architecture-patterns.md#5-the-subagent--task-pattern) |
| When is multi-agent worth it? | [06 §4 Multi-agent patterns](./06-architecture-patterns.md#4-multi-agent-patterns) (≈15× token cost) |
| How do hooks work? | [03 §Hooks](./03-claude-code-architecture.md#hooks) · [06 §8 Hooks and event systems](./06-architecture-patterns.md#8-hooks-and-event-systems) |
| How should permissions be evaluated? | [03 §Permissions and sandboxing](./03-claude-code-architecture.md#permissions-and-sandboxing) (deny→ask→allow) · [06 §9](./06-architecture-patterns.md#9-permissioning-models) |
| Do I need an OS-level sandbox? | [08 §Security levers](./08-design-considerations.md#security-levers) · [05 §Codex CLI](./05-comparative-harnesses.md#codex-cli) (gold standard) |
| How does MCP work? | [07 §7 MCP overview](./07-context-and-mcp.md#7-mcp-overview) |
| How do I keep many MCP tools from blowing context? | [03 §MCP integration](./03-claude-code-architecture.md#mcp-integration) (ToolSearch) · [07 §8](./07-context-and-mcp.md#8-mcp-best-practices) (code execution with MCP) |
| How should the model find code (grep vs index vs embeddings)? | [07 §5 Agentic discovery vs. pre-built indices](./07-context-and-mcp.md#5-agentic-context-discovery-vs-pre-built-indices) |
| What should the agent remember across sessions? | [07 §6 Memory systems](./07-context-and-mcp.md#6-memory-systems) · [03 §Memory system](./03-claude-code-architecture.md#memory-system) |
| What edit format should I use? | [05 §Themes](./05-comparative-harnesses.md#themes) · [08 §Bets the field hasn't settled](./08-design-considerations.md#bets-the-field-hasnt-settled) |
| Where should CLAUDE.md / AGENTS.md content go? | [06 §12](./06-architecture-patterns.md#12-claudemd--agentsmd--cursor-rules) · [03 §Memory system](./03-claude-code-architecture.md#memory-system) (user message, not system role) |
| How do I write a tool description that works? | [07 §9 Tool descriptions that actually work](./07-context-and-mcp.md#9-tool-descriptions-that-actually-work) |
| How do I write errors the model recovers from? | [07 §11 Error handling](./07-context-and-mcp.md#11-error-handling) |
| What should I instrument? | [07 §12 Observability](./07-context-and-mcp.md#12-observability) |
| What's the smallest thing I can ship? | [08 §v0](./08-design-considerations.md#v0--the-smallest-thing-that-works-12-weeks) |
| What are the common footguns? | [06 §13 Anti-patterns](./06-architecture-patterns.md#13-anti-patterns) · [08 §Anti-patterns to avoid](./08-design-considerations.md#anti-patterns-to-avoid) |

---

## 4. Route by concept (keyword map)

Alphabetical, with synonyms an agent might grep for. Each points to the primary
treatment; secondary mentions are everywhere.

- **ACI / Agent-Computer Interface** → [05 §SWE-agent](./05-comparative-harnesses.md#swe-agent)
- **ACP / Agent Client Protocol** → [05 §Zed Agent](./05-comparative-harnesses.md#zed-agent); [01 §Glossary](./01-overview.md#glossary)
- **Agent loop / query() / while(tool_use)** → [03 §The agent loop](./03-claude-code-architecture.md#the-agent-loop); [06 §1](./06-architecture-patterns.md#1-the-core-agent-loop)
- **AGENTS.md / CLAUDE.md / standing instructions** → [06 §12](./06-architecture-patterns.md#12-claudemd--agentsmd--cursor-rules); [02 §1](./02-twelve-patterns.md#1-persistent-instruction-file)
- **Compaction / microcompact / autocompact / summarize** → [07 §4](./07-context-and-mcp.md#4-compaction-strategies); [03 §Context management](./03-claude-code-architecture.md#context-management)
- **Context rot / context window / attention budget** → [07 §1](./07-context-and-mcp.md#1-context-as-a-finite-resource)
- **Dream consolidation / autoDream** → [02 §4](./02-twelve-patterns.md#4-dream-consolidation)
- **Edit format / SEARCH-REPLACE / unified diff / fast-apply** → [05 §Themes](./05-comparative-harnesses.md#themes); [08 §Bets](./08-design-considerations.md#bets-the-field-hasnt-settled)
- **Fast Apply / speculative edits** → [05 §Cursor](./05-comparative-harnesses.md#cursor-composer--agent)
- **Fork-join / parallel subagents / worktree** → [02 §8](./02-twelve-patterns.md#8-fork-join-parallelism)
- **Headless / SDK / -p mode** → [06 §11](./06-architecture-patterns.md#11-headless--sdk-mode); [03 §CLI surface](./03-claude-code-architecture.md#cli-surface-and-ide-integration)
- **Hooks / lifecycle events / PreToolUse** → [03 §Hooks](./03-claude-code-architecture.md#hooks); [06 §8](./06-architecture-patterns.md#8-hooks-and-event-systems)
- **MCP / Model Context Protocol** → [07 §7](./07-context-and-mcp.md#7-mcp-overview); [03 §MCP integration](./03-claude-code-architecture.md#mcp-integration)
- **Memory tiers / auto-memory / MEMORY.md** → [07 §6](./07-context-and-mcp.md#6-memory-systems); [02 §3](./02-twelve-patterns.md#3-tiered-memory)
- **Multi-agent / orchestrator-workers / swarm** → [06 §4](./06-architecture-patterns.md#4-multi-agent-patterns)
- **Permissions / deny-ask-allow / allowlist** → [03 §Permissions and sandboxing](./03-claude-code-architecture.md#permissions-and-sandboxing); [06 §9](./06-architecture-patterns.md#9-permissioning-models)
- **Plan mode / explore-plan-act / plan-and-execute** → [06 §3](./06-architecture-patterns.md#3-plan--act-mode); [02 §6](./02-twelve-patterns.md#6-explore-plan-act-loop)
- **Prompt caching / cache breakpoints / TTL / cache hit rate** → [07 §3](./07-context-and-mcp.md#3-prompt-caching)
- **ReAct / Reflexion / Tree of Thoughts / ReWOO** → [06 §2](./06-architecture-patterns.md#2-reasoning-patterns-react-plan-and-execute-reflexion-tot)
- **Repo map / tree-sitter / PageRank / symbol graph** → [05 §Aider](./05-comparative-harnesses.md#aider); [07 §5](./07-context-and-mcp.md#5-agentic-context-discovery-vs-pre-built-indices)
- **Sandbox / Seatbelt / Landlock / seccomp / bwrap** → [03 §Permissions and sandboxing](./03-claude-code-architecture.md#permissions-and-sandboxing); [05 §Codex CLI](./05-comparative-harnesses.md#codex-cli)
- **Skills / progressive disclosure / SKILL.md** → [06 §10](./06-architecture-patterns.md#10-slash-commands-and-skills); [02 §9](./02-twelve-patterns.md#9-progressive-tool-expansion)
- **Slash commands** → [03 §Slash commands and skills](./03-claude-code-architecture.md#slash-commands-and-skills); [06 §10](./06-architecture-patterns.md#10-slash-commands-and-skills)
- **Stop conditions / max_turns / runaway loops** → [06 §1](./06-architecture-patterns.md#1-the-core-agent-loop)
- **Subagent / Task tool / context isolation** → [06 §5](./06-architecture-patterns.md#5-the-subagent--task-pattern); [03 §Subagents](./03-claude-code-architecture.md#subagents-task-tool); [02 §7](./02-twelve-patterns.md#7-context-isolated-subagents)
- **System prompt / two-layer / cached prefix** → [03 §System prompt](./03-claude-code-architecture.md#system-prompt); [06 §7](./06-architecture-patterns.md#7-system-prompt-design)
- **Tool design / single-purpose tools / tool soup** → [06 §6](./06-architecture-patterns.md#6-tool-design-principles); [02 §11](./02-twelve-patterns.md#11-single-purpose-tool-design)
- **Tool result shaping / truncation / pagination** → [07 §10](./07-context-and-mcp.md#10-tool-result-shaping)
- **ToolSearch / tool-schema deferral** → [03 §MCP integration](./03-claude-code-architecture.md#mcp-integration); [07 §8](./07-context-and-mcp.md#8-mcp-best-practices)

---

## 5. Route by source project

Looking for what a *specific* harness does, or its single best idea:

| Project | Profile | Best idea to steal |
|---|---|---|
| **Claude Code** | [03 (whole file)](./03-claude-code-architecture.md) | [08 §Ideas worth stealing](./08-design-considerations.md#ideas-worth-stealing-by-project) |
| **sst/opencode** | [04 §sst/opencode](./04-claude-code-recreations.md#sstopencode--the-category-leading-clean-room-recreation) | DB-backed sessions; client/server; compaction math |
| **charmbracelet/crush** | [04 §crush](./04-claude-code-recreations.md#charmbraceletcrush--the-go-native-terminal-aesthetic-agent) | LSP-as-tool; curated model registry |
| **claude-code-router** | [04 §claude-code-router](./04-claude-code-recreations.md#musistudioclaude-code-router--the-proxy-not-a-fork) | Router pattern; per-task model routing |
| **shareAI-lab/Kode** | [04 §Kode](./04-claude-code-recreations.md#shareai-labkode--anon-kodes-successor-with-multi-model-collaboration) | Four model pointers; AskExpertModel |
| **learn-claude-code** | [04 §learn-claude-code](./04-claude-code-recreations.md#shareai-lablearn-claude-code--the-pedagogical-recreation) | 12-mechanism curriculum; "model is 80%" |
| **openai/codex** | [04 §codex](./04-claude-code-recreations.md#openaicodex--the-parallel-design-from-openai) · [05 §Codex CLI](./05-comparative-harnesses.md#codex-cli) | OS sandbox; App Server JSON-RPC; two-axis safety |
| **gemini-cli** | [04 §gemini-cli](./04-claude-code-recreations.md#google-geminigemini-cli--googles-answer-react-loop-first) · [05 §Gemini CLI](./05-comparative-harnesses.md#gemini-cli) | `stream-json` output; trusted-folders; hooks |
| **Aider** | [05 §Aider](./05-comparative-harnesses.md#aider) | Tree-sitter PageRank repo map; per-model edit format |
| **OpenHands** | [05 §OpenHands](./05-comparative-harnesses.md#openhands-formerly-opendevin) | Event-sourced log; persistent Jupyter kernel |
| **Cline / Roo Code** | [05 §Cline](./05-comparative-harnesses.md#cline) · [05 §Roo Code](./05-comparative-harnesses.md#roo-code) | Enforced plan/act; mode = persona+tools+model |
| **Continue** | [05 §Continue](./05-comparative-harnesses.md#continue) | Config-as-content; `create_rule_block` |
| **SWE-agent** | [05 §SWE-agent](./05-comparative-harnesses.md#swe-agent) | Reject human interface; linter-gated edits |
| **Cursor** | [05 §Cursor](./05-comparative-harnesses.md#cursor-composer--agent) | Fast Apply; worktree-per-agent; train for the harness |
| **Windsurf** | [05 §Windsurf](./05-comparative-harnesses.md#windsurf-cascade) | Continuous planner; user signals as context; Memories |
| **Devin** | [05 §Devin](./05-comparative-harnesses.md#devin-cognition) | Skeptic-by-prompt; replayable event timeline |
| **Goose** | [05 §Goose](./05-comparative-harnesses.md#goose) | Recipe as composable unit; sub-recipes as subagents |
| **Copilot Coding Agent** | [05 §Copilot](./05-comparative-harnesses.md#github-copilot-coding-agent) | Async-by-PR; CI runner as sandbox; Spaces |
| **Zed Agent** | [05 §Zed Agent](./05-comparative-harnesses.md#zed-agent) | ACP — the editor is not the agent |

---

## 6. Trust, freshness, and provenance

- **Captured 2026-05-18.** Patterns and architecture choices are durable.
  **Time-bound facts drift** — pricing, version numbers, star counts, model
  names, feature flags. The most volatile file is
  [07 (token economics, pricing)](./07-context-and-mcp.md#2-token-economics-may-2026); re-check vendor pages before relying on a number.
- **Community reverse-engineering** of Claude Code is flagged inline as
  **(community RE)** — chiefly in [03](./03-claude-code-architecture.md).
  Treat any single leaked number with skepticism; the structure is reliable,
  the exact figures are not.
- **Closed-source products** (Cursor, Windsurf, Devin, Copilot) are reconstructed
  from public posts, leaked prompts, and inference. Internals may differ.
- **Provenance.** Files 02–07 were drafted by independent parallel research
  passes (focused scope, inline citations). Files 01, 08, the README, and this
  index were synthesized after, so they reflect the corpus rather than guess.
- Each content file ends with its own **reading list** of primary sources.

## 7. Coverage and known gaps

**Covered:** the 12 Claude Code patterns; Claude Code's full architecture; the
clone/parallel-design ecosystem; ~15 comparative harnesses; the cross-cutting
design discipline (loop, reasoning patterns, multi-agent, tools, prompts, hooks,
permissions); context engineering (caching, compaction, discovery, memory); MCP;
and an opinionated build roadmap.

**Deliberately NOT covered here** (don't waste tool calls searching for them):
voice agents; computer-use / browser-use agents as a discipline; evaluation
harnesses (SWE-bench, METR); training a model *for* the harness (RL details);
and regulatory/compliance. See [01 §Caveats and gaps](./01-overview.md#caveats-and-gaps)
and [02 §What the article gets right (and what's missing)](./02-twelve-patterns.md#what-the-article-gets-right-and-whats-missing)
for the corpus's own account of its blind spots.
