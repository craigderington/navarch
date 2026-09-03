#!/usr/bin/env bash
# Bring your own infrastructure, proven the only way that means anything.
#
# A node enrols with a join token that names exactly one organization, runs its
# own router, and serves a stack through it — while the control plane's own
# router cannot reach that node at all. That last clause is the assertion. A
# demo where both routers work proves nothing, because the platform's router
# would be serving the traffic and the customer-side one would be decoration.
#
# The isolation is real, not simulated: the BYO node is a Docker-in-Docker
# daemon on its own network, and the platform's Traefik is not attached to it.
set -euo pipefail
cd "$(dirname "$0")/.."

API=${API:-http://localhost:8417}
API_TOKEN=${API_TOKEN:-dev-operator-token-change-me}
JOIN_TOKEN=${JOIN_TOKEN:-dev-join-token-change-me}
NAV=${NAV:-./bin/navarch}
SUFFIX=${SUFFIX:-$RANDOM}
HOST=${HOST:-byo-$SUFFIX.localhost}
BYO_PORT=${BYO_PORT:-8096}
export NAVARCH_URL=$API NAVARCH_TOKEN=$API_TOKEN

step() { printf '\n\033[36m▸ %s\033[0m\n' "$1"; }
note() { printf '  \033[90m%s\033[0m\n' "$1"; }
fail() { printf '\033[31mFAIL: %s\033[0m\n' "$1" >&2; exit 1; }

[ -x "$NAV" ] || fail "$NAV not built — run 'make build'"

COMPOSE=(docker compose -f compose.yaml -f deploy/byo/compose.yaml)
cleanup() {
    step "Tearing down the customer-side node"
    "${COMPOSE[@]}" rm -sf byo-agent byo-router byo-dind >/dev/null 2>&1 || true
    # And remove its node row. Leaving it behind is not untidiness: placement
    # scores by spread, so a node with zero environments homed on it is the most
    # attractive in the fleet — and this one has no agent any more. The next
    # deployment is placed there and sits in `scheduling` until the heartbeat
    # window closes. A passing system that looks broken, which is exactly the
    # hazard internal/api's testServer cleans up for the same reason.
    docker compose exec -T postgres psql -U composectl -d composectl -tAc         "DELETE FROM service_instances WHERE node_id IN (SELECT id FROM nodes WHERE hostname='byo-node-1');
         UPDATE environments SET home_node_id=NULL WHERE home_node_id IN (SELECT id FROM nodes WHERE hostname='byo-node-1');
         DELETE FROM nodes WHERE hostname='byo-node-1';" >/dev/null 2>&1 || true
}
trap cleanup EXIT

api() {
    local method=$1 path=$2 body=${3-} out code
    if [ -n "$body" ]; then
        out=$(curl -sS -H "Authorization: Bearer $API_TOKEN" -X "$method" "$API$path" \
            -H 'Content-Type: application/json' -d "$body" -w '\n%{http_code}')
    else
        out=$(curl -sS -H "Authorization: Bearer $API_TOKEN" -X "$method" "$API$path" -w '\n%{http_code}')
    fi
    code=$(tail -n1 <<<"$out"); out=$(sed '$d' <<<"$out")
    [[ $code =~ ^2 ]] || { printf '\033[31m  %s %s -> %s\033[0m\n%s\n' "$method" "$path" "$code" "$out" >&2; exit 1; }
    printf '%s' "$out"
}

step "Start a customer-owned node: its own daemon, its own router"
"${COMPOSE[@]}" up -d --build byo-dind byo-router byo-agent >/dev/null
note "byo-dind (daemon) · byo-router (their Traefik) · byo-agent (enrols + configures it)"

step "The customer puts their image on their own daemon"
# The platform does not build and does not distribute: `build:` is a rejected
# directive. In BYO that contract lands squarely on the customer, and this is
# what honouring it looks like from their side — no registry between us.
docker build -q -t ghcr.io/craigderington/navarch/site:3 examples/site >/dev/null
docker save ghcr.io/craigderington/navarch/site:3 | "${COMPOSE[@]}" exec -T byo-dind docker load >/dev/null
note "ghcr.io/craigderington/navarch/site:3 loaded onto byo-dind"

step "It enrolled with a join token, not the shared service token"
deadline=$((SECONDS + 90)); NODE=""
while [ $SECONDS -lt $deadline ]; do
    NODE=$($NAV node list --org dev -o json 2>/dev/null | jq -r '.[]|select(.hostname=="byo-node-1")|.id')
    [ -n "$NODE" ] && break
    sleep 2
done
[ -n "$NODE" ] || fail "the BYO node never registered"
note "byo-node-1 = $NODE"

step "Deploy a stack — the scheduler may place it anywhere, so pin it here"
ORG=$(api GET /v1/orgs | jq -r '.organizations[]|select(.slug=="dev")|.id')
APP=$(api POST "/v1/orgs/$ORG/apps" "{\"slug\":\"byo-$SUFFIX\",\"name\":\"BYO\"}" | jq -r .id)
STACK=$(api POST "/v1/apps/$APP/stacks" "{\"slug\":\"main\"}" | jq -r .id)
curl -sS -H "Authorization: Bearer $API_TOKEN" -X POST \
    "$API/v1/stacks/$STACK/versions?created_by=demo-byo" \
    --data-binary @examples/site/compose.yaml | jq -e .id >/dev/null
ENV_ID=$(api POST "/v1/stacks/$STACK/envs" "{\"slug\":\"prod\",\"hostname\":\"$HOST\"}" | jq -r .id)

# Drain every other node so placement has exactly one candidate. Draining is the
# product's own way of saying "not here", which is better than reaching into the
# database to force a placement the scheduler would not have made.
for n in $($NAV node list --org dev -o json | jq -r '.[]|select(.hostname!="byo-node-1")|.id'); do
    $NAV node drain "$n" >/dev/null 2>&1 || true
done
note "every other node cordoned; only byo-node-1 can take work"

DEP=$(api POST "/v1/envs/$ENV_ID/deployments" '{"created_by":"demo-byo"}' | jq -r .id)
deadline=$((SECONDS + 180))
while :; do
    state=$(api GET "/v1/deployments/$DEP" | jq -r .state)
    [ "$state" = "live" ] && break
    [ "$state" = "failed" ] && fail "rollout failed: $(api GET "/v1/deployments/$DEP" | jq -r .failure_reason)"
    [ $SECONDS -lt $deadline ] || fail "timed out at $state"
    sleep 2
done
PLACED=$(api GET "/v1/deployments/$DEP" | jq -r '.home_node // "?"')
[ "$PLACED" = "byo-node-1" ] || fail "expected placement on byo-node-1, got $PLACED"
note "live on $PLACED"

step "Their router serves it — configured by their agent, from their own routes"
deadline=$((SECONDS + 60)); code=000
while [ $SECONDS -lt $deadline ]; do
    code=$(curl -s -o /dev/null -w '%{http_code}' -m 5 -H "Host: $HOST" "http://localhost:$BYO_PORT/" || true)
    [ "$code" = "200" ] && break
    sleep 2
done
[ "$code" = "200" ] || fail "the customer's router did not serve $HOST (got $code)"
note "http://localhost:$BYO_PORT with Host: $HOST -> 200"

step "And the platform's own router cannot reach it at all"
# The assertion the whole demo exists for. The control plane never connects to
# customer infrastructure: the agent dials out, and ingress is theirs. If the
# platform's Traefik could serve this hostname, none of the above would mean
# anything.
# `|| true` because a timeout is a *pass* here and would otherwise kill the
# script under `set -e`. curl still prints the code (000 on a timeout), so a
# fallback echo would double it. A timeout is also the most likely outcome and
# the most telling one: the platform's Traefik holds a route pointing at an
# address on a network it is not attached to, so it cannot open a connection.
plat=$(curl -s -o /dev/null -w '%{http_code}' -m 5 -H "Host: $HOST" "http://localhost:8095/" || true)
[ "$plat" != "200" ] || fail "the platform's router served a BYO host — ingress is not actually theirs"
case "$plat" in
000) note "platform gateway cannot connect at all (timeout) — the node is not on its network" ;;
*)   note "platform gateway returns $plat for the same hostname, as it must" ;;
esac

step "Restoring the rest of the fleet"
for n in $($NAV node list --org dev -o json | jq -r '.[]|select(.hostname!="byo-node-1")|.id'); do
    $NAV node uncordon "$n" >/dev/null 2>&1 || true
done
note "uncordoned"

printf '\n\033[32mA node nobody at the control plane can reach ran, and served, a deployed stack.\033[0m\n'
