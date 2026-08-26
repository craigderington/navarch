package store

import (
	"bytes"
	"testing"
	"time"
)

func TestSecretVersioningAndKeys(t *testing.T) {
	st := testStore(t)
	dep, _ := deployFixture(t, st)
	envID := dep.EnvironmentID
	if err := st.SetSecret(testCtx(t), envID, "db_password", []byte("ct-v1"), "age1x"); err != nil {
		t.Fatalf("set v1: %v", err)
	}
	if err := st.SetSecret(testCtx(t), envID, "db_password", []byte("ct-v2"), "age1x"); err != nil {
		t.Fatalf("set v2: %v", err)
	}
	metas, err := st.SecretKeysForEnv(testCtx(t), envID)
	if err != nil {
		t.Fatalf("keys: %v", err)
	}
	if len(metas) != 1 || metas[0].Key != "db_password" || metas[0].Version != 2 {
		t.Fatalf("expected db_password v2, got %+v", metas)
	}
}

func TestEncryptedSecretsForNode(t *testing.T) {
	st := testStore(t)
	dep, node := deployFixture(t, st)
	_ = st.CreateServiceInstances(testCtx(t), dep.ID, node.ID, []NewInstance{{ServiceName: "api", Swappable: true, ImageRef: "x"}})
	_ = st.UpdateDeploymentState(testCtx(t), dep.ID, DeployScheduling, "")
	_ = st.SetSecret(testCtx(t), dep.EnvironmentID, "db_password", []byte("cipher"), "age1x")

	byEnv, err := st.EncryptedSecretsForNode(testCtx(t), node.ID)
	if err != nil {
		t.Fatalf("EncryptedSecretsForNode: %v", err)
	}
	env8 := shortID(dep.EnvironmentID)
	secs := byEnv[env8]
	if len(secs) != 1 || secs[0].Key != "db_password" || !bytes.Equal(secs[0].Ciphertext, []byte("cipher")) {
		t.Fatalf("expected the ciphertext for env %s, got %+v", env8, byEnv)
	}
}

func TestDeleteSecret(t *testing.T) {
	st := testStore(t)
	dep, _ := deployFixture(t, st)
	_ = st.SetSecret(testCtx(t), dep.EnvironmentID, "k", []byte("c"), "age1x")
	if err := st.DeleteSecret(testCtx(t), dep.EnvironmentID, "k"); err != nil {
		t.Fatalf("delete: %v", err)
	}
	metas, _ := st.SecretKeysForEnv(testCtx(t), dep.EnvironmentID)
	if len(metas) != 0 {
		t.Fatalf("expected no keys after delete, got %+v", metas)
	}
}

// Pruning removes superseded versions past retention — old ciphertext stays
// sealed to the recipients live at write time, so an unbounded version
// history is an unbounded at-rest exposure — while the live version and a
// recently-superseded one both survive. A prune that could remove the newest
// version would break deployments, not just history.
func TestPruneSecretVersions(t *testing.T) {
	st := testStore(t)
	dep, _ := deployFixture(t, st)
	envID := dep.EnvironmentID
	ctx := testCtx(t)

	set := func(key, ct string) {
		t.Helper()
		if err := st.SetSecret(ctx, envID, key, []byte(ct), "age1x"); err != nil {
			t.Fatalf("set %s: %v", key, err)
		}
	}
	// rotate_me gets two versions; its v1 is old enough to prune.
	set("rotate_me", "ct-v1")
	if _, err := st.pool.Exec(ctx,
		`UPDATE secrets SET created_at = now() - interval '90 days'
		 WHERE environment_id=$1 AND key='rotate_me' AND version=1`, envID); err != nil {
		t.Fatalf("age v1: %v", err)
	}
	set("rotate_me", "ct-v2")
	// fresh_key gets two versions, both recent — nothing to prune.
	set("fresh_key", "ct-v1")
	set("fresh_key", "ct-v2")
	// single_key has only a live version — never pruned.
	set("single_key", "ct-v1")

	n, err := st.PruneSecretVersions(ctx, 30*24*time.Hour)
	if err != nil {
		t.Fatalf("prune: %v", err)
	}
	if n != 1 {
		t.Fatalf("expected exactly 1 pruned version, got %d", n)
	}

	// The live version of rotate_me survived; both fresh_key versions and
	// single_key are untouched.
	metas, _ := st.SecretKeysForEnv(ctx, envID)
	versions := map[string]int{}
	for _, m := range metas {
		versions[m.Key] = m.Version
	}
	if versions["rotate_me"] != 2 || versions["fresh_key"] != 2 || versions["single_key"] != 1 {
		t.Fatalf("live versions must survive the prune, got %+v", versions)
	}
	var freshV1 int
	if err := st.pool.QueryRow(ctx,
		`SELECT COUNT(*) FROM secrets WHERE environment_id=$1 AND key='fresh_key' AND version=1`, envID).
		Scan(&freshV1); err != nil || freshV1 != 1 {
		t.Fatalf("a recently-superseded version must survive, got %d (err %v)", freshV1, err)
	}
}
