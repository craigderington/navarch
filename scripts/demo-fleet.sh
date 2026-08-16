#!/usr/bin/env bash
# Two nodes, two daemons, and ingress that no longer cares which is which.
#
# The assertions a single-node stack cannot make: that placement spreads across
# the fleet, that a tenant's containers exist on *that node's* Docker daemon and
# nowhere else, and — the point of Slice C — that a stack on the node with no
# router is served through the node that has one. A demo that only read the
# database would pass with the agent switched off.
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

psql() { docker compose exec -T postgres psql -U composectl -d composectl -tAc "$1" | tr -d '[:space:]'; }
placed() { psql "SELECT n.hostname FROM deployments d
                   JOIN environments e ON e.id = d.environment_id
                   JOIN nodes n ON n.id = e.home_node_id
                  WHERE d.id = '$1'"; }
env8_of() { psql "SELECT replace(e.id::text,'-','') FROM deployments d
                    JOIN environments e ON e.id = d.environment_id
                   WHERE d.id = '$1'" | cut -c1-8; }

GW_PORT=$(docker compose config --format json | jq -r '.services.traefik.ports[] | select(.target==80) | .published')

step "The fleet"
# No `| head`: it closes the pipe, navarch takes SIGPIPE and pipefail turns a
# cosmetic truncation into a failed demo.
$NAV -o json node list --org dev \
  | jq -r '.[]|select(.state=="ready")|"  \(.hostname)  \(.advertise_addr)  \((.labels // {}) | to_entries | map("\(.key)=\(.value)") | join(",") | if . == "" then "-" else . end)"'
READY=$($NAV -o json node list --org dev | jq -r '[.[]|select(.state=="ready")]|length')
[ "$READY" -ge 2 ] || fail "need two ready nodes, have $READY — is dind-b up?"
ROUTER_NODE=$($NAV -o json node list --org dev | jq -r '[.[]|select(.state=="ready" and .labels.ingress=="true")][0].hostname')
[ -n "$ROUTER_NODE" ] && [ "$ROUTER_NODE" != "null" ] || fail "no node advertises ingress=true"
note "the router runs on $ROUTER_NODE"

step "Catalog: one stack with no ingress service, two with one"
$NAV app create fleet-$SUFFIX --org dev >/dev/null
for s in jobs web1 web2; do $NAV stack create $s --app dev/fleet-$SUFFIX >/dev/null; done
$NAV stack push dev/fleet-$SUFFIX/jobs examples/worker/compose.yaml >/dev/null
$NAV stack push dev/fleet-$SUFFIX/web1 examples/hello/compose.yaml >/dev/null
$NAV stack push dev/fleet-$SUFFIX/web2 examples/hello/compose.yaml >/dev/null

deploy_web() { # stack -> deployment id, on stdout
  local st=$1 host=$2
  $NAV env create prod --stack dev/fleet-$SUFFIX/$st --hostname "$host" >/dev/null
  $NAV secret set --env dev/fleet-$SUFFIX/$st/prod db_password "fleet-$SUFFIX" >/dev/null
  $NAV -o json deploy --env dev/fleet-$SUFFIX/$st/prod | jq -r .id
}

step "Deploy the no-ingress stack"
$NAV env create prod --stack dev/fleet-$SUFFIX/jobs >/dev/null
JOBS=$($NAV -o json deploy --env dev/fleet-$SUFFIX/jobs/prod | jq -r .id)
$NAV wait "$JOBS" --state live --timeout 300 >/dev/null
JOBS_NODE=$(placed "$JOBS")
note "jobs -> $JOBS_NODE"

step "Deploy two ingress stacks — spread scoring puts them on different nodes"
W1=$(deploy_web web1 "web1-$SUFFIX.example.com")
$NAV wait "$W1" --state live --timeout 300 >/dev/null
W2=$(deploy_web web2 "web2-$SUFFIX.example.com")
$NAV wait "$W2" --state live --timeout 300 >/dev/null
W1_NODE=$(placed "$W1"); W2_NODE=$(placed "$W2")
note "web1 -> $W1_NODE"
note "web2 -> $W2_NODE"
[ "$W1_NODE" != "$W2_NODE" ] || fail "both ingress stacks landed on $W1_NODE — is placement scoring by spread?"

step "Both serve traffic, including the one on the node with no router"
for pair in "web1-$SUFFIX.example.com|$W1_NODE" "web2-$SUFFIX.example.com|$W2_NODE"; do
  host=${pair%%|*}; node=${pair##*|}
  code=$(curl -s -o /dev/null -w '%{http_code}' -H "Host: $host" "http://localhost:$GW_PORT/")
  [ "$code" = "200" ] || fail "expected 200 for $host on $node, got $code"
  if [ "$node" = "$ROUTER_NODE" ]; then note "$host on $node (the router's own node): HTTP 200"
  else note "$host on $node — no router there, served across the fleet: HTTP 200"; fi
done

# The whole point of the slice: at least one of those was NOT the router's node.
[ "$W1_NODE" != "$ROUTER_NODE" ] || [ "$W2_NODE" != "$ROUTER_NODE" ] \
  || fail "neither ingress stack landed off the router node, so cross-node ingress was not exercised"

step "The route targets a node address and a published port, not a container name"
docker compose exec -T traefik cat /dynamic/composectl.yml | grep -A 3 'services:' | sed 's/^/  /' | tail -4
grep_target=$(docker compose exec -T traefik cat /dynamic/composectl.yml | grep -c 'url: http://[0-9]' || true)
[ "$grep_target" -ge 2 ] || fail "expected routes to target addresses, not container names"

step "The no-ingress stack's containers are on its own node's daemon only"
JOBS_ENV8=$(env8_of "$JOBS")
if [ "$JOBS_NODE" != "$ROUTER_NODE" ]; then
  INNER=$(docker compose exec -T dind-b docker ps --filter "label=cc.env=$JOBS_ENV8" --format '{{.Names}}' | tr -d '\r')
  OUTER=$(docker ps --filter "label=cc.env=$JOBS_ENV8" --format '{{.Names}}')
  [ -n "$INNER" ] || fail "no containers for $JOBS_ENV8 on the second node's daemon"
  [ -z "$OUTER" ] || fail "containers for $JOBS_ENV8 leaked onto the host daemon: $OUTER"
  printf '%s\n' "$INNER" | sed 's/^/  inside dind-b: /'
  note "and nothing for this environment on the host daemon"
else
  note "jobs landed on the router node this run; the daemon-isolation check needs it on the other node"
fi

printf '\n\033[32m✓ two nodes, two daemons: placement spread across the fleet, and a stack on the node with no router served through the node that has one\033[0m\n'
