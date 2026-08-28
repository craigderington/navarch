# Sprint 7 Slice B — The tenant edges

**Closes:** S7 (registration silently overwrites a node's age recipient) from
`docs/audits/2026-08-19-qa-security-audit.md`, and settles the multi-org
node-pool question CLAUDE.md has deferred since Sprint 2.
**Parent:** `docs/superpowers/plans/2026-08-19-finish-line.md`.
**Depends on:** Slice A — "rotate becomes an operator action" is only a
sentence worth writing once operators exist.
**Verified against:** `master` at `d5fd470`, full suite green, demos green.

---

## S7: a recipient change stops being something a node can just do

`RegisterNode` upserts by `(org_id, hostname)` and assigns
`age_recipient = EXCLUDED.age_recipient`. Slice B of the audit work made that
change *audited*; it is still **accepted**. Anyone who can register a node —
which, after Slice A, means anyone holding the shared service token — can
re-register an existing hostname with their own recipient, and every secret set
afterwards for environments homed there is sealed to their key.

An audit event is the right thing to have and the wrong thing to stop at: it
records the redirect after it has happened, and the operator learns about it by
reading a timeline they have no reason to be reading.

### The shape: advertise, don't assign

A re-registering node that presents a *different* non-empty recipient no longer
changes anything. The value is recorded as **pending**, the effective recipient
is untouched, and an event says so. An operator promotes it with an explicit
action:

```
navarch node rotate-recipient dev/dev-node-2
```

The node keeps registering, keeps heartbeating and keeps its capacity
throughout — refusing the registration outright would take a node's capacity out
of the fleet over a key it has not been allowed to use yet, which punishes the
fleet for an attacker's request.

The operator does not type the key. They *confirm the one the node is already
advertising*, which is the only workable flow: the agent generates its identity
and the control plane never sees the private half. What the operator is
asserting is "this node legitimately has a new key", which is exactly the
judgement a human has and the control plane does not.

### Three cases, and why they differ

| Incoming | Recorded | Result |
|---|---|---|
| non-empty, no recorded recipient | none | **set directly** — nothing is displaced |
| non-empty, same as recorded | same | no-op; clears any stale pending |
| non-empty, differs | some | **pending**; effective recipient unchanged |
| empty | some | **ignored** — see below |

The first case is not a rotation. A node with no recorded key was excluded from
every prior sealing decision, so there is no ciphertext that becomes
unreadable and no key being displaced.

**An empty incoming recipient must not erase a recorded one**, and today it
does: the column is written from `NULLIF($7,'')`, so an agent that comes up
without an identity file NULLs the node's recipient. That is a
denial-of-decryption any node can inflict on itself by failing to read a file,
and it silently changes which recipients future secrets are sealed to. An agent
that has genuinely lost its identity generates a new one and advertises *that*,
which is the rotation case; empty means "this agent is not doing secrets right
now", and the right response to that is to keep what we know.

### What pending deliberately does not do

`RecipientsForEnvironment` never reads it. Sealing to a key nobody has approved
is the whole failure this closes, and a pending recipient is a request, not a
credential. The consequence is honest and visible: a node that rotated its key
without approval cannot decrypt secrets sealed to the old one, its containers
fail to start, and `failure_reason` says so — a loud failure an operator can act
on, rather than a quiet redirect they cannot see.

---

## The multi-org node pool question: decided, and the answer is no

CLAUDE.md defers this to exactly here: *"nodes are org-scoped today; either keep
it (single-tenant fleets per org) or design shared-node pools with placement
isolation. Not building it is a valid answer; not deciding is not."*

**Decision: nodes stay org-scoped. A node is a trust boundary, not a resource
pool.**

Two things make a shared node a cross-tenant exposure, and neither is fixable
by placement rules:

1. **One agent, one age identity, every environment on that node.** The agent
   decrypts secrets for every environment it hosts, using one identity that
   persists on its own volume. Two tenants on one node means one key opens both
   tenants' secrets, and compromising that node yields both. The entire point of
   sealing per-node is that the blast radius of a node is the node.
2. **One kernel.** Tenant containers are isolated by Docker, which is not a
   boundary against a determined tenant — the parser rejects `privileged`,
   `cap_add`, `pid`, `devices`, `security_opt` and the rest precisely because
   "host escape if applied". Those rules keep an honest tenant from breaking
   the platform; they are not an argument that a hostile one cannot.

Navarch's isolation story — a network per revision, no shared mesh, secrets
decrypted per environment — is isolation *within* a trust domain. Making a node
safe to share needs per-tenant sandboxing (a VM or a gVisor/Kata-class runtime
per tenant), which is a different product with a different runtime abstraction.
`internal/agent/dockerd` is deliberately the only package that knows the runtime,
so that door is not nailed shut — but walking through it is a sprint about
sandboxing, not a placement flag.

The practical cost is honest and small: an organization brings its own nodes.
For the single-operator and small-team cases this platform is built for, that is
what people do anyway.

---

## Work

1. `0014_node_pending_recipient.up.sql` — `nodes.pending_age_recipient TEXT`.
2. `RegisterNode`: the table above, including the empty-does-not-erase fix.
   `node.recipient_rotation_pending` replaces `node.recipient_rotated` on the
   registration path; the latter now means the promotion actually happened.
3. `RotateNodeRecipient(ctx, nodeID)` — promotes pending, clears it, appends
   `node.recipient_rotated` (the actor arrives on the context, per Slice A).
   `ErrConflict` when nothing is pending, so a double-click is not a silent
   success.
4. `POST /v1/nodes/{id}/rotate-recipient`, org-scoped like every other
   node route. `TestEveryOperatorRouteIsOrgScoped` picks it up automatically.
5. Client + `navarch node rotate-recipient`; `node get` and `node list` surface
   a pending rotation, because an approval nobody can see will not be given.
6. Tests, each verified to fail without its fix: a rotation does not take
   effect, an empty does not erase, promotion works and is idempotent-refusing,
   and `RecipientsForEnvironment` never returns a pending value.

## Exit criteria

- A re-registration cannot change an effective recipient; only an operator can.
- Secrets are never sealed to an unapproved key.
- The node-pool decision is recorded in CLAUDE.md and the README's
  deliberate-non-features list, not just here.
- Full suite green with zero skips; demo suite green from `make nuke`.
