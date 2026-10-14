#!/usr/bin/env bash
# Self-test for apply-rulesets.sh, with a fake `gh` that records its calls.
# The property that matters most: without --apply nothing is changed.
# Run by the CI "static" job and locally with
# `bash .github/scripts/apply-rulesets_test.sh`.
# No -e on purpose: every failure is counted and reported, not just the first.
set -uo pipefail

here="$(cd "$(dirname "$0")" && pwd)"
failures=0
work="$(mktemp -d)"
trap 'rm -rf "$work"' EXIT

# The fake gh records every call. `repo view` answers the repository name;
# `api .../rulesets` (a listing) answers from STUB_EXISTING (a JSON array).
mkdir -p "$work/bin"
cat > "$work/bin/gh" <<'STUB'
#!/usr/bin/env bash
echo "gh $*" >> "$STUB_CALLS"
if [ "$1 $2" = "repo view" ]; then
  if [[ "$*" == *defaultBranchRef* ]]; then echo "main"; else echo "owner/repo"; fi
  exit 0
fi
# What GitHub reports as enforced on the branch (a space-separated list of types).
if [ "$1" = "api" ] && [[ "$2" == repos/*/rules/branches/* ]]; then
  echo "${STUB_ENFORCED:-pull_request required_status_checks required_linear_history non_fast_forward deletion}"
  exit 0
fi
if [ "$1" = "api" ] && [[ "$*" == *"/rulesets"* ]] && [[ "$*" != *"-X"* ]] && [[ "$*" != *"--input"* ]]; then
  echo "${STUB_EXISTING:-[]}"
  exit 0
fi
exit 0
STUB
chmod +x "$work/bin/gh"

# run <existing rulesets json> [args...]
run() {
  local existing="$1"
  shift
  : > "$work/calls"
  PATH="$work/bin:$PATH" STUB_CALLS="$work/calls" STUB_EXISTING="$existing" \
    bash "$here/apply-rulesets.sh" "$@" >"$work/out" 2>&1
}

expect_call() {
  if ! grep -qE -- "$2" "$work/calls"; then
    echo "FAIL: $1"
    echo "  wanted a call matching: $2"
    echo "    | ${3:-}$(tr '\n' ';' < "$work/calls")"
    failures=$((failures + 1))
  fi
}
expect_no_call() {
  if grep -qE -- "$2" "$work/calls"; then
    echo "FAIL: $1"
    echo "  did not want a call matching: $2"
    echo "    | $(tr '\n' ';' < "$work/calls")"
    failures=$((failures + 1))
  fi
}

# Dry run: reads only, and says what it would do.
run '[]'
expect_no_call "dry run never changes the repository settings" "-X (PATCH|PUT|POST|DELETE)"
expect_no_call "dry run never sends a body" "--input"
if ! grep -qi "dry run" "$work/out"; then
  echo "FAIL: dry run must say it is a dry run"
  failures=$((failures + 1))
fi
if ! grep -q -- "--apply" "$work/out"; then
  echo "FAIL: dry run must say how to apply"
  failures=$((failures + 1))
fi

# Apply on an empty repository: settings patched, both rulesets created.
run '[]' --apply
expect_call "patches the merge settings" "api -X PATCH repos/owner/repo "
expect_call "allows only squash merge" "allow_merge_commit=false"
expect_call "deletes merged branches" "delete_branch_on_merge=true"
expect_call "creates the main ruleset" "api -X POST repos/owner/repo/rulesets --input .*main\.json"
expect_call "creates the tags ruleset" "api -X POST repos/owner/repo/rulesets --input .*tags\.json"
expect_no_call "creates rather than updates when none exist" "-X PUT"

# Apply when a ruleset with that name exists: updated in place, no duplicate.
run '[{"id":123,"name":"main"}]' --apply
expect_call "updates the existing main ruleset" "api -X PUT repos/owner/repo/rulesets/123 --input .*main\.json"
expect_call "still creates the missing tags ruleset" "api -X POST repos/owner/repo/rulesets --input .*tags\.json"
if [ "$(grep -c 'rulesets --input .*main\.json' "$work/calls")" -ne 0 ]; then
  echo "FAIL: must not create a duplicate main ruleset"
  failures=$((failures + 1))
fi

# After applying it asks GitHub what is enforced, and fails when a rule is missing.
run '[]' --apply
expect_call "verifies the enforced rules after applying" "api repos/owner/repo/rules/branches/main"
if ! grep -q "Verified" "$work/out"; then
  echo "FAIL: a successful apply must report that it verified the rules"
  failures=$((failures + 1))
fi
if STUB_ENFORCED="deletion" run '[]' --apply; then
  echo "FAIL: apply must fail when the branch does not enforce the required rules"
  failures=$((failures + 1))
fi
if ! grep -q "does not enforce" "$work/out"; then
  echo "FAIL: the failure must say which rule is missing"
  failures=$((failures + 1))
fi

# A dry run does not verify anything (nothing was applied).
run '[]'
expect_no_call "dry run does not query the enforced rules" "rules/branches"

# A missing tool is reported before any change is attempted: with a PATH that
# has the fake gh but no jq, --apply must stop without a single gh call.
mkdir -p "$work/nojq"
ln -sf "$work/bin/gh" "$work/nojq/gh"
: > "$work/calls"
if env -i PATH="$work/nojq" STUB_CALLS="$work/calls" /bin/bash "$here/apply-rulesets.sh" --apply >"$work/out" 2>&1; then
  echo "FAIL: --apply must fail when jq is missing"
  failures=$((failures + 1))
fi
if ! grep -q "jq is required" "$work/out"; then
  echo "FAIL: the missing tool must be named"
  failures=$((failures + 1))
fi
expect_no_call "nothing is attempted when a tool is missing" "^gh "

# Anything but --apply is a dry run, and unknown arguments are refused.
if run '[]' --yes; then
  echo "FAIL: an unknown argument must be refused"
  failures=$((failures + 1))
fi
expect_no_call "an unknown argument changes nothing" "-X (PATCH|PUT|POST|DELETE)"

if [ "$failures" -ne 0 ]; then
  echo "$failures failure(s)"
  exit 1
fi
echo "apply-rulesets self-test passed"
