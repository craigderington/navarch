package mail

import (
	"context"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"testing"
)

func testSender(t *testing.T, h http.HandlerFunc) *Sender {
	t.Helper()
	srv := httptest.NewServer(h)
	t.Cleanup(srv.Close)
	return New(Config{Domain: "mg.navar.ch", APIKey: "key-test", From: "Navarch <navarch@mg.navar.ch>", BaseURL: srv.URL})
}

func TestSendPostsTheMailgunForm(t *testing.T) {
	var gotPath, gotUser, gotPass string
	var form url.Values
	s := testSender(t, func(w http.ResponseWriter, r *http.Request) {
		gotPath = r.URL.Path
		gotUser, gotPass, _ = r.BasicAuth()
		_ = r.ParseForm()
		form = r.PostForm
		w.WriteHeader(http.StatusOK)
	})
	if err := s.Send(context.Background(), Message{
		To: []string{"ada@example.com"}, Subject: "Rollout failed", Body: "the reason",
	}); err != nil {
		t.Fatalf("Send: %v", err)
	}
	if gotPath != "/v3/mg.navar.ch/messages" {
		t.Fatalf("path %q", gotPath)
	}
	if gotUser != "api" || gotPass != "key-test" {
		t.Fatalf("auth %q/%q", gotUser, gotPass)
	}
	if form.Get("to") != "ada@example.com" || form.Get("subject") != "Rollout failed" {
		t.Fatalf("form %v", form)
	}
	// text, never html: the body carries container output.
	if form.Get("text") != "the reason" || form.Has("html") {
		t.Fatalf("body must be plain text only: %v", form)
	}
}

// Mailgun says why it refused, and a caller that logs "mail failed" with
// nothing else has learned nothing — the difference between a bad key and an
// unverified domain is the whole diagnosis.
func TestSendReportsTheProviderReason(t *testing.T) {
	s := testSender(t, func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusUnauthorized)
		_, _ = w.Write([]byte(`{"message":"Invalid private key"}`))
	})
	err := s.Send(context.Background(), Message{To: []string{"a@b.com"}, Subject: "x", Body: "y"})
	if err == nil || !strings.Contains(err.Error(), "Invalid private key") {
		t.Fatalf("want the provider's reason, got %v", err)
	}
}

// An unconfigured install must not send, and must not panic trying. Every
// consumer is expected to check Configured() first; this is the backstop.
func TestUnconfiguredSenderRefuses(t *testing.T) {
	if (Config{}).Configured() {
		t.Fatal("an empty config must not read as configured")
	}
	if err := New(Config{}).Send(context.Background(), Message{To: []string{"a@b.com"}}); err == nil {
		t.Fatal("expected an error from an unconfigured sender")
	}
}

// A failure reason reaches us from a tenant's container. It has no length limit
// and no guarantee of being one line, and both of those end up in a message.
func TestUntrustedContentIsBoundedAndCannotForgeAHeader(t *testing.T) {
	var form url.Values
	s := testSender(t, func(w http.ResponseWriter, r *http.Request) {
		_ = r.ParseForm()
		form = r.PostForm
		w.WriteHeader(http.StatusOK)
	})
	huge := strings.Repeat("x", maxBodyBytes*2)
	if err := s.Send(context.Background(), Message{
		To:      []string{"ada@example.com"},
		Subject: "failed: web\nBcc: attacker@evil.example",
		Body:    huge,
	}); err != nil {
		t.Fatalf("Send: %v", err)
	}
	if strings.ContainsAny(form.Get("subject"), "\r\n") {
		t.Fatalf("subject must not carry a line break: %q", form.Get("subject"))
	}
	if len(form.Get("text")) > maxBodyBytes+64 {
		t.Fatalf("body was not truncated: %d bytes", len(form.Get("text")))
	}
	if !strings.HasSuffix(form.Get("text"), "[truncated]") {
		t.Fatal("a truncated body must say so — a silently short one reads as a short one")
	}
}

// The EU region rejects requests to the US host with a 401, which looks exactly
// like a bad key. The base URL is configurable so that is a setting rather than
// an afternoon.
func TestBaseURLDefaultsToTheUSRegion(t *testing.T) {
	if got := New(Config{Domain: "d", APIKey: "k", From: "f"}).cfg.BaseURL; got != DefaultBaseURL {
		t.Fatalf("got %q", got)
	}
	if got := New(Config{BaseURL: "https://api.eu.mailgun.net"}).cfg.BaseURL; got != "https://api.eu.mailgun.net" {
		t.Fatalf("got %q", got)
	}
}

// Mailgun's own documentation gives the API base as https://api.mailgun.net/v3,
// so pasting that into the setting is the natural thing to do — and it would
// build /v3/v3/<domain>/messages and 404 with nothing pointing at the cause.
func TestBaseURLToleratesTheDocumentedV3Suffix(t *testing.T) {
	for _, base := range []string{
		"https://api.mailgun.net",
		"https://api.mailgun.net/",
		"https://api.mailgun.net/v3",
		"https://api.mailgun.net/v3/",
	} {
		var gotPath string
		srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			gotPath = r.URL.Path
			w.WriteHeader(http.StatusOK)
		}))
		// Rewrite the host to the test server while keeping the suffix shape.
		suffix := strings.TrimPrefix(base, "https://api.mailgun.net")
		s := New(Config{Domain: "mg.navar.ch", APIKey: "k", From: "f", BaseURL: srv.URL + suffix})
		if err := s.Send(context.Background(), Message{To: []string{"a@b.com"}, Subject: "s", Body: "b"}); err != nil {
			t.Fatalf("%s: %v", base, err)
		}
		if gotPath != "/v3/mg.navar.ch/messages" {
			t.Fatalf("base %q produced path %q", base, gotPath)
		}
		srv.Close()
	}
}
