package store

import (
	"context"
	"os"
	"testing"
	"time"

	"github.com/google/uuid"

	"github.com/craig/composectl/internal/spec"
)

// devDSN is the local dev stack from compose.yaml. Postgres is mandatory —
// there is no SQLite fallback, by design — so these tests talk to a real
// database and skip loudly when one is not up.
const devDSN = "postgres://composectl:composectl@localhost:5473/composectl?sslmode=disable"

func testStore(t *testing.T) *Store {
	t.Helper()
	dsn := os.Getenv("COMPOSECTL_TEST_DATABASE_URL")
	if dsn == "" {
		dsn = devDSN
	}
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()

	st, err := New(ctx, dsn)
	if err != nil {
		t.Skipf("postgres unreachable at %s — run `make up` (or set COMPOSECTL_TEST_DATABASE_URL): %v", dsn, err)
	}
	t.Cleanup(st.Close)
	return st
}

func testCtx(t *testing.T) context.Context {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	t.Cleanup(cancel)
	return ctx
}

// uniq builds a slug unique to this test run so parallel or repeated runs
// don't collide on the schema's UNIQUE constraints.
func uniq(prefix string) string {
	return prefix + "-" + uuid.NewString()[:8]
}

// newOrg creates a throwaway organization and deletes it — along with
// everything that cascades from it — when the test ends.
func newOrg(t *testing.T, st *Store) *Organization {
	t.Helper()
	ctx := testCtx(t)
	org, err := st.CreateOrganization(ctx, uniq("org"), "Test Org")
	if err != nil {
		t.Fatalf("CreateOrganization: %v", err)
	}
	t.Cleanup(func() {
		cleanupCtx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
		defer cancel()
		// environments.live_deployment_id is deferrable but still checked at
		// commit; clear it so the cascade has nothing dangling to trip on.
		if _, err := st.pool.Exec(cleanupCtx, `
			UPDATE environments SET live_deployment_id = NULL
			WHERE stack_id IN (
				SELECT s.id FROM stacks s
				JOIN applications a ON s.app_id = a.id
				WHERE a.org_id = $1
			)`, org.ID); err != nil {
			t.Errorf("cleanup (clear live deployment): %v", err)
		}
		if _, err := st.pool.Exec(cleanupCtx, `DELETE FROM organizations WHERE id = $1`, org.ID); err != nil {
			t.Errorf("cleanup (delete org): %v", err)
		}
	})
	return org
}

func newApp(t *testing.T, st *Store, orgID uuid.UUID) *Application {
	t.Helper()
	app, err := st.CreateApplication(testCtx(t), orgID, uniq("app"), "Test App")
	if err != nil {
		t.Fatalf("CreateApplication: %v", err)
	}
	return app
}

// specWithImage builds a minimal spec whose digest varies with the image,
// so tests can produce deliberately-distinct stack versions.
func specWithImage(t *testing.T, image string) *spec.DeploymentSpec {
	t.Helper()
	return &spec.DeploymentSpec{
		SpecVersion: spec.SpecVersion,
		Services: map[string]spec.Service{
			"app": {
				Name:      "app",
				Image:     image,
				Swappable: true,
				Limits:    spec.ResourceLimit{CPUMillis: 250, MemoryBytes: 256 << 20},
			},
		},
	}
}

func newStack(t *testing.T, st *Store, appID uuid.UUID) *Stack {
	t.Helper()
	stack, err := st.CreateStack(testCtx(t), appID, uniq("stack"))
	if err != nil {
		t.Fatalf("CreateStack: %v", err)
	}
	return stack
}
