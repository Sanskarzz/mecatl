#!/usr/bin/env bash
# extract-prompt_test.sh — OFFLINE bash tests for extract-prompt.sh. No bats dependency;
# plain bash + a fixture $GITHUB_EVENT_PATH JSON. Driven by `task test:actions`.
#
# THE SECURITY PROPERTY UNDER TEST. extract-prompt.sh reads the UNTRUSTED issue/comment
# text via jq over the event JSON FILE and writes it to a file VERBATIM — it must NEVER
# interpolate the body into a shell command (that would let an attacker's $(...) / backticks
# in the issue body execute). The injection cases below put shell-injection payloads in the
# issue body and assert the LITERAL text is written, never the result of executing it. These
# are written so that if the jq-over-file extraction were replaced by a shell interpolation
# of the body, the test would FAIL (the payload would execute / expand instead of landing
# verbatim). Per repo convention we use INNOCUOUS payloads — a marker file the injection
# would create, never a destructive command.
set -euo pipefail

HERE="$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)"
SCRIPT="${HERE}/extract-prompt.sh"

fail=0
note() { printf '%s\n' "$*" >&2; }
pass() { note "  ok: $1"; }
bad() { note "  FAIL: $1"; fail=1; }

# A throwaway sandbox (under the repo-local .scratch, never /tmp). .scratch/ is gitignored,
# so it is ABSENT in a fresh clone — the mktemp below would then abort under `set -euo
# pipefail` (the whole test silently never runs in CI). Create it up front.
ROOT="$(cd "${HERE}/../../.." && pwd)"
mkdir -p "${ROOT}/.scratch"
WORK="$(mktemp -d "${ROOT}/.scratch/extract-prompt-test.XXXXXX")"
trap 'rm -rf "${WORK}"' EXIT

# Write a fixture event JSON and return its path. $1 = json body.
write_event() {
  local path="${WORK}/event-$RANDOM.json"
  printf '%s' "$1" > "${path}"
  printf '%s' "${path}"
}

# ── Test 1: a plain issue event assembles title + body ───────────────────────────────────
test_basic_assembly() {
  local ev out
  ev="$(write_event '{"issue":{"number":42,"title":"Fix the thing","body":"Please fix the widget."}}')"
  out="${WORK}/out1.txt"
  GITHUB_EVENT_PATH="${ev}" "${SCRIPT}" "${out}" >/dev/null 2>&1
  if grep -q "# Issue #42: Fix the thing" "${out}" && grep -q "Please fix the widget." "${out}"; then
    pass "basic assembly writes title + body"
  else
    bad "basic assembly missing title or body"
  fi
}

# ── Test 2: an issue_comment event appends the comment section ────────────────────────────
test_comment_section() {
  local ev out
  ev="$(write_event '{"issue":{"number":7,"title":"T","body":"B"},"comment":{"body":"trigger comment text"}}')"
  out="${WORK}/out2.txt"
  GITHUB_EVENT_PATH="${ev}" "${SCRIPT}" "${out}" >/dev/null 2>&1
  if grep -q "## Triggering comment" "${out}" && grep -q "trigger comment text" "${out}"; then
    pass "comment event appends the triggering-comment section"
  else
    bad "comment event did not append the comment section"
  fi
}

# ── Test 3 (SECURITY): a $(...) payload in the body is written LITERALLY, never executed ──
# If the script interpolated the body into the shell, the $(...) would run and create the
# marker file; the literal text would NOT appear. We assert the inverse: the literal
# substring is present AND the marker file was NOT created.
test_command_substitution_not_executed() {
  local ev out marker
  marker="${WORK}/INJECTED_MARKER"
  rm -f "${marker}"
  # The payload, as it would appear in an attacker's issue body. Innocuous: it would only
  # `touch` a marker file if executed. We build it without letting THIS test's own shell
  # expand it (single quotes), then embed it in JSON via jq so quoting is correct.
  local payload
  payload='evil $(touch '"${marker}"') tail'
  ev="$(jq -n --arg b "${payload}" '{issue:{number:1,title:"x",body:$b}}' > "${WORK}/ev3.json"; printf '%s' "${WORK}/ev3.json")"
  out="${WORK}/out3.txt"
  GITHUB_EVENT_PATH="${ev}" "${SCRIPT}" "${out}" >/dev/null 2>&1
  if [ -e "${marker}" ]; then
    bad "command-substitution payload EXECUTED (marker file created) — the body was interpolated"
  elif grep -qF 'evil $(touch' "${out}"; then
    pass "command-substitution payload written literally, never executed"
  else
    bad "command-substitution payload neither executed nor written literally (unexpected)"
  fi
}

# ── Test 4 (SECURITY): a ${VAR} / backtick payload is written LITERALLY ──────────────────
test_var_and_backtick_literal() {
  local ev out marker
  marker="${WORK}/INJECTED_BACKTICK"
  rm -f "${marker}"
  local payload
  # shellcheck disable=SC2016
  payload='look: `touch '"${marker}"'` and ${HOME} stays literal'
  jq -n --arg b "${payload}" '{issue:{number:2,title:"y",body:$b}}' > "${WORK}/ev4.json"
  ev="${WORK}/ev4.json"
  out="${WORK}/out4.txt"
  GITHUB_EVENT_PATH="${ev}" "${SCRIPT}" "${out}" >/dev/null 2>&1
  if [ -e "${marker}" ]; then
    bad "backtick payload EXECUTED — the body was interpolated"
  elif grep -qF '${HOME} stays literal' "${out}"; then
    pass "backtick + \${VAR} payload written literally, never expanded"
  else
    bad "var/backtick payload was altered (not written verbatim)"
  fi
}

# ── Test 5: a missing GITHUB_EVENT_PATH fails loudly (non-zero), never silent ─────────────
test_missing_event_path_fails() {
  local out rc
  out="${WORK}/out5.txt"
  set +e
  GITHUB_EVENT_PATH="${WORK}/does-not-exist.json" "${SCRIPT}" "${out}" >/dev/null 2>&1
  rc=$?
  set -e
  if [ "${rc}" -ne 0 ]; then
    pass "missing event JSON exits non-zero"
  else
    bad "missing event JSON did not fail"
  fi
}

note "extract-prompt.sh tests:"
test_basic_assembly
test_comment_section
test_command_substitution_not_executed
test_var_and_backtick_literal
test_missing_event_path_fails

if [ "${fail}" -ne 0 ]; then
  note "extract-prompt.sh: FAILURES"
  exit 1
fi
note "extract-prompt.sh: all tests passed"
