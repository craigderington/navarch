package store

import (
	"context"
	"errors"
	"fmt"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
)

// Node enrolment credentials, scoped to one organization.
//
// The org is a property of the token, never of the request that presents it.
// That is the whole point: registration used to take the org from the request
// body and check it only for operators, so the shared service token could enrol
// a node anywhere. A join token cannot, because it has only one org to give.

type JoinToken struct {
	ID        uuid.UUID  `json:"id"`
	OrgID     uuid.UUID  `json:"org_id"`
	Name      string     `json:"name"`
	ExpiresAt *time.Time `json:"expires_at,omitempty"`
	MaxUses   *int       `json:"max_uses,omitempty"`
	Uses      int        `json:"uses"`
	CreatedAt time.Time  `json:"created_at"`
	// Plaintext is set only by CreateJoinToken, on the one response that
	// carries it.
	Plaintext string `json:"token,omitempty"`
}

type CreateJoinTokenParams struct {
	OrgID     uuid.UUID
	Name      string
	ExpiresAt *time.Time
	MaxUses   *int
	CreatedBy *uuid.UUID
}

func (s *Store) CreateJoinToken(ctx context.Context, p CreateJoinTokenParams) (*JoinToken, error) {
	if p.Name == "" {
		p.Name = "default"
	}
	if p.MaxUses != nil && *p.MaxUses < 1 {
		return nil, fmt.Errorf("%w: max_uses must be at least 1", ErrInvalid)
	}
	plain, hash, err := newBearerToken()
	if err != nil {
		return nil, err
	}
	t := JoinToken{Plaintext: plain}
	err = s.pool.QueryRow(ctx, `
		INSERT INTO node_join_tokens (org_id, token_hash, name, expires_at, max_uses, created_by)
		VALUES ($1,$2,$3,$4,$5,$6)
		RETURNING id, org_id, name, expires_at, max_uses, uses, created_at
	`, p.OrgID, hash, p.Name, p.ExpiresAt, p.MaxUses, p.CreatedBy).
		Scan(&t.ID, &t.OrgID, &t.Name, &t.ExpiresAt, &t.MaxUses, &t.Uses, &t.CreatedAt)
	if err != nil {
		return nil, mapErr(err)
	}
	return &t, nil
}

func (s *Store) ListJoinTokens(ctx context.Context, orgID uuid.UUID) ([]JoinToken, error) {
	rows, err := s.pool.Query(ctx, `
		SELECT id, org_id, name, expires_at, max_uses, uses, created_at
		FROM node_join_tokens WHERE org_id=$1 ORDER BY created_at DESC
	`, orgID)
	if err != nil {
		return nil, mapErr(err)
	}
	defer rows.Close()
	out := []JoinToken{}
	for rows.Next() {
		var t JoinToken
		if err := rows.Scan(&t.ID, &t.OrgID, &t.Name, &t.ExpiresAt, &t.MaxUses, &t.Uses, &t.CreatedAt); err != nil {
			return nil, err
		}
		out = append(out, t)
	}
	return out, rows.Err()
}

func (s *Store) RevokeJoinToken(ctx context.Context, orgID, tokenID uuid.UUID) error {
	// Scoped to the org so naming another org's token id changes nothing —
	// the same shape every node-facing and operator-facing delete uses here.
	tag, err := s.pool.Exec(ctx,
		`DELETE FROM node_join_tokens WHERE id=$1 AND org_id=$2`, tokenID, orgID)
	if err != nil {
		return mapErr(err)
	}
	if tag.RowsAffected() == 0 {
		return ErrNotFound
	}
	return nil
}

// RedeemJoinToken resolves a plaintext join token to the organization it admits
// a node to, and counts the use.
//
// Counting inside the same statement that selects it is what makes max_uses
// mean anything: two agents starting at once would otherwise both read uses=0
// and both be admitted by a single-use token. The UPDATE ... RETURNING is one
// atomic step, so the second one finds no row whose remaining uses are
// positive.
//
// Expired, exhausted and unknown are one answer — ErrNotFound — because the
// caller is unauthenticated and telling it which of its guesses named a real
// token is a gift.
func (s *Store) RedeemJoinToken(ctx context.Context, plain string) (uuid.UUID, error) {
	if plain == "" {
		return uuid.Nil, ErrNotFound
	}
	var orgID uuid.UUID
	err := s.pool.QueryRow(ctx, `
		UPDATE node_join_tokens
		SET uses = uses + 1
		WHERE token_hash = $1
		  AND (expires_at IS NULL OR expires_at > now())
		  AND (max_uses IS NULL OR uses < max_uses)
		RETURNING org_id
	`, hashBearerToken(plain)).Scan(&orgID)
	if err != nil {
		if errors.Is(err, pgx.ErrNoRows) {
			return uuid.Nil, ErrNotFound
		}
		return uuid.Nil, mapErr(err)
	}
	return orgID, nil
}

// EnsureJoinToken records a join token whose plaintext the caller already
// holds, idempotently. One caller: bootstrap, so the dev stack can pin a
// constant instead of scraping a generated value out of a log on every
// `make up`. Deliberately not exposed through the API, for the same reason
// EnsureOperatorToken is not — a caller-chosen credential is only ever as
// strong as whoever chose it.
func (s *Store) EnsureJoinToken(ctx context.Context, orgID uuid.UUID, name, plain string) error {
	if plain == "" {
		return fmt.Errorf("%w: token is required", ErrInvalid)
	}
	_, err := s.pool.Exec(ctx, `
		INSERT INTO node_join_tokens (org_id, token_hash, name)
		VALUES ($1,$2,$3)
		ON CONFLICT (token_hash) DO NOTHING
	`, orgID, hashBearerToken(plain), name)
	return mapErr(err)
}
