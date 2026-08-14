-- Audit events must outlive deployments (notably expired previews). Retain the
-- timeline entry while clearing the optional deployment link.
ALTER TABLE events DROP CONSTRAINT events_deployment_id_fkey;
ALTER TABLE events
    ADD CONSTRAINT events_deployment_id_fkey
    FOREIGN KEY (deployment_id) REFERENCES deployments(id) ON DELETE SET NULL;
