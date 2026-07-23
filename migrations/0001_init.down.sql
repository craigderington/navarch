DROP TRIGGER IF EXISTS deployments_touch ON deployments;
DROP TRIGGER IF EXISTS service_instances_notify ON service_instances;
DROP FUNCTION IF EXISTS touch_updated_at();
DROP FUNCTION IF EXISTS notify_node_desired_state();

DROP TABLE IF EXISTS events;
DROP TABLE IF EXISTS secrets;
DROP TABLE IF EXISTS volumes;
DROP TABLE IF EXISTS service_instances;

ALTER TABLE IF EXISTS environments DROP CONSTRAINT IF EXISTS environments_live_deployment_fk;
DROP TABLE IF EXISTS deployments;
DROP TABLE IF EXISTS environments;
DROP TABLE IF EXISTS stack_versions;
DROP TABLE IF EXISTS stacks;
DROP TABLE IF EXISTS nodes;
DROP TABLE IF EXISTS applications;
DROP TABLE IF EXISTS organizations;

DROP TYPE IF EXISTS instance_state;
DROP TYPE IF EXISTS deployment_state;
DROP TYPE IF EXISTS rollout_strategy;
DROP TYPE IF EXISTS node_state;
