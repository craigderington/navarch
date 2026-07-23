#!/usr/bin/env bash
# End-to-end walk through the Sprint 1 control plane: create the catalog,
# push a compose file, deploy it, and promote it.
#
# Advancing a deployment through pending → healthy is the node agent's job
# (Sprint 2). Until that exists this script drives those transitions with
# SQL and says so — every other step is a real HTTP call.
set -euo pipefail

API=${API:-http://localhost:8417}
COMPOSE=${COMPOSE:-examples/webapp/compose.yaml}
SUFFIX=${SUFFIX:-$RANDOM}

step() { printf '\n\033[36m▸ %s\033[0m\n' "$1"; }
note() { printf '  \033[90m%s\033[0m\n' "$1"; }

# post METHOD PATH [BODY] — fails loudly on a non-2xx response.
api() {
  local method=$1 path=$2 body=${3-}
  local out code
  if [ -n "$body" ]; then
    out=$(curl -sS -X "$method" "$API$path" -H 'Content-Type: application/json' \
      -d "$body" -w '\n%{http_code}')
  else
    out=$(curl -sS -X "$method" "$API$path" -w '\n%{http_code}')
  fi
  code=$(tail -n1 <<<"$out")
  out=$(sed '$d' <<<"$out")
  if [[ ! $code =~ ^2 ]]; then
    printf '\033[31m  %s %s -> HTTP %s\033[0m\n%s\n' "$method" "$path" "$code" "$out" >&2
    exit 1
  fi
  printf '%s' "$out"
}

sql() { docker compose exec -T postgres psql -qtAX -U composectl -d composectl -c "$1"; }

step "Organization"
ORG=$(api POST /v1/orgs "{\"slug\":\"acme-$SUFFIX\",\"name\":\"Acme\"}" | jq -r .id)
note "org $ORG"

step "Application"
APP=$(api POST "/v1/orgs/$ORG/apps" "{\"slug\":\"web-$SUFFIX\",\"name\":\"Web\"}" | jq -r .id)
note "app $APP"

step "Stack"
STACK=$(api POST "/v1/apps/$APP/stacks" "{\"slug\":\"main-$SUFFIX\"}" | jq -r .id)
note "stack $STACK"

step "Stack version from $COMPOSE"
VERSION=$(curl -sS -X POST "$API/v1/stacks/$STACK/versions?created_by=demo" \
  --data-binary "@$COMPOSE")
echo "$VERSION" | jq -e .id >/dev/null || { echo "$VERSION" >&2; exit 1; }
note "version $(jq -r .version <<<"$VERSION")  digest $(jq -r .spec_digest <<<"$VERSION" | cut -c1-16)…"

step "Pushing the same file again"
SAME=$(curl -sS -X POST "$API/v1/stacks/$STACK/versions" --data-binary "@$COMPOSE" | jq -r .version)
note "still version $SAME — identical digest does not manufacture churn"

step "Environment"
ENV_ID=$(api POST "/v1/stacks/$STACK/envs" \
  '{"slug":"prod","hostname":"prod.example.com","config":{"LOG_LEVEL":"info"}}' | jq -r .id)
note "env $ENV_ID"

step "Deployment"
DEPLOY=$(api POST "/v1/envs/$ENV_ID/deployments" '{"created_by":"demo"}')
DEP_ID=$(jq -r .id <<<"$DEPLOY")
note "revision $(jq -r .revision <<<"$DEPLOY")  slot $(jq -r .slot <<<"$DEPLOY")  project $(jq -r .project_name <<<"$DEPLOY")"
note "peak rollout memory $(jq -r .peak_memory_bytes <<<"$DEPLOY") bytes"

step "A second deployment is refused while one is active"
if curl -sS -o /dev/null -w '%{http_code}' -X POST "$API/v1/envs/$ENV_ID/deployments" \
    -H 'Content-Type: application/json' -d '{}' | grep -q 409; then
  note "409 Conflict — deployments_one_active_idx holds"
else
  echo "expected 409 for a second active deployment" >&2; exit 1
fi

step "Promoting before it is healthy is refused"
if curl -sS -o /dev/null -w '%{http_code}' -X POST "$API/v1/deployments/$DEP_ID/promote" | grep -q 409; then
  note "409 Conflict — only a healthy deployment may be promoted"
else
  echo "expected 409 promoting a pending deployment" >&2; exit 1
fi

step "Standing in for the Sprint 2 agent (SQL, not HTTP)"
for state in scheduling starting healthy; do
  sql "UPDATE deployments SET state='$state' WHERE id='$DEP_ID';" >/dev/null
  note "state -> $state"
done

step "Promote"
api POST "/v1/deployments/$DEP_ID/promote" >/dev/null
note "promoted"

LIVE=$(sql "SELECT live_deployment_id FROM environments WHERE id='$ENV_ID';" | tr -d ' ')
STATE=$(sql "SELECT state FROM deployments WHERE id='$DEP_ID';" | tr -d ' ')
[ "$LIVE" = "$DEP_ID" ] || { echo "environment does not point at the promoted deployment" >&2; exit 1; }
[ "$STATE" = "live" ] || { echo "expected state live, got $STATE" >&2; exit 1; }
note "environment repointed and deployment is live"

step "Second revision takes the opposite slot"
DEPLOY2=$(api POST "/v1/envs/$ENV_ID/deployments" '{"created_by":"demo"}')
note "revision $(jq -r .revision <<<"$DEPLOY2")  slot $(jq -r .slot <<<"$DEPLOY2")"

step "History"
api GET "/v1/envs/$ENV_ID/deployments" | jq -r '.deployments[] | "  r\(.revision)  \(.slot)\t\(.state)"'

printf '\n\033[32m✓ full loop exercised over HTTP\033[0m\n'
note "org=$ORG stack=$STACK env=$ENV_ID"
