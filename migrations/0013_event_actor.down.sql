DROP INDEX IF EXISTS events_actor_idx;
ALTER TABLE events
    DROP COLUMN IF EXISTS actor_email,
    DROP COLUMN IF EXISTS actor_operator_id;
