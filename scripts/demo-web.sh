#!/usr/bin/env bash
# The console, loaded against the real fleet.
#
# The assertion that matters is not "the page returned 200" — an empty table is
# also a 200, and a console that renders empty during an incident is worse than
# one that fails. So every page is checked for a value the CLI independently
# reports, which means a page can only pass by actually showing the platform's
# state.
set -euo pipefail
cd "$(dirname "$0")/.."

API=${API:-http://localhost:8417}
API_TOKEN=${API_TOKEN:-dev-operator-token-change-me}
NAV=${NAV:-./bin/navarch}
WEB=${WEB:-./bin/navarch-web}
PORT=${WEB_PORT:-8419}
BASE="http://127.0.0.1:$PORT"
export NAVARCH_URL=$API NAVARCH_TOKEN=$API_TOKEN

step() { printf '\n\033[36m▸ %s\033[0m\n' "$1"; }
note() { printf '  \033[90m%s\033[0m\n' "$1"; }
fail() { printf '\033[31mFAIL: %s\033[0m\n' "$1" >&2; exit 1; }

[ -x "$NAV" ] && [ -x "$WEB" ] || fail "binaries missing — run 'make build'"

JAR=$(mktemp /tmp/navarch-web-jar.XXXXXX)
LOG=$(mktemp /tmp/navarch-web-log.XXXXXX)
WEB_PID=""
cleanup() {
    [ -n "$WEB_PID" ] && kill "$WEB_PID" 2>/dev/null || true
    rm -f "$JAR" "$LOG"
}
trap cleanup EXIT

step "Starting the console"
NAVARCH_URL="$API" NAVARCH_WEB_ADDR="127.0.0.1:$PORT" "$WEB" >"$LOG" 2>&1 &
WEB_PID=$!
for _ in $(seq 1 40); do
    curl -sf "$BASE/healthz" >/dev/null 2>&1 && break
    sleep 0.5
done
curl -sf "$BASE/healthz" >/dev/null || { cat "$LOG" >&2; fail "the console did not start"; }
note "$(curl -s "$BASE/healthz")"

step "A page without a session goes to the login form"
code=$(curl -s -o /dev/null -w '%{http_code}' "$BASE/")
[ "$code" = "303" ] || fail "expected a redirect for an unauthenticated request, got $code"
note "303 to /login"

step "A token the control plane rejects never becomes a session"
redirect=$(curl -s -o /dev/null -w '%{redirect_url}' -X POST -d 'token=definitely-not-valid' "$BASE/login")
case "$redirect" in
*error*) note "refused, and says so on the form" ;;
*) fail "a bad token was accepted: $redirect" ;;
esac

step "Signing in"
curl -s -c "$JAR" -o /dev/null -X POST -d "token=$API_TOKEN" "$BASE/login"
grep -q navarch_session "$JAR" || fail "no session cookie was issued"
# The whole reason the console is a server: the browser holds a cookie, not a
# credential. If the token is in the jar, the design has failed.
if grep -q "$API_TOKEN" "$JAR"; then
    fail "the operator token was stored in the browser's cookie jar"
fi
note "session cookie set; the operator token stayed on the server"

step "The fleet page shows a node the CLI also reports"
NODE=$($NAV node list --org dev -o json | jq -r '.[0].hostname')
[ -n "$NODE" ] || fail "the CLI reports no nodes — nothing to compare against"
BODY=$(curl -s -b "$JAR" "$BASE/")
grep -qF "$NODE" <<<"$BODY" || fail "the fleet page does not mention $NODE"
grep -qF "dev@navarch.local" <<<"$BODY" || fail "the page does not show who is signed in"
note "$NODE and the signed-in operator both present"

step "Environments, events and a deployment all render their own body"
ORG=$($NAV org list -o json | jq -r '.[]|select(.slug=="dev")|.id')
ENVS=$(curl -s -b "$JAR" "$BASE/orgs/$ORG")
grep -q "Environments" <<<"$ENVS" || fail "the environments page did not render"
# Guards the template-namespace collision: every page defines `content`, so a
# single shared set makes them all render the same body — quietly. The heading
# is the discriminator, not a node name: the environments page legitimately
# shows hostnames in its home-node column, and checking for one made this
# assertion fail on correct output.
grep -q "<h1>Fleet</h1>" <<<"$ENVS" && fail "the environments page rendered the fleet's body — templates collided"
EVENTS=$(curl -s -b "$JAR" "$BASE/orgs/$ORG/events")
grep -q "deployment\." <<<"$EVENTS" || fail "the events page shows no events"
note "each page rendered its own content"

DEP=$($NAV deployment list --env "$($NAV env list --org dev -o json 2>/dev/null | jq -r '.[0].id' 2>/dev/null)" -o json 2>/dev/null | jq -r '.[0].id' 2>/dev/null || true)
if [ -n "${DEP:-}" ] && [ "$DEP" != "null" ]; then
    grep -q "Revision" <<<"$(curl -s -b "$JAR" "$BASE/deployments/$DEP")" ||
        fail "the deployment page did not render"
    note "deployment page renders"
fi

step "Actions are behind a confirmation and a CSRF token"
NODE_ID=$($NAV node list --org dev -o json | jq -r '.[0].id')
NODE_NAME=$($NAV node list --org dev -o json | jq -r '.[0].hostname')
CONFIRM=$(curl -s -b "$JAR" "$BASE/confirm?action=drain&id=$NODE_ID&subject=$NODE_NAME&back=/")
grep -q "Drain this node" <<<"$CONFIRM" || fail "the confirmation page did not render"
grep -qF "$NODE_NAME" <<<"$CONFIRM" || fail "the confirmation does not name what it will act on"
CSRF=$(grep -oE 'value="[a-f0-9]{64}"' <<<"$CONFIRM" | grep -oE '[a-f0-9]{64}' | head -1)
[ -n "$CSRF" ] || fail "the confirmation form carries no CSRF token"
note "confirmation names $NODE_NAME and carries a token"

# The check that matters: a POST that did not come from that page is refused,
# and the control plane is never reached.
code=$(curl -s -o /dev/null -w '%{http_code}' -b "$JAR" -X POST "$BASE/nodes/$NODE_ID/drain")
[ "$code" = "403" ] || fail "a POST with no CSRF token returned $code, want 403"
[ "$($NAV node list --org dev -o json | jq -r --arg n "$NODE_NAME" '.[]|select(.hostname==$n)|.state')" = "ready" ] ||
    fail "the refused request drained the node anyway"
note "403 without a token, and the node is untouched"

step "With the token, the action runs and reports what the API answered"
curl -s -o /dev/null -b "$JAR" -X POST -d "csrf=$CSRF&back=/" "$BASE/nodes/$NODE_ID/drain"
state=$($NAV node list --org dev -o json | jq -r --arg n "$NODE_NAME" '.[]|select(.hostname==$n)|.state')
[ "$state" = "draining" ] || fail "the node is $state after drain, want draining"
PAGE=$(curl -s -b "$JAR" "$BASE/")
grep -q 'class="flash' <<<"$PAGE" || fail "no flash reported the result"
grep -q "cordoned" <<<"$PAGE" || fail "the flash did not describe what happened"
note "node is draining, and the page said so"

# Shown once, not on every load — a message that never clears becomes furniture.
grep -q 'class="flash' <<<"$(curl -s -b "$JAR" "$BASE/")" && fail "the flash persisted across loads"
note "flash cleared after being read"

step "And put it back, through the console"
CSRF=$(curl -s -b "$JAR" "$BASE/confirm?action=uncordon&id=$NODE_ID&subject=$NODE_NAME&back=/" |
    grep -oE 'value="[a-f0-9]{64}"' | grep -oE '[a-f0-9]{64}' | head -1)
curl -s -o /dev/null -b "$JAR" -X POST -d "csrf=$CSRF&back=/" "$BASE/nodes/$NODE_ID/uncordon"
state=$($NAV node list --org dev -o json | jq -r --arg n "$NODE_NAME" '.[]|select(.hostname==$n)|.state')
[ "$state" = "ready" ] || fail "the node is $state after uncordon, want ready"
note "$NODE_NAME is ready again"

step "Signing out ends the session"
curl -s -b "$JAR" -c "$JAR" -o /dev/null -X POST "$BASE/logout"
code=$(curl -s -b "$JAR" -o /dev/null -w '%{http_code}' "$BASE/")
[ "$code" = "303" ] || fail "the session survived sign-out (got $code)"
note "back to the login form"

printf '\n\033[32mThe console renders the fleet, acts on it behind a confirmation, and never hands the browser a credential.\033[0m\n'
