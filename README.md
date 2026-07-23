# composectl

A container platform for **Docker Compose stacks**, not single containers.
Where Lightsail gives you one image per deployment, composectl treats an
entire compose stack as the deployable unit, with versioned revisions and
zero-downtime rollouts across the whole stack.

## Sprint 1 scope

- [x] Postgres schema (append-only deployments, slot alternation)
- [x] compose-go parser → normalized `DeploymentSpec`
- [x] Platform constraint validation
- [x] Control plane API skeleton
- [x] Dev stack via Compose
- [ ] Node agent reconciliation loop
- [ ] Traefik dynamic config generation

## Quickstart

```bash
make up          # postgres + migrations + control plane on :8417
make health
make validate    # parse the example stack, see the classification
```

## The core idea: swappable vs pinned

Blue/green means running two copies of a stack at once. Services holding
durable state cannot be duplicated — two Postgres processes on one volume
corrupt it. So the parser classifies every service:

| Classification | Trigger | Rollout behavior |
|---|---|---|
| **swappable** | no writable named volume | duplicated blue/green |
| **pinned** | mounts a writable named volume | runs once, shared across revisions |

`make validate` shows the classification and the peak memory a rollout
will need — swappable services counted twice, pinned once.

## Rejected compose directives

Rejected loudly rather than dropped silently, so a stack never runs
differently than its author expects:

| Directive | Reason |
|---|---|
| `build:` | pre-build and push; the platform does not build |
| `privileged`, `cap_add` | breaks isolation |
| `container_name` | collides between revisions |
| `ports: "8080:80"` | host port collides between revisions; use `x-composectl.ingress` |
| bind mounts | host paths are not portable across nodes |
| `network_mode` | each revision gets an isolated network |
| `deploy.replicas` | scaling is the platform's job |

All violations are collected in one pass.

## Secrets

Values containing `${secret:KEY}` are stored as **templates** in
`SecretEnv` and expanded by the agent at container start, so plaintext
never reaches the control plane or the database. Because a secret is
usually embedded in a larger string, the whole value is retained:

```yaml
DATABASE_URL: postgres://app:${secret:db_password}@db:5432/app
```

## Layout

```
cmd/controlplane   API server entrypoint
internal/spec      normalized DeploymentSpec — the platform's vocabulary
internal/parser    the ONLY package importing compose-go
internal/store     the ONLY package importing pgx
internal/api       thin handlers: decode, delegate, encode
migrations/        golang-migrate SQL
```

The parser boundary matters: everything downstream speaks `DeploymentSpec`,
so the compose implementation can be swapped without touching the
scheduler or agent.

## Next

**Sprint 2** — node agent, health gating, traffic flip, rollback.
