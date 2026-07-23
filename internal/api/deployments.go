package api

import (
	"net/http"
	"strconv"
	"time"

	"github.com/google/uuid"

	"github.com/craig/composectl/internal/store"
)

type createDeploymentRequest struct {
	// StackVersionID selects which version to deploy. Omit to use latest.
	StackVersionID string `json:"stack_version_id,omitempty"`
	CreatedBy      string `json:"created_by,omitempty"`
}

type deploymentResponse struct {
	*store.Deployment
	// PeakMemoryBytes surfaces the rollout's memory requirement so the
	// caller sees why a placement might be rejected.
	PeakMemoryBytes int64 `json:"peak_memory_bytes"`
}

func (s *Server) handleCreateDeployment(w http.ResponseWriter, r *http.Request) {
	ctx, cancel := contextWithTimeout(r, 15*time.Second)
	defer cancel()

	envID, ok := pathUUID(w, r, "env")
	if !ok {
		return
	}

	var req createDeploymentRequest
	if err := decodeJSON(r, &req); err != nil {
		writeError(w, http.StatusBadRequest, "invalid request body", nil)
		return
	}

	env, err := s.st.GetEnvironment(ctx, envID)
	if err != nil {
		s.writeStoreError(w, err)
		return
	}

	// Resolve which stack version to deploy.
	var svID uuid.UUID
	if req.StackVersionID != "" {
		if svID, err = uuid.Parse(req.StackVersionID); err != nil {
			writeError(w, http.StatusBadRequest, "invalid stack_version_id", nil)
			return
		}
	} else {
		latest, err := s.st.LatestStackVersion(ctx, env.StackID)
		if err != nil {
			s.writeStoreError(w, err)
			return
		}
		svID = latest.ID
	}

	sv, err := s.st.GetStackVersion(ctx, svID)
	if err != nil {
		s.writeStoreError(w, err)
		return
	}

	// Overlay the environment's non-secret config onto the spec. Secrets
	// stay as references; the agent resolves them at container start so
	// plaintext never touches the control plane or the database.
	resolved := applyEnvConfig(sv.Spec, env.Config)

	d, err := s.st.CreateDeployment(ctx, store.CreateDeploymentParams{
		EnvironmentID:  envID,
		StackVersionID: svID,
		ResolvedSpec:   resolved,
		CreatedBy:      req.CreatedBy,
	})
	if err != nil {
		s.writeStoreError(w, err)
		return
	}

	writeJSON(w, http.StatusAccepted, deploymentResponse{
		Deployment:      d,
		PeakMemoryBytes: resolved.PeakMemoryBytes(),
	})
}

func (s *Server) handleGetDeployment(w http.ResponseWriter, r *http.Request) {
	ctx, cancel := contextWithTimeout(r, 5*time.Second)
	defer cancel()

	id, ok := pathUUID(w, r, "id")
	if !ok {
		return
	}
	d, err := s.st.GetDeployment(ctx, id)
	if err != nil {
		s.writeStoreError(w, err)
		return
	}
	writeJSON(w, http.StatusOK, deploymentResponse{
		Deployment:      d,
		PeakMemoryBytes: d.ResolvedSpec.PeakMemoryBytes(),
	})
}

func (s *Server) handleListDeployments(w http.ResponseWriter, r *http.Request) {
	ctx, cancel := contextWithTimeout(r, 5*time.Second)
	defer cancel()

	envID, ok := pathUUID(w, r, "env")
	if !ok {
		return
	}
	limit, _ := strconv.Atoi(r.URL.Query().Get("limit"))

	ds, err := s.st.ListDeployments(ctx, envID, limit)
	if err != nil {
		s.writeStoreError(w, err)
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"deployments": ds})
}

// handlePromote flips traffic to a healthy deployment. In Sprint 1 this is
// operator-triggered; the rollout controller calls it automatically once
// health gating lands in Sprint 2.
func (s *Server) handlePromote(w http.ResponseWriter, r *http.Request) {
	ctx, cancel := contextWithTimeout(r, 10*time.Second)
	defer cancel()

	id, ok := pathUUID(w, r, "id")
	if !ok {
		return
	}

	superseded, err := s.st.PromoteDeployment(ctx, id)
	if err != nil {
		s.writeStoreError(w, err)
		return
	}

	resp := map[string]any{"promoted": id}
	if superseded != nil {
		resp["superseded"] = *superseded
	}
	writeJSON(w, http.StatusOK, resp)
}
