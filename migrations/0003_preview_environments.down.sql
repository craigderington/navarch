DROP TABLE IF EXISTS environment_tombstones;
DROP INDEX IF EXISTS environments_expiry_idx;
ALTER TABLE environments
    DROP CONSTRAINT IF EXISTS environments_ephemeral_expiry,
    DROP COLUMN IF EXISTS expires_at,
    DROP COLUMN IF EXISTS ephemeral;
