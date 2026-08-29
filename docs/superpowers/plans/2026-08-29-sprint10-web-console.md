# Sprint 10 — The web console

**Goal:** a browser view of the fleet for people who will not live in a
terminal. What is running, who deployed it, what it did, and its logs.

**Why now:** everything today is CLI or API, and `navarch tui` is deliberately
read-only. That suits a CLI-first buyer and nobody else. A team evaluating a
deployment platform expects to open a page and see the state of their systems,
and right now the answer is "ssh in and run a TUI".

**Verified against:** `master` at `4f599f2`, 348 tests, ten demos green.

---

## The architecture decision, and it is the whole sprint

CLAUDE.md is explicit: *"a second front end is a second consumer of `client`,
not a second implementation of the protocol."* The TUI honours that — nothing in
`internal/tui` knows a URL or a JSON shape. A web console must honour it too,
and a browser cannot import a Go package. So the console is **a server**, not a
bundle of JavaScript talking to the API.

### `cmd/navarch-web`: a separate binary that renders HTML

It holds the operator's session, calls the control plane through
`internal/client`, and renders server-side templates. Three things fall out of
that, and each is the reason to prefer it:

**The browser never holds a bearer token.** A single-page app talking to
`/v1/...` directly would have to keep an operator token in JavaScript's reach —
in `localStorage`, where any injected script can read it, and on every request
from a context with no `SameSite` protection. The API has no cookie or session
auth and should not grow one for a front end: it is a machine-facing surface
whose credential model (bearer tokens, hashed at rest, constant-time compared)
is deliberate. The console keeps the token server-side and gives the browser a
session cookie that is useless anywhere else.

**The boundary holds exactly as written.** The console imports `internal/client`
and nothing else of the platform — no `store`, no `parser`, no Docker SDK. It is
a second consumer, the same shape as the TUI. That is checkable, and CI will
check it.

**It is optional.** A separate binary and a separate compose service means an
install that does not want a web surface simply does not run one, and the API is
not carrying template rendering it never uses for anyone else.

The cost is a third binary and a second deployable. That is the honest trade,
and it is smaller than the alternative: cookie auth, CSRF, and HTML rendering
inside `internal/api`, whose stated job is "decode, delegate, encode".

### Session handling

A login form takes an operator token — the same credential `navarch login`
takes — and verifies it with `client.Whoami` before storing anything, exactly as
the CLI does. A rejected token must never reach a session, for the same reason
it must never reach a config file: every later request then fails with 401 and
nothing points back at the step that reported success.

Sessions are in memory, keyed by a 32-byte random id, with the token as the
value. In memory because a restart logging everybody out is a correct and
cheap behaviour for a console, and persisting bearer tokens to disk to avoid it
is a bad trade. Cookie is `HttpOnly`, `SameSite=Lax`, `Secure` when the console
is reached over TLS, and `Path=/`.

**Slice A renders no form that mutates**, so CSRF has nothing to protect yet.
That is stated here so Slice B cannot forget it: the moment a POST exists, it
needs a per-session token in the form and checked on submit.

---

## Slice A — read-only console

Read-only first, and not out of timidity: the read surface is where the value
is, it needs no CSRF, and shipping it settles the session and layout questions
before any button can do damage. It mirrors how the TUI was built.

Pages, all server-rendered:

| Path | Shows |
|---|---|
| `/` | Fleet: nodes, state, capacity, key-rotation pending |
| `/orgs/{org}` | Environments: app, stack, env, hostname, home node, live revision |
| `/envs/{env}` | Deployment history — revision, state, slot, who, when, failure reason |
| `/deployments/{id}` | One rollout: state, spec digest, home node and its reachability |
| `/orgs/{org}/events` | The audit timeline, with the actor Sprint 7 added |
| `/envs/{env}/logs` | Container output, on demand, for one service |

Everything comes from `internal/client`. Where a method is missing the client
gains it — never the console reaching for HTTP itself.

**The design is not the marketing site.** That page is a chart; a console is an
instrument panel, read at a glance under pressure. Different job, different
treatment: dense tables, state encoded as colour *and* shape so it survives
being colour-blind or printed, monospace for anything that lines up, and no
decoration that costs a row of information.

## Slice B — the actions, behind confirmation

Deploy, promote, rollback, drain, uncordon, rotate-recipient. Each one:

- a POST with a per-session CSRF token,
- a confirmation naming the exact object and what will happen,
- and the result rendered from the API's answer, never assumed.

`navarch tui` **stays read-only** and `TestNoKeyPerformsAnAction` stays. That
rule was about the TUI specifically — a full-screen terminal app where a
keystroke is one character away from an accident. A web form with an explicit
confirmation is a different interaction, and the console is where acting is
appropriate. Say that out loud in CLAUDE.md rather than letting the two rules
look contradictory.

## Not in this sprint

- **A JavaScript framework.** Server-rendered HTML with a little progressive
  enhancement is enough for tables that refresh. A build pipeline for a console
  this size is cost with no return.
- **Multi-user session storage.** In-memory is right until there is more than
  one console process, and there is no reason for there to be.
- **Editing compose files in the browser.** The stack is pushed from a
  repository; a console that lets someone edit what is deployed without a commit
  is a way to lose the reproducibility the whole platform is built on.

## Exit criteria

- `go list -deps ./cmd/navarch-web` shows no pgx, no Docker SDK, no compose-go —
  asserted in CI beside the existing boundary guards.
- A session is created only from a token the control plane accepted.
- Every page renders from `internal/client`; the console contains no URL of the
  API's and no knowledge of its JSON.
- `make demo-web` logs in, loads each page against the dev fleet, and asserts
  the fleet page names a node the CLI also reports — so a page that renders an
  empty table cannot pass.
