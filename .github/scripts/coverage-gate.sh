#!/usr/bin/env bash
# Coverage gate: every package under internal/domain/ must have at least
# THRESHOLD percent statement coverage. Packages elsewhere never fail the gate.
#
# Reads the output of `go tool covdata percent` on stdin, one package per line:
#   <TAB>github.com/o/r/internal/domain/money<TAB><TAB>coverage: 92.3% of statements
#
# While no domain package has statements yet (they only hold documentation),
# there is nothing to measure: the gate passes and says so, instead of failing
# or reporting a misleading 0%.
#
# Environment: THRESHOLD (default 90).
set -euo pipefail

THRESHOLD="${THRESHOLD:-90}"
[[ "$THRESHOLD" =~ ^[0-9]+(\.[0-9]+)?$ ]] || { echo "coverage-gate: THRESHOLD must be a number, got: $THRESHOLD" >&2; exit 2; }

# Packages internal/domain and internal/domain/...: the path has an
# internal/domain segment, not merely a name that starts with it (domainx).
domain_pattern='/internal/domain([[:space:]/]|$)'

checked=0
failed=0
while IFS= read -r line; do
  [[ "$line" =~ $domain_pattern ]] || continue

  # The line starts with a tab: the package is its first non-empty field.
  package="$(awk '{print $1}' <<<"$line")"

  if [[ ! "$line" =~ coverage:\ ([0-9]+(\.[0-9]+)?)%\ of\ statements ]]; then
    echo "coverage-gate: cannot read the coverage of ${package} from: ${line}" >&2
    exit 1
  fi
  percent="${BASH_REMATCH[1]}"
  checked=$((checked + 1))

  if awk -v p="$percent" -v t="$THRESHOLD" 'BEGIN { exit !(p + 0 < t + 0) }'; then
    echo "FAIL ${package}: ${percent}% is below the ${THRESHOLD}% required for the domain"
    failed=$((failed + 1))
  else
    echo "ok   ${package}: ${percent}% (required ${THRESHOLD}%)"
  fi
done

if [ "$checked" -eq 0 ]; then
  echo "Domain coverage gate is not yet in effect: no package under internal/domain has statements."
  exit 0
fi
[ "$failed" -eq 0 ]
