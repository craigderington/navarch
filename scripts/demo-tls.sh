#!/usr/bin/env bash
# The TLS posture, demonstrated rather than asserted.
#
# The control plane speaks plaintext HTTP and always will: it does not know
# whether it is behind TLS, and a process listening on loopback has no need of
# it. TLS terminates at a reverse proxy in front, and `deploy/tls/` is that
# proxy. This runs it.
#
# It exists because of what the empty-router-config bug taught this codebase: a
# config file that nobody has put the real software in front of is a claim, not
# a fact. `deploy/tls/Caddyfile` is the file an operator would deploy, with one
# extra site block using Caddy's internal CA instead of ACME — the reverse_proxy
# line, the headers and the admin-off are identical, so what passes here is what
# ships.
#
# The second half is the other side of the same bargain: the CLI refuses to put
# an operator token on a plaintext connection that could be read, so an operator
# who has not set up TLS finds out at the first command rather than never.
set -euo pipefail

cd "$(dirname "$0")/.."

API_TOKEN=${API_TOKEN:-dev-operator-token-change-me}
NAV=${NAV:-./bin/navarch}
HOST=navarch.localhost
PORT=8443
COMPOSE=(docker compose -f compose.yaml -f deploy/tls/compose.yaml)

step() { printf '\n\033[36m▸ %s\033[0m\n' "$1"; }
note() { printf '  \033[90m%s\033[0m\n' "$1"; }
fail() { printf '\033[31mFAIL: %s\033[0m\n' "$1" >&2; exit 1; }

[ -x "$NAV" ] || fail "$NAV not built — run 'make build' first"

CA=$(mktemp /tmp/navarch-tls-ca.XXXXXX.crt)
cleanup() {
    rm -f "$CA"
    step "Tearing down the proxy"
    "${COMPOSE[@]}" rm -sf tls-proxy >/dev/null 2>&1 || true
}
trap cleanup EXIT

step "Starting the TLS proxy in front of the control plane"
"${COMPOSE[@]}" up -d tls-proxy >/dev/null
# Caddy issues the internal certificate on startup, not on first request, but
# the file lands a moment after the container does.
for _ in $(seq 1 30); do
    if "${COMPOSE[@]}" exec -T tls-proxy \
        cat /data/caddy/pki/authorities/local/root.crt >"$CA" 2>/dev/null &&
        [ -s "$CA" ]; then
        break
    fi
    sleep 1
done
[ -s "$CA" ] || fail "Caddy never issued its internal CA certificate"
note "internal CA issued"

CURL=(curl -sS --cacert "$CA" --resolve "$HOST:$PORT:127.0.0.1")

step "Health over TLS"
# ssl_verify_result must be 0: a demo that accepted any certificate would pass
# against a man in the middle, which is the one thing TLS is here to stop.
out=$("${CURL[@]}" "https://$HOST:$PORT/healthz" -w '\n%{http_code} %{ssl_verify_result}')
code=$(echo "$out" | tail -1 | cut -d' ' -f1)
verify=$(echo "$out" | tail -1 | cut -d' ' -f2)
[ "$code" = "200" ] || fail "healthz over TLS returned $code"
[ "$verify" = "0" ] || fail "certificate did not verify (ssl_verify_result=$verify)"
note "200, certificate verified against Caddy's CA"

step "An authenticated operator route over TLS"
code=$("${CURL[@]}" -o /dev/null -w '%{http_code}' \
    -H "Authorization: Bearer $API_TOKEN" "https://$HOST:$PORT/v1/orgs")
[ "$code" = "200" ] || fail "GET /v1/orgs over TLS returned $code"
note "200 — the bearer token crossed an encrypted connection"

step "And the CLI through the proxy"
# SSL_CERT_FILE is how a private CA is trusted without installing it system
# wide; in production the certificate is publicly trusted and none of this is
# needed.
if ! SSL_CERT_FILE="$CA" NAVARCH_URL="https://$HOST:$PORT" NAVARCH_TOKEN="$API_TOKEN" \
    "$NAV" whoami >/dev/null; then
    fail "navarch whoami failed through the TLS proxy"
fi
note "navarch whoami answered over https://"

step "The other half: plaintext that could be read is refused"
# No opt-in set. The address is a LAN address, which is exactly the case the
# audit named — a shared network where a captured token is worth having.
if out=$(NAVARCH_URL=http://10.0.1.7:8417 NAVARCH_TOKEN="$API_TOKEN" \
    NAVARCH_INSECURE= "$NAV" whoami 2>&1); then
    fail "the CLI sent a token in the clear to a LAN address"
fi
case "$out" in
*NAVARCH_INSECURE*) note "refused, and the message names the way through" ;;
*) fail "refusal did not explain the override: $out" ;;
esac

# ...and loopback still works with no ceremony, or the guard would have made the
# ordinary case worse to no benefit.
if ! NAVARCH_URL=http://localhost:8417 NAVARCH_TOKEN="$API_TOKEN" "$NAV" whoami >/dev/null 2>&1; then
    fail "the guard broke plaintext to loopback, which is safe and expected"
fi
note "loopback is unaffected"

printf '\n\033[32mTLS posture verified: terminated at the proxy, refused where it is absent.\033[0m\n'
