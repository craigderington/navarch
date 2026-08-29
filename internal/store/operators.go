package store

import (
	"context"
	"fmt"
	"strings"
	"time"

	"github.com/google/uuid"
)

// Operator identity, org membership, and the resolvers that answer "which org
// owns this object" for id-addressed routes.
//
// The shape mirrors the node side, which has been per-credential since 0008:
// 32 bytes of crypto/rand, SHA-256 hex at rest, plaintext issued exactly once.
// What differs is the lookup. NodeTokenValid knows the node id from the path
// and fetches one hash to compare in constant time; an operator token names
// nobody, so it must be found *by* its hash. That is the standard shape for a
// bearer token with no accompanying identifier, and it is safe here for the
// same reason the node scheme is: the token is 256 bits of uniform randomness,
// so an index probe leaks nothing an attacker could not learn by guessing.

type Operator struct {
	ID    uuid.UUID `json:"id"`
	Email string    `json:"email"`
	Name  string    `json:"name"`
	// DisabledAt is set instead of deleting the row: events point at operators
	// and an event whose actor vanished cannot be read after an incident.
	DisabledAt *time.Time `json:"disabled_at,omitempty"`
	CreatedAt  time.Time  `json:"created_at"`
}

type OperatorToken struct {
	ID         uuid.UUID  `json:"id"`
	OperatorID uuid.UUID  `json:"operator_id"`
	Name       string     `json:"name"`
	ExpiresAt  *time.Time `json:"expires_at,omitempty"`
	LastUsedAt *time.Time `json:"last_used_at,omitempty"`
	CreatedAt  time.Time  `json:"created_at"`
	// Plaintext is populated only by IssueOperatorToken, on the one response
	// that carries it. Nothing stores it and no read ever sets it.
	Plaintext string `json:"token,omitempty"`
}

type OrgMember struct {
	OrgID      uuid.UUID `json:"org_id"`
	OperatorID uuid.UUID `json:"operator_id"`
	Email      string    `json:"email"`
	Name       string    `json:"name"`
	Role       string    `json:"role"`
	CreatedAt  time.Time `json:"created_at"`
}

const maxEmailLen = 254

// validateEmail is deliberately not an RFC 5322 parser. The email is an
// identifier and a display string, never something this system delivers mail
// to, so the only failures worth catching are the ones that would make it a
// bad identifier: empty, absurdly long, or carrying whitespace that makes two
// visually identical addresses distinct rows.
func validateEmail(email string) error {
	switch {
	case email == "":
		return fmt.Errorf("%w: email is required", ErrInvalid)
	case len(email) > maxEmailLen:
		return fmt.Errorf("%w: email must be at most %d characters", ErrInvalid, maxEmailLen)
	case strings.ContainsAny(email, " \t\r\n"):
		return fmt.Errorf("%w: email must not contain whitespace", ErrInvalid)
	case strings.Count(email, "@") != 1 ||
		strings.HasPrefix(email, "@") || strings.HasSuffix(email, "@"):
		return fmt.Errorf("%w: email must be of the form name@example.com", ErrInvalid)
	}
	return nil
}

func (s *Store) CreateOperator(ctx context.Context, email, name string) (*Operator, error) {
	email = strings.TrimSpace(email)
	if err := validateEmail(email); err != nil {
		return nil, err
	}
	if name == "" {
		name = email
	}
	var o Operator
	err := s.pool.QueryRow(ctx, `
		INSERT INTO operators (email, name)
		VALUES ($1, $2)
		RETURNING id, email, name, disabled_at, created_at
	`, email, name).Scan(&o.ID, &o.Email, &o.Name, &o.DisabledAt, &o.CreatedAt)
	if err != nil {
		return nil, mapErr(err)
	}
	return &o, nil
}

// GetOperatorByEmail matches case-insensitively, against the same expression
// the unique index is built on — so a lookup can never miss a row the index
// would have refused as a duplicate.
func (s *Store) GetOperatorByEmail(ctx context.Context, email string) (*Operator, error) {
	var o Operator
	err := s.pool.QueryRow(ctx, `
		SELECT id, email, name, disabled_at, created_at
		FROM operators WHERE lower(email) = lower($1)
	`, strings.TrimSpace(email)).Scan(&o.ID, &o.Email, &o.Name, &o.DisabledAt, &o.CreatedAt)
	if err != nil {
		return nil, mapErr(err)
	}
	return &o, nil
}

func (s *Store) GetOperator(ctx context.Context, id uuid.UUID) (*Operator, error) {
	var o Operator
	err := s.pool.QueryRow(ctx, `
		SELECT id, email, name, disabled_at, created_at
		FROM operators WHERE id = $1
	`, id).Scan(&o.ID, &o.Email, &o.Name, &o.DisabledAt, &o.CreatedAt)
	if err != nil {
		return nil, mapErr(err)
	}
	return &o, nil
}

// DisableOperator revokes access without deleting anything. Tokens are left in
// place on purpose: OperatorForToken refuses a disabled operator, so deleting
// them would buy nothing and would destroy the record of which credential was
// live at the time of whatever prompted the disable.
func (s *Store) DisableOperator(ctx context.Context, id uuid.UUID) error {
	tag, err := s.pool.Exec(ctx, `
		UPDATE operators SET disabled_at = COALESCE(disabled_at, now()) WHERE id = $1
	`, id)
	if err != nil {
		return mapErr(err)
	}
	if tag.RowsAffected() == 0 {
		return ErrNotFound
	}
	return nil
}

// IssueOperatorToken mints a token and returns the plaintext exactly once, in
// the returned struct. Nothing else can ever produce it again.
func (s *Store) IssueOperatorToken(ctx context.Context, operatorID uuid.UUID, name string, expiresAt *time.Time) (*OperatorToken, error) {
	if name == "" {
		name = "default"
	}
	plain, hash, err := newBearerToken()
	if err != nil {
		return nil, err
	}
	t := OperatorToken{Plaintext: plain}
	err = s.pool.QueryRow(ctx, `
		INSERT INTO operator_tokens (operator_id, token_hash, name, expires_at)
		VALUES ($1, $2, $3, $4)
		RETURNING id, operator_id, name, expires_at, last_used_at, created_at
	`, operatorID, hash, name, expiresAt).
		Scan(&t.ID, &t.OperatorID, &t.Name, &t.ExpiresAt, &t.LastUsedAt, &t.CreatedAt)
	if err != nil {
		return nil, mapErr(err)
	}
	return &t, nil
}

// lastUsedThrottle bounds how often authentication writes. Stamping
// last_used_at on every request would make operator_tokens the hottest write
// in the system — one UPDATE per API call, plus its WAL — to record a field
// nobody reads at second resolution. Five minutes is enough to answer "is this
// token still in use" and cheap enough to ignore.
const lastUsedThrottle = 5 * time.Minute

// OperatorForToken authenticates a bearer token. It returns ErrNotFound for an
// unknown, expired, or disabled credential without distinguishing them: the
// caller is an unauthenticated request, and telling it *why* it failed tells an
// attacker which of their guesses named a real token.
func (s *Store) OperatorForToken(ctx context.Context, plain string) (*Operator, error) {
	if plain == "" {
		return nil, ErrNotFound
	}
	hash := hashBearerToken(plain)
	var o Operator
	// One round trip: find the live token, stamp it if the stamp is stale, and
	// return its operator. The UPDATE is in a CTE rather than a second call so
	// authentication stays a single query on the hot path.
	err := s.pool.QueryRow(ctx, `
		WITH t AS (
			SELECT id, operator_id FROM operator_tokens
			WHERE token_hash = $1
			  AND (expires_at IS NULL OR expires_at > now())
		), touched AS (
			UPDATE operator_tokens SET last_used_at = now()
			WHERE id IN (SELECT id FROM t)
			  AND (last_used_at IS NULL
			       OR last_used_at < now() - make_interval(secs => $2))
			RETURNING id
		)
		SELECT o.id, o.email, o.name, o.disabled_at, o.created_at
		FROM operators o
		JOIN t ON t.operator_id = o.id
		WHERE o.disabled_at IS NULL
	`, hash, lastUsedThrottle.Seconds()).
		Scan(&o.ID, &o.Email, &o.Name, &o.DisabledAt, &o.CreatedAt)
	if err != nil {
		return nil, mapErr(err)
	}
	return &o, nil
}

func (s *Store) ListOperatorTokens(ctx context.Context, operatorID uuid.UUID) ([]OperatorToken, error) {
	rows, err := s.pool.Query(ctx, `
		SELECT id, operator_id, name, expires_at, last_used_at, created_at
		FROM operator_tokens WHERE operator_id = $1 ORDER BY created_at DESC
	`, operatorID)
	if err != nil {
		return nil, mapErr(err)
	}
	defer rows.Close()
	out := []OperatorToken{}
	for rows.Next() {
		var t OperatorToken
		if err := rows.Scan(&t.ID, &t.OperatorID, &t.Name, &t.ExpiresAt, &t.LastUsedAt, &t.CreatedAt); err != nil {
			return nil, err
		}
		out = append(out, t)
	}
	return out, rows.Err()
}

func (s *Store) RevokeOperatorToken(ctx context.Context, operatorID, tokenID uuid.UUID) error {
	// Scoped to the owner so one operator cannot revoke another's credential
	// by id — the same reason every node-facing query carries its node id.
	tag, err := s.pool.Exec(ctx, `
		DELETE FROM operator_tokens WHERE id = $1 AND operator_id = $2
	`, tokenID, operatorID)
	if err != nil {
		return mapErr(err)
	}
	if tag.RowsAffected() == 0 {
		return ErrNotFound
	}
	return nil
}

// ------------------------------------------------------------ membership

// AddOrgMember is an upsert so re-adding an existing member updates their role
// rather than failing. Membership is a set, and a caller asking for a state
// that already holds has not made a mistake worth a 409.
func (s *Store) AddOrgMember(ctx context.Context, orgID, operatorID uuid.UUID, role string) error {
	if role == "" {
		role = "owner"
	}
	_, err := s.pool.Exec(ctx, `
		INSERT INTO organization_members (org_id, operator_id, role)
		VALUES ($1, $2, $3)
		ON CONFLICT (org_id, operator_id) DO UPDATE SET role = EXCLUDED.role
	`, orgID, operatorID, role)
	return mapErr(err)
}

func (s *Store) RemoveOrgMember(ctx context.Context, orgID, operatorID uuid.UUID) error {
	tag, err := s.pool.Exec(ctx, `
		DELETE FROM organization_members WHERE org_id = $1 AND operator_id = $2
	`, orgID, operatorID)
	if err != nil {
		return mapErr(err)
	}
	if tag.RowsAffected() == 0 {
		return ErrNotFound
	}
	return nil
}

func (s *Store) ListOrgMembers(ctx context.Context, orgID uuid.UUID) ([]OrgMember, error) {
	rows, err := s.pool.Query(ctx, `
		SELECT m.org_id, m.operator_id, o.email, o.name, m.role, m.created_at
		FROM organization_members m
		JOIN operators o ON o.id = m.operator_id
		WHERE m.org_id = $1
		ORDER BY o.email
	`, orgID)
	if err != nil {
		return nil, mapErr(err)
	}
	defer rows.Close()
	out := []OrgMember{}
	for rows.Next() {
		var m OrgMember
		if err := rows.Scan(&m.OrgID, &m.OperatorID, &m.Email, &m.Name, &m.Role, &m.CreatedAt); err != nil {
			return nil, err
		}
		out = append(out, m)
	}
	return out, rows.Err()
}

// OperatorInOrg is the authorization primitive every scoped handler ends at.
// It answers a boolean and nothing else: a handler that learns *why* it was
// refused is one refactor away from telling the caller.
func (s *Store) OperatorInOrg(ctx context.Context, orgID, operatorID uuid.UUID) (bool, error) {
	var one int
	err := s.pool.QueryRow(ctx, `
		SELECT 1 FROM organization_members WHERE org_id = $1 AND operator_id = $2
	`, orgID, operatorID).Scan(&one)
	if err != nil {
		if mapErr(err) == ErrNotFound {
			return false, nil
		}
		return false, mapErr(err)
	}
	return true, nil
}

func (s *Store) OrgsForOperator(ctx context.Context, operatorID uuid.UUID) ([]Organization, error) {
	rows, err := s.pool.Query(ctx, `
		SELECT o.id, o.slug, o.name, COALESCE(o.preview_domain,''), o.created_at
		FROM organizations o
		JOIN organization_members m ON m.org_id = o.id
		WHERE m.operator_id = $1
		ORDER BY o.slug
	`, operatorID)
	if err != nil {
		return nil, mapErr(err)
	}
	defer rows.Close()
	out := []Organization{}
	for rows.Next() {
		var o Organization
		if err := rows.Scan(&o.ID, &o.Slug, &o.Name, &o.PreviewDomain, &o.CreatedAt); err != nil {
			return nil, err
		}
		out = append(out, o)
	}
	return out, rows.Err()
}

// ------------------------------------------------------------- resolvers

// Every id-addressed route needs to know which org owns the object it was
// handed before it can check membership. These are the only place that
// mapping lives.
//
// They return ErrNotFound for a missing object, and the handler turns a
// membership failure into the *same* 404 — so a caller cannot distinguish "no
// such environment" from "not yours". That distinction is a cross-tenant
// existence oracle: with 403 for the second case, anyone could probe another
// tenant's environment ids by watching the status code change.

func (s *Store) orgIDFrom(ctx context.Context, query string, id uuid.UUID) (uuid.UUID, error) {
	var orgID uuid.UUID
	if err := s.pool.QueryRow(ctx, query, id).Scan(&orgID); err != nil {
		return uuid.Nil, mapErr(err)
	}
	return orgID, nil
}

func (s *Store) OrgForApp(ctx context.Context, appID uuid.UUID) (uuid.UUID, error) {
	return s.orgIDFrom(ctx, `SELECT org_id FROM applications WHERE id = $1`, appID)
}

func (s *Store) OrgForStack(ctx context.Context, stackID uuid.UUID) (uuid.UUID, error) {
	return s.orgIDFrom(ctx, `
		SELECT a.org_id FROM stacks s
		JOIN applications a ON a.id = s.app_id
		WHERE s.id = $1`, stackID)
}

func (s *Store) OrgForStackVersion(ctx context.Context, versionID uuid.UUID) (uuid.UUID, error) {
	return s.orgIDFrom(ctx, `
		SELECT a.org_id FROM stack_versions v
		JOIN stacks s ON s.id = v.stack_id
		JOIN applications a ON a.id = s.app_id
		WHERE v.id = $1`, versionID)
}

// OrgForEnvironment is not here: it predates this file, in catalog.go, where
// the secret handlers already needed it to decide which nodes' recipients a
// value is sealed to. One resolver per object, wherever it first landed.

func (s *Store) OrgForDeployment(ctx context.Context, deployID uuid.UUID) (uuid.UUID, error) {
	return s.orgIDFrom(ctx, `
		SELECT a.org_id FROM deployments d
		JOIN environments e ON e.id = d.environment_id
		JOIN stacks s ON s.id = e.stack_id
		JOIN applications a ON a.id = s.app_id
		WHERE d.id = $1`, deployID)
}

// OrgForNode reads the column directly: nodes are org-scoped rows, not
// catalog objects, which is why there is no join here and why the multi-org
// node-pool question is still open.
func (s *Store) OrgForNode(ctx context.Context, nodeID uuid.UUID) (uuid.UUID, error) {
	return s.orgIDFrom(ctx, `SELECT org_id FROM nodes WHERE id = $1`, nodeID)
}

func (s *Store) OrgForLogRequest(ctx context.Context, requestID uuid.UUID) (uuid.UUID, error) {
	return s.orgIDFrom(ctx, `
		SELECT a.org_id FROM log_requests lr
		JOIN environments e ON e.id = lr.environment_id
		JOIN stacks s ON s.id = e.stack_id
		JOIN applications a ON a.id = s.app_id
		WHERE lr.id = $1`, requestID)
}

// EnsureOperatorToken records a token whose plaintext the caller already holds,
// idempotently. It exists for one caller: bootstrap, where the dev stack pins a
// known value so compose, the Makefile and the demo scripts can share a
// constant instead of scraping a generated token out of a log line on every
// `make up`.
//
// Deliberately not exposed through the API. A caller-chosen token is only ever
// as strong as whoever chose it, which is fine for a value committed to a dev
// compose file and is not fine for anything else — every operator token that
// protects something is minted by IssueOperatorToken from crypto/rand.
func (s *Store) EnsureOperatorToken(ctx context.Context, operatorID uuid.UUID, name, plain string) error {
	if plain == "" {
		return fmt.Errorf("%w: token is required", ErrInvalid)
	}
	_, err := s.pool.Exec(ctx, `
		INSERT INTO operator_tokens (operator_id, token_hash, name)
		VALUES ($1, $2, $3)
		ON CONFLICT (token_hash) DO NOTHING
	`, operatorID, hashBearerToken(plain), name)
	return mapErr(err)
}
