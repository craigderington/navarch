// Package config loads control plane configuration from the environment.
package config

import (
	"fmt"
	"os"
	"strconv"
	"strings"
	"time"

	"github.com/craigderington/navarch/internal/mail"
)

// DefaultListenAddr is the API bind address. Deliberately a high port —
// 3000/5000/8000/9000 are already crowded on a dev box.
const DefaultListenAddr = "0.0.0.0:8417"

// ListenAddr resolves the bind address without requiring the rest of the
// config. The -healthcheck probe runs in a container that has a listen
// address but no reason to hold a database URL, so it reads this alone.
func ListenAddr() string {
	return envOr("COMPOSECTL_LISTEN_ADDR", DefaultListenAddr)
}

type Config struct {
	DatabaseURL string
	ListenAddr  string
	// AgentToken is the shared bearer token. It no longer protects the whole
	// API: operator routes need an operator token and agent endpoints need
	// that node's own token, so this now opens exactly two machine-to-machine
	// paths — POST /v1/nodes/register and GET /metrics.
	//
	// It stays required because a node has no identity of its own until it has
	// registered, and a control plane that cannot enrol nodes is not a control
	// plane. See internal/api/identity.go for the full reasoning.
	AgentToken string
	// TickInterval paces the scheduler and rollout controller loops.
	TickInterval time.Duration
	// RouterDir is where Traefik dynamic config is written. Empty disables
	// routing (the controller runs without a router), which keeps the control
	// plane usable without Traefik in the stack.
	RouterDir string
	// PreviewDomain is the wildcard domain preview hostnames are generated under
	// (pr-142-hello.<domain>). The default resolves on a dev box without DNS
	// because Traefik routes on the Host header alone.
	PreviewDomain string
	// RouteStrandSeconds is how long a node may go unheard from before its
	// environments' routes are withdrawn. Deliberately separate from the 30s
	// staleness window that stops the scheduler placing new work: that decision
	// is cheap and reversible, while cutting live traffic is neither, and a
	// threshold chosen for one should not silently govern the other.
	//
	// The trade is real in both directions. With whole-stack placement there is
	// exactly one copy of an environment, so withdrawing the route of a node
	// that is unreachable-but-serving turns a working service into a 404. Keeping
	// it means every request to a genuinely dead node hangs until it times out —
	// and a timeout is the least diagnosable failure there is. Withdrawal wins
	// because a fast 404 can be explained and a hang cannot, but it waits four
	// times as long as the scheduler does before deciding.
	//
	// Zero means never withdraw, which is a legitimate operator preference: some
	// would rather have a hang than a 404. It is not the default because the
	// default should be the one that is easier to diagnose.
	RouteStrandSeconds int
	// BootstrapOperatorEmail creates the first operator at startup, so a fresh
	// install has somebody who can log in. Empty disables it, which is correct
	// once the install has operators of its own.
	//
	// From the environment rather than a seeded migration for the reason
	// POST /v1/orgs is self-serve: a UUID or token baked into a migration is
	// permanent and identical on every install.
	BootstrapOperatorEmail string
	// BootstrapOperatorToken pins that operator's token instead of generating
	// one. This is a dev-stack affordance — compose, the Makefile and the demo
	// scripts share a constant rather than scraping a generated value out of a
	// log line on every `make up`. Leave it empty anywhere real and the token
	// is minted from crypto/rand and logged once.
	BootstrapOperatorToken string
	// RequireJoinToken refuses node registration on the shared service token,
	// so a node's organization always comes from a credential that names one.
	// Off by default: an existing install's agents still present the shared
	// token, and an upgrade that stops the fleet re-registering is not an
	// upgrade. Anything serving more than one tenant sets it.
	RequireJoinToken bool
	// BootstrapJoinToken pins a join token for the bootstrap org, the way
	// BootstrapOperatorToken pins the first operator's. Dev-stack affordance,
	// same reasoning: compose and the demos share a constant rather than
	// scraping a generated one out of a log.
	BootstrapJoinToken string
	// RouterCertResolver names a Traefik ACME resolver. Set it and every tenant
	// route is served over HTTPS with a certificate Traefik obtains for that
	// hostname. Empty means plain HTTP, which is the dev stack and any install
	// whose hostnames do not resolve publicly — ACME cannot succeed there, and
	// a failing certificate request on every route is worse than no TLS.
	RouterCertResolver string
	// RouterWildcardResolver names a second Traefik ACME resolver, one using a
	// DNS-01 challenge, and switches preview routes onto a single
	// `*.<PreviewDomain>` certificate obtained by it.
	//
	// Scoped to previews on purpose, and it is the preview domain rather than a
	// domain of its own so the two cannot drift: a wildcard covering names the
	// platform does not generate would be a credential held for no reason.
	// Tenant hostnames and customers' own domains stay on RouterCertResolver's
	// HTTP-01, which needs no credential at all.
	//
	// Empty is the default and the right answer for most installs: it costs a
	// DNS-provider credential in the ingress, and that is only worth paying when
	// preview churn is high enough to approach the CA's per-domain issuance
	// limit. Nothing validates that the resolver exists — a name with no
	// matching --certificatesresolvers flag leaves previews on Traefik's
	// self-signed fallback, which is why deploy/README.md says to open one.
	RouterWildcardResolver string
	// Mail configures transactional email. Unconfigured is the default and a
	// supported mode, not a degraded one: every path that would send checks
	// first, and the two notification paths carry on without it. See
	// internal/mail for why only invites treat a send failure as fatal.
	Mail mail.Config
	// ConsoleURL is where an invited operator is sent to redeem an invitation.
	// It lives here rather than being derived from the request, because an
	// invite link built from a Host header is a link an attacker can aim.
	ConsoleURL string
}

func Load() (*Config, error) {
	c := &Config{
		DatabaseURL:        os.Getenv("COMPOSECTL_DATABASE_URL"),
		ListenAddr:         ListenAddr(),
		AgentToken:         os.Getenv("COMPOSECTL_AGENT_TOKEN"),
		TickInterval:       time.Duration(intEnv("COMPOSECTL_TICK_SECONDS", 1)) * time.Second,
		RouterDir:          os.Getenv("COMPOSECTL_ROUTER_DIR"),
		PreviewDomain:      envOr("COMPOSECTL_PREVIEW_DOMAIN", "preview.localhost"),
		RouteStrandSeconds: intEnv("COMPOSECTL_ROUTE_STRAND_SECONDS", 120),

		BootstrapOperatorEmail: os.Getenv("COMPOSECTL_BOOTSTRAP_OPERATOR_EMAIL"),
		BootstrapOperatorToken: os.Getenv("COMPOSECTL_BOOTSTRAP_OPERATOR_TOKEN"),
		RequireJoinToken:       os.Getenv("COMPOSECTL_REQUIRE_JOIN_TOKEN") == "1",
		BootstrapJoinToken:     os.Getenv("COMPOSECTL_BOOTSTRAP_JOIN_TOKEN"),
		RouterCertResolver:     os.Getenv("COMPOSECTL_ROUTER_CERT_RESOLVER"),
		RouterWildcardResolver: os.Getenv("COMPOSECTL_ROUTER_WILDCARD_RESOLVER"),
		ConsoleURL:             strings.TrimSuffix(os.Getenv("COMPOSECTL_CONSOLE_URL"), "/"),
		Mail: mail.Config{
			Domain:  os.Getenv("COMPOSECTL_MAILGUN_DOMAIN"),
			APIKey:  os.Getenv("COMPOSECTL_MAILGUN_API_KEY"),
			From:    os.Getenv("COMPOSECTL_MAIL_FROM"),
			BaseURL: os.Getenv("COMPOSECTL_MAILGUN_BASE_URL"),
		},
	}
	if c.DatabaseURL == "" {
		return nil, fmt.Errorf("COMPOSECTL_DATABASE_URL is required")
	}
	if c.AgentToken == "" {
		return nil, fmt.Errorf("COMPOSECTL_AGENT_TOKEN is required")
	}
	return c, nil
}

func envOr(key, def string) string {
	if v := os.Getenv(key); v != "" {
		return v
	}
	return def
}

func intEnv(key string, def int) int {
	if v := os.Getenv(key); v != "" {
		if n, err := strconv.Atoi(v); err == nil {
			return n
		}
	}
	return def
}
