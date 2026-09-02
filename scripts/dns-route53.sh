#!/usr/bin/env bash
# The four records Navarch needs, as a Route 53 change batch.
#
# A file rather than console clicks, because DNS is the one part of this deploy
# with no undo worth the name: a wrong record propagates, gets cached by
# resolvers you do not control, and takes its TTL to come back. A change batch
# is reviewable before it is applied and identical the second time.
#
# It prints the batch and does nothing else unless you pass --apply. Read it
# first; that is the entire point of it being a file.
#
#   ./scripts/dns-route53.sh 203.0.113.10            # show what would change
#   ./scripts/dns-route53.sh 203.0.113.10 --apply    # submit it
#
# The zone is found by name from NAVARCH_DOMAIN; pass --zone-id to override.
set -euo pipefail
cd "$(dirname "$0")/.."

ENV_FILE=${ENV_FILE:-deploy/production/.env}
[ -f "$ENV_FILE" ] || { echo "no $ENV_FILE" >&2; exit 2; }
# shellcheck disable=SC1090
set -a; . "$ENV_FILE"; set +a

DOMAIN=${NAVARCH_DOMAIN:?NAVARCH_DOMAIN must be set in $ENV_FILE}
PREVIEW=${NAVARCH_PREVIEW_DOMAIN:-preview.$DOMAIN}
# Five minutes. Short on purpose for a first deploy: the value of a low TTL is
# that a mistake costs five minutes instead of a day, and there is nothing to
# gain from caching a record that has never been right yet. Raise it once the
# addresses have stopped moving.
TTL=${TTL:-300}

IP=${1:-}
case "$IP" in
	*.*.*.*) ;;
	*) echo "usage: $0 <static-ip> [--apply] [--zone-id ZONE]" >&2; exit 2 ;;
esac
shift

APPLY=0
ZONE_ID=${ZONE_ID:-}
while [ $# -gt 0 ]; do
	case "$1" in
		--apply) APPLY=1 ;;
		--zone-id) ZONE_ID=$2; shift ;;
		*) echo "unknown argument $1" >&2; exit 2 ;;
	esac
	shift
done

# UPSERT, never CREATE. The apex almost certainly already holds the registrar's
# parking record, and CREATE would fail on it while succeeding on the three that
# do not exist — a half-applied batch is the worst outcome available here.
records=(
	"$DOMAIN"           # the apex: left for a stack you deploy, usually the site
	"api.$DOMAIN"       # the API: agents, the CLI, CI
	"console.$DOMAIN"   # the console: what a person opens
	"*.$PREVIEW"        # every preview environment, on one wildcard
)

batch=$(mktemp); trap 'rm -f "$batch"' EXIT
{
	printf '{\n  "Comment": "navarch %s",\n  "Changes": [\n' "${NAVARCH_VERSION:-}"
	for i in "${!records[@]}"; do
		[ "$i" -gt 0 ] && printf ',\n'
		printf '    {"Action":"UPSERT","ResourceRecordSet":{"Name":"%s","Type":"A","TTL":%s,"ResourceRecords":[{"Value":"%s"}]}}' \
			"${records[$i]}" "$TTL" "$IP"
	done
	printf '\n  ]\n}\n'
} > "$batch"

printf '\n\033[36m▸ %d records, all A → %s, TTL %s\033[0m\n' "${#records[@]}" "$IP" "$TTL"
for r in "${records[@]}"; do printf '  %s\n' "$r"; done

printf '\n\033[36m▸ Change batch\033[0m\n'
if command -v jq >/dev/null 2>&1; then jq . "$batch"; else cat "$batch"; fi

# Deliberately NOT here: Mailgun's MX, SPF and DKIM records for the sending
# subdomain. Their DKIM value is generated per domain and exists only in the
# Mailgun dashboard, so a script that pretended to know it would write a record
# that looks right and silently fails every signature.
printf '\n\033[33m!\033[0m Mailgun'"'"'s MX/TXT/DKIM records for %s are not in this batch —\n' "${COMPOSECTL_MAILGUN_DOMAIN:-mg.$DOMAIN}"
printf '  their DKIM value only exists in your Mailgun dashboard. Add those separately.\n'

if [ "$APPLY" -eq 0 ]; then
	printf '\n\033[33mNothing applied.\033[0m Re-run with --apply once the above reads correctly.\n'
	exit 0
fi

command -v aws >/dev/null 2>&1 || { echo "aws CLI not found" >&2; exit 1; }
if [ -z "$ZONE_ID" ]; then
	ZONE_ID=$(aws route53 list-hosted-zones-by-name --dns-name "$DOMAIN." \
		--query "HostedZones[?Name=='$DOMAIN.'].Id | [0]" --output text 2>/dev/null | sed 's|/hostedzone/||')
	[ -n "$ZONE_ID" ] && [ "$ZONE_ID" != "None" ] || {
		echo "no Route 53 hosted zone for $DOMAIN — create it, and point the registrar at its nameservers" >&2
		exit 1
	}
fi
printf '\n\033[36m▸ Applying to zone %s\033[0m\n' "$ZONE_ID"
change=$(aws route53 change-resource-record-sets --hosted-zone-id "$ZONE_ID" \
	--change-batch "file://$batch" --query 'ChangeInfo.Id' --output text)
printf '  submitted %s\n' "$change"

# Route 53 reports INSYNC when every one of its nameservers has the change. That
# is not the same as your resolver having forgotten the old answer, which is why
# preflight says out loud that a failure there can be a cache.
printf '  waiting for INSYNC (this is Route 53 agreeing with itself, not your resolver)\n'
aws route53 wait resource-record-sets-changed --id "$change" && printf '  \033[32m✓\033[0m propagated\n'
printf '\nNow: \033[1m./scripts/preflight.sh %s\033[0m\n' "$IP"
