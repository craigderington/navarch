# Sprint 7 Slice A — Operator identity and per-org authorization

**Closes:** S9 (shared operator token, no per-org authorization) from
`docs/audits/2026-08-19-qa-security-audit.md`, and completes S7.
**Parent:** `docs/superpowers/plans/2026-08-19-finish-line.md`.
**Decision taken:** the finish line is a **multi-tenant product**, so this
slice is built in full rather than shrunk to TLS-and-token-hygiene.
**Verified against:** `master` at `b1be6cc`, full suite green with zero skips.

---

## What is actually wrong today

`internal/config/config.go:25-27` names it: one shared bearer token guards
every operator route, and no handler scopes anything to an org. A token
holder reads every org's catalog, secret ciphertext metadata and log output,
and can mutate any org's state. The store is already scoped consistently on
the *node* side — `DesiredStateForNode`, `EncryptedSecretsForNode`,
`TombstonesForNode`, `LogRequestsForNode` — so this slice closes the operator
side to match a boundary the codebase already believes in.

## The shape: authenticate before the mux, authorize inside the handler

`Server.ServeHTTP` authorizes *before* handing off to the mux, and
`r.PathValue` is empty until the mux has matched. That is not a detail — it
is the bug that made every per-node endpoint 401 for a whole sprint, and
`nodeAgentPathID` exists to work around it by re-parsing the path.

**Do not extend that workaround to org scoping.** Resolving "which org does
this request touch" for `/v1/deployments/{id}/promote` means a database
lookup keyed on a path id, and doing that in `ServeHTTP` would require a
second hand-written parser for all ~38 routes — a new instance of exactly the
failure the existing comment warns about, with a worse blast radius, because
a parser that silently fails open leaks a tenant instead of returning 401.

The split instead:

- **`ServeHTTP` authenticates.** Token → identity. It answers *who* only, and
  the answer goes into the request context. No path parsing beyond the
  `nodeAgentPathID` that already exists.
- **Handlers authorize.** Each one resolves the org that owns the object it
  was handed and checks membership, through a small set of helpers that take
  the id already parsed by the mux.

Authentication yields one of three identities, and the type is closed so a
new one cannot be added without every switch failing to compile:

| Identity | Credential | May reach |
|---|---|---|
| `operator` | operator token | operator routes, scoped to their orgs |
| `node` | that node's token | only that node's agent endpoints (unchanged) |
| `registrar` | `COMPOSECTL_AGENT_TOKEN` | **only** `POST /v1/nodes/register` |

### The registrar is the wrinkle, and it must not be skipped

Agents register with the *operator* token today (`internal/agent/agent.go:59`
passes `cfg.AgentToken` into `POST /v1/nodes/register`, and only then swaps to
the per-node token the response issues). If operator identity simply replaces
the shared token, **every agent in the fleet stops being able to register** —
including on the restart that follows the upgrade, which is when it would be
discovered.

So `COMPOSECTL_AGENT_TOKEN` survives this slice, demoted: it authenticates
exactly one route, `POST /v1/nodes/register`, and nothing else. That is
strictly narrower than today (where it opens the whole operator surface) and
it keeps the bootstrap story honest — a node has to be able to join before it
has any identity of its own. Removing it entirely wants a node-enrolment flow
(operator issues a one-time join token), which is a Sprint 8 conversation,
not a prerequisite for closing S9.

## Schema

`0012_operators.up.sql`. Three tables, and the cascade directions are chosen
against the hazard CLAUDE.md already records — an org-delete cascade dropping
a parent before its referrer:

```sql
CREATE TABLE operators (
    id          UUID PRIMARY KEY DEFAULT gen_random_uuid(),
    email       TEXT NOT NULL,
    name        TEXT NOT NULL,
    disabled_at TIMESTAMPTZ,              -- disable, never delete: events reference it
    created_at  TIMESTAMPTZ NOT NULL DEFAULT now()
);

-- One human, one row, regardless of how they capitalised it at signup. Done
-- with an expression index rather than CITEXT deliberately: citext is
-- available on the dev image but is a contrib extension, and this is the
-- one table whose migration must not fail on whatever Postgres the control
-- plane is eventually pointed at. Every lookup goes through
-- GetOperatorByEmail, which lowercases to match.
CREATE UNIQUE INDEX operators_email_key ON operators (lower(email));

CREATE TABLE operator_tokens (
    id           UUID PRIMARY KEY DEFAULT gen_random_uuid(),
    operator_id  UUID NOT NULL REFERENCES operators(id) ON DELETE CASCADE,
    token_hash   BYTEA NOT NULL UNIQUE,   -- SHA-256, exactly as node tokens
    name         TEXT NOT NULL,
    expires_at   TIMESTAMPTZ,
    last_used_at TIMESTAMPTZ,
    created_at   TIMESTAMPTZ NOT NULL DEFAULT now()
);

CREATE TABLE organization_members (
    org_id      UUID NOT NULL REFERENCES organizations(id) ON DELETE CASCADE,
    operator_id UUID NOT NULL REFERENCES operators(id)     ON DELETE CASCADE,
    role        TEXT NOT NULL DEFAULT 'owner',
    created_at  TIMESTAMPTZ NOT NULL DEFAULT now(),
    PRIMARY KEY (org_id, operator_id)
);
```

`operators` is soft-deleted (`disabled_at`) rather than removed, because the
audit trail below points at it and an event whose actor vanished is an event
that cannot be read after an incident — the same reasoning that made
`events.deployment_id` `ON DELETE SET NULL` in `0004`.

Roles are a column, not a system, this slice: every member is `owner` and
nothing reads the value. It exists so adding `viewer` later is a migration of
data rather than of schema. **Do not build role checks now** — an
authorization model with no operational history behind it is the same guess
CLAUDE.md refuses to make about `retired`.

`0013_event_actor.up.sql` adds `events.actor_operator_id UUID REFERENCES
operators(id) ON DELETE SET NULL` plus a denormalized `actor_email TEXT`, so a
disabled-and-purged operator still leaves a readable name in the timeline.

## Store methods

Behind the boundary, as always — handlers never build SQL:

- `CreateOperator`, `GetOperatorByEmail`, `DisableOperator`
- `IssueOperatorToken` (plaintext returned exactly once), `OperatorForToken`
  (constant-time compare against the hash, same shape as `NodeTokenValid`),
  `RevokeOperatorToken`
- `AddOrgMember`, `RemoveOrgMember`, `ListOrgMembers`, `OperatorInOrg`
- `OrgsForOperator` — backs `GET /v1/orgs`, which stops listing every org
- **The resolvers**, one per addressable object, each answering "which org
  owns this": `OrgForApp`, `OrgForStack`, `OrgForStackVersion`, `OrgForEnv`,
  `OrgForDeployment`, `OrgForNode`, `OrgForLogRequest`

The resolvers are the load-bearing new surface. Each is a single indexed join
back to `organizations`, and each returns `ErrNotFound` for a missing object
so a handler cannot distinguish "does not exist" from "not yours" — that
distinction is a cross-tenant existence oracle, and the whole point of this
slice is that one tenant learns nothing about another.

## Handler-side authorization

One helper, used identically everywhere:

```go
// authorize resolves the org owning the addressed object and checks the
// caller's membership, writing the response itself and reporting whether to
// continue — the same contract as pathUUID, so handlers keep their shape.
func (s *Server) authorize(w http.ResponseWriter, r *http.Request,
    resolve func(context.Context) (uuid.UUID, error)) bool
```

**A non-member gets 404, not 403.** 403 confirms the object exists, which
hands a tenant a probe for another tenant's environment ids. The one
exception is a route addressed by an org id the caller is simply not in,
where 404 is also the honest answer.

`POST /v1/orgs` stays self-serve and additionally makes its creator an owner
member in the same transaction — otherwise the creator immediately cannot see
what they just made.

### The test that makes this not-quietly-wrong

The real risk is not a helper that mis-checks; it is a handler that forgets to
call one, which is invisible until a tenant finds it. So the slice ships a
table test driven off the mux's own route list:

`TestEveryOperatorRouteIsOrgScoped` enumerates the registered patterns, and
for each one issues a request as an operator in org B against an object in org
A, asserting 404. A route added later without scoping fails this test rather
than shipping open. Routes that are legitimately unscoped (`/healthz`,
`/metrics`, `POST /v1/orgs`, `POST /v1/nodes/register`, the four node-token
paths) sit in an explicit allowlist in the test, so exempting a route is a
visible edit in a security test and not an omission.

This is the same discipline as the skip-counting test runner: the failure mode
worth engineering against is *silence*.

## CLI

- `navarch login --url … --email …` prompts for a token (never argv — S6's
  lesson) and writes it to `~/.config/navarch/config.yaml`, where the shared
  token lived. Precedence is preserved exactly: `NAVARCH_TOKEN` >
  `COMPOSECTL_TOKEN` > `NAVARCH_AGENT_TOKEN` > `COMPOSECTL_AGENT_TOKEN`,
  tier-first and new-over-legacy second. `TestEnvPrecedenceAcrossTheRename`
  extends rather than changes.
- `navarch whoami` — prints the operator and their orgs. Small, and it is the
  first thing anyone runs when a 404 is ambiguous.
- `navarch org members list|add|remove`.

## Bootstrap

First operator from env at first boot: `NAVARCH_BOOTSTRAP_OPERATOR_EMAIL`,
which creates the operator, issues a token, and **logs it once** at startup.
No seeded migration UUID — the reason `POST /v1/orgs` is self-serve applies
unchanged: a constant in a migration is permanent and identical on every
install. Idempotent, like `BootstrapDevOrg`: an existing email is a no-op, so
a restart does not mint a second token.

The dev stack gets a fixed bootstrap email and the demos log in as it, so
`make demo` keeps working without a human in the loop.

## Order of work

1. Migration + store methods + their tests (org-scoped fixtures, as the
   loop tests already are).
2. Authentication in `ServeHTTP` — identity into context, registrar demoted.
   `TestNodeTokenAuthorizesAgentEndpoints` must keep passing untouched.
3. `authorize` helper + the resolvers, then apply route by route.
4. `TestEveryOperatorRouteIsOrgScoped` — written *before* the last routes are
   converted, so it is seen failing.
5. CLI login/whoami/members; agent unchanged except that its registration
   token is now only good for registering.
6. Event actor threading; `GET /v1/orgs/{org}/events` renders it.

## Exit criteria

- Two operators in different orgs cannot see each other's anything through
  CLI or API — a two-org fixture asserting cross-reads 404 on every scoped
  route.
- `TestEveryOperatorRouteIsOrgScoped` passes with an explicit, justified
  allowlist.
- Every event written from a handler names its actor.
- `COMPOSECTL_AGENT_TOKEN` opens exactly one route; a test asserts it is
  refused on an operator route.
- Full suite green with zero skips; `make demo` suite green from `make nuke`.

## Deliberately not in this slice

- **Roles/permissions beyond membership** — column present, unread.
- **Node enrolment tokens** — the registrar credential stays; replacing it is
  its own decision with its own failure modes.
- **TLS** — Slice C, where it is the subject rather than a footnote.
- **Multi-org node pools** — Slice B decides it. Nodes stay org-scoped here.
