package store

import (
	"context"
	"errors"
	"testing"
	"time"

	"github.com/google/uuid"
)

// cleanupInvitedOperator removes the operator an invite created. Operators do
// not hang off an organization, so nothing cascades them away — the same reason
// newOperator cleans up by hand.
func cleanupInvitedOperator(t *testing.T, st *Store, id uuid.UUID) {
	t.Helper()
	t.Cleanup(func() {
		c, cancel := context.WithTimeout(context.Background(), 10*time.Second)
		defer cancel()
		st.pool.Exec(c, `DELETE FROM operators WHERE id=$1`, id)
	})
}

func TestRedeemInviteCreatesAMemberWithAWorkingToken(t *testing.T) {
	st := testStore(t)
	org := newOrg(t, st)
	email := uniq("invitee") + "@example.com"

	inv, err := st.CreateInvite(testCtx(t), CreateInviteParams{OrgID: org.ID, Email: email, Role: "member"})
	if err != nil {
		t.Fatalf("CreateInvite: %v", err)
	}
	if inv.Plaintext == "" {
		t.Fatal("the plaintext token must be returned exactly once, at creation")
	}
	if inv.State() != "pending" {
		t.Fatalf("state = %q", inv.State())
	}

	op, tok, err := st.RedeemInvite(testCtx(t), inv.Plaintext, "console")
	if err != nil {
		t.Fatalf("RedeemInvite: %v", err)
	}
	cleanupInvitedOperator(t, st, op.ID)

	if op.Email != email {
		t.Fatalf("operator email = %q", op.Email)
	}
	// The token must actually authenticate. An invite that produced a token the
	// API rejects would fail at the first page load, with nothing pointing back
	// to the step that said it worked.
	who, err := st.OperatorForToken(testCtx(t), tok.Plaintext)
	if err != nil || who.ID != op.ID {
		t.Fatalf("issued token does not authenticate: %v", err)
	}
	member, err := st.OperatorInOrg(testCtx(t), org.ID, op.ID)
	if err != nil || !member {
		t.Fatalf("invited operator is not a member: %v", err)
	}
}

// Single-use is the property that matters most: the link travels through email,
// which is forwarded, archived and indexed. A second redemption must find
// nothing.
func TestInviteCannotBeRedeemedTwice(t *testing.T) {
	st := testStore(t)
	org := newOrg(t, st)
	inv, err := st.CreateInvite(testCtx(t), CreateInviteParams{OrgID: org.ID, Email: uniq("once") + "@example.com"})
	if err != nil {
		t.Fatalf("CreateInvite: %v", err)
	}
	op, _, err := st.RedeemInvite(testCtx(t), inv.Plaintext, "first")
	if err != nil {
		t.Fatalf("first redeem: %v", err)
	}
	cleanupInvitedOperator(t, st, op.ID)

	if _, _, err := st.RedeemInvite(testCtx(t), inv.Plaintext, "second"); !errors.Is(err, ErrNotFound) {
		t.Fatalf("second redeem must fail as not-found, got %v", err)
	}
}

// Expired, revoked and unknown are all ErrNotFound and indistinguishable.
// Redemption is unauthenticated, so telling the caller which one it was tells
// somebody guessing that they guessed a real invite.
func TestDeadInvitesAreAllIndistinguishable(t *testing.T) {
	st := testStore(t)
	org := newOrg(t, st)

	expired, _ := st.CreateInvite(testCtx(t), CreateInviteParams{OrgID: org.ID, Email: uniq("exp") + "@example.com"})
	if _, err := st.pool.Exec(testCtx(t),
		`UPDATE operator_invites SET expires_at = now() - interval '1 minute' WHERE id=$1`, expired.ID); err != nil {
		t.Fatalf("age the invite: %v", err)
	}
	revoked, _ := st.CreateInvite(testCtx(t), CreateInviteParams{OrgID: org.ID, Email: uniq("rev") + "@example.com"})
	if err := st.RevokeInvite(testCtx(t), org.ID, revoked.ID); err != nil {
		t.Fatalf("RevokeInvite: %v", err)
	}

	for name, plain := range map[string]string{
		"expired": expired.Plaintext,
		"revoked": revoked.Plaintext,
		"unknown": "nav_" + uuid.NewString(),
	} {
		if _, _, err := st.RedeemInvite(testCtx(t), plain, "t"); !errors.Is(err, ErrNotFound) {
			t.Fatalf("%s invite: got %v, want ErrNotFound", name, err)
		}
	}
}

// Re-inviting supersedes. Two live credentials for one person, only one of
// which anybody is tracking, is worse than either erroring or replacing — and
// revoking "the invite" would leave the other one working.
func TestReInvitingSupersedesTheOldLink(t *testing.T) {
	st := testStore(t)
	org := newOrg(t, st)
	email := uniq("again") + "@example.com"

	first, err := st.CreateInvite(testCtx(t), CreateInviteParams{OrgID: org.ID, Email: email})
	if err != nil {
		t.Fatalf("first invite: %v", err)
	}
	second, err := st.CreateInvite(testCtx(t), CreateInviteParams{OrgID: org.ID, Email: email})
	if err != nil {
		t.Fatalf("second invite must supersede rather than fail: %v", err)
	}
	if _, _, err := st.RedeemInvite(testCtx(t), first.Plaintext, "t"); !errors.Is(err, ErrNotFound) {
		t.Fatalf("the superseded link must be dead, got %v", err)
	}
	op, _, err := st.RedeemInvite(testCtx(t), second.Plaintext, "t")
	if err != nil {
		t.Fatalf("the current link must work: %v", err)
	}
	cleanupInvitedOperator(t, st, op.ID)
}

// One human, one operator row — which is what the lower(email) unique index on
// operators already insists on. Inviting somebody who already exists is how a
// person comes to belong to a second organization, not an error.
func TestInvitingAnExistingOperatorAddsAnOrgRatherThanARow(t *testing.T) {
	st := testStore(t)
	existing := newOperator(t, st)
	org := newOrg(t, st)

	inv, err := st.CreateInvite(testCtx(t), CreateInviteParams{OrgID: org.ID, Email: existing.Email})
	if err != nil {
		t.Fatalf("CreateInvite: %v", err)
	}
	op, _, err := st.RedeemInvite(testCtx(t), inv.Plaintext, "t")
	if err != nil {
		t.Fatalf("RedeemInvite: %v", err)
	}
	if op.ID != existing.ID {
		t.Fatalf("expected the existing operator %s, got a new row %s", existing.ID, op.ID)
	}
	if member, _ := st.OperatorInOrg(testCtx(t), org.ID, op.ID); !member {
		t.Fatal("the existing operator should now be a member of the new org")
	}
}

// A disabled account redeeming an invite would re-enable itself by the back
// door. Re-enabling somebody is its own decision.
func TestDisabledOperatorCannotRedeem(t *testing.T) {
	st := testStore(t)
	op := newOperator(t, st)
	org := newOrg(t, st)
	if err := st.DisableOperator(testCtx(t), op.ID); err != nil {
		t.Fatalf("DisableOperator: %v", err)
	}
	inv, _ := st.CreateInvite(testCtx(t), CreateInviteParams{OrgID: org.ID, Email: op.Email})
	if _, _, err := st.RedeemInvite(testCtx(t), inv.Plaintext, "t"); !errors.Is(err, ErrConflict) {
		t.Fatalf("want ErrConflict for a disabled account, got %v", err)
	}
	// And the invite must not have been spent by the attempt: the transaction
	// rolls back, so a re-enabled operator can still use it.
	list, _ := st.ListInvites(testCtx(t), org.ID)
	if len(list) != 1 || list[0].State() != "pending" {
		t.Fatalf("a refused redemption must not consume the invite: %+v", list)
	}
}

// A TTL beyond the cap is a hard error, not a silent clamp — storing a
// different expiry than the one asked for makes the API lie about how long a
// credential lives.
func TestInviteTTLIsCapped(t *testing.T) {
	st := testStore(t)
	org := newOrg(t, st)
	_, err := st.CreateInvite(testCtx(t), CreateInviteParams{
		OrgID: org.ID, Email: uniq("long") + "@example.com", TTL: MaxInviteTTL + time.Hour,
	})
	if !errors.Is(err, ErrInvalid) {
		t.Fatalf("want ErrInvalid, got %v", err)
	}
}

// Revoke is scoped in the WHERE clause, so naming another org's invite id
// changes nothing rather than deleting somebody else's.
func TestRevokeIsScopedToTheOrg(t *testing.T) {
	st := testStore(t)
	mine, theirs := newOrg(t, st), newOrg(t, st)
	inv, _ := st.CreateInvite(testCtx(t), CreateInviteParams{OrgID: theirs.ID, Email: uniq("other") + "@example.com"})
	if err := st.RevokeInvite(testCtx(t), mine.ID, inv.ID); !errors.Is(err, ErrNotFound) {
		t.Fatalf("want ErrNotFound, got %v", err)
	}
	list, _ := st.ListInvites(testCtx(t), theirs.ID)
	if len(list) != 1 || list[0].State() != "pending" {
		t.Fatalf("the other org's invite must be untouched: %+v", list)
	}
}
