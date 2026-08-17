# Sprint 4 Slice A — Environment Affinity and Scored Placement

**Goal:** An environment is bound to one node for its lifetime, every later
deployment for it is placed there or fails loudly, and first placement chooses a
node by score rather than by whichever row came back first.

**Why this is Slice A and not Slice B:** the affinity bug is live in the code
today and needs only a second node to become data loss — revision 2 of an
environment placed on a node that does not hold its pinned volume brings up an
empty database, passes health checks, and auto-promotes. Adding nodes before
fixing this would mean shipping the failure and then chasing it. Nothing here
needs a real second daemon: store tests fabricate node rows freely.

**Spec:** `docs/superpowers/specs/2026-08-15-sprint4-multi-node-design.md`

## Global Constraints

- Go 1.25. Postgres `5473`, API `8417`. Never 3000/5000/8000/9000.
- Commit locally only. **Never push.** Branch `sprint4-multi-node`.
- Postgres always; no SQLite fallback, not even in tests.
- **Boundaries:** only `internal/store` imports pgx; only `internal/agent/dockerd`
  imports the Docker SDK; only `internal/parser` imports compose-go; only
  `internal/client` knows the API wire format. Guard:
  `go list -deps ./cmd/controlplane | grep docker/docker` returns nothing.
- Migrations are immutable once applied — add `0007_*`, never edit an existing one.
- Errors wrap with `%w`; store returns `ErrNotFound` / `ErrConflict` / `ErrInvalid`.
- Comments explain **why**. Match the existing register.
- Tests skip loudly without their dependency. **Check for `--- SKIP`.**
- Run tests with the dev-stack control plane stopped.

---

## Task 1 — `environments.home_node_id`

- [ ] `migrations/0007_environment_home_node.up.sql` / `.down.sql`
- [ ] Column `home_node_id UUID REFERENCES nodes(id) ON DELETE SET NULL`

`ON DELETE SET NULL` is deliberate and worth the comment in the migration. The
semantically pure choice is RESTRICT — an environment's home node should not
vanish underneath it — but deleting a node means its volumes are gone with it,
so unbinding is the honest outcome and re-homing is the only possible recovery.
RESTRICT would also wedge organization deletion: `environments` cascade from
stacks and `nodes` cascade from the org, and the existing cleanup order
(instances → deployments → nodes → org) deletes nodes while environments still
reference them. This is the same cascade-ordering hazard CLAUDE.md already
records for `service_instances.node_id`.

## Task 2 — carry the home node to the scheduler

- [ ] Add `HomeNodeID *uuid.UUID` to `PendingDeployment`
- [ ] Select `e.home_node_id` in `listPendingDeployments` (the query already
      joins `environments`, so this is one column, not a new join)

## Task 3 — enforce affinity in the placing transaction

- [ ] `PlaceDeployment` reads `home_node_id … FOR UPDATE` inside its existing tx
- [ ] If set and it differs from the target node → `ErrConflict` naming both nodes
- [ ] If NULL → set it to the target node in the same transaction

Enforcement lives in the store, not only in the scheduler, for the same reason
the deployment state machine is enforced in SQL: a buggy or racing scheduler must
not be able to write a placement that contradicts durable state. Two schedulers
placing two deployments of one environment concurrently is exactly what the row
lock is for.

## Task 4 — scheduler targets the home node

- [ ] Home node set: place there. Not ready, or lacks capacity → fail the
      deployment with a reason naming the node. **Never** relocate.
- [ ] Home node NULL: score and choose

The temptation to fall back to another node when the home node is full is the
data-loss bug wearing a helpful face. Failing is correct.

## Task 5 — scored placement

- [ ] Hard filter first: existing free-CPU and free-memory check against
      `PeakCPUMillis` / `PeakMemoryBytes` (unchanged semantics)
- [ ] Score survivors: fewest environments homed, then greatest free-capacity
      ratio, then node id ascending
- [ ] `EnvironmentsHomedPerNode` store method feeding the spread term
- [ ] Pure, table-driven test over the scoring function — no database needed

Deterministic tie-break is not a detail: a scheduler whose output depends on row
order cannot be asserted on.

## Task 6 — secret recipients follow the home node

- [ ] `RecipientsForEnvironment` prefers the home node's recipient when set
- [ ] Keep the existing fallbacks: nodes already running it, then every ready
      node in the org when nothing is placed

## Task 7 — tests

- [ ] Store: second deployment for an environment placed on a different node is
      rejected — the regression test for the data-loss bug, asserted on the
      error, not just on a count
- [ ] Store: first placement sets `home_node_id`; a later one leaves it unchanged
- [ ] Store: `EnvironmentsHomedPerNode` counts per node
- [ ] Scheduler: with two ready nodes, two environments spread across them
- [ ] Scheduler: an environment homed to a full node fails rather than moving
- [ ] Scoring: pure table test including the tie-break
- [ ] Secrets: recipients for a homed environment are that node's

## Task 8 — documentation

- [ ] CLAUDE.md: affinity as an invariant, in the register of the existing ones
- [ ] Note the ON DELETE SET NULL rationale where the other cascade hazards live

## Verification

- [ ] `go build ./...`, `go vet ./...`, boundary guard clean
- [ ] `go test ./... -race -count=1` with the dev control plane stopped, `--- SKIP`
      checked
- [ ] `make demo` still reaches live and flips traffic (single node unaffected)
- [ ] `make demo-preview` still tears down completely
