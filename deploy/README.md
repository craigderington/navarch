# Deploying Navarch

Three things live here:

| Path | What it is |
|---|---|
| `Dockerfile` | Builds the control plane and agent images. Multi-stage, distroless, static. |
| `production/` | A single-host control plane: Postgres, migrations, API, router, one agent. |
| `tls/` | The reverse proxy that terminates TLS in front of the API. |

---

## Install

```bash
git clone https://github.com/craigderington/navarch && cd navarch
cp deploy/production/env.example .env
$EDITOR .env                      # version, database password, service token, operator email
docker compose -f deploy/production/compose.yaml up -d
```

**Capture the operator token now.** It is minted from `crypto/rand` on first
boot and logged exactly once. There is no second copy and no way to print it
again — the database stores only its SHA-256.

```bash
docker compose -f deploy/production/compose.yaml logs controlplane | grep bootstrap
```

Then log in. The token is never taken as an argument — it is prompted for
without echo, or read from stdin — because argv lands in shell history, `ps`,
and every exec audit log on the box:

```bash
navarch login --url https://navarch.example.com
Operator token:
logged in to https://navarch.example.com as you@example.com
```

It is verified against the control plane before it is written, so a config file
never holds a credential that does not work. The token is stored 0600 in
`~/.config/navarch/config.yaml`; `NAVARCH_TOKEN` still overrides it, which is
what CI should use.

Once in, mint a separate credential for anything that is not you, and give it
an expiry:

```bash
navarch token create ci --expires-in-days 90
navarch token list
navarch token revoke <id>
```

Revoking your only token is refused — nothing can issue a replacement to an
existing operator, so it would lock you out permanently. Create the new one
first, then revoke the old, which is the right order for a rotation anyway.
`navarch logout` only forgets the token on this machine; it stays valid until
it is revoked.

`navarch whoami` is the first thing to run and the first thing to run when
anything is confusing later. Authorization refuses with 404 rather than 403 so
that one tenant cannot probe another's ids, which means "not found" and "not
yours" look identical — `whoami` is how you tell them apart.

### Put TLS in front of it

`production/compose.yaml` publishes **no API port at all**. The control plane
speaks plaintext HTTP and does not know whether it is behind a proxy; that is
correct for a process on a private network and wrong the moment it is reachable
from anywhere else. An operator token is a bearer credential and a node token
pulls that node's age ciphertext.

```bash
docker compose -f deploy/production/compose.yaml -f deploy/tls/compose.yaml up -d
```

Edit `deploy/tls/Caddyfile` to your hostname first; Caddy obtains a certificate
on first request. The CLI enforces the other half: it refuses to send a token
over plaintext HTTP to anything that is not loopback or a container network,
unless `NAVARCH_INSECURE=1` — which warns on every single invocation.

---

## Upgrade

**Pull, migrate, restart. In that order.**

```bash
# 1. Choose the version
$EDITOR .env                      # NAVARCH_VERSION=1.1.0
docker compose -f deploy/production/compose.yaml pull

# 2. Apply migrations while the OLD control plane is still serving
docker compose -f deploy/production/compose.yaml run --rm migrate

# 3. Restart onto the new images
docker compose -f deploy/production/compose.yaml up -d
```

### Why that order is safe, and why it is the only safe one

Migrations here are **immutable and additive**. A migration file is never
edited once applied — a change is always a new file — and every migration so
far adds tables, columns or indexes rather than removing or narrowing them.
That is what makes step 2 safe to run before step 3: the old binary is still
running against the new schema for the length of the restart, and additive
changes are invisible to it.

Doing it the other way round is what breaks. Start the new binaries first and
they query columns that do not exist yet, for however long the migration takes.

`migrate up` on an already-current database is a clean no-op, so step 2 is safe
to run when there is nothing new — which means the sequence is the same whether
or not a release contains migrations, and you never have to know which.

The one thing this ordering does **not** cover is a migration that removes or
narrows something the old binary still uses. There has not been one, and if
there ever is it has to be split across two releases — add the new shape, ship
the binary that uses it, remove the old shape in the release after. That is a
rule about writing migrations, not about running them.

`scripts/migrate-check.sh` (`make migrate-check`) round-trips the whole chain up,
down and up again on a throwaway database, so a `down` migration that does not
actually undo its `up` fails in CI rather than during an incident.

### What survives an upgrade, and what you must back up

| Volume | Holds | If you lose it |
|---|---|---|
| `pgdata` | The entire catalog, deployment history, audit events, secret ciphertext | Everything. Back it up. |
| `age-identity` | The node's decryption key | Every secret must be re-set — the ciphertext in Postgres becomes unreadable. Back it up **with** `pgdata`; separately they are both useless. |
| `traefik-dynamic` | Generated router config | Nothing. It is rewritten from the database within a tick. |

Tenant stacks' own named volumes live on the Docker daemon the agent drives,
under `cc-{env8}-{volume}`. They are not in this compose file and are not
touched by an upgrade.

---

## Scaling to more than one node

`production/compose.yaml` runs one agent on the control plane's own host, which
is the smallest useful shape. A second node is a second machine running only the
agent, pointed at the same control plane:

```yaml
services:
  agent:
    image: ghcr.io/craigderington/navarch/agent:1.0.0
    restart: unless-stopped
    environment:
      COMPOSECTL_CONTROLPLANE_URL: https://navarch.example.com
      COMPOSECTL_ORG: dev
      COMPOSECTL_NODE_HOSTNAME: node-2
      COMPOSECTL_ADVERTISE_ADDR: 10.0.1.12   # reachable from the router's host
      COMPOSECTL_AGENT_TOKEN: <the service token>
    volumes:
      - /var/run/docker.sock:/var/run/docker.sock
      - age-identity:/identity
volumes:
  age-identity:
```

Two things to know before you do this:

- **Nodes are org-scoped, deliberately.** A node is a trust boundary: one agent
  holds one decryption identity for every environment it hosts, and tenant
  containers share a kernel. An organization brings its own nodes.
- **An environment is bound to its first node for life.** Its pinned containers
  and named volumes cannot follow it, so the scheduler refuses to place a later
  revision anywhere else rather than rebuild the stack over empty volumes. Moving
  one is an explicit `navarch node drain`.

The agent's registration announces an age public key. If a node ever comes back
with a *different* key — a rebuilt host, a lost `age-identity` volume — the
control plane records it as pending and keeps sealing to the old one until you
say so:

```bash
navarch node list --org dev            # KEY column shows "rotation pending"
navarch node rotate-recipient dev/node-2
```

That is deliberate: anyone able to register a node could otherwise redirect the
key every future secret is sealed to.
