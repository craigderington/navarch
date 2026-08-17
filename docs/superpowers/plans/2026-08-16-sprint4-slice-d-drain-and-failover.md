# Sprint 4 Slice D — Drain and Failover

> **STALE, corrected 2026-08-16.** Task 1 below plans `Store.ReadyNode`. That
> work shipped ahead of this plan in commit `11a98b4` as **`Store.UncordonNode`**,
> with richer semantics than planned: the returned state is *derived* from
> `last_heartbeat` rather than declared `ready`, and `retired` is refused with
> `ErrConflict` rather than lifted. Executing Task 1 literally would create a
> duplicate method. Treat it as done and read the uncordon invariant in CLAUDE.md
> for the semantics that actually shipped.


**Goal:** a node that is unreachable stops receiving traffic and says so; a node
being drained stops receiving work, can be un-drained, and reports honestly which
environments it cannot give up; and an environment with nothing to lose can be
re-homed elsewhere without reopening the hole Slice A closed.

**Why this is last:** every earlier slice made failover *possible* to reason
about. Affinity (A) established that an environment is bound to a node, which is
what makes "move it" a question with an answer rather than a shrug. The fleet (B)
gave a second node to fail over to. Cross-node ingress (C) made a route a
node-address rather than a container name, which is why withdrawing one is now a
one-line change rather than a redesign. Doing D first would have meant inventing
all three.

**Spec:** `docs/superpowers/specs/2026-08-15-sprint4-multi-node-design.md`

---

## What is true today (verified against the code, not assumed)

- `MarkStaleNodesUnreachable` (`store/nodes.go:158`) flips `ready → unreachable`
  after 30s of heartbeat silence and touches nothing else. Deployments on that
  node stay `live`.
- `ListLiveRoutes` (`store/deployments.go:400`) filters on `d.state='live'` and a
  non-empty hostname. **It does not look at node state.** A dead node keeps its
  route, so Traefik keeps sending traffic at it — which surfaces to a user as a
  timeout, the least diagnosable failure available.
- `DrainNode` (`store/nodes.go:144`) sets `draining` and nothing else. That is
  not useless: `ListReadyNodes` requires `state='ready'`, so a draining node
  already stops receiving *new* placements. Drain is a working cordon and a
  non-existent evacuation.
- `DesiredStateForNode` is not node-state aware, so a draining node's agent keeps
  its full desired state and keeps its containers running. Combined with the
  above, "drain" today means exactly "cordon", which is a coherent thing to mean
  and should be named as such.
- `Heartbeat` (`store/nodes.go:116`) resolves `ELSE 'ready'`, so
  `unreachable → ready` heals automatically the moment a heartbeat lands.
  **Unreachable is a soft, self-healing state.** Any design that takes
  irreversible action at the 30s mark is acting on a condition that routinely
  un-happens.

### Two hazards found while verifying

1. **Drain is a one-way door.** `DrainNode` sets `draining`; `Heartbeat`'s CASE
   preserves `draining`; and `RegisterNode`'s upsert has
   `CASE WHEN nodes.state = 'draining' THEN nodes.state ELSE 'ready'`, so even a
   full agent restart will not clear it. There is no API route that sets a node
   back to `ready`. Today a drained node is restored only by hand-written SQL.
   A slice that tells operators to drain nodes without giving them the inverse is
   shipping a trap.
2. **`retired` exists in the enum and nothing ever sets it.**
   `node_state` is `('pending','ready','draining','unreachable','retired')`.
   `retired` is the natural terminal state for "this node is gone for good, stop
   offering it work and stop expecting heartbeats", and it is sitting unused
   while `unreachable` is doing double duty for both "blip" and "gone".

---

## The central question, and the positions this plan takes

Failover normally means "run it somewhere else". Whole-stack placement plus
`home_node_id` means that for most environments **there is nowhere else** — the
pinned container and named volumes are on that node and cannot follow. So the
question is not "how do we move it" but "what is the honest behaviour when we
cannot". Four positions, each with its cost stated.

### 1. A deployment's state describes its rollout, not its connectivity

**Position: do not add a `stranded` deployment state, and do not move a live
deployment to `failed` or `stopped` because its node went quiet.**

The deployment genuinely is live: nothing superseded it, and its containers are
very likely still running — the control plane simply cannot see them. Writing a
state change would be the control plane asserting something about a world it has
just admitted it cannot observe. Worse, it does not unwind: `Heartbeat` revives
the node automatically, but `validTransitions` has no path back to `live` from
anywhere, and `deployments` is append-only by design. A 30-second network blip
would permanently rewrite deployment history.

Reachability already has a home: the `nodes` row. Surface it *alongside* the
deployment rather than folding it in.

*Cost:* `GET /v1/deployments/{id}` alone will still say `live` for a deployment
nobody can reach. Task 4 addresses that by making the node's state visible where
the deployment is read, rather than by lying in the state column.

### 2. Routes withdraw, but on their own threshold — not the placement one

**Position: `ListLiveRoutes` excludes instances whose node is not `ready`, and
the staleness threshold that governs *routing* is separate from the one that
governs *placement*.**

The 30s in `MarkStaleNodesUnreachable` was chosen to stop the scheduler putting
new work on a dead node. That is a cheap, reversible decision. Cutting live
traffic is neither, and it should not inherit a threshold picked for a different
purpose.

This is the one genuine trade-off in the slice and it deserves to be stated
plainly rather than resolved quietly. With whole-stack placement there is exactly
one copy of an environment. If the node is unreachable *from the control plane*
but still serving users, withdrawing the route converts a working service into a
404. If the node is actually dead, keeping the route means every request hangs
until it times out — which is precisely the failure mode that wasted an afternoon
on the Slice C preview bug, because a timeout tells you nothing about its cause.

Withdrawing wins on the balance of evidence: a fast, honest 404 is diagnosable
and a hanging request is not, and a control plane that cannot reach a node cannot
promise anything about it. But it should wait longer than the scheduler does.

- [ ] `COMPOSECTL_ROUTE_STRAND_SECONDS`, default 120s, separate from the 30s
      placement threshold and documented as a policy dial, not a constant

*Uncertainty, flagged rather than buried:* 120s is a guess. It should be long
enough to ride out an agent restart and short enough that a dead node does not
hang requests for minutes. If the answer is "never withdraw, I would rather have
a hang than a 404", that is a legitimate operator preference and the dial can be
set to 0 to mean never — but the default should be the safer-to-diagnose one.

### 3. Re-homing is explicit, proven safe, and never a scheduler fallback

**Position: an environment may be re-homed only through a new store method that
verifies it has no durable state, and `PlaceDeployment`'s refusal stays exactly
as strict as Slice A left it.**

The instinct is to relax `PlaceDeployment` — "if the home node is unreachable,
allow another one". That is the data-loss bug with a sympathetic motive, and the
Slice A plan already names it: *the temptation to fall back when the home node is
full is the data-loss bug wearing a helpful face.* Unreachable is a worse trigger
than full, because a node that is merely unreachable still has the volumes.

So: keep the check. Add `ReleaseEnvironmentHome(ctx, envID)` which sets
`home_node_id = NULL` **only when the environment has nothing durable on that
node**, checked in the same transaction that clears it. After the release, the
existing scheduler places the next deployment by score, through the unmodified
path — re-homing is the *absence* of a binding, not an override of one.

"Nothing durable" must be judged from the resolved spec of the live deployment:

- no pinned services (`PinnedServices()` is empty), **and**
- no named volumes — `spec.Volumes` empty and no service mount of kind
  `MountVolume`. A read-only volume mount on a swappable service still counts:
  the volume lives on that node, and read-only is a statement about the
  container's access, not about where the bytes are.

*Cost:* an environment that mounts a volume it never writes cannot be re-homed
even though moving it would be harmless. That is the correct direction to be
wrong in.

### 4. Drain reports what it cannot move; it does not refuse

**Position: drain cordons (already true), re-homes what is provably safe to
re-home, and returns the list of environments it cannot evacuate.**

Refusing to drain a node holding stateful environments would make drain useless
exactly when it is most wanted — the operator draining a node before maintenance
still wants new work to stop landing on it, even if three databases cannot move.
Silently draining and leaving them stranded is worse: the operator believes the
node is empty.

So drain returns a manifest. `POST /v1/nodes/{id}/drain` responds with what moved
and what did not and why, and the CLI prints it.

---

## Global Constraints

- Go 1.25. Postgres `5473`, API `8417`. Never 3000/5000/8000/9000.
- Commit locally only. **Never push.** Branch `sprint4-multi-node`.
- Postgres always; no SQLite fallback, not even in tests.
- **Boundaries:** only `internal/store` imports pgx; only `internal/agent/dockerd`
  imports the Docker SDK; only `internal/parser` imports compose-go; only
  `internal/client` knows the API wire format. Guard:
  `go list -deps ./cmd/controlplane | command grep docker/docker` returns
  nothing. Use `command grep` — this shell aliases `grep` to a wrapper that
  mis-parses the pipeline and prints `unknown option '-G'` while appearing to
  pass.
- Migrations are immutable once applied — the next free number is `0011`.
- Errors wrap with `%w`; store returns `ErrNotFound` / `ErrConflict` / `ErrInvalid`.
- Comments explain **why**. Match the existing register.
- Tests skip loudly without their dependency. **Check for `--- SKIP`.**
- Run tests with the dev-stack control plane stopped.

**No migration is expected.** This slice is behaviour over state that already
exists — `home_node_id`, the `node_state` enum, and the events table are all in
place. If a task appears to need a column, that is a signal to re-read position 1
before reaching for `0011`.

---

## Task 1 — un-drain, and a terminal state that means it

- [ ] `Store.ReadyNode(ctx, nodeID)`: `draining → ready`, `ErrNotFound` when no
      row matched so the handler's existing mapping gives a 404
- [ ] `Store.RetireNode(ctx, nodeID)`: `→ retired` from any non-retired state
- [ ] `POST /v1/nodes/{id}/ready` and `POST /v1/nodes/{id}/retire`
- [ ] `navarch node ready ORG/HOSTNAME` and `navarch node retire ORG/HOSTNAME`
- [ ] Client methods for both

Drain without un-drain is a trap, and it is the first thing an operator will hit
after using this slice as intended. `retired` closes the other half: it separates
"quiet for 30 seconds" from "this machine is gone", and only the second should
ever justify releasing an environment's binding (Task 3).

`RegisterNode`'s upsert deliberately preserves `draining` across a re-register,
which is correct — an agent restart must not silently uncordon a node an operator
cordoned. Leave it. `ReadyNode` is how that intent is reversed, by a person.

## Task 2 — routes follow node reachability

- [ ] `ListLiveRoutes` takes a staleness threshold and excludes an instance whose
      node is not `ready`, or whose `last_heartbeat` is older than it
- [ ] `COMPOSECTL_ROUTE_STRAND_SECONDS` (default 120) in `internal/config`
- [ ] The controller passes it through on each `syncRouter`
- [ ] Use `make_interval(secs => $n)`, never `Duration.String()::interval` — the
      house pattern, and the `.String()` form renders sub-second values as `1ns`,
      which Postgres rejects outright

This is safe to do *only because* the empty-router-config fix landed earlier: a
shrinking route set now withdraws correctly instead of being rejected and leaving
the stale route serving. Before that fix, this task would have produced the exact
opposite of its intent.

## Task 3 — release a binding, when and only when nothing is lost

- [ ] `Store.ReleaseEnvironmentHome(ctx, envID) error` — locks the environment
      row, reads the live deployment's resolved spec, and clears `home_node_id`
      only when the spec has no pinned service and no named volume
- [ ] Refuses with `ErrConflict` naming what is durable, so the caller can report
      *which* volume or pinned service pinned it
- [ ] Appends an environment event recording the release and the node it was
      released from — a binding that changes without a trace is a binding nobody
      can audit after an incident
- [ ] `PlaceDeployment` is **not** modified

## Task 4 — make an unreachable deployment visible as one

- [ ] `GetDeployment`/`ListDeployments` responses carry the home node's hostname
      and state (or a derived `reachable bool`)
- [ ] `navarch deployment list` and `deployment get` show it
- [ ] `navarch env get` shows the environment's home node

Position 1 says the state column must not lie. This is the other half of that
bargain: if `live` continues to mean live, the reader must be able to see, in the
same output, that the node behind it is unreachable. Otherwise "do not lie in the
state column" just becomes "do not tell them at all".

## Task 5 — drain evacuates what it can and reports what it cannot

- [ ] `Store.EnvironmentsHomedOnNode(ctx, nodeID)` returning, per environment,
      whether it holds durable state
- [ ] Drain: cordon (already), then attempt `ReleaseEnvironmentHome` for each
      environment with nothing durable
- [ ] Response body: `{drained: [...], stranded: [{env, reason}]}`
- [ ] CLI prints both lists, and exits non-zero **only** on a real error — a node
      with stranded environments drained successfully, it just did not empty

## Task 6 — do not fail over automatically

- [ ] No automatic re-homing on `unreachable`. Write the reason down in CLAUDE.md
      rather than leaving it as an absence.

An unreachable node is usually a node that is about to come back, and its agent
still holds desired rows for its environments. Re-homing automatically means two
copies of a stateless environment running until the old node returns and its
agent garbage-collects the orphans — which it will do correctly, but only once it
can reach the control plane again. The window is real, it is unbounded by
anything the control plane controls, and nothing forces the operator's hand
during it. Re-homing is therefore operator-initiated: `retire` the node, or
`drain` it, and the release happens as a consequence of a decision somebody made.

*This is the position I hold least firmly.* An automatic path gated on `retired`
(never on `unreachable`) would be defensible, since `retired` is an explicit
human judgement that the machine is gone. It is left out of this slice because
nothing yet sets `retired` and a policy loop with no operational history behind
it is a guess with a cron schedule.

## Task 7 — tests

- [ ] Store: `ListLiveRoutes` omits a deployment whose node is `unreachable`, and
      **includes it again when the node heartbeats back to ready** — the second
      half is what proves withdrawal is reversible rather than terminal
- [ ] Store: `ListLiveRoutes` omits a node whose `last_heartbeat` is older than
      the threshold while its state still reads `ready` (the two conditions are
      independent and a test that only sets state would pass with the heartbeat
      clause deleted)
- [ ] Store: `ReleaseEnvironmentHome` clears the binding for a volumeless,
      pin-free environment
- [ ] Store: it refuses for an environment with a pinned service, **and**
      refuses for one whose only durable state is a read-only volume mount on a
      swappable service — the second case is the one a naive implementation gets
      wrong, so it must be asserted separately
- [ ] Store: after a release, `PlaceDeployment` accepts a different node — proving
      re-homing works *through* the strict path rather than around it
- [ ] Store: `ReadyNode` restores a drained node; `RegisterNode` still does not
- [ ] Scheduler: a deployment for an environment whose home node is unreachable
      stays pending and is placed when the node returns (already the behaviour;
      pin it before touching this area)
- [ ] API: drain returns stranded environments with reasons

**Non-vacuity that must be verified by hand, not assumed:** the route-withdrawal
test must fail with the node-state clause removed, and the read-only-volume test
must fail if the durable-state check looks only at `PinnedServices()`. Both are
cheap to check by reverting one condition and re-running; do it, and say in the
commit message that it was done.

## Task 8 — documentation

- [ ] CLAUDE.md: node lifecycle (`pending → ready ⇄ draining`, `→ unreachable`
      self-healing, `→ retired` terminal) in the register of the other invariants
- [ ] CLAUDE.md: why deployment state does not track reachability
- [ ] CLAUDE.md: why re-homing goes through a release and never through
      `PlaceDeployment`
- [ ] `docs/superpowers/specs/…-multi-node-design.md`: mark Slice D's open fork
      resolved

---

## Non-goals (explicit)

- **No volume migration.** Moving durable state between nodes is a data-migration
  problem with its own consistency story; it is not scheduling and does not
  belong in this slice.
- **No automatic failover of stateful environments.** There is nowhere to fail
  them over to. Anything that appears to do this is recreating them empty.
- **No multi-replica ingress or load balancing across nodes.** One environment,
  one node, one route — unchanged.
- **No agent-side change.** The agent already GCs orphaned swappable containers
  when their desired rows disappear, which is the whole of what it needs to do
  here. If this slice starts editing `internal/agent`, something has gone wrong.
- **No node auto-retirement on a timer.** `retired` is a human judgement.

## Open questions worth a decision before starting

1. **`COMPOSECTL_ROUTE_STRAND_SECONDS` default.** 120s is a guess (position 2).
2. **Should `retire` release stateless bindings automatically?** It is the one
   automatic path with a defensible trigger (Task 6), and leaving it out may
   simply mean operators run drain-then-retire every time.
3. **Does `stranded` belong in the drain response as a hard error for scripting?**
   The plan says exit zero; CI that drains a node before maintenance may
   reasonably want a non-zero exit when the node did not empty.

## Verification

- [ ] `go build ./...`, `go vet ./...`, boundary guard clean (`command grep`)
- [ ] `go test ./... -race -count=1` with the dev control plane stopped,
      `--- SKIP` checked
- [ ] `make demo` still reaches live and flips traffic
- [ ] `make demo-fleet` still spreads and serves across nodes
- [ ] `make demo-preview` still tears down completely
- [ ] Manual: stop `agent-b`, watch node 2 go unreachable, watch its routes
      withdraw after the strand threshold, restart it, watch them come back
