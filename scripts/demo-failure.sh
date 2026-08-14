#!/usr/bin/env bash
# Shows a failed rollout: a stack with a bogus image never comes up, so the
# agent reports the instance failed and the controller marks the deployment
# failed. Any prior live deployment is left untouched — the safety of
# blue/green is the absence of a promotion.
set -euo pipefail
API=${API:-http://localhost:8417}
API_TOKEN=${API_TOKEN:-dev-token-change-me}
CURL_AUTH=(-H "Authorization: Bearer $API_TOKEN")

ORG=$(curl -sS "${CURL_AUTH[@]}" "$API/v1/orgs" | jq -r '.organizations[]|select(.slug=="dev")|.id')
APP=$(curl -sS "${CURL_AUTH[@]}" -X POST "$API/v1/orgs/$ORG/apps" -d "{\"slug\":\"fail-$RANDOM\",\"name\":\"f\"}" | jq -r .id)
STACK=$(curl -sS "${CURL_AUTH[@]}" -X POST "$API/v1/apps/$APP/stacks" -d "{\"slug\":\"s-$RANDOM\"}" | jq -r .id)
printf 'services:\n  api:\n    image: ghcr.io/composectl/does-not-exist:0.0.0\n' \
  | curl -sS "${CURL_AUTH[@]}" -X POST "$API/v1/stacks/$STACK/versions" --data-binary @- >/dev/null
ENV=$(curl -sS "${CURL_AUTH[@]}" -X POST "$API/v1/stacks/$STACK/envs" -d '{"slug":"prod"}' | jq -r .id)
DEP=$(curl -sS "${CURL_AUTH[@]}" -X POST "$API/v1/envs/$ENV/deployments" -d '{}' | jq -r .id)

echo "deployment $DEP — waiting for failure…"
for _ in $(seq 1 40); do
  S=$(curl -sS "${CURL_AUTH[@]}" "$API/v1/deployments/$DEP" | jq -r .state)
  echo "  state=$S"
  [ "$S" = failed ] && { echo "✓ reached failed as expected; no prior deployment was disturbed"; exit 0; }
  sleep 3
done
echo "did not reach failed" >&2
exit 1
