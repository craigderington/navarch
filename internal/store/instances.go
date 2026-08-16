package store

import (
	"context"
	"encoding/json"
	"fmt"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"

	"github.com/craig/composectl/internal/spec"
)

type NewInstance struct {
	ServiceName string
	Swappable   bool
	ImageRef    string
}

// CreateServiceInstances writes the desired instance rows for a deployment in
// one transaction. The unique (deployment_id, service_name) constraint makes a
// double-schedule a no-op rather than a duplicate.
func (s *Store) CreateServiceInstances(ctx context.Context, deploymentID, nodeID uuid.UUID, insts []NewInstance) error {
	return s.tx(ctx, func(tx pgx.Tx) error {
		return insertInstancesTx(ctx, tx, deploymentID, nodeID, insts)
	})
}

func insertInstancesTx(ctx context.Context, tx pgx.Tx, deploymentID, nodeID uuid.UUID, insts []NewInstance) error {
	for _, in := range insts {
		if _, err := tx.Exec(ctx, `
			INSERT INTO service_instances
				(deployment_id, node_id, service_name, swappable, image_ref, state)
			VALUES ($1,$2,$3,$4,$5,'pending')
			ON CONFLICT (deployment_id, service_name) DO NOTHING
		`, deploymentID, nodeID, in.ServiceName, in.Swappable, in.ImageRef); err != nil {
			return err
		}
	}
	return nil
}

// PlaceDeployment reserves node capacity and writes instances in one
// transaction so two pending rollouts cannot both see the same free space.
//
// It also binds the environment to this node the first time, and refuses any
// later placement that would contradict that binding. The check belongs here
// rather than only in the scheduler for the reason the deployment state machine
// is enforced in SQL: a buggy or racing scheduler must not be able to write a
// placement that contradicts durable state. Two schedulers placing two
// deployments of one environment concurrently is what the row lock is for.
func (s *Store) PlaceDeployment(ctx context.Context, depID, nodeID uuid.UUID, insts []NewInstance, peakCPU int, peakMem int64) error {
	return s.tx(ctx, func(tx pgx.Tx) error {
		var state DeploymentState
		var envID uuid.UUID
		if err := tx.QueryRow(ctx, `
			SELECT state, environment_id FROM deployments WHERE id=$1 FOR UPDATE
		`, depID).Scan(&state, &envID); err != nil {
			return err
		}
		if state != DeployPending {
			return nil
		}
		// Locked before the capacity read: the binding decides which node's
		// capacity is even the right one to check.
		var home *uuid.UUID
		if err := tx.QueryRow(ctx, `
			SELECT home_node_id FROM environments WHERE id=$1 FOR UPDATE
		`, envID).Scan(&home); err != nil {
			return err
		}
		if home != nil && *home != nodeID {
			// Deliberately not a relocation. The environment's pinned container
			// and named volumes are on the home node and cannot follow; placing
			// here would build a new pinned container over an empty volume and
			// report success.
			return fmt.Errorf("%w: environment is homed to node %s, cannot place on %s",
				ErrConflict, *home, nodeID)
		}
		var cpu, mem int64
		var allocCPU int
		var allocMem int64
		if err := tx.QueryRow(ctx, `
			SELECT cpu_millis, memory_bytes, alloc_cpu_millis, alloc_memory_bytes
			FROM nodes WHERE id=$1 FOR UPDATE
		`, nodeID).Scan(&cpu, &mem, &allocCPU, &allocMem); err != nil {
			return err
		}
		if int64(cpu)-int64(allocCPU) < int64(peakCPU) || mem-allocMem < peakMem {
			return fmt.Errorf("%w: node lacks capacity for this rollout", ErrConflict)
		}
		if err := insertInstancesTx(ctx, tx, depID, nodeID, insts); err != nil {
			return err
		}
		if home == nil {
			// First placement binds the environment. In the same transaction as
			// the instance rows, so a rollout that fails afterwards still leaves
			// the binding that its volumes will have been created under.
			if _, err := tx.Exec(ctx, `
				UPDATE environments SET home_node_id=$2 WHERE id=$1
			`, envID, nodeID); err != nil {
				return err
			}
		}
		if _, err := tx.Exec(ctx, `
			UPDATE nodes
			SET alloc_cpu_millis = alloc_cpu_millis + $2,
			    alloc_memory_bytes = alloc_memory_bytes + $3
			WHERE id=$1
		`, nodeID, peakCPU, peakMem); err != nil {
			return err
		}
		tag, err := tx.Exec(ctx, `
			UPDATE deployments SET state='scheduling', failure_reason=NULL
			WHERE id=$1 AND state='pending'
		`, depID)
		if err != nil {
			return err
		}
		if tag.RowsAffected() == 0 {
			return fmt.Errorf("%w: illegal transition to scheduling", ErrConflict)
		}
		return appendDeploymentEventTx(ctx, tx, depID, "deployment.state_changed",
			"deployment entered scheduling", map[string]any{"state": DeployScheduling})
	})
}

type DesiredInstance struct {
	InstanceID   uuid.UUID
	DeploymentID uuid.UUID
	Env8         string
	ProjectName  string
	Slot         string
	Revision     int
	ServiceName  string
	Swappable    bool
	Service      spec.Service
	State        InstanceState
	ContainerID  string
}

// DesiredStateForNode returns the instances a node must run, joined to the
// resolved Service spec from their deployment. Only deployments in an active
// rollout state are included: a superseded or failed deployment's instances
// vanish from desired state, so the agent garbage-collects their containers.
func (s *Store) DesiredStateForNode(ctx context.Context, nodeID uuid.UUID) ([]DesiredInstance, error) {
	rows, err := s.pool.Query(ctx, `
		SELECT si.id, si.deployment_id, d.environment_id, d.project_name, d.slot,
		       d.revision, si.service_name, si.swappable, si.state,
		       COALESCE(si.container_id,''), d.resolved_spec
		FROM service_instances si
		JOIN deployments d ON d.id = si.deployment_id
		WHERE si.node_id = $1
		  AND d.state IN ('scheduling','starting','healthy','live')
		ORDER BY d.revision, si.service_name
	`, nodeID)
	if err != nil {
		return nil, mapErr(err)
	}
	defer rows.Close()

	out := []DesiredInstance{}
	for rows.Next() {
		var di DesiredInstance
		var envID uuid.UUID
		var specJSON []byte
		if err := rows.Scan(&di.InstanceID, &di.DeploymentID, &envID, &di.ProjectName,
			&di.Slot, &di.Revision, &di.ServiceName, &di.Swappable, &di.State,
			&di.ContainerID, &specJSON); err != nil {
			return nil, err
		}
		var ds spec.DeploymentSpec
		if err := json.Unmarshal(specJSON, &ds); err != nil {
			return nil, err
		}
		svc, ok := ds.Services[di.ServiceName]
		if !ok {
			// The instance references a service not in its own spec — a
			// scheduler bug. Skip rather than hand the agent a zero Service.
			continue
		}
		di.Service = svc
		di.Env8 = shortID(envID)
		out = append(out, di)
	}
	return out, rows.Err()
}

type ObservedInstance struct {
	State        InstanceState
	ContainerID  string
	HealthStatus string
	LastError    string
	RestartCount int
	SetStarted   bool
	// IngressPort is the host port the agent observed Docker assign to this
	// instance's published ingress port. Zero means "nothing published", which
	// includes "not an ingress service" and "container not up yet" — the router
	// omits a route it has no port for rather than guessing one.
	IngressPort int
}

func (s *Store) ReportInstance(ctx context.Context, nodeID, instanceID uuid.UUID, o ObservedInstance) error {
	tag, err := s.pool.Exec(ctx, `
		UPDATE service_instances SET
			state         = $2,
			container_id  = NULLIF($3,''),
			health_status = NULLIF($4,''),
			last_error    = NULLIF($5,''),
			restart_count = $6,
			started_at    = CASE WHEN $7 AND started_at IS NULL THEN now() ELSE started_at END,
			ingress_port  = NULLIF($9, 0),
			updated_at    = now()
		WHERE id = $1 AND node_id = $8
	`, instanceID, o.State, o.ContainerID, o.HealthStatus, o.LastError, o.RestartCount, o.SetStarted, nodeID, o.IngressPort)
	if err != nil {
		return mapErr(err)
	}
	if tag.RowsAffected() == 0 {
		return ErrNotFound
	}
	return nil
}

func (s *Store) InstanceStates(ctx context.Context, deploymentID uuid.UUID) ([]InstanceState, error) {
	rows, err := s.pool.Query(ctx, `
		SELECT state FROM service_instances WHERE deployment_id=$1
	`, deploymentID)
	if err != nil {
		return nil, mapErr(err)
	}
	defer rows.Close()
	out := []InstanceState{}
	for rows.Next() {
		var st InstanceState
		if err := rows.Scan(&st); err != nil {
			return nil, err
		}
		out = append(out, st)
	}
	return out, rows.Err()
}

func (s *Store) DeleteInstances(ctx context.Context, deploymentID uuid.UUID) error {
	err := s.tx(ctx, func(tx pgx.Tx) error {
		var nodeID *uuid.UUID
		var specJSON []byte
		err := tx.QueryRow(ctx, `
			SELECT si.node_id, d.resolved_spec
			FROM deployments d
			LEFT JOIN service_instances si ON si.deployment_id = d.id
			WHERE d.id=$1
			LIMIT 1
		`, deploymentID).Scan(&nodeID, &specJSON)
		if err != nil {
			return err
		}
		tag, err := tx.Exec(ctx, `DELETE FROM service_instances WHERE deployment_id=$1`, deploymentID)
		if err != nil {
			return err
		}
		if tag.RowsAffected() == 0 || nodeID == nil || len(specJSON) == 0 {
			return nil
		}
		var ds spec.DeploymentSpec
		if err := json.Unmarshal(specJSON, &ds); err != nil {
			return err
		}
		_, err = tx.Exec(ctx, `
			UPDATE nodes
			SET alloc_cpu_millis = GREATEST(0, alloc_cpu_millis - $2),
			    alloc_memory_bytes = GREATEST(0, alloc_memory_bytes - $3)
			WHERE id=$1
		`, *nodeID, ds.PeakCPUMillis(), ds.PeakMemoryBytes())
		return err
	})
	return mapErr(err)
}
