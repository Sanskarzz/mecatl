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

The template (`.github/workflows/mecatequi-example.yml`) is three jobs with a hard token
boundary:

- **`acknowledge`** runs **first**, gated on the same trigger as `implement`, holding
  `permissions: issues: write` only — **no** LLM key, **no** agent code, **no** contents
  write. It posts one early comment to the issue (*"🤖 mecatequi is working on this — see
  the run: …"* with the run URL). This guarantees a **durable issue-side signal for every
  triggered run**, even if everything downstream fails silently — the failure mode field
  data surfaced, where a skipped or failed `publish` left the issue author with no trace at
  all. Its only capability (commenting) is the minimum needed for that trace.
- **`implement`** (`needs: acknowledge`) runs the agent. It holds `permissions: contents:
  read`, **no** id-token, **no** write scope. Its only secret is the LLM key. If a
  prompt-injection in untrusted issue text hijacks the agent, the blast radius is the
  **rotatable LLM key** — the agent cannot push code, open a PR, or comment, because the
  job has no token that can.
- **`publish`** (`needs: [acknowledge, implement]`) holds the write token (`contents:
  write`, `pull-requests: write`, `issues: write`) but **runs no agent code**. It downloads
  the `implement` job's artifacts (**non-fatally** — a missing artifact must not abort the
  job before `publish.sh` runs) and applies the patch as **data** (`git apply`), then posts
  the summary as text. Its `if:` (`!cancelled() && needs.acknowledge.result == 'success'`)
  fires on implement **success and failure**, so a failed run still lands an honest
  terminal comment instead of silence; `publish.sh` reads a missing/empty `EXIT_CLASS` as
  setup-failure, and a failed `git push` / `gh pr create` comments *before* exiting
  non-zero (never a silent abort after deciding to open a PR).

> **The token boundary invariant:** the step that can write to GitHub never runs agent
> code; the step that runs agent code never holds a write token. Only `acknowledge` and
> `publish` hold a write scope (and `acknowledge` holds only `issues: write`); `implement`
> never gains issues/pull-requests/contents write.

A stronger variant, noted in the template, replaces the broad job `GITHUB_TOKEN` in
`publish` with a just-in-time GitHub App installation token scoped to exactly this repo's
contents + pull-requests + issues — removing the standing write grant entirely. v1 ships
the `GITHUB_TOKEN` form for zero-config copyability and documents the App-token upgrade.

## 3. Trust boundary

| Source | Trust | Handling |
|---|---|---|
| Issue/comment text | **Untrusted** (attacker-controllable) | Extracted by jq from `$GITHUB_EVENT_PATH` into a file (`extract-prompt.sh`), passed via `--prompt-file`, fenced by the binary's `--untrusted-prompt`. |
| Operator workflow config (posture, flags, model) | **Trusted** | Set by a maintainer in the workflow; passed as action inputs. |

**The author gate is trigger-based, NOT `author_association`-based.** GitHub's webhook
`author_association` is **unreliable for membership** — it reports an org MEMBER as
`CONTRIBUTOR` — so a gate that asserts `author_association ∈ {OWNER, MEMBER, COLLABORATOR}`
silently **skips legitimate runs**. The two deployments differ:

- **Private repo** (the live `.github/workflows/mecatequi.yml`): the **trigger is the
  gate**. Applying a label needs triage/write access and commenting is team-only, so
  GitHub's own permission model decides who can start a run; no `author_association` check
  is wired, and `author-gate.sh` is absent.
- **Public repo**: do **not** trust `author_association`. Add a dedicated permission-check
  gate **job** that calls the `collaborators/{user}/permission` API and gates
  `implement`/`publish` on its result.

`author-gate.sh` (which asserts `author_association`) ships in the **example template
only**, as a defense-in-depth illustration behind a possibly-edited `if:`; it is
deliberately not part of the live workflow's gate.

**Public-vs-internal posture.** On a public repo, treat the issue text as hostile: keep
`untrusted: "true"` and prefer `posture: "auto"` (allow-all with child injection-defence
ON) over `yolo`. On an internal repo the same defaults are still correct — the author gate
is the trust decision, not the posture.

## 4. The binary's stable contract

The Action codes against these frozen surfaces in `cmd/mecatequi`:

- **The patch** — `cmd/mecatequi/run.go` (`gitDiffPatch`). A `git diff HEAD` shows only
  tracked edits + deletions and would silently **lose the agent's new files**; mecatequi
  therefore appends a `git diff --no-index -- /dev/null <file>` new-file hunk for every
  untracked, non-ignored file (`git ls-files --others --exclude-standard`) so the patch
  `publish.sh` applies with `git apply` reproduces **modified, added, and deleted** files.
  `non_empty_diff` and `diff_bytes` therefore agree: a single new file makes the patch
  non-empty. The git env is scrubbed (`gitenv.Scrub`) and every git call carries
  `--no-ext-diff`, so no inherited `GIT_*` or repo-named external diff driver can run.
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
  one-for-one; secrets are read from the environment, never a flag or an action input.
  `appConfig` (`cmd/mecatequi/flags.go`) applies the shared `internal/cliconfig`
  `ProviderFlags` — the SAME credential/base-URL helper `mecated` and `mecatui` use — so
  mecatequi reads all three provider keys (`OPENAI_API_KEY` / `OPENROUTER_API_KEY` /
  `ANTHROPIC_API_KEY`) and registers all three `--*-base-url` flags. A present
  `OPENAI_API_KEY` flips the OpenAI provider on automatically (the same flip `mecated`
  does); for OpenRouter or Anthropic set the respective key plus `--default-provider`. This
  is why the live `.github/workflows/mecatequi.yml` delivers `OPENROUTER_API_KEY` at the
  job's `env:` and never needs an OpenAI-specific input.

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

Prefer `model` for newer/passthrough models; `default-model` is catalog-validated and
rejects ids not in the embedded snapshot. `model` is the per-session passthrough path.
| `openai` | `--openai` (when `true`) | `""` |
| `openai-base-url` | `--openai-base-url` | `""` |
| `guardrails-model` | `--guardrails-model` | `""` |
| `out-diff` | `--out-diff` | `$RUNNER_TEMP/mecatequi.patch` |
| `out-summary` | `--out-summary` | `$RUNNER_TEMP/mecatequi.summary.json` |
| `out-events` | `--out-events` | `$RUNNER_TEMP/mecatequi.events.jsonl` |

Outputs (kebab-case, GitHub Actions house style): `patch-path`, `summary-path`,
`events-path`, `summary-json` (compacted; best-effort and `$GITHUB_OUTPUT`-size-bounded —
read `summary-path` for anything large), `stop-reason`, `non-empty-diff`, `exit-class`.

### Configurable PR-description formatting

`publish.sh` builds the PR title + body. A repo can override the **body** layout (and,
optionally, the **title**) with a template file of `{{placeholder}}` tokens, so different
repos get their own PR-description style. This is `publish.sh` + `action.yml` glue — no Go.

**Resolution order (in `publish.sh`):**

1. The `pr-body-template` input / `MQ_PR_BODY_TEMPLATE` env — a path **relative to the
   checkout** — if set and the file exists.
2. Else `.github/mecatequi/pr-body.md` in the checkout (the zero-config convention) if it
   exists.
3. Else the **built-in rich body** (caveat + "What the agent did" + "Files changed" + "Run"
   table + run link + `Closes #<n>`). **An absent template preserves today's behaviour
   byte-for-byte** — a default-path render is byte-equivalent to the prior body.

A missing **explicit** template path — or one that resolves **outside the checkout** (a
`../` traversal or an escaping symlink; the path is confined to `${MQ_WORKSPACE}` as
defense-in-depth, CWE-22) — logs a `::warning::` and falls back to the built-in body (it
never aborts the publish, and never reads an out-of-checkout file into the PR). The optional
`pr-title-template` / `MQ_PR_TITLE_TEMPLATE` controls the title the same way (same
confinement); its default is the prior title `mecatequi: changes for issue #<n>`. A rendered
title is flattened to one line — so use the **short** placeholders in a title
(`{{issue_ref}}`, `{{stop_reason}}`, `{{branch}}`); prose placeholders like
`{{what_agent_did}}` or `{{summary_table}}` flatten to one unwieldy line.

**Placeholders (the documented allowlist):**

| Token | Value |
|---|---|
| `{{what_agent_did}}` | the model's `final_text` (its own account of the change) |
| `{{files_changed}}` | the name-status bullet list of changed files |
| `{{summary_table}}` | the run-metadata markdown table |
| `{{run_url}}` | link to the workflow run |
| `{{issue}}` | the issue number, bare (e.g. `123`) |
| `{{issue_ref}}` | the issue reference (e.g. `#123`) |
| `{{stop_reason}}` | the terminal stop reason |
| `{{non_empty_diff}}` | whether the run left a diff (`true`/`false`) |
| `{{diff_bytes}}` | diff size in bytes |
| `{{total_tokens}}` | cumulative tokens for the run |
| `{{branch}}` | the head branch the PR is opened from |
| `{{base}}` | the base branch the PR targets |

An **unknown** `{{token}}` is left **intact** (the operator may want literal braces) — never
stripped, never an error. When there are no changed files, `{{files_changed}}` renders a
`_(no files changed)_` sentinel (parity with `{{summary_table}}`'s "no run summary"
fallback), so a template author's `## Files changed` header is never left dangling over an
empty value.

**Why a careful substitution mechanism (the security point).** `{{what_agent_did}}` is the
model's `final_text` — **agent-authored from untrusted issue text**. The substitution
(`render_template` in `publish.sh`, a small `python3` pass) is therefore:

- **Literal** — values are read from a JSON file (built with `jq --arg`, never argv, never
  the process env) and inserted via a `re.sub` *callable*, so a value containing `& \ /`,
  backticks, `$(...)`, or `{{...}}` is inserted **verbatim**, never interpreted. No naive
  `sed s///` (breaks on `& / \`), no `eval`, no `envsubst` against the env (would expand any
  `$VAR` an attacker put in `final_text`).
- **Single-pass** — one scan of the template; each `{{token}}` is resolved once against a
  fixed dict, and a value that itself contains `{{run_url}}` is **not** re-expanded.
- **Display-only** — the result is written to a `gh ... --body-file` and posted as PR text;
  it is never executed.

**The trust caveat is non-negotiable.** `publish.sh` **always force-prepends** the
`⚠️ Agent-authored from the issue text — review carefully before merging.` line above
whatever the template renders, so a custom template can never drop the safety warning. It is
not a placeholder — **do NOT include your own ⚠️ caveat line in the template, or you get a
duplicate.**

**Issue linkage.** The built-in default keeps `Closes #<n>`. A custom template controls its
own linkage (use `{{issue_ref}}` with `Closes`/`Refs`); `publish.sh` does **not** force-append
`Closes` when a template is used.

**Scope.** Templating applies to the **PR-create body + the re-run body refresh only**. The
failure / no-change / de-dup comment paths are unchanged.

**Example template + activation.** `.github/mecatequi/pr-body.md.example` ships a copyable
template (it is `.example` so this repo keeps the built-in default until someone opts in).
**Rename it to `.github/mecatequi/pr-body.md`** to activate it (resolution step 2 — no
workflow change), or point `MQ_PR_BODY_TEMPLATE` at any path. The live workflow keeps the
built-in default.

## 5. Distribution decision

**The matlatl pattern: build from the action's own checkout — single path, no auth.** The
composite action (`.github/actions/mecatequi/action.yml`) builds the binary from the
action's **own** source tree (`go build -C "$GITHUB_ACTION_PATH/../../.." ./cmd/mecatequi`,
with the toolchain provisioned from `${{ github.action_path }}/../../../go.mod`,
`GOTOOLCHAIN=local`, `GOFLAGS=-mod=readonly`). This is exactly how `stacklok/matlatl`'s
composite works, and it is **uniform** across both ways the action is referenced:

- **Self (this repo).** `uses: ./.github/actions/mecatequi` → `$GITHUB_ACTION_PATH` is the
  local workspace's action directory, so it builds the checked-out tree (HEAD). mecatl's own
  live workflow uses this — it dogfoods HEAD with no tag dependency.
- **Cross-repo.** `uses: stacklok/mecatl/.github/actions/mecatequi@<tag>` → GitHub checks
  the **whole mecatl repo out at that tag** into `$GITHUB_ACTION_PATH`, and the build uses
  that tagged source. **No mecatl checkout** is required in the consuming repo.

Because the action source itself is the build input, there is **no token, no `GOPRIVATE`,
and no PAT** — the same zero-credential posture as matlatl. The action lives at
`.github/actions/mecatequi/`, so the repo root (where `go.mod` + `cmd/mecatequi` live) is
`$GITHUB_ACTION_PATH/../../..`. Acquisition stays two isolated composite steps ("Set up Go" +
"Acquire mecatequi binary") so the signed-asset swap below remains a contained change.

**Cross-repo reference.** A consumer references the subdir action and pins a tag (or a SHA):

```yaml
- uses: stacklok/mecatl/.github/actions/mecatequi@v0.0.1   # SHA-pin in real workflows
  with:
    prompt-file: ${{ runner.temp }}/prompt.txt
    posture: auto
```

`owner/repo/path@ref` is the GitHub syntax for an action that lives in a repository
subdirectory; the `@<ref>` IS the version (there is no version input — matlatl has none
either). The **only** requirement is the org setting that lets GitHub Actions use actions
from the org's internal/private repositories (Settings → Actions → General → Access for
`stacklok/mecatl`, or `gh api -X PUT
repos/stacklok/mecatl/actions/permissions/access -f access_level=organization`). mecatl's own
workflow keeps `uses: ./.github/actions/mecatequi` (local build, always HEAD).

**Versioning: ALPHA, `v0.0.x`.** The first published tag is `v0.0.1`. Tags are cut by a
release process separate from this design (the orchestrator), not by editing the action.

**Residual / honest cost.** Each run builds the binary from source (no cached release
artifact). A future release job can publish a checksummed, cosign-signed `mecatequi` binary
or a container (mirroring the `mecated` image's keyless-signing flow in
`.github/workflows/release.yml`); swapping the action to consume it is then a one-step change
— drop the two acquisition steps and add a download + `cosign verify` + checksum-check, since
the run step already reads `$RUNNER_TEMP/mecatequi` regardless of how it got there. That
signed-binary / container path is the later speed optimization; this pipeline deliberately
does **not** add it yet.

## 6. The `/proc`-exfiltration gap — FIXED (the secret-scrubbed agent shell)

This **was** a real environment-exfiltration gap (security review "Finding B"). It is now
**closed at the engine layer**: every agent-facing Bash shell runs with the harness's
credentials scrubbed out of its environment.

**The gap (what it was).** The main-session command runner inherited `os.Environ()`
unscrubbed. Under posture `auto` (or looser) the model could run a Bash tool call like
`echo $OPENROUTER_API_KEY` or `cat /proc/self/environ` and read any secret in the process
environment. This is a **Bash** path, not a `Read` path — `Read` is confined to the
workspace by the osfs adapter, but the shell inherited the full environment, so the prompt
fence did not contain it: a successfully-injected agent with shell access could exfiltrate
the LLM key. The "hardened" sandboxed runners (read-only subagent / team-member /
force-copy) were no safer — `gitenv.Scrub` only ever dropped `GIT_*`/`PAGER`, never secrets.

**The fix (engine-layer, `internal/adapter/envscrub` + `internal/app`).** A new stdlib-only
leaf `internal/adapter/envscrub` (`envscrub.Scrub`) computes the agent shell's environment
as `os.Environ()` MINUS the harness's credentials, via a **precise denylist**:

- the EXACT credential variable names the harness reads — the provider keys
  (`OPENAI_API_KEY` / `OPENROUTER_API_KEY` / `ANTHROPIC_API_KEY`), the websearch keys
  (`WEBSEARCH_API_KEY` / `BRAVE_API_KEY` / `EXA_API_KEY`), the auth tokens
  (`MECATL_AUTH_TOKEN` / `MECATL_DRIVER_AUTH_TOKEN`), and the forge tokens
  (`GH_TOKEN` / `GITHUB_TOKEN`); PLUS
- a conservative secret-SHAPED name pattern as defence-in-depth (`*_API_KEY` / `*_TOKEN` /
  `*_SECRET` / `*_PASSWORD` / `*_PASSWD` / `AWS_*` / `AZURE_*` /
  `GOOGLE_APPLICATION_CREDENTIALS`).

A **denylist** (not an allowlist) is deliberate: a coding agent runs `go build`/`go test`/
`git`, which need `PATH`, `HOME`, `GOPATH`, `GOCACHE`, `GOMODCACHE`, `TMPDIR`, `LANG` and an
open-ended toolchain set — an allowlist would silently break a build the moment a tool
needed a var nobody enumerated. The scrub keeps the whole toolchain and removes only
credentials.

It is wired into the existing `osfs.WithCommandEnvList` seam for **every** agent-adjacent
shell, one policy:

- `buildCommandRunner` (the MAIN session, the runner posture `auto`/`yolo` exposes) —
  secret-scrubbed only (operator hooks/pager still honoured);
- `newHardenedCommandRunner` (read-only subagent / team-member / force-copy branches) and
  `gitSnapshot` and the forker's fork-time git — `gitenv.Scrub(envscrub.Scrub(os.Environ()))`,
  i.e. secret-scrub first, then git-neutralise.

**Residual.** The post-run diff/artifact git invocations in `cmd/mecatequi` are NOT
agent-facing (their output never reaches the model), so they are out of this scope. The
security oracle is `internal/app/command_runner_secret_scrub_test.go` (mutation-tested:
reverting the scrub makes it fail) plus `internal/adapter/envscrub/envscrub_test.go`.

**v1 workflow mitigation (still in force, defence-in-depth).** The `implement` job holds
**only** the LLM key and **no GitHub write token**, so even if a future regression reopened
the read, the blast radius would be the **rotatable LLM key**, not repository write access.

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
