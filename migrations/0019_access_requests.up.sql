-- A doorbell for an invite-only platform.
--
-- The Sprint 9 plan framed the choice as self-serve signup -- which needs email
-- verification, rate limiting and quotas before it is safe -- or a deliberate
-- decision to stay invite-only. This is neither: an access request creates no
-- identity, so it needs none of those three. There is no operator row, no
-- organization, no membership and no token; there is only a note that somebody
-- asked. An operator turns it into an invitation through the path that already
-- exists, and the address is verified the way it always was -- by an invite
-- being useless to anyone who did not receive it.
--
-- org_id is NOT NULL and set by the server from its own configuration, never by
-- the requester. A stranger does not know which organization they are asking to
-- join, and letting them name one would make this an organization-enumeration
-- oracle on an unauthenticated route.
CREATE TABLE access_requests (
    id         UUID PRIMARY KEY DEFAULT gen_random_uuid(),
    org_id     UUID NOT NULL REFERENCES organizations(id) ON DELETE CASCADE,
    email      TEXT NOT NULL,
    name       TEXT NOT NULL DEFAULT '',
    -- What they said they want to do with it. Free text, bounded by the
    -- handler, and shown to an operator as-is -- it is the only part of the
    -- request that helps decide.
    note       TEXT NOT NULL DEFAULT '',

    -- TEXT with a CHECK rather than a Postgres enum, unlike node_state and
    -- deployment_state. Those predate two live bugs this codebase records: pgx
    -- cannot encode a named-string slice against an enum array, and a CASE over
    -- unknown-type literals resolves as text and will not assign to an enum
    -- column. Neither is worth inviting for three values that are never queried
    -- as an array and never built in a CASE.
    state      TEXT NOT NULL DEFAULT 'pending'
               CHECK (state IN ('pending', 'approved', 'declined')),

    -- What approving produced. SET NULL rather than CASCADE for the reason the
    -- invite's own inviter is: the record that somebody asked and was let in
    -- must outlive the credential that let them in, or an incident review is
    -- left with a membership and no story about where it came from.
    invite_id  UUID REFERENCES operator_invites(id) ON DELETE SET NULL,
    decided_by UUID REFERENCES operators(id) ON DELETE SET NULL,
    decided_at TIMESTAMPTZ,
    created_at TIMESTAMPTZ NOT NULL DEFAULT now()
);

-- At most one pending request per address per organization, the same shape as
-- operator_invites_one_live_idx and for the same reason: a public form is
-- submitted twice by anyone who is not sure it worked the first time, and two
-- rows for one person means an operator approves one and leaves the other
-- pending forever. The insert upserts onto this index, so a second submission
-- updates what they said instead of queueing behind it.
--
-- Partial on state, so a declined address can ask again and an approved one
-- does not block a later re-request. That is deliberate: the alternative is a
-- permanent, silent denylist that nobody can see or lift.
CREATE UNIQUE INDEX access_requests_one_pending_idx
    ON access_requests (org_id, lower(email))
    WHERE state = 'pending';

CREATE INDEX access_requests_org_idx ON access_requests (org_id, created_at DESC);
