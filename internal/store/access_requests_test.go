package store

import (
	"context"
	"errors"
	"strings"
	"sync"
	"testing"
	"time"
)

func TestAccessRequestGrantsNothingUntilApproved(t *testing.T) {
	st := testStore(t)
	org := newOrg(t, st)
	email := uniq("stranger") + "@example.com"

	ar, fresh, err := st.CreateAccessRequest(testCtx(t), CreateAccessRequestParams{
		OrgID: org.ID, Email: email, Name: "A Stranger", Note: "I want to deploy a stack",
	})
	if err != nil {
		t.Fatalf("CreateAccessRequest: %v", err)
	}
	if !fresh {
		t.Fatal("the first request for an address must report itself new")
	}
	if ar.State != AccessRequestPending {
		t.Fatalf("state = %q", ar.State)
	}

	// The whole reason this can live on an unauthenticated route: it creates no
	// identity. If filing a request produced an operator, an unverified address
	// could be squatted and then silently collected by an invitation meant for
	// its real owner — which is exactly why signup needs verification first.
	if _, err := st.GetOperatorByEmail(testCtx(t), email); !errors.Is(err, ErrNotFound) {
		t.Fatalf("filing a request must not create an operator, got err=%v", err)
	}
	// And no invitation, so nothing redeemable exists yet.
	invs, err := st.ListInvites(testCtx(t), org.ID)
	if err != nil {
		t.Fatalf("ListInvites: %v", err)
	}
	if len(invs) != 0 {
		t.Fatalf("filing a request minted %d invitations", len(invs))
	}
}

// A public form is submitted twice by anyone unsure it worked. The second must
// update the first rather than queue behind it, and must not report itself new
// — that bool is what stops one notification per submission.
func TestResubmittingUpdatesTheSameRequest(t *testing.T) {
	st := testStore(t)
	org := newOrg(t, st)
	email := uniq("twice") + "@example.com"

	first, fresh, err := st.CreateAccessRequest(testCtx(t), CreateAccessRequestParams{
		OrgID: org.ID, Email: email, Note: "first try",
	})
	if err != nil || !fresh {
		t.Fatalf("first: err=%v fresh=%v", err, fresh)
	}
	// Different capitalisation, because one human is one row regardless of how
	// they typed it — the same rule operators.lower(email) already enforces.
	second, fresh, err := st.CreateAccessRequest(testCtx(t), CreateAccessRequestParams{
		OrgID: org.ID, Email: strings.ToUpper(email), Note: "second try",
	})
	if err != nil {
		t.Fatalf("second: %v", err)
	}
	if fresh {
		t.Fatal("a resubmission must not report itself new, or every submission mails")
	}
	if second.ID != first.ID {
		t.Fatalf("resubmission created a second row: %s then %s", first.ID, second.ID)
	}
	if second.Note != "second try" {
		t.Fatalf("note = %q, want the resubmitted text", second.Note)
	}
	// And a resubmission that omits a field must not erase it. The second
	// submission is characteristically from somebody who was not sure the first
	// worked, and it should not cost the operator what they need to decide on.
	third, _, err := st.CreateAccessRequest(testCtx(t), CreateAccessRequestParams{
		OrgID: org.ID, Email: email, Name: "Ada",
	})
	if err != nil {
		t.Fatalf("third: %v", err)
	}
	if third.Note != "second try" {
		t.Fatalf("an omitted note erased the previous one: %q", third.Note)
	}
	if third.Name != "Ada" {
		t.Fatalf("a supplied name was not kept: %q", third.Name)
	}
	list, err := st.ListAccessRequests(testCtx(t), org.ID)
	if err != nil {
		t.Fatalf("ListAccessRequests: %v", err)
	}
	if len(list) != 1 {
		t.Fatalf("%d rows for one address", len(list))
	}
}

func TestApprovalMintsExactlyOneInviteThroughTheOrdinaryPath(t *testing.T) {
	st := testStore(t)
	org := newOrg(t, st)
	email := uniq("approved") + "@example.com"

	ar, _, err := st.CreateAccessRequest(testCtx(t), CreateAccessRequestParams{OrgID: org.ID, Email: email})
	if err != nil {
		t.Fatalf("CreateAccessRequest: %v", err)
	}

	decided, inv, err := st.ApproveAccessRequest(testCtx(t), ApproveAccessRequestParams{
		OrgID: org.ID, RequestID: ar.ID, Role: "member",
	})
	if err != nil {
		t.Fatalf("ApproveAccessRequest: %v", err)
	}
	if decided.State != AccessRequestApproved {
		t.Fatalf("state = %q", decided.State)
	}
	if decided.InviteID == nil || *decided.InviteID != inv.ID {
		t.Fatal("the request must record the invitation it produced")
	}
	if inv.Plaintext == "" {
		t.Fatal("the plaintext must come back once, or nothing can be sent")
	}

	// It has to be a real invitation, not a lookalike row: redeeming it through
	// the ordinary path is the assertion that approval did not become a second
	// way to hand out access.
	op, tok, err := st.RedeemInvite(testCtx(t), inv.Plaintext, "console")
	if err != nil {
		t.Fatalf("the invitation approval minted was not redeemable: %v", err)
	}
	cleanupInvitedOperator(t, st, op.ID)
	if tok.Plaintext == "" {
		t.Fatal("redemption must mint a token")
	}
	if !strings.EqualFold(op.Email, email) {
		t.Fatalf("redeemed as %q, want %q", op.Email, email)
	}
}

// Two operators clicking approve at the same moment must not mint two
// invitations for one person. The guard is the state in the UPDATE's WHERE, so
// the loser fails the claim rather than issuing a second credential.
func TestApprovingTwiceIssuesOneInvitation(t *testing.T) {
	st := testStore(t)
	org := newOrg(t, st)
	email := uniq("race") + "@example.com"

	ar, _, err := st.CreateAccessRequest(testCtx(t), CreateAccessRequestParams{OrgID: org.ID, Email: email})
	if err != nil {
		t.Fatalf("CreateAccessRequest: %v", err)
	}

	var wg sync.WaitGroup
	results := make([]error, 2)
	for i := range results {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			c, cancel := context.WithTimeout(context.Background(), 20*time.Second)
			defer cancel()
			_, _, results[i] = st.ApproveAccessRequest(c, ApproveAccessRequestParams{
				OrgID: org.ID, RequestID: ar.ID,
			})
		}(i)
	}
	wg.Wait()

	won := 0
	for _, err := range results {
		switch {
		case err == nil:
			won++
		case errors.Is(err, ErrNotFound), errors.Is(err, ErrConflict):
		default:
			t.Fatalf("unexpected error from a losing approval: %v", err)
		}
	}
	if won != 1 {
		t.Fatalf("%d of 2 concurrent approvals succeeded, want exactly 1", won)
	}
	invs, err := st.ListInvites(testCtx(t), org.ID)
	if err != nil {
		t.Fatalf("ListInvites: %v", err)
	}
	live := 0
	for _, inv := range invs {
		if inv.State() == "pending" {
			live++
		}
	}
	if live != 1 {
		t.Fatalf("%d live invitations after a double approval, want 1", live)
	}
}

func TestDecliningRecordsTheDecisionAndIssuesNothing(t *testing.T) {
	st := testStore(t)
	org := newOrg(t, st)
	email := uniq("declined") + "@example.com"

	ar, _, err := st.CreateAccessRequest(testCtx(t), CreateAccessRequestParams{OrgID: org.ID, Email: email})
	if err != nil {
		t.Fatalf("CreateAccessRequest: %v", err)
	}
	decided, err := st.DeclineAccessRequest(testCtx(t), org.ID, ar.ID, nil)
	if err != nil {
		t.Fatalf("DeclineAccessRequest: %v", err)
	}
	if decided.State != AccessRequestDeclined {
		t.Fatalf("state = %q", decided.State)
	}
	invs, _ := st.ListInvites(testCtx(t), org.ID)
	if len(invs) != 0 {
		t.Fatalf("declining minted %d invitations", len(invs))
	}
	// The row survives: "did we ever hear from this person" has to stay
	// answerable, which a delete would not allow.
	if _, err := st.GetAccessRequest(testCtx(t), ar.ID); err != nil {
		t.Fatalf("a declined request must not vanish: %v", err)
	}
	// And declining is not a denylist — they may ask again.
	again, fresh, err := st.CreateAccessRequest(testCtx(t), CreateAccessRequestParams{OrgID: org.ID, Email: email})
	if err != nil {
		t.Fatalf("a declined address must be able to ask again: %v", err)
	}
	if !fresh || again.ID == ar.ID {
		t.Fatal("asking again after a decline must file a new request")
	}
}

// Approving is scoped to the org in the call, not just to the request id. A
// resolver bug that leaked the id must still not let another org decide it.
func TestDecisionsAreScopedToTheOrg(t *testing.T) {
	st := testStore(t)
	org := newOrg(t, st)
	other := newOrg(t, st)

	ar, _, err := st.CreateAccessRequest(testCtx(t), CreateAccessRequestParams{
		OrgID: org.ID, Email: uniq("scoped") + "@example.com",
	})
	if err != nil {
		t.Fatalf("CreateAccessRequest: %v", err)
	}
	if _, _, err := st.ApproveAccessRequest(testCtx(t), ApproveAccessRequestParams{
		OrgID: other.ID, RequestID: ar.ID,
	}); !errors.Is(err, ErrNotFound) {
		t.Fatalf("cross-org approve returned %v, want ErrNotFound", err)
	}
	if _, err := st.DeclineAccessRequest(testCtx(t), other.ID, ar.ID, nil); !errors.Is(err, ErrNotFound) {
		t.Fatalf("cross-org decline returned %v, want ErrNotFound", err)
	}
}

func TestAccessRequestRejectsUnusableInput(t *testing.T) {
	st := testStore(t)
	org := newOrg(t, st)

	for _, tc := range []struct {
		name string
		p    CreateAccessRequestParams
	}{
		{"no email", CreateAccessRequestParams{OrgID: org.ID}},
		{"not an address", CreateAccessRequestParams{OrgID: org.ID, Email: "nope"}},
		{"note too long", CreateAccessRequestParams{
			OrgID: org.ID, Email: uniq("long") + "@example.com",
			Note: strings.Repeat("x", maxNoteLen+1),
		}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			if _, _, err := st.CreateAccessRequest(testCtx(t), tc.p); !errors.Is(err, ErrInvalid) {
				t.Fatalf("got %v, want ErrInvalid", err)
			}
		})
	}
}
