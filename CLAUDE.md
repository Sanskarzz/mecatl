# CLAUDE.md — mecatl

A **headless agentic coding harness** in Go 1.26 (hexagonal/DDD). The streaming agent
loop, ~7 tools, permissions, hooks, and subagents — speaking the OpenAI Responses API
behind a provider-agnostic port. Headless: driven over gRPC + HTTP, no TUI.

> `AGENTS.md` and `docs/harnesses/` are the **research corpus** this was built from, not
> harness conventions. For the build itself, read `docs/architecture.md` (how it works),
> `docs/usage.md` (how to run it), and `docs/design/` (rationale).

## Commands

**Always build through the Taskfile** (`task build`) — not raw `go build`. It compiles to
`bin/` with the right flags; a bare `go build` in the repo root drops stray binaries and is
the wrong workflow here.

```sh
task build              # → bin/mecated, bin/mecademo, bin/mecatui  (ALWAYS use this; never `go build` to repo root)
task test               # full suite, -race
task test:golden        # refresh mecatui View/teatest goldens (-update) then re-run
task lint               # golangci-lint v2 (parallel-safe) + go vet
task generate           # regenerate contracts/gen from contracts/proto via buf
go test ./internal/agent/ -run TestFullCycle   # a single test
go run ./cmd/mecademo    # end-to-end demo, fully offline (mock provider)
```

## Architecture (where things live)

- `internal/session/` — DOMAIN: the `Session` aggregate (state machine), Conversation, the `ToolCall`/`ToolResult`/`Usage` value objects, the `Event` taxonomy.
- `internal/governance/` — DOMAIN: permission `Effect`/`Scope`/`Rule` + `Evaluator`, bash splitting/canonicalization, hook event types. **Session-free** (`session` imports `governance`, never the reverse). Scopes run highest→lowest `Managed > CLI > LocalProject > SharedProject > User > ScopeBuiltinDefault` (the last, added for issue #13, is the built-in floor's scope — BELOW every config scope). A higher-scope config Allow may loosen **only** the `ScopeBuiltinDefault` Ask floor; it can never suppress a configured Ask. Deny-dominant + plan-mode gating still hold. The allow-all operator posture (`--yolo` → `app.Config.AllowAllTools`, injected by `mainRules` in `internal/app/build.go`) is a **rule** (a single `ScopeCLI` allow-all), NOT a `PermissionMode` and NOT an evaluator bypass — it loosens only the `ScopeBuiltinDefault` floor, so both invariants above are unchanged.
- `internal/tool/` — DOMAIN: `Tool`/`ToolSpec`/`Catalog` **and** the `FileSystem`/`Workspace` interfaces (see gotcha below).
- `internal/prompt/` — DOMAIN: two-layer prompt assembly + AGENTS.md/CLAUDE.md discovery; the turn-0 `InstructionAssembler` chain and its consumer-local ports (`MemoryIndexSource`, `SoulSource` — issue #14 Phase 1's read-only persona seam, `UserModelSource` — issue #14 Phase 2's cross-project operator-FACTS seam). Turn-0 ORDER is soul → memory index → user model (identity → saved project facts → operator model), all on the volatile turn-0 user-message seam (never `StablePrefix`).
- `internal/port/` — the PORT interfaces the loop consumes (`LLMProvider`, `SessionStore`, `HookRunner`, `PermissionPolicy`, `Clock`, `Logger`, `EventSink`). `PermissionPolicy.Evaluate` carries the session as a READ-ONLY `tool.WorkspaceReader` (Root+Read+Stat — issue #13) so file-based config resolves per-session against that root without a mutate-capable handle; `ws` may be nil (child/member engines with no resolver).
- `internal/agent/` — APPLICATION: the loop (`Engine`/`Run`), dispatch, permission pause/resume, compaction, the Task subagent.
- `internal/adapter/` — ADAPTERS: `openai`, `mockllm`, `osfs`/`memfs`, `permpolicy`, `permconfig` (file-based permission config — issue #13: a `permpolicy.RuleResolver` that re-resolves per workspace-root the shared `.mecatl/settings.yaml`→`ScopeSharedProject`, the gitignored `.mecatl/settings.local.yaml`→`ScopeLocalProject`, the matching Claude `settings{,.local}.json` imports, explicit `--permission-config` files→`ScopeCLI`; trust-gates project allows; caches per root with **mtime/size revalidation** so a mid-process edit takes effect; byte+rule caps), `soul` (issue #14 Phase 1: a user-scoped, **agent-read-only** persona over `~/.config/mecatl/soul.md`, satisfying `prompt.SoulSource`; env-injected resolution — NOT the WorkspaceReader, the file is outside any session root — injection-scanned via `skills.ScanForInjection` + 20 KiB cap, fail-soft, **no write path** — every method is read-only: `Load`, `LoadWithMeta` (issue #14 Phase 3: the same clean body + its sha256 computed in one read, no second read), `ResolvedPath`. The drift BASELINE write lives in `internal/app/soulguard.go` (composition), NEVER the adapter: a harness-owned `<soulPath>.sha256` sidecar, trust-on-first-use, `slog.Warn` on mismatch, `--approve-soul` re-baselines, `--soul-strict` drops a drifted soul; drift is NOT a governance gate, the soul stays fenced DATA), `memory` (per-project Remember/Recall/SearchMemory store — AND issue #14 Phase 2's SECOND, user-scoped, **cross-project** user-model store: a separate `memory.New(<xdg>/mecatl/usermodel)` exposing the parameterized RememberUser/RecallUser/SearchUserModel family under an enforced `user/` prefix, satisfying `prompt.UserModelSource`, with a write-time `skills.ScanForInjection` over BOTH the value AND the effective description — the `<user-model>` block renders key+description, so a value-only scan would miss a payload in `description` — plus a `</user-model>` fence-close-tag reject (mirrors soul) — an adapter→adapter edge like `soul`; the WRITABLE user-model is FACTS not rules, never a governance scope), `hookexec`, `store/*`, `tools`, `server`.
- `internal/app/` — COMPOSITION (not domain): the single shared assembly of provider + catalog + policy + engine into a `server.Service` (`app.Build(ctx, Config)`). Both composition roots consume it — `cmd/mecated` (serves it over TCP) and `cmd/mecatui` (hosts it embedded over a UNIX socket). It MAY import adapters, `internal/agent`, and (via the `server` adapter) `contracts/gen`; nothing imports it except the `cmd/` mains.
- `contracts/proto/mecatl/v1/` — gRPC contract (source of truth); `contracts/gen/` is generated — do not hand-edit.
- `cmd/mecated/` — the standalone server (composition root): flag parsing, TLS/auth/rate-limit, the HTTP + metrics listeners, the `skills promote` subcommand; delegates the engine/service build to `internal/app`. `cmd/mecademo/` — the demo.
- `cmd/mecatui/` — an optional gRPC **client** TUI (Bubble Tea v2). It dials an external `mecated` (`--server`) or, by default, **hosts one in-process** over a UNIX socket (`embed.Start` → `app.Build`) — so a single binary "just works" with no daemon/port. The render packages (`ui`, `theme`) and the `client` package import no `internal/...` package and no proto directly — they render purely from proto `Event`s relayed by `client`. `contracts/gen` + grpc + `internal/app` are confined to `cmd/mecatui/client`, the new `cmd/mecatui/embed`, and the `cmd/mecatui` main. See `docs/tui.md`.

## The layering rule (the thing to get right)

Dependencies point **inward only** (verified by import review; not yet machine-enforced):

- Domain packages (`session`, `prompt`, `governance`, `tool`) and `internal/agent` must **never** import an adapter, `internal/agent` (from domain), `contracts/gen`, `os`, the OpenAI SDK, or gRPC.
- `internal/port` imports only domain packages + stdlib.
- `internal/agent` imports only domain + `port` — adapters are injected.
- Concrete adapters meet ports **only in the composition layer** — `internal/app` (the shared engine/service assembly) and the `cmd/` mains that consume it. No DI framework — explicit constructors. `internal/app` is the one composition package allowed to import adapters + `internal/agent`; keep that wiring there, not in domain/`port`/`agent`.

## Things That Will Bite You

- **`FileSystem`/`Workspace` live in `internal/tool`, NOT `internal/port`.** Moving them to `port` creates a `port↔tool` import cycle (`port.LLMRequest` references `tool.ToolSpec`; `Tool.Execute` takes a `Workspace`). Leave them in `tool`.
- **`port.PermissionPolicy` is implemented in `internal/adapter/permpolicy`, not `governance`** — `governance` can't import `session` (cycle), so the session-aware policy is an adapter over the session-free `governance.Evaluator`.
- **Preserve these invariants — they have tests that will fail if you regress them:**
  - Edit's three invariants: read-before-edit (+ unchanged-since), exact match, uniqueness-unless-`replace_all`.
  - Dispatch is **read-parallel / mutate-serial** (`tool.ReadOnly()` drives it). Two mutating tools must never run concurrently.
  - Permissions resolve **deny-dominant**: a deny in ANY scope is absolute. Among ask/allow the configured higher scope is honored, with ONE narrow exception (issue #13): a higher-scope config Allow may loosen **only** the built-in-default Ask (the `ScopeBuiltinDefault` floor) — it must NEVER suppress a configured Ask (a `ScopeManaged` Allow does not override a `ScopeSharedProject` Ask). An equal-scope ask/allow tie favors Ask. Plan mode still hard-denies mutations first.
  - The bash gate must stay substitution/newline-aware (`$()`, backticks, subshells, `\n` fail safe). Don't simplify `SplitCommands`/`ReadOnlyBash` back to operator-only splitting.
- **No stdio MCP, ever.** Future MCP is **streaming-HTTP transport only**. Never spawn `os/exec` MCP servers.
- **ACP prompt content is multimodal + provider-capability-gated.** `session/prompt` blocks become flattened text + `[]session.Content` media Parts via `acp.buildPromptContent` → `Service.StartRunContent`; it REUSES the `session.NewContent` validators + `ValidateMediaParts` at the boundary (no second validation path) and gates image/audio on `Service.ProviderCapabilities()` (advertised in `handleInitialize`, loud-rejected before the run). Never silent-drop a non-text block — every block becomes text, a Part, or a loud `codeInvalidParams`. Audio is wired but dormant (OpenAI Responses has no audio input member → `Audio:false`).
- **Tests are offline.** Use `mockllm` + `memfs`; never hit a live model/network in tests. The OpenAI SSE→Chunk path is tested from fixtures.
- **The OpenAI adapter is stateless:** `store:false`, no `previous_response_id`, reasoning items carried back via `encrypted_content`, byte-stable prompt prefix for caching. Don't switch to server-side conversation state.
- **`Session` is an aggregate** — mutate it through its methods (`RecordUserPrompt`, `RecordAssistant`, `ReplaceHistory`, …), not by poking `Conversation` directly.
- **Every run-entry path must reopen-if-completed.** A turn drives the session to a terminal state (`completed`) within a single `Engine.Run`; the engine deliberately does NOT reopen (the team supervisor owns its own `Reopen` so it can accumulate lifetime counters across rounds). So any path that hands a *reused* session to `Engine.Run`/`RunContent` for a follow-up prompt must reopen it first, or the engine's `RecordUserPrompt` rejects the terminal state (`illegal state transition: RecordUserPrompt from "completed"`). In the service layer that means **`loadAndReopen`, never `GetSession`, before a run** — `GetSession` is for read-only snapshots only. All wire surfaces (gRPC/HTTP/ACP) funnel through `Service.StartRunContent`, which reopens; the supervisor reopens between rounds; subagent/fork/judge create fresh single-shot sessions. `TestStartRunContentReopensCompletedSession` guards the in-process multi-turn path.

## Verification

After changes: `task lint && task test` must be green, and `go run ./cmd/mecademo` must still print a full offline session (turn → tool.call → permission.ask + approval → result). The 10 gauntlet items in `docs/harnesses/08-design-considerations.md` ("A closing test") each have a passing test — keep them passing.

## Workflow

- Commit directly to `main` for this repo. End commit messages with the `Co-Authored-By` trailer.
- Never `git add -A` — stage explicit paths.
- For smoke tests / scratch files, use the repo-local `.scratch/` dir (gitignored) — **not** `/tmp` or `mktemp`. (`go run`/`go test` still use the Go build cache; that's fine.)
