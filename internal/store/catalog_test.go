package store

import (
	"context"
	"errors"
	"testing"
	"time"

	"github.com/google/uuid"
)

// ------------------------------------------------------------ organizations

func TestCreateOrganization(t *testing.T) {
	st := testStore(t)
	org := newOrg(t, st)

	if org.ID == uuid.Nil {
		t.Fatal("expected an assigned id")
	}
	if org.CreatedAt.IsZero() {
		t.Fatal("expected created_at to be set")
	}
}

func TestCreateOrganizationDuplicateSlugConflicts(t *testing.T) {
	st := testStore(t)
	org := newOrg(t, st)

	_, err := st.CreateOrganization(testCtx(t), org.Slug, "Other Name")
	if !errors.Is(err, ErrConflict) {
		t.Fatalf("expected ErrConflict for a duplicate slug, got %v", err)
	}
}

func TestListOrganizationsIncludesCreated(t *testing.T) {
	st := testStore(t)
	org := newOrg(t, st)

	orgs, err := st.ListOrganizations(testCtx(t))
	if err != nil {
		t.Fatalf("ListOrganizations: %v", err)
	}
	for _, o := range orgs {
		if o.ID == org.ID {
			return
		}
	}
	t.Fatalf("created org %s missing from list of %d", org.ID, len(orgs))
}

// ------------------------------------------------------------- applications

func TestCreateApplication(t *testing.T) {
	st := testStore(t)
	org := newOrg(t, st)
	app := newApp(t, st, org.ID)

	if app.OrgID != org.ID {
		t.Fatalf("expected org %s, got %s", org.ID, app.OrgID)
	}
}

// Creating a child of a row that doesn't exist is a client mistake, not a
// server fault: the foreign key violation must surface as ErrNotFound so
// the API answers 404 rather than 500.
func TestCreateApplicationUnknownOrgIsNotFound(t *testing.T) {
	st := testStore(t)

	_, err := st.CreateApplication(testCtx(t), uuid.New(), uniq("app"), "Orphan")
	if !errors.Is(err, ErrNotFound) {
		t.Fatalf("expected ErrNotFound for an unknown org, got %v", err)
	}
}

func TestCreateApplicationDuplicateSlugConflicts(t *testing.T) {
	st := testStore(t)
	org := newOrg(t, st)
	app := newApp(t, st, org.ID)

	_, err := st.CreateApplication(testCtx(t), org.ID, app.Slug, "Same Slug")
	if !errors.Is(err, ErrConflict) {
		t.Fatalf("expected ErrConflict, got %v", err)
	}
}

// The uniqueness constraint is per-org, so the same slug in a different org
// is legitimate.
func TestApplicationSlugIsUniquePerOrgOnly(t *testing.T) {
	st := testStore(t)
	orgA := newOrg(t, st)
	orgB := newOrg(t, st)

	app := newApp(t, st, orgA.ID)
	if _, err := st.CreateApplication(testCtx(t), orgB.ID, app.Slug, "Same Slug"); err != nil {
		t.Fatalf("same slug under a different org must be allowed, got %v", err)
	}
}

func TestListApplicationsScopedToOrg(t *testing.T) {
	st := testStore(t)
	orgA := newOrg(t, st)
	orgB := newOrg(t, st)
	appA := newApp(t, st, orgA.ID)
	newApp(t, st, orgB.ID)

	apps, err := st.ListApplications(testCtx(t), orgA.ID)
	if err != nil {
		t.Fatalf("ListApplications: %v", err)
	}
	if len(apps) != 1 || apps[0].ID != appA.ID {
		t.Fatalf("expected only %s, got %+v", appA.ID, apps)
	}
}

// ------------------------------------------------------------------ stacks

func TestCreateStack(t *testing.T) {
	st := testStore(t)
	org := newOrg(t, st)
	app := newApp(t, st, org.ID)
	stack := newStack(t, st, app.ID)

	if stack.AppID != app.ID {
		t.Fatalf("expected app %s, got %s", app.ID, stack.AppID)
	}
}

func TestCreateStackUnknownAppIsNotFound(t *testing.T) {
	st := testStore(t)

	_, err := st.CreateStack(testCtx(t), uuid.New(), uniq("stack"))
	if !errors.Is(err, ErrNotFound) {
		t.Fatalf("expected ErrNotFound for an unknown app, got %v", err)
	}
}

// ------------------------------------------------------------- environments

func TestCreateEnvironmentDefaultsToBlueGreen(t *testing.T) {
	st := testStore(t)
	org := newOrg(t, st)
	app := newApp(t, st, org.ID)
	stack := newStack(t, st, app.ID)

	env, err := st.CreateEnvironment(testCtx(t), CreateEnvironmentParams{
		StackID: stack.ID,
		Slug:    "prod",
	})
	if err != nil {
		t.Fatalf("CreateEnvironment: %v", err)
	}
	if env.Strategy != StrategyBlueGreen {
		t.Fatalf("expected the schema default %q, got %q", StrategyBlueGreen, env.Strategy)
	}
	if env.Config == nil {
		t.Fatal("expected a non-nil config map")
	}
	if env.LiveDeploymentID != nil {
		t.Fatal("a new environment has no live deployment")
	}
}

func TestCreateEnvironmentRoundTripsConfigAndHostname(t *testing.T) {
	st := testStore(t)
	org := newOrg(t, st)
	app := newApp(t, st, org.ID)
	stack := newStack(t, st, app.ID)

	want := map[string]string{"LOG_LEVEL": "debug", "REGION": "us-east-1"}
	env, err := st.CreateEnvironment(testCtx(t), CreateEnvironmentParams{
		StackID:  stack.ID,
		Slug:     "staging",
		Strategy: StrategyRecreate,
		Hostname: "staging.example.com",
		Config:   want,
	})
	if err != nil {
		t.Fatalf("CreateEnvironment: %v", err)
	}

	got, err := st.GetEnvironment(testCtx(t), env.ID)
	if err != nil {
		t.Fatalf("GetEnvironment: %v", err)
	}
	if got.Hostname != "staging.example.com" {
		t.Fatalf("hostname not persisted: %q", got.Hostname)
	}
	if got.Strategy != StrategyRecreate {
		t.Fatalf("strategy not persisted: %q", got.Strategy)
	}
	for k, v := range want {
		if got.Config[k] != v {
			t.Fatalf("config[%s] = %q, want %q", k, got.Config[k], v)
		}
	}
}

func TestCreateEnvironmentDuplicateSlugConflicts(t *testing.T) {
	st := testStore(t)
	org := newOrg(t, st)
	app := newApp(t, st, org.ID)
	stack := newStack(t, st, app.ID)

	p := CreateEnvironmentParams{StackID: stack.ID, Slug: "prod"}
	if _, err := st.CreateEnvironment(testCtx(t), p); err != nil {
		t.Fatalf("first create: %v", err)
	}
	if _, err := st.CreateEnvironment(testCtx(t), p); !errors.Is(err, ErrConflict) {
		t.Fatalf("expected ErrConflict, got %v", err)
	}
}

func TestCreateEnvironmentUnknownStackIsNotFound(t *testing.T) {
	st := testStore(t)

	_, err := st.CreateEnvironment(testCtx(t), CreateEnvironmentParams{
		StackID: uuid.New(),
		Slug:    "prod",
	})
	if !errors.Is(err, ErrNotFound) {
		t.Fatalf("expected ErrNotFound for an unknown stack, got %v", err)
	}
}

func TestListEnvironmentsScopedToStack(t *testing.T) {
	st := testStore(t)
	org := newOrg(t, st)
	app := newApp(t, st, org.ID)
	stackA := newStack(t, st, app.ID)
	stackB := newStack(t, st, app.ID)

	envA, err := st.CreateEnvironment(testCtx(t), CreateEnvironmentParams{StackID: stackA.ID, Slug: "prod"})
	if err != nil {
		t.Fatalf("CreateEnvironment: %v", err)
	}
	if _, err := st.CreateEnvironment(testCtx(t), CreateEnvironmentParams{StackID: stackB.ID, Slug: "prod"}); err != nil {
		t.Fatalf("CreateEnvironment: %v", err)
	}

	envs, err := st.ListEnvironments(testCtx(t), stackA.ID)
	if err != nil {
		t.Fatalf("ListEnvironments: %v", err)
	}
	if len(envs) != 1 || envs[0].ID != envA.ID {
		t.Fatalf("expected only %s, got %+v", envA.ID, envs)
	}
}

// --------------------------------------------------------- stack versions

func TestListStackVersionsNewestFirst(t *testing.T) {
	st := testStore(t)
	org := newOrg(t, st)
	app := newApp(t, st, org.ID)
	stack := newStack(t, st, app.ID)

	// Two distinct specs, so the digest dedupe in CreateStackVersion does
	// not collapse them into one row.
	first := specWithImage(t, "nginx:1.25")
	second := specWithImage(t, "nginx:1.27")

	v1, err := st.CreateStackVersion(testCtx(t), stack.ID, "raw-1", first, "tester")
	if err != nil {
		t.Fatalf("CreateStackVersion: %v", err)
	}
	v2, err := st.CreateStackVersion(testCtx(t), stack.ID, "raw-2", second, "tester")
	if err != nil {
		t.Fatalf("CreateStackVersion: %v", err)
	}
	if v1.Version != 1 || v2.Version != 2 {
		t.Fatalf("expected versions 1 and 2, got %d and %d", v1.Version, v2.Version)
	}

	versions, err := st.ListStackVersions(testCtx(t), stack.ID, 0)
	if err != nil {
		t.Fatalf("ListStackVersions: %v", err)
	}
	if len(versions) != 2 {
		t.Fatalf("expected 2 versions, got %d", len(versions))
	}
	if versions[0].ID != v2.ID {
		t.Fatalf("expected newest first, got version %d first", versions[0].Version)
	}
}

// An unchanged stack must not manufacture revision churn.
func TestCreateStackVersionDedupesIdenticalSpec(t *testing.T) {
	st := testStore(t)
	org := newOrg(t, st)
	app := newApp(t, st, org.ID)
	stack := newStack(t, st, app.ID)

	s := specWithImage(t, "nginx:1.25")
	v1, err := st.CreateStackVersion(testCtx(t), stack.ID, "raw", s, "tester")
	if err != nil {
		t.Fatalf("first: %v", err)
	}
	v2, err := st.CreateStackVersion(testCtx(t), stack.ID, "raw", s, "tester")
	if err != nil {
		t.Fatalf("second: %v", err)
	}
	if v1.ID != v2.ID {
		t.Fatalf("identical spec produced a new version: %s then %s", v1.ID, v2.ID)
	}
}

// ------------------------------------------------------------ slug validation

// Slugs reach URLs and project names, so the format rule belongs in the
// store rather than being re-implemented by every caller.
func TestCreateOrganizationRejectsBadSlug(t *testing.T) {
	st := testStore(t)
	for _, slug := range []string{"", "  ", "Has Space", "UPPER", "trailing-", "-leading", "sym$bol"} {
		if _, err := st.CreateOrganization(testCtx(t), slug, "Name"); !errors.Is(err, ErrInvalid) {
			t.Errorf("slug %q: expected ErrInvalid, got %v", slug, err)
		}
	}
}

func TestCreateOrganizationAcceptsConventionalSlugs(t *testing.T) {
	st := testStore(t)
	for _, slug := range []string{uniq("acme"), uniq("a1"), uniq("multi-part-name")} {
		org, err := st.CreateOrganization(testCtx(t), slug, "Name")
		if err != nil {
			t.Errorf("slug %q: unexpected error %v", slug, err)
			continue
		}
		t.Cleanup(func() {
			ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
			defer cancel()
			st.pool.Exec(ctx, `DELETE FROM organizations WHERE id = $1`, org.ID)
		})
	}
}

func TestCreateEnvironmentRejectsBadSlug(t *testing.T) {
	st := testStore(t)
	org := newOrg(t, st)
	app := newApp(t, st, org.ID)
	stack := newStack(t, st, app.ID)

	_, err := st.CreateEnvironment(testCtx(t), CreateEnvironmentParams{StackID: stack.ID, Slug: "Not A Slug"})
	if !errors.Is(err, ErrInvalid) {
		t.Fatalf("expected ErrInvalid, got %v", err)
	}
}
