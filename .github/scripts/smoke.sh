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
  docker compose logs --no-color "$service" 2>/dev/null | grep -q '"msg":"identity provider ready"' \
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
api_status() { # <method> <port> <path> <token> [body]
  local method="$1" port="$2" path="$3" token="$4" body="${5:-}"
  local args=(-sS -o "${SMOKE_BODY}" -w '%{http_code}' -X "$method" "http://localhost:${port}${path}")
  [ -n "$token" ] && args+=(-H "Authorization: Bearer ${token}")
  [ -n "$body" ] && args+=(-H 'Content-Type: application/json' --data "$body")
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

# Another instance sees it: the instances share the database, not memory.
status=$(api_status GET "$second_port" "/wallets/${wallet_id}" "$internal_token")
[ "$status" = "200" ] || fail "GET /wallets/${wallet_id} through :${second_port} answered ${status}"
[ "$(json_get balance.amount < "$SMOKE_BODY")" = "100.00" ] || fail "the wallet read through :${second_port} does not have 100.00"
ok "the wallet is readable through another instance (:${second_port}) with its balance"

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
echo "$preflight" | grep -qi '^access-control-allow-origin: http://localhost:8089' \
  || fail "the API does not allow the swagger ui origin in a preflight"
ok "the API answers the swagger ui preflight"

token_preflight=$(curl -sS -o /dev/null -D - -X OPTIONS "${REALM_URL}/protocol/openid-connect/token" \
  -H 'Origin: http://localhost:8089' -H 'Access-Control-Request-Method: POST')
echo "$token_preflight" | grep -qi '^access-control-allow-origin: http://localhost:8089' \
  || fail "keycloak does not allow the swagger ui origin on the token endpoint"
ok "keycloak allows the swagger ui origin on the token endpoint"

spec_status=$(curl -sS -o /dev/null -w '%{http_code}' "http://localhost:${first_port}/openapi.yaml")
[ "$spec_status" = "200" ] || fail "GET /openapi.yaml answered ${spec_status}"
ok "the contract is served at /openapi.yaml"

echo "SMOKE PASSED"
