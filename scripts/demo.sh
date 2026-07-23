#!/usr/bin/env bash
# End-to-end walk through composectl: create the catalog, push a compose file,
# and watch the node agent bring the stack up for real — no SQL fakery. The
# agent drives pending → scheduling → starting → healthy on its own; every step
# here is a real HTTP call, and the containers are real.
#
# Deploys into the bootstrapped `dev` org, because that is the org the local
# agent registers its node into.
set -euo pipefail

API=${API:-http://localhost:8417}
COMPOSE=${COMPOSE:-examples/hello/compose.yaml}
SUFFIX=${SUFFIX:-$RANDOM}

step() { printf '\n\033[36m▸ %s\033[0m\n' "$1"; }
note() { printf '  \033[90m%s\033[0m\n' "$1"; }

# api METHOD PATH [BODY] — fails loudly on a non-2xx response.
api() {
  local method=$1 path=$2 body=${3-} out code
  if [ -n "$body" ]; then
    out=$(curl -sS -X "$method" "$API$path" -H 'Content-Type: application/json' -d "$body" -w '\n%{http_code}')
  else
    out=$(curl -sS -X "$method" "$API$path" -w '\n%{http_code}')
  fi
  code=$(tail -n1 <<<"$out"); out=$(sed '$d' <<<"$out")
  if [[ ! $code =~ ^2 ]]; then
    printf '\033[31m  %s %s -> HTTP %s\033[0m\n%s\n' "$method" "$path" "$code" "$out" >&2
    exit 1
  fi
  printf '%s' "$out"
}

# wait_state DEPLOY_ID TARGET — poll until the deployment reaches TARGET (or
# live), failing loudly if it goes to `failed` or times out.
wait_state() {
  local dep=$1 target=$2 deadline=$((SECONDS + 150)) state
  while :; do
    state=$(api GET "/v1/deployments/$dep" | jq -r .state)
    case "$state" in
      "$target"|live) note "state=$state"; return 0 ;;
      failed) api GET "/v1/deployments/$dep" | jq . >&2; echo "rollout failed" >&2; exit 1 ;;
    esac
    [ $SECONDS -lt $deadline ] || { echo "timed out waiting for $target (stuck at $state)" >&2; exit 1; }
    sleep 3
  done
}

step "Using the bootstrapped dev org"
ORG=$(api GET /v1/orgs | jq -r '.organizations[] | select(.slug=="dev") | .id')
[ -n "$ORG" ] || { echo "dev org not found — is the control plane up?" >&2; exit 1; }
note "org $ORG"

step "Application, stack, environment"
APP=$(api POST "/v1/orgs/$ORG/apps" "{\"slug\":\"web-$SUFFIX\",\"name\":\"Web\"}" | jq -r .id)
STACK=$(api POST "/v1/apps/$APP/stacks" "{\"slug\":\"main-$SUFFIX\"}" | jq -r .id)
note "app $APP  stack $STACK"

step "Stack version from $COMPOSE"
VERSION=$(curl -sS -X POST "$API/v1/stacks/$STACK/versions?created_by=demo" --data-binary "@$COMPOSE")
echo "$VERSION" | jq -e .id >/dev/null || { echo "$VERSION" >&2; exit 1; }
note "version $(jq -r .version <<<"$VERSION")  digest $(jq -r .spec_digest <<<"$VERSION" | cut -c1-16)…"

ENV_ID=$(api POST "/v1/stacks/$STACK/envs" \
  '{"slug":"prod","hostname":"prod.example.com","config":{"LOG_LEVEL":"info"}}' | jq -r .id)
note "env $ENV_ID"

step "Deploy revision 1"
DEPLOY=$(api POST "/v1/envs/$ENV_ID/deployments" '{"created_by":"demo"}')
DEP1=$(jq -r .id <<<"$DEPLOY")
note "revision 1  slot $(jq -r .slot <<<"$DEPLOY")  project $(jq -r .project_name <<<"$DEPLOY")"

step "A second deployment is refused while one is active"
curl -sS -o /dev/null -w '%{http_code}' -X POST "$API/v1/envs/$ENV_ID/deployments" \
  -H 'Content-Type: application/json' -d '{}' | grep -q 409 \
  && note "409 Conflict — one active deployment per environment" \
  || { echo "expected 409" >&2; exit 1; }

step "The agent brings revision 1 up (real containers, no SQL)"
wait_state "$DEP1" healthy
note "reached healthy on its own"
docker ps --filter "label=cc.deployment=$DEP1" --format '  {{.Names}}  {{.Status}}'

step "Promote revision 1 to live"
api POST "/v1/deployments/$DEP1/promote" >/dev/null
[ "$(api GET "/v1/deployments/$DEP1" | jq -r .state)" = live ] || { echo "not live" >&2; exit 1; }
note "revision 1 is live (blue)"

step "Deploy revision 2 — opposite slot, shared pinned db"
DEPLOY2=$(api POST "/v1/envs/$ENV_ID/deployments" '{"created_by":"demo"}')
DEP2=$(jq -r .id <<<"$DEPLOY2")
note "revision 2  slot $(jq -r .slot <<<"$DEPLOY2")"
wait_state "$DEP2" healthy

step "Blue and green coexist; the pinned db is shared, not duplicated"
docker ps --filter "label=cc.env=$(printf %s "$ENV_ID" | tr -d - | cut -c1-8)" \
  --format '  {{.Names}}  {{.Status}}' | sort
note "one cc-…-pinned-db; api/cache exist per revision. Slice B flips traffic + tears down blue."

step "History"
api GET "/v1/envs/$ENV_ID/deployments" | jq -r '.deployments[] | "  r\(.revision)  \(.slot)\t\(.state)"'

printf '\n\033[32m✓ agent-driven rollout to healthy, over HTTP, with real containers\033[0m\n'
note "org=$ORG env=$ENV_ID"
