package store

import (
	"testing"
	"time"
)

// The CHECK constraint is the whole safety story for expiry: an ephemeral
// environment with no expiry is a leak the reaper can never see, so the
// database must refuse to store one rather than let it be detected later.
func TestEphemeralEnvironmentRequiresExpiry(t *testing.T) {
	st := testStore(t)
	ctx := testCtx(t)
	org := newOrg(t, st)
	app := newApp(t, st, org.ID)
	stack := newStack(t, st, app.ID)

	env, err := st.CreateEnvironment(ctx, CreateEnvironmentParams{
		StackID: stack.ID, Slug: uniq("pr"),
	})
	if err != nil {
		t.Fatalf("CreateEnvironment: %v", err)
	}

	_, err = st.pool.Exec(ctx,
		`UPDATE environments SET ephemeral = true, expires_at = NULL WHERE id = $1`, env.ID)
	if err == nil {
		t.Fatal("ephemeral environment with NULL expires_at must be rejected")
	}

	// The same update with an expiry must succeed, or the constraint is too broad.
	if _, err := st.pool.Exec(ctx,
		`UPDATE environments SET ephemeral = true, expires_at = $2 WHERE id = $1`,
		env.ID, time.Now().Add(time.Hour)); err != nil {
		t.Fatalf("ephemeral with expiry must be allowed: %v", err)
	}

	got, err := st.GetEnvironment(ctx, env.ID)
	if err != nil {
		t.Fatalf("GetEnvironment: %v", err)
	}
	if !got.Ephemeral {
		t.Error("GetEnvironment must surface ephemeral")
	}
	if got.ExpiresAt == nil {
		t.Error("GetEnvironment must surface expires_at")
	}
}
