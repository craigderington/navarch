#!/usr/bin/env bash
# One certificate for every preview, proven against a real ACME server.
#
# The claim the preview wildcard makes is arithmetic: N preview hostnames cost
# one certificate instead of N. That is the whole reason it exists — a preview
# name is minted per CI run and never reused, so with a certificate each,
# previews reach the CA's per-registered-domain weekly limit long before tenants
# do, and reaching it stops issuance for the whole install.
#
# So the load-bearing assertion here is the challenge COUNT, not that TLS works.
# Three hostnames, three routes, one DNS-01 challenge. A demo that only checked
# the pages load would pass just as well against a certificate per hostname,
# which is the thing being fixed.
#
# Nothing about this needs public DNS or a credential: Pebble is Let's Encrypt's
# throwaway CA and its challenge DNS server answers only itself. What that costs
# is the one substitution — the DNS provider is a local adapter rather than
# Cloudflare — and it buys the thing a unit test cannot reach, which is whether
# a real CA hands back the certificate our config asks for.
set -euo pipefail
cd "$(dirname "$0")/.."

DIR=deploy/wildcard
PREVIEW_DOMAIN=${PREVIEW_DOMAIN:-preview.navar.ch}
PORT=${PORT:-44443}
COMPOSE=(docker compose -f "$DIR/compose.yaml")

step() { printf '\n\033[36m▸ %s\033[0m\n' "$1"; }
note() { printf '  \033[90m%s\033[0m\n' "$1"; }
fail() { printf '\033[31mFAIL: %s\033[0m\n' "$1" >&2; "${COMPOSE[@]}" logs traefik 2>&1 | tail -30 >&2; exit 1; }

cleanup() { "${COMPOSE[@]}" down -v --remove-orphans >/dev/null 2>&1 || true; }
trap cleanup EXIT

HOSTS=(
  "pr-1-main-abc12345.$PREVIEW_DOMAIN"
  "pr-2-main-def67890.$PREVIEW_DOMAIN"
  "pr-3-main-ghi11111.$PREVIEW_DOMAIN"
)

step "The control plane's own router writes the config"
rm -rf "$DIR/dynamic"; mkdir -p "$DIR/dynamic"
# Through internal/router, not by hand. A demo that hand-wrote this YAML would
# prove Traefik can obtain a wildcard — never in doubt — and prove nothing about
# whether we ask for one.
go run ./deploy/wildcard/gen "$DIR/dynamic" "$PREVIEW_DOMAIN" backend "${HOSTS[@]}"
grep -q "main: '\\*.$PREVIEW_DOMAIN'" "$DIR/dynamic/composectl.yml" \
  || fail "generated config does not request the wildcard"
note "$(grep -c "main: '\*.$PREVIEW_DOMAIN'" "$DIR/dynamic/composectl.yml") routes, all asking for *.$PREVIEW_DOMAIN"

step "A throwaway CA, and the certificate it uses for its own API"
cid=$(docker create ghcr.io/letsencrypt/pebble:latest)
docker cp "$cid:/test/certs/pebble.minica.pem" "$DIR/pebble.minica.pem" >/dev/null
docker rm -f "$cid" >/dev/null
"${COMPOSE[@]}" up -d >/dev/null 2>&1
note "pebble + challtestsrv + traefik up"

step "Traefik obtains the wildcard over DNS-01"
deadline=$((SECONDS + 120)); challenges=0
while [ $SECONDS -lt $deadline ]; do
  challenges=$("${COMPOSE[@]}" logs adapter 2>/dev/null | grep -c '/present' || true)
  [ "$challenges" -ge 1 ] && break
  sleep 2
done
[ "$challenges" -ge 1 ] || fail "no DNS-01 challenge was ever presented"
# The record is _acme-challenge under the BASE domain, which is what a wildcard
# is proved with — not under any of the three hostnames.
"${COMPOSE[@]}" logs adapter 2>/dev/null | grep -q "_acme-challenge.$PREVIEW_DOMAIN" \
  || fail "the challenge was not for the wildcard's base domain"
note "challenge presented for _acme-challenge.$PREVIEW_DOMAIN"

step "Every preview serves, on a chain that verifies"
# Pebble's issuance root is generated at startup and served by its management
# API. Verifying against it rather than passing -k is the whole difference
# between checking TLS and checking that TLS is trusted; demo-tls makes the same
# point with ssl_verify_result.
"${COMPOSE[@]}" exec -T traefik sh -c \
  'wget -q --no-check-certificate -O - https://pebble:15000/roots/0' > "$DIR/root.pem" \
  || fail "could not fetch pebble's issuance root"

for host in "${HOSTS[@]}"; do
  deadline=$((SECONDS + 60)); code=""
  while [ $SECONDS -lt $deadline ]; do
    code=$(curl -s -o /dev/null -w "%{http_code}:%{ssl_verify_result}" --cacert "$DIR/root.pem" \
      --resolve "$host:$PORT:127.0.0.1" "https://$host:$PORT/" || true)
    [ "$code" = "200:0" ] && break
    sleep 2
  done
  [ "$code" = "200:0" ] || fail "$host returned $code (want 200:0)"
  note "$host  200, chain verified"
done

step "The certificate they share is the wildcard, not one each"
san=$(echo | openssl s_client -connect "127.0.0.1:$PORT" -servername "${HOSTS[0]}" 2>/dev/null \
  | openssl x509 -noout -ext subjectAltName 2>/dev/null | tr -d ' ')
echo "$san" | grep -q "DNS:\*.$PREVIEW_DOMAIN" \
  || fail "served certificate is not the wildcard: $san"
note "subjectAltName: *.$PREVIEW_DOMAIN"

# The assertion the whole demo is for. Three routes went in; if each had taken
# its own certificate there would be three challenges here.
final=$("${COMPOSE[@]}" logs adapter 2>/dev/null | grep -c '/present' || true)
[ "$final" -eq 1 ] || fail "expected exactly 1 challenge for 3 previews, saw $final"

printf '\n\033[32m%d preview hostnames, %d certificate.\033[0m\n' "${#HOSTS[@]}" "$final"
note "with a certificate each, this is where the CA's weekly limit starts counting"
