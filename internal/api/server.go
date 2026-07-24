// Package api is the control plane HTTP surface. Handlers are thin:
// decode, delegate to store/parser, encode. No SQL, no compose knowledge.
package api

import (
	"context"
	"encoding/json"
	"errors"
	"log/slog"
	"net/http"
	"time"

	"github.com/craig/composectl/internal/store"
)

type Server struct {
	st  *store.Store
	log *slog.Logger
	mux *http.ServeMux
}

func NewServer(st *store.Store, log *slog.Logger) *Server {
	s := &Server{st: st, log: log, mux: http.NewServeMux()}
	s.routes()
	return s
}

func (s *Server) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	s.mux.ServeHTTP(w, r)
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

	// Organizations — the root of the catalog. Without a way to create one
	// nothing below it is reachable, so this is deliberately self-serve
	// rather than a seeded row (migrations are immutable, a seeded UUID
	// would be permanent).
	s.mux.HandleFunc("POST /v1/orgs", s.handleCreateOrg)
	s.mux.HandleFunc("GET /v1/orgs", s.handleListOrgs)

	// Applications
	s.mux.HandleFunc("POST /v1/orgs/{org}/apps", s.handleCreateApp)
	s.mux.HandleFunc("GET /v1/orgs/{org}/apps", s.handleListApps)

	// Stacks — POST accepts a compose file, parses it, stores a version
	s.mux.HandleFunc("POST /v1/apps/{app}/stacks", s.handleCreateStack)
	s.mux.HandleFunc("POST /v1/stacks/{stack}/versions", s.handleCreateStackVersion)
	s.mux.HandleFunc("GET /v1/stacks/{stack}/versions", s.handleListStackVersions)

	// Validation without persistence — the CLI's `composectl validate`
	s.mux.HandleFunc("POST /v1/validate", s.handleValidate)

	// Environments
	s.mux.HandleFunc("POST /v1/stacks/{stack}/envs", s.handleCreateEnv)
	s.mux.HandleFunc("GET /v1/stacks/{stack}/envs", s.handleListEnvs)

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

	// Secrets — encrypted at rest, plaintext never stored. The list response
	// never includes values; see internal/secrets for the encrypt boundary.
	s.mux.HandleFunc("POST /v1/envs/{env}/secrets", s.handleSetSecret)
	s.mux.HandleFunc("GET /v1/envs/{env}/secrets", s.handleListSecrets)
	s.mux.HandleFunc("DELETE /v1/envs/{env}/secrets/{key}", s.handleDeleteSecret)
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
