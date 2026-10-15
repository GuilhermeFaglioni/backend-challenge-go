#!/usr/bin/env bash
# Smoke test of the full docker compose environment.
#
# Assumes `docker compose up -d` was already run. Everything is awaited on an
# observable condition with a deadline; there are no fixed sleeps. Exits
# non-zero on the first failed check.
#
# Optional environment: INSTANCE_PORTS (default "8081 8082 8083"),
# KEYCLOAK_PORT (8180), WAIT_SECONDS (180).
set -euo pipefail

INSTANCE_PORTS="${INSTANCE_PORTS:-8081 8082 8083}"
KEYCLOAK_PORT="${KEYCLOAK_PORT:-8180}"
WAIT_SECONDS="${WAIT_SECONDS:-180}"
REALM_URL="http://localhost:${KEYCLOAK_PORT}/realms/wagering"

fail() {
  echo "SMOKE FAILED: $*" >&2
  exit 1
}

ok() {
  echo "ok: $*"
}

# wait_for <description> <command...>: retries until the command succeeds.
wait_for() {
  local description="$1"
  shift
  local deadline=$((SECONDS + WAIT_SECONDS))
  until "$@" >/dev/null 2>&1; do
    if [ "$SECONDS" -ge "$deadline" ]; then
      fail "timed out after ${WAIT_SECONDS}s waiting for: ${description}"
    fi
    sleep 2
  done
}

# --- migrations -------------------------------------------------------------

migrate_exited_ok() {
  local container
  container=$(docker compose ps -a -q migrate)
  [ -n "$container" ] || return 1
  [ "$(docker inspect -f '{{.State.Status}}' "$container")" = "exited" ] || return 1
  [ "$(docker inspect -f '{{.State.ExitCode}}' "$container")" = "0" ]
}

migrate_failed() {
  local container
  container=$(docker compose ps -a -q migrate)
  [ -n "$container" ] || return 1
  [ "$(docker inspect -f '{{.State.Status}}' "$container")" = "exited" ] || return 1
  [ "$(docker inspect -f '{{.State.ExitCode}}' "$container")" != "0" ]
}

deadline=$((SECONDS + WAIT_SECONDS))
until migrate_exited_ok; do
  if migrate_failed; then
    docker compose logs --no-color migrate >&2 || true
    fail "migrations exited with an error"
  fi
  [ "$SECONDS" -lt "$deadline" ] || fail "timed out waiting for migrations to finish"
  sleep 2
done
ok "migrations applied"

# --- application instances --------------------------------------------------

# json_get <path>: reads JSON from stdin and prints the value at a dotted path
# ("checks.postgres"); prints nothing when the path does not exist.
json_get() {
  python3 -c '
import json, sys
value = json.load(sys.stdin)
for key in sys.argv[1].split("."):
    value = value.get(key) if isinstance(value, dict) else None
print("" if value is None else value)
' "$1"
}

for port in $INSTANCE_PORTS; do
  ready_body=""
  deadline=$((SECONDS + WAIT_SECONDS))
  until ready_body=$(curl -fsS "http://localhost:${port}/health/ready" 2>/dev/null); do
    [ "$SECONDS" -lt "$deadline" ] || fail "timed out after ${WAIT_SECONDS}s waiting for: instance on :${port} ready"
    sleep 2
  done
  [ "$(json_get checks.postgres <<<"$ready_body")" = "ok" ] || fail "instance on :${port}: postgres is not ok: ${ready_body}"
  [ "$(json_get checks.sqs <<<"$ready_body")" = "ok" ] || fail "instance on :${port}: sqs is not ok: ${ready_body}"
  ok "instance on :${port} ready (postgres and sqs ok)"
done

# Each instance validates tokens locally, and cannot start without having found the
# identity provider's keys. Inside the compose network it reaches Keycloak by its
# service name, while tokens carry the host-facing issuer: an instance that logged
# this line got through that split.
for service in $(docker compose ps --services | grep '^app-'); do
  # The logs are captured first: `grep -q` leaves as soon as it matches, and with
  # pipefail the producer's SIGPIPE would turn a match into a failure once the log
  # is longer than a pipe buffer.
  service_logs=$(docker compose logs --no-color "$service" 2>/dev/null)
  grep -q '"msg":"identity provider ready"' <<<"$service_logs" \
    || fail "${service} never reported the identity provider as ready"
  ok "${service} discovered the identity provider (issuer/discovery split)"
done

# --- keycloak token ---------------------------------------------------------

wait_for "keycloak realm" curl -fsS "${REALM_URL}/.well-known/openid-configuration"

token_response=$(curl -fsS -X POST "${REALM_URL}/protocol/openid-connect/token" \
  -d grant_type=client_credentials \
  -d client_id=provider-a \
  -d client_secret=provider-a-local-secret) || fail "keycloak refused the client_credentials request"

# The token is only decoded, never logged: print the verdict, not the token.
TOKEN_RESPONSE="$token_response" python3 - <<'PY' || fail "token claims are not what the API expects"
import base64, json, os, sys

access_token = json.loads(os.environ["TOKEN_RESPONSE"])["access_token"]
payload = access_token.split(".")[1]
claims = json.loads(base64.urlsafe_b64decode(payload + "=" * (-len(payload) % 4)))

problems = []
if claims.get("provider_id") != "provider-a":
    problems.append("provider_id is not provider-a")
audience = claims.get("aud")
if "wallet-api" not in (audience if isinstance(audience, list) else [audience]):
    problems.append("audience does not include wallet-api")
roles = claims.get("resource_access", {}).get("wallet-api", {}).get("roles", [])
if "provider" not in roles:
    problems.append("role provider is missing")

if problems:
    print("; ".join(problems), file=sys.stderr)
    sys.exit(1)
PY
ok "keycloak token has provider_id, audience wallet-api and role provider"

# The application reaches Keycloak by its service name but tokens carry the
# host-facing issuer. Check both agree on the issuer, as seen from inside the
# compose network, which is where the application validates tokens.
inside_discovery=$(docker compose run --rm --no-deps --entrypoint wget migrate \
  -qO- http://keycloak:8080/realms/wagering/.well-known/openid-configuration) \
  || fail "keycloak discovery is not reachable from inside the compose network"
inside_issuer=$(json_get issuer <<<"$inside_discovery")
token_issuer=$(TOKEN_RESPONSE="$token_response" python3 - <<'PY'
import base64, json, os
token = json.loads(os.environ["TOKEN_RESPONSE"])["access_token"]
payload = token.split(".")[1]
print(json.loads(base64.urlsafe_b64decode(payload + "=" * (-len(payload) % 4)))["iss"])
PY
)
[ "$inside_issuer" = "$token_issuer" ] || fail "issuer mismatch: discovery says ${inside_issuer}, token says ${token_issuer}"
[ "$token_issuer" = "$REALM_URL" ] || fail "token issuer ${token_issuer} is not the expected ${REALM_URL}"
ok "token issuer matches the discovery issuer seen from inside the network"

# --- queues -----------------------------------------------------------------

awslocal_sqs() {
  docker compose exec -T localstack awslocal sqs "$@"
}

queue_url() {
  awslocal_sqs get-queue-url --queue-name "$1" --query QueueUrl --output text
}

for queue in wager-transactions.fifo wager-transactions-dlq.fifo wager-events.fifo; do
  queue_url "$queue" >/dev/null 2>&1 || fail "queue ${queue} does not exist"
  ok "queue ${queue} exists"
done

# One IAM user per component, each with its least-privilege policy (deploy/aws/iam).
for component in consumer publisher; do
  attached=$(docker compose exec -T localstack awslocal iam list-attached-user-policies --user-name "wagering-${component}" \
    --query 'AttachedPolicies[].PolicyName' --output text 2>/dev/null) || fail "IAM user wagering-${component} does not exist"
  [ "$attached" = "wagering-${component}-sqs" ] || fail "wagering-${component} has policies '${attached}', not wagering-${component}-sqs"
done
ok "the consumer and the publisher each have an IAM user with their own SQS policy"

redrive=$(awslocal_sqs get-queue-attributes \
  --queue-url "$(queue_url wager-transactions.fifo)" \
  --attribute-names RedrivePolicy --query Attributes.RedrivePolicy --output text) \
  || fail "could not read the redrive policy of wager-transactions.fifo"
[ "$(json_get deadLetterTargetArn <<<"$redrive")" = "arn:aws:sqs:us-east-1:000000000000:wager-transactions-dlq.fifo" ] \
  || fail "input queue does not redrive to the DLQ: ${redrive}"
[ "$(json_get maxReceiveCount <<<"$redrive")" = "5" ] \
  || fail "input queue maxReceiveCount is not 5: ${redrive}"
ok "input queue redrives to the DLQ after 5 receives"

# --- wallet API with a real token ---------------------------------------------

internal_token_response=$(curl -fsS -X POST "${REALM_URL}/protocol/openid-connect/token" \
  -d grant_type=client_credentials \
  -d client_id=internal-service \
  -d client_secret=internal-service-local-secret) || fail "keycloak refused the internal-service client_credentials request"
internal_token=$(json_get access_token <<<"$internal_token_response")
[ -n "$internal_token" ] || fail "no access token for internal-service"

first_port=$(echo "$INSTANCE_PORTS" | awk '{print $1}')
second_port=$(echo "$INSTANCE_PORTS" | awk '{print $2}')
third_port=$(echo "$INSTANCE_PORTS" | awk '{print $3}')
player_id=$(python3 -c 'import uuid; print(uuid.uuid4())')
open_body="{\"playerId\":\"${player_id}\",\"initialBalance\":{\"amount\":\"100.00\",\"currency\":\"BRL\"}}"

# The token is passed through a variable to curl; nothing is echoed.
api_status() { # <method> <port> <path> <token> [body] [extra header]
  local method="$1" port="$2" path="$3" token="$4" body="${5:-}" extra="${6:-}"
  local args=(-sS -o "${SMOKE_BODY}" -w '%{http_code}' -X "$method" "http://localhost:${port}${path}")
  [ -n "$token" ] && args+=(-H "Authorization: Bearer ${token}")
  [ -n "$body" ] && args+=(-H 'Content-Type: application/json' --data "$body")
  [ -n "$extra" ] && args+=(-H "$extra")
  curl "${args[@]}"
}
SMOKE_BODY=$(mktemp)
trap 'rm -f "$SMOKE_BODY"' EXIT

[ "$(api_status POST "$first_port" /wallets "")" = "401" ] || fail "POST /wallets without a token is not 401"
[ "$(api_status POST "$first_port" /wallets "$(json_get access_token <<<"$token_response")" "$open_body")" = "403" ] \
  || fail "POST /wallets with a provider token is not 403"
ok "wallet endpoints refuse a missing token (401) and a provider token (403)"

status=$(api_status POST "$first_port" /wallets "$internal_token" "$open_body")
[ "$status" = "201" ] || fail "POST /wallets with an internal token answered ${status}: $(cat "$SMOKE_BODY")"
wallet_id=$(json_get id < "$SMOKE_BODY")
[ -n "$wallet_id" ] || fail "the created wallet has no id"
ok "an internal token opened wallet ${wallet_id} through :${first_port}"

# The opening wrote two events to the outbox in the same transaction; the publishers
# of the instances send them to the events queue, one message group per wallet.
events_in_queue() {
  [ "$(awslocal_sqs get-queue-attributes --queue-url "$(queue_url wager-events.fifo)" \
    --attribute-names ApproximateNumberOfMessages --query Attributes.ApproximateNumberOfMessages --output text)" -ge 2 ]
}
wait_for "the wallet's two events in wager-events.fifo" events_in_queue
event_messages=$(awslocal_sqs receive-message --queue-url "$(queue_url wager-events.fifo)" \
  --max-number-of-messages 10 --visibility-timeout 0 --attribute-names MessageGroupId \
  --message-attribute-names eventType --output json)
EVENT_MESSAGES="$event_messages" WALLET_ID="$wallet_id" python3 - <<'PY' || fail "the events in the queue are not the wallet's two events"
import json, os, sys

wallet = os.environ["WALLET_ID"]
messages = json.loads(os.environ["EVENT_MESSAGES"]).get("Messages", [])
types = sorted(
    m["MessageAttributes"]["eventType"]["StringValue"]
    for m in messages
    if m["Attributes"].get("MessageGroupId") == wallet
)
if types != ["WagerTransactionProcessed", "WalletBalanceChanged"]:
    print(f"events of wallet {wallet}: {types}", file=sys.stderr)
    sys.exit(1)
PY
ok "the opening's two events reached wager-events.fifo, grouped by wallet"

# Another instance sees it: the instances share the database, not memory.
status=$(api_status GET "$second_port" "/wallets/${wallet_id}" "$internal_token")
[ "$status" = "200" ] || fail "GET /wallets/${wallet_id} through :${second_port} answered ${status}"
[ "$(json_get balance.amount < "$SMOKE_BODY")" = "100.00" ] || fail "the wallet read through :${second_port} does not have 100.00"
ok "the wallet is readable through another instance (:${second_port}) with its balance"

# A provider's operation: processed on one instance, replayed by another, and refused
# for a provider that is not the one in the body.
provider_token=$(json_get access_token <<<"$token_response")
bet_body="{\"providerId\":\"provider-a\",\"externalTransactionId\":\"smoke-bet-1\",\"playerId\":\"${player_id}\",\"walletId\":\"${wallet_id}\",\"roundId\":\"smoke-round\",\"gameId\":\"smoke-game\",\"kind\":\"BET\",\"money\":{\"amount\":\"25.00\",\"currency\":\"BRL\"}}"
status=$(api_status POST "$second_port" /wagering/transactions "$provider_token" "$bet_body" 'Idempotency-Key: smoke-key-1')
[ "$status" = "200" ] || fail "a bet through :${second_port} answered ${status}: $(cat "$SMOKE_BODY")"
[ "$(json_get balance.amount < "$SMOKE_BODY")" = "75.00" ] || fail "the bet left $(json_get balance.amount < "$SMOKE_BODY"), not 75.00"
[ "$(json_get idempotentReplay < "$SMOKE_BODY")" = "False" ] || fail "the first bet was reported as a replay"
status=$(api_status POST "$third_port" /wagering/transactions "$provider_token" "$bet_body" 'Idempotency-Key: smoke-key-1')
[ "$status" = "200" ] || fail "the same bet through :${third_port} answered ${status}"
[ "$(json_get idempotentReplay < "$SMOKE_BODY")" = "True" ] || fail "the same bet through :${third_port} was not a replay"
[ "$(json_get balance.amount < "$SMOKE_BODY")" = "75.00" ] || fail "the replay did not return the original balance"
ok "a bet was processed through :${second_port} and replayed through :${third_port}, once"

# The result can be read back by id and by external id, from any instance, by its owner
# only.
status=$(api_status GET "$first_port" "/providers/provider-a/wagering/transactions/smoke-bet-1" "$provider_token")
[ "$status" = "200" ] || fail "reading the bet by external id answered ${status}"
[ "$(json_get status < "$SMOKE_BODY")" = "PROCESSED" ] || fail "the bet read back is not PROCESSED"
[ "$(json_get balance.amount < "$SMOKE_BODY")" = "75.00" ] || fail "the bet read back does not show the balance of its processing"

provider_b_response=$(curl -fsS -X POST "${REALM_URL}/protocol/openid-connect/token" \
  -d grant_type=client_credentials -d client_id=provider-b -d client_secret=provider-b-local-secret) || fail "keycloak refused provider-b"
status=$(api_status POST "$first_port" /wagering/transactions "$(json_get access_token <<<"$provider_b_response")" "$bet_body" 'Idempotency-Key: smoke-key-2')
[ "$status" = "403" ] || fail "provider-b acting for provider-a answered ${status}, not 403"
status=$(api_status POST "$first_port" /wagering/transactions "$internal_token" "$bet_body" 'Idempotency-Key: smoke-key-3')
[ "$status" = "403" ] || fail "the internal role submitting an operation answered ${status}, not 403"
ok "an operation for another provider, or from the internal role, is refused (403)"
provider_b_token=$(json_get access_token <<<"$provider_b_response")
status=$(api_status GET "$second_port" "/providers/provider-b/wagering/transactions/smoke-bet-1" "$provider_b_token")
[ "$status" = "404" ] || fail "provider-b reading under its own name a transaction id it does not own answered ${status}, not 404"
status=$(api_status GET "$third_port" "/providers/provider-a/wagering/transactions/smoke-bet-1" "$provider_b_token")
[ "$status" = "403" ] || fail "provider-b reading under provider-a's name answered ${status}, not 403"
ok "a provider reads only its own transactions (404 for ids it does not own, 403 for another's namespace)"

# The ledger holds the opening and the bet, oldest first.
status=$(api_status GET "$third_port" "/wallets/${wallet_id}/ledger?limit=10" "$internal_token")
[ "$status" = "200" ] || fail "listing the ledger through :${third_port} answered ${status}: $(cat "$SMOKE_BODY")"
LEDGER_BODY="$(cat "$SMOKE_BODY")" python3 - <<'PY' || fail "the ledger is not the opening followed by the bet"
import json, os, sys

page = json.loads(os.environ["LEDGER_BODY"])
entries = page["entries"]
directions = [e["direction"] for e in entries]
if directions != ["CREDIT", "DEBIT"] or "nextCursor" in page:
    print(f"directions {directions}, cursor {page.get('nextCursor')}", file=sys.stderr)
    sys.exit(1)
if entries[0]["balanceAfter"]["amount"] != "100.00" or entries[1]["balanceBefore"]["amount"] != "100.00" or entries[1]["balanceAfter"]["amount"] != "75.00":
    print("the entries do not chain 0.00 -> 100.00 -> 75.00", file=sys.stderr)
    sys.exit(1)
PY
ok "the ledger lists the opening and the bet, chained"

# The books agree: the stored balance is what the ledger adds up to.
status=$(api_status POST "$second_port" "/wallets/${wallet_id}/reconciliation" "$internal_token")
[ "$status" = "200" ] || fail "reconciling the wallet through :${second_port} answered ${status}: $(cat "$SMOKE_BODY")"
RECONCILIATION_BODY="$(cat "$SMOKE_BODY")" python3 - <<'PY' || fail "the reconciliation does not find the books in agreement at 75.00"
import json, os, sys

result = json.loads(os.environ["RECONCILIATION_BODY"])
if not result["consistent"] or result["storedBalance"]["amount"] != "75.00" or result["calculatedBalance"]["amount"] != "75.00" or result["difference"]["amount"] != "0.00" or result["checkedEntries"] != 2:
    print(result, file=sys.stderr)
    sys.exit(1)
PY
status=$(api_status POST "$second_port" "/wallets/${wallet_id}/reconciliation" "$provider_token")
[ "$status" = "403" ] || fail "a provider reconciling a wallet answered ${status}, not 403"
ok "the reconciliation finds the books in agreement, and is the internal role's alone"

# The same use case through the queue: a bet sent as a message is applied, the message
# leaves the queue, and one that can never be handled ends in the dead-letter queue.
input_url=$(queue_url wager-transactions.fifo)
dlq_url=$(queue_url wager-transactions-dlq.fifo)
message_body="{\"messageId\":\"smoke-msg-1\",\"type\":\"WagerTransactionRequested\",\"occurredAt\":\"2026-09-08T12:00:00.000Z\",\"data\":{\"providerId\":\"provider-a\",\"externalTransactionId\":\"smoke-bet-2\",\"idempotencyKey\":\"smoke-key-2\",\"playerId\":\"${player_id}\",\"walletId\":\"${wallet_id}\",\"roundId\":\"smoke-round\",\"gameId\":\"smoke-game\",\"kind\":\"BET\",\"money\":{\"amount\":\"5.00\",\"currency\":\"BRL\"}}}"
awslocal_sqs send-message --queue-url "$input_url" --message-group-id "$wallet_id" --message-deduplication-id smoke-dedup-1 \
  --message-body "$message_body" >/dev/null || fail "could not send the bet message"
wallet_after_message() {
  local status
  status=$(api_status GET "$third_port" "/wallets/${wallet_id}" "$internal_token") && [ "$status" = "200" ] \
    && [ "$(json_get balance.amount < "$SMOKE_BODY")" = "70.00" ]
}
wait_for "the bet message to be applied (balance 70.00)" wallet_after_message
queue_is_empty() {
  [ "$(awslocal_sqs get-queue-attributes --queue-url "$input_url" --attribute-names All \
    --query 'Attributes.[ApproximateNumberOfMessages,ApproximateNumberOfMessagesNotVisible]' --output text | tr -d '[:space:]')" = "00" ]
}
wait_for "the handled message to leave the input queue" queue_is_empty
ok "a bet sent through the queue is applied (70.00) and its message is deleted"

awslocal_sqs send-message --queue-url "$input_url" --message-group-id "$wallet_id" --message-deduplication-id smoke-dedup-2 \
  --message-body '{"messageId":"smoke-msg-bad","type":"Unknown"}' >/dev/null || fail "could not send the bad message"
dead_letter_arrived() {
  [ "$(awslocal_sqs get-queue-attributes --queue-url "$dlq_url" --attribute-names ApproximateNumberOfMessages \
    --query Attributes.ApproximateNumberOfMessages --output text)" -ge 1 ]
}
wait_for "the unknown message in the dead-letter queue" dead_letter_arrived
wait_for "the bad message to leave the input queue" queue_is_empty
ok "a message that can never be handled goes to the dead-letter queue and stops blocking its group"

# WIN credits, LOSS moves nothing; the books still agree.
win_body="{\"providerId\":\"provider-a\",\"externalTransactionId\":\"smoke-win-1\",\"playerId\":\"${player_id}\",\"walletId\":\"${wallet_id}\",\"roundId\":\"smoke-round\",\"gameId\":\"smoke-game\",\"kind\":\"WIN\",\"money\":{\"amount\":\"10.00\",\"currency\":\"BRL\"}}"
status=$(api_status POST "$second_port" /wagering/transactions "$provider_token" "$win_body" 'Idempotency-Key: smoke-win-key-1')
[ "$status" = "200" ] || fail "a win answered ${status}: $(cat "$SMOKE_BODY")"
[ "$(json_get balance.amount < "$SMOKE_BODY")" = "80.00" ] || fail "the win left $(json_get balance.amount < "$SMOKE_BODY"), not 80.00"
loss_body="{\"providerId\":\"provider-a\",\"externalTransactionId\":\"smoke-loss-1\",\"playerId\":\"${player_id}\",\"walletId\":\"${wallet_id}\",\"roundId\":\"smoke-round\",\"gameId\":\"smoke-game\",\"kind\":\"LOSS\",\"money\":{\"amount\":\"0.00\",\"currency\":\"BRL\"}}"
status=$(api_status POST "$third_port" /wagering/transactions "$provider_token" "$loss_body" 'Idempotency-Key: smoke-loss-key-1')
[ "$status" = "200" ] || fail "a loss answered ${status}: $(cat "$SMOKE_BODY")"
[ "$(json_get balance.amount < "$SMOKE_BODY")" = "80.00" ] || fail "the loss changed the balance to $(json_get balance.amount < "$SMOKE_BODY")"
status=$(api_status POST "$second_port" "/wallets/${wallet_id}/reconciliation" "$internal_token")
[ "$status" = "200" ] || fail "reconciling after the win and the loss answered ${status}"
RECONCILIATION_BODY="$(cat "$SMOKE_BODY")" python3 - <<'PY' || fail "the books do not agree at 80.00 after a win and a loss (expected 4 entries: opening, bet, bet, win)"
import json, os, sys

result = json.loads(os.environ["RECONCILIATION_BODY"])
if not result["consistent"] or result["storedBalance"]["amount"] != "80.00" or result["checkedEntries"] != 4:
    print(result, file=sys.stderr)
    sys.exit(1)
PY
ok "a win credits, a loss moves nothing (no ledger entry), and the books agree at 80.00"

# A REFUND gives the first bet back, once: a second reversal of it is refused.
refund_body="{\"providerId\":\"provider-a\",\"externalTransactionId\":\"smoke-refund-1\",\"referenceExternalTransactionId\":\"smoke-bet-1\",\"playerId\":\"${player_id}\",\"walletId\":\"${wallet_id}\",\"roundId\":\"smoke-round\",\"gameId\":\"smoke-game\",\"kind\":\"REFUND\",\"money\":{\"amount\":\"25.00\",\"currency\":\"BRL\"}}"
status=$(api_status POST "$second_port" /wagering/transactions "$provider_token" "$refund_body" 'Idempotency-Key: smoke-refund-key-1')
[ "$status" = "200" ] || fail "a refund answered ${status}: $(cat "$SMOKE_BODY")"
[ "$(json_get balance.amount < "$SMOKE_BODY")" = "105.00" ] || fail "the refund left $(json_get balance.amount < "$SMOKE_BODY"), not 105.00"
rollback_body="${refund_body/smoke-refund-1/smoke-rollback-1}"
rollback_body="${rollback_body/REFUND/ROLLBACK}"
status=$(api_status POST "$third_port" /wagering/transactions "$provider_token" "$rollback_body" 'Idempotency-Key: smoke-rollback-key-1')
[ "$status" = "422" ] || fail "a rollback of the refunded bet answered ${status}, not 422"
[ "$(json_get failureCode < "$SMOKE_BODY")" = "REFERENCE_ALREADY_REVERSED" ] || fail "the rollback was refused with $(json_get failureCode < "$SMOKE_BODY")"
ok "a refund returns the bet (105.00) and a second reversal of it is refused (REFERENCE_ALREADY_REVERSED)"

# A reversal that arrives before its bet is accepted as waiting (202), and a worker applies it
# once the bet is there.
early_body="{\"providerId\":\"provider-a\",\"externalTransactionId\":\"smoke-refund-3\",\"referenceExternalTransactionId\":\"smoke-bet-3\",\"playerId\":\"${player_id}\",\"walletId\":\"${wallet_id}\",\"roundId\":\"smoke-round\",\"gameId\":\"smoke-game\",\"kind\":\"REFUND\",\"money\":{\"amount\":\"7.00\",\"currency\":\"BRL\"}}"
status=$(api_status POST "$second_port" /wagering/transactions "$provider_token" "$early_body" 'Idempotency-Key: smoke-refund-key-3')
[ "$status" = "202" ] || fail "a refund of a bet that is not there yet answered ${status}, not 202: $(cat "$SMOKE_BODY")"
[ "$(json_get status < "$SMOKE_BODY")" = "PENDING_REFERENCE" ] || fail "the early refund is not PENDING_REFERENCE"
late_bet="${early_body/smoke-refund-3/smoke-bet-3}"
late_bet="${late_bet/REFUND/BET}"
late_bet="${late_bet/\"referenceExternalTransactionId\":\"smoke-bet-3\",/}"
status=$(api_status POST "$third_port" /wagering/transactions "$provider_token" "$late_bet" 'Idempotency-Key: smoke-bet-key-3')
[ "$status" = "200" ] || fail "the late bet answered ${status}: $(cat "$SMOKE_BODY")"
early_refund_processed() {
  local status
  status=$(api_status GET "$second_port" "/providers/provider-a/wagering/transactions/smoke-refund-3" "$provider_token") && [ "$status" = "200" ] \
    && [ "$(json_get status < "$SMOKE_BODY")" = "PROCESSED" ]
}
wait_for "the early refund to be applied by a worker" early_refund_processed
[ "$(json_get balance.amount < "$SMOKE_BODY")" = "105.00" ] || fail "the early refund left $(json_get balance.amount < "$SMOKE_BODY"), not 105.00"
status=$(api_status POST "$second_port" "/wallets/${wallet_id}/reconciliation" "$internal_token")
[ "$(json_get consistent < "$SMOKE_BODY")" = "True" ] || fail "the books do not agree after the early refund"
ok "a reversal that arrived before its bet waited (202) and was applied by a worker once the bet came (105.00), books in agreement"

status=$(api_status POST "$third_port" /wallets "$internal_token" "$open_body")
[ "$status" = "409" ] || fail "opening the same wallet through :${third_port} answered ${status}, not 409"
[ "$(json_get error.walletId < "$SMOKE_BODY")" = "$wallet_id" ] || fail "the 409 does not name the existing wallet"
ok "a repeated open through a third instance is a 409 that names the wallet"

# --- swagger ui and CORS -----------------------------------------------------

wait_for "swagger ui" curl -fsS "http://localhost:8089/"
ok "swagger ui is up on :8089"

preflight=$(curl -sS -o /dev/null -D - -X OPTIONS "http://localhost:${first_port}/wallets" \
  -H 'Origin: http://localhost:8089' -H 'Access-Control-Request-Method: POST' \
  -H 'Access-Control-Request-Headers: authorization,content-type')
grep -qi '^access-control-allow-origin: http://localhost:8089' <<<"$preflight" \
  || fail "the API does not allow the swagger ui origin in a preflight"
ok "the API answers the swagger ui preflight"

token_preflight=$(curl -sS -o /dev/null -D - -X OPTIONS "${REALM_URL}/protocol/openid-connect/token" \
  -H 'Origin: http://localhost:8089' -H 'Access-Control-Request-Method: POST')
grep -qi '^access-control-allow-origin: http://localhost:8089' <<<"$token_preflight" \
  || fail "keycloak does not allow the swagger ui origin on the token endpoint"
ok "keycloak allows the swagger ui origin on the token endpoint"

spec_status=$(curl -sS -o /dev/null -w '%{http_code}' "http://localhost:${first_port}/openapi.yaml")
[ "$spec_status" = "200" ] || fail "GET /openapi.yaml answered ${spec_status}"
ok "the contract is served at /openapi.yaml"

echo "SMOKE PASSED"
