# Sprint 2 — Node Agent & Blue/Green Rollouts

**Status:** design approved, pending spec review
**Date:** 2026-07-23
**Depends on:** Sprint 1 (control plane, parser, store, catalog + deployment endpoints)

---

## Context

Sprint 1 built a control plane that can parse a compose stack, store versioned
revisions, and record deployments with an append-only state machine — but
nothing runs. A deployment reaches `pending` and stops; `make demo` fakes the
`pending → healthy` transitions with raw SQL because there is no agent to make
them real.

Sprint 2 is where blue/green stops being a data model and becomes running
containers. A node agent drives a real Docker daemon to bring a stack up per
revision, a control-plane scheduler decides placement, and a rollout controller
health-gates the new revision and flips traffic to it with zero downtime — then
tears the old one down. The intended end state: push a new stack version and
watch it go live by itself, with the previous revision serving throughout and
left untouched if the new one fails.

## Keystone decisions (settled in brainstorming)

1. **Real Docker, single node.** The agent drives the local Docker daemon via
   its API to start/stop real containers. One node stands in for the fleet;
   multi-node placement is Sprint 4. Traefik runs as a real service in the dev
   compose stack for the traffic flip.
2. **Control-plane scheduler owns placement.** A background loop turns a
   `pending` deployment into desired `service_instances` rows and picks the
   node. The agent is a dumb reconciler: read my node's desired state, make
   Docker match, report what I observe. This matches the existing
   `desired-state` / `report` endpoints and the `service_instances` NOTIFY
   trigger, and keeps placement — a cross-node concern — in the control plane
   where Sprint 4's scoring will extend it.
3. **Auto-promote via a rollout controller.** When every instance of the new
   revision reports healthy, the controller rewrites Traefik to the new slot,
   promotes atomically, and tears down the old slot's swappable containers. The
   existing manual `POST /promote` stays as an override.

## Non-goals (explicitly out of scope for Sprint 2)

- **Multi-node placement / WireGuard mesh** — Sprint 4. One node only.
- **Encrypted secret store.** The `secrets` table and per-environment injection
  are Sprint 3. Sprint 2 expands `${secret:KEY}` from a *trivial dev source*
  (see Slice A) purely so the example stack can start.
- **Preview environments** — Sprint 3.
- **Stateful (pinned) service migration.** A changed pinned service is recreated
  in place with brief downtime and flagged; true blue/green of durable state is
  not attempted (it is impossible on one volume by design).

---

## Slice decomposition

Sprint 2 is specced as one architecture but implemented in three independently
demoable, independently testable slices. Slice A is detailed in full below; B
and C are sketched and will get their own plan cycles.

- **Slice A — the reconciliation spine.** Real containers, health-gated, no
  routing. Scheduler + agent + Docker driver + health aggregation. Deploy →
  containers up → `healthy` → stop. Demo: `docker ps` shows the revision's
  containers and the deployment reaches `healthy` with no SQL fakery.
- **Slice B — routing & auto-promote.** Traefik as a real compose service, a
  config generator, and the rollout controller that flips traffic and
  auto-promotes. Demo: `curl` the env hostname, push a new version, watch the
  flip zero-downtime, old swappable containers torn down.
- **Slice C — rollback.** `POST /rollback` = re-deploy an older stack version as
  a new revision, reusing the whole A+B spine. Small once A+B exist.

---

## Component map

Each new package quarantines one external dependency behind one boundary, the
same discipline the codebase already lives by (compose-go in `internal/parser`,
pgx in `internal/store`).

| Package | Role | Sole importer of |
|---|---|---|
| `cmd/agent` | wire config + run the agent loop | — |
| `internal/agent` | reconciliation loop, one per node | — |
| `internal/agent/dockerd` | desired instance → container ops; expand secrets at start | **Docker SDK** |
| `internal/rollout` | scheduler (`pending→scheduling`, writes instances) **+** controller (aggregates health, auto-promotes, tears down) | — |
| `internal/router` | env hostname → live slot's ingress container (Traefik dynamic config) | **Traefik config schema** (Slice B) |
| `internal/store` | node + instance methods (below) | pgx (boundary unchanged) |

The scheduler and controller share `internal/rollout` because they are two loops
over the same deployment lifecycle. `internal/router` and the Docker SDK boundary
mean Traefik or the container runtime could be swapped without touching the
lifecycle logic.

---

## Instance model

The scheduler writes one `service_instances` row per (deployment, service) for
**every** service, pinned or swappable. This keeps health aggregation to a
single query — "are all of deployment R's instances healthy?" The
pinned/swappable difference lives entirely in what the **agent** does when it
reconciles a row:

- **swappable** → create a fresh container scoped to the project
  `cc-{env8}-r{R}-{slot}`. Blue and green each get their own.
- **pinned** → look for the environment-scoped container
  `cc-{env8}-pinned-{service}`. If it exists, **adopt** it: record its
  container_id, report its health, create nothing. If not (first deploy), create
  it under that stable name. Every deployment's row set is therefore complete,
  but the pinned container is created once and adopted forever after.

The pinned container's identity is revision-independent by name; its named
volume is `cc-{env8}-{volume}`, per-environment, so it survives every rollout.
This is the schema's "runs once, shared across revisions" rule made physical,
and it is exactly what `spec.PeakMemoryBytes()` already encodes (pinned ×1,
swappable ×2).

## Network topology

Each revision gets its own Docker network `cc-{env8}-r{R}-{slot}`, so blue's
`api` and green's `api` never collide on Docker DNS — satisfying "each revision
gets an isolated network."

- Swappable services of a revision join that revision's network and resolve each
  other by service name (`api` → `worker`).
- The pinned container is **attached to every revision network** as the agent
  reconciles each new revision. Green's `api` resolves `db` on green's network;
  blue's `api` resolves the *same* `db` container on blue's network. One
  database, both slots reach it.
- (Slice B) Traefik attaches to revision networks and routes the env hostname to
  the live revision's ingress container.

```
              cc-a1b2-pinned-db  (one container, one volume)
                 ▲                        ▲
     ┌───────────┴──────────┐   ┌─────────┴────────────┐
     │ net: cc-a1b2-r1-blue │   │ net: cc-a1b2-r2-green│
     │   api ─→ worker      │   │   api ─→ worker      │
     └──────────────────────┘   └──────────────────────┘
        (live, draining)             (new, health-gating)
```

The platform's Sprint 1 constraints are what make this possible: rejecting
`container_name`, bind mounts, and published ports removes exactly the
directives that would break per-revision naming and networking.

---

## State flow

The deployment state machine stays authoritative in SQL —
`store.UpdateDeploymentState`'s `WHERE state = ANY(...)` rejects illegal moves,
so a buggy component cannot drive a deployment backwards. Components only *nudge*
the deployment to its next legal state based on the instance aggregate.

| Transition | Driver | Basis |
|---|---|---|
| `pending → scheduling` | scheduler | pick node, write instance rows for all services, capacity-check `PeakMemoryBytes()` ≤ node free memory |
| agent wake | — | NOTIFY `node_{id}` + periodic resync (a missed NOTIFY must still converge) |
| instance `pending→pulling→starting→running` | agent | pull image, ensure network, create + attach + start container, poll `docker inspect` health |
| `scheduling → starting` | controller | every desired instance's container now exists |
| `starting → healthy` | controller | **all** the deployment's instances report healthy |
| `healthy → live` | controller (Slice B) | rewrite Traefik → `store.PromoteDeployment` (atomic, already exists) |
| `live → superseded` + teardown | controller | promotion supersedes the old revision; controller deletes the old deployment's instance rows |
| any active → `failed` | controller | an instance reports `failed` (see failure handling) |

### Reconciliation semantics — the load-bearing idea

Desired state = the `service_instances` rows for a node. The agent converges
Docker to match: any container labelled for this environment with **no backing
desired row is removed**. Teardown is therefore just *row deletion* — the
controller deletes the superseded deployment's rows, and the agent garbage-
collects the orphaned swappable containers on its next reconcile. The pinned
`db` container survives because the now-live deployment still holds its own
pinned row referencing the same stable-named container. One uniform rule, no
special-case teardown path.

Containers are labelled so the agent can list what it manages and identify
orphans: `cc.env={env8}`, `cc.deployment={id}`, `cc.service={name}`,
`cc.swappable={bool}`, `cc.pinned-key={env8}-{service}` (pinned only).

### Health criterion

A service that declares a healthcheck is healthy when `docker inspect` reports
`State.Health.Status == healthy`. A service with **no** healthcheck is healthy
when its container has stayed `running` for a short debounce (default 5s)
without exiting — the agent cannot know more than "it came up and stayed up."
The debounce keeps a container that crash-loops on startup from being reported
healthy in the gap before its first exit.

### Failure handling — blue is untouched

An image pull fails, a container exits non-zero past its restart cap, or a
health-checked service never reaches `healthy` within `start_period +
retries × interval` → the agent reports the instance `failed` → the controller
moves the deployment to `failed` with a reason. The live (blue) deployment never moved, so traffic keeps flowing. The
safety of blue/green is the *absence* of a promotion; it costs no extra
mechanism. A failed rollout's containers are torn down by the same
delete-rows-then-GC path.

---

## Store additions

No schema migration is required — every column already exists (`nodes`,
`service_instances`, the deployment state machine, `environments.live_deployment_id`).
If teardown ever needs a "draining" marker distinct from row deletion, add
migration `0002` (never edit `0001`); the reconciler model above avoids it.

New methods (`internal/store`, new files `nodes.go` and `instances.go`):

- **Nodes:** `RegisterNode` (upsert by `(org_id, hostname)`), `Heartbeat`
  (capacity + usage + `last_heartbeat`), `ListNodes(orgID)`,
  `ListReadyNodes(orgID)`.
- **Instances (desired):** `CreateServiceInstances(deploymentID, []NewInstance)`
  (batch insert), `DesiredStateForNode(nodeID)` — instance rows joined to their
  deployment's `resolved_spec` so the agent gets the full `spec.Service` (image,
  env, secret templates, ports, mounts, healthcheck, limits) plus project name,
  slot, network name and `env8`.
- **Instances (observed):** `ReportInstance(instanceID, Observed)` — updates
  container_id, state, health_status, restart_count, last_error, started_at.
- **Controller queries:** `ListPendingDeployments()`,
  `ListRolloutsInState(states...)`, `InstanceStates(deploymentID)` (for
  aggregation), `DeleteInstances(deploymentID)` (teardown).

## API — wire the 501 stubs

`internal/api/nodes.go` handlers become real, delegating to the new store
methods: `POST /v1/nodes/register`, `POST /v1/nodes/{id}/heartbeat`,
`GET /v1/nodes/{id}/desired-state`, `POST /v1/nodes/{id}/report`,
`GET /v1/nodes`. Rollback (`POST /v1/envs/{env}/rollback`) lands in Slice C.
Handlers stay thin — decode, delegate, encode — per the existing boundary.

Agent requests authenticate with the shared `COMPOSECTL_AGENT_TOKEN` already in
config; per-node mTLS is deferred (noted in `config.go` already).

---

## Agent runtime & ops

The agent runs as a container in `compose.yaml` with `/var/run/docker.sock`
mounted, creating sibling containers on the host daemon. The platform's own
constraints make this clean: bind mounts are already rejected, so there are no
host-path translation traps — only named volumes, which the host daemon owns.
The agent reaches the control plane at `controlplane:8417`.

Startup: the agent registers (`POST /v1/nodes/register`) with its hostname and
capacity (CPU/memory from the host, overridable by config), receives its
`node_id`, then loops: **wake** (NOTIFY on `node_{id}` or resync ticker) →
**fetch** desired state → **reconcile** Docker → **report** observed → heartbeat.

**Dev-org wrinkle (decided).** `nodes` are org-scoped, but `make demo` creates a
fresh random org each run, so one agent cannot belong to every org. Sprint 2
introduces a stable **dev org** (slug `dev`, agent configured with it, self-
registers into it); `make demo` deploys into that org instead of a random one.
Multi-org node pools are genuinely Sprint 4.

**Dev secret source (scope boundary).** So the example stack can start, the
agent expands `${secret:KEY}` templates from a trivial dev source — a static map
or the agent's own environment — using `spec.SecretRefPattern` (exported for
exactly this). The encrypted `secrets` table, KMS/age, and per-env injection are
Sprint 3. Plaintext still never touches the control plane or database; expansion
happens only in the agent at container start, as designed.

New config: agent (`COMPOSECTL_CONTROLPLANE_URL`, `COMPOSECTL_NODE_HOSTNAME`,
`COMPOSECTL_ORG`, `COMPOSECTL_AGENT_TOKEN`, `DOCKER_HOST`, resync interval);
control plane (scheduler/controller tick interval, default 1s).

---

## Testing strategy

Mirrors the Sprint 1 discipline: real dependencies, skip loudly when absent
(the store tests already skip without Postgres; Docker tests follow suit).

- **Docker driver (`internal/agent/dockerd`)** → integration tests against a
  real Docker daemon, skipped if unreachable. Cover: create from a desired
  instance, adopt an existing pinned container, network ensure + attach, health
  inspect, teardown, orphan GC. Use a tiny image (`busybox`/`nginx:alpine`) for
  speed.
- **Scheduler & controller (`internal/rollout`)** → against real Postgres with
  stubbed instance reports; neither touches Docker, so their logic tests without
  a daemon. Cover: pending→scheduling writes the right instance set (pinned once,
  swappable per slot), capacity rejection, health aggregation gating
  starting→healthy, failure propagation, teardown row deletion.
- **Store** → real Postgres, per the existing pattern (unique-slug orgs,
  cascade cleanup).
- **End-to-end** → `make demo` **retires the SQL fakery**: the agent brings the
  containers up for real and the deployment reaches `healthy` on its own.

## Verification / demo

- **Slice A:** `make up` now includes the agent. `make demo` deploys into the
  dev org → the agent starts real containers → the deployment reaches `healthy`
  unaided → manual promote (DB flip, no Traefik yet). Asserts: revision
  containers exist (`docker ps`), all instances healthy, no SQL drove the
  transitions. Plus a **failure demo**: a stack with a bad image → deployment
  `failed`, any prior live deployment untouched.
- **Slice B:** `curl` the env hostname through Traefik returns the live
  revision; pushing a new version flips traffic zero-downtime and the old
  swappable containers disappear.
- **Slice C:** `POST /rollback` on an environment brings a prior stack version
  back as a new revision through the same path.

Baseline invariants that must not regress: the `examples/webapp` digest
(`6072c68f…`), classification (`db` pinned; `api`/`cache`/`worker` swappable),
peak memory `2415919104`, and the Sprint 1 guards (409 on second active
deployment, 409 promoting a non-healthy deployment).

---

## Open questions

None blocking. Two items were decided rather than asked and are called out above
for a second look during spec review: the **dev org** (vs relaxing placement to
ignore org in dev) and the **dev secret source** (vs blocking the example stack
until Sprint 3). Both are reversible and scoped to dev ergonomics.
