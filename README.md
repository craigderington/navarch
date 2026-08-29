# Navarch

A container platform for **Docker Compose stacks**, not single containers.
Navarch treats an entire stack as the deployable unit, with immutable versions,
health-gated blue/green rollouts, rollback, encrypted secrets, ephemeral preview
environments, and placement across a multi-node fleet.

## Naming

The product is **Navarch** and the operator CLI is **`navarch`**. Several
internal identifiers still read `composectl`, and that is deliberate rather than
an unfinished rename — each one is either invisible to users or expensive to
change for no user-visible gain:

| Name | Value | Why |
|---|---|---|
| Product, CLI binary | `Navarch`, `navarch` | what a user says and types |
| CLI environment, config | `NAVARCH_*`, `~/.config/navarch/` | user-facing; `COMPOSECTL_*` still read at lower precedence |
| Go module | `github.com/craigderington/navarch` | matches the repository, so `go install` works |
| Control plane / agent env | `COMPOSECTL_*` | set by `compose.yaml`, not by hand |
| Container, network, volume prefix | `cc-`, labels `cc.*` | **not branding** — see below |
| Compose extension key | `x-composectl.rollout`, `x-composectl.ingress` | it is in every user's compose file; renaming it breaks all of them |
| Postgres role and database | `composectl` | a data migration with no user-visible gain |

The `cc-` prefix is the one that looks most like an oversight and is the most
deliberate. Nobody types it; it is a namespace. Renaming it would break three
things at once: the agent adopts pinned containers by the stable name
`cc-{env8}-pinned-{service}`, so a new scheme would start a *second* container
over the same volume; garbage collection only sees `cc.env`-labelled containers,
so every existing one would become invisible and leak; and teardown matches
volumes on that label. It buys nothing and risks data.

## Current status

The platform loop is implemented end to end across a multi-node fleet:

- Compose parsing into a normalized, versioned deployment specification
- Strict rejection of unsupported Compose behavior
- Postgres-backed catalog and append-only deployment history
- Node-agent reconciliation against Docker Engine, one agent per node
- Health-gated blue/green promotion and rollback
- Traefik file-provider routing
- Age-encrypted, per-environment secrets
- Preview environments with inherited secrets, TTL expiry, and durable teardown
- Scored placement across a fleet, with each environment bound to the node
  holding its durable state
- Cross-node ingress: a stack is reachable regardless of which node runs it
- On-demand container logs, fetched through the agent poll and never stored
- A read-only terminal dashboard (`navarch tui`)
- Operator identity with per-organization authorization, and an audit timeline
  that names who did what
- TLS terminated at a reverse proxy, with both binaries refusing to send
  credentials over plaintext they cannot contain
- Versioned, reproducible release binaries and a single-host deployment whose
  upgrade path is tested rather than described

Only `blue_green` is currently supported. Requests for `rolling` or `recreate`
are rejected rather than silently receiving different behavior.

Operator routes require an operator token and are scoped to the organizations
that operator belongs to; a non-member is refused with 404 rather than 403, so
one tenant cannot probe another's ids. Agents authenticate with a per-node
token, and the shared `COMPOSECTL_AGENT_TOKEN` now opens only node registration
and the metrics endpoint — a node has no identity of its own until it has
registered. `navarch whoami` reports who a token belongs to and what it can see.

A node can *advertise* a new age key but not adopt one: a changed recipient is
held pending, nothing is sealed to it, and an operator promotes it with
`navarch node rotate-recipient`. Otherwise anyone able to register a node could
redirect the keys that every later secret is sealed to.

**TLS terminates in front of the control plane, not inside it.** The API speaks
plaintext HTTP and does not know whether it is behind a proxy — correct for a
process on loopback, wrong the moment it is reachable from anywhere else.
`deploy/tls/` is the pattern: Caddy, with ACME in production and a local CA for
`make demo-tls`, which runs the real proxy in front of the real control plane
and checks the certificate actually verifies.

Neither the CLI nor the agent will send a credential over plaintext HTTP to
anywhere the traffic could be read. Loopback, `.localhost`, `.internal` and
container-network names are allowed; private LAN addresses are not, because a
shared network is exactly where a captured token is worth having.
`NAVARCH_INSECURE=1` (or `COMPOSECTL_INSECURE=1` for the agent) overrides, and
warns every time it does.

The compose file binds Postgres, the API, and Traefik to loopback so fleet
containers cannot hairpin to the control plane. Traefik's insecure API is
disabled.

## Install

```bash
go install github.com/craigderington/navarch/cmd/navarch@latest
```

Or take a binary from a release (`linux/amd64`, `linux/arm64`, `darwin/amd64`,
`darwin/arm64`), verify it, and put it on your PATH:

```bash
sha256sum -c SHA256SUMS
tar -xzf navarch_1.0.0_linux_amd64.tar.gz && sudo mv navarch /usr/local/bin/
navarch version
```

Running the control plane itself is a compose file and four environment
variables — see [deploy/README.md](deploy/README.md), which also documents the
upgrade sequence (pull, migrate, restart) and what to back up.

## Quickstart

The dev stack ships two credentials, deliberately distinct.
`dev-operator-token-change-me` is the bootstrap **operator** and is what the
Makefile's `API_TOKEN`, the demo scripts and `navarch` use.
`dev-token-change-me` is the shared **service** token the agents use to
register, and it opens nothing else but `GET /metrics`.

```bash
make up            # Postgres, migrations, control plane, agent, and Traefik
make health        # public health check
make metrics       # authenticated Prometheus-compatible metrics
make validate      # authenticated validation of examples/webapp
make demo          # two revisions, auto-promotion, and live traffic flip
make demo-failure  # bad image fails without disturbing the live revision
make demo-rollback # append-only rollback through the normal rollout path
make demo-secrets  # encrypted storage and agent-side secret injection
make demo-preview  # preview creation, routing, expiry, and full teardown
make demo-tls      # TLS terminated at a real proxy, and refused where absent
make demo-site     # Navarch's own marketing site, deployed on Navarch
make test          # race-enabled Go test suite
```

## CLI

`navarch` is the operator client. It carries an operator token and prints
tables by default (`--output json` for scripts).

```bash
make build                 # writes bin/navarch
navarch login --url http://localhost:8417   # prompts; the dev operator token
                                           # is dev-operator-token-change-me

navarch whoami             # who this token is, and which orgs it can see
navarch token list         # your own credentials
navarch health
navarch validate examples/hello/compose.yaml
navarch org list
navarch stack push dev/shop/main examples/hello/compose.yaml
navarch deploy --env dev/shop/main/staging
navarch events --org dev
```

### Naming things

Anywhere an id is accepted, a slug path rooted at the organization works too.
Depth follows the hierarchy, and segments may mix ids and slugs, so a script
that already captured one id can still name the rest:

| Flag | Path | Example |
|---|---|---|
| `--org` | `ORG` | `dev` |
| `--app` | `ORG/APP` | `dev/shop` |
| `--stack` | `ORG/APP/STACK` | `dev/shop/main` |
| `--env` | `ORG/APP/STACK/ENV` | `dev/shop/main/staging` |
| `node get`, `node drain` | `ORG/HOSTNAME` | `dev/dev-node-1` |

Resolution costs one request per level and happens only for paths — a reference
that is already an id is passed straight through, so existing scripts issue
exactly the calls they always did. Deployments are addressed by id only: they
have revisions, not slugs.

Config file (optional): `~/.config/navarch/config.yaml` with `url` and `token`.
Flags override the environment; the environment overrides the file.

The CLI was previously named `composectl`. `COMPOSECTL_URL`, `COMPOSECTL_TOKEN`,
`COMPOSECTL_TOKEN_FILE`, `COMPOSECTL_AGENT_TOKEN` and `COMPOSECTL_CONFIG`, along
with `~/.config/composectl/config.yaml`, are still read at lower precedence, so
an existing setup keeps working. These variables carry the bearer token, and an
unset token is a hard failure rather than a degraded mode — a rename that failed
closed on someone's shell profile would be a poor trade for a cosmetic gain. See
[Naming](#naming) for what else deliberately still reads `composectl`.

Direct API requests require an operator token:

```bash
curl -H 'Authorization: Bearer dev-operator-token-change-me' \
  http://localhost:8417/v1/orgs
```

The response lists only the organizations that operator belongs to.

`GET /healthz` is deliberately unauthenticated for container and load-balancer
probes.

## Audit timeline and metrics

Deployment creation, state transitions, promotion, supersession, preview
expiry, and secret set/delete metadata are appended to the existing `events`
table in the same transaction as the operation they describe. Audit entries
survive deployment deletion, and each names the operator who caused it; secret
values and request bodies are never recorded. An entry with no actor is one a
control-plane loop wrote — nobody asked, something noticed.

The authenticated organization timeline is cursor-paginated:

```bash
curl -H 'Authorization: Bearer dev-operator-token-change-me' \
  'http://localhost:8417/v1/orgs/<org-uuid>/events?limit=50&before_id=1234'
```

`GET /metrics` is authenticated and emits Prometheus text format. It includes
HTTP requests by method/route-pattern/status, scheduler/controller/reaper tick
results and latest durations, database availability, deployments by state,
ready nodes, active previews, and recent teardown tombstones. Raw request paths,
compose content, environment values, and secrets are not metric labels.

## The core idea: swappable vs pinned

Blue/green runs two revisions at once, and some services must not be duplicated.
**Every service declares which it is — the platform does not infer it:**

```yaml
x-composectl:
  rollout: swap   # duplicated blue/green
  # rollout: pin  # one instance, shared across revisions
```

| Classification | Declared | Rollout behavior |
|---|---|---|
| **swappable** | `rollout: swap` | duplicated during blue/green |
| **pinned** | `rollout: pin` | one container shared across revisions |

**Omitting it is a parse error.** There is no default, deliberately: the author
who does not realise blue/green changes cardinality is exactly the one an
optional field fails to protect.

This used to be inferred — writable named volume meant pinned — and that computed
the wrong property. A writable volume answers *"would two writers corrupt this
filesystem?"*; blue/green needs *"may this run twice?"*. They come apart at the
effect-singleton: a scheduler, cron runner or broker that owns no local state but
whose correctness assumes one instance. The sharpest case is a Redis run with no
persistence: it mounts nothing, so it was duplicated, and since each revision has
its own network, every revision got its own keyspace — two holders of "the" lock,
both succeeding.

The volume rule survives as a constraint rather than a definition: `rollout: swap`
on a service mounting a *writable* named volume is rejected. Read-only mounts are
exempt. Peak rollout capacity counts swappable services twice and pinned once,
for both CPU and memory.

Pinned containers are intentionally not recreated automatically. A deployment
that changes or removes an existing pinned service is rejected. The agent also
fingerprints resolved runtime configuration—including secret values—and refuses
to adopt a stale pinned container when that fingerprint changes. Stateful
migration/recreation needs an explicit operator-controlled workflow.

## Compose contract

Unsupported behavior is rejected loudly so a stack never runs differently than
its author expects. Important rejected directives include:

| Directive | Reason |
|---|---|
| `build:` | images must be built and pushed before deployment |
| `include`, `extends`, `env_file`, `label_file` | control plane must not read tenant-supplied paths |
| `privileged`, `cap_add`, `cap_drop` | breaks the current isolation contract |
| `pid`/`ipc`/`uts`/`userns_mode`, `devices`, `security_opt`, `sysctls`, `runtime` | host-namespace or device escape |
| `container_name` | collides between revisions |
| published host ports | collide during blue/green; use `x-composectl.ingress` |
| bind, tmpfs, or anonymous mounts | state is not portable or safely tracked |
| volume `driver_opts` / non-local drivers | bind-via-volume host-path escape |
| writable volume shared by services | unsupported ownership semantics |
| non-default `network_mode` | revisions receive isolated networks |
| replicas or scale greater than one | scaling is not implemented yet |
| `depends_on` condition other than `service_started` | start order is honoured; health-wait is not |
| cyclic or missing `depends_on` targets | invalid service graph |
| `profiles` | would silently omit services |
| pinned ingress service | cannot participate in traffic switching |

Validation collects all violations in one pass. Defaults are 250 millicpu and
256 MiB per service when limits are omitted.

## Secrets

Values containing `${secret:KEY}` are retained as templates and expanded by the
agent immediately before container creation:

```yaml
DATABASE_URL: postgres://app:${secret:db_password}@db:5432/app
```

The control plane encrypts values to the registered node's age recipient and
stores ciphertext only. Plaintext is decrypted agent-side and never returned by
the API.

## Architecture

```text
cmd/controlplane        API server and scheduler/controller/reaper loops
cmd/agent               node agent
internal/spec           normalized DeploymentSpec vocabulary
internal/parser         only package importing compose-go
internal/store          only package importing pgx
internal/api            HTTP decode/delegate/encode layer
internal/rollout        placement, rollout, and preview reaping
internal/agent           runtime-independent reconciliation
internal/agent/dockerd   only package importing the Docker SDK
internal/router          Traefik dynamic configuration
internal/secrets         age encryption boundary
migrations              golang-migrate SQL
```

These dependency boundaries are deliberate. In particular, the agent talks to
the control plane over HTTP and never accesses Postgres directly, while the
control plane binary does not link the Docker SDK.

## Direction

Next: packaging — versioned binaries and a deployment story for the control
plane itself, so a machine that is not the one it was written on can run it.

### What Navarch deliberately does not do

Named here rather than discovered in production:

- **No automatic failover.** Nothing re-homes an environment because its node
  went quiet. An unreachable node usually comes back, and its agent still holds
  desired state for its environments; re-homing automatically would run two
  copies of an environment for an unbounded window. Draining or retiring a node
  is an operator decision, and re-homing follows from it.
- **No durable log storage.** Container output is fetched on demand and buffered
  in memory, never written to Postgres. Application logs routinely contain
  secrets, and persisting them would undo what age sealing buys.
- **No cross-node overlay.** A deployment is placed whole onto one node. Ingress
  reaches it by node address and published port instead.
- **Node retirement is not automated.** `retired` is a state nothing sets: a
  policy loop with no operational history behind it is a guess on a schedule.
- **Roles are recorded but not enforced.** Every organization member has the
  same authority; the column exists so finer grants are a data migration later.
- **Nodes are not shared between organizations.** A node is a trust boundary:
  one agent holds one decryption identity for every environment it hosts, and
  tenant containers share a kernel. An organization brings its own nodes.
  Sharing them safely would need per-tenant sandboxing, which is a different
  product.
