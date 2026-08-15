package api

import (
	"errors"
	"io"
	"net/http"
	"strconv"
	"time"

	"github.com/google/uuid"

	"github.com/craig/composectl/internal/parser"
	"github.com/craig/composectl/internal/store"
)

// Handlers for the catalog hierarchy: organization → application → stack →
// stack version → environment. Together they are what makes the deployment
// endpoints reachable over HTTP rather than only from psql.

// ------------------------------------------------------------ organizations

type createOrgRequest struct {
	Slug string `json:"slug"`
	Name string `json:"name"`
}

func (s *Server) handleCreateOrg(w http.ResponseWriter, r *http.Request) {
	ctx, cancel := contextWithTimeout(r, 5*time.Second)
	defer cancel()

	var req createOrgRequest
	if err := decodeJSON(r, &req); err != nil {
		writeError(w, http.StatusBadRequest, "invalid request body", nil)
		return
	}

	org, err := s.st.CreateOrganization(ctx, req.Slug, req.Name)
	if err != nil {
		s.writeStoreError(w, err)
		return
	}
	writeJSON(w, http.StatusCreated, org)
}

func (s *Server) handleListOrgs(w http.ResponseWriter, r *http.Request) {
	ctx, cancel := contextWithTimeout(r, 5*time.Second)
	defer cancel()

	orgs, err := s.st.ListOrganizations(ctx)
	if err != nil {
		s.writeStoreError(w, err)
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"organizations": orgs})
}

// ------------------------------------------------------------- applications

type createAppRequest struct {
	Slug string `json:"slug"`
	Name string `json:"name"`
}

func (s *Server) handleCreateApp(w http.ResponseWriter, r *http.Request) {
	ctx, cancel := contextWithTimeout(r, 5*time.Second)
	defer cancel()

	orgID, ok := pathUUID(w, r, "org")
	if !ok {
		return
	}
	var req createAppRequest
	if err := decodeJSON(r, &req); err != nil {
		writeError(w, http.StatusBadRequest, "invalid request body", nil)
		return
	}

	app, err := s.st.CreateApplication(ctx, orgID, req.Slug, req.Name)
	if err != nil {
		s.writeStoreError(w, err)
		return
	}
	writeJSON(w, http.StatusCreated, app)
}

func (s *Server) handleListApps(w http.ResponseWriter, r *http.Request) {
	ctx, cancel := contextWithTimeout(r, 5*time.Second)
	defer cancel()

	orgID, ok := pathUUID(w, r, "org")
	if !ok {
		return
	}
	apps, err := s.st.ListApplications(ctx, orgID)
	if err != nil {
		s.writeStoreError(w, err)
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"applications": apps})
}

// ------------------------------------------------------------------ stacks

type createStackRequest struct {
	Slug string `json:"slug"`
}

func (s *Server) handleCreateStack(w http.ResponseWriter, r *http.Request) {
	ctx, cancel := contextWithTimeout(r, 5*time.Second)
	defer cancel()

	appID, ok := pathUUID(w, r, "app")
	if !ok {
		return
	}
	var req createStackRequest
	if err := decodeJSON(r, &req); err != nil {
		writeError(w, http.StatusBadRequest, "invalid request body", nil)
		return
	}

	stack, err := s.st.CreateStack(ctx, appID, req.Slug)
	if err != nil {
		s.writeStoreError(w, err)
		return
	}
	writeJSON(w, http.StatusCreated, stack)
}

func (s *Server) handleListStacks(w http.ResponseWriter, r *http.Request) {
	ctx, cancel := contextWithTimeout(r, 5*time.Second)
	defer cancel()
	appID, ok := pathUUID(w, r, "app")
	if !ok {
		return
	}
	if _, err := s.st.GetApplication(ctx, appID); err != nil {
		s.writeStoreError(w, err)
		return
	}
	stacks, err := s.st.ListStacks(ctx, appID)
	if err != nil {
		s.writeStoreError(w, err)
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"stacks": stacks})
}

func (s *Server) handleGetStack(w http.ResponseWriter, r *http.Request) {
	ctx, cancel := contextWithTimeout(r, 5*time.Second)
	defer cancel()
	id, ok := pathUUID(w, r, "stack")
	if !ok {
		return
	}
	stack, err := s.st.GetStack(ctx, id)
	if err != nil {
		s.writeStoreError(w, err)
		return
	}
	writeJSON(w, http.StatusOK, stack)
}

func (s *Server) handleGetEnv(w http.ResponseWriter, r *http.Request) {
	ctx, cancel := contextWithTimeout(r, 5*time.Second)
	defer cancel()
	id, ok := pathUUID(w, r, "env")
	if !ok {
		return
	}
	env, err := s.st.GetEnvironment(ctx, id)
	if err != nil {
		s.writeStoreError(w, err)
		return
	}
	writeJSON(w, http.StatusOK, env)
}

// ------------------------------------------------------------ stack versions

// handleCreateStackVersion takes the compose file as the raw request body,
// the same shape /v1/validate accepts, so a stack can be pushed with
// `curl --data-binary @compose.yaml`. Authorship rides along as a query
// parameter rather than forcing the body into a JSON envelope.
func (s *Server) handleCreateStackVersion(w http.ResponseWriter, r *http.Request) {
	ctx, cancel := contextWithTimeout(r, 15*time.Second)
	defer cancel()

	stackID, ok := pathUUID(w, r, "stack")
	if !ok {
		return
	}
	raw, err := io.ReadAll(http.MaxBytesReader(w, r.Body, 1<<20))
	if err != nil {
		writeError(w, http.StatusBadRequest, "cannot read body", nil)
		return
	}

	dspec, err := parser.Parse(ctx, raw, "stack")
	if err != nil {
		var verrs parser.ValidationErrors
		if errors.As(err, &verrs) {
			writeJSON(w, http.StatusUnprocessableEntity, errorBody{
				Error:   "compose file violates platform constraints",
				Details: verrs,
			})
			return
		}
		writeError(w, http.StatusBadRequest, err.Error(), nil)
		return
	}

	sv, err := s.st.CreateStackVersion(ctx, stackID, string(raw), dspec, r.URL.Query().Get("created_by"))
	if err != nil {
		s.writeStoreError(w, err)
		return
	}
	writeJSON(w, http.StatusCreated, sv)
}

func (s *Server) handleListStackVersions(w http.ResponseWriter, r *http.Request) {
	ctx, cancel := contextWithTimeout(r, 5*time.Second)
	defer cancel()

	stackID, ok := pathUUID(w, r, "stack")
	if !ok {
		return
	}
	limit, _ := strconv.Atoi(r.URL.Query().Get("limit"))

	versions, err := s.st.ListStackVersions(ctx, stackID, limit)
	if err != nil {
		s.writeStoreError(w, err)
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"versions": versions})
}

// ------------------------------------------------------------- environments

type createEnvRequest struct {
	Slug     string            `json:"slug"`
	Strategy string            `json:"strategy,omitempty"`
	Hostname string            `json:"hostname,omitempty"`
	Config   map[string]string `json:"config,omitempty"`
}

func (s *Server) handleCreateEnv(w http.ResponseWriter, r *http.Request) {
	ctx, cancel := contextWithTimeout(r, 5*time.Second)
	defer cancel()

	stackID, ok := pathUUID(w, r, "stack")
	if !ok {
		return
	}
	var req createEnvRequest
	if err := decodeJSON(r, &req); err != nil {
		writeError(w, http.StatusBadRequest, "invalid request body", nil)
		return
	}

	env, err := s.st.CreateEnvironment(ctx, store.CreateEnvironmentParams{
		StackID:  stackID,
		Slug:     req.Slug,
		Strategy: store.RolloutStrategy(req.Strategy),
		Hostname: req.Hostname,
		Config:   req.Config,
	})
	if err != nil {
		s.writeStoreError(w, err)
		return
	}
	writeJSON(w, http.StatusCreated, env)
}

func (s *Server) handleListEnvs(w http.ResponseWriter, r *http.Request) {
	ctx, cancel := contextWithTimeout(r, 5*time.Second)
	defer cancel()

	stackID, ok := pathUUID(w, r, "stack")
	if !ok {
		return
	}
	envs, err := s.st.ListEnvironments(ctx, stackID)
	if err != nil {
		s.writeStoreError(w, err)
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"environments": envs})
}

// pathUUID reads a UUID path parameter, writing a 400 and reporting false
// when it is not one. Every handler taking an id needs this, so it lives
// here rather than being repeated per handler.
func pathUUID(w http.ResponseWriter, r *http.Request, name string) (uuid.UUID, bool) {
	id, err := uuid.Parse(r.PathValue(name))
	if err != nil {
		writeError(w, http.StatusBadRequest, "invalid "+name+" id", nil)
		return uuid.Nil, false
	}
	return id, true
}
