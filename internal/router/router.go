// Package router generates Traefik file-provider dynamic config from the set
// of live ingress routes. It is the ONLY package that knows Traefik's config
// shape; it imports neither pgx nor the Docker SDK, taking plain Route values
// so the control plane stays Docker-free while still steering traffic.
package router

import (
	"fmt"
	"os"
	"path/filepath"
	"regexp"
	"sort"
	"strings"

	"gopkg.in/yaml.v3"
)

type Route struct {
	Key      string // stable per-environment id (env8)
	Hostname string
	// Target is the node address the ingress container is reachable at, and Port
	// is the host port published for it. This used to be the container's own
	// name, resolved by Docker DNS on a network the router had joined — which
	// only works while the router and the container share a daemon. Addressing
	// the node instead is the one form that works whether the tenant is local or
	// on another node, so there is no local-versus-remote branch to drift.
	Target string
	Port   int
}

type Router struct {
	dir string
	// certResolver names a Traefik ACME resolver. Empty means plain HTTP only,
	// which is the dev stack and any install with no public DNS.
	certResolver string
	// wildcardSuffix and wildcardResolver divert routes under one domain onto a
	// single wildcard certificate. Both empty is the ordinary case: every
	// hostname gets its own certificate.
	wildcardSuffix   string
	wildcardResolver string
}

type Option func(*Router)

// WithCertResolver makes every generated route serve HTTPS with a certificate
// obtained by the named Traefik ACME resolver.
//
// Traefik is the right place for tenant certificates rather than a proxy in
// front of it, for one reason that decides it: it already holds the router for
// every hostname the platform serves, so it issues certificates for exactly
// those and nothing else. A proxy in front would need to be told separately
// which hostnames are legitimate — an endpoint to ask, kept in step with the
// route list, and wrong in the dangerous direction whenever it drifts. Here the
// route list *is* the authorization.
//
// It also means a customer's own domain works: they point a record at this
// host, the control plane routes the hostname, and the certificate follows. No
// wildcard, and so no DNS-provider credential sitting in the ingress.
func WithCertResolver(name string) Option {
	return func(r *Router) { r.certResolver = name }
}

// WithWildcard makes routes one label below suffix serve a single wildcard
// certificate — `*.<suffix>` — obtained by the named resolver, instead of one
// certificate per hostname.
//
// This exists for preview environments and nothing else. A preview hostname is
// minted per run and never seen again, so with HTTP-01 a busy CI pipeline mints
// a certificate per run too, and Let's Encrypt counts issuance per *registered*
// domain per week. Previews are therefore the surface that reaches that ceiling
// first — and when it is reached, it is reached for the whole install, tenants
// included. One wildcard turns an unbounded count into one.
//
// The cost is a DNS-provider credential in the ingress, which is exactly what
// WithCertResolver's HTTP-01 challenge was chosen to avoid. That is why the
// wildcard is scoped to a suffix rather than applied to everything: the
// credential only ever needs to write under the domain the platform generates
// names in, so tenant hostnames and customers' own domains stay on HTTP-01,
// where no credential exists to be stolen. Narrowing the wildcard is the whole
// point; widening it to the apex would put every hostname behind the one
// credential.
//
// Matching is one label, deliberately. `*.preview.example.com` covers
// `pr-1-main-93fa144e.preview.example.com` and does NOT cover
// `a.b.preview.example.com` or the bare `preview.example.com` — that is what a
// DNS wildcard means, and claiming a route the certificate cannot cover would
// hand the browser a name mismatch. Generated preview hostnames are always one
// label; an operator is free to point an ordinary environment deeper, and that
// one keeps its own certificate.
func WithWildcard(suffix, resolver string) Option {
	return func(r *Router) {
		r.wildcardSuffix = strings.TrimPrefix(strings.Trim(suffix, "."), "*.")
		r.wildcardResolver = resolver
	}
}

// coveredByWildcard reports whether host is exactly one label below suffix, the
// only shape a `*.suffix` certificate is valid for.
func (r *Router) coveredByWildcard(host string) bool {
	if r.wildcardSuffix == "" || r.wildcardResolver == "" {
		return false
	}
	label, ok := strings.CutSuffix(host, "."+r.wildcardSuffix)
	return ok && label != "" && !strings.Contains(label, ".")
}

func New(dir string, opts ...Option) *Router {
	r := &Router{dir: dir}
	for _, o := range opts {
		o(r)
	}
	return r
}

// hostNamePattern matches a lowercase DNS name. Same alphabet as store
// hostname validation — Traefik Host() rules are interpolated from this.
// targetPattern accepts a DNS name or an IPv4/IPv6 literal — the forms a node's
// registered advertise_addr can take. It exists for the same reason
// hostNamePattern does: the value is interpolated into generated config, so its
// alphabet is constrained rather than trusted.
var targetPattern = regexp.MustCompile(`^[a-zA-Z0-9.:_\-\[\]]+$`)

var hostNamePattern = regexp.MustCompile(`^[a-z0-9]([a-z0-9-]{0,61}[a-z0-9])?(\.[a-z0-9]([a-z0-9-]{0,61}[a-z0-9])?)*$`)

type traefikHTTP struct {
	HTTP traefikRouting `yaml:"http"`
}

type traefikRouting struct {
	Routers  map[string]traefikRouter  `yaml:"routers"`
	Services map[string]traefikService `yaml:"services"`
}

type traefikRouter struct {
	Rule        string   `yaml:"rule"`
	EntryPoints []string `yaml:"entryPoints"`
	Service     string   `yaml:"service"`
	// omitempty matters: Traefik refuses an element with no children, so an
	// empty `tls: {}` fails the whole file the way `routers: {}` does. The
	// plaintext case must emit no key at all.
	TLS *traefikTLS `yaml:"tls,omitempty"`
}

type traefikTLS struct {
	CertResolver string `yaml:"certResolver"`
	// Domains overrides what Traefik asks the CA for. Without it a router is
	// issued a certificate for its own rule host; with it, exactly these — which
	// is the only way to ask for a wildcard, since no rule host is ever `*.x`.
	// omitempty because the ordinary route must emit no key at all.
	Domains []traefikDomain `yaml:"domains,omitempty"`
}

type traefikDomain struct {
	Main string `yaml:"main"`
}

type traefikService struct {
	LoadBalancer traefikLB `yaml:"loadBalancer"`
}

type traefikLB struct {
	Servers []traefikServer `yaml:"servers"`
}

type traefikServer struct {
	URL string `yaml:"url"`
}

// Sync writes the whole dynamic config from the current routes, atomically
// (temp file + rename) so Traefik's watcher never reads a half-written file.
// Regenerating the full file each call means a removed route simply disappears
// — but only if the file Traefik reads is one it accepts; see the empty case
// below, where writing the obvious thing withdrew nothing.
func (r *Router) Sync(routes []Route) error {
	if err := os.MkdirAll(r.dir, 0o700); err != nil {
		return err
	}
	sort.Slice(routes, func(i, j int) bool { return routes[i].Key < routes[j].Key })

	cfg := traefikHTTP{HTTP: traefikRouting{
		Routers:  map[string]traefikRouter{},
		Services: map[string]traefikService{},
	}}
	for _, rt := range routes {
		if !hostNamePattern.MatchString(rt.Hostname) {
			return fmt.Errorf("refusing to route unsafe hostname %q", rt.Hostname)
		}
		// The target is interpolated into a URL in the same generated file, so it
		// gets the same treatment as the hostname: an address is a DNS name or an
		// IP literal, and anything else does not belong in this file.
		if !targetPattern.MatchString(rt.Target) {
			return fmt.Errorf("refusing to route to unsafe target %q", rt.Target)
		}
		if rt.Port <= 0 || rt.Port > 65535 {
			return fmt.Errorf("refusing to route invalid port %d", rt.Port)
		}
		key := "r-" + rt.Key
		svc := "s-" + rt.Key
		router := traefikRouter{
			Rule:        fmt.Sprintf("Host(`%s`)", rt.Hostname),
			EntryPoints: []string{"web"},
			Service:     svc,
		}
		// A wildcard route names its certificate explicitly rather than relying on
		// Traefik noticing that an existing wildcard already covers the hostname.
		// The explicit form is what makes the count bounded no matter what order
		// routes appear in: every preview asks for the same one certificate, and
		// Traefik obtains it once.
		resolver, domains := r.certResolver, []traefikDomain(nil)
		if r.coveredByWildcard(rt.Hostname) {
			resolver = r.wildcardResolver
			domains = []traefikDomain{{Main: "*." + r.wildcardSuffix}}
		}
		if resolver != "" {
			// websecure only, and deliberately not "web" as well. A router with
			// `tls` set matches TLS connections *only*, so listing the plain
			// entrypoint achieves nothing — verified against traefik:v3.3, where
			// both forms behave identically. Sending HTTP somewhere useful is
			// the static config's job:
			//
			//   --entryPoints.web.http.redirections.entryPoint.to=websecure
			//
			// which redirects before routing and, checked on the same Traefik,
			// leaves /.well-known/acme-challenge/ alone — so HTTP-01 still
			// completes and the certificate arrives.
			router.EntryPoints = []string{"websecure"}
			router.TLS = &traefikTLS{CertResolver: resolver, Domains: domains}
		}
		cfg.HTTP.Routers[key] = router
		cfg.HTTP.Services[svc] = traefikService{
			LoadBalancer: traefikLB{Servers: []traefikServer{
				{URL: fmt.Sprintf("http://%s:%d", rt.Target, rt.Port)},
			}},
		}
	}

	out := []byte("# generated by composectl — do not edit\n")
	// No routes means no http section at all — not an empty one. Traefik's
	// parser refuses an element with no children: `routers: {}` fails the whole
	// file with "routers cannot be a standalone element", and `http: {}` (what
	// omitempty would produce) fails the same way. A rejected file is not read
	// as "no routes"; the file provider keeps the last config it accepted, so
	// the withdrawal is silently ignored and a torn-down environment's hostname
	// goes on routing. Verified against traefik:v3.3: with a live route in
	// place, writing `routers: {}` or `http: {}` leaves it serving 200, while
	// this comment-only file drops it to 404 and logs nothing.
	if len(routes) > 0 {
		body, err := yaml.Marshal(&cfg)
		if err != nil {
			return err
		}
		out = append(out, body...)
	}

	tmp := filepath.Join(r.dir, ".composectl.yml.tmp")
	if err := os.WriteFile(tmp, out, 0o600); err != nil {
		return err
	}
	return os.Rename(tmp, filepath.Join(r.dir, "composectl.yml"))
}
