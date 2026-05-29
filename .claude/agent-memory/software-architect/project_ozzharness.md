---
name: project-ozzharness
description: ozzharness is a NEW headless agentic coding-harness in Go (module github.com/stacklok/ozzharness, go 1.26.3); strict DDD/hexagonal, OpenAI Responses API behind an LLMProvider port.
metadata:
  type: project
---

ozzharness is a headless agentic coding-harness (no TUI) exposed as both a gRPC and HTTP service/library.

**Why:** Greenfield project; the repo started as only a research corpus under `docs/harnesses/` plus a bare `go.mod`. The architecture is distilled from that corpus, primarily `docs/harnesses/08-design-considerations.md` (the 13 load-bearing decisions + 10-point gauntlet).

**How to apply:**
- Scope is the v1 "production core" from doc 08: streaming async loop, 7-tool kit (Read/Edit/Write/Bash/Grep/Glob/Task), permission model (deny→ask→allow), hook surface, plan mode, subagents, single-stage compaction, stop conditions.
- Non-goals for v1: OS sandbox, four-tier compaction cascade, MCP client, repo map, embeddings, multi-agent. Design them as seams only.
- Design lives in `docs/design/ARCHITECTURE.md` and `docs/design/STEP-CHAIN.md` (both authored by this agent).
- The 10-point gauntlet in doc 08 §"A closing test" is the acceptance bar — every item maps to a design location.
- See [[domain-language]] and [[design-decisions]].
