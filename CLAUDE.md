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

## Quickstart

```bash
make tidy         # only after changing deps — go.sum is committed
make up           # postgres + migrations + control plane + node agent
make health
make validate     # parse examples/webapp, see classification
make demo         # agent-driven rollout to healthy, over HTTP, real containers
make demo-failure # bad image → failed, prior deployment untouched
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
internal/spec         normalized DeploymentSpec — the platform's vocabulary
internal/parser       the ONLY package importing compose-go
internal/store        the ONLY package importing pgx
internal/api          thin handlers: decode, delegate, encode
internal/rollout      scheduler + rollout controller (control-plane loops)
internal/agent        node reconciliation loop (imports neither pgx nor compose-go)
internal/agent/dockerd the ONLY package importing the Docker SDK
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

Blue/green runs two copies of a stack simultaneously. Services holding
durable state **cannot** be duplicated — two Postgres processes on one
volume corrupt it. So the parser classifies every service:

| Classification | Trigger | Rollout behavior |
|---|---|---|
| **swappable** | no writable named volume | duplicated blue/green |
| **pinned** | mounts a writable named volume | runs once, shared across revisions |

Read-only volume mounts stay swappable. This single bit drives rollout
behavior, node pinning, and `PeakMemoryBytes()` (swappable counted twice,
pinned once).

Anything touching rollout, scheduling, or capacity must respect it.

---

## Rejected compose directives

Rejected **loudly**, never silently dropped — a stack must never run
differently than its author expects. All violations collect in one pass
so the user fixes everything at once.

| Directive | Reason |
|---|---|
| `build:` | pre-build and push; platform does not build |
| `privileged`, `cap_add` | breaks isolation |
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
  would never touch. `make demo-preview` exercises the full lifecycle:
  inherited secret served through Traefik, then complete teardown.

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

**Sprint 4** — multi-node, WireGuard mesh, placement scoring
**Sprint 5** — Bubble Tea TUI, log aggregation, metrics

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
- `internal/api`, `internal/spec`, `internal/config` still have **no tests**.
  The overlay-precedence rules (`internal/api/overlay.go`) and digest
  stability (`internal/spec`) are the untested spots with a history of real
  bugs — worth covering.
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

## Verification

`go build ./...` before claiming anything compiles. Tested packages:
`internal/parser` (16, pure), `internal/store` (~27), `internal/rollout`
(scheduler + controller), `internal/api` (node handler), `internal/agent`
(reconcile logic, fake driver — pure), `internal/agent/dockerd` (against a
real daemon). `internal/spec`, `internal/config`, and most of
`internal/api` still have none.

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

**Postgres interval casts:** `time.Duration.String()` renders a sub-second
value as e.g. `"1ns"`, which Postgres's interval parser rejects outright.
Use `make_interval(secs => $n)` with `Duration.Seconds()` for any duration
that could be sub-second. `internal/store/previews.go` uses both forms:
`CreatePreview`'s TTL is hour-scale and safe as a `.String()` cast, but
`TombstonesForNode`/`SweepTombstones` take a retention window a test can
shrink to milliseconds, so they go through `make_interval`.

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
digest            6072c68f9a252be6aec77e816a6b4c43ef96244bdb8583c1854d657a32695730
swappable         api, cache, worker
pinned            db
ingress           api
peak_memory_bytes 2415919104   # (512+256+128 MiB)*2 + 512 MiB
```

The digest is stable across repeated calls; if it varies run-to-run on an
unchanged file, a new unsorted slice field has entered `DeploymentSpec`.
