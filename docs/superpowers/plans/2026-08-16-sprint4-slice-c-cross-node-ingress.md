# Sprint 4 Slice C — Cross-Node Ingress

**Goal:** a stack running on node 2 is served through the ingress node. The
router stops targeting a container name on its own daemon and starts targeting
a node address and a published port, which is the same code path whether the
tenant is local or remote.

**Decision (settled):** mesh-bound published port, not routed container subnets.
Routing container subnets would make every container on every node reachable
from every other node — the flat plane `reconcile.go` deliberately rejected at
L2 (*"tenant ingress stays off any shared mesh so one fleet cannot talk to
another"*) rebuilt at L3, with a larger blast radius and no label to constrain
it. Whole-stack placement means the only cross-node flow is router → ingress
container: one narrow path, which a published port serves exactly.

**Spec:** `docs/superpowers/specs/2026-08-15-sprint4-multi-node-design.md`

## Sequencing

Split so the fleet is green at every step, and so the user-visible capability
lands before the transport work:

- **C1 — address + published port + router target.** Cross-node ingress working
  end to end over the dev fleet's existing L3 connectivity.
- **C2 — WireGuard.** Replace "the node's reachable address" with a mesh address
  that also works across real hosts. C1's `advertise_addr` is the seam.

C1 is the whole user-visible capability. In the dev fleet the nodes already have
L3 connectivity (the compose network), so WireGuard is what makes the same design
work between real machines — not what makes it work at all. Building C1 first
means the routing change is proven before the transport is swapped underneath it.

## C1 tasks

- [ ] Migration `0010`: `service_instances.ingress_port INT` (nullable)
- [ ] `dockerd.ContainerSpec` gains a published port for ingress services;
      publish with host port `0` and read the assignment back from
      `NetworkSettings.Ports` — Docker allocates, so there is no allocator to
      write, no range to manage and no collisions to reason about
- [ ] Agent reports the assigned port with its existing instance report
- [ ] `ReportInstance` persists it; `ListLiveRoutes` returns the node's
      `advertise_addr` with it
- [ ] `router.Route` targets `host:port` instead of a container name
- [ ] Dev fleet advertises addresses that are actually reachable from the
      router's container

**The port is reported, the address is registered.** The agent chooses neither:
it reports what Docker assigned, and the address comes from the `nodes` row the
control plane already holds. An agent that could name its own address could
redirect another tenant's traffic.

**A route with no reported port is omitted, not guessed.** The empty-config fix
earlier this session is what makes that safe: a shrinking or empty route set now
withdraws correctly instead of being rejected and leaving stale routes live.

## Verification

- [ ] `make demo-fleet` extended: the no-ingress stack becomes an ingress stack
      on node 2, served through the ingress node
- [ ] `make demo` still flips traffic between revisions
- [ ] Full suite, boundary guard, `--- SKIP` checked

---

## C1 — DONE

Cross-node ingress works: a stack on the node with no router is served through
the node that has one, and so is a stack on the router's own node, by the same
address-and-port mechanism. `make demo`, `demo-fleet`, `demo-preview`,
`demo-rollback` and `demo-failure` all pass; full suite green with 0 skips.

### The bug that stopped this at the break: node 1 advertised an unreachable address

`COMPOSECTL_ADVERTISE_ADDR: host.docker.internal` with
`extra_hosts: host.docker.internal:host-gateway` resolves to **docker0's**
address, 172.17.0.1. That is the correct "how do I reach the host" answer for a
container on the *default* bridge, and the wrong one for a container on a
*user-defined* network — which is where both Traefik and the agent live. The
measurement, taken from inside Traefik's namespace with nothing else running:

```
172.18.0.1  (quartermaster_default gw, an attached scope-link subnet) -> 200
172.19.0.1  (a revision network gw, which owned the default route)    -> timeout
172.17.0.1  (docker0, not an attached subnet at all)                  -> timeout
```

Traffic to an address on no attached subnet leaves by the default route, and
Docker's inter-bridge isolation drops it. So the target was never reachable —
what varied was *which* interface owned the default route, and that changes
every time the agent attaches Traefik to a new revision network or the GC
removes one. Hence the maddening signature: the same node, the same mechanism,
200 one minute and a timeout the next, apparently discriminating by router.

**Fix:** pin the compose network to `10.201.0.0/24` and have node 1 advertise
its gateway, `10.201.0.1`. A gateway of an attached network is a *scope-link*
route — reachable without consulting the default route at all, so it no longer
matters which bridge wins that race. The subnet sits outside Docker's default
pools (172.16-172.31, 192.168) so an auto-created revision network can never be
allocated the same range.

Verified after the fix, with a node-1 environment live and Traefik attached to
its revision network — i.e. the default route *still* stolen by 172.19.0.1 —
both nodes serve HTTP 200. The fix removes the dependency rather than the
symptom.

### `AttachRouterToNetwork` is now vestigial (do not fix here)

Route generation is entirely address-based, so the router never needs to join a
tenant network. The attach still happens for node-1 environments and still
takes the default route with it — proven harmless now, but it is dead weight.
Removing it would also obsolete the `isIngressRouter` exemption in
`PruneRevisionNetworks` and the disconnect-unmanaged-containers logic in
`RemoveEnv`, both of which exist only to cope with the router being attached to
tenant networks. That is its own change with its own tests, not a rider on this
one. It was investigated as a suspected cause of this bug and **ruled out**:
the demos pass with the attach untouched.

### A demo race, fixed in passing

`demo-fleet` curled once, immediately after `wait --state live`. Promotion marks
a deployment live; the route appears when the controller's next tick resyncs the
router and Traefik reloads. The single shot lost that race and reported 404 for
a route that was already in the file seconds later — a passing system that looks
broken, which is worse than a real failure because it sends you hunting in the
router. It now retries to a deadline, as `demo-preview` already did.

### No unit test accompanies this fix

Both defects are outside Go: one is a compose address, the other a shell race.
The regression guard is the demo suite, and the fix makes node-1 routing
deterministic where it previously depended on which bridge won the default
route — the reason it passed often enough to look intermittent rather than
broken.
