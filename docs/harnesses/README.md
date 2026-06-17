# docs/harnesses — Agent Harness Engineering Reference

> Captured: 2026-05-18
> A research corpus on the design of coding-agent harnesses, compiled to support building a new harness from a position of knowledge.

This folder contains a working reference on agentic coding harnesses in 2026 — the patterns, architectures, recreations, and design decisions that constitute the discipline. Each file is independently readable; the suggested narrative order is below.

> **These docs describe the *field*, not mecatl's own conventions.** They are the research corpus mecatl was built *from* — survey material, not a description of this codebase. For mecatl's actual build conventions, architecture, and gotchas, see [`AGENTS.md`](../../AGENTS.md), [`docs/architecture.md`](../architecture.md), and [`docs/design/`](../design/README.md). (The one exception is [`09-agent-primitives-evaluation.md`](./09-agent-primitives-evaluation.md), which explicitly compares mecatl against the field.)

> **Looking for something specific — or an agent navigating this corpus?**
> Start with the **[INDEX — master router](./INDEX.md)**. It is the Tier-0 router: find your need
> by **goal**, **question**, **concept**, or **source project**, and it sends you
> to the exact file and section. This README is the human narrative onramp; the
> INDEX is the finding-aid. Every content file also opens with YAML frontmatter
> (`keywords`, `answers`, `related`) so a grep hit is self-describing.

## Contents

| # | File | Words | Topic |
|---|------|-------|-------|
| — | [INDEX — master router](./INDEX.md) | — | **Master router**: routes by goal, question, concept, and project. Read this first. |
| 01 | [Overview & glossary](./01-overview.md) | ~1.5K | Framing, audience, file map, glossary |
| 02 | [Twelve Patterns](./02-twelve-patterns.md) | ~3.7K | The Generative Programmer pattern catalogue + cross-references |
| 03 | [Claude Code Architecture](./03-claude-code-architecture.md) | ~5.9K | Deep dive: loop, tools, plan mode, subagents, hooks, MCP, memory, compaction, permissions |
| 04 | [Claude Code Recreations](./04-claude-code-recreations.md) | ~4.9K | Survey: sst/opencode, crush, Kode, learn-claude-code, claude-code-router, codex-cli, gemini-cli, qwen-code, claw-code |
| 05 | [Comparative Harnesses](./05-comparative-harnesses.md) | ~5.3K | Aider, Cline, Roo Code, Continue, OpenHands, SWE-agent, Cursor, Windsurf, Devin, Goose, Copilot, Augment, Zed |
| 06 | [Architecture Patterns](./06-architecture-patterns.md) | ~4.4K | Cross-cutting design: loop, ReAct/Plan-Execute/Reflexion/ToT, multi-agent, tools, system prompts, hooks, permissions, slash, headless, anti-patterns |
| 07 | [Context Engineering and MCP](./07-context-and-mcp.md) | ~4.0K | Token economics, prompt caching, compaction, agentic discovery vs. indices, memory, MCP protocol and best practices, tool descriptions, error handling, observability |
| 08 | [Design Considerations](./08-design-considerations.md) | ~3.5K | Opinionated synthesis: 13 load-bearing decisions, MVP→v3 roadmap, cost and security levers, ideas to steal by project, anti-patterns, unresolved bets, sanity-check gauntlet |
| 09 | [Agent-Primitives Evaluation](./09-agent-primitives-evaluation.md) | ~2.2K | mecatl vs the field (June 2026): delegation-primitives comparison, ahead/behind tables, the tiered roadmap that drove the work, remaining items as issues #28–#40 |

**Total:** 9 files, ~37K words, ~280KB of markdown.

## Suggested reading orders

**One-hour path.** README → 01 → 08. Vocabulary + landscape + actionable opinion.

**Building-a-harness path.** 01 → 02 → 06 → 07 → 08. Vocabulary, patterns, architecture discipline, plumbing, synthesis.

**Benchmark-against-Claude-Code path.** 03 → 04 → 08. What the reference implementation does and what every clone agreed to keep.

**Evaluate-the-field path.** 05 → 04 → 08. Broad survey + Claude Code orbit + synthesis.

**MCP-server-design path.** 07 §§7–10 → 06 §6 → external MCP spec.

## What this corpus draws on

Primary categories of source:

- **Anthropic engineering writing** — Building Effective Agents, Effective Context Engineering, Writing Effective Tools, How We Built Our Multi-Agent Research System, Code Execution with MCP, Claude Code official docs.
- **The Generative Programmer article** "12 Agentic Harness Patterns From Claude Code" and its companion pieces.
- **Community reverse-engineering** of Claude Code: the 2025 partial leak and the March 2026 source-map exposure (~512K lines TypeScript) — primary analyses by Ghuntley, Karan Prasad, bits-bytes-nn, Straiker, WaveSpeedAI, Sid Bharath, Kir Shatrov, Agiflow, MindStudio.
- **Open-source recreations and parallel designs** — sst/opencode, charmbracelet/crush, shareAI-lab/Kode and learn-claude-code, claude-code-router, claw-code, openai/codex, google-gemini/gemini-cli, QwenLM/qwen-code.
- **Comparative harnesses** — Aider (paul-gauthier), Cline, Roo Code, Continue.dev, OpenHands (All-Hands-AI), SWE-agent (Princeton), Cursor, Windsurf (Codeium), Devin (Cognition), Goose (Block / Linux Foundation), GitHub Copilot Coding Agent, Augment, Zed Agent.
- **Foundational papers** — ReAct, Reflexion, Tree of Thoughts, Plan-and-Solve, ReWOO, SWE-agent NeurIPS, OpenHands V1 SDK, Lilian Weng's foundational agent post, Chroma's Context Rot.
- **Standards and specs** — MCP specification (revision 2025-11-25), AGENTS.md open standard, Anthropic Agent SDK reference.

Each file has its own embedded reading list with direct links.

## Caveats

- Information is captured as of 2026-05-18 and includes details that will drift (pricing, version numbers, star counts, feature names). The patterns and architecture choices are durable; specific artifacts are not.
- Claims sourced from community reverse-engineering of Claude Code are flagged inline as "(community RE)" — treat any single number with appropriate skepticism.
- Closed-source products (Cursor, Windsurf, Devin, Copilot Coding Agent) are documented from public posts, leaked system prompts, and inference. Internals may differ.

## How this corpus was produced

The six topical files (02–07) were drafted by independent parallel research passes, each constrained to a focused scope and instructed to cite sources inline. The framing files (01, 08, this README) were synthesized after all six were complete, so they reflect the corpus rather than guess at it.

Updates and corrections welcome.

## Contributing to this corpus

If you add or substantially edit a content file, update **both** its frontmatter
(`keywords` / `answers` / `related`) **and** the relevant routing rows in
[the INDEX](./INDEX.md) — the index is the contract; let it drift and agents stop
finding the content. Keep the corpus discoverable from the INDEX rather than deeply
nested. Captured 2026-05-18; the patterns are durable, but time-bound facts (pricing,
versions, star counts) drift, and community reverse-engineering claims are flagged
inline as "(community RE)".
