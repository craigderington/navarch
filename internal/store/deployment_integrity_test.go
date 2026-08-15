package store

import (
	"errors"
	"testing"
	"time"
)

func TestCreateDeploymentRejectsVersionFromAnotherStack(t *testing.T) {
	st := testStore(t)
	org := newOrg(t, st)
	app := newApp(t, st, org.ID)
	targetStack := newStack(t, st, app.ID)
	foreignStack := newStack(t, st, app.ID)
	foreignVersion := newStackVersion(t, st, foreignStack.ID)
	env, err := st.CreateEnvironment(testCtx(t), CreateEnvironmentParams{
		StackID: targetStack.ID, Slug: uniq("prod"),
	})
	if err != nil {
		t.Fatalf("CreateEnvironment: %v", err)
	}

	_, err = st.CreateDeployment(testCtx(t), CreateDeploymentParams{
		EnvironmentID: env.ID, StackVersionID: foreignVersion.ID,
		ResolvedSpec: foreignVersion.Spec, CreatedBy: "test",
	})
	if !errors.Is(err, ErrInvalid) {
		t.Fatalf("foreign stack version: got %v, want ErrInvalid", err)
	}
}

func TestCreateDeploymentRejectsVersionFromAnotherOrganization(t *testing.T) {
	st := testStore(t)
	targetOrg := newOrg(t, st)
	foreignOrg := newOrg(t, st)
	targetStack := newStack(t, st, newApp(t, st, targetOrg.ID).ID)
	foreignStack := newStack(t, st, newApp(t, st, foreignOrg.ID).ID)
	foreignVersion := newStackVersion(t, st, foreignStack.ID)
	env, err := st.CreateEnvironment(testCtx(t), CreateEnvironmentParams{
		StackID: targetStack.ID, Slug: uniq("prod"),
	})
	if err != nil {
		t.Fatalf("CreateEnvironment: %v", err)
	}

	_, err = st.CreateDeployment(testCtx(t), CreateDeploymentParams{
		EnvironmentID: env.ID, StackVersionID: foreignVersion.ID,
		ResolvedSpec: foreignVersion.Spec, CreatedBy: "test",
	})
	if !errors.Is(err, ErrInvalid) {
		t.Fatalf("foreign organization version: got %v, want ErrInvalid", err)
	}
}

func TestCreatePreviewRejectsVersionFromAnotherStackAtomically(t *testing.T) {
	st := testStore(t)
	org := newOrg(t, st)
	app := newApp(t, st, org.ID)
	targetStack := newStack(t, st, app.ID)
	foreignStack := newStack(t, st, app.ID)
	foreignVersion := newStackVersion(t, st, foreignStack.ID)

	_, _, err := st.CreatePreview(testCtx(t), CreatePreviewParams{
		StackID: targetStack.ID, Slug: uniq("preview"),
		Hostname: "foreign.preview.localhost", TTL: time.Hour,
		StackVersionID: foreignVersion.ID, ResolvedSpec: foreignVersion.Spec,
		CreatedBy: "test",
	})
	if !errors.Is(err, ErrInvalid) {
		t.Fatalf("foreign stack version: got %v, want ErrInvalid", err)
	}

	var count int
	if err := st.pool.QueryRow(testCtx(t),
		`SELECT count(*) FROM environments WHERE stack_id=$1`, targetStack.ID).Scan(&count); err != nil {
		t.Fatalf("count environments: %v", err)
	}
	if count != 0 {
		t.Fatalf("rejected preview must roll back its environment, found %d", count)
	}
}

func TestDatabaseRejectsMismatchedDeploymentStack(t *testing.T) {
	st := testStore(t)
	org := newOrg(t, st)
	app := newApp(t, st, org.ID)
	targetStack := newStack(t, st, app.ID)
	foreignStack := newStack(t, st, app.ID)
	foreignVersion := newStackVersion(t, st, foreignStack.ID)
	env, err := st.CreateEnvironment(testCtx(t), CreateEnvironmentParams{
		StackID: targetStack.ID, Slug: uniq("prod"),
	})
	if err != nil {
		t.Fatalf("CreateEnvironment: %v", err)
	}

	_, err = st.pool.Exec(testCtx(t), `
		INSERT INTO deployments (
			environment_id, stack_version_id, stack_id, revision, slot,
			project_name, resolved_spec
		) VALUES ($1, $2, $3, 1, 'blue', $4, '{}'::jsonb)
	`, env.ID, foreignVersion.ID, targetStack.ID, "integrity-"+uniq("project"))
	if !isForeignKeyViolation(err) {
		t.Fatalf("database accepted mismatched stack relationship: %v", err)
	}
}
