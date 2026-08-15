-- Per-node bearer token, stored as a SHA-256 hex digest. The plaintext is
-- returned once at registration and never written here.
ALTER TABLE nodes ADD COLUMN token_hash TEXT;
