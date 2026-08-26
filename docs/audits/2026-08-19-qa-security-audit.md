# QA + Security Audit — 2026-08-19

Conducted against `master` at `f4d3839`, working tree clean. Every finding
below was verified against the code, not inferred from docs. Coverage
figures are from a full run with Postgres up and the control plane stopped
(the only configuration in which they mean anything).

**Baseline health:** `go build ./...` clean, `go vet ./...` clean, full
suite green with **zero skips** (Postgres up), boundary guards hold
(`go list -deps ./cmd/controlplane | grep docker/docker` empty; no
charmbracelet outside `cmd/navarch`). No TODO/FIXME markers anywhere.

- Real coverage with PG up: **store 66.6%, rollout 71.0%, api 50.0%**,
  logbuf 97.8%, metrics 92.4%, router 81.5%, parser 79.2%, dockerd 72.4%,
  secrets 71.7%, agent 62.7%, tui 60.8%, cli 29.3%, client 27.8%,
  spec 20.0%, cmd/* **0%**.

---

## Security findings

### S1. MEDIUM — Log delivery writes content before the node-ownership check

`internal/api/logs.go:78-91`. `handleLogDelivery` calls
`s.logs.Write(d.RequestID, d.Data)` (line 82) **unconditionally**, then
`CompleteLogRequest(ctx, nodeID, ...)` (line 88), which is the only place
node ownership is enforced (`internal/store/logs.go:180`: `AND si.node_id = $1`).
A node holding its own valid token can deliver arbitrary content under any
log-request UUID it knows; the ownership check rejects the *completion* but
the forged content is already in the buffer and is served verbatim to the
operator by `GET /v1/logs/{id}`. The existing regression test
(`TestLogDeliveryFromAnotherNodeDoesNotCompleteTheRequest`,
`internal/api/logs_test.go:113`) asserts only that the row stays `pending` —
the forged `"forged\n"` it delivers in fact lands in the buffer.

Exploitation needs a leaked 128-bit request UUID plus a compromised node
token, so this is defense-in-depth rather than an open door — but the trust
boundary being broken is exactly the one the store comment documents ("an
agent may only complete a request for a container on its own node"), and
the feature's entire value is that operator-visible output is trustworthy.

**Fix shape:** gate the `Write` on the same ownership the completion checks —
have `CompleteLogRequest` (or a sibling read) answer ownership *first*, and
write content only for requests this node owns. Extend the regression test
to assert the forged content never reaches a read.

### S2. LOW — logbuf allocates slots for unknown request UUIDs

`internal/logbuf/logbuf.go:86-92`. `Write` creates an entry for any unknown
UUID while under `DefaultMaxRequests` (64). An authenticated node can POST
deliveries naming 64 random UUIDs and occupy every slot until idle expiry; a
legitimate request whose buffer was never created is then silently dropped
(`Write → false`, and the handler comment at `api/logs.go:80-81` is wrong
about what that false means in this case). Bounded memory (~64 MiB), so this
is availability-of-one-feature. **Fix shape:** the buffer is an
operator-facing read buffer — only requests the control plane itself opened
should ever get an entry (e.g. seed the entry at `CreateLogRequest` time, or
have delivery accept a "known request" answer from the store).

### S3. LOW — a single oversized chunk is never trimmed

`internal/logbuf/logbuf.go:104`. The drop-oldest loop runs only while
`len(e.chunks) > 1`, so one ~1 MiB chunk (delivery bodies are capped at
1 MiB, `server.go:334`) can push an entry to ~2 MiB. Worst-case total is ~2×
the nominal cap. Bounded either way; fix only if touching logbuf for S2.

### S4. LOW — no TLS anywhere; tokens and age ciphertext transit plaintext HTTP

Defaults are `http://` in both binaries (`internal/agent/config.go:24`,
`internal/cli/config.go:19`); the client accepts any scheme without comment
(`internal/client/client.go:31-38`). One captured node token reads that
node's desired-state (including ciphertext) forever; the shared operator
token is game-over for the org. Correct for the dev compose network, but
there is no TLS path at all today and no warning when a production-ish URL
is plaintext. **Fix shape:** document the deployment posture (TLS terminates
at a reverse proxy in front of the control plane) and/or refuse non-loopback
non-TLS base URLs outside an explicit `COMPOSECTL_INSECURE=1` opt-in.

### S5. LOW — unsalted fingerprint label is a secret-equality oracle

`internal/agent/dockerd/driver.go:30,241,348-386`. The `cc.spec-fingerprint`
label is a deterministic SHA-256 over resolved config **including secret
plaintext**. Identical configs (including identical secret values) produce
identical labels across environments — an equality oracle — and a weak
secret inside an otherwise-known env map is brute-forceable offline from the
label alone. Practical impact is limited: anyone who can read Docker labels
can read the container's resolved env via `docker inspect` anyway. Worth a
comment on the label naming it secret-bearing-adjacent, so labels are never
harvested into metrics/exports where env is not.

### S6. LOW — `navarch secret set` takes the value as argv

`internal/cli/commands.go:484-497`; the usage example itself is
`db_password hunter2` (`internal/cli/cli.go:336`). Plaintext lands in shell
history, `ps`, and any exec audit logging. Everything downstream is careful;
this is the one casual handler. **Fix shape:** `--stdin` / prompt / `@file`,
and deprecate the positional form.

### S7. LOW — registration unconditionally overwrites a node's age recipient

`internal/api/nodes.go:46-51`, `internal/store/nodes.go:49-57`. `RegisterNode`
upserts by `(org_id, hostname)` and overwrites `age_recipient`. Anyone with
the shared operator token can re-register an existing hostname with their own
recipient, and every secret set afterward for environments homed there is
sealed to the attacker's key. Inside the current shared-token trust boundary
(see S9), but a persistent credential redirect worth closing when operator
identity lands. **Fix shape:** recipient changes should require an explicit
rotate action or at least be loud (audit event), not a silent upsert.

### S8. LOW — ciphertext sealed wider than necessary; old versions never pruned

`internal/store/secrets.go:26-41,81,131-144`. With no home node and no
running instances, `RecipientsForEnvironment` falls back to **every ready
node in the org** — a secret set before first deploy is decryptable by every
agent. And `SetSecret` appends a version per write; nothing ever prunes old
versions, so rotated secrets stay sealed to whatever recipients (including
since-removed nodes) were live at write time. At-rest exposure only — the
agent reads max version. **Fix shape:** retention window on old versions
(reaper job), and consider sealing only to the home node once one exists.

### S9. INFO — shared operator token; no per-org authorization (documented)

`internal/config/config.go:25-27` names this itself: every operator route is
guarded by the one shared token, and id-addressed handlers do no org
scoping of their own. Any token holder reads every org's catalog, secret
ciphertext metadata, log output, and can mutate any org's state. This is
the known pre-multi-tenant posture, not an oversight — but it is *the*
gating item for calling this a product, and the plan treats it as such.

### S10. INFO — minor notes

- `/healthz` is unauthenticated and pings the DB per call — a cheap
  unauthenticated load vector if the port is exposed. Loopback/compose
  network only today.
- No rate limiting anywhere; bounded by auth, 1 MiB body caps, ≤200-row
  list clamps, 5000-line tail clamp, and a 16-connection pool. Each agent
  request costs one `NodeTokenValid` query.
- `age_recipient` is accepted at registration with no format validation
  (`internal/api/nodes.go:29`) — a garbage recipient surfaces later as a 500
  on secret-set for exactly that node's environments.
- `loadNodeToken` (`internal/agent/agent.go:208-217`) silently degrades to
  the shared token on an unreadable token file, then 401-loops with no hint.
  Fails closed; diagnosability nit.
- Oversize bodies return 400 not 413 (`server.go:333-337`); `NodeTokenValid`
  DB errors surface as 401 not 500 (`server.go:106`). Cosmetic.
- Token-length leak before constant-time compare (`server.go:194`,
  `store/nodes.go:107`). Negligible — tokens are 32-byte random.

### Verified solid

No SQL injection (every store query parameterized; the only `fmt.Sprintf`
uses build project names and event messages, never SQL text). Node tokens:
256-bit crypto/rand, SHA-256 hash at rest, constant-time compare, plaintext
issued exactly once, persisted 0600. Plaintext secrets never reach the
control plane or Postgres; expansion is single-pass (`ReplaceAllStringFunc`
does not rescan, so a secret value containing `${secret:…}` is inert); the
fingerprint is a hash only and never transmitted; reports carry no env or
spec; `last_error` can name a secret *key* but never a value. The two
`/logs` routes authorize differently and correctly (pinned by test); the
operator token cannot reach node endpoints; a malformed node id cannot fall
through to the operator branch. Store-level tenant isolation is consistent
(`DesiredStateForNode`, `EncryptedSecretsForNode`, `ReportInstance`,
`LogRequestsForNode`, `TombstonesForNode` all node- or org-scoped, several
with cross-org regression tests). Metrics labels are route patterns and
enums only. All body reads bounded at 1 MiB. HTTP server has full timeouts
(no slowloris). `writeStoreError` messages audited: nothing echoes a secret
value, ciphertext, or compose body.

---

## QA findings

### Q1. HIGH — the regression suite is PG-gated and skips silently

With Postgres down, **114 of 121 tests across store/rollout/api SKIP while
`go test` reports `ok`** — every load-bearing invariant (placement refusal,
state machine SQL, tombstone expiry, route withdrawal) is verified only when
Postgres is up, and nothing in the tooling flags the difference. CLAUDE.md
documents this ("check for `--- SKIP` before trusting it") but the check is
manual. **Fix shape:** `make test` must fail on skip (e.g. `-v` piped to a
skip-count assertion) and must stop the dev control plane itself first —
today it doesn't, and CLAUDE.md says a running control plane corrupts
fixtures mid-run.

### Q2. HIGH — `cmd/` has zero tests; controlplane's main is not thin

`cmd/controlplane/main.go` (192 lines) contains real logic:
`healthCheck()` (lines 53-85, incl. the wildcard→loopback rewrite) and
`runLoop()` (174-192, ticker/timeout/metrics wiring). Options like
`WithRouteStrand` are passed nowhere else — a dropped option there silently
changes route-withdrawal behavior. `cmd/agent` and `cmd/navarch` are thin
enough to accept. **Fix shape:** extract `healthCheck`/`runLoop` to
testable functions in the package and test them.

### Q3. HIGH — internal/api at 50%: catalog/deployment handlers untested

Verified CLAUDE.md's admission is still true. `catalog.go`: 14 handlers, 2
test functions (both about `handleListOrgEnvs`). `deployments.go`: 4
handlers, **no test file**. `validate.go` `handleValidate`: the 1 MiB cap
and 422-vs-400 mapping untested. `overlay.go` `applyEnvConfig` — a **pure
function** embodying two "real bug, already fixed" precedence rules — has no
direct test; it is the cheapest high-value test in the repo. All existing
api tests skip without PG, so on a PG-less runner this package is at 5.4%.

### Q4. MEDIUM — two stacked untested layers on the operator path

`internal/cli`: 17 command groups, ~4 with tests — untested: `cmdWait`
(deadline/short-circuit), `cmdLogs` (follow loop, cursor advance, deferred
CloseLogs), `cmdEvents` (cursor pagination), drain/uncordon rendering incl.
the `(unreachable)` suffix, promote/rollback/deploy. `internal/client`: 36
exported methods, 6 tested — `Promote`, `Rollback`, `Deploy`, `OpenLogs`
and friends, `DrainNode`, `CreatePreview` all untested. The client is by
design the only package that knows the wire format, and both front ends
(CLI, TUI) depend on it; a URL or envelope bug there surfaces only through
the untested CLI layer above it.

### Q5. MEDIUM — thin-domain coverage gaps

`internal/spec` at 20%: `Digest()` — the stability invariant with its own
CLAUDE.md section — is tested only indirectly through parser. `internal/agent`'s
control-plane client layer (`register` incl. the token-persist fallback,
`reconcileTick`'s report-then-logs-then-heartbeat ordering, the
log-delivery-failure-must-not-skip-heartbeat behavior) has no direct tests.

### Q6. LOW — specific missing pins

No test for `strand == 0` disables-withdrawal (documented at
`store/deployments.go:413`). No test that `syncRouter` omits routes with
empty `NodeAddr`/`PublishedPort` (`rollout/controller.go:113`).

### Q7. LOW — stale doc vs code

CLAUDE.md's TUI section claims "no endpoint lists an organization's
environments", but `handleListOrgEnvs` exists (`api/catalog.go:300`), is
routed, has a client method and a test — the TUI's slow-tier 15-request
catalog walk can collapse to one request. CLAUDE.md should be updated and
the TUI re-tiered. (Also: `examples/webapp`'s stale `cache` comment, already
recorded in CLAUDE.md's loose ends.)

### Q8. POSITIVE — error handling and concurrency are clean

All `_ =` sites audited and justified (best-effort encode, stop-before-
force-remove, deferred rollback). No goroutine leaks; every ticker has
`defer t.Stop()`; the Reconciler's unmutexed maps honor a documented
single-goroutine invariant; `logbuf` is correctly mutex-guarded. `go vet`
clean. The pure-logic layers (parser, logbuf, metrics, router, tui,
reconcile) are genuinely well-tested.

---

## What this audit means

The platform's *design* security posture holds up under reading: no
injection, no plaintext at rest, correct constant-time compares, consistent
node/org scoping, bounded everything. The one genuine defect (S1) is a
defense-in-depth break in a trust boundary the code otherwise documents and
tests — small, fixable, and exactly the kind of thing an audit exists to
find before a tenant does.

The QA risk is structural, not scattered: the packages that hold the
platform's actual business logic (store/rollout/api) are well-tested *only
in the configuration `make test` neither creates nor verifies*, and the
operator-facing path (client → CLI → handlers) is two stacked untested
layers. Green-for-wrong-reasons is the failure mode to kill.

Both feed the plan: `docs/superpowers/plans/2026-08-19-finish-line.md`.
