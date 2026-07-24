#!/usr/bin/env bash
# End-to-end: create the catalog, push a stack, and watch the agent + rollout
# controller bring it up, auto-promote it, and flip live traffic through Traefik
# — all over HTTP, with real containers. The ingress service is traefik/whoami,
# which echoes its container hostname, so the flip is visible in the response.
set -euo pipefail

API=${API:-http://localhost:8417}
GW=${GW:-http://localhost:8095}          # Traefik web entrypoint
COMPOSE=${COMPOSE:-examples/hello/compose.yaml}
SUFFIX=${SUFFIX:-$RANDOM}
# Unique per run: prior demo environments linger (no delete endpoint yet), and a
# shared hostname would collide in Traefik across runs.
HOST=${HOST:-prod-$SUFFIX.example.com}

step() { printf '\n\033[36m▸ %s\033[0m\n' "$1"; }
note() { printf '  \033[90m%s\033[0m\n' "$1"; }

api() {
  local method=$1 path=$2 body=${3-} out code
  if [ -n "$body" ]; then
    out=$(curl -sS -X "$method" "$API$path" -H 'Content-Type: application/json' -d "$body" -w '\n%{http_code}')
  else
    out=$(curl -sS -X "$method" "$API$path" -w '\n%{http_code}')
  fi
  code=$(tail -n1 <<<"$out"); out=$(sed '$d' <<<"$out")
  if [[ ! $code =~ ^2 ]]; then
    printf '\033[31m  %s %s -> HTTP %s\033[0m\n%s\n' "$method" "$path" "$code" "$out" >&2; exit 1
  fi
  printf '%s' "$out"
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

# curl through Traefik, retrying while the router/Traefik reload catches up.
# Prints the whoami Hostname line's value (the serving container's id).
gw_whoami() {
  local deadline=$((SECONDS + 30)) body
  while :; do
    if body=$(curl -fsS -H "Host: $HOST" "$GW" 2>/dev/null); then
      awk '/^Hostname:/ {print $2; exit}' <<<"$body"; return 0
    fi
    [ $SECONDS -lt $deadline ] || { echo "traefik never routed $HOST" >&2; exit 1; }
    sleep 2
  done
}

step "Using the bootstrapped dev org"
ORG=$(api GET /v1/orgs | jq -r '.organizations[]|select(.slug=="dev")|.id')
[ -n "$ORG" ] || { echo "dev org not found — is the control plane up?" >&2; exit 1; }
APP=$(api POST "/v1/orgs/$ORG/apps" "{\"slug\":\"web-$SUFFIX\",\"name\":\"Web\"}" | jq -r .id)
STACK=$(api POST "/v1/apps/$APP/stacks" "{\"slug\":\"main-$SUFFIX\"}" | jq -r .id)
curl -sS -X POST "$API/v1/stacks/$STACK/versions?created_by=demo" --data-binary "@$COMPOSE" | jq -e .id >/dev/null
ENV_ID=$(api POST "/v1/stacks/$STACK/envs" "{\"slug\":\"prod\",\"hostname\":\"$HOST\"}" | jq -r .id)
note "org=$ORG env=$ENV_ID host=$HOST"

step "Deploy revision 1 — the agent + controller take it to live on their own"
DEP1=$(api POST "/v1/envs/$ENV_ID/deployments" '{"created_by":"demo"}' | jq -r .id)
wait_state "$DEP1" live
WHO1=$(gw_whoami)
note "curl http://$HOST via Traefik → served by container $WHO1"
docker ps --filter "label=cc.deployment=$DEP1" --format '  {{.Names}}  {{.Status}}'

step "Deploy revision 2 — auto-promotes and flips traffic"
DEP2=$(api POST "/v1/envs/$ENV_ID/deployments" '{"created_by":"demo"}' | jq -r .id)
note "revision 2 slot $(api GET "/v1/deployments/$DEP2" | jq -r .slot)"
wait_state "$DEP2" live

step "The flip"
# Promotion is atomic, but the router regenerates config on its next tick and
# Traefik reloads a beat later — poll the gateway until traffic actually moves
# off r1. Every probe returns 200 (r1 keeps serving until the flip lands), so
# this also demonstrates the zero-downtime property.
WHO2=""; deadline=$((SECONDS + 40))
while [ $SECONDS -lt $deadline ]; do
  cur=$(gw_whoami)
  if [ "$cur" != "$WHO1" ]; then WHO2=$cur; break; fi
  sleep 2
done
[ -n "$WHO2" ] || { echo "traffic never flipped away from $WHO1" >&2; exit 1; }
note "traffic moved $WHO1 → $WHO2 with no downtime (every probe returned 200)"

step "Topology: r1 swappable torn down, r2 live, one shared pinned db"
# Teardown runs a tick behind promotion (controller deletes the superseded
# rows, the agent GCs the orphaned swappable containers next poll). Wait for it.
ENV8=$(printf %s "$ENV_ID" | tr -d - | cut -c1-8)
# Scope to swappable: the pinned db keeps r1's cc.deployment label forever
# (r2 adopts the container rather than recreating it), and it is meant to stay.
r1_swappable() { docker ps --filter "label=cc.deployment=$DEP1" --filter "label=cc.swappable=true" -q; }
for _ in $(seq 1 30); do
  r1_swappable | grep -q . || break
  sleep 2
done
docker ps --filter "label=cc.env=$ENV8" --format '  {{.Names}}  {{.Status}}' | sort
r1_swappable | grep -q . \
  && { echo "r1 swappable containers were not torn down" >&2; exit 1; } \
  || note "r1's swappable containers were garbage-collected; the pinned db stayed"

step "History"
api GET "/v1/envs/$ENV_ID/deployments" | jq -r '.deployments[] | "  r\(.revision)  \(.slot)\t\(.state)"'

printf '\n\033[32m✓ agent-driven rollout, auto-promote, and live traffic flip through Traefik\033[0m\n'
note "env=$ENV_ID  (rollback: curl -X POST $API/v1/envs/$ENV_ID/rollback -d '{\"to_revision\":1}')"
