package store

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
)

// Access requests: the doorbell for an invite-only platform.
//
// A request grants nothing and creates nobody. That is the whole reason it can
// exist on an unauthenticated route while self-serve signup cannot: signup
// needs email verification, because an unverified operator row lets somebody
// squat an address and be silently added to an organization by an invitation
// meant for its real owner. A request row has no such power — approving one
// goes through createInviteTx like every other invitation, and the invitation
// is only useful to whoever actually receives the mail.

// maxNoteLen bounds the free-text field. Long enough for a paragraph explaining
// what somebody wants to deploy, short enough that the table cannot be used as
// storage by anyone who finds the form.
const maxNoteLen = 2000

// maxNameLen bounds the display name for the same reason.
const maxNameLen = 200

const (
	AccessRequestPending  = "pending"
	AccessRequestApproved = "approved"
	AccessRequestDeclined = "declined"
)

type AccessRequest struct {
	ID    uuid.UUID `json:"id"`
	OrgID uuid.UUID `json:"org_id"`
	Email string    `json:"email"`
	Name  string    `json:"name,omitempty"`
	Note  string    `json:"note,omitempty"`
	State string    `json:"state"`
	// InviteID is what approving produced, so an operator can go from "who
	// asked" to "what was issued" without guessing by address and timestamp.
	InviteID  *uuid.UUID `json:"invite_id,omitempty"`
	DecidedBy *uuid.UUID `json:"decided_by,omitempty"`
	DecidedAt *time.Time `json:"decided_at,omitempty"`
	CreatedAt time.Time  `json:"created_at"`
}

type CreateAccessRequestParams struct {
	OrgID uuid.UUID
	Email string
	Name  string
	Note  string
}

// CreateAccessRequest files a request, or updates the one already pending for
// that address.
//
// The bool reports whether this filed a new request rather than updating one.
// It exists for exactly one caller: the notification. A public form is
// submitted twice by anybody who is not sure it worked the first time, and a
// mail per submission is how an operator learns to filter the sender — the same
// bargain ClaimPreviewsForExpiryWarning makes, decided in the statement rather
// than by reading and then writing.
//
// `xmax = 0` is the standard way to ask Postgres which branch of an upsert ran:
// a freshly inserted row has no deleting transaction, an updated one carries
// the transaction that superseded its previous version. It is checked in the
// same statement that does the work, so two simultaneous submissions cannot
// both come back new.
func (s *Store) CreateAccessRequest(ctx context.Context, p CreateAccessRequestParams) (*AccessRequest, bool, error) {
	p.Email = strings.TrimSpace(p.Email)
	if err := validateEmail(p.Email); err != nil {
		return nil, false, err
	}
	p.Name = strings.TrimSpace(p.Name)
	p.Note = strings.TrimSpace(p.Note)
	if len(p.Name) > maxNameLen {
		return nil, false, fmt.Errorf("%w: name must be at most %d characters", ErrInvalid, maxNameLen)
	}
	if len(p.Note) > maxNoteLen {
		return nil, false, fmt.Errorf("%w: note must be at most %d characters", ErrInvalid, maxNoteLen)
	}

	var ar AccessRequest
	var fresh bool
	err := s.tx(ctx, func(tx pgx.Tx) error {
		if err := tx.QueryRow(ctx, `
			INSERT INTO access_requests (org_id, email, name, note)
			VALUES ($1, $2, $3, $4)
			ON CONFLICT (org_id, lower(email)) WHERE state = 'pending'
			-- A resubmission updates what it actually said, and keeps what it
			-- did not. Both fields are optional, so somebody who asked with a
			-- name and a paragraph and then submitted again from a blank form
			-- would otherwise erase the only two things an operator has to
			-- decide on -- and the second submission is characteristically the
			-- one from a person who was not sure the first worked.
			DO UPDATE SET
			    name = COALESCE(NULLIF(EXCLUDED.name, ''), access_requests.name),
			    note = COALESCE(NULLIF(EXCLUDED.note, ''), access_requests.note)
			RETURNING id, org_id, email, name, note, state, invite_id,
			          decided_by, decided_at, created_at, (xmax = 0)
		`, p.OrgID, p.Email, p.Name, p.Note).
			Scan(&ar.ID, &ar.OrgID, &ar.Email, &ar.Name, &ar.Note, &ar.State,
				&ar.InviteID, &ar.DecidedBy, &ar.DecidedAt, &ar.CreatedAt, &fresh); err != nil {
			return err
		}
		if !fresh {
			return nil
		}
		// No actor: nobody asked on the platform's behalf, and the requester
		// has no identity here by construction. The address is the subject of
		// the event and belongs in it; the note does not, because it is
		// unbounded text somebody outside the system typed.
		return appendEventTx(ctx, tx, p.OrgID, nil, nil, "access.requested",
			fmt.Sprintf("%s asked for access", ar.Email),
			map[string]any{"email": ar.Email, "request": ar.ID.String()})
	})
	if err != nil {
		return nil, false, mapErr(err)
	}
	return &ar, fresh, nil
}

const accessRequestColumns = `id, org_id, email, name, note, state, invite_id,
                              decided_by, decided_at, created_at`

func scanAccessRequest(row pgx.Row) (*AccessRequest, error) {
	var ar AccessRequest
	if err := row.Scan(&ar.ID, &ar.OrgID, &ar.Email, &ar.Name, &ar.Note, &ar.State,
		&ar.InviteID, &ar.DecidedBy, &ar.DecidedAt, &ar.CreatedAt); err != nil {
		return nil, err
	}
	return &ar, nil
}

// ListAccessRequests returns an organization's requests, newest first.
//
// Pending ones come first regardless of age: the list exists to be acted on,
// and a decided request is history that should not push a waiting person off
// the top of the page.
func (s *Store) ListAccessRequests(ctx context.Context, orgID uuid.UUID) ([]AccessRequest, error) {
	rows, err := s.pool.Query(ctx, `
		SELECT `+accessRequestColumns+`
		FROM access_requests WHERE org_id = $1
		ORDER BY (state = 'pending') DESC, created_at DESC
	`, orgID)
	if err != nil {
		return nil, mapErr(err)
	}
	defer rows.Close()
	out := []AccessRequest{}
	for rows.Next() {
		ar, err := scanAccessRequest(rows)
		if err != nil {
			return nil, mapErr(err)
		}
		out = append(out, *ar)
	}
	return out, mapErr(rows.Err())
}

func (s *Store) GetAccessRequest(ctx context.Context, id uuid.UUID) (*AccessRequest, error) {
	ar, err := scanAccessRequest(s.pool.QueryRow(ctx,
		`SELECT `+accessRequestColumns+` FROM access_requests WHERE id = $1`, id))
	if err != nil {
		return nil, mapErr(err)
	}
	return ar, nil
}

// OrgForAccessRequest resolves the owning org for the authorization check.
// ErrNotFound for a missing row, so "no such request" and "not yours" arrive
// identically at the handler and leave as the same 404.
func (s *Store) OrgForAccessRequest(ctx context.Context, id uuid.UUID) (uuid.UUID, error) {
	return s.orgIDFrom(ctx, `SELECT org_id FROM access_requests WHERE id = $1`, id)
}

type ApproveAccessRequestParams struct {
	OrgID     uuid.UUID
	RequestID uuid.UUID
	Role      string
	TTL       time.Duration
	DecidedBy *uuid.UUID
}

// ApproveAccessRequest turns a request into an invitation, in one transaction.
//
// The claim is an UPDATE ... RETURNING guarded on the current state, the same
// shape RedeemInvite and RedeemJoinToken use: two operators clicking approve at
// the same moment must not mint two invitations for one person. A request that
// has already been decided fails the WHERE and comes back ErrNotFound rather
// than quietly issuing a second credential.
//
// The invitation is minted by createInviteTx — the single path — so approving
// supersedes any live invite for that address exactly as inviting by hand does,
// and appends the same operator.invited event.
func (s *Store) ApproveAccessRequest(ctx context.Context, p ApproveAccessRequestParams) (*AccessRequest, *OperatorInvite, error) {
	if p.Role == "" {
		p.Role = "member"
	}
	switch p.TTL {
	case 0:
		p.TTL = DefaultInviteTTL
	default:
		if p.TTL < 0 || p.TTL > MaxInviteTTL {
			return nil, nil, fmt.Errorf("%w: invite ttl must be between 0 and %d hours",
				ErrInvalid, int(MaxInviteTTL.Hours()))
		}
	}

	var ar *AccessRequest
	var inv *OperatorInvite
	err := s.tx(ctx, func(tx pgx.Tx) error {
		var err error
		ar, err = scanAccessRequest(tx.QueryRow(ctx, `
			UPDATE access_requests
			SET state = 'approved', decided_at = now(), decided_by = $3
			WHERE id = $1 AND org_id = $2 AND state = 'pending'
			RETURNING `+accessRequestColumns, p.RequestID, p.OrgID, p.DecidedBy))
		if errors.Is(err, pgx.ErrNoRows) {
			return ErrNotFound
		}
		if err != nil {
			return err
		}
		inv, err = createInviteTx(ctx, tx, CreateInviteParams{
			OrgID:     p.OrgID,
			Email:     ar.Email,
			Role:      p.Role,
			TTL:       p.TTL,
			InvitedBy: p.DecidedBy,
		})
		if err != nil {
			return err
		}
		if _, err := tx.Exec(ctx,
			`UPDATE access_requests SET invite_id = $2 WHERE id = $1`, ar.ID, inv.ID); err != nil {
			return err
		}
		ar.InviteID = &inv.ID
		return appendEventTx(ctx, tx, p.OrgID, nil, nil, "access.approved",
			fmt.Sprintf("approved access for %s", ar.Email),
			map[string]any{"email": ar.Email, "request": ar.ID.String(), "invite": inv.ID.String()})
	})
	if err != nil {
		return nil, nil, mapErr(err)
	}
	return ar, inv, nil
}

// DeclineAccessRequest closes a request without issuing anything.
//
// It records the decision rather than deleting the row, for the reason invites
// are marked redeemed instead of removed: the question "did we ever hear from
// this person" has to be answerable later, and a deleted row answers no.
//
// Declining does not block the address. The partial unique index is on pending
// rows alone, so somebody can ask again — deliberately, because the alternative
// is a permanent denylist nobody can see or lift.
func (s *Store) DeclineAccessRequest(ctx context.Context, orgID, requestID uuid.UUID, decidedBy *uuid.UUID) (*AccessRequest, error) {
	var ar *AccessRequest
	err := s.tx(ctx, func(tx pgx.Tx) error {
		var err error
		ar, err = scanAccessRequest(tx.QueryRow(ctx, `
			UPDATE access_requests
			SET state = 'declined', decided_at = now(), decided_by = $3
			WHERE id = $1 AND org_id = $2 AND state = 'pending'
			RETURNING `+accessRequestColumns, requestID, orgID, decidedBy))
		if errors.Is(err, pgx.ErrNoRows) {
			return ErrNotFound
		}
		if err != nil {
			return err
		}
		return appendEventTx(ctx, tx, orgID, nil, nil, "access.declined",
			fmt.Sprintf("declined access for %s", ar.Email),
			map[string]any{"email": ar.Email, "request": ar.ID.String()})
	})
	if err != nil {
		return nil, mapErr(err)
	}
	return ar, nil
}
