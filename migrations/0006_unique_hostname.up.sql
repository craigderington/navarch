-- Environment hostnames are Traefik Host() keys. Duplicates misroute
-- traffic; the column previously had no uniqueness or shape constraint.
CREATE UNIQUE INDEX environments_hostname_uidx
    ON environments (hostname)
    WHERE hostname IS NOT NULL AND hostname <> '';
