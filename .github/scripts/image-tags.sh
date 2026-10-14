#!/usr/bin/env bash
# Prints the image references to publish, one per line.
#
#   workflow_run (CI green on main)  ->  sha-<short>, plus latest when MOVE_LATEST=true
#   push of a tag vX.Y.Z             ->  sha-<short>, X.Y.Z and X.Y   (never latest)
#
# Environment:
#   EVENT_NAME   workflow_run | push
#   COMMIT_SHA   full SHA of the commit being published (for workflow_run this
#                is the commit CI validated, not GITHUB_SHA, which can be newer)
#   REF_NAME     the tag name for push events; ignored for workflow_run
#   IMAGE        registry path without a tag, e.g. ghcr.io/Owner/Repo
#   MOVE_LATEST  workflow_run only: "true" when COMMIT_SHA is still the head of
#                main. Two quick merges can finish CI out of order, and a re-run
#                of an old commit must not move latest backwards.
#
# REF_NAME is user-controlled (anyone who can push a tag chooses it), so it is
# only ever matched against a strict pattern and never evaluated.
set -euo pipefail

fail() {
  echo "image-tags: $*" >&2
  exit 1
}

: "${EVENT_NAME:?EVENT_NAME is required}"
: "${COMMIT_SHA:?COMMIT_SHA is required}"
: "${IMAGE:?IMAGE is required}"
REF_NAME="${REF_NAME-}"
MOVE_LATEST="${MOVE_LATEST-false}"

[[ "$COMMIT_SHA" =~ ^[0-9a-f]{40}$ ]] || fail "COMMIT_SHA must be a full lowercase commit SHA"

# Registries require lowercase repository names; GitHub owner names may not be.
image="$(printf '%s' "$IMAGE" | tr '[:upper:]' '[:lower:]')"

printf '%s:sha-%s\n' "$image" "${COMMIT_SHA:0:7}"

case "$EVENT_NAME" in
  workflow_run)
    case "$MOVE_LATEST" in
      true) printf '%s:latest\n' "$image" ;;
      false) ;;
      *) fail "MOVE_LATEST must be true or false" ;;
    esac
    ;;
  push)
    if [[ "$REF_NAME" =~ ^v([0-9]+)\.([0-9]+)\.([0-9]+)$ ]]; then
      printf '%s:%s.%s.%s\n' "$image" "${BASH_REMATCH[1]}" "${BASH_REMATCH[2]}" "${BASH_REMATCH[3]}"
      printf '%s:%s.%s\n' "$image" "${BASH_REMATCH[1]}" "${BASH_REMATCH[2]}"
    else
      fail "tag must look like vX.Y.Z"
    fi
    ;;
  *)
    fail "unsupported event: ${EVENT_NAME}"
    ;;
esac
