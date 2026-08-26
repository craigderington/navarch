package store

import (
	"context"
	"errors"
	"fmt"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
)

// Log requests are instructions, never answers. No method here reads or writes
// log content: chunks are held in memory by the control plane and dropped once
// delivered, because container stdout routinely carries the secret plaintext
// this package's own secrets table exists to keep out of Postgres.

type LogRequestState string

const (
	LogPending LogRequestState = "pending"
	LogDone    LogRequestState = "done"
	LogFailed  LogRequestState = "failed"
)

// Bounds on what one request may pull. They are enforced here rather than in the
// handler because the agent acts on whatever the row says: a request that asked
// for a million lines would be honoured by a node that has no way to know the
// number was unreasonable.
const (
	MaxLogTailLines     = 5000
	DefaultLogTailLines = 200
	// LogRequestTTL bounds a request whose requester walked away. Following
	// keeps one row alive for a whole tail session, so this is the window a tail
	// may be idle in, not the length of a session.
	LogRequestTTL = 15 * time.Minute
)

type LogRequest struct {
	ID            uuid.UUID       `json:"id"`
	InstanceID    uuid.UUID       `json:"instance_id"`
	EnvironmentID uuid.UUID       `json:"environment_id"`
	ServiceName   string          `json:"service_name"`
	TailLines     int             `json:"tail_lines"`
	Follow        bool            `json:"follow"`
	State         LogRequestState `json:"state"`
	LastError     string          `json:"last_error,omitempty"`
	CreatedAt     time.Time       `json:"created_at"`
	ExpiresAt     time.Time       `json:"expires_at"`
}

type CreateLogRequestParams struct {
	EnvironmentID uuid.UUID
	ServiceName   string
	TailLines     int
	Follow        bool
}

// CreateLogRequest resolves an environment and service name to the one container
// currently running it, and records an instruction for that container's node.
//
// The resolution happens HERE, not on the agent. The agent is handed a container
// id it already runs and never decides whether a request is legitimate — an
// agent that could be asked for an arbitrary container id would be one
// compromised control plane away from reading every tenant's output on that
// node. It also means an unplaced or not-yet-started service fails now, with a
// reason, rather than as an agent-side silence the requester cannot interpret.
func (s *Store) CreateLogRequest(ctx context.Context, p CreateLogRequestParams) (*LogRequest, error) {
	if p.TailLines <= 0 {
		p.TailLines = DefaultLogTailLines
	}
	if p.TailLines > MaxLogTailLines {
		return nil, fmt.Errorf("%w: tail must be at most %d lines", ErrInvalid, MaxLogTailLines)
	}
	if p.ServiceName == "" {
		return nil, fmt.Errorf("%w: service is required", ErrInvalid)
	}

	var lr LogRequest
	err := s.tx(ctx, func(tx pgx.Tx) error {
		// Prefer the live deployment: during a blue/green rollout both revisions
		// have a container for the service, and the one serving traffic is the
		// one an operator asking for "the logs" means. Falling back to the newest
		// active revision is what makes this useful *during* a rollout, which is
		// exactly when someone reaches for logs.
		var instanceID uuid.UUID
		var containerID *string
		err := tx.QueryRow(ctx, `
			SELECT si.id, si.container_id
			FROM service_instances si
			JOIN deployments d ON d.id = si.deployment_id
			JOIN environments e ON e.id = d.environment_id
			WHERE e.id = $1 AND si.service_name = $2
			  AND d.state IN ('scheduling','starting','healthy','live')
			ORDER BY (d.state = 'live') DESC, d.revision DESC
			LIMIT 1
		`, p.EnvironmentID, p.ServiceName).Scan(&instanceID, &containerID)
		if err != nil {
			return err // pgx.ErrNoRows -> ErrNotFound via mapErr
		}
		if containerID == nil || *containerID == "" {
			// Nothing to read yet. Distinguished from "no such service" because
			// the two need different reactions: wait, versus fix the name.
			return fmt.Errorf("%w: service %q has no running container yet", ErrConflict, p.ServiceName)
		}
		return tx.QueryRow(ctx, `
			INSERT INTO log_requests
				(instance_id, environment_id, service_name, container_id,
				 tail_lines, follow, expires_at)
			VALUES ($1,$2,$3,$4,$5,$6, now() + make_interval(secs => $7))
			RETURNING id, instance_id, environment_id, service_name, tail_lines,
			          follow, state, COALESCE(last_error,''), created_at, expires_at
		`, instanceID, p.EnvironmentID, p.ServiceName, *containerID,
			p.TailLines, p.Follow, LogRequestTTL.Seconds()).
			Scan(&lr.ID, &lr.InstanceID, &lr.EnvironmentID, &lr.ServiceName,
				&lr.TailLines, &lr.Follow, &lr.State, &lr.LastError, &lr.CreatedAt, &lr.ExpiresAt)
	})
	if err != nil {
		return nil, mapErr(err)
	}
	return &lr, nil
}

// PendingLogRequest is what an agent is told to read: a container id it already
// runs, and bounds on how much of it.
type PendingLogRequest struct {
	ID          uuid.UUID  `json:"id"`
	ContainerID string     `json:"container_id"`
	TailLines   int        `json:"tail_lines"`
	SinceAt     *time.Time `json:"since_at,omitempty"`
	Follow      bool       `json:"follow"`
	ServiceName string     `json:"service_name"`
}

// LogRequestsForNode returns the instructions this node must act on. Scoped by
// the instance's node_id, so a node is only ever offered work for containers it
// owns — the same reason desired-state is scoped that way.
func (s *Store) LogRequestsForNode(ctx context.Context, nodeID uuid.UUID) ([]PendingLogRequest, error) {
	rows, err := s.pool.Query(ctx, `
		SELECT lr.id, lr.container_id, lr.tail_lines, lr.since_at, lr.follow, lr.service_name
		FROM log_requests lr
		JOIN service_instances si ON si.id = lr.instance_id
		WHERE si.node_id = $1 AND lr.state = 'pending' AND lr.expires_at > now()
		ORDER BY lr.created_at
	`, nodeID)
	if err != nil {
		return nil, mapErr(err)
	}
	defer rows.Close()
	out := []PendingLogRequest{}
	for rows.Next() {
		var p PendingLogRequest
		if err := rows.Scan(&p.ID, &p.ContainerID, &p.TailLines, &p.SinceAt, &p.Follow, &p.ServiceName); err != nil {
			return nil, err
		}
		out = append(out, p)
	}
	return out, rows.Err()
}

// LogRequestOwnedByNode reports whether nodeID owns the container a request
// reads. It exists so the API can gate the *content* of a delivery on
// ownership before buffering it: CompleteLogRequest enforces the same scoping
// for the row update, but by then a handler that wrote content first would
// have already accepted a foreign node's bytes into the operator's tail —
// and writing after the update flips a one-shot request to done before its
// content lands, a window in which the reader is told "finished" and shown
// nothing. Check without mutating, then buffer, then complete.
func (s *Store) LogRequestOwnedByNode(ctx context.Context, nodeID, requestID uuid.UUID) (bool, error) {
	var one int
	err := s.pool.QueryRow(ctx, `
		SELECT 1
		FROM log_requests lr
		JOIN service_instances si ON si.id = lr.instance_id
		WHERE lr.id = $1 AND si.node_id = $2
	`, requestID, nodeID).Scan(&one)
	if errors.Is(err, pgx.ErrNoRows) {
		return false, nil
	}
	if err != nil {
		return false, mapErr(err)
	}
	return true, nil
}

// CompleteLogRequest records the outcome of a delivery. It takes no content:
// the chunk goes to the in-memory buffer, and this only moves the instruction on.
//
// A following request goes back to pending with since_at advanced to now, so the
// next tick asks Docker only for what is new. Everything else is terminal.
//
// nodeID is checked, not trusted: an agent may only complete a request for a
// container on its own node.
func (s *Store) CompleteLogRequest(ctx context.Context, nodeID, requestID uuid.UUID, failure string) error {
	tag, err := s.pool.Exec(ctx, `
		UPDATE log_requests lr SET
			state      = (CASE
			                WHEN $3 <> '' THEN 'failed'
			                WHEN lr.follow THEN 'pending'
			                ELSE 'done'
			              END)::log_request_state,
			since_at   = CASE WHEN $3 = '' AND lr.follow THEN now() ELSE lr.since_at END,
			last_error = NULLIF($3, ''),
			updated_at = now()
		FROM service_instances si
		WHERE lr.id = $2 AND si.id = lr.instance_id AND si.node_id = $1
	`, nodeID, requestID, failure)
	if err != nil {
		return mapErr(err)
	}
	if tag.RowsAffected() == 0 {
		return ErrNotFound
	}
	return nil
}

// GetLogRequest reads an instruction's current state for the requester. The
// content it asked for lives in the control plane's buffer, not here.
func (s *Store) GetLogRequest(ctx context.Context, id uuid.UUID) (*LogRequest, error) {
	var lr LogRequest
	err := s.pool.QueryRow(ctx, `
		SELECT id, instance_id, environment_id, service_name, tail_lines,
		       follow, state, COALESCE(last_error,''), created_at, expires_at
		FROM log_requests WHERE id = $1
	`, id).Scan(&lr.ID, &lr.InstanceID, &lr.EnvironmentID, &lr.ServiceName,
		&lr.TailLines, &lr.Follow, &lr.State, &lr.LastError, &lr.CreatedAt, &lr.ExpiresAt)
	if err != nil {
		return nil, mapErr(err)
	}
	return &lr, nil
}

// CloseLogRequest ends a request early, which is what a `--follow` session does
// when the operator stops watching. Without it a followed request stays pending
// and its node keeps reading Docker every tick for output nobody reads.
func (s *Store) CloseLogRequest(ctx context.Context, id uuid.UUID) error {
	_, err := s.pool.Exec(ctx,
		`UPDATE log_requests SET state='done', updated_at=now() WHERE id=$1 AND state='pending'`, id)
	return mapErr(err)
}

// SweepLogRequests deletes expired instructions and returns their ids so the
// caller can free the matching in-memory buffers. Returning the ids is the point:
// a sweep that only deleted rows would leave the content behind, which is the one
// thing this design promises not to do.
func (s *Store) SweepLogRequests(ctx context.Context) ([]uuid.UUID, error) {
	rows, err := s.pool.Query(ctx,
		`DELETE FROM log_requests WHERE expires_at < now() RETURNING id`)
	if err != nil {
		return nil, mapErr(err)
	}
	defer rows.Close()
	out := []uuid.UUID{}
	for rows.Next() {
		var id uuid.UUID
		if err := rows.Scan(&id); err != nil {
			return nil, err
		}
		out = append(out, id)
	}
	return out, rows.Err()
}
