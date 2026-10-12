-- node_orgs: the orgs a node is opened to. Only an opened node may be pinned
-- by that org's owners (admins pin anywhere). The controller is open to every
-- org without a row. forgotten_nodes: a forgotten node's id and name, so its
-- envs (environments.node_id still names it) show as lost until redeployed.
CREATE TABLE node_orgs (
    node_id TEXT NOT NULL REFERENCES nodes (id) ON DELETE CASCADE,
    org_id  TEXT NOT NULL REFERENCES orgs (id) ON DELETE CASCADE,
    PRIMARY KEY (node_id, org_id)
);

CREATE TABLE forgotten_nodes (
    id           TEXT      PRIMARY KEY,
    name         TEXT      NOT NULL,
    forgotten_at TIMESTAMP NOT NULL
);
