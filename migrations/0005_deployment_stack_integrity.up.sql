-- A deployment's environment and manifest version must belong to the same
-- stack. The duplicated stack_id makes that relationship enforceable with
-- ordinary foreign keys instead of trusting every application caller to join
-- the two parent tables correctly.
ALTER TABLE environments
    ADD CONSTRAINT environments_id_stack_id_key UNIQUE (id, stack_id);

ALTER TABLE stack_versions
    ADD CONSTRAINT stack_versions_id_stack_id_key UNIQUE (id, stack_id);

ALTER TABLE deployments ADD COLUMN stack_id UUID;

UPDATE deployments d
SET stack_id = e.stack_id
FROM environments e
WHERE e.id = d.environment_id;

ALTER TABLE deployments ALTER COLUMN stack_id SET NOT NULL;

ALTER TABLE deployments
    ADD CONSTRAINT deployments_environment_stack_fk
    FOREIGN KEY (environment_id, stack_id)
    REFERENCES environments (id, stack_id) ON DELETE CASCADE;

ALTER TABLE deployments
    ADD CONSTRAINT deployments_version_stack_fk
    FOREIGN KEY (stack_version_id, stack_id)
    REFERENCES stack_versions (id, stack_id);
