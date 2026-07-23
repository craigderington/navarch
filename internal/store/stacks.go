package store

import (
	"context"
	"encoding/json"

	"github.com/google/uuid"

	"github.com/craig/composectl/internal/spec"
)

// CreateStackVersion appends a new immutable version of a stack's compose
// definition. If the spec digest matches the current latest version, the
// existing version is returned instead of creating a duplicate — deploying
// an unchanged stack should not manufacture revision churn.
func (s *Store) CreateStackVersion(ctx context.Context, stackID uuid.UUID, raw string, dspec *spec.DeploymentSpec, createdBy string) (*StackVersion, error) {
	digest, err := dspec.Digest()
	if err != nil {
		return nil, err
	}

	if latest, err := s.LatestStackVersion(ctx, stackID); err == nil {
		if latest.SpecDigest == digest {
			return latest, nil
		}
	} else if err != ErrNotFound {
		return nil, err
	}

	specJSON, err := json.Marshal(dspec)
	if err != nil {
		return nil, err
	}

	var sv StackVersion
	err = s.pool.QueryRow(ctx, `
		INSERT INTO stack_versions (stack_id, version, raw_compose, spec_json, spec_digest, created_by)
		VALUES (
			$1,
			(SELECT COALESCE(MAX(version), 0) + 1 FROM stack_versions WHERE stack_id = $1),
			$2, $3, $4, $5
		)
		RETURNING id, stack_id, version, spec_digest, COALESCE(created_by,''), created_at
	`, stackID, raw, specJSON, digest, createdBy).
		Scan(&sv.ID, &sv.StackID, &sv.Version, &sv.SpecDigest, &sv.CreatedBy, &sv.CreatedAt)
	if err != nil {
		return nil, mapErr(err)
	}
	sv.Spec = dspec
	sv.RawCompose = raw
	return &sv, nil
}

func (s *Store) GetStackVersion(ctx context.Context, id uuid.UUID) (*StackVersion, error) {
	var sv StackVersion
	var specJSON []byte
	err := s.pool.QueryRow(ctx, `
		SELECT id, stack_id, version, spec_json, spec_digest,
		       COALESCE(created_by,''), created_at
		FROM stack_versions WHERE id = $1
	`, id).Scan(&sv.ID, &sv.StackID, &sv.Version, &specJSON,
		&sv.SpecDigest, &sv.CreatedBy, &sv.CreatedAt)
	if err != nil {
		return nil, mapErr(err)
	}
	if err := json.Unmarshal(specJSON, &sv.Spec); err != nil {
		return nil, err
	}
	return &sv, nil
}

// ListStackVersions returns a stack's versions, newest first. The spec is
// deliberately not selected: it is the largest column in the row and a
// listing is a browse, not a fetch. Use GetStackVersion for the spec.
func (s *Store) ListStackVersions(ctx context.Context, stackID uuid.UUID, limit int) ([]StackVersion, error) {
	if limit <= 0 || limit > 200 {
		limit = 50
	}
	rows, err := s.pool.Query(ctx, `
		SELECT id, stack_id, version, spec_digest,
		       COALESCE(created_by,''), created_at
		FROM stack_versions
		WHERE stack_id = $1
		ORDER BY version DESC
		LIMIT $2
	`, stackID, limit)
	if err != nil {
		return nil, mapErr(err)
	}
	defer rows.Close()

	out := []StackVersion{}
	for rows.Next() {
		var sv StackVersion
		if err := rows.Scan(&sv.ID, &sv.StackID, &sv.Version, &sv.SpecDigest,
			&sv.CreatedBy, &sv.CreatedAt); err != nil {
			return nil, err
		}
		out = append(out, sv)
	}
	return out, rows.Err()
}

func (s *Store) LatestStackVersion(ctx context.Context, stackID uuid.UUID) (*StackVersion, error) {
	var sv StackVersion
	var specJSON []byte
	err := s.pool.QueryRow(ctx, `
		SELECT id, stack_id, version, spec_json, spec_digest,
		       COALESCE(created_by,''), created_at
		FROM stack_versions
		WHERE stack_id = $1
		ORDER BY version DESC
		LIMIT 1
	`, stackID).Scan(&sv.ID, &sv.StackID, &sv.Version, &specJSON,
		&sv.SpecDigest, &sv.CreatedBy, &sv.CreatedAt)
	if err != nil {
		return nil, mapErr(err)
	}
	if err := json.Unmarshal(specJSON, &sv.Spec); err != nil {
		return nil, err
	}
	return &sv, nil
}

func (s *Store) GetEnvironment(ctx context.Context, id uuid.UUID) (*Environment, error) {
	var e Environment
	var configJSON []byte
	err := s.pool.QueryRow(ctx, `
		SELECT id, stack_id, slug, strategy, COALESCE(hostname,''),
		       config, live_deployment_id, created_at
		FROM environments WHERE id = $1
	`, id).Scan(&e.ID, &e.StackID, &e.Slug, &e.Strategy, &e.Hostname,
		&configJSON, &e.LiveDeploymentID, &e.CreatedAt)
	if err != nil {
		return nil, mapErr(err)
	}
	if err := json.Unmarshal(configJSON, &e.Config); err != nil {
		return nil, err
	}
	return &e, nil
}
