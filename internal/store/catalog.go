package store

import (
	"context"
	"encoding/json"
	"fmt"

	"github.com/google/uuid"
)

// This file holds the catalog resources — the org → app → stack → env
// hierarchy a deployment hangs off. They are plain inserts and scoped
// lists; the interesting concurrency lives in deployments.go.

// ------------------------------------------------------------ organizations

func (s *Store) CreateOrganization(ctx context.Context, slug, name string) (*Organization, error) {
	if err := validateSlug("slug", slug); err != nil {
		return nil, err
	}
	var o Organization
	err := s.pool.QueryRow(ctx, `
		INSERT INTO organizations (slug, name)
		VALUES ($1, $2)
		RETURNING id, slug, name, created_at
	`, slug, name).Scan(&o.ID, &o.Slug, &o.Name, &o.CreatedAt)
	if err != nil {
		return nil, mapErr(err)
	}
	return &o, nil
}

func (s *Store) ListOrganizations(ctx context.Context) ([]Organization, error) {
	rows, err := s.pool.Query(ctx, `
		SELECT id, slug, name, created_at FROM organizations ORDER BY slug
	`)
	if err != nil {
		return nil, mapErr(err)
	}
	defer rows.Close()

	out := []Organization{}
	for rows.Next() {
		var o Organization
		if err := rows.Scan(&o.ID, &o.Slug, &o.Name, &o.CreatedAt); err != nil {
			return nil, err
		}
		out = append(out, o)
	}
	return out, rows.Err()
}

// ------------------------------------------------------------- applications

func (s *Store) CreateApplication(ctx context.Context, orgID uuid.UUID, slug, name string) (*Application, error) {
	if err := validateSlug("slug", slug); err != nil {
		return nil, err
	}
	var a Application
	err := s.pool.QueryRow(ctx, `
		INSERT INTO applications (org_id, slug, name)
		VALUES ($1, $2, $3)
		RETURNING id, org_id, slug, name, created_at
	`, orgID, slug, name).Scan(&a.ID, &a.OrgID, &a.Slug, &a.Name, &a.CreatedAt)
	if err != nil {
		return nil, mapErr(err)
	}
	return &a, nil
}

func (s *Store) ListApplications(ctx context.Context, orgID uuid.UUID) ([]Application, error) {
	rows, err := s.pool.Query(ctx, `
		SELECT id, org_id, slug, name, created_at
		FROM applications WHERE org_id = $1 ORDER BY slug
	`, orgID)
	if err != nil {
		return nil, mapErr(err)
	}
	defer rows.Close()

	out := []Application{}
	for rows.Next() {
		var a Application
		if err := rows.Scan(&a.ID, &a.OrgID, &a.Slug, &a.Name, &a.CreatedAt); err != nil {
			return nil, err
		}
		out = append(out, a)
	}
	return out, rows.Err()
}

// ------------------------------------------------------------------ stacks

func (s *Store) CreateStack(ctx context.Context, appID uuid.UUID, slug string) (*Stack, error) {
	if err := validateSlug("slug", slug); err != nil {
		return nil, err
	}
	var st Stack
	err := s.pool.QueryRow(ctx, `
		INSERT INTO stacks (app_id, slug)
		VALUES ($1, $2)
		RETURNING id, app_id, slug, created_at
	`, appID, slug).Scan(&st.ID, &st.AppID, &st.Slug, &st.CreatedAt)
	if err != nil {
		return nil, mapErr(err)
	}
	return &st, nil
}

func (s *Store) ListStacks(ctx context.Context, appID uuid.UUID) ([]Stack, error) {
	rows, err := s.pool.Query(ctx, `
		SELECT id, app_id, slug, created_at
		FROM stacks WHERE app_id=$1 ORDER BY slug
	`, appID)
	if err != nil {
		return nil, mapErr(err)
	}
	defer rows.Close()
	out := []Stack{}
	for rows.Next() {
		var st Stack
		if err := rows.Scan(&st.ID, &st.AppID, &st.Slug, &st.CreatedAt); err != nil {
			return nil, err
		}
		out = append(out, st)
	}
	return out, rows.Err()
}

func (s *Store) GetApplication(ctx context.Context, id uuid.UUID) (*Application, error) {
	var a Application
	err := s.pool.QueryRow(ctx, `
		SELECT id, org_id, slug, name, created_at FROM applications WHERE id=$1
	`, id).Scan(&a.ID, &a.OrgID, &a.Slug, &a.Name, &a.CreatedAt)
	if err != nil {
		return nil, mapErr(err)
	}
	return &a, nil
}

func (s *Store) GetNode(ctx context.Context, id uuid.UUID) (*Node, error) {
	nodes, err := s.queryNodes(ctx, `
		SELECT id, org_id, hostname, host(advertise_addr), state,
		       cpu_millis, memory_bytes, alloc_cpu_millis, alloc_memory_bytes,
		       labels, COALESCE(agent_version,''), COALESCE(age_recipient,''), last_heartbeat, created_at
		FROM nodes WHERE id=$1
	`, id)
	if err != nil {
		return nil, err
	}
	if len(nodes) == 0 {
		return nil, ErrNotFound
	}
	return &nodes[0], nil
}

// ------------------------------------------------------------- environments

// CreateEnvironmentParams is the input to a new environment. Strategy and
// Config may be zero; the schema default and an empty overlay apply.
type CreateEnvironmentParams struct {
	StackID  uuid.UUID
	Slug     string
	Strategy RolloutStrategy
	Hostname string
	Config   map[string]string
}

func (s *Store) CreateEnvironment(ctx context.Context, p CreateEnvironmentParams) (*Environment, error) {
	if err := validateSlug("slug", p.Slug); err != nil {
		return nil, err
	}
	if err := validateHostname(p.Hostname); err != nil {
		return nil, err
	}
	if p.Strategy != "" && p.Strategy != StrategyBlueGreen {
		return nil, fmt.Errorf("%w: rollout strategy %q is not supported; use %q", ErrInvalid, p.Strategy, StrategyBlueGreen)
	}
	configJSON, err := json.Marshal(orEmpty(p.Config))
	if err != nil {
		return nil, err
	}

	// Strategy is passed as NULL when unset so the column default stays the
	// single source of truth for what a new environment gets.
	var strategy *string
	if p.Strategy != "" {
		v := string(p.Strategy)
		strategy = &v
	}

	var e Environment
	var config []byte
	err = s.pool.QueryRow(ctx, `
		INSERT INTO environments (stack_id, slug, strategy, hostname, config)
		VALUES (
			$1, $2,
			COALESCE($3::rollout_strategy, 'blue_green'::rollout_strategy),
			NULLIF($4, ''),
			$5
		)
		RETURNING id, stack_id, slug, strategy, COALESCE(hostname,''),
		          config, live_deployment_id, ephemeral, expires_at, home_node_id, created_at
	`, p.StackID, p.Slug, strategy, p.Hostname, configJSON).
		Scan(&e.ID, &e.StackID, &e.Slug, &e.Strategy, &e.Hostname,
			&config, &e.LiveDeploymentID, &e.Ephemeral, &e.ExpiresAt, &e.HomeNodeID, &e.CreatedAt)
	if err != nil {
		return nil, mapErr(err)
	}
	if err := json.Unmarshal(config, &e.Config); err != nil {
		return nil, err
	}
	return &e, nil
}

func (s *Store) ListEnvironments(ctx context.Context, stackID uuid.UUID) ([]Environment, error) {
	rows, err := s.pool.Query(ctx, `
		SELECT id, stack_id, slug, strategy, COALESCE(hostname,''),
		       config, live_deployment_id, ephemeral, expires_at, home_node_id, created_at
		FROM environments WHERE stack_id = $1 ORDER BY slug
	`, stackID)
	if err != nil {
		return nil, mapErr(err)
	}
	defer rows.Close()

	out := []Environment{}
	for rows.Next() {
		var e Environment
		var config []byte
		if err := rows.Scan(&e.ID, &e.StackID, &e.Slug, &e.Strategy, &e.Hostname,
			&config, &e.LiveDeploymentID, &e.Ephemeral, &e.ExpiresAt, &e.HomeNodeID, &e.CreatedAt); err != nil {
			return nil, err
		}
		if err := json.Unmarshal(config, &e.Config); err != nil {
			return nil, err
		}
		out = append(out, e)
	}
	return out, rows.Err()
}

// OrgForEnvironment resolves an environment up through stacks and
// applications to the owning org. Secret handlers need this to scope which
// nodes' recipients a value should be encrypted to — an environment only
// ever names its stack, never its org directly.
func (s *Store) OrgForEnvironment(ctx context.Context, envID uuid.UUID) (uuid.UUID, error) {
	var orgID uuid.UUID
	err := s.pool.QueryRow(ctx, `
		SELECT a.org_id FROM environments e
		JOIN stacks s ON s.id = e.stack_id
		JOIN applications a ON a.id = s.app_id
		WHERE e.id = $1
	`, envID).Scan(&orgID)
	return orgID, mapErr(err)
}

// orEmpty keeps a nil map from marshalling to JSON null, which the config
// column (NOT NULL) would reject.
func orEmpty(m map[string]string) map[string]string {
	if m == nil {
		return map[string]string{}
	}
	return m
}
