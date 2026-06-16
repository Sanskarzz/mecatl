#!/usr/bin/env bash
# publish.sh — turn a mecatequi run's patch + summary into a pull request (or, on a
# non-clean run, an honest failure comment on the triggering issue).
#
# RUNS IN THE PRIVILEGED JOB. This is the ONLY script that holds a GitHub write token, and
# it NEVER runs agent output as code: the agent's contribution is a unified diff applied as
# DATA via `git apply`, never `eval`/`source`/a script the agent authored. The summary is
# posted as PR/issue body TEXT, never executed.
#
# INPUTS ARE ENV-FED, never argv-interpolated. Every value the workflow passes — the patch
# path, the summary path, the issue number, the exit class, the GitHub token — arrives via
# an environment variable, so no event-derived string is spliced into this script's source.
#
# Env contract:
#   GH_TOKEN        GitHub token with contents:write + pull-requests:write + issues:write
#   ISSUE_NUMBER    the triggering issue number (for the PR linkage + the failure comment)
#   PATCH_PATH      path to the working-tree patch the run produced
#   SUMMARY_PATH    path to the run-summary JSON
#   EXIT_CLASS      clean | run-failure | setup-failure (empty -> treated as setup-failure)
#   REPO            owner/repo (defaults to $GITHUB_REPOSITORY)
#   BASE_BRANCH     base branch for the PR (defaults to the repo default branch, then
#                   $GITHUB_REF_NAME)
set -euo pipefail

: "${GH_TOKEN:?publish: GH_TOKEN is required}"
: "${ISSUE_NUMBER:?publish: ISSUE_NUMBER is required}"
# EXIT_CLASS may be EMPTY when the implement job died before setting its output. Treat an
# empty value as a setup-failure so we still post an honest comment, never abort silently.
EXIT_CLASS="${EXIT_CLASS:-setup-failure}"
[ -z "${EXIT_CLASS}" ] && EXIT_CLASS="setup-failure"
PATCH_PATH="${PATCH_PATH:-}"
SUMMARY_PATH="${SUMMARY_PATH:-}"
REPO="${REPO:-${GITHUB_REPOSITORY:-}}"

export GH_TOKEN

# Authenticate git over HTTPS via GH_TOKEN. The publish checkout uses
# persist-credentials:false (the token is never written to .git/config), so a plain
# `git push` has no credentials ("could not read Username for https://github.com").
# gh's credential helper supplies the token from the environment for the push below.
gh auth setup-git

# A link back to this workflow run, for both comment paths.
run_url="${GITHUB_SERVER_URL:-https://github.com}/${REPO}/actions/runs/${GITHUB_RUN_ID:-}"

# A compact, human-readable summary block for the PR/comment body. Read as DATA (jq), never
# executed. Falls back to a placeholder when no summary file exists. When the summary
# carries a non-empty .error (a run-failure terminal), surface it — the binary serialized
# it for exactly this.
summary_block() {
  if [ -n "${SUMMARY_PATH}" ] && [ -s "${SUMMARY_PATH}" ]; then
    jq -r '
      "| field | value |",
      "|---|---|",
      "| stop_reason | " + (.stop_reason // "(none)") + " |",
      "| non_empty_diff | " + ((.non_empty_diff // false) | tostring) + " |",
      "| diff_bytes | " + ((.diff_bytes // 0) | tostring) + " |",
      "| total_tokens | " + ((.usage.total_tokens // 0) | tostring) + " |",
      ( if (.error // "") != "" then "| error | " + (.error | gsub("\n"; " ")) + " |" else empty end )
    ' "${SUMMARY_PATH}"
  else
    echo "_(no run summary was produced)_"
  fi
}

# A single .field read from the summary as DATA (never executed). $1 is a jq path.
summary_field() {
  if [ -n "${SUMMARY_PATH}" ] && [ -s "${SUMMARY_PATH}" ]; then
    jq -r "${1} // \"\"" "${SUMMARY_PATH}"
  fi
}

# Translate the raw exit class into a plain-language cause + next action. The bare class
# string ("setup-failure") is jargon the issue author cannot act on; this maps each class
# to what likely happened and what to do, keeping the run link for the detail.
exit_class_explanation() {
  case "${1}" in
    setup-failure)
      echo "The run could not start — usually a bad/uncatalogued model id or a missing/invalid provider key (e.g. \`OPENROUTER_API_KEY\` / \`OPENAI_API_KEY\`). Check the FIRST error in the run log."
      ;;
    run-failure)
      echo "The run started but ended in failure — a model/provider error, a cancelled run, the no-approver cancel-on-ask (posture \`strict\` + headless), or a \`timeout\`. The \`stop_reason\` in the summary below distinguishes error from cancelled."
      ;;
    *)
      echo "The run ended in an unexpected state (\`${1}\`). Check the run log."
      ;;
  esac
}

# ── Non-clean run: post an honest failure comment, open NO PR ────────────────────────────
if [ "${EXIT_CLASS}" != "clean" ]; then
  {
    echo "## mecatequi run did not complete cleanly"
    echo
    echo "$(exit_class_explanation "${EXIT_CLASS}") No pull request was opened."
    echo
    summary_block
    echo
    echo "[View the workflow run](${run_url}) for the per-event trace and the operator verdict line."
  } > "${RUNNER_TEMP}/mecatequi-comment.md"
  gh issue comment "${ISSUE_NUMBER}" --repo "${REPO}" --body-file "${RUNNER_TEMP}/mecatequi-comment.md"
  echo "publish: posted failure comment for exit_class=${EXIT_CLASS}"
  exit 0
fi

# ── Clean run with no changes: nothing to publish, post an informational comment ─────────
# Branch the headline on the stop reason: end_turn means the model decided no change was
# needed (include its own explanation when present); the budget/limit terminals mean it ran
# out of room before finishing (actionable: raise the budget or narrow the task).
non_empty="$(summary_field '.non_empty_diff')"
if [ "${non_empty}" != "true" ] || [ -z "${PATCH_PATH}" ] || [ ! -s "${PATCH_PATH}" ]; then
  stop_reason="$(summary_field '.stop_reason')"
  final_text="$(summary_field '.final_text')"
  {
    case "${stop_reason}" in
      end_turn)
        echo "## mecatequi run completed — no change needed"
        echo
        echo "The run concluded that no change was needed (\`stop_reason: end_turn\`), so there is nothing to open a PR for."
        if [ -n "${final_text}" ]; then
          echo
          echo "The model's explanation:"
          echo
          printf '%s\n' "${final_text}" | sed 's/^/> /'
        fi
        ;;
      budget | max_turns | max_tool_calls | max_consecutive_failures)
        echo "## mecatequi run stopped before finishing — no file changes"
        echo
        echo "The run hit a budget/limit before finishing (\`stop_reason: ${stop_reason}\`), so it produced no diff. Raise \`max-run-tokens\`/\`timeout\` or the turn/tool-call limits, or narrow the task, then re-run."
        ;;
      no_progress)
        # NOT budget exhaustion — the model ended a turn with no tool call and no meaningful
        # text (an empty/reasoning-only loop). More budget will NOT help; the prompt/task is
        # the lever.
        echo "## mecatequi run stalled — no file changes"
        echo
        echo "The run stopped making progress (\`stop_reason: no_progress\`): the model produced empty / reasoning-only turns and never acted. Raising the budget will NOT help — re-state the task more concretely (a clear, actionable instruction), then re-run."
        ;;
      structured_output)
        # NOT budget exhaustion — the model could not produce output matching the requested
        # schema within the retry budget. A schema/prompt problem, not a token problem.
        echo "## mecatequi run failed schema validation — no file changes"
        echo
        echo "The run could not produce output matching the requested schema (\`stop_reason: structured_output\`) within its validation-retry budget. Raising the token budget will NOT help — check the output schema and the prompt, then re-run."
        ;;
      *)
        echo "## mecatequi run completed — no file changes"
        echo
        echo "The run finished (\`stop_reason: ${stop_reason:-unknown}\`) but produced no working-tree diff. Note: a clean exit is NOT 'task accomplished' — check the stop reason above."
        ;;
    esac
    echo
    summary_block
    echo
    echo "[View the workflow run](${run_url})."
  } > "${RUNNER_TEMP}/mecatequi-comment.md"
  gh issue comment "${ISSUE_NUMBER}" --repo "${REPO}" --body-file "${RUNNER_TEMP}/mecatequi-comment.md"
  echo "publish: clean run with empty diff (stop_reason=${stop_reason}) — posted informational comment"
  exit 0
fi

# ── Clean run with a diff: gate touched paths -> branch -> apply -> commit -> push -> PR ──

# DEFENSE-IN-DEPTH (pwn-request residual): the patch is attacker-INFLUENCED output, so
# reject any patch that touches CI-control / build-control paths before applying it. A
# successful injection could otherwise rewrite the very workflow that runs the agent and
# slip it past human review of the "feature" diff. `git apply --numstat` lists touched
# paths WITHOUT modifying the tree.
if ! numstat="$(git apply --numstat "${PATCH_PATH}" 2>/dev/null)"; then
  # numstat itself failing means the patch is malformed for this tree; fall through to the
  # real apply below, which will fail loudly with a useful message.
  numstat=""
fi
blocked="$(printf '%s\n' "${numstat}" | awk '{print $3}' | grep -E '^(\.github/|\.gitattributes$|\.git/|Makefile$|Taskfile\.ya?ml$)' || true)"
if [ -n "${blocked}" ]; then
  {
    echo "## mecatequi run produced a patch that touches protected paths"
    echo
    echo "The patch modifies CI-control / build-control files, which is rejected automatically (the agent diff is attacker-influenced output). No pull request was opened."
    echo
    echo "Blocked paths:"
    echo
    printf '%s\n' "${blocked}" | sed 's/^/- `/; s/$/`/'
    echo
    echo "[View the workflow run](${run_url})."
  } > "${RUNNER_TEMP}/mecatequi-comment.md"
  gh issue comment "${ISSUE_NUMBER}" --repo "${REPO}" --body-file "${RUNNER_TEMP}/mecatequi-comment.md"
  echo "::error::publish: patch touches protected paths; refusing to open a PR"
  exit 1
fi

# Deterministic per-issue branch so a re-run updates the SAME PR rather than spawning
# duplicates (documented behaviour).
branch="mecatequi/issue-${ISSUE_NUMBER}"

# Resolve the base branch: explicit BASE_BRANCH, else the repo default branch, else the ref
# this run checked out. The defaultBranchRef call is guarded so a transient API failure does
# not abort the whole publish.
base="${BASE_BRANCH:-}"
if [ -z "${base}" ]; then
  base="$(gh repo view "${REPO}" --json defaultBranchRef --jq '.defaultBranchRef.name' 2>/dev/null || true)"
fi
[ -z "${base}" ] && base="${GITHUB_REF_NAME:-main}"

# Post an honest "could not open the PR" comment and exit non-zero. Called from the
# privileged tail (push / PR-create) so a failure THERE is never silent — the run decided
# to open a PR, then could not, and the author must hear about it. $1 is the cause line.
push_failure_comment() {
  {
    echo "## mecatequi could not open the pull request"
    echo
    echo "The run produced a diff that applied cleanly, but the publish step failed: ${1}. No pull request was opened — re-run, or open one from the branch \`${branch}\` manually."
    echo
    echo "[View the workflow run](${run_url})."
  } > "${RUNNER_TEMP}/mecatequi-comment.md"
  gh issue comment "${ISSUE_NUMBER}" --repo "${REPO}" --body-file "${RUNNER_TEMP}/mecatequi-comment.md" || true
}

git config user.name "mecatequi[bot]"
git config user.email "mecatequi@users.noreply.github.com"
git checkout -b "${branch}"

# Apply the agent's patch as DATA. --3way lets trivial drift apply against a slightly-moved
# tree while a REAL conflict still fails loud; git apply parses the diff, never executes it.
if ! git apply --3way --index --whitespace=nowarn "${PATCH_PATH}"; then
  {
    echo "## mecatequi patch did not apply cleanly"
    echo
    echo "The run produced a diff, but it did not apply (even 3-way) onto \`${base}\`. No pull request was opened."
    echo
    echo "[View the workflow run](${run_url})."
  } > "${RUNNER_TEMP}/mecatequi-comment.md"
  gh issue comment "${ISSUE_NUMBER}" --repo "${REPO}" --body-file "${RUNNER_TEMP}/mecatequi-comment.md"
  echo "::error::publish: the run patch did not apply (3-way) to ${base}"
  exit 1
fi

# De-dup: if an open mecatequi PR for this issue already exists, push the deterministic
# branch (force-with-lease) and comment instead of opening a duplicate.
existing_pr="$(gh pr list --repo "${REPO}" --head "${branch}" --state open --json number --jq '.[0].number' 2>/dev/null || true)"

git commit -m "$(printf 'mecatequi: changes for issue #%s\n\nAutomated change produced by a mecatequi single-shot run.\n\nCo-Authored-By: Claude Opus 4.8 <noreply@anthropic.com>' "${ISSUE_NUMBER}")"

# From here on a failure must COMMENT before aborting (never silent after deciding to open
# a PR). `set -e` would abort the script on a failed push/PR-create, so guard each with an
# explicit comment-then-exit. The push uses --force-with-lease so a re-run updates the same
# branch safely.
if ! git push --force-with-lease --set-upstream origin "${branch}"; then
  push_failure_comment "the git push failed (the bot may lack contents:write, or the branch moved)"
  echo "::error::publish: git push to ${branch} failed"
  exit 1
fi

# The PR body is the summary block as TEXT — never executed. The trust caveat is
# PROMINENT (top line): this diff was authored by an agent from untrusted issue text, so a
# human must scrutinise it before merging — merging IS the approval gate. We keep
# `Closes #N` (auto-closing the issue on merge is correct, since the merge is that human
# gate); a deployment that prefers NOT to auto-close on merge can swap `Closes #N` for
# `Refs #N` below (it links the issue without closing it).
{
  echo "⚠️ **Agent-authored from the issue text — review carefully before merging.**"
  echo
  echo "Automated change produced by a mecatequi single-shot run for issue #${ISSUE_NUMBER}."
  echo
  summary_block
  echo
  echo "[View the workflow run](${run_url})."
  echo
  echo "Closes #${ISSUE_NUMBER}"
} > "${RUNNER_TEMP}/mecatequi-pr-body.md"

if [ -n "${existing_pr}" ]; then
  echo "publish: updated existing PR #${existing_pr} on ${branch}"
  gh issue comment "${ISSUE_NUMBER}" --repo "${REPO}" \
    --body "Updated the existing pull request #${existing_pr} with a fresh mecatequi run. ⚠️ Agent-authored — review carefully before merging. [View the workflow run](${run_url})."
  exit 0
fi

# A failed PR-create after a successful push must COMMENT, never abort silently.
if ! gh pr create \
  --repo "${REPO}" \
  --base "${base}" \
  --head "${branch}" \
  --title "mecatequi: changes for issue #${ISSUE_NUMBER}" \
  --body-file "${RUNNER_TEMP}/mecatequi-pr-body.md"; then
  push_failure_comment "the branch was pushed but \`gh pr create\` failed (the bot may lack pull-requests:write)"
  echo "::error::publish: gh pr create failed for ${branch} -> ${base}"
  exit 1
fi

echo "publish: opened PR from ${branch} -> ${base}"
