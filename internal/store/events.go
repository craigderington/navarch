package store

import (
	"context"
	"encoding/json"
	"fmt"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
)

// actorKey carries who asked for the work an event describes.
//
// On the context rather than through every signature, deliberately. Events are
// appended deep inside store transactions — CreateDeployment, PromoteDeployment,
// SetSecret, RegisterNode, the preview reaper — and threading an actor argument
// down to each of them would churn a dozen signatures and every one of their
// callers, including the control-plane loops that legitimately have no actor at
// all. The actor is ambient request metadata, which is what a context value is
// for.
//
// The key lives here rather than in internal/api because the dependency runs
// api → store. The store must not learn about the HTTP layer to record who
// called it.
type actorKey struct{}

type actor struct {
	id    *uuid.UUID
	email string
}

// WithActor tags a context with the operator responsible for the work done
// under it. Every event appended in that context names them. A context without
// one produces events with no actor, which is the honest record for the
// scheduler, controller and reaper: nobody asked, a loop noticed.
func WithActor(ctx context.Context, id uuid.UUID, email string) context.Context {
	return context.WithValue(ctx, actorKey{}, actor{id: &id, email: email})
}

func actorFrom(ctx context.Context) actor {
	a, _ := ctx.Value(actorKey{}).(actor)
	return a
}

func appendEventTx(ctx context.Context, tx pgx.Tx, orgID uuid.UUID, deploymentID, nodeID *uuid.UUID, kind, message string, payload any) error {
	b, err := json.Marshal(payload)
	if err != nil {
		return fmt.Errorf("marshal event payload: %w", err)
	}
	a := actorFrom(ctx)
	_, err = tx.Exec(ctx, `
		INSERT INTO events (org_id, deployment_id, node_id, kind, message, payload,
		                    actor_operator_id, actor_email)
		VALUES ($1,$2,$3,$4,$5,$6,$7,NULLIF($8,''))
	`, orgID, deploymentID, nodeID, kind, message, b, a.id, a.email)
	return err
}

func appendDeploymentEventTx(ctx context.Context, tx pgx.Tx, deploymentID uuid.UUID, kind, message string, payload any) error {
	var orgID uuid.UUID
	if err := tx.QueryRow(ctx, `
		SELECT a.org_id FROM deployments d
		JOIN environments e ON e.id=d.environment_id
		JOIN stacks s ON s.id=e.stack_id
		JOIN applications a ON a.id=s.app_id
		WHERE d.id=$1
	`, deploymentID).Scan(&orgID); err != nil {
		return err
	}
	return appendEventTx(ctx, tx, orgID, &deploymentID, nil, kind, message, payload)
}

func appendEnvironmentEventTx(ctx context.Context, tx pgx.Tx, envID uuid.UUID, kind, message string, payload any) error {
	var orgID uuid.UUID
	if err := tx.QueryRow(ctx, `
		SELECT a.org_id FROM environments e
		JOIN stacks s ON s.id=e.stack_id
		JOIN applications a ON a.id=s.app_id
		WHERE e.id=$1
	`, envID).Scan(&orgID); err != nil {
		return err
	}
	return appendEventTx(ctx, tx, orgID, nil, nil, kind, message, payload)
}

// ListEvents returns an organization's timeline newest first. beforeID enables
// stable cursor pagination while new events are being appended.
func (s *Store) ListEvents(ctx context.Context, orgID uuid.UUID, beforeID int64, limit int) ([]Event, error) {
	if limit <= 0 || limit > 200 {
		limit = 50
	}
	rows, err := s.pool.Query(ctx, `
		SELECT id, org_id, deployment_id, node_id, kind, message, payload, created_at,
		       actor_operator_id, actor_email
		FROM events
		WHERE org_id=$1 AND ($2::bigint <= 0 OR id < $2)
		ORDER BY id DESC LIMIT $3
	`, orgID, beforeID, limit)
	if err != nil {
		return nil, mapErr(err)
	}
	defer rows.Close()
	out := []Event{}
	for rows.Next() {
		var e Event
		var payload []byte
		if err := rows.Scan(&e.ID, &e.OrgID, &e.DeploymentID, &e.NodeID, &e.Kind, &e.Message,
			&payload, &e.CreatedAt, &e.ActorOperatorID, &e.ActorEmail); err != nil {
			return nil, err
		}
		if err := json.Unmarshal(payload, &e.Payload); err != nil {
			return nil, err
		}
		out = append(out, e)
	}
	return out, rows.Err()
}
