# Sprint 2 Slice A — Reconciliation Spine Implementation Plan

> **For agentic workers:** REQUIRED SUB-SKILL: Use superpowers:subagent-driven-development (recommended) or superpowers:executing-plans to implement this plan task-by-task. Steps use checkbox (`- [ ]`) syntax for tracking.

**Goal:** A node agent drives a real Docker daemon to bring a stack up per revision, a control-plane scheduler places it, and a rollout controller health-gates it — so a deployment reaches `healthy` on its own, with no SQL fakery.

**Architecture:** Control plane runs two background loops (scheduler: `pending→scheduling` + writes desired `service_instances`; controller: aggregates instance health, drives `scheduling→starting→healthy`, tears down on failure/supersede). A separate `cmd/agent` binary polls the control-plane HTTP API for its node's desired state, reconciles the local Docker daemon, and reports observed state back. The agent never touches Postgres — it speaks only HTTP — so the "only `internal/store` imports pgx" boundary holds across binaries.

**Tech Stack:** Go 1.23, Postgres (pgx), Docker Engine API (`github.com/docker/docker/client`), `net/http` (stdlib routing).

## Global Constraints

- **Go directive stays `go 1.23`.** Do not let `go mod tidy` raise it (a modern local toolchain will, to satisfy the test-only `rogpeppe/go-internal`; it is pinned to v1.14.1). After any `go get`, verify `grep '^go ' go.mod` still says `1.23` or the Docker image build breaks.
- **Boundaries are load-bearing.** Only `internal/store` imports pgx. Only `internal/agent/dockerd` imports the Docker SDK. Only `internal/parser` imports compose-go. Handlers decode/delegate/encode. The agent imports neither pgx nor compose-go.
- **Postgres always** — no SQLite, even in tests. Store/rollout tests skip loudly when Postgres is unreachable (mirror the existing `testStore` skip).
- **Docker tests skip loudly** when no daemon is reachable, same pattern.
- **Ports:** never bind 3000/5000/8000/9000. Postgres `5473`, API `8417`.
- **Never push to git.** Commit locally only.
- **Migrations immutable** — Slice A needs none; if one becomes necessary, add `0002_*`, never edit `0001`.
- **Project naming:** swappable containers `cc-{env8}-r{rev}-{slot}-{service}`; pinned containers `cc-{env8}-pinned-{service}`; revision network `cc-{env8}-r{rev}-{slot}`; named volumes `cc-{env8}-{volume}`. `env8` = first 8 hex chars of the environment UUID (`store.shortID`).
- **Container labels** (so the agent can find and GC what it manages): `cc.env`, `cc.deployment`, `cc.service`, `cc.swappable`, `cc.pinned` (pinned key, pinned only).

## Prerequisite: git

This repo is **not yet a git repository**, so the `git commit` steps below cannot run as written. Before Task 1, initialize it locally (never pushed, per the ground rules):

```bash
cd /home/cd/Work/quartermaster
git init
printf 'bin/\n' > .gitignore
git add -A && git commit -m "chore: initial commit of Sprint 1 state"
```

If you prefer no git, skip every `git commit` step; nothing else depends on them.

## Deviations from the spec (read before starting)

1. **Agent polls, not NOTIFY.** The spec's "NOTIFY `node_{id}` + resync" would require the agent to `LISTEN` on Postgres, importing pgx and breaking the boundary. Slice A has the agent poll `GET /desired-state` on a ticker (default 2s). The `service_instances_notify` trigger stays in place for a future control-plane push endpoint (Sprint 5).
2. **Node capacity from config, not host probing.** Avoids a new dependency. `COMPOSECTL_NODE_CPU_MILLIS` (default `runtime.NumCPU()*1000`) and `COMPOSECTL_NODE_MEMORY_BYTES` (default `8<<30`).
3. **Desired state excludes terminal deployments.** `DesiredStateForNode` returns instances only for deployments in `scheduling|starting|healthy|live`. A superseded/failed deployment's containers thus become orphans the agent GCs immediately, independent of the controller's row cleanup.

---

## File Structure

**Create:**
- `internal/store/nodes.go` — node registration, heartbeat, ready-node queries
- `internal/store/instances.go` — desired-instance writes, desired-state read, observed reports, aggregation, teardown
- `internal/store/nodes_test.go`, `internal/store/instances_test.go`
- `internal/agent/dockerd/driver.go` — Docker SDK boundary: image/network/container ops
- `internal/agent/dockerd/driver_test.go` — integration, skips without Docker
- `internal/agent/reconcile.go` — pure reconcile logic over a `DockerDriver` interface
- `internal/agent/reconcile_test.go` — unit, fake driver
- `internal/agent/agent.go` — the loop: register, poll, reconcile, report, heartbeat
- `internal/agent/secrets.go` — dev secret source (static map / env)
- `internal/rollout/scheduler.go` — `pending→scheduling`, writes instances, capacity check
- `internal/rollout/controller.go` — health aggregation, `scheduling→starting→healthy`, failure + supersede teardown
- `internal/rollout/rollout_test.go` — against real Postgres
- `cmd/agent/main.go` — agent entrypoint
- `internal/agent/config.go` — agent config loader (package `agent`, so the Docker SDK stays out of the control-plane binary)

**Modify:**
- `internal/store/store.go` — add `GetOrganizationBySlug` (agent registers by org slug)
- `internal/store/deployments.go` — add `ListPendingDeployments`, `ListRolloutsInState`
- `internal/api/nodes.go` — replace 501 stubs with real handlers (register/heartbeat/desired-state/report/list)
- `internal/api/server.go` — bootstrap the `dev` org; no route changes (routes already registered)
- `cmd/controlplane/main.go` — start scheduler + controller goroutines
- `internal/config/config.go` — add scheduler/controller tick interval
- `compose.yaml` — add the `agent` service (docker.sock mount) + `dev` org note
- `scripts/demo.sh` — retire the SQL fakery; add a failure demo
- `Makefile` — `make demo` already exists; add `make agent-logs`

---

## Task 1: Node store methods

**Files:**
- Create: `internal/store/nodes.go`, `internal/store/nodes_test.go`
- Modify: `internal/store/store.go` (add `GetOrganizationBySlug`)

**Interfaces:**
- Consumes: existing `Store`, `mapErr`, `Node`, `NodeReady`, test helpers `testStore`/`testCtx`/`newOrg`/`uniq` from Sprint 1.
- Produces:
  - `func (s *Store) GetOrganizationBySlug(ctx, slug string) (*Organization, error)`
  - `type RegisterNodeParams struct { OrgID uuid.UUID; Hostname, AdvertiseAddr string; CPUMillis int; MemoryBytes int64; Labels map[string]string; AgentVersion string }`
  - `func (s *Store) RegisterNode(ctx, p RegisterNodeParams) (*Node, error)` — upsert by `(org_id, hostname)`, sets `state='ready'`, `last_heartbeat=now()`
  - `type HeartbeatParams struct { AllocCPUMillis int; AllocMemoryBytes int64 }`
  - `func (s *Store) Heartbeat(ctx, nodeID uuid.UUID, p HeartbeatParams) error`
  - `func (s *Store) ListNodes(ctx, orgID uuid.UUID) ([]Node, error)`
  - `func (s *Store) ListReadyNodes(ctx, orgID uuid.UUID) ([]Node, error)` — `state='ready'` AND `last_heartbeat > now() - interval '30s'`

- [ ] **Step 1: Write the failing test** — `internal/store/nodes_test.go`

```go
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

func TestGetOrganizationBySlugUnknownIsNotFound(t *testing.T) {
	st := testStore(t)
	if _, err := st.GetOrganizationBySlug(testCtx(t), uniq("nope")); !errors.Is(err, ErrNotFound) {
		t.Fatalf("expected ErrNotFound, got %v", err)
	}
}
```

- [ ] **Step 2: Run it, verify it fails to compile** (`RegisterNode` undefined)

Run: `go test ./internal/store/ -run 'Node|OrganizationBySlug' -count=1`
Expected: build failure, `RegisterNode`/`ListReadyNodes`/`GetOrganizationBySlug` undefined.

- [ ] **Step 3: Implement** — `internal/store/nodes.go`

```go
package store

import (
	"context"
	"encoding/json"

	"github.com/google/uuid"
)

type RegisterNodeParams struct {
	OrgID         uuid.UUID
	Hostname      string
	AdvertiseAddr string
	CPUMillis     int
	MemoryBytes   int64
	Labels        map[string]string
	AgentVersion  string
}

// RegisterNode upserts by (org_id, hostname): a re-registering agent keeps
// its node identity but refreshes capacity and advertise address. A node
// that is actively registering is, by definition, ready.
func (s *Store) RegisterNode(ctx context.Context, p RegisterNodeParams) (*Node, error) {
	labels, err := json.Marshal(orEmpty(p.Labels))
	if err != nil {
		return nil, err
	}
	var n Node
	var labelsOut []byte
	err = s.pool.QueryRow(ctx, `
		INSERT INTO nodes (org_id, hostname, advertise_addr, state,
		                   cpu_millis, memory_bytes, labels, agent_version, last_heartbeat)
		VALUES ($1,$2,$3,'ready',$4,$5,$6,NULLIF($7,''),now())
		ON CONFLICT (org_id, hostname) DO UPDATE SET
			advertise_addr = EXCLUDED.advertise_addr,
			state          = 'ready',
			cpu_millis     = EXCLUDED.cpu_millis,
			memory_bytes   = EXCLUDED.memory_bytes,
			labels         = EXCLUDED.labels,
			agent_version  = EXCLUDED.agent_version,
			last_heartbeat = now()
		RETURNING id, org_id, hostname, host(advertise_addr), state,
		          cpu_millis, memory_bytes, alloc_cpu_millis, alloc_memory_bytes,
		          labels, COALESCE(agent_version,''), last_heartbeat, created_at
	`, p.OrgID, p.Hostname, p.AdvertiseAddr, p.CPUMillis, p.MemoryBytes,
		labels, p.AgentVersion).
		Scan(&n.ID, &n.OrgID, &n.Hostname, &n.AdvertiseAddr, &n.State,
			&n.CPUMillis, &n.MemoryBytes, &n.AllocCPUMillis, &n.AllocMemoryBytes,
			&labelsOut, &n.AgentVersion, &n.LastHeartbeat, &n.CreatedAt)
	if err != nil {
		return nil, mapErr(err)
	}
	if err := json.Unmarshal(labelsOut, &n.Labels); err != nil {
		return nil, err
	}
	return &n, nil
}

type HeartbeatParams struct {
	AllocCPUMillis   int
	AllocMemoryBytes int64
}

func (s *Store) Heartbeat(ctx context.Context, nodeID uuid.UUID, p HeartbeatParams) error {
	tag, err := s.pool.Exec(ctx, `
		UPDATE nodes SET alloc_cpu_millis=$2, alloc_memory_bytes=$3,
		       last_heartbeat=now(), state='ready'
		WHERE id=$1 AND state <> 'retired'
	`, nodeID, p.AllocCPUMillis, p.AllocMemoryBytes)
	if err != nil {
		return mapErr(err)
	}
	if tag.RowsAffected() == 0 {
		return ErrNotFound
	}
	return nil
}

func (s *Store) ListNodes(ctx context.Context, orgID uuid.UUID) ([]Node, error) {
	return s.queryNodes(ctx, `
		SELECT id, org_id, hostname, host(advertise_addr), state,
		       cpu_millis, memory_bytes, alloc_cpu_millis, alloc_memory_bytes,
		       labels, COALESCE(agent_version,''), last_heartbeat, created_at
		FROM nodes WHERE org_id=$1 ORDER BY hostname
	`, orgID)
}

// ListReadyNodes returns nodes eligible for placement: ready and heartbeating.
func (s *Store) ListReadyNodes(ctx context.Context, orgID uuid.UUID) ([]Node, error) {
	return s.queryNodes(ctx, `
		SELECT id, org_id, hostname, host(advertise_addr), state,
		       cpu_millis, memory_bytes, alloc_cpu_millis, alloc_memory_bytes,
		       labels, COALESCE(agent_version,''), last_heartbeat, created_at
		FROM nodes
		WHERE org_id=$1 AND state='ready'
		  AND last_heartbeat > now() - interval '30 seconds'
		ORDER BY hostname
	`, orgID)
}

func (s *Store) queryNodes(ctx context.Context, sql string, args ...any) ([]Node, error) {
	rows, err := s.pool.Query(ctx, sql, args...)
	if err != nil {
		return nil, mapErr(err)
	}
	defer rows.Close()
	out := []Node{}
	for rows.Next() {
		var n Node
		var labels []byte
		if err := rows.Scan(&n.ID, &n.OrgID, &n.Hostname, &n.AdvertiseAddr, &n.State,
			&n.CPUMillis, &n.MemoryBytes, &n.AllocCPUMillis, &n.AllocMemoryBytes,
			&labels, &n.AgentVersion, &n.LastHeartbeat, &n.CreatedAt); err != nil {
			return nil, err
		}
		if err := json.Unmarshal(labels, &n.Labels); err != nil {
			return nil, err
		}
		out = append(out, n)
	}
	return out, rows.Err()
}
```

Add to `internal/store/store.go` (needs `import "context"` already present):

```go
// GetOrganizationBySlug lets the agent register by org slug rather than a
// UUID it has no way to know.
func (s *Store) GetOrganizationBySlug(ctx context.Context, slug string) (*Organization, error) {
	var o Organization
	err := s.pool.QueryRow(ctx, `
		SELECT id, slug, name, created_at FROM organizations WHERE slug=$1
	`, slug).Scan(&o.ID, &o.Slug, &o.Name, &o.CreatedAt)
	if err != nil {
		return nil, mapErr(err)
	}
	return &o, nil
}
```

Note: `advertise_addr` is `INET`; the `host()` cast returns it as text without a netmask so it scans into a plain string. `RegisterNodeParams.AdvertiseAddr` is passed as text and Postgres casts it into `INET`.

- [ ] **Step 4: Run tests, verify pass**

Run: `go test ./internal/store/ -run 'Node|OrganizationBySlug' -count=1 -v`
Expected: PASS (or SKIP if Postgres is down — then `make up` and re-run).

- [ ] **Step 5: Commit**

```bash
git add internal/store/nodes.go internal/store/nodes_test.go internal/store/store.go
git commit -m "feat(store): node registration, heartbeat, ready-node queries"
```

---

## Task 2: Instance store methods

**Files:**
- Create: `internal/store/instances.go`, `internal/store/instances_test.go`
- Modify: `internal/store/deployments.go` (add `ListPendingDeployments`, `ListRolloutsInState`)

**Interfaces:**
- Consumes: `Store`, `shortID`, `Deployment`, `InstanceState`, `spec.Service`, `mapErr`, `s.tx`.
- Produces:
  - `type NewInstance struct { ServiceName string; Swappable bool; ImageRef string }`
  - `func (s *Store) CreateServiceInstances(ctx, deploymentID, nodeID uuid.UUID, insts []NewInstance) error`
  - `type DesiredInstance struct { InstanceID, DeploymentID uuid.UUID; Env8, ProjectName, Slot string; Revision int; ServiceName string; Swappable bool; Service spec.Service; State InstanceState; ContainerID string }`
  - `func (s *Store) DesiredStateForNode(ctx, nodeID uuid.UUID) ([]DesiredInstance, error)`
  - `type ObservedInstance struct { State InstanceState; ContainerID, HealthStatus, LastError string; RestartCount int; SetStarted bool }`
  - `func (s *Store) ReportInstance(ctx, instanceID uuid.UUID, o ObservedInstance) error`
  - `func (s *Store) InstanceStates(ctx, deploymentID uuid.UUID) ([]InstanceState, error)`
  - `func (s *Store) DeleteInstances(ctx, deploymentID uuid.UUID) error`
  - `type PendingDeployment struct { Deployment; OrgID uuid.UUID }`
  - `func (s *Store) ListPendingDeployments(ctx) ([]PendingDeployment, error)`
  - `func (s *Store) ListRolloutsInState(ctx, states ...DeploymentState) ([]Deployment, error)`

- [ ] **Step 1: Write the failing test** — `internal/store/instances_test.go`

```go
package store

import (
	"testing"

	"github.com/craig/composectl/internal/spec"
)

// deployFixture creates org→app→stack→version→env→deployment and returns the
// deployment plus a registered node, so instance tests have a real graph.
func deployFixture(t *testing.T, st *Store) (*Deployment, *Node) {
	t.Helper()
	org := newOrg(t, st)
	app := newApp(t, st, org.ID)
	stack := newStack(t, st, app.ID)
	sv, err := st.CreateStackVersion(testCtx(t), stack.ID, "raw", twoServiceSpec(), "t")
	if err != nil {
		t.Fatalf("CreateStackVersion: %v", err)
	}
	env, err := st.CreateEnvironment(testCtx(t), CreateEnvironmentParams{StackID: stack.ID, Slug: "prod"})
	if err != nil {
		t.Fatalf("CreateEnvironment: %v", err)
	}
	dep, err := st.CreateDeployment(testCtx(t), CreateDeploymentParams{
		EnvironmentID: env.ID, StackVersionID: sv.ID, ResolvedSpec: sv.Spec, CreatedBy: "t",
	})
	if err != nil {
		t.Fatalf("CreateDeployment: %v", err)
	}
	node, err := st.RegisterNode(testCtx(t), RegisterNodeParams{
		OrgID: org.ID, Hostname: uniq("node"), AdvertiseAddr: "10.0.0.9",
		CPUMillis: 4000, MemoryBytes: 8 << 30,
	})
	if err != nil {
		t.Fatalf("RegisterNode: %v", err)
	}
	return dep, node
}

func twoServiceSpec() *spec.DeploymentSpec {
	return &spec.DeploymentSpec{
		SpecVersion: spec.SpecVersion,
		Services: map[string]spec.Service{
			"api": {Name: "api", Image: "nginx:alpine", Swappable: true,
				Limits: spec.ResourceLimit{CPUMillis: 250, MemoryBytes: 256 << 20}},
			"db": {Name: "db", Image: "postgres:16-alpine", Swappable: false,
				Limits: spec.ResourceLimit{CPUMillis: 250, MemoryBytes: 256 << 20}},
		},
	}
}

func TestCreateAndReadDesiredState(t *testing.T) {
	st := testStore(t)
	dep, node := deployFixture(t, st)

	err := st.CreateServiceInstances(testCtx(t), dep.ID, node.ID, []NewInstance{
		{ServiceName: "api", Swappable: true, ImageRef: "nginx:alpine"},
		{ServiceName: "db", Swappable: false, ImageRef: "postgres:16-alpine"},
	})
	if err != nil {
		t.Fatalf("CreateServiceInstances: %v", err)
	}

	// A pending deployment's instances are not in desired state yet — the
	// deployment must be in an active rollout state to be reconciled.
	if got, _ := st.DesiredStateForNode(testCtx(t), node.ID); len(got) != 0 {
		t.Fatalf("pending deployment should not appear in desired state, got %d", len(got))
	}
	if err := st.UpdateDeploymentState(testCtx(t), dep.ID, DeployScheduling, ""); err != nil {
		t.Fatalf("advance to scheduling: %v", err)
	}

	desired, err := st.DesiredStateForNode(testCtx(t), node.ID)
	if err != nil {
		t.Fatalf("DesiredStateForNode: %v", err)
	}
	if len(desired) != 2 {
		t.Fatalf("expected 2 desired instances, got %d", len(desired))
	}
	for _, d := range desired {
		if d.Env8 == "" || d.ProjectName == "" {
			t.Fatalf("desired instance missing project context: %+v", d)
		}
		if d.Service.Image == "" {
			t.Fatalf("desired instance %s missing resolved Service spec", d.ServiceName)
		}
	}
}

func TestReportInstanceAndAggregate(t *testing.T) {
	st := testStore(t)
	dep, node := deployFixture(t, st)
	_ = st.CreateServiceInstances(testCtx(t), dep.ID, node.ID, []NewInstance{
		{ServiceName: "api", Swappable: true, ImageRef: "nginx:alpine"},
	})
	_ = st.UpdateDeploymentState(testCtx(t), dep.ID, DeployScheduling, "")

	desired, _ := st.DesiredStateForNode(testCtx(t), node.ID)
	if err := st.ReportInstance(testCtx(t), desired[0].InstanceID, ObservedInstance{
		State: InstanceRunning, ContainerID: "deadbeef", HealthStatus: "healthy", SetStarted: true,
	}); err != nil {
		t.Fatalf("ReportInstance: %v", err)
	}
	states, err := st.InstanceStates(testCtx(t), dep.ID)
	if err != nil {
		t.Fatalf("InstanceStates: %v", err)
	}
	if len(states) != 1 || states[0] != InstanceRunning {
		t.Fatalf("expected [running], got %v", states)
	}
}

func TestDeleteInstances(t *testing.T) {
	st := testStore(t)
	dep, node := deployFixture(t, st)
	_ = st.CreateServiceInstances(testCtx(t), dep.ID, node.ID, []NewInstance{
		{ServiceName: "api", Swappable: true, ImageRef: "nginx:alpine"},
	})
	if err := st.DeleteInstances(testCtx(t), dep.ID); err != nil {
		t.Fatalf("DeleteInstances: %v", err)
	}
	states, _ := st.InstanceStates(testCtx(t), dep.ID)
	if len(states) != 0 {
		t.Fatalf("expected no instances after delete, got %v", states)
	}
}

func TestListPendingDeploymentsCarriesOrg(t *testing.T) {
	st := testStore(t)
	dep, _ := deployFixture(t, st)
	pend, err := st.ListPendingDeployments(testCtx(t))
	if err != nil {
		t.Fatalf("ListPendingDeployments: %v", err)
	}
	var found bool
	for _, p := range pend {
		if p.ID == dep.ID {
			found = true
			if p.OrgID == uuidNil() {
				t.Fatal("pending deployment must carry its org id")
			}
		}
	}
	if !found {
		t.Fatalf("created pending deployment %s not listed", dep.ID)
	}
}
```

Add a tiny helper at the bottom of `instances_test.go`:

```go
func uuidNil() uuid.UUID { return uuid.UUID{} }
```

and its import: `"github.com/google/uuid"`.

- [ ] **Step 2: Run it, verify it fails** — build failure, methods undefined.

Run: `go test ./internal/store/ -run 'Desired|ReportInstance|DeleteInstances|ListPending' -count=1`

- [ ] **Step 3: Implement** — `internal/store/instances.go`

```go
package store

import (
	"context"
	"encoding/json"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"

	"github.com/craig/composectl/internal/spec"
)

type NewInstance struct {
	ServiceName string
	Swappable   bool
	ImageRef    string
}

// CreateServiceInstances writes the desired instance rows for a deployment in
// one transaction. The unique (deployment_id, service_name) constraint makes a
// double-schedule a no-op-or-conflict rather than a duplicate.
func (s *Store) CreateServiceInstances(ctx context.Context, deploymentID, nodeID uuid.UUID, insts []NewInstance) error {
	return s.tx(ctx, func(tx pgx.Tx) error {
		for _, in := range insts {
			if _, err := tx.Exec(ctx, `
				INSERT INTO service_instances
					(deployment_id, node_id, service_name, swappable, image_ref, state)
				VALUES ($1,$2,$3,$4,$5,'pending')
				ON CONFLICT (deployment_id, service_name) DO NOTHING
			`, deploymentID, nodeID, in.ServiceName, in.Swappable, in.ImageRef); err != nil {
				return err
			}
		}
		return nil
	})
}

type DesiredInstance struct {
	InstanceID   uuid.UUID
	DeploymentID uuid.UUID
	Env8         string
	ProjectName  string
	Slot         string
	Revision     int
	ServiceName  string
	Swappable    bool
	Service      spec.Service
	State        InstanceState
	ContainerID  string
}

// DesiredStateForNode returns the instances a node must run, joined to the
// resolved Service spec from their deployment. Only deployments in an active
// rollout state are included: a superseded or failed deployment's instances
// vanish from desired state, so the agent garbage-collects their containers.
func (s *Store) DesiredStateForNode(ctx context.Context, nodeID uuid.UUID) ([]DesiredInstance, error) {
	rows, err := s.pool.Query(ctx, `
		SELECT si.id, si.deployment_id, d.environment_id, d.project_name, d.slot,
		       d.revision, si.service_name, si.swappable, si.state,
		       COALESCE(si.container_id,''), d.resolved_spec
		FROM service_instances si
		JOIN deployments d ON d.id = si.deployment_id
		WHERE si.node_id = $1
		  AND d.state IN ('scheduling','starting','healthy','live')
		ORDER BY d.revision, si.service_name
	`, nodeID)
	if err != nil {
		return nil, mapErr(err)
	}
	defer rows.Close()

	out := []DesiredInstance{}
	for rows.Next() {
		var di DesiredInstance
		var envID uuid.UUID
		var specJSON []byte
		if err := rows.Scan(&di.InstanceID, &di.DeploymentID, &envID, &di.ProjectName,
			&di.Slot, &di.Revision, &di.ServiceName, &di.Swappable, &di.State,
			&di.ContainerID, &specJSON); err != nil {
			return nil, err
		}
		var ds spec.DeploymentSpec
		if err := json.Unmarshal(specJSON, &ds); err != nil {
			return nil, err
		}
		svc, ok := ds.Services[di.ServiceName]
		if !ok {
			// The instance references a service not in its own spec — a
			// scheduler bug. Skip rather than hand the agent a zero Service.
			continue
		}
		di.Service = svc
		di.Env8 = shortID(envID)
		out = append(out, di)
	}
	return out, rows.Err()
}

type ObservedInstance struct {
	State        InstanceState
	ContainerID  string
	HealthStatus string
	LastError    string
	RestartCount int
	SetStarted   bool
}

func (s *Store) ReportInstance(ctx context.Context, instanceID uuid.UUID, o ObservedInstance) error {
	tag, err := s.pool.Exec(ctx, `
		UPDATE service_instances SET
			state         = $2,
			container_id  = NULLIF($3,''),
			health_status = NULLIF($4,''),
			last_error    = NULLIF($5,''),
			restart_count = $6,
			started_at    = CASE WHEN $7 AND started_at IS NULL THEN now() ELSE started_at END,
			updated_at    = now()
		WHERE id = $1
	`, instanceID, o.State, o.ContainerID, o.HealthStatus, o.LastError, o.RestartCount, o.SetStarted)
	if err != nil {
		return mapErr(err)
	}
	if tag.RowsAffected() == 0 {
		return ErrNotFound
	}
	return nil
}

func (s *Store) InstanceStates(ctx context.Context, deploymentID uuid.UUID) ([]InstanceState, error) {
	rows, err := s.pool.Query(ctx, `
		SELECT state FROM service_instances WHERE deployment_id=$1
	`, deploymentID)
	if err != nil {
		return nil, mapErr(err)
	}
	defer rows.Close()
	out := []InstanceState{}
	for rows.Next() {
		var st InstanceState
		if err := rows.Scan(&st); err != nil {
			return nil, err
		}
		out = append(out, st)
	}
	return out, rows.Err()
}

func (s *Store) DeleteInstances(ctx context.Context, deploymentID uuid.UUID) error {
	_, err := s.pool.Exec(ctx, `DELETE FROM service_instances WHERE deployment_id=$1`, deploymentID)
	return mapErr(err)
}
```

Add to `internal/store/deployments.go`:

```go
type PendingDeployment struct {
	Deployment
	OrgID uuid.UUID
}

// ListPendingDeployments returns deployments awaiting scheduling, oldest
// first, each carrying its org id so the scheduler can find eligible nodes.
func (s *Store) ListPendingDeployments(ctx context.Context) ([]PendingDeployment, error) {
	rows, err := s.pool.Query(ctx, `
		SELECT d.id, d.environment_id, d.stack_version_id, d.revision, d.slot,
		       d.project_name, d.state, d.resolved_spec, d.created_at, d.updated_at,
		       a.org_id
		FROM deployments d
		JOIN environments e ON e.id = d.environment_id
		JOIN stacks s       ON s.id = e.stack_id
		JOIN applications a ON a.id = s.app_id
		WHERE d.state='pending'
		ORDER BY d.created_at
	`)
	if err != nil {
		return nil, mapErr(err)
	}
	defer rows.Close()
	out := []PendingDeployment{}
	for rows.Next() {
		var p PendingDeployment
		var specJSON []byte
		if err := rows.Scan(&p.ID, &p.EnvironmentID, &p.StackVersionID, &p.Revision,
			&p.Slot, &p.ProjectName, &p.State, &specJSON, &p.CreatedAt, &p.UpdatedAt,
			&p.OrgID); err != nil {
			return nil, err
		}
		if err := json.Unmarshal(specJSON, &p.ResolvedSpec); err != nil {
			return nil, err
		}
		out = append(out, p)
	}
	return out, rows.Err()
}

// ListRolloutsInState returns deployments the controller must advance.
func (s *Store) ListRolloutsInState(ctx context.Context, states ...DeploymentState) ([]Deployment, error) {
	ss := make([]string, len(states))
	for i, st := range states {
		ss[i] = string(st)
	}
	rows, err := s.pool.Query(ctx, `
		SELECT id, environment_id, stack_version_id, revision, slot, project_name,
		       state, created_at, updated_at
		FROM deployments WHERE state = ANY($1) ORDER BY updated_at
	`, ss)
	if err != nil {
		return nil, mapErr(err)
	}
	defer rows.Close()
	out := []Deployment{}
	for rows.Next() {
		var d Deployment
		if err := rows.Scan(&d.ID, &d.EnvironmentID, &d.StackVersionID, &d.Revision,
			&d.Slot, &d.ProjectName, &d.State, &d.CreatedAt, &d.UpdatedAt); err != nil {
			return nil, err
		}
		out = append(out, d)
	}
	return out, rows.Err()
}
```

`deployments.go` already imports `encoding/json` and `uuid`; no import changes.

- [ ] **Step 4: Run tests, verify pass**

Run: `go test ./internal/store/ -count=1`
Expected: PASS (all Sprint 1 + Task 1 + Task 2 store tests).

- [ ] **Step 5: Commit**

```bash
git add internal/store/instances.go internal/store/instances_test.go internal/store/deployments.go
git commit -m "feat(store): desired-instance writes, desired-state read, reports, teardown"
```

---

## Task 3: Node API handlers

**Files:**
- Modify: `internal/api/nodes.go` (replace stubs), `internal/api/server.go` (bootstrap `dev` org)
- Create: `internal/api/nodes_test.go`

**Interfaces:**
- Consumes: Task 1/2 store methods, `pathUUID`, `decodeJSON`, `writeJSON`, `writeError`, `contextWithTimeout`, `s.writeStoreError`.
- Produces (HTTP): `POST /v1/nodes/register` → `201 {node}`; `POST /v1/nodes/{id}/heartbeat` → `200`; `GET /v1/nodes/{id}/desired-state` → `200 {instances:[...]}`; `POST /v1/nodes/{id}/report` → `200`; `GET /v1/nodes?org={id}` → `200 {nodes:[...]}`.

- [ ] **Step 1: Write the failing test** — `internal/api/nodes_test.go`

```go
package api

import (
	"bytes"
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"os"
	"testing"
	"time"

	"github.com/craig/composectl/internal/store"
)

func testServer(t *testing.T) *Server {
	t.Helper()
	dsn := os.Getenv("COMPOSECTL_TEST_DATABASE_URL")
	if dsn == "" {
		dsn = "postgres://composectl:composectl@localhost:5473/composectl?sslmode=disable"
	}
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	st, err := store.New(ctx, dsn)
	if err != nil {
		t.Skipf("postgres unreachable — run make up: %v", err)
	}
	t.Cleanup(st.Close)
	return NewServer(st, slogDiscard())
}

func TestRegisterNodeHandler(t *testing.T) {
	srv := testServer(t)
	// Needs an org to register into; the bootstrapped dev org is guaranteed.
	body, _ := json.Marshal(map[string]any{
		"org": "dev", "hostname": "test-" + time.Now().Format("150405.000"),
		"advertise_addr": "10.1.2.3", "cpu_millis": 2000, "memory_bytes": 1 << 31,
	})
	rec := httptest.NewRecorder()
	srv.ServeHTTP(rec, httptest.NewRequest(http.MethodPost, "/v1/nodes/register", bytes.NewReader(body)))
	if rec.Code != http.StatusCreated {
		t.Fatalf("expected 201, got %d: %s", rec.Code, rec.Body.String())
	}
	var got struct{ ID string `json:"id"` }
	_ = json.Unmarshal(rec.Body.Bytes(), &got)
	if got.ID == "" {
		t.Fatal("expected a node id in the response")
	}
}
```

Add `slogDiscard` helper to the test file:

```go
func slogDiscard() *slog.Logger {
	return slog.New(slog.NewTextHandler(os.NewFile(0, os.DevNull), nil))
}
```

with imports `"log/slog"`. (If `os.DevNull` file handling is awkward, use `slog.New(slog.DiscardHandler)` on Go 1.24+, but pin to the io.Discard writer form for 1.23: `slog.NewTextHandler(io.Discard, nil)` with `"io"`.)

Use the `io.Discard` form to stay on 1.23:

```go
func slogDiscard() *slog.Logger { return slog.New(slog.NewTextHandler(io.Discard, nil)) }
```

- [ ] **Step 2: Run it, verify it fails** — the current handler returns 501.

Run: `go test ./internal/api/ -run TestRegisterNodeHandler -count=1`
Expected: FAIL, got 501 (or 404 if the dev org isn't bootstrapped yet — Step 3 fixes both).

- [ ] **Step 3: Implement** — replace the node stubs in `internal/api/nodes.go`

```go
package api

import (
	"net/http"
	"time"

	"github.com/google/uuid"

	"github.com/craig/composectl/internal/store"
)

type registerNodeRequest struct {
	Org           string            `json:"org"`
	Hostname      string            `json:"hostname"`
	AdvertiseAddr string            `json:"advertise_addr"`
	CPUMillis     int               `json:"cpu_millis"`
	MemoryBytes   int64             `json:"memory_bytes"`
	Labels        map[string]string `json:"labels,omitempty"`
	AgentVersion  string            `json:"agent_version,omitempty"`
}

func (s *Server) handleRegisterNode(w http.ResponseWriter, r *http.Request) {
	ctx, cancel := contextWithTimeout(r, 5*time.Second)
	defer cancel()

	var req registerNodeRequest
	if err := decodeJSON(r, &req); err != nil {
		writeError(w, http.StatusBadRequest, "invalid request body", nil)
		return
	}
	org, err := s.st.GetOrganizationBySlug(ctx, req.Org)
	if err != nil {
		s.writeStoreError(w, err)
		return
	}
	node, err := s.st.RegisterNode(ctx, store.RegisterNodeParams{
		OrgID: org.ID, Hostname: req.Hostname, AdvertiseAddr: req.AdvertiseAddr,
		CPUMillis: req.CPUMillis, MemoryBytes: req.MemoryBytes,
		Labels: req.Labels, AgentVersion: req.AgentVersion,
	})
	if err != nil {
		s.writeStoreError(w, err)
		return
	}
	writeJSON(w, http.StatusCreated, node)
}

type heartbeatRequest struct {
	AllocCPUMillis   int   `json:"alloc_cpu_millis"`
	AllocMemoryBytes int64 `json:"alloc_memory_bytes"`
}

func (s *Server) handleHeartbeat(w http.ResponseWriter, r *http.Request) {
	ctx, cancel := contextWithTimeout(r, 5*time.Second)
	defer cancel()
	id, ok := pathUUID(w, r, "id")
	if !ok {
		return
	}
	var req heartbeatRequest
	if err := decodeJSON(r, &req); err != nil {
		writeError(w, http.StatusBadRequest, "invalid request body", nil)
		return
	}
	if err := s.st.Heartbeat(ctx, id, store.HeartbeatParams{
		AllocCPUMillis: req.AllocCPUMillis, AllocMemoryBytes: req.AllocMemoryBytes,
	}); err != nil {
		s.writeStoreError(w, err)
		return
	}
	writeJSON(w, http.StatusOK, map[string]string{"status": "ok"})
}

// desiredInstanceDTO is the wire shape the agent consumes. The full resolved
// Service travels inline so the agent needs no second call to build containers.
func (s *Server) handleDesiredState(w http.ResponseWriter, r *http.Request) {
	ctx, cancel := contextWithTimeout(r, 5*time.Second)
	defer cancel()
	id, ok := pathUUID(w, r, "id")
	if !ok {
		return
	}
	desired, err := s.st.DesiredStateForNode(ctx, id)
	if err != nil {
		s.writeStoreError(w, err)
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"instances": desired})
}

type reportRequest struct {
	Instances []struct {
		InstanceID   uuid.UUID `json:"instance_id"`
		State        string    `json:"state"`
		ContainerID  string    `json:"container_id,omitempty"`
		HealthStatus string    `json:"health_status,omitempty"`
		LastError    string    `json:"last_error,omitempty"`
		RestartCount int       `json:"restart_count,omitempty"`
		SetStarted   bool      `json:"set_started,omitempty"`
	} `json:"instances"`
}

func (s *Server) handleInstanceReport(w http.ResponseWriter, r *http.Request) {
	ctx, cancel := contextWithTimeout(r, 10*time.Second)
	defer cancel()
	if _, ok := pathUUID(w, r, "id"); !ok {
		return
	}
	var req reportRequest
	if err := decodeJSON(r, &req); err != nil {
		writeError(w, http.StatusBadRequest, "invalid request body", nil)
		return
	}
	for _, in := range req.Instances {
		if err := s.st.ReportInstance(ctx, in.InstanceID, store.ObservedInstance{
			State: store.InstanceState(in.State), ContainerID: in.ContainerID,
			HealthStatus: in.HealthStatus, LastError: in.LastError,
			RestartCount: in.RestartCount, SetStarted: in.SetStarted,
		}); err != nil {
			s.writeStoreError(w, err)
			return
		}
	}
	writeJSON(w, http.StatusOK, map[string]string{"status": "ok"})
}

func (s *Server) handleListNodes(w http.ResponseWriter, r *http.Request) {
	ctx, cancel := contextWithTimeout(r, 5*time.Second)
	defer cancel()
	orgID, err := uuid.Parse(r.URL.Query().Get("org"))
	if err != nil {
		writeError(w, http.StatusBadRequest, "org query parameter is required", nil)
		return
	}
	nodes, err := s.st.ListNodes(ctx, orgID)
	if err != nil {
		s.writeStoreError(w, err)
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"nodes": nodes})
}
```

Delete the corresponding stub funcs and the now-unused ones from `nodes.go`; keep `handleRollback` as the only remaining stub plus `notImplemented`:

```go
func (s *Server) handleRollback(w http.ResponseWriter, r *http.Request) { notImplemented(w) }

func notImplemented(w http.ResponseWriter) {
	writeError(w, http.StatusNotImplemented, "not implemented until sprint 2 slice C", nil)
}
```

Bootstrap the dev org — add to `internal/api/server.go` in `NewServer`, after `s.routes()`:

```go
// BootstrapDevOrg ensures the dev org the local agent registers into exists.
// Exported so cmd/controlplane can call it at startup. Idempotent: a duplicate
// slug on restart is fine.
func (s *Server) BootstrapDevOrg(ctx context.Context) {
	if _, err := s.st.CreateOrganization(ctx, "dev", "Development"); err != nil &&
		!errors.Is(err, store.ErrConflict) {
		s.log.Warn("could not bootstrap dev org", "err", err)
	}
}
```

Call it from `cmd/controlplane/main.go` at startup (Task 9 wires this alongside the loops), passing a short-lived context. For now, expose it; the test relies on it existing. Add to the test setup a direct call:

In `nodes_test.go` `testServer`, after `NewServer`, add:

```go
	srv.BootstrapDevOrg(ctx)
```

- [ ] **Step 4: Run tests, verify pass**

Run: `go test ./internal/api/ -run TestRegisterNodeHandler -count=1 -v && go build ./...`
Expected: PASS; build clean.

- [ ] **Step 5: Commit**

```bash
git add internal/api/nodes.go internal/api/nodes_test.go internal/api/server.go
git commit -m "feat(api): wire node register/heartbeat/desired-state/report/list handlers"
```

---

## Task 4: Docker driver (the SDK boundary)

**Files:**
- Create: `internal/agent/dockerd/driver.go`, `internal/agent/dockerd/driver_test.go`
- Modify: `go.mod`/`go.sum` (add `github.com/docker/docker` — verify `go 1.23` unchanged after)

**Interfaces:**
- Produces:
  - `type SecretSource interface { Get(key string) (string, bool) }`
  - `func New(host string, secrets SecretSource) (*Driver, error)`
  - `type ContainerSpec struct { Name, Image string; Env map[string]string; SecretEnv map[string]string; Cmd, Entrypoint []string; WorkingDir, User string; Mounts []VolumeMount; Health *spec.HealthCheck; Labels map[string]string; Network string; Restart string; CPUMillis int; MemoryBytes int64 }`
  - `type VolumeMount struct { Volume, Target string; ReadOnly bool }`
  - `func (d *Driver) EnsureImage(ctx, ref string) error`
  - `func (d *Driver) EnsureNetwork(ctx, name string, labels map[string]string) (string, error)`
  - `func (d *Driver) EnsureContainer(ctx, cs ContainerSpec) (id string, created bool, err error)`
  - `func (d *Driver) AttachNetwork(ctx, containerID, network string, aliases ...string) error`
  - `type Health struct { Running bool; Status string; ExitCode int; RestartCount int }`
  - `func (d *Driver) InspectHealth(ctx, containerID string) (Health, error)`
  - `func (d *Driver) StopRemove(ctx, containerID string) error`
  - `type Managed struct { ID, Name, Service string; Swappable bool }`
  - `func (d *Driver) ListManaged(ctx, env8 string) ([]Managed, error)`

- [ ] **Step 1: Add the dependency**

```bash
go get github.com/docker/docker@v27.5.1+incompatible
grep '^go ' go.mod   # MUST still read: go 1.23
```

If the directive moved, run `go mod edit -go=1.23 && go get github.com/rogpeppe/go-internal@v1.14.1 && go mod tidy` and re-check.

- [ ] **Step 2: Write the failing test** — `internal/agent/dockerd/driver_test.go`

```go
package dockerd

import (
	"context"
	"testing"
	"time"
)

type staticSecrets map[string]string

func (s staticSecrets) Get(k string) (string, bool) { v, ok := s[k]; return v, ok }

func testDriver(t *testing.T) *Driver {
	t.Helper()
	d, err := New("", staticSecrets{})
	if err != nil {
		t.Skipf("docker unavailable: %v", err)
	}
	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
	defer cancel()
	if _, err := d.cli.Ping(ctx); err != nil {
		t.Skipf("docker daemon unreachable: %v", err)
	}
	return d
}

func TestEnsureContainerCreatesAndAdopts(t *testing.T) {
	d := testDriver(t)
	ctx, cancel := context.WithTimeout(context.Background(), 60*time.Second)
	defer cancel()

	env8 := "testdrv1"
	name := "cc-" + env8 + "-r1-blue-web"
	netName := "cc-" + env8 + "-r1-blue"
	labels := map[string]string{"cc.env": env8, "cc.service": "web", "cc.swappable": "true"}

	if err := d.EnsureImage(ctx, "busybox:latest"); err != nil {
		t.Fatalf("EnsureImage: %v", err)
	}
	netID, err := d.EnsureNetwork(ctx, netName, map[string]string{"cc.env": env8})
	if err != nil {
		t.Fatalf("EnsureNetwork: %v", err)
	}
	if netID == "" {
		t.Fatal("expected a network id")
	}

	cs := ContainerSpec{
		Name: name, Image: "busybox:latest",
		Cmd: []string{"sh", "-c", "sleep 30"},
		Labels: labels, Network: netName, MemoryBytes: 64 << 20,
	}
	id, created, err := d.EnsureContainer(ctx, cs)
	if err != nil {
		t.Fatalf("EnsureContainer: %v", err)
	}
	if !created {
		t.Fatal("expected the container to be created")
	}
	t.Cleanup(func() {
		c, cancel := context.WithTimeout(context.Background(), 30*time.Second)
		defer cancel()
		_ = d.StopRemove(c, id)
		_ = d.removeNetwork(c, netName)
	})

	// Second call adopts the existing container rather than creating a new one.
	id2, created2, err := d.EnsureContainer(ctx, cs)
	if err != nil {
		t.Fatalf("EnsureContainer (adopt): %v", err)
	}
	if created2 || id2 != id {
		t.Fatalf("expected adoption of %s, got id=%s created=%v", id, id2, created2)
	}

	managed, err := d.ListManaged(ctx, env8)
	if err != nil {
		t.Fatalf("ListManaged: %v", err)
	}
	if len(managed) != 1 || managed[0].Service != "web" {
		t.Fatalf("expected one managed 'web' container, got %+v", managed)
	}

	h, err := d.InspectHealth(ctx, id)
	if err != nil {
		t.Fatalf("InspectHealth: %v", err)
	}
	if !h.Running {
		t.Fatalf("expected the container to be running, got %+v", h)
	}
}

func TestSecretExpansion(t *testing.T) {
	d, err := New("", staticSecrets{"db_password": "s3cr3t"})
	if err != nil {
		t.Skipf("docker client init: %v", err)
	}
	env, err := d.resolveEnv(
		map[string]string{"LOG_LEVEL": "info"},
		map[string]string{"URL": "postgres://app:${secret:db_password}@db/app"},
	)
	if err != nil {
		t.Fatalf("resolveEnv: %v", err)
	}
	if env["URL"] != "postgres://app:s3cr3t@db/app" {
		t.Fatalf("secret not expanded mid-string: %q", env["URL"])
	}
	if env["LOG_LEVEL"] != "info" {
		t.Fatalf("plain env lost: %q", env["LOG_LEVEL"])
	}
}

func TestSecretExpansionMissingKeyErrors(t *testing.T) {
	d, _ := New("", staticSecrets{})
	if _, err := d.resolveEnv(nil, map[string]string{"URL": "${secret:absent}"}); err == nil {
		t.Fatal("expected an error for a missing secret")
	}
}
```

- [ ] **Step 3: Run it, verify it fails** — `New`/`resolveEnv` undefined.

Run: `go test ./internal/agent/dockerd/ -run Secret -count=1`

- [ ] **Step 4: Implement** — `internal/agent/dockerd/driver.go`

```go
// Package dockerd is the ONLY package that imports the Docker SDK. Everything
// above it speaks ContainerSpec and the Driver methods, so the container
// runtime could be swapped without touching the agent's reconcile logic.
package dockerd

import (
	"context"
	"fmt"
	"io"
	"strings"

	"github.com/docker/docker/api/types/container"
	"github.com/docker/docker/api/types/filters"
	"github.com/docker/docker/api/types/image"
	"github.com/docker/docker/api/types/network"
	"github.com/docker/docker/client"

	"github.com/craig/composectl/internal/spec"
)

// SecretSource resolves ${secret:KEY} references at container start. Sprint 2
// uses a trivial dev implementation; Sprint 3 replaces it with the encrypted
// per-environment secret store. Plaintext never leaves the agent.
type SecretSource interface {
	Get(key string) (string, bool)
}

type Driver struct {
	cli     *client.Client
	secrets SecretSource
}

func New(host string, secrets SecretSource) (*Driver, error) {
	opts := []client.Opt{client.FromEnv, client.WithAPIVersionNegotiation()}
	if host != "" {
		opts = append(opts, client.WithHost(host))
	}
	cli, err := client.NewClientWithOpts(opts...)
	if err != nil {
		return nil, fmt.Errorf("docker client: %w", err)
	}
	return &Driver{cli: cli, secrets: secrets}, nil
}

type VolumeMount struct {
	Volume   string
	Target   string
	ReadOnly bool
}

type ContainerSpec struct {
	Name       string
	Image      string
	Env        map[string]string
	SecretEnv  map[string]string
	Cmd        []string
	Entrypoint []string
	WorkingDir string
	User       string
	Mounts     []VolumeMount
	Health     *spec.HealthCheck
	Labels     map[string]string
	Network    string
	Restart    string
	CPUMillis  int
	MemoryBytes int64
}

func (d *Driver) EnsureImage(ctx context.Context, ref string) error {
	// Pull only when absent — an image already present (common in dev) skips
	// the network round-trip. ImageInspectWithRaw is the form stable across
	// SDK versions (three returns: inspect, raw JSON, err).
	if _, _, err := d.cli.ImageInspectWithRaw(ctx, ref); err == nil {
		return nil
	}
	rc, err := d.cli.ImagePull(ctx, ref, image.PullOptions{})
	if err != nil {
		return fmt.Errorf("pull %s: %w", ref, err)
	}
	defer rc.Close()
	_, _ = io.Copy(io.Discard, rc) // draining the stream is what blocks until done
	return nil
}

func (d *Driver) EnsureNetwork(ctx context.Context, name string, labels map[string]string) (string, error) {
	nets, err := d.cli.NetworkList(ctx, network.ListOptions{
		Filters: filters.NewArgs(filters.Arg("name", name)),
	})
	if err != nil {
		return "", err
	}
	for _, n := range nets {
		if n.Name == name {
			return n.ID, nil
		}
	}
	created, err := d.cli.NetworkCreate(ctx, name, network.CreateOptions{Labels: labels})
	if err != nil {
		return "", fmt.Errorf("create network %s: %w", name, err)
	}
	return created.ID, nil
}

func (d *Driver) removeNetwork(ctx context.Context, name string) error {
	nets, err := d.cli.NetworkList(ctx, network.ListOptions{
		Filters: filters.NewArgs(filters.Arg("name", name)),
	})
	if err != nil {
		return err
	}
	for _, n := range nets {
		if n.Name == name {
			return d.cli.NetworkRemove(ctx, n.ID)
		}
	}
	return nil
}

// EnsureContainer creates and starts the container if absent, or adopts the
// existing one by name. Adoption is how a pinned service is shared: the second
// deployment to want it finds it already running and reports created=false.
func (d *Driver) EnsureContainer(ctx context.Context, cs ContainerSpec) (string, bool, error) {
	if existing, err := d.findByName(ctx, cs.Name); err != nil {
		return "", false, err
	} else if existing != "" {
		return existing, false, nil
	}

	env, err := d.resolveEnv(cs.Env, cs.SecretEnv)
	if err != nil {
		return "", false, err
	}
	envSlice := make([]string, 0, len(env))
	for k, v := range env {
		envSlice = append(envSlice, k+"="+v)
	}

	cfg := &container.Config{
		Image:      cs.Image,
		Env:        envSlice,
		Cmd:        cs.Cmd,
		Entrypoint: cs.Entrypoint,
		WorkingDir: cs.WorkingDir,
		User:       cs.User,
		Labels:     cs.Labels,
	}
	if cs.Health != nil && len(cs.Health.Test) > 0 {
		cfg.Healthcheck = &container.HealthConfig{
			Test:        cs.Health.Test,
			Interval:    durationSecs(cs.Health.IntervalSec),
			Timeout:     durationSecs(cs.Health.TimeoutSec),
			Retries:     cs.Health.Retries,
			StartPeriod: durationSecs(cs.Health.StartSec),
		}
	}

	binds := make([]string, 0, len(cs.Mounts))
	for _, m := range cs.Mounts {
		bind := m.Volume + ":" + m.Target
		if m.ReadOnly {
			bind += ":ro"
		}
		binds = append(binds, bind)
	}

	hostCfg := &container.HostConfig{
		Binds:         binds,
		RestartPolicy: restartPolicy(cs.Restart),
		Resources: container.Resources{
			NanoCPUs: int64(cs.CPUMillis) * 1_000_000, // millicpu → nanocpu
			Memory:   cs.MemoryBytes,
		},
	}

	var netCfg *network.NetworkingConfig
	if cs.Network != "" {
		netCfg = &network.NetworkingConfig{
			EndpointsConfig: map[string]*network.EndpointSettings{cs.Network: {}},
		}
	}

	created, err := d.cli.ContainerCreate(ctx, cfg, hostCfg, netCfg, nil, cs.Name)
	if err != nil {
		return "", false, fmt.Errorf("create %s: %w", cs.Name, err)
	}
	if err := d.cli.ContainerStart(ctx, created.ID, container.StartOptions{}); err != nil {
		return "", false, fmt.Errorf("start %s: %w", cs.Name, err)
	}
	return created.ID, true, nil
}

func (d *Driver) AttachNetwork(ctx context.Context, containerID, netName string, aliases ...string) error {
	// Idempotent: ignore "already exists" so re-reconciling is safe.
	err := d.cli.NetworkConnect(ctx, netName, containerID, &network.EndpointSettings{Aliases: aliases})
	if err != nil && !strings.Contains(err.Error(), "already exists") {
		return err
	}
	return nil
}

type Health struct {
	Running      bool
	Status       string // "healthy" | "unhealthy" | "starting" | "" (none)
	ExitCode     int
	RestartCount int
}

func (d *Driver) InspectHealth(ctx context.Context, containerID string) (Health, error) {
	c, err := d.cli.ContainerInspect(ctx, containerID)
	if err != nil {
		return Health{}, err
	}
	h := Health{
		Running:      c.State.Running,
		ExitCode:     c.State.ExitCode,
		RestartCount: c.RestartCount,
	}
	if c.State.Health != nil {
		h.Status = c.State.Health.Status
	}
	return h, nil
}

func (d *Driver) StopRemove(ctx context.Context, containerID string) error {
	_ = d.cli.ContainerStop(ctx, containerID, container.StopOptions{})
	return d.cli.ContainerRemove(ctx, containerID, container.RemoveOptions{Force: true})
}

type Managed struct {
	ID        string
	Name      string
	Service   string
	Swappable bool
}

func (d *Driver) ListManaged(ctx context.Context, env8 string) ([]Managed, error) {
	list, err := d.cli.ContainerList(ctx, container.ListOptions{
		All:     true,
		Filters: filters.NewArgs(filters.Arg("label", "cc.env="+env8)),
	})
	if err != nil {
		return nil, err
	}
	out := make([]Managed, 0, len(list))
	for _, c := range list {
		name := ""
		if len(c.Names) > 0 {
			name = strings.TrimPrefix(c.Names[0], "/")
		}
		out = append(out, Managed{
			ID: c.ID, Name: name, Service: c.Labels["cc.service"],
			Swappable: c.Labels["cc.swappable"] == "true",
		})
	}
	return out, nil
}

func (d *Driver) findByName(ctx context.Context, name string) (string, error) {
	list, err := d.cli.ContainerList(ctx, container.ListOptions{
		All:     true,
		Filters: filters.NewArgs(filters.Arg("name", "^/"+name+"$")),
	})
	if err != nil {
		return "", err
	}
	for _, c := range list {
		for _, n := range c.Names {
			if strings.TrimPrefix(n, "/") == name {
				return c.ID, nil
			}
		}
	}
	return "", nil
}

// resolveEnv merges plain env with secret templates expanded via the
// SecretSource, using exactly spec.SecretRefPattern so the agent expands the
// same syntax the parser recognized. A missing secret is a hard error — better
// than handing the container a half-built connection string.
func (d *Driver) resolveEnv(env, secretEnv map[string]string) (map[string]string, error) {
	out := make(map[string]string, len(env)+len(secretEnv))
	for k, v := range env {
		out[k] = v
	}
	for k, tmpl := range secretEnv {
		var missing string
		expanded := spec.SecretRefPattern.ReplaceAllStringFunc(tmpl, func(m string) string {
			sub := spec.SecretRefPattern.FindStringSubmatch(m)
			val, ok := d.secrets.Get(sub[1])
			if !ok {
				missing = sub[1]
				return m
			}
			return val
		})
		if missing != "" {
			return nil, fmt.Errorf("secret %q referenced by %s is not available", missing, k)
		}
		out[k] = expanded
	}
	return out, nil
}
```

Add helpers at the bottom (own the small conversions here, not scattered):

```go
func durationSecs(sec int) time.Duration { return time.Duration(sec) * time.Second }

func restartPolicy(mode string) container.RestartPolicy {
	switch mode {
	case "", "no":
		return container.RestartPolicy{Name: container.RestartPolicyDisabled}
	case "always":
		return container.RestartPolicy{Name: container.RestartPolicyAlways}
	case "unless-stopped":
		return container.RestartPolicy{Name: container.RestartPolicyUnlessStopped}
	default: // on-failure and friends
		return container.RestartPolicy{Name: container.RestartPolicyOnFailure, MaximumRetryCount: 3}
	}
}
```

and add `"time"` to the import block.

- [ ] **Step 5: Run tests, verify pass**

Run: `go test ./internal/agent/dockerd/ -count=1 -v`
Expected: `TestSecret*` PASS; the container test PASS if Docker is up, else SKIP. `go build ./...` clean, `grep '^go ' go.mod` still `1.23`.

- [ ] **Step 6: Commit**

```bash
git add internal/agent/dockerd/ go.mod go.sum
git commit -m "feat(agent): docker driver — image/network/container ops, secret expansion"
```

---

## Task 5: Agent reconcile logic

**Files:**
- Create: `internal/agent/reconcile.go`, `internal/agent/reconcile_test.go`, `internal/agent/secrets.go`

**Interfaces:**
- Consumes: `store.DesiredInstance`, `store.InstanceState` constants, `dockerd.ContainerSpec`/`Health`/`Managed`/`VolumeMount`.
- Produces:
  - `type DockerDriver interface { EnsureImage; EnsureNetwork; EnsureContainer; AttachNetwork; InspectHealth; StopRemove; ListManaged }` (method set matching Task 4 so the real `*dockerd.Driver` satisfies it and tests use a fake)
  - `type Report struct { InstanceID uuid.UUID; State store.InstanceState; ContainerID, HealthStatus, LastError string; RestartCount int; SetStarted bool }`
  - `type Reconciler struct { drv DockerDriver; debounce time.Duration }`
  - `func NewReconciler(drv DockerDriver) *Reconciler`
  - `func (r *Reconciler) Reconcile(ctx, desired []store.DesiredInstance) []Report`
  - `type EnvSecrets map[string]string` implementing `dockerd.SecretSource`

- [ ] **Step 1: Write the failing test** — `internal/agent/reconcile_test.go`

```go
package agent

import (
	"context"
	"testing"

	"github.com/google/uuid"

	"github.com/craig/composectl/internal/agent/dockerd"
	"github.com/craig/composectl/internal/spec"
	"github.com/craig/composectl/internal/store"
)

// fakeDriver records calls and returns canned health.
type fakeDriver struct {
	created  []string
	attached []string
	removed  []string
	managed  []dockerd.Managed
	health   map[string]dockerd.Health
}

func (f *fakeDriver) EnsureImage(ctx context.Context, ref string) error { return nil }
func (f *fakeDriver) EnsureNetwork(ctx context.Context, name string, l map[string]string) (string, error) {
	return "net-" + name, nil
}
func (f *fakeDriver) EnsureContainer(ctx context.Context, cs dockerd.ContainerSpec) (string, bool, error) {
	f.created = append(f.created, cs.Name)
	return "id-" + cs.Name, true, nil
}
func (f *fakeDriver) AttachNetwork(ctx context.Context, id, net string, a ...string) error {
	f.attached = append(f.attached, id+"->"+net)
	return nil
}
func (f *fakeDriver) InspectHealth(ctx context.Context, id string) (dockerd.Health, error) {
	if h, ok := f.health[id]; ok {
		return h, nil
	}
	return dockerd.Health{Running: true}, nil
}
func (f *fakeDriver) StopRemove(ctx context.Context, id string) error {
	f.removed = append(f.removed, id)
	return nil
}
func (f *fakeDriver) ListManaged(ctx context.Context, env8 string) ([]dockerd.Managed, error) {
	return f.managed, nil
}

func desired(service string, swappable bool, health dockerd.Health) store.DesiredInstance {
	return store.DesiredInstance{
		InstanceID: uuid.New(), DeploymentID: uuid.New(), Env8: "env12345",
		ProjectName: "cc-env12345-r1-blue", Slot: "blue", Revision: 1,
		ServiceName: service, Swappable: swappable, State: store.InstancePending,
		Service: spec.Service{Name: service, Image: "img:" + service, Swappable: swappable,
			Limits: spec.ResourceLimit{CPUMillis: 250, MemoryBytes: 256 << 20}},
	}
}

func TestReconcileCreatesSwappableAndPinned(t *testing.T) {
	f := &fakeDriver{health: map[string]dockerd.Health{}}
	r := NewReconciler(f)
	reports := r.Reconcile(context.Background(), []store.DesiredInstance{
		desired("api", true, dockerd.Health{}),
		desired("db", false, dockerd.Health{}),
	})
	if len(f.created) != 2 {
		t.Fatalf("expected 2 containers created, got %v", f.created)
	}
	// pinned db uses the env-scoped stable name; swappable api uses the project name.
	var sawPinned, sawSwappable bool
	for _, name := range f.created {
		if name == "cc-env12345-pinned-db" {
			sawPinned = true
		}
		if name == "cc-env12345-r1-blue-api" {
			sawSwappable = true
		}
	}
	if !sawPinned || !sawSwappable {
		t.Fatalf("naming wrong: created=%v", f.created)
	}
	if len(reports) != 2 {
		t.Fatalf("expected 2 reports, got %d", len(reports))
	}
}

func TestReconcileGCsOrphanSwappable(t *testing.T) {
	f := &fakeDriver{
		managed: []dockerd.Managed{
			{ID: "id-old", Name: "cc-env12345-r0-green-api", Service: "api", Swappable: true},
			{ID: "id-keep", Name: "cc-env12345-pinned-db", Service: "db", Swappable: false},
		},
	}
	r := NewReconciler(f)
	// Desired set no longer includes the r0-green api, but still includes db.
	r.Reconcile(context.Background(), []store.DesiredInstance{desired("db", false, dockerd.Health{})})
	if len(f.removed) != 1 || f.removed[0] != "id-old" {
		t.Fatalf("expected only the orphan swappable removed, got %v", f.removed)
	}
}

func TestHealthMappingHealthchecked(t *testing.T) {
	f := &fakeDriver{health: map[string]dockerd.Health{
		"id-cc-env12345-r1-blue-api": {Running: true, Status: "healthy"},
	}}
	r := NewReconciler(f)
	reports := r.Reconcile(context.Background(), []store.DesiredInstance{
		func() store.DesiredInstance {
			d := desired("api", true, dockerd.Health{})
			d.Service.Health = &spec.HealthCheck{Test: []string{"CMD", "true"}, Retries: 3}
			return d
		}(),
	})
	if reports[0].State != store.InstanceRunning {
		t.Fatalf("healthy container must map to running, got %s", reports[0].State)
	}
}

func TestHealthMappingExitedFails(t *testing.T) {
	f := &fakeDriver{health: map[string]dockerd.Health{
		"id-cc-env12345-r1-blue-api": {Running: false, ExitCode: 1},
	}}
	r := NewReconciler(f)
	reports := r.Reconcile(context.Background(), []store.DesiredInstance{desired("api", true, dockerd.Health{})})
	if reports[0].State != store.InstanceFailed {
		t.Fatalf("exited container must map to failed, got %s", reports[0].State)
	}
}
```

- [ ] **Step 2: Run it, verify it fails** — `NewReconciler` undefined.

Run: `go test ./internal/agent/ -run Reconcile -count=1`

- [ ] **Step 3: Implement** — `internal/agent/reconcile.go`

```go
package agent

import (
	"context"
	"fmt"
	"time"

	"github.com/google/uuid"

	"github.com/craig/composectl/internal/agent/dockerd"
	"github.com/craig/composectl/internal/store"
)

// DockerDriver is the subset of *dockerd.Driver the reconciler needs. Naming it
// here (rather than importing the concrete type) lets tests substitute a fake
// and keeps this logic free of the Docker SDK.
type DockerDriver interface {
	EnsureImage(ctx context.Context, ref string) error
	EnsureNetwork(ctx context.Context, name string, labels map[string]string) (string, error)
	EnsureContainer(ctx context.Context, cs dockerd.ContainerSpec) (string, bool, error)
	AttachNetwork(ctx context.Context, containerID, network string, aliases ...string) error
	InspectHealth(ctx context.Context, containerID string) (dockerd.Health, error)
	StopRemove(ctx context.Context, containerID string) error
	ListManaged(ctx context.Context, env8 string) ([]dockerd.Managed, error)
}

type Report struct {
	InstanceID   uuid.UUID
	State        store.InstanceState
	ContainerID  string
	HealthStatus string
	LastError    string
	RestartCount int
	SetStarted   bool
}

type Reconciler struct {
	drv      DockerDriver
	debounce time.Duration
}

func NewReconciler(drv DockerDriver) *Reconciler {
	return &Reconciler{drv: drv, debounce: 5 * time.Second}
}

// Reconcile converges Docker to the desired instance set and returns a report
// per instance. It also garbage-collects containers this env manages that are
// no longer desired — that is how a superseded revision's swappable containers
// are torn down.
func (r *Reconciler) Reconcile(ctx context.Context, desired []store.DesiredInstance) []Report {
	reports := make([]Report, 0, len(desired))
	wanted := map[string]bool{} // container name → desired
	envs := map[string]bool{}

	for _, di := range desired {
		name := containerName(di)
		wanted[name] = true
		envs[di.Env8] = true
		reports = append(reports, r.ensure(ctx, di, name))
	}

	// GC: any managed container in a touched env whose name is not wanted, and
	// which is swappable, is an orphan. Pinned containers are never GC'd here —
	// a live deployment still holds a desired row for them.
	for env8 := range envs {
		managed, err := r.drv.ListManaged(ctx, env8)
		if err != nil {
			continue
		}
		for _, m := range managed {
			if !wanted[m.Name] && m.Swappable {
				_ = r.drv.StopRemove(ctx, m.ID)
			}
		}
	}
	return reports
}

func (r *Reconciler) ensure(ctx context.Context, di store.DesiredInstance, name string) Report {
	rep := Report{InstanceID: di.InstanceID}
	fail := func(err error) Report {
		rep.State = store.InstanceFailed
		rep.LastError = err.Error()
		return rep
	}

	if err := r.drv.EnsureImage(ctx, di.Service.Image); err != nil {
		return fail(err)
	}
	if _, err := r.drv.EnsureNetwork(ctx, di.ProjectName, map[string]string{"cc.env": di.Env8}); err != nil {
		return fail(err)
	}

	cs := containerSpec(di, name)
	id, _, err := r.drv.EnsureContainer(ctx, cs)
	if err != nil {
		return fail(err)
	}
	// A pinned container is created once under its env network but must be
	// reachable from every revision's network; attach it to this revision's.
	if !di.Swappable {
		if err := r.drv.AttachNetwork(ctx, id, di.ProjectName, di.ServiceName); err != nil {
			return fail(err)
		}
	}

	rep.ContainerID = id
	rep.SetStarted = true
	h, err := r.drv.InspectHealth(ctx, id)
	if err != nil {
		return fail(err)
	}
	rep.RestartCount = h.RestartCount
	rep.State, rep.HealthStatus = mapHealth(di.Service.Health != nil && len(di.Service.Health.Test) > 0, h)
	return rep
}

// mapHealth turns a container's observed state into an instance state.
func mapHealth(hasHealthcheck bool, h dockerd.Health) (store.InstanceState, string) {
	if !h.Running {
		if h.ExitCode != 0 {
			return store.InstanceFailed, "exited"
		}
		return store.InstanceStopped, "stopped"
	}
	if hasHealthcheck {
		switch h.Status {
		case "healthy":
			return store.InstanceRunning, "healthy"
		case "unhealthy":
			return store.InstanceUnhealthy, "unhealthy"
		default: // "starting" or empty until the first probe resolves
			return store.InstanceStarting, "starting"
		}
	}
	// No healthcheck: running is the best signal we have. The controller's
	// start-timeout guards against a container that will crash after this tick.
	return store.InstanceRunning, "running"
}

func containerName(di store.DesiredInstance) string {
	if di.Swappable {
		return fmt.Sprintf("%s-%s", di.ProjectName, di.ServiceName)
	}
	return fmt.Sprintf("cc-%s-pinned-%s", di.Env8, di.ServiceName)
}

func containerSpec(di store.DesiredInstance, name string) dockerd.ContainerSpec {
	svc := di.Service
	mounts := make([]dockerd.VolumeMount, 0, len(svc.Mounts))
	for _, m := range svc.Mounts {
		if m.Kind != spec.MountVolume {
			continue // tmpfs handled by Docker config later; Slice A: named volumes only
		}
		mounts = append(mounts, dockerd.VolumeMount{
			Volume:   fmt.Sprintf("cc-%s-%s", di.Env8, m.Source),
			Target:   m.Target,
			ReadOnly: m.ReadOnly,
		})
	}
	return dockerd.ContainerSpec{
		Name: name, Image: svc.Image, Env: svc.Env, SecretEnv: svc.SecretEnv,
		Cmd: svc.Command, Entrypoint: svc.Entrypoint, WorkingDir: svc.WorkingDir,
		User: svc.User, Mounts: mounts, Health: svc.Health, Restart: svc.Restart,
		CPUMillis: svc.Limits.CPUMillis, MemoryBytes: svc.Limits.MemoryBytes,
		Network: di.ProjectName,
		Labels: map[string]string{
			"cc.env": di.Env8, "cc.deployment": di.DeploymentID.String(),
			"cc.service": di.ServiceName, "cc.swappable": boolStr(di.Swappable),
		},
	}
}

func boolStr(b bool) string {
	if b {
		return "true"
	}
	return "false"
}
```

Add `"github.com/craig/composectl/internal/spec"` to the imports (used by `containerSpec`).

Create `internal/agent/secrets.go`:

```go
package agent

// EnvSecrets is the Sprint 2 dev secret source: a static key→value map, loaded
// from the agent's own environment. It satisfies dockerd.SecretSource. Sprint 3
// replaces it with the encrypted per-environment store; nothing above this type
// changes when it does.
type EnvSecrets map[string]string

func (e EnvSecrets) Get(key string) (string, bool) { v, ok := e[key]; return v, ok }
```

- [ ] **Step 4: Run tests, verify pass**

Run: `go test ./internal/agent/ -run 'Reconcile|Health' -count=1 -v`
Expected: PASS.

- [ ] **Step 5: Commit**

```bash
git add internal/agent/reconcile.go internal/agent/reconcile_test.go internal/agent/secrets.go
git commit -m "feat(agent): reconcile logic — naming, pinned adoption, GC, health mapping"
```

---

## Task 6: Agent loop + entrypoint + config

**Files:**
- Create: `internal/agent/agent.go`, `cmd/agent/main.go`, `internal/config/agent.go`

**Interfaces:**
- Consumes: `Reconciler`, `dockerd.New`, `EnvSecrets`, the control-plane HTTP API from Task 3.
- Produces:
  - `type Config struct { ControlPlaneURL, Org, Hostname, AdvertiseAddr, AgentToken, DockerHost string; CPUMillis int; MemoryBytes int64; PollInterval time.Duration; Secrets map[string]string }`
  - `func Run(ctx, cfg Config, log *slog.Logger) error` — register, then loop {poll desired-state → reconcile → report → heartbeat}
  - `func LoadAgentConfig() (agent.Config, error)` in `internal/config`

This task is HTTP-client plumbing with no pure logic to unit-test in isolation; its verification is the end-to-end demo in Task 10. Keep it thin.

- [ ] **Step 1: Implement the agent loop** — `internal/agent/agent.go`

```go
package agent

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"time"

	"github.com/google/uuid"

	"github.com/craig/composectl/internal/agent/dockerd"
	"github.com/craig/composectl/internal/store"
)

type Config struct {
	ControlPlaneURL string
	Org             string
	Hostname        string
	AdvertiseAddr   string
	AgentToken      string
	DockerHost      string
	CPUMillis       int
	MemoryBytes     int64
	PollInterval    time.Duration
	Secrets         map[string]string
}

// Run registers this node and then reconciles on a ticker until ctx is done.
// The agent speaks only HTTP to the control plane — it never touches Postgres,
// preserving the store's exclusive ownership of pgx across binaries.
func Run(ctx context.Context, cfg Config, log *slog.Logger) error {
	drv, err := dockerd.New(cfg.DockerHost, EnvSecrets(cfg.Secrets))
	if err != nil {
		return err
	}
	rec := NewReconciler(drv)
	c := &cpClient{base: cfg.ControlPlaneURL, token: cfg.AgentToken, http: &http.Client{Timeout: 30 * time.Second}}

	nodeID, err := c.register(ctx, cfg)
	if err != nil {
		return fmt.Errorf("register: %w", err)
	}
	log.Info("agent registered", "node", nodeID, "org", cfg.Org)

	ticker := time.NewTicker(cfg.PollInterval)
	defer ticker.Stop()
	for {
		if err := c.reconcileTick(ctx, nodeID, rec, cfg, log); err != nil {
			log.Warn("reconcile tick failed", "err", err)
		}
		select {
		case <-ctx.Done():
			return nil
		case <-ticker.C:
		}
	}
}

type cpClient struct {
	base  string
	token string
	http  *http.Client
}

func (c *cpClient) register(ctx context.Context, cfg Config) (uuid.UUID, error) {
	var out struct {
		ID uuid.UUID `json:"id"`
	}
	err := c.do(ctx, http.MethodPost, "/v1/nodes/register", map[string]any{
		"org": cfg.Org, "hostname": cfg.Hostname, "advertise_addr": cfg.AdvertiseAddr,
		"cpu_millis": cfg.CPUMillis, "memory_bytes": cfg.MemoryBytes, "agent_version": "sprint2-a",
	}, &out)
	return out.ID, err
}

func (c *cpClient) reconcileTick(ctx context.Context, nodeID uuid.UUID, rec *Reconciler, cfg Config, log *slog.Logger) error {
	var desired struct {
		Instances []store.DesiredInstance `json:"instances"`
	}
	if err := c.do(ctx, http.MethodGet, "/v1/nodes/"+nodeID.String()+"/desired-state", nil, &desired); err != nil {
		return err
	}
	reports := rec.Reconcile(ctx, desired.Instances)
	if len(reports) > 0 {
		if err := c.do(ctx, http.MethodPost, "/v1/nodes/"+nodeID.String()+"/report",
			map[string]any{"instances": toReportDTO(reports)}, nil); err != nil {
			return err
		}
	}
	return c.do(ctx, http.MethodPost, "/v1/nodes/"+nodeID.String()+"/heartbeat",
		map[string]any{"alloc_cpu_millis": 0, "alloc_memory_bytes": 0}, nil)
}

func toReportDTO(reports []Report) []map[string]any {
	out := make([]map[string]any, len(reports))
	for i, r := range reports {
		out[i] = map[string]any{
			"instance_id": r.InstanceID, "state": string(r.State),
			"container_id": r.ContainerID, "health_status": r.HealthStatus,
			"last_error": r.LastError, "restart_count": r.RestartCount, "set_started": r.SetStarted,
		}
	}
	return out
}

func (c *cpClient) do(ctx context.Context, method, path string, body, out any) error {
	var rdr io.Reader
	if body != nil {
		b, _ := json.Marshal(body)
		rdr = bytes.NewReader(b)
	}
	req, err := http.NewRequestWithContext(ctx, method, c.base+path, rdr)
	if err != nil {
		return err
	}
	if body != nil {
		req.Header.Set("Content-Type", "application/json")
	}
	if c.token != "" {
		req.Header.Set("Authorization", "Bearer "+c.token)
	}
	resp, err := c.http.Do(req)
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	if resp.StatusCode >= 300 {
		msg, _ := io.ReadAll(resp.Body)
		return fmt.Errorf("%s %s: %s: %s", method, path, resp.Status, bytes.TrimSpace(msg))
	}
	if out != nil {
		return json.NewDecoder(resp.Body).Decode(out)
	}
	return nil
}
```

- [ ] **Step 2: Implement config** — `internal/agent/config.go`

Config loading lives in package `agent`, **not** `internal/config`. If it lived
in `internal/config`, that package would import `internal/agent` (for `Config`),
and since the control plane imports `internal/config`, the Docker SDK would be
dragged transitively into the control-plane binary — breaking the boundary the
design rests on. Keeping it here means only the agent binary links the SDK.

```go
package agent

import (
	"fmt"
	"os"
	"runtime"
	"strconv"
	"time"
)

// LoadConfig reads the node agent's configuration from the environment.
func LoadConfig() (Config, error) {
	host, _ := os.Hostname()
	cfg := Config{
		ControlPlaneURL: envOr("COMPOSECTL_CONTROLPLANE_URL", "http://controlplane:8417"),
		Org:             envOr("COMPOSECTL_ORG", "dev"),
		Hostname:        envOr("COMPOSECTL_NODE_HOSTNAME", host),
		AdvertiseAddr:   envOr("COMPOSECTL_ADVERTISE_ADDR", "127.0.0.1"),
		AgentToken:      os.Getenv("COMPOSECTL_AGENT_TOKEN"),
		DockerHost:      os.Getenv("DOCKER_HOST"),
		CPUMillis:       intEnv("COMPOSECTL_NODE_CPU_MILLIS", runtime.NumCPU()*1000),
		MemoryBytes:     int64(intEnv("COMPOSECTL_NODE_MEMORY_MB", 8192)) << 20,
		PollInterval:    time.Duration(intEnv("COMPOSECTL_POLL_SECONDS", 2)) * time.Second,
		Secrets:         parseSecrets(os.Getenv("COMPOSECTL_DEV_SECRETS")),
	}
	if cfg.ControlPlaneURL == "" {
		return Config{}, fmt.Errorf("COMPOSECTL_CONTROLPLANE_URL is required")
	}
	return cfg, nil
}

func envOr(key, def string) string {
	if v := os.Getenv(key); v != "" {
		return v
	}
	return def
}

func intEnv(key string, def int) int {
	if v := os.Getenv(key); v != "" {
		if n, err := strconv.Atoi(v); err == nil {
			return n
		}
	}
	return def
}

// parseSecrets reads "k1=v1,k2=v2" into the dev secret map. Dev-only; the real
// encrypted store is Sprint 3.
func parseSecrets(raw string) map[string]string {
	out := map[string]string{}
	for _, pair := range splitComma(raw) {
		if i := indexByte(pair, '='); i > 0 {
			out[pair[:i]] = pair[i+1:]
		}
	}
	return out
}

func splitComma(s string) []string {
	if s == "" {
		return nil
	}
	var out []string
	start := 0
	for i := 0; i < len(s); i++ {
		if s[i] == ',' {
			out = append(out, s[start:i])
			start = i + 1
		}
	}
	return append(out, s[start:])
}

func indexByte(s string, b byte) int {
	for i := 0; i < len(s); i++ {
		if s[i] == b {
			return i
		}
	}
	return -1
}
```

- [ ] **Step 3: Implement entrypoint** — `cmd/agent/main.go`

```go
// Command agent runs the composectl node agent: it reconciles the local Docker
// daemon to the control plane's desired state for this node.
package main

import (
	"context"
	"log/slog"
	"os"
	"os/signal"
	"syscall"

	"github.com/craig/composectl/internal/agent"
)

func main() {
	log := slog.New(slog.NewTextHandler(os.Stdout, &slog.HandlerOptions{Level: slog.LevelInfo}))

	cfg, err := agent.LoadConfig()
	if err != nil {
		log.Error("config", "err", err)
		os.Exit(1)
	}
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()

	if err := agent.Run(ctx, cfg, log); err != nil {
		log.Error("agent exited", "err", err)
		os.Exit(1)
	}
}
```

- [ ] **Step 4: Verify build**

Run: `go build ./... && go vet ./...`
Expected: clean. Then confirm the control-plane binary does **not** link the
Docker SDK — the boundary check for this whole slice:

Run: `go list -deps ./cmd/controlplane | grep 'docker/docker' && echo LEAK || echo clean`
Expected: `clean`. (Config loading lives in package `agent`, so only
`cmd/agent` pulls in the SDK.)

- [ ] **Step 5: Commit**

```bash
git add internal/agent/agent.go cmd/agent/main.go internal/config/agent.go
git commit -m "feat(agent): registration + poll/reconcile/report loop and entrypoint"
```

---

## Task 7: Scheduler

**Files:**
- Create: `internal/rollout/scheduler.go`
- Create: `internal/rollout/rollout_test.go` (shared by Tasks 7 & 8)

**Interfaces:**
- Consumes: `store` methods from Tasks 1/2, `spec.DeploymentSpec.PeakMemoryBytes`.
- Produces:
  - `type Scheduler struct { st *store.Store; log *slog.Logger }`
  - `func NewScheduler(st *store.Store, log *slog.Logger) *Scheduler`
  - `func (sc *Scheduler) ScheduleOnce(ctx) error` — one pass over pending deployments

- [ ] **Step 1: Write the failing test** — `internal/rollout/rollout_test.go`

```go
package rollout

import (
	"context"
	"os"
	"testing"
	"time"

	"github.com/google/uuid"

	"github.com/craig/composectl/internal/spec"
	"github.com/craig/composectl/internal/store"
)

func testStore(t *testing.T) *store.Store {
	t.Helper()
	dsn := os.Getenv("COMPOSECTL_TEST_DATABASE_URL")
	if dsn == "" {
		dsn = "postgres://composectl:composectl@localhost:5473/composectl?sslmode=disable"
	}
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	st, err := store.New(ctx, dsn)
	if err != nil {
		t.Skipf("postgres unreachable — run make up: %v", err)
	}
	t.Cleanup(st.Close)
	return st
}

func ctx(t *testing.T) context.Context {
	c, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	t.Cleanup(cancel)
	return c
}

// fixture builds a pending deployment on a ready node and returns ids. It
// cleans up its org on completion (cascades to everything below).
func fixture(t *testing.T, st *store.Store) (deployID, nodeID, orgID uuid.UUID) {
	t.Helper()
	slug := "rollout-" + uuid.NewString()[:8]
	org, err := st.CreateOrganization(ctx(t), slug, "Rollout")
	if err != nil {
		t.Fatalf("org: %v", err)
	}
	t.Cleanup(func() {
		c, cancel := context.WithTimeout(context.Background(), 10*time.Second)
		defer cancel()
		st.Pool().Exec(c, `UPDATE environments SET live_deployment_id=NULL
			WHERE stack_id IN (SELECT s.id FROM stacks s JOIN applications a ON s.app_id=a.id WHERE a.org_id=$1)`, org.ID)
		st.Pool().Exec(c, `DELETE FROM organizations WHERE id=$1`, org.ID)
	})
	app, _ := st.CreateApplication(ctx(t), org.ID, slug, "app")
	stack, _ := st.CreateStack(ctx(t), app.ID, slug)
	sv, _ := st.CreateStackVersion(ctx(t), stack.ID, "raw", fixtureSpec(), "t")
	env, _ := st.CreateEnvironment(ctx(t), store.CreateEnvironmentParams{StackID: stack.ID, Slug: "prod"})
	dep, _ := st.CreateDeployment(ctx(t), store.CreateDeploymentParams{
		EnvironmentID: env.ID, StackVersionID: sv.ID, ResolvedSpec: sv.Spec, CreatedBy: "t",
	})
	node, _ := st.RegisterNode(ctx(t), store.RegisterNodeParams{
		OrgID: org.ID, Hostname: slug, AdvertiseAddr: "10.0.0.1",
		CPUMillis: 8000, MemoryBytes: 16 << 30,
	})
	return dep.ID, node.ID, org.ID
}

func fixtureSpec() *spec.DeploymentSpec {
	return &spec.DeploymentSpec{
		SpecVersion: spec.SpecVersion,
		Services: map[string]spec.Service{
			"api": {Name: "api", Image: "nginx:alpine", Swappable: true,
				Limits: spec.ResourceLimit{CPUMillis: 250, MemoryBytes: 256 << 20}},
			"db": {Name: "db", Image: "postgres:16-alpine", Swappable: false,
				Limits: spec.ResourceLimit{CPUMillis: 250, MemoryBytes: 256 << 20}},
		},
	}
}

func TestSchedulerPlacesPendingDeployment(t *testing.T) {
	st := testStore(t)
	depID, nodeID, _ := fixture(t, st)

	sc := NewScheduler(st, discardLog())
	if err := sc.ScheduleOnce(ctx(t)); err != nil {
		t.Fatalf("ScheduleOnce: %v", err)
	}

	dep, _ := st.GetDeployment(ctx(t), depID)
	if dep.State != store.DeployScheduling {
		t.Fatalf("expected scheduling, got %s", dep.State)
	}
	desired, _ := st.DesiredStateForNode(ctx(t), nodeID)
	if len(desired) != 2 {
		t.Fatalf("expected 2 instances written, got %d", len(desired))
	}
}
```

Add a `discardLog` helper to the test file:

```go
func discardLog() *slog.Logger { return slog.New(slog.NewTextHandler(io.Discard, nil)) }
```

imports: `"io"`, `"log/slog"`.

- [ ] **Step 2: Run it, verify it fails** — `NewScheduler` undefined.

Run: `go test ./internal/rollout/ -run Scheduler -count=1`

- [ ] **Step 3: Implement** — `internal/rollout/scheduler.go`

```go
// Package rollout runs the two control-plane loops that turn a pending
// deployment into running, health-gated containers: the scheduler (placement)
// and the controller (health aggregation, promotion, teardown).
package rollout

import (
	"context"
	"fmt"
	"log/slog"

	"github.com/craig/composectl/internal/store"
)

type Scheduler struct {
	st  *store.Store
	log *slog.Logger
}

func NewScheduler(st *store.Store, log *slog.Logger) *Scheduler {
	return &Scheduler{st: st, log: log}
}

// ScheduleOnce places every pending deployment onto a ready node and writes its
// desired instance rows. A deployment with no ready node is left pending to
// retry next tick; one that cannot fit is failed. Placement is trivial in
// Sprint 2 (first ready node); Sprint 4 adds scoring.
func (sc *Scheduler) ScheduleOnce(ctx context.Context) error {
	pending, err := sc.st.ListPendingDeployments(ctx)
	if err != nil {
		return err
	}
	for _, dep := range pending {
		if err := sc.place(ctx, dep); err != nil {
			sc.log.Warn("placement failed", "deployment", dep.ID, "err", err)
		}
	}
	return nil
}

func (sc *Scheduler) place(ctx context.Context, dep store.PendingDeployment) error {
	nodes, err := sc.st.ListReadyNodes(ctx, dep.OrgID)
	if err != nil {
		return err
	}
	if len(nodes) == 0 {
		sc.log.Info("no ready node; leaving pending", "deployment", dep.ID)
		return nil // retried next tick
	}

	peak := dep.ResolvedSpec.PeakMemoryBytes()
	var chosen *store.Node
	for i := range nodes {
		if nodes[i].FreeMemoryBytes() >= peak {
			chosen = &nodes[i]
			break
		}
	}
	if chosen == nil {
		reason := fmt.Sprintf("no ready node has %d bytes free for the rollout", peak)
		return sc.st.UpdateDeploymentState(ctx, dep.ID, store.DeployFailed, reason)
	}

	insts := make([]store.NewInstance, 0, len(dep.ResolvedSpec.Services))
	for name, svc := range dep.ResolvedSpec.Services {
		insts = append(insts, store.NewInstance{
			ServiceName: name, Swappable: svc.Swappable, ImageRef: svc.Image,
		})
	}
	if err := sc.st.CreateServiceInstances(ctx, dep.ID, chosen.ID, insts); err != nil {
		return err
	}
	return sc.st.UpdateDeploymentState(ctx, dep.ID, store.DeployScheduling, "")
}
```

- [ ] **Step 4: Run tests, verify pass**

Run: `go test ./internal/rollout/ -run Scheduler -count=1 -v`
Expected: PASS.

- [ ] **Step 5: Commit**

```bash
git add internal/rollout/scheduler.go internal/rollout/rollout_test.go
git commit -m "feat(rollout): scheduler — placement, capacity check, desired instances"
```

---

## Task 8: Controller

**Files:**
- Create: `internal/rollout/controller.go`
- Modify: `internal/rollout/rollout_test.go` (add controller tests)

**Interfaces:**
- Consumes: `store.InstanceStates`, `store.ListRolloutsInState`, `store.UpdateDeploymentState`, `store.DeleteInstances`.
- Produces:
  - `type Controller struct { st *store.Store; log *slog.Logger; startTimeout time.Duration }`
  - `func NewController(st *store.Store, log *slog.Logger) *Controller`
  - `func (c *Controller) ReconcileOnce(ctx) error`

- [ ] **Step 1: Write the failing test** — append to `internal/rollout/rollout_test.go`

```go
func advance(t *testing.T, st *store.Store, id uuid.UUID, states ...store.DeploymentState) {
	t.Helper()
	for _, s := range states {
		if err := st.UpdateDeploymentState(ctx(t), id, s, ""); err != nil {
			t.Fatalf("advance to %s: %v", s, err)
		}
	}
}

func reportAll(t *testing.T, st *store.Store, nodeID uuid.UUID, state store.InstanceState, health string) {
	t.Helper()
	desired, _ := st.DesiredStateForNode(ctx(t), nodeID)
	for _, d := range desired {
		if err := st.ReportInstance(ctx(t), d.InstanceID, store.ObservedInstance{
			State: state, ContainerID: "c-" + d.ServiceName, HealthStatus: health, SetStarted: true,
		}); err != nil {
			t.Fatalf("report: %v", err)
		}
	}
}

func TestControllerDrivesSchedulingToHealthy(t *testing.T) {
	st := testStore(t)
	depID, nodeID, _ := fixture(t, st)
	if err := NewScheduler(st, discardLog()).ScheduleOnce(ctx(t)); err != nil {
		t.Fatalf("schedule: %v", err)
	}
	c := NewController(st, discardLog())

	// Instances still pending → controller cannot advance past scheduling.
	if err := c.ReconcileOnce(ctx(t)); err != nil {
		t.Fatalf("reconcile: %v", err)
	}
	if dep, _ := st.GetDeployment(ctx(t), depID); dep.State != store.DeployScheduling {
		t.Fatalf("expected still scheduling, got %s", dep.State)
	}

	// All instances have containers now → starting.
	reportAll(t, st, nodeID, store.InstanceStarting, "starting")
	_ = c.ReconcileOnce(ctx(t))
	if dep, _ := st.GetDeployment(ctx(t), depID); dep.State != store.DeployStarting {
		t.Fatalf("expected starting, got %s", dep.State)
	}

	// All healthy → healthy.
	reportAll(t, st, nodeID, store.InstanceRunning, "healthy")
	_ = c.ReconcileOnce(ctx(t))
	if dep, _ := st.GetDeployment(ctx(t), depID); dep.State != store.DeployHealthy {
		t.Fatalf("expected healthy, got %s", dep.State)
	}
}

func TestControllerFailsOnInstanceFailure(t *testing.T) {
	st := testStore(t)
	depID, nodeID, _ := fixture(t, st)
	_ = NewScheduler(st, discardLog()).ScheduleOnce(ctx(t))
	reportAll(t, st, nodeID, store.InstanceFailed, "exited")

	if err := NewController(st, discardLog()).ReconcileOnce(ctx(t)); err != nil {
		t.Fatalf("reconcile: %v", err)
	}
	dep, _ := st.GetDeployment(ctx(t), depID)
	if dep.State != store.DeployFailed {
		t.Fatalf("expected failed, got %s", dep.State)
	}
	// Teardown deleted the instance rows.
	if states, _ := st.InstanceStates(ctx(t), depID); len(states) != 0 {
		t.Fatalf("expected instances torn down, got %v", states)
	}
}

func TestControllerTearsDownSuperseded(t *testing.T) {
	st := testStore(t)
	depID, nodeID, _ := fixture(t, st)
	_ = NewScheduler(st, discardLog()).ScheduleOnce(ctx(t))
	reportAll(t, st, nodeID, store.InstanceRunning, "healthy")
	// Drive to superseded directly to simulate a promotion having moved past it.
	advance(t, st, depID, store.DeployStarting, store.DeployHealthy, store.DeployLive, store.DeploySuperseded)

	if err := NewController(st, discardLog()).ReconcileOnce(ctx(t)); err != nil {
		t.Fatalf("reconcile: %v", err)
	}
	if states, _ := st.InstanceStates(ctx(t), depID); len(states) != 0 {
		t.Fatalf("superseded deployment instances must be torn down, got %v", states)
	}
}
```

- [ ] **Step 2: Run it, verify it fails** — `NewController` undefined.

Run: `go test ./internal/rollout/ -run Controller -count=1`

- [ ] **Step 3: Implement** — `internal/rollout/controller.go`

```go
package rollout

import (
	"context"
	"log/slog"
	"time"

	"github.com/craig/composectl/internal/store"
)

type Controller struct {
	st           *store.Store
	log          *slog.Logger
	startTimeout time.Duration
}

func NewController(st *store.Store, log *slog.Logger) *Controller {
	return &Controller{st: st, log: log, startTimeout: 5 * time.Minute}
}

// ReconcileOnce advances every active rollout by the aggregate of its
// instances, and tears down deployments that have left the live path. The
// deployment state machine (enforced in SQL) rejects any illegal nudge, so this
// only ever proposes the next legal step.
func (c *Controller) ReconcileOnce(ctx context.Context) error {
	active, err := c.st.ListRolloutsInState(ctx,
		store.DeployScheduling, store.DeployStarting, store.DeployHealthy)
	if err != nil {
		return err
	}
	for _, dep := range active {
		if err := c.advance(ctx, dep); err != nil {
			c.log.Warn("advance failed", "deployment", dep.ID, "err", err)
		}
	}

	// Teardown: a superseded or failed deployment's instance rows are deleted;
	// the agent then GCs their swappable containers. Pinned containers survive
	// because the now-live deployment still holds its own rows for them.
	terminal, err := c.st.ListRolloutsInState(ctx, store.DeploySuperseded, store.DeployFailed)
	if err != nil {
		return err
	}
	for _, dep := range terminal {
		if err := c.st.DeleteInstances(ctx, dep.ID); err != nil {
			c.log.Warn("teardown failed", "deployment", dep.ID, "err", err)
		}
	}
	return nil
}

func (c *Controller) advance(ctx context.Context, dep store.Deployment) error {
	states, err := c.st.InstanceStates(ctx, dep.ID)
	if err != nil {
		return err
	}
	if len(states) == 0 {
		return nil // scheduler has not written instances yet
	}

	var pending, failed, healthy int
	for _, s := range states {
		switch s {
		case store.InstancePending:
			pending++
		case store.InstanceFailed, store.InstanceUnhealthy:
			failed++
		case store.InstanceRunning:
			healthy++
		}
	}

	switch {
	case failed > 0:
		return c.st.UpdateDeploymentState(ctx, dep.ID, store.DeployFailed, "an instance failed to start")
	case dep.State == store.DeployScheduling && pending == 0:
		// Every instance has a container (moved past pending) → starting.
		return c.st.UpdateDeploymentState(ctx, dep.ID, store.DeployStarting, "")
	case dep.State == store.DeployStarting && healthy == len(states):
		return c.st.UpdateDeploymentState(ctx, dep.ID, store.DeployHealthy, "")
	case dep.State == store.DeployStarting && time.Since(dep.UpdatedAt) > c.startTimeout:
		return c.st.UpdateDeploymentState(ctx, dep.ID, store.DeployFailed, "timed out waiting for health")
	}
	// Slice A stops at healthy. Slice B adds: healthy → rewrite Traefik →
	// PromoteDeployment → the terminal teardown above handles the old revision.
	return nil
}
```

- [ ] **Step 4: Run tests, verify pass**

Run: `go test ./internal/rollout/ -count=1 -v`
Expected: PASS (scheduler + all controller tests).

- [ ] **Step 5: Commit**

```bash
git add internal/rollout/controller.go internal/rollout/rollout_test.go
git commit -m "feat(rollout): controller — health aggregation, failure, teardown"
```

---

## Task 9: Wire the loops into the control plane

**Files:**
- Modify: `cmd/controlplane/main.go`, `internal/config/config.go`, `internal/api/server.go`

**Interfaces:**
- Consumes: `rollout.NewScheduler`, `rollout.NewController`, `Server.bootstrapDevOrg`.
- Produces: control plane that, on startup, bootstraps the dev org and runs both loops on a ticker until shutdown.

- [ ] **Step 1: Add tick config** — in `internal/config/config.go`, extend `Config` and `Load`:

```go
	// TickInterval paces the scheduler and rollout controller loops.
	TickInterval time.Duration
```

and in `Load`, after the existing fields:

```go
	c.TickInterval = time.Duration(intEnv("COMPOSECTL_TICK_SECONDS", 1)) * time.Second
```

The `config` package needs its own `intEnv` (the agent's copy lives in package
`agent` now, and is unexported). Add to `config.go`:

```go
func intEnv(key string, def int) int {
	if v := os.Getenv(key); v != "" {
		if n, err := strconv.Atoi(v); err == nil {
			return n
		}
	}
	return def
}
```

Add `"time"` and `"strconv"` to `config.go`'s imports.

- [ ] **Step 2: Run the loops** — modify `cmd/controlplane/main.go` `run`, after `st` is created and before/around the HTTP server:

```go
	srvHandler := api.NewServer(st, log)

	// Bootstrap the dev org the local agent registers into.
	bootCtx, bootCancel := context.WithTimeout(ctx, 5*time.Second)
	srvHandler.BootstrapDevOrg(bootCtx)
	bootCancel()

	// Scheduler + rollout controller: two loops over the deployment lifecycle.
	sched := rollout.NewScheduler(st, log)
	ctrl := rollout.NewController(st, log)
	go runLoop(ctx, cfg.TickInterval, log, "scheduler", sched.ScheduleOnce)
	go runLoop(ctx, cfg.TickInterval, log, "controller", ctrl.ReconcileOnce)

	srv := &http.Server{
		Addr:              cfg.ListenAddr,
		Handler:           srvHandler,
		ReadHeaderTimeout: 5 * time.Second,
		ReadTimeout:       30 * time.Second,
		WriteTimeout:      60 * time.Second,
		IdleTimeout:       120 * time.Second,
	}
```

Add the loop helper to `main.go`:

```go
func runLoop(ctx context.Context, every time.Duration, log *slog.Logger, name string, tick func(context.Context) error) {
	t := time.NewTicker(every)
	defer t.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-t.C:
			c, cancel := context.WithTimeout(ctx, 30*time.Second)
			if err := tick(c); err != nil {
				log.Warn("loop tick failed", "loop", name, "err", err)
			}
			cancel()
		}
	}
}
```

Add imports to `main.go`: `"github.com/craig/composectl/internal/rollout"`.

Rename the `NewServer`-created handler usage from the current inline `api.NewServer(st, log)` in the `http.Server{Handler: ...}` to the `srvHandler` variable created above.

- [ ] **Step 3: (already exported)** — `BootstrapDevOrg` was defined exported in Task 3, so `cmd/controlplane/main.go` calls it directly. Nothing to rename here; just confirm the Step 2 call compiles.

- [ ] **Step 4: Verify build + full suite**

Run: `go build ./... && go vet ./... && gofmt -l . && go test ./... -count=1`
Expected: build/vet/fmt clean; parser, store, api, rollout tests PASS (or SKIP without Postgres/Docker).

- [ ] **Step 5: Commit**

```bash
git add cmd/controlplane/main.go internal/config/config.go internal/api/server.go internal/api/nodes_test.go
git commit -m "feat(controlplane): run scheduler + controller loops, bootstrap dev org"
```

---

## Task 10: Compose wiring + demo (retire the SQL fakery)

**Files:**
- Modify: `compose.yaml` (add `agent` service), `deploy/Dockerfile` (build the agent binary too), `scripts/demo.sh`, `Makefile`

**Interfaces:**
- Consumes: everything above. Produces the end-to-end proof.

- [ ] **Step 1: Build the agent image.** Add a second binary to `deploy/Dockerfile` build stage and a small agent image. Append after the controlplane build:

```dockerfile
RUN CGO_ENABLED=0 GOOS=linux go build -ldflags="-s -w" -o /out/composectl-agent ./cmd/agent
```

Add a second final stage (the agent needs the docker CLI-less client only; distroless is fine — it talks to the socket over HTTP):

```dockerfile
FROM gcr.io/distroless/static-debian12:nonroot AS agent
COPY --from=build /out/composectl-agent /composectl-agent
ENTRYPOINT ["/composectl-agent"]
```

Name the controlplane stage explicitly (`AS controlplane`) so compose can target each.

- [ ] **Step 2: Add the agent service** to `compose.yaml`:

```yaml
  agent:
    build:
      context: .
      dockerfile: deploy/Dockerfile
      target: agent
    depends_on:
      controlplane:
        condition: service_healthy
    environment:
      COMPOSECTL_CONTROLPLANE_URL: http://controlplane:8417
      COMPOSECTL_ORG: dev
      COMPOSECTL_NODE_HOSTNAME: dev-node-1
      COMPOSECTL_AGENT_TOKEN: dev-token-change-me
      # Dev secret source — Sprint 3 replaces this with the encrypted store.
      COMPOSECTL_DEV_SECRETS: db_password=devpassword
    volumes:
      - /var/run/docker.sock:/var/run/docker.sock
```

Note in a comment: the agent creates sibling containers on the host daemon; the platform already rejects bind mounts, so there are no host-path traps.

- [ ] **Step 3: Rewrite the demo** — `scripts/demo.sh`, replace the "Standing in for the Sprint 2 agent (SQL...)" block. The agent now drives `pending → healthy` for real; the script polls the API and asserts.

```bash
step "Waiting for the agent to bring the rollout to healthy"
DEADLINE=$((SECONDS + 120))
while :; do
  STATE=$(api GET "/v1/deployments/$DEP_ID" | jq -r .state)
  note "state=$STATE"
  case "$STATE" in
    healthy|live) break ;;
    failed) echo "rollout failed" >&2; api GET "/v1/deployments/$DEP_ID" | jq . >&2; exit 1 ;;
  esac
  [ $SECONDS -lt $DEADLINE ] || { echo "timed out waiting for healthy" >&2; exit 1; }
  sleep 3
done
note "reached $STATE with real containers"

step "Containers the agent started"
docker ps --filter "label=cc.deployment=$DEP_ID" --format '  {{.Names}}\t{{.Status}}'
```

The demo must deploy into the **dev** org (not a random one) so the agent's node matches. Change the org step:

```bash
step "Using the bootstrapped dev org"
ORG=$(api GET /v1/orgs | jq -r '.organizations[] | select(.slug=="dev") | .id')
note "org $ORG"
```

Keep the app/stack/version/env/deploy steps, but give the app/stack a unique slug per run (they can stay random). Remove the SQL-driven promote fakery; keep the real `POST /promote` after healthy.

- [ ] **Step 4: Add a failure demo** — new script `scripts/demo-failure.sh` (or a flag). It pushes a stack version with a bogus image and asserts the deployment reaches `failed` while any prior live deployment is untouched:

```bash
#!/usr/bin/env bash
set -euo pipefail
API=${API:-http://localhost:8417}
ORG=$(curl -sS $API/v1/orgs | jq -r '.organizations[]|select(.slug=="dev")|.id')
APP=$(curl -sS -X POST $API/v1/orgs/$ORG/apps -d "{\"slug\":\"fail-$RANDOM\",\"name\":\"f\"}" | jq -r .id)
STACK=$(curl -sS -X POST $API/v1/apps/$APP/stacks -d "{\"slug\":\"s-$RANDOM\"}" | jq -r .id)
printf 'services:\n  api:\n    image: ghcr.io/nonexistent/nope:0.0.0\n' \
  | curl -sS -X POST "$API/v1/stacks/$STACK/versions" --data-binary @- >/dev/null
ENV=$(curl -sS -X POST $API/v1/stacks/$STACK/envs -d '{"slug":"prod"}' | jq -r .id)
DEP=$(curl -sS -X POST $API/v1/envs/$ENV/deployments -d '{}' | jq -r .id)
for _ in $(seq 1 40); do
  S=$(curl -sS $API/v1/deployments/$DEP | jq -r .state)
  echo "state=$S"; [ "$S" = failed ] && { echo "✓ failed as expected, live untouched"; exit 0; }
  sleep 3
done
echo "did not reach failed" >&2; exit 1
```

- [ ] **Step 5: Makefile** — add:

```makefile
.PHONY: agent-logs
agent-logs: ## Tail the node agent logs
	docker compose logs -f agent

.PHONY: demo-failure
demo-failure: ## Show a failed rollout leaving the live deployment untouched
	API=$(API) ./scripts/demo-failure.sh
```

- [ ] **Step 6: End-to-end verification**

```bash
make nuke && make up
sleep 20                     # let migrate + controlplane + agent settle
make health
make demo                    # deploys into dev org; agent brings up real containers
docker ps --filter label=cc.env --format '{{.Names}}'   # cc-<env8>-r1-blue-api, cc-<env8>-pinned-db
make demo-failure            # failed rollout, live untouched
```

Expected: `make demo` reaches `healthy`/`live` with real containers listed and **no SQL drove the transitions**; `make demo-failure` reaches `failed`.

- [ ] **Step 7: Commit**

```bash
chmod +x scripts/demo-failure.sh
git add compose.yaml deploy/Dockerfile scripts/demo.sh scripts/demo-failure.sh Makefile
git commit -m "feat: run the agent in the dev stack; demo drives real rollouts end to end"
```

---

## Final verification (whole slice)

```bash
go build ./... && go vet ./... && gofmt -l .        # all clean
grep '^go ' go.mod                                   # still: go 1.23
go test ./... -count=1                               # parser+store+api+rollout pass or skip
make nuke && make up && sleep 20 && make demo        # real end-to-end rollout to healthy
```

Baseline that must not regress: `examples/webapp` digest `6072c68f…`, `db` pinned / `api`+`cache`+`worker` swappable, peak `2415919104`, and the Sprint 1 guards (409 on second active deployment, 409 promoting a non-healthy deployment).

## What Slice A deliberately leaves for later

- **Traefik + traffic flip + auto-promote** — Slice B. The controller stops at `healthy`; promotion stays manual (`POST /promote`, DB-only).
- **Rollback** — Slice C (`handleRollback` still 501).
- **NOTIFY-driven agent wake** — polling for now; the trigger stays for a future push endpoint.
- **Encrypted secrets** — Sprint 3; dev secret source in use.
- **Multi-node placement / capacity scoring** — Sprint 4; first-fit on one node for now.
