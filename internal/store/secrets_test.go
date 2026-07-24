package store

import (
	"bytes"
	"testing"
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
