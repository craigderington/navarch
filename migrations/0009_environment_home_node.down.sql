DROP INDEX IF EXISTS environments_home_node_idx;
ALTER TABLE environments DROP COLUMN IF EXISTS home_node_id;
