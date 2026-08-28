package store

import (
	"context"
	"errors"
	"testing"
	"time"

	"github.com/google/uuid"
)

// newOperator creates a throwaway operator and deletes it when the test ends.
// Operators do not hang off an organization, so nothing cascades them away the
// way newOrg's cleanup handles the catalog — they need removing by hand or
// every run leaves rows behind in the shared dev database.
func newOperator(t *testing.T, st *Store) *Operator {
	t.Helper()
	op, err := st.CreateOperator(testCtx(t), uniq("op")+"@example.com", "Test Operator")
	if err != nil {
		t.Fatalf("CreateOperator: %v", err)
	}
	t.Cleanup(func() {
		c, cancel := context.WithTimeout(context.Background(), 10*time.Second)
		defer cancel()
		// Tokens and memberships cascade from the operator row; events do not
		// (actor_operator_id is SET NULL), which is the point of that choice.
		if _, err := st.pool.Exec(c, `DELETE FROM operators WHERE id=$1`, op.ID); err != nil {
			t.Errorf("cleanup operator: %v", err)
		}
	})
	return op
}

func TestOperatorTokenAuthenticatesOnlyItsOwner(t *testing.T) {
	st := testStore(t)
	a, b := newOperator(t, st), newOperator(t, st)

	ta, err := st.IssueOperatorToken(testCtx(t), a.ID, "cli", nil)
	if err != nil {
		t.Fatalf("issue: %v", err)
	}
	if ta.Plaintext == "" {
		t.Fatal("issuing a token must return the plaintext exactly once")
	}
	tb, err := st.IssueOperatorToken(testCtx(t), b.ID, "cli", nil)
	if err != nil {
		t.Fatalf("issue b: %v", err)
	}

	got, err := st.OperatorForToken(testCtx(t), ta.Plaintext)
	if err != nil {
		t.Fatalf("authenticate a: %v", err)
	}
	if got.ID != a.ID {
		t.Fatalf("token resolved to the wrong operator: %s want %s", got.ID, a.ID)
	}
	// The interesting half: b's token must never resolve to a. A lookup keyed
	// on anything but the hash — an accidental first-row query, say — passes
	// the assertion above and fails this one.
	got, err = st.OperatorForToken(testCtx(t), tb.Plaintext)
	if err != nil || got.ID != b.ID {
		t.Fatalf("b's token must resolve to b: %v %v", got, err)
	}
	if _, err := st.OperatorForToken(testCtx(t), "not-a-real-token"); !errors.Is(err, ErrNotFound) {
		t.Fatalf("an unknown token must be ErrNotFound, got %v", err)
	}
	if _, err := st.OperatorForToken(testCtx(t), ""); !errors.Is(err, ErrNotFound) {
		t.Fatalf("an empty token must be ErrNotFound, got %v", err)
	}
}

// Disabling is the revocation path, and it has to work through a credential
// that is otherwise still perfectly valid — the token row is deliberately left
// in place, so nothing but the disabled_at check stands between it and access.
func TestDisabledOperatorCannotAuthenticate(t *testing.T) {
	st := testStore(t)
	op := newOperator(t, st)
	tok, err := st.IssueOperatorToken(testCtx(t), op.ID, "cli", nil)
	if err != nil {
		t.Fatalf("issue: %v", err)
	}
	if _, err := st.OperatorForToken(testCtx(t), tok.Plaintext); err != nil {
		t.Fatalf("token should work before disable: %v", err)
	}
	if err := st.DisableOperator(testCtx(t), op.ID); err != nil {
		t.Fatalf("disable: %v", err)
	}
	if _, err := st.OperatorForToken(testCtx(t), tok.Plaintext); !errors.Is(err, ErrNotFound) {
		t.Fatalf("a disabled operator's token must stop authenticating, got %v", err)
	}
	// The row survives, because events point at it.
	if _, err := st.GetOperator(testCtx(t), op.ID); err != nil {
		t.Fatalf("disable must not delete the operator: %v", err)
	}
}

func TestExpiredOperatorTokenIsRefused(t *testing.T) {
	st := testStore(t)
	op := newOperator(t, st)
	past := time.Now().Add(-time.Minute)
	tok, err := st.IssueOperatorToken(testCtx(t), op.ID, "expired", &past)
	if err != nil {
		t.Fatalf("issue: %v", err)
	}
	if _, err := st.OperatorForToken(testCtx(t), tok.Plaintext); !errors.Is(err, ErrNotFound) {
		t.Fatalf("an expired token must be refused, got %v", err)
	}
	future := time.Now().Add(time.Hour)
	live, err := st.IssueOperatorToken(testCtx(t), op.ID, "live", &future)
	if err != nil {
		t.Fatalf("issue live: %v", err)
	}
	if _, err := st.OperatorForToken(testCtx(t), live.Plaintext); err != nil {
		t.Fatalf("an unexpired token must still work: %v", err)
	}
}

// One human, one row. The unique index is on lower(email), so the store must
// look up the same way or a lookup can miss a row the index would refuse.
func TestOperatorEmailIsCaseInsensitive(t *testing.T) {
	st := testStore(t)
	addr := uniq("Mixed") + "@Example.COM"
	op, err := st.CreateOperator(testCtx(t), addr, "Case Test")
	if err != nil {
		t.Fatalf("create: %v", err)
	}
	t.Cleanup(func() {
		c, cancel := context.WithTimeout(context.Background(), 10*time.Second)
		defer cancel()
		st.pool.Exec(c, `DELETE FROM operators WHERE id=$1`, op.ID)
	})

	if _, err := st.CreateOperator(testCtx(t), lowerASCII(addr), "Dup"); !errors.Is(err, ErrConflict) {
		t.Fatalf("a differently-cased duplicate must conflict, got %v", err)
	}
	found, err := st.GetOperatorByEmail(testCtx(t), lowerASCII(addr))
	if err != nil {
		t.Fatalf("lookup by lowercased address: %v", err)
	}
	if found.ID != op.ID {
		t.Fatalf("case-insensitive lookup found the wrong row: %s want %s", found.ID, op.ID)
	}
}

func lowerASCII(s string) string {
	b := []byte(s)
	for i, c := range b {
		if c >= 'A' && c <= 'Z' {
			b[i] = c + 32
		}
	}
	return string(b)
}

func TestInvalidOperatorEmailIsRefusedBeforePostgres(t *testing.T) {
	st := testStore(t)
	for _, bad := range []string{"", "no-at-sign", "@example.com", "user@", "two@at@signs", "has space@example.com"} {
		if _, err := st.CreateOperator(testCtx(t), bad, "x"); !errors.Is(err, ErrInvalid) {
			t.Fatalf("CreateOperator(%q) must be ErrInvalid, got %v", bad, err)
		}
	}
}

func TestMembershipDecidesWhichOrgsAnOperatorSees(t *testing.T) {
	st := testStore(t)
	mine, theirs := newOrg(t, st), newOrg(t, st)
	op := newOperator(t, st)

	if in, err := st.OperatorInOrg(testCtx(t), mine.ID, op.ID); err != nil || in {
		t.Fatalf("a fresh operator is in no org: in=%v err=%v", in, err)
	}
	if err := st.AddOrgMember(testCtx(t), mine.ID, op.ID, ""); err != nil {
		t.Fatalf("add member: %v", err)
	}
	if in, err := st.OperatorInOrg(testCtx(t), mine.ID, op.ID); err != nil || !in {
		t.Fatalf("expected membership after add: in=%v err=%v", in, err)
	}
	// The assertion that matters: membership in one org grants nothing in
	// another. Everything the authorization layer promises rests on this.
	if in, err := st.OperatorInOrg(testCtx(t), theirs.ID, op.ID); err != nil || in {
		t.Fatalf("membership must not leak across orgs: in=%v err=%v", in, err)
	}

	orgs, err := st.OrgsForOperator(testCtx(t), op.ID)
	if err != nil {
		t.Fatalf("OrgsForOperator: %v", err)
	}
	if len(orgs) != 1 || orgs[0].ID != mine.ID {
		t.Fatalf("expected exactly the joined org, got %+v", orgs)
	}

	// Re-adding is an upsert, not a conflict: membership is a set, and asking
	// for a state that already holds is not a client error.
	if err := st.AddOrgMember(testCtx(t), mine.ID, op.ID, "viewer"); err != nil {
		t.Fatalf("re-add must not conflict: %v", err)
	}
	members, err := st.ListOrgMembers(testCtx(t), mine.ID)
	if err != nil || len(members) != 1 || members[0].Role != "viewer" {
		t.Fatalf("re-add should have updated the role, got %+v (%v)", members, err)
	}

	if err := st.RemoveOrgMember(testCtx(t), mine.ID, op.ID); err != nil {
		t.Fatalf("remove: %v", err)
	}
	if in, _ := st.OperatorInOrg(testCtx(t), mine.ID, op.ID); in {
		t.Fatal("membership survived removal")
	}
	if err := st.RemoveOrgMember(testCtx(t), mine.ID, op.ID); !errors.Is(err, ErrNotFound) {
		t.Fatalf("removing a non-member must be ErrNotFound, got %v", err)
	}
}

func TestRevokeOperatorTokenIsScopedToItsOwner(t *testing.T) {
	st := testStore(t)
	a, b := newOperator(t, st), newOperator(t, st)
	tok, err := st.IssueOperatorToken(testCtx(t), a.ID, "cli", nil)
	if err != nil {
		t.Fatalf("issue: %v", err)
	}
	// b naming a's token id must change nothing — the same reason every
	// node-facing query carries its node id.
	if err := st.RevokeOperatorToken(testCtx(t), b.ID, tok.ID); !errors.Is(err, ErrNotFound) {
		t.Fatalf("cross-operator revoke must be ErrNotFound, got %v", err)
	}
	if _, err := st.OperatorForToken(testCtx(t), tok.Plaintext); err != nil {
		t.Fatalf("a's token must survive b's attempt: %v", err)
	}
	if err := st.RevokeOperatorToken(testCtx(t), a.ID, tok.ID); err != nil {
		t.Fatalf("owner revoke: %v", err)
	}
	if _, err := st.OperatorForToken(testCtx(t), tok.Plaintext); !errors.Is(err, ErrNotFound) {
		t.Fatalf("a revoked token must stop authenticating, got %v", err)
	}
}

// The resolvers are what let an id-addressed route find the org it must check
// membership against. Every one of them is a join the handler would otherwise
// have to write itself, which is exactly the boundary this package exists for.
func TestResolversNameTheOwningOrg(t *testing.T) {
	st := testStore(t)
	org := newOrg(t, st)
	app := newApp(t, st, org.ID)
	stack := newStack(t, st, app.ID)
	sv := newStackVersion(t, st, stack.ID)
	node := newNode(t, st, org.ID)
	env, err := st.CreateEnvironment(testCtx(t), CreateEnvironmentParams{StackID: stack.ID, Slug: "prod"})
	if err != nil {
		t.Fatalf("CreateEnvironment: %v", err)
	}
	dep, err := st.CreateDeployment(testCtx(t), CreateDeploymentParams{
		EnvironmentID: env.ID, StackVersionID: sv.ID, ResolvedSpec: sv.Spec, CreatedBy: "t",
	})
	if err != nil {
		t.Fatalf("CreateDeployment: %v", err)
	}

	for _, tc := range []struct {
		name string
		got  func() (uuid.UUID, error)
	}{
		{"app", func() (uuid.UUID, error) { return st.OrgForApp(testCtx(t), app.ID) }},
		{"stack", func() (uuid.UUID, error) { return st.OrgForStack(testCtx(t), stack.ID) }},
		{"stack version", func() (uuid.UUID, error) { return st.OrgForStackVersion(testCtx(t), sv.ID) }},
		{"environment", func() (uuid.UUID, error) { return st.OrgForEnvironment(testCtx(t), env.ID) }},
		{"deployment", func() (uuid.UUID, error) { return st.OrgForDeployment(testCtx(t), dep.ID) }},
		{"node", func() (uuid.UUID, error) { return st.OrgForNode(testCtx(t), node.ID) }},
	} {
		id, err := tc.got()
		if err != nil {
			t.Fatalf("resolve %s: %v", tc.name, err)
		}
		if id != org.ID {
			t.Fatalf("%s resolved to %s, want %s", tc.name, id, org.ID)
		}
	}

	// A missing object is ErrNotFound, which is what lets a handler answer 404
	// identically for "no such thing" and "not yours" — without that, the pair
	// of status codes is a cross-tenant existence oracle.
	missing := uuid.New()
	for _, tc := range []struct {
		name string
		got  func() (uuid.UUID, error)
	}{
		{"app", func() (uuid.UUID, error) { return st.OrgForApp(testCtx(t), missing) }},
		{"stack", func() (uuid.UUID, error) { return st.OrgForStack(testCtx(t), missing) }},
		{"stack version", func() (uuid.UUID, error) { return st.OrgForStackVersion(testCtx(t), missing) }},
		{"environment", func() (uuid.UUID, error) { return st.OrgForEnvironment(testCtx(t), missing) }},
		{"deployment", func() (uuid.UUID, error) { return st.OrgForDeployment(testCtx(t), missing) }},
		{"node", func() (uuid.UUID, error) { return st.OrgForNode(testCtx(t), missing) }},
		{"log request", func() (uuid.UUID, error) { return st.OrgForLogRequest(testCtx(t), missing) }},
	} {
		if _, err := tc.got(); !errors.Is(err, ErrNotFound) {
			t.Fatalf("resolving a missing %s must be ErrNotFound, got %v", tc.name, err)
		}
	}
}
