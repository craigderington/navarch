package api

import (
	"errors"
	"net/http"
	"time"

	"github.com/google/uuid"

	"github.com/craig/composectl/internal/rollout"
	"github.com/craig/composectl/internal/store"
)

// Node-facing handlers. The agent speaks only this HTTP surface — it never
// touches Postgres — so the store's exclusive ownership of pgx holds across
// binaries. Handlers stay thin: decode, delegate, encode.

type registerNodeRequest struct {
	Org           string            `json:"org"`
	Hostname      string            `json:"hostname"`
	AdvertiseAddr string            `json:"advertise_addr"`
	CPUMillis     int               `json:"cpu_millis"`
	MemoryBytes   int64             `json:"memory_bytes"`
	Labels        map[string]string `json:"labels,omitempty"`
	AgentVersion  string            `json:"agent_version,omitempty"`
	// AgeRecipient is the agent's public encryption key. Without it the node
	// is invisible to handleSetSecret's recipient collection, so it never
	// receives any secret sealed after it registers.
	AgeRecipient string `json:"age_recipient,omitempty"`
}

func (s *Server) handleRegisterNode(w http.ResponseWriter, r *http.Request) {
	ctx, cancel := contextWithTimeout(r, 5*time.Second)
	defer cancel()

	var req registerNodeRequest
	if err := decodeJSON(r, &req); err != nil {
		writeError(w, http.StatusBadRequest, "invalid request body", nil)
		return
	}
	org, err := s.st.GetOrganizationBySlug(ctx, req.Org)
	if err != nil {
		s.writeStoreError(w, err)
		return
	}
	node, err := s.st.RegisterNode(ctx, store.RegisterNodeParams{
		OrgID: org.ID, Hostname: req.Hostname, AdvertiseAddr: req.AdvertiseAddr,
		CPUMillis: req.CPUMillis, MemoryBytes: req.MemoryBytes,
		Labels: req.Labels, AgentVersion: req.AgentVersion,
		AgeRecipient: req.AgeRecipient,
	})
	if err != nil {
		s.writeStoreError(w, err)
		return
	}
	writeJSON(w, http.StatusCreated, node)
}

type heartbeatRequest struct {
	AllocCPUMillis   int   `json:"alloc_cpu_millis"`
	AllocMemoryBytes int64 `json:"alloc_memory_bytes"`
}

func (s *Server) handleHeartbeat(w http.ResponseWriter, r *http.Request) {
	ctx, cancel := contextWithTimeout(r, 5*time.Second)
	defer cancel()
	id, ok := pathUUID(w, r, "id")
	if !ok {
		return
	}
	var req heartbeatRequest
	if err := decodeJSON(r, &req); err != nil {
		writeError(w, http.StatusBadRequest, "invalid request body", nil)
		return
	}
	if err := s.st.Heartbeat(ctx, id, store.HeartbeatParams{
		AllocCPUMillis: req.AllocCPUMillis, AllocMemoryBytes: req.AllocMemoryBytes,
	}); err != nil {
		s.writeStoreError(w, err)
		return
	}
	writeJSON(w, http.StatusOK, map[string]string{"status": "ok"})
}

// handleDesiredState returns the instances this node must run, each with its
// resolved Service spec inline so the agent needs no second call to build
// containers, plus the ciphertext for every env with instances on this node
// so the agent can decrypt and inject secrets without a separate round trip,
// plus the envs (by env8) this node's own org has torn down and must now
// destroy outright — pinned containers and named volumes included.
func (s *Server) handleDesiredState(w http.ResponseWriter, r *http.Request) {
	ctx, cancel := contextWithTimeout(r, 5*time.Second)
	defer cancel()
	id, ok := pathUUID(w, r, "id")
	if !ok {
		return
	}
	desired, err := s.st.DesiredStateForNode(ctx, id)
	if err != nil {
		s.writeStoreError(w, err)
		return
	}
	secretsByEnv, err := s.st.EncryptedSecretsForNode(ctx, id)
	if err != nil {
		s.writeStoreError(w, err)
		return
	}
	// Environments this node must destroy outright. Explicit intent, never
	// inferred from an instance row's absence: an empty desired-state must
	// never be read as "delete the database".
	teardown, err := s.st.TombstonesForNode(ctx, id, rollout.TombstoneRetention)
	if err != nil {
		s.writeStoreError(w, err)
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{
		"instances": desired, "secrets": secretsByEnv, "teardown_envs": teardown,
	})
}

type reportRequest struct {
	Instances []struct {
		InstanceID   uuid.UUID `json:"instance_id"`
		State        string    `json:"state"`
		ContainerID  string    `json:"container_id,omitempty"`
		HealthStatus string    `json:"health_status,omitempty"`
		LastError    string    `json:"last_error,omitempty"`
		RestartCount int       `json:"restart_count,omitempty"`
		SetStarted   bool      `json:"set_started,omitempty"`
	} `json:"instances"`
}

func (s *Server) handleInstanceReport(w http.ResponseWriter, r *http.Request) {
	ctx, cancel := contextWithTimeout(r, 10*time.Second)
	defer cancel()
	nodeID, ok := pathUUID(w, r, "id")
	if !ok {
		return
	}
	var req reportRequest
	if err := decodeJSON(r, &req); err != nil {
		writeError(w, http.StatusBadRequest, "invalid request body", nil)
		return
	}
	for _, in := range req.Instances {
		err := s.st.ReportInstance(ctx, nodeID, in.InstanceID, store.ObservedInstance{
			State: store.InstanceState(in.State), ContainerID: in.ContainerID,
			HealthStatus: in.HealthStatus, LastError: in.LastError,
			RestartCount: in.RestartCount, SetStarted: in.SetStarted,
		})
		// A vanished row is not a failure, and it is routine: the agent
		// reconciles at the top of its tick and reports at the bottom, so a
		// preview reap or a supersede's DeleteInstances in between leaves it
		// holding reports for rows that no longer exist. That is information
		// the control plane no longer needs. Aborting here would 404 the whole
		// request, which makes reconcileTick return early — dropping the
		// reports for every other environment on this node and skipping the
		// heartbeat — and log "reconcile tick failed" when nothing failed.
		if errors.Is(err, store.ErrNotFound) {
			continue
		}
		if err != nil {
			s.writeStoreError(w, err)
			return
		}
	}
	writeJSON(w, http.StatusOK, map[string]string{"status": "ok"})
}

func (s *Server) handleListNodes(w http.ResponseWriter, r *http.Request) {
	ctx, cancel := contextWithTimeout(r, 5*time.Second)
	defer cancel()
	orgID, err := uuid.Parse(r.URL.Query().Get("org"))
	if err != nil {
		writeError(w, http.StatusBadRequest, "org query parameter is required", nil)
		return
	}
	nodes, err := s.st.ListNodes(ctx, orgID)
	if err != nil {
		s.writeStoreError(w, err)
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"nodes": nodes})
}

type rollbackRequest struct {
	// ToRevision selects the revision to re-deploy. 0 (or omitted) means the
	// revision before the current live one.
	ToRevision int `json:"to_revision,omitempty"`
}

// handleRollback re-deploys an earlier stack version as a new revision, which
// then runs the normal rollout and auto-promotes. deployments stays append-only.
func (s *Server) handleRollback(w http.ResponseWriter, r *http.Request) {
	ctx, cancel := contextWithTimeout(r, 15*time.Second)
	defer cancel()
	envID, ok := pathUUID(w, r, "env")
	if !ok {
		return
	}
	var req rollbackRequest
	if err := decodeJSON(r, &req); err != nil {
		writeError(w, http.StatusBadRequest, "invalid request body", nil)
		return
	}
	dep, err := s.st.RollbackDeployment(ctx, envID, req.ToRevision)
	if err != nil {
		s.writeStoreError(w, err)
		return
	}
	writeJSON(w, http.StatusAccepted, dep)
}
