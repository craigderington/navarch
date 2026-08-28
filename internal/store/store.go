// Package store is the only package that talks to Postgres. API handlers
// receive a *Store and never construct SQL themselves.
package store

import (
	"context"
	"errors"
	"fmt"
	"regexp"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgconn"
	"github.com/jackc/pgx/v5/pgxpool"
)

var (
	ErrNotFound = errors.New("not found")
	ErrConflict = errors.New("conflict")
	// ErrInvalid marks input the store refuses on its own terms, before
	// Postgres sees it. Handlers map it to 400.
	ErrInvalid = errors.New("invalid")
)

// slugPattern is the accepted form for the human-facing identifiers that
// end up in URLs, project names and ingress hostnames. Postgres enforces
// uniqueness but not shape, and an empty slug would otherwise be accepted.
var slugPattern = regexp.MustCompile(`^[a-z0-9]([a-z0-9-]*[a-z0-9])?$`)

const maxSlugLen = 63

func validateSlug(field, slug string) error {
	if slug == "" {
		return fmt.Errorf("%w: %s is required", ErrInvalid, field)
	}
	if len(slug) > maxSlugLen {
		return fmt.Errorf("%w: %s must be at most %d characters", ErrInvalid, field, maxSlugLen)
	}
	if !slugPattern.MatchString(slug) {
		return fmt.Errorf("%w: %s must be lowercase alphanumeric with internal dashes, e.g. %q", ErrInvalid, field, "my-app")
	}
	return nil
}

// hostnamePattern is a lowercase DNS name. It is also the Traefik Host()
// interpolation alphabet: no backticks, spaces, or newlines, so a hostname
// cannot break out of the file-provider rule.
var hostnamePattern = regexp.MustCompile(`^[a-z0-9]([a-z0-9-]{0,61}[a-z0-9])?(\.[a-z0-9]([a-z0-9-]{0,61}[a-z0-9])?)*$`)

const maxHostnameLen = 253

func validateHostname(hostname string) error {
	if hostname == "" {
		return nil
	}
	if len(hostname) > maxHostnameLen {
		return fmt.Errorf("%w: hostname must be at most %d characters", ErrInvalid, maxHostnameLen)
	}
	if !hostnamePattern.MatchString(hostname) {
		return fmt.Errorf("%w: hostname must be a lowercase DNS name", ErrInvalid)
	}
	return nil
}

type Store struct {
	pool *pgxpool.Pool
}

func New(ctx context.Context, dsn string) (*Store, error) {
	cfg, err := pgxpool.ParseConfig(dsn)
	if err != nil {
		return nil, fmt.Errorf("parse dsn: %w", err)
	}
	cfg.MaxConns = 16
	pool, err := pgxpool.NewWithConfig(ctx, cfg)
	if err != nil {
		return nil, fmt.Errorf("connect: %w", err)
	}
	if err := pool.Ping(ctx); err != nil {
		return nil, fmt.Errorf("ping: %w", err)
	}
	return &Store{pool: pool}, nil
}

func (s *Store) Close() { s.pool.Close() }

func (s *Store) Pool() *pgxpool.Pool { return s.pool }

// tx runs fn inside a transaction, rolling back on error.
func (s *Store) tx(ctx context.Context, fn func(pgx.Tx) error) error {
	t, err := s.pool.Begin(ctx)
	if err != nil {
		return err
	}
	defer func() { _ = t.Rollback(ctx) }()
	if err := fn(t); err != nil {
		return err
	}
	return t.Commit(ctx)
}

// isUniqueViolation reports whether err is a Postgres 23505.
func isUniqueViolation(err error) bool {
	var pgErr *pgconn.PgError
	return errors.As(err, &pgErr) && pgErr.Code == "23505"
}

// isForeignKeyViolation reports whether err is a Postgres 23503.
func isForeignKeyViolation(err error) bool {
	var pgErr *pgconn.PgError
	return errors.As(err, &pgErr) && pgErr.Code == "23503"
}

// GetOrganizationBySlug lets the agent register by org slug rather than a
// UUID it has no way to know.
// GetOrganization resolves an org by id. Authorization needs it so that a
// caller naming a nonexistent org and a caller naming one they are not in get
// the same 404 — without it, "org exists" leaks through the difference between
// a membership refusal and a missing row.
func (s *Store) GetOrganization(ctx context.Context, id uuid.UUID) (*Organization, error) {
	var o Organization
	err := s.pool.QueryRow(ctx, `
		SELECT id, slug, name, created_at FROM organizations WHERE id = $1
	`, id).Scan(&o.ID, &o.Slug, &o.Name, &o.CreatedAt)
	if err != nil {
		return nil, mapErr(err)
	}
	return &o, nil
}

func (s *Store) GetOrganizationBySlug(ctx context.Context, slug string) (*Organization, error) {
	var o Organization
	err := s.pool.QueryRow(ctx, `
		SELECT id, slug, name, created_at FROM organizations WHERE slug=$1
	`, slug).Scan(&o.ID, &o.Slug, &o.Name, &o.CreatedAt)
	if err != nil {
		return nil, mapErr(err)
	}
	return &o, nil
}

func mapErr(err error) error {
	switch {
	case err == nil:
		return nil
	case errors.Is(err, pgx.ErrNoRows):
		return ErrNotFound
	case isUniqueViolation(err):
		return ErrConflict
	case isForeignKeyViolation(err):
		// Every foreign key in the catalog points at a parent that must
		// already exist, so a violation means the caller named a parent
		// that doesn't — a 404, not a server fault. Revisit if a delete
		// path ever relies on RESTRICT to refuse removing a referenced row;
		// that would want 409 instead.
		return ErrNotFound
	default:
		return err
	}
}
