# MECATEQUI.md — running mecatl as a single-shot GitHub Action

`mecatequi` (`cmd/mecatequi/main.go`) is the headless, single-shot mecatl runner: one
prompt, one in-process engine, one terminal state, three artifacts (a working-tree git
diff, a machine-readable summary JSON, an optional durable event log), and an exit code.
This doc covers the **forge glue** that runs it inside GitHub Actions — a reusable
composite action and a split-privilege workflow template — and the trust model that keeps
an agent driven by untrusted issue text from doing damage.

The Go binary's contract is Pipeline 1 and is frozen. Everything here lives in `.github/`
and `docs/` and changes no Go.

## 1. Framing: the inverse of cloud-native

The cloud-native arc (`docs/design/CLOUD-NATIVE.md`) makes the mecatl **process**
disposable: state (sessions, the event log, approvals) is externalised to a durable store
so a crashed or restarted process re-attaches and continues. The Action is the *inverse*
move. It makes mecatl a **stateless one-shot** whose state is the GitHub issue and the pull
request it opens — **the forge is the store**. There is nothing to re-attach to: the run
reads its task from the issue, produces a patch + summary, and exits. The issue thread and
the PR are the durable record; the process keeps none.

That inversion is why the v1 Action does not wire `Config.EventLog` or a session store: a
single-shot run has no second consumer to read its state back. The durable JSONL event log
mecatequi emits is an *artifact* (uploaded for forensics), not a rehydration source.

## 2. The split-privilege model

The template (`.github/workflows/mecatequi-example.yml`) is two jobs with a hard token
boundary:

- **`implement`** runs the agent. It holds `permissions: contents: read`, **no** id-token,
  **no** write scope. Its only secret is the LLM key. If a prompt-injection in untrusted
  issue text hijacks the agent, the blast radius is the **rotatable LLM key** — the agent
  cannot push code, open a PR, or comment, because the job has no token that can.
- **`publish`** holds the write token (`contents: write`, `pull-requests: write`,
  `issues: write`) but **runs no agent code**. It downloads the `implement` job's artifacts
  and applies the patch as **data** (`git apply`), then posts the summary as text.

> **The token boundary invariant:** the step that can write to GitHub never runs agent
> code; the step that runs agent code never holds a write token.

A stronger variant, noted in the template, replaces the broad job `GITHUB_TOKEN` in
`publish` with a just-in-time GitHub App installation token scoped to exactly this repo's
contents + pull-requests + issues — removing the standing write grant entirely. v1 ships
the `GITHUB_TOKEN` form for zero-config copyability and documents the App-token upgrade.

## 3. Trust boundary

| Source | Trust | Handling |
|---|---|---|
| Issue/comment text | **Untrusted** (attacker-controllable) | Extracted by jq from `$GITHUB_EVENT_PATH` into a file (`extract-prompt.sh`), passed via `--prompt-file`, fenced by the binary's `--untrusted-prompt`. |
| Operator workflow config (posture, flags, model) | **Trusted** | Set by a maintainer in the workflow; passed as action inputs. |

Two gates keep an unauthorised author out: the job `if:` requires
`author_association ∈ {OWNER, MEMBER, COLLABORATOR}` (and the label/mention), and
`author-gate.sh` re-asserts it inside the job as defense-in-depth behind a possibly-edited
`if:`.

**Public-vs-internal posture.** On a public repo, treat the issue text as hostile: keep
`untrusted: "true"` and prefer `posture: "auto"` (allow-all with child injection-defence
ON) over `yolo`. On an internal repo the same defaults are still correct — the author gate
is the trust decision, not the posture.

## 4. The binary's stable contract

The Action codes against three frozen surfaces in `cmd/mecatequi`:

- **Summary JSON** — `cmd/mecatequi/run.go` (`Summary`). Additive-only; the action reads
  `.stop_reason`, `.non_empty_diff`, `.diff_bytes`, and `.usage.total_tokens` from it. The
  summary is written to a **file** (`--out-summary <path>`), never `-`, so it never
  collides with logs and stays clean for `jq`.
- **Exit code** — `cmd/mecatequi/run.go` (`exitCode`) plus the setup-failure paths in
  `cmd/mecatequi/main.go`: `0` = clean terminal (incl. the honest non-completions
  `no_progress`/`budget`/`max_*`), `1` = run failure (model error, cancelled, no-approver
  cancel-on-ask, timeout), `2` = setup failure. The action maps these to the
  `exit-class` output (`clean`/`run-failure`/`setup-failure`) and **never fails its own
  step on a non-zero code** — the caller branches on `exit-class`.
- **Flags** — `cmd/mecatequi/flags.go` (`flags`). The action's inputs map onto the flags
  one-for-one; secrets are read from the environment (`appConfig` in
  `cmd/mecatequi/flags.go` pulls the key from `OPENAI_API_KEY`), so the LLM key is never an
  action input.

### Action input → flag map

| Action input | Flag | Default |
|---|---|---|
| `prompt-file` (required) | `--prompt-file` | — |
| `untrusted` | `--untrusted-prompt` (when `true`) | `true` |
| `workspace` | `--workspace` | `${{ github.workspace }}` |
| `posture` | `--posture` | `auto` |
| `timeout` | `--timeout` | `15m` |
| `max-run-tokens` | `--max-run-tokens` (omitted when empty) | `""` |
| `model` | `--model` | `""` |
| `default-provider` | `--default-provider` | `""` |
| `default-model` | `--default-model` | `""` |
| `openai` | `--openai` (when `true`) | `""` |
| `openai-base-url` | `--openai-base-url` | `""` |
| `guardrails-model` | `--guardrails-model` | `""` |
| `out-diff` | `--out-diff` | `$RUNNER_TEMP/mecatequi.patch` |
| `out-summary` | `--out-summary` | `$RUNNER_TEMP/mecatequi.summary.json` |
| `out-events` | `--out-events` | `$RUNNER_TEMP/mecatequi.events.jsonl` |
| `ref` / `mecatequi-version` | source ref built from | `""` |

Outputs (kebab-case, GitHub Actions house style): `patch-path`, `summary-path`,
`events-path`, `summary-json` (compacted; best-effort and `$GITHUB_OUTPUT`-size-bounded —
read `summary-path` for anything large), `stop-reason`, `non-empty-diff`, `exit-class`.

## 5. Distribution decision

**v1: build-from-source.** There is no published mecatequi release asset — `release.yml`
ships only the `mecated` container image. So the composite action builds the binary from
the checked-out source in the low-priv `implement` job that already has the tree. The
action ships its **own** `actions/setup-go` step (SHA-pinned, `go-version-file: go.mod`,
`cache: true`) so every consumer provisions the go.mod-required toolchain regardless of the
runner's preinstalled Go; the build step then runs `go build` with `GOTOOLCHAIN=local`
(satisfied by the provisioned toolchain) and `GOFLAGS=-mod=readonly`. Acquisition is two
isolated composite steps — "Set up Go" + "Acquire mecatequi binary".

**Follow-up: signed release asset.** A future release job can publish a checksummed,
cosign-signed `mecatequi` binary (mirroring the `mecated` image's keyless-signing flow in
`.github/workflows/release.yml`). Swapping the Action to consume it is then a *one-step*
change: drop the two acquisition steps ("Set up Go" + "Acquire mecatequi binary") and add
a download + `cosign verify` + checksum-check step; the run step is unchanged because it
already reads `$RUNNER_TEMP/mecatequi` regardless of how it got there. This pipeline
deliberately does **not** add that release job.

## 6. Follow-up: the `/proc`-exfiltration gap (honest status)

There is a **real** environment-exfiltration gap, and v1 does not fully close it — it
mitigates it at the workflow layer and names the engine fix.

**The gap.** The main-session command runner inherits `os.Environ()` unscrubbed. Under
posture `auto` (or looser) the model can run a Bash tool call like `echo $OPENAI_API_KEY`
and read any secret in the process environment. This is a **Bash** path, not a `Read` path
— `Read` is confined to the workspace by the osfs adapter, but the shell inherits the full
environment. So the fence on the prompt does not contain it: a successfully-injected agent
with shell access can exfiltrate the LLM key.

**v1 mitigation (workflow-side, in scope).** The `implement` job holds **only** the LLM
key and **no GitHub write token**, so the blast radius of a successful exfiltration is the
**rotatable LLM key** — not repository write access. The produced PR is human-reviewed
before merge. This bounds the damage; it does not eliminate the read.

**Engine fix (out of scope here — not implemented).** The main-session command runner
should scrub its environment to an allowlist via the existing `WithCommandEnvList` seam, so
the shell never sees provider secrets. That is an `engine/`/`internal/app` change and is
deliberately **not** part of this forge-glue pipeline. Until it lands, the honest posture
is: **do not run mecatequi from Actions with a secret in the environment you are not
willing to rotate**, and keep the write token out of the agent's job.

## 7. Deferred: conversational v2

A multi-turn, conversational mecatequi (a bot that holds a thread across comments) is
deferred. It needs two things v1 lacks:

- **Snapshot fidelity** — the run state would have to survive between comment events,
  which means a durable session store the next invocation rehydrates from (the cloud-native
  rehydration seam, `docs/design/CLOUD-NATIVE.md`).
- **A driver-store-as-artifact backend** — the externalised store could be backed by a
  driver (`docs/design/DRIVERS.md`) whose storage is a workflow artifact or a repo branch,
  so the forge remains the store across turns.

v1 is one-shot on purpose: it is the simplest thing that is safe, and it defers the state
machinery until a conversational use case justifies it.

## 8. Usage

The operator-facing walkthrough — the action input/output table and the example-workflow
copy-and-review steps — lives in `docs/usage.md` ("Running mecatequi from GitHub Actions").
The example workflow itself is `.github/workflows/mecatequi-example.yml`, and its place
alongside the other workflows is documented in `.github/workflows/README.md`.

---

*Part of the [design docs](./README.md). Related: [CLOUD-NATIVE.md](./CLOUD-NATIVE.md)
(the inverse — disposable process, externalized state), [DRIVERS.md](./DRIVERS.md) (the
store seam a conversational v2 would build on), [ALLOW-ALL-POSTURE.md](./ALLOW-ALL-POSTURE.md)
(the `auto`/`yolo` posture the CI run selects).*
