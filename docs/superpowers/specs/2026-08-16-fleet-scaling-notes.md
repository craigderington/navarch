# Fleet Scaling — N Nodes, and Making the Fleet Symmetric

> **PARTLY STALE, corrected 2026-08-16.** The node-count and memory reasoning
> below stands. The sequencing does not: removing `AttachRouterToNetwork` was
> done in commit `df9a882`, and the hypothesis that it caused the Slice C preview
> bug was **tested and ruled out** — the real cause was node 1 advertising
> docker0's address, fixed in `41e8738`. `isIngressRouter` and `RemoveEnv`'s
> disconnect were deliberately KEPT after the removal: endpoints created before
> it still exist on any upgraded daemon.


**Date:** 2026-08-16
**Builds on:** Sprint 4 Slices A–C (`sprint4-multi-node`).
**Status:** investigation. No code changed; this is a decision document.
**Related:** `2026-08-15-sprint4-multi-node-design.md`,
`../plans/2026-08-16-sprint4-slice-c-cross-node-ingress.md`

---

## Context

The fleet is two nodes: node 1 is the `agent` service driving the **host** Docker
daemon with Traefik as a sibling on that same daemon, and node 2 is
`dind-b` + `agent-b`, a genuinely separate daemon reached over `DOCKER_HOST`.
Node 1 being on the host daemon is a documented deviation from "all nodes in
DinD", taken so Slice B could land without also solving cross-node ingress.

Two questions follow: what does it cost to go to four or five nodes, and what
does it take to remove the asymmetry. They turn out to have very different
answers — one is nearly free, and the other is *already mostly done* and nobody
has noticed.

---

## The finding that reframes both questions

**After Slice C, the router no longer needs to share a daemon with the tenant,
and the code that makes it do so is now vestigial.**

Verified:

- `internal/router/router.go` builds every backend URL from `rt.Target` and
  `rt.Port`, and `internal/rollout/controller.go` sets `Target: lr.NodeAddr`.
  There is no container-name path left anywhere in route generation.
- `reconcile.go:285` still calls `AttachRouterToNetwork` for every ingress
  instance, with a comment describing the pre-Slice-C model ("Traefik joins the
  revision network").
- That call already **no-ops on node 2**: it lists containers labelled
  `cc.role=ingress-router` on *the agent's own daemon*, and `dind-b` has no such
  container, so the loop body never runs.
- Node 2 has therefore never had the router attached to any of its networks —
  and its stacks are served correctly through the ingress node. Cross-node
  ingress working is itself the proof that the attach is not load-bearing.

The consequence for symmetry is large: **moving node 1 into DinD does not
require moving Traefik into DinD.** Traefik can stay exactly where it is, a
plain compose service on the host daemon, because all it needs is L3
reachability to each node's `advertise_addr` — which it already has for node 2.

A second consequence is a simplification the network-GC path would enjoy.
`PruneRevisionNetworks` needed the `isIngressRouter` exemption *only* because
Traefik holds an endpoint on every revision network it ever served on node 1. If
the attach goes, so does the entire class of problem: no router endpoints on
tenant networks, nothing unmanaged to disconnect, and the exemption becomes dead
weight rather than load-bearing subtlety.

**Unverified, and worth testing before removal.** I did not exercise the stack
(another agent owns it), so this is reasoning from the code plus the evidence
already in the Slice C record. Removing the attach should be done as its own
change with `make demo` and `make demo-fleet` green on either side of it.

### A lead on the open Slice C bug

Offered as a hypothesis, not a finding — it is outside this document's scope but
it bears directly on the recommendation to remove the attach.

The open bug is that a *preview* environment's hostname times out through Traefik
while a non-preview environment on the same node, same mechanism, same daemon
returns 200 in the same minute — and the backend answers 200 when reached
directly at the exact address the route names.

Traefik on node 1 accumulates an interface per revision network it has ever
served; the Slice C record shows it holding nine, with gateways spanning
`172.18.0.1`–`172.26.0.1`. A container with that many bridge interfaces has a
correspondingly large routing table, and egress to `172.17.0.1` — the host
gateway, which is *not* any of those bridges — depends on which route wins. The
same record notes that `172.17.0.1` was unreachable from a probe attached to a
revision network while reachable from one on `quartermaster_default`. That is
consistent with "the attach is changing Traefik's egress path", and it would
explain why the failure looks per-router rather than per-node.

If that is what is happening, removing the vestigial attach fixes the open bug
and the asymmetry in one change. Cheap to test: the attach can be disabled
behind a one-line edit and the preview demo re-run.

---

## 1. Adding node N

Per extra node, the compose additions are mechanical — a `dind-<x>` block, an
`agent-<x>` block, and two volumes:

| Element | Per node | Shared | Why |
|---|---|---|---|
| `dind-<x>` service | ✅ | | the point of the exercise: its own daemon |
| `dind-<x>-data` volume | ✅ | | `/var/lib/docker`; its own image cache and container state |
| `agent-<x>` service | ✅ | | one agent drives one daemon |
| `age-identity-<x>` volume | ✅ | | **see below — this is the one that must not be shared** |
| `COMPOSECTL_NODE_HOSTNAME` | ✅ | | `RegisterNode` upserts on `(org_id, hostname)` |
| `COMPOSECTL_ADVERTISE_ADDR` | ✅ | | the address the router connects to |
| `DOCKER_HOST` | ✅ | | points at that node's daemon |
| `COMPOSECTL_NODE_LABELS` | ✅ | | capabilities differ by node |
| `COMPOSECTL_NODE_MEMORY_MB` | ✅ | | see §3 — should shrink as N grows |
| `COMPOSECTL_AGENT_TOKEN` | | ✅ | the shared bootstrap token; a per-node token is issued at registration |
| `COMPOSECTL_CONTROLPLANE_URL`, `COMPOSECTL_ORG` | | ✅ | same control plane, same org |
| `NAVARCH_DIND_DNS` | | ✅ | same host resolver problem on every DinD node |

With the current naming (`dind-b` ↔ `dev-node-2`) the mapping between a node's
hostname and its daemon is arbitrary and has to be written down somewhere — and
it already is, as a `case` statement in `scripts/demo-preview.sh`. Adopting
`dind-a`/`dev-node-a` … `dind-e`/`dev-node-e` makes it derivable by suffix, which
turns that `case` into string interpolation and stops it growing a line per node.
Renaming node 2 is a nuke-boundary change (the hostname is the upsert key, so a
rename registers a *new* node row and orphans the old one's environments), which
is cheap now and expensive later.

### Why identity must not be shared

Two distinct failures, both silent-ish, and neither obvious from the compose file:

**Shared age keypair breaks the confidentiality property the sealing exists for.**
`secrets.Encrypt(plaintext, recipients)` seals to a list of X25519 recipients, and
after Slice A `RecipientsForEnvironment` narrows that list to the environment's
*home node* — the point being that only the node which will actually run the
environment can open its secrets. If two nodes share `/identity`, they register
the same `age_recipient`, so ciphertext sealed "to node A" is decryptable by node
B by construction. The narrowing still happens, the code still looks right, and
the property it buys is gone.

**Shared node-token file causes intermittent 401s.** A per-node token is issued
once per node row (`token_hash = COALESCE(nodes.token_hash, EXCLUDED.token_hash)`
— re-registration does not rotate it) and validated strictly per node id
(`NodeTokenValid(nodeID, plain)`). Both agents write to the same
`COMPOSECTL_NODE_TOKEN_FILE`, so the second to register clobbers the first's
token. Nothing fails immediately, because each agent holds its own token in
memory. It fails on the *next restart*, when an agent loads the file and finds
the other node's token, and its heartbeat / desired-state / report calls start
returning 401 — a failure separated from its cause by an arbitrary interval.

The existing comment on `age-identity-b` says "two rows in `nodes` that are one
identity", which is right; the above is what that cashes out to.

---

## 2. Symmetry: node 1 into DinD

Given the finding above, node 1 becomes an ordinary node:

- Add `dind-a` + `dind-a-data`, exactly like `dind-b`.
- `agent` loses its `/var/run/docker.sock` mount and gains
  `DOCKER_HOST: tcp://dind-a:2375`.
- `agent` loses `extra_hosts: host.docker.internal:host-gateway` and its
  `COMPOSECTL_ADVERTISE_ADDR: host.docker.internal`, and advertises `dind-a`
  instead — the same shape as node 2.
- Traefik does not move. It stays a compose service, keeps its
  `cc.role=ingress-router` label, keeps publishing `127.0.0.1:8095:80`, and
  reaches every node at a compose-network address.

What this *removes* is worth as much as what it adds: the `host.docker.internal`
/ host-gateway path disappears, and with it the one address in the system whose
reachability depended on which bridge network the caller sat on. Every route
target becomes a compose-network address reachable the same way from Traefik.

**The alternative — Traefik inside `dind-a`** — is the design the original spec
implied, and I do not recommend it. It requires the control plane's `/dynamic`
volume to be visible inside `dind-a` *and* bind-mounted again from within
`dind-a` into the Traefik container (DinD resolves bind sources against the
`dind-a` container's filesystem, so this works, but it is two hops of mounting to
reason about); it requires something to start Traefik inside `dind-a`, since
compose cannot reach in — either a one-shot init container talking to
`tcp://dind-a:2375`, or teaching the agent to run platform components, which is a
real design change; and it requires the published port to traverse
Traefik→`dind-a`→host. All of that buys nothing now that routing is
address-based. It becomes interesting again only if the router must reach tenants
that are *not* published — which is the routed-container-subnets option Slice C
rejected.

**What I am unsure about.** Whether anything else depends on tenant containers
being visible to a plain `docker ps` on the host. I found the demo scripts (§4)
but there may be muscle-memory workflows that break — after this change,
`docker ps` on the host shows Traefik, Postgres and the agents, and no tenant
containers at all. That is correct, and it will still surprise someone.

---

## 3. What a node actually costs

Measured on this box rather than estimated.

**Disk, per DinD node** — each daemon keeps its own `/var/lib/docker`, so every
tenant image is pulled and stored once per node:

| Image | Size |
|---|---|
| `postgres:16-alpine` | 420 MB |
| `redis:7-alpine` | 59 MB |
| `traefik/whoami` | 19 MB |
| `alpine:3.20` | ~8 MB |
| **≈ per node** | **~505 MB** |

`docker:27-dind` (530 MB) is pulled once on the *host* daemon and shared by every
`dind-*` container, so it does not multiply.

With 813 GB free, disk is not the constraint. Five nodes is ~2.5 GB of duplicated
cache.

**Memory, per DinD node** — a dockerd + containerd pair idles at roughly
200–300 MB, plus whatever tenant containers it hosts (a `hello`-shaped
environment is ~100 MB of actual usage). Five idle nodes is ~1–1.5 GB against
21 GB available. Also not the constraint.

**The real cost is pull latency.** Each new node starts with an empty cache and
must fetch every image over the network before its first deployment can start.
A fresh five-node fleet running the demos pulls Postgres five times. That is
minutes of wall clock on every `make nuke` cycle, and it is the thing that will
actually make a larger fleet annoying. Two mitigations worth considering if it
bites: point every DinD daemon at a shared pull-through registry mirror
(`--registry-mirror`, one flag in the `dockerd` command), or accept it and nuke
less often. The mirror is the better answer and is a small change.

**Advertised memory is a scheduling number and should shrink as N grows.**
`COMPOSECTL_NODE_MEMORY_MB` is what the scheduler reserves against, based on
declared limits rather than measured usage; it is unrelated to what the node can
really run. Two nodes at 16 GB already advertise 32 GB on a 30 GB box, which is
fine only because demo stacks use a fraction of their declared limits. At five
nodes it would advertise 80 GB, and the scheduler would never refuse work for
capacity — removing a real signal, and re-treading the confusion that produced
the "no ready node has … free" scare.

The useful invariant is to hold *fleet-wide* advertised capacity roughly
constant: `COMPOSECTL_NODE_MEMORY_MB ≈ 32768 / N`. Four nodes at 8 GB gives each
node the same headroom-per-node that two nodes at 16 GB did, because spread
scoring distributes environments across the fleet. The single `NAVARCH_NODE_MEMORY_MB`
override already exists; it just needs a value chosen per fleet size rather than
copied.

---

## 4. What hardcodes "two nodes"

Confined to two shell scripts — **the Go code is entirely node-count agnostic**
(the only matches for `dev-node` in `*.go` are example strings in CLI help text).

| Location | What it assumes |
|---|---|
| `scripts/demo-preview.sh:71` | `case dev-node-2) → docker compose exec -T dind-b docker`, defaulting everything else to the host daemon |
| `scripts/demo-fleet.sh:40` | `[ "$READY" -ge 2 ]`, with the failure message naming `dind-b` |
| `scripts/demo-fleet.sh:97,101` | inspects `dind-b` directly for the no-ingress stack's containers |

All three want the same thing: *given a node hostname, how do I run `docker`
against that node's daemon?* One shared helper, sourced by both scripts, is the
whole job — and with a derivable naming convention (§1) it is one line of
interpolation rather than a `case`. Note the ordering trap already recorded in
`demo-preview.sh`: the daemon can only be resolved *after* the deployment goes
live, because `home_node_id` is NULL until first placement.

Once node 1 is also DinD, the `*)` default branch stops being "the host daemon"
and becomes an error case, which is a small improvement in its own right — a
typo'd hostname currently falls back to inspecting the wrong machine and reports
"no containers" rather than "I don't know that node".

---

## 5. What gets better with a larger fleet

- **Spread scoring becomes properly testable.** With two nodes, "spread" and
  "not the same node" are the same assertion, and `bestNode`'s ordering —
  fewest-homed, then free-capacity ratio, then node id — is only exercised in
  unit tests. Four nodes lets a demo assert an actual *distribution*, and makes
  the tie-break observable rather than theoretical.
- **Slice D failover becomes meaningful.** Killing one of two nodes is
  indistinguishable from "the fleet is half down"; killing one of five is a
  realistic drain scenario where the interesting question — *where does the work
  go, and what refuses to move because it has durable state* — actually has more
  than one possible answer.
- **The home-node constraint gets exercised properly.** With two nodes, "placed
  on the home node" has a 50% chance of being right by accident. With five, an
  affinity bug shows up immediately.
- **Capacity refusal becomes visible again** if per-node advertised memory is
  scaled down as recommended, restoring a signal that is currently tuned away.

Against that: every additional node multiplies pull latency and adds a
`privileged` container to the dev stack. Neither is a reason not to, but they are
the reasons not to go past what the tests need.

---

## Recommendation

**Four nodes** — node 1 converted to DinD plus two new ones — and do the
conversion *before* adding the extra nodes.

Four rather than five because four is the smallest fleet where every property
above is genuinely exercised: a distribution rather than a split, a drain that
leaves a real choice, and an affinity bug that cannot pass by coincidence. Five
adds pull latency and a third `privileged` container for no additional property.
Nothing about the design stops someone going further; the compose blocks are
copy-paste once the naming convention lands.

Convert first because conversion *removes* a special case, and adding nodes
around a special case means writing the scripts twice — once for "host daemon or
`dind-b`", and again for "any DinD node". It is also the change that lets the
demo helper be written correctly the first time.

Suggested order, each step leaving the fleet green:

1. **Settle the open Slice C bug**, testing the vestigial-attach hypothesis
   first — it is a one-line experiment and it may resolve the bug and the
   asymmetry together.
2. **Remove `AttachRouterToNetwork`** if that holds, and with it the
   `isIngressRouter` exemption in `PruneRevisionNetworks` that only existed to
   accommodate it.
3. **Convert node 1 to `dind-a`**, adopt suffix naming, and generalise the
   demo scripts' node→daemon helper.
4. **Add nodes c and d**, set `COMPOSECTL_NODE_MEMORY_MB` to ~8192, and extend
   `demo-fleet` to assert a distribution rather than a split.
5. **Consider a registry mirror** if pull latency on a fresh fleet becomes the
   thing that discourages nuking.

Steps 1–2 are the ones with real design content. Steps 3–5 are mechanical once
they land.
