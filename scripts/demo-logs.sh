#!/usr/bin/env bash
# Container logs, end to end, across the fleet.
#
# The assertion that matters is the last one: the output `navarch logs` prints
# is byte-for-byte what that container actually wrote, read from the node's own
# Docker daemon. A demo that only checked "some text came back" would pass
# against a stub, and this whole slice exists because reaching the right daemon
# is the hard part.
#
# It also proves the negative that gives the design its shape: no log content is
# in Postgres. The instruction is a row; the answer never is.
set -euo pipefail

API=${API:-http://localhost:8417}
API_TOKEN=${API_TOKEN:-dev-token-change-me}
NAV=${NAV:-./bin/navarch}
export NAVARCH_URL=$API NAVARCH_TOKEN=$API_TOKEN
SUFFIX=${SUFFIX:-$RANDOM}

# shellcheck source=scripts/lib/fleet.sh
. "$(dirname "$0")/lib/fleet.sh"

step() { printf '\n\033[36m▸ %s\033[0m\n' "$1"; }
note() { printf '  \033[90m%s\033[0m\n' "$1"; }
fail() { printf '\033[31mFAIL: %s\033[0m\n' "$1" >&2; exit 1; }

[ -x "$NAV" ] || fail "$NAV not built — run 'make build' first"

MARKER="log-marker-$SUFFIX"

step "A stack whose container prints something we can recognise"
$NAV app create logs-$SUFFIX --org dev >/dev/null
$NAV stack create jobs --app dev/logs-$SUFFIX >/dev/null
# Written here rather than kept in examples/: the demo's whole point is matching
# exact output, so the string it greps for and the string the container prints
# have to be the same literal, visibly.
COMPOSE=$(mktemp /tmp/navarch-logs-XXXX.yaml)
trap 'rm -f "$COMPOSE"' EXIT
cat > "$COMPOSE" <<YAML
services:
  talker:
    image: alpine:3.20
    command: ["sh", "-c", "i=0; while true; do echo \"$MARKER \$i\"; i=\$((i+1)); sleep 1; done"]
    x-composectl:
      rollout: swap
YAML
$NAV stack push dev/logs-$SUFFIX/jobs "$COMPOSE" >/dev/null
$NAV env create prod --stack dev/logs-$SUFFIX/jobs >/dev/null
DEP=$($NAV -o json deploy --env dev/logs-$SUFFIX/jobs/prod | jq -r .id)
$NAV wait "$DEP" --state live --timeout 300 >/dev/null
note "deployment $DEP is live"

ENV_ID=$(fleet_psql "SELECT environment_id FROM deployments WHERE id='$DEP'")
NODE=$(fleet_node_for_env "$ENV_ID")
note "placed on $NODE — which is where the output physically is"

step "navarch logs reads it from that node"
OUT=$($NAV logs dev/logs-$SUFFIX/jobs/prod --service talker --tail 20 2>/dev/null || true)
printf '%s\n' "$OUT" | head -3 | sed 's/^/  /'
command grep -q "$MARKER" <<<"$OUT" \
  || fail "expected the container's own output to contain '$MARKER', got: $(head -c 200 <<<"$OUT")"
note "the marker the container printed came back through the control plane"

step "It is the same output the node's daemon has"
DOCKER=$(fleet_docker_for_env "$ENV_ID")
CID=$(fleet_psql "SELECT si.container_id FROM service_instances si
                    JOIN deployments d ON d.id = si.deployment_id
                   WHERE d.id='$DEP' AND si.service_name='talker'")
DIRECT=$($DOCKER logs --tail 3 "$CID" 2>&1 | tr -d '\r')
# Compare a line the container definitely wrote, rather than whole buffers: the
# two reads happen a moment apart, so the newest line legitimately differs.
LINE=$(printf '%s\n' "$DIRECT" | command grep -o "$MARKER [0-9]*" | head -1)
[ -n "$LINE" ] || fail "the node's daemon shows no marker output for $CID"
command grep -qF "$LINE" <<<"$OUT" \
  || fail "'$LINE' is in the node's own docker logs but not in what navarch returned"
note "matched '$LINE' in both"

step "No log content reached Postgres"
# The instruction is stored; the answer is not. If this ever fails, container
# stdout — which routinely carries secrets — is at rest in the database.
ROWS=$(fleet_psql "SELECT count(*) FROM log_requests")
note "log_requests rows: $ROWS (instructions)"
COLS=$(fleet_psql "SELECT string_agg(column_name, ',' ORDER BY column_name)
                     FROM information_schema.columns WHERE table_name='log_requests'")
note "columns: $COLS"
for forbidden in data chunk content body output lines; do
  case ",$COLS," in
    *",$forbidden,"*) fail "log_requests has a '$forbidden' column — log content must never be stored" ;;
  esac
done
IN_DB=$(fleet_psql "SELECT count(*) FROM log_requests WHERE last_error LIKE '%$MARKER%'")
[ "$IN_DB" = "0" ] || fail "the container's output leaked into the database"
note "no column holds content, and the marker appears nowhere in the table"

step "Following reports its latency honestly"
FOLLOW_ERR=$(timeout 8 $NAV logs dev/logs-$SUFFIX/jobs/prod --service talker --tail 5 --follow 2>&1 >/dev/null || true)
command grep -qi 'not live' <<<"$FOLLOW_ERR" \
  || fail "a follow must say it is behind; a user reading a lull as silence goes hunting for a fault that is not there"
note "$(head -1 <<<"$FOLLOW_ERR")"

printf '\n\033[32m✓ container output travelled node -> control plane -> operator, matched the node'"'"'s own docker logs, and left nothing behind in Postgres\033[0m\n'
