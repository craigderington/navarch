package store

import (
	"errors"
	"testing"
	"time"

	"github.com/google/uuid"
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

// Atomicity is the point of this task: a preview that existed with a
// hostname but no deployment would be an environment the reaper eventually
// collects and the user never sees work. A StackVersionID that doesn't
// exist makes the deployment insert's foreign key fail inside
// createDeploymentTx -- after the environment insert and the secret copy
// have already run in the same transaction -- so this exercises the
// rollback, not just an early bail-out.
func TestCreatePreviewRollsBackOnDeploymentFailure(t *testing.T) {
	st := testStore(t)
	ctx := testCtx(t)
	org := newOrg(t, st)
	app := newApp(t, st, org.ID)
	stack := newStack(t, st, app.ID)
	sv := newStackVersion(t, st, stack.ID)

	slug := "pr-rollback"
	_, _, err := st.CreatePreview(ctx, CreatePreviewParams{
		StackID: stack.ID, Slug: slug, Hostname: "pr-rollback-x.preview.localhost",
		TTL: time.Hour, StackVersionID: uuid.New(), ResolvedSpec: sv.Spec,
	})
	if err == nil {
		t.Fatal("CreatePreview must fail when the stack version does not exist")
	}

	if _, err := st.GetEnvironmentBySlug(ctx, stack.ID, slug); !errors.Is(err, ErrNotFound) {
		t.Fatalf("environment must not survive a rolled-back deployment insert, got %v", err)
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

// The handler generates the environment id so it can build a hostname
// containing env8 before the row exists. If the insert ignored the supplied id
// and let the column default win, the hostname would name an environment that
// does not exist and Traefik would route a name no container's labels match.
func TestCreatePreviewHonoursAnExplicitEnvironmentID(t *testing.T) {
	st := testStore(t)
	ctx := testCtx(t)
	org := newOrg(t, st)
	app := newApp(t, st, org.ID)
	stack := newStack(t, st, app.ID)
	sv := newStackVersion(t, st, stack.ID)

	want := uuid.New()
	env, _, err := st.CreatePreview(ctx, CreatePreviewParams{
		EnvironmentID: want,
		StackID:       stack.ID, Slug: "pr-explicit-id",
		Hostname: "pr-explicit-id-x-" + shortID(want) + ".preview.localhost",
		TTL:      time.Hour, StackVersionID: sv.ID, ResolvedSpec: sv.Spec,
	})
	if err != nil {
		t.Fatalf("CreatePreview: %v", err)
	}
	if env.ID != want {
		t.Fatalf("want environment id %s, got %s", want, env.ID)
	}
}

func TestExpireEnvironmentsReapsOnlyExpiredEphemerals(t *testing.T) {
	st := testStore(t)
	ctx := testCtx(t)
	org := newOrg(t, st)
	app := newApp(t, st, org.ID)
	stack := newStack(t, st, app.ID)
	sv := newStackVersion(t, st, stack.ID)

	prod, err := st.CreateEnvironment(ctx, CreateEnvironmentParams{StackID: stack.ID, Slug: "prod"})
	if err != nil {
		t.Fatalf("CreateEnvironment: %v", err)
	}
	fresh, _, err := st.CreatePreview(ctx, CreatePreviewParams{
		StackID: stack.ID, Slug: "pr-fresh", Hostname: "a.preview.localhost",
		TTL: time.Hour, StackVersionID: sv.ID, ResolvedSpec: sv.Spec,
	})
	if err != nil {
		t.Fatalf("CreatePreview fresh: %v", err)
	}
	stale, _, err := st.CreatePreview(ctx, CreatePreviewParams{
		StackID: stack.ID, Slug: "pr-stale", Hostname: "b.preview.localhost",
		TTL: time.Hour, StackVersionID: sv.ID, ResolvedSpec: sv.Spec,
	})
	if err != nil {
		t.Fatalf("CreatePreview stale: %v", err)
	}
	// Reach past the TTL rather than sleeping through it.
	if _, err := st.pool.Exec(ctx,
		`UPDATE environments SET expires_at = now() - interval '1 minute' WHERE id = $1`,
		stale.ID); err != nil {
		t.Fatalf("age the preview: %v", err)
	}

	reaped, err := st.ExpireEnvironments(ctx)
	if err != nil {
		t.Fatalf("ExpireEnvironments: %v", err)
	}
	// Containment and exclusion, not cardinality. ExpireEnvironments is
	// unscoped -- it reaps every expired ephemeral in the database -- and
	// three packages call it against the same DSN while `go test` runs their
	// binaries in parallel: this test, internal/rollout via ReapOnce, and
	// internal/api via newNodeWithReapedPreview. len(reaped)==1 therefore
	// fails whenever another package's victim was expired at the same moment.
	if !contains(reaped, shortID(stale.ID)) {
		// The race runs the other way too: another package's reaper can take
		// this victim in the window between the backdating UPDATE above and
		// this call. That is the same outcome reached by a different caller,
		// so accept it only on proof the environment really is gone -- a
		// genuine failure to reap still fails the test here.
		if _, err := st.GetEnvironment(ctx, stale.ID); err == nil {
			t.Fatalf("stale preview %s was neither reaped nor deleted, got %v", shortID(stale.ID), reaped)
		}
	}
	// Exclusion is race-free in both directions: no concurrent caller will
	// ever expire an environment whose TTL has not elapsed, or one that is
	// not ephemeral at all.
	if contains(reaped, shortID(fresh.ID)) {
		t.Fatalf("unexpired preview %s must not be reaped, got %v", shortID(fresh.ID), reaped)
	}
	if contains(reaped, shortID(prod.ID)) {
		t.Fatalf("non-ephemeral environment %s must not be reaped, got %v", shortID(prod.ID), reaped)
	}

	if _, err := st.GetEnvironment(ctx, stale.ID); err == nil {
		t.Error("expired preview must be deleted")
	}
	if _, err := st.GetEnvironment(ctx, fresh.ID); err != nil {
		t.Errorf("unexpired preview must survive: %v", err)
	}
	if _, err := st.GetEnvironment(ctx, prod.ID); err != nil {
		t.Errorf("non-ephemeral environment must survive: %v", err)
	}

	// The deployment created with the preview must have gone with it.
	var deps int
	if err := st.pool.QueryRow(ctx,
		`SELECT count(*) FROM deployments WHERE environment_id = $1`, stale.ID).Scan(&deps); err != nil {
		t.Fatalf("count deployments: %v", err)
	}
	if deps != 0 {
		t.Errorf("deleting the env must cascade its deployments, %d left", deps)
	}
}

// The tombstone must outlive the row it describes -- it is the only thing that
// will ever tell an agent to destroy that environment's durable state.
func TestTombstoneSurvivesTheEnvironmentAndExpires(t *testing.T) {
	st := testStore(t)
	ctx := testCtx(t)
	org := newOrg(t, st)
	app := newApp(t, st, org.ID)
	stack := newStack(t, st, app.ID)
	sv := newStackVersion(t, st, stack.ID)
	node := newNode(t, st, org.ID)

	env, _, err := st.CreatePreview(ctx, CreatePreviewParams{
		StackID: stack.ID, Slug: "pr-tomb", Hostname: "c.preview.localhost",
		TTL: time.Hour, StackVersionID: sv.ID, ResolvedSpec: sv.Spec,
	})
	if err != nil {
		t.Fatalf("CreatePreview: %v", err)
	}
	if _, err := st.pool.Exec(ctx,
		`UPDATE environments SET expires_at = now() - interval '1 minute' WHERE id = $1`,
		env.ID); err != nil {
		t.Fatalf("age the preview: %v", err)
	}
	if _, err := st.ExpireEnvironments(ctx); err != nil {
		t.Fatalf("ExpireEnvironments: %v", err)
	}

	got, err := st.TombstonesForNode(ctx, node.ID, 24*time.Hour)
	if err != nil {
		t.Fatalf("TombstonesForNode: %v", err)
	}
	if !contains(got, shortID(env.ID)) {
		t.Fatalf("want tombstone %s for this org's node, got %v", shortID(env.ID), got)
	}

	// Past the retention window it must disappear, so dead rows do not
	// accumulate forever.
	older, err := st.TombstonesForNode(ctx, node.ID, time.Nanosecond)
	if err != nil {
		t.Fatalf("TombstonesForNode (narrow window): %v", err)
	}
	if contains(older, shortID(env.ID)) {
		t.Error("tombstone outside the retention window must not be returned")
	}
}

func contains(hay []string, needle string) bool {
	for _, h := range hay {
		if h == needle {
			return true
		}
	}
	return false
}
