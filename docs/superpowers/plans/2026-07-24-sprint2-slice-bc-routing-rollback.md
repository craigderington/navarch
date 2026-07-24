# Sprint 2 Slices B & C — Routing, Auto-Promote, Rollback

> **For agentic workers:** execute task-by-task with TDD. Steps use checkbox (`- [ ]`) syntax.

**Goal:** Traefik routes the live revision's ingress; the controller auto-promotes on healthy and flips traffic zero-downtime, tearing down the old revision; `POST /rollback` re-deploys an older stack version.

**Builds on:** Sprint 2 Slice A (agent + scheduler + controller spine).

## Global Constraints

- Boundaries hold: only `internal/agent/dockerd` imports the Docker SDK; the control plane must not link it (`go list -deps ./cmd/controlplane | grep docker/docker` empty). `internal/router` imports neither pgx nor the Docker SDK — it takes plain `Route` values and writes files.
- go 1.25; ports never 3000/5000/8000/9000. Traefik web on host **8095**, dashboard **8096**.
- Commit locally only; work on branch `sprint2-slice-bc`.

## The routing design (the one hard part)

The control plane doesn't touch Docker, but Traefik must reach the live ingress container. Solution: a shared external Docker network **`cc-ingress`**.

- `cc-ingress` is created by `make up` before `docker compose up` (so compose can reference it `external: true`). Traefik joins it via compose.
- The **agent** attaches each *ingress-service* container to `cc-ingress` (in addition to its revision network). It knows a service is ingress from `di.Service.Ingress != nil`.
- The **router** (control plane) writes a Traefik file-provider config mapping `Host(hostname)` → `http://<live-ingress-container-name>:<port>`. Container names are unique per revision (`cc-{env8}-r{rev}-{slot}-{svc}`), so blue and green never collide on `cc-ingress` DNS.
- Traefik's file-provider watches a shared named volume `traefik-dynamic`. The control plane (distroless **nonroot**) writes there; a one-shot `traefik-init` chmods the volume writable first.

This keeps the control plane Docker-free (it writes a file) and the agent owning all Docker network plumbing (which it already does).

---

## File Structure

**Create:** `internal/router/router.go` + `_test.go`; `scripts/demo-rollback.sh`
**Modify:** `internal/store/deployments.go` (`ListLiveRoutes`); `internal/rollout/controller.go` (auto-promote + router sync); `internal/rollout/rollout_test.go`; `internal/agent/reconcile.go` (+ingress attach) + `reconcile_test.go`; `cmd/controlplane/main.go` (wire router); `internal/config/config.go` (`RouterDir`); `internal/api/nodes.go` (`handleRollback`); `internal/store/deployments.go` (`RollbackDeployment`); `compose.yaml` (traefik + traefik-init + cc-ingress); `Makefile` (create cc-ingress in `up`, `demo-flip` target); `examples/hello/compose.yaml` (api → `traefik/whoami` so the flip is visible); `scripts/demo.sh`

---

## Task B1: `internal/router` — Traefik config generation

**Files:** create `internal/router/router.go`, `internal/router/router_test.go`

**Interfaces:**
- `type Route struct { Hostname, ServiceContainer string; Port int; Key string }` (Key = stable id, e.g. env8)
- `type Router struct { dir string }`; `func New(dir string) *Router`
- `func (r *Router) Sync(routes []Route) error` — writes `<dir>/composectl.yml` atomically (temp + rename)

- [ ] **Step 1 — failing test** `router_test.go`:

```go
package router

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestSyncWritesTraefikConfig(t *testing.T) {
	dir := t.TempDir()
	r := New(dir)
	if err := r.Sync([]Route{
		{Key: "abc12345", Hostname: "prod.example.com", ServiceContainer: "cc-abc12345-r1-blue-api", Port: 80},
	}); err != nil {
		t.Fatalf("Sync: %v", err)
	}
	b, err := os.ReadFile(filepath.Join(dir, "composectl.yml"))
	if err != nil {
		t.Fatalf("read: %v", err)
	}
	out := string(b)
	for _, want := range []string{
		"Host(`prod.example.com`)",
		"http://cc-abc12345-r1-blue-api:80",
		"entryPoints",
	} {
		if !strings.Contains(out, want) {
			t.Fatalf("config missing %q:\n%s", want, out)
		}
	}
}

func TestSyncEmptyIsValid(t *testing.T) {
	dir := t.TempDir()
	if err := New(dir).Sync(nil); err != nil {
		t.Fatalf("empty sync: %v", err)
	}
	if _, err := os.Stat(filepath.Join(dir, "composectl.yml")); err != nil {
		t.Fatalf("expected a file even when empty: %v", err)
	}
}
```

- [ ] **Step 2 — run, expect fail** (`New` undefined): `go test ./internal/router/ -count=1`

- [ ] **Step 3 — implement** `router.go`:

```go
// Package router generates Traefik file-provider dynamic config from the set
// of live ingress routes. It is the ONLY package that knows Traefik's config
// shape; it imports neither pgx nor the Docker SDK, taking plain Route values
// so the control plane stays Docker-free while still steering traffic.
package router

import (
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strings"
)

type Route struct {
	Key              string // stable per-environment id (env8)
	Hostname         string
	ServiceContainer string // cc-{env8}-r{rev}-{slot}-{ingress}
	Port             int
}

type Router struct{ dir string }

func New(dir string) *Router { return &Router{dir: dir} }

// Sync writes the whole dynamic config from the current routes, atomically
// (temp file + rename) so Traefik's watcher never reads a half-written file.
// Regenerating the full file each call means a removed route simply disappears.
func (r *Router) Sync(routes []Route) error {
	if err := os.MkdirAll(r.dir, 0o755); err != nil {
		return err
	}
	sort.Slice(routes, func(i, j int) bool { return routes[i].Key < routes[j].Key })

	var b strings.Builder
	b.WriteString("# generated by composectl — do not edit\nhttp:\n  routers:\n")
	for _, rt := range routes {
		fmt.Fprintf(&b, "    r-%s:\n      rule: \"Host(`%s`)\"\n      entryPoints: [\"web\"]\n      service: s-%s\n",
			rt.Key, rt.Hostname, rt.Key)
	}
	b.WriteString("  services:\n")
	for _, rt := range routes {
		fmt.Fprintf(&b, "    s-%s:\n      loadBalancer:\n        servers:\n          - url: \"http://%s:%d\"\n",
			rt.Key, rt.ServiceContainer, rt.Port)
	}

	tmp := filepath.Join(r.dir, ".composectl.yml.tmp")
	if err := os.WriteFile(tmp, []byte(b.String()), 0o644); err != nil {
		return err
	}
	return os.Rename(tmp, filepath.Join(r.dir, "composectl.yml"))
}
```

- [ ] **Step 4 — pass:** `go test ./internal/router/ -count=1 -v`
- [ ] **Step 5 — commit:** `feat(router): traefik file-provider config generation`

---

## Task B2: `store.ListLiveRoutes`

**Files:** modify `internal/store/deployments.go`, add a test to `internal/store/instances_test.go`

**Interfaces:** `type LiveRoute struct { Env8, ProjectName, IngressService, Hostname string; IngressPort int }`; `func (s *Store) ListLiveRoutes(ctx) ([]LiveRoute, error)`

- [ ] **Step 1 — failing test** (append to `instances_test.go`): deploy fixture, promote to live via the state machine + `PromoteDeployment`, set env hostname, assert `ListLiveRoutes` returns the ingress route with the right port from `resolved_spec`.

```go
func TestListLiveRoutes(t *testing.T) {
	st := testStore(t)
	dep, node := deployFixture(t, st)
	// give the environment a hostname
	_, _ = st.Pool().Exec(testCtx(t), `UPDATE environments SET hostname='prod.example.com' WHERE id=(SELECT environment_id FROM deployments WHERE id=$1)`, dep.ID)
	_ = st.CreateServiceInstances(testCtx(t), dep.ID, node.ID, []NewInstance{{ServiceName: "api", Swappable: true, ImageRef: "x"}})
	// drive to live
	for _, s := range []DeploymentState{DeployScheduling, DeployStarting, DeployHealthy} {
		_ = st.UpdateDeploymentState(testCtx(t), dep.ID, s, "")
	}
	if _, err := st.PromoteDeployment(testCtx(t), dep.ID); err != nil {
		t.Fatalf("promote: %v", err)
	}
	routes, err := st.ListLiveRoutes(testCtx(t))
	if err != nil {
		t.Fatalf("ListLiveRoutes: %v", err)
	}
	var found bool
	for _, r := range routes {
		if r.Hostname == "prod.example.com" {
			found = true
			if r.IngressService != "api" || r.IngressPort == 0 {
				t.Fatalf("bad route: %+v", r)
			}
		}
	}
	if !found {
		t.Fatalf("live route not found in %+v", routes)
	}
}
```

The fixture's `twoServiceSpec` has no ingress; extend it so `api` declares one: add `Ingress: &spec.Ingress{Port: 80}` to the `api` service in `twoServiceSpec` (used only by tests).

- [ ] **Step 2 — run, expect fail** (`ListLiveRoutes` undefined).
- [ ] **Step 3 — implement** in `deployments.go`:

```go
type LiveRoute struct {
	Env8           string
	ProjectName    string
	IngressService string
	Hostname       string
	IngressPort    int
}

// ListLiveRoutes returns one route per live deployment whose environment has a
// hostname and whose spec declares an ingress service. The router turns these
// into Traefik config. resolved_spec is parsed here (not in the router) so the
// router stays free of the spec type and pgx.
func (s *Store) ListLiveRoutes(ctx context.Context) ([]LiveRoute, error) {
	rows, err := s.pool.Query(ctx, `
		SELECT d.environment_id, d.project_name, COALESCE(e.hostname,''), d.resolved_spec
		FROM deployments d
		JOIN environments e ON e.id = d.environment_id
		WHERE d.state = 'live' AND e.hostname IS NOT NULL AND e.hostname <> ''
	`)
	if err != nil {
		return nil, mapErr(err)
	}
	defer rows.Close()
	out := []LiveRoute{}
	for rows.Next() {
		var envID uuid.UUID
		var project, hostname string
		var specJSON []byte
		if err := rows.Scan(&envID, &project, &hostname, &specJSON); err != nil {
			return nil, err
		}
		var ds spec.DeploymentSpec
		if err := json.Unmarshal(specJSON, &ds); err != nil {
			return nil, err
		}
		name, ok := ds.IngressService()
		if !ok {
			continue
		}
		out = append(out, LiveRoute{
			Env8: shortID(envID), ProjectName: project, IngressService: name,
			Hostname: hostname, IngressPort: ds.Services[name].Ingress.Port,
		})
	}
	return out, rows.Err()
}
```

`deployments.go` already imports `encoding/json`, `uuid`, and `spec`? It imports json and uuid; add `"github.com/craig/composectl/internal/spec"` if not present.

- [ ] **Step 4 — pass:** `go test ./internal/store/ -run LiveRoutes -count=1`
- [ ] **Step 5 — commit:** `feat(store): ListLiveRoutes for the router`

---

## Task B3: controller auto-promote + router sync

**Files:** modify `internal/rollout/controller.go`, `internal/rollout/rollout_test.go`

**Interfaces:** `NewController` gains a router param: `func NewController(st *store.Store, log *slog.Logger, rtr RouterSync) *Controller` where `type RouterSync interface { Sync(routes []router.Route) error }`. Nil is allowed (Slice A tests).

- [ ] **Step 1 — failing test**: add `TestControllerAutoPromotes` — schedule, report all healthy, tick once (→ healthy), tick again (→ live). Update existing `NewController(st, discardLog())` calls to `NewController(st, discardLog(), nil)`.

```go
func TestControllerAutoPromotes(t *testing.T) {
	st := testStore(t)
	depID, nodeID, _ := fixture(t, st)
	_ = NewScheduler(st, discardLog()).ScheduleOnce(ctx(t))
	c := NewController(st, discardLog(), nil)
	reportAll(t, st, nodeID, store.InstanceRunning, "healthy")
	_ = c.ReconcileOnce(ctx(t)) // starting→healthy
	_ = c.ReconcileOnce(ctx(t)) // healthy→live (auto-promote)
	if dep, _ := st.GetDeployment(ctx(t), depID); dep.State != store.DeployLive {
		t.Fatalf("expected live after auto-promote, got %s", dep.State)
	}
}
```

- [ ] **Step 2 — run, expect fail** (signature mismatch / not live).
- [ ] **Step 3 — implement**: add the router field + interface; in `advance`, add a case for `DeployHealthy → PromoteDeployment`; at the end of `ReconcileOnce`, if `c.rtr != nil`, fetch `st.ListLiveRoutes` and `c.rtr.Sync(toRouterRoutes(...))`.

```go
type RouterSync interface {
	Sync(routes []router.Route) error
}

type Controller struct {
	st           *store.Store
	log          *slog.Logger
	rtr          RouterSync
	startTimeout time.Duration
}

func NewController(st *store.Store, log *slog.Logger, rtr RouterSync) *Controller {
	return &Controller{st: st, log: log, rtr: rtr, startTimeout: 5 * time.Minute}
}
```

In `advance`, add after the starting→healthy case:

```go
	case dep.State == store.DeployHealthy:
		// Auto-promote: flip to live atomically. The router sync at the end of
		// the tick repoints Traefik; teardown of the superseded revision is the
		// terminal-state cleanup below.
		if _, err := c.st.PromoteDeployment(ctx, dep.ID); err != nil {
			return err
		}
		c.log.Info("auto-promoted", "deployment", dep.ID)
		return nil
```

At the end of `ReconcileOnce`, before `return nil`:

```go
	if c.rtr != nil {
		routes, err := c.st.ListLiveRoutes(ctx)
		if err != nil {
			return err
		}
		rr := make([]router.Route, 0, len(routes))
		for _, lr := range routes {
			rr = append(rr, router.Route{
				Key: lr.Env8, Hostname: lr.Hostname,
				ServiceContainer: lr.ProjectName + "-" + lr.IngressService, Port: lr.IngressPort,
			})
		}
		if err := c.rtr.Sync(rr); err != nil {
			c.log.Warn("router sync failed", "err", err)
		}
	}
```

Add imports `"github.com/craig/composectl/internal/router"`. Remove the obsolete "Slice A stops at healthy" comment.

- [ ] **Step 4 — pass:** `go test ./internal/rollout/ -count=1 -v` (all: scheduler, the three existing controller tests, auto-promote).
- [ ] **Step 5 — commit:** `feat(rollout): controller auto-promote + router sync`

---

## Task B4: agent attaches ingress containers to `cc-ingress`

**Files:** modify `internal/agent/reconcile.go`, `internal/agent/reconcile_test.go`

- [ ] **Step 1 — failing test**: a desired ingress instance (Service.Ingress set) should cause an `AttachNetwork(..., "cc-ingress", ...)` call. Extend `fakeDriver` to record ingress attaches (it already records `attached`).

```go
func TestReconcileAttachesIngressToSharedNetwork(t *testing.T) {
	f := &fakeDriver{health: map[string]dockerd.Health{}}
	r := NewReconciler(f)
	d := desired("api", true, dockerd.Health{})
	d.Service.Ingress = &spec.Ingress{Port: 80}
	r.Reconcile(context.Background(), []store.DesiredInstance{d})
	var attached bool
	for _, a := range f.attached {
		if strings.HasSuffix(a, "->cc-ingress") {
			attached = true
		}
	}
	if !attached {
		t.Fatalf("ingress container must join cc-ingress, got attaches=%v", f.attached)
	}
}
```

Add `"strings"` to the test imports.

- [ ] **Step 2 — run, expect fail.**
- [ ] **Step 3 — implement**: in `ensure`, after the pinned-attach block, add:

```go
	// An ingress service also joins the shared cc-ingress network so Traefik
	// (permanently on it) can reach this revision's ingress container by its
	// unique name. Blue and green never collide because the name carries the
	// revision + slot.
	if di.Service.Ingress != nil {
		if _, err := r.drv.EnsureNetwork(ctx, "cc-ingress", map[string]string{"cc.shared": "ingress"}); err != nil {
			return fail(err)
		}
		if err := r.drv.AttachNetwork(ctx, id, "cc-ingress", name); err != nil {
			return fail(err)
		}
	}
```

- [ ] **Step 4 — pass:** `go test ./internal/agent/ -count=1 -v`
- [ ] **Step 5 — commit:** `feat(agent): attach ingress containers to shared cc-ingress network`

---

## Task B5: wire router into the control plane

**Files:** modify `internal/config/config.go` (`RouterDir`), `cmd/controlplane/main.go`

- [ ] **Step 1 — config**: add `RouterDir string` to `Config`, `RouterDir: envOr("COMPOSECTL_ROUTER_DIR", "")` in `Load`. Empty means "no router" (router stays nil).
- [ ] **Step 2 — wire**: in `main.go` `run`, build the router when configured and pass it to the controller:

```go
	var rtr rollout.RouterSync
	if cfg.RouterDir != "" {
		rtr = router.New(cfg.RouterDir)
	}
	ctrl := rollout.NewController(st, log, rtr)
```

Add import `"github.com/craig/composectl/internal/router"`.

- [ ] **Step 3 — verify:** `go build ./... && go vet ./... && go list -deps ./cmd/controlplane | grep docker/docker || echo clean` (router must not pull the Docker SDK).
- [ ] **Step 4 — commit:** `feat(controlplane): construct the router and hand it to the controller`

---

## Task B6: Traefik in the compose stack + visible-flip demo

**Files:** modify `compose.yaml`, `Makefile`, `examples/hello/compose.yaml`, `scripts/demo.sh`

- [ ] **Step 1 — make the flip visible**: in `examples/hello/compose.yaml`, change `api`'s image from `nginx:alpine` to `traefik/whoami` (serves its container name on `:80`), keep `x-composectl.ingress.port: 80`.

- [ ] **Step 2 — compose**: add the external network, the init one-shot, traefik, and the shared volume; give the control plane the router dir.

```yaml
# under services.controlplane.environment:
      COMPOSECTL_ROUTER_DIR: /dynamic
# under services.controlplane:
    volumes:
      - traefik-dynamic:/dynamic
    depends_on:
      # ...existing... plus:
      traefik-init:
        condition: service_completed_successfully

  traefik-init:
    image: busybox
    command: ["sh", "-c", "mkdir -p /dynamic && chmod 777 /dynamic"]
    volumes:
      - traefik-dynamic:/dynamic

  traefik:
    image: traefik:v3.3
    command:
      - "--entryPoints.web.address=:80"
      - "--providers.file.directory=/dynamic"
      - "--providers.file.watch=true"
      - "--api.insecure=true"
      - "--log.level=INFO"
    ports:
      - "8095:80"
      - "8096:8080"
    volumes:
      - traefik-dynamic:/dynamic:ro
    networks:
      - default
      - cc-ingress

networks:
  cc-ingress:
    external: true

volumes:
  pgdata:
  traefik-dynamic:
```

The agent needs to reach `cc-ingress` too — it attaches containers to it by name; no compose change needed for the agent (it uses the Docker API).

- [ ] **Step 3 — Makefile**: create `cc-ingress` before `up`, add a flip demo target.

```makefile
up: ## Start the dev stack
	docker network inspect cc-ingress >/dev/null 2>&1 || docker network create cc-ingress
	docker compose up -d --build

nuke: ## Stop the dev stack and delete volumes
	docker compose down -v
	docker network rm cc-ingress 2>/dev/null || true

demo-flip: ## Deploy twice and curl through Traefik to see the traffic flip
	API=$(API) ./scripts/demo.sh
```

- [ ] **Step 4 — demo**: extend `scripts/demo.sh` to curl through Traefik and show the flip. After r1 is live, and after r2 auto-promotes, curl `http://localhost:8095` with `Host: prod.example.com` and print the whoami container name each time; assert both return 200 and the name changes r1→r2. Also assert r1's swappable containers are gone after the flip (torn down), and the pinned db remains.

Because the controller now **auto-promotes**, drop the manual `POST /promote` step from the demo; a deployment reaches `live` on its own. Show: deploy r1 → wait live → curl (whoami = r1 api) → deploy r2 → wait live → curl (whoami = r2 api) → `docker ps` shows r2 + shared db, no r1 swappable.

- [ ] **Step 5 — end-to-end:** `make nuke && make up && sleep 20 && make demo`. Expect: curl through Traefik returns 200 before and after; the whoami name flips r1→r2; r1 swappable containers gone; one shared pinned db throughout.
- [ ] **Step 6 — commit:** `feat: traefik routing + auto-promote traffic flip, end to end`

---

## Task C1: `store.RollbackDeployment`

Rollback = re-deploy the stack version of an earlier revision as a **new** revision. `deployments` stays append-only; the new deployment runs through the normal spine and auto-promotes.

**Files:** modify `internal/store/deployments.go`, add test to `instances_test.go`

**Interfaces:** `func (s *Store) RollbackDeployment(ctx, envID uuid.UUID, toRevision int) (*Deployment, error)` — finds the stack_version_id of `toRevision` in this env (must exist), then creates a new deployment with it via the existing `CreateDeployment` path (new revision, opposite slot). If `toRevision <= 0`, target the revision before the current live one.

- [ ] **Step 1 — failing test**: fixture with two versions deployed (r1 live, r2 live after flip); `RollbackDeployment(env, 1)` creates r3 whose `stack_version_id` equals r1's. (Keep it a store-level assertion; the full rollout is covered by the demo.)
- [ ] **Step 2 — run, expect fail.**
- [ ] **Step 3 — implement**: resolve the target revision's `stack_version_id` (and `resolved_spec`) from `deployments` in the env, then delegate to `CreateDeployment`. Reuse `CreateDeploymentParams`. A rollback to a revision that doesn't exist → `ErrNotFound`.

```go
// RollbackDeployment re-deploys an earlier revision's stack version as a new
// revision. Append-only holds: nothing is mutated, a new deployment is created
// and runs the normal rollout. toRevision <= 0 means "the revision before the
// current live one".
func (s *Store) RollbackDeployment(ctx context.Context, envID uuid.UUID, toRevision int) (*Deployment, error) {
	var svID uuid.UUID
	var specJSON []byte
	q := `SELECT stack_version_id, resolved_spec FROM deployments WHERE environment_id=$1 AND revision=$2`
	args := []any{envID, toRevision}
	if toRevision <= 0 {
		// the revision before the current live one
		q = `SELECT stack_version_id, resolved_spec FROM deployments
		     WHERE environment_id=$1 AND revision < (
		       SELECT revision FROM deployments WHERE id=(SELECT live_deployment_id FROM environments WHERE id=$1))
		     ORDER BY revision DESC LIMIT 1`
		args = []any{envID}
	}
	if err := s.pool.QueryRow(ctx, q, args...).Scan(&svID, &specJSON); err != nil {
		return nil, mapErr(err)
	}
	var resolved spec.DeploymentSpec
	if err := json.Unmarshal(specJSON, &resolved); err != nil {
		return nil, err
	}
	return s.CreateDeployment(ctx, CreateDeploymentParams{
		EnvironmentID: envID, StackVersionID: svID, ResolvedSpec: &resolved, CreatedBy: "rollback",
	})
}
```

- [ ] **Step 4 — pass; Step 5 — commit:** `feat(store): RollbackDeployment re-deploys an earlier revision`

---

## Task C2: `POST /rollback` handler

**Files:** modify `internal/api/nodes.go` (replace `handleRollback`), add test to `internal/api/nodes_test.go`

**Interfaces:** `POST /v1/envs/{env}/rollback` body `{"to_revision": N}` (omit/0 = previous) → `202` with the new deployment.

- [ ] **Step 1 — failing test**: httptest POST to a fresh env with no deployments → expect 404 (nothing to roll back to). (A happy-path rollback needs a full rollout; leave that to the demo.)
- [ ] **Step 2 — run, expect fail** (currently 501).
- [ ] **Step 3 — implement**:

```go
type rollbackRequest struct {
	ToRevision int `json:"to_revision,omitempty"`
}

func (s *Server) handleRollback(w http.ResponseWriter, r *http.Request) {
	ctx, cancel := contextWithTimeout(r, 15*time.Second)
	defer cancel()
	envID, ok := pathUUID(w, r, "env")
	if !ok {
		return
	}
	var req rollbackRequest
	if err := decodeJSON(r, &req); err != nil {
		writeError(w, http.StatusBadRequest, "invalid request body", nil)
		return
	}
	dep, err := s.st.RollbackDeployment(ctx, envID, req.ToRevision)
	if err != nil {
		s.writeStoreError(w, err)
		return
	}
	writeJSON(w, http.StatusAccepted, dep)
}
```

Move `handleRollback` out of the stub list; keep `notImplemented` only if still used (it is not — remove it if `handleRollback` was the last caller; check other stubs first). If no other 501 handlers remain, delete `notImplemented`.

- [ ] **Step 4 — pass:** `go test ./internal/api/ -count=1`
- [ ] **Step 5 — commit:** `feat(api): POST /rollback re-deploys an earlier revision`

---

## Task C3: rollback demo

**Files:** create `scripts/demo-rollback.sh`, add `make demo-rollback`

- [ ] Deploy r1 (whoami build A via image tag or env marker), then r2, then `POST /rollback {to_revision:1}` → r3 comes up with r1's spec and auto-promotes; assert the live whoami matches r1's identity again. Since both revisions use the same image, distinguish by the env config or just assert r3's `stack_version_id` == r1's via the API history. Simplest: assert the rollback deployment reaches `live` and its stack version equals r1's (from `GET /v1/deployments/{id}` → compare `stack_version_id`). Commit: `feat: rollback demo`.

---

## Final verification (both slices)

```bash
go build ./... && go vet ./... && gofmt -l .
go list -deps ./cmd/controlplane | grep docker/docker && echo LEAK || echo clean
go test ./... -count=1          # needs Postgres + Docker up
make nuke && make up && sleep 20
make demo         # traffic flip visible through Traefik (whoami name r1→r2)
make demo-failure # unchanged: bad image → failed
make demo-rollback
curl -s -H 'Host: prod.example.com' http://localhost:8095   # 200 from the live revision
```

Baseline that must not regress: `examples/webapp` digest `6072c68f…`, classification, peak `2415919104`; the 409 guards. Note `examples/hello` digest changes (api image → whoami) — that's intended.
