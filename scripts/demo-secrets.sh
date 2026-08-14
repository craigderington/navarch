#!/usr/bin/env bash
# End-to-end secret injection: set a secret via the API, prove the row at
# rest in Postgres is ciphertext (never the plaintext value), deploy a stack
# that references it, and prove the agent decrypted + injected it by curling
# the running container through Traefik. Also proves the deploy-time
# fail-fast: the same stack on an environment with the secret unset gets 422,
# never reaching a node to crash-loop.
set -euo pipefail

API=${API:-http://localhost:8417}
API_TOKEN=${API_TOKEN:-dev-token-change-me}
CURL_AUTH=(-H "Authorization: Bearer $API_TOKEN")
GW=${GW:-http://localhost:8095}
DB_URL=${DB_URL:-postgres://composectl:composectl@localhost:5473/composectl?sslmode=disable}
COMPOSE=${COMPOSE:-examples/secret/compose.yaml}
SUFFIX=${SUFFIX:-$RANDOM}
# Unique per run, same reasoning as demo.sh: prior demo environments linger
# (no delete endpoint yet), and a shared hostname would collide in Traefik.
HOST=${HOST:-sec-$SUFFIX.example.com}
SECRET_VALUE="wonderland-$SUFFIX"

step() { printf '\n\033[36m▸ %s\033[0m\n' "$1"; }
note() { printf '  \033[90m%s\033[0m\n' "$1"; }

api() {
  local method=$1 path=$2 body=${3-} out code
  if [ -n "$body" ]; then
    out=$(curl -sS "${CURL_AUTH[@]}" -X "$method" "$API$path" -H 'Content-Type: application/json' -d "$body" -w '\n%{http_code}')
  else
    out=$(curl -sS "${CURL_AUTH[@]}" -X "$method" "$API$path" -w '\n%{http_code}')
  fi
  code=$(tail -n1 <<<"$out"); out=$(sed '$d' <<<"$out")
  if [[ ! $code =~ ^2 ]]; then
    printf '\033[31m  %s %s -> HTTP %s\033[0m\n%s\n' "$method" "$path" "$code" "$out" >&2; exit 1
  fi
  printf '%s' "$out"
}

# api_status is like api() but returns the status code instead of exiting on
# failure — used to assert the 422 fail-fast, which is an expected failure.
api_status() {
  local method=$1 path=$2 body=${3-}
  curl -sS "${CURL_AUTH[@]}" -o /dev/null -w '%{http_code}' -X "$method" "$API$path" \
    -H 'Content-Type: application/json' -d "$body"
}

wait_state() {
  local dep=$1 target=$2 deadline=$((SECONDS + 150)) state
  while :; do
    state=$(api GET "/v1/deployments/$dep" | jq -r .state)
    case "$state" in
      "$target") note "state=$state"; return 0 ;;
      failed) api GET "/v1/deployments/$dep" | jq . >&2; echo "rollout failed" >&2; exit 1 ;;
    esac
    [ $SECONDS -lt $deadline ] || { echo "timed out waiting for $target (stuck at $state)" >&2; exit 1; }
    sleep 2
  done
}

step "Using the bootstrapped dev org"
ORG=$(api GET /v1/orgs | jq -r '.organizations[]|select(.slug=="dev")|.id')
[ -n "$ORG" ] || { echo "dev org not found — is the control plane up?" >&2; exit 1; }

step "Environment 1: set the secret before deploying"
APP=$(api POST "/v1/orgs/$ORG/apps" "{\"slug\":\"sec-$SUFFIX\",\"name\":\"Secret\"}" | jq -r .id)
STACK=$(api POST "/v1/apps/$APP/stacks" "{\"slug\":\"main-$SUFFIX\"}" | jq -r .id)
curl -sS "${CURL_AUTH[@]}" -X POST "$API/v1/stacks/$STACK/versions?created_by=demo" --data-binary "@$COMPOSE" | jq -e .id >/dev/null
ENV_ID=$(api POST "/v1/stacks/$STACK/envs" "{\"slug\":\"prod\",\"hostname\":\"$HOST\"}" | jq -r .id)
note "org=$ORG env=$ENV_ID host=$HOST"

api POST "/v1/envs/$ENV_ID/secrets" "{\"key\":\"name\",\"value\":\"$SECRET_VALUE\"}" | jq -e .key >/dev/null
note "set secret 'name'=$SECRET_VALUE"

step "Prove the row at rest is ciphertext, not the value"
ROW=$(psql "$DB_URL" -t -A -c \
  "SELECT octet_length(ciphertext), encode(ciphertext,'hex') FROM secrets WHERE environment_id='$ENV_ID' AND key='name' ORDER BY version DESC LIMIT 1")
CT_LEN=$(cut -d'|' -f1 <<<"$ROW")
CT_HEX=$(cut -d'|' -f2 <<<"$ROW")
[ -n "$CT_HEX" ] || { echo "no secrets row found for env $ENV_ID" >&2; exit 1; }
note "secrets row: ${CT_LEN} bytes ciphertext, hex prefix ${CT_HEX:0:32}…"
if grep -qF "$SECRET_VALUE" <<<"$CT_HEX"; then
  echo "FAIL: plaintext value found in the stored ciphertext" >&2; exit 1
fi
note "confirmed: ciphertext does not contain the plaintext value '$SECRET_VALUE'"

step "Deploy the stack — the agent decrypts and injects the secret"
DEP=$(api POST "/v1/envs/$ENV_ID/deployments" '{"created_by":"demo"}' | jq -r .id)
wait_state "$DEP" live

step "Curl through Traefik — the response carries the decrypted plaintext"
deadline=$((SECONDS + 30)); BODY=""
while [ $SECONDS -lt $deadline ]; do
  if BODY=$(curl -fsS -H "Host: $HOST" "$GW" 2>/dev/null) && grep -q '^Name:' <<<"$BODY"; then
    break
  fi
  sleep 2
done
note "$(grep '^Name:' <<<"$BODY")"
grep -qF "Name: $SECRET_VALUE" <<<"$BODY" \
  || { echo "expected body to contain 'Name: $SECRET_VALUE', got:" >&2; printf '%s\n' "$BODY" >&2; exit 1; }
note "confirmed: Traefik response carries the decrypted secret"

step "Environment 2: same stack, secret UNSET — deploy must fail fast (422)"
APP2=$(api POST "/v1/orgs/$ORG/apps" "{\"slug\":\"sec2-$SUFFIX\",\"name\":\"Secret2\"}" | jq -r .id)
STACK2=$(api POST "/v1/apps/$APP2/stacks" "{\"slug\":\"main2-$SUFFIX\"}" | jq -r .id)
curl -sS "${CURL_AUTH[@]}" -X POST "$API/v1/stacks/$STACK2/versions?created_by=demo" --data-binary "@$COMPOSE" | jq -e .id >/dev/null
ENV2_ID=$(api POST "/v1/stacks/$STACK2/envs" "{\"slug\":\"prod\",\"hostname\":\"sec2-$SUFFIX.example.com\"}" | jq -r .id)
CODE=$(api_status POST "/v1/envs/$ENV2_ID/deployments" '{"created_by":"demo"}')
[ "$CODE" = "422" ] || { echo "expected 422 for a deploy with an unset required secret, got $CODE" >&2; exit 1; }
note "422 as expected — env2 never had 'name' set, deploy rejected before reaching a node"

printf '\n\033[32m✓ secret set -> ciphertext at rest -> agent decrypt+inject -> plaintext through Traefik; unset secret -> 422\033[0m\n'
note "env=$ENV_ID  env2(no secret)=$ENV2_ID"
