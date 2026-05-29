# ozzharness

A **headless agentic coding harness** in Go — the system around a model that lets it
actually finish a software task: a streaming agent loop, a core tool kit, an enforced
permission model, deterministic hooks, one-shot subagents, and prompt caching. It speaks
the OpenAI **Responses API** behind a provider-agnostic port, and is driven over a gRPC +
HTTP/SSE API. No TUI — it's a service and a library.

> *A decent model with a great harness beats a great model with a bad harness.* The
> leverage is in the harness. ozzharness is a small, strict, well-tested implementation of
> that idea, built from the research corpus in [`docs/harnesses/`](./docs/harnesses/).

## Features

- **Streaming agent loop** (`iter.Seq2`) with pause, resume, and cancel — every step is a typed `Event`.
- **Seven-tool core kit** — Read (line-numbered), Edit (read-before-edit / exact-match / uniqueness invariants), Write, Bash, Grep, Glob, and a WebFetch stub.
- **Read-parallel / mutate-serial dispatch** — read-only tools run concurrently; mutating tools never do (a correctness guarantee, not an optimization).
- **Permission model** — `deny → ask → allow` across merged scopes; a deny in any scope wins; compound-bash and command-substitution aware; plan mode hard-denies mutations.
- **Permission pause/resume over the wire** — an `ask` suspends the loop and surfaces on the stream; the client approves and the loop continues.
- **Deterministic hooks** — `PreToolUse`/`PostToolUse` (and the rest of the lifecycle), JSON event on stdin, exit-code `0` allow / `2` block.
- **One-shot subagents** — the Task tool runs an isolated child loop and returns only its final string.
- **OpenAI Responses adapter** — stateless (`store:false`), reasoning items preserved across turns, cache-stable prompt prefix; points at OpenAI-compatible endpoints via a base-URL override.
- **Two API surfaces, one event model** — a bidi gRPC `Converse` stream and an HTTP/SSE mirror, both over the same domain `Event`.
- **Observability & persistence** — per-tool-call logging, an append-only JSONL replay store, and in-memory/JSONL session stores.
- **Strict hexagonal/DDD** — the domain and the loop depend only on ports; the OpenAI client, the servers, the filesystem, and the tools are adapters wired only at the composition root.

## Quick start

Requires **Go 1.26.3** (the `go.mod` toolchain directive auto-fetches it) and
[go-task](https://taskfile.dev). [golangci-lint](https://golangci-lint.run) for linting,
[buf](https://buf.build) only to regenerate the proto.

```sh
task build          # compile → bin/ozzd (server) and bin/ozzdemo (demo)
task test           # full suite, with -race
task lint           # golangci-lint (parallel-safe) + go vet
```

### Run the demo (fully offline)

`ozzdemo` drives a real agent loop against a scripted mock provider — no network, no API
key — to show the whole shape: a tool call, a permission prompt with approval, and a final
result with usage accounting.

```sh
go run ./cmd/ozzdemo
```

```text
=== ozzharness demo (offline / mockllm) ===
[001] turn=0 turn.start
[002] turn=0 message.delta  text="I'll read the greeting file first."
[003] turn=0 tool.call      tool=Read args={"path":"greeting.txt"}
[004] turn=0 tool.result    error=false result="     1\thello from the ozzharness demo workspace"
[005] turn=1 turn.start
[006] turn=1 message.delta  text="Now I'll save a short note, which needs your approval."
[007] turn=1 permission.ask ASK tool=Write reason="approval required by rule for Write (note.txt)"  -> client auto-approves
[008] turn=1 tool.call      tool=Write args={"path":"note.txt","content":"reviewed the greeting\n"}
[009] turn=1 tool.result    error=false result="wrote \"note.txt\" (22 bytes)"
[010] turn=2 turn.start
[011] turn=2 message.delta  text="Done: I read greeting.txt and saved note.txt."
[012] turn=0 result         stop=end_turn text="Done: I read greeting.txt and saved note.txt."
      usage: in=4100 out=125 cacheRead=3600 cacheWrite=0 cacheHitRate=0.88
```

### Run the server

```sh
export OPENAI_API_KEY=sk-...
go run ./cmd/ozzd --openai           # gRPC on 127.0.0.1:8080, HTTP/SSE on 127.0.0.1:8081
```

The server binds loopback by default. It is **unauthenticated** in v1 — intended for
local, single-user use; do not expose it on a non-loopback address without putting auth in
front of it. See [`docs/usage.md`](./docs/usage.md) for flags, the gRPC `Converse` flow,
and `curl` examples for the HTTP/SSE routes.

## Architecture at a glance

Dependencies point inward only. The domain and the application (the loop) know nothing of
OpenAI, gRPC, or the filesystem — those are adapters behind ports, wired together only in
`cmd/ozzd`.

```
 driving adapters            domain + application                 driven adapters
 ┌─────────────┐      ┌──────────────────────────────┐      ┌────────────────────┐
 │ gRPC server │─────▶│ agent (loop, dispatch,        │◀─────│ OpenAI Responses   │
 │ HTTP / SSE  │      │        pause/resume, subagent)│      │ mock LLM           │
 └─────────────┘      │   ↓ depends only on ports     │      │ os / in-mem FS     │
 ┌─────────────┐      │ session · governance · tool · │◀─────│ permission policy  │
 │ ozzdemo CLI │─────▶│ prompt   (domain)             │      │ shell hooks        │
 └─────────────┘      └──────────────────────────────┘      │ session stores     │
                                                             └────────────────────┘
```

- **[`docs/architecture.md`](./docs/architecture.md)** — the system in depth: layers, the loop, ports, sequence diagrams, extension points.
- **[`docs/usage.md`](./docs/usage.md)** — build/run, the demo, `ozzd` flags, the gRPC + HTTP/SSE APIs with examples, permissions, hooks, troubleshooting.
- **[`docs/design/`](./docs/design/)** — design rationale: `ARCHITECTURE.md`, `STEP-CHAIN.md`, `OPENAI-RESPONSES-API.md`.
- **[`CLAUDE.md`](./CLAUDE.md)** — orientation for agents working in this codebase.

## Project layout

| Path | Contents |
|---|---|
| `internal/session`, `internal/governance`, `internal/tool`, `internal/prompt` | the domain (aggregate, permission/hook types, tool catalog + FS interfaces, prompt assembly) |
| `internal/port` | the port interfaces the loop consumes |
| `internal/agent` | the agent loop, dispatch, permission pause/resume, compaction, subagent |
| `internal/adapter/*` | adapters: `openai`, `mockllm`, `osfs`/`memfs`, `permpolicy`, `hookexec`, `store/*`, `tools`, `server` |
| `contracts/proto`, `contracts/gen` | gRPC contract (source of truth) and generated Go |
| `cmd/ozzd`, `cmd/ozzdemo` | the server (composition root) and the demo |

## Status

This is a **v1** that builds the *shape*: the loop, the tools, permissions, hooks, the
cache, subagents, and the API, all green under tests and lint. The closing 10-point
"gauntlet" in [`docs/harnesses/08-design-considerations.md`](./docs/harnesses/08-design-considerations.md)
each maps to a passing test.

**Deliberate non-goals for v1** (with seams left in place for them): an OS-level sandbox,
four-tier compaction, multi-vendor model routing, and an MCP client. **MCP, when added,
will be streaming-HTTP transport only — stdio MCP is not supported.**

## Development

```sh
task            # list tasks
task ci         # tidy → fmt → lint → test → build
task generate   # regenerate contracts/gen from contracts/proto (needs buf)
go test ./internal/agent/ -run TestFullCycle   # a single test
```

Tests are offline by design (a scripted mock provider + an in-memory filesystem); CI never
needs a live model or network.

---

## Research corpus

This repository also holds the research corpus the harness was designed from — a working
reference on agentic coding-harness design in 2026 (patterns, architectures, recreations,
and design decisions).

- **[`docs/harnesses/INDEX.md`](./docs/harnesses/INDEX.md)** — the master router (start here if you're an agent or hunting for something specific).
- **[`docs/harnesses/README.md`](./docs/harnesses/README.md)** — the human narrative onramp and reading orders.
- **[`AGENTS.md`](./AGENTS.md)** — conventions for the corpus.

Eight files (~33K words) cover framing and glossary, the 12 Claude Code patterns, a Claude
Code architecture deep-dive, recreations, a comparative survey, cross-cutting architecture
patterns, context engineering + MCP, and the opinionated build roadmap that this harness
follows. Captured 2026-05-18; the patterns are durable, time-bound facts drift.
