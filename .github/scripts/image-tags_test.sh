#!/usr/bin/env bash
# Self-test for image-tags.sh. Run by the CI "static" job and locally with
# `bash .github/scripts/image-tags_test.sh`.
# No -e on purpose: every failure is counted and reported, not just the first.
set -uo pipefail

here="$(cd "$(dirname "$0")" && pwd)"
failures=0

commit=0123456789abcdef0123456789abcdef01234567
base=ghcr.io/owner/repo

# run <event> <ref-name> <move-latest> [commit]: sets $out (stdout, one line)
# and $err (stderr), and returns the script's exit status.
run() {
  local errfile status
  errfile="$(mktemp)"
  out="$(EVENT_NAME="$1" REF_NAME="$2" MOVE_LATEST="$3" COMMIT_SHA="${4:-$commit}" IMAGE=ghcr.io/Owner/Repo \
    bash "$here/image-tags.sh" 2>"$errfile" | tr '\n' ' ')"
  status="${PIPESTATUS[0]}"
  err="$(cat "$errfile")"
  rm -f "$errfile"
  return "$status"
}

# expect_tags <description> <expected tags> <event> <ref-name> <move-latest>
expect_tags() {
  if ! run "$3" "$4" "$5"; then
    echo "FAIL: $1 (script failed: $err)"
    failures=$((failures + 1))
  elif [ "$out" != "$2" ]; then
    echo "FAIL: $1"
    echo "  want: $2"
    echo "  got:  $out"
    failures=$((failures + 1))
  fi
}

# expect_error <description> <event> <ref-name> <move-latest> [commit]: the
# script must fail with its own message (not for an unrelated reason) and
# publish nothing.
expect_error() {
  local description="$1"
  shift
  if run "$@"; then
    echo "FAIL: $description (expected an error, got: $out)"
    failures=$((failures + 1))
  elif [[ "$err" != image-tags:* ]]; then
    echo "FAIL: $description (failed, but not with an image-tags message: $err)"
    failures=$((failures + 1))
  fi
}

expect_tags "main at the head publishes sha and latest (image name lowercased)" \
  "$base:sha-0123456 $base:latest " workflow_run "" true
expect_tags "an older commit publishes only the sha tag, never moving latest" \
  "$base:sha-0123456 " workflow_run "" false
expect_tags "a release tag publishes sha, X.Y.Z and X.Y, never latest" \
  "$base:sha-0123456 $base:1.2.3 $base:1.2 " push v1.2.3 true
expect_tags "multi-digit version parts" \
  "$base:sha-0123456 $base:10.20.30 $base:10.20 " push v10.20.30 false

# Anything that is not a plain vX.Y.Z tag must publish nothing.
expect_error "pre-release tag" push v1.2.3-rc1 false
expect_error "tag without v" push 1.2.3 false
expect_error "branch name" push main false
expect_error "full ref instead of a tag name" push refs/tags/v1.2.3 false
expect_error "prefixed tag (anchor must hold)" push xv1.2.3 false
expect_error "suffixed tag (anchor must hold)" push v1.2.3x false
expect_error "tag with a newline and an injected line" push $'v1.2.3\nTAGS_END' false
# shellcheck disable=SC2016 # the single quotes are the point: literal shell syntax
expect_error "tag with shell syntax" push 'v1.2.3$(touch x)' false
expect_error "unknown event" pull_request "" false
expect_error "short commit sha" workflow_run "" true abc1234
expect_error "uppercase commit sha" workflow_run "" true 0123456789ABCDEF0123456789ABCDEF01234567
expect_error "invalid MOVE_LATEST" workflow_run "" maybe

if [ "$failures" -ne 0 ]; then
  echo "$failures failure(s)"
  exit 1
fi
echo "image-tags self-test passed"
