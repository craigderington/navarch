package api

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/google/uuid"

	"github.com/craigderington/navarch/internal/mail"
	"github.com/craigderington/navarch/internal/store"
)

// captureMailer records what would have been sent, so a test can assert on the
// recipients and on how many messages a sequence of requests produced.
type captureMailer struct {
	sent []mail.Message
	err  error
}

func (m *captureMailer) Send(_ context.Context, msg mail.Message) error {
	if m.err != nil {
		return m.err
	}
	m.sent = append(m.sent, msg)
	return nil
}

// signupServer builds a server whose request-access door is open onto a fresh
// org with one operator in it, and returns both. A fresh org rather than "dev":
// the assertions count rows and messages, and the dev org is shared with every
// other test and demo. The operator exists because a notification with no
// recipients is not sent at all, which would make the mail assertions pass
// vacuously.
func signupServer(t *testing.T, mailer Mailer) (*Server, *store.Organization, *store.Operator) {
	t.Helper()
	slug := "signup-" + uuid.NewString()[:8]
	opts := []ServerOption{WithSignupOrg(slug), WithConsoleURL("https://console.example.com")}
	if mailer != nil {
		opts = append(opts, WithMailer(mailer))
	}
	srv := testServer(t, opts...)

	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	org, err := srv.st.CreateOrganization(ctx, slug, "Signup Fixture")
	if err != nil {
		t.Fatalf("CreateOrganization: %v", err)
	}
	t.Cleanup(func() {
		c, cancel := context.WithTimeout(context.Background(), 10*time.Second)
		defer cancel()
		srv.st.Pool().Exec(c, `DELETE FROM organizations WHERE id=$1`, org.ID)
	})

	op, err := srv.st.CreateOperator(ctx, "owner-"+uuid.NewString()[:8]+"@example.com", "Owner")
	if err != nil {
		t.Fatalf("CreateOperator: %v", err)
	}
	t.Cleanup(func() {
		c, cancel := context.WithTimeout(context.Background(), 10*time.Second)
		defer cancel()
		srv.st.Pool().Exec(c, `DELETE FROM operators WHERE id=$1`, op.ID)
	})
	if err := srv.st.AddOrgMember(ctx, org.ID, op.ID, "owner"); err != nil {
		t.Fatalf("AddOrgMember: %v", err)
	}
	return srv, org, op
}

func postAccessRequest(t *testing.T, srv *Server, body string) *httptest.ResponseRecorder {
	t.Helper()
	req := httptest.NewRequest(http.MethodPost, "/v1/access-requests", strings.NewReader(body))
	req.Header.Set("Content-Type", "application/json")
	rec := httptest.NewRecorder()
	srv.ServeHTTP(rec, req)
	return rec
}

// The door is closed unless an operator opened it. An install that has not set
// COMPOSECTL_SIGNUP_ORG must have no unauthenticated write surface at all.
func TestAccessRequestsAreClosedByDefault(t *testing.T) {
	srv := testServer(t, WithBearerToken("shared-service-token"))
	rec := postAccessRequest(t, srv, `{"email":"stranger@example.com"}`)
	if rec.Code != http.StatusNotFound {
		t.Fatalf("got %d, want 404 on an install that has not opted in: %s",
			rec.Code, strings.TrimSpace(rec.Body.String()))
	}
}

// It has to work with no credential whatsoever — that is the entire point, and
// it is the one property a test server built without a bearer token cannot
// observe, since that skips authentication for every route alike.
func TestAnyoneMayAskAndNothingIsGranted(t *testing.T) {
	srv, org, _ := signupServer(t, nil)
	srv.bearerToken = "shared-service-token"

	rec := postAccessRequest(t, srv,
		`{"email":"stranger@example.com","name":"A Stranger","note":"I want to deploy a stack"}`)
	if rec.Code != http.StatusAccepted {
		t.Fatalf("got %d, want 202: %s", rec.Code, strings.TrimSpace(rec.Body.String()))
	}

	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()

	// No identity was created. This is what separates a request from signup,
	// and why signup needs email verification before it is safe while this does
	// not: there is no operator row to squat.
	if _, err := srv.st.GetOperatorByEmail(ctx, "stranger@example.com"); !errors.Is(err, store.ErrNotFound) {
		t.Fatalf("asking created an operator (err=%v)", err)
	}
	invs, err := srv.st.ListInvites(ctx, org.ID)
	if err != nil {
		t.Fatalf("ListInvites: %v", err)
	}
	if len(invs) != 0 {
		t.Fatalf("asking minted %d invitations", len(invs))
	}

	reqs, err := srv.st.ListAccessRequests(ctx, org.ID)
	if err != nil {
		t.Fatalf("ListAccessRequests: %v", err)
	}
	if len(reqs) != 1 || reqs[0].Email != "stranger@example.com" {
		t.Fatalf("expected exactly one request for the stranger, got %+v", reqs)
	}
	if reqs[0].Note != "I want to deploy a stack" {
		t.Fatalf("the note is what helps decide, and it was not kept: %q", reqs[0].Note)
	}
}

// Every outcome a caller could learn something from answers identically. The
// response must not distinguish a first request from a repeat, or the endpoint
// becomes an account-existence oracle anyone on the internet can query.
func TestTheAnswerNeverRevealsWhatAlreadyExists(t *testing.T) {
	m := &captureMailer{}
	srv, _, _ := signupServer(t, m)
	srv.bearerToken = "shared-service-token"

	body := `{"email":"repeat@example.com","note":"again"}`
	first := postAccessRequest(t, srv, body)
	second := postAccessRequest(t, srv, body)

	if first.Code != second.Code {
		t.Fatalf("a repeat answered %d where the first answered %d", second.Code, first.Code)
	}
	if first.Body.String() != second.Body.String() {
		t.Fatalf("a repeat answered differently:\n first: %s\nsecond: %s",
			first.Body.String(), second.Body.String())
	}

	// And the repeat must not mail again. A public form is submitted twice by
	// anyone unsure it worked; a message per submission is how an operator
	// learns to filter the sender.
	if len(m.sent) != 1 {
		t.Fatalf("%d messages for two submissions of one address, want 1", len(m.sent))
	}
}

// A misconfigured install must not look like a closed one. 404 for "the door is
// shut" and 503 for "the door is configured onto an organization that does not
// exist" — collapsing them hides a broken deployment behind an expected answer.
func TestAMisconfiguredDoorIsNotAClosedOne(t *testing.T) {
	srv := testServer(t, WithSignupOrg("no-such-org-"+uuid.NewString()[:8]))
	srv.bearerToken = "shared-service-token"

	rec := postAccessRequest(t, srv, `{"email":"stranger@example.com"}`)
	if rec.Code != http.StatusServiceUnavailable {
		t.Fatalf("got %d, want 503 when the configured org does not exist: %s",
			rec.Code, strings.TrimSpace(rec.Body.String()))
	}
}

func TestAMalformedAddressIsRefused(t *testing.T) {
	srv, _, _ := signupServer(t, nil)
	srv.bearerToken = "shared-service-token"

	rec := postAccessRequest(t, srv, `{"email":"not-an-address"}`)
	if rec.Code != http.StatusBadRequest {
		t.Fatalf("got %d, want 400: %s", rec.Code, strings.TrimSpace(rec.Body.String()))
	}
}

// The notification goes to the organization's operators, and carries a link
// built from the configured console URL rather than from the request.
func TestTheNotificationReachesTheOrgsOperators(t *testing.T) {
	m := &captureMailer{}
	srv, _, op := signupServer(t, m)
	srv.bearerToken = "shared-service-token"

	if rec := postAccessRequest(t, srv, `{"email":"asker@example.com","note":"please"}`); rec.Code != http.StatusAccepted {
		t.Fatalf("got %d", rec.Code)
	}
	if len(m.sent) != 1 {
		t.Fatalf("%d messages, want 1", len(m.sent))
	}
	msg := m.sent[0]
	if len(msg.To) != 1 || msg.To[0] != op.Email {
		t.Fatalf("sent to %v, want the org's operator %s", msg.To, op.Email)
	}
	if !strings.Contains(msg.Body, "https://console.example.com/access-requests") {
		t.Fatalf("the review link must come from the configured console URL:\n%s", msg.Body)
	}
	if !strings.Contains(msg.Body, "Nothing has been granted") {
		t.Fatalf("the message must say plainly that nothing was granted:\n%s", msg.Body)
	}
}

// A request that cannot be mailed is still filed. Mail is opt-in and nothing
// depends on it — the list is the durable copy, and refusing here would reject
// a request that had in fact been recorded.
func TestAFailedNotificationStillFilesTheRequest(t *testing.T) {
	m := &captureMailer{err: errors.New("provider is having a bad day")}
	srv, org, _ := signupServer(t, m)
	srv.bearerToken = "shared-service-token"

	if rec := postAccessRequest(t, srv, `{"email":"asker@example.com"}`); rec.Code != http.StatusAccepted {
		t.Fatalf("got %d, want 202 despite the mail failing: %s",
			rec.Code, strings.TrimSpace(rec.Body.String()))
	}
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	reqs, err := srv.st.ListAccessRequests(ctx, org.ID)
	if err != nil || len(reqs) != 1 {
		t.Fatalf("the request must survive a failed notification: %d rows, err=%v", len(reqs), err)
	}
}

// Approving is the ordinary invitation path, and the response has to carry the
// link — an install with no mail configured, or one whose provider is down,
// onboards by pasting it.
func TestApprovingReturnsARedeemableInvitation(t *testing.T) {
	srv, org, _ := signupServer(t, nil)

	ctx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
	defer cancel()
	ar, _, err := srv.st.CreateAccessRequest(ctx, store.CreateAccessRequestParams{
		OrgID: org.ID, Email: "approved@example.com",
	})
	if err != nil {
		t.Fatalf("CreateAccessRequest: %v", err)
	}

	rec := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodPost,
		"/v1/orgs/"+org.ID.String()+"/access-requests/"+ar.ID.String()+"/approve",
		strings.NewReader(`{"role":"member"}`))
	req.Header.Set("Content-Type", "application/json")
	srv.ServeHTTP(rec, req)
	if rec.Code != http.StatusCreated {
		t.Fatalf("got %d, want 201: %s", rec.Code, strings.TrimSpace(rec.Body.String()))
	}

	var resp struct {
		AccessRequest store.AccessRequest  `json:"access_request"`
		Invite        store.OperatorInvite `json:"invite"`
		URL           string               `json:"url"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &resp); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if resp.AccessRequest.State != store.AccessRequestApproved {
		t.Fatalf("state = %q", resp.AccessRequest.State)
	}
	if !strings.HasPrefix(resp.URL, "https://console.example.com/invite?token=") {
		t.Fatalf("url = %q, want a console invite link", resp.URL)
	}
	// The token is in the link and must never be in the serialised invite: the
	// response body is logged, proxied and pasted, and OperatorInvite.Plaintext
	// is json:"-" precisely so it cannot ride along.
	if strings.Contains(strings.ToLower(rec.Body.String()), `"plaintext"`) {
		t.Fatalf("the invite token was serialised into the response:\n%s", rec.Body.String())
	}

	// A second approval of the same request must not mint a second credential.
	rec2 := httptest.NewRecorder()
	req2 := httptest.NewRequest(http.MethodPost,
		"/v1/orgs/"+org.ID.String()+"/access-requests/"+ar.ID.String()+"/approve", strings.NewReader(`{}`))
	req2.Header.Set("Content-Type", "application/json")
	srv.ServeHTTP(rec2, req2)
	if rec2.Code != http.StatusNotFound {
		t.Fatalf("re-approving got %d, want 404: %s", rec2.Code, strings.TrimSpace(rec2.Body.String()))
	}
}

func TestDecliningIssuesNothing(t *testing.T) {
	srv, org, _ := signupServer(t, nil)

	ctx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
	defer cancel()
	ar, _, err := srv.st.CreateAccessRequest(ctx, store.CreateAccessRequestParams{
		OrgID: org.ID, Email: "declined@example.com",
	})
	if err != nil {
		t.Fatalf("CreateAccessRequest: %v", err)
	}

	rec := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodPost,
		"/v1/orgs/"+org.ID.String()+"/access-requests/"+ar.ID.String()+"/decline", nil)
	srv.ServeHTTP(rec, req)
	if rec.Code != http.StatusOK {
		t.Fatalf("got %d, want 200: %s", rec.Code, strings.TrimSpace(rec.Body.String()))
	}
	invs, err := srv.st.ListInvites(ctx, org.ID)
	if err != nil {
		t.Fatalf("ListInvites: %v", err)
	}
	if len(invs) != 0 {
		t.Fatalf("declining minted %d invitations", len(invs))
	}
}
