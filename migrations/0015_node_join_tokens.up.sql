-- Node enrolment, scoped to one organization.
--
-- RegisterNode resolves the org from the request body and checks membership
-- only when the caller is an operator. A caller holding the shared
-- COMPOSECTL_AGENT_TOKEN therefore gets no org check at all: it can register a
-- node into ANY org by naming its slug. That is correct for one trusted fleet
-- and fatal the moment a customer holds the token, which is what
-- bring-your-own-infrastructure requires.
--
-- A join token *is* the statement "this machine may join this organization", so
-- the org is taken from the token and never from the request body. A body
-- naming a different org is refused rather than silently overridden: a node
-- that believes it joined one org and actually joined another is worse than a
-- node that failed to start.
CREATE TABLE node_join_tokens (
    id          UUID PRIMARY KEY DEFAULT gen_random_uuid(),
    org_id      UUID NOT NULL REFERENCES organizations(id) ON DELETE CASCADE,
    -- SHA-256 hex, the one token format this codebase uses (see 0008, 0012).
    token_hash  TEXT NOT NULL UNIQUE,
    name        TEXT NOT NULL,
    expires_at  TIMESTAMPTZ,
    -- NULL means unlimited, which is what a fleet that adds nodes over time
    -- wants; a finite count is for handing one machine a single-use credential.
    max_uses    INTEGER,
    uses        INTEGER NOT NULL DEFAULT 0,
    -- SET NULL rather than CASCADE: the token outliving the operator who issued
    -- it is correct, and the audit trail should not lose the row.
    created_by  UUID REFERENCES operators(id) ON DELETE SET NULL,
    created_at  TIMESTAMPTZ NOT NULL DEFAULT now()
);

CREATE INDEX node_join_tokens_org_idx ON node_join_tokens (org_id);
