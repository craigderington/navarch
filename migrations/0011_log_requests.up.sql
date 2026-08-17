-- On-demand container logs. The agent has no inbound server -- it polls -- so
-- the control plane cannot call a node to ask for output. A request is recorded
-- here, the agent collects it alongside its desired state, reads Docker, and
-- posts the result back.
--
-- THIS TABLE HOLDS NO LOG CONTENT, AND THAT IS THE POINT. Container stdout
-- routinely contains secrets: an application logging its own DATABASE_URL, a
-- debug dump of the environment, a stack trace carrying a token. The platform
-- takes real trouble to keep secret plaintext out of the control plane -- values
-- are sealed to node recipients, decrypted agent-side, and never stored -- and
-- writing stdout to Postgres would undo that with a feature nobody asked to be
-- durable: plaintext at rest, in every backup, readable by anyone with database
-- access. Chunks live in a bounded in-memory buffer in the control plane, are
-- delivered to the requester, and are dropped. What is stored here is the
-- instruction, not the answer.
--
-- The columns are therefore all addressing and bounds:
--   instance_id  which container, resolved by the CONTROL PLANE at request time
--   container_id likewise -- the agent is told what to read, never asked to work
--                it out, so it can only ever be pointed at a container it runs
--   tail_lines   and since_at bound what one request can pull, so a single
--                `navarch logs` cannot drag a gigabyte through the poll
--   follow       keeps one row serving a whole tail session instead of a row per
--                poll, which at a 2s tick would be 30 rows a minute
--   expires_at   a requester that walks away must not leave work queued forever;
--                the reaper already sweeps on this pattern
CREATE TYPE log_request_state AS ENUM ('pending', 'done', 'failed');

CREATE TABLE log_requests (
    id            UUID PRIMARY KEY DEFAULT gen_random_uuid(),
    -- CASCADE on every parent deliberately. A log request is the most
    -- disposable row in the schema, and the existing cleanup order
    -- (instances -> deployments -> nodes -> org) must not gain a new referrer
    -- that blocks it -- the two FKs that lack CASCADE are already documented as
    -- a hazard and this is not going to be the third.
    instance_id   UUID NOT NULL REFERENCES service_instances(id) ON DELETE CASCADE,
    environment_id UUID NOT NULL REFERENCES environments(id) ON DELETE CASCADE,
    service_name  TEXT NOT NULL,
    container_id  TEXT NOT NULL,
    tail_lines    INTEGER NOT NULL,
    -- Advanced by the control plane after each delivery when following, so the
    -- next tick asks Docker only for what is new. Nullable: the first fetch of a
    -- non-following request wants whatever `tail_lines` gives it.
    since_at      TIMESTAMPTZ,
    follow        BOOLEAN NOT NULL DEFAULT false,
    state         log_request_state NOT NULL DEFAULT 'pending',
    last_error    TEXT,
    created_at    TIMESTAMPTZ NOT NULL DEFAULT now(),
    updated_at    TIMESTAMPTZ NOT NULL DEFAULT now(),
    expires_at    TIMESTAMPTZ NOT NULL
);

-- The agent's poll runs this every tick per node, so it is the one access path
-- that must not degrade as finished requests pile up before the sweep.
CREATE INDEX log_requests_pending_idx
    ON log_requests (instance_id) WHERE state = 'pending';

CREATE INDEX log_requests_expiry_idx ON log_requests (expires_at);
