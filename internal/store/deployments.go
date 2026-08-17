package store

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"reflect"
	"strings"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"

	"github.com/craig/composectl/internal/spec"
)

// CreateDeploymentParams is the input to a new rollout.
type CreateDeploymentParams struct {
	EnvironmentID  uuid.UUID
	StackVersionID uuid.UUID
	ResolvedSpec   *spec.DeploymentSpec
	CreatedBy      string
}

// CreateDeployment allocates the next revision number and the opposite
// color slot from the currently live deployment, then inserts a pending
// deployment row.
//
// Concurrency: the revision number is allocated inside the transaction
// with a row lock on the environment, so two simultaneous deploys cannot
// claim the same revision. The partial unique index additionally rejects
// a second active deployment for the environment — that surfaces as
// ErrConflict, which the API maps to 409.
func (s *Store) CreateDeployment(ctx context.Context, p CreateDeploymentParams) (*Deployment, error) {
	var d *Deployment
	err := s.tx(ctx, func(tx pgx.Tx) error {
		var err error
		d, err = s.createDeploymentTx(ctx, tx, p)
		return err
	})
	if err != nil {
		return nil, mapErr(err)
	}
	return d, nil
}

// createDeploymentTx holds the revision/slot allocation so a caller already
// inside a transaction -- CreatePreview -- can create an environment and its
// first deployment atomically. A preview that existed with a hostname but no
// deployment would be an env the reaper eventually collects and the user never
// sees work.
func (s *Store) createDeploymentTx(ctx context.Context, tx pgx.Tx, p CreateDeploymentParams) (*Deployment, error) {
	var d Deployment

	// Lock the environment row for the duration. Serializes rollouts
	// per environment without a global lock.
	var liveSlot *string
	var liveSpecJSON []byte
	var stackID uuid.UUID
	err := tx.QueryRow(ctx, `
		SELECT e.stack_id, d.slot, d.resolved_spec
		FROM environments e
		LEFT JOIN deployments d ON d.id = e.live_deployment_id
		WHERE e.id = $1
		FOR UPDATE OF e
	`, p.EnvironmentID).Scan(&stackID, &liveSlot, &liveSpecJSON)
	if err != nil {
		return nil, err
	}
	var versionBelongs bool
	if err := tx.QueryRow(ctx, `
		SELECT EXISTS (
			SELECT 1 FROM stack_versions WHERE id = $1 AND stack_id = $2
		)
	`, p.StackVersionID, stackID).Scan(&versionBelongs); err != nil {
		return nil, err
	}
	if !versionBelongs {
		return nil, fmt.Errorf("%w: stack version does not belong to environment stack", ErrInvalid)
	}
	if len(liveSpecJSON) > 0 {
		var liveSpec spec.DeploymentSpec
		if err := json.Unmarshal(liveSpecJSON, &liveSpec); err != nil {
			return nil, err
		}
		if err := rejectPinnedDrift(&liveSpec, p.ResolvedSpec); err != nil {
			return nil, err
		}
	}

	// Next revision.
	var revision int
	err = tx.QueryRow(ctx, `
		SELECT COALESCE(MAX(revision), 0) + 1
		FROM deployments WHERE environment_id = $1
	`, p.EnvironmentID).Scan(&revision)
	if err != nil {
		return nil, err
	}

	// Alternate slot. First ever deploy goes blue.
	slot := "blue"
	if liveSlot != nil && *liveSlot == "blue" {
		slot = "green"
	}

	projectName := fmt.Sprintf("cc-%s-r%d-%s",
		shortID(p.EnvironmentID), revision, slot)

	specJSON, err := json.Marshal(p.ResolvedSpec)
	if err != nil {
		return nil, err
	}

	err = tx.QueryRow(ctx, `
		INSERT INTO deployments (
			environment_id, stack_version_id, stack_id, revision, slot,
			project_name, state, resolved_spec, created_by
		) VALUES ($1,$2,$3,$4,$5,$6,'pending',$7,$8)
		RETURNING id, environment_id, stack_version_id, revision, slot,
		          project_name, state, created_at, updated_at
	`, p.EnvironmentID, p.StackVersionID, stackID, revision, slot,
		projectName, specJSON, p.CreatedBy,
	).Scan(&d.ID, &d.EnvironmentID, &d.StackVersionID, &d.Revision,
		&d.Slot, &d.ProjectName, &d.State, &d.CreatedAt, &d.UpdatedAt)
	if err != nil {
		return nil, err
	}
	d.ResolvedSpec = p.ResolvedSpec
	if err := appendDeploymentEventTx(ctx, tx, d.ID, "deployment.created",
		fmt.Sprintf("revision %d created", d.Revision), map[string]any{
			"revision": d.Revision, "slot": d.Slot, "created_by": p.CreatedBy,
		}); err != nil {
		return nil, err
	}
	return &d, nil
}

// rejectPinnedDrift prevents a rollout from pretending it changed a stateful
// container that the agent deliberately adopts across revisions. New pinned
// services are allowed; changing or removing an existing one needs a future,
// operator-approved recreate/migration workflow.
func rejectPinnedDrift(live, next *spec.DeploymentSpec) error {
	for name, oldSvc := range live.Services {
		if oldSvc.Swappable {
			continue
		}
		newSvc, ok := next.Services[name]
		if !ok || newSvc.Swappable || !reflect.DeepEqual(oldSvc, newSvc) {
			return fmt.Errorf("%w: pinned service %q changed; stateful recreation is not supported", ErrConflict, name)
		}
	}
	return nil
}

// RollbackDeployment re-deploys an earlier revision's stack version as a new
// revision. Append-only holds: nothing is mutated — a new deployment is created
// and runs the normal rollout (and auto-promotes). toRevision <= 0 means "the
// revision before the current live one".
func (s *Store) RollbackDeployment(ctx context.Context, envID uuid.UUID, toRevision int) (*Deployment, error) {
	var svID uuid.UUID
	var specJSON []byte
	q := `SELECT stack_version_id, resolved_spec FROM deployments WHERE environment_id=$1 AND revision=$2`
	args := []any{envID, toRevision}
	if toRevision <= 0 {
		q = `SELECT stack_version_id, resolved_spec FROM deployments
		     WHERE environment_id=$1 AND revision < (
		       SELECT revision FROM deployments
		       WHERE id=(SELECT live_deployment_id FROM environments WHERE id=$1))
		     ORDER BY revision DESC LIMIT 1`
		args = []any{envID}
	}
	if err := s.pool.QueryRow(ctx, q, args...).Scan(&svID, &specJSON); err != nil {
		return nil, mapErr(err)
	}
	var resolved spec.DeploymentSpec
	if err := json.Unmarshal(specJSON, &resolved); err != nil {
		return nil, err
	}
	if required := resolved.RequiredSecrets(); len(required) > 0 {
		have, err := s.SecretKeysForEnv(ctx, envID)
		if err != nil {
			return nil, err
		}
		set := map[string]bool{}
		for _, m := range have {
			set[m.Key] = true
		}
		var missing []string
		for _, k := range required {
			if !set[k] {
				missing = append(missing, k)
			}
		}
		if len(missing) > 0 {
			return nil, fmt.Errorf("%w: environment is missing required secrets: %s", ErrInvalid, strings.Join(missing, ", "))
		}
	}
	return s.CreateDeployment(ctx, CreateDeploymentParams{
		EnvironmentID:  envID,
		StackVersionID: svID,
		ResolvedSpec:   &resolved,
		CreatedBy:      "rollback",
	})
}

// GetDeployment returns a single deployment by id.
func (s *Store) GetDeployment(ctx context.Context, id uuid.UUID) (*Deployment, error) {
	var d Deployment
	var specJSON []byte
	err := s.pool.QueryRow(ctx, `
		SELECT id, environment_id, stack_version_id, revision, slot,
		       project_name, state, resolved_spec, promoted_at, superseded_at,
		       COALESCE(failure_reason,''), COALESCE(created_by,''),
		       created_at, updated_at
		FROM deployments WHERE id = $1
	`, id).Scan(&d.ID, &d.EnvironmentID, &d.StackVersionID, &d.Revision,
		&d.Slot, &d.ProjectName, &d.State, &specJSON, &d.PromotedAt,
		&d.SupersededAt, &d.FailureReason, &d.CreatedBy, &d.CreatedAt, &d.UpdatedAt)
	if err != nil {
		return nil, mapErr(err)
	}
	if err := json.Unmarshal(specJSON, &d.ResolvedSpec); err != nil {
		return nil, err
	}
	return &d, nil
}

// ListDeployments returns revisions for an environment, newest first.
func (s *Store) ListDeployments(ctx context.Context, envID uuid.UUID, limit int) ([]Deployment, error) {
	if limit <= 0 || limit > 200 {
		limit = 50
	}
	rows, err := s.pool.Query(ctx, `
		SELECT id, environment_id, stack_version_id, revision, slot,
		       project_name, state, promoted_at, superseded_at,
		       COALESCE(failure_reason,''), COALESCE(created_by,''),
		       created_at, updated_at
		FROM deployments
		WHERE environment_id = $1
		ORDER BY revision DESC
		LIMIT $2
	`, envID, limit)
	if err != nil {
		return nil, mapErr(err)
	}
	defer rows.Close()

	var out []Deployment
	for rows.Next() {
		var d Deployment
		if err := rows.Scan(&d.ID, &d.EnvironmentID, &d.StackVersionID,
			&d.Revision, &d.Slot, &d.ProjectName, &d.State, &d.PromotedAt,
			&d.SupersededAt, &d.FailureReason, &d.CreatedBy,
			&d.CreatedAt, &d.UpdatedAt); err != nil {
			return nil, err
		}
		out = append(out, d)
	}
	return out, rows.Err()
}

// UpdateDeploymentState transitions a deployment, rejecting illegal moves.
// The allowed-transition check lives in SQL so that a buggy agent cannot
// drive a deployment backwards.
func (s *Store) UpdateDeploymentState(ctx context.Context, id uuid.UUID, to DeploymentState, reason string) error {
	allowed, ok := validTransitions[to]
	if !ok {
		return fmt.Errorf("unknown target state %q", to)
	}
	// pgx cannot encode a []DeploymentState against the enum-array type, and
	// comparing the enum column to a text[] needs an explicit cast. Pass the
	// allowed states as text and cast the column: state::text = ANY($4).
	err := s.tx(ctx, func(tx pgx.Tx) error {
		var from DeploymentState
		err := tx.QueryRow(ctx, `
			UPDATE deployments
			SET state = $2, failure_reason = NULLIF($3, '')
			WHERE id = $1 AND state::text = ANY($4)
			RETURNING state
		`, id, to, reason, statesToText(allowed)).Scan(&from)
		if errors.Is(err, pgx.ErrNoRows) {
			return fmt.Errorf("%w: illegal transition to %s", ErrConflict, to)
		}
		if err != nil {
			return err
		}
		return appendDeploymentEventTx(ctx, tx, id, "deployment.state_changed",
			fmt.Sprintf("deployment entered %s", to), map[string]any{"state": to, "reason": reason})
	})
	return mapErr(err)
}

// statesToText renders deployment states as text for a text[] query parameter,
// working around pgx's inability to encode a named-string slice as an enum
// array. The column side is cast with state::text at the call site.
func statesToText(states []DeploymentState) []string {
	ss := make([]string, len(states))
	for i, st := range states {
		ss[i] = string(st)
	}
	return ss
}

// validTransitions maps a target state to the states it may be entered from.
var validTransitions = map[DeploymentState][]DeploymentState{
	DeployScheduling: {DeployPending},
	DeployStarting:   {DeployScheduling},
	DeployHealthy:    {DeployStarting},
	DeployLive:       {DeployHealthy},
	DeploySuperseded: {DeployLive},
	DeployFailed:     {DeployPending, DeployScheduling, DeployStarting, DeployHealthy},
	DeployStopped:    {DeploySuperseded, DeployFailed, DeployLive},
}

// PromoteDeployment flips traffic to a healthy deployment: marks it live,
// supersedes the previous live revision, and repoints the environment.
// All three happen in one transaction — a partial promotion would leave
// the router pointing at a deployment the database thinks is not live.
func (s *Store) PromoteDeployment(ctx context.Context, id uuid.UUID) (superseded *uuid.UUID, err error) {
	err = s.tx(ctx, func(tx pgx.Tx) error {
		var envID uuid.UUID
		var state DeploymentState
		if err := tx.QueryRow(ctx, `
			SELECT environment_id, state FROM deployments WHERE id = $1 FOR UPDATE
		`, id).Scan(&envID, &state); err != nil {
			return err
		}
		if state != DeployHealthy {
			return fmt.Errorf("%w: deployment is %s, must be healthy to promote", ErrConflict, state)
		}

		var prev *uuid.UUID
		if err := tx.QueryRow(ctx, `
			SELECT live_deployment_id FROM environments WHERE id = $1 FOR UPDATE
		`, envID).Scan(&prev); err != nil {
			return err
		}

		if prev != nil {
			if _, err := tx.Exec(ctx, `
				UPDATE deployments
				SET state = 'superseded', superseded_at = now()
				WHERE id = $1 AND state = 'live'
			`, *prev); err != nil {
				return err
			}
			superseded = prev
			if err := appendDeploymentEventTx(ctx, tx, *prev, "deployment.superseded",
				"deployment superseded", map[string]any{"replaced_by": id}); err != nil {
				return err
			}
		}

		if _, err := tx.Exec(ctx, `
			UPDATE deployments SET state = 'live', promoted_at = now() WHERE id = $1
		`, id); err != nil {
			return err
		}

		_, err := tx.Exec(ctx, `
			UPDATE environments SET live_deployment_id = $2 WHERE id = $1
		`, envID, id)
		if err != nil {
			return err
		}
		return appendDeploymentEventTx(ctx, tx, id, "deployment.promoted",
			"deployment promoted to live", map[string]any{"superseded_deployment_id": prev})
	})
	if err != nil {
		return nil, mapErr(err)
	}
	return superseded, nil
}

// shortID renders the first 8 hex chars of a UUID for use in project names.
func shortID(id uuid.UUID) string {
	return id.String()[:8]
}

type LiveRoute struct {
	Env8           string
	ProjectName    string
	IngressService string
	Hostname       string
	IngressPort    int // the container port the spec declares
	// NodeAddr and PublishedPort are where the router must actually connect:
	// the node's registered address and the host port its agent reported. The
	// container name is no longer usable as a target — it only resolves on the
	// daemon the container runs on, which stops being the router's daemon the
	// moment a stack is placed on another node.
	NodeAddr      string
	PublishedPort int
}

// ListLiveRoutes returns one route per live deployment whose environment has a
// hostname and whose spec declares an ingress service. The router turns these
// into Traefik config. resolved_spec is parsed here (not in the router) so the
// router stays free of the spec type and pgx.
func (s *Store) ListLiveRoutes(ctx context.Context) ([]LiveRoute, error) {
	rows, err := s.pool.Query(ctx, `
		SELECT d.environment_id, d.project_name, COALESCE(e.hostname,''), d.resolved_spec,
		       COALESCE(host(n.advertise_addr), ''), COALESCE(si.ingress_port, 0)
		FROM deployments d
		JOIN environments e ON e.id = d.environment_id
		-- Only the ingress service ever has a published port, so this selects
		-- exactly one instance without naming the service. LEFT so a live
		-- deployment whose agent has not reported yet still comes back and is
		-- skipped by the caller, rather than vanishing here for a reason the
		-- caller cannot distinguish from "not live".
		LEFT JOIN service_instances si
		       ON si.deployment_id = d.id AND si.ingress_port IS NOT NULL
		LEFT JOIN nodes n ON n.id = si.node_id
		WHERE d.state = 'live' AND e.hostname IS NOT NULL AND e.hostname <> ''
	`)
	if err != nil {
		return nil, mapErr(err)
	}
	defer rows.Close()
	out := []LiveRoute{}
	for rows.Next() {
		var envID uuid.UUID
		var project, hostname, nodeAddr string
		var publishedPort int
		var specJSON []byte
		if err := rows.Scan(&envID, &project, &hostname, &specJSON, &nodeAddr, &publishedPort); err != nil {
			return nil, err
		}
		var ds spec.DeploymentSpec
		if err := json.Unmarshal(specJSON, &ds); err != nil {
			return nil, err
		}
		name, ok := ds.IngressService()
		if !ok {
			continue
		}
		out = append(out, LiveRoute{
			Env8: shortID(envID), ProjectName: project, IngressService: name,
			Hostname: hostname, IngressPort: ds.Services[name].Ingress.Port,
			NodeAddr: nodeAddr, PublishedPort: publishedPort,
		})
	}
	return out, rows.Err()
}

type PendingDeployment struct {
	Deployment
	OrgID uuid.UUID
	// HomeNodeID is the node this environment's durable state already lives on,
	// nil before its first placement. A deployment for a homed environment goes
	// there or nowhere: its pinned container and named volumes cannot follow it.
	HomeNodeID *uuid.UUID
}

// ListPendingDeployments returns deployments awaiting scheduling, oldest
// first, each carrying its org id so the scheduler can find eligible nodes.
func (s *Store) ListPendingDeployments(ctx context.Context) ([]PendingDeployment, error) {
	return s.listPendingDeployments(ctx, nil)
}

// ListPendingDeploymentsForOrg is the org-scoped form used by isolated loop
// tests and future sharded control-plane workers.
func (s *Store) ListPendingDeploymentsForOrg(ctx context.Context, orgID uuid.UUID) ([]PendingDeployment, error) {
	return s.listPendingDeployments(ctx, &orgID)
}

func (s *Store) listPendingDeployments(ctx context.Context, orgID *uuid.UUID) ([]PendingDeployment, error) {
	rows, err := s.pool.Query(ctx, `
		SELECT d.id, d.environment_id, d.stack_version_id, d.revision, d.slot,
		       d.project_name, d.state, d.resolved_spec, d.created_at, d.updated_at,
		       a.org_id, e.home_node_id
		FROM deployments d
		JOIN environments e ON e.id = d.environment_id
		JOIN stacks s       ON s.id = e.stack_id
		JOIN applications a ON a.id = s.app_id
		WHERE d.state='pending' AND ($1::uuid IS NULL OR a.org_id=$1)
		ORDER BY d.created_at
	`, orgID)
	if err != nil {
		return nil, mapErr(err)
	}
	defer rows.Close()
	out := []PendingDeployment{}
	for rows.Next() {
		var p PendingDeployment
		var specJSON []byte
		if err := rows.Scan(&p.ID, &p.EnvironmentID, &p.StackVersionID, &p.Revision,
			&p.Slot, &p.ProjectName, &p.State, &specJSON, &p.CreatedAt, &p.UpdatedAt,
			&p.OrgID, &p.HomeNodeID); err != nil {
			return nil, err
		}
		if err := json.Unmarshal(specJSON, &p.ResolvedSpec); err != nil {
			return nil, err
		}
		out = append(out, p)
	}
	return out, rows.Err()
}

// ListRolloutsInState returns deployments the controller must advance.
func (s *Store) ListRolloutsInState(ctx context.Context, states ...DeploymentState) ([]Deployment, error) {
	return s.listRolloutsInState(ctx, nil, states...)
}

// ListRolloutsInStateForOrg scopes controller work to one organization.
func (s *Store) ListRolloutsInStateForOrg(ctx context.Context, orgID uuid.UUID, states ...DeploymentState) ([]Deployment, error) {
	return s.listRolloutsInState(ctx, &orgID, states...)
}

func (s *Store) listRolloutsInState(ctx context.Context, orgID *uuid.UUID, states ...DeploymentState) ([]Deployment, error) {
	rows, err := s.pool.Query(ctx, `
		SELECT d.id, d.environment_id, d.stack_version_id, d.revision, d.slot, d.project_name,
		       d.state, d.created_at, d.updated_at
		FROM deployments d
		JOIN environments e ON e.id = d.environment_id
		JOIN stacks s       ON s.id = e.stack_id
		JOIN applications a ON a.id = s.app_id
		WHERE d.state::text = ANY($1) AND ($2::uuid IS NULL OR a.org_id=$2)
		ORDER BY d.updated_at
	`, statesToText(states), orgID)
	if err != nil {
		return nil, mapErr(err)
	}
	defer rows.Close()
	out := []Deployment{}
	for rows.Next() {
		var d Deployment
		if err := rows.Scan(&d.ID, &d.EnvironmentID, &d.StackVersionID, &d.Revision,
			&d.Slot, &d.ProjectName, &d.State, &d.CreatedAt, &d.UpdatedAt); err != nil {
			return nil, err
		}
		out = append(out, d)
	}
	return out, rows.Err()
}
