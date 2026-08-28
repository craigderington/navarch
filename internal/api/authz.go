package api

import (
	"context"
	"net/http"
	"time"

	"github.com/google/uuid"

	"github.com/craig/composectl/internal/store"
)

// Authorization: may this caller touch this object?
//
// Every id-addressed route resolves the org that owns the object it was handed
// and checks the caller's membership. The resolvers live in the store, because
// the mapping is a join and handlers do not write SQL.
//
// A non-member gets 404, never 403. 403 confirms the object exists, which turns
// the status code into a probe: anyone could enumerate another tenant's
// environment ids by watching it change. The store's resolvers return
// ErrNotFound for a missing object, so both answers arrive here identical and
// leave identically.

// authorize resolves the owning org and checks membership. It writes the
// response itself and reports whether the handler should continue — the same
// contract as pathUUID, so handlers keep the shape they already have.
func (s *Server) authorize(w http.ResponseWriter, r *http.Request, resolve func(context.Context) (uuid.UUID, error)) bool {
	id, ok := identityFrom(r.Context())
	if !ok {
		// Authentication is disabled entirely: an in-process test server built
		// without a bearer token. The control-plane config refuses an empty
		// token at startup, so this is never a deployed configuration. It also
		// means a test that does not configure a token cannot observe any of
		// this — which is exactly how the per-node 401 bug survived a green
		// suite, and why the org-scoping tests configure one.
		return true
	}
	if !id.isOperator() {
		// A node or the shared service token reaching an operator route. The
		// credential checks in ServeHTTP already confine both, so this is
		// defence in depth rather than a live path — and it answers 404 like
		// every other refusal here, so it cannot be used to map the surface.
		s.notFound(w)
		return false
	}

	ctx, cancel := contextWithTimeout(r, 5*time.Second)
	defer cancel()
	orgID, err := resolve(ctx)
	if err != nil {
		s.writeStoreError(w, err)
		return false
	}
	member, err := s.st.OperatorInOrg(ctx, orgID, id.operator.ID)
	if err != nil {
		writeError(w, http.StatusInternalServerError, "could not check membership", nil)
		return false
	}
	if !member {
		s.notFound(w)
		return false
	}
	return true
}

// authorizeOrg is the org-addressed case: the id in the path *is* the org, so
// there is nothing to resolve. It still verifies the org exists, so a member of
// nothing and a caller naming a nonexistent org get the same 404.
func (s *Server) authorizeOrg(w http.ResponseWriter, r *http.Request, orgID uuid.UUID) bool {
	return s.authorize(w, r, func(ctx context.Context) (uuid.UUID, error) {
		if _, err := s.st.GetOrganization(ctx, orgID); err != nil {
			return uuid.Nil, err
		}
		return orgID, nil
	})
}

// The remaining helpers each name one resolver, so a handler reads as "this
// route is addressed by an environment" rather than as a closure.

func (s *Server) authorizeApp(w http.ResponseWriter, r *http.Request, appID uuid.UUID) bool {
	return s.authorize(w, r, func(ctx context.Context) (uuid.UUID, error) {
		return s.st.OrgForApp(ctx, appID)
	})
}

func (s *Server) authorizeStack(w http.ResponseWriter, r *http.Request, stackID uuid.UUID) bool {
	return s.authorize(w, r, func(ctx context.Context) (uuid.UUID, error) {
		return s.st.OrgForStack(ctx, stackID)
	})
}

func (s *Server) authorizeEnv(w http.ResponseWriter, r *http.Request, envID uuid.UUID) bool {
	return s.authorize(w, r, func(ctx context.Context) (uuid.UUID, error) {
		return s.st.OrgForEnvironment(ctx, envID)
	})
}

func (s *Server) authorizeDeployment(w http.ResponseWriter, r *http.Request, depID uuid.UUID) bool {
	return s.authorize(w, r, func(ctx context.Context) (uuid.UUID, error) {
		return s.st.OrgForDeployment(ctx, depID)
	})
}

func (s *Server) authorizeNode(w http.ResponseWriter, r *http.Request, nodeID uuid.UUID) bool {
	return s.authorize(w, r, func(ctx context.Context) (uuid.UUID, error) {
		return s.st.OrgForNode(ctx, nodeID)
	})
}

func (s *Server) authorizeLogRequest(w http.ResponseWriter, r *http.Request, requestID uuid.UUID) bool {
	return s.authorize(w, r, func(ctx context.Context) (uuid.UUID, error) {
		return s.st.OrgForLogRequest(ctx, requestID)
	})
}

// notFound is the single refusal shape, so "no such object" and "not yours"
// cannot be told apart by body or status.
func (s *Server) notFound(w http.ResponseWriter) {
	s.writeStoreError(w, store.ErrNotFound)
}
