-- Preview hostnames are generated under a wildcard domain that was one value
-- for the whole control plane (COMPOSECTL_PREVIEW_DOMAIN). An organization
-- running its own infrastructure owns its own DNS, so the domain belongs to the
-- org. NULL keeps the server-wide default, which is what every existing install
-- already has.
ALTER TABLE organizations ADD COLUMN preview_domain TEXT;
