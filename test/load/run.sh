#!/usr/bin/env bash
# Load test of the Compose environment (docs/LOAD_TEST.md).
#
#   docker compose up --build -d      # wait until the three instances are ready
#   bash test/load/run.sh             # or: make loadtest
#
# Prints one Markdown report: many independent wallets against a single hot wallet at
# several numbers of workers, and the same load with the deferred coherence triggers
# switched on and off, to show what they cost. Switching them off is only for
# measuring: they are switched back on at the end, and every run ends with the
# reconciliation of every wallet it used.
#
# Settings (environment): CONCURRENCY (32, for the trigger comparison), SWEEP ("1 4 16 64",
# the numbers of workers of the first comparison), DURATION (20s), WARMUP (3s), TARGETS (the
# three instances of the Compose file), KEYCLOAK_URL (http://localhost:8180), POSTGRES_PORT
# (5432) or DATABASE_OWNER_URL, to read the outbox exactly, and DRAIN (45s), how long a run
# waits for the outbox to catch up before the next one starts; what is left then is marked as
# published (test data) and reported, so that every run starts from an empty outbox.
set -euo pipefail

cd "$(dirname "$0")/../.."

CONCURRENCY="${CONCURRENCY:-32}"
DURATION="${DURATION:-20s}"
WARMUP="${WARMUP:-3s}"
TARGETS="${TARGETS:-http://localhost:8081,http://localhost:8082,http://localhost:8083}"
KEYCLOAK_URL="${KEYCLOAK_URL:-http://localhost:8180}"
LOADTEST_BIN="$(mktemp -d)/loadtest"
SWEEP="${SWEEP:-1 4 16 64}"
DRAIN="${DRAIN:-45s}"
POSTGRES_PORT="${POSTGRES_PORT:-5432}"
DATABASE_OWNER_URL="${DATABASE_OWNER_URL:-postgres://wagering_owner:owner_local_pw@localhost:${POSTGRES_PORT}/wagering?sslmode=disable}"

psql_owner() {
  docker compose exec -T postgres psql -q -U wagering_owner -d wagering -v ON_ERROR_STOP=1 -c "$1"
}

triggers_on() {
  psql_owner "ALTER TABLE wallets ENABLE TRIGGER wallets_ledger_guard; ALTER TABLE wallet_ledger_entries ENABLE TRIGGER wallet_ledger_entries_wallet_guard;" >/dev/null
}
triggers_off() {
  psql_owner "ALTER TABLE wallets DISABLE TRIGGER wallets_ledger_guard; ALTER TABLE wallet_ledger_entries DISABLE TRIGGER wallet_ledger_entries_wallet_guard;" >/dev/null
}
# Whatever happens, leave the schema as it was.
trap triggers_on EXIT

go build -o "${LOADTEST_BIN}" ./cmd/loadtest

echo "## Environment"
echo
echo "- date: $(date -u +%Y-%m-%dT%H:%MZ)"
echo "- machine: $(uname -sm), $(sysctl -n hw.ncpu 2>/dev/null || nproc) logical CPUs"
echo "- $(go version)"
echo "- Docker: $(docker version --format '{{.Server.Platform.Name}} {{.Server.Version}}' 2>/dev/null || echo unknown)"
echo "- instances: ${TARGETS}"
echo "- workers: ${CONCURRENCY}, measured for ${DURATION} after a ${WARMUP} warm-up"
echo
echo "## Results"
echo

run() { # <scenario> <workers> <label>
  "${LOADTEST_BIN}" -targets "$TARGETS" -keycloak "$KEYCLOAK_URL" -database "$DATABASE_OWNER_URL" \
    -scenario "$1" -concurrency "$2" -duration "$DURATION" -warmup "$WARMUP" -drain-timeout "$DRAIN" -discard-backlog -label "$3"
}

triggers_on

echo "## Independent wallets against one hot wallet, by number of workers"
echo
for workers in $SWEEP; do
  run independent "$workers" "Independent wallets, $workers worker(s)"
  run hot "$workers" "One hot wallet, $workers worker(s)"
done

echo "## What the coherence triggers cost"
echo
echo "The same load, $CONCURRENCY workers on independent wallets, with the triggers on, off, on, off, so that"
echo "an effect of order (warm caches, a tired laptop) shows as a difference between two runs of the same kind."
echo
for round in 1 2; do
  triggers_on
  run independent "$CONCURRENCY" "Triggers ON (round $round)"
  triggers_off
  run independent "$CONCURRENCY" "Triggers OFF, for measurement only (round $round)"
done
triggers_on
