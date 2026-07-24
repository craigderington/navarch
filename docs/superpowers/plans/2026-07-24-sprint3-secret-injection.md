# Sprint 3 Slice A — Secret Injection Implementation Plan

> **For agentic workers:** execute task-by-task with TDD. Steps use checkbox (`- [ ]`) syntax.

**Goal:** An operator sets a per-environment secret via the API; it is stored as age ciphertext the control plane cannot decrypt; the agent decrypts it at container start with a private key only it holds — retiring `COMPOSECTL_DEV_SECRETS`.

**Builds on:** Sprint 2 (agent, rollout, routing). **Spec:** `docs/superpowers/specs/2026-07-24-sprint3-secret-injection-design.md`.

## Global Constraints

- go 1.25; ports never 3000/5000/8000/9000. Commit locally only; branch `sprint3-secrets`.
- **Boundaries:** `internal/secrets` is the sole importer of `filippo.io/age`; `internal/store` never imports age (stores opaque bytes); the control-plane binary must not link the Docker SDK (`go list -deps ./cmd/controlplane | grep docker/docker` empty). `internal/secrets` and the reconcile logic import neither pgx nor the Docker SDK.
- Migrations immutable: add `0002_*`, never edit `0001`.
- Secret key charset matches `spec.SecretRefPattern`: `[A-Za-z0-9_.-]+`.

## File Structure

**Create:** `internal/secrets/secrets.go` + `_test.go`; `migrations/0002_node_age_recipient.up.sql` + `.down.sql`; `internal/store/secrets.go` + test additions; `internal/api/secrets.go` + test additions; `examples/secret/compose.yaml`; `scripts/demo-secrets.sh`
**Modify:** `internal/store/nodes.go` + `models.go` (age_recipient); `internal/api/nodes.go` (register recipient; desired-state secrets); `internal/api/deployments.go` (fail-fast); `internal/api/server.go` (routes); `internal/agent/dockerd/driver.go` (per-call source); `internal/agent/reconcile.go` + `agent.go` + `config.go` (identity, decrypt, inject); `cmd/agent/main.go`; `compose.yaml`; `scripts/demo.sh`, `scripts/demo-rollback.sh` (set secrets first); `Makefile`

---

## Task 1: `internal/secrets` — the age boundary

**Files:** create `internal/secrets/secrets.go`, `internal/secrets/secrets_test.go`

**Interfaces:**
- `type Identity struct { id *age.X25519Identity }`
- `func GenerateIdentity() (Identity, error)`
- `func LoadOrGenerateIdentity(path string) (Identity, error)` — path "" = ephemeral
- `func (i Identity) Recipient() string`
- `func (i Identity) Decrypt(ciphertext []byte) (string, error)`
- `func Encrypt(plaintext string, recipients []string) ([]byte, error)`

- [ ] **Step 1 — failing test** `secrets_test.go`:

```go
package secrets

import (
	"os"
	"path/filepath"
	"testing"
)

func TestEncryptDecryptRoundTrip(t *testing.T) {
	id, err := GenerateIdentity()
	if err != nil {
		t.Fatalf("gen: %v", err)
	}
	ct, err := Encrypt("hunter2", []string{id.Recipient()})
	if err != nil {
		t.Fatalf("encrypt: %v", err)
	}
	if string(ct) == "hunter2" {
		t.Fatal("ciphertext must not be the plaintext")
	}
	got, err := id.Decrypt(ct)
	if err != nil {
		t.Fatalf("decrypt: %v", err)
	}
	if got != "hunter2" {
		t.Fatalf("round trip: got %q", got)
	}
}

func TestDecryptWithWrongIdentityFails(t *testing.T) {
	a, _ := GenerateIdentity()
	b, _ := GenerateIdentity()
	ct, _ := Encrypt("secret", []string{a.Recipient()})
	if _, err := b.Decrypt(ct); err == nil {
		t.Fatal("decrypt with the wrong identity must fail")
	}
}

func TestMultiRecipientAnyCanDecrypt(t *testing.T) {
	a, _ := GenerateIdentity()
	b, _ := GenerateIdentity()
	ct, _ := Encrypt("x", []string{a.Recipient(), b.Recipient()})
	for _, id := range []Identity{a, b} {
		if v, err := id.Decrypt(ct); err != nil || v != "x" {
			t.Fatalf("recipient could not decrypt: %v %q", err, v)
		}
	}
}

func TestLoadOrGeneratePersists(t *testing.T) {
	p := filepath.Join(t.TempDir(), "age.key")
	a, err := LoadOrGenerateIdentity(p)
	if err != nil {
		t.Fatalf("first: %v", err)
	}
	if _, err := os.Stat(p); err != nil {
		t.Fatalf("identity not written: %v", err)
	}
	b, err := LoadOrGenerateIdentity(p)
	if err != nil {
		t.Fatalf("second: %v", err)
	}
	if a.Recipient() != b.Recipient() {
		t.Fatal("reload must return the same identity")
	}
}
```

- [ ] **Step 2 — run, expect fail** (undefined): `go test ./internal/secrets/ -count=1`

- [ ] **Step 3 — implement** `secrets.go`:

```go
// Package secrets is the ONLY package that imports filippo.io/age. The control
// plane uses it to encrypt to a node's recipient; the agent uses it to decrypt
// with its identity. Everything else handles opaque ciphertext bytes.
package secrets

import (
	"bytes"
	"fmt"
	"io"
	"os"
	"path/filepath"

	"filippo.io/age"
)

type Identity struct{ id *age.X25519Identity }

func GenerateIdentity() (Identity, error) {
	id, err := age.GenerateX25519Identity()
	if err != nil {
		return Identity{}, err
	}
	return Identity{id: id}, nil
}

// LoadOrGenerateIdentity reads an age identity from path, generating and
// persisting one (0600) if the file is absent. path "" yields an ephemeral,
// unpersisted identity (tests). The identity MUST persist across agent restarts
// — a fresh one cannot decrypt secrets encrypted to the old recipient.
func LoadOrGenerateIdentity(path string) (Identity, error) {
	if path == "" {
		return GenerateIdentity()
	}
	if b, err := os.ReadFile(path); err == nil {
		id, err := age.ParseX25519Identity(string(bytes.TrimSpace(b)))
		if err != nil {
			return Identity{}, fmt.Errorf("parse identity %s: %w", path, err)
		}
		return Identity{id: id}, nil
	} else if !os.IsNotExist(err) {
		return Identity{}, err
	}
	id, err := age.GenerateX25519Identity()
	if err != nil {
		return Identity{}, err
	}
	if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
		return Identity{}, err
	}
	if err := os.WriteFile(path, []byte(id.String()+"\n"), 0o600); err != nil {
		return Identity{}, err
	}
	return Identity{id: id}, nil
}

func (i Identity) Recipient() string { return i.id.Recipient().String() }

func (i Identity) Decrypt(ciphertext []byte) (string, error) {
	r, err := age.Decrypt(bytes.NewReader(ciphertext), i.id)
	if err != nil {
		return "", err
	}
	out, err := io.ReadAll(r)
	if err != nil {
		return "", err
	}
	return string(out), nil
}

// Encrypt seals plaintext to every recipient; any one's identity can open it.
func Encrypt(plaintext string, recipients []string) ([]byte, error) {
	if len(recipients) == 0 {
		return nil, fmt.Errorf("no recipients to encrypt to")
	}
	rs := make([]age.Recipient, 0, len(recipients))
	for _, s := range recipients {
		r, err := age.ParseX25519Recipient(s)
		if err != nil {
			return nil, fmt.Errorf("parse recipient %q: %w", s, err)
		}
		rs = append(rs, r)
	}
	var buf bytes.Buffer
	w, err := age.Encrypt(&buf, rs...)
	if err != nil {
		return nil, err
	}
	if _, err := io.WriteString(w, plaintext); err != nil {
		return nil, err
	}
	if err := w.Close(); err != nil {
		return nil, err
	}
	return buf.Bytes(), nil
}
```

- [ ] **Step 4 — pass:** `go test ./internal/secrets/ -count=1 -v`; `go mod tidy` (age becomes direct); `grep '^go ' go.mod` still 1.25.
- [ ] **Step 5 — commit:** `feat(secrets): age encrypt/decrypt boundary + identity persistence`

---

## Task 2: migration 0002 + `age_recipient` on nodes

**Files:** create `migrations/0002_node_age_recipient.up.sql`, `.down.sql`; modify `internal/store/models.go`, `internal/store/nodes.go`, `internal/store/nodes_test.go`

- [ ] **Step 1 — failing test** (add to `nodes_test.go`): register a node with a recipient, `ListReadyNodes` returns it.

```go
func TestRegisterNodeStoresRecipient(t *testing.T) {
	st := testStore(t)
	org := newOrg(t, st)
	n, err := st.RegisterNode(testCtx(t), RegisterNodeParams{
		OrgID: org.ID, Hostname: uniq("node"), AdvertiseAddr: "10.0.0.7",
		CPUMillis: 1000, MemoryBytes: 1 << 30, AgeRecipient: "age1exampletestrecipient",
	})
	if err != nil {
		t.Fatalf("register: %v", err)
	}
	if n.AgeRecipient != "age1exampletestrecipient" {
		t.Fatalf("recipient not returned: %q", n.AgeRecipient)
	}
	ready, _ := st.ListReadyNodes(testCtx(t), org.ID)
	if len(ready) != 1 || ready[0].AgeRecipient != "age1exampletestrecipient" {
		t.Fatalf("recipient not persisted: %+v", ready)
	}
}
```

- [ ] **Step 2 — run, expect fail** (migration not applied → column missing; `AgeRecipient` field undefined). Apply the migration first in Step 3, then the DB has the column.

- [ ] **Step 3 — implement**:

`migrations/0002_node_age_recipient.up.sql`:
```sql
-- The agent's age public key (recipient). The control plane encrypts secrets to
-- it; only the agent holds the matching private key. Nullable: a node sets it
-- at registration.
ALTER TABLE nodes ADD COLUMN age_recipient TEXT;
```
`migrations/0002_node_age_recipient.down.sql`:
```sql
ALTER TABLE nodes DROP COLUMN age_recipient;
```

Apply it to the running dev DB: `make migrate-up` (or `docker compose run --rm migrate -path=/migrations -database="$DB_URL" up`).

`models.go` — add to `Node` and `RegisterNodeParams`:
```go
	AgeRecipient string `json:"age_recipient,omitempty"`
```

`nodes.go` — thread it through `RegisterNode` (INSERT + ON CONFLICT UPDATE) and the two `queryNodes` callers. In `RegisterNode` add `age_recipient` to the column list, `$8` param, `age_recipient = EXCLUDED.age_recipient` in the UPDATE, and `COALESCE(age_recipient,'')` to the RETURNING; scan into `&n.AgeRecipient`. In `queryNodes` add `COALESCE(age_recipient,'')` to both SELECTs and scan it. (Renumber params: `agent_version` was `$7`; `age_recipient` is `$8`.)

- [ ] **Step 4 — pass:** `go test ./internal/store/ -run 'Node|Recipient' -count=1`
- [ ] **Step 5 — commit:** `feat(store): node age_recipient (migration 0002)`

---

## Task 3: store secret CRUD

**Files:** create `internal/store/secrets.go`; add tests to `internal/store/secrets_test.go`

**Interfaces:**
- `type SecretMeta struct { Key string; Version int; CreatedAt time.Time }`
- `type EncryptedSecret struct { Key string; Ciphertext []byte }`
- `func (s *Store) SetSecret(ctx, envID uuid.UUID, key string, ciphertext []byte, keyID string) error`
- `func (s *Store) SecretKeysForEnv(ctx, envID uuid.UUID) ([]SecretMeta, error)`
- `func (s *Store) DeleteSecret(ctx, envID uuid.UUID, key string) error`
- `func (s *Store) EncryptedSecretsForNode(ctx, nodeID uuid.UUID) (map[string][]EncryptedSecret, error)`

- [ ] **Step 1 — failing test** `secrets_test.go`: set two versions of a key, `SecretKeysForEnv` returns latest version metadata; `EncryptedSecretsForNode` returns the ciphertext for a node running that env; delete removes it. Reuse `deployFixture` (env + node), then `CreateServiceInstances` + `UpdateDeploymentState(scheduling)` so the env is "active on the node".

```go
package store

import (
	"bytes"
	"testing"
)

func TestSecretVersioningAndKeys(t *testing.T) {
	st := testStore(t)
	dep, _ := deployFixture(t, st)
	envID := dep.EnvironmentID
	if err := st.SetSecret(testCtx(t), envID, "db_password", []byte("ct-v1"), "age1x"); err != nil {
		t.Fatalf("set v1: %v", err)
	}
	if err := st.SetSecret(testCtx(t), envID, "db_password", []byte("ct-v2"), "age1x"); err != nil {
		t.Fatalf("set v2: %v", err)
	}
	metas, err := st.SecretKeysForEnv(testCtx(t), envID)
	if err != nil {
		t.Fatalf("keys: %v", err)
	}
	if len(metas) != 1 || metas[0].Key != "db_password" || metas[0].Version != 2 {
		t.Fatalf("expected db_password v2, got %+v", metas)
	}
}

func TestEncryptedSecretsForNode(t *testing.T) {
	st := testStore(t)
	dep, node := deployFixture(t, st)
	_ = st.CreateServiceInstances(testCtx(t), dep.ID, node.ID, []NewInstance{{ServiceName: "api", Swappable: true, ImageRef: "x"}})
	_ = st.UpdateDeploymentState(testCtx(t), dep.ID, DeployScheduling, "")
	_ = st.SetSecret(testCtx(t), dep.EnvironmentID, "db_password", []byte("cipher"), "age1x")

	byEnv, err := st.EncryptedSecretsForNode(testCtx(t), node.ID)
	if err != nil {
		t.Fatalf("EncryptedSecretsForNode: %v", err)
	}
	env8 := shortID(dep.EnvironmentID)
	secs := byEnv[env8]
	if len(secs) != 1 || secs[0].Key != "db_password" || !bytes.Equal(secs[0].Ciphertext, []byte("cipher")) {
		t.Fatalf("expected the ciphertext for env %s, got %+v", env8, byEnv)
	}
}

func TestDeleteSecret(t *testing.T) {
	st := testStore(t)
	dep, _ := deployFixture(t, st)
	_ = st.SetSecret(testCtx(t), dep.EnvironmentID, "k", []byte("c"), "age1x")
	if err := st.DeleteSecret(testCtx(t), dep.EnvironmentID, "k"); err != nil {
		t.Fatalf("delete: %v", err)
	}
	metas, _ := st.SecretKeysForEnv(testCtx(t), dep.EnvironmentID)
	if len(metas) != 0 {
		t.Fatalf("expected no keys after delete, got %+v", metas)
	}
}
```

Note: the `newOrg` cleanup already cascades `secrets` via `environments ON DELETE CASCADE`, so no cleanup change is needed.

- [ ] **Step 2 — run, expect fail.**
- [ ] **Step 3 — implement** `secrets.go`:

```go
package store

import (
	"context"
	"time"

	"github.com/google/uuid"
)

type SecretMeta struct {
	Key       string    `json:"key"`
	Version   int       `json:"version"`
	CreatedAt time.Time `json:"created_at"`
}

type EncryptedSecret struct {
	Key        string `json:"key"`
	Ciphertext []byte `json:"ciphertext"` // JSON-marshals as base64
}

// SetSecret stores a new version of a secret's ciphertext. Latest-version-wins;
// the agent reads the highest version. The control plane never sees plaintext
// at rest — only the ciphertext handed to it.
func (s *Store) SetSecret(ctx context.Context, envID uuid.UUID, key string, ciphertext []byte, keyID string) error {
	_, err := s.pool.Exec(ctx, `
		INSERT INTO secrets (environment_id, key, ciphertext, key_id, version)
		VALUES ($1, $2, $3, $4,
			(SELECT COALESCE(MAX(version),0)+1 FROM secrets WHERE environment_id=$1 AND key=$2))
	`, envID, key, ciphertext, keyID)
	return mapErr(err)
}

func (s *Store) SecretKeysForEnv(ctx context.Context, envID uuid.UUID) ([]SecretMeta, error) {
	rows, err := s.pool.Query(ctx, `
		SELECT key, MAX(version), MAX(created_at)
		FROM secrets WHERE environment_id=$1
		GROUP BY key ORDER BY key
	`, envID)
	if err != nil {
		return nil, mapErr(err)
	}
	defer rows.Close()
	out := []SecretMeta{}
	for rows.Next() {
		var m SecretMeta
		if err := rows.Scan(&m.Key, &m.Version, &m.CreatedAt); err != nil {
			return nil, err
		}
		out = append(out, m)
	}
	return out, rows.Err()
}

func (s *Store) DeleteSecret(ctx context.Context, envID uuid.UUID, key string) error {
	_, err := s.pool.Exec(ctx, `DELETE FROM secrets WHERE environment_id=$1 AND key=$2`, envID, key)
	return mapErr(err)
}

// EncryptedSecretsForNode returns the latest-version ciphertext for every env
// with an active deployment on the node, keyed by env8. The agent decrypts these
// locally to build a per-env secret source.
func (s *Store) EncryptedSecretsForNode(ctx context.Context, nodeID uuid.UUID) (map[string][]EncryptedSecret, error) {
	rows, err := s.pool.Query(ctx, `
		SELECT s.environment_id, s.key, s.ciphertext
		FROM secrets s
		WHERE s.environment_id IN (
			SELECT DISTINCT d.environment_id
			FROM deployments d
			JOIN service_instances si ON si.deployment_id = d.id
			WHERE si.node_id = $1 AND d.state IN ('scheduling','starting','healthy','live')
		)
		AND s.version = (SELECT MAX(version) FROM secrets s2 WHERE s2.environment_id=s.environment_id AND s2.key=s.key)
	`, nodeID)
	if err != nil {
		return nil, mapErr(err)
	}
	defer rows.Close()
	out := map[string][]EncryptedSecret{}
	for rows.Next() {
		var envID uuid.UUID
		var es EncryptedSecret
		if err := rows.Scan(&envID, &es.Key, &es.Ciphertext); err != nil {
			return nil, err
		}
		env8 := shortID(envID)
		out[env8] = append(out[env8], es)
	}
	return out, rows.Err()
}
```

- [ ] **Step 4 — pass:** `go test ./internal/store/ -run Secret -count=1`
- [ ] **Step 5 — commit:** `feat(store): secret CRUD + EncryptedSecretsForNode`

---

## Task 4: API — set/list/delete secrets + recipient at register

**Files:** create `internal/api/secrets.go`; modify `internal/api/nodes.go` (registerNodeRequest gains `age_recipient`), `internal/api/server.go` (routes), add tests to `internal/api/secrets_test.go`

**Interfaces (HTTP):** `POST /v1/envs/{env}/secrets {key,value}` → 201; `GET /v1/envs/{env}/secrets` → `{secrets:[{key,version,created_at}]}` (no values); `DELETE /v1/envs/{env}/secrets/{key}` → 204.

- [ ] **Step 1 — failing test** `secrets_test.go`: this needs the store + a registered node with a recipient (to encrypt to). Use `testServer` (has store); register a node with a real recipient via `secrets.GenerateIdentity()`; POST a secret; GET returns the key with **no value**; a POST with no ready node/recipient → 422 or 400.

```go
func TestSetAndListSecret(t *testing.T) {
	srv := testServer(t)
	// need an env and a node with a recipient in the dev org
	// (build via srv.st; see helper below)
	envID, _ := seedEnvWithNode(t, srv)

	body, _ := json.Marshal(map[string]string{"key": "db_password", "value": "hunter2"})
	rec := httptest.NewRecorder()
	srv.ServeHTTP(rec, httptest.NewRequest(http.MethodPost, "/v1/envs/"+envID+"/secrets", bytes.NewReader(body)))
	if rec.Code != http.StatusCreated {
		t.Fatalf("set: %d %s", rec.Code, rec.Body.String())
	}
	rec = httptest.NewRecorder()
	srv.ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/v1/envs/"+envID+"/secrets", nil))
	if rec.Code != http.StatusOK {
		t.Fatalf("list: %d", rec.Code)
	}
	if bytes.Contains(rec.Body.Bytes(), []byte("hunter2")) {
		t.Fatal("list must never leak the value")
	}
	if !bytes.Contains(rec.Body.Bytes(), []byte("db_password")) {
		t.Fatalf("list missing the key: %s", rec.Body.String())
	}
}
```

`seedEnvWithNode` is a test helper (in `secrets_test.go`) that uses `srv.st` to create org(dev)→app→stack→version→env and `RegisterNode` with `secrets.GenerateIdentity().Recipient()`, returning the env id string. (The dev org is bootstrapped by `testServer`.)

- [ ] **Step 2 — run, expect fail** (route 404 / handler missing).
- [ ] **Step 3 — implement** `secrets.go` handlers + register the routes in `server.go`:

```go
s.mux.HandleFunc("POST /v1/envs/{env}/secrets", s.handleSetSecret)
s.mux.HandleFunc("GET /v1/envs/{env}/secrets", s.handleListSecrets)
s.mux.HandleFunc("DELETE /v1/envs/{env}/secrets/{key}", s.handleDeleteSecret)
```

The novel handler is `handleSetSecret` (recipient collection + encrypt); the
other two are trivial. Full code for `internal/api/secrets.go`:

```go
package api

import (
	"net/http"
	"time"

	"github.com/craig/composectl/internal/secrets"
	"github.com/craig/composectl/internal/spec"
)

type setSecretRequest struct {
	Key   string `json:"key"`
	Value string `json:"value"`
}

func (s *Server) handleSetSecret(w http.ResponseWriter, r *http.Request) {
	ctx, cancel := contextWithTimeout(r, 5*time.Second)
	defer cancel()
	envID, ok := pathUUID(w, r, "env")
	if !ok {
		return
	}
	var req setSecretRequest
	if err := decodeJSON(r, &req); err != nil {
		writeError(w, http.StatusBadRequest, "invalid request body", nil)
		return
	}
	// A secret key must be a valid ${secret:KEY} reference key, since that is
	// how it is consumed. Anchor the pattern for the whole key.
	if req.Key == "" || !spec.SecretRefPattern.MatchString("${secret:"+req.Key+"}") {
		writeError(w, http.StatusBadRequest, "invalid secret key", nil)
		return
	}
	orgID, err := s.st.OrgForEnvironment(ctx, envID)
	if err != nil {
		s.writeStoreError(w, err)
		return
	}
	nodes, err := s.st.ListReadyNodes(ctx, orgID)
	if err != nil {
		s.writeStoreError(w, err)
		return
	}
	var recipients []string
	for _, n := range nodes {
		if n.AgeRecipient != "" {
			recipients = append(recipients, n.AgeRecipient)
		}
	}
	if len(recipients) == 0 {
		writeError(w, http.StatusUnprocessableEntity,
			"no ready node with an encryption key; is an agent running?", nil)
		return
	}
	ct, err := secrets.Encrypt(req.Value, recipients)
	if err != nil {
		s.writeStoreError(w, err)
		return
	}
	// key_id records what it was sealed to, for audit/rotation. The recipients
	// double as the id in Sprint 2's single-node world.
	if err := s.st.SetSecret(ctx, envID, req.Key, ct, recipients[0]); err != nil {
		s.writeStoreError(w, err)
		return
	}
	writeJSON(w, http.StatusCreated, map[string]string{"key": req.Key})
}

func (s *Server) handleListSecrets(w http.ResponseWriter, r *http.Request) {
	ctx, cancel := contextWithTimeout(r, 5*time.Second)
	defer cancel()
	envID, ok := pathUUID(w, r, "env")
	if !ok {
		return
	}
	metas, err := s.st.SecretKeysForEnv(ctx, envID)
	if err != nil {
		s.writeStoreError(w, err)
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"secrets": metas})
}

func (s *Server) handleDeleteSecret(w http.ResponseWriter, r *http.Request) {
	ctx, cancel := contextWithTimeout(r, 5*time.Second)
	defer cancel()
	envID, ok := pathUUID(w, r, "env")
	if !ok {
		return
	}
	if err := s.st.DeleteSecret(ctx, envID, r.PathValue("key")); err != nil {
		s.writeStoreError(w, err)
		return
	}
	w.WriteHeader(http.StatusNoContent)
}
```

Add `store.OrgForEnvironment` (join environments→stacks→applications):

```go
func (s *Store) OrgForEnvironment(ctx context.Context, envID uuid.UUID) (uuid.UUID, error) {
	var orgID uuid.UUID
	err := s.pool.QueryRow(ctx, `
		SELECT a.org_id FROM environments e
		JOIN stacks s ON s.id = e.stack_id
		JOIN applications a ON a.id = s.app_id
		WHERE e.id = $1
	`, envID).Scan(&orgID)
	return orgID, mapErr(err)
}
```

The register handler gains `AgeRecipient string json:"age_recipient"` in
`registerNodeRequest` and passes it to `RegisterNodeParams`.

- [ ] **Step 4 — pass:** `go test ./internal/api/ -run Secret -count=1`; `go build ./...`
- [ ] **Step 5 — commit:** `feat(api): set/list/delete secrets, recipient at register`

---

## Task 5: deploy-time fail-fast on missing secrets

**Files:** modify `internal/api/deployments.go`; add a test to `internal/api/secrets_test.go`

- [ ] **Step 1 — failing test**: deploy a stack whose spec requires a secret, with none set → 422 listing the missing key. Build a stack version whose spec has a `SecretEnv` referencing `${secret:db_password}` (via `seedEnvWithNode` variant or a raw compose push), then `POST .../deployments` → expect 422 and `db_password` in the body.
- [ ] **Step 2 — run, expect fail** (currently 202).
- [ ] **Step 3 — implement**: in `handleCreateDeployment`, after resolving `sv.Spec` and before `CreateDeployment`, compute `required := resolved.RequiredSecrets()`; if non-empty, `have := store.SecretKeysForEnv(env)`; compute missing; if any, `writeJSON(422, errorBody{Error:"missing required secrets", Details: missing})` and return.

```go
	if req := resolved.RequiredSecrets(); len(req) > 0 {
		have, err := s.st.SecretKeysForEnv(ctx, envID)
		if err != nil {
			s.writeStoreError(w, err)
			return
		}
		set := map[string]bool{}
		for _, m := range have {
			set[m.Key] = true
		}
		var missing []string
		for _, k := range req {
			if !set[k] {
				missing = append(missing, k)
			}
		}
		if len(missing) > 0 {
			writeJSON(w, http.StatusUnprocessableEntity, errorBody{
				Error: "environment is missing required secrets", Details: missing})
			return
		}
	}
```

- [ ] **Step 4 — pass:** `go test ./internal/api/ -run 'Secret|Deploy' -count=1`
- [ ] **Step 5 — commit:** `feat(api): reject deployment when required secrets are unset (422)`

---

## Task 6: dockerd driver — per-call secret source

**Files:** modify `internal/agent/dockerd/driver.go`, `internal/agent/dockerd/driver_test.go`

The global `Driver.secrets` field becomes a per-call argument, because secrets are now per-environment.

- [ ] **Step 1 — adapt the test**: `New` drops its secrets arg; `EnsureContainer` and `resolveEnv` take a `SecretSource`. Update `TestSecretExpansion*` to call `d.resolveEnv(env, secretEnv, staticSecrets{...})` and `New("")`.
- [ ] **Step 2 — run, expect fail** (signature mismatch).
- [ ] **Step 3 — implement**: remove the `secrets` field from `Driver`; `func New(host string) (*Driver, error)`; `func (d *Driver) EnsureContainer(ctx, cs ContainerSpec, secrets SecretSource) (...)`; `func (d *Driver) resolveEnv(env, secretEnv map[string]string, secrets SecretSource) (...)`. A nil `secrets` behaves like an empty source (missing key → error, as today). `SecretSource` interface stays.
- [ ] **Step 4 — pass:** `go test ./internal/agent/dockerd/ -count=1`
- [ ] **Step 5 — commit:** `refactor(dockerd): secret source is per-call, not global`

---

## Task 7: agent — identity, recipient at register, decrypt & inject

**Files:** modify `internal/agent/config.go`, `internal/agent/agent.go`, `internal/agent/reconcile.go`, `internal/agent/reconcile_test.go`, `cmd/agent/main.go`

- [ ] **Step 1 — failing test** (`reconcile_test.go`): `Reconcile` now takes a per-env secret source map; a desired instance with a `${secret:name}` SecretEnv gets the decrypted value injected. Extend `fakeDriver.EnsureContainer` to capture the resolved env (add the `secrets SecretSource` param and record `secrets.Get("name")`), and assert the reconciler passed the right source for the instance's env8.

```go
func TestReconcilePassesPerEnvSecrets(t *testing.T) {
	f := &fakeDriver{health: map[string]dockerd.Health{}}
	r := NewReconciler(f)
	d := desired("api", true, dockerd.Health{})
	d.Service.SecretEnv = map[string]string{"WHOAMI_NAME": "${secret:name}"}
	sources := map[string]dockerd.SecretSource{d.Env8: EnvSecrets{"name": "revealed"}}
	r.Reconcile(context.Background(), []store.DesiredInstance{d}, sources)
	if f.lastSecretValue != "revealed" {
		t.Fatalf("expected the env's secret source to reach the driver, got %q", f.lastSecretValue)
	}
}
```

(Update `fakeDriver.EnsureContainer` signature to `(ctx, cs, secrets)` and set `f.lastSecretValue, _ = secrets.Get("name")`. Update the `DockerDriver` interface accordingly.)

- [ ] **Step 2 — run, expect fail.**
- [ ] **Step 3 — implement**:
  - `DockerDriver` interface: `EnsureContainer(ctx, cs, secrets dockerd.SecretSource) (string, bool, error)`.
  - `Reconcile(ctx, desired []store.DesiredInstance, secrets map[string]dockerd.SecretSource) []Report` — `ensure` looks up `secrets[di.Env8]` (nil if absent) and passes it to `EnsureContainer`.
  - `config.go`: add `IdentityFile string` (`COMPOSECTL_AGE_IDENTITY_FILE`, default `/identity/age.key`); drop `Secrets`/`COMPOSECTL_DEV_SECRETS`.
  - `agent.go`: `Run` loads `id, _ := secrets.LoadOrGenerateIdentity(cfg.IdentityFile)`; sends `id.Recipient()` as `age_recipient` in `register`; in `reconcileTick`, decode the `secrets` block, decrypt each env's ciphertext with `id` into `EnvSecrets`, build `map[env8]dockerd.SecretSource`, pass to `rec.Reconcile`.

```go
// in reconcileTick, after fetching desired:
var payload struct {
	Instances []store.DesiredInstance          `json:"instances"`
	Secrets   map[string][]store.EncryptedSecret `json:"secrets"`
}
// ... decode into payload ...
sources := map[string]dockerd.SecretSource{}
for env8, list := range payload.Secrets {
	m := EnvSecrets{}
	for _, es := range list {
		v, err := id.Decrypt(es.Ciphertext)
		if err != nil {
			log.Warn("secret decrypt failed", "env", env8, "key", es.Key, "err", err)
			continue
		}
		m[es.Key] = v
	}
	sources[env8] = m
}
reports := rec.Reconcile(ctx, payload.Instances, sources)
```

  - `agent.go` `register`: add `"age_recipient": id.Recipient()` to the body; thread `id` into `reconcileTick` (pass as arg or store on a struct).
  - `dockerd.New` call in `Run` drops the secrets arg.

- [ ] **Step 4 — pass:** `go test ./internal/agent/... -count=1`; `go build ./...`; boundary check clean.
- [ ] **Step 5 — commit:** `feat(agent): age identity, recipient at register, per-env decrypt & inject`

---

## Task 8: desired-state carries secrets + compose + demos

**Files:** modify `internal/api/nodes.go` (`handleDesiredState`), `compose.yaml`, `scripts/demo.sh`, `scripts/demo-rollback.sh`, `Makefile`; create `examples/secret/compose.yaml`, `scripts/demo-secrets.sh`

- [ ] **Step 1 — desired-state secrets**: `handleDesiredState` also calls `store.EncryptedSecretsForNode(id)` and returns `{instances, secrets}`. (Add a test asserting the block is present for a node with a set secret, or rely on the E2E demo.)

```go
	secretsByEnv, err := s.st.EncryptedSecretsForNode(ctx, id)
	if err != nil {
		s.writeStoreError(w, err)
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"instances": desired, "secrets": secretsByEnv})
```

- [ ] **Step 2 — compose**: on the `agent` service, remove `COMPOSECTL_DEV_SECRETS`, add `COMPOSECTL_AGE_IDENTITY_FILE: /identity/age.key` and a named volume `age-identity:/identity`. Add `age-identity:` under top-level `volumes:`.

- [ ] **Step 3 — secret demo stack** `examples/secret/compose.yaml`:
```yaml
services:
  api:
    image: traefik/whoami
    environment:
      WHOAMI_NAME: ${secret:name}
    x-composectl:
      ingress:
        port: 80
```

- [ ] **Step 4 — `scripts/demo-secrets.sh`**: set `name=<value>` on a fresh env; `psql` show the `secrets` row is ciphertext (not the value); push `examples/secret/compose.yaml`; deploy; wait live; `curl -H "Host: <host>" localhost:8095` → body contains `Name: <value>`. Also: deploy the same stack on a second env with **no** secret set → assert 422. Add `make demo-secrets`.

- [ ] **Step 5 — update existing demos**: `scripts/demo.sh` and `scripts/demo-rollback.sh` use `examples/hello` which references `${secret:db_password}`. Before the first deploy, `POST /v1/envs/$ENV_ID/secrets {"key":"db_password","value":"devpassword"}`. (Without it, deploy now 422s.)

- [ ] **Step 6 — end-to-end**:
```bash
make nuke && make up && sleep 20
make demo          # sets db_password first, then the flip
make demo-secrets  # ciphertext at rest, plaintext Name: <value> through Traefik; unset → 422
make demo-rollback
make demo-failure
```

- [ ] **Step 7 — commit:** `feat: desired-state carries secrets; identity volume; secret demo; retire DEV_SECRETS`

---

## Final verification

```bash
go build ./... && go vet ./... && gofmt -l .
go list -deps ./cmd/controlplane | grep docker/docker && echo LEAK || echo clean
# run unit/integration with the dev stack's control loops stopped (test isolation):
docker compose stop controlplane agent && go test ./... -count=1 && docker compose start controlplane agent
grep '^go ' go.mod   # still 1.25
```

Baseline that must not regress: `examples/webapp` digest `6072c68f…`, classification, peak `2415919104`; the 409 guards; the flip and rollback demos (now setting secrets first). The `secrets` table must hold ciphertext (verify in `demo-secrets` via psql), never plaintext.

## What this slice leaves for later

- **Env deletion (3B)** and **preview envs (3C)** — separate Sprint 3 slices.
- **Multi-node re-keying** on node join — Sprint 4.
- **Client-side encryption / secrets CLI** — Sprint 5.
