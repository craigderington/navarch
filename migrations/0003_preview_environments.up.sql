-- Preview environments: an ephemeral env is an ordinary environment with an
-- expiry, not a separate concept, so the scheduler, controller, router and
-- agent all carry it unchanged.
ALTER TABLE environments
    ADD COLUMN ephemeral  BOOLEAN NOT NULL DEFAULT false,
    ADD COLUMN expires_at TIMESTAMPTZ,
    -- An ephemeral env with no expiry is a leak the reaper can never see.
    -- Refuse to store one rather than detect it later.
    ADD CONSTRAINT environments_ephemeral_expiry
        CHECK (NOT ephemeral OR expires_at IS NOT NULL);

CREATE INDEX environments_expiry_idx
    ON environments (expires_at) WHERE ephemeral;

-- Deliberately outlives the environment row it describes. Deleting an env
-- cascades its deployments, instances, volumes and secrets, which is how the
-- agent GCs swappable containers -- but pinned containers are never GC'd from
-- an absent row, because a control-plane outage returning an empty
-- desired-state would then destroy production databases. The tombstone is the
-- explicit instruction that survives the delete.
CREATE TABLE environment_tombstones (
    env8       TEXT PRIMARY KEY,
    org_id     UUID NOT NULL REFERENCES organizations(id) ON DELETE CASCADE,
    created_at TIMESTAMPTZ NOT NULL DEFAULT now()
);

CREATE INDEX environment_tombstones_org_idx
    ON environment_tombstones (org_id, created_at DESC);
