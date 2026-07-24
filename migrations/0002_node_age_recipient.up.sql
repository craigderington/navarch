-- The agent's age public key (recipient). The control plane encrypts secrets to
-- it; only the agent holds the matching private key. Nullable: a node sets it
-- at registration.
ALTER TABLE nodes ADD COLUMN age_recipient TEXT;
