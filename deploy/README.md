# Deploying Navarch

Three things live here:

| Path | What it is |
|---|---|
| `Dockerfile` | Builds the control plane, agent and console images. Multi-stage, distroless, static. |
| `production/` | A single-host install: Postgres, migrations, API, router, console, one agent. |
| `production/dynamic/` | Static Traefik routes for the console and the API. |
| `tls/` | The dev-stack proxy `make demo-tls` exercises. |

### What faces the world, and what does not

**One component: Traefik, on 80 and 443.** It serves your tenants' stacks, the
console and the API, with a certificate per hostname. The control plane,
the console and Postgres publish nothing at all.

One ingress rather than a proxy in front of another, because Traefik is already
the component that knows every hostname the platform serves — which makes it the
right place for certificates. **It issues for exactly the hostnames it has
routers for, so the route list is the authorization.** A separate proxy would
need telling which hostnames are legitimate, kept in step with the route list,
and wrong in the dangerous direction the moment it drifted.

Certificates come from the ACME HTTP-01 challenge on port 80, which Traefik
already owns. No wildcard, so **no DNS-provider credential sits in the
ingress** — and a customer pointing their own domain at this host gets a
certificate for it like any other route.

Plain HTTP redirects to HTTPS. That redirect does **not** swallow
`/.well-known/acme-challenge/` — checked against `traefik:v3.3`, where the
challenge is served before routing, so issuance still completes.

`navar.ch` throughout is the real install, not a placeholder — the domain spells
the product across the dot, so the worked examples below are the commands that
were actually run rather than ones with a name to substitute. Substitute your
own anywhere it appears. `deploy/production/env.example` deliberately does
*not* carry it: that file is copied to `.env` and started, and a real domain
left unedited would point this host's ACME requests at a name it does not own.

| Name | Serves |
|---|---|
| `console.navar.ch` | the console — what a person opens |
| `api.navar.ch` | the API — agents, the CLI, CI |
| `navar.ch` | whatever stack you deploy there — see below |
| anything else you route | your deployed environments |

**The platform claims only subdomains.** The bare domain is left for a stack you
deploy, because two routers with the same `Host` rule is not an error in Traefik
— it is an arbitrary winner, so a static route on the apex would quietly fight
every tenant route generated for it.

The platform's own two routes are static, in
`deploy/production/dynamic/platform.yml`, alongside the tenant routes the
control plane regenerates every tick. Traefik reads the whole directory, so
neither file knows about the other — which is the point: the control plane must
never write a route it did not derive from a live deployment.

### The preview wildcard, and when to turn it on

By default **every hostname is its own certificate** — the console, the API,
each tenant, and each preview, which is a fresh name every time CI opens one.
Let's Encrypt counts issuance per registered domain per week (50, at the time of
writing; check before you lean on it), and everything here now sits under one
registered domain. So **previews are what reach that ceiling, not tenants** —
and reaching it stops issuance for the whole install, tenants included.

The fix is one wildcard, `*.preview.navar.ch`, covering all of them at once. It
needs the DNS-01 challenge, because there is no host that can answer an HTTP-01
challenge for a name with a `*` in it — and that means a DNS-provider credential
sitting in the ingress, which is exactly what HTTP-01 was chosen to avoid.

That trade is why it is **off by default and scoped as narrowly as the mechanism
allows**. The wildcard covers only the domain the platform mints names in.
Tenant hostnames and customers' own domains stay on HTTP-01, where there is no
credential to steal. Turning it on is one variable:

```bash
cp deploy/production/dns.env.example deploy/production/dns.env
$EDITOR deploy/production/dns.env      # CF_DNS_API_TOKEN=...
$EDITOR deploy/production/.env         # NAVARCH_DNS_PROVIDER=cloudflare
docker compose -f deploy/production/compose.yaml up -d
```

`dns.env` is separate from `.env` on purpose: `.env` holds the database password
and the service token, and the one container facing the internet should not be
handed either. Both are gitignored.

**Scope the token to a delegated zone if you can.** Delegate
`preview.navar.ch` to its own zone with an `NS` record and issue the token
against that zone alone — then the credential in the ingress cannot touch the
apex, `api.`, or `console.`, the three names whose loss is an outage rather than
an inconvenience. Traefik does not care which zone the challenge record lands
in.

`make demo-wildcard` runs the whole path locally against Pebble — Let's
Encrypt's throwaway CA — and asserts the arithmetic: three preview hostnames,
**one** DNS-01 challenge, all three serving on a chain that verifies. No public
DNS and no credential needed, because the only substitution is the DNS provider.

**Check it by opening a preview, not by watching it boot.** Traefik starts
cleanly with a DNS-01 resolver that has no credentials, or a provider name that
is empty — verified against `traefik:v3.3`. The failure surfaces at issuance:
the preview serves Traefik's self-signed fallback and the log carries the ACME
error. `docker compose logs traefik | grep -i acme` is the place to look.

A preview hostname is one label below the preview domain, which is what a
wildcard covers. Point an ordinary environment two labels deep — 
`a.b.preview.navar.ch` — and it keeps its own certificate, because claiming a
name the wildcard cannot cover would serve the browser a mismatch.

### Transactional email

Optional, and off unless configured. With it, three things reach an
organization's operators: an **invitation** to join, a **failed rollout** with
the reason the node agent reported, and a **warning** an hour before a preview
environment is destroyed.

```bash
cp deploy/production/mail.env.example deploy/production/mail.env
$EDITOR deploy/production/mail.env      # domain, key, from
docker compose -f deploy/production/compose.yaml up -d
```

Its own file, like `dns.env` — so the key can be rotated without opening the one
that holds the database password. Gitignored.

**A Mailgun subdomain sends; it does not receive.** Do not put an address at it
anywhere something has to reach you — `NAVARCH_ACME_EMAIL` above all, since
Let's Encrypt mails expiry warnings there and one nobody reads is worse than
none.

**Nothing depends on it.** A rollout that failed is still recorded as failed, a
preview still expires on time, and an invitation still returns a link to paste.
`navarch invite create` says which of those happened rather than assuming the
mail arrived.

### Inviting operators

```bash
navarch invite create acme ada@example.com --role member
navarch invite list acme
navarch invite revoke acme <invite-id>
```

The invited person opens the link, which signs them in and creates their own
credentials — or, if they would rather stay in the terminal:

```bash
navarch invite accept nav_... --url https://api.navar.ch
```

That is the one command that needs no token first, which is the whole point.

**The link is exchanged for a credential; it is never the credential.** It works
once, expires in seven days by default, and is worth nothing afterwards.
Re-inviting the same address supersedes the previous link rather than leaving
two live. Opening the link does not spend it — only accepting does — so a mail
scanner or a link previewer cannot burn an invitation before its recipient sees
it.

**Back up the `acme` volume with `pgdata`.** Losing it re-issues every hostname
from scratch, against that same weekly budget.

---

## Install

```bash
git clone https://github.com/craigderington/navarch && cd navarch
cp deploy/production/env.example deploy/production/.env
$EDITOR deploy/production/.env    # version, database password, service token, operator email
docker compose -f deploy/production/compose.yaml up -d
```

**The `.env` goes beside the compose file, not in the repository root.** Compose
reads it from the *project* directory, which a bare
`-f deploy/production/compose.yaml` sets to that file's directory — checked, and
a root `.env` is ignored even when one is sitting there. The `:?` guards fail
loudly if you get it wrong, so this costs a minute rather than a mystery.

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
navarch login --url https://api.navar.ch
Operator token:
logged in to https://api.navar.ch as you@example.com
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

### TLS

The control plane speaks plaintext HTTP and does not know whether it is behind a
proxy — correct for a process on a private network, wrong the moment it is
reachable from anywhere else. An operator token is a bearer credential and a
node token pulls that node's age ciphertext.

DNS must resolve **before** you start it: ACME verifies by connecting to the
name, so a certificate request for a hostname that does not point here fails and
Traefik backs off.

```
A  console.navar.ch    -> your static IP   # the console
A  api.navar.ch        -> your static IP   # the API
A  navar.ch            -> your static IP   # a stack you deploy there
A  *.preview.navar.ch  -> your static IP   # preview environments
```

Separate names because they are separate audiences: a person opens the console,
agents and CI talk to the API. Neither has to know the other's routes, and
either can move without breaking the other.

Tenant hostnames need DNS too — each environment's hostname must resolve here
before it will get a certificate. A wildcard covers the ones you generate;
a customer's own domain is theirs to point.

The CLI enforces the other half: it refuses to send a token
over plaintext HTTP to anything that is not loopback or a container network,
unless `NAVARCH_INSECURE=1` — which warns on every single invocation.

---

## Deploy something on it

The first thing a fresh install should do is run a stack, because that is the
only proof the whole path works: parse, schedule, place, pull, start, health,
promote, route, certificate. This repository's own marketing site is the
smallest honest one — a single swappable service with an ingress and no durable
state — and it is what goes on the bare domain.

```bash
navarch app   create site --org acme --name "Site"
navarch stack create main --app acme/site
navarch stack push  acme/site/main examples/site/compose.yaml
navarch env   create www --stack acme/site/main --hostname navar.ch
navarch deploy --env acme/site/main/www
navarch wait <deployment-id> --state live
```

That is the whole sequence, and it is the same six commands for any stack. The
hostname is yours to choose; the certificate follows the route, so there is no
seventh step for TLS.

`ghcr.io/craigderington/navarch/site:2` is published by the release workflow
alongside the platform images. **The platform never builds** — `build:` is a
rejected compose directive — so every stack you deploy is an image you pushed
somewhere the node can pull from. That is the contract that makes a deployed
revision reproducible, and it applies to this repository's own site too.

If the deployment sits in `scheduling`, the node has no capacity: reservations
come from declared limits and nothing releases a live environment's, so
`NAVARCH_NODE_MEMORY_MB` is the number to look at. If it reaches `live` but the
hostname does not answer, DNS is the first thing to check — Traefik will not
have a certificate for a name that does not point here.

---

## Upgrade

**Pull, migrate, restart. In that order.**

```bash
# 1. Choose the version
$EDITOR deploy/production/.env    # NAVARCH_VERSION=1.2.0
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
      COMPOSECTL_CONTROLPLANE_URL: https://api.navar.ch
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

### Bring your own infrastructure

A node does not have to be yours. Someone else can run the machine, the
containers, the durable volumes and the ingress, while this control plane does
catalog, scheduling and secret sealing — and never connects to them.

That works because **the agent runs no server**. Every interaction is outbound:
desired state, reports, heartbeats, log delivery. The one inbound path in the
system is a router reaching a tenant container, so if the router runs on their
machine too, nothing reaches in at all. A box behind NAT joins the fleet with no
firewall change.

Issue them a join token scoped to their organization:

```bash
navarch node join-token create acme --name "acme fleet" --expires-in-days 30
```

They run an agent with it, plus a router beside it:

```yaml
services:
  agent:
    image: ghcr.io/craigderington/navarch/agent:1.0.0
    environment:
      COMPOSECTL_CONTROLPLANE_URL: https://api.navar.ch
      COMPOSECTL_AGENT_TOKEN: <the join token>   # no org: the token names it
      COMPOSECTL_NODE_HOSTNAME: acme-1
      COMPOSECTL_ADVERTISE_ADDR: 10.0.1.12       # reachable from THEIR router
      COMPOSECTL_ROUTER_DIR: /dynamic            # router mode
      COMPOSECTL_ROUTER_CERT_RESOLVER: le        # names the resolver below
    volumes:
      - /var/run/docker.sock:/var/run/docker.sock
      - dynamic:/dynamic
      - age-identity:/identity
  router:
    image: traefik:v3.3
    command: ["--entryPoints.web.address=:80",
              "--entryPoints.web.http.redirections.entryPoint.to=websecure",
              "--entryPoints.web.http.redirections.entryPoint.scheme=https",
              "--entryPoints.websecure.address=:443",
              "--providers.file.directory=/dynamic",
              "--providers.file.watch=true",
              "--providers.providersThrottleDuration=100ms",
              "--certificatesresolvers.le.acme.email=them@example.com",
              "--certificatesresolvers.le.acme.storage=/acme/acme.json",
              "--certificatesresolvers.le.acme.httpchallenge=true",
              "--certificatesresolvers.le.acme.httpchallenge.entrypoint=web"]
    ports: ["80:80", "443:443"]
    volumes: [ "dynamic:/dynamic:ro", "acme:/acme" ]
```

**Their router terminates TLS, not ours.** `COMPOSECTL_ROUTER_CERT_RESOLVER`
does for a customer-side router exactly what it does for the platform's: every
route the agent writes becomes an HTTPS router using that ACME resolver. Leave
it unset and their tenant hostnames are served over **plain HTTP** — the
platform's own router has served HTTPS since tenant TLS landed, and this is the
one place that asymmetry can hide, because a router we do not run is a router we
cannot observe.

The name has to match a `--certificatesresolvers.<name>.acme...` flag on their
Traefik, which is why it is set in their environment rather than delivered by
the control plane: a resolver name is Traefik's vocabulary, and
`GET /v1/nodes/{id}/routes` deliberately answers in ours. Both live in their
compose file, where one person sees them together.

The certificates are theirs too. Their tenant hostnames must resolve to **their**
box for HTTP-01 to complete, and the `acme` volume wants the same backup
treatment as ours.

`make demo-byo` runs exactly this shape and asserts the part that matters: the
platform's own router **cannot** reach that node.

Three things to know:

- **The org comes from the token, not the request.** A body naming a different
  org is refused. Set `COMPOSECTL_REQUIRE_JOIN_TOKEN=1` to stop the shared
  service token enrolling anything, which any multi-tenant control plane should.
- **Set their preview domain**, or previews are generated under yours and point
  at infrastructure they do not have. It is a per-org column.
- **Route withdrawal does not apply to their router.** It only updates while
  their agent is polling, so a node that is down keeps its routes until it comes
  back. That is their failure domain, not yours.

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
