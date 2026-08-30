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
		if r.certResolver != "" {
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
			router.TLS = &traefikTLS{CertResolver: r.certResolver}
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
