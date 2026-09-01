package store

import (
	"context"
	"fmt"
	"strings"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
)

// DefaultInviteTTL is how long an invitation stays redeemable.
//
// Seven days: long enough to survive a weekend and a holiday Monday, short
// enough that a link forwarded into a group chat two months ago is dead. It is
// a credential, and a credential with no expiry is one nobody ever revokes.
const DefaultInviteTTL = 7 * 24 * time.Hour

// MaxInviteTTL caps what a caller may ask for, and is a hard error rather than
// a silent clamp — the same bargain preview TTLs make. Storing a different
// expiry than the one requested makes the API lie about a credential's
// lifetime, which is the worst thing to be vague about.
const MaxInviteTTL = 30 * 24 * time.Hour

type OperatorInvite struct {
	ID    uuid.UUID `json:"id"`
	OrgID uuid.UUID `json:"org_id"`
	Email string    `json:"email"`
	Role  string    `json:"role"`
	// Plaintext is set only by CreateInvite, and only on the response that
	// created it. Nothing can produce it again.
	Plaintext  string     `json:"-"`
	InvitedBy  *uuid.UUID `json:"invited_by,omitempty"`
	ExpiresAt  time.Time  `json:"expires_at"`
	RedeemedAt *time.Time `json:"redeemed_at,omitempty"`
	RevokedAt  *time.Time `json:"revoked_at,omitempty"`
	CreatedAt  time.Time  `json:"created_at"`
}

// State renders the one word a human wants. Derived rather than stored: an
// invite becomes expired by the clock moving, and a column recording that would
// be wrong until something noticed.
func (i *OperatorInvite) State() string {
	switch {
	case i.RedeemedAt != nil:
		return "redeemed"
	case i.RevokedAt != nil:
		return "revoked"
	case time.Now().After(i.ExpiresAt):
		return "expired"
	default:
		return "pending"
	}
}

type CreateInviteParams struct {
	OrgID     uuid.UUID
	Email     string
	Role      string
	TTL       time.Duration
	InvitedBy *uuid.UUID
}

// CreateInvite mints an invitation and returns the plaintext token once.
//
// Re-inviting an address supersedes the previous invitation rather than
// failing. An admin who clicks twice means "send it again", and the alternative
// — two live credentials for one person, only one of which anybody is tracking
// — is worse than either erroring or superseding. It also keeps the one-live-
// invite index satisfiable, which a predicate cannot do on its own because
// `now()` is not immutable and so cannot appear in a partial index.
func (s *Store) CreateInvite(ctx context.Context, p CreateInviteParams) (*OperatorInvite, error) {
	p.Email = strings.TrimSpace(p.Email)
	if err := validateEmail(p.Email); err != nil {
		return nil, err
	}
	if p.Role == "" {
		p.Role = "member"
	}
	switch p.TTL {
	case 0:
		p.TTL = DefaultInviteTTL
	default:
		if p.TTL < 0 || p.TTL > MaxInviteTTL {
			return nil, fmt.Errorf("%w: invite ttl must be between 0 and %d hours",
				ErrInvalid, int(MaxInviteTTL.Hours()))
		}
	}

	plain, hash, err := newBearerToken()
	if err != nil {
		return nil, err
	}
	inv := OperatorInvite{Plaintext: plain}
	err = s.tx(ctx, func(tx pgx.Tx) error {
		if _, err := tx.Exec(ctx, `
			UPDATE operator_invites SET revoked_at = now()
			WHERE org_id = $1 AND lower(email) = lower($2)
			  AND redeemed_at IS NULL AND revoked_at IS NULL
		`, p.OrgID, p.Email); err != nil {
			return err
		}
		if err := tx.QueryRow(ctx, `
			INSERT INTO operator_invites (org_id, email, role, token_hash, invited_by, expires_at)
			VALUES ($1, $2, $3, $4, $5, now() + make_interval(secs => $6))
			RETURNING id, org_id, email, role, invited_by, expires_at, redeemed_at, revoked_at, created_at
		`, p.OrgID, p.Email, p.Role, hash, p.InvitedBy, p.TTL.Seconds()).
			Scan(&inv.ID, &inv.OrgID, &inv.Email, &inv.Role, &inv.InvitedBy,
				&inv.ExpiresAt, &inv.RedeemedAt, &inv.RevokedAt, &inv.CreatedAt); err != nil {
			return err
		}
		// The address is the subject of the event, so it belongs in it. Note
		// what does not: the token. An audit trail that recorded the credential
		// would be a second place to steal it from.
		return appendEventTx(ctx, tx, p.OrgID, nil, nil, "operator.invited",
			fmt.Sprintf("invited %s as %s", p.Email, p.Role),
			map[string]any{"email": p.Email, "role": p.Role, "invite": inv.ID.String()})
	})
	if err != nil {
		return nil, mapErr(err)
	}
	return &inv, nil
}

// RedeemInvite exchanges an invitation for an operator token.
//
// The claim is a single UPDATE ... RETURNING, the same shape RedeemJoinToken
// uses and for the same reason: a check in one statement and a mark in another
// lets two requests both pass the check, and this one hands out access to an
// organization. Everything after it — finding or creating the operator, adding
// the membership, minting the token — happens in the same transaction, so an
// invite is never spent without producing the access it promised.
//
// An address that already has an operator is reused rather than duplicated:
// one human, one row, which is what the lower(email) unique index on operators
// already insists on. That also means an existing operator can be invited into
// a second organization, which is the ordinary way somebody comes to belong to
// two.
func (s *Store) RedeemInvite(ctx context.Context, plain, tokenName string) (*Operator, *OperatorToken, error) {
	if plain == "" {
		return nil, nil, ErrNotFound
	}
	if tokenName == "" {
		tokenName = "invite"
	}
	var op Operator
	var tok OperatorToken
	err := s.tx(ctx, func(tx pgx.Tx) error {
		var orgID uuid.UUID
		var email, role string
		// Unknown, expired, revoked and already-redeemed all return no row, and
		// the caller cannot tell them apart. It is an unauthenticated request:
		// saying which of those it was tells someone guessing that they guessed
		// a real invite.
		err := tx.QueryRow(ctx, `
			UPDATE operator_invites
			SET redeemed_at = now()
			WHERE token_hash = $1
			  AND redeemed_at IS NULL
			  AND revoked_at IS NULL
			  AND expires_at > now()
			RETURNING org_id, email, role
		`, hashBearerToken(plain)).Scan(&orgID, &email, &role)
		if err != nil {
			return err
		}

		if err := tx.QueryRow(ctx, `
			INSERT INTO operators (email, name) VALUES ($1, $1)
			ON CONFLICT (lower(email)) DO UPDATE SET email = operators.email
			RETURNING id, email, name, disabled_at, created_at
		`, email).Scan(&op.ID, &op.Email, &op.Name, &op.DisabledAt, &op.CreatedAt); err != nil {
			return err
		}
		if op.DisabledAt != nil {
			// A disabled operator redeeming an invite would re-enable them by
			// the back door. Refuse: re-enabling is its own decision.
			return fmt.Errorf("%w: that account is disabled", ErrConflict)
		}
		if _, err := tx.Exec(ctx, `
			UPDATE operator_invites SET redeemed_by = $1 WHERE token_hash = $2
		`, op.ID, hashBearerToken(plain)); err != nil {
			return err
		}
		if _, err := tx.Exec(ctx, `
			INSERT INTO organization_members (org_id, operator_id, role)
			VALUES ($1, $2, $3)
			ON CONFLICT (org_id, operator_id) DO NOTHING
		`, orgID, op.ID, role); err != nil {
			return err
		}

		tokPlain, tokHash, err := newBearerToken()
		if err != nil {
			return err
		}
		tok.Plaintext = tokPlain
		if err := tx.QueryRow(ctx, `
			INSERT INTO operator_tokens (operator_id, token_hash, name)
			VALUES ($1, $2, $3)
			RETURNING id, operator_id, name, expires_at, last_used_at, created_at
		`, op.ID, tokHash, tokenName).
			Scan(&tok.ID, &tok.OperatorID, &tok.Name, &tok.ExpiresAt, &tok.LastUsedAt, &tok.CreatedAt); err != nil {
			return err
		}
		return appendEventTx(ctx, tx, orgID, nil, nil, "operator.joined",
			fmt.Sprintf("%s joined as %s", op.Email, role),
			map[string]any{"email": op.Email, "role": role})
	})
	if err != nil {
		return nil, nil, mapErr(err)
	}
	return &op, &tok, nil
}

func (s *Store) ListInvites(ctx context.Context, orgID uuid.UUID) ([]OperatorInvite, error) {
	rows, err := s.pool.Query(ctx, `
		SELECT id, org_id, email, role, invited_by, expires_at, redeemed_at, revoked_at, created_at
		FROM operator_invites WHERE org_id = $1 ORDER BY created_at DESC
	`, orgID)
	if err != nil {
		return nil, mapErr(err)
	}
	defer rows.Close()
	out := []OperatorInvite{}
	for rows.Next() {
		var i OperatorInvite
		if err := rows.Scan(&i.ID, &i.OrgID, &i.Email, &i.Role, &i.InvitedBy,
			&i.ExpiresAt, &i.RedeemedAt, &i.RevokedAt, &i.CreatedAt); err != nil {
			return nil, err
		}
		out = append(out, i)
	}
	return out, rows.Err()
}

// RevokeInvite kills an unredeemed invitation. Scoped to the org in the WHERE
// clause rather than checked first, so naming another organization's invite id
// changes nothing — the same shape RevokeOperatorToken uses.
func (s *Store) RevokeInvite(ctx context.Context, orgID, inviteID uuid.UUID) error {
	tag, err := s.pool.Exec(ctx, `
		UPDATE operator_invites SET revoked_at = now()
		WHERE id = $1 AND org_id = $2 AND redeemed_at IS NULL AND revoked_at IS NULL
	`, inviteID, orgID)
	if err != nil {
		return mapErr(err)
	}
	if tag.RowsAffected() == 0 {
		return ErrNotFound
	}
	return nil
}
