#!/usr/bin/env bash
# The fleet demo: N nodes, N daemons, and ingress that belongs to none of them.
#
# The assertions a single-node stack cannot make:
#   - the router runs on NO node's daemon, so every served stack is reached
#     across a daemon boundary rather than by sharing one;
#   - placement spreads across the fleet instead of piling onto one node;
#   - a tenant's containers exist on its own node's daemon and on no other.
#
# A demo that only read the database would pass with every agent switched off,
# which is why each of those is checked against real daemons.
set -euo pipefail

# Shared node->daemon resolution; see scripts/lib/fleet.sh.
. "$(dirname "$0")/lib/fleet.sh"

API=${API:-http://localhost:8417}
API_TOKEN=${API_TOKEN:-dev-operator-token-change-me}
NAV=${NAV:-./bin/navarch}
export NAVARCH_URL=$API NAVARCH_TOKEN=$API_TOKEN
SUFFIX=${SUFFIX:-$RANDOM}

step() { printf '\n\033[36m▸ %s\033[0m\n' "$1"; }
note() { printf '  \033[90m%s\033[0m\n' "$1"; }
fail() { printf '\033[31mFAIL: %s\033[0m\n' "$1" >&2; exit 1; }

[ -x "$NAV" ] || fail "$NAV not built — run 'make build' first"

placed() { fleet_psql "SELECT n.hostname FROM deployments d
                         JOIN environments e ON e.id = d.environment_id
                         JOIN nodes n ON n.id = e.home_node_id
                        WHERE d.id = '$1'"; }
env8_of() { fleet_psql "SELECT replace(e.id::text,'-','') FROM deployments d
                          JOIN environments e ON e.id = d.environment_id
                         WHERE d.id = '$1'" | cut -c1-8; }

# `first(...)` because the same container port is published once per address
# family — 127.0.0.1 and [::1] — and without it this yields two identical lines,
# making GW "http://localhost:8095\n8095". Every request then fails and the
# demo reports whatever it was checking as broken, which is exactly the wrong
# place to go looking.
GW_PORT=$(docker compose config --format json |
    jq -r 'first(.services.traefik.ports[] | select(.target==80) | .published)')

step "The fleet"
mapfile -t NODES < <(fleet_ready_nodes)
N=${#NODES[@]}
[ "$N" -ge 2 ] || fail "need at least two ready nodes, have $N — are the dind-* daemons up?"
$NAV -o json node list --org dev \
  | jq -r '.[]|select(.state=="ready")|"  \(.hostname)  \(.advertise_addr)  \((.labels // {}) | to_entries | map("\(.key)=\(.value)") | join(",") | if . == "" then "-" else . end)"'
note "$N ready nodes"

step "The router runs on no node's daemon"
# The symmetry property, asserted rather than assumed. While the router had to
# join a tenant's revision network to reach it, it had to share that tenant's
# daemon and one node was special forever. Now every node is reached the same
# way, so finding the router on any of them means that has regressed.
for node in "${NODES[@]}"; do
  d=$(fleet_docker_for_node "$node") || exit 1
  found=$($d ps --filter "label=cc.role=ingress-router" --format '{{.Names}}' | tr -d '\r')
  [ -z "$found" ] || fail "the ingress router is running on $node's daemon ($found) — the fleet is not symmetric"
done
note "checked all $N daemons: no router on any of them"

step "Catalog: one stack with no ingress service, $N with one"
$NAV app create fleet-$SUFFIX --org dev >/dev/null
$NAV stack create jobs --app dev/fleet-$SUFFIX >/dev/null
$NAV stack push dev/fleet-$SUFFIX/jobs examples/worker/compose.yaml >/dev/null
for i in $(seq 1 "$N"); do
  $NAV stack create web$i --app dev/fleet-$SUFFIX >/dev/null
  $NAV stack push dev/fleet-$SUFFIX/web$i examples/hello/compose.yaml >/dev/null
done

step "Deploy everything, then wait — the rollouts proceed in parallel across nodes"
# Deploying all of them before waiting on any is not just faster: it is what
# makes the spread assertion meaningful. Placement happens at deploy time, so
# issuing them together exercises the scheduler's choice across a fleet in one
# state rather than letting each rollout finish and change the picture first.
$NAV env create prod --stack dev/fleet-$SUFFIX/jobs >/dev/null
JOBS=$($NAV -o json deploy --env dev/fleet-$SUFFIX/jobs/prod | jq -r .id)
WEBS=()
for i in $(seq 1 "$N"); do
  $NAV env create prod --stack dev/fleet-$SUFFIX/web$i --hostname "web$i-$SUFFIX.example.com" >/dev/null
  $NAV secret set --env dev/fleet-$SUFFIX/web$i/prod db_password "fleet-$SUFFIX" >/dev/null
  WEBS+=("$($NAV -o json deploy --env dev/fleet-$SUFFIX/web$i/prod | jq -r .id)")
done
$NAV wait "$JOBS" --state live --timeout 600 >/dev/null
for d in "${WEBS[@]}"; do $NAV wait "$d" --state live --timeout 600 >/dev/null; done
note "all $((N + 1)) deployments live"

step "Where they landed"
JOBS_NODE=$(placed "$JOBS")
note "jobs -> $JOBS_NODE"
WEB_NODES=()
for i in "${!WEBS[@]}"; do
  n=$(placed "${WEBS[$i]}")
  WEB_NODES+=("$n")
  note "web$((i + 1)) -> $n"
done

# Spread, stated as a property of the distribution rather than "not the same
# node", which is all two nodes could express. Placement scores fewest-homed
# first, so N environments onto N nodes should touch nearly all of them; one
# collision is tolerated because environments left by earlier demo runs make the
# starting counts uneven. Everything landing on one node means scoring stopped
# spreading, and that is what this catches.
DISTINCT=$(printf '%s\n' "${WEB_NODES[@]}" | sort -u | wc -l)
WANT=$((N - 1)); [ "$WANT" -lt 2 ] && WANT=2
note "the $N ingress stacks occupy $DISTINCT distinct nodes (want at least $WANT)"
[ "$DISTINCT" -ge "$WANT" ] || fail "placement did not spread: $DISTINCT distinct nodes for $N stacks"

step "Every ingress stack serves, and none of them shares a daemon with the router"
# `live` and `routable` are not the same instant. Promotion marks the deployment
# live; the route appears when the controller's next tick resyncs the router and
# Traefik reloads the file. A single curl here raced that and reported 404 — a
# passing system looking broken, which is worse than a real failure because it
# sends you hunting in the router. Retry to a deadline, as demo-preview does.
serve_check() {
  local host=$1 node=$2 deadline=$((SECONDS + 45)) code=000
  while [ $SECONDS -lt $deadline ]; do
    code=$(curl -s -m 5 -o /dev/null -w '%{http_code}' -H "Host: $host" "http://localhost:$GW_PORT/")
    [ "$code" = "200" ] && break
    sleep 2
  done
  [ "$code" = "200" ] || fail "expected 200 for $host on $node, got $code after 45s"
  note "$host on $node: HTTP 200"
}
for i in "${!WEBS[@]}"; do
  serve_check "web$((i + 1))-$SUFFIX.example.com" "${WEB_NODES[$i]}"
done

step "Each tenant's containers exist on its own node's daemon and no other"
# The check the database cannot make. A deployment row says where the platform
# *intended* the containers to be; only the daemons say where they are.
check_isolation() { # deployment-id, node
  local dep=$1 owner=$2 env8 owner_docker inner
  env8=$(env8_of "$dep")
  owner_docker=$(fleet_docker_for_node "$owner") || exit 1
  inner=$($owner_docker ps --filter "label=cc.env=$env8" --format '{{.Names}}' | tr -d '\r')
  [ -n "$inner" ] || fail "no containers for $env8 on its own node $owner"
  for other in "${NODES[@]}"; do
    [ "$other" = "$owner" ] && continue
    local d stray
    d=$(fleet_docker_for_node "$other") || exit 1
    stray=$($d ps -a --filter "label=cc.env=$env8" --format '{{.Names}}' | tr -d '\r')
    [ -z "$stray" ] || fail "containers for $env8 (homed on $owner) also exist on $other: $stray"
  done
  note "$env8 on $owner only ($(printf '%s' "$inner" | tr '\n' ' '))"
}
check_isolation "$JOBS" "$JOBS_NODE"
for i in "${!WEBS[@]}"; do check_isolation "${WEBS[$i]}" "${WEB_NODES[$i]}"; done

step "Routes target node addresses and published ports, not container names"
docker compose exec -T traefik cat /dynamic/composectl.yml | command grep 'url: http' | tail -"$N" | sed 's/^/  /'
addressed=$(docker compose exec -T traefik cat /dynamic/composectl.yml | command grep -c 'url: http://[0-9]' || true)
[ "$addressed" -ge "$N" ] || fail "expected at least $N address-targeted routes, found $addressed"

printf '\n\033[32m✓ %s nodes, %s daemons, no router among them: placement spread across the fleet, every tenant confined to its own daemon, and all of them served from outside it\033[0m\n' "$N" "$N"
