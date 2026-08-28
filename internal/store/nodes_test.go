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

// A node may propose an age key; it may not assign one. Every secret set after
// a recipient change is sealed to the new key, and anyone holding the shared
// service token can register a node — so accepting the change on the word of
// whoever registered is a credential redirect. Auditing it was the previous
// pass and records the redirect after the fact; this refuses it until a human
// approves.
func TestARegisteringNodeCannotChangeItsOwnRecipient(t *testing.T) {
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
	countKind := func(kind string) int {
		t.Helper()
		evs, err := st.ListEvents(testCtx(t), org.ID, 0, 200)
		if err != nil {
			t.Fatalf("ListEvents: %v", err)
		}
		n := 0
		for _, e := range evs {
			if e.Kind == kind {
				n++
			}
		}
		return n
	}

	// First registration: nothing is displaced, so it takes effect directly.
	n := reg("age1first")
	if n.AgeRecipient != "age1first" || n.PendingAgeRecipient != "" {
		t.Fatalf("first registration must set the recipient outright: %+v", n)
	}

	// The same key again is not a change.
	n = reg("age1first")
	if n.AgeRecipient != "age1first" || n.PendingAgeRecipient != "" {
		t.Fatalf("an unchanged recipient must not become pending: %+v", n)
	}

	// A different key is a request, not an assignment. This is the assertion
	// the whole slice exists for.
	n = reg("age1attacker")
	if n.AgeRecipient != "age1first" {
		t.Fatalf("a re-registration changed the effective recipient to %q", n.AgeRecipient)
	}
	if n.PendingAgeRecipient != "age1attacker" {
		t.Fatalf("the proposed recipient must be recorded as pending, got %q", n.PendingAgeRecipient)
	}
	if got := countKind("node.recipient_rotation_pending"); got != 1 {
		t.Fatalf("expected exactly 1 pending event, got %d", got)
	}

	// A crashlooping agent re-registers on every restart. The same request
	// repeated must not bury the timeline.
	reg("age1attacker")
	if got := countKind("node.recipient_rotation_pending"); got != 1 {
		t.Fatalf("a repeated proposal must not append a second event, got %d", got)
	}

	// An empty recipient must not erase what we know. Writing it through would
	// let any agent revoke its own key by failing to read a file.
	n = reg("")
	if n.AgeRecipient != "age1first" {
		t.Fatalf("an empty recipient erased the recorded one: %q", n.AgeRecipient)
	}
	if n.PendingAgeRecipient != "age1attacker" {
		t.Fatalf("an empty recipient dropped the pending one: %q", n.PendingAgeRecipient)
	}

	// Advertising the original key again withdraws the request.
	n = reg("age1first")
	if n.PendingAgeRecipient != "" {
		t.Fatalf("re-advertising the effective key must clear pending, got %q", n.PendingAgeRecipient)
	}

	evs, _ := st.ListEvents(testCtx(t), org.ID, 0, 200)
	for _, e := range evs {
		if e.Kind == "node.recipient_rotation_pending" && e.NodeID == nil {
			t.Fatal("a pending-rotation event must name the node it happened to")
		}
	}
}

// The operator half: promotion is the only thing that changes an effective
// recipient, and it is what `node.recipient_rotated` now means.
func TestRotateNodeRecipientPromotesOnlyWhatIsPending(t *testing.T) {
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
	n := reg("age1old")

	// Nothing pending is a conflict, not a quiet success: a rotation that
	// reports "done" without changing anything is the ambiguity this removes.
	if _, err := st.RotateNodeRecipient(testCtx(t), n.ID); !errors.Is(err, ErrConflict) {
		t.Fatalf("rotating with nothing pending must be ErrConflict, got %v", err)
	}
	if _, err := st.RotateNodeRecipient(testCtx(t), uuid.New()); !errors.Is(err, ErrNotFound) {
		t.Fatalf("rotating an unknown node must be ErrNotFound, got %v", err)
	}

	reg("age1new")
	got, err := st.RotateNodeRecipient(testCtx(t), n.ID)
	if err != nil {
		t.Fatalf("rotate: %v", err)
	}
	if got.AgeRecipient != "age1new" || got.PendingAgeRecipient != "" {
		t.Fatalf("promotion should have moved pending into effect: %+v", got)
	}

	// Promotion is what the rotated event means now, and rotating twice is not
	// a second promotion.
	evs, _ := st.ListEvents(testCtx(t), org.ID, 0, 200)
	rotated := 0
	for _, e := range evs {
		if e.Kind == "node.recipient_rotated" {
			rotated++
		}
	}
	if rotated != 1 {
		t.Fatalf("expected exactly 1 rotated event, got %d", rotated)
	}
	if _, err := st.RotateNodeRecipient(testCtx(t), n.ID); !errors.Is(err, ErrConflict) {
		t.Fatalf("a second rotate must be ErrConflict, got %v", err)
	}
}

// Nothing is ever sealed to a key nobody approved — which is the entire point
// of the pending state, and the one place it could quietly leak back in.
func TestSecretsAreNeverSealedToAPendingRecipient(t *testing.T) {
	st := testStore(t)
	org := newOrg(t, st)
	app := newApp(t, st, org.ID)
	stack := newStack(t, st, app.ID)
	env, err := st.CreateEnvironment(testCtx(t), CreateEnvironmentParams{StackID: stack.ID, Slug: "prod"})
	if err != nil {
		t.Fatalf("CreateEnvironment: %v", err)
	}
	host := uniq("node")
	for _, r := range []string{"age1approved", "age1pending"} {
		if _, err := st.RegisterNode(testCtx(t), RegisterNodeParams{
			OrgID: org.ID, Hostname: host, AdvertiseAddr: "10.0.0.7",
			CPUMillis: 4000, MemoryBytes: 8 << 30, AgeRecipient: r,
		}); err != nil {
			t.Fatalf("register %s: %v", r, err)
		}
	}

	// RecipientsForEnvironment has three paths — home node, nodes already
	// running the environment, then every ready node — and they read the
	// recipient in different places. Checking only the fallback would leave the
	// primary one free to leak, so both are asserted.
	check := func(why string) {
		t.Helper()
		recipients, err := st.RecipientsForEnvironment(testCtx(t), env.ID)
		if err != nil {
			t.Fatalf("RecipientsForEnvironment (%s): %v", why, err)
		}
		var sawApproved bool
		for _, r := range recipients {
			if r == "age1pending" {
				t.Fatalf("a pending recipient was used to seal a secret (%s): %v", why, recipients)
			}
			if r == "age1approved" {
				sawApproved = true
			}
		}
		if !sawApproved {
			t.Fatalf("the approved recipient should still be sealed to (%s), got %v", why, recipients)
		}
	}

	// No home node yet: the ready-node fallback.
	check("unbound environment")

	// Bound: the home-node path, which is the one a deployed environment uses.
	nodes, err := st.ListNodes(testCtx(t), org.ID)
	if err != nil || len(nodes) == 0 {
		t.Fatalf("ListNodes: %v (%d)", err, len(nodes))
	}
	if _, err := st.Pool().Exec(testCtx(t),
		`UPDATE environments SET home_node_id=$2 WHERE id=$1`, env.ID, nodes[0].ID); err != nil {
		t.Fatalf("bind home node: %v", err)
	}
	check("homed environment")
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
