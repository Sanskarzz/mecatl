# Agent Harness Engineering — Research Corpus

A working reference on the design of **agentic coding harnesses** in 2026: the
patterns, architectures, recreations, and design decisions for building (or
evaluating) everything wrapped around a model so it can finish a software task.

> **A "harness" is** the system prompt, tool catalog, context policies, memory,
> sandboxes, permissions, subagents, hooks, plan/act gating, and observability
> around a model. *A decent model with a great harness beats a great model with a
> bad harness.* The leverage is in the harness — that's what this corpus is about.

## Where to start

| You are… | Go to |
|---|---|
| **An agent** (or want to find something specific) | **[`docs/harnesses/INDEX.md`](./docs/harnesses/INDEX.md)** — routes by goal, question, concept, and project |
| A human reading end to end | [`docs/harnesses/README.md`](./docs/harnesses/README.md) — narrative onramp and reading orders |
| Looking for repo conventions | [`AGENTS.md`](./AGENTS.md) |

## What's inside

8 files (~33K words) under [`docs/harnesses/`](./docs/harnesses/):

1. **Overview & glossary** — framing, audience, vocabulary.
2. **The 12 patterns** — reusable harness design patterns from Claude Code.
3. **Claude Code architecture** — loop, tools, plan mode, subagents, hooks, MCP, memory, compaction, permissions.
4. **Recreations** — the clones and parallel designs (opencode, crush, codex, gemini-cli, claw-code…).
5. **Comparative survey** — the wider field (Aider, Cline, Cursor, OpenHands, Devin, Goose, and more).
6. **Architecture patterns** — the cross-cutting design discipline.
7. **Context engineering & MCP** — the plumbing: caching, compaction, discovery, memory, MCP.
8. **Design considerations** — the opinionated synthesis and an MVP→v3 build roadmap.

Captured **2026-05-18**. Patterns are durable; time-bound facts (pricing, version
numbers) drift — see the index's freshness notes before relying on a number.
