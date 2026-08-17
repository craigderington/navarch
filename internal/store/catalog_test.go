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
		Strategy: StrategyBlueGreen,
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
	if got.Strategy != StrategyBlueGreen {
		t.Fatalf("strategy not persisted: %q", got.Strategy)
	}
	for k, v := range want {
		if got.Config[k] != v {
			t.Fatalf("config[%s] = %q, want %q", k, got.Config[k], v)
		}
	}
}

func TestValidateHostname(t *testing.T) {
	if err := validateHostname("staging.example.com"); err != nil {
		t.Fatalf("valid hostname rejected: %v", err)
	}
	if err := validateHostname(""); err != nil {
		t.Fatalf("empty hostname must be allowed: %v", err)
	}
	for _, h := range []string{
		"evil.com`) || Host(`x.com",
		"foo.com\ninjected: true",
		"NOT-LOWERCASE.com",
		"foo_bar.com",
		"-leading.com",
		"trailing-.com",
		"has space.com",
	} {
		if err := validateHostname(h); err == nil {
			t.Errorf("hostname %q should be rejected", h)
		}
	}
}

func TestCreateEnvironmentRejectsUnsupportedStrategies(t *testing.T) {
	st := testStore(t)
	org := newOrg(t, st)
	app := newApp(t, st, org.ID)
	stack := newStack(t, st, app.ID)

	for _, strategy := range []RolloutStrategy{StrategyRolling, StrategyRecreate, "surprise"} {
		t.Run(string(strategy), func(t *testing.T) {
			_, err := st.CreateEnvironment(testCtx(t), CreateEnvironmentParams{
				StackID: stack.ID, Slug: "strategy-test", Strategy: strategy,
			})
			if !errors.Is(err, ErrInvalid) {
				t.Fatalf("got %v, want ErrInvalid", err)
			}
		})
	}
}

func TestCreateEnvironmentDuplicateHostnameConflicts(t *testing.T) {
	st := testStore(t)
	org := newOrg(t, st)
	app := newApp(t, st, org.ID)
	stack := newStack(t, st, app.ID)

	if _, err := st.CreateEnvironment(testCtx(t), CreateEnvironmentParams{
		StackID: stack.ID, Slug: "one", Hostname: "shared.example.com",
	}); err != nil {
		t.Fatalf("first create: %v", err)
	}
	_, err := st.CreateEnvironment(testCtx(t), CreateEnvironmentParams{
		StackID: stack.ID, Slug: "two", Hostname: "shared.example.com",
	})
	if !errors.Is(err, ErrConflict) {
		t.Fatalf("expected ErrConflict for duplicate hostname, got %v", err)
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

// The tenant boundary for GET /v1/orgs/{org}/environments. An environment
// carries no org id — it reaches one only through stacks and applications — so
// the JOIN in ListOrgEnvironments *is* the isolation. Widening it hands one
// tenant another's catalog: environment slugs, ingress hostnames, and which
// node runs them. This codebase has been bitten by cross-tenant leakage before
// (see the preview-hostname section of CLAUDE.md for why env8 exists), so the
// scoping gets a test that fails if the WHERE clause stops discriminating.
func TestListOrgEnvironmentsIsScopedToItsOrg(t *testing.T) {
	st := testStore(t)
	ctx := testCtx(t)

	mine := envInOrg(t, st, "mine")
	theirs := envInOrg(t, st, "theirs")

	got, err := st.ListOrgEnvironments(ctx, mine.orgID)
	if err != nil {
		t.Fatalf("ListOrgEnvironments: %v", err)
	}
	var sawMine bool
	for _, e := range got {
		if e.ID == theirs.envID {
			t.Fatalf("environment %s belongs to another organization and must not be listed", e.ID)
		}
		if e.ID == mine.envID {
			sawMine = true
		}
	}
	if !sawMine {
		t.Fatalf("own environment %s missing from %d rows", mine.envID, len(got))
	}
	// Asserted both ways round: a query returning everything would pass the
	// "mine is present" half on its own.
	for _, e := range got {
		if e.AppSlug == "" || e.StackSlug == "" {
			t.Fatalf("row %s missing its catalog path: %+v", e.ID, e)
		}
	}
}

// An environment is bound to a node by its FIRST placement, so most of the
// catalog is unplaced most of the time. The join that resolves the hostname
// must be LEFT: an inner join would drop every never-deployed environment from
// the listing, which is a far worse failure than a blank column because the
// rows simply vanish.
func TestListOrgEnvironmentsIncludesUnplacedAndResolvesHostnames(t *testing.T) {
	st := testStore(t)
	ctx := testCtx(t)

	f := envInOrg(t, st, "homed")
	unplaced, err := st.CreateEnvironment(ctx, CreateEnvironmentParams{StackID: f.stackID, Slug: "unplaced"})
	if err != nil {
		t.Fatalf("CreateEnvironment: %v", err)
	}

	node := newNode(t, st, f.orgID)
	if _, err := st.Pool().Exec(ctx,
		`UPDATE environments SET home_node_id=$2 WHERE id=$1`, f.envID, node.ID); err != nil {
		t.Fatalf("home the environment: %v", err)
	}

	got, err := st.ListOrgEnvironments(ctx, f.orgID)
	if err != nil {
		t.Fatalf("ListOrgEnvironments: %v", err)
	}
	var homed, blank *OrgEnvironment
	for i := range got {
		switch got[i].ID {
		case f.envID:
			homed = &got[i]
		case unplaced.ID:
			blank = &got[i]
		}
	}
	if homed == nil || blank == nil {
		t.Fatalf("expected both environments listed, got %d rows", len(got))
	}
	if homed.HomeNode != node.Hostname {
		t.Fatalf("home node hostname = %q, want %q", homed.HomeNode, node.Hostname)
	}
	if blank.HomeNode != "" || blank.HomeNodeID != nil {
		t.Fatalf("an unplaced environment must report no node, got %+v", blank)
	}
}

// GetEnvironment and ListEnvironments resolve the hostname too — the CLI's
// `env get` and `env list` read through those, not through the org listing.
func TestEnvironmentReadsResolveHomeNodeHostname(t *testing.T) {
	st := testStore(t)
	ctx := testCtx(t)
	f := envInOrg(t, st, "reads")
	node := newNode(t, st, f.orgID)
	if _, err := st.Pool().Exec(ctx,
		`UPDATE environments SET home_node_id=$2 WHERE id=$1`, f.envID, node.ID); err != nil {
		t.Fatalf("home the environment: %v", err)
	}

	one, err := st.GetEnvironment(ctx, f.envID)
	if err != nil {
		t.Fatalf("GetEnvironment: %v", err)
	}
	if one.HomeNode != node.Hostname {
		t.Fatalf("GetEnvironment home node = %q, want %q", one.HomeNode, node.Hostname)
	}

	list, err := st.ListEnvironments(ctx, f.stackID)
	if err != nil {
		t.Fatalf("ListEnvironments: %v", err)
	}
	if len(list) == 0 || list[0].HomeNode != node.Hostname {
		t.Fatalf("ListEnvironments home node = %+v, want %q", list, node.Hostname)
	}
}

type orgEnvFixture struct {
	orgID   uuid.UUID
	stackID uuid.UUID
	envID   uuid.UUID
}

// envInOrg builds org → app → stack → env, the whole chain the org scoping has
// to traverse. Two of these in one test are two tenants.
func envInOrg(t *testing.T, st *Store, label string) orgEnvFixture {
	t.Helper()
	ctx := testCtx(t)
	org := newOrg(t, st)
	app := newApp(t, st, org.ID)
	stack := newStack(t, st, app.ID)
	env, err := st.CreateEnvironment(ctx, CreateEnvironmentParams{StackID: stack.ID, Slug: label})
	if err != nil {
		t.Fatalf("CreateEnvironment(%s): %v", label, err)
	}
	return orgEnvFixture{orgID: org.ID, stackID: stack.ID, envID: env.ID}
}
