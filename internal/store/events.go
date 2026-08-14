package store

import (
	"context"
	"encoding/json"
	"fmt"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
)

func appendEventTx(ctx context.Context, tx pgx.Tx, orgID uuid.UUID, deploymentID, nodeID *uuid.UUID, kind, message string, payload any) error {
	b, err := json.Marshal(payload)
	if err != nil {
		return fmt.Errorf("marshal event payload: %w", err)
	}
	_, err = tx.Exec(ctx, `
		INSERT INTO events (org_id, deployment_id, node_id, kind, message, payload)
		VALUES ($1,$2,$3,$4,$5,$6)
	`, orgID, deploymentID, nodeID, kind, message, b)
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
		SELECT id, org_id, deployment_id, node_id, kind, message, payload, created_at
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
		if err := rows.Scan(&e.ID, &e.OrgID, &e.DeploymentID, &e.NodeID, &e.Kind, &e.Message, &payload, &e.CreatedAt); err != nil {
			return nil, err
		}
		if err := json.Unmarshal(payload, &e.Payload); err != nil {
			return nil, err
		}
		out = append(out, e)
	}
	return out, rows.Err()
}
