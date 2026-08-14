ALTER TABLE events DROP CONSTRAINT events_deployment_id_fkey;
ALTER TABLE events
    ADD CONSTRAINT events_deployment_id_fkey
    FOREIGN KEY (deployment_id) REFERENCES deployments(id) ON DELETE CASCADE;
