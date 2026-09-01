package store

import (
	"context"
	"encoding/json"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"

	"github.com/craigderington/navarch/internal/spec"
)

type CreatePreviewParams struct {
	// EnvironmentID is supplied by the caller rather than defaulted by the
	// column, because the generated hostname embeds env8 and therefore needs
	// the id before the row exists. Zero means "let the store pick one" for
	// callers with no such need.
	EnvironmentID uuid.UUID
	StackID       uuid.UUID
	Slug          string
	Hostname      string
	TTL           time.Duration
	// InheritSecretsFrom copies that environment's latest ciphertext. Nil
	// means the preview starts with no secrets.
	InheritSecretsFrom *uuid.UUID
	StackVersionID     uuid.UUID
	ResolvedSpec       *spec.DeploymentSpec
	CreatedBy          string
}

// CreatePreview creates an ephemeral environment, copies the source
// environment's secrets into it, and creates its first deployment -- all in
// one transaction, so a preview never exists in a half-built state.
func (s *Store) CreatePreview(ctx context.Context, p CreatePreviewParams) (*Environment, *Deployment, error) {
	if err := validateSlug("slug", p.Slug); err != nil {
		return nil, nil, err
	}
	if err := validateHostname(p.Hostname); err != nil {
		return nil, nil, err
	}

	envID := p.EnvironmentID
	if envID == uuid.Nil {
		envID = uuid.New()
	}

	var env Environment
	var dep *Deployment

	err := s.tx(ctx, func(tx pgx.Tx) error {
		var config []byte
		// make_interval(secs => ...) is the house pattern for every
		// Duration→interval conversion in this package. Duration.String()
		// renders a sub-second value as e.g. "1ns", which Postgres's interval
		// literal parser rejects outright. This TTL is hour-scale and would
		// survive the literal form, but keeping one idiom means the next store
		// method added here has nothing wrong to copy.
		err := tx.QueryRow(ctx, `
			INSERT INTO environments (id, stack_id, slug, hostname, config, ephemeral, expires_at)
			VALUES ($1, $2, $3, NULLIF($4,''), '{}'::jsonb, true, now() + make_interval(secs => $5))
			RETURNING id, stack_id, slug, strategy, COALESCE(hostname,''),
			          config, live_deployment_id, ephemeral, expires_at, created_at
		`, envID, p.StackID, p.Slug, p.Hostname, p.TTL.Seconds()).
			Scan(&env.ID, &env.StackID, &env.Slug, &env.Strategy, &env.Hostname,
				&config, &env.LiveDeploymentID, &env.Ephemeral, &env.ExpiresAt, &env.CreatedAt)
		if err != nil {
			return err
		}
		if err := json.Unmarshal(config, &env.Config); err != nil {
			return err
		}

		// Ciphertext moves verbatim: it is sealed to node recipients, not to
		// an environment, so no re-encryption (and no plaintext) is involved.
		// Copies land at version 1 because version is per (environment, key)
		// and this environment has no history.
		if p.InheritSecretsFrom != nil {
			if _, err := tx.Exec(ctx, `
				INSERT INTO secrets (environment_id, key, ciphertext, key_id, version)
				SELECT $1, s.key, s.ciphertext, s.key_id, 1
				FROM secrets s
				WHERE s.environment_id = $2
				  AND s.version = (SELECT MAX(version) FROM secrets s2
				                    WHERE s2.environment_id = s.environment_id
				                      AND s2.key = s.key)
			`, env.ID, *p.InheritSecretsFrom); err != nil {
				return err
			}
		}

		dep, err = s.createDeploymentTx(ctx, tx, CreateDeploymentParams{
			EnvironmentID:  env.ID,
			StackVersionID: p.StackVersionID,
			ResolvedSpec:   p.ResolvedSpec,
			CreatedBy:      p.CreatedBy,
		})
		return err
	})
	if err != nil {
		return nil, nil, mapErr(err)
	}
	return &env, dep, nil
}

// ExpireEnvironments deletes every expired preview and returns the env8 of
// each. Deleting the environment cascades its deployments, instances, volumes
// and secrets, which is how the agent GCs the swappable containers; the
// tombstone written here is what later tells the agent to destroy the pinned
// container and named volumes too.
//
// The tombstone is written before the delete and in the same transaction: the
// instruction to destroy durable state must be durable before the state
// describing it is gone. If the transaction aborts the environment survives and
// is retried next tick, which is the safe direction to fail.
func (s *Store) ExpireEnvironments(ctx context.Context) ([]string, error) {
	return s.expireEnvironments(ctx, nil)
}

// ExpireEnvironmentsForOrg is the scoped form used by isolated loop tests and
// future sharded control-plane workers.
func (s *Store) ExpireEnvironmentsForOrg(ctx context.Context, orgID uuid.UUID) ([]string, error) {
	return s.expireEnvironments(ctx, &orgID)
}

func (s *Store) expireEnvironments(ctx context.Context, orgID *uuid.UUID) ([]string, error) {
	var reaped []string
	err := s.tx(ctx, func(tx pgx.Tx) error {
		// SKIP LOCKED because Sprint 4 runs more than one control plane and two
		// reapers racing the same environment would double-tombstone.
		rows, err := tx.Query(ctx, `
			SELECT e.id, a.org_id
			FROM environments e
			JOIN stacks       s ON s.id = e.stack_id
			JOIN applications a ON a.id = s.app_id
			WHERE e.ephemeral AND e.expires_at < now()
			  AND ($1::uuid IS NULL OR a.org_id=$1)
			FOR UPDATE OF e SKIP LOCKED
		`, orgID)
		if err != nil {
			return err
		}
		type victim struct {
			id    uuid.UUID
			orgID uuid.UUID
		}
		var victims []victim
		for rows.Next() {
			var v victim
			if err := rows.Scan(&v.id, &v.orgID); err != nil {
				rows.Close()
				return err
			}
			victims = append(victims, v)
		}
		rows.Close()
		if err := rows.Err(); err != nil {
			return err
		}

		// The whole batch shares one transaction, so a failure on the Nth
		// victim rolls back the tombstones and deletes already done for
		// 1..N-1. That is the safe direction to fail: those environments
		// simply survive with their expiry still in the past, and the next
		// tick picks them up again. Committing per victim would trade that
		// free retry for a half-reaped batch, and no failure mode here is
		// worth that.
		for _, v := range victims {
			env8 := shortID(v.id)
			if err := appendEventTx(ctx, tx, v.orgID, nil, nil,
				"preview.expired", "preview environment expired",
				map[string]any{"environment_id": v.id, "env8": env8}); err != nil {
				return err
			}
			if _, err := tx.Exec(ctx, `
				INSERT INTO environment_tombstones (env8, org_id) VALUES ($1, $2)
				ON CONFLICT (env8) DO NOTHING
			`, env8, v.orgID); err != nil {
				return err
			}
			// environments.live_deployment_id is deferrable but still checked at
			// commit, so clear it before the cascade removes the deployment it
			// points at.
			if _, err := tx.Exec(ctx,
				`UPDATE environments SET live_deployment_id = NULL WHERE id = $1`, v.id); err != nil {
				return err
			}
			if _, err := tx.Exec(ctx, `DELETE FROM environments WHERE id = $1`, v.id); err != nil {
				return err
			}
			reaped = append(reaped, env8)
		}
		return nil
	})
	if err != nil {
		return nil, mapErr(err)
	}
	return reaped, nil
}

// TombstonesForNode returns the environments this node should destroy: recent
// tombstones in the node's own org. Nodes are org-scoped, so a node must never
// be handed a teardown for an environment it could not have been running.
//
// maxAge goes through make_interval(secs => ...) rather than maxAge.String()
// cast to ::interval: Go renders sub-second durations with a "ns" suffix
// (e.g. "1ns" for the retention-window test below), a unit Postgres's
// interval literal parser does not recognize at all.
func (s *Store) TombstonesForNode(ctx context.Context, nodeID uuid.UUID, maxAge time.Duration) ([]string, error) {
	rows, err := s.pool.Query(ctx, `
		SELECT t.env8
		FROM environment_tombstones t
		JOIN nodes n ON n.org_id = t.org_id
		WHERE n.id = $1 AND t.created_at > now() - make_interval(secs => $2)
		ORDER BY t.created_at DESC
	`, nodeID, maxAge.Seconds())
	if err != nil {
		return nil, mapErr(err)
	}
	defer rows.Close()
	out := []string{}
	for rows.Next() {
		var env8 string
		if err := rows.Scan(&env8); err != nil {
			return nil, err
		}
		out = append(out, env8)
	}
	return out, rows.Err()
}

// SweepTombstones drops instructions no agent will act on any more. Past this
// window an offline node's containers and volumes leak and need manual removal
// -- the window is how long a node may be down and still clean up after itself.
func (s *Store) SweepTombstones(ctx context.Context, maxAge time.Duration) error {
	return s.sweepTombstones(ctx, nil, maxAge)
}

func (s *Store) SweepTombstonesForOrg(ctx context.Context, orgID uuid.UUID, maxAge time.Duration) error {
	return s.sweepTombstones(ctx, &orgID, maxAge)
}

func (s *Store) sweepTombstones(ctx context.Context, orgID *uuid.UUID, maxAge time.Duration) error {
	_, err := s.pool.Exec(ctx,
		`DELETE FROM environment_tombstones
		 WHERE created_at < now() - make_interval(secs => $1)
		   AND ($2::uuid IS NULL OR org_id=$2)`,
		maxAge.Seconds(), orgID)
	return mapErr(err)
}

// ExpiringPreview is a preview environment about to be reaped.
type ExpiringPreview struct {
	ID        uuid.UUID
	ExpiresAt time.Time
}

// ClaimPreviewsForExpiryWarning returns previews expiring within the window
// that have not been warned about, and marks them warned in the same statement.
//
// Claim-and-mark is one UPDATE ... RETURNING because the reaper ticks every
// second: a separate check and mark would let two ticks — or two control planes
// — both pass the check, and the result would be a mail per tick rather than a
// warning. The cost is that a warning whose send then fails is never retried.
// That is the right direction here and the opposite of what a retry loop would
// give: a missed warning about an expected event is a small loss, and a flood
// of them is the kind of noise that teaches operators to filter the sender.
//
// Already-expired previews are excluded. The reaper deletes those on the same
// tick, and warning about something already gone is worse than silence.
func (s *Store) ClaimPreviewsForExpiryWarning(ctx context.Context, within time.Duration) ([]ExpiringPreview, error) {
	return s.claimPreviewsForExpiryWarning(ctx, within, nil)
}

// ClaimPreviewsForExpiryWarningForOrg is the scoped form, used by isolated loop
// tests for the reason the other scoped variants exist: parallel test binaries
// share one database and must not claim one another's fixtures.
func (s *Store) ClaimPreviewsForExpiryWarningForOrg(ctx context.Context, within time.Duration, orgID uuid.UUID) ([]ExpiringPreview, error) {
	return s.claimPreviewsForExpiryWarning(ctx, within, &orgID)
}

func (s *Store) claimPreviewsForExpiryWarning(ctx context.Context, within time.Duration, orgID *uuid.UUID) ([]ExpiringPreview, error) {
	// make_interval(secs => ...) rather than a ::interval cast of
	// Duration.String(): the string form renders a sub-second value as "1ns",
	// which Postgres's interval parser rejects outright.
	rows, err := s.pool.Query(ctx, `
		UPDATE environments e
		SET expiry_warned_at = now()
		FROM stacks s, applications a
		WHERE e.stack_id = s.id AND s.app_id = a.id
		  AND e.ephemeral
		  AND e.expiry_warned_at IS NULL
		  AND e.expires_at IS NOT NULL
		  AND e.expires_at > now()
		  AND e.expires_at < now() + make_interval(secs => $1)
		  AND ($2::uuid IS NULL OR a.org_id = $2)
		RETURNING e.id, e.expires_at
	`, within.Seconds(), orgID)
	if err != nil {
		return nil, mapErr(err)
	}
	defer rows.Close()
	out := []ExpiringPreview{}
	for rows.Next() {
		var p ExpiringPreview
		if err := rows.Scan(&p.ID, &p.ExpiresAt); err != nil {
			return nil, err
		}
		out = append(out, p)
	}
	return out, rows.Err()
}
