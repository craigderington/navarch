package store

import (
	"errors"
	"testing"

	"github.com/google/uuid"
)

// logFixture builds a placed, running instance so a log request has a real
// container to resolve to, and returns the deployment, node and container id.
func logFixture(t *testing.T, st *Store) (dep *Deployment, node *Node, containerID string) {
	t.Helper()
	dep, node = deployFixture(t, st)
	if err := st.CreateServiceInstances(testCtx(t), dep.ID, node.ID, []NewInstance{
		{ServiceName: "api", Swappable: true, ImageRef: "nginx:alpine"},
	}); err != nil {
		t.Fatalf("CreateServiceInstances: %v", err)
	}
	if err := st.UpdateDeploymentState(testCtx(t), dep.ID, DeployScheduling, ""); err != nil {
		t.Fatalf("advance: %v", err)
	}
	desired, _ := st.DesiredStateForNode(testCtx(t), node.ID)
	if len(desired) == 0 {
		t.Fatal("fixture produced no desired instance")
	}
	containerID = "c-" + uuid.NewString()[:8]
	if err := st.ReportInstance(testCtx(t), node.ID, desired[0].InstanceID, ObservedInstance{
		State: InstanceRunning, ContainerID: containerID, HealthStatus: "healthy", SetStarted: true,
	}); err != nil {
		t.Fatalf("ReportInstance: %v", err)
	}
	return dep, node, containerID
}

func envIDOf(t *testing.T, st *Store, depID uuid.UUID) uuid.UUID {
	t.Helper()
	d, err := st.GetDeployment(testCtx(t), depID)
	if err != nil {
		t.Fatalf("GetDeployment: %v", err)
	}
	return d.EnvironmentID
}

// The control plane resolves a service name to a container, and the agent is
// only ever handed an id for a container on its own node. This is what stops the
// endpoint being a way to read any container anywhere.
func TestCreateLogRequestResolvesTheContainerForTheRightNode(t *testing.T) {
	st := testStore(t)
	dep, node, containerID := logFixture(t, st)

	lr, err := st.CreateLogRequest(testCtx(t), CreateLogRequestParams{
		EnvironmentID: envIDOf(t, st, dep.ID), ServiceName: "api", TailLines: 25,
	})
	if err != nil {
		t.Fatalf("CreateLogRequest: %v", err)
	}
	if lr.State != LogPending || lr.TailLines != 25 {
		t.Fatalf("unexpected request: %+v", lr)
	}

	pending, err := st.LogRequestsForNode(testCtx(t), node.ID)
	if err != nil {
		t.Fatalf("LogRequestsForNode: %v", err)
	}
	var found *PendingLogRequest
	for i := range pending {
		if pending[i].ID == lr.ID {
			found = &pending[i]
		}
	}
	if found == nil {
		t.Fatalf("request not offered to the node that runs the container")
	}
	if found.ContainerID != containerID {
		t.Fatalf("agent must be told the container id, got %q want %q", found.ContainerID, containerID)
	}

	// And no other node is offered it.
	other := newNode(t, st, orgOfNode(t, st, node.ID))
	otherPending, err := st.LogRequestsForNode(testCtx(t), other.ID)
	if err != nil {
		t.Fatalf("LogRequestsForNode (other): %v", err)
	}
	for _, p := range otherPending {
		if p.ID == lr.ID {
			t.Fatal("a node was offered a log request for a container it does not run")
		}
	}
}

func orgOfNode(t *testing.T, st *Store, nodeID uuid.UUID) uuid.UUID {
	t.Helper()
	n, err := st.GetNode(testCtx(t), nodeID)
	if err != nil {
		t.Fatalf("GetNode: %v", err)
	}
	return n.OrgID
}

// "No such service" and "not started yet" need different reactions from the
// operator — fix the name, versus wait — so they must not collapse into one
// error.
func TestCreateLogRequestDistinguishesUnknownFromNotStarted(t *testing.T) {
	st := testStore(t)
	dep, node := deployFixture(t, st)
	envID := envIDOf(t, st, dep.ID)

	if _, err := st.CreateLogRequest(testCtx(t), CreateLogRequestParams{
		EnvironmentID: envID, ServiceName: "nosuch",
	}); !errors.Is(err, ErrNotFound) {
		t.Fatalf("unknown service should be ErrNotFound, got %v", err)
	}

	// Placed but with no container reported yet.
	_ = st.CreateServiceInstances(testCtx(t), dep.ID, node.ID, []NewInstance{
		{ServiceName: "api", Swappable: true, ImageRef: "nginx:alpine"},
	})
	_ = st.UpdateDeploymentState(testCtx(t), dep.ID, DeployScheduling, "")
	if _, err := st.CreateLogRequest(testCtx(t), CreateLogRequestParams{
		EnvironmentID: envID, ServiceName: "api",
	}); !errors.Is(err, ErrConflict) {
		t.Fatalf("not-yet-started service should be ErrConflict, got %v", err)
	}
}

// Bounds are enforced in the store, not the handler: the agent acts on whatever
// the row says and has no way to know a number is unreasonable.
func TestCreateLogRequestBoundsTail(t *testing.T) {
	st := testStore(t)
	dep, _, _ := logFixture(t, st)
	envID := envIDOf(t, st, dep.ID)

	if _, err := st.CreateLogRequest(testCtx(t), CreateLogRequestParams{
		EnvironmentID: envID, ServiceName: "api", TailLines: MaxLogTailLines + 1,
	}); !errors.Is(err, ErrInvalid) {
		t.Fatalf("oversized tail should be ErrInvalid, got %v", err)
	}
	lr, err := st.CreateLogRequest(testCtx(t), CreateLogRequestParams{
		EnvironmentID: envID, ServiceName: "api",
	})
	if err != nil {
		t.Fatalf("CreateLogRequest: %v", err)
	}
	if lr.TailLines != DefaultLogTailLines {
		t.Fatalf("unspecified tail should default, got %d", lr.TailLines)
	}
}

// A following request must stay pending so one row serves a whole tail session;
// a one-shot must not, or its node reads Docker forever for nobody.
func TestCompleteLogRequestFollowStaysPendingOneShotDoesNot(t *testing.T) {
	st := testStore(t)
	dep, node, _ := logFixture(t, st)
	envID := envIDOf(t, st, dep.ID)

	follow, err := st.CreateLogRequest(testCtx(t), CreateLogRequestParams{
		EnvironmentID: envID, ServiceName: "api", Follow: true,
	})
	if err != nil {
		t.Fatalf("CreateLogRequest(follow): %v", err)
	}
	if err := st.CompleteLogRequest(testCtx(t), node.ID, follow.ID, ""); err != nil {
		t.Fatalf("CompleteLogRequest: %v", err)
	}
	got, _ := st.GetLogRequest(testCtx(t), follow.ID)
	if got.State != LogPending {
		t.Fatalf("a followed request must return to pending, got %s", got.State)
	}
	// since_at advanced, so the next tick asks only for what is new. Visible
	// here through the pending row the node is offered.
	pending, _ := st.LogRequestsForNode(testCtx(t), node.ID)
	for _, p := range pending {
		if p.ID == follow.ID && p.SinceAt == nil {
			t.Fatal("following must advance since_at, or every tick re-reads the whole tail")
		}
	}

	one, err := st.CreateLogRequest(testCtx(t), CreateLogRequestParams{
		EnvironmentID: envID, ServiceName: "api",
	})
	if err != nil {
		t.Fatalf("CreateLogRequest(one-shot): %v", err)
	}
	if err := st.CompleteLogRequest(testCtx(t), node.ID, one.ID, ""); err != nil {
		t.Fatalf("CompleteLogRequest: %v", err)
	}
	got, _ = st.GetLogRequest(testCtx(t), one.ID)
	if got.State != LogDone {
		t.Fatalf("a one-shot request must finish, got %s", got.State)
	}
}

// An agent may only answer for containers on its own node — the same rule that
// scopes instance reports.
func TestCompleteLogRequestRejectsAnotherNode(t *testing.T) {
	st := testStore(t)
	dep, node, _ := logFixture(t, st)
	lr, err := st.CreateLogRequest(testCtx(t), CreateLogRequestParams{
		EnvironmentID: envIDOf(t, st, dep.ID), ServiceName: "api",
	})
	if err != nil {
		t.Fatalf("CreateLogRequest: %v", err)
	}
	impostor := newNode(t, st, orgOfNode(t, st, node.ID))
	if err := st.CompleteLogRequest(testCtx(t), impostor.ID, lr.ID, ""); !errors.Is(err, ErrNotFound) {
		t.Fatalf("another node completing a request should be ErrNotFound, got %v", err)
	}
	got, _ := st.GetLogRequest(testCtx(t), lr.ID)
	if got.State != LogPending {
		t.Fatalf("the request must be untouched, got %s", got.State)
	}
}

// The ownership check the API gates buffered content on. It answers exactly
// what CompleteLogRequest enforces — this node's containers only — without
// mutating anything, and a request that never existed is simply not owned
// rather than an error: the caller treats "not ours" and "vanished" the same.
func TestLogRequestOwnedByNode(t *testing.T) {
	st := testStore(t)
	dep, node, _ := logFixture(t, st)
	lr, err := st.CreateLogRequest(testCtx(t), CreateLogRequestParams{
		EnvironmentID: envIDOf(t, st, dep.ID), ServiceName: "api",
	})
	if err != nil {
		t.Fatalf("CreateLogRequest: %v", err)
	}
	owned, err := st.LogRequestOwnedByNode(testCtx(t), node.ID, lr.ID)
	if err != nil || !owned {
		t.Fatalf("the owning node should own it, got owned=%v err=%v", owned, err)
	}
	impostor := newNode(t, st, orgOfNode(t, st, node.ID))
	owned, err = st.LogRequestOwnedByNode(testCtx(t), impostor.ID, lr.ID)
	if err != nil || owned {
		t.Fatalf("a foreign node must not own it, got owned=%v err=%v", owned, err)
	}
	owned, err = st.LogRequestOwnedByNode(testCtx(t), node.ID, uuid.New())
	if err != nil || owned {
		t.Fatalf("a request that never existed is not owned, got owned=%v err=%v", owned, err)
	}
	// The check must not touch the row.
	got, _ := st.GetLogRequest(testCtx(t), lr.ID)
	if got.State != LogPending {
		t.Fatalf("ownership check must not mutate the request, got %s", got.State)
	}
}

// A failure has to be recorded and terminal: retrying forever against a
// container that has gone would keep the node reading Docker for nothing.
func TestCompleteLogRequestRecordsFailure(t *testing.T) {
	st := testStore(t)
	dep, node, _ := logFixture(t, st)
	lr, _ := st.CreateLogRequest(testCtx(t), CreateLogRequestParams{
		EnvironmentID: envIDOf(t, st, dep.ID), ServiceName: "api", Follow: true,
	})
	if err := st.CompleteLogRequest(testCtx(t), node.ID, lr.ID, "No such container"); err != nil {
		t.Fatalf("CompleteLogRequest: %v", err)
	}
	got, _ := st.GetLogRequest(testCtx(t), lr.ID)
	if got.State != LogFailed {
		t.Fatalf("expected failed even for a follow, got %s", got.State)
	}
	if got.LastError == "" {
		t.Fatal("the reason must survive, or the requester cannot tell why it stopped")
	}
}

// The sweep must return the ids it deleted. A sweep that only dropped rows would
// leave the delivered content — possibly containing secrets — alive in memory
// with nothing left pointing at it.
func TestSweepLogRequestsReturnsIDsForBufferRelease(t *testing.T) {
	st := testStore(t)
	dep, _, _ := logFixture(t, st)
	lr, err := st.CreateLogRequest(testCtx(t), CreateLogRequestParams{
		EnvironmentID: envIDOf(t, st, dep.ID), ServiceName: "api",
	})
	if err != nil {
		t.Fatalf("CreateLogRequest: %v", err)
	}
	if _, err := st.Pool().Exec(testCtx(t),
		`UPDATE log_requests SET expires_at = now() - interval '1 minute' WHERE id=$1`, lr.ID); err != nil {
		t.Fatalf("backdate: %v", err)
	}
	swept, err := st.SweepLogRequests(testCtx(t))
	if err != nil {
		t.Fatalf("SweepLogRequests: %v", err)
	}
	var found bool
	for _, id := range swept {
		if id == lr.ID {
			found = true
		}
	}
	if !found {
		t.Fatalf("expired request %s not returned by the sweep: %v", lr.ID, swept)
	}
	if _, err := st.GetLogRequest(testCtx(t), lr.ID); !errors.Is(err, ErrNotFound) {
		t.Fatalf("expired request should be gone, got %v", err)
	}
}

// An expired request must not be handed to a node. Otherwise a tail whose
// requester left keeps a node reading Docker until the sweep happens to run.
func TestExpiredLogRequestIsNotOffered(t *testing.T) {
	st := testStore(t)
	dep, node, _ := logFixture(t, st)
	lr, _ := st.CreateLogRequest(testCtx(t), CreateLogRequestParams{
		EnvironmentID: envIDOf(t, st, dep.ID), ServiceName: "api", Follow: true,
	})
	if _, err := st.Pool().Exec(testCtx(t),
		`UPDATE log_requests SET expires_at = now() - interval '1 second' WHERE id=$1`, lr.ID); err != nil {
		t.Fatalf("backdate: %v", err)
	}
	pending, _ := st.LogRequestsForNode(testCtx(t), node.ID)
	for _, p := range pending {
		if p.ID == lr.ID {
			t.Fatal("an expired request must not be offered to a node")
		}
	}
}
