# mecatl — Usage & Operator Guide

`mecatl` is a headless, agentic coding harness. It owns its own context
window, tool loop, permission policy and lifecycle hooks, and talks to OpenAI
(or any OpenAI-compatible `/v1/responses` endpoint). The server, `mecated`, exposes
one agent run over **gRPC** and **HTTP/SSE** concurrently.

> Security, up front: **the `mecated` API is UNAUTHENTICATED.** It exposes command
> and file execution against the configured workspace with no caller identity
> check. It is intended for **localhost, single-user** use, which is why the
> default listen addresses bind the loopback interface (`127.0.0.1`). Binding a
> non-loopback address exposes unauthenticated command/file execution to the
> network and must not be done without an external trust boundary. Auth / mTLS
> is future work.

---

## 1. Prerequisites & install

| Tool | Version | Needed for |
| --- | --- | --- |
| Go | >= 1.26.3 (toolchain auto-resolves from `go.mod`) | building & running everything |
| [go-task](https://taskfile.dev) | v3 | the `task` build targets |
| [golangci-lint](https://golangci-lint.run/) | v2.x | `task lint` (config: `.golangci.yml`) |
| `goimports` | — | `task fmt` only |
| [buf](https://buf.build/docs/installation) | latest | `task generate` only — regenerating the proto |

Build the binaries into `bin/`:

```console
$ task build
go build -o bin/mecated ./cmd/mecated
go build -o bin/mecademo ./cmd/mecademo
go build -ldflags "-X main.version=..." -o bin/mecatui ./cmd/mecatui
```

This produces `bin/mecated` (the server), `bin/mecatui` (the terminal UI), and
`bin/mecademo` (the offline demo). To install the operator-facing binaries into
`GOBIN` / `GOPATH/bin`:

```console
$ task install
```

`task install` installs `mecated` and `mecatui`; it intentionally skips
`mecademo`, which is only a demo binary.

Other handy targets (`task --list` for the full set):

| Task | What it does |
| --- | --- |
| `task build` | compile `bin/mecated`, `bin/mecatui`, `bin/mecademo` |
| `task install` | install `mecated` and `mecatui` into `GOBIN` / `GOPATH/bin` |
| `task test` | `go test -race ./...` |
| `task test:cover` | tests + `coverage/coverage.{out,html}` |
| `task lint` | `golangci-lint run` + `go vet` |
| `task fmt` | `gofmt` + `goimports` |
| `task tidy` | `go mod tidy` |
| `task generate` | `buf generate` (no-op unless `buf` + `contracts/proto` present) |
| `task ci` | tidy → fmt → lint → test → build |

You do **not** need a network or an API key for `task build`, `task test`, or
the offline demo.

The default `mecated` build is CGO-free and statically linkable (the ko image
builds it with `CGO_ENABLED=0`).

---

## 2. The 60-second demo

`mecademo` drives a **real `agent.Engine`** through a scripted session against a
canned offline provider (`mockllm`) — no network, no key. It proves the full
shape of the loop: an auto-allowed tool call, a tool call that requires approval
(and is approved), and a final assistant message with usage accounting.

```console
$ go run ./cmd/mecademo
=== mecatl demo (offline / mockllm) ===
Driving a real agent.Engine: auto-allowed tool call -> permission ask + approval -> final result.

[001] turn=0 turn.start
[002] turn=0 message.delta  text="I'll read the greeting file first."
[003] turn=0 tool.call      tool=Read args={"path":"greeting.txt"}
[004] turn=0 tool.result    error=false result="     1\thello from the mecatl demo workspace"
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

What each line means:

- **`turn.start`** — a new model call begins (`turn=N`).
- **`message.delta`** — streamed assistant text for the turn.
- **`tool.call`** — the model requested a tool, with raw JSON `args`.
- **`tool.result`** — the tool's output (`error=false/true`), token-shaped by the tool.
- **`permission.ask`** — the loop paused for client approval; carries the tool,
  the proposed args, and a human `reason`. In the demo a simulated client clicks
  "allow" (`run.Approve(askID, true)`), so the loop resumes.
- **`result`** — the terminal event: `stop` reason (`end_turn`, `max_turns`,
  `cancelled`, …), final text, and cumulative `usage`. `cacheHitRate` is
  `cacheRead / inputTokens`.

### Running the demo live

Drive the same scenario against a real model:

```console
$ export OPENAI_API_KEY=sk-...
$ go run ./cmd/mecademo --openai --model gpt-5
```

Demo flags (`cmd/mecademo`):

| Flag | Default | Meaning |
| --- | --- | --- |
| `--openai` | `false` | run against the live OpenAI Responses API (key from `OPENAI_API_KEY`) |
| `--model` | `mock-model` | model identifier when `--openai` is set |
| `--openai-base-url` | `""` | override the OpenAI API base URL |

Without `--openai` the demo is fully offline. With `--openai` and no
`OPENAI_API_KEY`, it exits with `--openai requires OPENAI_API_KEY to be set`.

---

## 3. Running the server (`mecated`)

`mecated` is a composition root: it parses flags/env, then delegates the
assembly — an LLM provider, the seven-tool catalog plus a read-only-explorer
`Subagent` delegation tool (which gets a full shell inside an isolated git worktree when Bash
is configured), the permission policy, lifecycle hooks, the session store, and the
two-layer system prompt — to the shared `internal/app` package (`app.Build`),
and serves the resulting `HarnessService` over gRPC and HTTP/SSE concurrently.
(The TUI reuses that same `app.Build` to host an embedded server — see below.)

```console
$ export OPENAI_API_KEY=sk-...
$ go run ./cmd/mecated --openai --workspace "$PWD"
```

### Flags

| Flag | Default | Meaning |
| --- | --- | --- |
| `--grpc-addr` | `127.0.0.1:8080` | gRPC listen address (loopback; API is **unauthenticated**) |
| `--http-addr` | `127.0.0.1:8081` | HTTP/SSE listen address (loopback; API is **unauthenticated**) |
| `--workspace` | current working dir | default session workspace root |
| `--model` | `gpt-5` | model identifier sent to the provider |
| `--openai` | `false` | use the OpenAI Responses provider (key from `OPENAI_API_KEY`) |
| `--openai-base-url` | `""` | override the OpenAI API base URL (compatible endpoints) |
| `--openrouter-base-url` | `""` | override the OpenRouter API base URL (default `https://openrouter.ai/api/v1`; key from `OPENROUTER_API_KEY`) |
| `--anthropic-base-url` | `""` | override the native Anthropic API base URL (compatible/proxy endpoints; key from `ANTHROPIC_API_KEY`) |
| `--mock` | `false` | use a canned offline mock provider (no network; smoke tests only) |
| `--store-dir` | `""` | directory for the JSONL session store (empty → in-memory) |
| `--session-store-url` | `""` | `host:port` of a remote **session-store gRPC driver** (`mecatl.driver.v1.SessionStoreService`); replaces the local store — mutually exclusive with `--store-dir`. **See the store-driver note below.** |
| `--memory-store-url` | `""` | `host:port` of a remote **memory-store gRPC driver** (`mecatl.driver.v1.MemoryStoreService`); replaces the local flock store — mutually exclusive with `--memory-dir`, enables the memory tools like `--memory-dir` does. |
| `--driver-auth-token` | `""` | bearer token sent on every store-driver RPC (or `MECATL_DRIVER_AUTH_TOKEN`; empty disables driver auth). Refused over cleartext to a non-loopback driver — pair with `--driver-tls`. |
| `--driver-tls` | `false` | enable transport TLS on the store-driver connections. |
| `--driver-tls-ca` | `""` | PEM CA bundle to verify the store driver's certificate (with `--driver-tls`; empty uses system roots). |
| `--driver-tls-cert` / `--driver-tls-key` | `""` | PEM client certificate/key pair for **mutual TLS** to the store driver. |
| `--skill-source-url` | `""` | `host:port` of a remote **skill-source gRPC driver** (`mecatl.driver.v1.SkillSourceService`); replaces local skills discovery — mutually exclusive with `--skills-dir`/`--skills-conventional`. **See the source-driver note below.** |
| `--soul-source-url` | `""` | `host:port` of a remote **soul-source gRPC driver** (`mecatl.driver.v1.SoulSourceService`); occupies the USER slot of the soul selection — mutually exclusive with `--soul-file` (`--no-soul` still wins). **See the source-driver note below.** |
| `--agent-source-url` | `""` | `host:port` of a remote **agent-definition gRPC driver** (`mecatl.driver.v1.AgentSourceService`); the definition set is **snapshotted at startup** (fatal if unreachable). Mutually exclusive with `--agents-dir`; the default-on conventional discovery is **superseded** (not an error). **See the source-driver note below.** |
| `--command-source-url` | `""` | `host:port` of a remote **slash-command gRPC driver** (`mecatl.driver.v1.CommandSourceService`); **composes** with file-backed commands (a local command file shadows a same-named driver command) and is consulted **live** per expansion/listing. Probed at startup (fatal if unreachable); runtime faults fail soft. **See the source-driver note below.** |
| `--skills-dir` | `""` | directory to discover progressive-disclosure skills from, laid out as `<name>/SKILL.md`. **Repeatable** (highest precedence, in the order given); empty disables the `Skill` tool unless `--skills-conventional` is set. **See the skills trust note below.** |
| `--skills-conventional` | `false` | also discover skills from the conventional known paths: `<workspace>/.mecatl/skills`, `<workspace>/.claude/skills`, `$XDG_CONFIG_HOME/mecatl/skills` (or `~/.config/mecatl/skills`), and `~/.claude/skills` — lower precedence than `--skills-dir`. **OFF by default** (strict opt-in); only enable for trusted locations. **See the skills trust note below.** |
| `--skills-draft-dir` | `""` | enable the writable `SkillDraft` tool and set the **quarantine** directory for model-authored candidate skills. Empty disables the tool. Must be **outside the workspace root** (so the model's `Write`/`Edit` cannot reach it) and **disjoint** from every `--skills-dir` / conventional location — both fatal startup errors. **See the self-improving-skill loop note below.** |
| `--skills-draft-similarity-threshold` | `0.5` | 2-gram Jaccard similarity above which `SkillDraft` warns of a near-duplicate existing skill (warn-only; it never blocks the draft). |
| `--soul-file` | `""` | path to a user-scoped, **agent-read-only** persona/"soul" file (empty → the conventional `$XDG_CONFIG_HOME/mecatl/soul.md`, fallback `~/.config/mecatl/soul.md`). Injected as turn-0 context, **fail-soft** (missing/empty/oversized/injection-flagged → no fragment). No tool can write it. **See the persona/soul note below.** |
| `--no-soul` | `false` | disable the user-scoped persona/soul fragment entirely (otherwise it is read from the conventional location, fail-soft if absent). |
| `--approve-soul` | `false` | (re)write the soul **drift baseline** to the current soul's content hash, accepting the file as-is. The baseline is a harness-owned sidecar next to the soul (`<soul-path>.sha256`); a later run whose hash differs logs a drift `WARN`. Use once after intentionally editing your soul. **See the persona/soul note below.** |
| `--soul-strict` | `false` | refuse a **drifted** soul: if its content hash differs from the recorded baseline, contribute **no** soul fragment this run (instead of the default warn-and-load). Pair with `--approve-soul` to accept an edit. |
| `--user-model-dir` | `""` | directory for the user-scoped, **cross-project** user-model store of durable FACTS about the operator (empty → the conventional `$XDG_CONFIG_HOME/mecatl/usermodel`, fallback `~/.config/mecatl/usermodel`). Exposes **RememberUser/RecallUser/SearchUserModel** + a turn-0 `<user-model>` block. **See the user-model note below.** |
| `--no-user-model` | `false` | disable the user model entirely (the RememberUser/RecallUser/SearchUserModel tools and the `<user-model>` block). |
| `--user-model-review` | `false` | enable the **opt-in** background user-model reviewer: after a session stops, a fresh single-shot child extracts durable operator FACTS from the transcript via RememberUser. OFF by default. It **never reopens** the user session; the write path is injection-scanned. |
| `--user-model-review-interval` | `1` | session-count debounce for `--user-model-review` (review every Nth session that stops; 1 = every session). |
| `--user-model-consolidate-interval` | `0` | interval for background consolidation (dream) of the user-model store, scoped to the `user/` namespace; 0 disables. |
| `--permissions-conventional` | `true` | auto-discover the per-project permission config (`<workspace>/.mecatl/settings.yaml`, and with `--import-claude-permissions` also `<workspace>/.claude/settings.json`) plus the user-global file. **Re-resolved per session** against each session's workspace root. ON and inert until such a file exists. **See the permission-config note below.** |
| `--import-claude-permissions` | `false` | also import Claude-Code `settings.json` permissions (project + user). **Lossy** (fail-safe): see the table below. |
| `--trust-project` | `false` | honour the discovered **project authority set**: the project's ALLOW rules (its deny/ask are always honoured regardless), its project persona/soul at `<workspace>/.mecatl/soul.md`, AND the **project tier** of agent definitions, slash commands, and skills (`<workspace>/.mecatl/*`, `<workspace>/.claude/*`). OFF by default (the safe stance) — an untrusted repo's grants, persona, agents, commands, and skills are withheld; the agent still runs in "ask the human" mode (see the workspace-trust note below). **See the permission-config, persona/soul, and workspace-trust notes below.** |
| `--permission-config` | `""` | path to a YAML permission-config file loaded at the **user (fully-trusted) scope** (**repeatable**). Always loaded regardless of `--permissions-conventional`. |
| `--yolo` | `false` | **OPERATOR POSTURE (dangerous).** Suppress permission prompts for the built-in mutate-ask floor (`Bash`/`Edit`/`Write`/`Team`/`SkillDraft`) **server-wide** — for ephemeral, isolated, single-tenant deployments only. A `Deny` in **any** scope and any **deliberately configured** `Ask` (managed/project/user) still apply. **Refused when running as root** (euid 0) unless `MECATL_SANDBOX=1` (or `IS_SANDBOX=1`) is set. **See the allow-all note below.** |
| `--metrics-addr` | `127.0.0.1:9090` | loopback **admin/observability** listener (empty disables). Serves `/metrics` and the runtime-introspection endpoints — **see the observability note below**. |
| `--otlp-endpoint` | `""` | OTLP collector endpoint for trace export (empty → tracing is a no-op; metrics are always on via `/metrics`). |
| `--otlp-protocol` | `grpc` | OTLP transport: `grpc` or `http`. |
| `--otlp-insecure` | `false` | skip TLS for the OTLP exporter (for a local collector). |
| `--flight-recorder` | `true` | arm a bounded in-memory execution-trace **flight recorder** (8 MiB / 5s window) so a trace of the recent past can be snapshotted on demand. Low overhead; `=false` disables. |
| `--mutex-profile-fraction` | `0` | `runtime.SetMutexProfileFraction` rate (0 = off). Populates `/debug/pprof/mutex`; has runtime overhead — enable only while investigating lock contention. |
| `--block-profile-rate` | `0` | `runtime.SetBlockProfileRate` rate in ns (0 = off). Populates `/debug/pprof/block`; has runtime overhead — enable only while investigating blocking. |
| `--perf-mcp` | `false` | mount the **read-only perf MCP server** at `/mcp` on the admin listener (see the observability note). Requires `--metrics-addr`, and that address **must be loopback** — a non-loopback `--metrics-addr` with `--perf-mcp` is **refused** (fail-closed). |

#### Delegation & sub-agents

mecatl ships three delegation tools that each spin up an **isolated read-only
child loop** (full shell inside a per-child git worktree when Bash is configured —
see the architecture doc): **Subagent** (one isolated child, returns its result),
**Parallel** (N isolated branches fanned out, joined `all`/`first`/`judge`), and
**Team** (a coordinating crew over a shared task list, findings ledger, and
mailbox). See the delegation-capabilities note below.

| Flag | Default | Meaning |
| --- | --- | --- |
| `--max-run-tokens` | `0` | loop-level **cumulative token ceiling per run** (input + output). A run that crosses it ends cleanly with `stop=budget` (terminal `StopBudget`). The budget is **inherited by every Subagent / Parallel branch / team member**, so a delegation fan-out cannot blow past it. `0` (the default) **disables** it. |
| `--max-team-tokens` | `0` | **team-wide cumulative token ceiling per team run** (input + output, summed across **all members and rounds**). When crossed the team stops scheduling new rounds — the **in-flight round and the lead's synthesis still complete**, and the report states the budget stop. Applies to the `Team` tool and gRPC `CreateTeam`; a per-call Team `max_team_tokens` may only **tighten** it. **Orthogonal** to `--max-run-tokens` (per-run; both compose). `0` (the default) **disables** it. |
| `--enable-parallel` | `true` | register the **Parallel** fan-out tool (N parallel isolated child branches). On by default; `=false` disables it. *(Renamed from the former `--enable-fork`.)* |
| `--fork-preserved-cap` | `agent.DefaultPreservedForkCap` | max **PRESERVED** winner forks (for `join=first`/`judge`) kept on disk at once — the oldest beyond this is LRU-reaped. Preserved fork workspaces stay inspectable (their paths ride the Parallel result) until reaped. |
| `--enable-teams` | `true` | register the experimental **agent-teams** capability (`CreateTeam`/`SpawnTeammate`/`RunTeam` + the in-loop `Team` tool). On by default and **inert** until a client drives a team; `=false` disables it. |
| `--subagent-model` | `""` | global model override applied to every Subagent / team-member child that does not pin its own `model:` in its definition (the analogue of `CLAUDE_CODE_SUBAGENT_MODEL`). A concrete id or a `--model-alias`. Empty inherits the parent `--model`. |
| `--agents-dir` | `""` | directory of named **agent definitions** (`<name>.md` + YAML frontmatter), reusable as a `Subagent(agent=<name>)` delegate and as a team-member role (repeatable; highest precedence). **TRUST BOUNDARY:** a def body steers the model like `AGENTS.md`/`CLAUDE.md`. |
| `--agents-conventional` | `true` | also discover agent defs from the conventional locations (`<workspace>/.mecatl/agents`, `<workspace>/.claude/agents`, `$XDG_CONFIG_HOME/mecatl/agents`, `~/.claude/agents`; lower precedence than `--agents-dir`). ON and **inert** until such a dir exists. Project-tier defs are **trust-gated** (`--trust-project`). |
| `--model-alias` | `""` | model alias mapping `name=model-id` (repeatable), resolved only in composition — an agent def's `model: <alias>` resolves through this map (then the built-in sonnet/opus/haiku aliases). |

> **Delegation capabilities (Subagent / Parallel / Team).** Beyond the shared
> `--max-run-tokens` budget, every delegation supports: an explicit **child-concurrency
> cap** (default 4) bounding how many children run at once; **per-call limits**
> (`max_turns` / `max_tool_calls` / `timeout`, **tighten-only** — a call can never
> loosen the inherited bounds) plus a **per-call model override** and a **per-call
> token ceiling**; **opt-in structured output** (a synthetic `SubmitResult` tool with
> bounded validation-retry when the caller supplies a result schema); and, on a
> Subagent, an **agentId trailer** on the returned result plus a `References:`
> convention the explorer uses to cite the files it read. A Subagent call may also run
> **in the background** (`background: true` — returns immediately with the agentId; the
> child keeps working, RUN-scoped, and the model collects the result via the
> **`SubagentStatus`** tool, prompted by a turn-boundary notice; still running at run
> end ⇒ cancelled but persisted + resumable), and every child — subagent, parallel
> branch, team member — is **individually cancellable** by its id (gRPC `cancel_child`
> frame / HTTP `POST /v1/sessions/{id}/cancel-child` / the `x` key in mecatui's
> overlay) without touching the run; see `docs/design/BACKGROUND-SUBAGENTS.md`.
> **Parallel** is observable
> over a dedicated `parallel.*` event family, and its result carries the preserved
> fork-workspace paths. A **Team** additionally honours a **team-wide token budget**
> (`--max-team-tokens`, tightenable per call) checked at the round boundary — orthogonal
> to the per-run `--max-run-tokens`, which still bounds each member drive. Team stream
> events are intentionally watchable but bounded: member previews/tasks/findings are
> capped, permission asks are never forwarded, and `team.end` includes closed-enum
> member dispositions. (The `mecatui` `ctrl+a` overlay surfaces all three under
> **Subagents | Parallel | Teams** tabs with a fleet-status footer — see `docs/tui.md`.)

#### LLM resilience knobs

These tune the resilience decorator wrapped around every provider (see
`internal/adapter/llmresilience`). They split cleanly into the **establishment**
window (retryable) and the **post-first-chunk** stream (terminal).

| Flag | Default | Meaning |
| --- | --- | --- |
| `--llm-max-attempts` | `3` | max stream-establish attempts (initial call plus retries). Retries apply ONLY before the first chunk — no-replay-after-first-chunk. |
| `--llm-per-attempt-timeout` | `30s` | per-attempt timeout for **establishing** an LLM stream (connect + first chunk). A timeout here is **retryable**. 0 disables. |
| `--llm-stream-idle-timeout` | `120s` | max idle gap between LLM stream chunks **after the first chunk**. A longer stall **ends the turn as an error** (it is terminal, never retried — replay is unsafe mid-stream). Guards against an upstream SSE connection that stalls mid-stream and would otherwise hang the turn forever. 0 disables. |
| `--llm-breaker-threshold` | `5` | consecutive **transient** establishment failures that open the per-provider circuit breaker (0 disables). |
| `--llm-breaker-cooldown` | `30s` | how long the breaker stays open before admitting a half-open trial. |

### Observability (the loopback admin listener)

`--metrics-addr` (default `127.0.0.1:9090`, empty disables) serves, **loopback-only and
unauthenticated** by design — these endpoints can expose prompt text and internal
state, so they must never be bound off-localhost:

| Endpoint | What |
| --- | --- |
| `/metrics` | Prometheus scrape — domain metrics (`mecatl_*`, incl. the turn/TTFT/inter-token/tool latency exponential histograms) plus Go runtime + process-RSS series, via the OTel prometheus exporter. |
| `/debug/pprof/` | the standard pprof profiles (`heap`, `goroutine`, `allocs`, `profile` (CPU), `trace`, and — when the rate flags are set — `mutex`, `block`). Capture with `go tool pprof http://127.0.0.1:9090/debug/pprof/heap`. |
| `/debug/vars` | a curated `mecatl_runtime` JSON snapshot (goroutines, heap, GC pauses, RSS, uptime) from `runtime/metrics` — cheap, structured, no STW. |
| `/debug/flightrecorder` | a snapshot of the in-memory flight-recorder ring (an execution trace of the recent past); view with `go tool trace`. Absent when `--flight-recorder=false`. |
| `/mcp` | the **perf MCP server** (read-only). Mounted only with `--perf-mcp`. Lets an agent introspect this process's runtime/latency/profile state over MCP — `list_slow_turns`, runtime/heap/CPU profile rankings, FlightRecorder summaries — returning **reduced numeric summaries** (never raw blobs). Absent (404) when `--perf-mcp` is off. |

> These are the **in-process** profiling sources — no external profiling backend
> is required. The `/mcp` endpoint (opt-in via `--perf-mcp`) exposes reduced,
> agent-readable summaries of the same data. See `docs/design/perf-observability.md`.

#### The perf MCP server (`--perf-mcp`)

`--perf-mcp` mounts a read-only MCP server at `/mcp` on the admin listener so an
agent can scrape this process's own performance state through MCP tools instead
of a human reading raw `/metrics` / `/debug/pprof`. It is **unauthenticated**
(decision 6: loopback + the SDK's DNS-rebinding protection only) and its output
can embed goroutine-derived function names and timing, so it is **fail-closed**:
`--perf-mcp` on a **non-loopback** `--metrics-addr` is **refused at startup**
(`bind loopback or add auth (future work)`). Any future off-loopback exposure
**MUST** add auth.

Generate a paste-ready client config with:

```sh
mecated perf-mcp print-config --metrics-addr 127.0.0.1:9090
```

It prints (note: **no `Authorization` header** — the surface is loopback/no-auth):

```json
{
  "mcpServers": {
    "mecatl-perf": {
      "type": "http",
      "url": "http://127.0.0.1:9090/mcp"
    }
  }
}
```

> **Embedded `mecatui` server:** when `mecatui` hosts its own server (`--perf
> --perf-mcp`), the admin surface defaults to a **fixed `127.0.0.1:9099`** (whereas
> `mecated` defaults to `9090`). A `mecatui` user can generate the matching client
> snippet by overriding the address — `mecated perf-mcp print-config --metrics-addr
> 127.0.0.1:9099` — or simply hardcode the `http://127.0.0.1:9099/mcp` URL, since
> the port is now predictable across restarts.

A companion **interpretation skill** ships at
`.claude/skills/perf-mcp-interpretation/` — it teaches an agent to read this
server's reduced output (tool routing/cost, pprof rankings, the leak/contention/GC
signatures, the upper-bound caveat). Because it lives under `.claude/skills/`, an
agent working in this repo (e.g. Claude Code) discovers it automatically; an
external MCP client can copy it in alongside the perf-server config so the
connected agent knows how to act on the numbers.

### Environment

| Var | Effect |
| --- | --- |
| `OPENAI_API_KEY` | the OpenAI API key. **If set, it implies `--openai`** — the real provider is selected automatically. |
| `OPENROUTER_API_KEY` | the OpenRouter API key. **If set, the `openrouter` provider is auto-detected** — it rides the same stateless Responses adapter against `https://openrouter.ai/api/v1` (override with `--openrouter-base-url`). OpenRouter also accepts an `OPENAI_API_KEY` by convention. |
| `ANTHROPIC_API_KEY` | the Anthropic API key. **If set, the native `anthropic` provider is auto-detected** — the native Messages-API adapter (NOT the Responses adapter), stateless full-replay, with extended thinking **on** (model-aware: adaptive for Opus 4.8/4.7/4.6 + Sonnet 4.6, manual budget for older families). Default model `claude-sonnet-4-6`; override the host with `--anthropic-base-url`. |
| `MECATL_SANDBOX` / `IS_SANDBOX` | set either to `1` to affirm an isolated, disposable environment so `--yolo` is permitted while running as root. |

### Provider selection

A provider is **required** — the server has nothing to do without one. The server
builds an N-provider registry and AUTO-DETECTS availability from the environment:

- `OPENAI_API_KEY` set (or `--openai`) → the `openai` Responses provider.
- `OPENROUTER_API_KEY` set → the `openrouter` provider (same adapter, OpenRouter
  base URL; falls back to `OPENAI_API_KEY` by convention).
- `ANTHROPIC_API_KEY` set → the native `anthropic` Messages provider (extended
  thinking on, model-aware; default model `claude-sonnet-4-6`).
- `--mock` → canned offline provider (single text turn; smoke tests only).
- none of the above → startup error (the daemon refuses to start; mecatui fails the
  same check client-side before hosting an embedded server). The message enumerates
  the accepted keys per adapter, the compatible/proxy base-URL overrides
  (`--openai-base-url` / `--anthropic-base-url` / `--openrouter-base-url` for an
  endpoint without a public key), the offline `--mock` escape hatch, and points here:
  `no LLM provider available: set one of ANTHROPIC_API_KEY (Claude), OPENAI_API_KEY (OpenAI), or OPENROUTER_API_KEY (one key, many models — a good first choice) in the environment; for an OpenAI- or Anthropic-compatible/proxy endpoint pass the matching key plus --openai-base-url / --anthropic-base-url / --openrouter-base-url; to try mecatl offline with no key run with --mock; see docs/usage.md for provider setup`.

When more than one provider is available, `openai` is the default (single-provider
back-compat) — a `CreateSession` with no selector uses it.

#### Per-session provider/model selection (wire)

`CreateSession` accepts an OPTIONAL **`provider_id`** + **`model_id`** selector
(gRPC fields; JSON `provider_id`/`model_id` on `POST /v1/sessions`), so one server
can drive different providers/models per session. The provider is **fixed for the
session lifetime** — "switch provider" means a new session. The two fields are
distinct (never slash-joined). Resolution:

| `provider_id` | `model_id` | Result |
|---|---|---|
| empty | empty | the server **default** provider (today's behaviour) |
| empty | set | **`InvalidArgument`** — a bare model on the default provider is ambiguous; name the provider |
| available | empty | that provider's default model |
| available | catalogued | bound to (provider, model) |
| available | not catalogued | **passthrough** — the model string is sent verbatim (escape hatch for a model the catalog doesn't yet know) |
| unknown / unavailable | any | **`InvalidArgument`** — `unknown or unavailable provider`, never a silent fallback |

Keys are NEVER on the wire — only the provider id. A server caps the number of live
per-session engines (selector OR client-MCP sessions): exceeding it returns
`ResourceExhausted` (gRPC) / HTTP `429` — close sessions you finish with
`CloseSession` / `DELETE /v1/sessions/{id}` to free slots.

#### Discovering models — `ListModels` / `GET /v1/models`

`ListModels` returns the selectable-model inventory: every **available** provider's
models projected to public metadata only — `id`, `provider_id`,
`display_name`, `image`, `reasoning`, `context_limit` — **no keys, env-var names, or
base URLs**, and an unavailable provider is omitted entirely. The list is
`(provider_id, id)`-sorted; it is empty when no provider is available (or under
`--mock`). `ServerCapabilities.model_selection` is `true` iff the list is non-empty,
so a client can hide its picker against an older/empty server. The advertised
`context_limit` is the SAME catalog value the per-session engine uses for its
compaction trigger, so a large-context model is not compacted at the 128k default.

For a provider with **live model listing** (currently **OpenRouter**), the picker
reflects the provider's **real, live catalog** (~344 models) rather than the curated
embedded subset, when that provider is keyed. The live list is fetched once in the
background just after startup and swapped in — `/models` shows the embedded subset at
first and the full live set a moment later (and offline, or on an upstream blip, it
keeps showing the curated subset — the embedded catalog is the fallback floor). The
fetch is keyless and never blocks startup. Providers without a live lister (OpenAI)
show their curated embedded catalog as before.

> Network note: a keyed OpenRouter `Build` makes **one best-effort background
> outbound request** to `https://openrouter.ai/api/v1/models` to populate the live
> picker. It is unauthenticated (no key on the wire) and fail-safe — offline, blocked,
> or any error simply leaves the embedded curated subset in place.

#### The `mecatui` model picker + client-side last-used persistence

`mecatui` surfaces the wire selection as the **`/models`** palette command (gated on
`caps.model_selection`): a flat, **type-to-filter** picker — each row tagged with
its `provider_id` (`provider · name`), filtered live by a case-insensitive
substring over `provider_id`/`id`/display name, scrolled in a window clipped to the
terminal height that follows the cursor (`↑`/`↓` to move, `pgup`/`pgdown` to page,
`home`/`end` to jump, `enter` to select; `esc` clears a non-empty filter, then
closes) that **persists** your choice and applies it to the **next**
session — the provider is fixed per session, so a pick takes effect on the next
`CreateSession`, not the live run. The selection is stored **client-side** in
`$XDG_STATE_HOME/mecatui/models.yaml` (fallback `~/.local/state/mecatui/models.yaml`)
as a per-workspace map (realpath-keyed). A pick is scoped to its own workspace; an
unseen/new repo falls back to the server default rather than inheriting another repo's
pick. This is machine-written **state** under `XDG_STATE_HOME`
(a sibling of the human config, mirroring `trust.yaml`'s settings-vs-state split). On
launch the persisted selection is **reconciled** against `ListModels` BEFORE the
first `CreateSession`: if its provider is no longer available (a removed key), it
falls back to the server default for that run with a loud notice — the state file is
left intact (the preference returns next launch), and connect never hard-fails with
`InvalidArgument`. The reconcile is **provider-level only** (issue #41): a saved
model missing from the (possibly embedded-floor, pre-live-refresh) snapshot is sent
anyway — the server validates the model string verbatim. A server-**rejected** saved
selection retries once on the server default and surfaces a **loud warning** naming
the rejected model, never a silent downgrade; the state file is not rewritten.

> **`--model` vs the picker.** For the embedded `mecatui` server, `--model` is the
> server's **default** model (what it resolves when the client sends no `model_id`);
> the `/models` picker is the **client's** per-session selector layered on top. For an
> external `--server`, the server owns its provider config — `--model` is only a header
> display hint, and the picker's selection rides the wire as `provider_id`/`model_id`.

### The loopback / unauthenticated trust note

On startup `mecated` logs the trust posture for each listen address:

```
level=INFO msg="API bound to loopback (unauthenticated, single-user localhost trust model)" flag=grpc-addr addr=127.0.0.1:8080
level=INFO msg="API bound to loopback (unauthenticated, single-user localhost trust model)" flag=http-addr addr=127.0.0.1:8081
```

If you bind a **non-loopback** address you get a prominent warning instead:

```
level=WARN msg="API bound to a NON-loopback address: the mecated API is UNAUTHENTICATED and exposes command/file execution; do not do this without an external trust boundary (auth/mTLS is future work)" flag=http-addr addr=0.0.0.0:8081
```

### The skills directory trust note

Skills are an **operator-trust boundary**, the same trust class as the
`AGENTS.md` / `CLAUDE.md` instruction files. Every discovered skill's one-line
description is injected into the model's context on **every** request (it lives in
the `Skill` tool spec), and a skill's full body flows into context the moment the
model **activates** it. Both are model-steering instructions, not sandboxed data:
a malicious or careless skill can redirect the agent just as a tampered
`CLAUDE.md` could.

Where skills are loaded from, in **precedence order** (highest first):

1. **`--skills-dir`** (repeatable) — explicit, operator-configured directories.
2. **Project-level** (only with `--skills-conventional`): `<workspace>/.mecatl/skills`
   then `<workspace>/.claude/skills`.
3. **User-level** (only with `--skills-conventional`): `$XDG_CONFIG_HOME/mecatl/skills`
   (or `~/.config/mecatl/skills`) then `~/.claude/skills`.

A higher-precedence skill **shadows** a same-named lower-precedence one (the drop
is logged). The `.claude/skills` and user-home paths exist for Claude Code
compatibility — they are convenient, but they **widen the surface** through which
an untrusted `SKILL.md` body can enter model context. They are therefore the same
trust class as `AGENTS.md`/`CLAUDE.md`: only enable `--skills-conventional` when
**every** one of those locations is yours to trust, and treat third-party skills as
code to review — read the `SKILL.md` before adding it, exactly as you would a CI
script.

Skills are **strict opt-in**: with no `--skills-dir` and `--skills-conventional`
unset, the `Skill` tool is never registered and nothing is read — that is why the
conventional set defaults OFF rather than auto-discovering. The always-in-context
description cap and the on-activation body truncation apply to **every** source,
including the conventional ones. (An OS-level sandbox around tool execution remains
future work — see the deferral note in the architecture doc.)

A **discovered** skill's own directory (the one holding its `SKILL.md`) also
becomes **read-visible** to the model — `Read` accepts that directory's absolute
path even when it lies outside the workspace (e.g. `~/.claude/skills/<name>`), so
an activated skill's bundled `references/`, `scripts/`, and `assets/` files are
actually reachable (the `Skill` tool's result names the base directory). This is
**read-only and per-skill**: `Write`/`Edit` still refuse those paths, `Glob`/`Grep`
never enumerate them, a shadowed skill's directory or a sibling under a skills
source never becomes readable, an untrusted workspace's project-tier skills are
never discovered and therefore never readable, and the `SkillDraft` quarantine dir
is never a source so it can never enter the read allowlist. Bundled scripts run
via Bash by absolute path, under the same permission gates Bash always has.

### File-based permission config (`.mecatl/settings.yaml`, issue #13)

The built-in permission policy (read-only tools allowed; `Bash`/`Edit`/`Write`/
`Team`/`SkillDraft` ask) can be tuned per project and per user with config files,
**re-resolved per session** against each session's workspace root by the
`internal/adapter/permconfig` resolver. So two sessions running in different repos
under the same `mecated` get **different** decisions for the same tool call.

**Schema** — `.mecatl/settings.yaml` (the checked-in, shared file),
`.mecatl/settings.local.yaml` (a gitignored personal override at a higher scope),
and the user-global file all share the same shape, mirroring Claude-Code's
permissions:

```yaml
permissions:
  allow:
    - "Bash(go test:*)"   # Claude "prefix:*" form, normalised to the glob "go test*"
    - "Bash(go build*)"   # native mecatl glob
    - "Read"              # bare tool name = tool-wide
  ask:
    - "Bash(git push:*)"
  deny:
    - "Bash(rm:*)"        # deny wins absolutely, in any scope
```

Each entry is a rule spec `Tool(pattern)` or bare `Tool`. Patterns use the
evaluator's glob grammar; the Claude `prefix:*` / `prefix:` form is normalised to a
`prefix*` glob. Config rules use **glob** semantics (`Exact:false`) — only LEARNED
"allow always" rules are exact.

**Scope → location** (highest precedence first; see `engine/governance` Scope):

| Scope | Location | Trust |
| --- | --- | --- |
| `ScopeCLI` | each `--permission-config <file>` | fully trusted |
| `ScopeLocalProject` | `<workspace>/.mecatl/settings.local.yaml` (gitignored, personal); `<workspace>/.claude/settings.local.json` with `--import-claude-permissions` | **trust-gated** |
| `ScopeSharedProject` | `<workspace>/.mecatl/settings.yaml` (checked-in, shared); `<workspace>/.claude/settings.json` with `--import-claude-permissions` | **trust-gated** |
| `ScopeUser` | `$XDG_CONFIG_HOME/mecatl/settings.yaml` (or `~/.config/...`); `~/.claude/settings.json` with `--import-claude-permissions` | fully trusted |
| `ScopeBuiltinDefault` | the built-in floor (read-allow / mutate-ask) | n/a — lowest precedence |

The resolver re-reads project files **per session** against the session's workspace
root, and **revalidates** its per-root cache on the config files' mtime/size — so a
`deny` added mid-process takes effect on the next call, not at restart. Among
ask-vs-allow the configured **higher scope wins**, with ONE narrow exception: a
higher-scope config **Allow loosens ONLY the built-in `ScopeBuiltinDefault` Ask**
floor (e.g. allowing `Bash(go test:*)` relaxes the built-in Bash ask). It can never
suppress a **configured** Ask, **deny/ask in any scope still beats an allow**, and a
learned allow can never out-rank a configured deny/ask. Plan mode still hard-denies
mutations first. Config files are size- and rule-count-capped (defense-in-depth).

**The trust gate** — a project's config is part of the repo the model is editing.
Its **DENY and ASK** rules are **always** honoured (they only tighten). Its
**ALLOW** rules (shared AND local) are honoured **only with `--trust-project`**;
otherwise they are dropped (and logged) so a checked-in `settings.yaml` cannot
auto-approve tool calls in an untrusted repo. User-global and `--permission-config`
(CLI) files are the operator's own and are always fully trusted.

**Memory + soul are pre-approved at the floor** (issue #14) — the six memory tools
(`Remember`/`Recall`/`SearchMemory` and the cross-project `RememberUser`/`RecallUser`/
`SearchUserModel`) and the synthetic `soul:apply` action are explicit
`ScopeBuiltinDefault` Allows in the built-in ruleset, so by default they **do not
prompt**: an agent recalling and saving its own facts, and applying the operator's
soul, is part of "having a memory/identity", not a workspace mutation. They are
explicit (auditable in source + the `... ENABLED ...; permission: allow (built-in
default, overridable ...)` startup logs) and **overridable** — being the lowest scope,
any higher-scope config Ask/Deny wins. To require approval (or block) one, add it to a
`settings.yaml`:

```yaml
permissions:
  ask:
    - "Remember"     # require approval before the agent writes project memory
  deny:
    - "soul:apply"   # withhold the soul entirely this deployment
```

`soul:apply` is consulted at **soul-load (build time)**, not per tool call: `allow`
applies the soul, `deny` withholds it, and `ask` also **withholds** it (with a warning)
because there is no interactive gate at build time — set it back to `allow` to apply.

**Claude import is lossy** (`--import-claude-permissions`) — every lossy outcome is
logged:

| Claude spec | Outcome |
| --- | --- |
| `WebFetch(domain:x)` in an **allow** list | **demoted to `ask`** (domain/substring match is too risky to auto-allow) |
| `Read(~/...)` (leading `~`) | kept but **inert** — the `~` is left unexpanded, so it never matches the absolute path a tool resolves |
| unparseable spec | **dropped** |

The import never widens: a demotion only ever moves `allow → ask`, and the
`deny`/`ask` buckets import verbatim.

> **`mecatui` defaults match `mecated`.** The embedded TUI server sets
> `--permissions-conventional` and `--import-claude-permissions` ON, but
> `--trust-project` is **OFF by default** (WORKSPACE-TRUST Phase 0) — unified with
> `mecated`. So a project's ALLOW rules and its project soul are honoured only when
> you pass `--trust-project` to `mecatui`; deny/ask are always honoured regardless.
> (Earlier builds hardcoded trust ON for the TUI; that blanket-trust regression is
> gone.) The `mecated` daemon likewise defaults `--permissions-conventional` ON but
> `--trust-project` OFF (the safe stance); it defaults `--import-claude-permissions`
> OFF (the safe network stance).

### Declarative workspace trust (`trustedWorkspaces:`, WORKSPACE-TRUST Phase 1)

`--trust-project` is a **per-invocation** flag. For CI, a daemon, or a power-user
who works repeatedly in a known-good checkout, declaring the trust once is more
ergonomic than passing the flag every run. The **user-global** `settings.yaml`
(`$XDG_CONFIG_HOME/mecatl/settings.yaml`, or `~/.config/mecatl/settings.yaml`)
gains an optional top-level `trustedWorkspaces:` list of **absolute workspace
paths** to pre-trust:

```yaml
# ~/.config/mecatl/settings.yaml  (the operator's own, fully-trusted file)
trustedWorkspaces:
  - /home/me/src/my-project
  - /home/me/work/known-good-repo

permissions:            # the same file also carries user-scoped permission rules
  deny:
    - "Bash(curl:*)"
```

When the current workspace's path matches a declared entry, it is trusted exactly
as `--trust-project` would trust it — **the same admission gate, no separate
path**: the project's ALLOW rules and its project soul are honoured. This is
**read-only**: mecatl only ever *reads* `trustedWorkspaces:` from your
human-authored `settings.yaml`; it never writes it (the machine-written trust
registry is a **separate** file — see the next section). Both `mecated` and
`mecatui` honour it (it lives in the shared user config).

**Precedence and semantics:**

- The effective trust is `--trust-project` **OR** a `trustedWorkspaces` match.
  The flag wins as the *source label* when both apply; either way the workspace
  is trusted.
- **Path keying is symlink-safe.** Both the declared entries and the current
  workspace are compared on their **cleaned, absolute, symlink-resolved**
  (`realpath`) form, so a moved or symlinked path cannot forge or inherit another
  workspace's trust. A declared entry that does not resolve (typo / broken
  symlink) is ignored; the rest of the list is honoured.
- **Monotonic-positive.** `trustedWorkspaces` only ever **grants** trust. It can
  never override a **Deny** or a configured **Ask** anywhere — those tighten and
  are always honoured. Trust gates only whether a project's *ALLOW* rules (and
  its soul) are admitted, never the deny-dominant evaluation.
- **Fail-safe.** A missing key, a malformed entry, or an unparseable
  `settings.yaml` resolves to **untrusted** (a corrupt config never *grants*
  trust); it is logged, never an error that aborts startup. The composition logs
  the decision: `workspace trust trusted=… source=flag|declared|none`.

### Remembered trust + drift (`trust.yaml`, WORKSPACE-TRUST Phase 2b)

Beyond the human-authored `trustedWorkspaces:` list, mecatl keeps a
**machine-written** trust registry at
`$XDG_CONFIG_HOME/mecatl/trust.yaml` (fallback `~/.config/mecatl/trust.yaml`).
It is a **sibling of, but never inside,** the human `settings.yaml` (the
settings-vs-state split): you edit `settings.yaml`; only the harness writes
`trust.yaml`. Each entry remembers a trusted workspace (keyed by its
`realpath`) **and** the **identity-anchor hash** captured at the moment of
trust:

```yaml
# ~/.config/mecatl/trust.yaml   (machine-written; do not hand-edit)
version: 1
workspaces:
  /home/me/src/my-project:
    anchorSHA256: 9f2c…           # the project's identity surface at trust time
    trustedAt: 2026-06-04T12:00:00Z
```

A remembered entry is honoured as `source=remembered` (precedence **below**
`--trust-project` and a `trustedWorkspaces:` match) **only while its anchor still
matches** the workspace's live identity surface.

- **The identity anchor** is the high-signal, rarely-edited project authority
  surface: the project **soul** (`<ws>/.mecatl/soul.md`), and the **project-tier**
  **agent**, **slash-command**, and **skill** definitions under `<ws>/.mecatl/*`
  and `<ws>/.claude/*`. It is hashed deterministically (sorted file set, per-file
  content hashes folded).
- **`settings.yaml` is NOT in the anchor.** Editing your project's permission
  rules (which change on nearly every commit) does **not** trigger drift — that
  would nag-fatigue you into blind-clicking trust. Permission edits re-resolve
  live (permconfig's own mtime cache) without re-prompting. **Drift fires only
  when the project's persona / agents / commands / skills change.**
- **Drift fails safe.** If a remembered workspace's anchor no longer matches
  (the project's identity surface changed since you trusted it), the workspace is
  re-gated to **untrusted** for this run, logged at `WARN`
  (`workspace trust: identity anchor DRIFTED …`). `mecated` has no prompt, so a
  drifted entry never silently inherits the old grant; the interactive re-prompt
  that turns drift back into a fresh trust decision is the `mecatui` first-encounter
  prompt (see the next section). `--trust-project` always overrides (it
  short-circuits before the registry is even read).
- **`mecated` is read-only on the registry** — it *reads* `trust.yaml`
  declaratively (a remembered + undrifted workspace is trusted) but **never
  prompts and never writes** it. A repo trusted in `mecatui` for a given user is
  honoured by that same user's `mecated` (shared `$XDG_CONFIG_HOME`); cross-user
  is not shared (use that user's `trustedWorkspaces:`).
- **Security.** The registry is keyed by `realpath` (symlink-safe, symmetric on
  read and write); the write uses `O_NOFOLLOW` + `0o600` + temp-then-rename (a
  pre-planted symlink at the path is refused); and an unreadable / oversized /
  corrupt / wrong-version `trust.yaml` resolves to **untrusted** (a corrupt
  registry never *grants* trust). The path is derived solely from your user XDG
  config dir, never from a repo-controlled path — a repo cannot self-trust.

### The `mecatui` first-encounter trust prompt (WORKSPACE-TRUST Phase 2c)

`mecated` is purely declarative — it never asks. But when you launch **`mecatui`**
with its **embedded** server (the default: no `--server`, and no `mecated` already
running on the loopback default) in a workspace that is **not yet trusted** and
that carries a **project authority set** worth gating, `mecatui` prompts you once,
**before** the TUI takes over the screen:

```
mecatui: do you trust the project files in this workspace?
  /home/me/src/some-cloned-repo
Trusting honours this project's soul, agents, commands, skills, and ALLOW rules. Its deny/ask rules apply regardless.
[t]rust (persist) / [o]nce (this run only) / [n]o (default):
```

- **`t` (trust)** — trust this run **and remember it**: writes the workspace +
  its current identity-anchor hash to `trust.yaml`, so future launches (and your
  own `mecated`) trust it without asking, until the project's identity surface
  drifts.
- **`o` (once)** — trust **this run only**; nothing is persisted. Next launch asks
  again.
- **`n` / Enter / anything else** — **do not trust** (the safe default): the
  project's soul, agents, commands, skills, and ALLOW rules are withheld; the agent
  still runs with your user-tier config and the built-in tools.

**When the prompt fires.** Only when there is something a trust grant would
actually admit: a project soul (`<ws>/.mecatl/soul.md`), a project-tier
agent/command/skill definition, or a project `settings.yaml`/`settings.local.yaml`
carrying **ALLOW** rules. A repo with only deny/ask rules (which apply regardless)
or no project authority at all is **never** prompted — you are not nagged for a
workspace that has nothing to gate. A workspace already trusted (via
`--trust-project`, a `trustedWorkspaces:` match, or a remembered + undrifted
`trust.yaml` entry) is **not** prompted either.

**Drift is a re-prompt.** If you previously trusted a workspace and its identity
surface (soul / agents / commands / skills) has since **changed**, the prompt
re-fires with a "this workspace **CHANGED** since you trusted it" notice —
answering `t` re-persists the new anchor.

**Non-interactive = untrusted (fail-safe).** If `mecatui`'s stdin is **not a
terminal** (piped, redirected, headless), it **cannot** prompt — so it proceeds
**untrusted** for that run and prints a one-line note. It never blocks startup
waiting for input and never auto-trusts off a pipe. To trust non-interactively,
pass `--trust-project` or declare the workspace in `trustedWorkspaces:`.

**Security.** The echoed workspace path is **terminal-escape-sanitized** before
display, so a repo directory named with embedded ANSI/OSC escapes cannot corrupt
or spoof the prompt (CWE-150). The prompt and the registry write live in the
`mecatui` composition root, not the render layer.

### What an untrusted workspace withholds (WORKSPACE-TRUST Phase 2a)

Trust is **not** a kill-switch. An untrusted repo is still a fully usable coding
agent — it degrades to **"ask the human" mode**, never **"do nothing" mode**. The
line is drawn between the agent's **own capability** (never gated) and the repo's
**injected steering/authority** (gated).

**Always active on ANY repo — trusted or not (NEVER gated):**

- the built-in tools (Read, Edit, Bash, …) and the whole agent loop;
- the base system prompt;
- **your own user-tier config**: the user soul, and your user-level agent
  definitions, slash commands, and skills under `$XDG_CONFIG_HOME/mecatl/*` (or
  `~/.config/mecatl/*`) and `~/.claude/*`. A repo cannot touch these;
- **every Deny / Ask rule** from any scope (they only tighten);
- the permission prompt itself — on an untrusted repo the agent still runs; it
  just **asks** for the tool calls the repo would have auto-allowed.

**Withheld when the workspace is UNTRUSTED — the project-injected authority set:**

- the project's permission **ALLOW** rules (auto-approval the repo grants itself);
- the project **soul** (`<workspace>/.mecatl/soul.md` — a repo rewriting the
  agent's persona);
- the **project tier** of **agent definitions** (`<workspace>/.mecatl/agents`,
  `<workspace>/.claude/agents`), **slash commands** (`<workspace>/.mecatl/commands`,
  `<workspace>/.claude/commands`), and **skills** (`<workspace>/.mecatl/skills`,
  `<workspace>/.claude/skills`).

So a freshly-cloned, untrusted repo cannot silently steer the model with a
`.claude/agents/evil.md`, a malicious slash command, an injected skill, a
self-granted auto-approve, or a repo persona — but you can still read, edit, and
run-with-a-prompt in it from the first run. An explicit `--commands-dir`,
`--agents-dir`, or `--skills-dir` you pass is **operator-supplied** (not
repo-injected) and is honoured regardless of trust. Each withheld project-tier
source is logged at `WARN` so the degradation is visible. Trust the repo
(`--trust-project` or a `trustedWorkspaces:` entry) to admit its full project
authority set.

> The soul carries a **double gate**: it is admitted only if the workspace is
> trusted (provenance) **AND** the `soul:apply` permission resolves to Allow
> (policy) — a logical AND. An untrusted repo's soul is withheld irrespective of
> `soul:apply`; a trusted repo's soul still obeys an explicit `soul:apply: deny`.

### The allow-all posture (`--yolo`)

For unattended runs (CI, a throwaway container, a disposable VM) you can suppress
the permission prompts for the **built-in mutate-ask floor** with the operator
flag `--yolo`. It is available on
`mecated` and on the embedded `mecatui` server (it is **ignored when `mecatui`
dials an external `--server`** — that server owns its own posture).

It is **not** a `PermissionMode` and **not** an evaluator bypass. It injects a
single `ScopeCLI` allow-all **rule** into the main engine's static ruleset, which
loosens **only** the built-in `Bash`/`Edit`/`Write`/`Team`/`SkillDraft` Ask floor.
The governance invariants are unchanged:

- A `Deny` in **any** scope (including `ScopeManaged`) still wins — deny-dominance is
  absolute. An admin can forbid specific tools/patterns even under allow-all.
- Any **deliberately configured** `Ask` (managed/project/user) still asks. Allow-all
  never suppresses a configured Ask, so a misconfigured Ask can still **block an
  unattended run** — the startup warning says so. (The common CI case configures no
  asks beyond the built-in floor, so allow-all is fully unattended there.)

The allow-all rule also blankets the synthetic `soul:apply` floor (the soul is
applied without prompting under `--yolo`) — consistent and expected, since the soul
is already floor-Allow by default. A **configured** `Deny`/`Ask` on `soul:apply` (or
on any memory tool) still wins under `--yolo`, exactly like every other tool.

**Sandbox-first.** The flag bypasses the *prompt*, never a *sandbox*. The real
boundary for unattended agentic execution is OS-level isolation (container/microVM,
network-off-by-default, ephemeral filesystem) — enable allow-all **only where the
harness cannot cause durable harm**, and only on single-tenant daemons (the flag
makes *every* session on that daemon allow-all).

**Root refusal.** If allow-all is requested **and** the process runs as root
(`euid 0`) **and** neither `MECATL_SANDBOX=1` nor `IS_SANDBOX=1` is set, the process
**refuses to start** with a clear error: root + no prompts can modify anything on the
host, so the operator must affirm an isolated, disposable environment via the env var.

```sh
# CI / sandboxed container, offline mock, no prompts:
MECATL_SANDBOX=1 bin/mecated --mock --yolo
```

### The self-improving-skill loop (`SkillDraft` + `mecated skills promote`)

`--skills-draft-dir <quarantine>` enables a **writable** `SkillDraft` tool so the
agent can author a reusable skill from a procedure it just performed. This is the
*only* tool that produces skills, and it is bounded by a hard trust boundary:

- A drafted skill is **never active in the session that wrote it.** `SkillDraft`
  validates and sanitizes the (untrusted) candidate — name regex, an
  injection-marker scan on both the description and the body, size caps, atomic
  write — and persists it to the **quarantine** directory. The quarantine is
  **never** registered as a `Skill` catalog source, so its contents add **zero**
  tokens to any context.
- To make it active, an **operator** runs the gate (flags precede the `<name>`):

  ```sh
  mecated skills promote \
    --skills-draft-dir <quarantine> \
    --skills-dir <active-skills-dir> \
    <name>            # add --yes to skip the interactive review (scripted/CI use)
  ```

  Promotion **prints the full candidate and asks for confirmation**, then verifies
  `origin: model` provenance, re-runs structural validation and the injection scan,
  refuses to overwrite an existing skill, and moves the candidate (provenance
  frontmatter intact: `origin: model`, `drafted_at`) into the active tree. It takes
  effect on the **next** server start — the convention is *author in session N →
  operator promotes → active in N+1.*
- **The trust boundary is structural.** The quarantine dir must live **outside the
  workspace root** (fatal startup error otherwise): the model's `Write`/`Edit` are
  confined to the workspace, so they cannot reach an out-of-workspace quarantine —
  the `Drafter` is the only way a candidate gets there. It must also be **disjoint**
  from every `--skills-dir` (fatal on overlap). `SkillDraft` itself defaults to
  **ask**, and being a mutating tool it is unavailable in plan mode.
- **Residual to know:** absent the (deferred) OS sandbox, the `Bash` tool can write
  to any path, so the structural boundary covers `Write`/`Edit` only — `mecated`
  warns when `SkillDraft` and `Bash` run together. For a fully structural boundary,
  run shell-less (`--no-bash`) or under an OS sandbox, and place active `--skills-dir`
  trees outside the workspace too (a startup warning flags an in-workspace one).

When you promote, **read the body** — it is agent-authored, untrusted,
instruction-like text that becomes trusted on promotion. The automated injection
scan is a backstop, not a substitute for reading it.

### Persona / soul (`~/.config/mecatl/soul.md`, issue #14)

A **user-scoped, agent-read-only** persona fragment — the operator's "soul": who
the agent is, its style, the posture it should take. It is read from
`$XDG_CONFIG_HOME/mecatl/soul.md` (fallback `~/.config/mecatl/soul.md`), or from an
explicit path via `--soul-file`, and injected as a **turn-0 user message** (after
the cache-stable system prefix, before the memory index — identity before saved
facts), fenced in a `<soul>…</soul>` data block so the model treats it as persona
data rather than a new instruction stream.

It is **on by default** and costs nothing when absent — a missing file is fail-soft.
The whole load is fail-soft: a missing, empty, whitespace-only, oversized (> 20 KiB),
unreadable, or **prompt-injection-flagged** file degrades to **no fragment**, never an
error that aborts a run. Disable it entirely with `--no-soul`.

It is **read-only to the agent by construction**: no tool can write the soul, and the
loader has no write path. This is deliberate — a writable identity anchor is a
prompt-injection trap (a single poisoned write would rewrite "who the agent is" across
*every* future session). Bootstrap and edit it by hand, with a text editor. (See
`docs/design/SOUL-SPIKE.md` for the threat model and the Phase-2 learning loop.)

**Drift detection (issue #14, Phase 3).** The harness fingerprints the soul's content
(sha256 of the clean body) and records it in a **harness-owned sidecar** next to the
soul file: `<soul-path>.sha256` (e.g. `~/.config/mecatl/soul.md.sha256`, or
`PATH.sha256` for `--soul-file PATH`). On the first load with no sidecar it records the
current hash as the baseline (trust-on-first-use) and logs `soul: baseline established`.
On a later load whose hash differs it logs a **`WARN` drift alert** with both hashes and
**still loads** the soul — a hand-edit on your own box is expected, so drift is surfaced,
not blocked (the soul is fenced DATA, never a permission gate). To manage drift:

- `--approve-soul` — (re)write the baseline to the current hash, accepting your edit.
  Run it once after you intentionally change your soul to silence the warning.
- `--soul-strict` — refuse a **drifted** soul: contribute no fragment this run until you
  `--approve-soul` the change. Useful on a shared/locked-down box.

The hash is computed by the read-only loader; the baseline **write** lives only in the
composition layer, so the agent still cannot touch the soul *or* its baseline. Note this
is **detection only** — there is no automatic restore-to-baseline (that would require a
harness-held copy of the approved bytes; deferred as a future opt-in). Delete the
`.sha256` sidecar to reset to trust-on-first-use.

**Project-sourced soul + trust gate (issue #14, Phase 3, Item 2).** Besides the
user-scoped soul above, the harness can also discover a **project soul** at
`<workspace>/.mecatl/soul.md` — a persona checked into the repo (parallel to
`.mecatl/settings.yaml`). Because it comes from a repo rather than your own config, it
is **untrusted by default**: it contributes **no fragment** unless you pass
`--trust-project` — the **same** flag that gates a project's permission ALLOW rules (no
separate soul-trust knob). An untrusted project soul is dropped with a `WARN` log, never
an error. Precedence is **USER-WINS** (a single identity anchor, not a merge):

- A **user-scoped** soul present (`<xdg>/mecatl/soul.md` or `--soul-file`) → it is used,
  and the project soul is **ignored** — even with `--trust-project`.
- **No** user soul **and** `--trust-project` set → the project soul loads (through the
  same byte-cap / injection-scan / fence / drift discipline as the user soul).
- **No** user soul and `--trust-project` **unset** → nothing (the project soul is
  dropped + logged).

Your **user-scoped soul is never trust-gated** — it always loads if present, regardless
of `--trust-project`. (Note: the embedded TUI server defaults `--trust-project` OFF,
unified with `mecated` (WORKSPACE-TRUST Phase 0), so a project `.mecatl/soul.md` is
honoured only when you pass `--trust-project` to `mecatui`.)

### User model (`~/.config/mecatl/usermodel`, issue #14 Phase 2)

A **user-scoped, cross-project** model of durable **FACTS about the operator** — who
they are and how they like to work. Unlike the soul (read-only) and per-project memory
(`--memory-dir`), the user model is **writable by the agent** and **shared across every
project**, backed by a SECOND `memory` store at `$XDG_CONFIG_HOME/mecatl/usermodel`
(fallback `~/.config/mecatl/usermodel`), overridable with `--user-model-dir`.

It surfaces two ways:

- **Tools (on by default):** `RememberUser`, `RecallUser`, `SearchUserModel` — the
  user-model siblings of the per-project memory tools. Keys are auto-namespaced under
  `user/`. The model sees a turn-0 `<user-model>` block summarising the saved facts
  (injected LAST: soul → memory index → user model).
- **Background reviewer (off by default, `--user-model-review`):** after a session
  stops, a fresh single-shot child reads the transcript and extracts operator facts via
  RememberUser. It is debounced by `--user-model-review-interval` and **never reopens or
  re-runs the user's session** — it spawns a brand-new child. A
  `--user-model-consolidate-interval` points a `dream` consolidator at the `user/`
  namespace.

**Rules vs facts — the operator boundary.** The user model holds **FACTS about the
operator** (stated preferences, communication style, domain background), **never rules
or behavioural instructions for the agent**. How the agent behaves comes from its soul
and the system rules; the `<user-model>` block is fenced **DATA** the model treats as
facts, not a new instruction stream, and the tool descriptions forbid storing rules or
anything the workspace already knows. The RememberUser write path injection-scans both
the value AND the effective description (reusing `skills.ScanForInjection`) — the
`<user-model>` block renders the key + description, so scanning only the value would
miss a payload hidden in `description` — and additionally rejects any field containing
the data-fence close-tag `</user-model>` (mirroring soul's reject-on-close-tag), so a
poisoned transcript cannot launder steering into the block or break its data fence. The user model is an instruction
**fragment**, not a governance scope — it can never loosen a configured permission Ask.
Over-eager memory is *steered* (by the descriptions), not *enforced* (there is no
rule/fact classifier); this is a deliberate, accepted residual risk. Single-operator
assumption: there is no per-user keying — "the operator" is implicitly singular, the
same trust-zone assumption the soul and `docs/design/MEMORY-TIERING.md` carry. Disable
it with `--no-user-model`.

### Graceful shutdown

`mecated` traps `SIGINT` / `SIGTERM`, stops accepting new work, drains the HTTP
server (bounded by a 10 s timeout) and `GracefulStop`s the gRPC server:

```
level=INFO msg="shutdown signal received; stopping servers"
```

---

## 4. The gRPC API

Service: `mecatl.v1.HarnessService` (`contracts/proto/mecatl/v1/harness.proto`).

| RPC | Kind | Purpose |
| --- | --- | --- |
| `CreateSession(CreateSessionRequest) → CreateSessionResponse` | unary | allocate a server-side session, return its id |
| `GetSession(GetSessionRequest) → GetSessionResponse` | unary | snapshot of an existing session |
| `Converse(stream ConverseRequest) → stream ConverseResponse` | bidi | drive one agent run |

### The `Converse` flow

The bidi stream drives exactly one run:

1. The client sends the **mandatory first frame**, a `Prompt{session_id, text}`.
   (A first frame that is not a prompt → `InvalidArgument`.)
2. The server streams `ConverseResponse{Event}` envelopes in sequence order.
3. On a `permission.ask` event, the client sends a control frame
   `ResumeApproval{ask_id, allow}` — `ask_id` echoes `Event.ask.ask_id`.
4. The client may send `Cancel{}` at any time to abort; the run terminates with
   a `result` whose `stop = "cancelled"`.
5. The server emits a terminal `result` event and closes the stream.

`ConverseRequest` is a `oneof`:

| Field | When |
| --- | --- |
| `prompt` (`Prompt{session_id, text}`) | mandatory first frame |
| `resume_approval` (`ResumeApproval{ask_id, allow}`) | resolve a paused ask |
| `cancel` (`Cancel{}`) | abort the in-flight run |

A second `prompt`, or any unknown control frame, is ignored — a single
`Converse` stream drives a single run.

### Event envelope

Every event is the provider-neutral `Event` message (mirrors the domain
`session.Event` one-for-one — never an OpenAI type):

```protobuf
message Event {
  string type = 1;          // session.init | turn.start | message.delta |
                            // tool.call | tool.result | permission.ask |
                            // hook | compaction | result
  int64  seq  = 2;          // monotonic per run
  int32  turn = 3;
  string text = 4;          // streamed/final text where applicable
  ToolCall      tool_call   = 5;
  ToolResult    tool_result = 6;
  PermissionAsk ask         = 7;   // ask_id echoed in ResumeApproval
  Result        result      = 8;   // terminal event
  Usage         usage       = 9;
}
```

`Result.stop` is one of: `end_turn`, `max_turns`, `max_tool_calls`,
`max_consecutive_failures`, `budget` (the `--max-run-tokens` ceiling crossed —
a clean, reopen-able terminal), `no_progress`, `cancelled`, `error`.

### Go client snippet

`mecated` does **not** register gRPC server reflection, so `grpcurl` must be
pointed at the proto (and its `buf.validate` import) explicitly. A generated Go
client is the simplest path:

```go
package main

import (
	"context"
	"io"
	"log"

	"google.golang.org/grpc"
	"google.golang.org/grpc/credentials/insecure"

	mecatlv1 "github.com/stacklok/mecatl/contracts/gen/go/mecatl/v1"
)

func main() {
	conn, err := grpc.NewClient("127.0.0.1:8080",
		grpc.WithTransportCredentials(insecure.NewCredentials()))
	if err != nil {
		log.Fatal(err)
	}
	defer conn.Close()
	client := mecatlv1.NewHarnessServiceClient(conn)

	// 1. Create a session.
	cs, err := client.CreateSession(context.Background(), &mecatlv1.CreateSessionRequest{
		Workspace: "/path/to/workspace",
		Mode:      mecatlv1.PermissionMode_PERMISSION_MODE_DEFAULT,
	})
	if err != nil {
		log.Fatal(err)
	}

	// 2. Open the Converse stream and send the mandatory first Prompt.
	stream, err := client.Converse(context.Background())
	if err != nil {
		log.Fatal(err)
	}
	if err := stream.Send(&mecatlv1.ConverseRequest{
		Kind: &mecatlv1.ConverseRequest_Prompt{
			Prompt: &mecatlv1.Prompt{SessionId: cs.GetSessionId(), Text: "List the Go files."},
		},
	}); err != nil {
		log.Fatal(err)
	}

	// 3. Relay events; approve any permission.ask.
	for {
		resp, err := stream.Recv()
		if err == io.EOF {
			return // terminal result delivered, stream closed
		}
		if err != nil {
			log.Fatal(err)
		}
		ev := resp.GetEvent()
		log.Printf("[%d] %s %s", ev.GetSeq(), ev.GetType(), ev.GetText())

		if ev.GetType() == "permission.ask" {
			_ = stream.Send(&mecatlv1.ConverseRequest{
				Kind: &mecatlv1.ConverseRequest_ResumeApproval{
					ResumeApproval: &mecatlv1.ResumeApproval{
						AskId: ev.GetAsk().GetAskId(),
						Allow: true,
					},
				},
			})
		}
		// To abort instead, send a Cancel{} frame:
		//   stream.Send(&mecatlv1.ConverseRequest{Kind: &mecatlv1.ConverseRequest_Cancel{Cancel: &mecatlv1.Cancel{}}})
	}
}
```

`GetSession` returns a snapshot (`session_id`, `state`, `mode`, `workspace`,
`limits`, `turns`, `tool_calls`, `created_at_unix`).

### The terminal UI (`mecatui`)

`mecatui` is an optional, flashy terminal UI that drives a `mecated` over this
same gRPC `Converse` stream. After `task build` it lands at `bin/mecatui`. It
needs no separate server by default — with no `--server` it reuses a `mecated`
already running on `127.0.0.1:8080`, or else **hosts one in-process** over a UNIX
socket (built via `internal/app`, the same assembly `mecated` uses):

```sh
OPENAI_API_KEY=sk-... bin/mecatui --workspace "$PWD"   # embedded (default)
bin/mecatui --mock --workspace "$PWD"                  # embedded, offline mock

bin/mecated &                                          # …or an external server
bin/mecatui --server 127.0.0.1:8080 --workspace "$PWD"
```

The embedded server keeps the heavier opt-ins (MCP, ToolHive, skills, memory,
server-side slash-command expansion) off; run a full `mecated` and use `--server`
for those. It also accepts **`--perf`** (off by default) to bring up the same
loopback observability surface `mecated` exposes — `/metrics`, `/debug/pprof/*`,
`/debug/vars`, `/debug/flightrecorder` — on a **fixed** `127.0.0.1:9099` port by
default (predictable, so an MCP-client config can hardcode the `/mcp` URL once;
distinct from `mecated`'s `:9090`). Pass `--perf-addr host:port` to move it, or
`--perf-addr 127.0.0.1:0` for an ephemeral port. On a port clash, startup **fails
with guidance** rather than silently falling back (`--perf-goroutine-warn-threshold`
arms the goroutine alarm). The chosen address is logged at startup (loopback, unauthenticated —
same posture as `mecated`'s admin listener; see the observability note in §3).
With `--perf` it also accepts **`--perf-mcp`** to mount the read-only perf MCP
server at `/mcp` on that admin surface (same fail-closed loopback enforcement: a
non-loopback `--perf-addr` with `--perf-mcp` is refused). This is the in-process
way to profile a freeze in the embedded server itself.
The embedded server also accepts `--yolo` (the
allow-all operator posture — same semantics, root refusal, and `MECATL_SANDBOX`/
`IS_SANDBOX` env as `mecated`; see the allow-all note in §7). It is **ignored when
dialling an external `--server`**. Note the TUI's **built-in slash commands** (`/clear`, `/help`, and the
caps-gated `/mcp`/`/agents`) still work regardless — they act on the TUI itself,
not the server, so typing `/` always opens a useful palette even with workspace
slash-command expansion off (`/agents` browses the agent-definition inventory;
`/team`, also `ctrl+a`, opens the live agent-team overlay). See `docs/tui.md` for
all flags.

It streams the conversation (glamour markdown for assistant text, themed cards
for tool I/O), shows a thinking spinner and a usage footer, and pops an inline
modal for permission asks that you approve/deny without leaving the stream. It is
themeable (Aztec default, plus `mono`/`solar`, plus drop-in JSON themes) and
respects the same trust model: it refuses to send `--auth-token` in cleartext to
a non-loopback server (use `--tls`). Full flag, key, and theming reference is in
**`docs/tui.md`**.

---

## 5. The HTTP / SSE API

The HTTP adapter wraps the same service. Every event is emitted as one SSE
`data:` line carrying the proto `Event` marshalled to JSON — so HTTP and gRPC
share one event shape.

| Method & path | Body | Response |
| --- | --- | --- |
| `POST /v1/sessions` | `{workspace, mode?, limits?}` | `201` `{session_id}` |
| `GET /v1/sessions/{id}` | — | `200` session snapshot |
| `POST /v1/sessions/{id}/prompt` | `{text}` | `200` `text/event-stream` of events |
| `POST /v1/sessions/{id}/approve` | `{ask_id, allow}` | `204` |
| `POST /v1/sessions/{id}/cancel` | — | `204` |

All examples below were captured against a live `mecated --mock`.

### Create a session

```console
$ curl -s -X POST http://127.0.0.1:8081/v1/sessions \
       -d '{"workspace":"/tmp/mecatlws"}'
{"session_id":"8867bdea940108c1dd82d13d3fb7fc61"}
```

Optional fields:

```json
{
  "workspace": "/tmp/mecatlws",
  "mode": "plan",
  "limits": { "max_turns": 20, "max_tool_calls": 80, "max_consecutive_failures": 3 }
}
```

`mode` accepts `default`, `plan`, `acceptedits` (also `accept_edits` / `accept`);
unknown/empty falls back to the server default (`default`). A `limits` object
with all-zero (or omitted) fields gets the server's non-zero defaults
substituted (see §6). `workspace` is required — omitting it returns `400`
`{"error":"workspace is required"}`.

### Inspect a session

```console
$ curl -s http://127.0.0.1:8081/v1/sessions/8867bdea940108c1dd82d13d3fb7fc61
{"session_id":"8867bdea940108c1dd82d13d3fb7fc61","state":"idle","mode":"default","workspace":"/tmp/mecatlws","turns":0,"tool_calls":0}
```

A missing id returns `404` `{"error":"not found: \"...\""}`.

### Start a run (SSE stream)

```console
$ curl -s -N -X POST http://127.0.0.1:8081/v1/sessions/8867bdea940108c1dd82d13d3fb7fc61/prompt \
       -d '{"text":"hello"}'
data: {"type":"turn.start","seq":1}

data: {"type":"message.delta","seq":2,"text":"Mock provider: no real model is configured. Set --openai/OPENAI_API_KEY for live use."}

data: {"type":"result","seq":3,"result":{"stop":"end_turn","text":"Mock provider: no real model is configured. Set --openai/OPENAI_API_KEY for live use.","usage":{}},"usage":{}}
```

> `-N` disables curl's buffering so you see events as they stream. The example
> above is the `--mock` provider (one text turn). Against a real model you also
> see `tool.call`, `tool.result`, and — for tools that need approval —
> `permission.ask`. The JSON field names follow the proto JSON shape:
> `tool_call`, `tool_result`, `ask` (`{ask_id, tool, args, reason}`),
> `is_error`, `call_id`.

Disconnecting the client (closing the curl connection) cancels the run.
`text` is required — omitting it returns `400` `{"error":"text is required"}`.

### Approve / deny a pending ask

When the stream emits a `permission.ask` with an `ask.ask_id`, resolve it on a
**second** connection while the SSE stream is still open:

```console
$ curl -s -X POST http://127.0.0.1:8081/v1/sessions/<id>/approve \
       -d '{"ask_id":"<ask_id-from-the-event>","allow":true}'
# 204 No Content
```

Set `"allow":false` to deny (the model receives the denial reason and adapts).
If there is no in-flight run for the session you get `404`
`{"error":"no in-flight run for session"}`.

### Cancel a run

```console
$ curl -s -X POST http://127.0.0.1:8081/v1/sessions/<id>/cancel
# 204 No Content
```

The run terminates with a `result` whose `stop` is `cancelled`. No in-flight run
→ `404` `{"error":"no in-flight run for session"}`.

---

## 6. Configuration

### Workspace

`--workspace` (server-wide default) and the per-session `workspace` field set
the root all file/command tools operate against. The server builds an `osfs`
workspace rooted there. A root that cannot be opened yields a nil workspace;
tool calls then return readable errors the model can act on.

### Model

`--model` (default `gpt-5`) is the identifier sent to the provider and stamped
into the system-prompt env. Pass **strings** for forward-compatibility and for
compatible endpoints.

### Session store

| `--store-dir` | Store | Behaviour |
| --- | --- | --- |
| empty (default) | in-memory (`memstore`) | nothing persists across restarts |
| set to a dir | JSONL replay (`jsonlstore`) | snapshots + tool-call log on disk |

The JSONL store writes two files per session under `--store-dir`:

```
<dir>/<id>.session.jsonl   # one snapshot per Save (latest line wins)
<dir>/<id>.tools.jsonl     # one record per tool call (call, result, duration)
```

### Remote store drivers

A third option points the session store (and/or the memory store) at a
**remote driver process** speaking the `mecatl.driver.v1` gRPC protocol:

```sh
mecated --session-store-url 127.0.0.1:7443 --memory-store-url 127.0.0.1:7443
```

`--session-store-url` is mutually exclusive with `--store-dir` (and
`--memory-store-url` with `--memory-dir`) — a fatal startup error, never a
silent precedence. Equal URLs share one connection. The driver only ever sees
**opaque snapshots** (the `sessnap` encoding under a `"sessnap-json/1"` format
tag); it sits at the same trust tier as the on-disk store directory. A
conforming driver must accept snapshot payloads up to **64 MiB** (mount the
gRPC server with a matching receive limit; the harness client is already
configured for it).

Transport posture: **only LOCAL targets may ride plaintext** — loopback hosts
and unix sockets (the single-user default). Any other driver target
**requires `--driver-tls`, token or not**: the client refuses cleartext
pre-dial, because a driver delivers session payloads, memories,
model-steering skill bodies, and executable skill assets — an on-path
attacker over a cleartext remote link would gain driver-equivalent
capability regardless of auth. `--driver-auth-token` adds per-RPC bearer
auth on top; `--driver-tls-ca` pins a custom CA;
`--driver-tls-cert`/`--driver-tls-key` add a client certificate for mTLS.
Setting any `--driver-tls-*` file **without** `--driver-tls` is a fatal
startup error (it would otherwise be silently ignored). There are **no
retries and no default deadline** on driver RPCs — a driver failure surfaces
as the same unit failure a disk error would.

### Remote content-source drivers (skills + soul)

The same protocol carries two **content sources**:

```sh
mecated --skill-source-url 127.0.0.1:7443 --soul-source-url 127.0.0.1:7443
```

`--skill-source-url` replaces local skills discovery entirely (mutually
exclusive with `--skills-dir`/`--skills-conventional`). The driver's skill
set is **snapshotted once at startup** (fatal if the driver cannot answer —
an explicitly configured source that is down is a misconfiguration, never a
silent no-skills run). Skills cross the wire as **logical bundles** — name,
description, body, and payloads addressed by slash-relative logical names
(`references/api.md`, `scripts/run.sh`) — no paths. On a skill's **first
activation** its payloads materialize into a temporary, build-scoped **asset
cache** (the `Base directory` the activation header advertises); a
never-activated skill transfers zero bytes. Materialization is capped
(16 MiB per file, 64 MiB per bundle), name-validated and containment-checked
(an invalid bundle fails that activation with a model-addressable error,
never a partial bundle), honors the executable bit, and the whole cache is
removed on shutdown. **Trust:** a driver-served `SKILL.md` steers the model
like AGENTS.md/CLAUDE.md — point this only at a driver you trust (the same
tier as `--skills-dir`).

`--soul-source-url` serves the persona from the driver instead of the local
user soul file, occupying the **user slot** of the selection precedence (it
shadows a project soul exactly like a present user soul; `--no-soul` and the
`soul:apply` permission gate still apply). The driver is **probed at
startup** (fatal if unreachable); a fault at run time degrades fail-soft to
no fragment with a logged warning. The body is **re-validated locally**
(byte cap, injection scan, data-fence integrity — a driver is never trusted
to sanitize). The **drift baseline is skipped** for driver souls — the
baseline is sidecar-file machinery for a local file you edit, while a driver
sits behind the operator's own auth — so `--soul-strict` and
`--approve-soul` are no-ops for this provenance (one INFO line records the
skip).

Both share the `--driver-auth-token`/`--driver-tls*` posture, and equal URLs
share one connection with the store drivers.

### Remote content-source drivers (agent definitions + slash commands)

Phase C2 completes the family with two more sources on the same protocol:

```sh
mecated --agent-source-url 127.0.0.1:7443 --command-source-url 127.0.0.1:7443
```

`--agent-source-url` serves the **agent definitions** (the Subagent
specialists / team-member roles) from the driver. Like skills, the set is
**snapshotted once at startup** (fatal if the driver cannot answer — per-def
child engines are built once at build time, so there is no re-fetch). It is
mutually exclusive with explicit `--agents-dir`; the default-on
`--agents-conventional` discovery is simply **superseded** (an INFO line
narrates it — failing every default deployment over an ON-by-default,
usually-inert flag would be wrong; this asymmetry vs the opt-in skills
conventional discovery is deliberate). Defs cross the wire whole — tools,
limits, model/provider hints, skills, hooks, scoped MCP servers — with **no
path**: diagnostics identify a driver def as `driver: <target>`. Inline MCP
server **headers are secret-shaped** (e.g. `Authorization`): the harness
never logs or projects them; they ride this wire only because driver dials
refuse all non-local cleartext. The driver's claimed origin tier is ignored —
every driver-served def is stamped `driver`. **Trust:** this is STRONGER than
model steering — a def's `hooks:` map executes as **ungated shell on the
harness host** (`hookexec`, every scoped lifecycle phase, no permission ask),
strictly more capability than the skill driver, whose payloads still ride the
permission-gated Bash path. **A compromised agent-source driver executes
arbitrary shell on the harness host via def hooks; treat it as
harness-equivalent infrastructure.** The build narrates every driver def that
carries hooks (`agent def carries lifecycle hooks (harness-side shell)` —
names only, never hook values) so the capability is visible at startup.

`--command-source-url` serves **slash-command templates**. Unlike every
other source driver it **composes instead of replacing**: the expansion
order is file-backed commands → driver commands → MCP prompts
(first-match-wins), so a local `<name>.md` shadows a same-named driver
command, and the palette merges all three. It is also **live**, not a
snapshot — the driver is consulted on every expansion and palette listing,
matching the file expander's reads-current-files behaviour, so the command
set may change while the server runs. The driver returns the RAW template
(frontmatter allowed); the harness strips frontmatter and substitutes
`$ARGUMENTS`/`$1`/`$2`… exactly as for a file command, so templates are
portable between the two backends byte-for-byte. The driver is probed once
at startup (fatal if unreachable); a fault at run time **fails soft** — the
raw input passes through unchanged and the palette omits the source (a
transient blip never aborts a run and never latches a command "missing").

All four content-source drivers share the `--driver-auth-token`/
`--driver-tls*` posture, and equal URLs share one connection.

### Permission modes

Set per session via `CreateSession` `mode` (HTTP `mode` string / proto
`PermissionMode`):

| Mode | Proto enum | Posture |
| --- | --- | --- |
| `default` | `PERMISSION_MODE_DEFAULT` | standard deny → ask → allow |
| `plan` | `PERMISSION_MODE_PLAN` | read-only toolset; mutations hard-denied |
| `acceptedits` | `PERMISSION_MODE_ACCEPT_EDITS` | auto-accept edits |

`PERMISSION_MODE_UNSPECIFIED` (and any unknown string) defaults to `default`.

### Default limits

A **zero** `Limits` value disables every stop condition, so the composition root
injects non-zero defaults for any session created without explicit limits, so a
default session is always bounded:

| Limit | Default | Disables when 0 |
| --- | --- | --- |
| `max_turns` | `50` | yes |
| `max_tool_calls` | `200` | yes |
| `max_consecutive_failures` | `5` | yes |

Supplying **any** non-zero limit field is taken as explicit and used as-is.

---

## 7. Permissions

### How a decision resolves

Each tool call is evaluated against a merged set of `Rule`s. A `Rule` is
`{Scope, Tool, Pattern, Effect}` where `Effect` is `deny`, `ask`, or `allow`,
`Tool` empty matches any tool, and `Pattern` empty matches any args (otherwise a
shell-style glob, with an exact-match fast path, over the canonicalized
command/argument string).

Resolution precedence:

1. **`deny` → `ask` → `allow`**: a `deny` in *any* scope beats an `ask` or
   `allow` anywhere; otherwise an `ask` beats an `allow`.
2. **Scope** breaks same-effect ties (highest precedence first):
   `Managed > CLI > LocalProject > SharedProject > User`.
3. **No matching rule → `ask`** — the safe default. The harness never silently
   allows an unconfigured call.

A `deny`/`ask` carries a human `reason`, surfaced to the model (on deny, so it
can adapt) and to the client (on ask).

### The default ruleset `mecated` ships

| Tool | Default effect |
| --- | --- |
| `Read`, `Grep`, `Glob`, `WebFetch`, `Subagent` | `allow` |
| `Bash`, `Edit`, `Write` | `ask` |

Read-only exploration runs without interruption; anything that can mutate the
workspace pauses for approval.

### Plan mode hard-denies mutations

When a session is in `plan` mode, the evaluator gates *before* the rule engine:

- `Edit` and `Write` are unconditionally **denied** (they always mutate).
- A `Bash` command that is **not** read-only is **denied**; read-only Bash and
  the read-only tools (`Read`/`Grep`/`Glob`) fall through to the rules.

The deny reason tells the model to present a plan and exit plan mode first.

### Compound-Bash & substitution safety

For `Bash`, the evaluator splits compound command lines and requires **every**
sub-command to pass; the **worst** outcome wins. So `git status && rm -rf /`
inherits the deny/ask from the `rm` segment even if `git status` would be
allowed. Any segment containing command/process substitution or subshell
grouping (which could smuggle a hidden inner command past the splitter) is
floored at **`ask`** — an allow rule for the outer literal can never silently
approve a concealed destructive command.

> Permission rules are configured in Go in the shared composition layer
> (`internal/app`, `defaultRules()`), via `permpolicy.NewPolicy([]governance.Rule{…})`.
> There is no rules config file in v1; to change the shipped policy, edit
> `defaultRules()` and rebuild. (It lives in `internal/app` so both `mecated` and
> the embedded `mecatui` server share one ruleset.)

---

## 8. Hooks

Lifecycle hooks let an external command observe or veto agent actions. A hook is
a phase → shell-command map; each command is run as `<shell> -c <command>`
(default shell `/bin/sh`), with the JSON `HookEvent` written to its **stdin**.

### Phases

| Phase | Fires |
| --- | --- |
| `SessionStart` | once when a session begins |
| `UserPromptSubmit` | when the user submits a prompt |
| `PreToolUse` | before a tool runs — a block aborts the call |
| `PostToolUse` | after a tool runs |
| `Stop` | when the main loop stops |
| `SubagentStop` | when a subagent loop stops |

> v1 fully implements `PreToolUse` and `PostToolUse`; the others are defined and
> wired as the injection seam. **`mecated` ships with no hooks configured by
> default** (`hookexec.New(nil)`), so every event is allowed. Hooks are
> configured in Go at the composition root by passing a populated
> `map[governance.HookPhase]string` to `hookexec.New(...)`.

### The stdin contract

The hook receives a JSON `HookEvent` on stdin:

```json
{
  "Phase": "PreToolUse",
  "Tool": "Bash",
  "Input": { "command": "rm -rf build" },
  "SessionID": "8867bdea940108c1dd82d13d3fb7fc61"
}
```

### The exit-code contract

| Exit code | Outcome |
| --- | --- |
| `0` | **allow** — the message (if any) is read from stdout |
| `2` | **block** — the action is vetoed; the reason is read from stdout (preferred) or stderr |
| any other | **error** — the hook itself failed; surfaced to the caller |

A single invocation is bounded by a timeout (default 30 s).

### Example hook script

A `PreToolUse` hook that blocks any `Bash` command containing `rm -rf`:

```sh
#!/bin/sh
# pretooluse-guard.sh — exit 2 to block, 0 to allow.
event="$(cat)"                       # the HookEvent JSON arrives on stdin
if printf '%s' "$event" | grep -q 'rm -rf'; then
  echo "blocked: 'rm -rf' is not permitted by policy"   # reason -> client/model
  exit 2
fi
exit 0
```

Wire it (in `cmd/mecated/main.go`, where `hookexec.New(nil)` is today):

```go
hooks := hookexec.New(map[governance.HookPhase]string{
    governance.PhasePreToolUse: "/path/to/pretooluse-guard.sh",
})
```

---

## 9. OpenAI & compatible endpoints

The OpenAI provider talks to the **Responses API** (`POST /v1/responses`) via
`github.com/openai/openai-go/v3`. The harness owns its own conversation state:
every request is stateless (`store:false`, no `previous_response_id`) and
resends the full input slice, carrying reasoning items forward.

| Setting | How |
| --- | --- |
| API key | `OPENAI_API_KEY` env var (selects `--openai` automatically) |
| Base URL | `--openai-base-url https://your-host/v1` (SDK appends `/responses`) |
| Model | `--model <id>` |

**OpenRouter** rides this same stateless Responses adapter: set `OPENROUTER_API_KEY`
and the `openrouter` provider is auto-detected against `https://openrouter.ai/api/v1`
(override with `--openrouter-base-url`). It accepts an `OPENAI_API_KEY` by convention
when no dedicated key is set.

```console
# OpenAI
$ OPENAI_API_KEY=sk-... go run ./cmd/mecated --openai --model gpt-5

# An OpenAI-compatible endpoint (vLLM / LiteLLM / local proxy)
$ OPENAI_API_KEY=token go run ./cmd/mecated --openai \
    --openai-base-url http://127.0.0.1:8000/v1 --model my-model
```

### What compatible servers may lack

`/v1/chat/completions` is broadly supported, but `/v1/responses` support is thin
and version-dependent (llama.cpp: none yet; vLLM: partial; LiteLLM proxies
translate). Per `docs/design/OPENAI-RESPONSES-API.md` §8, features **commonly
missing** on compatible servers include:

- `previous_response_id` / `store` (the harness already avoids these by design —
  it keeps state itself, so it is portable),
- hosted tools,
- automatic caching / `prompt_cache_key` (expect a lower or zero `cacheHitRate`),
- encrypted reasoning content and reasoning summaries,
- strict mode and `parallel_tool_calls` (these vary).

If a compatible endpoint behaves oddly, suspect missing Responses-API support
before suspecting the harness.

---

## 10. Troubleshooting / FAQ

**`no LLM provider available: set OPENAI_API_KEY or OPENROUTER_API_KEY (run with --openai/--mock for offline)`**
You started `mecated` with no provider key in the environment. Set
`OPENAI_API_KEY` or `OPENROUTER_API_KEY`, pass `--openai`, or pass `--mock` for an
offline smoke test.

**`--openai requires OPENAI_API_KEY to be set`** (the `cmd/mecademo` demo only)
The demo's `--openai` flag was passed but no key is in the environment.
`export OPENAI_API_KEY=…`. (`mecated`/`mecatui` instead auto-detect the provider
from the environment and emit the `no LLM provider available: …` message above when
no key resolves.)

**`bind: address already in use`**
Another `mecated` (or process) holds the port. Pick free ports with
`--http-addr` / `--grpc-addr`, or stop the other process.

**WARN: "API bound to a NON-loopback address …"**
You bound something other than `127.0.0.1` / `localhost`. The API is
unauthenticated and exposes command/file execution. Bind loopback, or put a real
trust boundary (auth/mTLS proxy, network policy) in front of it.

**Edit fails: "you must have read the file with the Read tool this session …"**
`Edit` enforces read-before-edit: the file must have been read this session and
be unchanged since. Have the model `Read` the file (again) and retry the edit.

**Tool call hangs / never completes**
It is probably a `permission.ask` awaiting approval. Watch the SSE/event stream
for `permission.ask` and resolve it with `POST …/approve` (or a `ResumeApproval`
frame over gRPC). Denied calls return the reason to the model.

**The run "won't stop" / loops**
It can't run unbounded: default limits cap it (`max_turns=50`,
`max_tool_calls=200`, `max_consecutive_failures=5`). The terminal `result.stop`
tells you which limit fired (`max_turns`, `max_tool_calls`,
`max_consecutive_failures`). Tighten them per session via `limits`.

**`404 no in-flight run for session`** on approve/cancel
There is no active run for that session id — the run already finished, was never
started, or you used the wrong id. Approve/cancel only work while the prompt's
SSE stream is open.

**Reading the JSONL replay log**
With `--store-dir DIR`, inspect a session after the fact:

```console
$ ls DIR
8867….session.jsonl   8867….tools.jsonl

# Latest session snapshot (last line wins):
$ tail -n1 DIR/8867….session.jsonl | jq .

# Every tool call with its result and duration:
$ jq . DIR/8867….tools.jsonl
```
