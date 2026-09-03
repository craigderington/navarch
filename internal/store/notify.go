package store

import (
	"context"

	"github.com/google/uuid"
)

// NotifyTarget is who to tell about something, and enough context to say what.
//
// One type and one query per subject, rather than a general "operators for org"
// helper plus a separate lookup for names. The loops that use these are on a
// tick and must not grow a round trip each; more importantly, a notification
// assembled from two reads can describe an environment that the second read saw
// differently from the first.
type NotifyTarget struct {
	// Emails is every enabled member of the owning organization.
	//
	// Everyone, deliberately, with no per-operator preference. An install where
	// that is too noisy is an install big enough to want routing rules, and a
	// half-built preference model is worse than none: the failure mode of the
	// simple version is an unwanted email, and of the clever one is a missing
	// alert nobody notices is missing.
	Emails []string
	Org    string
	App    string
	Stack  string
	Env    string
	// Hostname is what the environment is served as, and may be empty — not
	// every environment has one.
	Hostname string
}

// NotifyTargetsForDeployment resolves the recipients and the human-readable
// path for a deployment, in one query.
//
// Disabled operators are excluded: an operator is disabled and never deleted so
// audit events keep their actor, but continuing to mail someone whose access
// was revoked is the opposite of what disabling them meant.
func (s *Store) NotifyTargetsForDeployment(ctx context.Context, deploymentID uuid.UUID) (*NotifyTarget, error) {
	return s.notifyTargets(ctx, `
		SELECT o.slug, a.slug, st.slug, e.slug, COALESCE(e.hostname, ''),
		       COALESCE(array_agg(op.email) FILTER (WHERE op.disabled_at IS NULL), '{}')
		FROM deployments d
		JOIN environments e ON e.id = d.environment_id
		JOIN stacks st ON st.id = e.stack_id
		JOIN applications a ON a.id = st.app_id
		JOIN organizations o ON o.id = a.org_id
		LEFT JOIN organization_members m ON m.org_id = o.id
		LEFT JOIN operators op ON op.id = m.operator_id
		WHERE d.id = $1
		GROUP BY o.slug, a.slug, st.slug, e.slug, e.hostname
	`, deploymentID)
}

// NotifyTargetsForEnvironment is the same for an environment with no deployment
// in hand — the reaper's case, where the subject is the environment itself.
func (s *Store) NotifyTargetsForEnvironment(ctx context.Context, environmentID uuid.UUID) (*NotifyTarget, error) {
	return s.notifyTargets(ctx, `
		SELECT o.slug, a.slug, st.slug, e.slug, COALESCE(e.hostname, ''),
		       COALESCE(array_agg(op.email) FILTER (WHERE op.disabled_at IS NULL), '{}')
		FROM environments e
		JOIN stacks st ON st.id = e.stack_id
		JOIN applications a ON a.id = st.app_id
		JOIN organizations o ON o.id = a.org_id
		LEFT JOIN organization_members m ON m.org_id = o.id
		LEFT JOIN operators op ON op.id = m.operator_id
		WHERE e.id = $1
		GROUP BY o.slug, a.slug, st.slug, e.slug, e.hostname
	`, environmentID)
}

// OrgOperatorEmails is every enabled member of an organization.
//
// Its own method rather than a NotifyTarget, because the subject here is the
// organization itself: an access request names no app, stack or environment, so
// the four slug fields would all be empty and the type would be describing
// something that does not exist. Disabled operators are excluded for the reason
// they are everywhere else — an operator is disabled rather than deleted so
// audit events keep their actor, and continuing to mail someone whose access
// was revoked is the opposite of what disabling them meant.
func (s *Store) OrgOperatorEmails(ctx context.Context, orgID uuid.UUID) ([]string, error) {
	var emails []string
	err := s.pool.QueryRow(ctx, `
		SELECT COALESCE(array_agg(op.email) FILTER (WHERE op.disabled_at IS NULL), '{}')
		FROM organization_members m
		JOIN operators op ON op.id = m.operator_id
		WHERE m.org_id = $1
	`, orgID).Scan(&emails)
	if err != nil {
		return nil, mapErr(err)
	}
	return emails, nil
}

func (s *Store) notifyTargets(ctx context.Context, query string, id uuid.UUID) (*NotifyTarget, error) {
	var t NotifyTarget
	err := s.pool.QueryRow(ctx, query, id).
		Scan(&t.Org, &t.App, &t.Stack, &t.Env, &t.Hostname, &t.Emails)
	if err != nil {
		return nil, mapErr(err)
	}
	return &t, nil
}

// Path renders the slug path the CLI accepts, so an operator can paste the
// subject line of an email straight into a command.
func (t *NotifyTarget) Path() string {
	return t.Org + "/" + t.App + "/" + t.Stack + "/" + t.Env
}
