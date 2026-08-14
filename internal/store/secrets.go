package store

import (
	"context"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
)

type SecretMeta struct {
	Key       string    `json:"key"`
	Version   int       `json:"version"`
	CreatedAt time.Time `json:"created_at"`
}

type EncryptedSecret struct {
	Key        string `json:"key"`
	Ciphertext []byte `json:"ciphertext"` // JSON-marshals as base64
}

// SetSecret stores a new version of a secret's ciphertext. Latest-version-wins;
// the agent reads the highest version. The control plane never sees plaintext
// at rest — only the ciphertext handed to it.
func (s *Store) SetSecret(ctx context.Context, envID uuid.UUID, key string, ciphertext []byte, keyID string) error {
	err := s.tx(ctx, func(tx pgx.Tx) error {
		var version int
		if err := tx.QueryRow(ctx, `
			INSERT INTO secrets (environment_id, key, ciphertext, key_id, version)
			VALUES ($1, $2, $3, $4,
				(SELECT COALESCE(MAX(version),0)+1 FROM secrets WHERE environment_id=$1 AND key=$2))
			RETURNING version
		`, envID, key, ciphertext, keyID).Scan(&version); err != nil {
			return err
		}
		return appendEnvironmentEventTx(ctx, tx, envID, "secret.set", "secret version stored",
			map[string]any{"key": key, "version": version})
	})
	return mapErr(err)
}

func (s *Store) SecretKeysForEnv(ctx context.Context, envID uuid.UUID) ([]SecretMeta, error) {
	rows, err := s.pool.Query(ctx, `
		SELECT key, MAX(version), MAX(created_at)
		FROM secrets WHERE environment_id=$1
		GROUP BY key ORDER BY key
	`, envID)
	if err != nil {
		return nil, mapErr(err)
	}
	defer rows.Close()
	out := []SecretMeta{}
	for rows.Next() {
		var m SecretMeta
		if err := rows.Scan(&m.Key, &m.Version, &m.CreatedAt); err != nil {
			return nil, err
		}
		out = append(out, m)
	}
	return out, rows.Err()
}

func (s *Store) DeleteSecret(ctx context.Context, envID uuid.UUID, key string) error {
	err := s.tx(ctx, func(tx pgx.Tx) error {
		tag, err := tx.Exec(ctx, `DELETE FROM secrets WHERE environment_id=$1 AND key=$2`, envID, key)
		if err != nil {
			return err
		}
		if tag.RowsAffected() == 0 {
			return ErrNotFound
		}
		return appendEnvironmentEventTx(ctx, tx, envID, "secret.deleted", "secret deleted",
			map[string]any{"key": key})
	})
	return mapErr(err)
}

// EncryptedSecretsForNode returns the latest-version ciphertext for every env
// with an active deployment on the node, keyed by env8. The agent decrypts these
// locally to build a per-env secret source.
func (s *Store) EncryptedSecretsForNode(ctx context.Context, nodeID uuid.UUID) (map[string][]EncryptedSecret, error) {
	rows, err := s.pool.Query(ctx, `
		SELECT s.environment_id, s.key, s.ciphertext
		FROM secrets s
		WHERE s.environment_id IN (
			SELECT DISTINCT d.environment_id
			FROM deployments d
			JOIN service_instances si ON si.deployment_id = d.id
			WHERE si.node_id = $1 AND d.state IN ('scheduling','starting','healthy','live')
		)
		AND s.version = (SELECT MAX(version) FROM secrets s2 WHERE s2.environment_id=s.environment_id AND s2.key=s.key)
	`, nodeID)
	if err != nil {
		return nil, mapErr(err)
	}
	defer rows.Close()
	out := map[string][]EncryptedSecret{}
	for rows.Next() {
		var envID uuid.UUID
		var es EncryptedSecret
		if err := rows.Scan(&envID, &es.Key, &es.Ciphertext); err != nil {
			return nil, err
		}
		env8 := shortID(envID)
		out[env8] = append(out[env8], es)
	}
	return out, rows.Err()
}
