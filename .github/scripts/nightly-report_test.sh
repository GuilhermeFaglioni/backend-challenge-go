#!/usr/bin/env bash
# Self-test for nightly-report.sh, with a fake `gh` that records its calls.
# Run by the CI "static" job and locally with
# `bash .github/scripts/nightly-report_test.sh`.
# No -e on purpose: every failure is counted and reported, not just the first.
set -uo pipefail

here="$(cd "$(dirname "$0")" && pwd)"
failures=0
work="$(mktemp -d)"
trap 'rm -rf "$work"' EXIT

# The fake gh answers `issue list` from STUB_OPEN_ISSUE and records every call
# (arguments, then the body file when there is one) in $work/calls.
mkdir -p "$work/bin"
cat > "$work/bin/gh" <<'STUB'
#!/usr/bin/env bash
echo "gh $*" >> "$STUB_CALLS"
if [ "$1 $2" = "issue list" ]; then
  echo "${STUB_OPEN_ISSUE:-}"
  exit 0
fi
# Keep what would be posted: the argument after --body-file.
while [ $# -gt 0 ]; do
  if [ "$1" = "--body-file" ]; then
    { echo "--- body"; cat "$2"; echo "--- end"; } >> "$STUB_CALLS"
  fi
  shift
done
exit 0
STUB
chmod +x "$work/bin/gh"

# report <open issue number or ""> <integration result> <multi result> [seeds...]
report() {
  : > "$work/calls"
  PATH="$work/bin:$PATH" STUB_CALLS="$work/calls" STUB_OPEN_ISSUE="$1" \
    RUN_URL="https://github.com/o/r/actions/runs/42" \
    INTEGRATION_RESULT="$2" MULTI_INSTANCE_RESULT="$3" \
    INTEGRATION_SEED="${4-}" MULTI_INSTANCE_SEED="${5-}" \
    bash "$here/nightly-report.sh" >"$work/out" 2>&1
}

# expect <description> <pattern that must appear in the calls>
expect_call() {
  if ! grep -qE -- "$2" "$work/calls"; then
    echo "FAIL: $1"
    echo "  wanted a call matching: $2"
    sed 's/^/    | /' "$work/calls"
    failures=$((failures + 1))
  fi
}
expect_no_call() {
  if grep -qE -- "$2" "$work/calls"; then
    echo "FAIL: $1"
    echo "  did not want a call matching: $2"
    sed 's/^/    | /' "$work/calls"
    failures=$((failures + 1))
  fi
}

# Everything green: nothing is reported, not even a label.
report "" success success
expect_no_call "no failure means no call at all" "^gh "

# A failure with no open nightly issue opens one that names the layer, the run
# and the seed.
report "" failure success 1791996195940080000 ""
expect_call "opens an issue" "^gh issue create"
expect_call "looks only at open issues of the nightly label" "^gh issue list .*--label nightly-failure.*--state open|^gh issue list .*--state open.*--label nightly-failure"
expect_call "labels it" "--label nightly-failure"
expect_call "puts the date and the layer in the title" "--title Nightly failed \([0-9]{4}-[0-9]{2}-[0-9]{2}\): Integration( |$)"
expect_call "names the failing layer" "Integration"
expect_call "links the run" "https://github.com/o/r/actions/runs/42"
expect_call "states the shuffle seed" "1791996195940080000"
expect_no_call "does not name a layer that passed" "Multi-instance"
expect_no_call "does not comment when none is open" "^gh issue comment"

# The next failure while that issue is still open comments instead of opening a
# duplicate.
report "17" success failure "" 987654321
expect_call "comments on the open issue" "^gh issue comment 17"
expect_call "names the failing layer in the comment" "Multi-instance"
expect_call "states the seed in the comment" "shuffle seed: 987654321"
expect_no_call "does not open a duplicate" "^gh issue create"

# Both layers failing are both reported, in one issue.
report "" failure failure 1 2
expect_call "names the first layer" "Integration"
expect_call "names the second layer" "Multi-instance"
expect_call "lists both layers in the title" "--title Nightly failed \([0-9-]+\): Integration, Multi-instance"
[ "$(grep -c '^gh issue create' "$work/calls")" -eq 1 ] || { echo "FAIL: both layers must share one issue"; failures=$((failures + 1)); }

# A missing seed is said so rather than printed as an empty value.
report "" failure success ""
expect_call "admits an unknown seed" "unknown"

# Cancelled or skipped layers are not failures.
report "" cancelled skipped
expect_no_call "cancelled/skipped is not reported" "^gh "

if [ "$failures" -ne 0 ]; then
  echo "$failures failure(s)"
  exit 1
fi
echo "nightly-report self-test passed"
