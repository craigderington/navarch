package store

import (
	"context"
	"encoding/json"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"

	"github.com/craig/composectl/internal/spec"
)

type CreatePreviewParams struct {
	StackID  uuid.UUID
	Slug     string
	Hostname string
	TTL      time.Duration
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

	var env Environment
	var dep *Deployment

	err := s.tx(ctx, func(tx pgx.Tx) error {
		var config []byte
		// TTL.String() (e.g. "1h0m0s") is passed as the interval literal
		// directly -- Postgres's interval parser accepts Go's duration
		// rendering as-is, so no reformatting is needed here.
		err := tx.QueryRow(ctx, `
			INSERT INTO environments (stack_id, slug, hostname, config, ephemeral, expires_at)
			VALUES ($1, $2, NULLIF($3,''), '{}'::jsonb, true, now() + $4::interval)
			RETURNING id, stack_id, slug, strategy, COALESCE(hostname,''),
			          config, live_deployment_id, ephemeral, expires_at, created_at
		`, p.StackID, p.Slug, p.Hostname, p.TTL.String()).
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
