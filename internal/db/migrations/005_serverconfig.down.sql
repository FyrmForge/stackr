ALTER TABLE org_config_plans DROP COLUMN confirmed;
ALTER TABLE org_config_plans DROP COLUMN ticked;

DROP TABLE IF EXISTS server_config_plans;
DROP TABLE IF EXISTS connector_shares;

-- A server connector has no org to belong to in the old shape: it goes, and
-- an org or stack binding that named it forgets the id.
UPDATE orgs SET config_connector_id = ''
    WHERE config_connector_id IN (SELECT id FROM connectors WHERE org_id IS NULL);
UPDATE stacks SET config_connector_id = ''
    WHERE config_connector_id IN (SELECT id FROM connectors WHERE org_id IS NULL);
DELETE FROM connectors WHERE org_id IS NULL;

DROP INDEX IF EXISTS connectors_server_name;

CREATE TABLE connectors_old (
    id          TEXT      PRIMARY KEY,
    org_id      TEXT      NOT NULL REFERENCES orgs (id) ON DELETE CASCADE,
    provider    TEXT      NOT NULL,
    name        TEXT      NOT NULL,
    host        TEXT      NOT NULL,
    config      TEXT      NOT NULL, -- encrypted: holds the App private key
    created_at  DATETIME  NOT NULL,
    UNIQUE (org_id, host)
);

INSERT INTO connectors_old (id, org_id, provider, name, host, config, created_at)
    SELECT id, org_id, provider, name, host, config, created_at FROM connectors;

DROP TABLE connectors;
ALTER TABLE connectors_old RENAME TO connectors;
