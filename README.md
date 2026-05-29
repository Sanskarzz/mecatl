# ozzharness

A **headless agentic coding harness** in Go (1.26) — the streaming agent loop,
~7 core tools, an enforced plan/act gate, one-shot subagents, deterministic hooks,
a deny→ask→allow permission model, and prompt caching — speaking the OpenAI
**Responses API** behind a provider-agnostic port. Strict hexagonal/DDD: the domain
and the agent loop depend only on ports; the OpenAI client, the gRPC/HTTP servers,
the filesystem, and the tools are adapters wired only at the composition root.

This repo also contains the **research corpus** the design is built on (below).

## Quick start

```sh
task build            # → bin/ozzd (server), bin/ozzdemo (demo)
task test             # full suite (-race)
task lint             # golangci-lint (parallel-safe)

go run ./cmd/ozzdemo  # end-to-end demo, fully offline (scripted mock provider):
                      #   text → tool call → permission ask + approval → result + cache usage
go run ./cmd/ozzd --openai   # serve gRPC (:8080) + HTTP/SSE (:8081), loopback by default
                             #   (needs OPENAI_API_KEY)
```

The API is a bidi gRPC `Converse` stream (the client sends a `Prompt` then
`ResumeApproval`/`Cancel` frames; the server streams typed `Event`s) plus an HTTP/SSE
mirror — both over the same domain `Event`. See **[`docs/design/`](./docs/design/)**:
`ARCHITECTURE.md`, `STEP-CHAIN.md`, `OPENAI-RESPONSES-API.md`.

> **Note:** MCP support (future) is **streaming-HTTP transport only** — stdio MCP is
> not supported. OS-level sandboxing, four-tier compaction, and an MCP client are
> documented v1 non-goals with seams left for them.

---

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
