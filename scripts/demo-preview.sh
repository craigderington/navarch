#!/usr/bin/env bash
# Preview environments end to end: create one with inherited secrets, prove it
# serves traffic with the decrypted value, then prove the platform destroys it
# — containers, pinned container and named volumes included. Step 6 is the
# point of the whole slice: a demo that stops at "the preview works" proves
# only the easy half.
set -euo pipefail

API=${API:-http://localhost:8417}
API_TOKEN=${API_TOKEN:-dev-token-change-me}
CURL_AUTH=(-H "Authorization: Bearer $API_TOKEN")
# Neither existing runnable example fits both assertions this demo has to
# make: examples/hello has a pinned db (needed so expiry has something
# non-trivial to destroy) but nothing that echoes a secret's value into its
# HTTP response; examples/secret echoes WHOAMI_NAME (needed to prove
# inheritance, not just a copied row) but every service is swappable, so its
# teardown would pass via ordinary GC without ever exercising the
# tombstone/RemoveEnv path Task 7/8 added. examples/preview/compose.yaml
# combines both: a swappable WHOAMI_NAME ingress service plus a pinned db
# with a named volume.
COMPOSE=${COMPOSE:-examples/preview/compose.yaml}
SUFFIX=${SUFFIX:-$RANDOM}
SECRET_VALUE="preview-$SUFFIX"

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

# Traefik's host port lives in the dev stack's own compose file; ask `docker
# compose config` to resolve it (it applies the same defaulting compose
# itself would) rather than duplicating that logic by hardcoding 8095 here.
GW_PORT=$(docker compose config --format json | jq -r '.services.traefik.ports[] | select(.target==80) | .published')
[ -n "$GW_PORT" ] || { echo "could not read traefik's published port from compose.yaml" >&2; exit 1; }
GW=${GW:-http://localhost:$GW_PORT}

# Which daemon holds this environment's containers? With a fleet, a preview is
# placed by the scheduler, so the demo has to look where the platform actually
# put it rather than assuming the host. Everything below inspects through this.
node_docker() {
  local env_id=$1 host
  host=$(docker compose exec -T postgres psql -U composectl -d composectl -tAc \
    "SELECT COALESCE(n.hostname,'') FROM environments e
       LEFT JOIN nodes n ON n.id = e.home_node_id WHERE e.id = '$env_id'" | tr -d '[:space:]')
  case "$host" in
    dev-node-2) echo "docker compose exec -T dind-b docker" ;;
    *)          echo "docker" ;;
  esac
}

step "Using the bootstrapped dev org"
ORG=$(api GET /v1/orgs | jq -r '.organizations[]|select(.slug=="dev")|.id')
[ -n "$ORG" ] || { echo "dev org not found — is the control plane up?" >&2; exit 1; }

step "Create the catalog and push the stack"
APP=$(api POST "/v1/orgs/$ORG/apps" "{\"slug\":\"prev-$SUFFIX\",\"name\":\"Preview\"}" | jq -r .id)
STACK=$(api POST "/v1/apps/$APP/stacks" "{\"slug\":\"main-$SUFFIX\"}" | jq -r .id)
curl -sS "${CURL_AUTH[@]}" -X POST "$API/v1/stacks/$STACK/versions?created_by=demo" --data-binary "@$COMPOSE" | jq -e .id >/dev/null
note "org=$ORG stack=$STACK"

step "Environment: staging, with the secret the stack requires"
STAGING=$(api POST "/v1/stacks/$STACK/envs" '{"slug":"staging"}' | jq -r .id)
api POST "/v1/envs/$STAGING/secrets" "{\"key\":\"name\",\"value\":\"$SECRET_VALUE\"}" | jq -e .key >/dev/null
note "staging=$STAGING  secret 'name'=$SECRET_VALUE"

step "Create a preview, inheriting staging's secrets"
PREVIEW=$(api POST "/v1/stacks/$STACK/previews" "{\"slug\":\"pr-$SUFFIX\",\"inherit_secrets_from\":\"staging\",\"ttl_hours\":1}")
ENV_ID=$(jq -r .environment_id <<<"$PREVIEW")
HOST=$(jq -r .hostname <<<"$PREVIEW")
DEP=$(jq -r .deployment_id <<<"$PREVIEW")
ENV8=${ENV_ID:0:8}
note "env=$ENV_ID  env8=$ENV8  host=$HOST  deployment=$DEP"

step "Poll until the preview deployment is live"
wait_state "$DEP" live

# Only now is the placement known: home_node_id is set by the first placement,
# so resolving the daemon any earlier always answers "the host" and every
# container assertion below would look at a machine the preview is not on.
DOCKER=$(node_docker "$ENV_ID")
note "placed on the node reached via: $DOCKER"

step "Curl through Traefik — the response carries staging's inherited secret"
deadline=$((SECONDS + 30)); BODY=""
while [ $SECONDS -lt $deadline ]; do
  if BODY=$(curl -fsS -H "Host: $HOST" "$GW" 2>/dev/null) && grep -q '^Name:' <<<"$BODY"; then
    break
  fi
  sleep 2
done
note "$(grep '^Name:' <<<"$BODY")"
grep -qF "Name: $SECRET_VALUE" <<<"$BODY" \
  || { echo "expected body to contain 'Name: $SECRET_VALUE' — inheritance did not carry the plaintext" >&2; printf '%s\n' "$BODY" >&2; exit 1; }
note "confirmed: the preview served staging's decrypted secret — inheritance proven end to end, not just a copied row"

step "Topology before expiry — this is what teardown has to destroy"
$DOCKER ps -a --filter "label=cc.env=$ENV8" --format '  {{.Names}}  {{.Status}}' | sort
$DOCKER volume ls --filter "label=cc.env=$ENV8" --format '  {{.Name}}'
[ -n "$($DOCKER ps -a --filter "label=cc.env=$ENV8" --filter "label=cc.swappable=false" -q)" ] \
  || { echo "FAIL: no pinned container exists to prove teardown against — the demo would pass vacuously" >&2; exit 1; }
[ -n "$($DOCKER volume ls --filter "label=cc.env=$ENV8" -q)" ] \
  || { echo "FAIL: no labelled volume exists to prove teardown against — the demo would pass vacuously" >&2; exit 1; }

step "Force expiry"
docker compose exec -T postgres psql -U composectl -d composectl \
  -c "UPDATE environments SET expires_at = now() - interval '1 minute' WHERE id = '$ENV_ID'" >/dev/null
note "expires_at backdated for env $ENV_ID — the reaper's next tick should tombstone and delete it"

step "Wait for the reaper + agent to tear everything down"
# The point of this demo: not just that the preview worked, but that nothing
# of it survives — the environment row, the pinned container, and the named
# volume all have to be gone.
deadline=$((SECONDS + 90))
in_catalog=true containers_left=1 volumes_left=1
while [ $SECONDS -lt $deadline ]; do
  api GET "/v1/stacks/$STACK/envs" | jq -e ".environments[] | select(.id==\"$ENV_ID\")" >/dev/null 2>&1 \
    && in_catalog=true || in_catalog=false
  containers_left=$($DOCKER ps -a --filter "label=cc.env=$ENV8" -q | wc -l)
  volumes_left=$($DOCKER volume ls --filter "label=cc.env=$ENV8" -q | wc -l)
  if [ "$in_catalog" = false ] && [ "$containers_left" -eq 0 ] && [ "$volumes_left" -eq 0 ]; then
    break
  fi
  sleep 2
done

note "catalog listing: $([ "$in_catalog" = false ] && echo gone || echo 'STILL LISTED')"
note "containers labelled cc.env=$ENV8: $containers_left remaining"
note "volumes labelled cc.env=$ENV8: $volumes_left remaining"

fail=false
if [ "$in_catalog" = true ]; then
  echo "FAIL: preview still listed in GET /v1/stacks/$STACK/envs after expiry" >&2
  fail=true
fi
if [ "$containers_left" -ne 0 ]; then
  echo "FAIL: containers labelled cc.env=$ENV8 survive teardown (pinned container not destroyed?)" >&2
  $DOCKER ps -a --filter "label=cc.env=$ENV8" >&2
  fail=true
fi
if [ "$volumes_left" -ne 0 ]; then
  echo "FAIL: volumes labelled cc.env=$ENV8 survive teardown" >&2
  $DOCKER volume ls --filter "label=cc.env=$ENV8" >&2
  fail=true
fi
[ "$fail" = false ] || exit 1

printf '\n\033[32m✓ preview served staging'"'"'s inherited secret through Traefik, then was destroyed completely — no containers, no volumes, gone from the catalog\033[0m\n'
note "stack=$STACK  (expired preview was env=$ENV_ID)"
