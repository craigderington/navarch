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
make tidy        # only after changing deps — go.sum is committed
make up          # postgres + migrations + control plane
make health
make validate    # parse examples/webapp, see classification
make demo        # full loop: catalog -> version -> deploy -> promote
make psql        # database shell
make logs        # tail control plane
make nuke        # down + delete volumes
```

---

## Architecture: the boundaries are load-bearing

```
cmd/controlplane   API server entrypoint
cmd/agent          (empty — Sprint 2)
internal/spec      normalized DeploymentSpec — the platform's vocabulary
internal/parser    the ONLY package importing compose-go
internal/store     the ONLY package importing pgx
internal/api       thin handlers: decode, delegate, encode
internal/config    env-var config
migrations/        golang-migrate SQL
```

**Do not violate these boundaries.** They're the main design decision in
the codebase:

- Only `internal/parser` imports compose-go. Everything downstream speaks
  `spec.DeploymentSpec`. This means the compose implementation can be
  swapped, or another input format accepted, without touching the
  scheduler or agent.
- Only `internal/store` imports pgx. Handlers never build SQL.
- Handlers decode, delegate, encode. Business logic belongs in store or
  parser, not in `internal/api`.

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

**Agents are woken by `LISTEN`/`NOTIFY`, not polling.** The
`service_instances_notify` trigger fires on insert/update and emits
`pg_notify('node_' || node_id-without-dashes, '')`. The Sprint 2 agent must
subscribe to exactly that channel name.

---

## Sprint status

**Sprint 1 (current) — control plane foundation**

- [x] Postgres schema
- [x] compose-go parser → `DeploymentSpec`
- [x] Constraint validation
- [x] API skeleton + `/v1/validate`
- [x] Deployment create/get/list/promote
- [x] Dev stack via Compose
- [x] `go mod tidy` — `go.sum` committed; `go build ./...` and `go vet ./...` clean
- [x] `-healthcheck` probe so the container healthcheck can pass
- [x] Parser unit tests — 16 cases in `internal/parser/parser_test.go`
- [x] Catalog handlers — org → app → stack → version → env, the chain that
      makes the deployment endpoints reachable over HTTP
- [x] Store tests — 20 cases in `internal/store`, against real Postgres
- [ ] Tests for `internal/api` overlay precedence and `internal/spec`
      digest stability — still zero coverage in those packages, and the
      overlay rules are the ones with a history of real bugs.
- [ ] Node endpoints + rollback (Sprint 2; no store methods behind them yet)

`make demo` walks the whole loop end to end — catalog, stack version,
deployment, promotion — and asserts on the way through. It is the fastest
proof the control plane still works after a change.

Store methods (`internal/store`):

`CreateOrganization`, `ListOrganizations`, `CreateApplication`,
`ListApplications`, `CreateStack`, `CreateEnvironment`, `ListEnvironments`,
`GetEnvironment`, `CreateStackVersion`, `GetStackVersion`,
`LatestStackVersion`, `ListStackVersions`, `CreateDeployment`,
`GetDeployment`, `ListDeployments`, `UpdateDeploymentState`,
`PromoteDeployment`

`UpdateDeploymentState` still has no HTTP caller — it is the seam the
Sprint 2 agent will drive. `make demo` fakes those transitions with SQL,
which is the one part of the loop that is not yet a real API call.

Route table (all registered in `internal/api/server.go:routes`):

| Route | Status |
|---|---|
| `GET /healthz` | implemented |
| `POST /v1/validate` | implemented |
| `POST\|GET /v1/orgs` | implemented |
| `POST\|GET /v1/orgs/{org}/apps` | implemented |
| `POST /v1/apps/{app}/stacks` | implemented |
| `POST\|GET /v1/stacks/{stack}/versions` | implemented |
| `POST\|GET /v1/stacks/{stack}/envs` | implemented |
| `POST\|GET /v1/envs/{env}/deployments` | implemented |
| `GET /v1/deployments/{id}` | implemented |
| `POST /v1/deployments/{id}/promote` | implemented |
| `POST /v1/envs/{env}/rollback` | 501 |
| `POST /v1/nodes/register`, `/{id}/heartbeat`, `/{id}/report` | 501 |
| `GET /v1/nodes`, `/v1/nodes/{id}/desired-state` | 501 |

There is no `POST /v1/orgs/{org}` style nesting for organizations because
they are the root: `POST /v1/orgs` is self-serve on purpose. Seeding an org
in a migration was the alternative and was rejected — migrations are
immutable here, so a seeded UUID would be permanent.

`POST /v1/stacks/{stack}/versions` takes the compose file as the **raw
body**, like `/v1/validate`, so `curl --data-binary @compose.yaml` works.
Authorship rides along as `?created_by=`.

Files in `internal/api` now match the route groups: `catalog.go`,
`deployments.go`, `validate.go`, `nodes.go` (the remaining stubs).

**Sprint 2 — node agent + rollouts.** Agent reconciliation loop, health
gating, Traefik dynamic config, traffic flip, rollback. This is where
blue/green stops being a data model and becomes real.

**Sprint 3** — environments, secret injection, preview envs
**Sprint 4** — multi-node, WireGuard mesh, placement scoring
**Sprint 5** — Bubble Tea TUI, log aggregation, metrics

---

## Conventions

- Go 1.23. Method-and-wildcard routes (`POST /v1/x/{id}`) — no router dep.
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

- `store.uuidOrNil` is dead code — no caller. Either wire it into the
  handlers that parse path UUIDs or delete it; right now it duplicates the
  `uuid.Parse` + 400 dance the handlers already do inline.
- `cmd/agent` is referenced in the layout above but does not exist on disk
  yet. Sprint 2 creates it.
- **Do not let `go mod tidy` raise the `go` directive.** The Dockerfile
  builds on `golang:1.23-alpine` with `GOTOOLCHAIN=local`, so a `go.mod`
  requiring anything above 1.23 fails the image build at `go mod download`
  — and only there, never locally. A modern local toolchain will happily
  bump the directive to satisfy a *test-only* transitive dep: specifically
  `github.com/rogpeppe/go-internal`, which reaches the graph via
  yaml.v3 → check.v1 → kr/pretty and whose v1.15.0 demands go 1.25. It is
  pinned to **v1.14.1** (needs exactly 1.23) for that reason. If you tidy
  and `grep '^go ' go.mod` no longer says 1.23, you've broken `make up`;
  either re-pin or bump the builder image deliberately.

- The example stack's comment claims `cache` is "swappable (tmpfs only)",
  but `cache` declares no mount at all. Harmless — it is swappable either
  way — but the comment describes a case the example never exercises.

## Verification

`go build ./...` before claiming anything compiles. `make test` is
meaningful for `internal/parser` (16 cases) and `internal/store` (20), both
under `-race`; `internal/api`, `internal/spec` and `internal/config` still
have none, so a green suite says nothing about them. Run a single case with
`go test ./internal/parser/ -run TestSingleCharacterVolumeName -v`.

**Store tests need a live Postgres** — no SQLite fallback, by design. They
use the dev stack on `5473` by default, or `COMPOSECTL_TEST_DATABASE_URL`.
Without one they **skip**, so a green run with the stack down proves
nothing; check for `--- SKIP` before trusting it. Each test creates its own
org with a unique slug and deletes it (and everything cascading) on
cleanup, so runs don't pollute the dev database.

`make validate` exercises the full parse path against the example stack —
fastest end-to-end check that parser changes didn't break classification.

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
