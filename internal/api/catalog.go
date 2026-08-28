package api

import (
	"errors"
	"io"
	"net/http"
	"strconv"
	"time"

	"github.com/google/uuid"

	"github.com/craigderington/navarch/internal/parser"
	"github.com/craigderington/navarch/internal/store"
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
	// The creator becomes an owner in the same request, or they immediately
	// cannot see what they just made — every other route in the org would
	// answer 404 to the person who created it.
	if id, ok := identityFrom(r.Context()); ok && id.isOperator() {
		if err := s.st.AddOrgMember(ctx, org.ID, id.operator.ID, "owner"); err != nil {
			s.writeStoreError(w, err)
			return
		}
	}
	writeJSON(w, http.StatusCreated, org)
}

func (s *Server) handleListOrgs(w http.ResponseWriter, r *http.Request) {
	ctx, cancel := contextWithTimeout(r, 5*time.Second)
	defer cancel()

	// Scoped to membership. This is the one route where the old behaviour was
	// visibly wrong rather than merely permissive: it listed every tenant's
	// organizations to anyone holding the shared token, which is also how a
	// caller found the ids that made every other route reachable.
	var orgs []store.Organization
	var err error
	if id, ok := identityFrom(r.Context()); ok && id.isOperator() {
		orgs, err = s.st.OrgsForOperator(ctx, id.operator.ID)
	} else {
		// Authentication disabled: an in-process test server. See authz.go.
		orgs, err = s.st.ListOrganizations(ctx)
	}
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
	if !s.authorizeOrg(w, r, orgID) {
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
	if !s.authorizeOrg(w, r, orgID) {
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
	if !s.authorizeApp(w, r, appID) {
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
	if !s.authorizeApp(w, r, appID) {
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
	if !s.authorizeStack(w, r, id) {
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
	if !s.authorizeEnv(w, r, id) {
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
	if !s.authorizeStack(w, r, stackID) {
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
	if !s.authorizeStack(w, r, stackID) {
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
	if !s.authorizeStack(w, r, stackID) {
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
	if !s.authorizeStack(w, r, stackID) {
		return
	}
	envs, err := s.st.ListEnvironments(ctx, stackID)
	if err != nil {
		s.writeStoreError(w, err)
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"environments": envs})
}

// handleListOrgEnvs answers the question the catalog hierarchy cannot: every
// environment in an organization, in one request. Clients previously walked
// apps → stacks → environments, which costs a request per app and per stack and
// grows with the catalog rather than with what is being displayed.
//
// The org scoping lives in the store query, which reaches the org through
// stacks and applications because an environment carries no org id of its own.
// That makes the store method the tenant boundary for this route.
func (s *Server) handleListOrgEnvs(w http.ResponseWriter, r *http.Request) {
	ctx, cancel := contextWithTimeout(r, 10*time.Second)
	defer cancel()

	orgID, ok := pathUUID(w, r, "org")
	if !ok {
		return
	}
	if !s.authorizeOrg(w, r, orgID) {
		return
	}
	envs, err := s.st.ListOrgEnvironments(ctx, orgID)
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
