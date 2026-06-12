# mecatl live e2e suite

A **live**, ginkgo-driven end-to-end suite: it spawns `./bin/mecated` against
**OpenRouter** (real model calls, real money — fractions of a cent per run) and
drives full runs over the gRPC `Converse` stream using `cmd/mecatui/client` —
the exact client package mecatui is built on, so the TUI's wire path
(CreateSession → OpenConverse → event translation → approvals) is covered
transitively. There is no teatest-live lane in v1.

Everything here carries the `e2e` build tag: `task build` / `task test` /
`task lint` never compile it, and ginkgo/gomega never enter their build graphs.

## Running it

```sh
export OPENROUTER_API_KEY=$(cat ~/Development/api-keys/ozzrouter.io)  # the conventional key location; export in-shell, never on a logged command line
task e2e            # task build first, then: go test -tags e2e -count=1 -timeout 60m ./e2e/...
```

Timeouts are layered: per-run driver timeouts (cancel + drain → a TIMEOUT
classification in the failure report), per-spec ginkgo `SpecTimeout`s (driver
timeout + 30s; flake retries get a fresh one), and the outer
`go test -timeout 60m`. The per-spec budgets × attempts sum to ~50m worst-case,
so the clean failure path (transcript + AfterSuite teardown + cost ledger)
always fires before go test's panic path — see the arithmetic in
`e2e/suite_test.go`.

Environment knobs (all optional):

| Variable | Default | Meaning |
|---|---|---|
| `MECATL_E2E_TARGET` | (unset → spawn local) | `host:port` of an existing mecated; skips the local spawn |
| `MECATL_E2E_MODEL` | `anthropic/claude-3.5-haiku` | default-lane model for all tool scenarios (see "Prompt phrasing vs the upstream prompt filter" for why the OpenAI-family lane was demoted) |
| `MECATL_E2E_MODEL_SECONDARY` | `openai/gpt-4.1-mini` | second lane (single-turn smoke only); `skip` disables it |
| `MECATL_E2E_MAX_RUN_TOKENS` | `20000` | `--max-run-tokens` for the spawned server (a single full-catalog turn is ~5-6k input tokens, so a 4k budget trips at the first turn boundary) |
| `MECATL_E2E_MAX_TEAM_TOKENS` | `60000` | `--max-team-tokens` for the spawned server |
| `MECATL_E2E_WORKSPACE` | — | remote target only: absolute workspace root on the server host (required) |
| `MECATL_E2E_METRICS_URL` | — | remote target only: the `/metrics` URL (metrics spec Skips without it) |
| `MECATL_E2E_AUTH_TOKEN` | — | remote target only: bearer token |

## What the local target spawns

`harness.Local` runs `bin/mecated` with fully ephemeral state under
`<repo>/.scratch/e2e-<timestamp>-<rand>/` (never `/tmp` — house rule): fake
`HOME`/`XDG_*`, a git-initialised fixture workspace, JSONL store, memory dir,
user-model dir, soul file, and the checked-in skill fixtures laid out in the
conventional locations (`~/.claude/skills/greet` under the fake HOME;
`<workspace>/.claude/skills/repo-fact`). Flags (all verified against
`cmd/mecated/main.go`):

- loopback TCP on harness-picked free ports (`--grpc-addr`/`--http-addr`/
  `--metrics-addr`; mecated has **no UNIX-socket listen mode** — the TUI's UDS
  hosting is an embedded-server construct, not a daemon flag)
- `--skills-conventional` + `--trust-project` (workspace skill tier is
  trust-gated)
- `--soul-file`, `--memory-dir`, `--user-model-dir`, `--store-dir`
- `--permission-config <root>/permissions.yaml` — CLI-scope allows for
  `Skill`/`Parallel`/`Team` (those resolve to Ask otherwise); everything else
  keeps the production posture and the driver auto-DENIES unexpected asks
- `--max-run-tokens` / `--max-team-tokens` (the budget brakes)
- `--toolhive=false` (hermetic: never adopt the developer's running workloads)

The spawn sets `SysProcAttr.Pdeathsig = SIGTERM` (orphan prevention: a
hard-killed test runner takes mecated with it). `Pdeathsig` exists only on
Linux, so the live harness is Linux-only by design.

The subprocess environment is **minimal and explicit**: `PATH`, the fake
HOME/XDG dirs, and `OPENROUTER_API_KEY` — deliberately *not* `os.Environ()`, so
a developer's `OPENAI_API_KEY`/`ANTHROPIC_API_KEY` can't flip provider
detection. The provider is pinned per session via the `openrouter` provider id
on `CreateSession`.

## Scenarios

One ordered root container (ginkgo randomizes top-level containers, so ordering
+ the canary gate need a single root; that is also why the feature specs live
in `e2e/*_test.go` rather than a separate `e2e/features/` package — a second
package would need its own suite bootstrap and its own server):

1. **provider smoke** — one-word turn, `stop=end_turn`, usage > 0. The canary:
   its failure skips every dependent scenario. Secondary OpenAI-family lane
   included (skippable).
2. **global skill** — `greet` from the fake `~/.claude/skills`; asserts the
   `Skill` tool.call + non-error result, AND that zero interactive approvals
   were needed (the `permissions.yaml` CLI-scope allow is the wiring under
   test — no `ApproveTools` backup).
3. **workspace skill** — `repo-fact` from `<workspace>/.claude/skills`.
4. **parallel subagents** — two `Subagent` calls in one message; asserts ≥2
   distinct child ids + ≥2 `agentId:` trailers (N results, not temporal overlap).
5. **Parallel construct** — one 2-branch `Parallel` call; asserts
   `parallel.start(BranchCount=2)` → branch events → `parallel.end`.
6. **teams** — 3-member roster (lead+2 workers) recording findings; asserts
   `team.start`, **recorded findings** in the snapshots (the goal forces two
   `RecordFinding` calls; an empty snapshot does not pass), `team.end`, and the
   `Team id:` line in the tool result (which also rides the `joinTeamFallback`
   path — a degraded pass is accepted and documented in the spec).
7. **metrics** — scrapes `/metrics` after the delegation scenarios; asserts
   `mecatl_tool_calls_total{role="main"} > 0` and a `role="subagent"` series.
   Local target only (a remote server's pre-existing counters make `>0`
   vacuous); the subagent-series assertion is gated on the subagents spec
   having observed real child activity, to avoid double-reporting.
8. **user memory** — `RememberUser` event + the fact lands in
   `<user-model-dir>/memory.json` (file check is local-target only).
9. **project memory** — `Remember` + `<memory-dir>/memory.json`.
10. **soul** — deterministic: the `soul ENABLED (user provenance...` composition
    fact in the captured stderr; the behavioural `SOUL-OK:` marker is a
    `quarantine`-labelled spec that reports but never fails.

All assertions are event-stream / side-effect assertions — never model prose.
`FlakeAttempts(2)` is on the cheap specs — provider smoke, skills, and memory —
never on the expensive delegation scenarios (subagents/parallel/teams).

## Artifacts

Every run writes a JSONL transcript per scenario under
`<scratch-root>/artifacts/<scenario>/transcript-*.jsonl` (prompt, every
translated event with its Go type, ask/approval ledger, timeout markers), and
mecated's combined stdout+stderr is captured to `artifacts/mecated.log`. A
failing spec
attaches a self-diagnosing report (network-vs-model-vs-harness classification,
transcript path, resolved model, usage, stderr tail) via `AddReportEntry`. The
suite ends with a cumulative token/cost estimate (delegation children included).

## Permission posture

The driver answers permission asks by policy: allow-once for the scenario's
`ApproveTools`, **deny** for everything else (recorded in the transcript and
the failure classification). A live scenario must never park on a human.

## Prompt phrasing vs the upstream prompt filter

OpenRouter routes `openai/*` to OpenAI **and Azure** endpoints. With mecatl's
full request shape (big instructions + the tool catalog), certain innocuous
imperative phrasings get DETERMINISTICALLY rejected with
`response incomplete: content_filter` in ~1-2s (an input-side prompt-shield,
not output moderation). Probe-verified on `openai/gpt-4.1-mini`:

- `"Reply with the single word ok. Do not use any tools."` → content_filter, 3/3
- `"Reply with exactly the single word: ok. Do not call any tools."` → clean, 3/3
- `"...then follow its instructions to greet Ozz..."` → content_filter
- `"...durable fact about me: my name is Ozz..."` → content_filter

The same text WITHOUT mecatl's tool catalog passes — it is a joint score over
the whole request. Worse: the filter also hits MODEL-AUTHORED child prompts
(the parent paraphrases Subagent goals into the child's first turn), so no
amount of suite-side prompt rewording makes the OpenAI-family lane reliable.
That is why the **default lane is `anthropic/claude-3.5-haiku`** (Bedrock
endpoints, no such filter observed) and the OpenAI-family lane is the
single-turn secondary smoke. If you edit a prompt and a scenario starts
failing instantly with `content_filter`, phrasing is the first suspect.

Two account-level realities, verified live with this key:

- `openai/gpt-4o-mini` is fully blocked for *tool-bearing* `/responses`
  requests — 404 "no endpoints available matching your guardrail restrictions
  and data policy".
- The filter behaviour above applies to the other OpenAI-family models
  (`gpt-4.1-mini` verified; all route to OpenAI+Azure endpoints).

A possible operator-side fix is ignoring the filtered provider at
https://openrouter.ai/settings/preferences (mecatl deliberately has no
provider-routing knob — `port.LLMRequest` stays provider-neutral).

## Findings protocol

If a scenario fails because the FEATURE is broken live (not the suite), don't
patch the harness around it: capture the artifact, mark the spec
`Skip("FINDING: ...")` with a precise description, and file it for its own dev
pipeline.
