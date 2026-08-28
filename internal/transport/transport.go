// Package transport decides whether a control-plane base URL is somewhere
// credentials may be sent in the clear.
//
// It is a leaf with no dependencies on purpose. Two binaries need the same
// answer and neither should have to import the other to get it: the CLI (and
// through it the TUI) reaches the control plane with an operator token, and the
// agent reaches it with a node token and pulls age ciphertext back. Nothing here
// knows the wire format, so this is not a second implementation of the protocol.
package transport

import (
	"fmt"
	"net"
	"net/url"
	"strings"
)

// InsecureError says the URL is plaintext HTTP to somewhere the traffic could
// leave the machine or its container network. It is a distinct type because an
// explicit opt-in may override exactly this and nothing else — a malformed URL
// or an unusable scheme stays an error no environment variable can rescue.
type InsecureError struct {
	URL  string
	Host string
}

func (e *InsecureError) Error() string {
	return fmt.Sprintf("refusing to send credentials in the clear to %s: %s is not loopback "+
		"or a container network, so the bearer token and any age ciphertext would cross it unencrypted. "+
		"Use https:// (terminate TLS at a reverse proxy in front of the control plane), "+
		"or set the insecure opt-in if this network really is trusted", e.URL, e.Host)
}

// CheckBaseURL reports whether raw is safe to use as a control-plane base URL.
//
// https is always fine. Plaintext http is fine only where the request cannot
// reach a network anyone else is on:
//
//   - loopback, by address or by name — nothing leaves the host.
//   - a `.localhost` name, which RFC 6761 reserves as loopback.
//   - a single-label name such as `controlplane`. Docker's embedded DNS
//     resolves service names only within the network the container is attached
//     to, which is how the dev stack's agents reach the control plane. It is
//     not a proof — a search domain could in principle make a bare name public
//     — but a name with no dots is not something you type at a host on the
//     internet.
//   - a `.internal` name, the usual convention for private service discovery.
//
// Everything else over http is refused, and private ranges are refused
// deliberately: `http://10.0.1.7:8417` is not a safer case than a public
// address, it is the exact case the audit called out — a shared network where a
// captured node token reads that node's ciphertext for as long as it is valid.
func CheckBaseURL(raw string) error {
	u, err := url.Parse(raw)
	if err != nil {
		return fmt.Errorf("invalid control plane URL %q: %w", raw, err)
	}
	switch u.Scheme {
	case "https":
		return nil
	case "http":
	case "":
		return fmt.Errorf("control plane URL %q needs a scheme, e.g. https://host:8417", raw)
	default:
		return fmt.Errorf("control plane URL %q has unsupported scheme %q; use https or http", raw, u.Scheme)
	}
	host := u.Hostname()
	if host == "" {
		return fmt.Errorf("control plane URL %q has no host", raw)
	}
	if plaintextIsContained(host) {
		return nil
	}
	return &InsecureError{URL: raw, Host: host}
}

// plaintextIsContained reports whether traffic to host cannot reach a network
// someone else is on.
func plaintextIsContained(host string) bool {
	h := strings.ToLower(strings.TrimSuffix(host, "."))
	if ip := net.ParseIP(h); ip != nil {
		// Only loopback. A private address is not contained: it is a LAN, and a
		// LAN is precisely where a captured token is worth having.
		return ip.IsLoopback()
	}
	if h == "localhost" || strings.HasSuffix(h, ".localhost") {
		return true
	}
	if strings.HasSuffix(h, ".internal") {
		return true
	}
	// A single-label name resolves through a container network or a local
	// search domain, never through public DNS.
	return !strings.Contains(h, ".")
}

// Insecure reports whether an opt-in value means "yes". Written once here so
// the CLI and the agent cannot drift on what counts as set — a guard that is
// on for one binary and off for the other is worse than no guard, because it
// looks like it is protecting both.
func Insecure(v string) bool {
	switch strings.ToLower(strings.TrimSpace(v)) {
	case "1", "true", "yes", "on":
		return true
	}
	return false
}
