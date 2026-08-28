-- Operator identity. Until now every operator route was guarded by one shared
-- bearer token, which internal/config names as pre-multi-tenant scaffolding:
-- any holder reads every org's catalog, secret ciphertext metadata and log
-- output, and can mutate any org's state. The store is already scoped
-- consistently on the node side (DesiredStateForNode, EncryptedSecretsForNode,
-- TombstonesForNode, LogRequestsForNode); this is the operator side catching
-- up to a boundary the codebase already believes in.

CREATE TABLE operators (
    id          UUID PRIMARY KEY DEFAULT gen_random_uuid(),
    email       TEXT NOT NULL,
    name        TEXT NOT NULL,
    -- Disabled, never deleted. events.actor_operator_id points here, and an
    -- event whose actor vanished is an event that cannot be read after an
    -- incident -- the same reasoning that made events.deployment_id
    -- ON DELETE SET NULL in 0004 rather than letting the row disappear.
    disabled_at TIMESTAMPTZ,
    created_at  TIMESTAMPTZ NOT NULL DEFAULT now()
);

-- One human, one row, however they capitalised it. An expression index rather
-- than CITEXT deliberately: citext is available on the dev image but is a
-- contrib extension, and this is the one table whose migration must not fail
-- on whatever Postgres the control plane is eventually pointed at. Lookups go
-- through GetOperatorByEmail, which lowercases to match.
CREATE UNIQUE INDEX operators_email_key ON operators (lower(email));

CREATE TABLE operator_tokens (
    id           UUID PRIMARY KEY DEFAULT gen_random_uuid(),
    operator_id  UUID NOT NULL REFERENCES operators(id) ON DELETE CASCADE,
    -- SHA-256 hex, exactly as nodes.token_hash (0008): same generation (32
    -- bytes of crypto/rand, hex-encoded), same hash at rest, same
    -- constant-time compare. TEXT rather than BYTEA because that is what 0008
    -- chose, and one token format in the codebase beats a tidier column.
    token_hash   TEXT NOT NULL UNIQUE,
    name         TEXT NOT NULL,
    expires_at   TIMESTAMPTZ,
    last_used_at TIMESTAMPTZ,
    created_at   TIMESTAMPTZ NOT NULL DEFAULT now()
);

-- Authentication looks a token up by hash on every request, so the UNIQUE
-- above is doing double duty as that index; this one serves listing and
-- revoking an operator's tokens.
CREATE INDEX operator_tokens_operator_idx ON operator_tokens (operator_id);

CREATE TABLE organization_members (
    org_id      UUID NOT NULL REFERENCES organizations(id) ON DELETE CASCADE,
    operator_id UUID NOT NULL REFERENCES operators(id)     ON DELETE CASCADE,
    -- A column, not a system. Every member is 'owner' this slice and nothing
    -- reads the value; it exists so adding 'viewer' later is a migration of
    -- data rather than of schema. An authorization model with no operational
    -- history behind it is the same guess the platform refuses to make about
    -- node retirement, so the check is membership and nothing finer.
    role        TEXT NOT NULL DEFAULT 'owner',
    created_at  TIMESTAMPTZ NOT NULL DEFAULT now(),
    PRIMARY KEY (org_id, operator_id)
);

-- Both directions are cascades on purpose. The cascade-ordering hazard already
-- recorded for service_instances.node_id and deployments.stack_version_id is
-- that a parent can be dropped before its referrer; a membership row is pure
-- association owned by both ends, so either end going away should take it with
-- them rather than wedging the delete.
CREATE INDEX organization_members_operator_idx ON organization_members (operator_id);
