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

## Status at break — C1 works, one open bug

**Proven working:**

- An ingress stack placed on node 2 (no router there) is served through the
  ingress node: `HTTP 200`, body from a container inside `dind-b`.
- `make demo-fleet` asserts it: two ingress stacks land on different nodes by
  spread scoring, both return 200, and at least one is off the router node.
- `make demo` still flips traffic between revisions, now via published ports.
- Generated config targets addresses, one code path for local and remote:
  `http://172.17.0.1:32769` (node 1), `http://172.18.0.4:32768` (node 2).
- Slice B's `ingress=true` placement filter has been **removed** — it was
  scaffolding for exactly this, and its test is replaced by one pinning the
  removal.
- Full suite green; boundary guard clean (check it with `command grep`, the
  shell's grep alias mis-parses the pipeline and prints "unknown option -G"
  while appearing to pass).

**Open bug — `make demo-preview` fails at the curl step.**

Narrowed, not solved. For a preview environment:

- the agent reports a port and the route *is* written
  (`r-02416a85` → `Host(pr-31396-main-31396-02416a85.preview.localhost)`),
- the backend is reachable at that address from the host **and** from a
  container on the compose network: `curl 172.17.0.1:32776` → `200`,
- but the same request through Traefik times out: `HTTP 000`,
- while a *non-preview* env on the same node, same mechanism, same daemon
  returns `200` through Traefik in the same minute.

So it is not connectivity and not the published port. The difference is
something about that specific router — the preview hostname is far longer than
the others and uses the `.preview.localhost` domain, which is the first thing to
rule out. Next steps: raise Traefik's log level and read what it says about that
router, and compare the two router entries as Traefik parsed them rather than as
we wrote them.

`demo-preview` was also made fleet-aware in this slice (it inspects the daemon of
the node the preview was actually placed on, resolved *after* the deployment goes
live — `home_node_id` is NULL until placement, so asking earlier always answers
"the host"). That fix is correct and independent of the bug above.
