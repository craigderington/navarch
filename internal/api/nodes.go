package api

import (
	"errors"
	"net/http"
	"time"

	"github.com/google/uuid"

	"github.com/craigderington/navarch/internal/rollout"
	"github.com/craigderington/navarch/internal/secrets"
	"github.com/craigderington/navarch/internal/store"
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
	// Which organization this node joins is decided by the credential, never by
	// the body — see the three branches below. Before join tokens existed the
	// org came from `req.Org` and was checked only for operators, so anyone
	// holding the shared service token could enrol a node into any org by
	// naming its slug. That is the hole this closes.
	org, ok := s.enrolmentOrg(w, r, req.Org)
	if !ok {
		return
	}
	// Reject a malformed recipient here rather than at the first secret
	// write: that failure arrives as a 500 confined to environments homed to
	// this node, which reads as a control-plane bug. A node without a key is
	// legitimate (it just gets no secrets), so only the non-empty case is
	// checked.
	if req.AgeRecipient != "" && !secrets.ValidRecipient(req.AgeRecipient) {
		writeError(w, http.StatusBadRequest, "age_recipient is not a valid X25519 recipient", nil)
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
	if !s.authorizeNode(w, r, id) {
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
	if !s.authorizeNode(w, r, id) {
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

// handleRotateNodeRecipient promotes the age key a node has been advertising.
//
// This is the operator action that S7 was missing. A re-registering node can
// propose a recipient but not assign one, so a compromised or merely
// mistaken registration cannot redirect the keys that future secrets are sealed
// to. Approving it is a human judgement — the control plane only ever sees
// public halves and cannot tell a legitimate new key from a hostile one.
func (s *Server) handleRotateNodeRecipient(w http.ResponseWriter, r *http.Request) {
	ctx, cancel := contextWithTimeout(r, 5*time.Second)
	defer cancel()
	id, ok := pathUUID(w, r, "id")
	if !ok {
		return
	}
	if !s.authorizeNode(w, r, id) {
		return
	}
	n, err := s.st.RotateNodeRecipient(ctx, id)
	if err != nil {
		s.writeStoreError(w, err)
		return
	}
	writeJSON(w, http.StatusOK, n)
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
	if !s.authorizeNode(w, r, id) {
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
	if !s.authorizeOrg(w, r, orgID) {
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
	if !s.authorizeEnv(w, r, envID) {
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

// enrolmentOrg decides which organization a registering node joins.
//
// Three credentials can reach this route and they are not interchangeable:
//
//   - A **join token** names exactly one org, so that is the org. If the body
//     also names one it must agree: a node that believes it joined `acme` and
//     actually joined `dev` is a worse outcome than a node that refused to
//     start, and silently overriding the caller is how that happens.
//   - An **operator** may enrol a node by hand, into an org they belong to.
//     The membership check is the same one every other org route makes.
//   - The **shared service token** is the compatibility path for a
//     single-tenant install whose agents predate join tokens. It is exactly as
//     wide as it always was, which is why it is refused outright once
//     COMPOSECTL_REQUIRE_JOIN_TOKEN is set — the switch an install flips once
//     its agents are migrated, and the switch a multi-tenant control plane
//     never has off.
func (s *Server) enrolmentOrg(w http.ResponseWriter, r *http.Request, bodyOrg string) (*store.Organization, bool) {
	ctx, cancel := contextWithTimeout(r, 5*time.Second)
	defer cancel()

	id, authenticated := identityFrom(r.Context())

	// A join token was already redeemed during authentication, and it carries
	// the only org it can admit to.
	if authenticated && id.kind == identityJoin {
		org, err := s.st.GetOrganization(ctx, id.orgID)
		if err != nil {
			s.writeStoreError(w, err)
			return nil, false
		}
		if bodyOrg != "" && bodyOrg != org.Slug {
			writeError(w, http.StatusBadRequest, "this join token admits nodes to organization "+
				org.Slug+", not "+bodyOrg, nil)
			return nil, false
		}
		return org, true
	}

	if authenticated && id.kind == identityService && s.requireJoinToken {
		writeError(w, http.StatusForbidden,
			"this control plane requires a node join token; create one with `navarch node join-token create ORG`", nil)
		return nil, false
	}
	if bodyOrg == "" {
		writeError(w, http.StatusBadRequest, "org is required", nil)
		return nil, false
	}
	org, err := s.st.GetOrganizationBySlug(ctx, bodyOrg)
	if err != nil {
		s.writeStoreError(w, err)
		return nil, false
	}
	if authenticated && id.isOperator() {
		if !s.authorizeOrg(w, r, org.ID) {
			return nil, false
		}
	}
	return org, true
}

// ------------------------------------------------------ node join tokens

type createJoinTokenRequest struct {
	Name          string `json:"name,omitempty"`
	ExpiresInDays int    `json:"expires_in_days,omitempty"`
	MaxUses       int    `json:"max_uses,omitempty"`
}

func (s *Server) handleCreateJoinToken(w http.ResponseWriter, r *http.Request) {
	ctx, cancel := contextWithTimeout(r, 5*time.Second)
	defer cancel()
	orgID, ok := pathUUID(w, r, "org")
	if !ok {
		return
	}
	if !s.authorizeOrg(w, r, orgID) {
		return
	}
	var req createJoinTokenRequest
	if r.ContentLength > 0 {
		if err := decodeJSON(r, &req); err != nil {
			writeError(w, http.StatusBadRequest, "invalid request body", nil)
			return
		}
	}
	p := store.CreateJoinTokenParams{OrgID: orgID, Name: req.Name}
	if req.ExpiresInDays > 0 {
		t := time.Now().Add(time.Duration(req.ExpiresInDays) * 24 * time.Hour)
		p.ExpiresAt = &t
	}
	if req.MaxUses > 0 {
		p.MaxUses = &req.MaxUses
	}
	if id, authed := identityFrom(r.Context()); authed && id.isOperator() {
		p.CreatedBy = &id.operator.ID
	}
	tok, err := s.st.CreateJoinToken(ctx, p)
	if err != nil {
		s.writeStoreError(w, err)
		return
	}
	writeJSON(w, http.StatusCreated, tok)
}

func (s *Server) handleListJoinTokens(w http.ResponseWriter, r *http.Request) {
	ctx, cancel := contextWithTimeout(r, 5*time.Second)
	defer cancel()
	orgID, ok := pathUUID(w, r, "org")
	if !ok {
		return
	}
	if !s.authorizeOrg(w, r, orgID) {
		return
	}
	tokens, err := s.st.ListJoinTokens(ctx, orgID)
	if err != nil {
		s.writeStoreError(w, err)
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"join_tokens": tokens})
}

func (s *Server) handleRevokeJoinToken(w http.ResponseWriter, r *http.Request) {
	ctx, cancel := contextWithTimeout(r, 5*time.Second)
	defer cancel()
	orgID, ok := pathUUID(w, r, "org")
	if !ok {
		return
	}
	if !s.authorizeOrg(w, r, orgID) {
		return
	}
	tokenID, ok := pathUUID(w, r, "id")
	if !ok {
		return
	}
	// Revoking a join token does not unregister the nodes it admitted. They
	// hold their own per-node tokens now, which is the point of issuing those
	// at registration — enrolment is a moment, not a standing permission.
	if err := s.st.RevokeJoinToken(ctx, orgID, tokenID); err != nil {
		s.writeStoreError(w, err)
		return
	}
	w.WriteHeader(http.StatusNoContent)
}

// ------------------------------------------------------------ node routes

type nodeRoute struct {
	Key      string `json:"key"`
	Hostname string `json:"hostname"`
	Target   string `json:"target"`
	Port     int    `json:"port"`
}

// handleNodeRoutes returns the live routes for the node's own organization, so
// a customer running their own router can configure it.
//
// This is a node-token route, alongside /desired-state, because it is the agent
// that asks — and because it means a customer's router needs no operator
// credential sitting in a config file next to it.
//
// The shape is the control plane's vocabulary, not Traefik's. That is
// deliberate: internal/router stays the only thing in the tree that knows what
// Traefik's config looks like, so a Caddy or nginx backend is a change in one
// package rather than a change to a published API contract.
//
// Routes with no address or no reported port are omitted here for the same
// reason the control-plane router omits them: a target that is not known yet is
// not a target, and inventing one sends a hostname's traffic somewhere.
func (s *Server) handleNodeRoutes(w http.ResponseWriter, r *http.Request) {
	ctx, cancel := contextWithTimeout(r, 10*time.Second)
	defer cancel()
	nodeID, ok := pathUUID(w, r, "id")
	if !ok {
		return
	}
	node, err := s.st.GetNode(ctx, nodeID)
	if err != nil {
		s.writeStoreError(w, err)
		return
	}
	live, err := s.st.ListLiveRoutesForOrg(ctx, node.OrgID, s.routeStrand)
	if err != nil {
		s.writeStoreError(w, err)
		return
	}
	out := make([]nodeRoute, 0, len(live))
	for _, lr := range live {
		if lr.NodeAddr == "" || lr.PublishedPort == 0 {
			continue
		}
		out = append(out, nodeRoute{
			Key: lr.Env8, Hostname: lr.Hostname, Target: lr.NodeAddr, Port: lr.PublishedPort,
		})
	}
	writeJSON(w, http.StatusOK, map[string]any{"routes": out})
}
