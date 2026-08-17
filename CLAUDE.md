# composectl

A container platform for **Docker Compose stacks**, not single containers.
Lightsail gives you one image per deployment; composectl treats an entire
compose stack as the deployable unit, with versioned revisions and
zero-downtime rollouts across the whole stack.

Go + Postgres. Terminal-first. Craig's project.

---

## Ground rules

- **Never push to the GH repo.** Commit locally, leave pushing to Craig.
- **No live production access.** Local dev stack only.
- **Random high ports.** Postgres `5473`, API `8417`. Never bind 3000/5000/8000/9000 — too much else is already running there.
- **Postgres always.** No SQLite fallback, not even for tests.
- Work in sprints against a checklist. Plan → Work → Assess → Build → Test → Deploy → Iterate.

---

## The name: Navarch

**The product is Navarch. The CLI binary is `navarch`. Everything else still
says composectl, on purpose** — the rename was scoped to what a user types, and
the rest is deliberately deferred rather than half-finished. Do not "complete"
it opportunistically; each remaining layer has a reason:

| Layer | State | Why |
|---|---|---|
| CLI binary, help, usage strings, README | **renamed** | user-facing |
| `NAVARCH_*` env vars, `~/.config/navarch/` | **renamed**, legacy read as fallback | see below |
| Go module `github.com/craig/composectl` | unchanged | churns every import for no user-visible gain; one atomic commit whenever it's wanted |
| `cc-` container/network/volume prefix, `cc.*` labels | unchanged | **this is the dangerous one** |
| Control plane / agent `COMPOSECTL_*` vars | unchanged | set by `compose.yaml`, not by hand |
| Postgres role + database `composectl` | unchanged | pure data migration, zero user-visible gain |

**Why `cc-` must not be renamed casually.** The prefix and labels are not
branding — nobody types them — and renaming them breaks three things at once:
the agent adopts pinned containers by the *stable name*
`cc-{env8}-pinned-{service}`, so a renamed scheme creates a second pinned
container over the same volume; reconcile GC only sees `cc.env`-labelled
containers, so every pre-rename container becomes invisible and leaks; and
`RemoveEnv` matches volumes on that label, recreating the unlabelled-volume
loose end above for every existing environment. If it is ever done, it belongs
at a `make nuke` boundary with a manual sweep, or behind a dual-read window.

**The env-var fallback is a real fallback, not decoration.** `NAVARCH_TOKEN`
wins over `COMPOSECTL_TOKEN`, and both outrank `NAVARCH_AGENT_TOKEN` /
`COMPOSECTL_AGENT_TOKEN` — the dedicated CLI token beating the shared stack
token is pre-existing behaviour, preserved deliberately, so precedence is
tier-first and new-over-legacy second. `~/.config/navarch/config.yaml` is read
before `~/.config/composectl/config.yaml`. This exists because these variables
carry the bearer token and an unset token is a hard failure, not a degraded
mode. `TestEnvPrecedenceAcrossTheRename` covers both directions. The legacy
half is removable once nothing sets it.

`docs/navarch-brand-and-naming-guide.md` is gitignored and not present in the
working tree; if it exists elsewhere it, not this table, is the authority on
naming.

**References resolve by parsing, never by shape** (`internal/cli/resolve.go`).
Anywhere the CLI takes an id it also takes a slug path rooted at the org —
`dev`, `dev/shop`, `dev/shop/main`, `dev/shop/main/staging`, and `dev/hostname`
for a node — with segments free to mix ids and slugs. Two properties are
load-bearing:

- **A UUID is itself valid slug syntax** — lowercase alphanumeric with internal
  dashes, 36 chars, inside the store's 63-char cap — so nothing stops a stack
  being named after one. `isID` therefore *parses*, and accepts only the
  canonical dashed form: `uuid.Parse` also takes a bare 32-hex string, which is
  indistinguishable from an ordinary slug, and treating that as an id would make
  one unlucky name permanently unaddressable.
- **An id costs no request.** Each resolver returns immediately for an id, so
  existing scripts issue exactly the calls they always did and only a path pays
  the one-request-per-level walk. `TestResolveIDCostsNoRequests` asserts this on
  the request log, not the return value.

Wrong-depth paths are usage errors (exit 2) naming the expected shape; a
resolution miss is a runtime error (exit 1) naming what was not found and where.
An ambiguous node hostname errors with the candidates rather than picking, but
that branch should be unreachable: `nodes` carries `UNIQUE (org_id, hostname)`
and the resolver scopes to one org. Deployments are id-only: they have
revisions, not slugs.

---

## Quickstart

```bash
make tidy         # only after changing deps — go.sum is committed
make up           # postgres + migrations + control plane + node agent
make health
make validate     # parse examples/webapp, see classification
make demo         # agent-driven rollout to healthy, over HTTP, real containers
make demo-failure # bad image → failed, prior deployment untouched
make demo-fleet   # two nodes, two daemons: ingress pinned, worker spread
make agent-logs   # tail the node agent
make psql         # database shell
make logs         # tail control plane
make nuke         # down + delete volumes
```

---

## Architecture: the boundaries are load-bearing

```
cmd/controlplane      API server + scheduler/controller loops
cmd/agent             node agent binary (Sprint 2)
cmd/navarch           operator CLI binary (dir named for the binary, not the module)
internal/spec         normalized DeploymentSpec — the platform's vocabulary
internal/parser       the ONLY package importing compose-go
internal/store        the ONLY package importing pgx
internal/api          thin handlers: decode, delegate, encode
internal/rollout      scheduler + rollout controller (control-plane loops)
internal/agent        node reconciliation loop (imports neither pgx nor compose-go)
internal/agent/dockerd the ONLY package importing the Docker SDK
internal/router       the ONLY package knowing Traefik's config shape
internal/client       the ONLY package knowing the HTTP API's wire format
internal/cli          argv parsing, output formatting; no HTTP of its own
internal/secrets      age sealing/opening
internal/metrics      Prometheus text-format registry
internal/config       env-var config (control plane only)
migrations/           golang-migrate SQL
```

**Do not violate these boundaries.** They're the main design decision in
the codebase:

- Only `internal/parser` imports compose-go. Everything downstream speaks
  `spec.DeploymentSpec`.
- Only `internal/store` imports pgx. Handlers never build SQL.
- Only `internal/agent/dockerd` imports the Docker SDK. Everything above it
  speaks `dockerd.ContainerSpec` and the `Driver` methods, so the container
  runtime could be swapped without touching reconcile logic.
- Handlers decode, delegate, encode. Business logic belongs in store or
  parser, not in `internal/api`.
- Only `internal/client` knows the API's wire format — URLs, envelopes,
  status-code mapping. `internal/cli` parses argv and formats output; it must
  never build an HTTP request itself. This is the boundary Sprint 5's TUI
  depends on: a second front end is a second consumer of `client`, not a
  second implementation of the protocol.

Named volumes are created explicitly (`EnsureVolume`, with a `cc.env` label)
rather than left to Docker's implicit creation on first mount. An implicit
volume carries no label, and teardown (`RemoveEnv`) matches on that label
because it has to be exact — a name-substring match risks catching a volume
that outlived its environment for an unrelated reason. Volumes created before
this landed carry no label and will not be reaped; they need manual cleanup.

**The boundaries hold across binaries, not just packages:**

- **The agent never imports pgx.** It speaks only the control plane's HTTP
  API (register / desired-state / report / heartbeat). This is why the
  Sprint 2 agent *polls* rather than consuming the `node_{id}` NOTIFY — a
  `LISTEN` would drag pgx into the agent binary.
- **The agent's config loader lives in package `agent`, NOT `internal/config`.**
  If it lived in `internal/config`, the control plane (which imports that
  package) would transitively link the Docker SDK via `agent → dockerd`.
  Guard command: `go list -deps ./cmd/controlplane | grep docker/docker`
  must return nothing.

If a change seems to require crossing one of these lines, that's a signal
the abstraction is wrong — raise it rather than routing around it.

---

## The core concept: swappable vs pinned

Blue/green runs two copies of a stack simultaneously. Some services must not
be duplicated. **Every service declares which it is** — the platform does not
infer it:

```yaml
x-composectl:
  rollout: swap   # duplicated blue/green
  # rollout: pin  # one instance, attached to every revision's network
```

| Classification | Declared | Rollout behavior |
|---|---|---|
| **swappable** | `rollout: swap` | duplicated blue/green |
| **pinned** | `rollout: pin` | runs once, shared across revisions |

**Omitting it is a parse error.** There is no default, deliberately: the
author who does not realise blue/green changes cardinality is exactly the one
an optional field fails to protect.

The declaration drives rollout behavior, node pinning, and
`PeakMemoryBytes()` (swappable counted twice, pinned once).

**The volume rule survives as a constraint, not a definition.** `rollout:
swap` on a service mounting a *writable* named volume is rejected — two
revisions writing one filesystem is the failure the classification exists to
prevent, and it is the one thing an author may not declare their way into.
Read-only mounts are exempt and may be `swap`. A pinned ingress is still
rejected; it could not participate in blue/green.

### Why it is declared rather than inferred

It used to be inferred: writable named volume → pinned, else swappable. That
computed the wrong property. A writable volume answers *"would two writers
corrupt this filesystem?"*; blue/green needs *"may this run twice?"*. They
coincide for Postgres, which is why the conflation held for two sprints.

They come apart at the **effect-singleton**: a scheduler, cron runner,
migration step or broker that owns no local state but whose correctness
assumes one instance. It mounts nothing, so the old rule called it swappable
and ran two copies against the shared pinned database — every periodic task
firing twice, external side effects included, while the rollout reported
success. Nothing in a compose file distinguishes that from a stateless
worker.

The sharpest case is the broker. A Redis run correctly as `--save ""
--appendonly no` mounts nothing, so it was duplicated — and since swappable
containers live only on their own revision network (`reconcile.go`, pinned
containers alone are attached to *every* revision's network), each revision
got its own keyspace. Blue's lock and green's lock were taken in different
Redises: both succeed, both hold "the" lock. Duplicating the stack destroyed
the mechanism authors use to make singletons safe. `examples/webapp` carries
this case with the reasoning inline.

Note what this does **not** fix: declaring `swap` on a worker with a
scheduler embedded in it still double-fires. The platform stops guessing;
it does not make the author right. That is the intended trade — a wrong
answer the author typed beats a wrong answer the platform inferred.

Anything touching rollout, scheduling, or capacity must respect it.

---

## Rejected compose directives

Rejected **loudly**, never silently dropped — a stack must never run
differently than its author expects. All violations collect in one pass
so the user fixes everything at once.

| Directive | Reason |
|---|---|
| `build:` | pre-build and push; platform does not build |
| `include`, `extends`, `env_file`, `label_file` | control plane must not read tenant paths |
| `privileged`, `cap_add`, `cap_drop` | breaks isolation |
| `pid`/`ipc`/`uts`/`devices`/`security_opt`/`sysctls`/`runtime` | host escape if applied |
| `container_name` | collides between revisions |
| `ports: "8080:80"` | host port collides between revisions; use `x-composectl.ingress` |
| bind mounts | host paths not portable across nodes |
| `network_mode` | each revision gets an isolated network |
| `deploy.replicas` | scaling is the platform's job |
| `scale:` (legacy) | same, checked independently — see below |
| volume mounted by 2+ services | shared writable volumes unsupported |
| volume declared but unmounted | almost always a typo |
| anonymous volumes (`- /data`) | untracked durable state; would pin the service out of blue/green |
| single-character volume names | compose-go reads `d:/mnt` as a Windows drive path (see below) |

Two directives are rejected *conditionally*, which the table flattens —
check the code before assuming: `network_mode` is permitted when it is
exactly `bridge`, and `deploy.replicas` / `scale` are permitted when
exactly `1`. `build:` is not detected as such; it surfaces as the `image is
required` error, because compose-go leaves `Image` empty.

**Read `scale` from `svc.Scale`, never through `Deploy.Replicas`.**
compose-go folds one into the other only inside the consistency pass, and
only when a `deploy:` block already exists. Reading it through `Deploy`
therefore misses a bare `scale: 3` outright — and silently deploys one
replica where three were asked for.

Graph-level checks: `depends_on` must reference real services and be
acyclic; at most one ingress service; an ingress service may not be
pinned (it couldn't participate in blue/green).

Defaults when unspecified: **250 millicpu**, **256 MiB**.

---

## Secrets

Values containing `${secret:KEY}` are stored as **templates** in
`Service.SecretEnv` and expanded by the agent at container start.
Plaintext never reaches the control plane or the database.

The pattern is deliberately **unanchored** — a secret is usually embedded
in a larger string:

```yaml
DATABASE_URL: postgres://app:${secret:db_password}@db:5432/app
```

An anchored pattern stores that as a literal and hands the container a
broken URL, failing only at runtime. This was a real bug, already fixed.
Don't reintroduce it. `spec.SecretRefPattern` is exported so the agent
expands using exactly the syntax the parser recognized.

**Overlay precedence:** env config supplies defaults; values set
explicitly in the compose file win. Secret templates always win over env
config. Reversed precedence would let an environment-wide `LOG_LEVEL`
silently clobber a worker deliberately set to debug — also a real bug,
also already fixed.

---

## Preview environments

`POST /v1/stacks/{stack}/previews` creates an ephemeral environment, copies
another environment's secret **ciphertext** into it, and deploys — one call,
one URL back, so CI doesn't orchestrate three requests.

**Hostname is generated, never client-supplied:**

```
{slug}-{stackSlug}-{env8}.{COMPOSECTL_PREVIEW_DOMAIN}
pr-1-main-93fa144e.preview.localhost
```

`env8` is load-bearing, not decoration. `stacks.slug` is only
`UNIQUE (app_id, slug)`, so `{slug}-{stack}` alone collides between two apps —
same org or different ones — that each own a stack `main` with a preview
`pr-1`. Two defences now stand behind that, and they are not interchangeable:
`0006_unique_hostname` adds a partial unique index on `environments (hostname)`,
which turns a collision into a 409 at create time rather than a misroute; `env8`
prevents the collision arising at all, so an ordinary preview never sees that
409. Without the index, the older failure was silent — `ListLiveRoutes` returns
both, Traefik gets two routers with the same `Host` rule, and the winner is
arbitrary: a cross-tenant misroute into someone else's branch with its inherited
secrets. `env8` comes from the environment's own UUID, so it is
unique by construction — which is also why `handleCreatePreview` generates
that UUID itself and inserts it explicitly rather than letting the column
default win. The generated left-most label must be ≤63 characters, checked
**including** the `-{env8}` suffix; over that is a 400.

**`COMPOSECTL_PREVIEW_DOMAIN`** (control plane, default `preview.localhost`)
is the wildcard domain those hostnames are generated under. The default works
on a dev box with no DNS at all, because Traefik routes on the `Host` header:
`curl -H "Host: pr-1-main-93fa144e.preview.localhost"` reaches it.

**TTL is the only lifecycle control.** There is no `DELETE /v1/envs/{env}`
and no extend endpoint — don't go looking for one. `ttl_hours` defaults to 24
and is capped at 168 (one week); above the cap is a 400, not a silent clamp,
because storing a different TTL than the one asked for makes the API lie.
Everything else is the reaper's job.

**Teardown runs on a tombstone, never on inference.** The reaper writes an
`environment_tombstones` row *before* deleting the environment, in the same
transaction — the instruction to destroy durable state must be durable before
the state describing it is gone. `TombstonesForNode` hands each node only its
own org's tombstones, and the agent's `RemoveEnv` is the ONLY path that
destroys pinned containers or named volumes. An empty desired-state is a
control-plane outage, not "drop the database"; `internal/agent/reconcile_test.go`
guards that non-vacuously.

The agent keeps an in-memory set of env8s it has already torn down, because
the same tombstone is re-offered every tick for its full 24h retention. It is
deliberately not persisted: a restart re-running an idempotent teardown is
fine, permanently skipping one that never completed is not.

---

## Invariants

**`deployments` is append-only.** Rollback = promote an older revision,
never mutate. Only `state`, `failure_reason`, and timestamps are updated.

**Deployment state machine** (enforced in SQL via `WHERE state = ANY(...)`,
so a buggy agent cannot drive a deployment backwards):

```
pending → scheduling → starting → healthy → live → superseded
   ↓          ↓           ↓          ↓                  ↓
 failed ←─────┴───────────┴──────────┘              stopped
```

Two Postgres gotchas around these enum columns, both real bugs already hit:

- **pgx cannot encode a `[]DeploymentState` against the enum array.** Query
  `state = ANY($n)` with a named-string slice fails at runtime (`unknown
  type OID`). Pass `[]string` and cast the column: `state::text = ANY($n)`
  (see `statesToText`). `UpdateDeploymentState` had this latent from Sprint
  1 — untested because the old demo drove states with raw SQL.
- **`service_instances.node_id → nodes` and `deployments.stack_version_id
  → stack_versions` lack `ON DELETE CASCADE`.** An org-delete cascade can
  drop a parent before its referrer. Test cleanup deletes bottom-up
  (instances → deployments → nodes → org); any real org/node delete path
  must too.

**Promotion is atomic.** `PromoteDeployment` supersedes the old revision,
marks the new one live, and repoints the environment in one transaction.
A partial promotion leaves the router pointing at a deployment the
database doesn't consider live.

**Slot alternation.** Each deployment gets `blue` or `green`, opposite the
current live one. First deploy is blue. Project name:
`cc-{env8}-r{revision}-{slot}`.

**One active deployment per environment**, enforced by partial unique
index `deployments_one_active_idx`. Surfaces as `ErrConflict` → HTTP 409.

**Spec digests must be stable.** `json.Marshal` sorts map keys but *not*
slices — so the parser sorts `Ports`, `Mounts`, and `Depends` before they
reach the digest. Without that, identical compose files produce different
digests and trigger spurious redeployments. Any new slice field in
`DeploymentSpec` must be sorted at parse time.

That invariant has one concrete consumer: `CreateStackVersion` compares the
new digest against the latest version's and returns the **existing** row
unchanged when they match, so re-pushing an unmodified stack doesn't
manufacture version churn. An unstable digest therefore doesn't just cause
redeploys — it silently defeats this dedupe.

**The `environments` ⇄ `deployments` foreign key is circular**, and
`environments_live_deployment_fk` is `DEFERRABLE INITIALLY DEFERRED` to
make it satisfiable. `PromoteDeployment` relies on this: it updates
deployments and repoints the environment inside one transaction, and the
constraint is only checked at commit. Don't "clean up" that deferral.

**The parser loads compose twice, on purpose.** `Parse` calls the loader
first with `SkipConsistencyCheck = true`, collects every platform
violation, and only if that comes back clean loads a second time with the
check enabled. Both halves are load-bearing:

- Collapsing it to one *checked* load restores the original bug —
  compose-go's check is fail-fast, so a dependency cycle or an unknown
  `depends_on` returns immediately and every other violation in the file
  goes unreported. That breaks "all violations in one pass".
- Dropping the second load silently discards the rules compose-go enforces
  and we don't model: `healthcheck.test` must begin `CMD`/`CMD-SHELL`/
  `NONE`, mounts may not name an undeclared volume, and legacy `mem_limit`
  /`cpus` may not contradict `deploy.resources`.

Two `parser_test.go` cases (`TestDeferredConsistencyCheck…`) exist purely
to fail if someone removes the second load. The extra parse costs nothing
that matters — it happens only on files that already passed validation.

**`source == ""` means the mount is anonymous, and is always an error.**
compose-go yields it for a genuine anonymous volume *and* for `d:/mnt`,
where a single-character name is indistinguishable from a Windows drive
letter, so the loader keeps `d:/mnt` as the target and drops the source.
Untreated, the service is pinned out of blue/green by state its author
never declared, while the real volume is reported "declared but not
mounted" — an error pointing at the wrong line. `driveLetterMount`
distinguishes the two so each gets an accurate message, and
`misparsedVolumeNames` suppresses the misleading second error.

**The agent polls; it is NOT woken by NOTIFY (Slice A).** The
`service_instances_notify` trigger fires on insert/update and emits
`pg_notify('node_' || node_id-without-dashes, '')`, but nothing consumes it
yet: the agent can't `LISTEN` without importing pgx and breaking the
across-binaries boundary. It polls `GET /desired-state` on a ticker
(`COMPOSECTL_POLL_SECONDS`, default 2). The trigger stays for a future
control-plane push endpoint (Sprint 5).

**Reconcile converges to the instance rows; teardown is row deletion.**
Desired state = the `service_instances` rows for a node (only for
deployments in `scheduling|starting|healthy|live` — `DesiredStateForNode`
excludes terminal ones). The agent removes any container labelled for the
env with no backing desired row. So tearing down a superseded revision is
just deleting its rows (`DeleteInstances`) — the agent GCs the orphaned
**swappable** containers next tick. The pinned container survives because
the now-live deployment still holds its own row for it (adoption by the
stable name `cc-{env8}-pinned-{service}`). Pinned containers are never GC'd
here.

**Teardown of durable state requires an explicit tombstone; an absent row is
never enough.** The reconcile rule above (superseded revision → row deleted →
swappable containers GC'd) never touches a pinned container or a named
volume — GC only ever removes swappable orphans. Destroying the pinned
container and its volumes happens exactly once, in `RemoveEnv`, and only when
the control plane hands the agent that env8 in `teardown_envs`. This is
deliberate: an empty desired-state can be produced by a control-plane outage
(the agent just sees nothing to do) and must never be read as "drop the
database" — only a tombstone, written durably before the environment row is
deleted, means that. `TombstoneRetention` is 24h; past it an offline node
never sees the tombstone again, and its containers and volumes leak until
someone removes them by hand.

**Naming is fixed and load-bearing** (the agent and router both compute
these, so don't drift them): swappable container
`cc-{env8}-r{rev}-{slot}-{service}`, pinned container
`cc-{env8}-pinned-{service}`, revision network `cc-{env8}-r{rev}-{slot}`,
named volume `cc-{env8}-{volume}`. Labels: `cc.env`, `cc.deployment`,
`cc.service`, `cc.swappable`. `env8` = `store.shortID(environmentID)`.

**Node labels are advertisement, and `ingress=true` no longer constrains
placement.** The agent sends `COMPOSECTL_NODE_LABELS` at registration; malformed
entries are dropped rather than aborting startup, because a node that refuses to
start takes its capacity with it while a missing label is a placement the
scheduler can explain. Slice B *did* filter on `ingress=true`, when the router
could only reach a tenant by joining its revision network on a shared daemon —
an ingress stack anywhere else went healthy, went live, and served nothing.
Slice C's address-and-published-port routing removed the need, and the filter
with it: any node can host an ingress stack so long as *some* node runs a
router. `TestIngressStackMayBePlacedOnANodeWithoutARouter` pins the removal, so
a reintroduced filter fails loudly rather than quietly stranding capacity. The
label survives as the record of where the router actually runs.

**Node capacity is reserved from declared limits, not measured usage**, and a
live environment holds its reservation until its instances are deleted — nothing
but a preview's TTL expires one on its own. A dev box therefore fills up over a
session of demo runs, and the next rollout fails with "no ready node has …
free", which reads like a bug and is not one. The dev nodes advertise 16 GB
(`NAVARCH_NODE_MEMORY_MB`) to push that wall out; `make nuke` is the real reset.
Every ingress stack lands on node 1, so it fills first.

**An environment is bound to one node for its lifetime.**
`environments.home_node_id` is set by the first placement, in the same
transaction, and never changes. Every later deployment for that environment goes
there or fails — `PlaceDeployment` refuses a contradicting node with
`ErrConflict`, and the scheduler fails the deployment rather than looking
elsewhere. **Falling back to another node when the home node is full is the
data-loss bug wearing a helpful face:** the pinned container and named volumes
cannot follow, so the agent would build a fresh pinned container over an *empty*
volume, pass its health check, and be auto-promoted while the real data sat
unreferenced on the original node — a rollout that reports success and loses the
database. The check is enforced in the store, not only in the scheduler, for the
same reason the deployment state machine is enforced in SQL: a buggy or racing
scheduler must not be able to write a placement that contradicts durable state.
`TestPlacementRefusesToMoveAHomedEnvironment` is the regression, verified to fail
without the refusal.

The column is `ON DELETE SET NULL` deliberately. RESTRICT is semantically purer,
but deleting a node destroys the volumes it held, so unbinding is the honest
outcome and re-homing the only recovery — and RESTRICT would wedge organization
deletion, since environments cascade from stacks while nodes cascade from the
org (the cascade-ordering hazard already recorded above).

Placement for an *unbound* environment is scored, not first-fit: capacity is a
hard filter, then fewest environments homed, then greatest free-capacity ratio
(of the more constrained of cpu/memory), then node id ascending. The id
tie-break exists so the same fleet state always yields the same choice — a
scheduler whose output depends on Postgres row order cannot be asserted on, and
its bugs reproduce only sometimes. `bestNode` is pure and table-tested.

**No shared network in either direction.** A tenant joins no network but its own
revision's, and nothing joins a tenant's network on the platform's behalf.
Traefik reaches a revision's ingress container at its node's registered address
and the port the agent published for it, so ingress works identically whether the
tenant is on the router's node or another one. Two earlier designs are history,
not the design: the Sprint 2 plan in `docs/superpowers/` specifies a shared
external `cc-ingress` network every ingress container joins, and Slice B attached
the *router* to each revision network instead. Don't reintroduce either — a
shared mesh puts two tenants' ingress containers on one network with working DNS
between them, and the router-side attach made every superseded revision network
unprunable. `isIngressRouter` and `RemoveEnv`'s unconditional disconnect survive
that removal on purpose: endpoints created before it still exist on any upgraded
daemon, and they are what let those networks converge instead of leaking.

**`PublishPort` is deliberately outside the container fingerprint, and verified
against the container instead.** The fingerprint hashes resolved runtime config
*including secret plaintext*, and a mismatch is a hard error rather than a
rebuild — that error is what stops a rotated secret being claimed by an adopted
container still running the old value, forcing the change through a new revision
and its blue/green rollout. Adding `PublishPort` to it would make every container
created before published-port routing fail adoption permanently, so the agent
would error every tick on a healthy live deployment instead of converging.
`EnsureContainer` compares the adopted container's `HostConfig.PortBindings`
against the spec instead — create-time bindings, because `findByName` adopts
stopped containers too and a stopped one reports no active ports however it was
created. A container that disagrees is **replaced only if swappable**; the parser
forbids those from mounting a writable named volume, which is exactly what makes
one safe to rebuild and a pinned one not. Replacements are reported through
`Ensured{ID, Created, Recreated}` and logged by the agent, because rebuilding a
possibly-live container is disruptive and nothing else would show it happened.
Before this, an upgraded agent adopted its existing ingress containers, they
published nothing, `ingress_port` went NULL and the router silently dropped every
route while the deployments stayed live and healthy.

**Per-node capacity, not fleet capacity, is what a single environment can hit.**
An environment is bound to one node, so its blue/green rollout — which holds
reservations for the old *and* new revision at once — is bounded by that node's
advertised memory, not the fleet's. At 8 GB per node a `hello`-shaped stack
reserves ~1.28 GB and ~2.5 GB mid-rollout, so a handful of live environments
homed on the same node will fail the next rollout there with "home node ... lacks
capacity" while the fleet still looks half empty. That is the capacity signal
working, not a bug, and the fix is `make nuke` — nothing releases a live
environment's reservation. The full demo suite passes from a clean fleet and
will not survive being run several times over without one.

**`make up` passes `--remove-orphans`, and that is load-bearing.** Renaming a
service leaves the old container running, and a stale agent keeps registering:
`RegisterNode` upserts by `(org_id, hostname)`, so an orphan and its replacement
fight over one node row, alternately publishing their own advertise address, and
routes point at whichever won last. The four-node rename produced exactly that —
an orphan `agent` driving the *host* daemon while `agent-1` drove `dind-1`, both
claiming `dev-node-1`. It is invisible in `navarch node list`, which shows one
healthy node either way.

**Draining is reversible; the state a node returns to is derived, not declared.**
`DrainNode` sets `draining`, and both `Heartbeat` and `RegisterNode` preserve it
— deliberately, so an agent restart cannot silently un-cordon a node an operator
cordoned. That made drain a one-way door until `UncordonNode` existed: nothing
set a node back, and recovery meant hand-written SQL. Uncordon lifts operator
*intent* only; it asserts nothing about liveness, so the node lands in `ready` or
`unreachable` according to `last_heartbeat` against the same 30-second window
`MarkStaleNodesUnreachable` and `ListReadyNodes` use. Both answers self-correct —
a live agent's next heartbeat promotes `unreachable` to `ready`. The API and CLI
report the resulting state rather than a fixed `"ready"`, so they cannot
contradict the next `node list`. `retired` is refused, not lifted: `Heartbeat`
skips retired nodes, so resurrecting one leaves a row claiming readiness that
nothing can update. **Nothing sets `retired` yet, on purpose** — it would be a
second irreversible operation, and it needs Slice D to decide what happens to
environments homed on a retired node and the agent to stop reconciling rather
than error-loop on a refused heartbeat.

**A test must not leave a node in the `dev` org.** Placement scores by spread, so
a freshly registered fixture node — zero environments homed on it — is the *most*
attractive node in the fleet, and nothing drives it: the next demo's deployment
is placed there and sits in `scheduling` until the heartbeat window closes. A
passing system that looks broken. `internal/api`'s `testServer` snapshots the
dev org's nodes and deletes any the test added, scoped by difference rather than
by name so it also covers registrations made through the HTTP handler (where the
test never holds the id). This was flakiness before Slice A's scoring existed —
see the comment on `TestSetSecretWithNoReadyNodeIsUnprocessable`, which dodges it
by using its own org — and a hazard for anything sharing the database after.

**Log content never reaches Postgres, and that is the design.** A `log_requests`
row is an instruction — which container, what bounds, follow or not — and never
an answer. Container stdout routinely carries secrets (an app logging its own
`DATABASE_URL`, a debug dump of the environment, a stack trace with a token), so
persisting it would put plaintext at rest, in every backup and readable by anyone
with database access, undoing what age sealing and agent-side decryption buy.
Chunks live in `internal/logbuf`: bounded per request and in total, freed when the
requester reads them, when the tail closes, or when the reaper sweeps the row —
buffers first, then rows, because a deleted row with live content behind it is
the one outcome the design promises to avoid. Content still transits
control-plane *memory*, which is unavoidable while the agent has no inbound
server, and must never reach a log line. `make demo-logs` asserts the negative:
no column of `log_requests` may hold content.

**The control plane resolves service→container; the agent is told what to read.**
An agent that could be asked for an arbitrary container id would be one
compromised control plane away from reading every tenant's output on that node.
Bounds live in the store, not the handler: the agent acts on whatever the row
says and cannot know a number is unreasonable. `Tail` counts lines and a line has
no length limit, so `dockerd` caps bytes too and says so in the output — a
silently short answer is indistinguishable from a container that stopped logging.

**Two routes end in `/logs` and authorize differently.**
`POST /v1/nodes/{id}/logs` is a node delivering and takes that node's token;
`POST /v1/envs/{env}/logs` and `GET`/`DELETE /v1/logs/{id}` are operator-facing.
`nodeAgentPathID` matches on the `/v1/nodes/` prefix for exactly this reason — a
node token that could open tails would read any environment, and an operator
token demanded on the delivery path would stop every agent answering.

**The TUI observes; it never acts.** `internal/tui` is read-only by design —
every destructive operation stays in the CLI, where it is explicit, scriptable
and reviewable. `TestNoKeyPerformsAnAction` pins it, so adding "just one" action
means deleting a test that explains why not. It is a second consumer of
`internal/client`, never a second protocol: nothing in the package knows a URL or
a JSON shape, and `navarch tui` builds the client and passes it in so URL/token
precedence keeps one implementation. The charmbracelet tree links into `navarch`
only — `go list -deps ./cmd/controlplane` and `./cmd/agent` must show none of it,
the same guard shape as the Docker SDK.

**A failed refresh keeps the previous timestamp.** The data on screen really is
that old; advancing it would make "the control plane stopped answering"
indistinguishable from "nothing is happening", which is precisely when the
distinction matters. Panes also do not cost the same and the poll cadence
reflects it: fleet, events and health are one request each, while the environment
catalog is a walk of apps → stacks → envs (15 requests against the dev fleet,
growing with the catalog) because **no endpoint lists an organization's
environments** — that walk runs on a slow tier and only while its pane is in
front. Adding `GET /v1/orgs/{org}/environments` would collapse it to one request.

**An empty router config is a file with no `http` section — never an empty
one.** Traefik's parser refuses an element with no children, so `routers: {}`
fails the *whole* file with `routers cannot be a standalone element`, and
`http: {}` (what `omitempty` produces) fails identically. Critically, a
rejected file is not read as "no routes": the file provider keeps the last
config it accepted, so the withdrawal is silently ignored and a torn-down
environment's hostname goes on routing — the stale-route case the preview
hostname scheme exists to prevent. `Sync` therefore writes only its header
comment when `len(routes) == 0`. Verified against `traefik:v3.3` with a live
backend: the empty-map file and `http: {}` both leave the route serving 200;
only the section-less file drops it to 404, and it logs nothing doing so.
This also produced ~320 parse errors on a dev stack before its first
deployment. Note what the old `TestSyncEmptyIsValid` asserted — that `Sync`
returned nil and left a file behind — and why that let the bug through:
"valid" has to mean *Traefik accepts it*, which no unit test can check.

---

## Sprint status

**Sprint 1 — control plane foundation. DONE.** Schema, parser, constraint
validation, catalog + deployment endpoints, dev stack. Parser (16 cases)
and store (now ~27 cases) tested against real Postgres.

**Sprint 2 — node agent + rollouts. DONE.**

The design and per-slice plan live in `docs/superpowers/`:
`specs/2026-07-23-sprint2-agent-rollouts-design.md` and
`plans/2026-07-23-sprint2-slice-a-reconciliation-spine.md`.

- **Slice A** — the reconciliation spine. The agent drives a real Docker
  daemon; the control-plane scheduler places pending deployments and writes
  desired instances; the controller aggregates instance health and drives
  `scheduling → starting → healthy`, failing the deployment (blue untouched)
  if an instance fails. `make demo` is agent-driven end to end — **no SQL
  fakery** — and shows blue/green coexistence with one shared pinned db.
  `make demo-failure` shows a bad image → `failed`.
- **Slice B** — Traefik as a real compose service, `internal/router`
  generating its file-provider config, and the controller *auto-promoting*
  on healthy (`PromoteDeployment` → router resync → old swappable torn
  down). `make demo`'s traffic-flip step exercises this: revision 2 goes
  live and Traefik moves traffic to it with zero downtime, no manual
  `POST /promote` involved.
- **Slice C** — `POST /v1/envs/{env}/rollback` re-deploys an older stack
  version as a new revision through the same spine (append-only: rollback is
  a new row, never a mutation of the old one). `make demo-rollback` exercises
  it.

**Sprint 3 — secrets + preview environments. DONE.**

- **Slice A (secrets)** — `POST /v1/envs/{env}/secrets` stores age
  ciphertext, sealed to every ready node's recipient at write time; the
  control plane never holds a decryption key. The agent decrypts
  per-environment at container start using an identity that persists across
  restarts (`COMPOSECTL_AGE_IDENTITY_FILE`, on the `age-identity` volume) and
  injects into `${secret:KEY}` references. A deployment whose resolved spec
  needs a secret the target environment never set is rejected with 422
  before it reaches a node. This retired the Sprint 2 dev stand-in
  (`COMPOSECTL_DEV_SECRETS`, a static `k=v` map baked into the agent's
  environment) entirely. `make demo-secrets` exercises ciphertext-at-rest,
  decrypt+inject, and the 422 fail-fast.
- **Slice B (previews)** — `POST /v1/stacks/{stack}/previews` creates an
  ephemeral environment, its first deployment, and (optionally) a copy of
  another environment's secret ciphertext, all in one transaction. The
  reaper loop expires previews past `expires_at`, writes a tombstone before
  deleting the environment row, and the agent acts on that tombstone to
  destroy the pinned container and named volumes an ordinary reconcile tick
  would never touch. Hostnames are generated as
  `{slug}-{stack}-{env8}.{COMPOSECTL_PREVIEW_DOMAIN}` and TTL is the only
  lifecycle control — see **Preview environments** above. `make demo-preview`
  exercises the full lifecycle: inherited secret served through Traefik, then
  complete teardown.

**Placement/agent model:** the scheduler owns placement (writes desired
`service_instances`); the agent is a dumb reconciler. Nodes are org-scoped,
so the agent registers into a stable **`dev` org** (bootstrapped at
control-plane startup via `BootstrapDevOrg`); `make demo` deploys into it.
Multi-org node pools are Sprint 4.

**Store methods** now also include, beyond the Sprint 1 catalog set:
`RegisterNode`, `Heartbeat`, `ListNodes`, `ListReadyNodes`,
`GetOrganizationBySlug`, `CreateServiceInstances`, `DesiredStateForNode`,
`ReportInstance`, `InstanceStates`, `DeleteInstances`,
`ListPendingDeployments`, `ListRolloutsInState`, `CreatePreview`,
`ExpireEnvironments`, `TombstonesForNode`, `SweepTombstones`, `GetStack`,
`GetEnvironmentBySlug`. All node/deployment/preview endpoints are wired.
Handler files in `internal/api`: `catalog.go`, `deployments.go`,
`validate.go`, `nodes.go`, `previews.go`.

`POST /v1/orgs` is self-serve (orgs are the root; a seeded migration UUID
would be permanent). `POST /v1/stacks/{stack}/versions` takes the compose
file as the **raw body** (`curl --data-binary @compose.yaml`), authorship
via `?created_by=`.

**Sprint 4 — multi-node fleet. IN PROGRESS** (branch `sprint4-multi-node`).
Design: `docs/superpowers/specs/2026-08-15-sprint4-multi-node-design.md`.
Settled: a deployment is placed **whole onto one node** (no cross-node overlay,
so the agent's container model is untouched); **one ingress node** reaches
tenants over the mesh; the dev fleet is **Docker-in-Docker** so nodes are real
separate daemons.

- **Slice A — environment affinity + scored placement. DONE.** `home_node_id`,
  refusal to relocate, spread-first scoring, secret recipients keyed to the home
  node. It landed first because the affinity bug was live in the code and needed
  only a second node to become data loss.
- **Slice B — the dev fleet. DONE.** `dind-b` + `agent-b` give node 2 its own
  Docker daemon (`DOCKER_HOST`, no socket mount, its own age identity), nodes
  advertise capabilities via `COMPOSECTL_NODE_LABELS`, and the scheduler places
  against them. `make demo-fleet` proves an ingress stack lands on the router
  node while a no-ingress stack spreads to the other *and runs on that node's
  daemon*. Node 1 stays on the host daemon this slice: moving Traefik into DinD
  means solving cross-node ingress, which is Slice C's subject, and would leave
  `make demo` broken in between.
- **Slice C — cross-node ingress. DONE.** The router targets a node address and
  a published port instead of a container name, so ingress no longer requires
  the router to share a daemon with the tenant. Slice B's `ingress=true`
  placement filter was scaffolding for exactly this and is gone;
  `AttachRouterToNetwork` went with it. `make demo-fleet` asserts a stack on the
  router-less node is served through the node that has one.
- **Slice D** — drain and failover. Planned, not built:
  `docs/superpowers/plans/2026-08-16-sprint4-slice-d-drain-and-failover.md`.
  The drain one-way door is already fixed (see the uncordon invariant above);
  what remains is that `ListLiveRoutes` filters only on `d.state='live'` and
  joins `nodes` purely for the address, so a drained or unreachable node keeps
  serving routes — users get hangs rather than errors.

**Sprint 5** — Bubble Tea TUI, log aggregation, metrics

**Post-review hardening. DONE.** Every API route except `/healthz` requires
the shared `COMPOSECTL_AGENT_TOKEN`; both binaries fail closed when it is
absent. Instance reports are scoped to the node in the request. Unsupported
rollout strategies are rejected (only blue/green is implemented). Agent
heartbeats report unique-container CPU/memory allocation and placement checks
both resources. Existing pinned services cannot change or disappear across a
deployment, and Docker adoption verifies a resolved-runtime fingerprint so a
secret rotation cannot be silently claimed without recreating the container.

**Audit + operational metrics. DONE.** Deployment lifecycle changes, preview
expiry, and secret key/version mutations append events transactionally. Events
survive deployment deletion (`0004_audit_events` changes the FK to `SET NULL`),
and `GET /v1/orgs/{org}/events` provides newest-first cursor pagination.
`GET /metrics` is authenticated Prometheus text format: bounded HTTP route
labels, loop results/durations, DB availability, deployment states, ready
nodes, active previews, and recent tombstones. Never add secret values, raw
request paths, compose bodies, or environment values to events or metric labels.

---

## Conventions

- Go 1.25 (bumped from 1.23 in Slice A — see loose ends). Method-and-wildcard
  routes (`POST /v1/x/{id}`) — no router dep.
- `log/slog` for logging. Structured, no `fmt.Println`.
- Errors wrap with `%w`. Store exposes `ErrNotFound` / `ErrConflict` /
  `ErrInvalid`; `writeStoreError` maps them to 404 / 409 / 400 so handlers
  don't repeat it. A Postgres **foreign key violation maps to `ErrNotFound`**
  — creating a child under a parent that doesn't exist is a client mistake
  (404), not a server fault (500). Revisit if a delete path ever leans on
  RESTRICT to refuse removing a referenced row; that wants 409.
- Slug format is validated in the **store**, not the handlers, because
  business rules belong behind that boundary: lowercase alphanumeric with
  internal dashes, ≤63 chars. Slugs reach URLs and ingress hostnames.
- Handlers read UUID path params through `pathUUID`, which writes the 400
  itself and reports whether to continue.
- Every context gets a timeout.
- Migrations are immutable once applied — new file, never edit `0001`.
- Comments explain **why**, not what. The existing comments carry real
  design rationale; match that register.

## Three bugs the rollout-mode work uncovered

All three sat on the agent↔control-plane path, stacked so that each hid the
next. Fixed, each with a regression test verified to fail without its fix.

- **Every per-node endpoint returned 401.** `Server.ServeHTTP` authorizes
  *before* handing off to the mux, and `r.PathValue("id")` is only populated
  once the mux has matched a route — so it was empty, `uuid.Parse` failed, and
  heartbeat / desired-state / report were refused unconditionally. The agent
  could register (operator token) and do nothing else. `nodeAgentPathID` now
  parses the id out of the path. The `internal/api` tests missed it because
  they build a `Server` with no bearer token, which skips authorization
  entirely — `TestNodeTokenAuthorizesAgentEndpoints` configures one.
- **`Heartbeat` 500'd on an enum cast.** Its `CASE` is built from unknown-type
  literals, so Postgres resolved it as `text` and refused to assign that to
  the `node_state` column. Needs `(CASE … END)::node_state`. Same family as
  the deployment-state enum gotchas below. `Heartbeat` had *no* test at all;
  it has two now. Nodes never left `unreachable`, so `ListReadyNodes` stayed
  empty and secret writes failed with "no ready node with an encryption key".
- **Traefik never started.** `--api.disabled=true` is not a Traefik flag; v3
  aborts with `field not found, node: disabled`. From the demo's side a router
  that never booted is indistinguishable from a routing bug. The API is off by
  default, so the flag is simply gone.

The demo passes end to end again — rollout, auto-promote, zero-downtime flip.
Worth noting `make demo` is the only check that exercises this path: the
unit tests ran green against all three bugs.

## Known loose ends

- **Toolchain is go 1.25** (go.mod directive + `golang:1.25-alpine` builder,
  kept in step). Slice A bumped it from 1.23 because the Docker Engine SDK
  pulls an OpenTelemetry/grpc stack that requires 1.25 — a real runtime
  dependency, bumped deliberately. This supersedes the old "keep 1.23 / pin
  go-internal" rule; `go mod tidy` raising the directive is no longer the
  hazard it was, though the Dockerfile builder and go.mod must still move
  together. `go mod tidy` overshot the OTel stack to the latest (1.25-needing)
  versions rather than the older ones Docker minimally requires — fine, since
  we're at 1.25 anyway.
- `internal/api` still lacks direct coverage for much of the catalog and
  deployment surface. Overlay precedence has regression coverage through API
  tests, while `internal/spec` and both config loaders now have focused tests.
- The example stack's comment claims `cache` is "swappable (tmpfs only)",
  but `cache` declares no mount at all. Harmless — it is swappable either
  way — but the comment describes a case the example never exercises.
- `examples/webapp/compose.yaml` uses placeholder images that don't pull;
  it's for parsing/classification only. The **runnable** demo stack is
  `examples/hello/compose.yaml` (real images), which `make demo` uses.
  `examples/secret/compose.yaml` (`make demo-secrets`) and
  `examples/preview/compose.yaml` (`make demo-preview`) are runnable too,
  each shaped for what its demo has to assert: `secret` echoes an injected
  value into its HTTP response (`hello`'s services don't echo anything, so
  it can't prove secret content that way); `preview` pairs that same
  echoing service with a pinned db, so an expiring preview has a pinned
  container and a named volume actually worth destroying.
- Revision-network cleanup is agent-owned. Reconcile keeps the current desired
  project-network set, disconnects same-environment managed containers **and
  the platform's own ingress router** from obsolete labelled networks, and
  removes those networks. The router exemption is load-bearing: it holds an
  endpoint on every revision network it ever served, carries no `cc.env` label,
  and without it every superseded revision network survived for the life of the
  environment — one leaked network per revision until Docker's address pool ran
  out and rollouts could no longer create one. Disconnecting it is safe only
  because the network is not in `wanted`: no live revision uses it, and served
  routes resolve to containers on the live revision's network. Anything *else*
  unmanaged still blocks the prune, which is the line between this path and
  `RemoveEnv`. Reconcile remembers envs seen on the prior tick so a failed first
  rollout can still be cleaned after its desired rows disappear; cleanup
  failures retain that memory and retry, and now report their reason
  (`EnvFailure{Env8, Op, Err}`) instead of collapsing into a boolean.
  Only `cc.env`-labelled networks are ever eligible, which is what keeps the
  sweep off anything the platform did not create.
- Loop integration tests are organization-scoped so parallel `go test`
  package binaries sharing one Postgres database cannot schedule, advance, or
  reap one another's fixtures. Production constructors retain the global scan;
  unexported rollout test constructors select the scoped store methods.

## Verification

`go build ./...` before claiming anything compiles. Tested packages:
`internal/parser` (16, pure), `internal/store` (~27), `internal/rollout`
(scheduler + controller + reaper), `internal/api` (node, secret and preview
handlers), `internal/agent` (reconcile logic, fake driver — pure),
`internal/agent/dockerd` (against a real daemon), `internal/cli` (9, pure —
argv, output, config precedence), `internal/client` (6, pure — `httptest`),
`internal/router`, `internal/secrets`, `internal/metrics`, `internal/spec` and
`internal/config`. The catalog/deployment half of `internal/api` still has none.

A `router` unit test can only check the *bytes* written — whether Traefik
accepts them is outside Go's reach, which is exactly how the empty-config bug
survived a passing `TestSyncEmptyIsValid`. Config-shape changes want a real
Traefik in the loop (`make demo` exercises the populated path; a stack with no
deployments exercises the empty one — its log must stay clean).

**Two live dependencies, both skip loudly when absent** — no fallbacks, by
design. `store`/`rollout`/`api` tests need Postgres (dev stack on `5473`, or
`COMPOSECTL_TEST_DATABASE_URL`); `dockerd` tests need a reachable Docker
daemon. A green run with either down proves nothing — **check for
`--- SKIP`** before trusting it. Test fixtures create a unique-slug org and
delete it (and children, bottom-up) on cleanup.

**Tests must run with the dev-stack control plane stopped**
(`docker compose stop controlplane agent`; leave Postgres up). Its
scheduler/controller/reaper loops mutate the same database the tests use,
and the reaper now DELETEs environments — a running control plane corrupts
test fixtures mid-run, and `go test ./...` fails on unrelated tests as a
result. Restart the control plane (`docker compose start controlplane
agent`, or `make up`) before running demos again.

**Postgres interval casts:** `make_interval(secs => $n)` with
`Duration.Seconds()` is the house pattern for every `time.Duration` → interval
conversion. `Duration.String()` cast to `::interval` renders a sub-second value
as e.g. `"1ns"`, which Postgres's interval parser rejects outright — and a
store method whose duration is hour-scale today is one test away from being
handed a millisecond. Don't reintroduce the `.String()` form.

**Boundary guard:** `go list -deps ./cmd/controlplane | grep docker/docker`
must return nothing — the control-plane binary must not link the Docker SDK.

`make demo` is the real end-to-end check now: it brings a stack up through
the agent and asserts it reaches `healthy` unaided. `make demo-preview`
covers the other half of the deployment lifecycle: a preview environment
served through Traefik with an inherited secret, then expired and reaped —
containers, pinned container, and named volumes all gone. `make validate`
exercises the parse path (fastest check that parser changes didn't break
classification). All three need `make up` first.

Worth testing against: dependency cycles, bind mounts, published ports,
`privileged`, pinned ingress services, read-only volumes (must stay
swappable), and secrets embedded mid-string.

**Known-good baseline** for `examples/webapp` — a parser change that moves
either number changed classification, so justify it or revert:

```
digest            98d75411a605267292ea77e565309d6c7f60766b8c75c3ecc1e0404903b6e138
swappable         api, worker
pinned            cache, db
ingress           api
peak_memory_bytes 2281701376   # (512+256 MiB)*2 + 512 + 128 MiB
```

This baseline moved once, deliberately, when rollout mode became a declared
field. `cache` was swappable and is now pinned: it is a Redis that api and
worker both address as `redis://cache:6379`, and duplicating it gave each
revision its own keyspace and its own copy of any lock taken in it. The
previous baseline was `6072c68f…` / swappable `api, cache, worker` / pinned
`db` / `2415919104`. Anything that moves it *again* needs its own
justification.

The digest is stable across repeated calls; if it varies run-to-run on an
unchanged file, a new unsorted slice field has entered `DeploymentSpec`.
