# Sprint 3 Slice A — Secret Injection (encrypted store)

**Status:** design approved, pending spec review
**Date:** 2026-07-24
**Depends on:** Sprint 2 (agent reconciliation, rollout controller, routing)

---

## Context

Sprint 2's agent injects `${secret:KEY}` values from a **dev source** — a static
`COMPOSECTL_DEV_SECRETS` map baked into the agent's environment. That was an
explicit shortcut: real secrets never touched the control plane, but they were
plaintext in a compose env var, shared across every environment, and set by
editing `compose.yaml`.

Sprint 3 Slice A replaces that with a real encrypted, per-environment secret
store. The `secrets` table already exists (env-scoped, `ciphertext BYTEA`,
`key_id`, versioned) and the agent seam (`dockerd.SecretSource`) was built for
exactly this swap. The goal: an operator sets a secret via the API, the value is
stored as ciphertext the control plane cannot decrypt, and the agent decrypts it
at container start with a private key only it holds.

This is Sprint 3's first slice. Environment deletion (3B) and preview
environments (3C) are separate, later slices.

## Keystone decisions (settled in brainstorming)

1. **First slice: secret injection.** The most self-contained Sprint 3 piece;
   the seam already exists; it retires the biggest Sprint 2 shortcut.
2. **Crypto: age, asymmetric, agent keypair.** The agent generates an age
   keypair and sends its **recipient** (public key) at registration. The control
   plane encrypts secrets *to* that recipient and can never decrypt them; only
   the agent can, with its private key. Real separation of duties.

## Non-goals (explicit)

- **Multi-node re-keying.** "Encrypt to all ready nodes" is forward-looking, but
  a node that joins *after* a secret is set can't decrypt it. Re-keying on node
  join is Sprint 4 (multi-node). Sprint 3 is single-node: one recipient.
- **Strict client-side encryption.** The strongest model — operator encrypts
  locally, plaintext never touches the control plane — needs a CLI (Sprint 5).
  Sprint 3 accepts plaintext in the `POST` request; it exists only inside that
  one request, never at rest.
- **Secret rotation policy.** Latest-version-wins on re-set. Basic
  `DELETE .../secrets/{key}` is in scope; rotation schedules are not.
- **A secrets CLI/TUI.** Sprint 5.

---

## Crypto flow & key management

The agent owns the private key; the control plane holds only public recipients.

- **Identity, persisted.** On startup the agent loads its age identity from
  `COMPOSECTL_AGE_IDENTITY_FILE`; if absent it generates one and writes it there
  (0600). This *must* survive agent restarts — an identity regenerated each boot
  would make all existing ciphertext undecryptable. In the dev stack it lives on
  a named volume.
- **Recipient at registration.** The agent sends its age recipient (public key,
  `age1...`) in the register call. The control plane stores it on the node
  (`nodes.age_recipient`, via migration `0002`). No private key ever leaves the
  agent.
- **Encrypt to recipients, at set-time.** `POST /v1/envs/{env}/secrets` encrypts
  the value with age to the recipients of all ready nodes and stores the
  ciphertext. `key_id` records which recipients it was encrypted to. The control
  plane keeps no key capable of decrypting.
- **Agent decrypts at container start.** The agent fetches the env's ciphertext,
  decrypts each value with its identity into an in-memory per-env source, and
  expands `${secret:KEY}` exactly as today. Plaintext lives only in the agent
  process, at the moment it launches the container.

If no ready node has a recipient when a secret is set, the set fails with a
clear error (there is nothing to encrypt *to*). In practice the agent registers
at startup, so a recipient exists before secrets are set.

---

## Component map & boundaries

| Package | Role | Sole importer of |
|---|---|---|
| `internal/secrets` | age encrypt / decrypt / keypair load+generate | **filippo.io/age** |
| `internal/store` | ciphertext persistence; `age_recipient` on nodes | pgx (unchanged) |
| `internal/api` | set/list/delete secrets; recipient at register; secrets in desired-state; deploy-time fail-fast | — |
| `internal/agent` | load identity, decrypt per-env secrets in reconcile | — |
| `internal/agent/dockerd` | secret source becomes per-call, not global | Docker SDK (unchanged) |

`internal/secrets` is used by **both** binaries (control plane encrypts, agent
decrypts) and is the only place `filippo.io/age` is imported. The control-plane
binary linking age is expected and fine; it must still not link the Docker SDK.

### `internal/secrets` interface

```
type Identity struct { ... }          // wraps an age X25519 identity
func LoadOrGenerateIdentity(path string) (Identity, error)
func (i Identity) Recipient() string  // "age1..."
func Encrypt(plaintext string, recipients []string) ([]byte, error)
func (i Identity) Decrypt(ciphertext []byte) (string, error)
```

---

## Data flow & API

**Set path (control plane).** `POST /v1/envs/{env}/secrets {key, value}`:
handler → `ListReadyNodes` (recipients) → `secrets.Encrypt(value, recipients)` →
`store.SetSecret(env, key, ciphertext, keyID)` inserts a new **version** row
(latest-version-wins). `key` must match `SecretRefPattern`'s key charset.

**List path (operators).** `GET /v1/envs/{env}/secrets` returns metadata only —
`[{key, version, created_at}]`, **never** values or ciphertext.
`DELETE /v1/envs/{env}/secrets/{key}` removes all versions of a key.

**Get path (agent).** The `desired-state` response gains a `secrets` block:
`store.EncryptedSecretsForNode(nodeID)` returns the ciphertext for every env with
active deployments on that node, keyed by `env8`:

```json
{ "instances": [ ... ],
  "secrets": { "a1b2c3d4": [ {"key":"db_password","ciphertext":"<base64>"} ] } }
```

The agent decrypts each env's secrets once into a `map[string]string`, wraps it
as a `dockerd.SecretSource`, and injects it into that env's containers.

**Driver refactor.** Slice A's `dockerd.Driver` held a global `SecretSource`.
Secrets are per-environment, so that field is removed: `EnsureContainer` takes
the resolved source per call. `New(host)` drops the secrets parameter. The
`${secret:KEY}` expansion in `resolveEnv` is unchanged — only the source's
origin moves from a global field to a call argument.

**Fail-fast on missing secrets.** `POST .../deployments` calls
`spec.RequiredSecrets()` on the resolved spec and checks each key is set for the
env (`store.SecretKeysForEnv`). If any are missing it rejects with **422** and
lists them, so a stack referencing `${secret:db_password}` with no secret set is
caught at deploy time, not by a crash-looping container. Flow: set secrets, then
deploy.

### Store additions

- `SetSecret(ctx, envID, key string, ciphertext []byte, keyID string) error` —
  insert next version.
- `SecretKeysForEnv(ctx, envID) ([]SecretMeta, error)` — `{Key, Version, CreatedAt}`, latest per key, no values.
- `DeleteSecret(ctx, envID, key string) error`.
- `EncryptedSecretsForNode(ctx, nodeID) (map[string][]EncryptedSecret, error)` —
  keyed by env8, latest version per key, for the node's active envs.
- `age_recipient` threaded through `RegisterNodeParams`, `RegisterNode`,
  `ListReadyNodes`, `ListNodes`, `Node`.

---

## Migration

`migrations/0002_node_age_recipient.up.sql`:

```sql
ALTER TABLE nodes ADD COLUMN age_recipient TEXT;
```

`0002_..._down.sql`: `ALTER TABLE nodes DROP COLUMN age_recipient;`. `0001`
stays immutable. This is the project's first migration since the initial schema
— the `make migrate-up` path and the compose `migrate` service already apply
all files in `migrations/`, so no tooling change is needed.

---

## Testing

Same real-dependency discipline; skip loudly when a dependency is absent.

- **`internal/secrets`** — pure unit tests: encrypt→decrypt round-trip; decrypt
  with the *wrong* identity errors; multi-recipient (any recipient decrypts);
  `LoadOrGenerateIdentity` persists and reloads the same identity.
- **`store`** — `SetSecret` versioning; `SecretKeysForEnv` returns metadata not
  values; `EncryptedSecretsForNode` scopes to the node's active envs;
  `age_recipient` round-trips through register/list. Real Postgres.
- **`api`** — set/list/delete handlers; the **422** when a required secret is
  missing; list never leaks values.
- **`agent`/`dockerd`** — the per-call secret source refactor (adapt existing
  tests); reconcile decrypts a real age-encrypted value with a test keypair and
  injects it.

**Test-hygiene carry-over (from Sprint 2):** store/api tests are not isolated
from a running control plane's scheduler; run them with the dev stack's control
loops stopped, or against a separate DB.

## Demo & verification

`make demo-secrets` proves the full loop *visibly*:

1. `POST` secret `name=<value>` to an environment.
2. `psql` shows the `secrets` row holds **ciphertext, not the value**.
3. Deploy a stack whose ingress is `traefik/whoami` with
   `WHOAMI_NAME=${secret:name}` (whoami echoes `WHOAMI_NAME` in its response).
4. `curl` through Traefik returns `Name: <value>` — the agent decrypted and
   injected it.

Plus: deploying with the secret **unset** returns 422 (fail-fast).

**Retires `COMPOSECTL_DEV_SECRETS`.** `examples/hello` references
`${secret:db_password}`, so `make demo` must now **set that secret before
deploying** — the demos are updated to set secrets first (the intended flow).
The agent no longer reads `COMPOSECTL_DEV_SECRETS`; it reads its identity file
and decrypts from the store.

Baseline that must not regress: `examples/webapp` digest `6072c68f…`,
classification, peak `2415919104`; the 409 guards; the Sprint 2 flip and
rollback demos (updated to set secrets where needed).

## Open questions

None blocking. One item decided rather than asked, flagged for spec review:
**encrypt-to-all-ready-nodes** (vs. encrypt-to-the-placed-node). All-ready-nodes
is simpler for single-node dev and forward-looking; the placed-node approach
would defer encryption until scheduling. Reversible; single-node makes them
equivalent today.
