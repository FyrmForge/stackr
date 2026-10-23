-- DNS connectors (docs/rewrite/tasks/dns-connectors.md, D1). A connector
-- row says what it can do: capabilities is a JSON array ("git", "dns");
-- provider is the kind (github, namecheap).
ALTER TABLE connectors ADD COLUMN capabilities TEXT NOT NULL DEFAULT '["git"]';

-- dns_domains: the domains of a DNS connector's account that stackr
-- manages. org_id is the connector's org (NULL = a server connector, whose
-- domain is available to every org when share_all, else to org_ids, a JSON
-- array of org ids). A domain is managed through one connector only.
CREATE TABLE dns_domains (
    id            TEXT      PRIMARY KEY,
    connector_id  TEXT      NOT NULL REFERENCES connectors (id) ON DELETE CASCADE,
    org_id        TEXT      REFERENCES orgs (id) ON DELETE CASCADE,
    name          TEXT      NOT NULL UNIQUE,
    share_all     INTEGER   NOT NULL,
    org_ids       TEXT      NOT NULL,
    created_at    DATETIME  NOT NULL
);

CREATE INDEX dns_domains_connector ON dns_domains (connector_id);
