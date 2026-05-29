---
title: "Overview & Glossary — An Engineer's Reference on Agent Harness Design"
doc_id: 01-overview
layer: meta
captured: 2026-05-18
status: stable
keywords: [harness definition, corpus map, glossary, ACI, ACP, agentic discovery, AGENTS.md, CLAUDE.md, auto-memory, compaction, context rot, edit format, fast apply, hooks, plan mode, prompt caching, ReAct, recipe, repo map, skill, subagent, tool soup, TodoWrite, worktree]
answers:
  - "What is an agent harness?"
  - "What do the core terms (ACI, compaction, context rot, edit format, plan mode, subagent, repo map) mean?"
  - "Where should I start reading, and in what order for my goal?"
  - "What does this corpus cover and what are its caveats?"
related: [INDEX, README, 08-design-considerations]
---

# Overview: An Engineer's Reference on Agent Harness Design

> Captured: 2026-05-18
> Audience: engineers building (or evaluating) a coding-agent harness
> Scope: design patterns, architecture decisions, and the post-Claude-Code landscape

## What this corpus is

This is a working reference compiled to support building a new agent harness from a position of knowledge rather than improvisation. It draws on:

- The Generative Programmer ["12 Agentic Harness Patterns" article](https://generativeprogrammer.com/p/12-agentic-harness-patterns-from) and its companions.
- Anthropic's engineering writing on agents, context engineering, tools, and MCP.
- Community reverse-engineering of Claude Code (the partial 2025 leak plus the larger 2026 source-map exposure).
- Open-source recreations and clean-room rewrites (sst/opencode, charmbracelet/crush, shareAI-lab/Kode, learn-claude-code, claw-code, codex-cli, gemini-cli, qwen-code, claude-code-router).
- The wider field of coding agent harnesses (Aider, Cline, Roo Code, Continue, OpenHands, SWE-agent, Cursor Composer, Windsurf Cascade, Devin, Goose, GitHub Copilot Coding Agent, Augment, Zed Agent).
- Foundational papers (ReAct, Reflexion, Tree of Thoughts, Plan-and-Solve, SWE-agent's ACI paper, OpenHands V1 SDK paper).

The corpus is opinionated where the field has converged, agnostic where it hasn't, and honest about the gaps.

## What is a "harness"?

A harness is everything wrapped around a model so it can actually finish a task: system prompt, tool catalog, context policies, memory, sandboxes, permissions, subagents, hooks, plan/act gating, observability. Addy Osmani's framing — *a decent model with a great harness beats a great model with a bad harness* — is now standard. The thesis behind every file in this corpus is that the harness, not the base model, is where most of the engineering leverage in 2026 agentic coding lives.

The 2025–2026 evolution of harness design is the move from "an LLM in a tool-use loop" to a coherent set of patterns that defend the model's finite context, separate read from write, push critical behavior out of prompts into deterministic code, and isolate noisy work into context-fresh subprocesses.

## Map of the corpus

Each file is independently readable, with cross-references where they help. The numbered ordering is a suggested narrative; nothing forces a sequential read.

- **[02 — Twelve Patterns](./02-twelve-patterns.md).** A pattern catalogue digested from the Generative Programmer article and corroborated against leak analyses. Read this first if you want a vocabulary for what a harness *does*: persistent instructions, scoped context assembly, tiered memory, dream consolidation, progressive compaction, explore-plan-act, context-isolated subagents, fork-join parallelism, progressive tool expansion, command risk classification, single-purpose tool design, deterministic lifecycle hooks.

- **[03 — Claude Code Architecture](./03-claude-code-architecture.md).** A deep technical reference on how Claude Code is built: the streaming async-generator agent loop, the two-layer system prompt, the ~40-tool catalogue with parameter shapes and invariants, plan mode, subagents, hooks, MCP integration, the dual memory system (CLAUDE.md + auto-memory), the four-tier compaction cascade, the permission model, slash commands and skills, settings precedence, the SDK and headless surface. Closes with a load-bearing-vs-cosmetic list aimed at re-implementors.

- **[04 — Claude Code Recreations](./04-claude-code-recreations.md).** A survey of the projects that grew up around the Claude Code leak: clean-room rewrites (sst/opencode, claw-code), forks (Kode), proxies (claude-code-router), pedagogical reimplementations (learn-claude-code), parallel designs (Codex CLI, Gemini CLI, qwen-code). Documents what every credible recreation kept (one loop, ~7 core tools, TodoWrite-style planning, AGENTS.md/CLAUDE.md discovery, MCP, per-tool permissions, compaction-as-separate-agent) and where they diverged (multi-model, sandbox approach, plugin systems, client/server split).

- **[05 — Comparative Harnesses](./05-comparative-harnesses.md).** The wider landscape outside the Claude Code orbit. Per-project profiles plus a comparison matrix and themes. Strongest insights cluster around: edit-format as a first-class design decision (SEARCH/REPLACE vs. unified diff vs. fast-apply); repo map / context discovery strategy (Aider's PageRank symbol graph remains under-borrowed); plan/act as enforced gating (Cline); persistent Jupyter kernel (OpenHands); linter-gated edits (SWE-agent); event-sourced agent runtime (OpenHands); JSON-RPC App Server / out-of-process agent (Codex, Zed ACP).

- **[06 — Architecture Patterns](./06-architecture-patterns.md).** The cross-cutting design discipline. The agent loop, reasoning patterns (ReAct, Plan-and-Execute, Reflexion, ToT) and when each is right, plan/act mode, multi-agent vs. subagent (Anthropic's "spawn 3–5 subagents" vs. Cognition's "don't build multi-agents"), tool-design principles, system-prompt anatomy, hooks vs. prompts, permissioning, slash commands and skills, headless mode, CLAUDE.md/AGENTS.md, anti-patterns.

- **[07 — Context Engineering and MCP](./07-context-and-mcp.md).** The plumbing. Context as a finite, degrading resource (context rot is real). Current token economics. Prompt caching — Anthropic's explicit four-breakpoint model with TTLs, OpenAI's automatic prefix cache, the practical cache architecture for a long session. Compaction strategies and what to preserve. Agentic discovery vs. pre-built indices vs. embeddings. Memory tiers and the "don't save what the file system already knows" rule. The MCP protocol, server best practices, and the code-execution-with-MCP pattern. Tool descriptions and result shaping. Error handling that teaches recovery. Observability.

- **[08 — Design Considerations](./08-design-considerations.md).** The opinionated synthesis. What the corpus implies for someone building a new harness in 2026: the load-bearing decision list, an MVP-through-v3 roadmap, the cost and security levers worth pulling early, ideas worth stealing from specific projects, and the bets the field hasn't yet resolved.

## Suggested reading orders

**If you have an hour.** README → 01 (this file) → 08 (design considerations). You'll come out with the vocabulary, the landscape, and an actionable opinion.

**If you're starting to design.** 01 → 02 (patterns) → 06 (architecture) → 07 (context + MCP) → 08. Builds the design vocabulary, then the implementation discipline, then the synthesis.

**If you're benchmarking against Claude Code specifically.** 03 (architecture deep-dive) → 04 (recreations) → 08. You'll understand what's load-bearing in the reference implementation and what every clone agreed to keep.

**If you're evaluating the field.** 05 (comparative harnesses) is the broadest survey; pair with 04 for the Claude Code orbit.

**If you're deciding between MCP server design choices.** 07 §§7–10 are the densest on this.

## Glossary

Terms used repeatedly across files.

- **ACI (Agent-Computer Interface).** The interfaces designed specifically for LLMs rather than humans. From SWE-agent: a paginated viewer is an ACI; raw `cat` is not.
- **ACP (Agent Client Protocol).** Out-of-process agent protocol pioneered by Zed and Codex, now also adopted by JetBrains. Speaks JSON-RPC over stdio.
- **Agentic discovery.** Letting the model find code via grep/find/read rather than via a pre-built index. The 2025–2026 default in coding agents.
- **AGENTS.md.** Open standard for project-level standing instructions; supersedes the original CLAUDE.md convention in cross-vendor settings.
- **Auto-memory.** Claude-authored persistent memory at `~/.claude/projects/<repo>/memory/`. Distinct from user-authored CLAUDE.md.
- **CLAUDE.md.** Anthropic's project- and user-level instruction file. Delivered as a *user message* after the system prompt, not as part of the system prompt itself.
- **CodeAct.** OpenHands' thesis that bash + a Python REPL + a browser DSL beats N bespoke tools with JSON schemas.
- **Compaction.** Replacing older conversation history with a summary to free context. Claude Code's pipeline has four tiers (snip / microcompact / collapse / autocompact); most recreations use a single LLM-summary stage.
- **Context rot.** Empirically measured degradation in LLM reasoning quality as input length grows, even within the model's window.
- **Edit format.** How the model expresses file edits: SEARCH/REPLACE block, unified diff, structured tool call with line ranges, fast-apply (model describes coarsely, separate fast model materializes the diff), or whole-file rewrite.
- **Fast Apply.** Cursor's pattern: planner LLM emits a coarse edit description, a separate speculative-decoding model materializes the actual diff at ~1000 tok/s.
- **Hooks.** Deterministic shell or LLM callbacks fired at lifecycle events (SessionStart, PreToolUse, PostToolUse, Stop, ...). Used for linting, formatting, blocking, logging, secret scanning.
- **MCP (Model Context Protocol).** JSON-RPC standard for connecting agents to tools, resources, and prompts. Anthropic-originated, now cross-vendor.
- **Plan mode.** Read-only operating mode where the agent explores and produces a plan but cannot write or run commands. Enforced by the harness, not the model.
- **Prompt caching.** Vendor mechanism for re-using a static prefix at a fraction of the input cost. Anthropic: explicit, 4 breakpoints, 5-min default TTL. OpenAI: automatic, prefix-based.
- **ReAct.** Reasoning + Acting alternation, now implicit in every native tool-use API.
- **Recipe (Goose).** Composable unit of agent behavior: instructions + extensions + parameters + provider settings + retry logic + response schema. Goose's answer to "what should the shareable unit be?"
- **Repo map (Aider).** Tree-sitter symbol graph ranked by personalized PageRank, presented to the model as scoped function/class signatures. The strongest non-embedding context strategy.
- **Skill.** A directory containing a SKILL.md (with YAML frontmatter) plus optional scripts and assets. Progressive disclosure: header always in context, body loaded when matched, assets loaded on demand.
- **Subagent.** A child agent with a fresh context window, scoped tools, and a one-shot return. Safer than true multi-agent because there's no coordination back-and-forth — the parent gets only the final string.
- **Tool soup.** The anti-pattern of registering too many overlapping tools, eating context and confusing the model's selection.
- **TodoWrite.** Claude Code's plan-tracking tool, mirrored as `Task` / `Todo` / `Plan` in every recreation.
- **Worktree (git worktree).** Mechanism for running multiple checked-out copies of a repo against one `.git` database. The basis for parallel subagent execution without lock contention.

## Caveats and gaps

- **Time-bound information.** Pricing, version numbers, star counts, and feature flags are accurate as of capture but will drift. The patterns are durable; the artifacts are not.
- **Some claims rely on community reverse-engineering** of the Claude Code bundle and are flagged inline as such. Anthropic has not published Claude Code's verbatim system prompt or every internal feature flag.
- **Closed-source products** (Cursor, Windsurf, Devin) are documented from public blog posts, leaked system prompts, and inference. Internal details may differ.
- **Not covered in depth.** Voice agents, computer-use / browser-use agents (Anthropic Computer Use, OpenAI Operator, Bytedance UI-TARS) as a separate discipline, evaluation harnesses (SWE-bench, METR Vending Bench), training-the-model-for-the-harness (Cursor Composer, Codex's GPT-5.2-Codex), and the regulatory/compliance angle (AI Act, etc.).
- **No code in this corpus** beyond illustrative pseudocode. The reading-list links go to real implementations.

## Provenance

All six topical files (02–07) were drafted in parallel by independent research passes, each constrained to a focused scope and instructed to cite sources inline. The overview, design considerations, and README (01, 08, this README) were synthesized after all six were complete to ensure they reflect the corpus rather than guess at it.
