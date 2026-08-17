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
	// Log instructions ride the same poll. They are deliberately part of this
	// response rather than a second endpoint: an agent that had to ask
	// separately would either poll twice as often or learn about a tail a tick
	// late, and neither buys anything.
	logReqs, err := s.st.LogRequestsForNode(ctx, id)
	if err != nil {
		s.writeStoreError(w, err)
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{
		"instances": desired, "secrets": secretsByEnv, "teardown_envs": teardown,
		"log_requests": logReqs,
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
		IngressPort  int       `json:"ingress_port,omitempty"`
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
			IngressPort: in.IngressPort,
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

func (s *Server) handleGetNode(w http.ResponseWriter, r *http.Request) {
	ctx, cancel := contextWithTimeout(r, 5*time.Second)
	defer cancel()
	id, ok := pathUUID(w, r, "id")
	if !ok {
		return
	}
	node, err := s.st.GetNode(ctx, id)
	if err != nil {
		s.writeStoreError(w, err)
		return
	}
	writeJSON(w, http.StatusOK, node)
}

func (s *Server) handleDrainNode(w http.ResponseWriter, r *http.Request) {
	ctx, cancel := contextWithTimeout(r, 5*time.Second)
	defer cancel()
	id, ok := pathUUID(w, r, "id")
	if !ok {
		return
	}
	if err := s.st.DrainNode(ctx, id); err != nil {
		s.writeStoreError(w, err)
		return
	}

	// Cordoning is the part that always works; evacuation is best-effort, and
	// the difference has to reach the operator. Refusing to drain a node holding
	// three databases would make drain useless exactly when it is most wanted —
	// the operator taking a machine down for maintenance still needs new work to
	// stop landing on it. Draining silently and leaving them behind is worse:
	// they would believe the node was empty.
	//
	// So the response is a manifest. An environment with nothing durable is
	// released here and the scheduler will place its next deployment elsewhere by
	// score; one with a pinned service or a named volume stays, with the reason.
	homed, err := s.st.EnvironmentsHomedOnNode(ctx, id)
	if err != nil {
		s.writeStoreError(w, err)
		return
	}
	released := []map[string]any{}
	stranded := []map[string]any{}
	for _, h := range homed {
		path := h.AppSlug + "/" + h.StackSlug + "/" + h.Slug
		if len(h.DurableReasons) > 0 {
			stranded = append(stranded, map[string]any{
				"id": h.ID, "path": path, "reasons": h.DurableReasons,
			})
			continue
		}
		if err := s.st.ReleaseEnvironmentHome(ctx, h.ID); err != nil {
			// A release that fails is stranded for a different reason, and
			// saying so beats aborting the whole drain: the cordon has already
			// taken effect and the other environments still deserve their answer.
			stranded = append(stranded, map[string]any{
				"id": h.ID, "path": path, "reasons": []string{err.Error()},
			})
			continue
		}
		released = append(released, map[string]any{"id": h.ID, "path": path})
	}
	writeJSON(w, http.StatusOK, map[string]any{
		"status": "draining", "released": released, "stranded": stranded,
	})
}

// handleUncordonNode lifts a drain. It reports the state the node actually
// landed in rather than a fixed "ready", because the store derives that from the
// last heartbeat — telling the caller "ready" when the row says `unreachable`
// would make the API disagree with the very next `navarch node list`.
func (s *Server) handleUncordonNode(w http.ResponseWriter, r *http.Request) {
	ctx, cancel := contextWithTimeout(r, 5*time.Second)
	defer cancel()
	id, ok := pathUUID(w, r, "id")
	if !ok {
		return
	}
	if err := s.st.UncordonNode(ctx, id); err != nil {
		s.writeStoreError(w, err)
		return
	}
	n, err := s.st.GetNode(ctx, id)
	if err != nil {
		s.writeStoreError(w, err)
		return
	}
	writeJSON(w, http.StatusOK, map[string]string{"status": string(n.State)})
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
