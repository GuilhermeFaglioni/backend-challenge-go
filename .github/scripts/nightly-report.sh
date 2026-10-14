#!/usr/bin/env bash
# Reports a failed nightly run: opens an issue, or comments on the one that is
# still open so repeated failures do not pile up duplicates. Reports nothing
# when no layer failed (cancelled and skipped layers are not failures).
#
# Environment (all produced by the workflow, none user-controlled text):
#   RUN_URL                          link to the workflow run
#   INTEGRATION_RESULT               result of the integration job
#   MULTI_INSTANCE_RESULT            result of the multi-instance job
#   INTEGRATION_SEED, MULTI_INSTANCE_SEED   -shuffle seed of each job, if known
#   GH_TOKEN, GH_REPO                used by the gh CLI
set -euo pipefail

: "${RUN_URL:?RUN_URL is required}"
: "${INTEGRATION_RESULT:?INTEGRATION_RESULT is required}"
: "${MULTI_INSTANCE_RESULT:?MULTI_INSTANCE_RESULT is required}"
INTEGRATION_SEED="${INTEGRATION_SEED-}"
MULTI_INSTANCE_SEED="${MULTI_INSTANCE_SEED-}"

label=nightly-failure
today="$(date -u +%F)"

failed=""
layers=""
add_failed() { # <layer name> <seed>
  failed+="- **$1** (shuffle seed: ${2:-unknown}). Reproduce with \`-shuffle=${2:-<seed>}\`."$'\n'
  layers+="${layers:+, }$1"
}
[ "$INTEGRATION_RESULT" = failure ] && add_failed "Integration" "$INTEGRATION_SEED"
[ "$MULTI_INSTANCE_RESULT" = failure ] && add_failed "Multi-instance" "$MULTI_INSTANCE_SEED"

if [ -z "$failed" ]; then
  echo "No layer failed: nothing to report."
  exit 0
fi

body="$(mktemp)"
trap 'rm -f "$body"' EXIT
{
  echo "The nightly run of ${today} failed in:"
  echo
  printf '%s' "$failed"
  echo
  echo "Run: ${RUN_URL}"
  echo
  echo "The logs of every container and application instance are attached to the run as artifacts for 7 days."
  echo "All repetitions of one run (-count) share the seed above: it fixes the order of the tests, the repetitions exercise the same order again."
  echo "Failures that do not reproduce are not noise to retry away: in a concurrency test they are a possible race."
} > "$body"

# Idempotent: creates the label the first time, updates it afterwards.
gh label create "$label" --color B60205 --description "The nightly run failed" --force >/dev/null

open_issue="$(gh issue list --label "$label" --state open --json number --jq '.[0].number // empty')"
if [ -n "$open_issue" ]; then
  gh issue comment "$open_issue" --body-file "$body"
else
  gh issue create --title "Nightly failed (${today}): ${layers}" --label "$label" --body-file "$body"
fi
