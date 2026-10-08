-- Migration E: the server config file. Server connectors and their shares,
-- the server file's plans, and the approver's ticks on both plan tables; see
-- docs/rewrite/tasks/serverconfig.md.

-- connectors: org_id becomes nullable (NULL = a server connector, shared
-- with orgs through connector_shares or share_all). SQLite cannot drop a
-- NOT NULL, so the table is rebuilt. Nothing references connectors by a
-- foreign key (orgs and stacks hold the id as plain text), so the copy,
-- drop and rename are safe with foreign keys on.
CREATE TABLE connectors_new (
    id          TEXT      PRIMARY KEY,
    org_id      TEXT      REFERENCES orgs (id) ON DELETE CASCADE,
    provider    TEXT      NOT NULL,
    name        TEXT      NOT NULL,
    host        TEXT      NOT NULL,
    config      TEXT      NOT NULL, -- encrypted: holds the App private key
    created_at  DATETIME  NOT NULL,
    share_all   INTEGER   NOT NULL DEFAULT 0, -- server connector: every org may use it
    UNIQUE (org_id, host)
);

INSERT INTO connectors_new (id, org_id, provider, name, host, config, created_at)
    SELECT id, org_id, provider, name, host, config, created_at FROM connectors;

DROP TABLE connectors;
ALTER TABLE connectors_new RENAME TO connectors;

-- the server file names its connectors, so their names are unique; NULL
-- org_ids are distinct to UNIQUE (org_id, host), so a host may repeat.
CREATE UNIQUE INDEX connectors_server_name ON connectors (name) WHERE org_id IS NULL;

-- connector_shares: the orgs a server connector is shared with by name.
CREATE TABLE connector_shares (
    connector_id  TEXT  NOT NULL REFERENCES connectors (id) ON DELETE CASCADE,
    org_id        TEXT  NOT NULL REFERENCES orgs (id) ON DELETE CASCADE,
    PRIMARY KEY (connector_id, org_id)
);

CREATE INDEX connector_shares_org ON connector_shares (org_id);

-- server_config_plans: org_config_plans without the org, plus where the
-- file came from. file holds a local file's bytes ('' for a repo plan).
-- ticked is the JSON list of removal keys the approver ticked.
CREATE TABLE server_config_plans (
    id          TEXT      PRIMARY KEY,
    commit_sha  TEXT      NOT NULL,
    summary     TEXT      NOT NULL,
    plan        TEXT      NOT NULL, -- JSON serverconfig.Plan
    status      TEXT      NOT NULL CHECK (status IN ('pending', 'clean', 'error', 'superseded', 'applied', 'rejected')),
    error       TEXT      NOT NULL,
    created_at  DATETIME  NOT NULL,
    decided_at  DATETIME,
    source      TEXT      NOT NULL CHECK (source IN ('repo', 'local')),
    file        TEXT      NOT NULL,
    ticked      TEXT      NOT NULL DEFAULT '[]',
    confirmed   INTEGER   NOT NULL DEFAULT 0
);

CREATE INDEX server_config_plans_created ON server_config_plans (created_at DESC);

ALTER TABLE org_config_plans ADD COLUMN ticked TEXT NOT NULL DEFAULT '[]';
ALTER TABLE org_config_plans ADD COLUMN confirmed INTEGER NOT NULL DEFAULT 0;
