# Sprint 9 — Bring your own infrastructure

**Goal:** a customer runs the machines their containers run on; Navarch runs the
control plane. They sign up, point an agent at us, and deploy — without ever
opening an inbound port, and without us being able to read their secrets.

**Why this shape and not hosted multi-tenant:** Sprint 7 Slice B decided that a
node is a trust boundary, because one agent holds one decryption identity for
every environment on it and tenant containers share a kernel. That decision
makes shared hosted nodes a sandboxing project. It makes *this* product nearly
free, because the same property that stops us hosting other people's containers
safely is the property that makes it safe for them to host their own.

**Verified against:** `master` at `32d5e8d`, full suite green, nine demos green.

---

## What already works, and why that is the whole point

**The agent runs no server.** It has no `ListenAndServe` anywhere. Every
interaction is the agent dialling out: `GET /desired-state`, `POST /report`,
`POST /heartbeat`, `POST /nodes/{id}/logs`, and the tombstone poll. A customer's
box behind NAT, on a home connection, on a laptop, joins the fleet with no
firewall change at all.

**There is exactly one inbound path in the entire system**, and it is not to the
agent — it is the router reaching a tenant container at
`advertise_addr:published_port`. `NodeAddr` is consumed in precisely one place,
`internal/rollout/controller.go:192`, where routes are built. Everything else
that looks like it might need to reach a node does not.

That single fact is what makes this sprint small. **Move the router to the
customer's side and no inbound connection to their infrastructure exists at
all.**

**Secrets already work.** `RecipientsForEnvironment` seals to the *home node's*
age recipient, and the agent decrypts at container start with a key that never
leaves their machine. The control plane holds ciphertext it cannot open. This
needs no change, and it is the commercial claim: *we cannot read your database
password*, backed by code rather than policy.

---

## The three things that block it

### 1. Enrolment: the shared token can join any organization

`handleRegisterNode` resolves the org from the request body, then checks
membership **only if the caller is an operator**
(`internal/api/nodes.go`). A caller holding the shared
`COMPOSECTL_AGENT_TOKEN` gets no org check whatsoever — it can register a node
into any org by naming its slug.

That is correct today: one token, one operator, one trusted fleet. It is fatal
the moment a customer holds it. Slice A recorded the exit as "an operator-issued
node join token"; this is where that gets built.

`0015_node_join_tokens.up.sql`:

```sql
CREATE TABLE node_join_tokens (
    id          UUID PRIMARY KEY DEFAULT gen_random_uuid(),
    org_id      UUID NOT NULL REFERENCES organizations(id) ON DELETE CASCADE,
    token_hash  TEXT NOT NULL UNIQUE,   -- SHA-256 hex, as every other token here
    name        TEXT NOT NULL,
    expires_at  TIMESTAMPTZ,
    -- A join token is a bearer credential handed to a machine, so it should be
    -- possible to make it single-use. NULL means unlimited, which is what a
    -- fleet that autoscales wants.
    max_uses    INTEGER,
    uses        INTEGER NOT NULL DEFAULT 0,
    created_by  UUID REFERENCES operators(id) ON DELETE SET NULL,
    created_at  TIMESTAMPTZ NOT NULL DEFAULT now()
);
```

**The org comes from the token, never from the request body.** That is the whole
fix: a join token *is* the statement "this machine may join this organization",
so letting the body name an org would reintroduce exactly the hole being closed.
A body naming a different org is a 400, not a silent override — a node that
believes it joined `acme` and actually joined `dev` is worse than a refusal.

`COMPOSECTL_AGENT_TOKEN` is then demoted a second time, to `GET /metrics` alone.
The dev stack pins a join token the way it already pins the bootstrap operator
token (`COMPOSECTL_BOOTSTRAP_JOIN_TOKEN`), which keeps `make up` a one-liner and
is precedent this codebase has already set rather than a new kind of shortcut.

New: `navarch node join-token create|list|revoke`, org-scoped like every other
node route, and the plaintext shown exactly once.

### 2. Routing: one global router that must reach the customer's containers

`ListLiveRoutes(ctx, strand)` takes no org. One Traefik serves every
organization, and the control plane writes its config to a local directory
(`COMPOSECTL_ROUTER_DIR`). For a customer behind NAT that router can never
connect, and for a customer with a public address it means their ingress traffic
transits us for no reason.

**The customer runs the router, and their agent configures it.**

- `ListLiveRoutesForOrg(ctx, orgID, strand)` — the same query with one predicate
  added. The global form stays for the self-hosted single-tenant case.
- `GET /v1/nodes/{id}/routes` — a node-token route, alongside `/desired-state`,
  returning that node's org's routes in the control plane's own vocabulary
  (hostname, target, port), *not* Traefik's.
- The agent gains an optional router mode: with `COMPOSECTL_ROUTER_DIR` set, it
  fetches routes each tick and writes the file for a Traefik running beside it.

The agent imports `internal/router` to do it, which keeps that package the only
thing in the tree that knows Traefik's config shape — the boundary is preserved
rather than crossed. The endpoint returning our vocabulary rather than Traefik's
is what makes a future Caddy or nginx backend a change in one package.

**Why the agent and not Traefik's HTTP provider.** Traefik v3 can poll a URL
directly, which needs no agent involvement — but it would put a bearer token in
Traefik's command line, make the control plane serve Traefik-shaped JSON as a
public API contract, and give us no place to stand when the config shape needs
to change. The agent already polls, already holds a per-node credential, and
already has the file-writing code. Worth revisiting if customers want a router
on a machine with no agent.

**One behavioural difference to accept deliberately.** Route withdrawal on a
silent node (`COMPOSECTL_ROUTE_STRAND_SECONDS`) currently works because the
control plane's router sees every node's heartbeat. With a customer-side router,
the config only updates while *their* router's agent is polling. If that machine
is down, its routes go stale and keep serving. That is the right answer for BYO —
a customer's router serving a customer's node is their failure domain, not ours,
and withdrawing routes for a fleet we cannot see would be asserting something we
do not know. Say it in the docs rather than discover it.

### 3. Preview domains are server-wide

`COMPOSECTL_PREVIEW_DOMAIN` is one value for the whole control plane, and
preview hostnames are generated under it. A BYO customer owns their own DNS, so
this becomes `organizations.preview_domain`, falling back to the server default.
`0016_org_preview_domain.up.sql`, and the generation in `handleCreatePreview`
reads the org's value.

---

## Signup, now that it is possible

With enrolment scoped to an org, self-service signup stops being dangerous:

- `POST /v1/signup` — unauthenticated, creates operator + their first org +
  membership + a first token, in one transaction. The second unauthenticated
  route in the system, and it needs the scrutiny that implies.
- **Email verification is not optional.** The email *is* the identity
  (`operators.email` is uniquely indexed, case-insensitively) and
  `GetOperatorByEmail` is how an invite finds an existing person. Unverified
  signup lets anyone squat an address and then be silently added to somebody's
  org by an invite meant for the real owner.
- **Rate limiting.** The 2026-08-19 audit recorded that there is none anywhere
  (S10). Bounded bodies and auth were the reason that was acceptable; an
  unauthenticated route that writes rows removes it.
- **Quotas.** Orgs, apps, stacks and previews are all currently unbounded per
  operator. A free tier needs a ceiling before it needs a billing integration.

---

## Order of work

1. **Join tokens** (migration, store, API, CLI, and the demotion of the shared
   token to `/metrics`). Ends with a test that a join token for org A cannot
   register a node into org B — the hole this closes, asserted directly.
2. **Per-org routing**: `ListLiveRoutesForOrg`, `GET /v1/nodes/{id}/routes`,
   agent router mode. Ends with `make demo-byo`: a second Traefik configured by
   an agent, serving a stack the platform's own router cannot reach at all.
   That last clause is the assertion — a demo where both routers work proves
   nothing.
3. **Per-org preview domain.**
4. **Publish the images.** `deploy/production/compose.yaml` already references
   `ghcr.io/craigderington/navarch/agent`, which nothing pushes. `make release`
   builds CLI binaries only. BYO is undeliverable until `docker pull` works for
   a stranger.
5. **Signup**, with verification, rate limiting and quotas — or a deliberate
   decision to stay invite-only and sell to teams rather than individuals.

Steps 1–4 are the product. Step 5 is a separate decision about who buys it.

## Exit criteria

- A machine with **no inbound ports open** runs a stack deployed from a hosted
  control plane, served by a router on that same machine.
- A join token for one org cannot enrol a node into another, pinned by a test.
- The control plane never connects to customer infrastructure — assertable,
  because `NodeAddr` has exactly one consumer and it moves to the agent side.
- Existing self-hosted single-tenant installs keep working untouched: the global
  router, the shared token's metrics role, and the server-wide preview domain
  all remain the defaults.

## Not in this sprint

- **Hosted multi-tenant nodes.** Still needs per-tenant sandboxing; the
  trust-boundary reasoning in CLAUDE.md is unchanged by any of this.
- **A web UI.** Everything here is CLI and API. That is a separate question
  about who is buying, and it is a bigger sprint than this one.
- **Tunnelled ingress** (an agent-to-control-plane tunnel carrying tenant
  traffic, the WireGuard mesh the Sprint 2 docs imagined). Customer-side routing
  makes it unnecessary for BYO, and it is the wrong thing to build before
  knowing whether anyone wants ingress they do not control.
