# AGENTS.md

This repository is **mecatl** — a headless agentic coding harness in Go, plus the
**research corpus** that informed its design. The corpus documents patterns for
everything wrapped around a model so it can finish a software task: system prompt,
tools, context policy, memory, sandboxes, permissions, subagents, hooks, plan/act
gating, and observability.

## Start here

- **Using or modifying the harness?** → the [project README](./README.md) for architecture,
  quick start, and API reference.
- **Researching harness design patterns?** → the [research-corpus master index](./docs/harnesses/INDEX.md)
  is the master router into the corpus.

The INDEX is a Tier-0 routing index: find your need by **goal**, **question**,
**concept**, or **source project**, then open only the file and section it names.
Each of the 8 content files is independently readable and opens with YAML frontmatter
(`keywords`, `answers`, `related`) so any grep hit is self-describing.

## What's here

- `docs/harnesses/INDEX.md` — master index / router (read first).
- `docs/harnesses/README.md` — human narrative onramp and reading orders.
- `docs/harnesses/01-overview.md` … `08-design-considerations.md` — the corpus:
  framing + glossary (01), the 12 Claude Code patterns (02), a Claude Code
  architecture deep dive (03), recreations (04), a comparative survey (05),
  cross-cutting architecture patterns (06), context engineering + MCP (07), and
  an opinionated build roadmap (08).

## Conventions

- **Docs live under `docs/`.** Subfolders are fine; keep the corpus discoverable
  from the INDEX rather than deeply nested.
- **Captured 2026-05-18.** Patterns are durable; time-bound facts (pricing,
  versions, star counts) drift — see `INDEX.md` §6 for the freshness/trust notes.
- Claims from community reverse-engineering of Claude Code are flagged inline as
  **(community RE)**; treat single numbers with skepticism.

## When adding to the corpus

If you add or substantially edit a content file, update **both** its frontmatter
(`keywords`/`answers`/`related`) **and** the relevant routing rows in
`docs/harnesses/INDEX.md`. The index is the contract; let it drift and agents
stop finding the content.
