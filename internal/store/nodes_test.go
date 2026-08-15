package store

import (
	"errors"
	"testing"
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
