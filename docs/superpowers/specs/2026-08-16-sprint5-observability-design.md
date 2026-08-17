# Sprint 5 — Observability: Logs and a TUI

**Date:** 2026-08-16
**Builds on:** Sprint 4 (four-node fleet, environment affinity, cross-node ingress).
**Branch:** `sprint5-observability`

---

## Context

The platform can place, roll out, route and tear down across a fleet, and an
operator can see none of it without `docker` on the right node. Sprint 4 made
that worse in one specific way: with four symmetric DinD nodes, `docker ps` on
the host shows **no tenant containers at all**. The information exists; reaching
it requires knowing which daemon to ask, which is exactly the knowledge a
platform is supposed to absorb.

Two of the three things the roadmap lists for this sprint are unequal in size:

- **Metrics is largely done.** Eight families ship today — deployments by state,
  ready nodes, active previews, recent tombstones, loop results and durations, DB
  availability, and HTTP requests with bounded route labels. What is missing is a
  consumer, not more metrics.
- **Logs are greenfield.** Nothing in the codebase reads container logs.

So this sprint is logs and a TUI, with metrics as something the TUI surfaces
rather than something to build again.

## The constraint that shapes the design

**The agent has no inbound server. It polls.** That is not incidental — it is
what keeps pgx out of the agent binary and what lets a node sit behind any
network without the control plane needing a route to it. The control plane
therefore cannot call a node to ask for logs.

Every option had to work with that, break it, or defer to the WireGuard mesh
Sprint 4 left unbuilt.

## Keystone decisions (settled)

1. **Logs are fetched on demand, through the existing poll.** The control plane
   records a log request; the agent picks it up on its next tick alongside its
   desired state, reads from Docker, and POSTs the chunk back. Latency is a poll
   interval (~2s). Nothing is stored that nobody asked for.

   *Rejected — continuous shipping:* every container's stdout streamed and stored
   would be the simplest thing to consume, and it makes logs outlive the
   container. It is also a storage problem this platform has no answer for: one
   Postgres, no object store, no retention machinery, and a single misbehaving
   container can flood it. That is a sprint of its own, and it would be built on
   guesses about volume nobody has measured yet.

   *Rejected — control plane calls the agent:* a true live tail with no polling
   latency, but it inverts the connection direction and needs inbound
   reachability to every node. That is the WireGuard mesh, deferred from Sprint 4
   and still deferred. Worth revisiting *after* the mesh exists, at which point
   this design's request/response shape can stay and only the transport changes.

2. **The TUI observes; it does not act.** Fleet, environments, deployments,
   rollout progress, events and logs. Every destructive action stays in the CLI,
   where it is explicit, scriptable and reviewable. A mis-keyed rollback in a
   full-screen UI is a real outage, and the confirmation model that would make it
   safe is more design than the read model needs. Actions can be added once the
   read model has proven itself; the reverse order is much harder.

3. **The TUI is a second consumer of `internal/client`, never a second
   protocol.** That boundary was written down in Sprint 4 for exactly this
   moment: `internal/client` is the only package that knows the API's wire
   format. A TUI that grows its own HTTP calls forks the protocol quietly, and
   the fork is discovered when one of them is fixed and the other is not.

## Non-goals (explicit)

- **No log storage or retention.** A chunk is delivered to the requester and not
  kept. If a container dies, its logs die with it — the same as today, but at
  least reachable while it lives. Say so plainly in the UI rather than implying
  durability.
- **No new metrics.** Surface what exists.
- **No actions in the TUI.** Decision 2.
- **No mesh work.** Decision 1's rejected option depends on it; this sprint does
  not unblock it.
- **No log search or indexing.** Fetch, filter client-side, done.

## Architecture

```
navarch logs dev/app/main/prod --service api --follow
      │
      ├─ POST /v1/envs/{env}/logs        → records a request row, returns its id
      │
      │   agent tick (~2s): GET /desired-state
      │        └─ carries pending log requests for this node
      │           agent reads Docker, POSTs chunks back
      │
      └─ GET  /v1/logs/{request}          → chunks so far, cursor for the next
```

The request is scoped to a deployment or service the node already runs, so the
agent can only be asked for logs of containers it is responsible for. A request
for anything else is rejected at the control plane rather than trusted to the
agent — the agent should never be in the position of deciding whether a request
is legitimate.

## Slices

- **Slice A — log aggregation.** Migration, store, the desired-state extension,
  the agent side, `internal/client`, and `navarch logs` with `--follow`.
- **Slice B — the TUI.** `internal/tui` on top of `internal/client`, read-only,
  with a logs pane that consumes Slice A when it lands.

A and B are deliberately separable: B builds against the 32 client methods that
already exist and treats the logs pane as the last thing to wire, so neither
blocks the other.
