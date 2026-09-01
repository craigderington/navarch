-- Invitations, so onboarding an operator does not mean handing them a bearer
-- token out of band.
--
-- The token is hashed, never stored, exactly as operator_tokens are: this row
-- is reachable by anyone with database access, and an invite that could be read
-- out of it is a way to become the person who was invited. The plaintext exists
-- for one response and one email.
--
-- redeemed_at rather than a DELETE on redemption. An invite is the record of
-- how somebody got access, and deleting it after use destroys exactly the
-- evidence an incident review would want -- the same reasoning that disables
-- operators instead of deleting them.
CREATE TABLE operator_invites (
    id          UUID PRIMARY KEY DEFAULT gen_random_uuid(),
    org_id      UUID NOT NULL REFERENCES organizations(id) ON DELETE CASCADE,
    email       TEXT NOT NULL,
    role        TEXT NOT NULL DEFAULT 'member',
    token_hash  TEXT NOT NULL,
    -- SET NULL, not CASCADE: an invite whose inviter was later disabled and
    -- removed must not vanish, for the reason above.
    invited_by  UUID REFERENCES operators(id) ON DELETE SET NULL,
    redeemed_by UUID REFERENCES operators(id) ON DELETE SET NULL,
    expires_at  TIMESTAMPTZ NOT NULL,
    redeemed_at TIMESTAMPTZ,
    revoked_at  TIMESTAMPTZ,
    created_at  TIMESTAMPTZ NOT NULL DEFAULT now()
);

CREATE UNIQUE INDEX operator_invites_token_hash_key ON operator_invites (token_hash);

-- Redemption looks up by hash alone; every other read is "this org's invites".
CREATE INDEX operator_invites_org_idx ON operator_invites (org_id, created_at DESC);

-- At most one live invitation per address per organization. Without it, an
-- admin clicking twice mints two credentials for one person and only one of
-- them is ever accounted for -- and revoking "the invite" would leave the other
-- working.
CREATE UNIQUE INDEX operator_invites_one_live_idx
    ON operator_invites (org_id, lower(email))
    WHERE redeemed_at IS NULL AND revoked_at IS NULL;
