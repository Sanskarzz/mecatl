# CLAUDE.md — ozzharness

A **headless agentic coding harness** in Go 1.26 (hexagonal/DDD). The streaming agent
loop, ~7 tools, permissions, hooks, and subagents — speaking the OpenAI Responses API
behind a provider-agnostic port. Headless: driven over gRPC + HTTP, no TUI.

> `AGENTS.md` and `docs/harnesses/` are the **research corpus** this was built from, not
> harness conventions. For the build itself, read `docs/architecture.md` (how it works),
> `docs/usage.md` (how to run it), and `docs/design/` (rationale).

## Commands

```sh
task build              # → bin/ozzd, bin/ozzdemo  (NEVER `go build` to repo root)
task test               # full suite, -race
task lint               # golangci-lint v2 (parallel-safe) + go vet
task generate           # regenerate contracts/gen from contracts/proto via buf
go test ./internal/agent/ -run TestFullCycle   # a single test
go run ./cmd/ozzdemo    # end-to-end demo, fully offline (mock provider)
```

## Architecture (where things live)

- `internal/session/` — DOMAIN: the `Session` aggregate (state machine), Conversation, the `ToolCall`/`ToolResult`/`Usage` value objects, the `Event` taxonomy.
- `internal/governance/` — DOMAIN: permission `Effect`/`Scope`/`Rule` + `Evaluator`, bash splitting/canonicalization, hook event types. **Session-free** (`session` imports `governance`, never the reverse).
- `internal/tool/` — DOMAIN: `Tool`/`ToolSpec`/`Catalog` **and** the `FileSystem`/`Workspace` interfaces (see gotcha below).
- `internal/prompt/` — DOMAIN: two-layer prompt assembly + AGENTS.md/CLAUDE.md discovery.
- `internal/port/` — the PORT interfaces the loop consumes (`LLMProvider`, `SessionStore`, `HookRunner`, `PermissionPolicy`, `Clock`, `Logger`, `EventSink`).
- `internal/agent/` — APPLICATION: the loop (`Engine`/`Run`), dispatch, permission pause/resume, compaction, the Task subagent.
- `internal/adapter/` — ADAPTERS: `openai`, `mockllm`, `osfs`/`memfs`, `permpolicy`, `hookexec`, `store/*`, `tools`, `server`.
- `contracts/proto/ozz/v1/` — gRPC contract (source of truth); `contracts/gen/` is generated — do not hand-edit.
- `cmd/ozzd/` — the server (composition root); `cmd/ozzdemo/` — the demo.

## The layering rule (the thing to get right)

Dependencies point **inward only**. Enforced by `.golangci.yml` depguard and by intent:

- Domain packages (`session`, `prompt`, `governance`, `tool`) and `internal/agent` must **never** import an adapter, `internal/agent` (from domain), `contracts/gen`, `os`, the OpenAI SDK, or gRPC.
- `internal/port` imports only domain packages + stdlib.
- `internal/agent` imports only domain + `port` — adapters are injected.
- Concrete adapters meet ports **only in `cmd/`** (the composition root). No DI framework — explicit constructors.

## Things That Will Bite You

- **`FileSystem`/`Workspace` live in `internal/tool`, NOT `internal/port`.** Moving them to `port` creates a `port↔tool` import cycle (`port.LLMRequest` references `tool.ToolSpec`; `Tool.Execute` takes a `Workspace`). Leave them in `tool`.
- **`port.PermissionPolicy` is implemented in `internal/adapter/permpolicy`, not `governance`** — `governance` can't import `session` (cycle), so the session-aware policy is an adapter over the session-free `governance.Evaluator`.
- **Preserve these invariants — they have tests that will fail if you regress them:**
  - Edit's three invariants: read-before-edit (+ unchanged-since), exact match, uniqueness-unless-`replace_all`.
  - Dispatch is **read-parallel / mutate-serial** (`tool.ReadOnly()` drives it). Two mutating tools must never run concurrently.
  - Permissions resolve **deny → ask → allow**, deny in any scope beats allow in any scope; plan mode hard-denies mutations.
  - The bash gate must stay substitution/newline-aware (`$()`, backticks, subshells, `\n` fail safe). Don't simplify `SplitCommands`/`ReadOnlyBash` back to operator-only splitting.
- **No stdio MCP, ever.** Future MCP is **streaming-HTTP transport only**. Never spawn `os/exec` MCP servers.
- **Tests are offline.** Use `mockllm` + `memfs`; never hit a live model/network in tests. The OpenAI SSE→Chunk path is tested from fixtures.
- **The OpenAI adapter is stateless:** `store:false`, no `previous_response_id`, reasoning items carried back via `encrypted_content`, byte-stable prompt prefix for caching. Don't switch to server-side conversation state.
- **`Session` is an aggregate** — mutate it through its methods (`RecordUserPrompt`, `RecordAssistant`, `ReplaceHistory`, …), not by poking `Conversation` directly.

## Verification

After changes: `task lint && task test` must be green, and `go run ./cmd/ozzdemo` must still print a full offline session (turn → tool.call → permission.ask + approval → result). The 10 gauntlet items in `docs/harnesses/08-design-considerations.md` ("A closing test") each have a passing test — keep them passing.

## Workflow

- Commit directly to `main` for this repo. End commit messages with the `Co-Authored-By` trailer.
- Never `git add -A` — stage explicit paths.
