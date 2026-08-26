# Crossing the Finish Line — 2026-08-19

**Goal:** take Navarch from "sprints complete, demos green" to a product:
every audit finding closed or consciously accepted, a test suite whose
green *means* green, operator identity instead of a shared password, a TLS
story, and a versioned thing you can hand to a machine that isn't this one.

**Inputs:** `docs/audits/2026-08-19-qa-security-audit.md` (every S/Q
reference below is a finding there), plus CLAUDE.md's loose ends. Verified
against `master` at `f4d3839`.

---

## The one decision that shapes everything

**Is the finish line single-operator or multi-tenant?**

The shared `COMPOSECTL_AGENT_TOKEN` is named by the code itself as
pre-multi-tenant scaffolding (`internal/config/config.go:25-27`). Everything
below assumes the answer is **multi-tenant product** — that is what "product"
means, and S9 is the largest latent isolation gap on the operator side. If
the answer is actually "a tool Craig runs for himself," Sprint 7 shrinks to
its Slice C (TLS posture) plus token hygiene, and the finish line moves a
sprint closer. Decide before starting Sprint 7; nothing before it depends
on the answer.

## Ordering rationale

Test integrity comes **first**, not because it's the most exciting work but
because everything after it lands on top of it: auth changes touch every
handler, packaging freezes the API surface. A suite that skips silently
would let any of the later slices regress unnoticed. Security fixes ride
with Sprint 6 because they are small, independent, and each wants exactly
the regression-test discipline that slice establishes.

---

## Sprint 6 — Make green mean green + audit remediation

### Slice A: test integrity (kills Q1, Q2)

1. **`make test` grows teeth.** It must (a) stop the dev control plane and
   agents itself and restart them after — CLAUDE.md requires it and the
   target currently doesn't do it, so the one command everyone runs is the
   one that corrupts fixtures; (b) fail if any test skipped, by counting
   `--- SKIP` in a `-v` run against the PG-dependent packages. A runner
   without Postgres must be *loudly* useless, not silently green.
2. **CI.** Whatever the host (a self-hosted runner with Docker, or GH
   Actions with a `postgres:16` service on `5473`): `go vet`, `go build`,
   the full suite with PG up, boundary guards
   (`go list -deps ./cmd/controlplane | grep docker/docker` empty; same
   shape for charmbracelet), and the known-good `examples/webapp` digest
   baseline asserted by a parser test so a classification change fails a
   build instead of a demo. This is the single highest-leverage item in the
   plan — every "skip silently" hazard becomes a red build.
3. **Extract and test `cmd/controlplane`'s main logic** (Q2): `healthCheck`
   (the wildcard→loopback rewrite is exactly the kind of thing that breaks a
   deployment health gate invisibly) and `runLoop` wiring, so a dropped
   `WithRouteStrand` fails a test.

### Slice B: security fixes (S1-S3, S5-S8)

Each fix ships with its regression test, verified to fail without the fix —
the house rule, and the audit found one test
(`TestLogDeliveryFromAnotherNodeDoesNotCompleteTheRequest`) that passes while
its subject is half-broken, which is what happens when the assertion doesn't
cover the claim.

1. **S1, the one genuine defect:** log-delivery content write moves behind
   the node-ownership check (internal/api/logs.go:82). The regression test
   must assert the forged content never reaches a *read*, not just that the
   row stays pending.
2. **S2:** logbuf only buffers requests the control plane opened — seed the
   entry at `CreateLogRequest`, or let delivery ask the store whether the
   request is known to this node before writing. Kills the slot-exhaustion
   path and fixes the inaccurate `Write → false` comment.
3. **S6:** `navarch secret set --stdin` (and `@file`); deprecate the
   positional value with a warning, remove it in 1.1.
4. **S7:** a recipient change on re-register becomes an explicit, audited
   event — at minimum an environment/node event naming the rotation, so
   S7's silent credential redirect leaves a trace.
5. **S8:** secret-version retention (reaper sweeps versions older than N
   days once a newer version exists); seal-to-home-node-only once a home
   node exists. Old ciphertext sealed to since-removed nodes should not
   outlive its usefulness indefinitely.
6. **S5, S10 minor:** comments/docs where the fix is awareness (fingerprint
   label is secret-bearing-adjacent; age_recipient format validation at
   register, which also turns a later 500 into an early 400).

**Accepted with rationale, not fixed:** S3 (single oversized chunk — bounded
2×, fix only if touching logbuf anyway), S10's cosmetics (400-vs-413,
401-on-DB-error, token-length leak), S4 deferred to Sprint 7 Slice C where
TLS is the subject rather than a footnote.

### Slice C: coverage where it's cheapest per risk (Q3-Q6)

Priority order is risk-over-effort:

1. **`applyEnvConfig` direct test** — pure function, two historical bugs
   live in it, currently tested only through PG-gated API tests. Cheapest
   high-value test in the repo.
2. **`internal/api` handler tests** for catalog + deployments (the 50%
   number): create org/app/stack/env/version, the raw-body + `?created_by=`
   contract, 422-vs-400 in validate, promote/rollback through the store.
   Target ≥70%.
3. **`internal/client` wire tests** (27.8% → ≥70%): every untested method
   against `httptest`, per the existing six. The client is the only package
   that knows the wire format and both front ends depend on it; this is
   where a URL typo is cheapest to catch.
4. **`internal/cli`** for the flows that carry state: `cmdWait` deadlines,
   `cmdLogs` follow/cursor/Close, `cmdEvents` pagination, drain rendering
   incl. `(unreachable)`.
5. **`internal/spec`** digest stability direct test (Q5) and the two missing
   route pins (Q6: `strand == 0` disables withdrawal; `syncRouter` omits
   routes with no address/port).

Docs truthing rides here (Q7): CLAUDE.md's TUI section updated for
`handleListOrgEnvs`, the TUI catalog walk collapsed to one request and
re-tiered, the `examples/webapp` cache comment fixed.

**Exit criteria for Sprint 6:** CI red on any skip; all audit findings
closed or listed as accepted; api/client ≥70%; `make demo` suite green from
`make nuke`.

---

## Sprint 7 — Operator identity and transport (the product gap)

### Slice A: operator identity + per-org scoping (S9)

This is the architectural lift, and the reason it's not earlier is that it
touches every handler — doing it before the test-integrity slice would be
re-flighting the fleet with the instruments turned off.

- Operators table, login (or token issuance) at the control plane; requests
  carry an operator identity, not a shared secret.
- **Org membership as an authorization scope:** every org-addressed route
  checks it; id-addressed routes resolve the owning org and check it. The
  store already scopes everything node/org-side — this closes the operator
  side to match.
- CLI gains a login flow; `~/.config/navarch/` stores the operator token
  where the shared token lived, precedence preserved.
- Bootstrap: the first operator is created by an install-time flow (env var
  on first boot is acceptable; a seeded migration UUID is not, for the
  reason `POST /v1/orgs` is self-serve).
- Agent/node tokens are already per-node and correctly implemented
  (hash-at-rest, constant-time) — they stay as they are.
- **Audit trail:** events already exist; operator identity goes into every
  event written from a handler, so the audit log becomes attributable.

### Slice B: seal the tenant edges the audit named

- S7 completes: registration cannot repoint an age recipient silently once
  operators are identifiable (rotate becomes an operator action).
- Decide and document the multi-org node-pool question CLAUDE.md defers to
  this point: nodes are org-scoped today; either keep it (single-tenant
  fleets per org) or design shared-node pools with placement isolation.
  Not building it is a valid answer; not deciding is not.

### Slice C: transport (S4)

TLS terminates at a reverse proxy in front of the control plane for any
non-loopback deployment; document the compose pattern (Caddy/Traefik with
ACME is already in the stack's vocabulary). The client and agent get a
guard: refuse a non-TLS base URL outside loopback/`.internal`/compose
networks unless `COMPOSECTL_INSECURE=1`. A captured node token reads
ciphertext forever; that stops being a dev-network-only caveat the day
someone deploys on a shared network, and a warning at startup is cheaper
than an incident.

**Exit criteria for Sprint 7:** two operators in different orgs cannot see
each other's anything through the CLI or API (a test fixture with two orgs
asserting cross-reads 404); the audit log names actors; the TLS posture is
documented and warned.

---

## Sprint 8 — Packaging and the 1.0 line

1. **Release engineering:** `make release` producing versioned `navarch`
   binaries (the module rename decision — `github.com/craig/composectl` →
   navarch — is *this* slice's one atomic commit, or it never happens; pick
   deliberately, per the naming table's "one atomic commit whenever it's
   wanted").
2. **Deployment story:** the Lightsail compose file for the control plane
   itself (Postgres, control plane, agent, router), upgrade path documented
   — migrations are immutable, so an upgrade is pull + `migrate up` +
   restart, and that sequence needs to be written down and tested once
   against a snapshot volume.
3. **The deliberate non-features get named in the README** as such, the way
   CLAUDE.md's naming table does: no auto-failover (operator-initiated by
   design), `retired` set by nothing yet, no durable logs, no cross-node
   overlay. A finish line that doesn't say what the product *doesn't* do
   invites someone to discover it in production.
4. **Known-good baselines refreshed** (parser digest, demo suite from
   `make nuke`) and asserted in CI so 1.0 has a fingerprint.
5. **Docs truthing final pass:** CLAUDE.md sprint sections collapse into a
   short history; the invariants sections stay — they are the product's
   real documentation.

**Exit criteria (the finish line):** a person who is not Craig can, from a
clean machine, install `navarch`, log in, create an org/app/stack, deploy,
watch it roll out, read logs, expire a preview, drain a node — and every
step they take is covered by a test that cannot silently skip, on an
identity that is theirs.

---

## Explicitly not in scope for 1.0 (say no early)

- **NOTIFY push wakeups** (Slice A leftover): the trigger exists, nothing
  consumes it, polling at 2s is correct for the fleet sizes in question.
- **Auto-failover / a policy loop that sets `retired`**: CLAUDE.md's
  reasoning stands — a policy with no operational history is a guess with a
  cron schedule.
- **The `cc-` prefix rename**: stays behind its `make nuke` boundary,
  untouched, per the naming table.
- **Durable log storage**: the design says no, and the audit found the
  design holding; content stays in memory, bounded, never at rest.
