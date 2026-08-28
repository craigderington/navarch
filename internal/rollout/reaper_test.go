package rollout

import (
	"testing"
	"time"

	"github.com/google/uuid"

	"github.com/craigderington/navarch/internal/store"
)

// previewStack builds the org/app/stack/version a preview environment hangs
// off of, without fixture()'s node and "prod" environment/deployment, which
// CreatePreview doesn't need and would only be extra rows to clean up.
// Cleanup mirrors fixture()'s bottom-up order for the same reason: two FKs
// (deployments→stack_versions, and via cascading environments) require it.
func previewStack(t *testing.T, st *store.Store) uuid.UUID {
	t.Helper()
	slug := "reaper-" + uuid.NewString()[:8]
	org, err := st.CreateOrganization(ctx(t), slug, "Reaper")
	if err != nil {
		t.Fatalf("org: %v", err)
	}
	app, err := st.CreateApplication(ctx(t), org.ID, slug, "app")
	if err != nil {
		t.Fatalf("app: %v", err)
	}
	stack, err := st.CreateStack(ctx(t), app.ID, slug)
	if err != nil {
		t.Fatalf("stack: %v", err)
	}
	t.Cleanup(func() {
		c := ctx(t)
		// Environments cascade their deployments/instances on delete, so
		// dropping the org's environments first, then the org, is enough --
		// unlike fixture(), there's no separately-registered node here.
		st.Pool().Exec(c, `DELETE FROM environments WHERE stack_id IN (
			SELECT s.id FROM stacks s JOIN applications a ON s.app_id=a.id WHERE a.org_id=$1)`, org.ID)
		st.Pool().Exec(c, `DELETE FROM organizations WHERE id=$1`, org.ID)
	})
	return stack.ID
}

// newExpiredPreview creates a preview and ages it past its TTL directly in
// the database, the same "reach past expiry" trick previews_test.go uses --
// rollout tests can't reach store's unexported pool, so this goes through
// the already-exported st.Pool() instead of adding a new store method.
func newExpiredPreview(t *testing.T, st *store.Store) *store.Environment {
	t.Helper()
	env := newLivePreview(t, st, time.Hour)
	if _, err := st.Pool().Exec(ctx(t),
		`UPDATE environments SET expires_at = now() - interval '1 minute' WHERE id = $1`,
		env.ID); err != nil {
		t.Fatalf("age the preview: %v", err)
	}
	return env
}

// newLivePreview creates a preview whose TTL has not elapsed.
func newLivePreview(t *testing.T, st *store.Store, ttl time.Duration) *store.Environment {
	t.Helper()
	stackID := previewStack(t, st)
	sv, err := st.CreateStackVersion(ctx(t), stackID, "raw", fixtureSpec(), "t")
	if err != nil {
		t.Fatalf("stack version: %v", err)
	}
	env, _, err := st.CreatePreview(ctx(t), store.CreatePreviewParams{
		StackID: stackID, Slug: "pr-" + uuid.NewString()[:8],
		Hostname: uuid.NewString()[:8] + ".preview.localhost",
		TTL:      ttl, StackVersionID: sv.ID, ResolvedSpec: sv.Spec,
	})
	if err != nil {
		t.Fatalf("CreatePreview: %v", err)
	}
	return env
}

func previewOrg(t *testing.T, st *store.Store, envID uuid.UUID) uuid.UUID {
	t.Helper()
	var orgID uuid.UUID
	if err := st.Pool().QueryRow(ctx(t), `
		SELECT a.org_id FROM environments e
		JOIN stacks s ON s.id=e.stack_id
		JOIN applications a ON a.id=s.app_id
		WHERE e.id=$1`, envID).Scan(&orgID); err != nil {
		t.Fatalf("preview org: %v", err)
	}
	return orgID
}

func TestReapOnceDeletesExpiredPreviewAndTombstonesIt(t *testing.T) {
	st := testStore(t)
	env := newExpiredPreview(t, st)

	r := newReaperForOrg(st, discardLog(), previewOrg(t, st, env.ID))
	if err := r.ReapOnce(ctx(t)); err != nil {
		t.Fatalf("ReapOnce: %v", err)
	}

	if _, err := st.GetEnvironment(ctx(t), env.ID); err == nil {
		t.Error("expired preview must be gone after a reap tick")
	}
}

func TestReapOnceLeavesUnexpiredPreviewAlone(t *testing.T) {
	st := testStore(t)
	env := newLivePreview(t, st, time.Hour)

	r := newReaperForOrg(st, discardLog(), previewOrg(t, st, env.ID))
	if err := r.ReapOnce(ctx(t)); err != nil {
		t.Fatalf("ReapOnce: %v", err)
	}
	if _, err := st.GetEnvironment(ctx(t), env.ID); err != nil {
		t.Errorf("unexpired preview must survive a reap tick: %v", err)
	}
}

// fakeLogBuffer records what the reaper freed, so a test can assert the memory
// went — not merely that the rows did.
type fakeLogBuffer struct {
	dropped []uuid.UUID
	expired int
}

func (f *fakeLogBuffer) Drop(id uuid.UUID)               { f.dropped = append(f.dropped, id) }
func (f *fakeLogBuffer) Expire(maxAge time.Duration) int { f.expired++; return 0 }

// Sweeping the rows without freeing the buffers would leave container output —
// which may contain a tenant's secrets — alive in the control plane with nothing
// left pointing at it. That is the one thing this design promises not to do.
func TestReaperFreesLogBuffersItSweeps(t *testing.T) {
	st := testStore(t)
	ctx := ctx(t)
	dep, node, _ := reaperLogFixture(t, st)
	lr, err := st.CreateLogRequest(ctx, store.CreateLogRequestParams{
		EnvironmentID: dep.EnvironmentID, ServiceName: "api",
	})
	if err != nil {
		t.Fatalf("CreateLogRequest: %v", err)
	}
	_ = node
	if _, err := st.Pool().Exec(ctx,
		`UPDATE log_requests SET expires_at = now() - interval '1 minute' WHERE id=$1`, lr.ID); err != nil {
		t.Fatalf("backdate: %v", err)
	}

	buf := &fakeLogBuffer{}
	r := NewReaper(st, discardLog()).WithLogBuffer(buf)
	if err := r.ReapOnce(ctx); err != nil {
		t.Fatalf("ReapOnce: %v", err)
	}
	var freed bool
	for _, id := range buf.dropped {
		if id == lr.ID {
			freed = true
		}
	}
	if !freed {
		t.Fatalf("swept request %s left its buffer behind: dropped=%v", lr.ID, buf.dropped)
	}
	if buf.expired == 0 {
		t.Fatal("idle buffers must also be expired, or a requester that walked away leaks memory")
	}
}

// reaperLogFixture builds a deployment with one running instance so a log
// request has a container to resolve against.
func reaperLogFixture(t *testing.T, st *store.Store) (*store.Deployment, uuid.UUID, string) {
	t.Helper()
	depID, nodeID, orgID := fixture(t, st)
	// Place it: fixture leaves the deployment pending, so without this there are
	// no instances and therefore no container for a log request to resolve to.
	if err := newSchedulerForOrg(st, discardLog(), orgID).ScheduleOnce(ctx(t)); err != nil {
		t.Fatalf("ScheduleOnce: %v", err)
	}
	desired, err := st.DesiredStateForNode(ctx(t), nodeID)
	if err != nil || len(desired) == 0 {
		t.Fatalf("DesiredStateForNode: %v (%d)", err, len(desired))
	}
	cid := "c-" + uuid.NewString()[:8]
	for _, d := range desired {
		if d.ServiceName == "api" {
			if err := st.ReportInstance(ctx(t), nodeID, d.InstanceID, store.ObservedInstance{
				State: store.InstanceRunning, ContainerID: cid, SetStarted: true,
			}); err != nil {
				t.Fatalf("ReportInstance: %v", err)
			}
		}
	}
	dep, err := st.GetDeployment(ctx(t), depID)
	if err != nil {
		t.Fatalf("GetDeployment: %v", err)
	}
	return dep, nodeID, cid
}
