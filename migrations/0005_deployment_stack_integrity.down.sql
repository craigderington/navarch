ALTER TABLE deployments DROP CONSTRAINT deployments_version_stack_fk;
ALTER TABLE deployments DROP CONSTRAINT deployments_environment_stack_fk;
ALTER TABLE deployments DROP COLUMN stack_id;
ALTER TABLE stack_versions DROP CONSTRAINT stack_versions_id_stack_id_key;
ALTER TABLE environments DROP CONSTRAINT environments_id_stack_id_key;
