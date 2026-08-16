#!/usr/bin/env bash
# Shared fleet helpers for the demo scripts.
#
# Every demo that inspects containers has the same problem: the scheduler decides
# which node an environment lands on, so the demo cannot assume a daemon. It has
# to ask where the platform actually put it and then talk to that node's Docker.
# Both demo-fleet and demo-preview grew their own version of this; the copies
# disagreed about what to do with an unknown node, and neither survived the fleet
# growing past two nodes. This is the one copy.

# fleet_psql SQL — run a query against the dev database, trimmed to a bare value.
fleet_psql() {
  docker compose exec -T postgres psql -U composectl -d composectl -tAc "$1" | tr -d '[:space:]'
}

# fleet_node_for_env ENV_ID — the hostname of the node an environment is homed
# on, empty if it has not been placed yet.
#
# Ordering trap: home_node_id is NULL until the FIRST placement, so calling this
# before a deployment is live always answers "not placed". Every caller must wait
# for the deployment first, or it will inspect the wrong machine — and get the
# most confusing possible answer, which is "no containers".
fleet_node_for_env() {
  fleet_psql "SELECT COALESCE(n.hostname,'')
                FROM environments e
                LEFT JOIN nodes n ON n.id = e.home_node_id
               WHERE e.id = '$1'"
}

# fleet_docker_for_node HOSTNAME — the docker invocation for that node's daemon.
#
# Derived from the hostname by suffix (dev-node-3 -> dind-3) rather than looked
# up in a table, so adding a node to compose.yaml needs no change here.
#
# An unrecognised hostname is an error, deliberately. The older copies defaulted
# to the host daemon, which meant a typo or an unknown node inspected the wrong
# machine and reported "no containers" — a wrong answer that looks like a real
# finding. Now that no node is the host daemon, there is no sensible default.
fleet_docker_for_node() {
  local host=$1
  case "$host" in
    dev-node-*) printf 'docker compose exec -T dind-%s docker' "${host#dev-node-}" ;;
    "")         echo "fleet_docker_for_node: no node (environment not placed yet?)" >&2; return 1 ;;
    *)          echo "fleet_docker_for_node: unknown node '$host'" >&2; return 1 ;;
  esac
}

# fleet_docker_for_env ENV_ID — the two above, composed, for the common case.
fleet_docker_for_env() {
  local host
  host=$(fleet_node_for_env "$1") || return 1
  fleet_docker_for_node "$host"
}

# fleet_ready_nodes — hostnames of every ready node, one per line.
fleet_ready_nodes() {
  local nav=${NAV:-./bin/navarch}
  $nav -o json node list --org dev | jq -r '.[]|select(.state=="ready")|.hostname' | sort
}
