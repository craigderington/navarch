package api

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/craigderington/navarch/internal/mail"
)

type fakeMailer struct {
	sent []mail.Message
	err  error
}

func (f *fakeMailer) Send(_ context.Context, m mail.Message) error {
	f.sent = append(f.sent, m)
	return f.err
}

func postJSON(t *testing.T, srv *Server, token, path, body string) *httptest.ResponseRecorder {
	t.Helper()
	req := httptest.NewRequest("POST", path, strings.NewReader(body))
	req.Header.Set("Content-Type", "application/json")
	if token != "" {
		req.Header.Set("Authorization", "Bearer "+token)
	}
	rec := httptest.NewRecorder()
	srv.ServeHTTP(rec, req)
	return rec
}

// The full round trip: an admin invites, the invitee redeems, and the token
// that comes back actually authenticates. Anything less than the last clause
// would let an invite "succeed" and fail at the first page load.
func TestInviteRoundTripProducesAWorkingCredential(t *testing.T) {
	m := &fakeMailer{}
	srv := testServer(t, WithBearerToken("shared-service-token"),
		WithMailer(m), WithConsoleURL("https://console.navar.ch"))
	f := newScopedFixture(t, srv.st)

	rec := postJSON(t, srv, f.op.token, "/v1/orgs/"+f.orgID.String()+"/invites",
		`{"email":"newcomer@example.com","role":"member"}`)
	if rec.Code != http.StatusCreated {
		t.Fatalf("create invite: %d %s", rec.Code, rec.Body.String())
	}
	var created createInviteResponse
	if err := json.Unmarshal(rec.Body.Bytes(), &created); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if !strings.HasPrefix(created.URL, "https://console.navar.ch/invite?token=") {
		t.Fatalf("link must be built from the configured console URL: %q", created.URL)
	}
	if !created.Emailed || len(m.sent) != 1 {
		t.Fatalf("expected one email, emailed=%v sent=%d", created.Emailed, len(m.sent))
	}
	if m.sent[0].To[0] != "newcomer@example.com" || !strings.Contains(m.sent[0].Body, created.URL) {
		t.Fatalf("the email must carry the link to the invitee: %+v", m.sent[0])
	}
	// The response body must not repeat the raw token as a field of its own —
	// the URL is the one place it appears, so there is one thing to be careful
	// with rather than two.
	if created.Invite.Plaintext != "" {
		t.Fatal("the invite struct must not serialise its plaintext")
	}

	token := strings.TrimPrefix(created.URL, "https://console.navar.ch/invite?token=")
	rec = postJSON(t, srv, "", "/v1/invites/redeem", `{"token":"`+token+`"}`)
	if rec.Code != http.StatusOK {
		t.Fatalf("redeem must work unauthenticated: %d %s", rec.Code, rec.Body.String())
	}
	var redeemed redeemInviteResponse
	if err := json.Unmarshal(rec.Body.Bytes(), &redeemed); err != nil {
		t.Fatalf("decode: %v", err)
	}
	t.Cleanup(func() {
		srv.st.Pool().Exec(context.Background(), `DELETE FROM operators WHERE id=$1`, redeemed.Operator.ID)
	})

	// The minted token must reach an org-scoped route that the invite granted.
	req := httptest.NewRequest("GET", "/v1/orgs/"+f.orgID.String()+"/apps", nil)
	req.Header.Set("Authorization", "Bearer "+redeemed.Token)
	check := httptest.NewRecorder()
	srv.ServeHTTP(check, req)
	if check.Code != http.StatusOK {
		t.Fatalf("the invited operator cannot reach their new org: %d %s", check.Code, check.Body.String())
	}
}

// A provider outage must not destroy a credential that was already minted. The
// invitation exists and the caller has the link; failing the request would
// leave a live invite nobody was told about and that the caller must go and
// revoke.
func TestInviteSurvivesAMailFailureAndSaysSo(t *testing.T) {
	m := &fakeMailer{err: errors.New("mailgun: 401 Unauthorized")}
	srv := testServer(t, WithBearerToken("shared-service-token"),
		WithMailer(m), WithConsoleURL("https://console.navar.ch"))
	f := newScopedFixture(t, srv.st)

	rec := postJSON(t, srv, f.op.token, "/v1/orgs/"+f.orgID.String()+"/invites",
		`{"email":"unreachable@example.com"}`)
	if rec.Code != http.StatusCreated {
		t.Fatalf("create invite: %d %s", rec.Code, rec.Body.String())
	}
	var created createInviteResponse
	json.Unmarshal(rec.Body.Bytes(), &created)
	if created.Emailed {
		t.Fatal("emailed must be false when the provider refused")
	}
	if created.Error == "" {
		t.Fatal("the provider's reason must reach the caller, or they will believe it arrived")
	}
	if created.URL == "" {
		t.Fatal("the link must still be returned — it is the fallback path")
	}
}

// With no mail configured at all the feature still works: the link comes back
// and an operator pastes it. Mail is an accelerant, not a dependency.
func TestInviteWorksWithNoMailProvider(t *testing.T) {
	srv := testServer(t, WithBearerToken("shared-service-token"),
		WithConsoleURL("https://console.navar.ch"))
	f := newScopedFixture(t, srv.st)

	rec := postJSON(t, srv, f.op.token, "/v1/orgs/"+f.orgID.String()+"/invites",
		`{"email":"nomail@example.com"}`)
	if rec.Code != http.StatusCreated {
		t.Fatalf("create invite: %d %s", rec.Code, rec.Body.String())
	}
	var created createInviteResponse
	json.Unmarshal(rec.Body.Bytes(), &created)
	if created.Emailed || created.URL == "" {
		t.Fatalf("want a link and no claim of email: %+v", created)
	}
}

// Unknown, expired, revoked and already-redeemed must all look the same from
// outside. Redemption is unauthenticated, so a distinguishable failure tells
// somebody guessing that they guessed a real invite.
func TestRedeemingRubbishIsAnOrdinary404(t *testing.T) {
	srv := testServer(t, WithBearerToken("shared-service-token"))
	rec := postJSON(t, srv, "", "/v1/invites/redeem", `{"token":"nav_not-a-real-invite"}`)
	if rec.Code != http.StatusNotFound {
		t.Fatalf("got %d, want 404: %s", rec.Code, rec.Body.String())
	}
	if strings.Contains(strings.ToLower(rec.Body.String()), "expired") {
		t.Fatalf("the reason must not be disclosed: %s", rec.Body.String())
	}
}

// The link is built from configuration, never from the request. A link
// assembled out of the Host header is one an attacker can aim: invite someone,
// set Host to a site you control, and the victim types their invitation into it.
func TestInviteLinkIgnoresTheHostHeader(t *testing.T) {
	srv := testServer(t, WithBearerToken("shared-service-token"),
		WithConsoleURL("https://console.navar.ch"))
	f := newScopedFixture(t, srv.st)

	req := httptest.NewRequest("POST", "/v1/orgs/"+f.orgID.String()+"/invites",
		strings.NewReader(`{"email":"aimed@example.com"}`))
	req.Host = "evil.example.com"
	req.Header.Set("Host", "evil.example.com")
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Authorization", "Bearer "+f.op.token)
	rec := httptest.NewRecorder()
	srv.ServeHTTP(rec, req)

	var created createInviteResponse
	json.Unmarshal(rec.Body.Bytes(), &created)
	if !strings.HasPrefix(created.URL, "https://console.navar.ch/") {
		t.Fatalf("the link followed the request rather than the configuration: %q", created.URL)
	}
}
