-- Connectors core (docs/rewrite/tasks/connectors-architecture.md, phase 1).
-- A row is one connection: org_id its scope (NULL = the server's), kind,
-- capabilities (the ones switched on), share_all and connector_shares its
-- sharing, settings the kind's sealed blob. Renamed in place: dns_domains
-- and connector_shares reference connectors ON DELETE CASCADE, so a rebuild
-- (DROP TABLE) would delete them.
ALTER TABLE connectors RENAME COLUMN provider TO kind;
ALTER TABLE connectors RENAME COLUMN config TO settings;

-- host is the git host of a git connector until the Git source capability
-- owns it (phase 2). Any other kind keeps its kind there, so the old
-- UNIQUE (org_id, host) never makes two kinds of one org collide.
UPDATE connectors SET host = kind
    WHERE NOT EXISTS (SELECT 1 FROM json_each(capabilities) WHERE value = 'git');

-- one connection of a kind per org (server connections: names are unique)
CREATE UNIQUE INDEX connectors_org_kind ON connectors (org_id, kind);
