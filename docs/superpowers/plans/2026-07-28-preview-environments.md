# Preview Environments Implementation Plan

> **For agentic workers:** REQUIRED SUB-SKILL: Use superpowers:subagent-driven-development (recommended) or superpowers:executing-plans to implement this plan task-by-task. Steps use checkbox (`- [ ]`) syntax for tracking.

**Goal:** An operator creates an ephemeral per-branch environment with one API call, gets a working URL back, and the platform destroys it — containers, pinned database, and named volumes — when its TTL expires.

**Architecture:** A preview is an ordinary `environments` row with `ephemeral` and `expires_at` set, so the existing scheduler → agent → router spine carries it unchanged. Secrets are inherited by copying ciphertext (sealed to node age recipients, never to an environment, so no decryption is involved). Teardown is driven by an `environment_tombstones` row that deliberately outlives the deleted environment: the agent must be *told* to destroy durable state, because "no desired row" must never mean "drop the database".

**Tech Stack:** Go 1.25, Postgres (pgx), Docker Engine SDK, Traefik file provider, `log/slog`.

**Spec:** `docs/superpowers/specs/2026-07-28-preview-environments-design.md`

## Global Constraints

- Go 1.25. Ports never 3000/5000/8000/9000 — Postgres `5473`, API `8417`.
- Commit locally only. **Never push.** Branch `sprint3-previews` (already created).
- Postgres always; no SQLite fallback, not even in tests.
- **Boundaries:** only `internal/store` imports pgx; only `internal/agent/dockerd` imports the Docker SDK; only `internal/parser` imports compose-go. The agent never imports pgx. Guard: `go list -deps ./cmd/controlplane | grep docker/docker` must return nothing.
- Migrations are immutable once applied — add `0003_*`, never edit `0001` or `0002`.
- Errors wrap with `%w`. Store returns `ErrNotFound` / `ErrConflict` / `ErrInvalid`; handlers use `s.writeStoreError`.
- Every context gets a timeout. Structured logging via `log/slog`, no `fmt.Println`.
- Comments explain **why**, not what. Match the existing register.
- **Naming is fixed and load-bearing:** swappable container `cc-{env8}-r{rev}-{slot}-{service}`, pinned container `cc-{env8}-pinned-{service}`, revision network `cc-{env8}-r{rev}-{slot}`, named volume `cc-{env8}-{volume}`. Labels `cc.env`, `cc.deployment`, `cc.service`, `cc.swappable`. `env8` = `store.shortID(environmentID)`.
- Tests skip loudly without their dependency. **A green run with Postgres or Docker down proves nothing — check for `--- SKIP`.**

---

## File Structure

**Create:**

| File | Responsibility |
|---|---|
| `migrations/0003_preview_environments.up.sql` / `.down.sql` | `ephemeral`/`expires_at` columns, CHECK, index, `environment_tombstones` |
| `internal/store/previews.go` | `CreatePreview`, `ExpireEnvironments`, tombstone reads/sweeps |
| `internal/store/previews_test.go` | store tests against real Postgres |
| `internal/rollout/reaper.go` | `ReapOnce` — the third control-plane loop |
| `internal/rollout/reaper_test.go` | reaper tests |
| `internal/api/previews.go` | the single `POST /previews` handler + hostname generation |
| `internal/api/previews_test.go` | handler tests |
| `scripts/demo-preview.sh` | end-to-end demo including the reap |

**Modify:**

| File | Change |
|---|---|
| `internal/store/models.go` | `Environment` gains `Ephemeral`, `ExpiresAt` |
| `internal/store/stacks.go` | `GetEnvironment` selects new columns; add `GetStack`, `GetEnvironmentBySlug` |
| `internal/store/catalog.go` | `ListEnvironments` selects new columns |
| `internal/store/deployments.go` | extract `createDeploymentTx` so `CreatePreview` can share the transaction |
| `internal/api/server.go` | `NewServer` variadic options; `WithPreviewDomain`; route |
| `internal/api/nodes.go` | `teardown_envs` in the desired-state response |
| `internal/config/config.go` | `PreviewDomain` |
| `cmd/controlplane/main.go` | wire the reaper loop and the preview domain |
| `internal/agent/reconcile.go` | `DockerDriver` gains two methods; `Reconcile` takes teardown envs |
| `internal/agent/agent.go` | decode and pass `teardown_envs` |
| `internal/agent/dockerd/driver.go` | implement `EnsureVolume`, `RemoveEnv` |
| `Makefile` | `demo-preview` target |
| `CLAUDE.md` | sprint status (currently stale) |

---

## Task 1: Schema and model

**Files:**
- Create: `migrations/0003_preview_environments.up.sql`, `migrations/0003_preview_environments.down.sql`
- Create: `internal/store/previews_test.go`
- Modify: `internal/store/models.go:52-61`, `internal/store/stacks.go` (`GetEnvironment`), `internal/store/catalog.go:162-187` (`ListEnvironments`)

**Interfaces:**
- Consumes: nothing.
- Produces: `Environment.Ephemeral bool`, `Environment.ExpiresAt *time.Time`; table `environment_tombstones(env8 TEXT PK, org_id UUID, created_at TIMESTAMPTZ)`.

- [ ] **Step 1: Write the failing test**

Create `internal/store/previews_test.go`:

```go
package store

import (
	"testing"
	"time"
)

// The CHECK constraint is the whole safety story for expiry: an ephemeral
// environment with no expiry is a leak the reaper can never see, so the
// database must refuse to store one rather than let it be detected later.
func TestEphemeralEnvironmentRequiresExpiry(t *testing.T) {
	st := testStore(t)
	ctx := testCtx(t)
	org := newOrg(t, st)
	app := newApp(t, st, org.ID)
	stack := newStack(t, st, app.ID)

	env, err := st.CreateEnvironment(ctx, CreateEnvironmentParams{
		StackID: stack.ID, Slug: uniq("pr"),
	})
	if err != nil {
		t.Fatalf("CreateEnvironment: %v", err)
	}

	_, err = st.pool.Exec(ctx,
		`UPDATE environments SET ephemeral = true, expires_at = NULL WHERE id = $1`, env.ID)
	if err == nil {
		t.Fatal("ephemeral environment with NULL expires_at must be rejected")
	}

	// The same update with an expiry must succeed, or the constraint is too broad.
	if _, err := st.pool.Exec(ctx,
		`UPDATE environments SET ephemeral = true, expires_at = $2 WHERE id = $1`,
		env.ID, time.Now().Add(time.Hour)); err != nil {
		t.Fatalf("ephemeral with expiry must be allowed: %v", err)
	}

	got, err := st.GetEnvironment(ctx, env.ID)
	if err != nil {
		t.Fatalf("GetEnvironment: %v", err)
	}
	if !got.Ephemeral {
		t.Error("GetEnvironment must surface ephemeral")
	}
	if got.ExpiresAt == nil {
		t.Error("GetEnvironment must surface expires_at")
	}
}
```

- [ ] **Step 2: Run the test to verify it fails**

Run: `go test ./internal/store/ -run TestEphemeralEnvironmentRequiresExpiry -v`
Expected: FAIL — `got.Ephemeral` undefined (compile error). If it SKIPs, Postgres is down; run `make up` first.

- [ ] **Step 3: Write the migration**

`migrations/0003_preview_environments.up.sql`:

```sql
-- Preview environments: an ephemeral env is an ordinary environment with an
-- expiry, not a separate concept, so the scheduler, controller, router and
-- agent all carry it unchanged.
ALTER TABLE environments
    ADD COLUMN ephemeral  BOOLEAN NOT NULL DEFAULT false,
    ADD COLUMN expires_at TIMESTAMPTZ,
    -- An ephemeral env with no expiry is a leak the reaper can never see.
    -- Refuse to store one rather than detect it later.
    ADD CONSTRAINT environments_ephemeral_expiry
        CHECK (NOT ephemeral OR expires_at IS NOT NULL);

CREATE INDEX environments_expiry_idx
    ON environments (expires_at) WHERE ephemeral;

-- Deliberately outlives the environment row it describes. Deleting an env
-- cascades its deployments, instances, volumes and secrets, which is how the
-- agent GCs swappable containers -- but pinned containers are never GC'd from
-- an absent row, because a control-plane outage returning an empty
-- desired-state would then destroy production databases. The tombstone is the
-- explicit instruction that survives the delete.
CREATE TABLE environment_tombstones (
    env8       TEXT PRIMARY KEY,
    org_id     UUID NOT NULL REFERENCES organizations(id) ON DELETE CASCADE,
    created_at TIMESTAMPTZ NOT NULL DEFAULT now()
);

CREATE INDEX environment_tombstones_org_idx
    ON environment_tombstones (org_id, created_at DESC);
```

`migrations/0003_preview_environments.down.sql`:

```sql
DROP TABLE IF EXISTS environment_tombstones;
DROP INDEX IF EXISTS environments_expiry_idx;
ALTER TABLE environments
    DROP CONSTRAINT IF EXISTS environments_ephemeral_expiry,
    DROP COLUMN IF EXISTS expires_at,
    DROP COLUMN IF EXISTS ephemeral;
```

- [ ] **Step 4: Add the model fields**

In `internal/store/models.go`, `Environment` becomes:

```go
type Environment struct {
	ID               uuid.UUID         `json:"id"`
	StackID          uuid.UUID         `json:"stack_id"`
	Slug             string            `json:"slug"`
	Strategy         RolloutStrategy   `json:"strategy"`
	Hostname         string            `json:"hostname,omitempty"`
	Config           map[string]string `json:"config"`
	LiveDeploymentID *uuid.UUID        `json:"live_deployment_id,omitempty"`
	// Ephemeral marks a preview environment; the reaper deletes it once
	// ExpiresAt passes. Non-ephemeral environments never expire.
	Ephemeral bool       `json:"ephemeral"`
	ExpiresAt *time.Time `json:"expires_at,omitempty"`
	CreatedAt time.Time  `json:"created_at"`
}
```

- [ ] **Step 5: Select the new columns everywhere an Environment is read**

Three call sites must change together, or a preview read through one path looks non-ephemeral and is never reaped. In `internal/store/catalog.go` (`CreateEnvironment`, `ListEnvironments`) and `internal/store/stacks.go` (`GetEnvironment`), extend both the `RETURNING`/`SELECT` list and the `Scan` target list:

```go
// SELECT / RETURNING list becomes:
//   id, stack_id, slug, strategy, COALESCE(hostname,''),
//   config, live_deployment_id, ephemeral, expires_at, created_at

// Scan becomes:
&e.ID, &e.StackID, &e.Slug, &e.Strategy, &e.Hostname,
&config, &e.LiveDeploymentID, &e.Ephemeral, &e.ExpiresAt, &e.CreatedAt
```

- [ ] **Step 6: Apply the migration and run the test**

Run: `make migrate-up && go test ./internal/store/ -run TestEphemeralEnvironmentRequiresExpiry -v`
Expected: PASS. Confirm it did not SKIP.

- [ ] **Step 7: Run the full store suite for regressions**

Run: `go test ./internal/store/ -v 2>&1 | tail -30`
Expected: all PASS — the column additions touch every environment read path.

- [ ] **Step 8: Commit**

```bash
git add migrations/0003_preview_environments.up.sql migrations/0003_preview_environments.down.sql \
        internal/store/models.go internal/store/stacks.go internal/store/catalog.go \
        internal/store/previews_test.go
git commit -m "feat(store): ephemeral environments + tombstone table (0003)"
```

---

## Task 2: `CreatePreview` — env, secret copy, deployment in one transaction

**Files:**
- Create: `internal/store/previews.go`
- Modify: `internal/store/deployments.go:31-90` (extract `createDeploymentTx`), `internal/store/stacks.go` (add `GetStack`, `GetEnvironmentBySlug`)
- Test: `internal/store/previews_test.go`

**Interfaces:**
- Consumes: `Environment.Ephemeral`/`ExpiresAt` (Task 1); `spec.DeploymentSpec`.
- Produces:
  - `func (s *Store) CreatePreview(ctx context.Context, p CreatePreviewParams) (*Environment, *Deployment, error)`
  - `type CreatePreviewParams struct { StackID uuid.UUID; Slug, Hostname string; TTL time.Duration; InheritSecretsFrom *uuid.UUID; StackVersionID uuid.UUID; ResolvedSpec spec.DeploymentSpec; CreatedBy string }`
  - `func (s *Store) GetStack(ctx context.Context, id uuid.UUID) (*Stack, error)`
  - `func (s *Store) GetEnvironmentBySlug(ctx context.Context, stackID uuid.UUID, slug string) (*Environment, error)`

- [ ] **Step 1: Write the failing test**

Append to `internal/store/previews_test.go`:

```go
// Inheritance copies ciphertext only. This is safe because a secret is sealed
// to node age recipients, never to an environment, so a copied row is
// decryptable by exactly the same agents as the original -- and the control
// plane still never holds plaintext.
func TestCreatePreviewCopiesLatestSecretsOnly(t *testing.T) {
	st := testStore(t)
	ctx := testCtx(t)
	org := newOrg(t, st)
	app := newApp(t, st, org.ID)
	stack := newStack(t, st, app.ID)

	src, err := st.CreateEnvironment(ctx, CreateEnvironmentParams{StackID: stack.ID, Slug: "staging"})
	if err != nil {
		t.Fatalf("CreateEnvironment: %v", err)
	}
	if err := st.SetSecret(ctx, src.ID, "db_password", []byte("ct-v1"), "age1src"); err != nil {
		t.Fatalf("SetSecret v1: %v", err)
	}
	if err := st.SetSecret(ctx, src.ID, "db_password", []byte("ct-v2"), "age1src"); err != nil {
		t.Fatalf("SetSecret v2: %v", err)
	}
	if err := st.SetSecret(ctx, src.ID, "api_key", []byte("ct-key"), "age1src"); err != nil {
		t.Fatalf("SetSecret api_key: %v", err)
	}

	sv := newStackVersion(t, st, stack.ID)
	env, dep, err := st.CreatePreview(ctx, CreatePreviewParams{
		StackID: stack.ID, Slug: "pr-142", Hostname: "pr-142-x.preview.localhost",
		TTL: time.Hour, InheritSecretsFrom: &src.ID,
		StackVersionID: sv.ID, ResolvedSpec: sv.Spec,
	})
	if err != nil {
		t.Fatalf("CreatePreview: %v", err)
	}
	if !env.Ephemeral || env.ExpiresAt == nil {
		t.Fatal("preview must be ephemeral with an expiry")
	}
	if dep.Revision != 1 || dep.Slot != "blue" {
		t.Errorf("first deployment must be r1/blue, got r%d/%s", dep.Revision, dep.Slot)
	}

	keys, err := st.SecretKeysForEnv(ctx, env.ID)
	if err != nil {
		t.Fatalf("SecretKeysForEnv: %v", err)
	}
	if len(keys) != 2 {
		t.Fatalf("want 2 inherited keys, got %d: %+v", len(keys), keys)
	}
	for _, k := range keys {
		// Copies start a fresh history: version is per (environment_id, key)
		// and the new environment has none.
		if k.Version != 1 {
			t.Errorf("%s: copied secret must be version 1, got %d", k.Key, k.Version)
		}
	}

	var ct []byte
	if err := st.pool.QueryRow(ctx,
		`SELECT ciphertext FROM secrets WHERE environment_id=$1 AND key='db_password'`,
		env.ID).Scan(&ct); err != nil {
		t.Fatalf("read copied ciphertext: %v", err)
	}
	if string(ct) != "ct-v2" {
		t.Errorf("must copy the latest version, got %q", ct)
	}
}

func TestCreatePreviewWithoutInheritanceHasNoSecrets(t *testing.T) {
	st := testStore(t)
	ctx := testCtx(t)
	org := newOrg(t, st)
	app := newApp(t, st, org.ID)
	stack := newStack(t, st, app.ID)
	sv := newStackVersion(t, st, stack.ID)

	env, _, err := st.CreatePreview(ctx, CreatePreviewParams{
		StackID: stack.ID, Slug: "pr-1", Hostname: "pr-1-x.preview.localhost",
		TTL: time.Hour, StackVersionID: sv.ID, ResolvedSpec: sv.Spec,
	})
	if err != nil {
		t.Fatalf("CreatePreview: %v", err)
	}
	keys, err := st.SecretKeysForEnv(ctx, env.ID)
	if err != nil {
		t.Fatalf("SecretKeysForEnv: %v", err)
	}
	if len(keys) != 0 {
		t.Errorf("want no secrets without inheritance, got %+v", keys)
	}
}
```

If `newStackVersion` does not already exist in `internal/store/store_test.go`, add it there beside `newStack`:

```go
// newStackVersion pushes a minimal one-service spec so deployment-creating
// tests have something valid to resolve.
func newStackVersion(t *testing.T, st *Store, stackID uuid.UUID) *StackVersion {
	t.Helper()
	ctx := testCtx(t)
	sv, err := st.CreateStackVersion(ctx, CreateStackVersionParams{
		StackID: stackID,
		Spec: spec.DeploymentSpec{
			Services: map[string]spec.Service{
				"web": {
					Name: "web", Image: "nginx:alpine", Swappable: true,
					Limits: spec.ResourceLimit{CPUMillis: 250, MemoryBytes: 256 << 20},
				},
			},
		},
		ComposeYAML: "services:\n  web:\n    image: nginx:alpine\n",
		CreatedBy:   "test",
	})
	if err != nil {
		t.Fatalf("CreateStackVersion: %v", err)
	}
	return sv
}
```

Check `CreateStackVersionParams`' actual field names in `internal/store/stacks.go` before writing this helper and match them exactly.

- [ ] **Step 2: Run the test to verify it fails**

Run: `go test ./internal/store/ -run TestCreatePreview -v`
Expected: FAIL — `st.CreatePreview` undefined.

- [ ] **Step 3: Extract the deployment insert so it can share a transaction**

In `internal/store/deployments.go`, move the body of `CreateDeployment`'s closure into a method that takes the transaction. `CreateDeployment` then becomes a thin wrapper, and behaviour is unchanged:

```go
func (s *Store) CreateDeployment(ctx context.Context, p CreateDeploymentParams) (*Deployment, error) {
	var d *Deployment
	err := s.tx(ctx, func(tx pgx.Tx) error {
		var err error
		d, err = s.createDeploymentTx(ctx, tx, p)
		return err
	})
	if err != nil {
		return nil, mapErr(err)
	}
	return d, nil
}

// createDeploymentTx holds the revision/slot allocation so a caller already
// inside a transaction -- CreatePreview -- can create an environment and its
// first deployment atomically. A preview that existed with a hostname but no
// deployment would be an env the reaper eventually collects and the user never
// sees work.
func (s *Store) createDeploymentTx(ctx context.Context, tx pgx.Tx, p CreateDeploymentParams) (*Deployment, error) {
	// ... exact body previously inside the closure, returning (&d, nil) ...
}
```

- [ ] **Step 4: Add the two lookups**

In `internal/store/stacks.go`:

```go
func (s *Store) GetStack(ctx context.Context, id uuid.UUID) (*Stack, error) {
	var st Stack
	err := s.pool.QueryRow(ctx, `
		SELECT id, app_id, slug, created_at FROM stacks WHERE id = $1
	`, id).Scan(&st.ID, &st.AppID, &st.Slug, &st.CreatedAt)
	if err != nil {
		return nil, mapErr(err)
	}
	return &st, nil
}

// GetEnvironmentBySlug resolves the secret-inheritance source, which callers
// name by slug ("staging") rather than by id.
func (s *Store) GetEnvironmentBySlug(ctx context.Context, stackID uuid.UUID, slug string) (*Environment, error) {
	var e Environment
	var config []byte
	err := s.pool.QueryRow(ctx, `
		SELECT id, stack_id, slug, strategy, COALESCE(hostname,''),
		       config, live_deployment_id, ephemeral, expires_at, created_at
		FROM environments WHERE stack_id = $1 AND slug = $2
	`, stackID, slug).Scan(&e.ID, &e.StackID, &e.Slug, &e.Strategy, &e.Hostname,
		&config, &e.LiveDeploymentID, &e.Ephemeral, &e.ExpiresAt, &e.CreatedAt)
	if err != nil {
		return nil, mapErr(err)
	}
	if err := json.Unmarshal(config, &e.Config); err != nil {
		return nil, err
	}
	return &e, nil
}
```

- [ ] **Step 5: Write `CreatePreview`**

Create `internal/store/previews.go`:

```go
package store

import (
	"context"
	"encoding/json"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"

	"github.com/craig/composectl/internal/spec"
)

type CreatePreviewParams struct {
	StackID  uuid.UUID
	Slug     string
	Hostname string
	TTL      time.Duration
	// InheritSecretsFrom copies that environment's latest ciphertext. Nil
	// means the preview starts with no secrets.
	InheritSecretsFrom *uuid.UUID
	StackVersionID     uuid.UUID
	ResolvedSpec       spec.DeploymentSpec
	CreatedBy          string
}

// CreatePreview creates an ephemeral environment, copies the source
// environment's secrets into it, and creates its first deployment -- all in
// one transaction, so a preview never exists in a half-built state.
func (s *Store) CreatePreview(ctx context.Context, p CreatePreviewParams) (*Environment, *Deployment, error) {
	if err := validateSlug("slug", p.Slug); err != nil {
		return nil, nil, err
	}

	var env Environment
	var dep *Deployment

	err := s.tx(ctx, func(tx pgx.Tx) error {
		var config []byte
		err := tx.QueryRow(ctx, `
			INSERT INTO environments (stack_id, slug, hostname, config, ephemeral, expires_at)
			VALUES ($1, $2, NULLIF($3,''), '{}'::jsonb, true, now() + $4::interval)
			RETURNING id, stack_id, slug, strategy, COALESCE(hostname,''),
			          config, live_deployment_id, ephemeral, expires_at, created_at
		`, p.StackID, p.Slug, p.Hostname, p.TTL.String()).
			Scan(&env.ID, &env.StackID, &env.Slug, &env.Strategy, &env.Hostname,
				&config, &env.LiveDeploymentID, &env.Ephemeral, &env.ExpiresAt, &env.CreatedAt)
		if err != nil {
			return err
		}
		if err := json.Unmarshal(config, &env.Config); err != nil {
			return err
		}

		// Ciphertext moves verbatim: it is sealed to node recipients, not to
		// an environment, so no re-encryption (and no plaintext) is involved.
		// Copies land at version 1 because version is per (environment, key)
		// and this environment has no history.
		if p.InheritSecretsFrom != nil {
			if _, err := tx.Exec(ctx, `
				INSERT INTO secrets (environment_id, key, ciphertext, key_id, version)
				SELECT $1, s.key, s.ciphertext, s.key_id, 1
				FROM secrets s
				WHERE s.environment_id = $2
				  AND s.version = (SELECT MAX(version) FROM secrets s2
				                    WHERE s2.environment_id = s.environment_id
				                      AND s2.key = s.key)
			`, env.ID, *p.InheritSecretsFrom); err != nil {
				return err
			}
		}

		dep, err = s.createDeploymentTx(ctx, tx, CreateDeploymentParams{
			EnvironmentID:  env.ID,
			StackVersionID: p.StackVersionID,
			ResolvedSpec:   p.ResolvedSpec,
			CreatedBy:      p.CreatedBy,
		})
		return err
	})
	if err != nil {
		return nil, nil, mapErr(err)
	}
	return &env, dep, nil
}
```

- [ ] **Step 6: Run the tests**

Run: `go test ./internal/store/ -run TestCreatePreview -v`
Expected: both PASS.

- [ ] **Step 7: Verify the extraction did not change deployment behaviour**

Run: `go test ./internal/store/ ./internal/rollout/ -v 2>&1 | tail -40`
Expected: all PASS — slot alternation and the one-active-deployment index are the things the refactor could break.

- [ ] **Step 8: Commit**

```bash
git add internal/store/previews.go internal/store/previews_test.go \
        internal/store/deployments.go internal/store/stacks.go internal/store/store_test.go
git commit -m "feat(store): CreatePreview — ephemeral env, inherited ciphertext, first deployment in one tx"
```

---

## Task 3: Expiry and tombstones in the store

**Files:**
- Modify: `internal/store/previews.go`
- Test: `internal/store/previews_test.go`

**Interfaces:**
- Consumes: `environment_tombstones` (Task 1).
- Produces:
  - `func (s *Store) ExpireEnvironments(ctx context.Context) ([]string, error)` — returns reaped `env8` values
  - `func (s *Store) TombstonesForNode(ctx context.Context, nodeID uuid.UUID, maxAge time.Duration) ([]string, error)`
  - `func (s *Store) SweepTombstones(ctx context.Context, maxAge time.Duration) error`

- [ ] **Step 1: Write the failing test**

Append to `internal/store/previews_test.go`:

```go
func TestExpireEnvironmentsReapsOnlyExpiredEphemerals(t *testing.T) {
	st := testStore(t)
	ctx := testCtx(t)
	org := newOrg(t, st)
	app := newApp(t, st, org.ID)
	stack := newStack(t, st, app.ID)
	sv := newStackVersion(t, st, stack.ID)

	prod, err := st.CreateEnvironment(ctx, CreateEnvironmentParams{StackID: stack.ID, Slug: "prod"})
	if err != nil {
		t.Fatalf("CreateEnvironment: %v", err)
	}
	fresh, _, err := st.CreatePreview(ctx, CreatePreviewParams{
		StackID: stack.ID, Slug: "pr-fresh", Hostname: "a.preview.localhost",
		TTL: time.Hour, StackVersionID: sv.ID, ResolvedSpec: sv.Spec,
	})
	if err != nil {
		t.Fatalf("CreatePreview fresh: %v", err)
	}
	stale, _, err := st.CreatePreview(ctx, CreatePreviewParams{
		StackID: stack.ID, Slug: "pr-stale", Hostname: "b.preview.localhost",
		TTL: time.Hour, StackVersionID: sv.ID, ResolvedSpec: sv.Spec,
	})
	if err != nil {
		t.Fatalf("CreatePreview stale: %v", err)
	}
	// Reach past the TTL rather than sleeping through it.
	if _, err := st.pool.Exec(ctx,
		`UPDATE environments SET expires_at = now() - interval '1 minute' WHERE id = $1`,
		stale.ID); err != nil {
		t.Fatalf("age the preview: %v", err)
	}

	reaped, err := st.ExpireEnvironments(ctx)
	if err != nil {
		t.Fatalf("ExpireEnvironments: %v", err)
	}
	if len(reaped) != 1 || reaped[0] != shortID(stale.ID) {
		t.Fatalf("want only the stale preview reaped, got %v", reaped)
	}

	if _, err := st.GetEnvironment(ctx, stale.ID); err == nil {
		t.Error("expired preview must be deleted")
	}
	if _, err := st.GetEnvironment(ctx, fresh.ID); err != nil {
		t.Errorf("unexpired preview must survive: %v", err)
	}
	if _, err := st.GetEnvironment(ctx, prod.ID); err != nil {
		t.Errorf("non-ephemeral environment must survive: %v", err)
	}

	// The deployment created with the preview must have gone with it.
	var deps int
	if err := st.pool.QueryRow(ctx,
		`SELECT count(*) FROM deployments WHERE environment_id = $1`, stale.ID).Scan(&deps); err != nil {
		t.Fatalf("count deployments: %v", err)
	}
	if deps != 0 {
		t.Errorf("deleting the env must cascade its deployments, %d left", deps)
	}
}

// The tombstone must outlive the row it describes -- it is the only thing that
// will ever tell an agent to destroy that environment's durable state.
func TestTombstoneSurvivesTheEnvironmentAndExpires(t *testing.T) {
	st := testStore(t)
	ctx := testCtx(t)
	org := newOrg(t, st)
	app := newApp(t, st, org.ID)
	stack := newStack(t, st, app.ID)
	sv := newStackVersion(t, st, stack.ID)
	node := newNode(t, st, org.ID)

	env, _, err := st.CreatePreview(ctx, CreatePreviewParams{
		StackID: stack.ID, Slug: "pr-tomb", Hostname: "c.preview.localhost",
		TTL: time.Hour, StackVersionID: sv.ID, ResolvedSpec: sv.Spec,
	})
	if err != nil {
		t.Fatalf("CreatePreview: %v", err)
	}
	if _, err := st.pool.Exec(ctx,
		`UPDATE environments SET expires_at = now() - interval '1 minute' WHERE id = $1`,
		env.ID); err != nil {
		t.Fatalf("age the preview: %v", err)
	}
	if _, err := st.ExpireEnvironments(ctx); err != nil {
		t.Fatalf("ExpireEnvironments: %v", err)
	}

	got, err := st.TombstonesForNode(ctx, node.ID, 24*time.Hour)
	if err != nil {
		t.Fatalf("TombstonesForNode: %v", err)
	}
	if !contains(got, shortID(env.ID)) {
		t.Fatalf("want tombstone %s for this org's node, got %v", shortID(env.ID), got)
	}

	// Past the retention window it must disappear, so dead rows do not
	// accumulate forever.
	older, err := st.TombstonesForNode(ctx, node.ID, time.Nanosecond)
	if err != nil {
		t.Fatalf("TombstonesForNode (narrow window): %v", err)
	}
	if contains(older, shortID(env.ID)) {
		t.Error("tombstone outside the retention window must not be returned")
	}
}

func contains(hay []string, needle string) bool {
	for _, h := range hay {
		if h == needle {
			return true
		}
	}
	return false
}
```

If `newNode` does not exist in `internal/store/store_test.go`, add it beside `newStack`, matching `RegisterNodeParams`' real fields:

```go
func newNode(t *testing.T, st *Store, orgID uuid.UUID) *Node {
	t.Helper()
	ctx := testCtx(t)
	n, err := st.RegisterNode(ctx, RegisterNodeParams{
		OrgID: orgID, Hostname: uniq("node"), AdvertiseAddr: "127.0.0.1",
		CPUMillis: 4000, MemoryBytes: 8 << 30,
	})
	if err != nil {
		t.Fatalf("RegisterNode: %v", err)
	}
	return n
}
```

- [ ] **Step 2: Run the test to verify it fails**

Run: `go test ./internal/store/ -run 'TestExpireEnvironments|TestTombstone' -v`
Expected: FAIL — `ExpireEnvironments` undefined.

- [ ] **Step 3: Implement expiry and tombstone reads**

Append to `internal/store/previews.go`:

```go
// ExpireEnvironments deletes every expired preview and returns the env8 of
// each. Deleting the environment cascades its deployments, instances, volumes
// and secrets, which is how the agent GCs the swappable containers; the
// tombstone written here is what later tells the agent to destroy the pinned
// container and named volumes too.
//
// The tombstone is written before the delete and in the same transaction: the
// instruction to destroy durable state must be durable before the state
// describing it is gone. If the transaction aborts the environment survives and
// is retried next tick, which is the safe direction to fail.
func (s *Store) ExpireEnvironments(ctx context.Context) ([]string, error) {
	var reaped []string
	err := s.tx(ctx, func(tx pgx.Tx) error {
		// SKIP LOCKED because Sprint 4 runs more than one control plane and two
		// reapers racing the same environment would double-tombstone.
		rows, err := tx.Query(ctx, `
			SELECT e.id, a.org_id
			FROM environments e
			JOIN stacks       s ON s.id = e.stack_id
			JOIN applications a ON a.id = s.app_id
			WHERE e.ephemeral AND e.expires_at < now()
			FOR UPDATE OF e SKIP LOCKED
		`)
		if err != nil {
			return err
		}
		type victim struct {
			id    uuid.UUID
			orgID uuid.UUID
		}
		var victims []victim
		for rows.Next() {
			var v victim
			if err := rows.Scan(&v.id, &v.orgID); err != nil {
				rows.Close()
				return err
			}
			victims = append(victims, v)
		}
		rows.Close()
		if err := rows.Err(); err != nil {
			return err
		}

		for _, v := range victims {
			env8 := shortID(v.id)
			if _, err := tx.Exec(ctx, `
				INSERT INTO environment_tombstones (env8, org_id) VALUES ($1, $2)
				ON CONFLICT (env8) DO NOTHING
			`, env8, v.orgID); err != nil {
				return err
			}
			// environments.live_deployment_id is deferrable but still checked at
			// commit, so clear it before the cascade removes the deployment it
			// points at.
			if _, err := tx.Exec(ctx,
				`UPDATE environments SET live_deployment_id = NULL WHERE id = $1`, v.id); err != nil {
				return err
			}
			if _, err := tx.Exec(ctx, `DELETE FROM environments WHERE id = $1`, v.id); err != nil {
				return err
			}
			reaped = append(reaped, env8)
		}
		return nil
	})
	if err != nil {
		return nil, mapErr(err)
	}
	return reaped, nil
}

// TombstonesForNode returns the environments this node should destroy: recent
// tombstones in the node's own org. Nodes are org-scoped, so a node must never
// be handed a teardown for an environment it could not have been running.
func (s *Store) TombstonesForNode(ctx context.Context, nodeID uuid.UUID, maxAge time.Duration) ([]string, error) {
	rows, err := s.pool.Query(ctx, `
		SELECT t.env8
		FROM environment_tombstones t
		JOIN nodes n ON n.org_id = t.org_id
		WHERE n.id = $1 AND t.created_at > now() - $2::interval
		ORDER BY t.created_at DESC
	`, nodeID, maxAge.String())
	if err != nil {
		return nil, mapErr(err)
	}
	defer rows.Close()
	out := []string{}
	for rows.Next() {
		var env8 string
		if err := rows.Scan(&env8); err != nil {
			return nil, err
		}
		out = append(out, env8)
	}
	return out, rows.Err()
}

// SweepTombstones drops instructions no agent will act on any more. Past this
// window an offline node's containers and volumes leak and need manual removal
// -- the window is how long a node may be down and still clean up after itself.
func (s *Store) SweepTombstones(ctx context.Context, maxAge time.Duration) error {
	_, err := s.pool.Exec(ctx,
		`DELETE FROM environment_tombstones WHERE created_at < now() - $1::interval`,
		maxAge.String())
	return mapErr(err)
}
```

- [ ] **Step 4: Run the tests**

Run: `go test ./internal/store/ -run 'TestExpireEnvironments|TestTombstone' -v`
Expected: both PASS, no SKIP.

- [ ] **Step 5: Commit**

```bash
git add internal/store/previews.go internal/store/previews_test.go internal/store/store_test.go
git commit -m "feat(store): expire ephemeral envs, write tombstones that outlive them"
```

---

## Task 4: The reaper loop

**Files:**
- Create: `internal/rollout/reaper.go`, `internal/rollout/reaper_test.go`
- Modify: `cmd/controlplane/main.go:119-122`

**Interfaces:**
- Consumes: `store.ExpireEnvironments`, `store.SweepTombstones` (Task 3).
- Produces: `func NewReaper(st *store.Store, log *slog.Logger) *Reaper`, `func (r *Reaper) ReapOnce(ctx context.Context) error`, `const TombstoneRetention = 24 * time.Hour`.

- [ ] **Step 1: Write the failing test**

Create `internal/rollout/reaper_test.go`. Match the existing fixture helpers in `internal/rollout/rollout_test.go` — read it first and reuse its store/org/stack setup rather than inventing new ones.

```go
package rollout

import (
	"testing"
	"time"
)

func TestReapOnceDeletesExpiredPreviewAndTombstonesIt(t *testing.T) {
	st := testStore(t)
	ctx := testCtx(t)
	env := newExpiredPreview(t, st) // helper below

	r := NewReaper(st, slogDiscard())
	if err := r.ReapOnce(ctx); err != nil {
		t.Fatalf("ReapOnce: %v", err)
	}

	if _, err := st.GetEnvironment(ctx, env.ID); err == nil {
		t.Error("expired preview must be gone after a reap tick")
	}
}

func TestReapOnceLeavesUnexpiredPreviewAlone(t *testing.T) {
	st := testStore(t)
	ctx := testCtx(t)
	env := newLivePreview(t, st, time.Hour) // helper below

	r := NewReaper(st, slogDiscard())
	if err := r.ReapOnce(ctx); err != nil {
		t.Fatalf("ReapOnce: %v", err)
	}
	if _, err := st.GetEnvironment(ctx, env.ID); err != nil {
		t.Errorf("unexpired preview must survive a reap tick: %v", err)
	}
}
```

Write `newExpiredPreview` and `newLivePreview` in this file using `st.CreatePreview` plus the same `UPDATE environments SET expires_at = now() - interval '1 minute'` trick from Task 3, adapted to whatever fixture helpers `rollout_test.go` already provides. If `slogDiscard` is not already defined in the package's tests, add `func slogDiscard() *slog.Logger { return slog.New(slog.NewTextHandler(io.Discard, nil)) }`.

- [ ] **Step 2: Run the test to verify it fails**

Run: `go test ./internal/rollout/ -run TestReapOnce -v`
Expected: FAIL — `NewReaper` undefined.

- [ ] **Step 3: Implement the reaper**

Create `internal/rollout/reaper.go`:

```go
package rollout

import (
	"context"
	"log/slog"
	"time"

	"github.com/craig/composectl/internal/store"
)

// TombstoneRetention is how long a teardown instruction is offered to agents.
// It doubles as how long a node may be offline and still clean up after itself:
// past it, that environment's pinned containers and named volumes leak and need
// manual removal.
const TombstoneRetention = 24 * time.Hour

// Reaper is the third control-plane loop. The scheduler places deployments and
// the controller drives them to live; the reaper is what ends them.
type Reaper struct {
	st  *store.Store
	log *slog.Logger
}

func NewReaper(st *store.Store, log *slog.Logger) *Reaper {
	return &Reaper{st: st, log: log}
}

// ReapOnce deletes expired preview environments and drops teardown instructions
// no agent will act on any more.
func (r *Reaper) ReapOnce(ctx context.Context) error {
	reaped, err := r.st.ExpireEnvironments(ctx)
	if err != nil {
		return err
	}
	for _, env8 := range reaped {
		r.log.Info("preview environment expired", "env", env8)
	}
	return r.st.SweepTombstones(ctx, TombstoneRetention)
}
```

- [ ] **Step 4: Run the tests**

Run: `go test ./internal/rollout/ -run TestReapOnce -v`
Expected: PASS, no SKIP.

- [ ] **Step 5: Wire the loop**

In `cmd/controlplane/main.go`, beside the existing scheduler and controller loops:

```go
sched := rollout.NewScheduler(st, log)
ctrl := rollout.NewController(st, log, rtr)
reaper := rollout.NewReaper(st, log)
go runLoop(ctx, cfg.TickInterval, log, "scheduler", sched.ScheduleOnce)
go runLoop(ctx, cfg.TickInterval, log, "controller", ctrl.ReconcileOnce)
go runLoop(ctx, cfg.TickInterval, log, "reaper", reaper.ReapOnce)
```

- [ ] **Step 6: Verify the build and the boundary**

Run: `go build ./... && go list -deps ./cmd/controlplane | grep docker/docker; echo "exit=$?"`
Expected: build succeeds; the grep prints nothing and reports `exit=1` (no match).

- [ ] **Step 7: Commit**

```bash
git add internal/rollout/reaper.go internal/rollout/reaper_test.go cmd/controlplane/main.go
git commit -m "feat(rollout): reaper loop expires previews and sweeps tombstones"
```

---

## Task 5: `POST /v1/stacks/{stack}/previews`

**Files:**
- Create: `internal/api/previews.go`, `internal/api/previews_test.go`
- Modify: `internal/api/server.go:16-26` (options + route), `internal/config/config.go`, `cmd/controlplane/main.go`

**Interfaces:**
- Consumes: `store.CreatePreview`, `store.GetStack`, `store.GetEnvironmentBySlug` (Task 2); `applyEnvConfig` and `resolved.RequiredSecrets()` (existing, see `internal/api/deployments.go:72-98`).
- Produces: `func WithPreviewDomain(domain string) ServerOption`, `Config.PreviewDomain`, route `POST /v1/stacks/{stack}/previews`.

**Note for the implementer:** the preview's `config` overlay is deliberately empty — only *secrets* are inherited, not the source environment's non-secret config. If that turns out to be wrong in use, it is a one-line change to the `INSERT` in `CreatePreview`, but do not make it as part of this task.

- [ ] **Step 1: Write the failing test**

Create `internal/api/previews_test.go`:

```go
package api

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

func TestPreviewHostnameGeneration(t *testing.T) {
	got := previewHostname("pr-142", "hello", "preview.localhost")
	if got != "pr-142-hello.preview.localhost" {
		t.Errorf("got %q", got)
	}
}

// A DNS label over 63 characters is silently truncated by resolvers, which
// would route the preview nowhere. Reject at creation instead of failing at
// the first request.
func TestPreviewHostnameLabelTooLong(t *testing.T) {
	long := strings.Repeat("a", 60)
	if err := validatePreviewLabel(long, "hello"); err == nil {
		t.Fatal("a >63 character label must be rejected")
	}
	if err := validatePreviewLabel("pr-1", "hello"); err != nil {
		t.Errorf("a short label must be accepted: %v", err)
	}
}

func TestCreatePreviewRejectsExcessiveTTL(t *testing.T) {
	srv := testServer(t)
	stackID := newTestStack(t, srv) // helper below

	body, _ := json.Marshal(map[string]any{
		"slug": "pr-9", "ttl_hours": 1000,
	})
	rec := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodPost, "/v1/stacks/"+stackID+"/previews",
		strings.NewReader(string(body)))
	srv.ServeHTTP(rec, req)

	if rec.Code != http.StatusBadRequest {
		t.Fatalf("want 400 for a TTL above the cap, got %d: %s", rec.Code, rec.Body)
	}
}

func TestCreatePreviewUnknownInheritSourceIs404(t *testing.T) {
	srv := testServer(t)
	stackID := newTestStack(t, srv)

	body, _ := json.Marshal(map[string]any{
		"slug": "pr-10", "inherit_secrets_from": "nope",
	})
	rec := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodPost, "/v1/stacks/"+stackID+"/previews",
		strings.NewReader(string(body)))
	srv.ServeHTTP(rec, req)

	if rec.Code != http.StatusNotFound {
		t.Fatalf("want 404 for an unknown inherit source, got %d: %s", rec.Code, rec.Body)
	}
}
```

Write `newTestStack(t, srv) string` in this file: create an org, application, stack and one stack version through `srv`'s store (reachable as `srv.st` from inside the package), returning the stack UUID as a string. Follow the cleanup discipline in `internal/store/store_test.go:51` — delete bottom-up (instances → deployments → nodes → org) because `service_instances.node_id` and `deployments.stack_version_id` lack `ON DELETE CASCADE`.

- [ ] **Step 2: Run the test to verify it fails**

Run: `go test ./internal/api/ -run TestPreview -v`
Expected: FAIL — `previewHostname` undefined.

- [ ] **Step 3: Add the server option and config**

In `internal/api/server.go`:

```go
type Server struct {
	st            *store.Store
	log           *slog.Logger
	mux           *http.ServeMux
	previewDomain string
}

// ServerOption keeps NewServer's existing two-argument form working; only the
// control plane binary passes options, so tests and callers stay unchanged.
type ServerOption func(*Server)

// WithPreviewDomain sets the wildcard domain preview hostnames are generated
// under. Preview hostnames are always generated, never client-supplied, so a
// caller cannot claim another environment's hostname.
func WithPreviewDomain(domain string) ServerOption {
	return func(s *Server) { s.previewDomain = domain }
}

func NewServer(st *store.Store, log *slog.Logger, opts ...ServerOption) *Server {
	s := &Server{st: st, log: log, mux: http.NewServeMux(), previewDomain: DefaultPreviewDomain}
	for _, o := range opts {
		o(s)
	}
	s.routes()
	return s
}
```

Add the route in `routes()`, next to the other environment routes:

```go
s.mux.HandleFunc("POST /v1/stacks/{stack}/previews", s.handleCreatePreview)
```

In `internal/config/config.go`, add the field and load it:

```go
// PreviewDomain is the wildcard domain preview hostnames are generated under
// (pr-142-hello.<domain>). The default resolves on a dev box without DNS
// because Traefik routes on the Host header alone.
PreviewDomain string
```

```go
PreviewDomain: envOr("COMPOSECTL_PREVIEW_DOMAIN", "preview.localhost"),
```

And in `cmd/controlplane/main.go`:

```go
srvHandler := api.NewServer(st, log, api.WithPreviewDomain(cfg.PreviewDomain))
```

- [ ] **Step 4: Write the handler**

Create `internal/api/previews.go`:

```go
package api

import (
	"fmt"
	"net/http"
	"time"

	"github.com/google/uuid"

	"github.com/craig/composectl/internal/store"
)

// DefaultPreviewDomain works on a dev box with no DNS at all: Traefik routes on
// the Host header, so `curl -H "Host: pr-1-hello.preview.localhost"` reaches it.
const DefaultPreviewDomain = "preview.localhost"

const (
	defaultPreviewTTLHours = 24
	maxPreviewTTLHours     = 168 // one week
	maxDNSLabel            = 63
)

type createPreviewRequest struct {
	Slug           string `json:"slug"`
	StackVersionID string `json:"stack_version_id,omitempty"`
	// InheritSecretsFrom names an environment by slug. Inheritance must be
	// explicit: a preview that silently picked up production secrets would be
	// a credential leak with a convenient API.
	InheritSecretsFrom string `json:"inherit_secrets_from,omitempty"`
	TTLHours           int    `json:"ttl_hours,omitempty"`
	CreatedBy          string `json:"created_by,omitempty"`
}

type createPreviewResponse struct {
	EnvironmentID uuid.UUID  `json:"environment_id"`
	Hostname      string     `json:"hostname"`
	DeploymentID  uuid.UUID  `json:"deployment_id"`
	ExpiresAt     *time.Time `json:"expires_at"`
}

func previewHostname(slug, stackSlug, domain string) string {
	return fmt.Sprintf("%s-%s.%s", slug, stackSlug, domain)
}

func validatePreviewLabel(slug, stackSlug string) error {
	if n := len(slug) + 1 + len(stackSlug); n > maxDNSLabel {
		return fmt.Errorf("generated hostname label is %d characters; DNS allows %d", n, maxDNSLabel)
	}
	return nil
}

// handleCreatePreview creates an ephemeral environment, inherits secrets, and
// deploys — one call, one URL back. The point of previews is that CI can create
// one without orchestrating three requests.
func (s *Server) handleCreatePreview(w http.ResponseWriter, r *http.Request) {
	ctx, cancel := contextWithTimeout(r, 15*time.Second)
	defer cancel()

	stackID, ok := pathUUID(w, r, "stack")
	if !ok {
		return
	}
	var req createPreviewRequest
	if err := decodeJSON(r, &req); err != nil {
		writeError(w, http.StatusBadRequest, "invalid request body", nil)
		return
	}

	ttlHours := req.TTLHours
	if ttlHours == 0 {
		ttlHours = defaultPreviewTTLHours
	}
	// Reject rather than clamp: silently storing a different TTL than the one
	// asked for makes the API lie about what it did.
	if ttlHours < 0 || ttlHours > maxPreviewTTLHours {
		writeError(w, http.StatusBadRequest,
			fmt.Sprintf("ttl_hours must be between 1 and %d", maxPreviewTTLHours), nil)
		return
	}

	stack, err := s.st.GetStack(ctx, stackID)
	if err != nil {
		s.writeStoreError(w, err)
		return
	}
	if err := validatePreviewLabel(req.Slug, stack.Slug); err != nil {
		writeError(w, http.StatusBadRequest, err.Error(), nil)
		return
	}

	// Resolve which stack version to deploy — same rule as handleCreateDeployment.
	var svID uuid.UUID
	if req.StackVersionID != "" {
		if svID, err = uuid.Parse(req.StackVersionID); err != nil {
			writeError(w, http.StatusBadRequest, "invalid stack_version_id", nil)
			return
		}
	} else {
		latest, err := s.st.LatestStackVersion(ctx, stackID)
		if err != nil {
			s.writeStoreError(w, err)
			return
		}
		svID = latest.ID
	}
	sv, err := s.st.GetStackVersion(ctx, svID)
	if err != nil {
		s.writeStoreError(w, err)
		return
	}

	var srcID *uuid.UUID
	var srcKeys []store.SecretMeta
	if req.InheritSecretsFrom != "" {
		src, err := s.st.GetEnvironmentBySlug(ctx, stackID, req.InheritSecretsFrom)
		if err != nil {
			s.writeStoreError(w, err)
			return
		}
		srcID = &src.ID
		if srcKeys, err = s.st.SecretKeysForEnv(ctx, src.ID); err != nil {
			s.writeStoreError(w, err)
			return
		}
	}

	// A preview starts with an empty config overlay: only secrets are inherited.
	resolved := applyEnvConfig(sv.Spec, nil)

	// Fail fast against the *source* environment's keys, before anything is
	// created. Checking after creation would leave a half-built preview behind
	// for the reaper to collect and the user to wonder about.
	if required := resolved.RequiredSecrets(); len(required) > 0 {
		set := map[string]bool{}
		for _, m := range srcKeys {
			set[m.Key] = true
		}
		var missing []string
		for _, k := range required {
			if !set[k] {
				missing = append(missing, k)
			}
		}
		if len(missing) > 0 {
			writeJSON(w, http.StatusUnprocessableEntity, errorBody{
				Error: "preview would be missing required secrets", Details: missing})
			return
		}
	}

	hostname := previewHostname(req.Slug, stack.Slug, s.previewDomain)
	env, dep, err := s.st.CreatePreview(ctx, store.CreatePreviewParams{
		StackID: stackID, Slug: req.Slug, Hostname: hostname,
		TTL: time.Duration(ttlHours) * time.Hour, InheritSecretsFrom: srcID,
		StackVersionID: svID, ResolvedSpec: resolved, CreatedBy: req.CreatedBy,
	})
	if err != nil {
		s.writeStoreError(w, err)
		return
	}

	writeJSON(w, http.StatusCreated, createPreviewResponse{
		EnvironmentID: env.ID, Hostname: env.Hostname,
		DeploymentID: dep.ID, ExpiresAt: env.ExpiresAt,
	})
}
```

Check `errorBody`'s real field names in `internal/api/server.go` (or wherever `writeError` lives) and match them; `internal/api/deployments.go:94-96` shows the existing usage.

- [ ] **Step 5: Run the tests**

Run: `go test ./internal/api/ -run TestPreview -v`
Expected: all PASS, no SKIP.

- [ ] **Step 6: Verify nothing else broke**

Run: `go build ./... && go test ./internal/api/ ./internal/config/ -v 2>&1 | tail -25`
Expected: PASS.

- [ ] **Step 7: Commit**

```bash
git add internal/api/previews.go internal/api/previews_test.go internal/api/server.go \
        internal/config/config.go cmd/controlplane/main.go
git commit -m "feat(api): POST /previews — ephemeral env, inherited secrets, generated hostname"
```

---

## Task 6: `teardown_envs` in desired-state

**Files:**
- Modify: `internal/api/nodes.go:87-105`
- Test: `internal/api/previews_test.go`

**Interfaces:**
- Consumes: `store.TombstonesForNode` (Task 3), `rollout.TombstoneRetention` (Task 4).
- Produces: desired-state response field `teardown_envs []string`.

- [ ] **Step 1: Write the failing test**

Append to `internal/api/previews_test.go`:

```go
func TestDesiredStateIncludesTeardownEnvs(t *testing.T) {
	srv := testServer(t)
	nodeID, env8 := newNodeWithReapedPreview(t, srv) // helper below

	rec := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodGet, "/v1/nodes/"+nodeID+"/desired-state", nil)
	srv.ServeHTTP(rec, req)

	if rec.Code != http.StatusOK {
		t.Fatalf("want 200, got %d: %s", rec.Code, rec.Body)
	}
	var resp struct {
		TeardownEnvs []string `json:"teardown_envs"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &resp); err != nil {
		t.Fatalf("decode: %v", err)
	}
	found := false
	for _, e := range resp.TeardownEnvs {
		if e == env8 {
			found = true
		}
	}
	if !found {
		t.Errorf("want %s in teardown_envs, got %v", env8, resp.TeardownEnvs)
	}
}
```

Write `newNodeWithReapedPreview(t, srv) (nodeID, env8 string)`: register a node in a fresh org, create a preview in that org's stack, age it past its expiry with a direct `UPDATE`, call `srv.st.ExpireEnvironments`, and return the node id and the reaped `env8`.

- [ ] **Step 2: Run the test to verify it fails**

Run: `go test ./internal/api/ -run TestDesiredStateIncludesTeardownEnvs -v`
Expected: FAIL — `teardown_envs` is absent, so `found` stays false.

- [ ] **Step 3: Extend the handler**

In `internal/api/nodes.go`, `handleDesiredState` gains a third read and one response field:

```go
	// Environments this node must destroy outright. Explicit intent, never
	// inferred from an instance row's absence: an empty desired-state must
	// never be read as "delete the database".
	teardown, err := s.st.TombstonesForNode(ctx, id, rollout.TombstoneRetention)
	if err != nil {
		s.writeStoreError(w, err)
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{
		"instances": desired, "secrets": secretsByEnv, "teardown_envs": teardown,
	})
```

If importing `internal/rollout` from `internal/api` would create an import cycle, declare the constant in `internal/api` instead of importing it — check with `go build ./...` and keep the two values equal, noting the pairing in a comment.

- [ ] **Step 4: Run the test**

Run: `go test ./internal/api/ -run TestDesiredStateIncludesTeardownEnvs -v`
Expected: PASS, no SKIP.

- [ ] **Step 5: Commit**

```bash
git add internal/api/nodes.go internal/api/previews_test.go
git commit -m "feat(api): desired-state carries teardown_envs"
```

---

## Task 7: Agent acts on teardown instructions

**Files:**
- Modify: `internal/agent/reconcile.go:18-83`, `internal/agent/agent.go:92-129`
- Test: `internal/agent/reconcile_test.go`

**Interfaces:**
- Consumes: `teardown_envs` (Task 6).
- Produces:
  - `DockerDriver` gains `EnsureVolume(ctx context.Context, name string, labels map[string]string) error` and `RemoveEnv(ctx context.Context, env8 string) error`
  - `Reconcile(ctx context.Context, desired []store.DesiredInstance, secrets map[string]dockerd.SecretSource, teardownEnvs []string) []Report`

- [ ] **Step 1: Write the failing test**

Append to `internal/agent/reconcile_test.go` (and add `removedEnvs []string` plus the two new methods to `fakeDriver`):

```go
func (f *fakeDriver) EnsureVolume(ctx context.Context, name string, l map[string]string) error {
	f.volumes = append(f.volumes, name)
	return nil
}
func (f *fakeDriver) RemoveEnv(ctx context.Context, env8 string) error {
	f.removedEnvs = append(f.removedEnvs, env8)
	return nil
}

func TestReconcileTearsDownTombstonedEnv(t *testing.T) {
	f := &fakeDriver{}
	r := NewReconciler(f)

	r.Reconcile(context.Background(), nil, nil, []string{"deadbeef"})

	if len(f.removedEnvs) != 1 || f.removedEnvs[0] != "deadbeef" {
		t.Fatalf("want RemoveEnv(deadbeef), got %v", f.removedEnvs)
	}
}

// The invariant this whole slice comes closest to breaking. An empty
// desired-state means "I have nothing to tell you", not "destroy everything":
// a control-plane outage, a failed migration, or an auth error can all produce
// one, and none of them should drop a production database.
func TestReconcileEmptyDesiredStateNeverRemovesAnEnv(t *testing.T) {
	f := &fakeDriver{
		managed: []dockerd.Managed{
			{ID: "id-pinned", Name: "cc-env12345-pinned-db", Service: "db", Swappable: false},
		},
	}
	r := NewReconciler(f)

	r.Reconcile(context.Background(), nil, nil, nil)

	if len(f.removedEnvs) != 0 {
		t.Fatalf("empty desired-state must never trigger RemoveEnv, got %v", f.removedEnvs)
	}
	if len(f.removed) != 0 {
		t.Fatalf("empty desired-state must not stop any container, got %v", f.removed)
	}
}
```

Add `volumes []string` and `removedEnvs []string` to the `fakeDriver` struct at `internal/agent/reconcile_test.go:16`.

- [ ] **Step 2: Run the test to verify it fails**

Run: `go test ./internal/agent/ -run TestReconcile -v`
Expected: FAIL — `Reconcile` takes three arguments, not four.

- [ ] **Step 3: Extend the interface and Reconcile**

In `internal/agent/reconcile.go`, add to `DockerDriver`:

```go
	// EnsureVolume creates a named volume with labels. Docker would create it
	// implicitly on first mount, but implicit volumes carry no labels, and
	// teardown must match volumes exactly — a volume is the one object here
	// whose deletion cannot be undone.
	EnsureVolume(ctx context.Context, name string, labels map[string]string) error
	// RemoveEnv destroys everything belonging to an environment: containers
	// (including pinned), networks and named volumes. This is the ONLY path
	// that removes pinned containers or volumes, and it fires only on an
	// explicit tombstone.
	RemoveEnv(ctx context.Context, env8 string) error
```

Change the signature and add the teardown pass at the end of `Reconcile`:

```go
func (r *Reconciler) Reconcile(ctx context.Context, desired []store.DesiredInstance, secrets map[string]dockerd.SecretSource, teardownEnvs []string) []Report {
```

```go
	// Teardown runs last: an environment being destroyed may also appear in
	// desired (a tick straddling the reap), and destroying it after converging
	// costs one wasted create rather than leaving a half-removed env behind.
	for _, env8 := range teardownEnvs {
		if err := r.drv.RemoveEnv(ctx, env8); err != nil {
			// Logged by the caller; a failed teardown is retried while the
			// tombstone is still inside its retention window.
			continue
		}
	}
	return reports
```

In `ensure`, create the named volumes before the container so they carry labels:

```go
	cs := containerSpec(di, name)
	for _, m := range cs.Mounts {
		if err := r.drv.EnsureVolume(ctx, m.Volume, map[string]string{"cc.env": di.Env8}); err != nil {
			return fail(err)
		}
	}
	id, _, err := r.drv.EnsureContainer(ctx, cs, secrets)
```

- [ ] **Step 4: Pass the field through the agent client**

In `internal/agent/agent.go`, `reconcileTick`:

```go
	var desired struct {
		Instances    []store.DesiredInstance            `json:"instances"`
		Secrets      map[string][]store.EncryptedSecret `json:"secrets"`
		TeardownEnvs []string                           `json:"teardown_envs"`
	}
```

```go
	reports := rec.Reconcile(ctx, desired.Instances, sources, desired.TeardownEnvs)
```

- [ ] **Step 5: Run the tests**

Run: `go test ./internal/agent/ -v`
Expected: all PASS. This package is pure — it must not SKIP.

- [ ] **Step 6: Commit**

```bash
git add internal/agent/reconcile.go internal/agent/agent.go internal/agent/reconcile_test.go
git commit -m "feat(agent): destroy an environment on explicit tombstone, never on empty desired-state"
```

---

## Task 8: Docker driver — labelled volumes and environment teardown

**Files:**
- Modify: `internal/agent/dockerd/driver.go`
- Test: `internal/agent/dockerd/driver_test.go`

**Interfaces:**
- Consumes: the `DockerDriver` interface (Task 7).
- Produces: `func (d *Driver) EnsureVolume(ctx context.Context, name string, labels map[string]string) error`, `func (d *Driver) RemoveEnv(ctx context.Context, env8 string) error`.

- [ ] **Step 1: Write the failing test**

Append to `internal/agent/dockerd/driver_test.go`, following that file's existing skip-when-no-daemon helper:

```go
func TestEnsureVolumeAndRemoveEnv(t *testing.T) {
	d := testDriver(t) // existing helper: skips loudly without a daemon
	ctx := context.Background()
	env8 := "test" + uuid.NewString()[:4]
	vol := "cc-" + env8 + "-data"

	if err := d.EnsureVolume(ctx, vol, map[string]string{"cc.env": env8}); err != nil {
		t.Fatalf("EnsureVolume: %v", err)
	}
	// Idempotent: reconcile calls this on every tick.
	if err := d.EnsureVolume(ctx, vol, map[string]string{"cc.env": env8}); err != nil {
		t.Fatalf("EnsureVolume (second call): %v", err)
	}

	if err := d.RemoveEnv(ctx, env8); err != nil {
		t.Fatalf("RemoveEnv: %v", err)
	}
	vols, err := d.cli.VolumeList(ctx, volume.ListOptions{
		Filters: filters.NewArgs(filters.Arg("label", "cc.env="+env8)),
	})
	if err != nil {
		t.Fatalf("VolumeList: %v", err)
	}
	if len(vols.Volumes) != 0 {
		t.Errorf("RemoveEnv must delete the env's volumes, %d left", len(vols.Volumes))
	}

	// Idempotent: a tombstone is re-offered every tick for its whole retention
	// window, so removing an already-gone environment must not error.
	if err := d.RemoveEnv(ctx, env8); err != nil {
		t.Errorf("RemoveEnv must be idempotent: %v", err)
	}
}
```

- [ ] **Step 2: Run the test to verify it fails**

Run: `go test ./internal/agent/dockerd/ -run TestEnsureVolumeAndRemoveEnv -v`
Expected: FAIL — `EnsureVolume` undefined. If it SKIPs, start Docker; a skip proves nothing here.

- [ ] **Step 3: Implement both methods**

In `internal/agent/dockerd/driver.go`:

```go
// EnsureVolume creates a named volume with labels, ignoring "already exists".
// Docker creates named volumes implicitly when a container mounts them, but
// implicit volumes carry no labels — and teardown matches on the label,
// because a volume is the one object in this system whose deletion cannot be
// undone and a name-substring filter is not an exact match.
func (d *Driver) EnsureVolume(ctx context.Context, name string, labels map[string]string) error {
	_, err := d.cli.VolumeCreate(ctx, volume.CreateOptions{Name: name, Labels: labels})
	return err
}

// RemoveEnv destroys everything belonging to one environment: containers
// (including pinned ones), the revision networks, and the named volumes. It is
// the only path that removes durable state, and the reconciler calls it only
// for an environment the control plane has explicitly tombstoned.
//
// Order matters: a network in use cannot be removed while a container is
// attached, and a volume in use cannot be removed at all.
func (d *Driver) RemoveEnv(ctx context.Context, env8 string) error {
	f := filters.NewArgs(filters.Arg("label", "cc.env="+env8))

	containers, err := d.cli.ContainerList(ctx, container.ListOptions{All: true, Filters: f})
	if err != nil {
		return err
	}
	for _, c := range containers {
		if err := d.StopRemove(ctx, c.ID); err != nil {
			return fmt.Errorf("remove container %s: %w", c.ID, err)
		}
	}

	nets, err := d.cli.NetworkList(ctx, network.ListOptions{Filters: f})
	if err != nil {
		return err
	}
	for _, n := range nets {
		if err := d.cli.NetworkRemove(ctx, n.ID); err != nil && !errdefs.IsNotFound(err) {
			return fmt.Errorf("remove network %s: %w", n.Name, err)
		}
	}

	vols, err := d.cli.VolumeList(ctx, volume.ListOptions{Filters: f})
	if err != nil {
		return err
	}
	for _, v := range vols.Volumes {
		if err := d.cli.VolumeRemove(ctx, v.Name, false); err != nil && !errdefs.IsNotFound(err) {
			return fmt.Errorf("remove volume %s: %w", v.Name, err)
		}
	}
	return nil
}
```

Match the SDK import paths and the not-found helper already used in this file (see `removeNetwork` at `internal/agent/dockerd/driver.go:104`) rather than assuming `errdefs.IsNotFound` — the Docker SDK has moved this symbol between versions.

- [ ] **Step 4: Run the test**

Run: `go test ./internal/agent/dockerd/ -run TestEnsureVolumeAndRemoveEnv -v`
Expected: PASS. Confirm no `--- SKIP`.

- [ ] **Step 5: Full build and boundary check**

Run: `go build ./... && go vet ./... && go list -deps ./cmd/controlplane | grep docker/docker; echo "exit=$?"`
Expected: clean build and vet; grep prints nothing, `exit=1`.

- [ ] **Step 6: Commit**

```bash
git add internal/agent/dockerd/driver.go internal/agent/dockerd/driver_test.go
git commit -m "feat(dockerd): labelled volumes + RemoveEnv teardown"
```

---

## Task 9: End-to-end demo and documentation

**Files:**
- Create: `scripts/demo-preview.sh`
- Modify: `Makefile`, `CLAUDE.md`

**Interfaces:**
- Consumes: everything above.
- Produces: `make demo-preview`.

- [ ] **Step 1: Write the demo script**

Create `scripts/demo-preview.sh`, modelled on `scripts/demo-secrets.sh` (read it first and reuse its API base, `jq` usage, and polling helper):

```bash
#!/usr/bin/env bash
# Preview environments end to end: create one with inherited secrets, prove it
# serves traffic, then prove the platform destroys it — containers, pinned
# container and named volumes included. Step 5 is the point: a demo that stops
# at "the preview works" proves the easy half.
set -euo pipefail
```

The script must:

1. Create org/app/stack, push `examples/hello/compose.yaml` as a stack version.
2. Create a `staging` environment and set the secret the hello stack requires.
3. `POST /v1/stacks/{stack}/previews` with `inherit_secrets_from=staging` and `ttl_hours=1`; capture `environment_id`, `hostname`, `deployment_id`.
4. Poll `GET /v1/deployments/{id}` until `state == "live"`, failing after a bounded number of attempts.
5. `curl -H "Host: <hostname>" http://localhost:<traefik port>/` and assert the response contains the inherited secret value — this is what proves inheritance end to end, not just that rows were copied.
6. Force expiry with `docker compose exec -T postgres psql ... -c "UPDATE environments SET expires_at = now() - interval '1 minute' WHERE id = '<env>'"`, then poll until:
   - `GET /v1/stacks/{stack}/envs` no longer lists the preview,
   - `docker ps -a --filter label=cc.env=<env8>` is empty (including the pinned container),
   - `docker volume ls --filter label=cc.env=<env8>` is empty.
7. Exit non-zero with a clear message if any assertion fails.

Read the Traefik host port from `compose.yaml` rather than hardcoding it.

- [ ] **Step 2: Add the Makefile target**

Beside the other demo targets, matching their existing style:

```make
demo-preview: ## Create a preview env with inherited secrets, then watch it expire and get reaped
	@./scripts/demo-preview.sh
```

- [ ] **Step 3: Run it**

Run: `make up && make demo-preview`
Expected: exits 0, having asserted both that the preview served traffic and that every container and volume is gone after expiry.

- [ ] **Step 4: Verify no regressions in the other demos**

Run: `make demo && make demo-rollback && make demo-secrets`
Expected: all pass. These share the reconcile path that Task 7 changed, and `EnsureVolume` is new in the container-start path every one of them exercises.

- [ ] **Step 5: Update CLAUDE.md**

The sprint status is stale — it claims Sprint 2 Slices B and C are "next" when both, plus Sprint 3 secret injection, have landed. Update:

- **Sprint status:** mark Sprint 2 Slice B and C done, Sprint 3 Slice A (secrets) done, Sprint 3 Slice B (previews) done.
- **Invariants:** add the teardown rule — *reconcile converges to instance rows; pinned containers and volumes are destroyed only on an explicit tombstone in `teardown_envs`, never from an absent row. `TombstoneRetention` is 24h; past it an offline node's containers leak and need manual removal.*
- **Architecture:** note that named volumes are created explicitly with `cc.env` labels so teardown matches exactly, and that volumes predating this carry no label and will not be reaped.
- **Store methods:** add `CreatePreview`, `ExpireEnvironments`, `TombstonesForNode`, `SweepTombstones`, `GetStack`, `GetEnvironmentBySlug`.
- **Verification:** add `make demo-preview`.
- Remove the "only rollback is still 501" line — nothing is 501 now.

- [ ] **Step 6: Full verification**

Run: `go build ./... && go vet ./... && go test ./... 2>&1 | tail -30`
Expected: all packages PASS. **Check for `--- SKIP`** — a green run with Postgres or Docker down proves nothing.

Run: `go list -deps ./cmd/controlplane | grep docker/docker; echo "exit=$?"`
Expected: no output, `exit=1`.

- [ ] **Step 7: Commit**

```bash
git add scripts/demo-preview.sh Makefile CLAUDE.md
git commit -m "feat: preview environment demo + refresh sprint status"
```

---

## Self-Review Notes

Checked against the spec:

- Every spec section maps to a task: data model → 1, API + hostname + inheritance → 2/5, reaper + `SKIP LOCKED` + retention → 3/4, desired-state field → 6, agent teardown + volume labelling → 7/8, testing + demo → spread across all, with the demo in 9.
- Type consistency verified: `CreatePreviewParams` fields used in Task 5's handler match Task 2's definition; `TombstonesForNode(ctx, nodeID, maxAge)` matches its use in Task 6; the four-argument `Reconcile` in Task 7 matches both the fake driver and `agent.go`.
- Known deviation from the spec, deliberate: the spec's `ExpireEnvironments` sketch shows a bare `DELETE`; Task 3 clears `live_deployment_id` first, because that circular FK is deferrable but still checked at commit. `internal/store/store_test.go:60-67` does the same thing for the same reason.
