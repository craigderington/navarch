-- Exactly one live revision per environment. PromoteDeployment supersedes
-- the previous live row in the same transaction, so this is satisfiable.
CREATE UNIQUE INDEX deployments_one_live_idx
    ON deployments (environment_id)
    WHERE state = 'live';
