// Package api is the control plane HTTP surface. Handlers are thin:
// decode, delegate to store/parser, encode. No SQL, no compose knowledge.
package api

import (
	"context"
	"crypto/subtle"
	"encoding/json"
	"errors"
	"log/slog"
	"net/http"
	"strings"
	"time"

	"github.com/craigderington/navarch/internal/logbuf"
	"github.com/craigderington/navarch/internal/metrics"
	"github.com/craigderington/navarch/internal/store"
	"github.com/google/uuid"
)

type Server struct {
	st            *store.Store
	log           *slog.Logger
	mux           *http.ServeMux
	previewDomain string
	bearerToken   string
	metrics       *metrics.Registry
	// logs holds delivered container output in memory and nowhere else. It is
	// not a cache of something durable — there is no durable copy, on purpose.
	logs *logbuf.Buffer
	// patterns records every route as it is registered. It exists so the
	// org-scoping test can enumerate the real surface rather than be handed a
	// hand-written list, which would drift the moment someone adds a route and
	// would then pass by omitting exactly the route nobody checked.
	patterns []string
	// requireJoinToken refuses node registration on the shared service token.
	// Off by default so an existing single-tenant install upgrades without its
	// agents failing to re-register; on for anything multi-tenant, where a
	// credential that can enrol into any org must not exist.
	requireJoinToken bool
	// routeStrand is how long a node may go unheard from before its routes are
	// withdrawn. The server needs it for the same reason the controller does:
	// GET /v1/nodes/{id}/routes answers the same question the control-plane
	// router asks, and two different answers would be worse than either.
	routeStrand time.Duration
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

// WithBearerToken protects every /v1 endpoint with one shared bearer token.
// Health remains public so container and load-balancer probes need no secret.
// An empty token leaves authentication disabled for in-process tests only;
// the control-plane config rejects an empty token at startup.
func WithBearerToken(token string) ServerOption {
	return func(s *Server) { s.bearerToken = token }
}

// WithRequireJoinToken refuses node registration on the shared service token,
// so every node enrols with a credential that names exactly one organization.
// WithRouteStrand sets the staleness window used when answering a node's route
// request, matching the controller's.
func WithRouteStrand(d time.Duration) ServerOption {
	return func(s *Server) { s.routeStrand = d }
}

func WithRequireJoinToken(v bool) ServerOption {
	return func(s *Server) { s.requireJoinToken = v }
}

func WithMetrics(reg *metrics.Registry) ServerOption {
	return func(s *Server) { s.metrics = reg }
}

// WithLogBuffer supplies the in-memory buffer that delivered log chunks land in.
// Without one the log endpoints still work and simply return nothing, which is
// the right degradation: a control plane that cannot buffer output should say it
// has none rather than refuse the request or, worse, start persisting it.
func WithLogBuffer(b *logbuf.Buffer) ServerOption {
	return func(s *Server) { s.logs = b }
}

func NewServer(st *store.Store, log *slog.Logger, opts ...ServerOption) *Server {
	s := &Server{st: st, log: log, mux: http.NewServeMux(), previewDomain: DefaultPreviewDomain}
	for _, o := range opts {
		o(s)
	}
	s.routes()
	return s
}

func (s *Server) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	rw := &statusWriter{ResponseWriter: w, status: http.StatusOK}
	if r.URL.Path != "/healthz" && s.bearerToken != "" {
		id, ok := s.authenticate(r)
		if !ok {
			rw.Header().Set("WWW-Authenticate", `Bearer realm="composectl"`)
			writeError(rw, http.StatusUnauthorized, "authentication required", nil)
			if s.metrics != nil {
				s.metrics.ObserveHTTP(r.Method, "unauthenticated", rw.status)
			}
			return
		}
		// The identity travels on the request context so handlers can scope
		// themselves without re-reading the Authorization header — and so a
		// handler that forgets to is a missing call, not a silently different
		// interpretation of the same header.
		ctx := withIdentity(r.Context(), id)
		// Tagging the same context with the actor is what makes the audit log
		// attributable. Done once here rather than at each of the dozen store
		// methods that append an event: handlers derive their contexts from
		// this one, so an event written anywhere below inherits the actor
		// without a signature carrying it. Machine identities add nothing,
		// which is correct — an agent's report has no human behind it.
		if actorID, email := id.actor(); actorID != nil {
			ctx = store.WithActor(ctx, *actorID, email)
		}
		r = r.WithContext(ctx)
	}
	s.mux.ServeHTTP(rw, r)
	if s.metrics != nil {
		route := r.Pattern
		if route == "" {
			route = "unmatched"
		}
		s.metrics.ObserveHTTP(r.Method, route, rw.status)
	}
}

// authenticate resolves the credential on the request to an identity. It
// answers who, never what they may touch — see identity.go for why that split
// is load-bearing rather than tidy.
func (s *Server) authenticate(r *http.Request) (identity, bool) {
	// Agent endpoints accept only that node's own token, so no other
	// credential can pull a node's desired-state ciphertext.
	if nodeID, ok := nodeAgentPathID(r); ok {
		plain := bearerToken(r)
		if plain == "" {
			return identity{}, false
		}
		ctx, cancel := context.WithTimeout(r.Context(), 2*time.Second)
		defer cancel()
		valid, err := s.st.NodeTokenValid(ctx, nodeID, plain)
		if err != nil || !valid {
			return identity{}, false
		}
		return identity{kind: identityNode, nodeID: nodeID}, true
	}

	if bearerToken(r) == "" {
		return identity{}, false
	}

	// The shared token, on the two routes it still opens. Checked before the
	// operator lookup so node enrolment and metrics scraping cost no database
	// round trip, and so a fleet restart cannot be slowed by the operators
	// table being busy.
	if isServicePath(r) && validBearerToken(r, s.bearerToken) {
		return identity{kind: identityService}, true
	}

	// A join token, on the one route it opens. Redeeming here rather than in
	// the handler is deliberate: the redeem is a single atomic statement, so
	// max_uses holds when two agents start at once — whereas a check here and
	// an increment later would let both through a single-use token. The cost is
	// that a registration which then fails for some other reason has still
	// spent a use, which is the safe direction for a credential whose whole
	// point may be that it works once.
	if isNodeRegistration(r) && s.st != nil {
		ctx, cancel := context.WithTimeout(r.Context(), 3*time.Second)
		defer cancel()
		if orgID, err := s.st.RedeemJoinToken(ctx, bearerToken(r)); err == nil {
			return identity{kind: identityJoin, orgID: orgID}, true
		}
	}

	if s.st == nil {
		// Only reachable from an in-package test that builds a Server without
		// a store. There is no credential to check against, so the answer is
		// no — a nil store must never be a way past authentication.
		return identity{}, false
	}
	ctx, cancel := context.WithTimeout(r.Context(), 2*time.Second)
	defer cancel()
	op, err := s.st.OperatorForToken(ctx, bearerToken(r))
	if err != nil {
		// Unknown, expired and disabled are all one answer here. Telling a
		// caller which of its guesses named a real token is a gift.
		return identity{}, false
	}
	return identity{kind: identityOperator, operator: op}, true
}

// isServicePath reports whether r targets one of the two machine-to-machine
// routes the shared token still opens. Exact matches only: a prefix test would
// hand the shared token every route that happens to start the same way.
// isNodeRegistration is the one route a join token opens.
func isNodeRegistration(r *http.Request) bool {
	return r.Method == http.MethodPost && r.URL.Path == "/v1/nodes/register"
}

func isServicePath(r *http.Request) bool {
	switch {
	case isNodeRegistration(r):
		return true
	case r.Method == http.MethodGet && r.URL.Path == "/metrics":
		return true
	}
	return false
}

// nodeAgentPathID reports whether r targets a per-node agent endpoint, and
// the node id it names.
//
// The id is parsed out of the path here rather than read from
// r.PathValue("id"). Authorization runs in ServeHTTP, *before* the request
// reaches the mux, and PathValue is only populated once the mux has matched
// the request against a registered pattern — so it is empty at this point.
// Reading it here made uuid.Parse fail on every request and returned 401 for
// heartbeat, desired-state and report unconditionally, leaving an agent able
// to register and do nothing else. The api tests did not catch it because
// they construct a Server with no bearer token, which skips authorization in
// ServeHTTP entirely.
func nodeAgentPathID(r *http.Request) (uuid.UUID, bool) {
	var want string
	switch {
	case r.Method == http.MethodPost && strings.HasSuffix(r.URL.Path, "/heartbeat"):
		want = "heartbeat"
	case r.Method == http.MethodGet && strings.HasSuffix(r.URL.Path, "/desired-state"):
		want = "desired-state"
	case r.Method == http.MethodGet && strings.HasSuffix(r.URL.Path, "/routes") &&
		strings.HasPrefix(r.URL.Path, "/v1/nodes/"):
		// A node asking for its org's routes, so it can configure a router
		// running beside it. Node token only: an operator token reaching here
		// would be a way to read another org's hostnames.
		want = "routes"
	case r.Method == http.MethodPost && strings.HasSuffix(r.URL.Path, "/report"):
		want = "report"
	case r.Method == http.MethodPost && strings.HasSuffix(r.URL.Path, "/logs") &&
		strings.HasPrefix(r.URL.Path, "/v1/nodes/"):
		// Only the node-scoped delivery path. The operator-facing
		// /v1/envs/{env}/logs also ends in /logs and must stay on the operator
		// token — hence the prefix check, without which opening a tail would
		// demand a node token nobody has.
		want = "logs"
	default:
		return uuid.Nil, false
	}
	// /v1/nodes/{id}/{want} -> ["", "v1", "nodes", "{id}", "{want}"]
	parts := strings.Split(r.URL.Path, "/")
	if len(parts) != 5 || parts[1] != "v1" || parts[2] != "nodes" || parts[4] != want {
		return uuid.Nil, false
	}
	id, err := uuid.Parse(parts[3])
	if err != nil {
		// A malformed id on an agent path must stay on the agent branch and
		// fail, never fall through to the operator-token check — that would
		// let the shared token reach a node endpoint.
		return uuid.Nil, true
	}
	return id, true
}

func bearerToken(r *http.Request) string {
	const prefix = "Bearer "
	auth := r.Header.Get("Authorization")
	if len(auth) <= len(prefix) || auth[:len(prefix)] != prefix {
		return ""
	}
	return auth[len(prefix):]
}

type statusWriter struct {
	http.ResponseWriter
	status      int
	wroteHeader bool
}

func (w *statusWriter) WriteHeader(code int) {
	if w.wroteHeader {
		return
	}
	w.wroteHeader = true
	w.status = code
	w.ResponseWriter.WriteHeader(code)
}
func (w *statusWriter) Write(b []byte) (int, error) {
	if !w.wroteHeader {
		w.WriteHeader(http.StatusOK)
	}
	return w.ResponseWriter.Write(b)
}

func validBearerToken(r *http.Request, want string) bool {
	const prefix = "Bearer "
	auth := r.Header.Get("Authorization")
	if len(auth) <= len(prefix) || auth[:len(prefix)] != prefix {
		return false
	}
	got := auth[len(prefix):]
	return len(got) == len(want) && subtle.ConstantTimeCompare([]byte(got), []byte(want)) == 1
}

// BootstrapDevOrg ensures the dev org the local agent registers into exists.
// Called at startup; idempotent, so a duplicate slug on restart is fine.
func (s *Server) BootstrapDevOrg(ctx context.Context) {
	if _, err := s.st.CreateOrganization(ctx, "dev", "Development"); err != nil &&
		!errors.Is(err, store.ErrConflict) {
		s.log.Warn("could not bootstrap dev org", "err", err)
	}
}

// BootstrapOperator ensures a first operator exists, so a fresh install has
// somebody who can log in. Idempotent, like BootstrapDevOrg: an existing email
// is a no-op, so a restart never mints a second credential.
//
// The first operator comes from the environment rather than from a seeded
// migration, for the same reason POST /v1/orgs is self-serve: a constant baked
// into a migration is permanent and identical on every install, which is the
// one property a root credential must not have.
//
// If token is set, it is pinned rather than generated. That is for the dev
// stack, where compose, the Makefile and the demo scripts share a constant and
// the alternative is scraping a generated value out of a log line on every
// `make up`. Production leaves it empty and gets crypto/rand.
func (s *Server) BootstrapOperator(ctx context.Context, email, token string) {
	if email == "" {
		return
	}
	op, err := s.st.GetOperatorByEmail(ctx, email)
	if errors.Is(err, store.ErrNotFound) {
		op, err = s.st.CreateOperator(ctx, email, email)
	}
	if err != nil {
		s.log.Warn("could not bootstrap operator", "email", email, "err", err)
		return
	}
	// The dev org is where the local agents register and where the demos
	// deploy, so the bootstrap operator has to be able to see it. Any other org
	// they create themselves, and POST /v1/orgs makes its creator an owner.
	if org, orgErr := s.st.GetOrganizationBySlug(ctx, "dev"); orgErr == nil {
		if err := s.st.AddOrgMember(ctx, org.ID, op.ID, "owner"); err != nil {
			s.log.Warn("could not add bootstrap operator to the dev org", "err", err)
		}
	}

	if token != "" {
		if err := s.st.EnsureOperatorToken(ctx, op.ID, "bootstrap", token); err != nil {
			s.log.Warn("could not pin the bootstrap operator token", "err", err)
		}
		return
	}

	existing, err := s.st.ListOperatorTokens(ctx, op.ID)
	if err != nil {
		s.log.Warn("could not read bootstrap operator tokens", "err", err)
		return
	}
	if len(existing) > 0 {
		return
	}
	t, err := s.st.IssueOperatorToken(ctx, op.ID, "bootstrap", nil)
	if err != nil {
		s.log.Warn("could not issue the bootstrap operator token", "err", err)
		return
	}
	// The one place this codebase deliberately logs a secret. Everything else
	// goes to great lengths not to -- logbuf will not even log a chunk -- but a
	// generated root credential that is never shown is an install nobody can
	// use, and the alternatives (a file the operator may not be able to read, a
	// fixed default) are worse. It happens once, on first boot, for a token the
	// operator should replace.
	s.log.Info("bootstrap operator created — this token is shown once and never again",
		"email", email, "token", t.Plaintext)
}

// handle registers a route and records its pattern. Every route goes through
// here rather than calling s.mux directly, so `patterns` cannot fall behind the
// mux.
func (s *Server) handle(pattern string, h http.HandlerFunc) {
	s.patterns = append(s.patterns, pattern)
	s.mux.HandleFunc(pattern, h)
}

// Patterns returns every registered route pattern. Exported for tests in this
// package's own test binary; nothing in the running server reads it.
func (s *Server) Patterns() []string { return append([]string(nil), s.patterns...) }

// BootstrapJoinToken pins a join token for an org, so the dev stack's agents
// can enrol with a credential that names exactly one organization instead of
// the shared service token. Idempotent on the token value, like the bootstrap
// operator's — a restart does not mint a second one.
func (s *Server) BootstrapJoinToken(ctx context.Context, orgSlug, token string) {
	if token == "" {
		return
	}
	org, err := s.st.GetOrganizationBySlug(ctx, orgSlug)
	if err != nil {
		s.log.Warn("could not bootstrap join token", "org", orgSlug, "err", err)
		return
	}
	if err := s.st.EnsureJoinToken(ctx, org.ID, "bootstrap", token); err != nil {
		s.log.Warn("could not pin the bootstrap join token", "err", err)
	}
}

// routes uses Go 1.22+ method-and-wildcard patterns, so no router
// dependency is needed.
func (s *Server) routes() {
	s.handle("GET /healthz", s.handleHealth)
	s.handle("GET /metrics", s.handleMetrics)

	// Organizations — the root of the catalog. Without a way to create one
	// nothing below it is reachable, so this is deliberately self-serve
	// rather than a seeded row (migrations are immutable, a seeded UUID
	// would be permanent).
	// Operator identity: who am I, and who else may see this org.
	s.handle("GET /v1/whoami", s.handleWhoami)
	s.handle("GET /v1/operators/me/tokens", s.handleListTokens)
	s.handle("POST /v1/operators/me/tokens", s.handleCreateToken)
	s.handle("DELETE /v1/operators/me/tokens/{id}", s.handleRevokeToken)
	s.handle("GET /v1/orgs/{org}/members", s.handleListMembers)
	s.handle("POST /v1/orgs/{org}/members", s.handleAddMember)
	s.handle("DELETE /v1/orgs/{org}/members/{operator}", s.handleRemoveMember)

	s.handle("POST /v1/orgs", s.handleCreateOrg)
	s.handle("GET /v1/orgs", s.handleListOrgs)
	s.handle("GET /v1/orgs/{org}/events", s.handleListEvents)

	// Applications
	s.handle("POST /v1/orgs/{org}/apps", s.handleCreateApp)
	s.handle("GET /v1/orgs/{org}/apps", s.handleListApps)
	s.handle("GET /v1/orgs/{org}/environments", s.handleListOrgEnvs)

	// Stacks — POST accepts a compose file, parses it, stores a version
	s.handle("POST /v1/apps/{app}/stacks", s.handleCreateStack)
	s.handle("GET /v1/apps/{app}/stacks", s.handleListStacks)
	s.handle("GET /v1/stacks/{stack}", s.handleGetStack)
	s.handle("POST /v1/stacks/{stack}/versions", s.handleCreateStackVersion)
	s.handle("GET /v1/stacks/{stack}/versions", s.handleListStackVersions)

	// Validation without persistence — the CLI's `composectl validate`
	s.handle("POST /v1/validate", s.handleValidate)

	// Environments
	s.handle("POST /v1/stacks/{stack}/envs", s.handleCreateEnv)
	s.handle("GET /v1/stacks/{stack}/envs", s.handleListEnvs)
	s.handle("GET /v1/envs/{env}", s.handleGetEnv)
	s.handle("POST /v1/stacks/{stack}/previews", s.handleCreatePreview)

	// Deployments
	s.handle("POST /v1/envs/{env}/deployments", s.handleCreateDeployment)
	s.handle("GET /v1/envs/{env}/deployments", s.handleListDeployments)
	s.handle("GET /v1/deployments/{id}", s.handleGetDeployment)
	s.handle("POST /v1/deployments/{id}/promote", s.handlePromote)
	s.handle("POST /v1/envs/{env}/rollback", s.handleRollback)

	// Nodes — agent-facing
	s.handle("POST /v1/nodes/register", s.handleRegisterNode)
	s.handle("POST /v1/nodes/{id}/heartbeat", s.handleHeartbeat)
	s.handle("GET /v1/nodes/{id}/desired-state", s.handleDesiredState)
	s.handle("GET /v1/nodes/{id}/routes", s.handleNodeRoutes)
	s.handle("POST /v1/nodes/{id}/report", s.handleInstanceReport)
	s.handle("GET /v1/nodes", s.handleListNodes)
	s.handle("GET /v1/nodes/{id}", s.handleGetNode)
	s.handle("POST /v1/nodes/{id}/logs", s.handleLogDelivery)
	s.handle("POST /v1/nodes/{id}/drain", s.handleDrainNode)
	s.handle("POST /v1/nodes/{id}/uncordon", s.handleUncordonNode)
	s.handle("POST /v1/nodes/{id}/rotate-recipient", s.handleRotateNodeRecipient)
	s.handle("GET /v1/orgs/{org}/join-tokens", s.handleListJoinTokens)
	s.handle("POST /v1/orgs/{org}/join-tokens", s.handleCreateJoinToken)
	s.handle("DELETE /v1/orgs/{org}/join-tokens/{id}", s.handleRevokeJoinToken)

	// Secrets — encrypted at rest, plaintext never stored. The list response
	// never includes values; see internal/secrets for the encrypt boundary.
	s.handle("POST /v1/envs/{env}/secrets", s.handleSetSecret)
	s.handle("GET /v1/envs/{env}/secrets", s.handleListSecrets)
	s.handle("DELETE /v1/envs/{env}/secrets/{key}", s.handleDeleteSecret)

	// Logs — a request is an instruction; the answer lives in memory only.
	s.handle("POST /v1/envs/{env}/logs", s.handleCreateLogRequest)
	s.handle("GET /v1/logs/{id}", s.handleGetLogs)
	s.handle("DELETE /v1/logs/{id}", s.handleCloseLogRequest)
}

func (s *Server) handleMetrics(w http.ResponseWriter, r *http.Request) {
	if s.metrics == nil {
		http.NotFound(w, r)
		return
	}
	ctx, cancel := contextWithTimeout(r, 3*time.Second)
	defer cancel()
	g, err := s.st.OperationalGauges(ctx)
	if err != nil {
		g = metrics.Gauges{DatabaseUp: false, Deployments: map[string]int64{}}
	}
	w.Header().Set("Content-Type", "text/plain; version=0.0.4; charset=utf-8")
	s.metrics.WritePrometheus(w, g)
}

func (s *Server) handleHealth(w http.ResponseWriter, r *http.Request) {
	ctx, cancel := contextWithTimeout(r, 2*time.Second)
	defer cancel()
	if err := s.st.Pool().Ping(ctx); err != nil {
		writeError(w, http.StatusServiceUnavailable, "database unreachable", nil)
		return
	}
	writeJSON(w, http.StatusOK, map[string]string{"status": "ok"})
}

// ------------------------------------------------------------- responses

type errorBody struct {
	Error   string `json:"error"`
	Details any    `json:"details,omitempty"`
}

func writeJSON(w http.ResponseWriter, code int, v any) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(code)
	if v != nil {
		_ = json.NewEncoder(w).Encode(v)
	}
}

func writeError(w http.ResponseWriter, code int, msg string, details any) {
	writeJSON(w, code, errorBody{Error: msg, Details: details})
}

// writeStoreError maps store sentinels to status codes so handlers don't
// repeat the switch.
func (s *Server) writeStoreError(w http.ResponseWriter, err error) {
	switch {
	case errors.Is(err, store.ErrNotFound):
		writeError(w, http.StatusNotFound, "not found", nil)
	case errors.Is(err, store.ErrConflict):
		writeError(w, http.StatusConflict, err.Error(), nil)
	case errors.Is(err, store.ErrInvalid):
		// The message names the offending field, so it is safe and useful
		// to return verbatim.
		writeError(w, http.StatusBadRequest, err.Error(), nil)
	default:
		s.log.Error("internal error", "err", err)
		writeError(w, http.StatusInternalServerError, "internal error", nil)
	}
}

func decodeJSON(r *http.Request, v any) error {
	dec := json.NewDecoder(http.MaxBytesReader(nil, r.Body, 1<<20))
	dec.DisallowUnknownFields()
	return dec.Decode(v)
}
