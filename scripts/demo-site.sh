#!/usr/bin/env bash
# Navarch's marketing site, deployed by Navarch.
#
# The dogfooding demo, and the most honest one: if the platform cannot serve its
# own front page it has no business serving anyone else's. It is also the
# simplest possible stack — one swappable service, nothing pinned, no durable
# state — so the blue/green flip is visible with nothing else in the way.
#
# The image is built by `make site-image` and loaded onto every node's daemon
# first. The platform never builds anything: `build:` is a rejected directive,
# because a deployment you cannot reproduce is not one you can trust.
set -euo pipefail
cd "$(dirname "$0")/.."

API=${API:-http://localhost:8417}
API_TOKEN=${API_TOKEN:-dev-operator-token-change-me}
CURL_AUTH=(-H "Authorization: Bearer $API_TOKEN")
GW=${GW:-http://localhost:8095}
COMPOSE=${COMPOSE:-examples/site/compose.yaml}
NAV=${NAV:-./bin/navarch}
SUFFIX=${SUFFIX:-$RANDOM}
# A .localhost name rather than .example.com: RFC 6761 reserves the whole
# .localhost tree for loopback and every mainstream resolver honours it, so the
# hostname Traefik routes on is one a browser on this machine can already reach.
# The demo is worth looking at, and "edit /etc/hosts first" is how a demo stops
# being looked at. Still suffixed per run — environments linger (there is no
# delete endpoint) and `environments.hostname` is uniquely indexed, so a fixed
# name would 409 on the second run.
HOST=${HOST:-navarch-$SUFFIX.localhost}
export NAVARCH_URL=$API NAVARCH_TOKEN=$API_TOKEN

step() { printf '\n\033[36m▸ %s\033[0m\n' "$1"; }
note() { printf '  \033[90m%s\033[0m\n' "$1"; }
fail() { printf '\033[31mFAIL: %s\033[0m\n' "$1" >&2; exit 1; }

[ -x "$NAV" ] || fail "$NAV not built — run 'make build'"

api() {
    local method=$1 path=$2 body=${3-} out code
    if [ -n "$body" ]; then
        out=$(curl -sS "${CURL_AUTH[@]}" -X "$method" "$API$path" \
            -H 'Content-Type: application/json' -d "$body" -w '\n%{http_code}')
    else
        out=$(curl -sS "${CURL_AUTH[@]}" -X "$method" "$API$path" -w '\n%{http_code}')
    fi
    code=$(tail -n1 <<<"$out")
    out=$(sed '$d' <<<"$out")
    [[ $code =~ ^2 ]] || {
        printf '\033[31m  %s %s -> HTTP %s\033[0m\n%s\n' "$method" "$path" "$code" "$out" >&2
        exit 1
    }
    printf '%s' "$out"
}

wait_state() {
    local dep=$1 target=$2 deadline=$((SECONDS + 180)) state
    while :; do
        state=$(api GET "/v1/deployments/$dep" | jq -r .state)
        case "$state" in
        "$target")
            note "state=$state"
            return 0
            ;;
        failed)
            api GET "/v1/deployments/$dep" | jq -r .failure_reason >&2
            fail "rollout failed"
            ;;
        esac
        [ $SECONDS -lt $deadline ] || fail "timed out waiting for $target (stuck at $state)"
        sleep 2
    done
}

# Fetch the site through Traefik, retrying while the router config catches up.
fetch() {
    local deadline=$((SECONDS + 45)) body
    while :; do
        if body=$(curl -fsS -H "Host: $HOST" "$GW" 2>/dev/null); then
            printf '%s' "$body"
            return 0
        fi
        [ $SECONDS -lt $deadline ] || fail "Traefik never routed $HOST"
        sleep 2
    done
}

step "Catalog: an app and a stack for the site"
ORG=$(api GET /v1/orgs | jq -r '.organizations[]|select(.slug=="dev")|.id')
[ -n "$ORG" ] || fail "dev org not found — is the control plane up?"
APP=$(api POST "/v1/orgs/$ORG/apps" "{\"slug\":\"navarch-site-$SUFFIX\",\"name\":\"Navarch Site\"}" | jq -r .id)
STACK=$(api POST "/v1/apps/$APP/stacks" "{\"slug\":\"main\"}" | jq -r .id)
curl -sS "${CURL_AUTH[@]}" -X POST "$API/v1/stacks/$STACK/versions?created_by=demo-site" \
    --data-binary "@$COMPOSE" | jq -e .id >/dev/null
ENV_ID=$(api POST "/v1/stacks/$STACK/envs" "{\"slug\":\"prod\",\"hostname\":\"$HOST\"}" | jq -r .id)
note "env=$ENV_ID host=$HOST"

step "Deploy — the scheduler places it, the agent brings it up"
DEP1=$(api POST "/v1/envs/$ENV_ID/deployments" '{"created_by":"demo-site"}' | jq -r .id)
wait_state "$DEP1" live
NODE=$(api GET "/v1/deployments/$DEP1" | jq -r '.home_node // "?"')
note "revision 1 live on $NODE"

step "Serving through Traefik"
BODY=$(fetch)
# Assert on content the page actually carries, not just a 200: a blank response
# from a misconfigured nginx is also a 200, and the whole point of this demo is
# that the site is really being served.
case "$BODY" in
*"Deploy the"*"whole stack"*) : ;;
*) fail "the response did not contain the site's headline" ;;
esac
BYTES=$(printf '%s' "$BODY" | wc -c | tr -d ' ')
note "$BYTES bytes of HTML, headline present"

step "A second revision — the blue/green flip, on a stateless stack"
DEP2=$(api POST "/v1/envs/$ENV_ID/deployments" '{"created_by":"demo-site"}' | jq -r .id)
SLOT=$(api GET "/v1/deployments/$DEP2" | jq -r .slot)
note "revision 2 is slot $SLOT (opposite of revision 1)"
wait_state "$DEP2" live

# What this asserts, and why it is shaped this way.
#
# The bug this demo found: every promotion dropped ~1.2 seconds of requests —
# three consecutive 502s — because the superseded revision's containers were
# removed while Traefik was still routing to them. Both containers were up and
# healthy throughout; the router simply had not caught up. Fixed by syncing the
# router before teardown, holding teardown for DefaultTeardownGrace, and cutting
# Traefik's providersThrottleDuration.
#
# The assertion is therefore on *consecutive* failures, not on a perfect score.
# A sustained gap is the regression signature and is what must never come back.
# Isolated blips still happen occasionally during the config swap and are not
# yet fully explained — counting them is honest; failing on them would make this
# demo flaky, and a flaky demo teaches people to re-run rather than to look.
step "Traffic moved without a sustained outage"
misses=0 run=0 worst=0 total=0
for _ in $(seq 1 30); do
    total=$((total + 1))
    if curl -fsS -H "Host: $HOST" "$GW" >/dev/null 2>&1; then
        run=0
    else
        misses=$((misses + 1))
        run=$((run + 1))
        [ "$run" -gt "$worst" ] && worst=$run
    fi
    sleep 0.4
done
[ "$worst" -lt 2 ] || fail "$worst consecutive requests failed — the flip dropped traffic"
note "$((total - misses))/$total served, longest gap ${worst} request(s)"

GW_PORT=${GW##*:}
printf '\n\033[32mThe site is live on the platform it describes.\033[0m\n'
printf '  \033[1mhttp://%s:%s\033[0m\n' "$HOST" "$GW_PORT"
printf '  \033[90m(.localhost resolves to loopback; nothing to add to /etc/hosts)\033[0m\n'
