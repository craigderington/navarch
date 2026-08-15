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

	"github.com/craig/composectl/internal/metrics"
	"github.com/craig/composectl/internal/store"
	"github.com/google/uuid"
)

type Server struct {
	st            *store.Store
	log           *slog.Logger
	mux           *http.ServeMux
	previewDomain string
	bearerToken   string
	metrics       *metrics.Registry
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

func WithMetrics(reg *metrics.Registry) ServerOption {
	return func(s *Server) { s.metrics = reg }
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
	if r.URL.Path != "/healthz" && s.bearerToken != "" && !s.authorized(r) {
		rw.Header().Set("WWW-Authenticate", `Bearer realm="composectl"`)
		writeError(rw, http.StatusUnauthorized, "authentication required", nil)
		if s.metrics != nil {
			s.metrics.ObserveHTTP(r.Method, "unauthenticated", rw.status)
		}
		return
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

// authorized accepts the operator API token for catalog and register, and a
// per-node token only for that node's heartbeat / desired-state / report.
func (s *Server) authorized(r *http.Request) bool {
	// Agent endpoints accept only that node's token so the operator token
	// cannot be used to pull another node's desired-state ciphertext.
	if id, ok := nodeAgentPathID(r); ok {
		plain := bearerToken(r)
		if plain == "" {
			return false
		}
		ctx, cancel := context.WithTimeout(r.Context(), 2*time.Second)
		defer cancel()
		ok, err := s.st.NodeTokenValid(ctx, id, plain)
		return err == nil && ok
	}
	return validBearerToken(r, s.bearerToken)
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
	case r.Method == http.MethodPost && strings.HasSuffix(r.URL.Path, "/report"):
		want = "report"
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

// routes uses Go 1.22+ method-and-wildcard patterns, so no router
// dependency is needed.
func (s *Server) routes() {
	s.mux.HandleFunc("GET /healthz", s.handleHealth)
	s.mux.HandleFunc("GET /metrics", s.handleMetrics)

	// Organizations — the root of the catalog. Without a way to create one
	// nothing below it is reachable, so this is deliberately self-serve
	// rather than a seeded row (migrations are immutable, a seeded UUID
	// would be permanent).
	s.mux.HandleFunc("POST /v1/orgs", s.handleCreateOrg)
	s.mux.HandleFunc("GET /v1/orgs", s.handleListOrgs)
	s.mux.HandleFunc("GET /v1/orgs/{org}/events", s.handleListEvents)

	// Applications
	s.mux.HandleFunc("POST /v1/orgs/{org}/apps", s.handleCreateApp)
	s.mux.HandleFunc("GET /v1/orgs/{org}/apps", s.handleListApps)

	// Stacks — POST accepts a compose file, parses it, stores a version
	s.mux.HandleFunc("POST /v1/apps/{app}/stacks", s.handleCreateStack)
	s.mux.HandleFunc("GET /v1/apps/{app}/stacks", s.handleListStacks)
	s.mux.HandleFunc("GET /v1/stacks/{stack}", s.handleGetStack)
	s.mux.HandleFunc("POST /v1/stacks/{stack}/versions", s.handleCreateStackVersion)
	s.mux.HandleFunc("GET /v1/stacks/{stack}/versions", s.handleListStackVersions)

	// Validation without persistence — the CLI's `composectl validate`
	s.mux.HandleFunc("POST /v1/validate", s.handleValidate)

	// Environments
	s.mux.HandleFunc("POST /v1/stacks/{stack}/envs", s.handleCreateEnv)
	s.mux.HandleFunc("GET /v1/stacks/{stack}/envs", s.handleListEnvs)
	s.mux.HandleFunc("GET /v1/envs/{env}", s.handleGetEnv)
	s.mux.HandleFunc("POST /v1/stacks/{stack}/previews", s.handleCreatePreview)

	// Deployments
	s.mux.HandleFunc("POST /v1/envs/{env}/deployments", s.handleCreateDeployment)
	s.mux.HandleFunc("GET /v1/envs/{env}/deployments", s.handleListDeployments)
	s.mux.HandleFunc("GET /v1/deployments/{id}", s.handleGetDeployment)
	s.mux.HandleFunc("POST /v1/deployments/{id}/promote", s.handlePromote)
	s.mux.HandleFunc("POST /v1/envs/{env}/rollback", s.handleRollback)

	// Nodes — agent-facing
	s.mux.HandleFunc("POST /v1/nodes/register", s.handleRegisterNode)
	s.mux.HandleFunc("POST /v1/nodes/{id}/heartbeat", s.handleHeartbeat)
	s.mux.HandleFunc("GET /v1/nodes/{id}/desired-state", s.handleDesiredState)
	s.mux.HandleFunc("POST /v1/nodes/{id}/report", s.handleInstanceReport)
	s.mux.HandleFunc("GET /v1/nodes", s.handleListNodes)
	s.mux.HandleFunc("GET /v1/nodes/{id}", s.handleGetNode)
	s.mux.HandleFunc("POST /v1/nodes/{id}/drain", s.handleDrainNode)

	// Secrets — encrypted at rest, plaintext never stored. The list response
	// never includes values; see internal/secrets for the encrypt boundary.
	s.mux.HandleFunc("POST /v1/envs/{env}/secrets", s.handleSetSecret)
	s.mux.HandleFunc("GET /v1/envs/{env}/secrets", s.handleListSecrets)
	s.mux.HandleFunc("DELETE /v1/envs/{env}/secrets/{key}", s.handleDeleteSecret)
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
