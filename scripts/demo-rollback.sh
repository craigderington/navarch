#!/usr/bin/env bash
# Rollback: deploy two distinct stack versions, then roll back to the first.
# A rollback creates a NEW revision reusing the earlier version's spec and runs
# it through the normal rollout (auto-promoting) — deployments stays append-only.
set -euo pipefail

API=${API:-http://localhost:8417}
API_TOKEN=${API_TOKEN:-dev-token-change-me}
CURL_AUTH=(-H "Authorization: Bearer $API_TOKEN")
SUFFIX=${SUFFIX:-$RANDOM}
HOST=${HOST:-rb-$SUFFIX.example.com}

step() { printf '\n\033[36m▸ %s\033[0m\n' "$1"; }
note() { printf '  \033[90m%s\033[0m\n' "$1"; }

api() {
  local m=$1 p=$2 b=${3-} out code
  if [ -n "$b" ]; then out=$(curl -sS "${CURL_AUTH[@]}" -X "$m" "$API$p" -H 'Content-Type: application/json' -d "$b" -w '\n%{http_code}')
  else out=$(curl -sS "${CURL_AUTH[@]}" -X "$m" "$API$p" -w '\n%{http_code}'); fi
  code=$(tail -n1 <<<"$out"); out=$(sed '$d' <<<"$out")
  [[ $code =~ ^2 ]] || { printf '\033[31m%s %s -> %s\033[0m\n%s\n' "$m" "$p" "$code" "$out" >&2; exit 1; }
  printf '%s' "$out"
}

wait_live() {
  local dep=$1 deadline=$((SECONDS + 150)) s
  while :; do
    s=$(api GET "/v1/deployments/$dep" | jq -r .state)
    [ "$s" = live ] && return 0
    [ "$s" = failed ] && { echo "rollout $dep failed" >&2; exit 1; }
    [ $SECONDS -lt $deadline ] || { echo "timed out ($s)" >&2; exit 1; }
    sleep 2
  done
}
sv_of() { api GET "/v1/deployments/$1" | jq -r .stack_version_id; }

ORG=$(api GET /v1/orgs | jq -r '.organizations[]|select(.slug=="dev")|.id')
APP=$(api POST "/v1/orgs/$ORG/apps" "{\"slug\":\"rb-$SUFFIX\",\"name\":\"RB\"}" | jq -r .id)
STACK=$(api POST "/v1/apps/$APP/stacks" "{\"slug\":\"s-$SUFFIX\"}" | jq -r .id)
ENV_ID=$(api POST "/v1/stacks/$STACK/envs" "{\"slug\":\"prod\",\"hostname\":\"$HOST\"}" | jq -r .id)

step "Set the secret examples/hello references — deploy now 422s without it"
api POST "/v1/envs/$ENV_ID/secrets" '{"key":"db_password","value":"devpassword"}' | jq -e .key >/dev/null

step "Version 1 → revision 1 (live)"
curl -sS "${CURL_AUTH[@]}" -X POST "$API/v1/stacks/$STACK/versions" --data-binary @examples/hello/compose.yaml | jq -e .id >/dev/null
D1=$(api POST "/v1/envs/$ENV_ID/deployments" '{}' | jq -r .id)
wait_live "$D1"; SV1=$(sv_of "$D1")
note "revision 1 live on stack version ${SV1:0:8}…"

step "Version 2 (LOG_LEVEL=debug) → revision 2 (live)"
sed 's/LOG_LEVEL: info/LOG_LEVEL: debug/' examples/hello/compose.yaml \
  | curl -sS "${CURL_AUTH[@]}" -X POST "$API/v1/stacks/$STACK/versions" --data-binary @- | jq -e .id >/dev/null
D2=$(api POST "/v1/envs/$ENV_ID/deployments" '{}' | jq -r .id)
wait_live "$D2"; SV2=$(sv_of "$D2")
note "revision 2 live on stack version ${SV2:0:8}…"
[ "$SV1" != "$SV2" ] || { echo "expected two distinct stack versions" >&2; exit 1; }

step "Rollback to revision 1"
D3=$(api POST "/v1/envs/$ENV_ID/rollback" '{"to_revision":1}' | jq -r .id)
note "rollback created revision $(api GET "/v1/deployments/$D3" | jq -r .revision)"
wait_live "$D3"; SV3=$(sv_of "$D3")

step "Verify"
if [ "$SV3" = "$SV1" ]; then
  note "revision 3 is live on version 1 (${SV3:0:8}…) — rolled back, append-only"
else
  echo "rollback should have re-deployed version 1 ($SV1), got $SV3" >&2; exit 1
fi

api GET "/v1/envs/$ENV_ID/deployments" | jq -r '.deployments[] | "  r\(.revision)  \(.slot)\t\(.state)\tsv=\(.stack_version_id[0:8])"'
printf '\n\033[32m✓ rollback re-deployed an earlier version as a new revision\033[0m\n'
