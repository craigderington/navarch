package rollout

import (
	"context"
	"strings"
	"testing"
	"time"

	"github.com/google/uuid"

	"github.com/craigderington/navarch/internal/mail"
	"github.com/craigderington/navarch/internal/store"
)

type fakeNotifier struct {
	sent []mail.Message
	err  error
}

func (f *fakeNotifier) Send(_ context.Context, m mail.Message) error {
	f.sent = append(f.sent, m)
	return f.err
}

// addOperator makes the org notifiable. The rollout fixtures create an
// organization with no members, which is a legitimate state — and one worth
// keeping in mind, because it means these paths send nothing at all unless a
// test says who to send to.
func addOperator(t *testing.T, st *store.Store, orgID uuid.UUID) string {
	t.Helper()
	email := "op-" + uuid.NewString()[:8] + "@example.com"
	op, err := st.CreateOperator(ctx(t), email, "Notified")
	if err != nil {
		t.Fatalf("CreateOperator: %v", err)
	}
	if err := st.AddOrgMember(ctx(t), orgID, op.ID, "admin"); err != nil {
		t.Fatalf("AddOrgMember: %v", err)
	}
	t.Cleanup(func() {
		c, cancel := context.WithTimeout(context.Background(), 10*time.Second)
		defer cancel()
		st.Pool().Exec(c, `DELETE FROM organization_members WHERE operator_id=$1`, op.ID)
		st.Pool().Exec(c, `DELETE FROM operators WHERE id=$1`, op.ID)
	})
	return email
}

// reportFailure fails the first instance with an agent-supplied error, the
// shape ReportInstance receives when EnsureImage fails on the node.
func reportFailure(t *testing.T, st *store.Store, nodeID uuid.UUID, lastError string) {
	t.Helper()
	desired, _ := st.DesiredStateForNode(ctx(t), nodeID)
	for i, d := range desired {
		obs := store.ObservedInstance{State: store.InstanceRunning, ContainerID: "c", HealthStatus: "healthy", SetStarted: true}
		if i == 0 {
			obs = store.ObservedInstance{State: store.InstanceFailed, LastError: lastError}
		}
		if err := st.ReportInstance(ctx(t), nodeID, d.InstanceID, obs); err != nil {
			t.Fatalf("report: %v", err)
		}
	}
}

// The reason is the whole point. failure_reason exists because the agent's
// description of what went wrong is deleted moments later with the instance
// rows; an email that said only "a rollout failed" would recreate the problem
// that invariant was written to solve.
func TestFailedRolloutEmailsTheReasonToTheOrgsOperators(t *testing.T) {
	st := testStore(t)
	depID, nodeID, orgID := fixture(t, st)
	email := addOperator(t, st, orgID)
	_ = newSchedulerForOrg(st, discardLog(), orgID).ScheduleOnce(ctx(t))
	reportFailure(t, st, nodeID, "image pull failed: not found")

	n := &fakeNotifier{}
	c := newControllerForOrg(st, discardLog(), nil, orgID)
	c.notify = n
	if err := c.ReconcileOnce(ctx(t)); err != nil {
		t.Fatalf("reconcile: %v", err)
	}

	if len(n.sent) != 1 {
		t.Fatalf("expected exactly one notification, got %d", len(n.sent))
	}
	m := n.sent[0]
	if len(m.To) != 1 || m.To[0] != email {
		t.Fatalf("recipients = %v, want [%s]", m.To, email)
	}
	if !strings.Contains(m.Body, "image pull failed: not found") {
		t.Fatalf("the agent's reason must reach the operator:\n%s", m.Body)
	}
	// The subject carries the slug path the CLI accepts, so it can be pasted
	// straight into a command rather than translated from a UUID.
	dep, _ := st.GetDeployment(ctx(t), depID)
	if !strings.Contains(m.Subject, "rollout failed") || !strings.Contains(m.Body, dep.ID.String()) {
		t.Fatalf("subject/body do not identify the deployment: %q\n%s", m.Subject, m.Body)
	}
	// An operator whose first thought is "did it roll back" must not have to
	// go and look.
	if !strings.Contains(m.Body, "still serving") {
		t.Fatalf("the message must say the previous revision is untouched:\n%s", m.Body)
	}
}

// Mail is a courtesy; the state transition is the record. A provider outage
// must not leave a deployment that failed sitting in `starting` forever, which
// is what returning the send error from advance() would do.
func TestRolloutStillFailsWhenTheProviderIsDown(t *testing.T) {
	st := testStore(t)
	depID, nodeID, orgID := fixture(t, st)
	addOperator(t, st, orgID)
	_ = newSchedulerForOrg(st, discardLog(), orgID).ScheduleOnce(ctx(t))
	reportFailure(t, st, nodeID, "exited")

	c := newControllerForOrg(st, discardLog(), nil, orgID)
	c.notify = &fakeNotifier{err: context.DeadlineExceeded}
	if err := c.ReconcileOnce(ctx(t)); err != nil {
		t.Fatalf("a mail failure must not fail the tick: %v", err)
	}
	dep, _ := st.GetDeployment(ctx(t), depID)
	if dep.State != store.DeployFailed {
		t.Fatalf("deployment must still be failed, got %s", dep.State)
	}
	if dep.FailureReason == "" {
		t.Fatal("the durable record of why must survive a mail failure")
	}
}

// The reaper ticks every second. Claim-and-mark is one statement precisely so
// this is a warning rather than a mail per second, and this is the test that
// would catch it being split back into a check and an update.
func TestExpiringPreviewIsWarnedExactlyOnce(t *testing.T) {
	st := testStore(t)
	env := newLivePreview(t, st, 30*time.Minute)
	orgID := previewOrg(t, st, env.ID)
	addOperator(t, st, orgID)

	n := &fakeNotifier{}
	r := newReaperForOrg(st, discardLog(), orgID).WithNotifier(n)
	for i := 0; i < 3; i++ {
		if err := r.ReapOnce(ctx(t)); err != nil {
			t.Fatalf("ReapOnce: %v", err)
		}
	}
	if len(n.sent) != 1 {
		t.Fatalf("expected 1 warning across 3 ticks, got %d", len(n.sent))
	}
	// Expiry destroys durable state. A message that only said "expires" would
	// let an operator assume the weaker meaning and not act in time.
	if !strings.Contains(n.sent[0].Body, "destroyed") || !strings.Contains(n.sent[0].Body, "not recoverable") {
		t.Fatalf("the warning must say what expiry actually does:\n%s", n.sent[0].Body)
	}
}

// A preview well inside its TTL is not news. Warning at creation for every
// preview is how a notification channel becomes one people filter.
func TestPreviewFarFromExpiryIsNotWarned(t *testing.T) {
	st := testStore(t)
	env := newLivePreview(t, st, 24*time.Hour)
	orgID := previewOrg(t, st, env.ID)
	addOperator(t, st, orgID)

	n := &fakeNotifier{}
	r := newReaperForOrg(st, discardLog(), orgID).WithNotifier(n)
	if err := r.ReapOnce(ctx(t)); err != nil {
		t.Fatalf("ReapOnce: %v", err)
	}
	if len(n.sent) != 0 {
		t.Fatalf("expected no warning 24h out, got %d", len(n.sent))
	}
}

// One already past its TTL is deleted on this same tick. Warning about
// something that is already gone is worse than saying nothing.
func TestAlreadyExpiredPreviewIsReapedNotWarned(t *testing.T) {
	st := testStore(t)
	env := newExpiredPreview(t, st)
	orgID := previewOrg(t, st, env.ID)
	addOperator(t, st, orgID)

	n := &fakeNotifier{}
	r := newReaperForOrg(st, discardLog(), orgID).WithNotifier(n)
	if err := r.ReapOnce(ctx(t)); err != nil {
		t.Fatalf("ReapOnce: %v", err)
	}
	if len(n.sent) != 0 {
		t.Fatalf("an expired preview must be reaped silently, got %d warnings", len(n.sent))
	}
	if _, err := st.GetEnvironment(ctx(t), env.ID); err == nil {
		t.Fatal("expired preview must be gone")
	}
}

// Without a notifier nothing is sent and nothing else changes. Mail is opt-in,
// and an install with none must behave exactly as it did before it existed.
func TestNoNotifierChangesNothing(t *testing.T) {
	st := testStore(t)
	depID, nodeID, orgID := fixture(t, st)
	addOperator(t, st, orgID)
	_ = newSchedulerForOrg(st, discardLog(), orgID).ScheduleOnce(ctx(t))
	reportFailure(t, st, nodeID, "exited")

	c := newControllerForOrg(st, discardLog(), nil, orgID)
	if err := c.ReconcileOnce(ctx(t)); err != nil {
		t.Fatalf("reconcile: %v", err)
	}
	if dep, _ := st.GetDeployment(ctx(t), depID); dep.State != store.DeployFailed {
		t.Fatalf("expected failed, got %s", dep.State)
	}
}
