#!/usr/bin/env bash
# Two nodes, two daemons, two placements. The assertions that matter are the
# ones a single-node stack cannot make: that a stack without an ingress service
# is placed on the node that has no router, and that its containers exist on
# *that node's* Docker daemon and nowhere else. A demo that only checked the
# database would pass just as well if the agent never ran.
set -euo pipefail

API=${API:-http://localhost:8417}
API_TOKEN=${API_TOKEN:-dev-token-change-me}
NAV=${NAV:-./bin/navarch}
export NAVARCH_URL=$API NAVARCH_TOKEN=$API_TOKEN
SUFFIX=${SUFFIX:-$RANDOM}

step() { printf '\n\033[36m▸ %s\033[0m\n' "$1"; }
note() { printf '  \033[90m%s\033[0m\n' "$1"; }
fail() { printf '\033[31mFAIL: %s\033[0m\n' "$1" >&2; exit 1; }

[ -x "$NAV" ] || fail "$NAV not built — run 'make build' first"

step "The fleet"
# No `| head`: it closes the pipe, navarch takes SIGPIPE and pipefail turns a
# cosmetic truncation into a failed demo.
$NAV -o json node list --org dev \
  | jq -r '.[]|select(.state=="ready")|"  \(.hostname)  \((.labels // {}) | to_entries | map("\(.key)=\(.value)") | join(",") | if . == "" then "-" else . end)"'

INGRESS_NODE=$($NAV -o json node list --org dev | jq -r '[.[]|select(.state=="ready" and .labels.ingress=="true")][0].hostname')
PLAIN_NODE=$($NAV -o json node list --org dev | jq -r '[.[]|select(.state=="ready" and (.labels.ingress != "true"))][0].hostname')
[ -n "$INGRESS_NODE" ] && [ "$INGRESS_NODE" != "null" ] || fail "no ready node advertises ingress=true"
[ -n "$PLAIN_NODE" ] && [ "$PLAIN_NODE" != "null" ] || fail "need a second ready node without the ingress label — is dind-b up?"
note "ingress node: $INGRESS_NODE   plain node: $PLAIN_NODE"

step "Catalog"
$NAV app create fleet-$SUFFIX --org dev >/dev/null
$NAV stack create web  --app dev/fleet-$SUFFIX >/dev/null
$NAV stack create jobs --app dev/fleet-$SUFFIX >/dev/null
$NAV stack push dev/fleet-$SUFFIX/web  examples/hello/compose.yaml  >/dev/null
$NAV stack push dev/fleet-$SUFFIX/jobs examples/worker/compose.yaml >/dev/null
note "two stacks: web (has an ingress service), jobs (has none)"

step "Deploy the ingress stack — only the ingress node can serve it"
$NAV env create prod --stack dev/fleet-$SUFFIX/web --hostname web-$SUFFIX.example.com >/dev/null
$NAV secret set --env dev/fleet-$SUFFIX/web/prod db_password "fleet-$SUFFIX" >/dev/null
WEB=$($NAV -o json deploy --env dev/fleet-$SUFFIX/web/prod | jq -r .id)
$NAV wait "$WEB" --state live --timeout 240 >/dev/null
note "web is live"

step "Deploy the no-ingress stack — free to land anywhere, so scoring spreads it"
$NAV env create prod --stack dev/fleet-$SUFFIX/jobs >/dev/null
JOBS=$($NAV -o json deploy --env dev/fleet-$SUFFIX/jobs/prod | jq -r .id)
$NAV wait "$JOBS" --state live --timeout 240 >/dev/null
note "jobs is live"

step "Where did each one land?"
placed() { # deployment id -> node hostname
  docker compose exec -T postgres psql -U composectl -d composectl -tAc \
    "SELECT n.hostname FROM deployments d
       JOIN environments e ON e.id = d.environment_id
       JOIN nodes n ON n.id = e.home_node_id
      WHERE d.id = '$1'"
}
WEB_NODE=$(placed "$WEB" | tr -d '[:space:]')
JOBS_NODE=$(placed "$JOBS" | tr -d '[:space:]')
note "web  -> $WEB_NODE"
note "jobs -> $JOBS_NODE"

[ "$WEB_NODE" = "$INGRESS_NODE" ] \
  || fail "a stack with an ingress service must be placed on the ingress node, got $WEB_NODE"
[ "$JOBS_NODE" = "$PLAIN_NODE" ] \
  || fail "the no-ingress stack should have spread to $PLAIN_NODE, got $JOBS_NODE — is placement scoring by spread?"

step "The containers are on the second node's own daemon, not the host's"
# This is the assertion that makes the fleet real: dind-b is a separate Docker
# daemon, so a container visible there and absent here is running on a different
# host in every sense the platform cares about.
JOBS_ENV8=$(docker compose exec -T postgres psql -U composectl -d composectl -tAc \
  "SELECT replace(e.id::text,'-','') FROM deployments d JOIN environments e ON e.id=d.environment_id WHERE d.id='$JOBS'" \
  | tr -d '[:space:]' | cut -c1-8)
note "jobs env8=$JOBS_ENV8"

INNER=$(docker compose exec -T dind-b docker ps --filter "label=cc.env=$JOBS_ENV8" --format '{{.Names}}' | tr -d '\r')
OUTER=$(docker ps --filter "label=cc.env=$JOBS_ENV8" --format '{{.Names}}')
printf '%s\n' "$INNER" | sed 's/^/  inside dind-b: /'
[ -n "$INNER" ] || fail "no containers for $JOBS_ENV8 on the second node's daemon"
[ -z "$OUTER" ] || fail "containers for $JOBS_ENV8 leaked onto the host daemon: $OUTER"
note "and nothing for this environment on the host daemon"

step "The ingress stack still serves traffic"
GW_PORT=$(docker compose config --format json | jq -r '.services.traefik.ports[] | select(.target==80) | .published')
code=$(curl -s -o /dev/null -w '%{http_code}' -H "Host: web-$SUFFIX.example.com" "http://localhost:$GW_PORT/")
[ "$code" = "200" ] || fail "expected 200 through Traefik, got $code"
note "HTTP 200 through Traefik"

printf '\n\033[32m✓ two nodes on two daemons: the ingress stack placed where a router exists, the no-ingress stack spread to the other node and running on its daemon\033[0m\n'
