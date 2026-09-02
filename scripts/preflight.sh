#!/usr/bin/env bash
# Everything that must be true BEFORE the first `up -d`, checked rather than hoped.
#
# The expensive mistake this exists to prevent is starting the stack before DNS
# resolves. ACME verifies by connecting to the name, and Let's Encrypt backs off
# on repeated failure — so a premature start does not merely fail, it puts the
# domain in a penalty box and the second attempt fails too, for reasons that
# look nothing like the first. Everything else here is cheap by comparison.
#
# Run it from anywhere: a laptop before provisioning, or the host itself. It
# reads deploy/production/.env for what to check and takes the target IP as its
# only argument.
#
#   ./scripts/preflight.sh 203.0.113.10
set -uo pipefail
cd "$(dirname "$0")/.."

ENV_FILE=${ENV_FILE:-deploy/production/.env}
MAIL_FILE=${MAIL_FILE:-deploy/production/mail.env}
DNS_FILE=${DNS_FILE:-deploy/production/dns.env}

step() { printf '\n\033[36m▸ %s\033[0m\n' "$1"; }
ok()   { printf '  \033[32m✓\033[0m %s\n' "$1"; }
bad()  { printf '  \033[31m✗\033[0m %s\n' "$1"; FAILED=$((FAILED + 1)); }
warn() { printf '  \033[33m!\033[0m %s\n' "$1"; WARNED=$((WARNED + 1)); }
FAILED=0
WARNED=0

IP=${1:-}
[ -f "$ENV_FILE" ] || { echo "no $ENV_FILE — copy deploy/production/env.example to it first" >&2; exit 2; }
# shellcheck disable=SC1090
set -a; . "$ENV_FILE"; set +a

DOMAIN=${NAVARCH_DOMAIN:-}
PREVIEW=${NAVARCH_PREVIEW_DOMAIN:-}
VERSION=${NAVARCH_VERSION:-}

# resolve prints every A record for a name, or nothing. getent rather than dig
# because dig is not installed on a stock Lightsail image and this must run
# before anything has been installed.
resolve() { getent ahostsv4 "$1" 2>/dev/null | awk '{print $1}' | sort -u; }

# mx_of prints a domain's MX records, empty if it has none, or the literal
# "unknown" when this machine has no tool that can ask. Those three are
# genuinely different answers and collapsing the third into the second is how a
# check reports a fault that is really a missing dependency.
mx_of() {
	if command -v dig >/dev/null 2>&1; then dig +short MX "$1" 2>/dev/null
	elif command -v host >/dev/null 2>&1; then host -t MX "$1" 2>/dev/null | grep -i "mail is handled" || true
	elif command -v nslookup >/dev/null 2>&1; then nslookup -type=MX "$1" 2>/dev/null | grep -i "mail exchanger" || true
	else echo unknown
	fi
}

step "Configuration"
[ -n "$DOMAIN" ]  && ok "domain $DOMAIN"            || bad "NAVARCH_DOMAIN is unset"
[ -n "$VERSION" ] && ok "version $VERSION"          || bad "NAVARCH_VERSION is unset"
[ -n "$PREVIEW" ] && ok "preview domain $PREVIEW"   || warn "NAVARCH_PREVIEW_DOMAIN is unset; previews will use the default"

# A placeholder that reaches production is a credential everyone who has read
# the repository already knows.
for var in POSTGRES_PASSWORD NAVARCH_SERVICE_TOKEN; do
	val=${!var:-}
	case "$val" in
		""|*change-me*) bad "$var is still a placeholder — generate one: openssl rand -hex 32" ;;
		*) [ ${#val} -ge 24 ] && ok "$var set (${#val} chars)" || warn "$var is only ${#val} characters" ;;
	esac
done
case "${NAVARCH_ACME_EMAIL:-}" in
	""|*example.com*) bad "NAVARCH_ACME_EMAIL must be a mailbox that RECEIVES — expiry warnings go there" ;;
	*) ok "acme contact ${NAVARCH_ACME_EMAIL}" ;;
esac

step "DNS"
if [ -z "$IP" ]; then
	warn "no target IP given; skipping (usage: $0 <static-ip>)"
else
	ok "expecting $IP"
	# The platform claims only subdomains; the apex is left for a stack. All
	# four still have to resolve here, because the apex is where the site goes
	# and its certificate is issued the same way.
	for name in "console.$DOMAIN" "api.$DOMAIN" "$DOMAIN"; do
		got=$(resolve "$name")
		case "$got" in
			"")    bad "$name does not resolve" ;;
			*"$IP"*) ok "$name → $IP" ;;
			*)     bad "$name → $(echo "$got" | tr '\n' ' ')(want $IP)" ;;
		esac
	done
	# A wildcard cannot be asked for directly, so probe a name under it that
	# nothing would ever create by hand.
	if [ -n "$PREVIEW" ]; then
		probe="preflight-$$.$PREVIEW"
		got=$(resolve "$probe")
		case "$got" in
			"")      bad "*.$PREVIEW does not resolve (probed $probe)" ;;
			*"$IP"*) ok "*.$PREVIEW → $IP" ;;
			*)       bad "*.$PREVIEW → $(echo "$got" | tr '\n' ' ')(want $IP)" ;;
		esac
	fi
	echo "     a failure here can be your resolver's cache, not the zone — records"
	echo "     you just added may need their TTL to pass before this agrees"
fi

step "Images"
if command -v docker >/dev/null 2>&1 && [ -n "$VERSION" ]; then
	for img in "controlplane:$VERSION" "agent:$VERSION" "web:$VERSION"; do
		if docker manifest inspect "ghcr.io/craigderington/navarch/$img" >/dev/null 2>&1; then
			ok "$img pullable"
		else
			bad "$img is NOT pullable — is the release for $VERSION green?"
		fi
	done
else
	warn "docker not available here; image check skipped"
fi

step "Mail"
if [ -f "$MAIL_FILE" ]; then
	# shellcheck disable=SC1090
	mg_domain=$(grep -E '^COMPOSECTL_MAILGUN_DOMAIN=' "$MAIL_FILE" | cut -d= -f2-)
	mg_key=$(grep -E '^COMPOSECTL_MAILGUN_API_KEY=' "$MAIL_FILE" | cut -d= -f2-)
	case "$mg_key" in
		""|*change-me*) bad "mail.env has no API key; invitations will return a link instead" ;;
		*) ok "mailgun key present" ;;
	esac
	if [ -n "$mg_domain" ]; then
		# MX, never A. A Mailgun sending domain correctly has NO address record,
		# so an A lookup fails on a perfectly configured one — this check said
		# "no DNS yet" about a domain whose MX, SPF and DKIM were all in place.
		# A preflight that cries wolf is one whose next warning gets ignored.
		case "$(mx_of "$mg_domain")" in
			unknown) warn "cannot check MX for $mg_domain from here (no dig/host/nslookup)" ;;
			"")      bad "$mg_domain has no MX — Mailgun will not send until its records exist" ;;
			*)       ok "$mg_domain has MX records" ;;
		esac
	fi
else
	warn "no $MAIL_FILE — mail is off; invitations still return a link to paste"
fi

step "Wildcard certificate"
if [ -n "${NAVARCH_DNS_PROVIDER:-}" ]; then
	if [ -f "$DNS_FILE" ]; then
		ok "DNS-01 provider ${NAVARCH_DNS_PROVIDER} with a credential file"
	else
		bad "NAVARCH_DNS_PROVIDER is set but $DNS_FILE is missing — previews will get no certificate"
	fi
else
	warn "no DNS provider; every preview takes its own certificate (fine until CI churn is high)"
fi

printf '\n'
if [ "$FAILED" -gt 0 ]; then
	printf '\033[31m%d check(s) failed.\033[0m Fix them before `up -d`: a premature start\n' "$FAILED"
	printf 'spends failed ACME validations, and Let'"'"'s Encrypt remembers them.\n'
	exit 1
fi
printf '\033[32mReady.\033[0m'
[ "$WARNED" -gt 0 ] && printf ' %d warning(s) above are choices, not faults.' "$WARNED"
printf '\n'
