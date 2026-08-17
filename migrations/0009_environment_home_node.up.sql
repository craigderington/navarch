-- An environment's durable state -- its pinned container and named volumes --
-- lives on exactly one node, and nothing recorded where. Placement chose a node
-- per deployment, so a later revision could land elsewhere, bring up a pinned
-- container over an empty volume, pass its health check and be promoted while
-- the real data sat unreferenced on the original node. Recording the home node
-- is the same principle teardown already follows: durable state that must be
-- found again is written down, never inferred.
--
-- ON DELETE SET NULL, not RESTRICT, for two reasons. Deleting a node destroys
-- the volumes it held, so an environment homed there has nothing left to be
-- bound to and re-homing is the only possible recovery. And RESTRICT would wedge
-- organization deletion: environments cascade from stacks while nodes cascade
-- from the org, so a node is deleted while environments still reference it --
-- the cascade-ordering hazard already documented for service_instances.node_id.
ALTER TABLE environments
    ADD COLUMN home_node_id UUID REFERENCES nodes(id) ON DELETE SET NULL;

-- Placement reads this per environment on every pending deployment.
CREATE INDEX environments_home_node_idx ON environments (home_node_id)
    WHERE home_node_id IS NOT NULL;
