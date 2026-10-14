#!/usr/bin/env bash
# Merges the coverage data of the test layers into one report, writes it as
# Markdown, and applies the domain gate (coverage-gate.sh).
#
# Each layer leaves binary coverage data (GOCOVERDIR format) in its own
# directory: the unit and integration tests through `go test -cover
# -coverpkg=... -args -test.gocoverdir=DIR`, and the multi-instance processes
# through GOCOVERDIR. Merging them is what makes code that only one layer runs
# (for example the main package, which only the real processes execute) count.
#
# Environment:
#   COVER_DIRS    space-separated list of coverage directories, one per layer
#   SUMMARY_FILE  where the Markdown goes. Defaults to $GITHUB_STEP_SUMMARY (the
#                 run summary page, set by the Actions runner), else standard output
#   THRESHOLD     domain threshold, passed to coverage-gate.sh (default 90)
set -euo pipefail

: "${COVER_DIRS:?COVER_DIRS is required}"
SUMMARY_FILE="${SUMMARY_FILE:-${GITHUB_STEP_SUMMARY:-/dev/stdout}}"
here="$(cd "$(dirname "$0")" && pwd)"

# A layer that wrote nothing is a broken pipeline, not a layer with 0%: fail.
dirs=()
for dir in $COVER_DIRS; do
  if ! compgen -G "$dir/covcounters.*" >/dev/null; then
    echo "coverage-report: no coverage data in ${dir}" >&2
    exit 1
  fi
  dirs+=("$dir")
done

work="$(mktemp -d)"
trap 'rm -rf "$work"' EXIT

joined="$(IFS=,; echo "${dirs[*]}")"
mkdir "$work/merged"
go tool covdata merge -i="$joined" -o="$work/merged"

module="$(go list -m)"

# total <covdata dir>: statement coverage of everything in it, e.g. "62.9%".
total() {
  go tool covdata textfmt -i="$1" -o="$work/profile.txt"
  go tool cover -func="$work/profile.txt" | awk '/^total:/ { print $NF }'
}

combined_total="$(total "$work/merged")"
percent="$(go tool covdata percent -i="$work/merged")"

# A domain package that has statements but that no layer ever ran (no test
# imports it, no binary links it) is absent from the merged data, so the gate
# would never see it. Go itself knows which packages have statements: it prints
# a coverage line for each of them, and none for packages that only document.
# Anything it lists that the merged data lacks was executed by nobody: 0%.
while IFS= read -r package; do
  [ -n "$package" ] || continue
  if ! grep -qF -- "$package"$'\t' <<<"$percent"; then
    percent+=$'\n'"$(printf '\t%s\t\tcoverage: 0.0%% of statements' "$package")"
  fi
done < <(go test -cover -run '^$' ./internal/domain/... 2>&1 \
  | grep 'coverage:' | grep -oE '[^[:space:]]*/internal/domain[^[:space:]]*' | sort -u)

{
  echo "## Test coverage"
  echo
  echo "Combined from: unit tests, integration tests and the multi-instance processes."
  echo
  echo "**Total: ${combined_total}**"
  echo
  echo "| Package | Coverage (combined) |"
  echo "| --- | --- |"
  while IFS= read -r line; do
    [ -n "$line" ] || continue
    package="$(awk '{print $1}' <<<"$line")"
    value="$(grep -oE '[0-9]+(\.[0-9]+)?%' <<<"$line")"
    echo "| \`${package#"$module"/}\` | ${value} |"
  done <<<"$percent"
  echo
} >> "$SUMMARY_FILE"

# The gate decides the exit status, but its verdict is written to the summary
# first, so a failing run still explains itself on the run page.
gate_output="$work/gate.txt"
status=0
bash "$here/coverage-gate.sh" <<<"$percent" >"$gate_output" 2>&1 || status=$?
{
  echo "### Domain gate (internal/domain, at least ${THRESHOLD:-90}%)"
  echo
  echo '```'
  cat "$gate_output"
  echo '```'
} >> "$SUMMARY_FILE"
cat "$gate_output" >&2
# Annotate failures on the run page when running under GitHub Actions.
if [ "$status" -ne 0 ] && [ -n "${GITHUB_ACTIONS:-}" ]; then
  grep '^FAIL' "$gate_output" | while IFS= read -r failure; do echo "::error title=Domain coverage::${failure}"; done
fi
exit "$status"
