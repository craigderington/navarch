# Sprint 4 — Multi-Node Fleet, Mesh, and Placement

**Date:** 2026-08-15
**Builds on:** Sprint 2 (agent, rollout spine, routing, rollback), Sprint 3
(secrets, preview environments).
**Branch:** `sprint4-multi-node`

---

## Context

Everything above the node is already fleet-shaped. Nodes are org-scoped rows
with capacity and heartbeats, the scheduler owns placement and writes desired
`service_instances`, and the agent is a dumb reconciler that never learns where
anything else runs. What is missing is a second node.

That framing is misleading in one important way, and this sprint starts from the
correction: **the single-node assumption is not confined to the number of rows in
`nodes`. It is baked into three code paths that are silently wrong the moment a
second node registers.** Sprint 4 is therefore not "add nodes"; it is "remove the
assumptions, then add nodes".

## What is actually broken today

Verified against the code, not inferred:

1. **An environment is not bound to the node holding its state.**
   `PlaceDeployment(depID, nodeID, …)` chooses one node per deployment and
   consults nothing about where that environment's previous revision lives. With
   two nodes, revision 2 of an environment can be placed on node B while the
   pinned Postgres and its named volume are on node A. The agent on B creates a
   *fresh* pinned container with an *empty* volume, the app becomes healthy
   against it, and the controller auto-promotes and flips traffic. The original
   data is intact on A and unreferenced. **The rollout reports success.** This is
   the worst failure the platform can have and it needs no unusual conditions —
   just a second node and a second deploy.

2. **Secret recipients follow placement, not the environment.**
   `RecipientsForEnvironment` seals to the age recipients of nodes already
   running the environment, falling back to every ready node when nothing is
   placed. A deployment placed outside that set cannot decrypt. This fails
   loudly rather than silently, but it is the same fault line as (1).

3. **There is no failover.** `MarkStaleNodesUnreachable` flips a node to
   `unreachable` after 30s of heartbeat silence and does nothing about the
   deployments on it. They remain `live`, and the router keeps pointing at them.

4. **`advertise_addr` is dead weight.** Collected at registration, stored as
   `INET`, rendered by `navarch node list`, referenced by no logic. It is the
   WireGuard-shaped hole already cut in the schema.

5. **Ingress is single-daemon by construction.** Traefik reaches a tenant by
   joining that tenant's revision network, which is only possible on its own
   Docker daemon. It cannot reach a container on another node at all.

(1) and (2) must be fixed *before* a second node exists, which is why they are
Slice A and the dev fleet is Slice B rather than the other way round.

---

## Keystone decisions (settled)

1. **A deployment is placed whole, onto one node.** Services of a stack are never
   split across nodes. The fleet scales by hosting different stacks on different
   nodes, not by spreading one stack. This preserves the entire container model:
   services keep addressing each other by container name on a local revision
   bridge, and `EnsureNetwork`, naming, and reconcile are unchanged. Per-service
   spread was rejected for this sprint: it requires replacing the local bridge
   with a cross-node overlay plus cross-node service DNS, which rewrites the
   agent's networking wholesale for bin-packing we do not yet need.

2. **An environment has a home node, recorded explicitly.** A new
   `environments.home_node_id`, NULL until first placement and set in the same
   transaction that places the first deployment. Every later placement for that
   environment targets the home node or fails. This is chosen over deriving the
   node from the latest live deployment because inference is exactly what the
   codebase already refuses for teardown: durable state that must be found again
   is recorded, not reconstructed.

3. **One ingress node, reached over the mesh.** Traefik stays in one place and
   reaches tenant ingress containers on other nodes across WireGuard. One route
   table, one place to terminate TLS when that eventually lands, and the
   router-joins-the-tenant model degrades gracefully into a mesh route. The
   accepted cost is a single point of failure for ingress; a router per node was
   rejected for this sprint because it multiplies both the route table and the
   future certificate story by the fleet size.

4. **The dev fleet is Docker-in-Docker.** Each agent gets its own daemon, so
   nodes are genuinely separate hosts with separate network namespaces. Several
   agents against one host daemon was rejected: it cannot exercise the mesh at
   all, and reconcile's GC is env-scoped rather than node-scoped, so co-tenant
   agents would delete each other's containers.

5. **Placement is scored, not first-fit.** The current loop takes the first ready
   node with room. Scoring adds spread (prefer the node hosting fewest
   environments) over free capacity ratio, with a deterministic tie-break so the
   same fleet state always yields the same choice — a scheduler that is not
   reproducible cannot be tested.

## Non-goals (explicit)

- **No moving an environment between nodes.** A home node is assigned once. Drain
  and failover for environments holding durable state require moving volumes,
  which is a data-migration problem, not a scheduling one. Slice D covers only
  what can be rescheduled safely: environments with no durable state.
- **No cross-node overlay networking.** Decision 1.
- **No TLS, no ACME, no public DNS.** The internet edge remains out of scope; the
  fleet is reachable exactly as it is today. This is tracked separately and is
  the largest remaining product gap.
- **No autoscaling or node provisioning.** Nodes register themselves; nothing
  creates them.
- **No per-service resource rebalancing.** Capacity accounting stays whole-stack.

---

## Architecture

### Placement

```
pending deployment
      │
      ├─ environment has home_node_id?  ── yes ─▶ that node (or fail loudly)
      │
      └─ no ─▶ score every ready node in the org
                   spread   : fewest environments homed
                   capacity : free memory and millicpu ratio
                   tie-break: node id, ascending  (reproducible)
               ─▶ place, and set home_node_id in the same transaction
```

The capacity check that exists today (`FreeMemoryBytes`/`FreeCPUMillis` against
`PeakMemoryBytes`/`PeakCPUMillis`) is retained as a hard filter; scoring only
orders the nodes that already fit. A home node that no longer fits is a failed
deployment with a clear reason, **not** a silent relocation — relocating is the
data-loss bug in a different costume.

### Mesh

The control plane is the source of truth for mesh membership: a node registers
with a WireGuard public key, receives a mesh address from a per-org pool, and
polls its peer list alongside desired state. `advertise_addr` finally carries
meaning. The agent configures the interface; the control plane never holds a
private key, matching the age-identity precedent.

### Cross-node ingress — one fork left open

Traefik must reach an ingress container on another node. Two routes, to be
settled at the start of Slice C rather than now, because neither blocks A or B:

- **(a) Mesh-bound published port.** The agent publishes the ingress container on
  a port bound to the node's WireGuard interface only, and the router targets
  `{node_mesh_ip}:{port}`. Container networking stays local and unchanged. The
  "no host ports" rule is a restriction on *tenant compose files*, not on the
  platform, so this does not violate it — but that distinction should be written
  down where the rule is stated.
- **(b) Routed container subnets.** Each node gets a non-overlapping Docker
  address pool from the control plane, and container subnets are routed over the
  mesh. More faithful, and it makes any future overlay work cheaper, but it
  requires managing each node's Docker daemon configuration.

(a) is the current preference on effort and blast radius.

---

## Slices

- **Slice A — environment affinity and scored placement.** `home_node_id`,
  placement targeting it, secret recipients keyed to it, scoring with spread.
  Almost entirely testable today: store tests can fabricate many node rows
  without a single extra daemon, which is why this lands before the dev fleet.
- **Slice B — the dev fleet.** DinD nodes in `compose.yaml`, agent registration
  per node, `make up` bringing up a two-node fleet, and a demo that shows two
  environments landing on different nodes.
- **Slice C — WireGuard mesh and cross-node ingress.** Key exchange, address
  allocation, peer distribution, and the fork above; ends with a stack on node B
  served through the ingress node.
- **Slice D — drain and failover.** `POST /v1/nodes/{id}/drain` currently marks a
  node and nothing else. Give it meaning for environments without durable state,
  and make an unreachable node's deployments visible as such rather than
  silently "live".
