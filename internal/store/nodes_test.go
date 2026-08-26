package store

import (
	"errors"
	"testing"

	"github.com/google/uuid"
)

func TestRegisterNodeIsUpsertByHostname(t *testing.T) {
	st := testStore(t)
	org := newOrg(t, st)
	p := RegisterNodeParams{
		OrgID: org.ID, Hostname: uniq("node"), AdvertiseAddr: "10.0.0.5",
		CPUMillis: 4000, MemoryBytes: 8 << 30, AgentVersion: "test",
	}
	n1, err := st.RegisterNode(testCtx(t), p)
	if err != nil {
		t.Fatalf("first register: %v", err)
	}
	if n1.State != NodeReady {
		t.Fatalf("expected a registered node to be ready, got %q", n1.State)
	}
	p.CPUMillis = 8000 // re-register same hostname with new capacity
	n2, err := st.RegisterNode(testCtx(t), p)
	if err != nil {
		t.Fatalf("re-register: %v", err)
	}
	if n2.ID != n1.ID {
		t.Fatalf("re-register must reuse the node row: %s vs %s", n1.ID, n2.ID)
	}
	if n2.CPUMillis != 8000 {
		t.Fatalf("capacity not updated on re-register: %d", n2.CPUMillis)
	}
}

func TestRegisterNodeIssuesTokenOnce(t *testing.T) {
	st := testStore(t)
	org := newOrg(t, st)
	p := RegisterNodeParams{
		OrgID: org.ID, Hostname: uniq("node"), AdvertiseAddr: "10.0.0.8",
		CPUMillis: 1000, MemoryBytes: 1 << 30,
	}
	n1, err := st.RegisterNode(testCtx(t), p)
	if err != nil {
		t.Fatalf("first register: %v", err)
	}
	if n1.Token == "" {
		t.Fatal("first register must return a node token")
	}
	ok, err := st.NodeTokenValid(testCtx(t), n1.ID, n1.Token)
	if err != nil || !ok {
		t.Fatalf("issued token should validate: ok=%v err=%v", ok, err)
	}
	n2, err := st.RegisterNode(testCtx(t), p)
	if err != nil {
		t.Fatalf("re-register: %v", err)
	}
	if n2.Token != "" {
		t.Fatal("re-register must not rotate or re-issue the node token")
	}
	ok, err = st.NodeTokenValid(testCtx(t), n1.ID, n1.Token)
	if err != nil || !ok {
		t.Fatal("original token must still validate after re-register")
	}
}

// Heartbeat had no coverage at all and shipped broken: its CASE resolved to
// text, which Postgres refuses to assign to the node_state enum, so every
// heartbeat 500'd. The agent path hid it behind an earlier 401.
func TestHeartbeatKeepsNodeReady(t *testing.T) {
	st := testStore(t)
	org := newOrg(t, st)
	n := newNode(t, st, org.ID)

	if err := st.Heartbeat(testCtx(t), n.ID, HeartbeatParams{}); err != nil {
		t.Fatalf("heartbeat: %v", err)
	}
	ready, err := st.ListReadyNodes(testCtx(t), org.ID)
	if err != nil {
		t.Fatalf("list: %v", err)
	}
	if len(ready) != 1 || ready[0].ID != n.ID {
		t.Fatalf("expected the heartbeating node to be ready, got %+v", ready)
	}
}

// The state machine the CASE exists to protect: a heartbeat proves liveness
// and must not un-drain a node the operator deliberately drained.
func TestHeartbeatDoesNotUndrainNode(t *testing.T) {
	st := testStore(t)
	org := newOrg(t, st)
	n := newNode(t, st, org.ID)

	if err := st.DrainNode(testCtx(t), n.ID); err != nil {
		t.Fatalf("drain: %v", err)
	}
	if err := st.Heartbeat(testCtx(t), n.ID, HeartbeatParams{}); err != nil {
		t.Fatalf("heartbeat: %v", err)
	}
	ready, err := st.ListReadyNodes(testCtx(t), org.ID)
	if err != nil {
		t.Fatalf("list: %v", err)
	}
	if len(ready) != 0 {
		t.Fatalf("heartbeat must not un-drain a node, got %+v", ready)
	}
}

func TestDrainNodeLeavesReadyPool(t *testing.T) {
	st := testStore(t)
	org := newOrg(t, st)
	n, err := st.RegisterNode(testCtx(t), RegisterNodeParams{
		OrgID: org.ID, Hostname: uniq("node"), AdvertiseAddr: "10.0.0.4",
		CPUMillis: 1000, MemoryBytes: 1 << 30,
	})
	if err != nil {
		t.Fatalf("register: %v", err)
	}
	if err := st.DrainNode(testCtx(t), n.ID); err != nil {
		t.Fatalf("drain: %v", err)
	}
	ready, err := st.ListReadyNodes(testCtx(t), org.ID)
	if err != nil {
		t.Fatalf("list: %v", err)
	}
	if len(ready) != 0 {
		t.Fatalf("draining node must not be ready, got %+v", ready)
	}
}

func TestListReadyNodesReturnsRegistered(t *testing.T) {
	st := testStore(t)
	org := newOrg(t, st)
	n, err := st.RegisterNode(testCtx(t), RegisterNodeParams{
		OrgID: org.ID, Hostname: uniq("node"), AdvertiseAddr: "10.0.0.6",
		CPUMillis: 1000, MemoryBytes: 1 << 30,
	})
	if err != nil {
		t.Fatalf("register: %v", err)
	}
	ready, err := st.ListReadyNodes(testCtx(t), org.ID)
	if err != nil {
		t.Fatalf("ListReadyNodes: %v", err)
	}
	if len(ready) != 1 || ready[0].ID != n.ID {
		t.Fatalf("expected the registered node, got %+v", ready)
	}
}

func TestRegisterNodeStoresRecipient(t *testing.T) {
	st := testStore(t)
	org := newOrg(t, st)
	n, err := st.RegisterNode(testCtx(t), RegisterNodeParams{
		OrgID: org.ID, Hostname: uniq("node"), AdvertiseAddr: "10.0.0.7",
		CPUMillis: 1000, MemoryBytes: 1 << 30, AgeRecipient: "age1exampletestrecipient",
	})
	if err != nil {
		t.Fatalf("register: %v", err)
	}
	if n.AgeRecipient != "age1exampletestrecipient" {
		t.Fatalf("recipient not returned: %q", n.AgeRecipient)
	}
	ready, _ := st.ListReadyNodes(testCtx(t), org.ID)
	if len(ready) != 1 || ready[0].AgeRecipient != "age1exampletestrecipient" {
		t.Fatalf("recipient not persisted: %+v", ready)
	}
}

func TestGetOrganizationBySlugUnknownIsNotFound(t *testing.T) {
	st := testStore(t)
	if _, err := st.GetOrganizationBySlug(testCtx(t), uniq("nope")); !errors.Is(err, ErrNotFound) {
		t.Fatalf("expected ErrNotFound, got %v", err)
	}
}

// A changing age_recipient is a credential redirect: every secret set after
// it is sealed to the new key. It must leave a trace in the org timeline —
// "why can this node read our secrets" deserves an answer that is not
// guesswork — while an unchanged recipient, a first registration, and a
// routine capacity refresh must not add noise.
func TestRegisterNodeRecipientRotationIsAudited(t *testing.T) {
	st := testStore(t)
	org := newOrg(t, st)
	host := uniq("node")
	reg := func(recipient string) *Node {
		t.Helper()
		n, err := st.RegisterNode(testCtx(t), RegisterNodeParams{
			OrgID: org.ID, Hostname: host, AdvertiseAddr: "10.0.0.7",
			CPUMillis: 1000, MemoryBytes: 1 << 30, AgeRecipient: recipient,
		})
		if err != nil {
			t.Fatalf("register: %v", err)
		}
		return n
	}

	countRotations := func() int {
		t.Helper()
		evs, err := st.ListEvents(testCtx(t), org.ID, 0, 200)
		if err != nil {
			t.Fatalf("ListEvents: %v", err)
		}
		n := 0
		for _, e := range evs {
			if e.Kind == "node.recipient_rotated" {
				n++
			}
		}
		return n
	}

	reg("age1first")            // first registration: no event
	reg("age1first")            // same recipient: no event
	reg("age1rotated")          // rotation: one event
	reg("age1rotated")          // unchanged again: no event
	if got := countRotations(); got != 1 {
		t.Fatalf("expected exactly 1 rotation event, got %d", got)
	}

	evs, _ := st.ListEvents(testCtx(t), org.ID, 0, 200)
	for _, e := range evs {
		if e.Kind == "node.recipient_rotated" && e.NodeID == nil {
			t.Fatal("rotation event must name the node it happened to")
		}
	}
}

// The regression this fixes: draining was a one-way door. DrainNode set
// `draining`, Heartbeat's CASE preserved it and RegisterNode's upsert preserved
// it too, so a drained node could not be returned to service by any means the
// product offered — reinstalling the agent did not clear it, and only hand-
// written SQL against a live database did.
func TestUncordonReturnsADrainedNodeToService(t *testing.T) {
	st := testStore(t)
	org := newOrg(t, st)
	n, err := st.RegisterNode(testCtx(t), RegisterNodeParams{
		OrgID: org.ID, Hostname: uniq("node"), AdvertiseAddr: "10.0.0.4",
		CPUMillis: 1000, MemoryBytes: 1 << 30,
	})
	if err != nil {
		t.Fatalf("register: %v", err)
	}
	if err := st.DrainNode(testCtx(t), n.ID); err != nil {
		t.Fatalf("drain: %v", err)
	}

	if err := st.UncordonNode(testCtx(t), n.ID); err != nil {
		t.Fatalf("uncordon: %v", err)
	}

	got, err := st.GetNode(testCtx(t), n.ID)
	if err != nil {
		t.Fatalf("get: %v", err)
	}
	// Registration set last_heartbeat=now(), so this node is provably live and
	// the derived state must be ready rather than unreachable.
	if got.State != NodeReady {
		t.Fatalf("a freshly-heartbeating node must come back ready, got %q", got.State)
	}
	ready, err := st.ListReadyNodes(testCtx(t), org.ID)
	if err != nil {
		t.Fatalf("list ready: %v", err)
	}
	// The state column alone is not the point — the node has to be schedulable
	// again, which is a different query with its own freshness filter.
	var found bool
	for _, r := range ready {
		if r.ID == n.ID {
			found = true
		}
	}
	if !found {
		t.Fatalf("an uncordoned node must be schedulable again, ready pool: %+v", ready)
	}
}

// Uncordon lifts an operator's intent not to schedule here; it does not assert
// the node is alive, and only the heartbeat knows that. A node drained long
// enough to go stale must come back `unreachable`, not `ready` — declaring
// otherwise would show a state in `navarch node list` that nothing has any
// evidence for.
func TestUncordonDerivesStateFromLastHeartbeat(t *testing.T) {
	st := testStore(t)
	org := newOrg(t, st)
	n, err := st.RegisterNode(testCtx(t), RegisterNodeParams{
		OrgID: org.ID, Hostname: uniq("node"), AdvertiseAddr: "10.0.0.4",
		CPUMillis: 1000, MemoryBytes: 1 << 30,
	})
	if err != nil {
		t.Fatalf("register: %v", err)
	}
	if err := st.DrainNode(testCtx(t), n.ID); err != nil {
		t.Fatalf("drain: %v", err)
	}
	// Backdate past the same 30s window MarkStaleNodesUnreachable uses.
	if _, err := st.pool.Exec(testCtx(t),
		`UPDATE nodes SET last_heartbeat = now() - interval '5 minutes' WHERE id=$1`, n.ID); err != nil {
		t.Fatalf("backdate heartbeat: %v", err)
	}

	if err := st.UncordonNode(testCtx(t), n.ID); err != nil {
		t.Fatalf("uncordon: %v", err)
	}

	got, err := st.GetNode(testCtx(t), n.ID)
	if err != nil {
		t.Fatalf("get: %v", err)
	}
	if got.State != NodeUnreachable {
		t.Fatalf("a silent node must come back unreachable, got %q", got.State)
	}
	// And it heals itself: the agent proving liveness is what makes it ready,
	// which is the same path any unreachable node takes.
	if err := st.Heartbeat(testCtx(t), n.ID, HeartbeatParams{}); err != nil {
		t.Fatalf("heartbeat: %v", err)
	}
	got, err = st.GetNode(testCtx(t), n.ID)
	if err != nil {
		t.Fatalf("get after heartbeat: %v", err)
	}
	if got.State != NodeReady {
		t.Fatalf("a heartbeat must promote an uncordoned node to ready, got %q", got.State)
	}
}

func TestUncordonEdgeCases(t *testing.T) {
	st := testStore(t)
	org := newOrg(t, st)
	newNodeFor := func() *Node {
		t.Helper()
		n, err := st.RegisterNode(testCtx(t), RegisterNodeParams{
			OrgID: org.ID, Hostname: uniq("node"), AdvertiseAddr: "10.0.0.4",
			CPUMillis: 1000, MemoryBytes: 1 << 30,
		})
		if err != nil {
			t.Fatalf("register: %v", err)
		}
		return n
	}

	t.Run("a node that is not draining is left alone", func(t *testing.T) {
		// Idempotent rather than an error: the caller asked for a node that is
		// not cordoned, and that is what they have. Reporting ErrNotFound here
		// would say "no such node" about a node plainly in the listing.
		n := newNodeFor()
		if err := st.UncordonNode(testCtx(t), n.ID); err != nil {
			t.Fatalf("uncordon of a ready node must succeed: %v", err)
		}
		got, _ := st.GetNode(testCtx(t), n.ID)
		if got.State != NodeReady {
			t.Fatalf("state changed unexpectedly: %q", got.State)
		}
	})

	t.Run("a retired node is refused", func(t *testing.T) {
		// Heartbeat will not touch a retired node, so resurrecting one would
		// leave a row claiming readiness that nothing can ever update again.
		n := newNodeFor()
		if _, err := st.pool.Exec(testCtx(t),
			`UPDATE nodes SET state='retired' WHERE id=$1`, n.ID); err != nil {
			t.Fatalf("retire: %v", err)
		}
		err := st.UncordonNode(testCtx(t), n.ID)
		if !errors.Is(err, ErrConflict) {
			t.Fatalf("expected ErrConflict for a retired node, got %v", err)
		}
	})

	t.Run("an unknown node is not found", func(t *testing.T) {
		err := st.UncordonNode(testCtx(t), uuid.New())
		if !errors.Is(err, ErrNotFound) {
			t.Fatalf("expected ErrNotFound, got %v", err)
		}
	})
}
