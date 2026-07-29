# Sprint 3 Slice B — Preview Environments

**Date:** 2026-07-28
**Builds on:** Sprint 2 (agent, rollout spine, routing, rollback), Sprint 3 Slice A
(encrypted secrets).
**Branch:** `sprint3-previews`

---

## Context

An ephemeral environment per branch or pull request is the feature that separates
a deploy tool from a platform. It is also the one the compose ecosystem does not
have: the hyperscalers ingest a compose file and convert it to their own
orchestrator, and the self-hosted compose PaaS tier runs `docker compose up -d`
with no revision model to build previews on.

composectl already has the whole spine — environment → deployment → scheduler →
agent → router. A preview environment is that spine plus three things it lacks: a
generated hostname, an expiry, and a way to destroy durable state on purpose.

The database already anticipated this. `environments.slug` is commented
`prod | staging | pr-142`.

---

## Keystone decisions (settled in brainstorming)

1. **A preview is an environment, not a new concept.** Two columns on
   `environments` (`ephemeral`, `expires_at`), not a `previews` table. Nothing
   downstream — scheduler, controller, router, agent — learns a new noun.
2. **Secrets are inherited by copying ciphertext** from an explicitly named source
   environment at creation time. Snapshot semantics.
3. **Teardown of pinned state is driven by explicit tombstones**, never inferred
   from an empty desired-state.
4. **One endpoint does the whole job** — create env, copy secrets, create
   deployment, return the URL.
5. **Volumes get `cc.env` labels** so teardown matches exactly rather than by name
   prefix.

## Non-goals (explicit)

- **No git/webhook integration.** No GitHub App, no PR event handling. The API is
  the interface; CI calls it. Webhook ingestion is a later slice and does not
  change anything here.
- **No idle-based expiry.** TTL is absolute wall-clock from creation. "Expire
  after N hours without traffic" requires request accounting the platform does not
  have.
- **No `DELETE /envs/{env}` or `/extend`.** Deliberately deferred; TTL is the only
  lifecycle control in this slice. Both are small follow-ons once the reap path is
  proven.
- **No preview quota per stack.** A cap on concurrent previews is a real
  requirement eventually; it is not needed to prove the mechanism.
- **No cross-node concerns.** Single node, as with every slice before Sprint 4.

---

## Why teardown is the only hard part

Everything else in this slice composes existing machinery. Teardown does not,
because it collides with an existing invariant.

`DELETE FROM environments WHERE id = $1` is sufficient on the database side —
verified against `0001_init.up.sql`, not assumed:

| Table | Path to `environments` | On delete |
|---|---|---|
| `deployments` | `environment_id` | CASCADE |
| `service_instances` | `deployment_id` → `deployments` | CASCADE |
| `volumes` | `environment_id` | CASCADE |
| `secrets` | `environment_id` | CASCADE |

Deleting the instance rows is how the agent already tears down swappable
containers: reconcile converges to the rows, and a container with no backing row
is GC'd next tick.

**But pinned containers are never GC'd that way, by design.** The agent must not
treat "no desired row" as permission to destroy durable state — a control-plane
outage, a failed migration, or an auth error that yields an empty desired-state
would drop production databases. Previews are created and destroyed constantly, so
a leaked pinned container and volume per PR is a disk-exhaustion bug with a delay
fuse.

The resolution is to make teardown an **explicit instruction** that outlives the
row it refers to. Hence tombstones.

---

## Data model — migration `0003`

```sql
ALTER TABLE environments
  ADD COLUMN ephemeral  BOOLEAN NOT NULL DEFAULT false,
  ADD COLUMN expires_at TIMESTAMPTZ,
  -- An ephemeral env with no expiry is a leak the reaper can never see.
  -- Refuse to store one rather than detect it later.
  ADD CONSTRAINT environments_ephemeral_expiry
      CHECK (NOT ephemeral OR expires_at IS NOT NULL);

CREATE INDEX environments_expiry_idx
    ON environments (expires_at) WHERE ephemeral;

-- Deliberately outlives the environment row it describes: the agent has to be
-- told to destroy durable state, and the row that would have carried that
-- instruction is exactly what was deleted. org_id because nodes are org-scoped
-- and a node must only ever be handed teardowns for its own org.
CREATE TABLE environment_tombstones (
    env8       TEXT PRIMARY KEY,
    org_id     UUID NOT NULL REFERENCES organizations(id) ON DELETE CASCADE,
    created_at TIMESTAMPTZ NOT NULL DEFAULT now()
);

CREATE INDEX environment_tombstones_org_idx
    ON environment_tombstones (org_id, created_at DESC);
```

`env8` is `store.shortID(environmentID)` — the same value already used in every
container name, network name, volume name, and `cc.env` label. The tombstone
stores it rather than the UUID because it is what the agent matches on.

---

## API

### `POST /v1/stacks/{stack}/previews`

```json
{
  "slug": "pr-142",
  "stack_version_id": "…",
  "inherit_secrets_from": "staging",
  "ttl_hours": 24
}
```

```json
201 Created
{
  "environment_id": "…",
  "hostname": "pr-142-hello-93fa144e.preview.localhost",
  "deployment_id": "…",
  "expires_at": "2026-07-29T14:02:11Z"
}
```

**Fields.** `slug` required, validated by the existing store slug rules.
`stack_version_id` required. `inherit_secrets_from` optional — omitted means the
preview starts with no secrets. `ttl_hours` optional, default 24, capped at 168
(one week); above the cap is a 400 rather than a silent clamp.

**Hostname** is generated, never client-supplied:
`{slug}-{stack}-{env8}.{COMPOSECTL_PREVIEW_DOMAIN}`, default domain
`preview.localhost`. The generated left-most DNS label must be ≤63 characters
— counted with the `-{env8}` suffix included — or the request is a 400: a
truncated hostname routes to nothing, and failing at creation beats failing at
first request.

`env8` is required for uniqueness, not cosmetics. `stacks.slug` is only
`UNIQUE (app_id, slug)` and `environments.hostname` has no unique constraint, so
`{slug}-{stack}` collides across two applications that each own a stack `main`
with a preview `pr-1` — the router would then emit two Traefik routers with the
same `Host` rule and pick between them arbitrarily, which is a cross-tenant
misroute into a preview holding inherited secrets. Because `env8` comes from the
environment's own UUID, the handler generates that UUID itself and passes it to
`CreatePreview` rather than letting the column default win: the hostname has to
exist before the row does.

**Error mapping.** Unknown stack or unknown `inherit_secrets_from` env → 404
(consistent with the existing FK-violation → `ErrNotFound` rule). Duplicate slug
within the stack → 409 via the existing `UNIQUE (stack_id, slug)`. A spec whose
required secrets are unset after inheritance → the existing 422 fail-fast, unchanged.

**Transaction.** Env insert, secret copy, and deployment creation happen in one
store transaction. A preview that exists with a hostname but no deployment would
be an env the reaper eventually collects and the user never sees work.

### Secret inheritance

```sql
INSERT INTO secrets (environment_id, key, ciphertext, key_id, version)
SELECT $newEnv, s.key, s.ciphertext, s.key_id, 1
  FROM secrets s
 WHERE s.environment_id = $srcEnv
   AND s.version = (SELECT max(version) FROM secrets s2
                     WHERE s2.environment_id = s.environment_id
                       AND s2.key = s.key);
```

No decryption, no plaintext, no new key material. This works because ciphertext is
sealed to **node** age recipients, not to an environment — verified in
`internal/api/secrets.go`, where `Encrypt` takes the recipients of every
registered node. Copied rows are decryptable by exactly the same agents as the
originals.

Copies land at `version = 1` because version is per `(environment_id, key)` and the
new environment has no history.

Snapshot semantics are correct here: rotating a secret in `staging` should not
mutate a running preview underneath it. A preview lives hours to days.

---

## Reaper — third control-plane loop

`rollout.NewReaper(st, log)` exposing `ReapOnce(ctx) error`, wired in
`cmd/controlplane/main.go` beside the scheduler and controller on the same
`runLoop` helper and tick interval.

One store method, `ExpireEnvironments(ctx) ([]string, error)`, one transaction:

```sql
SELECT e.id, a.org_id
  FROM environments e
  JOIN stacks       s ON s.id = e.stack_id
  JOIN applications a ON a.id = s.app_id
 WHERE e.ephemeral AND e.expires_at < now()
   FOR UPDATE OF e SKIP LOCKED;

INSERT INTO environment_tombstones (env8, org_id) VALUES (…)
    ON CONFLICT (env8) DO NOTHING;

DELETE FROM environments WHERE id = ANY(…);
```

Tombstone **before** delete, in the same transaction: the instruction to destroy
durable state must be durable before the state describing it is gone. If the
transaction aborts, the environment survives and is retried next tick — the safe
direction.

`SKIP LOCKED` because Sprint 4 runs more than one control plane and two reapers
racing the same environment would double-tombstone. The clause costs nothing now.

Tombstones are retained **24 hours**, then swept by the same loop. The window is
how long an agent may be offline and still be told to clean up; past it, the
containers and volumes leak and need manual `docker` removal. 24h covers a node
reboot or a long maintenance window.

---

## Agent

### Desired-state gains a field

```json
GET /v1/nodes/{id}/desired-state
{
  "instances": [ … ],
  "secrets":   { … },
  "teardown_envs": ["a3f9c1d2"]
}
```

Tombstones for the requesting node's org, younger than 24h. The field is additive,
so an older agent that does not decode it simply never reaps — it degrades to the
current leak rather than to an error.

The store method is `TombstonesForNode(nodeID, maxAge)`, not the `TombstonesForOrg`
this document originally named. Deliberate: taking the node id and joining
`nodes n ON n.org_id = t.org_id` derives the org scope from the row the request is
already authenticated against, instead of trusting a caller-supplied org id. This
endpoint hands out destructive instructions, so a caller that could name the org
could name someone else's.

The agent also keeps an in-memory set of env8s whose `RemoveEnv` already
succeeded, since the same tombstone is re-offered on every tick for the whole
24h window. It is not persisted: a restart re-running an idempotent teardown is
harmless, whereas persisting the set could permanently skip one that never
actually completed.

### Driver gains one method

```go
// RemoveEnv destroys everything belonging to an environment: containers
// (including pinned), networks, and named volumes. It is the ONLY path that
// removes pinned containers or volumes, and it fires only on an explicit
// tombstone — never on an empty desired-state.
RemoveEnv(ctx context.Context, env8 string) error
```

Added to `dockerd.Driver` and to the `agent.DockerDriver` interface. Order is
containers → networks → volumes, since a volume in use cannot be removed.
Idempotent: removing an already-gone object is a no-op, so a repeated tombstone
across ticks is harmless and no acknowledgement protocol is needed.

### Volumes must be labelled

The driver has no `VolumeCreate` today — Docker implicitly creates named volumes
when a container mounts them, so they carry **no labels**. Label-based teardown
would silently find nothing.

Add `EnsureVolume(ctx, name string, labels map[string]string)` and call it in
reconcile before `EnsureContainer` for every named mount, labelling with `cc.env`.

Deleting by label rather than by the `cc-{env8}-` name prefix is deliberate.
Docker's name filter is a substring match, and a volume is the one object in this
system whose deletion is unrecoverable. An exact label match is worth one extra
method.

Volumes created before this change carry no label and will not be reaped. This is
acceptable: they belong to long-lived dev environments, not previews, and `make
nuke` clears them.

---

## Component map & boundaries

Unchanged boundaries — this slice adds no imports to any package.

| Package | Change |
|---|---|
| `internal/store` | `CreatePreview`, `ExpireEnvironments`, `TombstonesForNode`, `SweepTombstones`; `Environment` gains `Ephemeral`, `ExpiresAt` |
| `internal/api` | `previews.go` — one handler; `nodes.go` — `teardown_envs` in the desired-state response |
| `internal/rollout` | `reaper.go` — `ReapOnce` |
| `internal/agent` | `DockerDriver` gains `RemoveEnv`/`EnsureVolume`; reconcile calls `RemoveEnv` per teardown env |
| `internal/agent/dockerd` | implements both |
| `internal/config` | `COMPOSECTL_PREVIEW_DOMAIN` |
| `migrations` | `0003_preview_environments.up.sql` / `.down.sql` |

The control plane still must not link the Docker SDK:
`go list -deps ./cmd/controlplane | grep docker/docker` stays empty.

---

## Testing

**`internal/store`** (real Postgres):
- preview creation copies exactly the latest version of each source secret, and
  copies nothing when `inherit_secrets_from` is omitted
- the CHECK constraint rejects `ephemeral = true` with a NULL `expires_at`
- `ExpireEnvironments` collects an expired preview and leaves both an unexpired
  preview and a non-ephemeral `prod` env untouched
- deleting the env really does cascade deployments, instances, volumes, secrets
- a tombstone row exists after the environment is gone

**`internal/rollout`**: `ReapOnce` tombstones before deleting; sweeps tombstones
past 24h; a preview one second from expiry survives the tick.

**`internal/api`**: hostname generation; >63-char label → 400; `ttl_hours` above
the cap → 400; unknown inherit source → 404; duplicate slug → 409.

**`internal/agent`** (fake driver, pure): a `teardown_envs` entry calls `RemoveEnv`
for that env — and, the regression test that matters, **an empty desired-state with
no teardown envs does not call `RemoveEnv` at all**. That is the invariant this
slice comes closest to breaking, so it gets an explicit test.

**`internal/agent/dockerd`** (real daemon): `EnsureVolume` labels; `RemoveEnv`
removes a pinned container and its labelled volume.

## Demo & verification

`make demo-preview` / `scripts/demo-preview.sh`:

1. push the `hello` stack version, set a secret on a `staging` env
2. `POST /previews` with `inherit_secrets_from=staging`, `ttl_hours` short enough
   to observe
3. assert the returned deployment reaches `live` unaided
4. `curl -H "Host: pr-142-hello-93fa144e.preview.localhost"` through Traefik and assert the
   inherited secret is visible in the response — proving inheritance end to end
5. wait for expiry, then assert: env gone from `GET /envs`, containers gone
   (including the pinned one), and the named volume gone from `docker volume ls`

Step 5 is the whole point of the slice. A demo that stops at "the preview works"
proves the easy half.

`make demo`, `demo-rollback`, and `demo-secrets` must all still pass — the
regression surface is the shared reconcile path and the new `EnsureVolume` call in
it.

---

## Open questions

None blocking. Deferred by choice: preview quotas, idle-based expiry, explicit
`DELETE`/`extend` endpoints, and git webhook ingestion.
