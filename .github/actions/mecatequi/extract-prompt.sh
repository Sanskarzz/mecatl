#!/usr/bin/env bash
# extract-prompt.sh — write the RAW untrusted prompt body from the triggering GitHub
# event to the file path given as $1, using jq over $GITHUB_EVENT_PATH.
#
# INJECTION SAFETY (the whole point): the issue title/body (and, on issue_comment, the
# comment body) are UNTRUSTED attacker-controllable text. They MUST NOT pass through a
# ${{ }} expression (which would splice them into the shell before execution) or an argv
# token. jq reads the event JSON FILE and writes the body to a file; the body then reaches
# mecatequi only via --prompt-file, where the binary fences it as untrusted data. Nothing
# here interpolates the body into a command.
#
# Usage: extract-prompt.sh <output-file>
set -euo pipefail

out="${1:?usage: extract-prompt.sh <output-file>}"

if [ -z "${GITHUB_EVENT_PATH:-}" ] || [ ! -f "${GITHUB_EVENT_PATH}" ]; then
  echo "extract-prompt: GITHUB_EVENT_PATH is unset or missing" >&2
  exit 1
fi

# Assemble the prompt from the event JSON. The issue title + body are always present on
# issues/issue_comment events; .comment.body is present only on issue_comment (jq emits
# nothing for an absent field, so the comment section is added only when it exists). All
# reads are jq over the event FILE — no shell interpolation of any field.
jq -r '
  ( "# Issue #" + (.issue.number | tostring) + ": " + (.issue.title // "") ) ,
  "" ,
  ( .issue.body // "" ) ,
  ( if .comment.body then ("", "## Triggering comment", "", .comment.body) else empty end )
' "${GITHUB_EVENT_PATH}" > "${out}"

echo "extract-prompt: wrote $(wc -c < "${out}") bytes to ${out}" >&2
