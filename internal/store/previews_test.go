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

// Inheritance copies ciphertext only. This is safe because a secret is sealed
// to node age recipients, never to an environment, so a copied row is
// decryptable by exactly the same agents as the original -- and the control
// plane still never holds plaintext.
func TestCreatePreviewCopiesLatestSecretsOnly(t *testing.T) {
	st := testStore(t)
	ctx := testCtx(t)
	org := newOrg(t, st)
	app := newApp(t, st, org.ID)
	stack := newStack(t, st, app.ID)

	src, err := st.CreateEnvironment(ctx, CreateEnvironmentParams{StackID: stack.ID, Slug: "staging"})
	if err != nil {
		t.Fatalf("CreateEnvironment: %v", err)
	}
	if err := st.SetSecret(ctx, src.ID, "db_password", []byte("ct-v1"), "age1src"); err != nil {
		t.Fatalf("SetSecret v1: %v", err)
	}
	if err := st.SetSecret(ctx, src.ID, "db_password", []byte("ct-v2"), "age1src"); err != nil {
		t.Fatalf("SetSecret v2: %v", err)
	}
	if err := st.SetSecret(ctx, src.ID, "api_key", []byte("ct-key"), "age1src"); err != nil {
		t.Fatalf("SetSecret api_key: %v", err)
	}

	sv := newStackVersion(t, st, stack.ID)
	env, dep, err := st.CreatePreview(ctx, CreatePreviewParams{
		StackID: stack.ID, Slug: "pr-142", Hostname: "pr-142-x.preview.localhost",
		TTL: time.Hour, InheritSecretsFrom: &src.ID,
		StackVersionID: sv.ID, ResolvedSpec: sv.Spec,
	})
	if err != nil {
		t.Fatalf("CreatePreview: %v", err)
	}
	if !env.Ephemeral || env.ExpiresAt == nil {
		t.Fatal("preview must be ephemeral with an expiry")
	}
	if dep.Revision != 1 || dep.Slot != "blue" {
		t.Errorf("first deployment must be r1/blue, got r%d/%s", dep.Revision, dep.Slot)
	}

	keys, err := st.SecretKeysForEnv(ctx, env.ID)
	if err != nil {
		t.Fatalf("SecretKeysForEnv: %v", err)
	}
	if len(keys) != 2 {
		t.Fatalf("want 2 inherited keys, got %d: %+v", len(keys), keys)
	}
	for _, k := range keys {
		// Copies start a fresh history: version is per (environment_id, key)
		// and the new environment has none.
		if k.Version != 1 {
			t.Errorf("%s: copied secret must be version 1, got %d", k.Key, k.Version)
		}
	}

	var ct []byte
	if err := st.pool.QueryRow(ctx,
		`SELECT ciphertext FROM secrets WHERE environment_id=$1 AND key='db_password'`,
		env.ID).Scan(&ct); err != nil {
		t.Fatalf("read copied ciphertext: %v", err)
	}
	if string(ct) != "ct-v2" {
		t.Errorf("must copy the latest version, got %q", ct)
	}
}

func TestCreatePreviewWithoutInheritanceHasNoSecrets(t *testing.T) {
	st := testStore(t)
	ctx := testCtx(t)
	org := newOrg(t, st)
	app := newApp(t, st, org.ID)
	stack := newStack(t, st, app.ID)
	sv := newStackVersion(t, st, stack.ID)

	env, _, err := st.CreatePreview(ctx, CreatePreviewParams{
		StackID: stack.ID, Slug: "pr-1", Hostname: "pr-1-x.preview.localhost",
		TTL: time.Hour, StackVersionID: sv.ID, ResolvedSpec: sv.Spec,
	})
	if err != nil {
		t.Fatalf("CreatePreview: %v", err)
	}
	keys, err := st.SecretKeysForEnv(ctx, env.ID)
	if err != nil {
		t.Fatalf("SecretKeysForEnv: %v", err)
	}
	if len(keys) != 0 {
		t.Errorf("want no secrets without inheritance, got %+v", keys)
	}
}
