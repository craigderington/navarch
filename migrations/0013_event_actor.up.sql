-- Events already record what happened; they do not record who asked for it.
-- With one shared token there was no answer to record. Operator identity makes
-- the audit log attributable, which is the point of having one.
ALTER TABLE events
    -- SET NULL rather than CASCADE for the reason 0004 gave about deployments:
    -- the timeline entry must outlive the thing it points at. Operators are
    -- soft-deleted anyway, so this fires only on a deliberate purge.
    ADD COLUMN actor_operator_id UUID REFERENCES operators(id) ON DELETE SET NULL,
    -- Denormalized so a purged operator still leaves a readable name in the
    -- timeline. An audit trail that degrades to a NULL uuid answers "someone"
    -- when the question is "who", which is the one question it exists for.
    ADD COLUMN actor_email TEXT;

-- Actor-filtered reads walk an org's timeline, which is already the shape of
-- events_org_time_idx; this covers "everything this operator did".
CREATE INDEX events_actor_idx ON events (actor_operator_id, created_at DESC)
    WHERE actor_operator_id IS NOT NULL;
