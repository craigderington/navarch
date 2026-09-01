// Package mail sends transactional email through Mailgun's HTTP API.
//
// It is a leaf: no pgx, no Docker SDK, no knowledge of the platform's types.
// Callers hand it a Message and get an error back, which is what lets the three
// consumers — invites, failed rollouts, expiring previews — each decide for
// themselves what a send failure means. For two of them it means nothing at
// all: a rollout that failed must be recorded as failed whether or not anyone
// could be told, and a preview must expire on time whether or not the warning
// arrived. Only the invite treats a send failure as the operation failing,
// because an invite nobody receives is not an invite.
//
// No Mailgun SDK. The API is a form POST with basic auth, which is the forty
// lines below; the SDK is a dependency with its own release cadence for the
// same result. This also keeps the provider swappable at one seam — Sender is
// the only thing that knows Mailgun exists.
package mail

import (
	"context"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"strings"
	"time"
)

// DefaultBaseURL is Mailgun's US region. EU-hosted domains use
// https://api.eu.mailgun.net and reject requests to this one, so it is
// configurable rather than constant — the failure otherwise is a 401 that looks
// like a bad key.
const DefaultBaseURL = "https://api.mailgun.net"

// maxBodyBytes truncates a message body.
//
// This is not politeness. A failed rollout's reason comes from the agent, which
// read it from a container the platform does not control, so the one piece of
// attacker-influenced text in any of these messages is the one most likely to
// be enormous. Truncating is cheaper than discovering the limit at Mailgun.
const maxBodyBytes = 16 << 10

type Config struct {
	// Domain is the Mailgun sending domain — a subdomain like mg.example.com,
	// which sends and does not receive. Nothing here should ever be used as a
	// reply-to or a contact address for that reason.
	Domain string
	APIKey string
	// From is the envelope sender, e.g. "Navarch <navarch@mg.example.com>".
	From string
	// BaseURL selects the Mailgun region. Empty means DefaultBaseURL.
	BaseURL string
}

// Configured reports whether enough is set to send. Everything downstream is
// optional-by-design: an install with no mail configuration runs exactly as it
// did before, and the loops that would have sent simply do not.
func (c Config) Configured() bool {
	return c.Domain != "" && c.APIKey != "" && c.From != ""
}

type Sender struct {
	cfg    Config
	client *http.Client
}

func New(cfg Config) *Sender {
	if cfg.BaseURL == "" {
		cfg.BaseURL = DefaultBaseURL
	}
	// Mailgun documents its API base *as* https://api.mailgun.net/v3, so
	// copying it verbatim into this setting is the natural mistake — and it
	// would produce /v3/v3/<domain>/messages and a 404 that says nothing about
	// why. Accept both forms rather than making an operator find that.
	cfg.BaseURL = strings.TrimSuffix(strings.TrimSuffix(strings.TrimSuffix(cfg.BaseURL, "/"), "/v3"), "/")
	return &Sender{
		cfg: cfg,
		// A mail provider is a third party on the far side of the internet, and
		// this runs inside control-plane loops that must keep ticking. Every
		// context gets a timeout; this one gets a ceiling too.
		client: &http.Client{Timeout: 20 * time.Second},
	}
}

type Message struct {
	To      []string
	Subject string
	// Body is plain text, deliberately. The only variable content in any of
	// these messages is a hostname, a URL, and an error string that came from a
	// tenant's container — and an HTML body would make that last one an
	// injection surface for the price of a prettier email nobody asked for.
	Body string
}

// Send posts one message. It returns an error for any non-2xx, including the
// body, because Mailgun's failures are descriptive and a caller logging "mail
// failed" with nothing else has learned nothing.
func (s *Sender) Send(ctx context.Context, m Message) error {
	if !s.cfg.Configured() {
		return fmt.Errorf("mail is not configured")
	}
	if len(m.To) == 0 {
		return fmt.Errorf("mail: no recipients")
	}

	form := url.Values{}
	form.Set("from", s.cfg.From)
	for _, to := range m.To {
		form.Add("to", sanitizeHeader(to))
	}
	form.Set("subject", sanitizeHeader(m.Subject))
	form.Set("text", truncate(m.Body, maxBodyBytes))

	endpoint := strings.TrimSuffix(s.cfg.BaseURL, "/") + "/v3/" + s.cfg.Domain + "/messages"
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, endpoint, strings.NewReader(form.Encode()))
	if err != nil {
		return err
	}
	req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	req.SetBasicAuth("api", s.cfg.APIKey)

	resp, err := s.client.Do(req)
	if err != nil {
		return fmt.Errorf("mailgun: %w", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode < 200 || resp.StatusCode > 299 {
		detail, _ := io.ReadAll(io.LimitReader(resp.Body, 512))
		return fmt.Errorf("mailgun: %s: %s", resp.Status, strings.TrimSpace(string(detail)))
	}
	return nil
}

// sanitizeHeader strips CR and LF from a value that becomes a mail header.
//
// The HTTP API means these are form fields rather than headers we write, so
// this is not the classic SMTP header-injection hole. It is here because the
// subject of a failure notice names a service, and a service name reaches us
// from a tenant's compose file — the day a value like that is passed to
// something that does write headers, this is what makes that change safe rather
// than a vulnerability.
func sanitizeHeader(v string) string {
	return strings.NewReplacer("\r", " ", "\n", " ").Replace(v)
}

func truncate(s string, max int) string {
	if len(s) <= max {
		return s
	}
	// Say so. A silently short message is indistinguishable from a short one,
	// which is the same reasoning the log tail uses when it caps bytes.
	return s[:max] + "\n\n[truncated]"
}
