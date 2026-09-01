// Command gen writes the demo's Traefik config using the control plane's own
// router package.
//
// It exists so `make demo-wildcard` proves something about *us*. A demo that
// hand-writes the YAML would put a real Traefik in front of a real ACME server
// and establish only that Traefik can obtain a wildcard — which was never in
// doubt. What is in doubt is whether internal/router emits config that gets
// one, so the bytes under test have to come from internal/router.
package main

import (
	"fmt"
	"os"
	"strconv"

	"github.com/craigderington/navarch/internal/router"
)

func main() {
	if len(os.Args) < 5 {
		fmt.Fprintln(os.Stderr, "usage: gen <dir> <previewDomain> <target> <host>...")
		os.Exit(2)
	}
	dir, previewDomain, target := os.Args[1], os.Args[2], os.Args[3]

	// The same two options cmd/controlplane passes when NAVARCH_DNS_PROVIDER is
	// set: per-hostname HTTP-01 for everything, and the wildcard for previews.
	r := router.New(dir,
		router.WithCertResolver("le"),
		router.WithWildcard(previewDomain, "lewild"),
	)

	var routes []router.Route
	for i, host := range os.Args[4:] {
		routes = append(routes, router.Route{
			Key:      "env" + strconv.Itoa(i),
			Hostname: host,
			Target:   target,
			Port:     80,
		})
	}
	if err := r.Sync(routes); err != nil {
		fmt.Fprintln(os.Stderr, "sync:", err)
		os.Exit(1)
	}
}
