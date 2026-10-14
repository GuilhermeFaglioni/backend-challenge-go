#!/usr/bin/env bash
# Self-test for pr-title.sh. Run by the PR title workflow before the real
# check, and locally with `bash .github/scripts/pr-title_test.sh`.
# No -e on purpose: expect() must keep going after a rejected title so every
# failure is counted and reported, instead of stopping at the first one.
set -uo pipefail

here="$(cd "$(dirname "$0")" && pwd)"
failures=0

# expect <accept|reject> <title>
expect() {
  local want="$1" title="$2" got
  if PR_TITLE="$title" bash "$here/pr-title.sh" >/dev/null 2>&1; then got=accept; else got=reject; fi
  if [ "$got" != "$want" ]; then
    echo "FAIL: expected $want for title: $title"
    failures=$((failures + 1))
  fi
}

# Accepted: every type, optional scope, optional breaking-change marker.
expect accept "feat(wallet): open wallet"
expect accept "fix!: reject negative amounts"
expect accept "fix(money)!: handle overflow on negate"
expect accept "docs: record API documentation decision"
expect accept "chore(deps): bump the go-modules group"
for type in feat fix docs chore test refactor perf ci build; do
  expect accept "$type: something"
done

# Rejected: wrong shape, unknown type, empty pieces.
expect reject "Add stuff"
expect reject "feature: add stuff"
expect reject "Feat: capitalised type"
expect reject "feat:missing space"
expect reject "feat: "
expect reject "feat():  empty scope"
expect reject "feat (wallet): space before scope"
expect reject ""
expect reject "feat(Wallet): uppercase scope"
expect reject $'feat: first line\nsecond line'

# The title is user-controlled text. Shell syntax in it must be treated as
# plain characters: nothing may be executed and nothing may be written.
canary="$(mktemp -u)"
expect accept "feat: \$(touch $canary) \`touch $canary\` ; touch $canary"
expect reject "\$(touch $canary): not a type"
if [ -e "$canary" ]; then
  echo "FAIL: shell syntax in the title was executed"
  rm -f "$canary"
  failures=$((failures + 1))
fi

if [ "$failures" -ne 0 ]; then
  echo "$failures failure(s)"
  exit 1
fi
echo "pr-title self-test passed"
