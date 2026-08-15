# composectl

A container platform for **Docker Compose stacks**, not single containers.
composectl treats an entire stack as the deployable unit, with immutable
versions, health-gated blue/green rollouts, rollback, encrypted secrets, and
ephemeral preview environments.

The repository is currently being developed under the Quartermaster project
name; binaries, environment variables, labels, and the Go module still use the
original `composectl` identity.

## Current status

The single-node platform loop is implemented end to end:

- Compose parsing into a normalized, versioned deployment specification
- Strict rejection of unsupported Compose behavior
- Postgres-backed catalog and append-only deployment history
- Node-agent reconciliation against Docker Engine
- Health-gated blue/green promotion and rollback
- Traefik file-provider routing
- Age-encrypted, per-environment secrets
- Preview environments with inherited secrets, TTL expiry, and durable teardown
- Shared bearer authentication for the development API

Only `blue_green` is currently supported. Requests for `rolling` or `recreate`
are rejected rather than silently receiving different behavior. The current
security model is a single shared token and is suitable for trusted development
deployments, not a public multi-tenant control plane. The compose file binds
Postgres, the API, and Traefik to loopback so fleet containers cannot hairpin
to the control plane. Traefik's insecure API is disabled.

## Quickstart

The development token defaults to `dev-token-change-me` in `compose.yaml` and
the Makefile. Override `API_TOKEN` when using a different
`COMPOSECTL_AGENT_TOKEN`.

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
make test          # race-enabled Go test suite
```

## CLI

`composectl` is the operator client. It speaks the same bearer token as the
API and prints tables by default (`--output json` for scripts).

```bash
make build                 # writes bin/composectl
export COMPOSECTL_URL=http://localhost:8417
export COMPOSECTL_TOKEN=dev-token-change-me

composectl health
composectl validate examples/hello/compose.yaml
composectl org list
composectl stack push <stack-id> examples/hello/compose.yaml
composectl deploy --env <env-id>
composectl events --org <org-id>
```

Config file (optional): `~/.config/composectl/config.yaml` with `url` and `token`.
Flags override the environment; the environment overrides the file.

Direct API requests require a bearer token:

```bash
curl -H 'Authorization: Bearer dev-token-change-me' \
  http://localhost:8417/v1/orgs
```

`GET /healthz` is deliberately unauthenticated for container and load-balancer
probes.

## Audit timeline and metrics

Deployment creation, state transitions, promotion, supersession, preview
expiry, and secret set/delete metadata are appended to the existing `events`
table in the same transaction as the operation they describe. Audit entries
survive deployment deletion; secret values and request bodies are never
recorded.

The authenticated organization timeline is cursor-paginated:

```bash
curl -H 'Authorization: Bearer dev-token-change-me' \
  'http://localhost:8417/v1/orgs/<org-uuid>/events?limit=50&before_id=1234'
```

`GET /metrics` is authenticated and emits Prometheus text format. It includes
HTTP requests by method/route-pattern/status, scheduler/controller/reaper tick
results and latest durations, database availability, deployments by state,
ready nodes, active previews, and recent teardown tombstones. Raw request paths,
compose content, environment values, and secrets are not metric labels.

## The core idea: swappable vs pinned

Blue/green runs two revisions at once. Services holding durable state cannot be
duplicated safely, so the parser classifies every service:

| Classification | Trigger | Rollout behavior |
|---|---|---|
| **swappable** | no writable named volume | duplicated during blue/green |
| **pinned** | mounts a writable named volume | one container shared across revisions |

Read-only named-volume mounts remain swappable. Peak rollout capacity counts
swappable services twice and pinned services once, for both CPU and memory.

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

The next reliability work is an explicit stateful-service migration workflow
and alerting/dashboard definitions over the metrics surface. Multi-node
placement, node draining, per-node credentials, secret re-encryption on
membership changes, and the WireGuard mesh belong to a later phase.
