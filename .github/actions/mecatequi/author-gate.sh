#!/usr/bin/env bash
# author-gate.sh — defense-in-depth author-association gate.
#
# The triggering actor's author_association MUST be one of OWNER, MEMBER, or COLLABORATOR
# for an agent run to proceed. This is the SECOND line of defence behind the job `if:`
# gate in the workflow — the `if:` keeps the job from starting at all, and this script
# re-asserts the invariant inside the job so a misconfigured/edited `if:` cannot silently
# open the door to a drive-by issue from a random account.
#
# The value is fed via the ASSOC env var (set from ${{ github.event.*.author_association }}
# in the workflow), never as an argv token, and is matched with an exact `case` — anything
# outside the allowlist (CONTRIBUTOR, FIRST_TIME_CONTRIBUTOR, NONE, MANNEQUIN, an empty
# string) exits non-zero and stops the job.
set -euo pipefail

assoc="${ASSOC:-}"

case "${assoc}" in
  OWNER | MEMBER | COLLABORATOR)
    echo "author-gate: association '${assoc}' is allowed" >&2
    ;;
  *)
    echo "::error::author-gate: association '${assoc}' is not authorised to run mecatequi (allowed: OWNER, MEMBER, COLLABORATOR)" >&2
    exit 1
    ;;
esac
