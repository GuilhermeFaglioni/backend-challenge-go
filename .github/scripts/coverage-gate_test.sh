#!/usr/bin/env bash
# Self-test for coverage-gate.sh. Run by the CI "static" job and locally with
# `bash .github/scripts/coverage-gate_test.sh`.
# No -e on purpose: every failure is counted and reported, not just the first.
set -uo pipefail

here="$(cd "$(dirname "$0")" && pwd)"
failures=0

mod=github.com/o/r

# line <package path> <percent>: one line as `go tool covdata percent` prints it.
line() { printf '\t%s/%s\t\tcoverage: %s%% of statements\n' "$mod" "$1" "$2"; }

# gate <expected exit 0|1> <description> <expected text or ""> <input...>
gate() {
  local want="$1" description="$2" text="$3"
  shift 3
  local out status
  out="$(printf '%s\n' "$@" | bash "$here/coverage-gate.sh" 2>&1)"
  status=$?
  if [ "$status" -ne "$want" ]; then
    echo "FAIL: $description (exit $status, want $want)"
    echo "    | ${out//$'\n'/$'\n'    | }"
    failures=$((failures + 1))
  elif [ -n "$text" ] && ! grep -qF -- "$text" <<<"$out"; then
    echo "FAIL: $description (output lacks: $text)"
    echo "    | ${out//$'\n'/$'\n'    | }"
    failures=$((failures + 1))
  fi
}

gate 0 "no domain statements yet: passes and says the gate is not in effect" "not yet in effect" \
  "$(line internal/app 12.0)" "$(line internal/platform/config 91.4)"

gate 0 "empty input (no coverage at all) is not the gate's business" "not yet in effect" ""

gate 0 "domain packages above the threshold pass" "internal/domain/money" \
  "$(line internal/domain/money 100.0)" "$(line internal/domain/wallet 92.3)"

gate 0 "exactly 90.0 passes" "" \
  "$(line internal/domain/money 90.0)"

gate 1 "a domain package below the threshold fails and shows the percentage" "89.9%" \
  "$(line internal/domain/money 100.0)" "$(line internal/domain/wager 89.9)"

gate 1 "the failing package is named" "internal/domain/wager" \
  "$(line internal/domain/money 100.0)" "$(line internal/domain/wager 12.5)"

gate 0 "low coverage outside the domain never fails" "" \
  "$(line internal/domain/money 95.0)" "$(line internal/adapters/postgres 0.0)" "$(line cmd/api 0.0)"

# internal/domain/... includes the package internal/domain itself.
gate 1 "the root domain package is part of the domain" "internal/domain:" \
  "$(line internal/domain 40.0)"

# Only internal/domain/... counts, not a package that merely starts with "domain".
gate 0 "a look-alike package is not part of the domain" "not yet in effect" \
  "$(line internal/domainx 0.0)" "$(line internal/platform/domain 0.0)"

gate 1 "a domain line that cannot be read fails loudly" "cannot read" \
  "$(printf '\t%s/internal/domain/money\t\tcoverage: n/a of statements' "$mod")"

# A threshold can be overridden by the workflow.
if ! out="$(printf '%s\n' "$(line internal/domain/money 85.0)" | THRESHOLD=80 bash "$here/coverage-gate.sh" 2>&1)"; then
  echo "FAIL: THRESHOLD=80 must accept 85.0"
  echo "$out"
  failures=$((failures + 1))
fi

if [ "$failures" -ne 0 ]; then
  echo "$failures failure(s)"
  exit 1
fi
echo "coverage-gate self-test passed"
